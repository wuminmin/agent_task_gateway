// Command runner executes the four TaskGate evaluation baselines and writes
// immutable raw JSONL/CSV observations. It does not synthesize fallback data:
// unavailable databases, gateways, task pools, or metrics probes are errors.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

const schemaVersion = 1

const (
	baselineDirect           = "direct_postgresql"
	baselineNativeView       = "native_view"
	baselineASTOnly          = "ast_only_gateway"
	baselineResourceTaskGate = "resource_taskgate"

	orderingSeededRandom       = "seeded_random"
	cacheStrategyWarm          = "warm"
	cacheStrategyCold          = "cold"
	taskConcurrencyDistinct    = "distinct_task"
	taskConcurrencySameTask    = "same_task"
	defaultApplicationDirect   = "taskgate-eval-direct"
	defaultApplicationNative   = "taskgate-eval-native"
	environmentManifestVersion = 1
)

var requiredBaselines = []string{
	baselineDirect,
	baselineNativeView,
	baselineASTOnly,
	baselineResourceTaskGate,
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var campaignIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("config path cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

type suiteConfig struct {
	SchemaVersion          int          `json:"schema_version"`
	Name                   string       `json:"name"`
	Mode                   string       `json:"mode"`
	Seed                   int64        `json:"seed"`
	WorkloadLineage        string       `json:"workload_lineage"`
	OrderingStrategy       string       `json:"ordering_strategy"`
	CacheStrategy          string       `json:"cache_strategy"`
	TaskConcurrencyMode    string       `json:"task_concurrency_mode"`
	EnvironmentManifestEnv string       `json:"environment_manifest_env"`
	WarmupRunsPerWorker    int          `json:"warmup_runs_per_worker"`
	MeasuredRunsPerWorker  int          `json:"measured_runs_per_worker"`
	TaskGateQueriesPerTask int          `json:"taskgate_queries_per_task"`
	Concurrency            []int        `json:"concurrency"`
	BaselineOrder          []string     `json:"baseline_order"`
	MaxResultRows          int64        `json:"max_result_rows"`
	StatementTimeout       string       `json:"statement_timeout"`
	RequireResourceMetrics bool         `json:"require_resource_metrics"`
	Experiments            []experiment `json:"experiments"`
}

type experiment struct {
	ID                   string            `json:"id"`
	Family               string            `json:"family"`
	ScaleFactor          int               `json:"scale_factor"`
	Workload             string            `json:"workload"`
	DatasetDigestEnv     string            `json:"dataset_digest_env"`
	DatasetManifestEnv   string            `json:"dataset_manifest_env"`
	DirectDSNEnv         string            `json:"direct_dsn_env"`
	NativeDSNEnv         string            `json:"native_dsn_env"`
	ASTURLenv            string            `json:"ast_url_env"`
	ASTTokenEnv          string            `json:"ast_token_env"`
	TaskGateURLenv       string            `json:"taskgate_url_env"`
	TaskGateTokenEnv     string            `json:"taskgate_token_env"`
	TaskGateTasksFileEnv string            `json:"taskgate_tasks_file_env"`
	MetricsProbeEnv      map[string]string `json:"metrics_probe_env"`
	CacheResetEnv        map[string]string `json:"cache_reset_env,omitempty"`
	configDir            string
}

type workloadManifest struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	Family        string       `json:"family"`
	Lineage       string       `json:"lineage"`
	Queries       []queryEntry `json:"queries"`
	path          string
}

type queryEntry struct {
	ID  string            `json:"id"`
	SQL map[string]string `json:"sql"`
}

type loadedQuery struct {
	ID  string
	SQL map[string]string
}

type taskPoolFile struct {
	SchemaVersion int                            `json:"schema_version"`
	Experiments   map[string]map[string][]string `json:"experiments"`
}

type runMetadata struct {
	SchemaVersion             int                          `json:"schema_version"`
	RunID                     string                       `json:"run_id"`
	Suite                     string                       `json:"suite"`
	Mode                      string                       `json:"mode"`
	CampaignID                string                       `json:"campaign_id,omitempty"`
	Status                    string                       `json:"status"`
	StartedAt                 string                       `json:"started_at"`
	FinishedAt                string                       `json:"finished_at,omitempty"`
	GitRevision               string                       `json:"git_revision"`
	GitDirty                  bool                         `json:"git_dirty"`
	ConfigPath                string                       `json:"config_path"`
	ConfigSHA256              string                       `json:"config_sha256"`
	WorkloadSHA256            map[string]string            `json:"workload_sha256"`
	DatasetSHA256Manifests    map[string]string            `json:"dataset_sha256_manifests"`
	DatasetManifestPaths      map[string]string            `json:"dataset_manifest_paths"`
	MetricsProbePaths         map[string]map[string]string `json:"metrics_probe_paths"`
	MetricsProbeSHA256        map[string]map[string]string `json:"metrics_probe_sha256"`
	CacheResetPaths           map[string]map[string]string `json:"cache_reset_paths,omitempty"`
	CacheResetSHA256          map[string]map[string]string `json:"cache_reset_sha256,omitempty"`
	GoVersion                 string                       `json:"go_version"`
	GOOS                      string                       `json:"goos"`
	GOARCH                    string                       `json:"goarch"`
	BaselineOrder             []string                     `json:"baseline_order"`
	BaselineOrderSeed         int64                        `json:"baseline_order_seed"`
	OrderingStrategy          string                       `json:"ordering_strategy"`
	CellOrder                 []cellSchedule               `json:"cell_order"`
	CacheStrategy             string                       `json:"cache_strategy"`
	TaskConcurrencyMode       string                       `json:"task_concurrency_mode"`
	WorkloadLineage           string                       `json:"workload_lineage"`
	EnvironmentManifestPath   string                       `json:"environment_manifest_path,omitempty"`
	EnvironmentManifestSHA256 string                       `json:"environment_manifest_sha256,omitempty"`
	WarmupRunsPerWorker       int                          `json:"warmup_runs_per_worker"`
	MeasuredRunsPerWorker     int                          `json:"measured_runs_per_worker"`
	TaskGateQueriesPerTask    int                          `json:"taskgate_queries_per_task"`
	Concurrency               []int                        `json:"concurrency"`
	Endpoints                 map[string]interface{}       `json:"endpoints"`
	Error                     string                       `json:"error,omitempty"`
}

type cellSchedule struct {
	Order       int    `json:"order"`
	Experiment  string `json:"experiment"`
	Baseline    string `json:"baseline"`
	Concurrency int    `json:"concurrency"`
}

type sample struct {
	SchemaVersion int                `json:"schema_version"`
	RunID         string             `json:"run_id"`
	Experiment    string             `json:"experiment"`
	Family        string             `json:"family"`
	ScaleFactor   int                `json:"scale_factor"`
	Baseline      string             `json:"baseline"`
	Concurrency   int                `json:"concurrency"`
	Worker        int                `json:"worker"`
	Iteration     int                `json:"iteration"`
	QueryID       string             `json:"query_id"`
	RequestID     string             `json:"request_id"`
	StartedUnixNS int64              `json:"started_unix_ns"`
	LatencyMS     float64            `json:"latency_ms"`
	DatabaseMS    *float64           `json:"database_ms"`
	RowCount      int64              `json:"row_count"`
	ResultSHA256  string             `json:"result_sha256"`
	ReceiptBytes  *int64             `json:"receipt_bytes"`
	ComponentMS   map[string]float64 `json:"component_ms"`
	Success       bool               `json:"success"`
	Error         string             `json:"error,omitempty"`
}

type metricsResult struct {
	CPUSeconds          map[string]float64 `json:"cpu_seconds"`
	PeakMemoryBytes     map[string]int64   `json:"peak_memory_bytes"`
	ControlTransactions *int64             `json:"control_transactions"`
	ReceiptStorageBytes *int64             `json:"receipt_storage_bytes"`
	ComponentMS         map[string]float64 `json:"component_ms,omitempty"`
}

type cell struct {
	SchemaVersion       int                `json:"schema_version"`
	RunID               string             `json:"run_id"`
	Experiment          string             `json:"experiment"`
	Family              string             `json:"family"`
	ScaleFactor         int                `json:"scale_factor"`
	Baseline            string             `json:"baseline"`
	Concurrency         int                `json:"concurrency"`
	WarmupSamples       int                `json:"warmup_samples"`
	MeasuredSamples     int                `json:"measured_samples"`
	MeasurementStartNS  int64              `json:"measurement_start_unix_ns"`
	MeasurementEndNS    int64              `json:"measurement_end_unix_ns"`
	MeasurementSeconds  float64            `json:"measurement_seconds"`
	ThroughputQPS       float64            `json:"throughput_qps"`
	CPUSeconds          map[string]float64 `json:"cpu_seconds"`
	PeakMemoryBytes     map[string]int64   `json:"peak_memory_bytes"`
	ControlTransactions *int64             `json:"control_transactions"`
	ReceiptStorageBytes *int64             `json:"receipt_storage_bytes"`
	ComponentMS         map[string]float64 `json:"component_ms,omitempty"`
}

type backendResult struct {
	Rows         json.RawMessage
	RowCount     int64
	DatabaseMS   *float64
	ReceiptBytes *int64
	ComponentMS  map[string]float64
}

type backend interface {
	Run(context.Context, int, string, string, string) (backendResult, error)
	Close()
}

func main() {
	configPath := flag.String("config", "", "evaluation suite JSON")
	var additionalConfigs stringListFlag
	flag.Var(&additionalConfigs, "additional-config", "additional suite JSON used only by combined preflight (repeatable)")
	outputDir := flag.String("output", "", "raw run output directory")
	runID := flag.String("run-id", "", "stable run identifier")
	validateOnly := flag.Bool("validate-only", false, "validate config and workload files without contacting backends")
	preflightOnly := flag.Bool("preflight-only", false, "validate all runtime prerequisites without measuring")
	flag.Parse()

	if *configPath == "" {
		fatal(errors.New("runner: -config is required"))
	}
	cfg, configBytes, err := loadSuite(*configPath)
	if err != nil {
		fatal(err)
	}
	workloads, err := loadWorkloads(cfg)
	if err != nil {
		fatal(err)
	}
	if *validateOnly && *preflightOnly {
		fatal(errors.New("runner: -validate-only and -preflight-only are mutually exclusive"))
	}
	if *validateOnly {
		if len(additionalConfigs) != 0 {
			fatal(errors.New("runner: -additional-config is only valid with -preflight-only"))
		}
		fmt.Printf("ok - %s: %d experiment(s), four baselines, %d measured run(s) per worker\n",
			cfg.Name, len(cfg.Experiments), cfg.MeasuredRunsPerWorker)
		return
	}
	if *preflightOnly {
		configs := []suiteConfig{cfg}
		workloadSets := []map[string][]loadedQuery{workloads}
		for _, path := range additionalConfigs {
			additional, _, loadErr := loadSuite(path)
			if loadErr != nil {
				fatal(loadErr)
			}
			additionalWorkloads, loadErr := loadWorkloads(additional)
			if loadErr != nil {
				fatal(loadErr)
			}
			configs = append(configs, additional)
			workloadSets = append(workloadSets, additionalWorkloads)
		}
		if err := preflightSuites(configs, workloadSets); err != nil {
			fatal(fmt.Errorf("runner preflight: %w", err))
		}
		fmt.Printf("ok - runtime preflight passed for %d suite(s) before measurement\n", len(configs))
		return
	}
	if len(additionalConfigs) != 0 {
		fatal(errors.New("runner: -additional-config is only valid with -preflight-only"))
	}
	if *outputDir == "" || *runID == "" {
		fatal(errors.New("runner: -output and -run-id are required unless -validate-only is used"))
	}
	if err := preflightSuites([]suiteConfig{cfg}, []map[string][]loadedQuery{workloads}); err != nil {
		fatal(fmt.Errorf("runner preflight: %w", err))
	}

	metadata, err := buildMetadata(cfg, *configPath, configBytes, workloads, *runID)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		fatal(fmt.Errorf("runner: create output directory: %w", err))
	}
	metadataPath := filepath.Join(*outputDir, "run.json")
	if err := writeJSONFile(metadataPath, metadata); err != nil {
		fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allSamples, cells, runErr := executeSuite(ctx, cfg, workloads, *runID)
	if err := writeSamples(*outputDir, allSamples); err != nil && runErr == nil {
		runErr = err
	}
	if err := writeJSONLines(filepath.Join(*outputDir, "cells.jsonl"), cells); err != nil && runErr == nil {
		runErr = err
	}
	if runErr == nil {
		runErr = validateResultEquivalence(allSamples)
	}

	metadata.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	metadata.Status = "complete"
	if runErr != nil {
		metadata.Status = "failed"
		metadata.Error = runErr.Error()
	}
	if err := writeJSONFile(metadataPath, metadata); err != nil {
		fatal(err)
	}
	if runErr != nil {
		fatal(runErr)
	}
	fmt.Printf("ok - raw evaluation data written to %s\n", *outputDir)
}

func executeSuite(ctx context.Context, cfg suiteConfig, workloads map[string][]loadedQuery, runID string) ([]sample, []cell, error) {
	var allSamples []sample
	var allCells []cell
	experiments := make(map[string]experiment, len(cfg.Experiments))
	for _, exp := range cfg.Experiments {
		experiments[exp.ID] = exp
	}
	for _, item := range buildCellSchedule(cfg) {
		exp, ok := experiments[item.Experiment]
		if !ok {
			return allSamples, allCells, fmt.Errorf("scheduled unknown experiment %s", item.Experiment)
		}
		queries := workloads[exp.ID]
		fmt.Printf("run order=%d experiment=%s baseline=%s concurrency=%d\n", item.Order, exp.ID, item.Baseline, item.Concurrency)
		observations, summary, err := runCell(ctx, cfg, exp, queries, runID, item.Baseline, item.Concurrency)
		allSamples = append(allSamples, observations...)
		if summary != nil {
			allCells = append(allCells, *summary)
		}
		if err != nil {
			return allSamples, allCells, fmt.Errorf("%s/%s/c%d: %w", exp.ID, item.Baseline, item.Concurrency, err)
		}
	}
	return allSamples, allCells, nil
}

func buildCellSchedule(cfg suiteConfig) []cellSchedule {
	var schedule []cellSchedule
	for _, exp := range cfg.Experiments {
		for _, concurrency := range cfg.Concurrency {
			for _, baseline := range cfg.BaselineOrder {
				schedule = append(schedule, cellSchedule{
					Experiment: exp.ID, Baseline: baseline, Concurrency: concurrency,
				})
			}
		}
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	rng.Shuffle(len(schedule), func(i, j int) {
		schedule[i], schedule[j] = schedule[j], schedule[i]
	})
	for index := range schedule {
		schedule[index].Order = index + 1
	}
	return schedule
}

func runCell(ctx context.Context, cfg suiteConfig, exp experiment, queries []loadedQuery, runID, baseline string, concurrency int) ([]sample, *cell, error) {
	b, err := openBackend(ctx, cfg, exp, baseline, concurrency)
	if err != nil {
		return nil, nil, err
	}
	defer b.Close()

	if err := runPhase(ctx, b, queries, runID, exp, baseline, concurrency, cfg.WarmupRunsPerWorker, true, nil); err != nil {
		return nil, nil, fmt.Errorf("warmup: %w", err)
	}
	if cfg.CacheStrategy == cacheStrategyCold {
		if err := resetCache(ctx, exp, baseline, concurrency, "measurement_start"); err != nil {
			return nil, nil, err
		}
	}

	started := time.Now().UTC()
	var observations []sample
	if err := runPhase(ctx, b, queries, runID, exp, baseline, concurrency, cfg.MeasuredRunsPerWorker, false, &observations); err != nil {
		return observations, nil, fmt.Errorf("measurement: %w", err)
	}
	finished := time.Now().UTC()
	duration := finished.Sub(started).Seconds()
	if duration <= 0 {
		return observations, nil, errors.New("non-positive measurement duration")
	}

	metrics, err := collectMetrics(ctx, cfg, exp, baseline, concurrency, started, finished)
	if err != nil {
		return observations, nil, err
	}
	result := &cell{
		SchemaVersion:      schemaVersion,
		RunID:              runID,
		Experiment:         exp.ID,
		Family:             exp.Family,
		ScaleFactor:        exp.ScaleFactor,
		Baseline:           baseline,
		Concurrency:        concurrency,
		WarmupSamples:      cfg.WarmupRunsPerWorker * concurrency * len(queries),
		MeasuredSamples:    len(observations),
		MeasurementStartNS: started.UnixNano(),
		MeasurementEndNS:   finished.UnixNano(),
		MeasurementSeconds: duration,
		ThroughputQPS:      float64(len(observations)) / duration,
	}
	if metrics != nil {
		result.CPUSeconds = metrics.CPUSeconds
		result.PeakMemoryBytes = metrics.PeakMemoryBytes
		result.ControlTransactions = metrics.ControlTransactions
		result.ReceiptStorageBytes = metrics.ReceiptStorageBytes
		result.ComponentMS = metrics.ComponentMS
	}
	return observations, result, nil
}

func runPhase(ctx context.Context, b backend, queries []loadedQuery, runID string, exp experiment, baseline string, concurrency, iterations int, warmup bool, output *[]sample) error {
	phaseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for worker := 0; worker < concurrency; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each worker runs every declared query `iterations` times, so the
			// per-query sample count at concurrency C is iterations*C. This keeps
			// the table-level ">=30 measured runs per query" guarantee (iterations
			// == MeasuredRunsPerWorker) independent of how many queries a workload
			// declares, instead of rotating one query per iteration.
			for queryIndex, query := range queries {
				query := query
				sqlText := query.SQL[baseline]
				for iteration := 0; iteration < iterations; iteration++ {
					if phaseCtx.Err() != nil {
						return
					}
					phase := "m"
					if warmup {
						phase = "w"
					}
					requestID := makeRequestID(runID, exp.ID, baseline, concurrency, worker, queryIndex, iteration, phase)
					started := time.Now().UTC()
					result, err := b.Run(phaseCtx, worker, query.ID, requestID, sqlText)
					latency := time.Since(started)
					if err != nil {
						observation := newSample(runID, exp, baseline, concurrency, worker, iteration, query.ID, requestID, started, latency, result)
						observation.Success = false
						observation.Error = err.Error()
						mu.Lock()
						if !warmup && output != nil {
							*output = append(*output, observation)
						}
						if firstErr == nil {
							firstErr = err
							cancel()
						}
						mu.Unlock()
						return
					}
					if warmup {
						continue
					}
					observation := newSample(runID, exp, baseline, concurrency, worker, iteration, query.ID, requestID, started, latency, result)
					observation.Success = true
					mu.Lock()
					*output = append(*output, observation)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if output != nil {
		sort.Slice(*output, func(i, j int) bool {
			if (*output)[i].Worker != (*output)[j].Worker {
				return (*output)[i].Worker < (*output)[j].Worker
			}
			return (*output)[i].Iteration < (*output)[j].Iteration
		})
	}
	return firstErr
}

func newSample(runID string, exp experiment, baseline string, concurrency, worker, iteration int, queryID, requestID string, started time.Time, latency time.Duration, result backendResult) sample {
	return sample{
		SchemaVersion: schemaVersion, RunID: runID, Experiment: exp.ID,
		Family: exp.Family, ScaleFactor: exp.ScaleFactor, Baseline: baseline,
		Concurrency: concurrency, Worker: worker, Iteration: iteration,
		QueryID: queryID, RequestID: requestID, StartedUnixNS: started.UnixNano(),
		LatencyMS: milliseconds(latency), DatabaseMS: result.DatabaseMS,
		RowCount: result.RowCount, ResultSHA256: hashJSON(result.Rows),
		ReceiptBytes: result.ReceiptBytes, ComponentMS: result.ComponentMS,
	}
}

func openBackend(ctx context.Context, cfg suiteConfig, exp experiment, baseline string, concurrency int) (backend, error) {
	switch baseline {
	case baselineDirect:
		return openPostgres(ctx, cfg, exp.DirectDSNEnv, concurrency, defaultApplicationDirect)
	case baselineNativeView:
		return openPostgres(ctx, cfg, exp.NativeDSNEnv, concurrency, defaultApplicationNative)
	case baselineASTOnly:
		return openAST(exp)
	case baselineResourceTaskGate:
		return openTaskGate(exp, concurrency, cfg.TaskConcurrencyMode)
	default:
		return nil, fmt.Errorf("unknown baseline %q", baseline)
	}
}

type postgresBackend struct {
	connector *dataconnector.Connector
	timeout   time.Duration
	maxRows   int64
}

func openPostgres(ctx context.Context, cfg suiteConfig, envName string, concurrency int, applicationName string) (backend, error) {
	dsn := os.Getenv(envName)
	if dsn == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", envName)
	}
	timeout, _ := time.ParseDuration(cfg.StatementTimeout)
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: timeout, ConnectTimeout: 10 * time.Second,
		MaxRows: cfg.MaxResultRows, MaxConnections: int32(concurrency), ApplicationName: applicationName,
	})
	if err != nil {
		return nil, fmt.Errorf("connect using %s: %w", envName, err)
	}
	return &postgresBackend{connector: connector, timeout: timeout, maxRows: cfg.MaxResultRows}, nil
}

func (b *postgresBackend) Run(ctx context.Context, _ int, _, _, sqlText string) (backendResult, error) {
	result, err := b.connector.Query(ctx, dataconnector.QueryRequest{
		SQL: sqlText, StatementTimeout: b.timeout, MaxRows: b.maxRows,
	})
	if err != nil {
		return backendResult{}, err
	}
	rows, err := json.Marshal(result.Rows)
	if err != nil {
		return backendResult{}, fmt.Errorf("encode PostgreSQL rows: %w", err)
	}
	databaseMS := milliseconds(result.DatabaseTime)
	return backendResult{
		Rows: rows, RowCount: result.RowCount, DatabaseMS: &databaseMS,
		ComponentMS: map[string]float64{"database": databaseMS},
	}, nil
}

func (b *postgresBackend) Close() { b.connector.Close() }

type httpBackend struct {
	client     *http.Client
	endpoint   string
	token      string
	experiment string
	taskIDs    []string
	sequence   atomic.Int64
	taskGate   bool
}

func openAST(exp experiment) (backend, error) {
	endpoint := os.Getenv(exp.ASTURLenv)
	if endpoint == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", exp.ASTURLenv)
	}
	if err := validateHTTPURL(endpoint); err != nil {
		return nil, fmt.Errorf("%s: %w", exp.ASTURLenv, err)
	}
	token := optionalEnv(exp.ASTTokenEnv)
	return newHTTPBackend(endpoint, token, exp.ID, nil, false), nil
}

