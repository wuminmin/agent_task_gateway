package exposureeval

import "testing"

// TestRewriteInvarianceClosedLanguage is the load-bearing RQ2 check: every
// closed-language rewrite preserves TaskGate's typed normal form, its release
// and positive-output dependency FactSets (stored under the compatibility name
// Influence), and yields zero incremental charge, across every data instance
// and snapshot. A single mismatch fails the build.
func TestRewriteInvarianceClosedLanguage(t *testing.T) {
	summary, err := RunExposureRewriteInvariance()
	if err != nil {
		t.Fatalf("rewrite invariance campaign failed: %v", err)
	}
	if summary.Mismatches != 0 {
		t.Fatalf("expected zero mismatches, got %d", summary.Mismatches)
	}
	if summary.Cases < 8 {
		t.Fatalf("expected at least one case per rewrite (>=8), got %d", summary.Cases)
	}
	if summary.NormalFormChecks == 0 {
		t.Fatalf("expected at least one NF-canonical rewrite, got %d", summary.NormalFormChecks)
	}
	if summary.Rewrites < 8 || summary.Datasets < 4 {
		t.Fatalf("expected >=8 rewrites over >=4 datasets, got %d rewrites / %d datasets",
			summary.Rewrites, summary.Datasets)
	}
	for _, result := range summary.Results {
		nfOK := !result.NormalFormRequired || result.NormalFormEqual
		if !nfOK || !result.ReleaseEqual || !result.InfluenceEqual || !result.ChargeDeltaZero {
			t.Errorf("rewrite %s on %s not invariant: %+v", result.Case, result.Dataset, result)
		}
	}
}
