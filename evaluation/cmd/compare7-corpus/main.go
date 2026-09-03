// Command compare7-corpus regenerates the frozen comparison sequence from
// the closed-form dataset model. The output must be committed; the package
// pin test requires byte identity.
package main

import (
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5compare"
)

func main() {
	manifest, err := finalv5compare.BuildManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoded, err := finalv5compare.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	target := "evaluation/finalv5compare/corpus-v1.json"
	if err := os.WriteFile(target, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d steps, %d bytes)\n", target, len(manifest.Steps), len(encoded))
}
