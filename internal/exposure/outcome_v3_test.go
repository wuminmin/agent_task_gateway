package exposure

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestOutcomeFactSeparatesPredicatesWithSameEmptyResult(t *testing.T) {
	empty := Observation{ProfileVersion: ProfileV2}
	firstPlan := sha256HexForTest("filter:amount>100")
	secondPlan := sha256HexForTest("filter:amount>200")

	first, err := AttachOutcomeV3(empty, "taskgate-query-normal-form-v3", firstPlan, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AttachOutcomeV3(empty, "taskgate-query-normal-form-v3", secondPlan, 0)
	if err != nil {
		t.Fatal(err)
	}
	firstHash, _ := first.Outcome[0].Hash()
	secondHash, _ := second.Outcome[0].Hash()
	if firstHash == secondHash {
		t.Fatal("different normalized predicates collapsed to one outcome fact")
	}
	if first.Outcome[0].OutcomeSHA256 != second.Outcome[0].OutcomeSHA256 {
		t.Fatal("the same empty released result should retain the same result digest")
	}
}

func TestOutcomeFactDeduplicatesReplayAndEquivalentRewrite(t *testing.T) {
	cell, err := NewBaseCellFactV2("orders", "snapshot-1", "order-1", "amount", "integer", int64(42))
	if err != nil {
		t.Fatal(err)
	}
	observation := Observation{ProfileVersion: ProfileV2, Release: []FactID{cell}}
	plan := sha256HexForTest("canonical-plan")
	first, err := AttachOutcomeV3(observation, "taskgate-query-normal-form-v3", plan, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AttachOutcomeV3(observation, "taskgate-query-normal-form-v3", plan, 1)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeObservations(ProfileV3, first, second)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(merged.Outcome); got != 1 {
		t.Fatalf("merged outcome facts = %d, want 1", got)
	}
}

func sha256HexForTest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
