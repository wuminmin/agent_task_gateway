package finalv5adversary

import "testing"

// The headline claims frozen in docs/p9c_optimizing_adversary_design.md.
// They are pinned against the deterministic derivation so any drift in the
// simulation, the fixture, or the tier budgets fails loudly here before a
// pilot ever runs.
func TestFrozenHeadlineClaims(t *testing.T) {
	manifest, err := BuildManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.HiddenTarget != 1910 {
		t.Fatalf("hidden sales maximum = %d, the frozen fixture pins 1910", manifest.HiddenTarget)
	}
	tierByName := map[string]Tier{}
	for _, tier := range Tiers {
		tierByName[tier.Name] = tier
	}
	bits := map[string]int{"owner": 6, "tightened": 4, "loosened": 11}
	widths := map[string]int64{"owner": 32, "tightened": 128, "loosened": 1}
	greedyDep := map[string]int64{"owner": 18, "tightened": 12, "loosened": 18}
	for _, trace := range manifest.Traces {
		tier := tierByName[trace.Tier]
		if trace.DistinctRelease > tier.MaxRelease || trace.DistinctDep > tier.MaxDependency ||
			trace.DistinctOutcome > tier.MaxOutcome {
			t.Fatalf("%s/%s final union exceeds its own budgets: %+v", trace.Tier, trace.Strategy, trace)
		}
		switch trace.Strategy {
		case "bisection":
			if trace.RecoveredBits != bits[trace.Tier] {
				t.Fatalf("%s bisection recovered %d bits, frozen design pins %d",
					trace.Tier, trace.RecoveredBits, bits[trace.Tier])
			}
			if trace.RecoveredHi-trace.RecoveredLo != widths[trace.Tier] {
				t.Fatalf("%s bisection interval width %d, frozen design pins %d",
					trace.Tier, trace.RecoveredHi-trace.RecoveredLo, widths[trace.Tier])
			}
			if half := int(tier.MaxOutcome / 2); trace.RecoveredBits > half {
				t.Fatalf("%s bisection recovered %d bits above the floor(B_O/2)=%d bound",
					trace.Tier, trace.RecoveredBits, half)
			}
			if trace.Tier == "loosened" {
				if trace.RecoveredValue == nil || *trace.RecoveredValue != manifest.HiddenTarget {
					t.Fatalf("loosened bisection did not recover the hidden target exactly: %+v", trace)
				}
			} else if trace.RecoveredValue != nil {
				t.Fatalf("%s bisection recovered the exact value despite its budgets", trace.Tier)
			}
		case "greedy":
			if trace.DistinctDep != greedyDep[trace.Tier] {
				t.Fatalf("%s greedy distinct dependency %d, frozen design pins %d",
					trace.Tier, trace.DistinctDep, greedyDep[trace.Tier])
			}
		default:
			t.Fatalf("unknown strategy %q", trace.Strategy)
		}
	}
	if _, err := Load(); err != nil {
		t.Fatalf("embedded corpus does not round-trip: %v", err)
	}
	if len(CorpusSHA256()) != 64 {
		t.Fatal("corpus digest is not a SHA-256 hex string")
	}
}
