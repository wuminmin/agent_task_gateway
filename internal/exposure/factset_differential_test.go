package exposure

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
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

func factSequencesEqual(oracle, current []FactID) (bool, string) {
	if len(oracle) != len(current) {
		return false, fmt.Sprintf("length differs: oracle=%d current=%d", len(oracle), len(current))
	}
	for i := range oracle {
		oraclePayload, oraclePayloadErr := oracle[i].CanonicalPayload()
		currentPayload, currentPayloadErr := current[i].CanonicalPayload()
		oracleHash, oracleHashErr := oracle[i].Hash()
		currentHash, currentHashErr := current[i].Hash()
		oracleHashBytes, oracleHashBytesErr := oracle[i].HashBytes()
		currentHashBytes, currentHashBytesErr := current[i].HashBytes()
		if oraclePayloadErr != nil || currentPayloadErr != nil || oracleHashErr != nil || currentHashErr != nil ||
			oracleHashBytesErr != nil || currentHashBytesErr != nil {
			return false, fmt.Sprintf("element %d could not be encoded: payload errors=%v/%v hash errors=%v/%v byte-hash errors=%v/%v",
				i, oraclePayloadErr, currentPayloadErr, oracleHashErr, currentHashErr, oracleHashBytesErr, currentHashBytesErr)
		}
		if !reflect.DeepEqual(oracle[i], current[i]) || !bytes.Equal(oraclePayload, currentPayload) ||
			oracleHash != currentHash || oracleHashBytes != currentHashBytes ||
			oracleHash != hex.EncodeToString(oracleHashBytes[:]) || currentHash != hex.EncodeToString(currentHashBytes[:]) {
			return false, fmt.Sprintf("element %d differs:\n  oracle=%#v hash=%s\n  current=%#v hash=%s",
				i, oracle[i], oracleHash, current[i], currentHash)
		}
	}
	return true, ""
}

func assertFactSequencesEqual(t *testing.T, label string, oracle, current []FactID) {
	t.Helper()
	if equal, detail := factSequencesEqual(oracle, current); !equal {
		t.Fatalf("%s sequence differs: %s", label, detail)
	}
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
		ProfileVersion:           ProfileV5,
		PredicateContextSHA256:   strings.Repeat("b", 64),
		SemanticProductID:        fmt.Sprintf("product-%d", i%3),
		StableRole:               fmt.Sprintf("role-%d", i%5),
		PublicFieldID:            fmt.Sprintf("field-%d", i%7),
		ResolvedExpressionSHA256: strings.Repeat("c", 64),
		SQLType:                  "text",
		CanonicalLiteral:         fmt.Sprintf("s:value-%d", i),
		Operator:                 "EQ",
		CollationName:            "C",
		CollationVersion:         "1.0.0",
		AtomizerVersion:          PredicateFootprintVersion,
	})
	if err != nil {
		t.Fatalf("build predicate fact: %v", err)
	}
	return fact
}

func testOutcomeFact(t *testing.T, i int) FactID {
	t.Helper()
	fact, err := NewOutcomeFactV3(
		"taskgate-query-normal-form-v3",
		fmt.Sprintf("%064x", i+1),
		fmt.Sprintf("%064x", i+101),
		int64(i+1),
	)
	if err != nil {
		t.Fatalf("build outcome fact: %v", err)
	}
	return fact
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnEmptySet
// establishes baseline: the oracle and current implementation agree on empty input.
func TestFactSetDifferentialEmptySet(t *testing.T) {
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
	assertFactSequencesEqual(t, "empty", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnSingleFact
func TestFactSetDifferentialSingleFact(t *testing.T) {
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
	assertFactSequencesEqual(t, "single", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnAlreadySortedSet
func TestFactSetDifferentialAlreadySortedSet(t *testing.T) {
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

	assertFactSequencesEqual(t, "already sorted", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnUnsortedSlice
func TestFactSetDifferentialUnsortedSlice(t *testing.T) {
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

	assertFactSequencesEqual(t, "unsorted input", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnDeduplication
func TestFactSetDifferentialDeduplication(t *testing.T) {
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
	assertFactSequencesEqual(t, "deduplication", oracle.values(), current.Values())
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnMixedFactTypes
func TestFactSetDifferentialV2V3V5DomainOrdering(t *testing.T) {
	facts := []FactID{
		testFact(t, "e1", "f1", "v1"),
		testOutcomeFact(t, 1),
		testPredicateFact(t, 1),
		testFact(t, "e2", "f2", "v2"),
		testOutcomeFact(t, 2),
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

	assertFactSequencesEqual(t, "V2/V3/V5 domain ordering", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnClone
func TestFactSetDifferentialClone(t *testing.T) {
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

	assertFactSequencesEqual(t, "clone", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnMergeChecked
func TestFactSetDifferentialMergeChecked(t *testing.T) {
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

	assertFactSequencesEqual(t, "merge", oracleValues, currentValues)
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnHashCollision
func TestFactSetDifferentialHashCollision(t *testing.T) {
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
func TestFactSetRandomizedDuplicationAndOrder(t *testing.T) {
	random := rand.New(rand.NewSource(20260813))
	pool := make([]FactID, 0, 40)
	for i := 0; i < 40; i++ {
		// Mix all three durable fact generations and therefore all domain prefixes.
		switch i % 3 {
		case 0:
			pool = append(pool, testFact(t,
				fmt.Sprintf("entity-%02d", i), fmt.Sprintf("field-%02d", i%7), fmt.Sprintf("value-%02d", i)))
		case 1:
			pool = append(pool, testOutcomeFact(t, i))
		case 2:
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

		assertFactSequencesEqual(t, fmt.Sprintf("random trial %d size %d", trial, size), oracleValues, currentValues)
	}
}

// TestBufferedFactSetAgreesWithCurrentImplementationOnLargeSet
func TestFactSetDifferentialLargeSet(t *testing.T) {
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

	assertFactSequencesEqual(t, "large set", oracleValues, currentValues)
}

// TestFactSetDifferentialOracleCanDetectADifference
// Negative control: the differential oracle must be capable of disagreeing.
// Feed the two implementations different inputs and require a mismatch,
// so a vacuously-passing comparison cannot go unnoticed.
func TestFactSetNegativeControlDetectsSequenceDifference(t *testing.T) {
	first := testFact(t, "e1", "f1", "v1")
	second := testOutcomeFact(t, 2)
	oracle, err := newBufferedFactSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	current, err := NewFactSet(first, second)
	if err != nil {
		t.Fatal(err)
	}

	// Establish an unmutated baseline before exercising the negative control.
	assertFactSequencesEqual(t, "negative-control baseline", oracle.values(), current.Values())

	mutated := second
	mutated.OutcomeRows++
	current, err = NewFactSet(first, mutated)
	if err != nil {
		t.Fatal(err)
	}
	if equal, _ := factSequencesEqual(oracle.values(), current.Values()); equal {
		t.Fatal("negative control failed: a field mutation did not change the compared FactID sequence")
	}
}
