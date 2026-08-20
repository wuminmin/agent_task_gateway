package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const diagnosisMarker = "DIAGNOSIS-NOT-FOR-PUBLICATION"
const snapshotRecord = "taskgate-final-v5-cliff-observer-snapshot-v1"
const tableStatsSQL = `SELECT relname,
	COALESCE(n_live_tup, 0), COALESCE(seq_scan, 0), COALESCE(seq_tup_read, 0),
	COALESCE(idx_scan, 0), COALESCE(idx_tup_fetch, 0)
	FROM pg_stat_user_tables WHERE relname = ANY($1::text[]) ORDER BY relname`

type snapshot struct {
	SchemaVersion  int                `json:"schema_version"`
	Record         string             `json:"record"`
	Classification string             `json:"classification"`
	Sequence       int                `json:"sequence"`
	ObservedAt     string             `json:"observed_at"`
	Metrics        map[string]float64 `json:"metrics"`
}

type observer struct {
	control  *pgxpool.Pool
	business *pgxpool.Pool
	oaPID    int
}

func main() {
	flags := flag.NewFlagSet("final-v5-cliff-observer", flag.ContinueOnError)
	output := flags.String("output", "", "create-exclusive diagnostic JSONL output")
	interval := flags.Duration("interval", 30*time.Second, "read-only observation interval")
	oaContainer := flags.String("oa-container", "", "OA container whose host PID is observed read-only")
	if err := flags.Parse(os.Args[1:]); err != nil || *output == "" || *interval <= 0 || *oaContainer == "" {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *output, *oaContainer, *interval); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, output, oaContainer string, interval time.Duration) error {
	control, err := readOnlyPool(ctx, os.Getenv("TASKGATE_FINAL_V5_CONTROL_DSN"))
	if err != nil {
		return fmt.Errorf("control observer: %w", err)
	}
	defer control.Close()
	business, err := readOnlyPool(ctx, os.Getenv("TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN"))
	if err != nil {
		return fmt.Errorf("business observer: %w", err)
	}
	defer business.Close()
	oaPID, err := containerPID(ctx, oaContainer)
	if err != nil {
		return fmt.Errorf("OA observer: %w", err)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create observer output: %w", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	instance := observer{control: control, business: business, oaPID: oaPID}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for sequence := 1; ; sequence++ {
		record, err := instance.capture(ctx, sequence)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		if err := encoder.Encode(record); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func readOnlyPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("DSN is required")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func (o observer) capture(ctx context.Context, sequence int) (snapshot, error) {
	metrics := make(map[string]float64)
	if err := captureControl(ctx, o.control, metrics); err != nil {
		return snapshot{}, fmt.Errorf("capture Control: %w", err)
	}
	if err := captureBusiness(ctx, o.business, metrics); err != nil {
		return snapshot{}, fmt.Errorf("capture Business: %w", err)
	}
	if err := captureOAProcess(o.oaPID, metrics); err != nil {
		return snapshot{}, fmt.Errorf("capture OA: %w", err)
	}
	// OA creates its in-memory draft before Control persists the corresponding
	// task. There is no OA database or O(1) read-only count endpoint. Control's
	// persisted task count is therefore retained under an explicitly inferred
	// name, alongside direct OA process-memory observations. Calling OA's
	// /tasks page here would itself copy and sort the unbounded draft map and
	// contaminate the suspected lock path.
	metrics["oa.drafts_inferred_control_task_lower_bound"] = metrics["control.tasks"]
	return snapshot{SchemaVersion: 1, Record: snapshotRecord, Classification: diagnosisMarker,
		Sequence: sequence, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Metrics: metrics}, nil
}

func containerPID(ctx context.Context, container string) (int, error) {
	command := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Pid}}", container)
	payload, err := command.Output()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil || pid < 1 {
		return 0, errors.New("OA container has no live host PID")
	}
	return pid, nil
}

func captureOAProcess(pid int, metrics map[string]float64) error {
	payload, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return err
	}
	values, err := parseProcStatus(payload)
	if err != nil {
		return err
	}
	metrics["oa.process_rss_bytes"] = values["VmRSS"] * 1024
	metrics["oa.process_virtual_bytes"] = values["VmSize"] * 1024
	metrics["oa.process_threads"] = values["Threads"]
	return nil
}

func parseProcStatus(payload []byte) (map[string]float64, error) {
	result := make(map[string]float64)
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if name != "VmRSS" && name != "VmSize" && name != "Threads" {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return nil, err
		}
		result[name] = value
	}
	if _, ok := result["VmRSS"]; !ok {
		return nil, errors.New("OA process status omits VmRSS")
	}
	if _, ok := result["VmSize"]; !ok {
		return nil, errors.New("OA process status omits VmSize")
	}
	if _, ok := result["Threads"]; !ok {
		return nil, errors.New("OA process status omits Threads")
	}
	return result, nil
}

