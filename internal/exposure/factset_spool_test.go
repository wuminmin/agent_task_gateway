package exposure

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestFactSetThresholdScanPreservesExactSequenceAndDigests(t *testing.T) {
	v2 := []FactID{
		testFact(t, "threshold-v2-a", "field-a", "value-a"),
		testFact(t, "threshold-v2-b", "field-b", "value-b"),
	}
	v3 := testOutcomeFact(t, 17)
	v5a := testPredicateFact(t, 21)
	v5b := testPredicateFact(t, 22)
	inputs := []FactID{v5b, v2[0], v3, v5a, v2[1], v2[0], v5a}

	spilled, err := newFactSet(1, t.TempDir(), 128, false, inputs...)
	if err != nil {
		t.Fatal(err)
	}
	defer spilled.Close()
	memory, err := newFactSet(math.MaxInt64, t.TempDir(), 128, false, inputs...)
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	if !spilled.Spilled() || memory.Spilled() {
		t.Fatalf("threshold modes spilled=%t memory_spilled=%t", spilled.Spilled(), memory.Spilled())
	}

	spilledValues := mustFactSetValues(t, spilled)
	memoryValues := mustFactSetValues(t, memory)
	assertFactSequencesEqual(t, "threshold scan", memoryValues, spilledValues)
	assertFactSequencesEqual(t, "repeat encrypted read", spilledValues, mustFactSetValues(t, spilled))
	spilledSequenceDigest := testFactSequenceDigest(t, spilledValues)
	memorySequenceDigest := testFactSequenceDigest(t, memoryValues)
	if spilledSequenceDigest != memorySequenceDigest {
		t.Fatalf("threshold changed sequence digest: spilled=%s memory=%s", spilledSequenceDigest, memorySequenceDigest)
	}

	spilledV2, err := newFactSet(1, t.TempDir(), 64, false, v2...)
	if err != nil {
		t.Fatal(err)
	}
	defer spilledV2.Close()
	memoryV2, err := newFactSet(math.MaxInt64, t.TempDir(), 64, false, v2...)
	if err != nil {
		t.Fatal(err)
	}
	defer memoryV2.Close()
	spilledOutcomeDigest, err := ReleaseOutcomeDigest(mustFactSetValues(t, spilledV2), 2)
	if err != nil {
		t.Fatal(err)
	}
	memoryOutcomeDigest, err := ReleaseOutcomeDigest(mustFactSetValues(t, memoryV2), 2)
	if err != nil {
		t.Fatal(err)
	}
	if spilledOutcomeDigest != memoryOutcomeDigest {
		t.Fatalf("threshold changed ReleaseOutcomeDigest: spilled=%s memory=%s", spilledOutcomeDigest, memoryOutcomeDigest)
	}
	spilledAtoms := predicateAtomsForTest(spilledValues)
	memoryAtoms := predicateAtomsForTest(memoryValues)
	spilledPredicateDigest, err := PredicateSetHashV1(spilledAtoms)
	if err != nil {
		t.Fatal(err)
	}
	memoryPredicateDigest, err := PredicateSetHashV1(memoryAtoms)
	if err != nil {
		t.Fatal(err)
	}
	if spilledPredicateDigest != memoryPredicateDigest {
		t.Fatalf("predicate-set digest fixture is not order/dedup stable: %s / %s", spilledPredicateDigest, memoryPredicateDigest)
	}
	t.Logf("threshold scan invariant PASS: facts=%d sequence_sha256=%s release_outcome_sha256=%s predicate_set_sha256=%s",
		len(spilledValues), spilledSequenceDigest, spilledOutcomeDigest, spilledPredicateDigest)
}

func TestFactSetSpilloverEmptySingleDuplicateAndChunkBoundaries(t *testing.T) {
	empty, err := newFactSet(1, t.TempDir(), 128, false)
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if empty.Spilled() || empty.Len() != 0 || len(mustFactSetValues(t, empty)) != 0 {
		t.Fatal("empty FactSet did not remain an empty in-memory state")
	}

	single := testFact(t, "single", "field", "value")
	set, err := newFactSet(1, t.TempDir(), 128, false, single, single)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if !set.Spilled() || set.Len() != 1 {
		t.Fatalf("single duplicate spill state spilled=%t len=%d", set.Spilled(), set.Len())
	}
	assertFactSequencesEqual(t, "single duplicate", []FactID{single}, mustFactSetValues(t, set))

	const chunkSize = 4096
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("one_chunk%+d", delta), func(t *testing.T) {
			target := chunkSize + delta
			fact := factSetRecordSizedPredicateFact(t, target)
			record, err := encodeFactSetRecord(fact)
			if err != nil {
				t.Fatal(err)
			}
			if len(record) != target {
				t.Fatalf("record bytes=%d, want %d", len(record), target)
			}
			boundarySet, err := newFactSet(1, t.TempDir(), chunkSize, true, fact)
			if err != nil {
				t.Fatal(err)
			}
			defer boundarySet.Close()
			values := mustFactSetValues(t, boundarySet)
			assertFactSequencesEqual(t, "chunk boundary", []FactID{fact}, values)
			wantChunks := uint64(1)
			if delta > 0 {
				wantChunks = 2
			}
			if got := boundarySet.spool.ChunkCount(); got != wantChunks {
				t.Fatalf("encrypted chunks=%d, want %d", got, wantChunks)
			}
		})
	}
}

