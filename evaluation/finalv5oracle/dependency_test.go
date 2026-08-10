package finalv5oracle

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestExposureScaleDependencySmallFixtureHasCompleteExactSets(t *testing.T) {
	report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
		CandidateFacts: 20, ExistingFacts: 20, OverlapFacts: 10,
		SetOptions: StreamSetOptions{MaxInMemoryMembers: 8, CaptureMembers: 40, TempDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProductID != ExposureScaleProductID || report.SourceNamespace != ExposureScaleSourceNamespace ||
		report.Snapshot != ExposureScaleSnapshot || report.CandidateRows != 4 || report.ExistingRows != 4 || report.FormalScale {
		t.Fatalf("dependency binding = %+v", report)
	}
	if report.Candidate.Cardinality != 20 || report.Existing.Cardinality != 20 || report.Overlap.Cardinality != 10 ||
		report.Novel.Cardinality != 10 || report.Union.Cardinality != 30 {
		t.Fatalf("dependency algebra = candidate %d existing %d overlap %d novel %d union %d",
			report.Candidate.Cardinality, report.Existing.Cardinality, report.Overlap.Cardinality,
			report.Novel.Cardinality, report.Union.Cardinality)
	}
	witnessMembers := append([]string(nil), report.Candidate.Members...)
	sort.Strings(witnessMembers)
	witnessValues := make([]string, 0, len(witnessMembers)*2)
	for _, member := range witnessMembers {
		witnessValues = append(witnessValues, member, "00000000000000000001")
	}
	wantWitness, err := ComposeOracleCanonicalKeyV2("witness-multiset", witnessValues...)
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateWitnessCommitment != wantWitness {
		t.Fatalf("candidate witness = %s, want %s", report.CandidateWitnessCommitment, wantWitness)
	}
	for name, summary := range map[string]StreamSetSummary{
		"candidate": report.Candidate, "existing": report.Existing, "overlap": report.Overlap,
		"novel": report.Novel, "union": report.Union,
	} {
		if !summary.MembersComplete || int64(len(summary.Members)) != summary.Cardinality {
			t.Fatalf("%s did not retain its small audit fixture: %+v", name, summary)
		}
	}
	intersection := intersectTestMembers(report.Candidate.Members, report.Existing.Members)
	if !slices.Equal(intersection, report.Overlap.Members) {
		t.Fatalf("candidate/existing intersection differs from overlap:\nintersection=%v\noverlap=%v", intersection, report.Overlap.Members)
	}
	if report.Candidate.SetSHA256 != "ba1e95a0c1ad8aaca1785c172d4baa711f714112fe4fe3886ef12f43be264bc4" ||
		report.Existing.SetSHA256 != "a515a24a84acceb19bf3c0cc16eb39b0caa6e60843ee1f425c8f2180668e9997" ||
		report.Overlap.SetSHA256 != "8c6d30299ea443a54663a28c7f2186f98619ddcea2272151026ff1d2d90b5e06" ||
		report.Novel.SetSHA256 != "f63509d2051d9ec3ea7c6865c7069d18d52094f88f51561be14add4bca39addf" ||
		report.Union.SetSHA256 != "bfd4a6d9cd54c1a51c27e9c02d8cb7248c1ae403ed9fc2c8dc12af1e9e13d2cb" {
		t.Fatalf("small fixed digests candidate=%s existing=%s overlap=%s novel=%s union=%s",
			report.Candidate.SetSHA256, report.Existing.SetSHA256, report.Overlap.SetSHA256,
			report.Novel.SetSHA256, report.Union.SetSHA256)
	}
}

func TestExposureScaleSingleProductFactsUseBareFieldIDs(t *testing.T) {
	actual, err := buildExposureScaleRowFacts(0)
	if err != nil {
		t.Fatal(err)
	}
	entityKey := exposureScaleEntityKeyForTest(t, 1)
	inputs := []V2BaseCellInput{
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot, EntityKey: entityKey,
			Field: "member_rank", SQLType: "bigint", CanonicalValue: "i:1"},
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot, EntityKey: entityKey,
			Field: "metric", SQLType: "numeric", CanonicalValue: "n:113/100"},
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot, EntityKey: entityKey,
			Field: "family_id", SQLType: "integer", CanonicalValue: "i:1"},
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot, EntityKey: entityKey,
			Field: "partition_key", SQLType: "integer", CanonicalValue: "i:1"},
	}
	for index, input := range inputs {
		want, err := BuildV2BaseCellFact(input)
		if err != nil {
			t.Fatal(err)
		}
		if actual[index+1].SHA256 != want.SHA256 || !slices.Equal(actual[index+1].Payload, want.Payload) {
			t.Fatalf("fixed single-Product field %q has the wrong Fact identity", input.Field)
		}
		qualified := input
		qualified.Field = ExposureScaleStableRole + "." + input.Field
		other, err := BuildV2BaseCellFact(qualified)
		if err != nil {
			t.Fatal(err)
		}
		if other.SHA256 == actual[index+1].SHA256 {
			t.Fatalf("role-qualified field %q collapsed onto the fixed bare FieldID", qualified.Field)
		}
	}
}

