package finalv5attack

import (
	"strings"
	"testing"
)

func TestFrozenCorpusLoadsAndCoversEveryCell(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, cell := range [][2]string{
		{"A-pagination", "complete-to-pages"}, {"A-pagination", "pages-to-complete"},
		{"B-equivalent-sql", "variants-v1"}, {"C-request-id", "same-and-different"},
		{"D-split-union", "complete-to-split"}, {"D-split-union", "split-to-complete"},
		{"E-threshold", "preregistered-v1"},
	} {
		if _, ok := manifest.Lookup(cell[0], cell[1]); !ok {
			t.Fatalf("missing attack cell %s/%s", cell[0], cell[1])
		}
	}
}

func TestFrozenCorpusBindsPhysicalAttackProductAndRealECeiling(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, attackCase := range manifest.Cases {
		for _, step := range attackCase.Steps {
			if attackCase.WorkloadID == "E-threshold" {
				if !strings.Contains(step.LogicalSQL, "FROM expense_detail") ||
					!strings.Contains(step.DirectSQL, "FROM reporting.expense_detail") {
					t.Fatalf("E escaped production expense product: %#v", step)
				}
				continue
			}
			if !strings.Contains(step.LogicalSQL, "final_v5_attack_expense_detail") ||
				!strings.Contains(step.DirectSQL, "reporting.final_v5_attack_expense_detail") {
				t.Fatalf("A--D step lacks independent physical product binding: %#v", step)
			}
		}
	}
	eCase, _ := manifest.Lookup("E-threshold", "preregistered-v1")
	if len(eCase.Steps) != 6 || len(eCase.Thresholds) != 3 || len(eCase.ThresholdResults) != 3 ||
		eCase.Steps[2].ID != "outcome-primer-320-detail" || eCase.Steps[2].Role != "outcome_primer" ||
		eCase.Steps[3].ID != "outcome-primer-320-replay" || eCase.Steps[3].Classification != "semantic_replay" ||
		eCase.Steps[4].ID != "outcome-primer-320-rewrite" || eCase.Steps[4].Classification != "semantic_replay" ||
		eCase.Steps[5].ID != "threshold-880-budget" || eCase.Steps[5].Threshold != 880 ||
		eCase.Steps[5].ExpectedErrorCode != "EXPOSURE_BUDGET_EXHAUSTED" {
		t.Fatalf("E B+1 preregistration drifted: %#v", eCase)
	}
}

func TestEquivalentSQLCorpusUsesCompilerProvenNumericCanonicalization(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	attackCase, found := manifest.Lookup("B-equivalent-sql", "variants-v1")
	if !found || len(attackCase.Steps) != 5 {
		t.Fatalf("B corpus = %#v", attackCase)
	}
	step := attackCase.Steps[3]
	if step.ID != "equal-numeric-spelling" || step.Classification != "accepted_equivalent" ||
		!strings.Contains(step.LogicalSQL, "amount = 320.00") || strings.Contains(step.LogicalSQL, " IN ") {
		t.Fatalf("B4 is not the compiler-proven typed-normal-form variant: %#v", step)
	}
}

func TestCorpusBytesAreDefensiveCopy(t *testing.T) {
	first := Bytes()
	first[0] = 'x'
	if second := Bytes(); second[0] != '{' {
		t.Fatal("caller mutated embedded corpus")
	}
}
