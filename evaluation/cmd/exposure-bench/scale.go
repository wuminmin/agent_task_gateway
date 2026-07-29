package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const scalePoolSchema = 1

type scaleTask struct {
	TaskID string `json:"task_id"`
	Trial  int    `json:"trial"`
	Orders int    `json:"orders"`
}

type scaleTaskPool struct {
	SchemaVersion int         `json:"schema_version"`
	Dataset       string      `json:"dataset"`
	Tasks         []scaleTask `json:"tasks"`
}

type scalePoint struct {
	Trial                  int                `json:"trial"`
	Orders                 int                `json:"orders"`
	JoinedRows             int                `json:"joined_rows"`
	ExpectedInfluenceFacts int64              `json:"expected_influence_facts"`
	Operation              string             `json:"operation"`
	LatencyMS              float64            `json:"latency_ms"`
	DatabaseMS             float64            `json:"database_ms,omitempty"`
	ComponentMS            map[string]float64 `json:"component_ms,omitempty"`
	Rows                   int64              `json:"rows"`
	ActualReleaseFacts     int64              `json:"actual_release_facts"`
	ActualInfluenceFacts   int64              `json:"actual_influence_facts"`
	ChargedReleaseFacts    int64              `json:"charged_release_facts"`
	ChargedInfluenceFacts  int64              `json:"charged_influence_facts"`
	ObservationSHA256      string             `json:"observation_sha256,omitempty"`
	LedgerBefore           *ledgerSnapshot    `json:"ledger_before,omitempty"`
	LedgerAfter            *ledgerSnapshot    `json:"ledger_after,omitempty"`
}

type scaleAggregate struct {
	Orders                 int                     `json:"orders"`
	JoinedRows             int                     `json:"joined_rows"`
	ExpectedInfluenceFacts int64                   `json:"expected_influence_facts"`
	Operation              string                  `json:"operation"`
	Trials                 int                     `json:"trials"`
	LatencyMS              distribution            `json:"latency_ms"`
	DatabaseMS             *distribution           `json:"database_ms,omitempty"`
	ComponentMS            map[string]distribution `json:"component_ms,omitempty"`
}

type scaleReport struct {
	SchemaVersion          int               `json:"schema_version"`
	Status                 string            `json:"status"`
	StartedAt              time.Time         `json:"started_at"`
	FinishedAt             time.Time         `json:"finished_at"`
	PostgreSQLVersion      string            `json:"postgres_version"`
	Configuration          map[string]any    `json:"configuration"`
	MetricSemantics        map[string]string `json:"metric_semantics"`
	RawPoints              []scalePoint      `json:"raw_points"`
	Aggregates             []scaleAggregate  `json:"aggregates"`
	ServicePeakMemoryBytes map[string]int64  `json:"service_peak_memory_bytes,omitempty"`
}