func TestFactSetCrossingThresholdMovesEveryResidentValue(t *testing.T) {
	first := testFact(t, "threshold-first", "field", strings.Repeat("a", 64))
	second := testFact(t, "threshold-second", "field", strings.Repeat("b", 64))
	firstRecord, err := encodeFactSetRecord(first)
	if err != nil {
		t.Fatal(err)
	}
	set, err := newFactSet(int64(len(firstRecord)), t.TempDir(), 128, false)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if err := set.Add(first); err != nil {
		t.Fatal(err)
	}
	if set.Spilled() || len(set.memory) != 1 {
		t.Fatalf("FactSet spilled at its exact threshold: spilled=%t resident=%d", set.Spilled(), len(set.memory))
	}
	if err := set.Add(second); err != nil {
		t.Fatal(err)
	}
	if !set.Spilled() || set.memory != nil || set.Len() != 2 {
		t.Fatalf("threshold crossing did not replace memory values: spilled=%t memory_nil=%t len=%d",
			set.Spilled(), set.memory == nil, set.Len())
	}
	oracle, err := newBufferedFactSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	assertFactSequencesEqual(t, "threshold crossing", oracle.values(), mustFactSetValues(t, set))
}

func TestFactSetCollisionGuardEstablishesBaselineBeforeMutation(t *testing.T) {
	baseline := testFact(t, "collision-a", "field", "value-a")
	baselineHash, err := baseline.HashBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := compareFactSetCollision(baselineHash, baseline, baseline); err != nil {
		t.Fatalf("unmutated collision baseline failed: %v", err)
	}
	mutated := testFact(t, "collision-b", "field", "value-b")
	if err := compareFactSetCollision(baselineHash, baseline, mutated); err == nil || !strings.Contains(err.Error(), "fact hash collision") {
		t.Fatalf("collision mutation was not rejected: %v", err)
	}
}

func TestFactSetTamperedCiphertextFailsClosed(t *testing.T) {
	base := t.TempDir()
	facts := []FactID{
		testFact(t, "tamper-a", "field", strings.Repeat("a", 200)),
		testOutcomeFact(t, 31),
		testPredicateFact(t, 32),
	}
	set, err := newFactSet(1, base, 128, true, facts...)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	baseline := mustFactSetValues(t, set)
	assertFactSequencesEqual(t, "tamper baseline", mustFactSetValues(t, set), baseline)

	path := set.spool.CiphertextPath()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	position := info.Size() - 1
	value := []byte{0}
	if _, err := file.ReadAt(value, position); err != nil {
		t.Fatal(err)
	}
	value[0] ^= 0xff
	if _, err := file.WriteAt(value, position); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := set.Values(); err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("tampered ciphertext was not rejected by authentication: %v", err)
	}
}

func TestFactSetSpillUsesPrivateModesAndAnonymousProductionLifetime(t *testing.T) {
	namedBase := t.TempDir()
	named, err := newFactSet(1, namedBase, 128, true, testFact(t, "mode", "field", "value"))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Stat(named.spool.Directory())
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(named.spool.CiphertextPath())
	if err != nil {
		t.Fatal(err)
	}
	if directory.Mode().Perm() != 0o700 || file.Mode().Perm() != 0o600 {
		t.Fatalf("FactSet spool modes directory=%o file=%o", directory.Mode().Perm(), file.Mode().Perm())
	}
	if err := named.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(namedBase)
	if err != nil || len(entries) != 0 {
		t.Fatalf("named FactSet spool cleanup entries=%d err=%v", len(entries), err)
	}

	anonymousBase := t.TempDir()
	anonymous, err := newFactSet(1, anonymousBase, 128, false, testFact(t, "anonymous", "field", "value"))
	if err != nil {
		t.Fatal(err)
	}
	defer anonymous.Close()
	entries, err = os.ReadDir(anonymousBase)
	if err != nil || len(entries) != 0 || anonymous.spool.CiphertextPath() != "" || anonymous.spool.Directory() != "" {
		t.Fatalf("production FactSet retained named ciphertext: entries=%d path=%q dir=%q err=%v",
			len(entries), anonymous.spool.CiphertextPath(), anonymous.spool.Directory(), err)
	}
	assertFactSequencesEqual(t, "anonymous spool", []FactID{testFact(t, "anonymous", "field", "value")},
		mustFactSetValues(t, anonymous))
}

