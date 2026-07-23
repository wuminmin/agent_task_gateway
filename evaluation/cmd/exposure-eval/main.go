package main

import (
	"encoding/json"
	"fmt"
	"os"

	exposureeval "taskbound.local/agent-data-gateway/evaluation/exposure"
)

func main() {
	report, err := exposureeval.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "exposure evaluation failed:", err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, "encode exposure report:", err)
		os.Exit(1)
	}
}
