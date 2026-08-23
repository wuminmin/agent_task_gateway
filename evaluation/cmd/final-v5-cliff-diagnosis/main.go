package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/control"
)

const diagnosisMarker = "DIAGNOSIS-NOT-FOR-PUBLICATION"

type observerSnapshot struct {
	SchemaVersion  int                `json:"schema_version"`
	Record         string             `json:"record"`
	Classification string             `json:"classification"`
	Sequence       int                `json:"sequence"`
	ObservedAt     string             `json:"observed_at"`
	Metrics        map[string]float64 `json:"metrics"`
}

type sampleMigration struct {
	SampleID      string
	CellID        string
	OrderPosition int
	Warmup        bool
	ObservedAt    time.Time
	TaskHashes    map[string]bool
	Submission    waitAggregate
	Activation    waitAggregate
	SampleStatus  string
	ErrorCode     string
}

type waitAggregate struct {
	Count    int
	Timeouts int
	SumMS    float64
	MaxMS    float64
}

type migrationTask struct {
	SampleID      string
	CellID        string
	OrderPosition int
	Warmup        bool
	TaskOrdinal   int
	TaskRole      string
}

type callbackPhaseAggregate struct {
	Count  int
	Errors int
	SumMS  float64
	MaxMS  float64
	// InFlightMaxMS is the longest elapsed time an in-flight snapshot reported
	// for this phase while it was still in progress. Completed aggregates above
	// never include it; the verdict uses it to attribute a hung callback.
	InFlightMaxMS float64
}

// inFlightObservation is the latest in-flight snapshot seen for one task.
type inFlightObservation struct {
	CurrentPhase string
	AgeMS        float64
	SnapshotAt   time.Time
}

type operationCallbackPhases struct {
	SampleID         string
	CellID           string
	OrderPosition    int
	Warmup           bool
	CallbackCount    int
	FinalErrors      int
	ObservedAt       time.Time
	Phases           map[string]callbackPhaseAggregate
	InFlightRecords  int
	InFlightMaxAgeMS float64
	InFlight         map[string]inFlightObservation
}

// inFlightCurrentPhase is the modal current phase over the latest in-flight
// snapshot of every task in the operation, or "" when none is in flight.
func (operation operationCallbackPhases) inFlightCurrentPhase() string {
	counts := make(map[string]int, len(operation.InFlight))
	for _, observation := range operation.InFlight {
		counts[observation.CurrentPhase]++
	}
	best, bestCount := "", 0
	for _, name := range callbackPhaseNames {
		if counts[name] > bestCount {
			best, bestCount = name, counts[name]
		}
	}
	for name, count := range counts {
		if count > bestCount {
			best, bestCount = name, count
		}
	}
	return best
}

const (
	callbackPhaseVerdictReproduced    = "callback_phase_cliff_reproduced"
	callbackPhaseVerdictNotAttributed = "callback_phase_cliff_not_attributed"
	// callbackPhaseVerdictNotObserved means every timed-out operation has no
	// phase record at all, completed or in flight: the Gateway never started a
	// submitted-callback transaction for those tasks while the stall timing was
	// armed, so the wedge sits before ApplyApprovalCallback or outside the Gateway.
	callbackPhaseVerdictNotObserved = "callback_not_observed_at_gateway"
	stuckPhaseNotObserved           = "not_observed_at_gateway"
)

type callbackPhaseVerdict struct {
	Verdict                      string  `json:"verdict"`
	StuckPhase                   string  `json:"stuck_phase"`
	Attribution                  string  `json:"attribution"`
	FirstTimeoutOrderPosition    int     `json:"first_timeout_order_position"`
	LastTimeoutOrderPosition     int     `json:"last_timeout_order_position"`
	PreCliffP95MS                float64 `json:"pre_cliff_p95_ms"`
	TimeoutTailMaxMS             float64 `json:"timeout_tail_max_ms"`
	TimeoutTailStalledOperations int     `json:"timeout_tail_stalled_operations"`
	TimeoutOperations            int     `json:"timeout_operations"`
	InFlightTailOperations       int     `json:"in_flight_timeout_operations"`
	InFlightMaxAgeMS             float64 `json:"in_flight_max_age_ms"`
	UnobservedTimeoutOperations  int     `json:"unobserved_timeout_operations"`
}

// poolSnapshotSummary is the condensed view of the periodic Control pool
// snapshots on the Gateway diagnostic stream.
type poolSnapshotSummary struct {
	Snapshots           int     `json:"snapshots"`
	PoolMaxOpen         int     `json:"pool_max_open"`
	MaxInUse            int     `json:"max_in_use"`
	MaxWaitCount        int64   `json:"max_wait_count"`
	MaxWaitDurationMS   float64 `json:"max_wait_duration_ms"`
	MaxInFlight         int     `json:"max_in_flight_callbacks"`
	MaxStalledInFlight  int     `json:"max_stalled_in_flight"`
	MaxOldestInFlightMS float64 `json:"max_oldest_in_flight_age_ms"`
	PoolExhaustedSnaps  int     `json:"pool_exhausted_snapshots"`
}

