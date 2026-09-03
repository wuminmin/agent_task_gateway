// Command scale7-ladder-corpus regenerates the frozen scale7 SUM
// ladder corpus from the closed-form dataset model. The output must be
// committed; the package pin test requires byte identity.
package main

import (
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5scale7"
)

func main() {
	manifest, err := finalv5scale7.BuildManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, err := finalv5scale7.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	target := "evaluation/finalv5scale7/corpus-v1.json"
	if err := os.WriteFile(target, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d rungs, %d bytes)\n", target, len(manifest.Rungs), len(encoded))
}
