package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	reportSchema = 1
	profile      = exposure.ProfileV2
	digestValue  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type options struct {
	dsn    string
	output string
	trials int
	sizes  []int
}

type storageSnapshot struct {
	FactRows       int64 `json:"fact_rows"`
	PayloadBytes   int64 `json:"canonical_payload_bytes"`
	TableBytes     int64 `json:"table_bytes"`
	IndexBytes     int64 `json:"index_bytes"`
	AllocatedBytes int64 `json:"allocated_bytes"`
}

type point struct {
	Trial                  int             `json:"trial"`
	FactsPerLedger         int             `json:"facts_per_ledger"`
	Operation              string          `json:"operation"`
	ActualReleaseFacts     int64           `json:"actual_release_facts"`
	ActualDependencyFacts  int64           `json:"actual_dependency_facts"`
	ChargedReleaseFacts    int64           `json:"charged_release_facts"`
	ChargedDependencyFacts int64           `json:"charged_dependency_facts"`
	SettlementMS           float64         `json:"settlement_ms"`
	FactStoreMS            float64         `json:"fact_store_ms"`
	Storage                storageSnapshot `json:"storage"`
}

type aggregatePoint struct {
	FactsPerLedger int            `json:"facts_per_ledger"`
	Operation      string         `json:"operation"`
	Trials         int            `json:"trials"`
	SettlementMS   summary        `json:"settlement_ms"`
	FactStoreMS    summary        `json:"fact_store_ms"`
	Storage        storageSummary `json:"storage"`
}

type summary struct {
	Median     float64   `json:"median"`
	TrialRange []float64 `json:"trial_range"`
}

type storageSummary struct {
	FactRows       int64Summary `json:"fact_rows"`
	PayloadBytes   int64Summary `json:"canonical_payload_bytes"`
	TableBytes     int64Summary `json:"table_bytes"`
	IndexBytes     int64Summary `json:"index_bytes"`
	AllocatedBytes int64Summary `json:"allocated_bytes"`
}

type int64Summary struct {
	Median     int64   `json:"median"`
	TrialRange []int64 `json:"trial_range"`
}

type boundaryResult struct {
	Trial                   int   `json:"trial"`
	BudgetFactsPerLedger    int   `json:"budget_facts_per_ledger"`
	AttemptedFactsPerLedger int   `json:"attempted_facts_per_ledger"`
	Rejected                bool  `json:"rejected"`
	FactRowsBefore          int64 `json:"fact_rows_before"`
	FactRowsAfter           int64 `json:"fact_rows_after"`
}

type report struct {
	SchemaVersion    int               `json:"schema_version"`
	Status           string            `json:"status"`
	GeneratedAt      time.Time         `json:"generated_at"`
	PostgreSQL       string            `json:"postgres_version"`
	Trials           int               `json:"trials"`
	Sizes            []int             `json:"facts_per_ledger_sizes"`
	Semantics        map[string]string `json:"semantics"`
	SourceSHA256     string            `json:"source_sha256"`
	Raw              []point           `json:"raw_points"`
	Aggregates       []aggregatePoint  `json:"aggregates"`
	BudgetBoundaries []boundaryResult  `json:"budget_boundaries"`
}

