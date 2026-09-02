package finalv5scale7

import (
	"bytes"
	"testing"
)

// The embedded corpus must be byte-identical to a fresh rebuild from the
// closed-form dataset model and the declared Dependency rule.
func TestScale7CorpusMatchesRebuild(t *testing.T) {
	if testing.Short() {
		t.Skip("the 1.125e7-hash rebuild is not a -short test")
	}
	manifest, err := BuildManifest()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := EncodeManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rebuilt, corpusBytes) {
		t.Fatal("embedded scale-7 corpus differs from a fresh rebuild; regenerate evaluation/finalv5scale7/corpus-v1.json")
	}
}

func TestScale7CorpusInvariants(t *testing.T) {
	manifest, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	largest := manifest.Rungs[len(manifest.Rungs)-1]
	if largest.Dependency.Cardinality <= 10000000 {
		t.Fatalf("the largest rung settles %d dependency facts, the scale claim needs more than 1e7",
			largest.Dependency.Cardinality)
	}
	if largest.Dependency.Cardinality >= MaxDependencyFacts {
		t.Fatal("the dependency ceiling must clear the largest rung")
	}
}
