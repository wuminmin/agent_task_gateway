package ordinal

import (
	"container/heap"
	"crypto/sha256"
	"fmt"
)

// MultiIndex is a read-only dictionary-digest router and global FactHash
// streamer for join witnesses spanning several snapshot publications.
type MultiIndex struct {
	indexes map[string]SnapshotIndex
}

func NewMultiIndex(indexes ...SnapshotIndex) (*MultiIndex, error) {
	result := &MultiIndex{indexes: make(map[string]SnapshotIndex, len(indexes))}
	for _, index := range indexes {
		if index == nil || !validDigest(index.DictionaryDigest()) {
			return nil, fmt.Errorf("%w: invalid snapshot index", ErrInvalid)
		}
		if _, duplicate := result.indexes[index.DictionaryDigest()]; duplicate {
			return nil, fmt.Errorf("%w: duplicate snapshot index", ErrInvalid)
		}
		result.indexes[index.DictionaryDigest()] = index
	}
	return result, nil
}

func (m *MultiIndex) Hash(ref FactRef) ([sha256.Size]byte, error) {
	if m == nil || m.indexes[ref.DictionaryDigest] == nil {
		return [sha256.Size]byte{}, ErrUnknownFact
	}
	return m.indexes[ref.DictionaryDigest].Hash(ref)
}

func (m *MultiIndex) ValidateSetBounds(set BitmapSet) error {
	if m == nil {
		return fmt.Errorf("%w: nil multi-index", ErrInvalid)
	}
	for _, bound := range set.SegmentBounds() {
		index := m.indexes[bound.Segment.DictionaryDigest]
		if index == nil {
			return ErrUnknownFact
		}
		count, found := index.SegmentFactCount(bound.Segment.SegmentID)
		if !found || uint64(bound.MaxOrdinal) >= count {
			return ErrUnknownFact
		}
	}
	return nil
}

// StreamHashesByFactHash k-way merges every selected segment from every
// dictionary. Each segment is ordinal-sorted by full FactHash, so memory is
// O(total segment count), not O(total fact count).
func (m *MultiIndex) StreamHashesByFactHash(set BitmapSet, yield func(FactRef, [sha256.Size]byte) error) error {
	if yield == nil {
		return fmt.Errorf("%w: nil multi-index callback", ErrInvalid)
	}
	if err := m.ValidateSetBounds(set); err != nil {
		return err
	}
	queue := hashCursorHeap{}
	for _, segment := range sortedSegmentRefs(set.segments) {
		iterator := set.segments[segment].Iterator()
		if iterator.HasNext() {
			ordinal := iterator.Next()
			ref := FactRef{DictionaryDigest: segment.DictionaryDigest, SegmentID: segment.SegmentID, Ordinal: ordinal}
			hash, err := m.Hash(ref)
			if err != nil {
				return err
			}
			heap.Push(&queue, hashCursor{segment: segment, iterator: iterator, ordinal: ordinal, hash: hash})
		}
	}
	var previousHash [sha256.Size]byte
	var previousRef FactRef
	havePrevious := false
	for queue.Len() != 0 {
		cursor := heap.Pop(&queue).(hashCursor)
		ref := FactRef{DictionaryDigest: cursor.segment.DictionaryDigest, SegmentID: cursor.segment.SegmentID, Ordinal: cursor.ordinal}
		if havePrevious && cursor.hash == previousHash && ref != previousRef {
			return fmt.Errorf("%w: two dictionaries contain the same FactID hash", ErrFactCollision)
		}
		if err := yield(ref, cursor.hash); err != nil {
			return err
		}
		previousHash, previousRef, havePrevious = cursor.hash, ref, true
		if cursor.iterator.HasNext() {
			cursor.ordinal = cursor.iterator.Next()
			ref.Ordinal = cursor.ordinal
			var err error
			cursor.hash, err = m.Hash(ref)
			if err != nil {
				return err
			}
			heap.Push(&queue, cursor)
		}
	}
	return nil
}