type correlation struct {
	Metric       string  `json:"metric"`
	Pearson      float64 `json:"pearson"`
	Observations int     `json:"observations"`
}

func main() {
	flags := flag.NewFlagSet("final-v5-cliff-diagnosis", flag.ContinueOnError)
	samplesPath := flags.String("samples", "", "retained profile-campaign sample JSONL")
	migrationPath := flags.String("migration", "", "credential-gated adapter diagnostic JSONL")
	observerPath := flags.String("observer", "", "read-only observer JSONL")
	summaryPath := flags.String("summary", "", "create-exclusive diagnosis summary")
	migrationCSV := flags.String("migration-curve", "", "create-exclusive per-sample migration curve CSV")
	stateCSV := flags.String("state-curve", "", "create-exclusive observer state curve CSV")
	correlationPath := flags.String("correlation", "", "create-exclusive correlation JSON")
	callbackPhasesPath := flags.String("callback-phases", "", "retained Gateway callback phase JSONL")
	callbackPhaseCSV := flags.String("callback-phase-curve", "", "create-exclusive per-operation callback phase curve CSV")
	poolCurveCSV := flags.String("pool-curve", "", "create-exclusive Control pool snapshot curve CSV (optional, needs -callback-phases)")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	for _, path := range []string{*samplesPath, *migrationPath, *observerPath, *summaryPath, *migrationCSV, *stateCSV, *correlationPath} {
		if strings.TrimSpace(path) == "" {
			fmt.Fprintln(os.Stderr, "all diagnosis input/output paths are required")
			os.Exit(2)
		}
	}
	if (*callbackPhasesPath == "") != (*callbackPhaseCSV == "") {
		fmt.Fprintln(os.Stderr, "callback phase input and output must be supplied together")
		os.Exit(2)
	}
	if *poolCurveCSV != "" && *callbackPhasesPath == "" {
		fmt.Fprintln(os.Stderr, "pool curve output requires the callback phase input")
		os.Exit(2)
	}
	reproduced, err := diagnose(*samplesPath, *migrationPath, *observerPath, *summaryPath, *migrationCSV, *stateCSV,
		*correlationPath, *callbackPhasesPath, *callbackPhaseCSV, *poolCurveCSV)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if !reproduced {
		fmt.Fprintln(os.Stderr, "diagnostic deployment completed without reproducing a migration timeout")
		os.Exit(1)
	}
}

