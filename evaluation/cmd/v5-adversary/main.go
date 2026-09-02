// Command v5-adversary runs the optimizing-adversary extension experiment
// through the shared experiment runner.
package main

import (
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(experiment.RunCommand("adversary")) }
