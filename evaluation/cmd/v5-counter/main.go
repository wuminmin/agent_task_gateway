// Command v5-counter runs the comparator-arms extension experiment through
// the shared Final-V5 runner harness.
package main

import (
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(experiment.RunCommand("counter")) }
