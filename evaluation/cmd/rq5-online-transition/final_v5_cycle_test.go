package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func TestRQ5EvidenceSampleCopiesWarmupIdentity(t *testing.T) {
	operation := experiment.AdapterOperation{
		SchemaVersion: 1, CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "rq5",
		CellID: "cell", SampleID: "sample", Iteration: 1, ProcessReplicate: 1, Warmup: true,
		OrderPosition: 1, RandomSeed: 7, PairID: "pair", PairedSystemOrder: "build_verify_activate,retained_route",
		RootGroupID: "root", WorkloadID: "daily-publication-v5", Scale: "345000", Mode: "build_verify_activate",
	}
	evidence := &experiment.RQ5VerificationEvidence{Route: experiment.RQ5RouteEvidence{
		NewInitial: experiment.RQ5QueryEvidence{VerifierManifest: &experiment.RedactedVerifierManifest{}},
	}}
	sample := rq5EvidenceSample(operation, evidence)
	if !sample.Warmup || sample.ProcessReplicate != operation.ProcessReplicate ||
		sample.OrderPosition != operation.OrderPosition {
		t.Fatalf("operation identity was not copied: %#v", sample)
	}
}

func TestFinalV5SubcommandDispatchCannotEnterLegacySyntheticPath(t *testing.T) {
	originalLegacy, originalFinal := legacyRunCommand, finalV5CycleCommand
	defer func() { legacyRunCommand, finalV5CycleCommand = originalLegacy, originalFinal }()
	legacyCalled, finalCalled := false, false
	legacyRunCommand = func(runOptions) error {
		legacyCalled = true
		return nil
	}
	finalV5CycleCommand = func(finalV5CycleOptions) error {
		finalCalled = true
		return nil
	}
	if err := execute([]string{"final-v5-cycle"}); err != nil {
		t.Fatal(err)
	}
	if !finalCalled || legacyCalled {
		t.Fatalf("formal dispatch final=%t legacy=%t", finalCalled, legacyCalled)
	}

	files, err := filepath.Glob("final_v5_*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"capturingApproval", "httptest.", "DemoReceiptSigner", "newExperimentRouter", "runOnlineExperiment"}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, symbol := range forbidden {
			if strings.Contains(string(value), symbol) {
				t.Fatalf("formal final-v5 source %s reaches legacy symbol %s", path, symbol)
			}
		}
	}
}