func TestExposureScaleDependencyDetectsFootprintAndBindingMutations(t *testing.T) {
	members := make([]string, 0, 10)
	if err := StreamExposureScaleFacts(0, 10, func(fact CanonicalFact) error {
		members = append(members, fact.SHA256)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	baseline, err := SummarizeSemanticSet("candidate", digestSliceStream(members),
		StreamSetOptions{MaxInMemoryMembers: 16, CaptureMembers: 16})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutation := range map[string][]string{
		"missing-row-fact":       members[1:],
		"missing-predicate-cell": append(append([]string(nil), members[:4]...), members[5:]...),
	} {
		changed, err := SummarizeSemanticSet("candidate", digestSliceStream(mutation),
			StreamSetOptions{MaxInMemoryMembers: 16, CaptureMembers: 16})
		if err != nil {
			t.Fatal(err)
		}
		if changed.Cardinality != baseline.Cardinality-1 || changed.SetSHA256 == baseline.SetSHA256 {
			t.Fatalf("%s was not detected: base=%+v changed=%+v", name, baseline, changed)
		}
	}

	original, err := ExposureScaleFactAt(4) // partition_key predicate cell.
	if err != nil {
		t.Fatal(err)
	}
	mutations := []V2BaseCellInput{
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot,
			EntityKey: strings.Repeat("f", 64), Field: "partition_key", SQLType: "integer", CanonicalValue: "i:1"},
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: "final-v5-exposure-scale-2026-v2",
			EntityKey: exposureScaleEntityKeyForTest(t, 1), Field: "partition_key", SQLType: "integer", CanonicalValue: "i:1"},
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot,
			EntityKey: exposureScaleEntityKeyForTest(t, 1), Field: "family_id", SQLType: "integer", CanonicalValue: "i:1"},
		{SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot,
			EntityKey: exposureScaleEntityKeyForTest(t, 1), Field: "partition_key", SQLType: "integer", CanonicalValue: "i:2"},
	}
	for index, input := range mutations {
		changed, err := BuildV2BaseCellFact(input)
		if err != nil {
			t.Fatalf("binding mutation %d: %v", index, err)
		}
		if changed.SHA256 == original.SHA256 {
			t.Fatalf("binding mutation %d reused the original dependency fact", index)
		}
	}

	left, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{CandidateFacts: 20, ExistingFacts: 20,
		OverlapFacts: 10, SetOptions: StreamSetOptions{MaxInMemoryMembers: 8, TempDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	right, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{CandidateFacts: 20, ExistingFacts: 20,
		OverlapFacts: 15, SetOptions: StreamSetOptions{MaxInMemoryMembers: 8, TempDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if left.Overlap.Cardinality == right.Overlap.Cardinality || left.Overlap.SetSHA256 == right.Overlap.SetSHA256 ||
		left.Union.Cardinality == right.Union.Cardinality || left.Union.SetSHA256 == right.Union.SetSHA256 {
		t.Fatal("overlap interval mutation did not change exact overlap/union expectations")
	}

	reversed := append([]string(nil), members...)
	slices.Reverse(reversed)
	reordered, err := SummarizeSemanticSet("candidate", digestSliceStream(reversed),
		StreamSetOptions{MaxInMemoryMembers: 3, CaptureMembers: 16, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if reordered.SetSHA256 != baseline.SetSHA256 {
		t.Fatal("input enumeration order leaked into canonical dependency-set sorting")
	}
}

func TestExposureScaleDependencyFormalScalesAndOverlapLabels(t *testing.T) {
	for _, n := range []int64{DependencyScale10K, DependencyScale100K, DependencyScale1035000} {
		for _, percent := range []int{0, 50, 90, 100} {
			overlap, err := ExposureScaleOverlapFacts(n, percent)
			if err != nil || overlap != n*int64(percent)/100 || overlap%ExposureScaleFactsPerRow != 0 {
				t.Fatalf("N=%d/o%d overlap=%d err=%v", n, percent, overlap, err)
			}
		}
	}
	for _, percent := range []int64{0, 50, 90, 100} {
		n := DependencyScale10K
		report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
			CandidateFacts: n, ExistingFacts: n, OverlapFacts: n * percent / 100,
			SetOptions: StreamSetOptions{MaxInMemoryMembers: 1024, CaptureMembers: 4, TempDir: t.TempDir()},
		})
		if err != nil {
			t.Fatalf("10k/o%d: %v", percent, err)
		}
		if !report.FormalScale || report.Existing.Cardinality != n || report.Candidate.Cardinality != n ||
			report.Overlap.Cardinality != n*percent/100 || report.Novel.Cardinality != n*(100-percent)/100 ||
			report.Union.Cardinality != n*(200-percent)/100 || report.Stats.PeakBufferedMembers > 1024 {
			t.Fatalf("10k/o%d report = %+v", percent, report)
		}
	}

	n := DependencyScale100K
	report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
		CandidateFacts: n, ExistingFacts: n, OverlapFacts: n * 90 / 100,
		SetOptions: StreamSetOptions{MaxInMemoryMembers: 4096, CaptureMembers: 4, TempDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.FormalScale || report.Overlap.Cardinality != 90_000 || report.Novel.Cardinality != 10_000 || report.Union.Cardinality != 110_000 {
		t.Fatalf("100k/o90 report = %+v", report)
	}
}

func TestExposureScaleDependency1035000StreamsWithBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping million-member external-sort target in short mode")
	}
	const limit = 32 * 1024
	n := DependencyScale1035000
	report, err := GenerateExposureScaleDependency(ExposureScaleDependencyRequest{
		CandidateFacts: n, ExistingFacts: n, OverlapFacts: n,
		SetOptions: StreamSetOptions{MaxInMemoryMembers: limit, CaptureMembers: 8, TempDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.FormalScale || report.Candidate.Cardinality != n || report.Existing.Cardinality != n ||
		report.Overlap.Cardinality != n || report.Novel.Cardinality != 0 || report.Union.Cardinality != n {
		t.Fatalf("1,035,000/o100 algebra = %+v", report)
	}
	if report.Stats.PeakBufferedMembers > limit || report.Stats.SpillRuns == 0 ||
		report.Candidate.Stats.PeakBufferedMembers > limit || report.Candidate.MembersComplete || len(report.Candidate.SampleMembers) != 8 {
		t.Fatalf("1,035,000 bounded-memory proof = %+v candidate=%+v", report.Stats, report.Candidate)
	}
	if report.Stats.FactEmissions != n {
		t.Fatalf("o100 should sort one shared stream, emitted %d facts", report.Stats.FactEmissions)
	}
}

func TestExposureScaleDependencyRejectsNonContractShapes(t *testing.T) {
	for _, request := range []ExposureScaleDependencyRequest{
		{CandidateFacts: 9, ExistingFacts: 9, OverlapFacts: 0},
		{CandidateFacts: 10, ExistingFacts: 5, OverlapFacts: 0},
		{CandidateFacts: 10, ExistingFacts: 10, OverlapFacts: 6},
		{CandidateFacts: 10, ExistingFacts: 10, OverlapFacts: 15},
	} {
		if _, err := GenerateExposureScaleDependency(request); err == nil {
			t.Fatalf("accepted non-contract dependency shape %+v", request)
		}
	}
}

func exposureScaleEntityKeyForTest(t *testing.T, rank int64) string {
	t.Helper()
	key, err := ComposeOracleCanonicalKeyV2("base-entity", ExposureScaleSourceNamespace,
		"member_rank", "bigint", "i:"+strconv.FormatInt(rank, 10))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func intersectTestMembers(left, right []string) []string {
	rightSet := make(map[string]bool, len(right))
	for _, member := range right {
		rightSet[member] = true
	}
	result := make([]string, 0)
	for _, member := range left {
		if rightSet[member] {
			result = append(result, member)
		}
	}
	return result
}
