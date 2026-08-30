package exposure

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"

	"taskbound.local/agent-data-gateway/internal/encryptedspool"
)

const (
	releaseObservationSpoolMagic = "TGROBS1"
	// releaseObservationSpillSlots bounds the runs being sorted and written in
	// the background; each holds at most one residency threshold of payloads.
	releaseObservationSpillSlots = 4
)

// ReleaseObservation accumulates one query's V2 release set for
// ReleaseOutcomeDigest without retaining decoded FactIDs. Each fact is kept as
// its canonical payload keyed by its FactID hash. Resident payloads are bounded
// by factSetSpoolThresholdBytes; every overflow becomes one hash-sorted run in
// an authenticated encrypted spool, and Digest streams a k-way merge of the
// runs in hash order. The committed byte sequence is therefore exactly the one
// ReleaseOutcomeDigest produces over FactSet.Values: same domain, same unique
// count, equal hashes collapse to one payload, and an equal hash with a
// different payload fails closed as a collision. Only the cost changed: no
// JSON encoding, no re-decoding, no re-validation, and one hash per fact.
type ReleaseObservation struct {
	threshold     int64
	chunkSize     int
	baseDir       string
	resident      []releaseEntry
	residentBytes int64
	total         int
	closed        bool
	err           error

	// Runs are sorted and written by background goroutines so the caller's
	// row path does not pay for the sort; mu guards runs and spillErr, spills
	// waits for every in-flight run, and slots bounds them.
	mu        sync.Mutex
	runs      []*releaseRun
	spillErr  error
	spills    sync.WaitGroup
	slots     chan struct{}
	finalizer bool
}

type releaseEntry struct {
	hash    [32]byte
	payload []byte
}

type releaseRun struct {
	spool *encryptedspool.Spool
	count int
	// hashes is the run's sorted, deduplicated hash sequence, kept resident
	// (32 bytes per fact) so the unique count needs no decrypting pass.
	hashes [][32]byte
}

// releaseEntries sorts by hash without reflection; ties keep insertion order
// under sort.Stable.
type releaseEntries []releaseEntry

func (e releaseEntries) Len() int           { return len(e) }
func (e releaseEntries) Less(i, j int) bool { return bytes.Compare(e[i].hash[:], e[j].hash[:]) < 0 }
func (e releaseEntries) Swap(i, j int)      { e[i], e[j] = e[j], e[i] }

// NewReleaseObservation returns an empty accumulator with the FactSet
// residency threshold.
func NewReleaseObservation() *ReleaseObservation {
	return newReleaseObservation(factSetSpoolThresholdBytes, "", factSetSpoolChunkSize)
}

func newReleaseObservation(threshold int64, baseDir string, chunkSize int) *ReleaseObservation {
	return &ReleaseObservation{threshold: threshold, baseDir: baseDir, chunkSize: chunkSize,
		slots: make(chan struct{}, releaseObservationSpillSlots)}
}

// spillError reports the first background run failure, if any.
func (o *ReleaseObservation) spillError() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.spillErr
}

// Add hashes the fact once and records it.
func (o *ReleaseObservation) Add(fact FactID) error {
	if o == nil {
		return fmt.Errorf("%w: nil release observation", ErrInvalid)
	}
	payload, hash, err := fact.CanonicalPayloadHash()
	if err != nil {
		return err
	}
	return o.AddCanonical(fact, payload, hash)
}

