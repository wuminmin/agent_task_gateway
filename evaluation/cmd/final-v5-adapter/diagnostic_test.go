package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func captureAdapterDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := adapterDiagnosticOutput
	var output bytes.Buffer
	adapterDiagnosticOutput = &output
	t.Cleanup(func() { adapterDiagnosticOutput = previous })
	return &output
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
