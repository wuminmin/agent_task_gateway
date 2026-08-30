package generatedalgebra

import "testing"

func TestGeneratedPlansAgreeWithProduction(t *testing.T) {
	report := Run(20260830, 3, 40)
	if report.Mismatches != 0 || report.HashMismatches != 0 || len(report.Failures) != 0 {
		t.Fatalf("differential campaign failed: %+v", report)
	}
	for _, kind := range []string{"project", "group", "global", "join", "select", "page"} {
		if report.Coverage[kind] == 0 {
			t.Fatalf("no coverage for %s: %+v", kind, report.Coverage)
		}
	}
	if report.Conservation["split_projection_MISMATCH"] != 0 || report.Conservation["page_partition_MISMATCH"] != 0 {
		t.Fatalf("conservation failed: %+v", report.Conservation)
	}
}