// AddCanonical records a fact whose canonical payload and hash the caller has
// already computed; the derivation verifies base facts against the snapshot
// ordinal with that same hash, so it is computed exactly once per fact. The
// observation takes ownership of payload: the caller must not read or modify
// it afterwards (it is zeroed once spooled or closed).
func (o *ReleaseObservation) AddCanonical(fact FactID, payload []byte, hash [32]byte) error {
	if o == nil {
		return fmt.Errorf("%w: nil release observation", ErrInvalid)
	}
	if o.closed {
		return os.ErrClosed
	}
	if o.err != nil {
		return o.err
	}
	if err := o.spillError(); err != nil {
		o.err = err
		return err
	}
	if !fact.IsV2() {
		return fmt.Errorf("%w: outcome release set contains a non-V2 fact", ErrInvalid)
	}
	if len(payload) == 0 || len(payload) > maxFactSetRecordBytes {
		return fmt.Errorf("%w: release observation record length is invalid", ErrInvalid)
	}
	o.resident = append(o.resident, releaseEntry{hash: hash, payload: payload})
	o.residentBytes += int64(len(payload))
	o.total++
	if o.residentBytes > o.threshold {
		if err := o.spillRun(); err != nil {
			o.err = err
			return err
		}
	}
	return nil
}

// Len reports the number of recorded facts before deduplication.
func (o *ReleaseObservation) Len() int {
	if o == nil {
		return 0
	}
	return o.total
}

// Spilled reports whether at least one run reached the encrypted spool.
func (o *ReleaseObservation) Spilled() bool {
	if o == nil {
		return false
	}
	o.spills.Wait()
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.runs) != 0
}

// sortRun orders entries by hash, keeping insertion order among equal hashes,
// and collapses equal hashes to their first payload; an equal hash with a
// different payload is a collision and fails closed exactly as FactSet.Add.
func sortRun(entries []releaseEntry) ([]releaseEntry, error) {
	sort.Stable(releaseEntries(entries))
	unique := entries[:0]
	for index, entry := range entries {
		if index > 0 && entry.hash == entries[index-1].hash {
			if !bytes.Equal(unique[len(unique)-1].payload, entry.payload) {
				return nil, fmt.Errorf("%w: fact hash collision for %s", ErrInvalid, hex.EncodeToString(entry.hash[:]))
			}
			zeroBytes(entry.payload)
			continue
		}
		unique = append(unique, entry)
	}
	return unique, nil
}

// spillRun hands the resident entries to a background goroutine that sorts
// them and writes one run; the caller continues with an empty resident set.
// A sort collision or write failure surfaces at the next AddCanonical or at
// Digest, before any digest is produced.
func (o *ReleaseObservation) spillRun() error {
	entries := o.resident
	o.resident = nil
	o.residentBytes = 0
	if !o.finalizer {
		o.finalizer = true
		runtime.SetFinalizer(o, (*ReleaseObservation).finalize)
	}
	o.slots <- struct{}{}
	o.spills.Add(1)
	go func() {
		defer o.spills.Done()
		defer func() { <-o.slots }()
		run, err := writeReleaseRun(entries, o.baseDir, o.chunkSize)
		o.mu.Lock()
		defer o.mu.Unlock()
		if err != nil {
			if o.spillErr == nil {
				o.spillErr = err
			}
			return
		}
		o.runs = append(o.runs, run)
	}()
	return nil
}

// writeReleaseRun sorts entries and writes them as one encrypted run.
func writeReleaseRun(entries []releaseEntry, baseDir string, chunkSize int) (*releaseRun, error) {
	sorted, err := sortRun(entries)
	if err != nil {
		return nil, err
	}
	runID := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, runID); err != nil {
		return nil, fmt.Errorf("create release observation run identity: %w", err)
	}
	aad := append([]byte("taskgate-release-observation-v1\x00"), runID...)
	spool, err := encryptedspool.New(encryptedspool.Config{
		BaseDir: baseDir, DirectoryPrefix: ".taskgate-release-observation-", FileName: "release.spool",
		Magic: releaseObservationSpoolMagic, AAD: aad, Threshold: 1, ChunkSize: chunkSize,
		UnlinkImmediately: true,
	})
	if err != nil {
		return nil, err
	}
	run := &releaseRun{spool: spool, count: len(sorted), hashes: make([][32]byte, 0, len(sorted))}
	for _, entry := range sorted {
		run.hashes = append(run.hashes, entry.hash)
	}
	// Record: 32-byte hash, 4-byte payload length, payload. The hash is stored
	// so readers need not re-hash; the spool's AEAD chunks authenticate it.
	var header [sha256.Size + 4]byte
	for _, entry := range sorted {
		copy(header[:sha256.Size], entry.hash[:])
		binary.BigEndian.PutUint32(header[sha256.Size:], uint32(len(entry.payload)))
		if _, err := spool.Write(header[:]); err != nil {
			_ = spool.Close()
			return nil, fmt.Errorf("write encrypted release observation record: %w", err)
		}
		if _, err := spool.Write(entry.payload); err != nil {
			_ = spool.Close()
			return nil, fmt.Errorf("write encrypted release observation record: %w", err)
		}
		zeroBytes(entry.payload)
	}
	return run, nil
}

