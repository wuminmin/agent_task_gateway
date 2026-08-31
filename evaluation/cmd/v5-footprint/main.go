// Command v5-footprint runs the refused-footprint ladder extension experiment
// through the shared Final-V5 runner harness.
package main

import (
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(experiment.RunCommand("footprint")) }
