// Command generated-algebra runs the generated-plan differential campaign and
// writes its report.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/generatedalgebra"
)

func main() {
	seed := flag.Int64("seed", 20260830, "random seed")
	fixtures := flag.Int("fixtures", 50, "random fixtures")
	plans := flag.Int("plans", 100, "plans per fixture")
	out := flag.String("out", "evaluation/generatedalgebra/results.json", "output path")
	flag.Parse()
	report := generatedalgebra.Run(*seed, *fixtures, *plans)
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(encoded, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("plans=%d mismatches=%d hash_mismatches=%d failures=%d coverage=%v conservation=%v\n", report.Plans, report.Mismatches, report.HashMismatches, len(report.Failures), report.Coverage, report.Conservation)
	if report.Mismatches != 0 || report.HashMismatches != 0 || len(report.Failures) != 0 {
		os.Exit(2)
	}
}