func bootstrapScale(ctx context.Context, opts options) error {
	if len(opts.scaleSizes) < 2 || opts.scaleTrials < 1 {
		return errors.New("scale campaign requires at least two sizes and one trial")
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
	pool := scaleTaskPool{SchemaVersion: scalePoolSchema, Dataset: "deterministic-tpch-derived-orders-lineitem-v1"}
	for _, orders := range opts.scaleSizes {
		for trial := 1; trial <= opts.scaleTrials; trial++ {
			arguments := map[string]any{
				"objective":     fmt.Sprintf("PostgreSQL 16 Join-Group scale evaluation: %d orders, trial %d", orders, trial),
				"data_products": []string{"scale_orders", "scale_lineitem"},
				"columns": map[string][]string{
					"scale_orders":   {"o_orderkey", "o_orderstatus"},
					"scale_lineitem": {"l_orderkey", "l_linenumber", "l_extendedprice"},
				},
				"scopes": map[string]any{"dataset_partition": []string{"1"}},
			}
			var created struct {
				TaskID string `json:"task_id"`
				OAURL  string `json:"oa_url"`
			}
			if err := mcp.call(ctx, "request_data_task", arguments, &created); err != nil {
				return fmt.Errorf("request scale=%d trial=%d: %w", orders, trial, err)
			}
			if created.TaskID == "" || created.OAURL == "" {
				return errors.New("scale task response omitted task_id or oa_url")
			}
			draftID := pathTail(created.OAURL)
			if err := oaAction(ctx, alice, opts.oaURL, draftID, "submit", ""); err != nil {
				return fmt.Errorf("submit scale=%d trial=%d: %w", orders, trial, err)
			}
			if err := waitTask(ctx, mcp, created.TaskID, "AWAITING_APPROVAL", opts.requestTimeout); err != nil {
				return err
			}
			if err := oaAction(ctx, bob, opts.oaURL, draftID, "decision", "approved"); err != nil {
				return fmt.Errorf("approve scale=%d trial=%d: %w", orders, trial, err)
			}
			if err := waitTask(ctx, mcp, created.TaskID, "ACTIVE", opts.requestTimeout); err != nil {
				return err
			}
			pool.Tasks = append(pool.Tasks, scaleTask{TaskID: created.TaskID, Trial: trial, Orders: orders})
			fmt.Fprintf(os.Stderr, "bootstrapped scale=%d trial=%d\n", orders, trial)
		}
	}
	if err := os.MkdirAll(filepath.Dir(opts.tasksPath), 0o755); err != nil {
		return err
	}
	return writeJSON(opts.tasksPath, pool)
}

func runScale(ctx context.Context, opts options) error {
	started := time.Now().UTC()
	var tasks scaleTaskPool
	if err := readJSON(opts.tasksPath, &tasks); err != nil {
		return err
	}
	if err := validateScaleTaskPool(tasks, opts); err != nil {
		return err
	}
	control, err := pgxpool.New(ctx, opts.controlDSN)
	if err != nil {
		return fmt.Errorf("open control PostgreSQL: %w", err)
	}
	defer control.Close()
	business, err := pgxpool.New(ctx, opts.businessDSN)
	if err != nil {
		return fmt.Errorf("open business PostgreSQL: %w", err)
	}
	defer business.Close()
	var postgresVersion string
	if err := business.QueryRow(ctx, "SHOW server_version").Scan(&postgresVersion); err != nil {
		return err
	}
	if !strings.HasPrefix(postgresVersion, "16.") {
		return fmt.Errorf("scale campaign requires PostgreSQL 16, got %s", postgresVersion)
	}
	if err := validateScaleDataset(ctx, business, opts.scaleSizes[len(opts.scaleSizes)-1]); err != nil {
		return err
	}
	mcp := &mcpClient{url: strings.TrimRight(opts.gatewayURL, "/") + "/mcp", token: opts.gatewayToken,
		http: &http.Client{Timeout: opts.requestTimeout}}
	report := scaleReport{
		SchemaVersion:     1,
		Status:            "running",
		StartedAt:         started,
		PostgreSQLVersion: postgresVersion,
		Configuration: map[string]any{
			"orders_per_scale": opts.scaleSizes, "lineitems_per_order": 5, "trials": opts.scaleTrials,
			"workload":       "TPC-H-derived (not official TPC-H) Orders-Lineitem equijoin; group by 3-valued status; sum price, sum line number, and count",
			"data_generator": "PostgreSQL generate_series with fixed integer formulas",
			"cache_strategy": "warm services; direct baseline immediately precedes novel and replay calls",
			"task_isolation": "fresh independent root task per scale and trial",
		},
		MetricSemantics: map[string]string{
			"latency_ms":               "client wall-clock time for one completed operation",
			"database_ms":              "Gateway-reported sum of visible and provenance PostgreSQL time; direct_sql uses client wall time",
			"novel":                    "first semantically identical observation within its fresh root ledger",
			"replay":                   "same plan and root ledger under a distinct request_id after the novel operation",
			"expected_influence_facts": "3 facts per order plus 4 facts per each of five lineitems = 23 facts/order",
		},
	}
	for _, task := range tasks.Tasks {
		points, err := runScaleTask(ctx, opts, business, control, mcp, task)
		if err != nil {
			return fmt.Errorf("scale=%d trial=%d: %w", task.Orders, task.Trial, err)
		}
		report.RawPoints = append(report.RawPoints, points...)
		fmt.Fprintf(os.Stderr, "measured scale=%d trial=%d influence=%d\n", task.Orders, task.Trial, int64(task.Orders)*23)
	}
	report.Aggregates = summarizeScale(report.RawPoints)
	report.Status = "complete_postgresql16_multiscale_join_group_campaign"
	report.FinishedAt = time.Now().UTC()
	if err := os.MkdirAll(opts.outputDir, 0o755); err != nil {
		return err
	}
	return writeJSON(filepath.Join(opts.outputDir, "report.json"), report)
}

func validateScaleTaskPool(tasks scaleTaskPool, opts options) error {
	if tasks.SchemaVersion != scalePoolSchema || tasks.Dataset != "deterministic-tpch-derived-orders-lineitem-v1" {
		return errors.New("invalid scale task pool")
	}
	expected := len(opts.scaleSizes) * opts.scaleTrials
	if len(tasks.Tasks) != expected {
		return fmt.Errorf("scale task pool has %d tasks, want %d", len(tasks.Tasks), expected)
	}
	seen := make(map[string]struct{}, len(tasks.Tasks))
	counts := make(map[int]int)
	for _, task := range tasks.Tasks {
		if task.TaskID == "" || task.Trial < 1 || task.Trial > opts.scaleTrials {
			return errors.New("scale task pool contains an invalid task")
		}
		if _, duplicate := seen[task.TaskID]; duplicate {
			return errors.New("scale task pool contains a duplicate task_id")
		}
		seen[task.TaskID] = struct{}{}
		counts[task.Orders]++
	}
	for _, size := range opts.scaleSizes {
		if counts[size] != opts.scaleTrials {
			return fmt.Errorf("scale task pool has %d trials for %d orders", counts[size], size)
		}
	}
	return nil
}

func validateScaleDataset(ctx context.Context, pool *pgxpool.Pool, largest int) error {
	var orders, lines int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reporting.scale_orders WHERE o_orderkey <= $1`, largest).Scan(&orders); err != nil {
		return err
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM reporting.scale_lineitem WHERE l_orderkey <= $1`, largest).Scan(&lines); err != nil {
		return err
	}
	if orders != int64(largest) || lines != int64(largest*5) {
		return fmt.Errorf("scale fixture cardinality is orders=%d lines=%d", orders, lines)
	}
	return nil
}

