package ordinal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"
)

const (
	containerDigestDomain = "TASKGATE-ORDINAL-CONTAINER-V1\x00"
	segmentDigestDomain   = "TASKGATE-ORDINAL-SEGMENT-V1\x00"
	bitmapSetDigestDomain = "TASKGATE-ORDINAL-BITMAP-SET-V1\x00"
)

// FactRef is the lossless physical address of one canonical FactID. A
// dictionary compiler guarantees that the mapping is a bijection.
type FactRef struct {
	DictionaryDigest string `json:"dictionary_digest"`
	SegmentID        string `json:"segment_id"`
	Ordinal          uint32 `json:"ordinal"`
}

func (r FactRef) Validate() error {
	if !validDigest(r.DictionaryDigest) {
		return fmt.Errorf("%w: dictionary digest must be lowercase SHA-256", ErrInvalid)
	}
	if !validID(r.SegmentID) {
		return fmt.Errorf("%w: segment id must be a canonical token", ErrInvalid)
	}
	return nil
}

// SegmentRef names one disjoint uint32 ordinal space in a dictionary.
type SegmentRef struct {
	DictionaryDigest string `json:"dictionary_digest"`
	SegmentID        string `json:"segment_id"`
}

func (r SegmentRef) Validate() error {
	return (FactRef{DictionaryDigest: r.DictionaryDigest, SegmentID: r.SegmentID}).Validate()
}

func (r FactRef) segment() SegmentRef {
	return SegmentRef{DictionaryDigest: r.DictionaryDigest, SegmentID: r.SegmentID}
}

// ContainerKey is the durable key for one immutable Roaring high-16
// container. Bitmap contains complete uint32 ordinals, while High16 makes the
// storage partition explicit and independently addressable.
type ContainerKey struct {
	DictionaryDigest string `json:"dictionary_digest"`
	SegmentID        string `json:"segment_id"`
	High16           uint16 `json:"high16"`
}

func (k ContainerKey) segment() SegmentRef {
	return SegmentRef{DictionaryDigest: k.DictionaryDigest, SegmentID: k.SegmentID}
}

func (k ContainerKey) Validate() error { return k.segment().Validate() }

// PortableContainer is the canonical, cross-language Roaring serialization
// of exactly one high-16 container. Callers must treat Bitmap as immutable.
type PortableContainer struct {
	Key         ContainerKey `json:"key"`
	Bitmap      []byte       `json:"bitmap"`
	Cardinality uint64       `json:"cardinality"`
	Digest      string       `json:"digest"`
}

// PortableSegment is a convenience encoding for a whole dictionary segment.
// Durable stores should normally prefer PortableContainer for COW updates.
type PortableSegment struct {
	Segment     SegmentRef `json:"segment"`
	Bitmap      []byte     `json:"bitmap"`
	Cardinality uint64     `json:"cardinality"`
	Digest      string     `json:"digest"`
}

// SegmentBound permits fail-closed ordinal-range validation in O(number of
// segments), without expanding every FactRef.
type SegmentBound struct {
	Segment     SegmentRef `json:"segment"`
	Cardinality uint64     `json:"cardinality"`
	MinOrdinal  uint32     `json:"min_ordinal"`
	MaxOrdinal  uint32     `json:"max_ordinal"`
}

// BitmapSet is an immutable collection of exact FactRef values. Mutating
// Roaring bitmaps are never returned to callers; every algebra operation owns
// its result.
type BitmapSet struct {
	segments map[SegmentRef]*roaring.Bitmap
}

// Builder amortizes insertion while preserving BitmapSet immutability at the
// boundary. A Builder must not be copied after first use.
type Builder struct {
	segments map[SegmentRef]*roaring.Bitmap
	err      error
}

func NewBuilder() *Builder {
	return &Builder{segments: make(map[SegmentRef]*roaring.Bitmap)}
}

func (b *Builder) Add(ref FactRef) error {
	if b == nil {
		return fmt.Errorf("%w: nil bitmap builder", ErrInvalid)
	}
	if b.err != nil {
		return b.err
	}
	if b.segments == nil {
		b.segments = make(map[SegmentRef]*roaring.Bitmap)
	}
	key := ref.segment()
	bitmap := b.segments[key]
	if bitmap == nil {
		// Dictionary and segment identity are invariant for every ordinal in a
		// segment. Validate once when the segment first enters this builder rather
		// than rescanning the same 64-byte digest for every streamed fact.
		if err := ref.Validate(); err != nil {
			b.err = err
			return err
		}
		bitmap = roaring.New()
		b.segments[key] = bitmap
	}
	bitmap.Add(ref.Ordinal)
	return nil
}

func (b *Builder) AddMany(refs []FactRef) error {
	for _, ref := range refs {
		if err := b.Add(ref); err != nil {
			return err
		}
	}
	return nil
}

