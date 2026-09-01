// Command counter-comparator-corpus derives the frozen (a) comparator corpus.
package main

import (
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/finalv5counter"
)

func main() {
	out := flag.String("out", "evaluation/finalv5counter/corpus-v1.json", "corpus output path")
	flag.Parse()
	manifest, err := finalv5counter.BuildManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, "counter-comparator-corpus:", err)
		os.Exit(1)
	}
	encoded, err := finalv5counter.EncodeManifest(manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "counter-comparator-corpus:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "counter-comparator-corpus:", err)
		os.Exit(1)
	}
	for _, trace := range manifest.Traces {
		fmt.Printf("%-8s %-17s accepted=%2d refused=%2d first=%2d rows=%3d U_R=%2d U_D=%2d U_O=%2d\n",
			trace.Arm, trace.Ordering, trace.AcceptedSteps, trace.RefusedSteps, trace.FirstRefusal,
			trace.ReleasedRowTotal, trace.DistinctRelease, trace.DistinctDep, trace.DistinctOutcome)
	}
}