func diagnose(samplesPath, migrationPath, observerPath, summaryPath, migrationCSV, stateCSV,
	correlationPath, callbackPhasesPath, callbackPhaseCSV, poolCurveCSV string) (bool, error) {
	records, err := experiment.ReadProfileCampaignSamples(samplesPath)
	if err != nil {
		return false, err
	}
	samples, statusCounts, excludedWarmupRecords, err := selectMeasuredSamples(records)
	if err != nil {
		return false, err
	}
	migrations, timeoutRecords, migrationTasks, err := readMigrations(migrationPath, samples)
	if err != nil {
		return false, err
	}
	observers, err := readObservers(observerPath)
	if err != nil {
		return false, err
	}
	if len(samples) != 270 {
		return false, fmt.Errorf("diagnosis retained %d measured samples, want frozen density 270", len(samples))
	}
	var phaseVerdict *callbackPhaseVerdict
	var poolSummary *poolSnapshotSummary
	callbackPhaseRecords := 0
	if callbackPhasesPath != "" {
		phaseCurve, phaseRecords, err := readCallbackPhases(callbackPhasesPath, migrationTasks)
		if err != nil {
			return false, err
		}
		// Every operation must be covered unless its callbacks timed out: a
		// hung callback that never reached the Gateway leaves no record by
		// construction, and that absence is itself reported by the verdict.
		// Missing records for an operation that completed are still data loss.
		covered := make(map[int]bool, len(phaseCurve))
		for _, operation := range phaseCurve {
			covered[operation.OrderPosition] = true
		}
		for _, migration := range migrations {
			if !covered[migration.OrderPosition] && migration.Submission.Timeouts+migration.Activation.Timeouts == 0 {
				return false, fmt.Errorf("callback phase data covers %d operations, want %d: completed operation %d has no record",
					len(phaseCurve), len(migrations), migration.OrderPosition)
			}
		}
		if err := writeCallbackPhaseCSV(callbackPhaseCSV, phaseCurve); err != nil {
			return false, err
		}
		verdict := diagnoseCallbackPhaseCliff(phaseCurve, migrations)
		phaseVerdict = &verdict
		callbackPhaseRecords = phaseRecords
		pools, poolRecords, err := readPoolSnapshots(callbackPhasesPath)
		if err != nil {
			return false, err
		}
		if poolCurveCSV != "" {
			if err := writePoolCurveCSV(poolCurveCSV, pools); err != nil {
				return false, err
			}
		}
		summary := summarizePoolSnapshots(pools)
		summary.Snapshots = poolRecords
		poolSummary = &summary
	}
	if err := writeMigrationCSV(migrationCSV, migrations); err != nil {
		return false, err
	}
	if err := writeStateCSV(stateCSV, observers); err != nil {
		return false, err
	}
	correlations := correlate(migrations, observers)
	correlationDocument := map[string]any{
		"schema_version": 1, "record": "taskgate-final-v5-cliff-correlation-v1",
		"classification": diagnosisMarker, "latency_axis": "max_of_submission_and_activation_wait_ms",
		"alignment": "nearest_observer_snapshot_at_or_before_wait_completion", "correlations": correlations,
	}
	if err := writeJSONExclusive(correlationPath, correlationDocument); err != nil {
		return false, err
	}
	firstTimeout, lastTimeout := 0, 0
	for _, migration := range migrations {
		if migration.Submission.Timeouts+migration.Activation.Timeouts == 0 {
			continue
		}
		if firstTimeout == 0 || migration.OrderPosition < firstTimeout {
			firstTimeout = migration.OrderPosition
		}
		if migration.OrderPosition > lastTimeout {
			lastTimeout = migration.OrderPosition
		}
	}
	reproduced := timeoutRecords > 0
	summary := map[string]any{
		"schema_version": 1, "record": "taskgate-final-v5-cliff-diagnosis-v1",
		"classification": diagnosisMarker, "status": "complete", "publication_eligible": false,
		"measured_samples": len(samples), "excluded_warmup_records": excludedWarmupRecords,
		"operation_records": len(migrations), "observer_snapshots": len(observers),
		"sample_status_counts": statusCounts, "migration_timeout_records": timeoutRecords,
		"first_timeout_order_position": firstTimeout, "last_timeout_order_position": lastTimeout,
		"cliff_reproduced": reproduced,
		"migration_curve":  filepath.Base(migrationCSV), "state_curve": filepath.Base(stateCSV),
		"correlation": filepath.Base(correlationPath),
	}
	if phaseVerdict != nil {
		summary["schema_version"] = 3
		summary["record"] = "taskgate-final-v5-callback-phase-diagnosis-v3"
		summary["callback_phase_records"] = callbackPhaseRecords
		summary["callback_phase_curve"] = filepath.Base(callbackPhaseCSV)
		summary["callback_phase_verdict"] = phaseVerdict
		if poolSummary != nil {
			summary["control_pool_snapshots"] = poolSummary
			if poolCurveCSV != "" {
				summary["pool_curve"] = filepath.Base(poolCurveCSV)
			}
		}
		// Both attributed outcomes close the diagnosis: the hung phase is named,
		// or the Gateway provably never saw the timed-out callbacks. Only an
		// unattributed cliff leaves the question open.
		reproduced = reproduced && (phaseVerdict.Verdict == callbackPhaseVerdictReproduced ||
			phaseVerdict.Verdict == callbackPhaseVerdictNotObserved)
		summary["cliff_reproduced"] = reproduced
	}
	if err := writeJSONExclusive(summaryPath, summary); err != nil {
		return false, err
	}
	return reproduced, nil
}

func selectMeasuredSamples(records []experiment.ProfileCampaignSampleV1) (map[string]experiment.Sample, map[string]int, int, error) {
	samples := make(map[string]experiment.Sample, len(records))
	statusCounts := make(map[string]int)
	seen := make(map[string]bool, len(records))
	excludedWarmupRecords := 0
	for _, record := range records {
		sample := record.Sample
		if record.CampaignClass != "pilot" || sample.PublicationEligible || sample.ExperimentID != "concurrency" {
			return nil, nil, 0, errors.New("diagnosis samples are not non-publication concurrency records")
		}
		if seen[sample.SampleID] {
			return nil, nil, 0, errors.New("diagnosis samples contain a duplicate identity")
		}
		seen[sample.SampleID] = true
		if sample.Warmup {
			excludedWarmupRecords++
			continue
		}
		samples[sample.SampleID] = sample
		statusCounts[sample.Status]++
	}
	return samples, statusCounts, excludedWarmupRecords, nil
}