func openTaskGate(exp experiment, concurrency int, taskConcurrencyMode string) (backend, error) {
	endpoint := os.Getenv(exp.TaskGateURLenv)
	if endpoint == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", exp.TaskGateURLenv)
	}
	if err := validateHTTPURL(endpoint); err != nil {
		return nil, fmt.Errorf("%s: %w", exp.TaskGateURLenv, err)
	}
	token := os.Getenv(exp.TaskGateTokenEnv)
	if token == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", exp.TaskGateTokenEnv)
	}
	path := os.Getenv(exp.TaskGateTasksFileEnv)
	if path == "" {
		return nil, fmt.Errorf("required environment variable %s is not set", exp.TaskGateTasksFileEnv)
	}
	tasks, err := loadTaskPool(path, exp.ID, concurrency, taskConcurrencyMode)
	if err != nil {
		return nil, err
	}
	return newHTTPBackend(endpoint, token, exp.ID, tasks, true), nil
}

func newHTTPBackend(endpoint, token, experiment string, tasks []string, taskGate bool) *httpBackend {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	return &httpBackend{
		client:   &http.Client{Transport: transport, Timeout: 65 * time.Second},
		endpoint: endpoint, token: token, experiment: experiment,
		taskIDs: tasks, taskGate: taskGate,
	}
}

