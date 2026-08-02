package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() {
	experimentID := flag.String("experiment", "", "source-controlled experiment implementation")
	capabilities := flag.Bool("capabilities", false, "print implemented experiment capabilities")
	flag.Parse()
	if *capabilities {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "adapter": "final-v5-adapter", "experiments": map[string]bool{"baseline": true, "scale": false, "artifact": false, "rls": false, "attack": false, "provsql": false, "compiler": false, "concurrency": false, "rq5": false}})
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	var adapter *realAdapter
	var initializationCode string
	if *experimentID == "baseline" {
		var err error
		adapter, err = newRealAdapter(context.Background())
		if err != nil {
			initializationCode = "adapter_environment_invalid"
		}
	} else {
		initializationCode = "source_controlled_experiment_not_implemented"
	}
	if adapter != nil {
		defer adapter.Close()
	}
	for scanner.Scan() {
		var operation experiment.AdapterOperation
		if experiment.StrictJSON(scanner.Bytes(), &operation) != nil {
			os.Exit(1)
		}
		var sample experiment.Sample
		if initializationCode != "" || operation.ExperimentID != *experimentID {
			code := initializationCode
			if code == "" {
				code = "experiment_identity_mismatch"
			}
			sample = invalidSample(operation, code)
		} else {
			sample = adapter.Execute(context.Background(), operation)
		}
		if encoder.Encode(sample) != nil {
			os.Exit(1)
		}
	}
	if scanner.Err() != nil {
		os.Exit(1)
	}
}