func runScaleTask(ctx context.Context, opts options, business, control *pgxpool.Pool, mcp *mcpClient, task scaleTask) ([]scalePoint, error) {
	expectedInfluence := int64(task.Orders) * 23
	joinedRows := task.Orders * 5
	direct := scalePoint{Trial: task.Trial, Orders: task.Orders, JoinedRows: joinedRows,
		ExpectedInfluenceFacts: expectedInfluence, Operation: "direct_sql"}
	directStarted := time.Now()
	rows, err := business.Query(ctx, `SELECT o.o_orderstatus, sum(l.l_extendedprice), sum(l.l_linenumber), count(*)
FROM reporting.scale_orders AS o
JOIN reporting.scale_lineitem AS l ON l.l_orderkey = o.o_orderkey
WHERE o.dataset_partition = 1 AND l.dataset_partition = 1 AND l.l_orderkey <= $1
GROUP BY o.o_orderstatus
ORDER BY o.o_orderstatus`, task.Orders)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		direct.Rows++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	direct.LatencyMS = durationMS(time.Since(directStarted))
	direct.DatabaseMS = direct.LatencyMS
	if direct.Rows != 3 {
		return nil, fmt.Errorf("direct query returned %d groups, want 3", direct.Rows)
	}
	before, err := readLedger(ctx, control, task.TaskID)
	if err != nil {
		return nil, err
	}
	if before.FactRows != 0 || before.ReleaseUsed != 0 || before.InfluenceUsed != 0 {
		return nil, errors.New("scale task ledger is not fresh")
	}
	plan := scalePlan(task.Orders)
	novel, err := executeScaleMCP(ctx, mcp, task, "novel", plan)
	if err != nil {
		return nil, err
	}
	afterNovel, err := readLedger(ctx, control, task.TaskID)
	if err != nil {
		return nil, err
	}
	novel.LedgerBefore, novel.LedgerAfter = &before, &afterNovel
	if novel.Rows != 3 || novel.ActualInfluenceFacts != expectedInfluence || novel.ChargedInfluenceFacts != expectedInfluence ||
		novel.ActualReleaseFacts == 0 || novel.ChargedReleaseFacts != novel.ActualReleaseFacts {
		return nil, fmt.Errorf("novel accounting mismatch: %+v", novel)
	}
	if afterNovel.FactRows != novel.ActualReleaseFacts+novel.ActualInfluenceFacts ||
		afterNovel.ReleaseUsed != novel.ActualReleaseFacts || afterNovel.InfluenceUsed != novel.ActualInfluenceFacts {
		return nil, fmt.Errorf("novel ledger mismatch: %+v", afterNovel)
	}
	replay, err := executeScaleMCP(ctx, mcp, task, "replay", plan)
	if err != nil {
		return nil, err
	}
	afterReplay, err := readLedger(ctx, control, task.TaskID)
	if err != nil {
		return nil, err
	}
	replay.LedgerBefore, replay.LedgerAfter = &afterNovel, &afterReplay
	if replay.Rows != novel.Rows || replay.ActualReleaseFacts != novel.ActualReleaseFacts ||
		replay.ActualInfluenceFacts != novel.ActualInfluenceFacts || replay.ChargedReleaseFacts != 0 ||
		replay.ChargedInfluenceFacts != 0 || replay.ObservationSHA256 != novel.ObservationSHA256 {
		return nil, fmt.Errorf("replay accounting mismatch: %+v", replay)
	}
	if afterReplay.FactRows != afterNovel.FactRows || afterReplay.ReleaseUsed != afterNovel.ReleaseUsed ||
		afterReplay.InfluenceUsed != afterNovel.InfluenceUsed {
		return nil, errors.New("replay changed root-ledger state")
	}
	return []scalePoint{direct, novel, replay}, nil
}

