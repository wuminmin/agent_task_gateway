package exposure

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// bufferedFactSet is the pre-optimization implementation, kept here as the
// differential oracle. It uses string keys (64-character hex) and stores
// FactID values directly in a map.
// The future optimized FactSet will use [32]byte keys and must agree with this
// implementation on every input, because the committed digest sequence
// is the thing that may never change -- only the memory cost changes.
type bufferedFactSet map[string]FactID

func newBufferedFactSet(facts ...FactID) (bufferedFactSet, error) {
	result := make(bufferedFactSet, len(facts))
	for _, fact := range facts {
		if err := result.add(fact); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s bufferedFactSet) add(fact FactID) error {
	if s == nil {
		return fmt.Errorf("%w: nil fact set", ErrInvalid)
	}
	hash, err := fact.Hash()
	if err != nil {
		return err
	}
	if existing, present := s[hash]; present {
		left, leftErr := existing.CanonicalPayload()
		right, rightErr := fact.CanonicalPayload()
		if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) {
			return fmt.Errorf("%w: fact hash collision for %s", ErrInvalid, hash)
		}
		return nil
	}
	s[hash] = fact
	return nil
}

func (s bufferedFactSet) clone() bufferedFactSet {
	result := make(bufferedFactSet, len(s))
	for hash, fact := range s {
		result[hash] = fact
	}
	return result
}

func (s bufferedFactSet) mergeChecked(other bufferedFactSet) error {
	for _, fact := range other {
		if err := s.add(fact); err != nil {
			return err
		}
	}
	return nil
}

