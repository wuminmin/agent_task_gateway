// Command v5-scale7 runs the P9.E scale-ladder extension experiment through
// the shared Final-V5 runner harness.
package main

import (
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(experiment.RunCommand("scale7")) }