func main() {
	opts, err := parseOptions()
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	result, err := run(ctx, opts)
	if err != nil {
		fatal(err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(opts.output, encoded, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s: %d trials, %d measured points\n", opts.output, opts.trials, len(result.Raw))
}

func parseOptions() (options, error) {
	var opts options
	var rawSizes string
	flag.StringVar(&opts.dsn, "dsn", strings.TrimSpace(os.Getenv("EXPOSURE_STORAGE_POSTGRES_DSN")), "administrative PostgreSQL DSN")
	flag.StringVar(&opts.output, "output", "evaluation/exposure-storage/results.json", "output JSON")
	flag.IntVar(&opts.trials, "trials", 3, "fresh-schema trials")
	flag.StringVar(&rawSizes, "sizes", "10,100,1000,10000", "cumulative facts per ledger")
	flag.Parse()
	if opts.dsn == "" || opts.trials < 2 {
		return opts, errors.New("a PostgreSQL DSN and at least two trials are required")
	}
	previous := 0
	for _, raw := range strings.Split(rawSizes, ",") {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value <= previous {
			return opts, fmt.Errorf("sizes must be strictly increasing positive integers: %q", rawSizes)
		}
		opts.sizes = append(opts.sizes, value)
		previous = value
	}
	return opts, nil
}

func run(ctx context.Context, opts options) (report, error) {
	admin, err := sql.Open("pgx", opts.dsn)
	if err != nil {
		return report{}, err
	}
	defer admin.Close()
	var postgresVersion string
	if err := admin.QueryRowContext(ctx, `SELECT version()`).Scan(&postgresVersion); err != nil {
		return report{}, err
	}
	sourceHash, err := sourceDigest(".")
	if err != nil {
		return report{}, err
	}
	result := report{SchemaVersion: reportSchema, Status: "complete_control_postgresql_storage_scaling",
		GeneratedAt: time.Now().UTC(), PostgreSQL: postgresVersion, Trials: opts.trials,
		Sizes: append([]int(nil), opts.sizes...), SourceSHA256: sourceHash,
		Semantics: map[string]string{
			"novel":       "cumulative observation; charged facts are the suffix absent from the root ledger",
			"replay":      "new query and request identifiers with the identical cumulative observation; expected zero charge",
			"storage":     "pg_table_size and pg_indexes_size for the production exposure_facts relation in an isolated migrated schema",
			"uncertainty": "median and min-max range across fresh-schema trials",
		}}
	for trial := 1; trial <= opts.trials; trial++ {
		schema := fmt.Sprintf("exposure_storage_%d_%d", time.Now().UnixNano(), trial)
		if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
			return report{}, err
		}
		trialPoints, boundary, trialErr := runTrial(ctx, schemaDSN(opts.dsn, schema), trial, opts.sizes)
		_, dropErr := admin.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		if trialErr != nil {
			return report{}, trialErr
		}
		if dropErr != nil {
			return report{}, dropErr
		}
		result.Raw = append(result.Raw, trialPoints...)
		result.BudgetBoundaries = append(result.BudgetBoundaries, boundary)
		fmt.Fprintf(os.Stderr, "completed storage trial %d/%d\n", trial, opts.trials)
	}
	result.Aggregates = aggregate(result.Raw, opts.sizes, opts.trials)
	for _, boundary := range result.BudgetBoundaries {
		if !boundary.Rejected || boundary.FactRowsBefore != boundary.FactRowsAfter {
			return report{}, fmt.Errorf("budget boundary trial %d did not reject atomically", boundary.Trial)
		}
	}
	return result, nil
}

func runTrial(ctx context.Context, dsn string, trial int, sizes []int) ([]point, boundaryResult, error) {
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{byte(40 + trial)}, 32))
	if err != nil {
		return nil, boundaryResult{}, err
	}
	store, err := control.Open(ctx, dsn, cipher)
	if err != nil {
		return nil, boundaryResult{}, err
	}
	defer store.Close()
	maxFacts := sizes[len(sizes)-1]
	taskID := fmt.Sprintf("storage_task_%d", trial)
	if err := activateTask(ctx, store, taskID, maxFacts); err != nil {
		return nil, boundaryResult{}, err
	}
	release, dependency, err := makeFacts(maxFacts + 1)
	if err != nil {
		return nil, boundaryResult{}, err
	}
	points := make([]point, 0, len(sizes)*2)
	for _, size := range sizes {
		observation := exposure.Observation{ProfileVersion: profile, Release: release[:size], Influence: dependency[:size]}
		novel, err := settle(ctx, store, taskID, trial, size, "novel", observation)
		if err != nil {
			return nil, boundaryResult{}, err
		}
		points = append(points, novel)
		replay, err := settle(ctx, store, taskID, trial, size, "replay", observation)
		if err != nil {
			return nil, boundaryResult{}, err
		}
		if replay.ChargedReleaseFacts != 0 || replay.ChargedDependencyFacts != 0 || replay.Storage != novel.Storage {
			return nil, boundaryResult{}, fmt.Errorf("size %d replay was not a zero-growth hit", size)
		}
		points = append(points, replay)
	}
	before, err := readStorage(ctx, store.DB())
	if err != nil {
		return nil, boundaryResult{}, err
	}
	overflow := exposure.Observation{ProfileVersion: profile, Release: release, Influence: dependency}
	queryID := fmt.Sprintf("storage_q_%d_boundary", trial)
	if _, err := reserve(ctx, store, taskID, queryID, queryID, int64(len(release))); err != nil {
		return nil, boundaryResult{}, err
	}
	_, _, settleErr := store.FinalizeQueryMeasured(ctx, control.BudgetSettlement{
		QueryID: queryID, Rows: 1, DBMS: 1, ObservedDBMS: 1, Exposure: &overflow,
	}, []byte(`{"rows":[[1]]}`))
	rejected := errors.Is(settleErr, control.ErrExposureBudgetExhausted)
	if settleErr == nil || !rejected {
		return nil, boundaryResult{}, fmt.Errorf("overflow settlement error = %v", settleErr)
	}
	if _, err := store.FailBudget(ctx, control.BudgetSettlement{QueryID: queryID, Rows: 1, DBMS: 1, ObservedDBMS: 1, ErrorCode: "EXPOSURE_BUDGET_EXHAUSTED"}); err != nil {
		return nil, boundaryResult{}, err
	}
	after, err := readStorage(ctx, store.DB())
	if err != nil {
		return nil, boundaryResult{}, err
	}
	return points, boundaryResult{Trial: trial, BudgetFactsPerLedger: maxFacts,
		AttemptedFactsPerLedger: maxFacts + 1, Rejected: rejected,
		FactRowsBefore: before.FactRows, FactRowsAfter: after.FactRows}, nil
}

