package ordinal

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	hotIndexDigestDomain    = "TASKGATE-ORDINAL-HOT-INDEX-V1\x00"
	coldPayloadDigestDomain = "TASKGATE-ORDINAL-COLD-PAYLOAD-V1\x00"
	sidecarDigestDomain     = "TASKGATE-ORDINAL-ROW-SIDECAR-V1\x00"
)

// CompiledArtifact explicitly separates the Gateway hot index from cold audit
// material. Register only Hot; store Cold in a payload service or mmap-backed
// audit artifact.
type CompiledArtifact struct {
	Hot  *HotDictionary
	Cold *ColdDictionary
}

// HotDictionary contains only manifest metadata, fixed-width FactID hashes,
// ordinal bounds and row-handle refs. It holds no FactID or canonical payload.
type HotDictionary struct {
	manifest       DictionaryManifest
	manifestDigest string
	hashes         map[string][][sha256.Size]byte
	entityToHandle map[string]RowHandle
	rowSegment     string
	fields         []hotField
	rows           []hotRow
}

type hotField struct {
	name      string
	segmentID string
}

type hotRow struct {
	entityKey      string
	rowSegmentID   string
	rowOrdinal     uint32
	cellSegmentIDs []string
	cellOrdinals   []uint32
}

type coldEntry struct {
	payload  []byte
	factJSON []byte
}