func captureControl(ctx context.Context, pool *pgxpool.Pool, metrics map[string]float64) error {
	const counts = `SELECT
 (SELECT count(*) FROM tasks),
 (SELECT count(*) FROM tasks WHERE state='AWAITING_SUBMISSION'),
 (SELECT count(*) FROM tasks WHERE state='AWAITING_APPROVAL'),
 (SELECT count(*) FROM tasks WHERE state='ACTIVE'),
 (SELECT count(*) FROM tasks WHERE state='ARCHIVED'),
 (SELECT count(*) FROM task_grants), (SELECT count(*) FROM approval_events),
 (SELECT count(*) FROM callback_idempotency), (SELECT count(*) FROM audit_events),
 (SELECT last_sequence FROM audit_chain_head WHERE singleton=TRUE),
 pg_total_relation_size('tasks'), pg_total_relation_size('task_grants'),
 pg_total_relation_size('approval_events'), pg_total_relation_size('callback_idempotency'),
 pg_total_relation_size('audit_events'),
 (SELECT count(*) FROM pg_locks WHERE NOT granted),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='active'),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'
    AND position('audit_chain_head' in lower(query)) > 0),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'
    AND position('tasks' in lower(query)) > 0),
 (SELECT xact_commit FROM pg_stat_database WHERE datname=current_database()),
 (SELECT xact_rollback FROM pg_stat_database WHERE datname=current_database()),
 (SELECT blks_read FROM pg_stat_database WHERE datname=current_database()),
 (SELECT blks_hit FROM pg_stat_database WHERE datname=current_database()),
 (SELECT temp_bytes FROM pg_stat_database WHERE datname=current_database()),
 (SELECT deadlocks FROM pg_stat_database WHERE datname=current_database())`
	names := []string{"control.tasks", "control.tasks_awaiting_submission", "control.tasks_awaiting_approval",
		"control.tasks_active", "control.tasks_archived", "control.task_grants", "control.approval_events",
		"control.callback_idempotency", "control.audit_events", "control.audit_head_sequence",
		"control.tasks_bytes", "control.task_grants_bytes", "control.approval_events_bytes",
		"control.callback_idempotency_bytes", "control.audit_events_bytes", "control.locks_waiting",
		"control.activity_active", "control.activity_lock_waiters", "control.activity_audit_lock_waiters",
		"control.activity_task_lock_waiters", "control.xact_commit", "control.xact_rollback",
		"control.blks_read", "control.blks_hit", "control.temp_bytes", "control.deadlocks"}
	values := make([]int64, len(names))
	destinations := make([]any, len(values))
	for index := range values {
		destinations[index] = &values[index]
	}
	if err := pool.QueryRow(ctx, counts).Scan(destinations...); err != nil {
		return err
	}
	for index, name := range names {
		metrics[name] = float64(values[index])
	}
	return captureTableStats(ctx, pool, "control", []string{"tasks", "task_grants", "approval_events", "callback_idempotency", "audit_events"}, metrics)
}

func captureBusiness(ctx context.Context, pool *pgxpool.Pool, metrics map[string]float64) error {
	const counts = `SELECT
 (SELECT count(*) FROM reporting.expense_detail),
 (SELECT count(*) FROM reporting.final_v5_concurrency_expense_detail),
 pg_total_relation_size('reporting.expense_detail'),
 pg_total_relation_size('reporting.final_v5_concurrency_expense_detail'),
 (SELECT xact_commit FROM pg_stat_database WHERE datname=current_database()),
 (SELECT blks_read FROM pg_stat_database WHERE datname=current_database()),
 (SELECT blks_hit FROM pg_stat_database WHERE datname=current_database()),
 (SELECT temp_bytes FROM pg_stat_database WHERE datname=current_database())`
	names := []string{"business.expense_detail_rows", "business.concurrency_detail_rows",
		"business.expense_detail_bytes", "business.concurrency_detail_bytes",
		"business.xact_commit", "business.blks_read", "business.blks_hit", "business.temp_bytes"}
	values := make([]int64, len(names))
	destinations := make([]any, len(values))
	for index := range values {
		destinations[index] = &values[index]
	}
	if err := pool.QueryRow(ctx, counts).Scan(destinations...); err != nil {
		return err
	}
	for index, name := range names {
		metrics[name] = float64(values[index])
	}
	return captureTableStats(ctx, pool, "business", []string{"expense_detail", "final_v5_concurrency_expense_detail"}, metrics)
}

func captureTableStats(ctx context.Context, pool *pgxpool.Pool, prefix string, relations []string,
	metrics map[string]float64) error {
	// PostgreSQL reports several pg_stat counters as NULL until the relation has
	// exercised the corresponding access path. Preserve the counter semantics by
	// normalizing that initial state to zero before scanning into int64.
	rows, err := pool.Query(ctx, tableStatsSQL, relations)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relation string
		var values [5]int64
		if err := rows.Scan(&relation, &values[0], &values[1], &values[2], &values[3], &values[4]); err != nil {
			return err
		}
		for index, suffix := range []string{"live_tuples", "seq_scans", "seq_tuples_read", "idx_scans", "idx_tuples_fetched"} {
			metrics[prefix+".table."+relation+"."+suffix] = float64(values[index])
		}
	}
	return rows.Err()
}
