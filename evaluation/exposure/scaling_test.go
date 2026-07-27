package exposureeval

import "testing"

// TestScalingSweepsCoverCoreDimensions is the in-process RQ4 scaling check:
// it confirms that observe, normalizer, and settlement-dedup cost are
// all measured across growing sizes, and that the novel-vs-replay charge gap is
// exactly as the history-aware accounting predicts (novel charges all, replay
// charges none).
func TestScalingSweepsCoverCoreDimensions(t *testing.T) {
	summary, err := RunScaling()
	if err != nil {
		t.Fatalf("scaling sweep failed: %v", err)
	}
	if len(summary.Curves) != 3 {
		t.Fatalf("expected 3 scaling curves, got %d", len(summary.Curves))
	}
	for _, curve := range summary.Curves {
		if len(curve.Points) < 4 {
			t.Fatalf("curve %s has %d points, want >=4", curve.Dimension, len(curve.Points))
		}
		for _, point := range curve.Points {
			if point.NsPerOp <= 0 {
				t.Fatalf("curve %s size %d reported non-positive ns/op %d", curve.Dimension, point.Size, point.NsPerOp)
			}
		}
	}
	// The closed accounting's central promise: a replayed (history-hit) write
	// charges zero, while a fresh write charges every novel fact.
	for _, curve := range summary.Curves {
		if curve.Dimension != "novel_vs_replay" {
			continue
		}
		for _, point := range curve.Points {
			if point.NovelCharge != point.Size {
				t.Fatalf("novel charge %d != size %d", point.NovelCharge, point.Size)
			}
			if point.ReplayCharge != 0 {
				t.Fatalf("replay charge %d != 0 at size %d", point.ReplayCharge, point.Size)
			}
		}
	}
}