func digestColdEntries(manifests []SegmentManifest, segments map[string][]dictionaryEntry) string {
	hash := sha256.New()
	hash.Write([]byte(coldPayloadDigestDomain))
	writeUint64(hash, uint64(len(manifests)))
	for _, manifest := range manifests {
		writeString(hash, manifest.ID)
		writeUint64(hash, uint64(len(segments[manifest.ID])))
		for _, entry := range segments[manifest.ID] {
			writeBytes(hash, entry.payload)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestHotDictionary(dictionaryDigest string, manifests []SegmentManifest, segments map[string][]dictionaryEntry,
	rows map[RowHandle]RowRefs) string {
	hash := sha256.New()
	hash.Write([]byte(hotIndexDigestDomain))
	writeString(hash, dictionaryDigest)
	writeUint64(hash, uint64(len(manifests)))
	for _, manifest := range manifests {
		writeString(hash, manifest.ID)
		writeUint64(hash, uint64(len(segments[manifest.ID])))
		for _, entry := range segments[manifest.ID] {
			_, _ = hash.Write(entry.hash[:])
		}
	}
	writeUint64(hash, uint64(len(rows)))
	for handle := RowHandle(1); uint64(handle) <= uint64(len(rows)); handle++ {
		row := rows[handle]
		writeUint64(hash, uint64(handle))
		writeString(hash, row.Row.SegmentID)
		writeUint32(hash, row.Row.Ordinal)
		fields := make([]string, 0, len(row.Cells))
		for field := range row.Cells {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		writeUint64(hash, uint64(len(fields)))
		for _, field := range fields {
			writeString(hash, field)
			writeString(hash, row.Cells[field].SegmentID)
			writeUint32(hash, row.Cells[field].Ordinal)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestSidecarRows(rows map[RowHandle]RowRefs) string {
	hash := sha256.New()
	hash.Write([]byte(sidecarDigestDomain))
	writeUint64(hash, uint64(len(rows)))
	for handle := RowHandle(1); uint64(handle) <= uint64(len(rows)); handle++ {
		writeUint64(hash, uint64(handle))
		writeString(hash, rows[handle].EntityKey)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ColdDictionary is an exact resolver for audit Decode. A production
// implementation may replace it with content-addressed compressed chunks.
type ColdDictionary struct {
	manifest         DictionaryManifest
	manifestDigest   string
	dictionaryDigest string
	segments         map[string][]coldEntry
}

func (d *Dictionary) Split() (CompiledArtifact, error) {
	return d.split(false)
}

// splitOwned transfers the immutable payload backing arrays into the cold
// artifact instead of cloning them. It is reserved for compiler entrypoints
// that discard the combined oracle dictionary immediately afterwards. The
// distinction matters at publication scale: retaining two complete copies of
// every canonical payload serves no semantic purpose and can exceed the
// offline builder's memory envelope.
func (d *Dictionary) splitOwned() (CompiledArtifact, error) {
	return d.split(true)
}

func (d *Dictionary) split(transferOwnership bool) (CompiledArtifact, error) {
	if d == nil {
		return CompiledArtifact{}, fmt.Errorf("%w: nil dictionary", ErrInvalid)
	}
	hot := &HotDictionary{manifest: d.Manifest(), manifestDigest: d.ManifestDigest(),
		hashes:         make(map[string][][sha256.Size]byte, len(d.segments)),
		entityToHandle: make(map[string]RowHandle, len(d.entityToHandle)), rows: make([]hotRow, len(d.rows))}
	cold := &ColdDictionary{manifest: d.Manifest(), manifestDigest: d.ManifestDigest(), dictionaryDigest: d.DictionaryDigest(),
		segments: make(map[string][]coldEntry, len(d.segments))}
	for segmentID, entries := range d.segments {
		hashes := make([][sha256.Size]byte, len(entries))
		coldEntries := make([]coldEntry, len(entries))
		for index, entry := range entries {
			hashes[index] = entry.hash
			if transferOwnership {
				coldEntries[index] = coldEntry{payload: entry.payload, factJSON: entry.factJSON}
			} else {
				coldEntries[index] = coldEntry{payload: append([]byte(nil), entry.payload...),
					factJSON: append([]byte(nil), entry.factJSON...)}
			}
		}
		hot.hashes[segmentID] = hashes
		cold.segments[segmentID] = coldEntries
		if transferOwnership {
			delete(d.segments, segmentID)
		}
	}
	for entity, handle := range d.entityToHandle {
		hot.entityToHandle[entity] = handle
	}
	if len(d.rows) != 0 {
		first := d.rows[1]
		if first.Handle != 1 {
			return CompiledArtifact{}, fmt.Errorf("%w: non-dense row handles", ErrInvalid)
		}
		hot.rowSegment = first.Row.SegmentID
		fieldNames := make([]string, 0, len(first.Cells))
		for field := range first.Cells {
			fieldNames = append(fieldNames, field)
		}
		sort.Strings(fieldNames)
		for _, field := range fieldNames {
			hot.fields = append(hot.fields, hotField{name: field, segmentID: first.Cells[field].SegmentID})
		}
		for handle := RowHandle(1); uint64(handle) <= uint64(len(d.rows)); handle++ {
			row, found := d.rows[handle]
			if !found || row.Handle != handle {
				return CompiledArtifact{}, fmt.Errorf("%w: non-dense row handles", ErrInvalid)
			}
			if row.Row.SegmentID != hot.rowSegment {
				hot.rowSegment = ""
			}
			for index, field := range hot.fields {
				ref, present := row.Cells[field.name]
				if !present {
					return CompiledArtifact{}, fmt.Errorf("%w: inconsistent row cell fields", ErrInvalid)
				}
				if ref.SegmentID != field.segmentID {
					hot.fields[index].segmentID = ""
				}
			}
		}
	}
	for handle := RowHandle(1); uint64(handle) <= uint64(len(d.rows)); handle++ {
		row, found := d.rows[handle]
		if !found || row.Handle != handle {
			return CompiledArtifact{}, fmt.Errorf("%w: non-dense row handles", ErrInvalid)
		}
		compact := hotRow{entityKey: row.EntityKey, rowOrdinal: row.Row.Ordinal,
			cellSegmentIDs: make([]string, len(hot.fields)), cellOrdinals: make([]uint32, len(hot.fields))}
		if hot.rowSegment == "" {
			compact.rowSegmentID = row.Row.SegmentID
		}
		for index, field := range hot.fields {
			ref, present := row.Cells[field.name]
			if !present {
				return CompiledArtifact{}, fmt.Errorf("%w: inconsistent row cell fields", ErrInvalid)
			}
			if field.segmentID == "" {
				compact.cellSegmentIDs[index] = ref.SegmentID
			} else if ref.SegmentID != field.segmentID {
				return CompiledArtifact{}, fmt.Errorf("%w: inconsistent row cell segments", ErrInvalid)
			}
			compact.cellOrdinals[index] = ref.Ordinal
		}
		hot.rows[handle-1] = compact
	}
	return CompiledArtifact{Hot: hot, Cold: cold}, nil
}

func (d *HotDictionary) Manifest() DictionaryManifest {
	if d == nil {
		return DictionaryManifest{}
	}
	result := d.manifest
	result.Segments = append([]SegmentManifest(nil), d.manifest.Segments...)
	return result
}

func (d *HotDictionary) DictionaryDigest() string {
	if d == nil {
		return ""
	}
	return d.manifest.DictionaryDigest
}

func (d *HotDictionary) ManifestDigest() string {
	if d == nil {
		return ""
	}
	return d.manifestDigest
}

func (d *HotDictionary) Hash(ref FactRef) ([sha256.Size]byte, error) {
	if d == nil || ref.Validate() != nil || ref.DictionaryDigest != d.DictionaryDigest() {
		return [sha256.Size]byte{}, ErrUnknownFact
	}
	hashes := d.hashes[ref.SegmentID]
	if uint64(ref.Ordinal) >= uint64(len(hashes)) {
		return [sha256.Size]byte{}, ErrUnknownFact
	}
	return hashes[ref.Ordinal], nil
}

func (d *HotDictionary) SegmentFactCount(segmentID string) (uint64, bool) {
	if d == nil {
		return 0, false
	}
	hashes, found := d.hashes[segmentID]
	return uint64(len(hashes)), found
}

func (d *HotDictionary) ValidateSetBounds(set BitmapSet) error {
	if d == nil {
		return fmt.Errorf("%w: nil hot dictionary", ErrInvalid)
	}
	for _, bound := range set.SegmentBounds() {
		count, found := d.SegmentFactCount(bound.Segment.SegmentID)
		if bound.Segment.DictionaryDigest != d.DictionaryDigest() || !found || uint64(bound.MaxOrdinal) >= count {
			return ErrUnknownFact
		}
	}
	return nil
}

func (d *HotDictionary) RowCount() uint64 {
	if d == nil {
		return 0
	}
	return uint64(len(d.rows))
}

func (d *HotDictionary) LookupRowHandle(entityKey string) (RowHandle, bool) {
	if d == nil {
		return 0, false
	}
	handle, found := d.entityToHandle[entityKey]
	return handle, found
}

func (d *HotDictionary) LookupRow(handle RowHandle) (RowRefs, bool) {
	if d == nil || handle == 0 || uint64(handle) > uint64(len(d.rows)) {
		return RowRefs{}, false
	}
	compact := d.rows[handle-1]
	rowSegmentID := d.rowSegment
	if rowSegmentID == "" {
		rowSegmentID = compact.rowSegmentID
	}
	row := RowRefs{Handle: handle, EntityKey: compact.entityKey,
		Row:   FactRef{DictionaryDigest: d.DictionaryDigest(), SegmentID: rowSegmentID, Ordinal: compact.rowOrdinal},
		Cells: make(map[string]FactRef, len(d.fields))}
	for index, field := range d.fields {
		segmentID := field.segmentID
		if segmentID == "" {
			segmentID = compact.cellSegmentIDs[index]
		}
		row.Cells[field.name] = FactRef{DictionaryDigest: d.DictionaryDigest(), SegmentID: segmentID, Ordinal: compact.cellOrdinals[index]}
	}
	return row, true
}

// LookupRowIdentity returns the immutable row identity without materializing
// the complete field-to-ref map.  It is equivalent to the identity portion of
// LookupRow.
func (d *HotDictionary) LookupRowIdentity(handle RowHandle) (string, FactRef, bool) {
	if d == nil || handle == 0 || uint64(handle) > uint64(len(d.rows)) {
		return "", FactRef{}, false
	}
	compact := d.rows[handle-1]
	segmentID := d.rowSegment
	if segmentID == "" {
		segmentID = compact.rowSegmentID
	}
	return compact.entityKey, FactRef{DictionaryDigest: d.DictionaryDigest(), SegmentID: segmentID, Ordinal: compact.rowOrdinal}, true
}

// LookupCellRef returns one immutable cell ref without allocating a map.  HOT
// fields are manifest-validated in strict lexical order, so binary search is
// deterministic and bounded by the (normally tiny) publication schema.
func (d *HotDictionary) LookupCellRef(handle RowHandle, fieldID string) (FactRef, bool) {
	if d == nil || handle == 0 || uint64(handle) > uint64(len(d.rows)) {
		return FactRef{}, false
	}
	fieldIndex := sort.Search(len(d.fields), func(index int) bool { return d.fields[index].name >= fieldID })
	if fieldIndex == len(d.fields) || d.fields[fieldIndex].name != fieldID {
		return FactRef{}, false
	}
	compact := d.rows[handle-1]
	segmentID := d.fields[fieldIndex].segmentID
	if segmentID == "" {
		segmentID = compact.cellSegmentIDs[fieldIndex]
	}
	return FactRef{DictionaryDigest: d.DictionaryDigest(), SegmentID: segmentID, Ordinal: compact.cellOrdinals[fieldIndex]}, true
}

func (d *HotDictionary) LookupEntity(entityKey string) (RowRefs, bool) {
	handle, found := d.LookupRowHandle(entityKey)
	if !found {
		return RowRefs{}, false
	}
	return d.LookupRow(handle)
}

// StreamHashes visits fixed-width hashes in ref order without loading cold
// payloads or allocating a FactRef slice.
func (d *HotDictionary) StreamHashes(set BitmapSet, yield func(FactRef, [sha256.Size]byte) error) error {
	if yield == nil {
		return fmt.Errorf("%w: nil hash callback", ErrInvalid)
	}
	if err := d.ValidateSetBounds(set); err != nil {
		return err
	}
	var streamErr error
	set.Refs(func(ref FactRef) bool {
		hash, err := d.Hash(ref)
		if err != nil {
			streamErr = err
			return false
		}
		streamErr = yield(ref, hash)
		return streamErr == nil
	})
	return streamErr
}

// StreamHashesByFactHash performs a k-way merge over hash-sorted ordinal
// segments. Memory is O(segment count), not O(fact count), which is suitable
// for witness commitments requiring global FactHash order.
func (d *HotDictionary) StreamHashesByFactHash(set BitmapSet, yield func(FactRef, [sha256.Size]byte) error) error {
	if yield == nil {
		return fmt.Errorf("%w: nil hash callback", ErrInvalid)
	}
	if err := d.ValidateSetBounds(set); err != nil {
		return err
	}
	queue := hashCursorHeap{}
	for _, segment := range sortedSegmentRefs(set.segments) {
		iterator := set.segments[segment].Iterator()
		if iterator.HasNext() {
			ordinal := iterator.Next()
			heap.Push(&queue, hashCursor{segment: segment, iterator: iterator, ordinal: ordinal,
				hash: d.hashes[segment.SegmentID][ordinal]})
		}
	}
	for queue.Len() != 0 {
		cursor := heap.Pop(&queue).(hashCursor)
		ref := FactRef{DictionaryDigest: cursor.segment.DictionaryDigest, SegmentID: cursor.segment.SegmentID, Ordinal: cursor.ordinal}
		if err := yield(ref, cursor.hash); err != nil {
			return err
		}
		if cursor.iterator.HasNext() {
			cursor.ordinal = cursor.iterator.Next()
			cursor.hash = d.hashes[cursor.segment.SegmentID][cursor.ordinal]
			heap.Push(&queue, cursor)
		}
	}
	return nil
}

type hashCursor struct {
	segment  SegmentRef
	iterator roaring.IntPeekable
	ordinal  uint32
	hash     [sha256.Size]byte
}

type hashCursorHeap []hashCursor

func (h hashCursorHeap) Len() int { return len(h) }
func (h hashCursorHeap) Less(i, j int) bool {
	comparison := bytes.Compare(h[i].hash[:], h[j].hash[:])
	if comparison != 0 {
		return comparison < 0
	}
	if h[i].segment.SegmentID != h[j].segment.SegmentID {
		return h[i].segment.SegmentID < h[j].segment.SegmentID
	}
	return h[i].ordinal < h[j].ordinal
}
func (h hashCursorHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *hashCursorHeap) Push(value any) { *h = append(*h, value.(hashCursor)) }
func (h *hashCursorHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func (d *ColdDictionary) Expand(ref FactRef) (exposure.FactID, error) {
	if d == nil || ref.Validate() != nil || ref.DictionaryDigest != d.dictionaryDigest {
		return exposure.FactID{}, ErrUnknownFact
	}
	entries := d.segments[ref.SegmentID]
	if uint64(ref.Ordinal) >= uint64(len(entries)) {
		return exposure.FactID{}, ErrUnknownFact
	}
	fact, err := decodeFact(entries[ref.Ordinal].factJSON)
	if err != nil {
		return exposure.FactID{}, fmt.Errorf("%w: stored cold fact JSON", ErrInvalid)
	}
	return fact, nil
}

func (d *ColdDictionary) CanonicalPayload(ref FactRef) ([]byte, error) {
	if d == nil || ref.Validate() != nil || ref.DictionaryDigest != d.dictionaryDigest {
		return nil, ErrUnknownFact
	}
	entries := d.segments[ref.SegmentID]
	if uint64(ref.Ordinal) >= uint64(len(entries)) {
		return nil, ErrUnknownFact
	}
	return append([]byte(nil), entries[ref.Ordinal].payload...), nil
}

func (d *ColdDictionary) StreamExpand(set BitmapSet, yield func(ExpandedContent) error) error {
	if d == nil || yield == nil {
		return fmt.Errorf("%w: nil cold dictionary or callback", ErrInvalid)
	}
	var streamErr error
	set.Refs(func(ref FactRef) bool {
		payload, err := d.CanonicalPayload(ref)
		if err != nil {
			streamErr = err
			return false
		}
		fact, _ := d.Expand(ref)
		hashText, _ := fact.Hash()
		var hash [sha256.Size]byte
		decoded, _ := hex.DecodeString(hashText)
		copy(hash[:], decoded)
		streamErr = yield(ExpandedContent{Ref: ref, Hash: hash, CanonicalPayload: payload})
		return streamErr == nil
	})
	return streamErr
}
