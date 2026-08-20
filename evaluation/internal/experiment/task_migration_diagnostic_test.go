package experiment

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestTaskMigrationWaitDiagnosticV1IsExplicitAndOperationBound(t *testing.T) {
	operation := AdapterOperation{CampaignID: "p68", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		CellID: "shared-root/500/natural_contention", SampleID: "sample-0275", OrderPosition: 275}
	diagnostic := NewTaskMigrationWaitDiagnosticV1(operation, 501, "delegated_child", "task-private",
		"ACTIVE", "AWAITING_APPROVAL", "timeout", 30*time.Second, 299, "none", time.Now())
	if err := diagnostic.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("task-private")) || !bytes.Contains(encoded, []byte(TaskMigrationWaitDiagnosticV1Record)) ||
		diagnostic.CallbackTaskIDSHA256 == "" {
		t.Fatalf("diagnostic leaks the task ID or omits its explicit record version: %s", encoded)
	}
	mutated := diagnostic
	mutated.Status = "reached"
	if err := mutated.Validate(); err == nil {
		t.Fatal("reached diagnostic with the wrong last state was accepted")
	}
	mutated = diagnostic
	mutated.TaskIDHash = "not-a-digest"
	if err := mutated.Validate(); err == nil {
		t.Fatal("non-hex task identity hash was accepted")
	}
	mutated = diagnostic
	mutated.CallbackTaskIDSHA256 = "not-a-digest"
	if err := mutated.Validate(); err == nil {
		t.Fatal("non-hex callback task identity hash was accepted")
	}
	legacy := diagnostic
	legacy.CallbackTaskIDSHA256 = ""
	if err := legacy.Validate(); err != nil {
		t.Fatalf("historical P68 diagnostic without the P69 join key was rejected: %v", err)
	}
}

func TestRunnerAcceptsTaskMigrationDiagnosticsOnlyInP68Mode(t *testing.T) {
	operations := []AdapterOperation{
		{CampaignID: "p68", DeploymentID: "deployment-01", ExperimentID: "concurrency", CellID: "serial-control/1/serial", SampleID: "sample-1", OrderPosition: 1},
		{CampaignID: "p68", DeploymentID: "deployment-01", ExperimentID: "concurrency", CellID: "shared-root/10/forced_queue_safety", SampleID: "sample-2", OrderPosition: 2},
	}
	var payload bytes.Buffer
	for index, operation := range operations {
		first := NewTaskMigrationWaitDiagnosticV1(operation, 1, "root", "task-"+operation.SampleID,
			"AWAITING_APPROVAL", "AWAITING_APPROVAL", "reached", time.Millisecond, 1, "none", time.Now())
		secondStatus, secondLast := "reached", "ACTIVE"
		if index == 1 {
			secondStatus, secondLast = "timeout", "AWAITING_APPROVAL"
		}
		second := NewTaskMigrationWaitDiagnosticV1(operation, 1, "root", "task-"+operation.SampleID,
			"ACTIVE", secondLast, secondStatus, 30*time.Second, 300, "none", time.Now())
		_ = json.NewEncoder(&payload).Encode(first)
		_ = json.NewEncoder(&payload).Encode(second)
	}
	t.Setenv(p68CliffDiagnosisEnv, p68CliffDiagnosisMarker)
	if err := validateAdapterDiagnostics("concurrency", operations, make([]*Sample, len(operations)), payload.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := validateTaskMigrationWaitDiagnostics("concurrency", operations[:1], payload.Bytes()); err == nil {
		t.Fatal("diagnostics for an unrequested sample were accepted")
	}
	t.Setenv(p68CliffDiagnosisEnv, "")
	if err := validateAdapterDiagnostics("concurrency", operations, make([]*Sample, len(operations)), payload.Bytes()); err == nil {
		t.Fatal("P68 migration diagnostics were accepted in an ordinary campaign")
	}
}

func TestP68DiagnosisConfigUsesOnlyTheExactReviewedDensity(t *testing.T) {
	config := Config{
		SchemaVersion: 1, CampaignClass: "pilot", PilotKind: "real_system",
		CampaignID: "p68-diagnosis", ExperimentID: "concurrency",
		SubmissionCommit: "0123456789abcdef0123456789abcdef01234567",
		Deployments:      1, ProcessReplicates: 1, Warmups: 5, Samples: 30,
		RandomSeed: 20260801, FreshRootPerSample: true,
		Workloads: []Workload{{ID: "shared-root", Scales: []string{"10"}, Modes: []string{"natural_contention"}}},
	}
	t.Setenv(p68CliffDiagnosisEnv, p68CliffDiagnosisMarker)
	if err := config.Validate("concurrency"); err != nil {
		t.Fatalf("exact P68 diagnosis config was refused: %v", err)
	}

	for name, mutate := range map[string]func(*Config){
		"samples_low":        func(value *Config) { value.Samples = 2 },
		"samples_high":       func(value *Config) { value.Samples = 31 },
		"warmups":            func(value *Config) { value.Warmups = 4 },
		"process_replicates": func(value *Config) { value.ProcessReplicates = 2 },
		"deployments":        func(value *Config) { value.Deployments = 2 },
		"experiment":         func(value *Config) { value.ExperimentID = "baseline" },
		"pilot_kind":         func(value *Config) { value.PilotKind = "synthetic_smoke" },
		"fresh_root":         func(value *Config) { value.FreshRootPerSample = false },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := config
			mutate(&mutated)
			if err := mutated.Validate(""); err == nil {
				t.Fatal("non-reviewed P68 diagnosis config was accepted")
			}
		})
	}

	t.Setenv(p68CliffDiagnosisEnv, "")
	if err := config.Validate("concurrency"); err == nil {
		t.Fatal("30-sample pilot was accepted without the P68 diagnosis marker")
	}
}
