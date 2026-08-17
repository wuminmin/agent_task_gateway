package main

import (
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
	fmt.Fprintf(adapterDiagnosticOutput, "%s %s/%s/%s failed: %v\n",
		adapter, operation.WorkloadID, operation.Scale, operation.Mode, err)
}

func writeAdapterInitializationDiagnostic(adapter string, err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(adapterDiagnosticOutput, "%s adapter initialization failed: %v\n", adapter, err)
}