func readMigrations(path string, samples map[string]experiment.Sample) ([]sampleMigration, int, map[string]migrationTask, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, nil, err
	}
	defer file.Close()
	bySample := make(map[string]*sampleMigration)
	tasks := make(map[string]migrationTask)
	timeouts := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var members map[string]json.RawMessage
		if err := experiment.StrictJSON(line, &members); err != nil {
			return nil, 0, nil, err
		}
		var record string
		_ = json.Unmarshal(members["record"], &record)
		if record != experiment.TaskMigrationWaitDiagnosticV1Record {
			continue
		}
		var diagnostic experiment.TaskMigrationWaitDiagnosticV1
		if err := experiment.StrictJSON(line, &diagnostic); err != nil {
			return nil, 0, nil, err
		}
		if err := diagnostic.Validate(); err != nil {
			return nil, 0, nil, err
		}
		if diagnostic.CallbackTaskIDSHA256 != "" {
			identity := migrationTask{SampleID: diagnostic.SampleID, CellID: diagnostic.CellID,
				OrderPosition: diagnostic.OrderPosition, Warmup: diagnostic.Warmup,
				TaskOrdinal: diagnostic.TaskOrdinal, TaskRole: diagnostic.TaskRole}
			if existing, present := tasks[diagnostic.CallbackTaskIDSHA256]; present && existing != identity {
				return nil, 0, nil, errors.New("callback task identity joins different operations")
			}
			tasks[diagnostic.CallbackTaskIDSHA256] = identity
		}
		migration := bySample[diagnostic.SampleID]
		if migration == nil {
			migration = &sampleMigration{SampleID: diagnostic.SampleID, CellID: diagnostic.CellID,
				OrderPosition: diagnostic.OrderPosition, Warmup: diagnostic.Warmup, TaskHashes: make(map[string]bool)}
			if sample, present := samples[diagnostic.SampleID]; present {
				migration.SampleStatus, migration.ErrorCode = sample.Status, sample.ErrorCode
			} else if diagnostic.Warmup {
				migration.SampleStatus = "warmup_unretained"
			} else {
				return nil, 0, nil, errors.New("measured migration diagnostic has no retained sample")
			}
			bySample[diagnostic.SampleID] = migration
		}
		observedAt, _ := time.Parse(time.RFC3339Nano, diagnostic.ObservedAt)
		if observedAt.After(migration.ObservedAt) {
			migration.ObservedAt = observedAt
		}
		migration.TaskHashes[diagnostic.TaskIDHash] = true
		aggregate := &migration.Submission
		if diagnostic.ExpectedState == "ACTIVE" {
			aggregate = &migration.Activation
		}
		aggregate.Count++
		aggregate.SumMS += diagnostic.ElapsedMS
		aggregate.MaxMS = math.Max(aggregate.MaxMS, diagnostic.ElapsedMS)
		if diagnostic.Status == "timeout" {
			aggregate.Timeouts++
			timeouts++
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, nil, err
	}
	result := make([]sampleMigration, 0, len(bySample))
	for _, migration := range bySample {
		result = append(result, *migration)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OrderPosition < result[j].OrderPosition })
	if len(result) != 315 {
		return nil, 0, nil, fmt.Errorf("diagnosis retained migration data for %d operations, want 315", len(result))
	}
	return result, timeouts, tasks, nil
}

var callbackPhaseNames = [...]string{"callback_claim", "task_row_lock", "audit_chain_head", "commit"}

func readCallbackPhases(path string, tasks map[string]migrationTask) ([]operationCallbackPhases, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	byOrder := make(map[int]*operationCallbackPhases)
	linkedRecords := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var members map[string]json.RawMessage
		if err := experiment.StrictJSON(line, &members); err != nil {
			return nil, 0, err
		}
		var recordName string
		_ = json.Unmarshal(members["record"], &recordName)
		if recordName != control.CallbackPhaseTimingV1Record {
			continue
		}
		var record control.CallbackPhaseTimingV1
		if err := experiment.StrictJSON(line, &record); err != nil {
			return nil, 0, err
		}
		observedAt, err := validateCallbackPhaseRecord(record)
		if err != nil {
			return nil, 0, err
		}
		task, linked := tasks[record.TaskIDSHA256]
		if !linked {
			continue
		}
		operation := byOrder[task.OrderPosition]
		if operation == nil {
			operation = &operationCallbackPhases{SampleID: task.SampleID, CellID: task.CellID,
				OrderPosition: task.OrderPosition, Warmup: task.Warmup,
				Phases:   make(map[string]callbackPhaseAggregate, len(callbackPhaseNames)),
				InFlight: make(map[string]inFlightObservation)}
			byOrder[task.OrderPosition] = operation
		} else if operation.SampleID != task.SampleID || operation.CellID != task.CellID || operation.Warmup != task.Warmup {
			return nil, 0, errors.New("callback phase order joins different operations")
		}
		if record.FinalResult == callbackPhaseFinalInFlight {
			// A snapshot of a callback that has not finished: keep the latest
			// observation per task and fold the in-progress elapsed time into
			// the phase it is stuck in, separately from completed durations.
			snapshotAt, _ := time.Parse(time.RFC3339Nano, record.SnapshotAt)
			operation.InFlightRecords++
			linkedRecords++
			if record.InFlightAgeMS > operation.InFlightMaxAgeMS {
				operation.InFlightMaxAgeMS = record.InFlightAgeMS
			}
			if previous, seen := operation.InFlight[record.TaskIDSHA256]; !seen || snapshotAt.After(previous.SnapshotAt) {
				operation.InFlight[record.TaskIDSHA256] = inFlightObservation{CurrentPhase: record.CurrentPhase,
					AgeMS: record.InFlightAgeMS, SnapshotAt: snapshotAt}
			}
			for _, phase := range record.Phases {
				if phase.Attempted && phase.Result == callbackPhaseResultInProgress {
					aggregate := operation.Phases[phase.Name]
					aggregate.InFlightMaxMS = math.Max(aggregate.InFlightMaxMS, phase.DurationMS)
					operation.Phases[phase.Name] = aggregate
				}
			}
			if snapshotAt.After(operation.ObservedAt) {
				operation.ObservedAt = snapshotAt
			}
			continue
		}
		operation.CallbackCount++
		if record.FinalResult == "error" {
			operation.FinalErrors++
		}
		if observedAt.After(operation.ObservedAt) {
			operation.ObservedAt = observedAt
		}
		for _, phase := range record.Phases {
			if !phase.Attempted {
				continue
			}
			aggregate := operation.Phases[phase.Name]
			aggregate.Count++
			aggregate.SumMS += phase.DurationMS
			aggregate.MaxMS = math.Max(aggregate.MaxMS, phase.DurationMS)
			if phase.Result == "error" {
				aggregate.Errors++
			}
			operation.Phases[phase.Name] = aggregate
		}
		linkedRecords++
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	result := make([]operationCallbackPhases, 0, len(byOrder))
	for _, operation := range byOrder {
		result = append(result, *operation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OrderPosition < result[j].OrderPosition })
	return result, linkedRecords, nil
}

