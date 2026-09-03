// Command v5-compare7 runs the P9.F comparison-sequence extension experiment
// through the shared Final-V5 runner harness.
package main

import (
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(experiment.RunCommand("compare7")) }
