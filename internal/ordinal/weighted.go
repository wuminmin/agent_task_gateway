package ordinal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const weightedDigestDomain = "TASKGATE-ORDINAL-WEIGHTED-SET-V1\x00"

// WeightedSet is an immutable exact witness multiset. The million-element
// multiplicity-one case is represented only by a compressed support bitmap;
// extras stores multiplicity-1 and is allocated solely for refs above one.
type WeightedSet struct {
	support BitmapSet
	extras  map[FactRef]uint64
}

type WeightedItem struct {
	Ref          FactRef `json:"ref"`
	Multiplicity uint64  `json:"multiplicity"`
}

// WeightedBuilder is the streaming derivation accumulator. It avoids repeated
// cloning of immutable WeightedSet values across provenance rows.
type WeightedBuilder struct {
	support *Builder
	extras  map[FactRef]uint64
	err     error
}

func NewWeightedBuilder() *WeightedBuilder {
	return &WeightedBuilder{support: NewBuilder()}
}

func (b *WeightedBuilder) AddRef(ref FactRef, multiplicity uint64) error {
	if b == nil {
		return fmt.Errorf("%w: nil weighted builder", ErrInvalid)
	}
	if b.err != nil {
		return b.err
	}
	if b.support == nil {
		b.support = NewBuilder()
	}
	if multiplicity == 0 {
		b.err = fmt.Errorf("%w: weighted builder item", ErrInvalid)
		return b.err
	}
	segment := b.support.segments[ref.segment()]
	present := segment != nil && segment.Contains(ref.Ordinal)
	if !present {
		if err := b.support.Add(ref); err != nil {
			b.err = err
			return err
		}
		if multiplicity > 1 {
			if b.extras == nil {
				b.extras = make(map[FactRef]uint64)
			}
			b.extras[ref] = multiplicity - 1
		}
		return nil
	}
	current, err := checkedAdd(1, b.extras[ref])
	if err == nil {
		current, err = checkedAdd(current, multiplicity)
	}
	if err != nil {
		b.err = err
		return err
	}
	if b.extras == nil {
		b.extras = make(map[FactRef]uint64)
	}
	b.extras[ref] = current - 1
	return nil
}

func (b *WeightedBuilder) AddWeighted(value WeightedSet, scale uint64) error {
	if b == nil || scale == 0 {
		return fmt.Errorf("%w: weighted scale", ErrInvalid)
	}
	if b.err != nil {
		return b.err
	}
	if b.support == nil {
		b.support = NewBuilder()
	}
	value.Refs(func(ref FactRef, multiplicity uint64) bool {
		if multiplicity > ^uint64(0)/scale {
			b.err = ErrMultiplicityOverflow
			return false
		}
		return b.AddRef(ref, multiplicity*scale) == nil
	})
	return b.err
}

func (b *WeightedBuilder) MaxWeighted(value WeightedSet) error {
	if b == nil {
		return fmt.Errorf("%w: nil weighted builder", ErrInvalid)
	}
	if b.err != nil {
		return b.err
	}
	if b.support == nil {
		b.support = NewBuilder()
	}
	for key, source := range value.support.segments {
		target := b.support.segments[key]
		if target == nil {
			b.support.segments[key] = source.Clone()
		} else {
			target.Or(source)
		}
	}
	for ref, extra := range value.extras {
		if extra > b.extras[ref] {
			if b.extras == nil {
				b.extras = make(map[FactRef]uint64)
			}
			b.extras[ref] = extra
		}
	}
	return nil
}

func (b *WeightedBuilder) Freeze() (WeightedSet, error) {
	if b == nil || b.support == nil {
		return WeightedSet{}, fmt.Errorf("%w: nil weighted builder", ErrInvalid)
	}
	if b.err != nil {
		return WeightedSet{}, b.err
	}
	support, err := b.support.Freeze()
	if err != nil {
		return WeightedSet{}, err
	}
	extras := make(map[FactRef]uint64, len(b.extras))
	for ref, extra := range b.extras {
		extras[ref] = extra
	}
	if len(extras) == 0 {
		extras = nil
	}
	return WeightedSet{support: support, extras: extras}, nil
}

func NewWeightedSet(items ...WeightedItem) (WeightedSet, error) {
	builder := NewBuilder()
	extras := make(map[FactRef]uint64)
	seen := make(map[FactRef]struct{}, len(items))
	for _, item := range items {
		if err := item.Ref.Validate(); err != nil || item.Multiplicity == 0 {
			return WeightedSet{}, fmt.Errorf("%w: weighted item", ErrInvalid)
		}
		if _, duplicate := seen[item.Ref]; duplicate {
			return WeightedSet{}, fmt.Errorf("%w: duplicate weighted fact", ErrInvalid)
		}
		seen[item.Ref] = struct{}{}
		if err := builder.Add(item.Ref); err != nil {
			return WeightedSet{}, err
		}
		if item.Multiplicity > 1 {
			extras[item.Ref] = item.Multiplicity - 1
		}
	}
	support, err := builder.Freeze()
	if err != nil {
		return WeightedSet{}, err
	}
	if len(extras) == 0 {
		extras = nil
	}
	return WeightedSet{support: support, extras: extras}, nil
}