func activateTask(ctx context.Context, store *control.Store, taskID string, maxFacts int) error {
	principalID := "principal_" + taskID
	if err := store.CreatePrincipal(ctx, control.Principal{ID: principalID, Subject: "subject_" + taskID, Role: "requester"}); err != nil {
		return err
	}
	expires := time.Now().UTC().Add(time.Hour)
	if err := store.CreateTask(ctx, control.Task{ID: taskID, PrincipalID: principalID,
		Objective: "measure immutable exposure storage", State: control.TaskAwaitingApproval,
		CatalogVersion: "storage-eval-v1", RequestedBudget: json.RawMessage(`{"queries":20}`),
		RequestContext: json.RawMessage(`{"evaluation":"exposure-storage"}`), ExpiresAt: &expires}); err != nil {
		return err
	}
	callback := control.ApprovalCallback{EventID: "approval_" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		ExpectedState: control.TaskAwaitingApproval, NewState: control.TaskActive, Response: []byte(`{"ok":true}`),
		Event: control.ApprovalEvent{TaskID: taskID, Actor: "storage-eval-approver", Decision: "approved", Payload: json.RawMessage(`{"route":"evaluation"}`)},
		Grant: &control.TaskGrant{TaskID: taskID, Subject: "subject_" + taskID, Purpose: "storage evaluation",
			ApprovedProducts: []string{"synthetic_fact_stream"}, ApprovedColumns: map[string][]string{"synthetic_fact_stream": {"amount", "department"}},
			MandatoryScope: json.RawMessage(`{"evaluation":true}`), SensitivityCeiling: "internal",
			Budget:    control.BudgetLimits{Queries: 20, Rows: 20, DBMS: 100000},
			Exposure:  control.ExposureGrant{Limits: control.ExposureLimits{ReleaseFacts: int64(maxFacts), InfluenceFacts: int64(maxFacts)}, ProfileVersion: profile},
			ExpiresAt: expires, CatalogVersion: "storage-eval-v1", CatalogDigest: digestValue,
			DatasourceID: "synthetic-storage-eval", SchemaDigest: digestValue, ApprovalReceipt: "receipt_" + taskID}}
	_, err := store.ApplyApprovalCallback(ctx, callback)
	return err
}

func makeFacts(count int) ([]exposure.FactID, []exposure.FactID, error) {
	release := make([]exposure.FactID, 0, count)
	dependency := make([]exposure.FactID, 0, count)
	for index := 0; index < count; index++ {
		entity := fmt.Sprintf("entity-%06d", index)
		oneRelease, err := exposure.NewBaseCellFactV2("storage.synthetic", "storage-snapshot-v1", entity, "amount", "numeric", strconv.Itoa(index))
		if err != nil {
			return nil, nil, err
		}
		oneDependency, err := exposure.NewBaseCellFactV2("storage.synthetic", "storage-snapshot-v1", entity, "department", "text", "sales")
		if err != nil {
			return nil, nil, err
		}
		release = append(release, oneRelease)
		dependency = append(dependency, oneDependency)
	}
	return release, dependency, nil
}

func settle(ctx context.Context, store *control.Store, taskID string, trial, size int, operation string, observation exposure.Observation) (point, error) {
	queryID := fmt.Sprintf("storage_q_%d_%d_%s", trial, size, operation)
	reservation, err := reserve(ctx, store, taskID, queryID, queryID, int64(size))
	if err != nil {
		return point{}, err
	}
	_, metrics, err := store.FinalizeQueryMeasured(ctx, control.BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1, ObservedDBMS: 1, Exposure: &observation,
	}, []byte(`{"rows":[[1]]}`))
	if err != nil {
		return point{}, err
	}
	charge, err := store.GetExposureCharge(ctx, queryID)
	if err != nil {
		return point{}, err
	}
	storage, err := readStorage(ctx, store.DB())
	if err != nil {
		return point{}, err
	}
	return point{Trial: trial, FactsPerLedger: size, Operation: operation,
		ActualReleaseFacts: charge.ActualReleaseFacts, ActualDependencyFacts: charge.ActualInfluenceFacts,
		ChargedReleaseFacts: charge.ChargedReleaseFacts, ChargedDependencyFacts: charge.ChargedInfluenceFacts,
		SettlementMS: durationMS(metrics.SettlementStore), FactStoreMS: durationMS(metrics.ExposureFactStore), Storage: storage}, nil
}

