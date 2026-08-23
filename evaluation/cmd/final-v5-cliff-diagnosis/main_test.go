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

func writeCallbackPhaseLogForTest(t *testing.T, records []control.CallbackPhaseTimingV1, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "callback-phases.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(file, `{"time":"2026-08-23T00:00:00Z","level":"INFO","msg":"gateway listening"}`); err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	for _, line := range extra {
		if _, err := fmt.Fprintln(file, line); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func inFlightCallbackPhaseRecordForTest(taskID string, position int, currentPhase string, ageMS float64, index int) control.CallbackPhaseTimingV1 {
	record := callbackPhaseRecordForTest(taskID, position, false)
	record.FinalResult = "in_flight"
	record.ErrorClass = "none"
	record.SnapshotReason = "stall_threshold"
	record.SnapshotAt = time.Date(2026, 8, 23, 0, 30, index, 0, time.UTC).Format(time.RFC3339Nano)
	record.InFlightAgeMS = ageMS
	record.CurrentPhase = currentPhase
	record.SnapshotIndex = index
	for i := range record.Phases {
		if record.Phases[i].Name == currentPhase {
			record.Phases[i] = control.CallbackPhaseTimingPhaseV1{Name: currentPhase, Attempted: true,
				StartedOffsetMS: 6, FinishedOffsetMS: 6 + ageMS, DurationMS: ageMS, Result: "in_progress"}
		} else if record.Phases[i].Name == "commit" {
			record.Phases[i] = control.CallbackPhaseTimingPhaseV1{Name: "commit", Result: "not_attempted"}
		}
	}
	return record
}

func TestCallbackPhaseInFlightSnapshotsAttributeHungPhase(t *testing.T) {
	tasks := make(map[string]migrationTask)
	var migrations []sampleMigration
	var records []control.CallbackPhaseTimingV1
	for position := 1; position <= 4; position++ {
		taskID := fmt.Sprintf("task-%d", position)
		tasks[control.CallbackPhaseTaskIDSHA256(taskID)] = migrationTask{SampleID: fmt.Sprintf("sample-%d", position),
			CellID: "shared-root/10/natural_contention", OrderPosition: position, TaskOrdinal: 1, TaskRole: "root"}
		migration := sampleMigration{SampleID: fmt.Sprintf("sample-%d", position), OrderPosition: position}
		switch {
		case position <= 2:
			records = append(records, callbackPhaseRecordForTest(taskID, position, false))
		case position == 3:
			// Hung callback: two stall snapshots, the later one wins; no final record.
			migration.Submission.Timeouts = 1
			records = append(records, inFlightCallbackPhaseRecordForTest(taskID, position, "audit_chain_head", 12_000, 1),
				inFlightCallbackPhaseRecordForTest(taskID, position, "audit_chain_head", 1_500_000, 2))
		default:
			// Timed out but never reached the Gateway: no record at all.
			migration.Submission.Timeouts = 1
		}
		migrations = append(migrations, migration)
	}
	path := writeCallbackPhaseLogForTest(t, records,
		`{"schema_version":1,"record":"taskgate-control-pool-snapshot-v1","observed_at":"2026-08-23T00:30:00Z","reason":"periodic","pool_open_connections":32,"pool_in_use":32,"pool_idle":0,"pool_max_open":32,"pool_wait_count":9,"pool_wait_duration_ms":4200.5,"in_flight_callbacks":1,"oldest_in_flight_age_ms":1500000,"stalled_in_flight":1,"stalled_threshold_ms":10000,"in_flight_current_phase":{"audit_chain_head":1}}`)
	curve, linked, err := readCallbackPhases(path, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if linked != 4 || len(curve) != 3 || curve[2].OrderPosition != 3 || curve[2].CallbackCount != 0 ||
		curve[2].InFlightRecords != 2 || curve[2].InFlightMaxAgeMS != 1_500_000 ||
		curve[2].Phases["audit_chain_head"].InFlightMaxMS != 1_500_000 || curve[2].Phases["audit_chain_head"].Count != 0 ||
		curve[2].inFlightCurrentPhase() != "audit_chain_head" || len(curve[2].InFlight) != 1 {
		t.Fatalf("in-flight curve = %+v linked=%d", curve, linked)
	}
	verdict := diagnoseCallbackPhaseCliff(curve, migrations)
	if verdict.Verdict != callbackPhaseVerdictReproduced || verdict.StuckPhase != "audit_chain_head" ||
		verdict.Attribution != "in_flight_snapshots" || verdict.InFlightTailOperations != 1 ||
		verdict.UnobservedTimeoutOperations != 1 || verdict.TimeoutOperations != 2 ||
		verdict.TimeoutTailMaxMS != 1_500_000 || verdict.InFlightMaxAgeMS != 1_500_000 ||
		verdict.FirstTimeoutOrderPosition != 3 || verdict.LastTimeoutOrderPosition != 4 {
		t.Fatalf("in-flight verdict = %+v", verdict)
	}
	pools, count, err := readPoolSnapshots(path)
	if err != nil || count != 1 {
		t.Fatalf("pool snapshots = %d, %v", count, err)
	}
	summary := summarizePoolSnapshots(pools)
	if summary.MaxInUse != 32 || summary.PoolMaxOpen != 32 || summary.PoolExhaustedSnaps != 1 || summary.MaxWaitCount != 9 ||
		summary.MaxStalledInFlight != 1 || summary.MaxOldestInFlightMS != 1_500_000 {
		t.Fatalf("pool summary = %+v", summary)
	}
}

func TestCallbackPhaseVerdictNotObservedAtGatewayAndCoverageRule(t *testing.T) {
	tasks := make(map[string]migrationTask)
	var migrations []sampleMigration
	var records []control.CallbackPhaseTimingV1
	for position := 1; position <= 4; position++ {
		taskID := fmt.Sprintf("task-%d", position)
		tasks[control.CallbackPhaseTaskIDSHA256(taskID)] = migrationTask{SampleID: fmt.Sprintf("sample-%d", position),
			CellID: "shared-root/10/natural_contention", OrderPosition: position, TaskOrdinal: 1, TaskRole: "root"}
		migration := sampleMigration{SampleID: fmt.Sprintf("sample-%d", position), OrderPosition: position}
		if position <= 2 {
			records = append(records, callbackPhaseRecordForTest(taskID, position, false))
		} else {
			migration.Submission.Timeouts = 1
		}
		migrations = append(migrations, migration)
	}
	curve, _, err := readCallbackPhases(writeCallbackPhaseLogForTest(t, records), tasks)
	if err != nil {
		t.Fatal(err)
	}
	verdict := diagnoseCallbackPhaseCliff(curve, migrations)
	if verdict.Verdict != callbackPhaseVerdictNotObserved || verdict.StuckPhase != stuckPhaseNotObserved ||
		verdict.UnobservedTimeoutOperations != 2 || verdict.InFlightTailOperations != 0 || verdict.TimeoutOperations != 2 ||
		verdict.TimeoutTailMaxMS != 0 || verdict.PreCliffP95MS == 0 {
		t.Fatalf("not-observed verdict = %+v", verdict)
	}

	// A completed (non-timeout) operation without any record is still data loss.
	migrations[1].Submission.Timeouts = 0
	curveMissing, _, err := readCallbackPhases(writeCallbackPhaseLogForTest(t, records[:1]), tasks)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[int]bool{}
	for _, operation := range curveMissing {
		covered[operation.OrderPosition] = true
	}
	missingCompleted := 0
	for _, migration := range migrations {
		if !covered[migration.OrderPosition] && migration.Submission.Timeouts+migration.Activation.Timeouts == 0 {
			missingCompleted++
		}
	}
	if missingCompleted != 1 {
		t.Fatalf("coverage rule missed a completed operation without records: %d", missingCompleted)
	}
}

func TestCallbackPhaseInFlightValidationFailsClosed(t *testing.T) {
	good := inFlightCallbackPhaseRecordForTest("task-ok", 1, "task_row_lock", 20_000, 1)
	if _, err := validateCallbackPhaseRecord(good); err != nil {
		t.Fatalf("valid in-flight snapshot was rejected: %v", err)
	}
	twoInProgress := inFlightCallbackPhaseRecordForTest("task-two", 1, "task_row_lock", 20_000, 1)
	twoInProgress.Phases[0].Result = "in_progress"
	if _, err := validateCallbackPhaseRecord(twoInProgress); err == nil {
		t.Fatal("in-flight snapshot with two in-progress phases was accepted")
	}
	wrongCurrent := inFlightCallbackPhaseRecordForTest("task-wrong", 1, "task_row_lock", 20_000, 1)
	wrongCurrent.CurrentPhase = "commit"
	if _, err := validateCallbackPhaseRecord(wrongCurrent); err == nil {
		t.Fatal("in-flight snapshot whose in-progress phase is not the current phase was accepted")
	}
	badReason := inFlightCallbackPhaseRecordForTest("task-reason", 1, "task_row_lock", 20_000, 1)
	badReason.SnapshotReason = "whenever"
	if _, err := validateCallbackPhaseRecord(badReason); err == nil {
		t.Fatal("in-flight snapshot with an unknown reason was accepted")
	}
	finishedWithSnapshotFields := callbackPhaseRecordForTest("task-final", 1, false)
	finishedWithSnapshotFields.CurrentPhase = "commit"
	if _, err := validateCallbackPhaseRecord(finishedWithSnapshotFields); err == nil {
		t.Fatal("finished record carrying snapshot fields was accepted")
	}
	inProgressOutsideInFlight := callbackPhaseRecordForTest("task-progress", 1, false)
	inProgressOutsideInFlight.Phases[1].Result = "in_progress"
	if _, err := validateCallbackPhaseRecord(inProgressOutsideInFlight); err == nil {
		t.Fatal("finished record with an in-progress phase was accepted")
	}
}

func TestCallbackPhaseVerdictNotReproducedClosesCleanRun(t *testing.T) {
	phases := []operationCallbackPhases{
		{OrderPosition: 1, CallbackCount: 2, Phases: map[string]callbackPhaseAggregate{"audit_chain_head": {Count: 2, MaxMS: 5}}},
		{OrderPosition: 2, CallbackCount: 2, Phases: map[string]callbackPhaseAggregate{"audit_chain_head": {Count: 2, MaxMS: 6}}},
	}
	migrations := []sampleMigration{{OrderPosition: 1}, {OrderPosition: 2}}
	verdict := diagnoseCallbackPhaseCliff(phases, migrations)
	if verdict.Verdict != "callback_phase_cliff_not_reproduced" || verdict.StuckPhase != "none" ||
		verdict.Attribution != "no_migration_timeouts" || verdict.TimeoutOperations != 0 ||
		verdict.FirstTimeoutOrderPosition != 0 || verdict.LastTimeoutOrderPosition != 0 {
		t.Fatalf("clean fix-confirmation run verdict = %+v", verdict)
	}
}