func WeightedFromSupport(support BitmapSet) WeightedSet {
	return WeightedSet{support: BitmapSet{segments: cloneSegments(support.segments)}}
}

func (s WeightedSet) Len() uint64 { return s.support.Cardinality() }

func (s WeightedSet) ExtraCount() int { return len(s.extras) }

func (s WeightedSet) Multiplicity(ref FactRef) uint64 {
	if !s.support.Contains(ref) {
		return 0
	}
	return 1 + s.extras[ref]
}

// Add implements witness multiplication. It does not enumerate either full
// support except where their intersection truly acquires multiplicity two.
func (s WeightedSet) Add(other WeightedSet) (WeightedSet, error) {
	result := WeightedSet{support: s.support.Union(other.support)}
	overlap := s.support.Intersection(other.support)
	if !overlap.IsEmpty() || len(s.extras) != 0 || len(other.extras) != 0 {
		result.extras = make(map[FactRef]uint64)
	}
	var addErr error
	overlap.Refs(func(ref FactRef) bool {
		left, err := checkedAdd(1, s.extras[ref])
		if err == nil {
			var right uint64
			right, err = checkedAdd(1, other.extras[ref])
			if err == nil {
				left, err = checkedAdd(left, right)
			}
		}
		if err != nil {
			addErr = err
			return false
		}
		result.extras[ref] = left - 1
		return true
	})
	if addErr != nil {
		return WeightedSet{}, addErr
	}
	for ref, extra := range s.extras {
		if !other.support.Contains(ref) {
			result.extras[ref] = extra
		}
	}
	for ref, extra := range other.extras {
		if !s.support.Contains(ref) {
			result.extras[ref] = extra
		}
	}
	if len(result.extras) == 0 {
		result.extras = nil
	}
	return result, nil
}

// Max implements alternative-proof union without enumerating either support:
// multiplicity-one defaults require no per-ref state.
func (s WeightedSet) Max(other WeightedSet) WeightedSet {
	result := WeightedSet{support: s.support.Union(other.support)}
	if len(s.extras) != 0 || len(other.extras) != 0 {
		result.extras = make(map[FactRef]uint64, len(s.extras)+len(other.extras))
	}
	for ref, extra := range s.extras {
		result.extras[ref] = extra
	}
	for ref, extra := range other.extras {
		if extra > result.extras[ref] {
			result.extras[ref] = extra
		}
	}
	return result
}

func (s WeightedSet) Support() BitmapSet {
	return BitmapSet{segments: cloneSegments(s.support.segments)}
}

// Refs visits the multiset in deterministic FactRef order without allocating
// a full refs/items slice.
func (s WeightedSet) Refs(yield func(FactRef, uint64) bool) {
	if yield == nil {
		return
	}
	s.support.Refs(func(ref FactRef) bool {
		return yield(ref, 1+s.extras[ref])
	})
}

type FactHashStreamer interface {
	StreamHashesByFactHash(BitmapSet, func(FactRef, [sha256.Size]byte) error) error
}

// StreamByFactHash is the commitment path: the HotDictionary performs a
// segment-count-sized k-way merge and multiplicity lookup touches only sparse
// extras.
func (s WeightedSet) StreamByFactHash(index FactHashStreamer, yield func(FactRef, [sha256.Size]byte, uint64) error) error {
	if index == nil || yield == nil {
		return fmt.Errorf("%w: nil fact-hash streamer or callback", ErrInvalid)
	}
	return index.StreamHashesByFactHash(s.support, func(ref FactRef, hash [sha256.Size]byte) error {
		return yield(ref, hash, 1+s.extras[ref])
	})
}

func (s WeightedSet) Digest() (string, error) {
	hash := sha256.New()
	hash.Write([]byte(weightedDigestDomain))
	writeUint64(hash, s.Len())
	var digestErr error
	s.Refs(func(ref FactRef, multiplicity uint64) bool {
		if err := ref.Validate(); err != nil || multiplicity == 0 {
			digestErr = fmt.Errorf("%w: weighted item", ErrInvalid)
			return false
		}
		writeString(hash, ref.DictionaryDigest)
		writeString(hash, ref.SegmentID)
		writeUint32(hash, ref.Ordinal)
		writeUint64(hash, multiplicity)
		return true
	})
	if digestErr != nil {
		return "", digestErr
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
