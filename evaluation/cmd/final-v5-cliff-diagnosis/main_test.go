package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/control"
)

func TestPearsonRanksSynchronousAndConstantMetrics(t *testing.T) {
	value, ok := pearson([]float64{1, 2, 3, 4}, []float64{10, 20, 30, 40})
	if !ok || math.Abs(value-1) > 1e-12 {
		t.Fatalf("pearson=%v ok=%v, want 1,true", value, ok)
	}
	if _, ok := pearson([]float64{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Fatal("constant diagnostic metric produced a correlation")
	}
}

func TestSelectMeasuredSamplesReportsAndExcludesWarmups(t *testing.T) {
	records := []experiment.ProfileCampaignSampleV1{
		{CampaignClass: "pilot", Sample: experiment.Sample{SampleID: "warmup-1", ExperimentID: "concurrency", Warmup: true, Status: "invalid"}},
		{CampaignClass: "pilot", Sample: experiment.Sample{SampleID: "measured-1", ExperimentID: "concurrency", Status: "pass"}},
	}
	samples, statuses, excluded, err := selectMeasuredSamples(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples["measured-1"].Warmup || statuses["pass"] != 1 || excluded != 1 {
		t.Fatalf("samples=%+v statuses=%+v excluded=%d", samples, statuses, excluded)
	}
}

func TestSelectMeasuredSamplesKeepsNonWarmupContractFailClosed(t *testing.T) {
	tests := []experiment.ProfileCampaignSampleV1{
		{CampaignClass: "publication", Sample: experiment.Sample{SampleID: "wrong-class", ExperimentID: "concurrency", PublicationEligible: true}},
		{CampaignClass: "pilot", Sample: experiment.Sample{SampleID: "wrong-experiment", ExperimentID: "baseline"}},
	}
	for _, record := range tests {
		if _, _, _, err := selectMeasuredSamples([]experiment.ProfileCampaignSampleV1{record}); err == nil {
			t.Fatalf("accepted invalid record: %+v", record)
		}
	}
}

func TestReadMigrationsRetainsFrozenDensityAndFindsTailTimeouts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	if _, err := fmt.Fprintln(file, "concurrency diagnostic progress"); err != nil {
		t.Fatal(err)
	}
	samples := make(map[string]experiment.Sample, 270)
	for position := 1; position <= 315; position++ {
		sampleID := fmt.Sprintf("sample-%04d", position)
		warmup := position <= 45
		operation := experiment.AdapterOperation{CampaignID: "p68", DeploymentID: "deployment-01",
			ExperimentID: "concurrency", CellID: "shared-root/500/natural_contention",
			SampleID: sampleID, OrderPosition: position, Warmup: warmup}
		if !warmup {
			status := "pass"
			if position >= 275 {
				status = "fail"
			}
			samples[sampleID] = experiment.Sample{SampleID: sampleID, Status: status, ErrorCode: ""}
		}
		first := experiment.NewTaskMigrationWaitDiagnosticV1(operation, 1, "root", "task-"+sampleID,
			"AWAITING_APPROVAL", "AWAITING_APPROVAL", "reached", time.Millisecond, 1, "none", time.Now())
		secondStatus, secondLast := "reached", "ACTIVE"
		if position >= 275 {
			secondStatus, secondLast = "timeout", "AWAITING_APPROVAL"
		}
		second := experiment.NewTaskMigrationWaitDiagnosticV1(operation, 1, "root", "task-"+sampleID,
			"ACTIVE", secondLast, secondStatus, 30*time.Second, 300, "none", time.Now())
		if err := encoder.Encode(first); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(second); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	migrations, timeouts, tasks, err := readMigrations(path, samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 315 || timeouts != 41 || len(tasks) != 315 || migrations[274].OrderPosition != 275 || migrations[274].Activation.Timeouts != 1 {
		t.Fatalf("migration curve operations=%d timeouts=%d tasks=%d first_tail=%+v", len(migrations), timeouts, len(tasks), migrations[274])
	}
}

func TestCallbackPhaseCurveJoinsOperationsAndAttributesAuditHeadCliff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "callback-phases.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(file, `{"time":"2026-08-21T00:00:00Z","level":"INFO","msg":"ordinary gateway log"}`); err != nil {
		t.Fatal(err)
	}
	tasks := make(map[string]migrationTask)
	var migrations []sampleMigration
	for position := 1; position <= 4; position++ {
		taskID := fmt.Sprintf("task-%d", position)
		taskHash := control.CallbackPhaseTaskIDSHA256(taskID)
		tasks[taskHash] = migrationTask{SampleID: fmt.Sprintf("sample-%d", position), CellID: "shared-root/10/natural_contention",
			OrderPosition: position, TaskOrdinal: 1, TaskRole: "root"}
		migration := sampleMigration{SampleID: fmt.Sprintf("sample-%d", position), OrderPosition: position}
		if position >= 3 {
			migration.Submission.Timeouts = 1
		}
		migrations = append(migrations, migration)
		record := callbackPhaseRecordForTest(taskID, position, position >= 3)
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	curve, records, err := readCallbackPhases(path, tasks)
	if err != nil {
		t.Fatal(err)
	}
	verdict := diagnoseCallbackPhaseCliff(curve, migrations)
	if records != 4 || len(curve) != 4 || curve[2].Phases["audit_chain_head"].MaxMS != 30_050 ||
		verdict.Verdict != "callback_phase_cliff_reproduced" || verdict.StuckPhase != "audit_chain_head" ||
		verdict.FirstTimeoutOrderPosition != 3 || verdict.LastTimeoutOrderPosition != 4 ||
		verdict.TimeoutTailStalledOperations != 2 {
		t.Fatalf("records=%d curve=%+v verdict=%+v", records, curve, verdict)
	}
}

func TestCallbackPhaseValidationAndVerdictFailClosed(t *testing.T) {
	record := callbackPhaseRecordForTest("task-negative", 1, false)
	record.TaskIDSHA256 = strings.Repeat("0", 64)
	if _, err := validateCallbackPhaseRecord(record); err == nil {
		t.Fatal("callback phase record with a self-inconsistent task identity was accepted")
	}
	phases := []operationCallbackPhases{
		{OrderPosition: 1, Phases: map[string]callbackPhaseAggregate{"audit_chain_head": {Count: 1, MaxMS: 90}}},
		{OrderPosition: 2, Phases: map[string]callbackPhaseAggregate{"audit_chain_head": {Count: 1, MaxMS: 110}}},
	}
	migrations := []sampleMigration{{OrderPosition: 1}, {OrderPosition: 2, Submission: waitAggregate{Timeouts: 1}}}
	verdict := diagnoseCallbackPhaseCliff(phases, migrations)
	if verdict.Verdict != "callback_phase_cliff_not_attributed" {
		t.Fatalf("sub-threshold phase movement was attributed as a cliff: %+v", verdict)
	}
}

func callbackPhaseRecordForTest(taskID string, position int, stuck bool) control.CallbackPhaseTimingV1 {
	phase := func(name string, duration float64) control.CallbackPhaseTimingPhaseV1 {
		return control.CallbackPhaseTimingPhaseV1{Name: name, Attempted: true,
			StartedOffsetMS: 1, FinishedOffsetMS: 1 + duration, DurationMS: duration, Result: "ok"}
	}
	record := control.CallbackPhaseTimingV1{
		SchemaVersion: 1, Record: control.CallbackPhaseTimingV1Record,
		TaskID: taskID, TaskIDSHA256: control.CallbackPhaseTaskIDSHA256(taskID), EventID: fmt.Sprintf("event-%d", position),
		ObservedAt: time.Date(2026, 8, 21, 0, 0, position, 0, time.UTC).Format(time.RFC3339Nano),
		Phases: []control.CallbackPhaseTimingPhaseV1{
			phase("callback_claim", 2), phase("task_row_lock", 3), phase("audit_chain_head", 4), phase("commit", 2),
		},
		FinalResult: "committed", ErrorClass: "none",
	}
	if stuck {
		record.Phases[2] = phase("audit_chain_head", 30_050)
		record.Phases[2].Result = "error"
		record.Phases[3] = control.CallbackPhaseTimingPhaseV1{Name: "commit", Result: "not_attempted"}
		record.FinalResult = "error"
		record.ErrorClass = "context_deadline_exceeded"
	}
	return record
}
