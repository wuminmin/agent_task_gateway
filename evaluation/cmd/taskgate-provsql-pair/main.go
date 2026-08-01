package main

import (
	"os"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() { os.Exit(experiment.RunCommand("provsql")) }