func (b *Builder) Freeze() (BitmapSet, error) {
	if b == nil {
		return BitmapSet{}, fmt.Errorf("%w: nil bitmap builder", ErrInvalid)
	}
	if b.err != nil {
		return BitmapSet{}, b.err
	}
	return BitmapSet{segments: cloneSegments(b.segments)}, nil
}

func NewBitmapSet(refs ...FactRef) (BitmapSet, error) {
	builder := NewBuilder()
	if err := builder.AddMany(refs); err != nil {
		return BitmapSet{}, err
	}
	return builder.Freeze()
}

// Cardinality is exact because ordinals are unique inside each disjoint
// dictionary segment. Dictionary compilation additionally prevents a FactID
// from appearing in more than one segment.
func (s BitmapSet) Cardinality() uint64 {
	var result uint64
	for _, bitmap := range s.segments {
		result += bitmap.GetCardinality()
	}
	return result
}

func (s BitmapSet) IsEmpty() bool { return s.Cardinality() == 0 }

func (s BitmapSet) Contains(ref FactRef) bool {
	if ref.Validate() != nil {
		return false
	}
	bitmap := s.segments[ref.segment()]
	return bitmap != nil && bitmap.Contains(ref.Ordinal)
}

func (s BitmapSet) MaxOrdinal(segment SegmentRef) (uint32, bool) {
	bitmap := s.segments[segment]
	if bitmap == nil || bitmap.IsEmpty() {
		return 0, false
	}
	return bitmap.Maximum(), true
}

func (s BitmapSet) SegmentBounds() []SegmentBound {
	keys := sortedSegmentRefs(s.segments)
	result := make([]SegmentBound, 0, len(keys))
	for _, key := range keys {
		bitmap := s.segments[key]
		result = append(result, SegmentBound{Segment: key, Cardinality: bitmap.GetCardinality(),
			MinOrdinal: bitmap.Minimum(), MaxOrdinal: bitmap.Maximum()})
	}
	return result
}

// Refs visits FactRefs in deterministic dictionary, segment, ordinal order.
// Returning false stops iteration. It does not allocate a million-element
// materialization.
func (s BitmapSet) Refs(yield func(FactRef) bool) {
	if yield == nil {
		return
	}
	for _, key := range sortedSegmentRefs(s.segments) {
		iterator := s.segments[key].Iterator()
		for iterator.HasNext() {
			if !yield(FactRef{DictionaryDigest: key.DictionaryDigest, SegmentID: key.SegmentID, Ordinal: iterator.Next()}) {
				return
			}
		}
	}
}

// Union implements exact set OR without mutating either operand.
func (s BitmapSet) Union(other BitmapSet) BitmapSet {
	result := cloneSegments(s.segments)
	if result == nil && len(other.segments) != 0 {
		result = make(map[SegmentRef]*roaring.Bitmap, len(other.segments))
	}
	for key, right := range other.segments {
		if left := result[key]; left != nil {
			left.Or(right)
		} else {
			result[key] = right.Clone()
		}
	}
	return BitmapSet{segments: result}
}

// Difference implements exact ANDNOT without mutating either operand.
func (s BitmapSet) Difference(other BitmapSet) BitmapSet {
	result := make(map[SegmentRef]*roaring.Bitmap, len(s.segments))
	for key, left := range s.segments {
		bitmap := left.Clone()
		if right := other.segments[key]; right != nil {
			bitmap.AndNot(right)
		}
		if !bitmap.IsEmpty() {
			result[key] = bitmap
		}
	}
	return BitmapSet{segments: result}
}

// Intersection implements exact AND without mutating either operand.
func (s BitmapSet) Intersection(other BitmapSet) BitmapSet {
	result := make(map[SegmentRef]*roaring.Bitmap)
	for key, left := range s.segments {
		if right := other.segments[key]; right != nil {
			bitmap := roaring.And(left, right)
			if !bitmap.IsEmpty() {
				result[key] = bitmap
			}
		}
	}
	return BitmapSet{segments: result}
}

func (s BitmapSet) Equal(other BitmapSet) bool {
	if s.Cardinality() != other.Cardinality() || len(s.segments) != len(other.segments) {
		return false
	}
	for key, left := range s.segments {
		right := other.segments[key]
		if right == nil || !left.Equals(right) {
			return false
		}
	}
	return true
}

