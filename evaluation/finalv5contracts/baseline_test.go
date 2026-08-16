package finalv5contracts

import "testing"

// TestBaselineCellsResolveFromTheFrozenContract holds the decoder to the
// contract rather than to a hand-kept table: every preregistered Baseline cell
// must decode, and the two workloads the Adapter implements must render both
// arms from their indexed templates.
func TestBaselineCellsResolveFromTheFrozenContract(t *testing.T) {
	runtime, err := LoadRuntime()
	if err != nil {
		t.Fatalf("load contract runtime: %v", err)
	}
	cells, err := runtime.BaselineCells()
	if err != nil {
		t.Fatalf("decode Baseline cells: %v", err)
	}
	if len(cells) != 58 {
		t.Fatalf("Baseline contract decoded %d cells, want the 58 the protocol matrix preregisters", len(cells))
	}
	seen := map[CellIdentity]bool{}
	implemented := 0
	for _, decoded := range cells {
		if seen[decoded.Identity] {
			t.Fatalf("Baseline contract repeats cell %s", decoded.Identity)
		}
		seen[decoded.Identity] = true
		if decoded.Identity.WorkloadID != "S1" && decoded.Identity.WorkloadID != "S2" {
			continue
		}
		implemented++
		contract, err := runtime.BaselineQueryContract(decoded)
		if err != nil {
			t.Fatalf("render Baseline cell %s: %v", decoded.Identity, err)
		}
		if contract.BDG.SQL == "" || contract.Direct.SQL == "" {
			t.Fatalf("Baseline cell %s rendered an empty arm", decoded.Identity)
		}
		if contract.BDG.SQL == contract.Direct.SQL {
			t.Fatalf("Baseline cell %s rendered identical arms; the Direct arm must name the reporting schema", decoded.Identity)
		}
		if len(contract.BDG.Parameters) != 1 || len(contract.Direct.Parameters) != 1 {
			t.Fatalf("Baseline cell %s did not bind exactly one frozen parameter per arm", decoded.Identity)
		}
	}
	if implemented != 20 {
		t.Fatalf("S1 and S2 contribute %d cells, want 20", implemented)
	}
}

// TestBaselineCellLookupIsWorkloadScoped proves the lookup key includes the
// workload. S1 and S2 share every scale and mode, so a workload-blind lookup
// would silently return the wrong cell's query.
func TestBaselineCellLookupIsWorkloadScoped(t *testing.T) {
	runtime, err := LoadRuntime()
	if err != nil {
		t.Fatalf("load contract runtime: %v", err)
	}
	first, err := runtime.BaselineCell("S1", "SF10", "novel")
	if err != nil {
		t.Fatalf("look up S1/SF10/novel: %v", err)
	}
	second, err := runtime.BaselineCell("S2", "SF10", "novel")
	if err != nil {
		t.Fatalf("look up S2/SF10/novel: %v", err)
	}
	if first.QueryTemplate == second.QueryTemplate {
		t.Fatal("S1 and S2 resolved the same query template")
	}
	if first.ExpectedRows == second.ExpectedRows {
		t.Fatalf("S1 and S2 both expect %d rows; the grouped join must not project one row per key", first.ExpectedRows)
	}
	if _, err := runtime.BaselineCell("S1", "SF10", "no-such-mode"); err == nil {
		t.Fatal("an unknown mode resolved a Baseline cell")
	}
}