// Digest returns the ReleaseOutcomeDigest of every recorded fact.
func (o *ReleaseObservation) Digest(visibleRows int64) (string, error) {
	if o == nil {
		return "", fmt.Errorf("%w: nil release observation", ErrInvalid)
	}
	if o.closed {
		return "", os.ErrClosed
	}
	if o.err != nil {
		return "", o.err
	}
	if visibleRows < 0 {
		return "", fmt.Errorf("%w: visible row count cannot be negative", ErrInvalid)
	}
	o.spills.Wait()
	if err := o.spillError(); err != nil {
		o.err = err
		return "", err
	}
	sorted, err := sortRun(o.resident)
	if err != nil {
		o.err = err
		return "", err
	}
	o.resident = sorted
	o.residentBytes = 0
	for _, entry := range sorted {
		o.residentBytes += int64(len(entry.payload))
	}
	unique := o.uniqueHashCount()
	digestState := sha256.New()
	digestState.Write([]byte(outcomeDigestDomainV1))
	writeCanonicalUint64Stream(digestState, uint64(visibleRows))
	writeCanonicalUint64Stream(digestState, uint64(unique))
	if err := o.merge(func(_ [32]byte, payload []byte) error {
		writeCanonicalUint64Stream(digestState, uint64(len(payload)))
		digestState.Write(payload)
		return nil
	}); err != nil {
		o.err = err
		return "", err
	}
	return hex.EncodeToString(digestState.Sum(nil)), nil
}

// uniqueHashCount merges the resident hashes with every run's resident hash
// sequence and counts distinct hashes; payload equality of equal hashes is
// checked by the payload merge that follows, before any digest is returned.
func (o *ReleaseObservation) uniqueHashCount() int {
	o.mu.Lock()
	sources := make([][][32]byte, 0, len(o.runs)+1)
	for _, run := range o.runs {
		sources = append(sources, run.hashes)
	}
	o.mu.Unlock()
	resident := make([][32]byte, 0, len(o.resident))
	for _, entry := range o.resident {
		resident = append(resident, entry.hash)
	}
	sources = append(sources, resident)
	positions := make([]int, len(sources))
	unique := 0
	for {
		var lowest *[32]byte
		for index, source := range sources {
			if positions[index] >= len(source) {
				continue
			}
			if lowest == nil || bytes.Compare(source[positions[index]][:], lowest[:]) < 0 {
				lowest = &source[positions[index]]
			}
		}
		if lowest == nil {
			return unique
		}
		hash := *lowest
		unique++
		for index, source := range sources {
			if positions[index] < len(source) && source[positions[index]] == hash {
				positions[index]++
			}
		}
	}
}

// releaseCursor walks one hash-sorted source: a sealed run behind an
// authenticated reader, or the sorted resident slice.
type releaseCursor struct {
	reader    io.ReadCloser
	remaining int
	resident  []releaseEntry
	position  int
	hash      [32]byte
	payload   []byte
	buffer    []byte
	active    bool
}

