package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// adapterDiagnosticOutput is the stderr-only evidence boundary. The campaign
// launcher captures it in a mode-0600 file and applies the credential scan
// before admitting the deployment record. Keeping the writer injectable lets
// tests prove that the diagnostic changes without changing Sample fields.
var adapterDiagnosticOutput io.Writer = os.Stderr

func writeAdapterFailureDiagnostic(adapter string, operation experiment.AdapterOperation, err error) {
	if err == nil {
		return
	}
	// The P68 diagnostic channel already contains one strict, versioned record
	// for a migration timeout. Do not add an unstructured duplicate line: the
	// runner validates every retained diagnostic record before the credential
	// scan admits it.
	if os.Getenv(p68CliffDiagnosisEnv) == p68CliffDiagnosisMarker && isTaskMigrationWaitError(err) {
		return
	}
	fmt.Fprintf(adapterDiagnosticOutput, "%s %s/%s/%s failed: %v\n",
		adapter, operation.WorkloadID, operation.Scale, operation.Mode, err)
}

func writeAdapterSampleFailureDiagnostic(adapter string, sample experiment.Sample, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(adapterDiagnosticOutput, "%s %s/%s/%s failed: %v\n",
		adapter, sample.WorkloadID, sample.Scale, sample.Mode, err)
}

func writeAdapterInitializationDiagnostic(adapter string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(adapterDiagnosticOutput, "%s adapter initialization failed: %v\n", adapter, err)
}

func writePreregisteredConcurrencyMissDiagnostic(sample experiment.Sample, err error) {
	diagnostic := experiment.NewPreregisteredConcurrencyMissDiagnosticV1(sample, err)
	_ = json.NewEncoder(adapterDiagnosticOutput).Encode(diagnostic)
}