func TestFactSetMemorySpillMeasurement(t *testing.T) {
	const sampleFacts = 30_000
	facts := make([]FactID, 0, sampleFacts)
	for index := 0; index < sampleFacts; index++ {
		facts = append(facts, testFact(t, fmt.Sprintf("memory-%06d", index), "payload", strings.Repeat("v", 192)))
	}

	baseline := heapAllocAfterGC()
	memory, err := newFactSet(math.MaxInt64, t.TempDir(), factSetSpoolChunkSize, false, facts...)
	if err != nil {
		t.Fatal(err)
	}
	memoryHeap := heapAllocAfterGC()
	runtime.KeepAlive(memory)
	memoryResident := positiveDifference(memoryHeap, baseline)
	if err := memory.Close(); err != nil {
		t.Fatal(err)
	}
	memory = nil
	runtime.GC()

	spillBaseline := heapAllocAfterGC()
	spilled, err := newFactSet(1, t.TempDir(), factSetSpoolChunkSize, false, facts...)
	if err != nil {
		t.Fatal(err)
	}
	spilledHeap := heapAllocAfterGC()
	runtime.KeepAlive(spilled)
	spilledResident := positiveDifference(spilledHeap, spillBaseline)
	if err := spilled.Close(); err != nil {
		t.Fatal(err)
	}
	if memoryResident == 0 || spilledResident >= memoryResident {
		t.Fatalf("FactSet resident heap did not fall: memory=%d spilled=%d", memoryResident, spilledResident)
	}
	ratio := float64(memoryResident) / float64(spilledResident)
	t.Logf("Go heap measurement (not cgroup peak): facts=%d memory_resident_bytes=%d spilled_resident_bytes=%d compression_ratio=%.2fx",
		sampleFacts, memoryResident, spilledResident, ratio)
	runtime.KeepAlive(facts)
}

func factSetRecordSizedPredicateFact(t *testing.T, target int) FactID {
	t.Helper()
	base := testPredicateFact(t, 99)
	base.CanonicalLiteral = "s:"
	record, err := encodeFactSetRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(record) > target {
		t.Fatalf("base predicate record=%d exceeds target=%d", len(record), target)
	}
	base.CanonicalLiteral += strings.Repeat("x", target-len(record))
	record, err = encodeFactSetRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(record) != target {
		t.Fatalf("sized predicate record=%d, want %d", len(record), target)
	}
	return base
}

func testFactSequenceDigest(t *testing.T, facts []FactID) string {
	t.Helper()
	digest := sha256.New()
	for _, fact := range facts {
		payload, err := fact.CanonicalPayload()
		if err != nil {
			t.Fatal(err)
		}
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write(payload)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func predicateAtomsForTest(facts []FactID) []FactID {
	result := make([]FactID, 0)
	for _, fact := range facts {
		if fact.IsV5() && fact.Kind == FactPredicateAtom {
			result = append(result, fact)
		}
	}
	return result
}

func heapAllocAfterGC() uint64 {
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return memory.HeapAlloc
}

func positiveDifference(after, before uint64) uint64 {
	if after <= before {
		return 0
	}
	return after - before
}

func TestFactSetThresholdNegativeControlMutatesAfterBaseline(t *testing.T) {
	baselineFacts := []FactID{testFact(t, "negative", "field", "value"), testOutcomeFact(t, 44)}
	spilled, err := newFactSet(1, t.TempDir(), 128, false, baselineFacts...)
	if err != nil {
		t.Fatal(err)
	}
	defer spilled.Close()
	memory, err := newFactSet(math.MaxInt64, t.TempDir(), 128, false, baselineFacts...)
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	baselineSpilled := mustFactSetValues(t, spilled)
	baselineMemory := mustFactSetValues(t, memory)
	assertFactSequencesEqual(t, "threshold negative-control baseline", baselineMemory, baselineSpilled)

	mutated := append([]FactID(nil), baselineFacts...)
	mutated[1].OutcomeRows++
	mutatedSet, err := newFactSet(1, t.TempDir(), 128, false, mutated...)
	if err != nil {
		t.Fatal(err)
	}
	defer mutatedSet.Close()
	if equal, _ := factSequencesEqual(baselineMemory, mustFactSetValues(t, mutatedSet)); equal {
		t.Fatal("threshold negative control failed: post-baseline mutation was not detected")
	}
}

func TestFactSetThresholdScanCoversAllDomainPrefixes(t *testing.T) {
	facts := []FactID{testFact(t, "domain-v2", "field", "value"), testOutcomeFact(t, 55), testPredicateFact(t, 56)}
	wantPrefixes := [][]byte{[]byte(factDomainV2), []byte(factDomainV3), []byte(factDomainV5)}
	for index, fact := range facts {
		payload, err := fact.CanonicalPayload()
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(append(append([]byte(nil), wantPrefixes[index]...), payload...))
		got, err := fact.HashBytes()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[:], want[:]) {
			t.Fatalf("domain prefix %d changed", index)
		}
	}
}