// advance loads the next entry. A spooled cursor reads into its own reusable
// buffer, so payload is only valid until the next advance or close.
func (c *releaseCursor) advance() error {
	c.payload = nil
	c.active = false
	if c.reader == nil {
		if c.position >= len(c.resident) {
			return nil
		}
		entry := c.resident[c.position]
		c.position++
		c.hash, c.payload, c.active = entry.hash, entry.payload, true
		return nil
	}
	if c.remaining == 0 {
		var extra [1]byte
		if count, err := c.reader.Read(extra[:]); count != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return errors.New("encrypted release observation contains trailing plaintext")
		}
		return nil
	}
	var header [sha256.Size + 4]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return fmt.Errorf("read encrypted release observation record header: %w", err)
	}
	length := int(binary.BigEndian.Uint32(header[sha256.Size:]))
	if length < 1 || length > maxFactSetRecordBytes {
		return errors.New("encrypted release observation record length is invalid")
	}
	if cap(c.buffer) < length {
		zeroBytes(c.buffer)
		c.buffer = make([]byte, length, length+length/2)
	}
	payload := c.buffer[:length]
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return fmt.Errorf("read encrypted release observation record: %w", err)
	}
	c.remaining--
	copy(c.hash[:], header[:sha256.Size])
	c.payload, c.active = payload, true
	return nil
}

func (c *releaseCursor) close() error {
	zeroBytes(c.buffer)
	c.buffer, c.payload = nil, nil
	if c.reader == nil {
		return nil
	}
	return c.reader.Close()
}

// merge visits every distinct payload in ascending hash order across all
// runs and the resident slice. Equal hashes must carry equal payloads.
func (o *ReleaseObservation) merge(visit func([32]byte, []byte) error) (err error) {
	cursors := make([]*releaseCursor, 0, releaseObservationSpillSlots+1)
	defer func() {
		for _, cursor := range cursors {
			if closeErr := cursor.close(); closeErr != nil && err == nil {
				err = closeErr
			}
		}
	}()
	o.mu.Lock()
	runs := append([]*releaseRun(nil), o.runs...)
	o.mu.Unlock()
	for _, run := range runs {
		reader, err := run.spool.Snapshot()
		if err != nil {
			return fmt.Errorf("open encrypted release observation: %w", err)
		}
		cursors = append(cursors, &releaseCursor{reader: reader, remaining: run.count})
	}
	cursors = append(cursors, &releaseCursor{resident: o.resident})
	for _, cursor := range cursors {
		if err := cursor.advance(); err != nil {
			return err
		}
	}
	matched := make([]*releaseCursor, 0, len(cursors))
	for {
		var lowest *releaseCursor
		for _, cursor := range cursors {
			if !cursor.active {
				continue
			}
			if lowest == nil || bytes.Compare(cursor.hash[:], lowest.hash[:]) < 0 {
				lowest = cursor
			}
		}
		if lowest == nil {
			return nil
		}
		hash, payload := lowest.hash, lowest.payload
		if err := visit(hash, payload); err != nil {
			return err
		}
		// Compare every equal-hash cursor before advancing any of them:
		// advancing zeroes the visited payload.
		matched = matched[:0]
		for _, cursor := range cursors {
			if !cursor.active || cursor.hash != hash {
				continue
			}
			if cursor != lowest && !bytes.Equal(cursor.payload, payload) {
				return fmt.Errorf("%w: fact hash collision for %s", ErrInvalid, hex.EncodeToString(hash[:]))
			}
			matched = append(matched, cursor)
		}
		for _, cursor := range matched {
			if err := cursor.advance(); err != nil {
				return err
			}
		}
	}
}

// Close clears resident plaintext and removes every run.
func (o *ReleaseObservation) Close() error {
	if o == nil || o.closed {
		return nil
	}
	o.closed = true
	runtime.SetFinalizer(o, nil)
	o.spills.Wait()
	o.mu.Lock()
	runs := o.runs
	o.runs = nil
	o.mu.Unlock()
	var err error
	for _, run := range runs {
		if closeErr := run.spool.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	for _, entry := range o.resident {
		zeroBytes(entry.payload)
	}
	o.resident = nil
	o.residentBytes = 0
	return err
}

func (o *ReleaseObservation) finalize() { _ = o.Close() }

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
