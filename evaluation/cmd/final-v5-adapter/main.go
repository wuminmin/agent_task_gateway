package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"os"
	"reflect"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

var experimentIDs = [...]string{"baseline", "scale", "artifact", "rls", "attack", "provsql", "compiler", "concurrency", "rq5"}

type sourceControlledAdapter interface {
	Execute(context.Context, experiment.AdapterOperation) experiment.Sample
	Close()
}

type adapterFactory func(context.Context) (sourceControlledAdapter, error)

// adapterFactories is the single capability gate. An experiment is reported
// as implemented only after its real, source-controlled constructor is wired
// here; unsupported and placeholder handlers cannot turn a capability on.
var adapterFactories = map[string]adapterFactory{
	"baseline":    func(ctx context.Context) (sourceControlledAdapter, error) { return newRealAdapter(ctx) },
	"scale":       func(ctx context.Context) (sourceControlledAdapter, error) { return newScaleAdapter(ctx) },
	"artifact":    func(ctx context.Context) (sourceControlledAdapter, error) { return newArtifactAdapter(ctx) },
	"rls":         func(ctx context.Context) (sourceControlledAdapter, error) { return newRLSAdapter(ctx) },
	"attack":      func(ctx context.Context) (sourceControlledAdapter, error) { return newAttackAdapter(ctx) },
	"provsql":     func(ctx context.Context) (sourceControlledAdapter, error) { return newProvSQLAdapter(ctx) },
	"compiler":    func(ctx context.Context) (sourceControlledAdapter, error) { return newCompilerAdapter(ctx) },
	"concurrency": func(ctx context.Context) (sourceControlledAdapter, error) { return newConcurrencyAdapter(ctx) },
	"rq5":         func(ctx context.Context) (sourceControlledAdapter, error) { return newRQ5Adapter(ctx) },
}

func implementedCapabilities() map[string]bool {
	result := make(map[string]bool, len(experimentIDs))
	for _, experimentID := range experimentIDs {
		factory, registered := adapterFactories[experimentID]
		result[experimentID] = registered && factory != nil
	}
	return result
}

func initializeAdapter(ctx context.Context, experimentID string) (sourceControlledAdapter, string) {
	factory, registered := adapterFactories[experimentID]
	if !registered || factory == nil {
		return nil, "source_controlled_experiment_not_implemented"
	}
	adapter, err := factory(ctx)
	if err != nil || nilSourceControlledAdapter(adapter) {
		return nil, "adapter_environment_invalid"
	}
	return adapter, ""
}

func nilSourceControlledAdapter(adapter sourceControlledAdapter) bool {
	if adapter == nil {
		return true
	}
	value := reflect.ValueOf(adapter)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func main() {
	experimentID := flag.String("experiment", "", "source-controlled experiment implementation")
	capabilities := flag.Bool("capabilities", false, "print implemented experiment capabilities")
	flag.Parse()
	if *capabilities {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"schema_version": 1, "adapter": "final-v5-adapter", "experiments": implementedCapabilities()})
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	encoder := json.NewEncoder(os.Stdout)
	adapter, initializationCode := initializeAdapter(context.Background(), *experimentID)
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