const (
	callbackPhaseFinalInFlight    = "in_flight"
	callbackPhaseResultInProgress = "in_progress"
)

var inFlightSnapshotReasons = map[string]bool{"stall_threshold": true, "shutdown": true, "store_close": true}

func validateCallbackPhaseRecord(record control.CallbackPhaseTimingV1) (time.Time, error) {
	observedAt, timestampErr := time.Parse(time.RFC3339Nano, record.ObservedAt)
	inFlight := record.FinalResult == callbackPhaseFinalInFlight
	validFinal := record.FinalResult == "committed" || record.FinalResult == "replay_committed" ||
		record.FinalResult == "error" || inFlight
	validErrorClass := record.ErrorClass == "none" || record.ErrorClass == "context_deadline_exceeded" ||
		record.ErrorClass == "context_cancelled" || record.ErrorClass == "other"
	if record.SchemaVersion != 1 || record.Record != control.CallbackPhaseTimingV1Record ||
		strings.TrimSpace(record.TaskID) == "" || record.TaskIDSHA256 != control.CallbackPhaseTaskIDSHA256(record.TaskID) ||
		strings.TrimSpace(record.EventID) == "" || timestampErr != nil || !validFinal || !validErrorClass ||
		len(record.Phases) != len(callbackPhaseNames) ||
		(record.FinalResult == "error") != (record.ErrorClass != "none") {
		return time.Time{}, errors.New("invalid submitted-callback phase timing record")
	}
	if inFlight {
		_, snapshotErr := time.Parse(time.RFC3339Nano, record.SnapshotAt)
		validCurrent := record.CurrentPhase == "before_first_phase" || record.CurrentPhase == "between_phases"
		for _, name := range callbackPhaseNames {
			validCurrent = validCurrent || record.CurrentPhase == name
		}
		if !inFlightSnapshotReasons[record.SnapshotReason] || snapshotErr != nil || record.InFlightAgeMS < 0 ||
			!validCurrent || record.SnapshotIndex < 1 {
			return time.Time{}, errors.New("invalid in-flight callback phase snapshot")
		}
	} else if record.SnapshotReason != "" || record.SnapshotAt != "" || record.InFlightAgeMS != 0 ||
		record.CurrentPhase != "" || record.SnapshotIndex != 0 {
		return time.Time{}, errors.New("finished callback phase record carries snapshot fields")
	}
	inProgress := 0
	for index, phase := range record.Phases {
		if phase.Name != callbackPhaseNames[index] || phase.StartedOffsetMS < 0 || phase.FinishedOffsetMS < 0 ||
			phase.DurationMS < 0 || phase.FinishedOffsetMS < phase.StartedOffsetMS {
			return time.Time{}, errors.New("invalid submitted-callback phase member")
		}
		if phase.Attempted {
			switch phase.Result {
			case "ok", "error":
			case callbackPhaseResultInProgress:
				inProgress++
				if !inFlight || phase.Name != record.CurrentPhase {
					return time.Time{}, errors.New("in-progress callback phase outside an in-flight snapshot or not the current phase")
				}
			default:
				return time.Time{}, errors.New("attempted callback phase has an invalid result")
			}
		} else if phase.Result != "not_attempted" || phase.StartedOffsetMS != 0 ||
			phase.FinishedOffsetMS != 0 || phase.DurationMS != 0 {
			return time.Time{}, errors.New("unattempted callback phase carries timing")
		}
	}
	if inProgress > 1 {
		return time.Time{}, errors.New("in-flight snapshot reports more than one phase in progress")
	}
	return observedAt, nil
}

// readPoolSnapshots collects the periodic Control pool snapshots emitted on the
// same diagnostic stream. Unknown lines are skipped exactly as the phase reader
// does; a malformed pool snapshot fails closed.
func readPoolSnapshots(path string) ([]control.CallbackPoolSnapshotV1, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	var pools []control.CallbackPoolSnapshotV1
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var members map[string]json.RawMessage
		if err := experiment.StrictJSON(line, &members); err != nil {
			return nil, 0, err
		}
		var recordName string
		_ = json.Unmarshal(members["record"], &recordName)
		if recordName != control.CallbackPoolSnapshotV1Record {
			continue
		}
		var record control.CallbackPoolSnapshotV1
		if err := experiment.StrictJSON(line, &record); err != nil {
			return nil, 0, err
		}
		if _, err := time.Parse(time.RFC3339Nano, record.ObservedAt); err != nil || record.SchemaVersion != 1 ||
			record.PoolMaxOpen < 0 || record.PoolInUse < 0 || record.InFlightCallbacks < 0 {
			return nil, 0, errors.New("invalid Control pool snapshot record")
		}
		pools = append(pools, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}
	return pools, len(pools), nil
}

