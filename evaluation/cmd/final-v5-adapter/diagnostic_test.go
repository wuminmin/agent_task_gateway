package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func captureAdapterDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := adapterDiagnosticOutput
	var output bytes.Buffer
	adapterDiagnosticOutput = &output
	t.Cleanup(func() { adapterDiagnosticOutput = previous })
	return &output
}

func TestEveryAdapterFailureCauseReachesDiagnosticBoundary(t *testing.T) {
	output := captureAdapterDiagnostics(t)
	for _, adapter := range experimentIDs {
		operation := experiment.AdapterOperation{WorkloadID: "cause", Scale: "negative", Mode: "control"}
		writeAdapterFailureDiagnostic(adapter, operation, fmt.Errorf("%s private cause", adapter))
	}
	for _, adapter := range experimentIDs {
		want := adapter + " cause/negative/control failed: " + adapter + " private cause"
		if !strings.Contains(output.String(), want) {
			t.Errorf("%s cause was not retained on stderr: %q", adapter, output.String())
		}
	}
}

func TestEveryAdapterExecutionSourceUsesCauseDiagnostic(t *testing.T) {
	for _, adapter := range experimentIDs {
		payload, err := os.ReadFile(adapter + ".go")
		if err != nil {
			t.Fatal(err)
		}
		operationCall := `writeAdapterFailureDiagnostic("` + adapter + `"`
		sampleCall := `writeAdapterSampleFailureDiagnostic("` + adapter + `"`
		if !strings.Contains(string(payload), operationCall) && !strings.Contains(string(payload), sampleCall) {
			t.Errorf("%s execution source has no cause diagnostic call", adapter)
		}
	}
}

func TestAdapterInitializationFailureWritesCauseToDiagnosticBoundary(t *testing.T) {
	output := captureAdapterDiagnostics(t)
	previous := adapterFactories["concurrency"]
	adapterFactories["concurrency"] = func(context.Context) (sourceControlledAdapter, error) {
		return nil, errors.New("private initialization cause")
	}
	t.Cleanup(func() { adapterFactories["concurrency"] = previous })

	adapter, code := initializeAdapter(t.Context(), "concurrency")
	if adapter != nil || code != "adapter_environment_invalid" {
		t.Fatalf("initialization outcome changed: adapter=%v code=%q", adapter, code)
	}
	if got := output.String(); !strings.Contains(got, "concurrency adapter initialization failed: private initialization cause") {
		t.Fatalf("initialization cause was not retained in adapter stderr: %q", got)
	}
}
