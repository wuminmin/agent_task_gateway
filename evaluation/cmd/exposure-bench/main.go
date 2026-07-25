package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
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
	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	reportSchema = 1
	querySQL     = `SELECT receipt_no, department, amount
FROM reporting.expense_detail
WHERE department = '销售部'
ORDER BY receipt_no ASC
LIMIT 1 OFFSET %d`
)

type options struct {
	mode            string
	gatewayURL      string
	gatewayToken    string
	oaURL           string
	alicePassword   string
	bobPassword     string
	businessDSN     string
	controlDSN      string
	tasksPath       string
	outputDir       string
	workers         int
	runs            int
	rampRuns        int
	concurrencies   []int
	lockInterval    time.Duration
	requestTimeout  time.Duration
	statementTimout time.Duration
}

type taskPool struct {
	SchemaVersion int      `json:"schema_version"`
	RootTaskID    string   `json:"root_task_id"`
	WorkerTaskIDs []string `json:"worker_task_ids"`
}

type sample struct {
	Phase                  string             `json:"phase"`
	Concurrency            int                `json:"concurrency"`
	Worker                 int                `json:"worker"`
	Iteration              int                `json:"iteration"`
	LatencyMS              float64            `json:"latency_ms"`
	DatabaseMS             float64            `json:"database_ms,omitempty"`
	ComponentMS            map[string]float64 `json:"component_ms,omitempty"`
	ActualReleaseFacts     int64              `json:"actual_release_facts,omitempty"`
	ActualInfluenceFacts   int64              `json:"actual_influence_facts,omitempty"`
	ChargedReleaseFacts    int64              `json:"charged_release_facts,omitempty"`
	ChargedInfluenceFacts  int64              `json:"charged_influence_facts,omitempty"`
	ObservationSHA256      string             `json:"observation_sha256,omitempty"`
	Rows                   int64              `json:"rows"`
	RequestID              string             `json:"request_id,omitempty"`
}

type distribution struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

type ledgerSnapshot struct {
	ReleaseUsed       int64 `json:"release_used"`
	InfluenceUsed     int64 `json:"influence_used"`
	FactRows          int64 `json:"fact_rows"`
	FactPayloadBytes  int64 `json:"fact_payload_bytes"`
	TableBytes        int64 `json:"table_bytes"`
	IndexesBytes      int64 `json:"indexes_bytes"`
}

type ledgerDelta struct {
	ReleaseUsed      int64 `json:"release_used"`
	InfluenceUsed    int64 `json:"influence_used"`
	FactRows         int64 `json:"fact_rows"`
	FactPayloadBytes int64 `json:"fact_payload_bytes"`
	TableBytes       int64 `json:"table_bytes"`
	IndexesBytes     int64 `json:"indexes_bytes"`
}

type lockStats struct {
	Samples                int   `json:"samples"`
	SamplesWithWaiters     int   `json:"samples_with_waiters"`
	MaxWaitingSessions     int64 `json:"max_waiting_sessions"`
	WaitingSessionMillis   int64 `json:"waiting_session_ms_approx"`
	SamplerErrors          int   `json:"sampler_errors"`
}

type cell struct {
	Phase                 string                    `json:"phase"`
	Concurrency           int                       `json:"concurrency"`
	Samples               int                       `json:"samples"`
	ElapsedMS             float64                   `json:"elapsed_ms"`
	ThroughputQPS         float64                   `json:"throughput_qps"`
	LatencyMS             distribution              `json:"latency_ms"`
	DatabaseMS            *distribution             `json:"database_ms,omitempty"`
	ComponentMS           map[string]distribution   `json:"component_ms,omitempty"`
	ClientPeakHeapBytes   uint64                    `json:"client_peak_heap_bytes"`
	LedgerBefore          *ledgerSnapshot           `json:"ledger_before,omitempty"`
	LedgerAfter           *ledgerSnapshot           `json:"ledger_after,omitempty"`
	LedgerGrowth          *ledgerDelta              `json:"ledger_growth,omitempty"`
	LockContention        *lockStats                `json:"lock_contention,omitempty"`
	FactHistoryHitRate    *float64                  `json:"fact_history_hit_rate,omitempty"`
	QueryHistoryHitRate   *float64                  `json:"query_history_hit_rate,omitempty"`
	ActualFacts           int64                     `json:"actual_facts,omitempty"`
	ChargedFacts          int64                     `json:"charged_facts,omitempty"`
}

