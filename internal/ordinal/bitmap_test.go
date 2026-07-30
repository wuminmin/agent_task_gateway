package ordinal

import (
	"errors"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"
)

func testDigest(character string) string { return strings.Repeat(character, 64) }

func testRef(dictionary, segment string, ordinal uint32) FactRef {
	return FactRef{DictionaryDigest: testDigest(dictionary), SegmentID: segment, Ordinal: ordinal}
}

func TestBitmapSetExactAlgebraIsImmutable(t *testing.T) {
	left, err := NewBitmapSet(testRef("a", "row", 1), testRef("a", "row", 2), testRef("a", "cell:amount", 1))
	if err != nil {
		t.Fatalf("NewBitmapSet(left): %v", err)
	}
	right, err := NewBitmapSet(testRef("a", "row", 2), testRef("a", "row", 3), testRef("a", "cell:amount", 1))
	if err != nil {
		t.Fatalf("NewBitmapSet(right): %v", err)
	}

	union := left.Union(right)
	difference := left.Difference(right)
	intersection := left.Intersection(right)
	if got := union.Cardinality(); got != 4 {
		t.Fatalf("union cardinality = %d, want 4", got)
	}
	if got := difference.Cardinality(); got != 1 || !difference.Contains(testRef("a", "row", 1)) {
		t.Fatalf("difference = %#v, cardinality %d", difference.SegmentBounds(), got)
	}
	if got := intersection.Cardinality(); got != 2 {
		t.Fatalf("intersection cardinality = %d, want 2", got)
	}
	if left.Cardinality() != 3 || right.Cardinality() != 3 {
		t.Fatal("set algebra mutated an operand")
	}
	if !union.Difference(left).Contains(testRef("a", "row", 3)) {
		t.Fatal("(left OR right) ANDNOT left lost the novel ref")
	}
	if fromEmpty := (BitmapSet{}).Union(left); !fromEmpty.Equal(left) {
		t.Fatal("empty OR nonempty differs from the nonempty operand")
	}
}

func TestPortableContainersRoundTripAndValidateHigh16(t *testing.T) {
	original, err := NewBitmapSet(
		testRef("b", "row", 1),
		testRef("b", "row", 1<<16),
		testRef("b", "row", (2<<16)+7),
		testRef("b", "cell:value", 9),
	)
	if err != nil {
		t.Fatalf("NewBitmapSet: %v", err)
	}
	containers, err := original.PortableContainers()
	if err != nil {
		t.Fatalf("PortableContainers: %v", err)
	}
	if len(containers) != 4 {
		t.Fatalf("portable container count = %d, want 4", len(containers))
	}
	// Persistence order is irrelevant; Parse canonicalizes by storage key.
	for left, right := 0, len(containers)-1; left < right; left, right = left+1, right-1 {
		containers[left], containers[right] = containers[right], containers[left]
	}
	roundTrip, err := ParsePortableContainers(containers)
	if err != nil {
		t.Fatalf("ParsePortableContainers: %v", err)
	}
	if !roundTrip.Equal(original) {
		t.Fatal("portable container round trip changed the exact set")
	}
	leftDigest, _ := original.Digest()
	rightDigest, _ := roundTrip.Digest()
	if leftDigest != rightDigest {
		t.Fatalf("content digest changed across persistence order: %s != %s", leftDigest, rightDigest)
	}

	tampered := append([]PortableContainer(nil), containers...)
	tampered[0].Key.High16++
	if _, err := ParsePortableContainers(tampered); err == nil {
		t.Fatal("tampered high16 container was accepted")
	}
	tampered = append([]PortableContainer(nil), containers...)
	tampered[0].Digest = testDigest("f")
	if _, err := ParsePortableContainers(tampered); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("tampered digest error = %v, want ErrDigestMismatch", err)
	}

	bounds := original.SegmentBounds()
	if len(bounds) != 2 || bounds[1].MaxOrdinal != (2<<16)+7 {
		t.Fatalf("unexpected segment bounds: %#v", bounds)
	}
}

func TestPortableSegmentRejectsNonCanonicalRoaringHistory(t *testing.T) {
	bitmap := roaring.New()
	for value := uint32(0); value < 100; value++ {
		bitmap.Add(value)
	}
	encoded, err := bitmap.MarshalBinary() // deliberately not RunOptimize'd
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}
	segment := SegmentRef{DictionaryDigest: testDigest("c"), SegmentID: "row"}
	item := PortableSegment{Segment: segment, Bitmap: encoded, Cardinality: 100}
	item.Digest = segmentDigest(segment, item.Cardinality, item.Bitmap)
	if _, err := ParsePortableSegments([]PortableSegment{item}); !errors.Is(err, ErrNonCanonical) {
		t.Fatalf("non-canonical portable error = %v, want ErrNonCanonical", err)
	}
}
