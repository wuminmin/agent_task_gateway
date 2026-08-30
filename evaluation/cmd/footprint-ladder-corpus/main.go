// Command footprint-ladder-corpus regenerates the frozen refused-footprint
// ladder corpus from the closed-form dataset model. The output must be
// committed; the package pin test requires byte identity.
package main

import (
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5footprint"
)

func main() {
	manifest, err := finalv5footprint.BuildManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, err := finalv5footprint.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	target := "evaluation/finalv5footprint/corpus-v1.json"
	if err := os.WriteFile(target, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d rungs, %d bytes)\n", target, len(manifest.Rungs), len(encoded))
}
