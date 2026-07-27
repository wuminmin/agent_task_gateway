// Command merge-evidence splices the deterministic in-process exposure
// evidence into an existing PG-backed results.json without re-running the
// PostgreSQL oracle, RQ3 integration, or RQ4 performance campaign.
//
// The operator's run-exposure.sh pipeline regenerates the whole report from
// scratch (including these in-process sections) when it next runs. This command
// exists so the committed results.json stays coherent between operator runs:
// it overwrites only the in-process sections (rq5_agent_tasks,
// rq2_exposure_invariance, rq4_scaling) and leaves every PG-backed field
// (rq1, the rq2 SQL campaign, rq3 integration, rq4 runtime status,
// charge_baselines, rq5 planner oracle) untouched.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	exposureeval "taskbound.local/agent-data-gateway/evaluation/exposure"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: merge-evidence <results.json>")
		os.Exit(2)
	}
	path := os.Args[1]
	existing := make(map[string]any)
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read results:", err)
		os.Exit(1)
	}
	if err := json.Unmarshal(raw, &existing); err != nil {
		fmt.Fprintln(os.Stderr, "parse results:", err)
		os.Exit(1)
	}

	report, err := exposureeval.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "in-process exposure evaluation failed:", err)
		os.Exit(1)
	}
	existing["rq5_agent_tasks"] = report.RQ5Agent
	existing["rq2_exposure_invariance"] = report.RQ2Exposure
	existing["rq4_scaling"] = report.RQ4Scaling

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(existing); err != nil {
		fmt.Fprintln(os.Stderr, "encode results:", err)
		os.Exit(1)
	}
}