func scalePlan(orders int) map[string]any {
	return map[string]any{
		"from": map[string]any{"join": map[string]any{
			"left":  map[string]any{"product": "scale_orders", "role": "scale_orders"},
			"right": map[string]any{"product": "scale_lineitem", "role": "scale_lineitem"},
			"on": []map[string]any{{
				"left": "scale_orders.o_orderkey", "right": "scale_lineitem.l_orderkey",
			}},
		}},
		"columns": []string{"scale_orders.o_orderstatus"},
		"aggregates": []map[string]any{
			{"function": "sum", "column": "scale_lineitem.l_extendedprice", "alias": "revenue"},
			{"function": "sum", "column": "scale_lineitem.l_linenumber", "alias": "line_positions"},
			{"function": "count", "column": "*", "alias": "items"},
		},
		"filters":  []map[string]any{{"column": "scale_lineitem.l_orderkey", "op": "<=", "value": orders}},
		"group_by": []string{"scale_orders.o_orderstatus"},
	}
}

func executeScaleMCP(ctx context.Context, mcp *mcpClient, task scaleTask, operation string, plan map[string]any) (scalePoint, error) {
	point := scalePoint{Trial: task.Trial, Orders: task.Orders, JoinedRows: task.Orders * 5,
		ExpectedInfluenceFacts: int64(task.Orders) * 23, Operation: operation}
	requestID := fmt.Sprintf("scale-%d-trial-%d-%s", task.Orders, task.Trial, operation)
	started := time.Now()
	var response executeResponse
	if err := mcp.call(ctx, "execute_plan", map[string]any{
		"task_id": task.TaskID, "request_id": requestID, "plan": plan,
	}, &response); err != nil {
		return point, err
	}
	point.LatencyMS = durationMS(time.Since(started))
	point.DatabaseMS = response.DatabaseMS
	point.ComponentMS = response.ComponentMS
	point.Rows = response.RowCount
	point.ActualReleaseFacts = response.Exposure.ActualReleaseFacts
	point.ActualInfluenceFacts = response.Exposure.ActualInfluenceFacts
	point.ChargedReleaseFacts = response.Exposure.ChargedReleaseFacts
	point.ChargedInfluenceFacts = response.Exposure.ChargedInfluenceFacts
	point.ObservationSHA256 = response.Exposure.ObservationSHA256
	return point, nil
}

func summarizeScale(points []scalePoint) []scaleAggregate {
	type key struct {
		orders    int
		operation string
	}
	grouped := make(map[key][]scalePoint)
	for _, point := range points {
		grouped[key{point.Orders, point.Operation}] = append(grouped[key{point.Orders, point.Operation}], point)
	}
	keys := make([]key, 0, len(grouped))
	for item := range grouped {
		keys = append(keys, item)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].orders != keys[j].orders {
			return keys[i].orders < keys[j].orders
		}
		return keys[i].operation < keys[j].operation
	})
	result := make([]scaleAggregate, 0, len(keys))
	for _, item := range keys {
		values := grouped[item]
		latencies := make([]float64, 0, len(values))
		database := make([]float64, 0, len(values))
		components := make(map[string][]float64)
		for _, point := range values {
			latencies = append(latencies, point.LatencyMS)
			if point.DatabaseMS > 0 {
				database = append(database, point.DatabaseMS)
			}
			for name, value := range point.ComponentMS {
				components[name] = append(components[name], value)
			}
		}
		aggregate := scaleAggregate{Orders: item.orders, JoinedRows: item.orders * 5,
			ExpectedInfluenceFacts: int64(item.orders) * 23, Operation: item.operation,
			Trials: len(values), LatencyMS: summarize(latencies)}
		if len(database) > 0 {
			value := summarize(database)
			aggregate.DatabaseMS = &value
		}
		if len(components) > 0 {
			aggregate.ComponentMS = make(map[string]distribution, len(components))
			for name, values := range components {
				aggregate.ComponentMS[name] = summarize(values)
			}
		}
		result = append(result, aggregate)
	}
	return result
}