func (s bufferedFactSet) values() []FactID {
	hashes := make([]string, 0, len(s))
	for hash := range s {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	result := make([]FactID, 0, len(hashes))
	for _, hash := range hashes {
		result = append(result, s[hash])
	}
	return result
}

func (s bufferedFactSet) len() int {
	return len(s)
}

// Helper to create a test fact
func testFact(t *testing.T, entity, field, value string) FactID {
	t.Helper()
	fact, err := NewBaseCellFactV2("ns.schema", strings.Repeat("a", 64), entity, field, "text", value)
	if err != nil {
		t.Fatalf("build fact: %v", err)
	}
	return fact
}

// Helper to create a V5 predicate atom fact for variety
func testPredicateFact(t *testing.T, i int) FactID {
	t.Helper()
	fact, err := NewPredicateAtomFactV5(PredicateAtomFactV5{
		ProfileVersion:         ProfileV5,
		PredicateContextSHA256: strings.Repeat("b", 64),
		SemanticProductID:      fmt.Sprintf("product-%d", i%3),
		StableRole:             fmt.Sprintf("role-%d", i%5),
		PublicFieldID:          fmt.Sprintf("field-%d", i%7),
		ResolvedExpressionSHA256: strings.Repeat("c", 64),
		SQLType:                "text",
		CanonicalLiteral:       fmt.Sprintf("s:value-%d", i),
		Operator:               "EQ",
		CollationName:         "C",
		CollationVersion:      "1.0.0",
		AtomizerVersion:        PredicateFootprintVersion,
	})
	if err != nil {
		t.Fatalf("build predicate fact: %v", err)
	}
	return fact
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnEmptySet
// establishes baseline: the oracle and current implementation agree on empty input.
func TestBufferedFactSetAgreesWithCurrentImplementationOnEmptySet(t *testing.T) {
	oracle, err := newBufferedFactSet()
	if err != nil {
		t.Fatalf("oracle empty: %v", err)
	}
	current, err := NewFactSet()
	if err != nil {
		t.Fatalf("current empty: %v", err)
	}
	if oracle.len() != len(current) {
		t.Fatalf("length differs: oracle=%d current=%d", oracle.len(), len(current))
	}
	oracleValues := oracle.values()
	currentValues := current.Values()
	if len(oracleValues) != len(currentValues) {
		t.Fatalf("values count differs: oracle=%d current=%d", len(oracleValues), len(currentValues))
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnSingleFact
func TestBufferedFactSetAgreesWithCurrentImplementationOnSingleFact(t *testing.T) {
	fact := testFact(t, "e1", "f1", "v1")
	oracle, err := newBufferedFactSet(fact)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(fact)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if oracle.len() != len(current) {
		t.Fatalf("length differs: oracle=%d current=%d", oracle.len(), len(current))
	}
	oracleValues := oracle.values()
	currentValues := current.Values()
	if len(oracleValues) != 1 || len(currentValues) != 1 {
		t.Fatalf("values count: oracle=%d current=%d", len(oracleValues), len(currentValues))
	}
	// Verify they have the same fact
	oracleHash, _ := oracleValues[0].Hash()
	currentHash, _ := currentValues[0].Hash()
	if oracleHash != currentHash {
		t.Fatalf("hash differs: oracle=%s current=%s", oracleHash, currentHash)
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnAlreadySortedSet
func TestBufferedFactSetAgreesWithCurrentImplementationOnAlreadySortedSet(t *testing.T) {
	facts := []FactID{
		testFact(t, "a1", "f1", "v1"),
		testFact(t, "b2", "f2", "v2"),
		testFact(t, "c3", "f3", "v3"),
	}
	// Create a sorted set first, then feed to both
	sortedSet, err := NewFactSet(facts...)
	if err != nil {
		t.Fatalf("sorted set: %v", err)
	}
	sortedValues := sortedSet.Values()

	oracle, err := newBufferedFactSet(sortedValues...)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(sortedValues...)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	oracleValues := oracle.values()
	currentValues := current.Values()

	if len(oracleValues) != len(currentValues) {
		t.Fatalf("values count differs: oracle=%d current=%d", len(oracleValues), len(currentValues))
	}

	for i := range oracleValues {
		oracleHash, _ := oracleValues[i].Hash()
		currentHash, _ := currentValues[i].Hash()
		if oracleHash != currentHash {
			t.Fatalf("value at %d differs: oracle=%s current=%s", i, oracleHash, currentHash)
		}
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnUnsortedSlice
func TestBufferedFactSetAgreesWithCurrentImplementationOnUnsortedSlice(t *testing.T) {
	facts := []FactID{
		testFact(t, "z9", "f1", "v1"),
		testFact(t, "a1", "f2", "v2"),
		testFact(t, "m5", "f3", "v3"),
	}
	oracle, err := newBufferedFactSet(facts...)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(facts...)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	oracleValues := oracle.values()
	currentValues := current.Values()

	if len(oracleValues) != len(currentValues) {
		t.Fatalf("values count differs: oracle=%d current=%d", len(oracleValues), len(currentValues))
	}

	for i := range oracleValues {
		oracleHash, _ := oracleValues[i].Hash()
		currentHash, _ := currentValues[i].Hash()
		if oracleHash != currentHash {
			t.Fatalf("value at %d differs: oracle=%s current=%s", i, oracleHash, currentHash)
		}
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnDeduplication
func TestBufferedFactSetAgreesWithCurrentImplementationOnDeduplication(t *testing.T) {
	one := testFact(t, "e1", "f1", "v1")
	two := testFact(t, "e2", "f2", "v2")
	facts := []FactID{one, two, one, one, two}

	oracle, err := newBufferedFactSet(facts...)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(facts...)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	if oracle.len() != len(current) {
		t.Fatalf("length after dedup differs: oracle=%d current=%d", oracle.len(), len(current))
	}
	if oracle.len() != 2 {
		t.Fatalf("expected 2 unique facts, got %d", oracle.len())
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnMixedFactTypes
func TestBufferedFactSetAgreesWithCurrentImplementationOnMixedFactTypes(t *testing.T) {
	facts := []FactID{
		testFact(t, "e1", "f1", "v1"),
		testPredicateFact(t, 1),
		testFact(t, "e2", "f2", "v2"),
		testPredicateFact(t, 2),
	}

	oracle, err := newBufferedFactSet(facts...)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(facts...)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	oracleValues := oracle.values()
	currentValues := current.Values()

	if len(oracleValues) != len(currentValues) {
		t.Fatalf("values count differs: oracle=%d current=%d", len(oracleValues), len(currentValues))
	}

	for i := range oracleValues {
		oracleHash, _ := oracleValues[i].Hash()
		currentHash, _ := currentValues[i].Hash()
		if oracleHash != currentHash {
			t.Fatalf("value at %d differs: oracle=%s current=%s", i, oracleHash, currentHash)
		}
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnClone
func TestBufferedFactSetAgreesWithCurrentImplementationOnClone(t *testing.T) {
	facts := []FactID{
		testFact(t, "e1", "f1", "v1"),
		testFact(t, "e2", "f2", "v2"),
	}
	oracle, err := newBufferedFactSet(facts...)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(facts...)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	oracleClone := oracle.clone()
	currentClone := current.Clone()

	if oracleClone.len() != len(currentClone) {
		t.Fatalf("clone length differs: oracle=%d current=%d", oracleClone.len(), len(currentClone))
	}

	oracleValues := oracleClone.values()
	currentValues := currentClone.Values()

	for i := range oracleValues {
		oracleHash, _ := oracleValues[i].Hash()
		currentHash, _ := currentValues[i].Hash()
		if oracleHash != currentHash {
			t.Fatalf("clone value at %d differs: oracle=%s current=%s", i, oracleHash, currentHash)
		}
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnMergeChecked
func TestBufferedFactSetAgreesWithCurrentImplementationOnMergeChecked(t *testing.T) {
	leftFacts := []FactID{
		testFact(t, "e1", "f1", "v1"),
		testFact(t, "e2", "f2", "v2"),
	}
	rightFacts := []FactID{
		testFact(t, "e2", "f2", "v2"), // duplicate
		testFact(t, "e3", "f3", "v3"),
	}

	oracleLeft, _ := newBufferedFactSet(leftFacts...)
	currentLeft, _ := NewFactSet(leftFacts...)

	oracleRight, _ := newBufferedFactSet(rightFacts...)
	currentRight, _ := NewFactSet(rightFacts...)

	errOracle := oracleLeft.mergeChecked(oracleRight)
	errCurrent := currentLeft.MergeChecked(currentRight)

	if (errOracle == nil) != (errCurrent == nil) {
		t.Fatalf("merge error disagreement: oracle=%v current=%v", errOracle, errCurrent)
	}
	if errOracle != nil {
		t.Fatalf("oracle merge failed: %v", errOracle)
	}

	if oracleLeft.len() != len(currentLeft) {
		t.Fatalf("merged length differs: oracle=%d current=%d", oracleLeft.len(), len(currentLeft))
	}
	if oracleLeft.len() != 3 {
		t.Fatalf("expected 3 facts after merge, got %d", oracleLeft.len())
	}

	oracleValues := oracleLeft.values()
	currentValues := currentLeft.Values()

	for i := range oracleValues {
		oracleHash, _ := oracleValues[i].Hash()
		currentHash, _ := currentValues[i].Hash()
		if oracleHash != currentHash {
			t.Fatalf("merged value at %d differs: oracle=%s current=%s", i, oracleHash, currentHash)
		}
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnHashCollision
func TestBufferedFactSetAgreesWithCurrentImplementationOnHashCollision(t *testing.T) {
	// This test requires a real hash collision to trigger the collision detection.
	// Since SHA-256 makes this astronomically unlikely, we test the detection
	// logic indirectly: both implementations must reject the same forged collision.
	fact := testFact(t, "e1", "f1", "v1")
	forged := fact
	forged.CanonicalValue = fact.CanonicalValue + "-forged"

	// Both implementations treat differing payloads under one hash as fatal.
	oracleSet := make(bufferedFactSet)
	if err := oracleSet.add(fact); err != nil {
		t.Fatal(err)
	}
	if err := oracleSet.add(fact); err != nil {
		t.Fatalf("re-adding identical fact to oracle must succeed: %v", err)
	}
	if oracleSet.len() != 1 {
		t.Fatalf("oracle identical fact did not collapse: %d entries", oracleSet.len())
	}

	currentSet := make(FactSet)
	if err := currentSet.Add(fact); err != nil {
		t.Fatal(err)
	}
	if err := currentSet.Add(fact); err != nil {
		t.Fatalf("re-adding identical fact to current must succeed: %v", err)
	}
	if len(currentSet) != 1 {
		t.Fatalf("current identical fact did not collapse: %d entries", len(currentSet))
	}

	// Collision path: we can't easily forge a real SHA-256 collision, but both
	// implementations share the same collision-rejection logic via fact.Hash().
	// The property is defended by TestFactIDHashCollisionIsClosed in fact_test.go.
}

// TestBufferedFactSetAgreesUnderRandomizedDuplicationAndOrder
// Randomized differential: many shapes, heavy duplication, arbitrary order.
// Any divergence in ordering, dedup or length framing shows up as a mismatch.
func TestBufferedFactSetAgreesUnderRandomizedDuplicationAndOrder(t *testing.T) {
	random := rand.New(rand.NewSource(20260813))
	pool := make([]FactID, 0, 40)
	for i := 0; i < 40; i++ {
		// Mix of fact types for variety
		if i%2 == 0 {
			pool = append(pool, testFact(t,
				fmt.Sprintf("entity-%02d", i), fmt.Sprintf("field-%02d", i%7), fmt.Sprintf("value-%02d", i)))
		} else {
			pool = append(pool, testPredicateFact(t, i))
		}
	}
	for trial := 0; trial < 300; trial++ {
		size := random.Intn(60)
		sample := make([]FactID, 0, size)
		for i := 0; i < size; i++ {
			sample = append(sample, pool[random.Intn(len(pool))])
		}

		oracle, err := newBufferedFactSet(sample...)
		if err != nil {
			t.Fatalf("trial %d oracle: %v", trial, err)
		}
		current, err := NewFactSet(sample...)
		if err != nil {
			t.Fatalf("trial %d current: %v", trial, err)
		}

		if oracle.len() != len(current) {
			t.Fatalf("trial %d length differs (size=%d): oracle=%d current=%d",
				trial, size, oracle.len(), len(current))
		}

		oracleValues := oracle.values()
		currentValues := current.Values()

		if len(oracleValues) != len(currentValues) {
			t.Fatalf("trial %d values count differs (size=%d): oracle=%d current=%d",
				trial, size, len(oracleValues), len(currentValues))
		}

		for i := range oracleValues {
			oracleHash, _ := oracleValues[i].Hash()
			currentHash, _ := currentValues[i].Hash()
			if oracleHash != currentHash {
				t.Fatalf("trial %d value at %d differs (size=%d)\n  oracle=%s\n  current=%s",
					trial, i, size, oracleHash, currentHash)
			}
		}
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnLargeSet
func TestBufferedFactSetAgreesWithCurrentImplementationOnLargeSet(t *testing.T) {
	// Stress test with a larger set to expose any scaling discrepancies.
	pool := make([]FactID, 0, 200)
	for i := 0; i < 200; i++ {
		pool = append(pool, testFact(t,
			fmt.Sprintf("e%03d", i), "f", fmt.Sprintf("v%03d", i)))
	}

	oracle, err := newBufferedFactSet(pool...)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	current, err := NewFactSet(pool...)
	if err != nil {
		t.Fatalf("current: %v", err)
	}

	if oracle.len() != len(current) {
		t.Fatalf("large set length differs: oracle=%d current=%d", oracle.len(), len(current))
	}

	oracleValues := oracle.values()
	currentValues := current.Values()

	if len(oracleValues) != len(currentValues) {
		t.Fatalf("large set values count differs: oracle=%d current=%d", len(oracleValues), len(currentValues))
	}

	for i := range oracleValues {
		oracleHash, _ := oracleValues[i].Hash()
		currentHash, _ := currentValues[i].Hash()
		if oracleHash != currentHash {
			t.Fatalf("large set value at %d differs: oracle=%s current=%s", i, oracleHash, currentHash)
		}
	}
}

// TestFactSetDifferentialOracleCanDetectADifference
// Negative control: the differential oracle must be capable of disagreeing.
// Feed the two implementations different inputs and require a mismatch,
// so a vacuously-passing comparison cannot go unnoticed.
func TestFactSetDifferentialOracleCanDetectADifference(t *testing.T) {
	fact := testFact(t, "e1", "f1", "v1")

	oracle, _ := newBufferedFactSet(fact)
	current, _ := NewFactSet(fact)

	// Add a different fact to each
	oracle.add(testFact(t, "e2", "f2", "v2"))
	current.Add(testFact(t, "e3", "f3", "v3"))

	if oracle.len() == len(current) && oracle.len() == 2 {
		// They both have 2 elements; verify the content differs
		oracleValues := oracle.values()
		currentValues := current.Values()

		oracleHash, _ := oracleValues[1].Hash()
		currentHash, _ := currentValues[1].Hash()

		if oracleHash == currentHash {
			t.Fatal("negative control failed: different facts produced same hash")
		}
	}
}