// PortableContainers returns independently content-addressed high-16
// containers. Every returned byte slice is newly allocated.
func (s BitmapSet) PortableContainers() ([]PortableContainer, error) {
	result := make([]PortableContainer, 0)
	for _, segment := range sortedSegmentRefs(s.segments) {
		iterator := s.segments[segment].Iterator()
		var currentHigh uint16
		var current *roaring.Bitmap
		flush := func() error {
			if current == nil || current.IsEmpty() {
				return nil
			}
			encoded, err := marshalCanonicalBitmap(current)
			if err != nil {
				return err
			}
			item := PortableContainer{
				Key:    ContainerKey{DictionaryDigest: segment.DictionaryDigest, SegmentID: segment.SegmentID, High16: currentHigh},
				Bitmap: encoded, Cardinality: current.GetCardinality(),
			}
			item.Digest = containerDigest(item.Key, item.Cardinality, item.Bitmap)
			result = append(result, item)
			return nil
		}
		for iterator.HasNext() {
			value := iterator.Next()
			high := uint16(value >> 16)
			if current == nil || high != currentHigh {
				if err := flush(); err != nil {
					return nil, err
				}
				currentHigh = high
				current = roaring.New()
			}
			current.Add(value)
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// ParsePortableContainers validates canonical serialization, content digest,
// high-16 partitioning and duplicate keys before constructing a set.
func ParsePortableContainers(items []PortableContainer) (BitmapSet, error) {
	segments := make(map[SegmentRef]*roaring.Bitmap)
	seen := make(map[ContainerKey]struct{}, len(items))
	for index, item := range items {
		if err := item.Key.Validate(); err != nil {
			return BitmapSet{}, fmt.Errorf("%w: container %d key: %v", ErrInvalid, index, err)
		}
		if _, duplicate := seen[item.Key]; duplicate {
			return BitmapSet{}, fmt.Errorf("%w: duplicate container key", ErrInvalid)
		}
		seen[item.Key] = struct{}{}
		bitmap, err := parseCanonicalBitmap(item.Bitmap)
		if err != nil {
			return BitmapSet{}, fmt.Errorf("container %d: %w", index, err)
		}
		if bitmap.IsEmpty() || bitmap.GetCardinality() != item.Cardinality {
			return BitmapSet{}, fmt.Errorf("%w: container %d cardinality", ErrInvalid, index)
		}
		iterator := bitmap.Iterator()
		for iterator.HasNext() {
			if uint16(iterator.Next()>>16) != item.Key.High16 {
				return BitmapSet{}, fmt.Errorf("%w: container %d crosses high16 boundary", ErrInvalid, index)
			}
		}
		if !validDigest(item.Digest) || item.Digest != containerDigest(item.Key, item.Cardinality, item.Bitmap) {
			return BitmapSet{}, fmt.Errorf("%w: container %d", ErrDigestMismatch, index)
		}
		segment := item.Key.segment()
		if segments[segment] == nil {
			segments[segment] = bitmap
		} else {
			segments[segment].Or(bitmap)
		}
	}
	return BitmapSet{segments: segments}, nil
}

func (s BitmapSet) PortableSegments() ([]PortableSegment, error) {
	result := make([]PortableSegment, 0, len(s.segments))
	for _, segment := range sortedSegmentRefs(s.segments) {
		encoded, err := marshalCanonicalBitmap(s.segments[segment])
		if err != nil {
			return nil, err
		}
		item := PortableSegment{Segment: segment, Bitmap: encoded, Cardinality: s.segments[segment].GetCardinality()}
		item.Digest = segmentDigest(item.Segment, item.Cardinality, item.Bitmap)
		result = append(result, item)
	}
	return result, nil
}

func ParsePortableSegments(items []PortableSegment) (BitmapSet, error) {
	segments := make(map[SegmentRef]*roaring.Bitmap, len(items))
	for index, item := range items {
		if err := item.Segment.Validate(); err != nil {
			return BitmapSet{}, fmt.Errorf("%w: segment %d key: %v", ErrInvalid, index, err)
		}
		if _, duplicate := segments[item.Segment]; duplicate {
			return BitmapSet{}, fmt.Errorf("%w: duplicate portable segment", ErrInvalid)
		}
		bitmap, err := parseCanonicalBitmap(item.Bitmap)
		if err != nil {
			return BitmapSet{}, fmt.Errorf("segment %d: %w", index, err)
		}
		if bitmap.IsEmpty() || bitmap.GetCardinality() != item.Cardinality {
			return BitmapSet{}, fmt.Errorf("%w: segment %d cardinality", ErrInvalid, index)
		}
		if !validDigest(item.Digest) || item.Digest != segmentDigest(item.Segment, item.Cardinality, item.Bitmap) {
			return BitmapSet{}, fmt.Errorf("%w: segment %d", ErrDigestMismatch, index)
		}
		segments[item.Segment] = bitmap
	}
	return BitmapSet{segments: segments}, nil
}

// Digest commits the normalized set contents, independently of insertion or
// map order.
func (s BitmapSet) Digest() (string, error) {
	containers, err := s.PortableContainers()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(bitmapSetDigestDomain))
	writeUint64(hash, uint64(len(containers)))
	for _, item := range containers {
		writeString(hash, item.Key.DictionaryDigest)
		writeString(hash, item.Key.SegmentID)
		writeUint16(hash, item.Key.High16)
		writeUint64(hash, item.Cardinality)
		writeString(hash, item.Digest)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func marshalCanonicalBitmap(source *roaring.Bitmap) ([]byte, error) {
	if source == nil || source.IsEmpty() {
		return nil, fmt.Errorf("%w: empty portable bitmap", ErrInvalid)
	}
	// Rebuild through sorted values to erase representation history. RunOptimize
	// then makes the choice of a run container solely content-dependent.
	normalized := roaring.New()
	buffer := make([]uint32, 0, 4096)
	flush := func() {
		if len(buffer) != 0 {
			normalized.AddMany(buffer)
			buffer = buffer[:0]
		}
	}
	source.Iterate(func(value uint32) bool {
		buffer = append(buffer, value)
		if len(buffer) == cap(buffer) {
			flush()
		}
		return true
	})
	flush()
	normalized.RunOptimize()
	if err := normalized.Validate(); err != nil {
		return nil, fmt.Errorf("%w: roaring validation: %v", ErrInvalid, err)
	}
	encoded, err := normalized.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("%w: marshal roaring bitmap: %v", ErrInvalid, err)
	}
	return encoded, nil
}

func parseCanonicalBitmap(encoded []byte) (*roaring.Bitmap, error) {
	if len(encoded) == 0 {
		return nil, fmt.Errorf("%w: empty portable bitmap", ErrInvalid)
	}
	bitmap := roaring.New()
	if err := bitmap.UnmarshalBinary(append([]byte(nil), encoded...)); err != nil {
		return nil, fmt.Errorf("%w: decode roaring bitmap: %v", ErrInvalid, err)
	}
	if err := bitmap.Validate(); err != nil {
		return nil, fmt.Errorf("%w: validate roaring bitmap: %v", ErrInvalid, err)
	}
	canonical, err := marshalCanonicalBitmap(bitmap)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, encoded) {
		return nil, ErrNonCanonical
	}
	return bitmap, nil
}

func cloneSegments(source map[SegmentRef]*roaring.Bitmap) map[SegmentRef]*roaring.Bitmap {
	if len(source) == 0 {
		return nil
	}
	result := make(map[SegmentRef]*roaring.Bitmap, len(source))
	for key, bitmap := range source {
		if bitmap != nil && !bitmap.IsEmpty() {
			result[key] = bitmap.Clone()
		}
	}
	return result
}

func sortedSegmentRefs(segments map[SegmentRef]*roaring.Bitmap) []SegmentRef {
	keys := make([]SegmentRef, 0, len(segments))
	for key, bitmap := range segments {
		if bitmap != nil && !bitmap.IsEmpty() {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].DictionaryDigest != keys[j].DictionaryDigest {
			return keys[i].DictionaryDigest < keys[j].DictionaryDigest
		}
		return keys[i].SegmentID < keys[j].SegmentID
	})
	return keys
}

func containerDigest(key ContainerKey, cardinality uint64, encoded []byte) string {
	hash := sha256.New()
	hash.Write([]byte(containerDigestDomain))
	writeString(hash, key.DictionaryDigest)
	writeString(hash, key.SegmentID)
	writeUint16(hash, key.High16)
	writeUint64(hash, cardinality)
	writeBytes(hash, encoded)
	return hex.EncodeToString(hash.Sum(nil))
}

func segmentDigest(key SegmentRef, cardinality uint64, encoded []byte) string {
	hash := sha256.New()
	hash.Write([]byte(segmentDigestDomain))
	writeString(hash, key.DictionaryDigest)
	writeString(hash, key.SegmentID)
	writeUint64(hash, cardinality)
	writeBytes(hash, encoded)
	return hex.EncodeToString(hash.Sum(nil))
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validID(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 256 && !strings.ContainsAny(value, "\x00\r\n\t")
}

type byteWriter interface{ Write([]byte) (int, error) }

func writeString(writer byteWriter, value string) { writeBytes(writer, []byte(value)) }

func writeBytes(writer byteWriter, value []byte) {
	writeUint64(writer, uint64(len(value)))
	_, _ = writer.Write(value)
}

func writeUint16(writer byteWriter, value uint16) {
	var encoded [2]byte
	binary.BigEndian.PutUint16(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeUint32(writer byteWriter, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func writeUint64(writer byteWriter, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}

func checkedAdd(left, right uint64) (uint64, error) {
	if right > math.MaxUint64-left {
		return 0, ErrMultiplicityOverflow
	}
	return left + right, nil
}
