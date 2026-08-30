package finalv5footprint

import (
	"math/big"
	"strings"
	"testing"
)

func TestEmbeddedCorpusIsExactlyTheRecomputedBuild(t *testing.T) {
	if err := VerifyAgainstBuild(); err != nil {
		t.Fatal(err)
	}
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Rungs) != 12 {
		t.Fatalf("rungs = %d", len(manifest.Rungs))
	}
	accepted := 0
	for _, rung := range manifest.Rungs {
		if !rung.BoundedRefused {
			accepted++
			if rung.Rows != 100 || len(rung.Columns) != 1 {
				t.Fatalf("unexpected accepted rung %#v", rung)
			}
		}
		if !strings.Contains(rung.DirectSQL, "category IN ('alpha', 'beta', 'gamma', 'delta')") ||
			!strings.Contains(rung.DirectSQL, "row_id <= ") {
			t.Fatalf("rung %d SQL lacks the frozen scope shape: %s", rung.Index, rung.DirectSQL)
		}
		if rung.LogicalSQL(BoundedProduct) == rung.DirectSQL {
			t.Fatalf("rung %d logical SQL did not substitute the product", rung.Index)
		}
	}
	if accepted != 1 {
		t.Fatalf("bounded arm accepts %d rungs, the frozen design accepts exactly one", accepted)
	}
}

// The dependency arithmetic must match the declared rule: rows * (row fact +
// row_id cell + category cell + argument cells), with every fact distinct.
func TestDependencyCardinalityFollowsTheDeclaredRule(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, rung := range manifest.Rungs {
		want := rung.Rows * int64(3+len(rung.Columns))
		if rung.Dependency.Cardinality != want {
			t.Fatalf("rung %d dependency = %d, declared rule gives %d", rung.Index, rung.Dependency.Cardinality, want)
		}
	}
}

// Spot-check one closed-form scalar against an independent small-k sum.
func TestExpectedScalarsMatchTheClosedFormModel(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	first := manifest.Rungs[0]
	total := new(big.Rat)
	for rowID := int64(1); rowID <= first.Rows; rowID++ {
		total.Add(total, RowAmount(rowID))
	}
	if first.ExpectedScalars[0] != total.RatString() {
		t.Fatalf("rung 1 sum(amount) = %s, closed form gives %s", first.ExpectedScalars[0], total.RatString())
	}
	if RowCategory(1) != "alpha" || RowCategory(4) != "delta" || RowQuantity(10001) != 1 {
		t.Fatal("dataset model spot checks failed")
	}
}
