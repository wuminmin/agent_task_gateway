package finalv5counter

import (
	"bytes"
	"testing"
)

func TestCounterCorpusMatchesRebuild(t *testing.T) {
	manifest, err := BuildManifest()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, corpusBytes) {
		t.Fatal("embedded counter corpus differs from a fresh rebuild; regenerate evaluation/finalv5counter/corpus-v1.json")
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

// The study's central a-priori claims, pinned so the frozen corpus cannot
// drift away from them silently.
func TestCounterCorpusComparatorClaims(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ordering := range Orderings {
		exact, err := manifest.Trace("exact", ordering)
		if err != nil {
			t.Fatal(err)
		}
		// Exact accounting is order-robust: no ordering exceeds the floors.
		if exact.DistinctDep > ExactMaxDependency || exact.DistinctRelease > ExactMaxRelease ||
			exact.DistinctOutcome > ExactMaxOutcome {
			t.Fatalf("exact arm exceeded its floors under %s", ordering)
		}
		for _, arm := range []string{"rows", "queries"} {
			naive, err := manifest.Trace(arm, ordering)
			if err != nil {
				t.Fatal(err)
			}
			// Every naive resource counter leaks the full dependency set.
			if naive.DistinctDep != 18 {
				t.Fatalf("%s arm under %s leaked %d distinct dependencies, the full trace has 18",
					arm, ordering, naive.DistinctDep)
			}
		}
		release, err := manifest.Trace("release", ordering)
		if err != nil {
			t.Fatal(err)
		}
		// The release-set-only counter caps Result facts but still leaks the
		// full dependency set: one dimension is not three.
		if release.DistinctRelease > ExactMaxRelease || release.DistinctDep != 18 {
			t.Fatalf("release arm under %s: R=%d D=%d", ordering, release.DistinctRelease, release.DistinctDep)
		}
	}
	// Orderings are permutations of the whole trace.
	for name, order := range manifest.OrderingIndexes {
		seen := map[int]bool{}
		for _, index := range order {
			if index < 0 || index >= 100 || seen[index] {
				t.Fatalf("ordering %s is not a permutation", name)
			}
			seen[index] = true
		}
		if len(seen) != 100 {
			t.Fatalf("ordering %s does not cover the trace", name)
		}
	}
}
