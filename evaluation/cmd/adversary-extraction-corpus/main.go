// Command adversary-extraction-corpus derives the frozen P9.C
// optimizing-adversary corpus.
package main

import (
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5adversary"
)

func main() {
	out := flag.String("out", "evaluation/finalv5adversary/corpus-v1.json", "corpus output path")
	flag.Parse()
	manifest, err := finalv5adversary.BuildManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, "adversary-extraction-corpus:", err)
		os.Exit(1)
	}
	encoded, err := finalv5adversary.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "adversary-extraction-corpus:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "adversary-extraction-corpus:", err)
		os.Exit(1)
	}
	for _, trace := range manifest.Traces {
		recovered := fmt.Sprintf("[%d,%d) bits=%d", trace.RecoveredLo, trace.RecoveredHi, trace.RecoveredBits)
		if trace.RecoveredValue != nil {
			recovered = fmt.Sprintf("exact=%d bits=%d", *trace.RecoveredValue, trace.RecoveredBits)
		}
		if trace.Strategy == "greedy" {
			recovered = "-"
		}
		fmt.Printf("%-9s %-9s accepted=%2d refused=%2d U_R=%2d U_D=%2d U_O=%2d recovery=%s\n",
			trace.Tier, trace.Strategy, trace.AcceptedSteps, trace.RefusedSteps,
			trace.DistinctRelease, trace.DistinctDep, trace.DistinctOutcome, recovered)
	}
}