type report struct {
	SchemaVersion          int               `json:"schema_version"`
	Status                 string            `json:"status"`
	StartedAt              time.Time         `json:"started_at"`
	FinishedAt             time.Time         `json:"finished_at"`
	Configuration          map[string]any    `json:"configuration"`
	TaskFamily             map[string]any    `json:"task_family"`
	MetricSemantics        map[string]string `json:"metric_semantics"`
	Cells                  []cell            `json:"cells"`
	ServicePeakMemoryBytes map[string]int64  `json:"service_peak_memory_bytes,omitempty"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

type executeResponse struct {
	RowCount    int64              `json:"row_count"`
	DatabaseMS  float64            `json:"database_ms"`
	ComponentMS map[string]float64 `json:"component_ms"`
	Exposure    struct {
		ActualReleaseFacts    int64  `json:"actual_release_facts"`
		ActualInfluenceFacts  int64  `json:"actual_influence_facts"`
		ChargedReleaseFacts   int64  `json:"charged_release_facts"`
		ChargedInfluenceFacts int64  `json:"charged_influence_facts"`
		ObservationSHA256     string `json:"observation_sha256"`
	} `json:"exposure"`
}

func main() {
	opts, err := parseOptions()
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()
	switch opts.mode {
	case "bootstrap":
		err = bootstrap(ctx, opts)
	case "run":
		err = run(ctx, opts)
	default:
		err = fmt.Errorf("unsupported mode %q", opts.mode)
	}
	if err != nil {
		fatal(err)
	}
}

func parseOptions() (options, error) {
	var opts options
	var concurrencies string
	flag.StringVar(&opts.mode, "mode", env("EXPOSURE_BENCH_MODE", "run"), "bootstrap or run")
	flag.StringVar(&opts.gatewayURL, "gateway-url", env("EXPOSURE_GATEWAY_URL", "http://gateway:8082"), "Gateway base URL")
	flag.StringVar(&opts.gatewayToken, "gateway-token", env("TASKBOUND_ALICE_TOKEN", ""), "Alice MCP bearer token")
	flag.StringVar(&opts.oaURL, "oa-url", env("EXPOSURE_OA_URL", "http://oa-demo:8092"), "OA base URL")
	flag.StringVar(&opts.alicePassword, "alice-password", env("OA_ALICE_PASSWORD", ""), "OA Alice password")
	flag.StringVar(&opts.bobPassword, "bob-password", env("OA_BOB_PASSWORD", ""), "OA Bob password")
	flag.StringVar(&opts.businessDSN, "business-dsn", env("EXPOSURE_BUSINESS_DSN", ""), "Business PostgreSQL DSN")
	flag.StringVar(&opts.controlDSN, "control-dsn", env("EXPOSURE_CONTROL_DSN", ""), "Control PostgreSQL DSN")
	flag.StringVar(&opts.tasksPath, "tasks", env("EXPOSURE_TASKS_PATH", "/results/tasks.json"), "task pool JSON")
	flag.StringVar(&opts.outputDir, "output", env("EXPOSURE_OUTPUT_DIR", "/results"), "output directory")
	flag.IntVar(&opts.workers, "workers", envInt("EXPOSURE_WORKERS", 4), "bootstrap worker task count")
	flag.IntVar(&opts.runs, "runs", envInt("EXPOSURE_RUNS", 10), "measured operations per worker")
	flag.IntVar(&opts.rampRuns, "ramp-runs", envInt("EXPOSURE_RAMP_RUNS", 8), "sequential history-ramp operations")
	flag.StringVar(&concurrencies, "concurrency", env("EXPOSURE_CONCURRENCY", "1,4"), "comma-separated concurrency levels")
	flag.DurationVar(&opts.lockInterval, "lock-sample-interval", envDuration("EXPOSURE_LOCK_SAMPLE_INTERVAL", 10*time.Millisecond), "Control PG lock sampling interval")
	flag.DurationVar(&opts.requestTimeout, "request-timeout", envDuration("EXPOSURE_REQUEST_TIMEOUT", 30*time.Second), "MCP/OA request timeout")
	flag.DurationVar(&opts.statementTimout, "statement-timeout", envDuration("EXPOSURE_STATEMENT_TIMEOUT", 5*time.Second), "Business PostgreSQL statement timeout")
	flag.Parse()
	for _, raw := range strings.Split(concurrencies, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value < 1 {
			return opts, fmt.Errorf("invalid concurrency %q", raw)
		}
		opts.concurrencies = append(opts.concurrencies, value)
	}
	if opts.gatewayToken == "" {
		return opts, errors.New("TASKBOUND_ALICE_TOKEN or -gateway-token is required")
	}
	if opts.mode == "bootstrap" && (opts.alicePassword == "" || opts.bobPassword == "") {
		return opts, errors.New("OA_ALICE_PASSWORD and OA_BOB_PASSWORD are required for bootstrap")
	}
	if opts.mode == "run" && (opts.businessDSN == "" || opts.controlDSN == "") {
		return opts, errors.New("business and control PostgreSQL DSNs are required")
	}
	return opts, nil
}

func bootstrap(ctx context.Context, opts options) error {
	if opts.workers < maxInt(opts.concurrencies...) {
		return fmt.Errorf("workers=%d is below maximum concurrency", opts.workers)
	}
	alice, err := oaClient(opts.oaURL, "alice", opts.alicePassword, opts.requestTimeout)
	if err != nil {
		return err
	}
	bob, err := oaClient(opts.oaURL, "bob", opts.bobPassword, opts.requestTimeout)
	if err != nil {
		return err
	}
	mcp := &mcpClient{url: strings.TrimRight(opts.gatewayURL, "/") + "/mcp", token: opts.gatewayToken,
		http: &http.Client{Timeout: opts.requestTimeout}}
	pool := taskPool{SchemaVersion: reportSchema}
	for index := 0; index < opts.workers; index++ {
		arguments := map[string]any{
			"objective":     fmt.Sprintf("Exposure end-to-end benchmark worker %d", index),
			"data_products": []string{"expense_detail"},
			"columns": map[string][]string{"expense_detail": {"receipt_no", "department", "amount"}},
			"scopes":  map[string]any{"department": []string{"销售部"}},
			"requested_budget": map[string]int64{"max_queries": 1000, "max_rows": 1000,
				"max_release_facts": 100000, "max_influence_facts": 500000},
		}
		if index > 0 {
			arguments["parent_task_id"] = pool.RootTaskID
		}
		var created struct {
			TaskID string `json:"task_id"`
			OAURL  string `json:"oa_url"`
		}
		if err := mcp.call(ctx, "request_data_task", arguments, &created); err != nil {
			return fmt.Errorf("request worker task %d: %w", index, err)
		}
		if created.TaskID == "" || created.OAURL == "" {
			return fmt.Errorf("worker task %d omitted task_id or oa_url", index)
		}
		if index == 0 {
			pool.RootTaskID = created.TaskID
		}
		pool.WorkerTaskIDs = append(pool.WorkerTaskIDs, created.TaskID)
		draftID := pathTail(created.OAURL)
		if err := oaAction(ctx, alice, opts.oaURL, draftID, "submit", ""); err != nil {
			return fmt.Errorf("submit worker task %d: %w", index, err)
		}
		if err := waitTask(ctx, mcp, created.TaskID, "AWAITING_APPROVAL", opts.requestTimeout); err != nil {
			return err
		}
		if err := oaAction(ctx, bob, opts.oaURL, draftID, "decision", "approved"); err != nil {
			return fmt.Errorf("approve worker task %d: %w", index, err)
		}
		if err := waitTask(ctx, mcp, created.TaskID, "ACTIVE", opts.requestTimeout); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "bootstrapped task %d/%d\n", index+1, opts.workers)
	}
	if err := os.MkdirAll(filepath.Dir(opts.tasksPath), 0o755); err != nil {
		return err
	}
	return writeJSON(opts.tasksPath, pool)
}

func run(ctx context.Context, opts options) error {
	started := time.Now().UTC()
	var tasks taskPool
	if err := readJSON(opts.tasksPath, &tasks); err != nil {
		return err
	}
	if tasks.SchemaVersion != reportSchema || tasks.RootTaskID == "" || len(tasks.WorkerTaskIDs) < maxInt(opts.concurrencies...) {
		return errors.New("invalid or undersized task pool")
	}
	controlPool, err := pgxpool.New(ctx, opts.controlDSN)
	if err != nil {
		return fmt.Errorf("open control PostgreSQL: %w", err)
	}
	defer controlPool.Close()
	if err := validateTaskFamily(ctx, controlPool, tasks); err != nil {
		return err
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{DSN: opts.businessDSN,
		StatementTimeout: opts.statementTimout, MaxRows: 10, MaxConnections: int32(maxInt(opts.concurrencies...) + 2),
		ApplicationName: "taskgate-exposure-bench"})
	if err != nil {
		return fmt.Errorf("open business PostgreSQL: %w", err)
	}
	defer connector.Close()
	mcp := &mcpClient{url: strings.TrimRight(opts.gatewayURL, "/") + "/mcp", token: opts.gatewayToken,
		http: &http.Client{Timeout: opts.requestTimeout}}
	result := report{SchemaVersion: reportSchema, Status: "smoke", StartedAt: started,
		Configuration: map[string]any{"runs_per_worker": opts.runs, "ramp_runs": opts.rampRuns,
			"concurrency": opts.concurrencies, "lock_sample_interval_ms": float64(opts.lockInterval) / float64(time.Millisecond),
			"workload": "expense_detail/sales/ordered/limit-1", "cache_strategy": "warm", "task_concurrency_mode": "delegated_tasks_shared_root"},
		TaskFamily: map[string]any{"root_task_sha256": hashString(tasks.RootTaskID), "worker_count": len(tasks.WorkerTaskIDs)},
		MetricSemantics: metricSemantics()}
	var allSamples []sample
	for _, concurrency := range opts.concurrencies {
		for _, phase := range []string{"business_sql", "paired_snapshot", "paired_plus_algebra"} {
			cellResult, samples, err := runCell(ctx, opts, phase, concurrency, nil, connector, controlPool, mcp, tasks)
			if err != nil {
				return fmt.Errorf("%s concurrency %d: %w", phase, concurrency, err)
			}
			result.Cells = append(result.Cells, cellResult)
			allSamples = append(allSamples, samples...)
			fmt.Fprintf(os.Stderr, "measured %s c=%d\n", phase, concurrency)
		}
	}
	rampCell, rampSamples, err := runCell(ctx, opts, "full_history_ramp", 1, &tasks, connector, controlPool, mcp, tasks)
	if err != nil {
		return err
	}
	result.Cells = append(result.Cells, rampCell)
	allSamples = append(allSamples, rampSamples...)
	for _, concurrency := range opts.concurrencies {
		cellResult, samples, err := runCell(ctx, opts, "full_history_hit", concurrency, &tasks, connector, controlPool, mcp, tasks)
		if err != nil {
			return fmt.Errorf("full_history_hit concurrency %d: %w", concurrency, err)
		}
		result.Cells = append(result.Cells, cellResult)
		allSamples = append(allSamples, samples...)
		fmt.Fprintf(os.Stderr, "measured full_history_hit c=%d\n", concurrency)
	}
	result.FinishedAt = time.Now().UTC()
	if err := os.MkdirAll(opts.outputDir, 0o755); err != nil {
		return err
	}
	if err := writeSamples(filepath.Join(opts.outputDir, "samples.jsonl"), allSamples); err != nil {
		return err
	}
	return writeJSON(filepath.Join(opts.outputDir, "report.json"), result)
}

func runCell(ctx context.Context, opts options, phase string, concurrency int, full *taskPool,
	connector *dataconnector.Connector, controlPool *pgxpool.Pool, mcp *mcpClient, tasks taskPool) (cell, []sample, error) {
	iterations := opts.runs
	if phase == "full_history_ramp" {
		iterations = opts.rampRuns
		concurrency = 1
	}
	var before *ledgerSnapshot
	if full != nil {
		value, err := readLedger(ctx, controlPool, tasks.RootTaskID)
		if err != nil {
			return cell{}, nil, err
		}
		before = &value
	}
	cellCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	peakDone := make(chan uint64, 1)
	go sampleHeap(cellCtx, peakDone)
	var lockDone chan lockStats
	if full != nil {
		lockDone = make(chan lockStats, 1)
		go sampleLocks(cellCtx, controlPool, opts.lockInterval, lockDone)
	}
	started := time.Now()
	samples := make([]sample, 0, concurrency*iterations)
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				if firstErr != nil {
					return
				}
				one, err := executeSample(ctx, opts, phase, worker, iteration, connector, mcp, tasks)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if err == nil {
					samples = append(samples, one)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	cancel()
	peak := <-peakDone
	var locks *lockStats
	if lockDone != nil {
		value := <-lockDone
		locks = &value
	}
	if firstErr != nil {
		return cell{}, samples, firstErr
	}
	var after *ledgerSnapshot
	if full != nil {
		value, err := readLedger(ctx, controlPool, tasks.RootTaskID)
		if err != nil {
			return cell{}, samples, err
		}
		after = &value
	}
	return summarizeCell(phase, concurrency, elapsed, peak, before, after, locks, samples), samples, nil
}

func executeSample(ctx context.Context, opts options, phase string, worker, iteration int,
	connector *dataconnector.Connector, mcp *mcpClient, tasks taskPool) (sample, error) {
	offset := (worker*opts.runs + iteration) % 4
	if phase == "full_history_hit" {
		offset = 0
	}
	statement := fmt.Sprintf(querySQL, offset)
	result := sample{Phase: phase, Worker: worker, Iteration: iteration, Rows: 1}
	started := time.Now()
	switch phase {
	case "business_sql":
		data, err := connector.Query(ctx, dataconnector.QueryRequest{SQL: statement, StatementTimeout: opts.statementTimout, MaxRows: 1})
		result.LatencyMS = durationMS(time.Since(started))
		if err != nil {
			return result, err
		}
		result.Rows = data.RowCount
		result.DatabaseMS = durationMS(data.DatabaseTime)
	case "paired_snapshot", "paired_plus_algebra":
		pair, err := connector.QueryPair(ctx, dataconnector.QueryPairRequest{
			Visible: dataconnector.QueryRequest{SQL: statement, StatementTimeout: opts.statementTimout, MaxRows: 1},
			Provenance: dataconnector.QueryRequest{SQL: statement, StatementTimeout: opts.statementTimout, MaxRows: 1},
		})
		if err != nil {
			return result, err
		}
		result.Rows = pair.Visible.RowCount
		result.DatabaseMS = durationMS(pair.Visible.DatabaseTime + pair.Provenance.DatabaseTime)
		if phase == "paired_plus_algebra" {
			algebraStarted := time.Now()
			observation, err := deriveV2(pair.Provenance)
			if err != nil {
				return result, err
			}
			result.ComponentMS = map[string]float64{"exposure_derivation": durationMS(time.Since(algebraStarted))}
			result.ActualReleaseFacts = int64(len(observation.Release))
			result.ActualInfluenceFacts = int64(len(observation.Influence))
		}
		result.LatencyMS = durationMS(time.Since(started))
	case "full_history_ramp", "full_history_hit":
		requestID := fmt.Sprintf("expbench-%d-%s-%d-%d", time.Now().UnixNano(), phase, worker, iteration)
		arguments := map[string]any{"task_id": tasks.WorkerTaskIDs[worker], "request_id": requestID,
			"plan": map[string]any{"product": "expense_detail", "columns": []string{"receipt_no", "department", "amount"},
				"filters": []map[string]any{{"column": "department", "op": "=", "value": "销售部"}},
				"order_by": []map[string]any{{"column": "receipt_no", "direction": "asc"}}, "limit": 1, "offset": offset}}
		var response executeResponse
		if err := mcp.call(ctx, "execute_plan", arguments, &response); err != nil {
			return result, err
		}
		result.LatencyMS = durationMS(time.Since(started))
		result.DatabaseMS = response.DatabaseMS
		result.ComponentMS = response.ComponentMS
		result.Rows = response.RowCount
		result.RequestID = requestID
		result.ActualReleaseFacts = response.Exposure.ActualReleaseFacts
		result.ActualInfluenceFacts = response.Exposure.ActualInfluenceFacts
		result.ChargedReleaseFacts = response.Exposure.ChargedReleaseFacts
		result.ChargedInfluenceFacts = response.Exposure.ChargedInfluenceFacts
		result.ObservationSHA256 = response.Exposure.ObservationSHA256
	default:
		return result, fmt.Errorf("unknown phase %q", phase)
	}
	if result.Rows != 1 {
		return result, fmt.Errorf("phase %s returned %d rows, want 1", phase, result.Rows)
	}
	result.Concurrency = 1
	return result, nil
}

func deriveV2(data dataconnector.Result) (exposure.Observation, error) {
	positions := make(map[string]int, len(data.Columns))
	for index, column := range data.Columns {
		positions[column.Name] = index
	}
	for _, required := range []string{"receipt_no", "department", "amount"} {
		if _, ok := positions[required]; !ok {
			return exposure.Observation{}, fmt.Errorf("paired result omitted %s", required)
		}
	}
	rows := make([]exposure.BaseRowV2, 0, len(data.Rows))
	for _, values := range data.Rows {
		receipt := values[positions["receipt_no"]]
		canonical, err := exposure.CanonicalSQLValue("text", receipt)
		if err != nil {
			return exposure.Observation{}, err
		}
		key, err := exposure.ComposeCanonicalKeyV2("base-entity", "travel.expense_receipt", "receipt_no", "text", canonical)
		if err != nil {
			return exposure.Observation{}, err
		}
		rows = append(rows, exposure.BaseRowV2{EntityKey: key, Values: map[string]any{
			"receipt_no": receipt, "department": values[positions["department"]], "amount": values[positions["amount"]],
		}})
	}
	relation, err := exposure.ScanV2(exposure.BaseRelationSpecV2{SourceNamespace: "travel.expense_receipt",
		Snapshot: "travel-demo-2026-v1", StableRole: "expense_detail",
		Fields: []exposure.FieldV2{
			{ID: "receipt_no", SQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true},
			{ID: "department", SQLType: "text", Collation: "en_US.utf8", CollationVersion: "2.36", CollationDeterministic: true},
			{ID: "amount", SQLType: "numeric"},
		}, Rows: rows})
	if err != nil {
		return exposure.Observation{}, err
	}
	relation, err = exposure.SelectV2(relation, []string{"department"}, func(exposure.AnnotatedRowV2) exposure.SQLTruth { return exposure.SQLTrue })
	if err != nil {
		return exposure.Observation{}, err
	}
	return exposure.ObserveV2(relation, "receipt_no", "department", "amount")
}

func summarizeCell(phase string, concurrency int, elapsed time.Duration, peak uint64, before, after *ledgerSnapshot,
	locks *lockStats, samples []sample) cell {
	latencies := make([]float64, 0, len(samples))
	database := make([]float64, 0, len(samples))
	components := make(map[string][]float64)
	var actual, charged int64
	var queryHits int
	for index := range samples {
		samples[index].Concurrency = concurrency
		latencies = append(latencies, samples[index].LatencyMS)
		if samples[index].DatabaseMS > 0 {
			database = append(database, samples[index].DatabaseMS)
		}
		for name, value := range samples[index].ComponentMS {
			components[name] = append(components[name], value)
		}
		oneActual := samples[index].ActualReleaseFacts + samples[index].ActualInfluenceFacts
		oneCharged := samples[index].ChargedReleaseFacts + samples[index].ChargedInfluenceFacts
		actual += oneActual
		charged += oneCharged
		if oneActual > 0 && oneCharged == 0 {
			queryHits++
		}
	}
	result := cell{Phase: phase, Concurrency: concurrency, Samples: len(samples), ElapsedMS: durationMS(elapsed),
		ThroughputQPS: float64(len(samples)) / elapsed.Seconds(), LatencyMS: summarize(latencies),
		ClientPeakHeapBytes: peak, LedgerBefore: before, LedgerAfter: after, LockContention: locks,
		ActualFacts: actual, ChargedFacts: charged}
	if len(database) > 0 {
		value := summarize(database)
		result.DatabaseMS = &value
	}
	if len(components) > 0 {
		result.ComponentMS = make(map[string]distribution, len(components))
		for name, values := range components {
			result.ComponentMS[name] = summarize(values)
		}
	}
	if before != nil && after != nil {
		result.LedgerGrowth = &ledgerDelta{ReleaseUsed: after.ReleaseUsed - before.ReleaseUsed,
			InfluenceUsed: after.InfluenceUsed - before.InfluenceUsed, FactRows: after.FactRows - before.FactRows,
			FactPayloadBytes: after.FactPayloadBytes - before.FactPayloadBytes, TableBytes: after.TableBytes - before.TableBytes,
			IndexesBytes: after.IndexesBytes - before.IndexesBytes}
	}
	if actual > 0 {
		factRate := float64(actual-charged) / float64(actual)
		queryRate := float64(queryHits) / float64(len(samples))
		result.FactHistoryHitRate = &factRate
		result.QueryHistoryHitRate = &queryRate
	}
	return result
}

func summarize(values []float64) distribution {
	if len(values) == 0 {
		return distribution{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum float64
	for _, value := range sorted {
		sum += value
	}
	return distribution{Count: len(sorted), Min: sorted[0], P50: percentile(sorted, 0.50), P95: percentile(sorted, 0.95),
		P99: percentile(sorted, 0.99), Max: sorted[len(sorted)-1], Mean: sum / float64(len(sorted))}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := float64(len(sorted)-1) * p
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	return sorted[lower] + (sorted[upper]-sorted[lower])*(position-float64(lower))
}

func readLedger(ctx context.Context, pool *pgxpool.Pool, root string) (ledgerSnapshot, error) {
	var result ledgerSnapshot
	if err := pool.QueryRow(ctx, `SELECT used_release_facts, used_influence_facts FROM exposure_ledgers WHERE root_task_id=$1`, root).
		Scan(&result.ReleaseUsed, &result.InfluenceUsed); err != nil {
		return result, fmt.Errorf("read exposure ledger: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(identity_json::text) + octet_length(canonical_payload)),0)
FROM exposure_facts WHERE root_task_id=$1`, root).Scan(&result.FactRows, &result.FactPayloadBytes); err != nil {
		return result, fmt.Errorf("read exposure facts: %w", err)
	}
	if err := pool.QueryRow(ctx, `SELECT pg_table_size('exposure_facts'), pg_indexes_size('exposure_facts')`).
		Scan(&result.TableBytes, &result.IndexesBytes); err != nil {
		return result, fmt.Errorf("read exposure relation sizes: %w", err)
	}
	return result, nil
}

