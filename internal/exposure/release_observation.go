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

	"taskbound.local/agent-data-gateway/internal/encryptedspool"
)

const releaseObservationSpoolMagic = "TGROBS1"

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
	runs          []*releaseRun
	total         int
	closed        bool
	err           error
}

type releaseEntry struct {
	hash    [32]byte
	payload []byte
}

type releaseRun struct {
	spool *encryptedspool.Spool
	count int
}

// NewReleaseObservation returns an empty accumulator with the FactSet
// residency threshold.
func NewReleaseObservation() *ReleaseObservation {
	return newReleaseObservation(factSetSpoolThresholdBytes, "", factSetSpoolChunkSize)
}

func newReleaseObservation(threshold int64, baseDir string, chunkSize int) *ReleaseObservation {
	return &ReleaseObservation{threshold: threshold, baseDir: baseDir, chunkSize: chunkSize}
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
// ordinal with that same hash, so it is computed exactly once per fact.
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
	if !fact.IsV2() {
		return fmt.Errorf("%w: outcome release set contains a non-V2 fact", ErrInvalid)
	}
	if len(payload) == 0 || len(payload) > maxFactSetRecordBytes {
		return fmt.Errorf("%w: release observation record length is invalid", ErrInvalid)
	}
	o.resident = append(o.resident, releaseEntry{hash: hash, payload: append([]byte(nil), payload...)})
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
func (o *ReleaseObservation) Spilled() bool { return o != nil && len(o.runs) != 0 }

// sortRun orders entries by hash, keeping insertion order among equal hashes,
// and collapses equal hashes to their first payload; an equal hash with a
// different payload is a collision and fails closed exactly as FactSet.Add.
func sortRun(entries []releaseEntry) ([]releaseEntry, error) {
	sort.SliceStable(entries, func(i, j int) bool {
		return bytes.Compare(entries[i].hash[:], entries[j].hash[:]) < 0
	})
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

func (o *ReleaseObservation) spillRun() error {
	sorted, err := sortRun(o.resident)
	if err != nil {
		return err
	}
	runID := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, runID); err != nil {
		return fmt.Errorf("create release observation run identity: %w", err)
	}
	aad := append([]byte("taskgate-release-observation-v1\x00"), runID...)
	spool, err := encryptedspool.New(encryptedspool.Config{
		BaseDir: o.baseDir, DirectoryPrefix: ".taskgate-release-observation-", FileName: "release.spool",
		Magic: releaseObservationSpoolMagic, AAD: aad, Threshold: 1, ChunkSize: o.chunkSize,
		UnlinkImmediately: true,
	})
	if err != nil {
		return err
	}
	if len(o.runs) == 0 {
		runtime.SetFinalizer(o, (*ReleaseObservation).finalize)
	}
	run := &releaseRun{spool: spool, count: len(sorted)}
	o.runs = append(o.runs, run)
	var header [4]byte
	for _, entry := range sorted {
		binary.BigEndian.PutUint32(header[:], uint32(len(entry.payload)))
		if _, err := spool.Write(header[:]); err != nil {
			return fmt.Errorf("write encrypted release observation record: %w", err)
		}
		if _, err := spool.Write(entry.payload); err != nil {
			return fmt.Errorf("write encrypted release observation record: %w", err)
		}
		zeroBytes(entry.payload)
	}
	o.resident = nil
	o.residentBytes = 0
	return nil
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
	unique := 0
	if err := o.merge(func([32]byte, []byte) error {
		unique++
		return nil
	}); err != nil {
		o.err = err
		return "", err
	}
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

// releaseCursor walks one hash-sorted source: a sealed run behind an
// authenticated reader, or the sorted resident slice.
type releaseCursor struct {
	reader    io.ReadCloser
	remaining int
	resident  []releaseEntry
	position  int
	hash      [32]byte
	payload   []byte
	active    bool
}

func (c *releaseCursor) advance() error {
	if c.payload != nil && c.reader != nil {
		zeroBytes(c.payload)
	}
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
	var header [4]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return fmt.Errorf("read encrypted release observation record length: %w", err)
	}
	length := int(binary.BigEndian.Uint32(header[:]))
	if length < 1 || length > maxFactSetRecordBytes {
		return errors.New("encrypted release observation record length is invalid")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return fmt.Errorf("read encrypted release observation record: %w", err)
	}
	c.remaining--
	c.hash = sha256.Sum256(append([]byte(factDomainV2), payload...))
	c.payload, c.active = payload, true
	return nil
}

// merge visits every distinct payload in ascending hash order across all
// runs and the resident slice. Equal hashes must carry equal payloads.
func (o *ReleaseObservation) merge(visit func([32]byte, []byte) error) (err error) {
	cursors := make([]*releaseCursor, 0, len(o.runs)+1)
	defer func() {
		for _, cursor := range cursors {
			if cursor.reader != nil {
				if closeErr := cursor.reader.Close(); closeErr != nil && err == nil {
					err = closeErr
				}
			}
		}
	}()
	for _, run := range o.runs {
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
		for _, cursor := range cursors {
			if !cursor.active || cursor.hash != hash {
				continue
			}
			if cursor != lowest && !bytes.Equal(cursor.payload, payload) {
				return fmt.Errorf("%w: fact hash collision for %s", ErrInvalid, hex.EncodeToString(hash[:]))
			}
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
	var err error
	for _, run := range o.runs {
		if closeErr := run.spool.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	o.runs = nil
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