func summarizePoolSnapshots(pools []control.CallbackPoolSnapshotV1) poolSnapshotSummary {
	var summary poolSnapshotSummary
	for _, pool := range pools {
		summary.PoolMaxOpen = pool.PoolMaxOpen
		if pool.PoolInUse > summary.MaxInUse {
			summary.MaxInUse = pool.PoolInUse
		}
		if pool.PoolWaitCount > summary.MaxWaitCount {
			summary.MaxWaitCount = pool.PoolWaitCount
		}
		summary.MaxWaitDurationMS = math.Max(summary.MaxWaitDurationMS, pool.PoolWaitDurationMS)
		if pool.InFlightCallbacks > summary.MaxInFlight {
			summary.MaxInFlight = pool.InFlightCallbacks
		}
		if pool.StalledInFlight > summary.MaxStalledInFlight {
			summary.MaxStalledInFlight = pool.StalledInFlight
		}
		summary.MaxOldestInFlightMS = math.Max(summary.MaxOldestInFlightMS, pool.OldestInFlightAgeMS)
		if pool.PoolMaxOpen > 0 && pool.PoolInUse >= pool.PoolMaxOpen {
			summary.PoolExhaustedSnaps++
		}
	}
	return summary
}

func writePoolCurveCSV(path string, pools []control.CallbackPoolSnapshotV1) error {
	file, err := exclusiveFile(path)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	_ = writer.Write([]string{"observed_at", "reason", "pool_max_open", "pool_open", "pool_in_use", "pool_idle",
		"pool_wait_count", "pool_wait_duration_ms", "in_flight_callbacks", "stalled_in_flight", "oldest_in_flight_age_ms",
		"in_flight_current_phase"})
	for _, pool := range pools {
		phases := make([]string, 0, len(pool.InFlightCurrentPhase))
		for name, count := range pool.InFlightCurrentPhase {
			phases = append(phases, name+"="+strconv.Itoa(count))
		}
		sort.Strings(phases)
		_ = writer.Write([]string{pool.ObservedAt, pool.Reason, strconv.Itoa(pool.PoolMaxOpen), strconv.Itoa(pool.PoolOpen),
			strconv.Itoa(pool.PoolInUse), strconv.Itoa(pool.PoolIdle), strconv.FormatInt(pool.PoolWaitCount, 10),
			formatFloat(pool.PoolWaitDurationMS), strconv.Itoa(pool.InFlightCallbacks), strconv.Itoa(pool.StalledInFlight),
			formatFloat(pool.OldestInFlightAgeMS), strings.Join(phases, ";")})
	}
	writer.Flush()
	closeErr := file.Close()
	if err := writer.Error(); err != nil {
		return err
	}
	return closeErr
}