func reserve(ctx context.Context, store *control.Store, taskID, queryID, requestID string, estimate int64) (control.BudgetReservation, error) {
	return store.ReserveBudget(ctx, control.ReserveRequest{QueryID: queryID, TaskID: taskID, RequestID: requestID,
		Actor: "storage-eval", RequestDigest: "request-" + requestID, SQLFingerprint: "storage-eval",
		CatalogVersion: "storage-eval-v1", CatalogDigest: digestValue, DatasourceID: "synthetic-storage-eval",
		SchemaDigest: digestValue, ManifestDigest: digestValue, GrantDigest: digestValue, PolicyDecision: "ALLOW",
		RequestedRows: 1, RequestedDBMS: 1000,
		Exposure: &control.ExposureReservationRequest{ProfileVersion: profile, EstimatedReleaseFacts: estimate, EstimatedInfluenceFacts: estimate}})
}

func readStorage(ctx context.Context, db *sql.DB) (storageSnapshot, error) {
	var result storageSnapshot
	err := db.QueryRowContext(ctx, `SELECT count(*),
COALESCE(sum(octet_length(identity_json::text) + octet_length(canonical_payload)), 0),
pg_table_size('exposure_facts'), pg_indexes_size('exposure_facts') FROM exposure_facts`).
		Scan(&result.FactRows, &result.PayloadBytes, &result.TableBytes, &result.IndexBytes)
	result.AllocatedBytes = result.TableBytes + result.IndexBytes
	return result, err
}

func aggregate(points []point, sizes []int, trials int) []aggregatePoint {
	result := make([]aggregatePoint, 0, len(sizes)*2)
	for _, size := range sizes {
		for _, operation := range []string{"novel", "replay"} {
			var rows []point
			for _, one := range points {
				if one.FactsPerLedger == size && one.Operation == operation {
					rows = append(rows, one)
				}
			}
			if len(rows) != trials {
				panic("incomplete storage aggregation")
			}
			result = append(result, aggregatePoint{FactsPerLedger: size, Operation: operation, Trials: trials,
				SettlementMS: summarizeFloat(rows, func(row point) float64 { return row.SettlementMS }),
				FactStoreMS:  summarizeFloat(rows, func(row point) float64 { return row.FactStoreMS }),
				Storage: storageSummary{
					FactRows:       summarizeInt(rows, func(row point) int64 { return row.Storage.FactRows }),
					PayloadBytes:   summarizeInt(rows, func(row point) int64 { return row.Storage.PayloadBytes }),
					TableBytes:     summarizeInt(rows, func(row point) int64 { return row.Storage.TableBytes }),
					IndexBytes:     summarizeInt(rows, func(row point) int64 { return row.Storage.IndexBytes }),
					AllocatedBytes: summarizeInt(rows, func(row point) int64 { return row.Storage.AllocatedBytes }),
				}})
		}
	}
	return result
}

func summarizeFloat(rows []point, value func(point) float64) summary {
	values := make([]float64, len(rows))
	for index, row := range rows {
		values[index] = value(row)
	}
	sort.Float64s(values)
	return summary{Median: medianFloat(values), TrialRange: []float64{values[0], values[len(values)-1]}}
}

func summarizeInt(rows []point, value func(point) int64) int64Summary {
	values := make([]int64, len(rows))
	for index, row := range rows {
		values[index] = value(row)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return int64Summary{Median: values[len(values)/2], TrialRange: []int64{values[0], values[len(values)-1]}}
}

func medianFloat(values []float64) float64 {
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func schemaDSN(raw, schema string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func sourceDigest(root string) (string, error) {
	var paths []string
	for _, target := range []string{"go.mod", "go.sum", "evaluation/cmd/exposure-storage", "internal/control", "internal/exposure"} {
		err := filepath.WalkDir(filepath.Join(root, target), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".sql") || path == "go.mod" || path == "go.sum" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		relative, _ := filepath.Rel(root, path)
		hash.Write([]byte(filepath.ToSlash(relative)))
		hash.Write([]byte{0})
		hash.Write(content)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func durationMS(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "exposure storage evaluation failed:", err)
	os.Exit(1)
}
