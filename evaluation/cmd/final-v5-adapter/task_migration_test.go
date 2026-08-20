package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func TestTaskMigrationRecorderEmitsPerSampleVersionedWaits(t *testing.T) {
	t.Setenv(p68CliffDiagnosisEnv, p68CliffDiagnosisMarker)
	operation := experiment.AdapterOperation{CampaignID: "p68", DeploymentID: "deployment-01",
		ExperimentID: "concurrency", CellID: "shared-root/500/natural_contention", SampleID: "sample-275", OrderPosition: 275}
	ctx := withTaskMigrationOperation(context.Background(), operation)
	var output bytes.Buffer
	original := adapterDiagnosticOutput
	adapterDiagnosticOutput = &output
	t.Cleanup(func() { adapterDiagnosticOutput = original })
	recordTaskMigrationWait(ctx, "private-task", "root", "AWAITING_APPROVAL", "AWAITING_APPROVAL",
		"reached", time.Millisecond, 1, nil)
	recordTaskMigrationWait(ctx, "private-task", "root", "ACTIVE", "AWAITING_APPROVAL",
		"timeout", 30*time.Second, 300, errors.New("last polling error"))
	scanner := json.NewDecoder(&output)
	for index := 0; index < 2; index++ {
		var diagnostic experiment.TaskMigrationWaitDiagnosticV1
		if err := scanner.Decode(&diagnostic); err != nil {
			t.Fatal(err)
		}
		if err := diagnostic.Validate(); err != nil || diagnostic.SampleID != operation.SampleID || diagnostic.TaskOrdinal != 1 {
			t.Fatalf("record %d is not bound to the operation/task: %+v err=%v", index, diagnostic, err)
		}
	}
	if strings.Contains(output.String(), "private-task") {
		t.Fatal("task migration diagnostic leaked the raw task identity")
	}
}

func TestTaskMigrationTimeoutErrorCarriesLastObservation(t *testing.T) {
	err := &taskMigrationWaitError{expected: "ACTIVE", lastState: "AWAITING_APPROVAL", elapsed: 30 * time.Second,
		polls: 300, lastErr: errors.New("last polling error"), status: "timed out"}
	message := err.Error()
	for _, wanted := range []string{"expected=ACTIVE", "last_state=AWAITING_APPROVAL", "elapsed=30s", "polls=300", "last_error=last polling error"} {
		if !strings.Contains(message, wanted) {
			t.Fatalf("timeout error %q omits %q", message, wanted)
		}
	}
}