func sampleLocks(ctx context.Context, pool *pgxpool.Pool, interval time.Duration, done chan<- lockStats) {
	var result lockStats
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer func() { done <- result }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var waiting int64
			err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND pid<>pg_backend_pid()
  AND (wait_event_type='Lock' OR cardinality(pg_blocking_pids(pid))>0)`).Scan(&waiting)
			if err != nil {
				result.SamplerErrors++
				continue
			}
			result.Samples++
			if waiting > 0 {
				result.SamplesWithWaiters++
			}
			if waiting > result.MaxWaitingSessions {
				result.MaxWaitingSessions = waiting
			}
			result.WaitingSessionMillis += int64(float64(waiting) * float64(interval) / float64(time.Millisecond))
		}
	}
}

func sampleHeap(ctx context.Context, done chan<- uint64) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var peak uint64
	defer func() { done <- peak }()
	for {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		if stats.HeapAlloc > peak {
			peak = stats.HeapAlloc
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func validateTaskFamily(ctx context.Context, pool *pgxpool.Pool, tasks taskPool) error {
	seen := make(map[string]struct{}, len(tasks.WorkerTaskIDs))
	for _, taskID := range tasks.WorkerTaskIDs {
		if _, duplicate := seen[taskID]; duplicate {
			return errors.New("task pool contains a duplicate task")
		}
		seen[taskID] = struct{}{}
		var root, state string
		if err := pool.QueryRow(ctx, `SELECT root_task_id, state FROM tasks WHERE id=$1`, taskID).Scan(&root, &state); err != nil {
			return fmt.Errorf("read benchmark task: %w", err)
		}
		if root != tasks.RootTaskID || state != "ACTIVE" {
			return fmt.Errorf("task family mismatch or inactive task")
		}
	}
	return nil
}

type mcpClient struct {
	url   string
	token string
	http  *http.Client
	next  atomic.Int64
}

func (client *mcpClient) call(ctx context.Context, tool string, arguments any, output any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": client.next.Add(1), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var rpc rpcResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return err
	}
	if rpc.Error != nil {
		return fmt.Errorf("MCP RPC %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	var toolResult toolResult
	if err := json.Unmarshal(rpc.Result, &toolResult); err != nil {
		return err
	}
	if toolResult.IsError {
		message := "tool returned isError=true"
		if len(toolResult.Content) > 0 {
			message = toolResult.Content[0].Text
		}
		return errors.New(message)
	}
	if len(toolResult.StructuredContent) == 0 || string(toolResult.StructuredContent) == "null" {
		return errors.New("tool omitted structuredContent")
	}
	return json.Unmarshal(toolResult.StructuredContent, output)
}

func oaClient(baseURL, username, password string, timeout time.Duration) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: timeout}
	page, err := httpGet(context.Background(), client, strings.TrimRight(baseURL, "/")+"/login")
	if err != nil {
		return nil, err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return nil, err
	}
	values := url.Values{"csrf": {csrf}, "username": {username}, "password": {password}}
	if _, err := httpPostForm(context.Background(), client, strings.TrimRight(baseURL, "/")+"/login", values); err != nil {
		return nil, fmt.Errorf("OA login %s: %w", username, err)
	}
	return client, nil
}

func oaAction(ctx context.Context, client *http.Client, baseURL, draftID, action, decision string) error {
	taskURL := strings.TrimRight(baseURL, "/") + "/tasks/" + url.PathEscape(draftID)
	page, err := httpGet(ctx, client, taskURL)
	if err != nil {
		return err
	}
	csrf, err := csrfToken(page)
	if err != nil {
		return err
	}
	values := url.Values{"csrf": {csrf}}
	if decision != "" {
		values.Set("decision", decision)
	}
	_, err = httpPostForm(ctx, client, taskURL+"/"+action, values)
	return err
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfToken(page []byte) (string, error) {
	match := csrfPattern.FindSubmatch(page)
	if len(match) != 2 {
		return "", errors.New("OA page omitted CSRF token")
	}
	return string(match[1]), nil
}

func httpGet(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", target, response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 2<<20))
}

func httpPostForm(ctx context.Context, client *http.Client, target string, values url.Values) ([]byte, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s returned %d", target, response.StatusCode)
	}
	return body, nil
}

func waitTask(ctx context.Context, client *mcpClient, taskID, expected string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status struct {
			State string `json:"state"`
		}
		if err := client.call(ctx, "get_task_status", map[string]string{"task_id": taskID}, &status); err == nil && status.State == expected {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("task did not reach %s", expected)
}

func metricSemantics() map[string]string {
	return map[string]string{
		"latency_ms": "client wall-clock latency per completed operation",
		"throughput_qps": "completed measured operations divided by cell wall-clock seconds",
		"client_peak_heap_bytes": "maximum Go HeapAlloc sampled every 5 ms during the cell",
		"service_peak_memory_bytes": "peak container memory usage from Docker stats; injected by the Compose wrapper",
		"ledger_growth": "after-minus-before root-ledger counters, root fact payload, and global exposure_facts physical sizes",
		"lock_contention": "10 ms pg_stat_activity sampling; waiting_session_ms is a sampling approximation",
		"exposure_ledger_lock": "client-side duration of SELECT ... FOR UPDATE on the root ledger, including driver/server round-trip",
		"fact_history_hit_rate": "(actual release+influence facts - newly charged facts) / actual facts",
		"query_history_hit_rate": "fraction of exposure queries with nonzero actual facts and zero newly charged facts",
		"percentiles": "Hyndman-Fan type 7 over per-operation samples",
	}
}

func writeSamples(path string, samples []sample) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	for _, value := range samples {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func writeJSON(path string, value any) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func readJSON(path string, output any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	return decoder.Decode(output)
}

func durationMS(value time.Duration) float64 { return float64(value) / float64(time.Millisecond) }

func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func pathTail(value string) string {
	parsed, err := url.Parse(value)
	if err == nil {
		value = parsed.Path
	}
	return strings.TrimRight(value, "/")[strings.LastIndex(strings.TrimRight(value, "/"), "/")+1:]
}

func maxInt(values ...int) int {
	result := 0
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err == nil && value > 0 {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(os.Getenv(name))
	if err == nil && value > 0 {
		return value
	}
	return fallback
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "exposure benchmark failed:", err)
	os.Exit(1)
}
