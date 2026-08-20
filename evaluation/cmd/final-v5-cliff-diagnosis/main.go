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
}

type operationCallbackPhases struct {
	SampleID      string
	CellID        string
	OrderPosition int
	Warmup        bool
	CallbackCount int
	FinalErrors   int
	ObservedAt    time.Time
	Phases        map[string]callbackPhaseAggregate
}

type callbackPhaseVerdict struct {
	Verdict                      string  `json:"verdict"`
	StuckPhase                   string  `json:"stuck_phase"`
	FirstTimeoutOrderPosition    int     `json:"first_timeout_order_position"`
	LastTimeoutOrderPosition     int     `json:"last_timeout_order_position"`
	PreCliffP95MS                float64 `json:"pre_cliff_p95_ms"`
	TimeoutTailMaxMS             float64 `json:"timeout_tail_max_ms"`
	TimeoutTailStalledOperations int     `json:"timeout_tail_stalled_operations"`
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
	reproduced, err := diagnose(*samplesPath, *migrationPath, *observerPath, *summaryPath, *migrationCSV, *stateCSV,
		*correlationPath, *callbackPhasesPath, *callbackPhaseCSV)
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
	correlationPath, callbackPhasesPath, callbackPhaseCSV string) (bool, error) {
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
	callbackPhaseRecords := 0
	if callbackPhasesPath != "" {
		phaseCurve, phaseRecords, err := readCallbackPhases(callbackPhasesPath, migrationTasks)
		if err != nil {
			return false, err
		}
		if len(phaseCurve) != len(migrations) {
			return false, fmt.Errorf("callback phase data covers %d operations, want %d", len(phaseCurve), len(migrations))
		}
		if err := writeCallbackPhaseCSV(callbackPhaseCSV, phaseCurve); err != nil {
			return false, err
		}
		verdict := diagnoseCallbackPhaseCliff(phaseCurve, migrations)
		phaseVerdict = &verdict
		callbackPhaseRecords = phaseRecords
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
		summary["schema_version"] = 2
		summary["record"] = "taskgate-final-v5-callback-phase-diagnosis-v2"
		summary["callback_phase_records"] = callbackPhaseRecords
		summary["callback_phase_curve"] = filepath.Base(callbackPhaseCSV)
		summary["callback_phase_verdict"] = phaseVerdict
		reproduced = reproduced && phaseVerdict.Verdict == "callback_phase_cliff_reproduced"
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
				Phases: make(map[string]callbackPhaseAggregate, len(callbackPhaseNames))}
			byOrder[task.OrderPosition] = operation
		} else if operation.SampleID != task.SampleID || operation.CellID != task.CellID || operation.Warmup != task.Warmup {
			return nil, 0, errors.New("callback phase order joins different operations")
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

func validateCallbackPhaseRecord(record control.CallbackPhaseTimingV1) (time.Time, error) {
	observedAt, timestampErr := time.Parse(time.RFC3339Nano, record.ObservedAt)
	validFinal := record.FinalResult == "committed" || record.FinalResult == "replay_committed" || record.FinalResult == "error"
	validErrorClass := record.ErrorClass == "none" || record.ErrorClass == "context_deadline_exceeded" ||
		record.ErrorClass == "context_cancelled" || record.ErrorClass == "other"
	if record.SchemaVersion != 1 || record.Record != control.CallbackPhaseTimingV1Record ||
		strings.TrimSpace(record.TaskID) == "" || record.TaskIDSHA256 != control.CallbackPhaseTaskIDSHA256(record.TaskID) ||
		strings.TrimSpace(record.EventID) == "" || timestampErr != nil || !validFinal || !validErrorClass ||
		len(record.Phases) != len(callbackPhaseNames) ||
		(record.FinalResult == "error") != (record.ErrorClass != "none") {
		return time.Time{}, errors.New("invalid submitted-callback phase timing record")
	}
	for index, phase := range record.Phases {
		if phase.Name != callbackPhaseNames[index] || phase.StartedOffsetMS < 0 || phase.FinishedOffsetMS < 0 ||
			phase.DurationMS < 0 || phase.FinishedOffsetMS < phase.StartedOffsetMS {
			return time.Time{}, errors.New("invalid submitted-callback phase member")
		}
		if phase.Attempted {
			if phase.Result != "ok" && phase.Result != "error" {
				return time.Time{}, errors.New("attempted callback phase has an invalid result")
			}
		} else if phase.Result != "not_attempted" || phase.StartedOffsetMS != 0 ||
			phase.FinishedOffsetMS != 0 || phase.DurationMS != 0 {
			return time.Time{}, errors.New("unattempted callback phase carries timing")
		}
	}
	return observedAt, nil
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
	verdict := callbackPhaseVerdict{Verdict: "callback_phase_cliff_not_attributed",
		FirstTimeoutOrderPosition: firstTimeout, LastTimeoutOrderPosition: lastTimeout}
	bestDelta := math.Inf(-1)
	for _, phaseName := range callbackPhaseNames {
		var preCliff, timeoutTail []float64
		for _, operation := range phases {
			aggregate := operation.Phases[phaseName]
			if aggregate.Count == 0 {
				continue
			}
			if operation.OrderPosition < firstTimeout {
				preCliff = append(preCliff, aggregate.MaxMS)
			}
			if timeoutOrders[operation.OrderPosition] {
				timeoutTail = append(timeoutTail, aggregate.MaxMS)
			}
		}
		preP95 := percentile(preCliff, 0.95)
		tailMax := maximum(timeoutTail)
		delta := tailMax - preP95
		if delta > bestDelta {
			bestDelta = delta
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
		verdict.Verdict = "callback_phase_cliff_reproduced"
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
		header = append(header, phaseName+"_count", phaseName+"_sum_ms", phaseName+"_max_ms", phaseName+"_errors")
	}
	header = append(header, "observed_at")
	_ = writer.Write(header)
	for _, operation := range operations {
		row := []string{strconv.Itoa(operation.OrderPosition), operation.SampleID, operation.CellID,
			strconv.FormatBool(operation.Warmup), strconv.Itoa(operation.CallbackCount), strconv.Itoa(operation.FinalErrors)}
		for _, phaseName := range callbackPhaseNames {
			aggregate := operation.Phases[phaseName]
			row = append(row, strconv.Itoa(aggregate.Count), formatFloat(aggregate.SumMS),
				formatFloat(aggregate.MaxMS), strconv.Itoa(aggregate.Errors))
		}
		row = append(row, operation.ObservedAt.UTC().Format(time.RFC3339Nano))
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