func diagnoseCallbackPhaseCliff(phases []operationCallbackPhases, migrations []sampleMigration) callbackPhaseVerdict {
	timeoutOrders := make(map[int]bool)
	firstTimeout, lastTimeout := 0, 0
	for _, migration := range migrations {
		if migration.Submission.Timeouts+migration.Activation.Timeouts == 0 {
			continue
		}
		timeoutOrders[migration.OrderPosition] = true
		if firstTimeout == 0 || migration.OrderPosition < firstTimeout {
			firstTimeout = migration.OrderPosition
		}
		if migration.OrderPosition > lastTimeout {
			lastTimeout = migration.OrderPosition
		}
	}
	verdict := callbackPhaseVerdict{Verdict: callbackPhaseVerdictNotAttributed, Attribution: "none",
		FirstTimeoutOrderPosition: firstTimeout, LastTimeoutOrderPosition: lastTimeout, TimeoutOperations: len(timeoutOrders)}
	// Tail coverage: a timed-out operation is observed when any completed or
	// in-flight record joined it; the rest never reached ApplyApprovalCallback.
	observedTail := make(map[int]bool, len(timeoutOrders))
	for _, operation := range phases {
		if !timeoutOrders[operation.OrderPosition] {
			continue
		}
		observed := operation.CallbackCount > 0 || operation.InFlightRecords > 0
		for _, aggregate := range operation.Phases {
			observed = observed || aggregate.Count > 0 || aggregate.InFlightMaxMS > 0
		}
		if observed {
			observedTail[operation.OrderPosition] = true
		}
		if operation.InFlightRecords > 0 {
			verdict.InFlightTailOperations++
			verdict.InFlightMaxAgeMS = math.Max(verdict.InFlightMaxAgeMS, operation.InFlightMaxAgeMS)
		}
	}
	verdict.UnobservedTimeoutOperations = len(timeoutOrders) - len(observedTail)
	bestDelta := math.Inf(-1)
	bestInFlight := false
	for _, phaseName := range callbackPhaseNames {
		var preCliff, timeoutTail []float64
		tailInFlight := false
		for _, operation := range phases {
			aggregate := operation.Phases[phaseName]
			if operation.OrderPosition < firstTimeout && aggregate.Count > 0 {
				preCliff = append(preCliff, aggregate.MaxMS)
			}
			if timeoutOrders[operation.OrderPosition] {
				// A hung callback contributes the elapsed time of the phase it
				// was snapshotted in; a completed slow callback its duration.
				value := aggregate.MaxMS
				if aggregate.InFlightMaxMS > value {
					value = aggregate.InFlightMaxMS
					tailInFlight = true
				}
				if aggregate.Count > 0 || aggregate.InFlightMaxMS > 0 {
					timeoutTail = append(timeoutTail, value)
				}
			}
		}
		preP95 := percentile(preCliff, 0.95)
		tailMax := maximum(timeoutTail)
		delta := tailMax - preP95
		if delta > bestDelta {
			bestDelta = delta
			bestInFlight = tailInFlight
			verdict.StuckPhase = phaseName
			verdict.PreCliffP95MS = preP95
			verdict.TimeoutTailMaxMS = tailMax
			verdict.TimeoutTailStalledOperations = 0
			for _, value := range timeoutTail {
				if value >= 10_000 {
					verdict.TimeoutTailStalledOperations++
				}
			}
		}
	}
	if firstTimeout > 0 && verdict.TimeoutTailStalledOperations > 0 && verdict.TimeoutTailMaxMS >= 10_000 &&
		verdict.TimeoutTailMaxMS >= verdict.PreCliffP95MS+5_000 &&
		(verdict.PreCliffP95MS == 0 || verdict.TimeoutTailMaxMS >= verdict.PreCliffP95MS*10) {
		verdict.Verdict = callbackPhaseVerdictReproduced
		verdict.Attribution = "completed_phase_durations"
		if bestInFlight {
			verdict.Attribution = "in_flight_snapshots"
		}
	} else if firstTimeout > 0 && len(observedTail) == 0 {
		// Every timed-out callback is absent from the Gateway's stream even
		// with stall snapshots armed: the wedge is not inside a callback phase.
		verdict.Verdict = callbackPhaseVerdictNotObserved
		verdict.StuckPhase = stuckPhaseNotObserved
		verdict.Attribution = "no_gateway_record_for_timed_out_callbacks"
		verdict.PreCliffP95MS, verdict.TimeoutTailMaxMS, verdict.TimeoutTailStalledOperations = 0, 0, 0
		for _, phaseName := range callbackPhaseNames {
			var preCliff []float64
			for _, operation := range phases {
				if aggregate := operation.Phases[phaseName]; operation.OrderPosition < firstTimeout && aggregate.Count > 0 {
					preCliff = append(preCliff, aggregate.MaxMS)
				}
			}
			verdict.PreCliffP95MS = math.Max(verdict.PreCliffP95MS, percentile(preCliff, 0.95))
		}
	}
	return verdict
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func maximum(values []float64) float64 {
	result := 0.0
	for _, value := range values {
		result = math.Max(result, value)
	}
	return result
}

func readObservers(path string) ([]observerSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var result []observerSnapshot
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var snapshot observerSnapshot
		if err := experiment.StrictJSON(scanner.Bytes(), &snapshot); err != nil {
			return nil, err
		}
		if snapshot.SchemaVersion != 1 || snapshot.Record != "taskgate-final-v5-cliff-observer-snapshot-v1" ||
			snapshot.Classification != diagnosisMarker || snapshot.Sequence != len(result)+1 || len(snapshot.Metrics) == 0 {
			return nil, errors.New("invalid cliff observer snapshot")
		}
		if _, err := time.Parse(time.RFC3339Nano, snapshot.ObservedAt); err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) < 2 {
		return nil, errors.New("cliff diagnosis requires at least two observer snapshots")
	}
	return result, nil
}

func writeMigrationCSV(path string, migrations []sampleMigration) error {
	file, err := exclusiveFile(path)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	_ = writer.Write([]string{"order_position", "sample_id", "cell_id", "warmup", "sample_status", "error_code",
		"task_count", "submission_count", "submission_sum_ms", "submission_max_ms", "submission_timeouts",
		"activation_count", "activation_sum_ms", "activation_max_ms", "activation_timeouts", "observed_at"})
	for _, item := range migrations {
		_ = writer.Write([]string{strconv.Itoa(item.OrderPosition), item.SampleID, item.CellID,
			strconv.FormatBool(item.Warmup), item.SampleStatus, item.ErrorCode, strconv.Itoa(len(item.TaskHashes)),
			strconv.Itoa(item.Submission.Count), formatFloat(item.Submission.SumMS), formatFloat(item.Submission.MaxMS), strconv.Itoa(item.Submission.Timeouts),
			strconv.Itoa(item.Activation.Count), formatFloat(item.Activation.SumMS), formatFloat(item.Activation.MaxMS), strconv.Itoa(item.Activation.Timeouts),
			item.ObservedAt.UTC().Format(time.RFC3339Nano)})
	}
	writer.Flush()
	closeErr := file.Close()
	if err := writer.Error(); err != nil {
		return err
	}
	return closeErr
}

