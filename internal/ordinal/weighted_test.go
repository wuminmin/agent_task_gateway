package ordinal

import (
	"errors"
	"math"
	"testing"
)

func TestWeightedMillionSingletonsUseBitmapWithoutPerFactExtras(t *testing.T) {
	const factCount = 1_035_000
	builder := NewBuilder()
	for ordinal := uint32(0); ordinal < factCount; ordinal++ {
		if err := builder.Add(testRef("d", "cell:value", ordinal)); err != nil {
			t.Fatalf("Add(%d): %v", ordinal, err)
		}
	}
	support, err := builder.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	weighted := WeightedFromSupport(support)
	if weighted.Len() != factCount || weighted.ExtraCount() != 0 || weighted.extras != nil {
		t.Fatalf("weighted singleton representation len=%d extras=%d/%#v", weighted.Len(), weighted.ExtraCount(), weighted.extras)
	}
	if weighted.Multiplicity(testRef("d", "cell:value", factCount-1)) != 1 {
		t.Fatal("millionth singleton lost its multiplicity")
	}
	containers, err := weighted.Support().PortableContainers()
	if err != nil || len(containers) != 16 {
		t.Fatalf("million-fact support containers=%d, err=%v, want 16", len(containers), err)
	}
}

func TestWeightedAddMaxAndOverflowAreExact(t *testing.T) {
	ref := testRef("e", "row", 7)
	left, _ := NewWeightedSet(WeightedItem{Ref: ref, Multiplicity: 3})
	right, _ := NewWeightedSet(WeightedItem{Ref: ref, Multiplicity: 4})
	sum, err := left.Add(right)
	if err != nil || sum.Multiplicity(ref) != 7 || sum.ExtraCount() != 1 {
		t.Fatalf("weighted Add multiplicity=%d extras=%d err=%v, want 7/1", sum.Multiplicity(ref), sum.ExtraCount(), err)
	}
	maximum := left.Max(right)
	if maximum.Multiplicity(ref) != 4 {
		t.Fatalf("weighted Max multiplicity=%d, want 4", maximum.Multiplicity(ref))
	}
	maxValue, _ := NewWeightedSet(WeightedItem{Ref: ref, Multiplicity: math.MaxUint64})
	one, _ := NewWeightedSet(WeightedItem{Ref: ref, Multiplicity: 1})
	if _, err := maxValue.Add(one); !errors.Is(err, ErrMultiplicityOverflow) {
		t.Fatalf("overflow error = %v, want ErrMultiplicityOverflow", err)
	}
}

func TestWeightedFactHashStreamCarriesSparseMultiplicity(t *testing.T) {
	artifact, err := CompileSnapshotArtifact(snapshotTestSpec("expense.amount", false))
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	rowA, _ := artifact.Hot.LookupEntity("row-a")
	rowB, _ := artifact.Hot.LookupEntity("row-b")
	weighted, err := NewWeightedSet(
		WeightedItem{Ref: rowA.Row, Multiplicity: 1},
		WeightedItem{Ref: rowA.Cells["expense.amount"], Multiplicity: 5},
		WeightedItem{Ref: rowB.Row, Multiplicity: 1},
	)
	if err != nil {
		t.Fatalf("NewWeightedSet: %v", err)
	}
	if weighted.ExtraCount() != 1 {
		t.Fatalf("extras = %d, want 1", weighted.ExtraCount())
	}
	var previous [32]byte
	count := 0
	err = weighted.StreamByFactHash(artifact.Hot, func(ref FactRef, hash [32]byte, multiplicity uint64) error {
		if count > 0 && string(previous[:]) >= string(hash[:]) {
			t.Fatalf("fact-hash stream is not strictly sorted")
		}
		if ref == rowA.Cells["expense.amount"] && multiplicity != 5 {
			t.Fatalf("sparse multiplicity = %d, want 5", multiplicity)
		}
		previous = hash
		count++
		return nil
	})
	if err != nil || count != 3 {
		t.Fatalf("StreamByFactHash count=%d err=%v", count, err)
	}
}

func TestWeightedBuilderAccumulatesWithoutImmutableCloneLoop(t *testing.T) {
	first := testRef("f", "row", 1)
	second := testRef("f", "row", 2)
	builder := NewWeightedBuilder()
	if err := builder.AddRef(first, 1); err != nil {
		t.Fatalf("AddRef first: %v", err)
	}
	if err := builder.AddRef(first, 2); err != nil {
		t.Fatalf("AddRef repeated: %v", err)
	}
	input, _ := NewWeightedSet(WeightedItem{Ref: second, Multiplicity: 2})
	if err := builder.AddWeighted(input, 3); err != nil {
		t.Fatalf("AddWeighted: %v", err)
	}
	result, err := builder.Freeze()
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if result.Multiplicity(first) != 3 || result.Multiplicity(second) != 6 || result.ExtraCount() != 2 {
		t.Fatalf("weighted builder result first=%d second=%d extras=%d", result.Multiplicity(first), result.Multiplicity(second), result.ExtraCount())
	}
	maximumBuilder := NewWeightedBuilder()
	_ = maximumBuilder.AddRef(first, 2)
	if err := maximumBuilder.MaxWeighted(result); err != nil {
		t.Fatalf("MaxWeighted: %v", err)
	}
	maximum, _ := maximumBuilder.Freeze()
	if maximum.Multiplicity(first) != 3 || maximum.Multiplicity(second) != 6 {
		t.Fatalf("weighted builder max first=%d second=%d", maximum.Multiplicity(first), maximum.Multiplicity(second))
	}
}

func TestZeroValueBuildersAreSafe(t *testing.T) {
	ref := testRef("1", "row", 1)
	var bitmap Builder
	if err := bitmap.Add(ref); err != nil {
		t.Fatalf("zero Bitmap Builder Add: %v", err)
	}
	if set, err := bitmap.Freeze(); err != nil || !set.Contains(ref) {
		t.Fatalf("zero Bitmap Builder Freeze set=%#v err=%v", set.SegmentBounds(), err)
	}
	var weighted WeightedBuilder
	if err := weighted.AddRef(ref, 2); err != nil {
		t.Fatalf("zero WeightedBuilder AddRef: %v", err)
	}
	if set, err := weighted.Freeze(); err != nil || set.Multiplicity(ref) != 2 {
		t.Fatalf("zero WeightedBuilder Freeze multiplicity=%d err=%v", set.Multiplicity(ref), err)
	}
}