func (b *httpBackend) Run(ctx context.Context, worker int, _ string, requestID, sqlText string) (backendResult, error) {
	if b.taskGate {
		return b.runTaskGate(ctx, worker, requestID, sqlText)
	}
	payload := map[string]any{"sql": sqlText, "request_id": requestID, "experiment": b.experiment}
	var response struct {
		Rows        json.RawMessage    `json:"rows"`
		RowCount    int64              `json:"row_count"`
		DatabaseMS  float64            `json:"database_ms"`
		ComponentMS map[string]float64 `json:"component_ms"`
		Error       string             `json:"error"`
	}
	status, err := b.postJSON(ctx, payload, &response)
	if err != nil {
		return backendResult{}, err
	}
	if status != http.StatusOK || response.Error != "" {
		return backendResult{}, fmt.Errorf("AST gateway returned HTTP %d (%s)", status, safeError(response.Error))
	}
	if len(response.Rows) == 0 {
		return backendResult{}, errors.New("AST gateway response omitted rows")
	}
	databaseMS := response.DatabaseMS
	return backendResult{
		Rows: response.Rows, RowCount: response.RowCount, DatabaseMS: &databaseMS,
		ComponentMS: response.ComponentMS,
	}, nil
}

func (b *httpBackend) runTaskGate(ctx context.Context, worker int, requestID, sqlText string) (backendResult, error) {
	if worker < 0 || worker >= len(b.taskIDs) {
		return backendResult{}, errors.New("TaskGate task pool does not cover worker")
	}
	rpcID := b.sequence.Add(1)
	payload := map[string]any{
		"jsonrpc": "2.0", "id": rpcID, "method": "tools/call",
		"params": map[string]any{
			"name": "query_sql",
			"arguments": map[string]any{
				"task_id": b.taskIDs[worker], "request_id": requestID, "sql": sqlText,
			},
		},
	}
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	status, err := b.postJSON(ctx, payload, &envelope)
	if err != nil {
		return backendResult{}, err
	}
	if status != http.StatusOK {
		return backendResult{}, fmt.Errorf("TaskGate returned HTTP %d", status)
	}
	if envelope.Error != nil {
		return backendResult{}, fmt.Errorf("TaskGate RPC error %d (%s)", envelope.Error.Code, safeError(envelope.Error.Message))
	}
	if envelope.Result.IsError {
		return backendResult{}, errors.New("TaskGate rejected the measured query; inspect Gateway logs and task grant")
	}
	var content struct {
		Rows        json.RawMessage    `json:"rows"`
		RowCount    int64              `json:"row_count"`
		DatabaseMS  float64            `json:"database_ms"`
		Receipt     json.RawMessage    `json:"receipt"`
		ComponentMS map[string]float64 `json:"component_ms"`
	}
	if err := json.Unmarshal(envelope.Result.StructuredContent, &content); err != nil {
		return backendResult{}, fmt.Errorf("decode TaskGate structured content: %w", err)
	}
	if len(content.Rows) == 0 {
		return backendResult{}, errors.New("TaskGate structured content omitted rows")
	}
	databaseMS := content.DatabaseMS
	var receiptBytes *int64
	if len(content.Receipt) > 0 && string(content.Receipt) != "null" {
		value := int64(len(compactJSON(content.Receipt)))
		receiptBytes = &value
	}
	components := content.ComponentMS
	if components == nil {
		components = map[string]float64{"database": databaseMS}
	}
	return backendResult{
		Rows: content.Rows, RowCount: content.RowCount, DatabaseMS: &databaseMS,
		ReceiptBytes: receiptBytes, ComponentMS: components,
	}, nil
}

