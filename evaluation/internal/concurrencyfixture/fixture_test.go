package concurrencyfixture

import (
	"reflect"
	"strings"
	"testing"
)

func TestFrozenConcurrencyFixtureIsExactAndDeterministic(t *testing.T) {
	if err := Validate(); err != nil {
		t.Fatal(err)
	}
	if len(FrozenCells) != 9 {
		t.Fatalf("cells = %d", len(FrozenCells))
	}
	for _, cell := range FrozenCells {
		got, ok := Lookup(cell.WorkloadID, cell.Scale, cell.Mode)
		if !ok || got != cell {
			t.Fatalf("lookup %+v = %+v, %v", cell, got, ok)
		}
	}
	if _, ok := Lookup("shared-root", "500", "client_barrier"); ok {
		t.Fatal("client-only concurrency mode was accepted")
	}
}

func TestRoundAndParticipantBindingsAreStableAndUnique(t *testing.T) {
	operation := RoundIdentity{
		CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		CellID: "shared-root/500/natural_contention", SampleID: "sample", Iteration: 1,
		ProcessReplicate: 1, PairID: "pair", RootGroupID: "root",
	}
	first := RoundSHA256(operation)
	second := RoundSHA256(operation)
	if first != second {
		t.Fatal("round binding is nondeterministic")
	}
	participants := ParticipantSHA256s(first, 500)
	seen := map[string]bool{}
	for _, participant := range participants {
		if seen[participant] {
			t.Fatal("participant binding repeated")
		}
		seen[participant] = true
	}
	if ParticipantSetSHA256(first, 500) == ParticipantSetSHA256(first, 499) {
		t.Fatal("participant-set digest does not bind width")
	}
}

func TestContenderResultOracleIsSourceControlled(t *testing.T) {
	if len(ExpectedContenderResultSHA256()) != 64 ||
		!reflect.DeepEqual(ExpectedContenderRows(), [][]any{{"TR-2026-0001", "机票"}}) {
		t.Fatal("contender result oracle drifted")
	}
}

func TestBoundarySQLIdentitiesAreClosedAndUnique(t *testing.T) {
	all := append(append([]string(nil), PrefixSQL...), ContenderSQL, OverflowSQL)
	seen := map[string]bool{}
	for _, sqlText := range all {
		identity := strings.ToUpper(strings.TrimSpace(sqlText))
		if seen[identity] {
			t.Fatalf("duplicate boundary SQL identity: %s", sqlText)
		}
		seen[identity] = true
	}
	if len(seen) != 5 {
		t.Fatalf("boundary identities = %d, want 5", len(seen))
	}
}

func TestFixtureValidationRejectsDuplicateBoundaryIdentity(t *testing.T) {
	original := PrefixSQL[1]
	PrefixSQL[1] = ContenderSQL
	t.Cleanup(func() { PrefixSQL[1] = original })
	if err := Validate(); err == nil {
		t.Fatal("fixture validation accepted a zero-novelty duplicate boundary identity")
	}
}