func writeCallbackPhaseCSV(path string, operations []operationCallbackPhases) error {
	file, err := exclusiveFile(path)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	header := []string{"order_position", "sample_id", "cell_id", "warmup", "callback_records", "final_errors"}
	for _, phaseName := range callbackPhaseNames {
		header = append(header, phaseName+"_count", phaseName+"_sum_ms", phaseName+"_max_ms", phaseName+"_errors",
			phaseName+"_in_flight_max_ms")
	}
	header = append(header, "in_flight_records", "in_flight_tasks", "in_flight_max_age_ms", "in_flight_current_phase", "observed_at")
	_ = writer.Write(header)
	for _, operation := range operations {
		row := []string{strconv.Itoa(operation.OrderPosition), operation.SampleID, operation.CellID,
			strconv.FormatBool(operation.Warmup), strconv.Itoa(operation.CallbackCount), strconv.Itoa(operation.FinalErrors)}
		for _, phaseName := range callbackPhaseNames {
			aggregate := operation.Phases[phaseName]
			row = append(row, strconv.Itoa(aggregate.Count), formatFloat(aggregate.SumMS),
				formatFloat(aggregate.MaxMS), strconv.Itoa(aggregate.Errors), formatFloat(aggregate.InFlightMaxMS))
		}
		row = append(row, strconv.Itoa(operation.InFlightRecords), strconv.Itoa(len(operation.InFlight)),
			formatFloat(operation.InFlightMaxAgeMS), operation.inFlightCurrentPhase(),
			operation.ObservedAt.UTC().Format(time.RFC3339Nano))
		_ = writer.Write(row)
	}
	writer.Flush()
	closeErr := file.Close()
	if err := writer.Error(); err != nil {
		return err
	}
	return closeErr
}

func writeStateCSV(path string, snapshots []observerSnapshot) error {
	metricSet := make(map[string]bool)
	for _, snapshot := range snapshots {
		for metric := range snapshot.Metrics {
			metricSet[metric] = true
		}
	}
	metrics := make([]string, 0, len(metricSet))
	for metric := range metricSet {
		metrics = append(metrics, metric)
	}
	sort.Strings(metrics)
	file, err := exclusiveFile(path)
	if err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	_ = writer.Write(append([]string{"sequence", "observed_at"}, metrics...))
	for _, snapshot := range snapshots {
		row := []string{strconv.Itoa(snapshot.Sequence), snapshot.ObservedAt}
		for _, metric := range metrics {
			row = append(row, formatFloat(snapshot.Metrics[metric]))
		}
		_ = writer.Write(row)
	}
	writer.Flush()
	closeErr := file.Close()
	if err := writer.Error(); err != nil {
		return err
	}
	return closeErr
}

func correlate(migrations []sampleMigration, snapshots []observerSnapshot) []correlation {
	snapshotTimes := make([]time.Time, len(snapshots))
	for index := range snapshots {
		snapshotTimes[index], _ = time.Parse(time.RFC3339Nano, snapshots[index].ObservedAt)
	}
	metricValues := make(map[string][]float64)
	latencies := make(map[string][]float64)
	for _, migration := range migrations {
		index := sort.Search(len(snapshotTimes), func(index int) bool { return snapshotTimes[index].After(migration.ObservedAt) }) - 1
		if index < 0 {
			continue
		}
		latency := math.Max(migration.Submission.MaxMS, migration.Activation.MaxMS)
		for metric, value := range snapshots[index].Metrics {
			metricValues[metric] = append(metricValues[metric], value)
			latencies[metric] = append(latencies[metric], latency)
		}
	}
	result := make([]correlation, 0, len(metricValues))
	for metric, values := range metricValues {
		value, ok := pearson(values, latencies[metric])
		if ok {
			result = append(result, correlation{Metric: metric, Pearson: value, Observations: len(values)})
		}
	}
	sort.Slice(result, func(i, j int) bool { return math.Abs(result[i].Pearson) > math.Abs(result[j].Pearson) })
	return result
}

func pearson(x, y []float64) (float64, bool) {
	if len(x) != len(y) || len(x) < 2 {
		return 0, false
	}
	var sumX, sumY float64
	for index := range x {
		sumX, sumY = sumX+x[index], sumY+y[index]
	}
	meanX, meanY := sumX/float64(len(x)), sumY/float64(len(y))
	var numerator, squareX, squareY float64
	for index := range x {
		dx, dy := x[index]-meanX, y[index]-meanY
		numerator += dx * dy
		squareX += dx * dx
		squareY += dy * dy
	}
	if squareX == 0 || squareY == 0 {
		return 0, false
	}
	return numerator / math.Sqrt(squareX*squareY), true
}

func writeJSONExclusive(path string, value any) error {
	file, err := exclusiveFile(path)
	if err != nil {
		return err
	}
	encodeErr := json.NewEncoder(file).Encode(value)
	closeErr := file.Close()
	return errors.Join(encodeErr, closeErr)
}

func exclusiveFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

func formatFloat(value float64) string { return strconv.FormatFloat(value, 'f', 6, 64) }