func (b *httpBackend) postJSON(ctx context.Context, payload any, output any) (int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if b.token != "" {
		request.Header.Set("Authorization", "Bearer "+b.token)
	}
	response, err := b.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("call %s: %w", redactedURL(b.endpoint), err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, 64<<20)
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(output); err != nil {
		return response.StatusCode, fmt.Errorf("decode HTTP %d response: %w", response.StatusCode, err)
	}
	return response.StatusCode, nil
}

func (b *httpBackend) Close() {
	if transport, ok := b.client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func preflightSuites(configs []suiteConfig, workloadSets []map[string][]loadedQuery) error {
	if len(configs) != len(workloadSets) {
		return errors.New("suite/workload preflight input mismatch")
	}
	seenExperiments := make(map[string]string)
	seenTasks := make(map[string]string)
	poolCache := make(map[string]taskPoolFile)
	for suiteIndex, cfg := range configs {
		for _, exp := range cfg.Experiments {
			location := cfg.Name + "/" + exp.ID
			if previous := seenExperiments[exp.ID]; previous != "" {
				return fmt.Errorf("experiment %s occurs in both %s and %s", exp.ID, previous, cfg.Name)
			}
			seenExperiments[exp.ID] = cfg.Name
			for _, item := range []struct {
				name  string
				label string
			}{
				{exp.DirectDSNEnv, "direct PostgreSQL DSN"},
				{exp.NativeDSNEnv, "native PostgreSQL DSN"},
			} {
				value, err := requiredEnvironment(item.name, location+" "+item.label)
				if err != nil {
					return err
				}
				if _, err := pgxpool.ParseConfig(value); err != nil {
					return fmt.Errorf("%s %s is not a valid PostgreSQL DSN: %w", location, item.name, err)
				}
			}
			for _, item := range []struct {
				name  string
				label string
			}{
				{exp.ASTURLenv, "AST-only URL"},
				{exp.TaskGateURLenv, "TaskGate URL"},
			} {
				value, err := requiredEnvironment(item.name, location+" "+item.label)
				if err != nil {
					return err
				}
				if err := validateHTTPURL(value); err != nil {
					return fmt.Errorf("%s %s: %w", location, item.name, err)
				}
			}
			if _, err := requiredEnvironment(exp.TaskGateTokenEnv, location+" TaskGate token"); err != nil {
				return err
			}
			poolPath, err := requiredEnvironment(exp.TaskGateTasksFileEnv, location+" TaskGate task pool")
			if err != nil {
				return err
			}
			pool, ok := poolCache[poolPath]
			if !ok {
				pool, err = readTaskPoolFile(poolPath)
				if err != nil {
					return err
				}
				poolCache[poolPath] = pool
			}
			for _, concurrency := range cfg.Concurrency {
				tasks, taskErr := tasksForCell(pool, exp.ID, concurrency, cfg.TaskConcurrencyMode)
				if taskErr != nil {
					return taskErr
				}
				cell := fmt.Sprintf("%s/c%d", location, concurrency)
				if err := reserveUniqueTasks(seenTasks, tasks, cell); err != nil {
					return err
				}
				requiredQueries := taskQueriesForCell(cfg, len(workloadSets[suiteIndex][exp.ID]), concurrency)
				if err := preflightTaskBudgets(exp, tasks, requiredQueries); err != nil {
					return fmt.Errorf("%s: %w", cell, err)
				}
			}
			if cfg.Mode == "full" {
				if _, _, err := datasetProvenance(exp); err != nil {
					return fmt.Errorf("%s: %w", location, err)
				}
			}
			if cfg.RequireResourceMetrics {
				for _, baseline := range requiredBaselines {
					envName := exp.MetricsProbeEnv[baseline]
					if envName == "" {
						return fmt.Errorf("%s has no metrics probe environment name for %s", location, baseline)
					}
					if _, _, err := metricsProbeProvenance(envName); err != nil {
						return fmt.Errorf("%s/%s: %w", location, baseline, err)
					}
				}
			}
			if cfg.CacheStrategy == cacheStrategyCold {
				for _, baseline := range requiredBaselines {
					envName := exp.CacheResetEnv[baseline]
					if envName == "" {
						return fmt.Errorf("%s has no cache reset environment name for %s", location, baseline)
					}
					if _, _, err := cacheResetProvenance(envName); err != nil {
						return fmt.Errorf("%s/%s: %w", location, baseline, err)
					}
				}
			}
		}
		if cfg.Mode == "full" {
			if _, _, err := environmentManifestProvenance(cfg); err != nil {
				return err
			}
		}
	}
	return nil
}

// preflightTaskBudgets uses the public read-only task context tool so a
// campaign fails before warmup when a task is inactive or its remaining query
// budget cannot cover every query in that worker's cell.
func preflightTaskBudgets(exp experiment, tasks []string, requiredQueries int) error {
	endpoint := os.Getenv(exp.TaskGateURLenv)
	token := os.Getenv(exp.TaskGateTokenEnv)
	client := newHTTPBackend(endpoint, token, exp.ID, nil, false)
	defer client.Close()

	unique := make([]string, 0, len(tasks))
	seen := make(map[string]bool, len(tasks))
	for _, taskID := range tasks {
		if !seen[taskID] {
			seen[taskID] = true
			unique = append(unique, taskID)
		}
	}
	errorsByTask := make([]error, len(unique))
	var wg sync.WaitGroup
	for index, taskID := range unique {
		index, taskID := index, taskID
		wg.Add(1)
		go func() {
			defer wg.Done()
			errorsByTask[index] = preflightOneTaskBudget(client, index+1, taskID, requiredQueries)
		}()
	}
	wg.Wait()
	for _, err := range errorsByTask {
		if err != nil {
			return err
		}
	}
	return nil
}

func preflightOneTaskBudget(client *httpBackend, rpcID int, taskID string, requiredQueries int) error {
	payload := map[string]any{
		"jsonrpc": "2.0", "id": rpcID, "method": "tools/call",
		"params": map[string]any{
			"name": "get_task_context", "arguments": map[string]any{"task_id": taskID},
		},
	}
	var envelope struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	status, err := client.postJSON(context.Background(), payload, &envelope)
	if err != nil {
		return fmt.Errorf("TaskGate task %s budget preflight failed: %w", taskID, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("TaskGate task %s budget preflight returned HTTP %d", taskID, status)
	}
	if envelope.Error != nil {
		return fmt.Errorf("TaskGate task %s budget preflight RPC error %d (%s)", taskID, envelope.Error.Code, safeError(envelope.Error.Message))
	}
	if envelope.Result.IsError {
		return fmt.Errorf("TaskGate task %s is not an accessible ACTIVE task", taskID)
	}
	var content struct {
		Budget struct {
			Remaining struct {
				Queries int64 `json:"queries"`
			} `json:"remaining"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(envelope.Result.StructuredContent, &content); err != nil {
		return fmt.Errorf("decode TaskGate task %s budget preflight: %w", taskID, err)
	}
	if content.Budget.Remaining.Queries < int64(requiredQueries) {
		return fmt.Errorf("TaskGate task %s has %d remaining queries, need %d", taskID, content.Budget.Remaining.Queries, requiredQueries)
	}
	return nil
}

func requiredEnvironment(name, label string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%s has no configured environment variable name", label)
	}
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("required environment variable %s is not set (%s)", name, label)
	}
	return value, nil
}

func repositoryRelativePath(path string) (string, error) {
	clean := filepath.Clean(path)
	const root = "/workspace/"
	if !strings.HasPrefix(clean, root) {
		return "", fmt.Errorf("path must be inside the mounted repository at /workspace: %s", path)
	}
	relative := strings.TrimPrefix(clean, root)
	if relative == "" || relative == "." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("invalid repository path: %s", path)
	}
	return filepath.ToSlash(relative), nil
}

func datasetProvenance(exp experiment) (string, string, error) {
	digest, err := requiredEnvironment(exp.DatasetDigestEnv, exp.ID+" dataset SHA-256")
	if err != nil {
		return "", "", err
	}
	if !sha256Pattern.MatchString(digest) {
		return "", "", fmt.Errorf("%s must be exactly 64 lowercase hexadecimal characters", exp.DatasetDigestEnv)
	}
	manifestPath, err := requiredEnvironment(exp.DatasetManifestEnv, exp.ID+" dataset manifest")
	if err != nil {
		return "", "", err
	}
	relativePath, err := repositoryRelativePath(manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", exp.DatasetManifestEnv, err)
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("read dataset manifest from %s: %w", exp.DatasetManifestEnv, err)
	}
	actual := sha256Hex(manifest)
	if actual != digest {
		return "", "", fmt.Errorf("%s does not match %s: got %s", exp.DatasetDigestEnv, exp.DatasetManifestEnv, actual)
	}
	return digest, relativePath, nil
}

func environmentManifestProvenance(cfg suiteConfig) (string, string, error) {
	envName := strings.TrimSpace(cfg.EnvironmentManifestEnv)
	if envName == "" {
		if cfg.Mode == "full" {
			return "", "", errors.New("full evaluation requires environment_manifest_env")
		}
		return "", "", nil
	}
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" {
		if cfg.Mode == "full" {
			return "", "", fmt.Errorf("required environment variable %s is not set (environment manifest)", envName)
		}
		return "", "", nil
	}
	relativePath, err := repositoryRelativePath(path)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", envName, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read environment manifest from %s: %w", envName, err)
	}
	var manifest map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&manifest); err != nil {
		return "", "", fmt.Errorf("decode environment manifest from %s: %w", envName, err)
	}
	if version, ok := manifest["schema_version"].(float64); !ok || int(version) != environmentManifestVersion {
		return "", "", fmt.Errorf("%s must point to a schema_version=%d environment manifest", envName, environmentManifestVersion)
	}
	for _, field := range []string{"host", "software", "database", "datasets"} {
		if _, ok := manifest[field].(map[string]any); !ok {
			return "", "", fmt.Errorf("%s environment manifest omits object field %q", envName, field)
		}
	}
	return sha256Hex(data), relativePath, nil
}

func metricsProbeProvenance(envName string) ([]string, string, error) {
	return commandProvenance(envName, "metrics probe")
}

func cacheResetProvenance(envName string) ([]string, string, error) {
	return commandProvenance(envName, "cache reset command")
}

func commandProvenance(envName string, label string) ([]string, string, error) {
	encoded, err := requiredEnvironment(envName, label)
	if err != nil {
		return nil, "", err
	}
	var arguments []string
	decoder := json.NewDecoder(strings.NewReader(encoded))
	if err := decoder.Decode(&arguments); err != nil || len(arguments) == 0 || strings.TrimSpace(arguments[0]) == "" {
		return nil, "", fmt.Errorf("%s must be a JSON array containing a command and arguments", envName)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("%s contains trailing JSON", envName)
	}
	executable, err := exec.LookPath(arguments[0])
	if err != nil {
		return nil, "", fmt.Errorf("%s %s is not executable: %w", label, envName, err)
	}
	relativePath, err := repositoryRelativePath(executable)
	if err != nil {
		return nil, "", fmt.Errorf("%s %s must use a checksummable executable under /workspace: %w", label, envName, err)
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, "", fmt.Errorf("%s %s command is not a regular executable file", label, envName)
	}
	return arguments, relativePath, nil
}

func resetCache(ctx context.Context, exp experiment, baseline string, concurrency int, phase string) error {
	envName := exp.CacheResetEnv[baseline]
	arguments, _, err := cacheResetProvenance(envName)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	command.Env = append(os.Environ(),
		"EVAL_CACHE_EXPERIMENT="+exp.ID,
		"EVAL_CACHE_BASELINE="+baseline,
		"EVAL_CACHE_CONCURRENCY="+strconv.Itoa(concurrency),
		"EVAL_CACHE_PHASE="+phase,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cache reset %s failed: %w (%s)", envName, err, safeError(strings.TrimSpace(string(output))))
	}
	return nil
}

func collectMetrics(ctx context.Context, cfg suiteConfig, exp experiment, baseline string, concurrency int, started, finished time.Time) (*metricsResult, error) {
	envName := exp.MetricsProbeEnv[baseline]
	if envName == "" {
		if cfg.RequireResourceMetrics {
			return nil, fmt.Errorf("resource metrics are required but metrics_probe_env has no %s entry", baseline)
		}
		return nil, nil
	}
	if os.Getenv(envName) == "" {
		if cfg.RequireResourceMetrics {
			return nil, fmt.Errorf("required metrics probe environment variable %s is not set", envName)
		}
		return nil, nil
	}
	arguments, _, err := metricsProbeProvenance(envName)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	command.Env = append(os.Environ(),
		"EVAL_PROBE_EXPERIMENT="+exp.ID,
		"EVAL_PROBE_BASELINE="+baseline,
		"EVAL_PROBE_CONCURRENCY="+strconv.Itoa(concurrency),
		"EVAL_PROBE_START_UNIX_NS="+strconv.FormatInt(started.UnixNano(), 10),
		"EVAL_PROBE_END_UNIX_NS="+strconv.FormatInt(finished.UnixNano(), 10),
	)
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("metrics probe %s failed: %w", envName, err)
	}
	var result metricsResult
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("metrics probe %s returned invalid JSON: %w", envName, err)
	}
	if len(result.CPUSeconds) == 0 || len(result.PeakMemoryBytes) == 0 {
		return nil, fmt.Errorf("metrics probe %s omitted cpu_seconds or peak_memory_bytes", envName)
	}
	return &result, nil
}

func loadSuite(path string) (suiteConfig, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return suiteConfig{}, nil, fmt.Errorf("runner: read config: %w", err)
	}
	var cfg suiteConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return suiteConfig{}, nil, fmt.Errorf("runner: decode config: %w", err)
	}
	if cfg.SchemaVersion != schemaVersion || cfg.Name == "" || (cfg.Mode != "smoke" && cfg.Mode != "full") {
		return suiteConfig{}, nil, errors.New("runner: schema_version=1, name, and mode smoke|full are required")
	}
	if cfg.WarmupRunsPerWorker < 1 || cfg.MeasuredRunsPerWorker < 1 || cfg.MaxResultRows < 1 {
		return suiteConfig{}, nil, errors.New("runner: warmup, measured runs, and max_result_rows must be positive")
	}
	if cfg.TaskGateQueriesPerTask < 1 {
		return suiteConfig{}, nil, errors.New("runner: taskgate_queries_per_task must be positive")
	}
	if _, err := time.ParseDuration(cfg.StatementTimeout); err != nil {
		return suiteConfig{}, nil, fmt.Errorf("runner: invalid statement_timeout: %w", err)
	}
	if cfg.WorkloadLineage != "TPC-derived" {
		return suiteConfig{}, nil, errors.New("runner: workload_lineage must be TPC-derived for the current benchmark-derived workloads")
	}
	if cfg.OrderingStrategy != orderingSeededRandom {
		return suiteConfig{}, nil, fmt.Errorf("runner: ordering_strategy must be %q", orderingSeededRandom)
	}
	if cfg.CacheStrategy != cacheStrategyWarm && cfg.CacheStrategy != cacheStrategyCold {
		return suiteConfig{}, nil, fmt.Errorf("runner: cache_strategy must be %q or %q", cacheStrategyWarm, cacheStrategyCold)
	}
	if cfg.TaskConcurrencyMode != taskConcurrencyDistinct && cfg.TaskConcurrencyMode != taskConcurrencySameTask {
		return suiteConfig{}, nil, fmt.Errorf("runner: task_concurrency_mode must be %q or %q", taskConcurrencyDistinct, taskConcurrencySameTask)
	}
	if cfg.Mode == "full" && strings.TrimSpace(cfg.EnvironmentManifestEnv) == "" {
		return suiteConfig{}, nil, errors.New("runner: full suites require environment_manifest_env")
	}
	if cfg.Mode == "full" {
		if cfg.MeasuredRunsPerWorker < 30 {
			return suiteConfig{}, nil, errors.New("runner: full suites require at least 30 measured runs per worker")
		}
		for _, required := range []int{1, 8, 32} {
			if !containsInt(cfg.Concurrency, required) {
				return suiteConfig{}, nil, fmt.Errorf("runner: full suites must include concurrency %d", required)
			}
		}
	}
	if !sameStrings(cfg.BaselineOrder, requiredBaselines) {
		return suiteConfig{}, nil, fmt.Errorf("runner: baseline_order must contain exactly %v", requiredBaselines)
	}
	if len(cfg.Experiments) == 0 || len(cfg.Concurrency) == 0 {
		return suiteConfig{}, nil, errors.New("runner: at least one experiment and concurrency are required")
	}
	seenExperiments := make(map[string]bool)
	seenConcurrency := make(map[int]bool)
	configDir, _ := filepath.Abs(filepath.Dir(path))
	for _, concurrency := range cfg.Concurrency {
		if concurrency < 1 || seenConcurrency[concurrency] {
			return suiteConfig{}, nil, errors.New("runner: concurrency values must be positive and unique")
		}
		seenConcurrency[concurrency] = true
	}
	for index := range cfg.Experiments {
		exp := &cfg.Experiments[index]
		exp.configDir = configDir
		if exp.ID == "" || seenExperiments[exp.ID] || (exp.Family != "tpch" && exp.Family != "tpcds") || (exp.ScaleFactor != 1 && exp.ScaleFactor != 10) {
			return suiteConfig{}, nil, errors.New("runner: experiments need unique IDs, family tpch|tpcds, and scale_factor 1|10")
		}
		seenExperiments[exp.ID] = true
		if exp.Workload == "" || exp.DirectDSNEnv == "" || exp.NativeDSNEnv == "" || exp.ASTURLenv == "" || exp.TaskGateURLenv == "" || exp.TaskGateTokenEnv == "" || exp.TaskGateTasksFileEnv == "" {
			return suiteConfig{}, nil, fmt.Errorf("runner: experiment %s omits a workload or backend environment variable name", exp.ID)
		}
		if cfg.Mode == "full" && (exp.DatasetDigestEnv == "" || exp.DatasetManifestEnv == "") {
			return suiteConfig{}, nil, fmt.Errorf("runner: full experiment %s requires dataset_digest_env and dataset_manifest_env", exp.ID)
		}
		if cfg.CacheStrategy == cacheStrategyCold && !sameStringKeys(exp.CacheResetEnv, requiredBaselines) {
			return suiteConfig{}, nil, fmt.Errorf("runner: cold-cache experiment %s requires cache_reset_env entries for all baselines", exp.ID)
		}
	}
	return cfg, data, nil
}

func loadWorkloads(cfg suiteConfig) (map[string][]loadedQuery, error) {
	result := make(map[string][]loadedQuery, len(cfg.Experiments))
	for _, exp := range cfg.Experiments {
		manifestPath := exp.Workload
		if !filepath.IsAbs(manifestPath) {
			manifestPath = filepath.Join(exp.configDir, manifestPath)
		}
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, fmt.Errorf("runner: read workload %s: %w", exp.ID, err)
		}
		var manifest workloadManifest
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil {
			return nil, fmt.Errorf("runner: decode workload %s: %w", exp.ID, err)
		}
		if manifest.SchemaVersion != schemaVersion || manifest.ID == "" || manifest.Family != exp.Family || manifest.Lineage != cfg.WorkloadLineage || len(manifest.Queries) == 0 {
			return nil, fmt.Errorf("runner: workload %s has invalid identity, family, lineage, or empty queries", exp.ID)
		}
		manifest.path = manifestPath
		queries := make([]loadedQuery, 0, len(manifest.Queries))
		seen := make(map[string]bool)
		for _, entry := range manifest.Queries {
			if entry.ID == "" || seen[entry.ID] || !sameStringKeys(entry.SQL, requiredBaselines) {
				return nil, fmt.Errorf("runner: workload %s query IDs must be unique and map all four baselines", exp.ID)
			}
			seen[entry.ID] = true
			loaded := loadedQuery{ID: entry.ID, SQL: make(map[string]string, len(entry.SQL))}
			for baseline, relativePath := range entry.SQL {
				queryPath := relativePath
				if !filepath.IsAbs(queryPath) {
					queryPath = filepath.Join(filepath.Dir(manifestPath), queryPath)
				}
				query, err := os.ReadFile(queryPath)
				if err != nil {
					return nil, fmt.Errorf("runner: read %s/%s/%s: %w", exp.ID, entry.ID, baseline, err)
				}
				if strings.TrimSpace(string(query)) == "" {
					return nil, fmt.Errorf("runner: query %s/%s/%s is empty", exp.ID, entry.ID, baseline)
				}
				loaded.SQL[baseline] = string(query)
			}
			queries = append(queries, loaded)
		}
		result[exp.ID] = queries
		required := taskQueriesForLargestCell(cfg, len(queries))
		if cfg.TaskGateQueriesPerTask != required {
			return nil, fmt.Errorf("runner: taskgate_queries_per_task=%d, want %d for workload %s (%d queries x %d warmup+measured calls%s)",
				cfg.TaskGateQueriesPerTask, required, exp.ID, len(queries), cfg.WarmupRunsPerWorker+cfg.MeasuredRunsPerWorker,
				taskQueryConcurrencySuffix(cfg))
		}
	}
	return result, nil
}

func taskQueriesForCell(cfg suiteConfig, queryCount, concurrency int) int {
	result := queryCount * (cfg.WarmupRunsPerWorker + cfg.MeasuredRunsPerWorker)
	if cfg.TaskConcurrencyMode == taskConcurrencySameTask {
		result *= concurrency
	}
	return result
}

func taskQueriesForLargestCell(cfg suiteConfig, queryCount int) int {
	result := queryCount * (cfg.WarmupRunsPerWorker + cfg.MeasuredRunsPerWorker)
	if cfg.TaskConcurrencyMode != taskConcurrencySameTask {
		return result
	}
	largest := 0
	for _, concurrency := range cfg.Concurrency {
		if concurrency > largest {
			largest = concurrency
		}
	}
	return result * largest
}

func taskQueryConcurrencySuffix(cfg suiteConfig) string {
	if cfg.TaskConcurrencyMode == taskConcurrencySameTask {
		return " x largest shared-task concurrency"
	}
	return ""
}

func loadTaskPool(path, experimentID string, concurrency int, taskConcurrencyMode string) ([]string, error) {
	pool, err := readTaskPoolFile(path)
	if err != nil {
		return nil, err
	}
	return tasksForCell(pool, experimentID, concurrency, taskConcurrencyMode)
}

func readTaskPoolFile(path string) (taskPoolFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return taskPoolFile{}, fmt.Errorf("read TaskGate task pool: %w", err)
	}
	var pool taskPoolFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pool); err != nil {
		return taskPoolFile{}, fmt.Errorf("decode TaskGate task pool: %w", err)
	}
	if pool.SchemaVersion != schemaVersion {
		return taskPoolFile{}, errors.New("TaskGate task pool schema_version must be 1")
	}
	return pool, nil
}

func tasksForCell(pool taskPoolFile, experimentID string, concurrency int, taskConcurrencyMode string) ([]string, error) {
	tasks := pool.Experiments[experimentID][strconv.Itoa(concurrency)]
	switch taskConcurrencyMode {
	case taskConcurrencyDistinct:
		if len(tasks) != concurrency {
			return nil, fmt.Errorf("TaskGate task pool %s/c%d must contain exactly %d tasks for distinct_task mode, got %d", experimentID, concurrency, concurrency, len(tasks))
		}
		seen := make(map[string]bool)
		for _, task := range tasks {
			if strings.TrimSpace(task) == "" || seen[task] {
				return nil, fmt.Errorf("TaskGate task pool %s/c%d contains empty or duplicate task IDs", experimentID, concurrency)
			}
			seen[task] = true
		}
		return append([]string(nil), tasks...), nil
	case taskConcurrencySameTask:
		if len(tasks) != 1 {
			return nil, fmt.Errorf("TaskGate task pool %s/c%d must contain exactly 1 task for same_task mode, got %d", experimentID, concurrency, len(tasks))
		}
		if strings.TrimSpace(tasks[0]) == "" {
			return nil, fmt.Errorf("TaskGate task pool %s/c%d contains an empty task ID", experimentID, concurrency)
		}
		result := make([]string, concurrency)
		for index := range result {
			result[index] = tasks[0]
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown task_concurrency_mode %q", taskConcurrencyMode)
	}
}

func reserveUniqueTasks(seen map[string]string, tasks []string, cell string) error {
	local := make(map[string]bool)
	for _, task := range tasks {
		if local[task] {
			continue
		}
		local[task] = true
		if previous := seen[task]; previous != "" {
			return fmt.Errorf("TaskGate task ID is reused across cells: %s appears in %s and %s", task, previous, cell)
		}
		seen[task] = cell
	}
	return nil
}

func buildMetadata(cfg suiteConfig, configPath string, configBytes []byte, workloads map[string][]loadedQuery, runID string) (runMetadata, error) {
	relativeConfig, err := repositoryRelativePath(configPath)
	if err != nil {
		return runMetadata{}, fmt.Errorf("config provenance: %w", err)
	}
	campaignID := strings.TrimSpace(os.Getenv("EVAL_CAMPAIGN_ID"))
	if cfg.Mode == "full" && campaignID == "" {
		return runMetadata{}, errors.New("full evaluation requires EVAL_CAMPAIGN_ID to link SF1 and SF10 runs")
	}
	if campaignID != "" && !campaignIDPattern.MatchString(campaignID) {
		return runMetadata{}, errors.New("EVAL_CAMPAIGN_ID must be 1-128 URL-safe identifier characters")
	}
	metadata := runMetadata{
		SchemaVersion: schemaVersion, RunID: runID, Suite: cfg.Name, Mode: cfg.Mode,
		CampaignID: campaignID,
		Status:     "running", StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		GitRevision: os.Getenv("EVAL_GIT_REVISION"), GitDirty: os.Getenv("EVAL_GIT_DIRTY") == "1",
		ConfigPath: relativeConfig, ConfigSHA256: sha256Hex(configBytes),
		WorkloadSHA256: make(map[string]string), DatasetSHA256Manifests: make(map[string]string),
		DatasetManifestPaths: make(map[string]string), MetricsProbePaths: make(map[string]map[string]string),
		MetricsProbeSHA256: make(map[string]map[string]string),
		CacheResetPaths:    make(map[string]map[string]string), CacheResetSHA256: make(map[string]map[string]string),
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		BaselineOrder:          append([]string(nil), cfg.BaselineOrder...),
		BaselineOrderSeed:      cfg.Seed,
		OrderingStrategy:       cfg.OrderingStrategy,
		CellOrder:              buildCellSchedule(cfg),
		CacheStrategy:          cfg.CacheStrategy,
		TaskConcurrencyMode:    cfg.TaskConcurrencyMode,
		WorkloadLineage:        cfg.WorkloadLineage,
		WarmupRunsPerWorker:    cfg.WarmupRunsPerWorker,
		MeasuredRunsPerWorker:  cfg.MeasuredRunsPerWorker,
		TaskGateQueriesPerTask: cfg.TaskGateQueriesPerTask,
		Concurrency:            append([]int(nil), cfg.Concurrency...), Endpoints: make(map[string]interface{}),
	}
	envDigest, envPath, err := environmentManifestProvenance(cfg)
	if err != nil {
		return runMetadata{}, err
	}
	metadata.EnvironmentManifestSHA256 = envDigest
	metadata.EnvironmentManifestPath = envPath
	for _, exp := range cfg.Experiments {
		queryHasher := sha256.New()
		for _, query := range workloads[exp.ID] {
			_, _ = io.WriteString(queryHasher, query.ID)
			for _, baseline := range requiredBaselines {
				_, _ = io.WriteString(queryHasher, baseline)
				_, _ = io.WriteString(queryHasher, query.SQL[baseline])
			}
		}
		metadata.WorkloadSHA256[exp.ID] = hex.EncodeToString(queryHasher.Sum(nil))
		if cfg.Mode == "full" {
			digest, manifestPath, provenanceErr := datasetProvenance(exp)
			if provenanceErr != nil {
				return runMetadata{}, provenanceErr
			}
			metadata.DatasetSHA256Manifests[exp.ID] = digest
			metadata.DatasetManifestPaths[exp.ID] = manifestPath
		}
		if cfg.RequireResourceMetrics {
			metadata.MetricsProbePaths[exp.ID] = make(map[string]string)
			metadata.MetricsProbeSHA256[exp.ID] = make(map[string]string)
			for _, baseline := range requiredBaselines {
				envName := exp.MetricsProbeEnv[baseline]
				_, probePath, provenanceErr := metricsProbeProvenance(envName)
				if provenanceErr != nil {
					return runMetadata{}, provenanceErr
				}
				probeBytes, readErr := os.ReadFile(filepath.Join("/workspace", filepath.FromSlash(probePath)))
				if readErr != nil {
					return runMetadata{}, fmt.Errorf("read metrics probe %s: %w", envName, readErr)
				}
				metadata.MetricsProbePaths[exp.ID][baseline] = probePath
				metadata.MetricsProbeSHA256[exp.ID][baseline] = sha256Hex(probeBytes)
			}
		}
		if cfg.CacheStrategy == cacheStrategyCold {
			metadata.CacheResetPaths[exp.ID] = make(map[string]string)
			metadata.CacheResetSHA256[exp.ID] = make(map[string]string)
			for _, baseline := range requiredBaselines {
				envName := exp.CacheResetEnv[baseline]
				_, resetPath, provenanceErr := cacheResetProvenance(envName)
				if provenanceErr != nil {
					return runMetadata{}, provenanceErr
				}
				resetBytes, readErr := os.ReadFile(filepath.Join("/workspace", filepath.FromSlash(resetPath)))
				if readErr != nil {
					return runMetadata{}, fmt.Errorf("read cache reset command %s: %w", envName, readErr)
				}
				metadata.CacheResetPaths[exp.ID][baseline] = resetPath
				metadata.CacheResetSHA256[exp.ID][baseline] = sha256Hex(resetBytes)
			}
		}
		metadata.Endpoints[exp.ID] = map[string]string{
			baselineDirect:           redactDSN(os.Getenv(exp.DirectDSNEnv)),
			baselineNativeView:       redactDSN(os.Getenv(exp.NativeDSNEnv)),
			baselineASTOnly:          redactedURL(os.Getenv(exp.ASTURLenv)),
			baselineResourceTaskGate: redactedURL(os.Getenv(exp.TaskGateURLenv)),
		}
	}
	return metadata, nil
}

func validateResultEquivalence(samples []sample) error {
	type key struct {
		experiment  string
		concurrency int
		query       string
	}
	seen := make(map[key]map[string]map[string]bool)
	for _, observation := range samples {
		if !observation.Success {
			continue
		}
		cellKey := key{observation.Experiment, observation.Concurrency, observation.QueryID}
		if seen[cellKey] == nil {
			seen[cellKey] = make(map[string]map[string]bool)
		}
		if seen[cellKey][observation.Baseline] == nil {
			seen[cellKey][observation.Baseline] = make(map[string]bool)
		}
		seen[cellKey][observation.Baseline][observation.ResultSHA256] = true
	}
	for cellKey, baselines := range seen {
		var expected string
		for _, baseline := range requiredBaselines {
			hashes := baselines[baseline]
			if len(hashes) != 1 {
				return fmt.Errorf("result stability failure for %s/c%d/%s/%s", cellKey.experiment, cellKey.concurrency, cellKey.query, baseline)
			}
			for hash := range hashes {
				if expected == "" {
					expected = hash
				} else if hash != expected {
					return fmt.Errorf("four-baseline result mismatch for %s/c%d/%s", cellKey.experiment, cellKey.concurrency, cellKey.query)
				}
			}
		}
	}
	return nil
}

func writeSamples(directory string, samples []sample) error {
	if err := writeJSONLines(filepath.Join(directory, "samples.jsonl"), samples); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(directory, "samples.csv"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("runner: create samples CSV: %w", err)
	}
	writer := csv.NewWriter(file)
	header := []string{"schema_version", "run_id", "experiment", "family", "scale_factor", "baseline", "concurrency", "worker", "iteration", "query_id", "request_id", "started_unix_ns", "latency_ms", "database_ms", "row_count", "result_sha256", "receipt_bytes", "component_ms_json", "success", "error"}
	if err := writer.Write(header); err != nil {
		_ = file.Close()
		return err
	}
	for _, item := range samples {
		component, _ := json.Marshal(item.ComponentMS)
		databaseMS := ""
		if item.DatabaseMS != nil {
			databaseMS = formatFloat(*item.DatabaseMS)
		}
		receiptBytes := ""
		if item.ReceiptBytes != nil {
			receiptBytes = strconv.FormatInt(*item.ReceiptBytes, 10)
		}
		record := []string{
			strconv.Itoa(item.SchemaVersion), item.RunID, item.Experiment, item.Family,
			strconv.Itoa(item.ScaleFactor), item.Baseline, strconv.Itoa(item.Concurrency),
			strconv.Itoa(item.Worker), strconv.Itoa(item.Iteration), item.QueryID, item.RequestID,
			strconv.FormatInt(item.StartedUnixNS, 10), formatFloat(item.LatencyMS), databaseMS,
			strconv.FormatInt(item.RowCount, 10), item.ResultSHA256, receiptBytes,
			string(component), strconv.FormatBool(item.Success), item.Error,
		}
		if err := writer.Write(record); err != nil {
			_ = file.Close()
			return err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeJSONLines[T any](path string, items []T) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("runner: create %s: %w", path, err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	for _, item := range items {
		if err := encoder.Encode(item); err != nil {
			_ = file.Close()
			return fmt.Errorf("runner: write %s: %w", path, err)
		}
	}
	return file.Close()
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("runner: write %s: %w", path, err)
	}
	return nil
}

func makeRequestID(runID, experiment, baseline string, concurrency, worker, queryIndex, iteration int, phase string) string {
	input := fmt.Sprintf("%s/%s/%s/%d/%d/%d/%d/%s", runID, experiment, baseline, concurrency, worker, queryIndex, iteration, phase)
	digest := sha256.Sum256([]byte(input))
	return "eval-" + phase + "-" + hex.EncodeToString(digest[:16])
}

func hashJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return sha256Hex(compactJSON(raw))
}

func compactJSON(raw []byte) []byte {
	var output bytes.Buffer
	if err := json.Compact(&output, raw); err != nil {
		return raw
	}
	return output.Bytes()
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func sameStrings(actual, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	values := make(map[string]bool, len(actual))
	for _, item := range actual {
		if values[item] {
			return false
		}
		values[item] = true
	}
	for _, item := range expected {
		if !values[item] {
			return false
		}
	}
	return true
}

func sameStringKeys(actual map[string]string, expected []string) bool {
	if len(actual) != len(expected) {
		return false
	}
	for _, key := range expected {
		if actual[key] == "" {
			return false
		}
	}
	return true
}

func containsInt(values []int, expected int) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validateHTTPURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return errors.New("must be an http(s) URL without userinfo")
	}
	return nil
}

func redactDSN(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "configured-invalid-url"
	}
	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}
	parsed.RawQuery = ""
	return parsed.String()
}

func redactedURL(value string) string {
	if value == "" {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "configured-invalid-url"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func optionalEnv(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

func safeError(value string) string {
	if value == "" {
		return "no error body"
	}
	if len(value) > 120 {
		return value[:120]
	}
	return value
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Nanoseconds()) / float64(time.Millisecond)
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', 6, 64)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
