package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	reportSchema = 1
	boundaryID   = "query-result-plus-provenance-representation-generation-v2"
)

var (
	hex40   = regexp.MustCompile(`^[0-9a-f]{40}$`)
	envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	uuid    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

type config struct {
	SchemaVersion          int        `json:"schema_version"`
	CampaignID             string     `json:"campaign_id"`
	DataCacheStrategy      string     `json:"data_cache_strategy"`
	CircuitStrategy        string     `json:"circuit_strategy"`
	Warmups                int        `json:"warmups"`
	Runs                   int        `json:"runs"`
	OrderSeed              int64      `json:"order_seed"`
	StatementTimeoutMS     int        `json:"statement_timeout_ms"`
	DirectDSNEnv           string     `json:"direct_dsn_env"`
	ProvSQLDSNEnv          string     `json:"provsql_dsn_env"`
	ExpectedProvSQLVersion string     `json:"expected_provsql_version"`
	ExpectedProvSQLCommit  string     `json:"expected_provsql_commit"`
	DatasetFingerprintSQL  string     `json:"dataset_fingerprint_sql"`
	Workloads              []workload `json:"workloads"`
}

type workload struct {
	ID                 string `json:"id"`
	Scale              int64  `json:"scale"`
	ExpectedRows       int    `json:"expected_rows"`
	ProvenanceCarriers int    `json:"provenance_carrier_columns"`
	NonceStart         int64  `json:"novelty_nonce_start"`
	CarrierGateType    string `json:"expected_carrier_gate_type"`
	RowGateType        string `json:"expected_row_gate_type"`
	SQL                string `json:"sql"`
}

type report struct {
	SchemaVersion int           `json:"schema_version"`
	Status        string        `json:"status"`
	Boundary      boundary      `json:"comparison_boundary"`
	Campaign      campaign      `json:"campaign"`
	Systems       systems       `json:"systems"`
	Dataset       dataset       `json:"dataset"`
	Samples       []sample      `json:"samples"`
	Summaries     []summary     `json:"summaries"`
	Gates         []gate        `json:"gates"`
	Errors        []string      `json:"errors,omitempty"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    time.Time     `json:"finished_at"`
	Provenance    reportBinding `json:"provenance"`
	Limitations   []string      `json:"limitations"`
}

type boundary struct {
	ID               string   `json:"id"`
	Included         []string `json:"included"`
	Excluded         []string `json:"excluded"`
	Comparability    string   `json:"comparability"`
	MissingSemantics string   `json:"provsql_taskgate_semantics"`
}

type campaign struct {
	ID                       string `json:"id"`
	DataCacheStrategy        string `json:"data_cache_strategy"`
	CircuitStrategy          string `json:"circuit_strategy"`
	PostgreSQLProtocol       string `json:"postgresql_protocol"`
	Warmups                  int    `json:"warmups_per_workload_and_system"`
	Runs                     int    `json:"measured_runs_per_workload_and_system"`
	OrderSeed                int64  `json:"order_seed"`
	WorkloadOrderingStrategy string `json:"workload_ordering_strategy"`
	BaselineOrderingStrategy string `json:"baseline_ordering_strategy"`
	StatementTimeoutMS       int    `json:"statement_timeout_ms"`
}

type systems struct {
	Direct  system `json:"direct_postgresql"`
	ProvSQL system `json:"provsql"`
}

type system struct {
	PostgreSQLVersion    string `json:"postgresql_version"`
	PostgreSQLVersionNum string `json:"postgresql_version_num"`
	StatementTimeoutMS   int64  `json:"statement_timeout_ms"`
	MaxParallelWorkers   int64  `json:"max_parallel_workers_per_gather"`
	ClientMinMessages    string `json:"client_min_messages"`
	LogMinMessages       string `json:"log_min_messages"`
	ExtensionVersion     string `json:"extension_version,omitempty"`
	SourceCommit         string `json:"source_commit,omitempty"`
	SharedPreload        bool   `json:"shared_preload_verified,omitempty"`
	AggTokenTextAsUUID   bool   `json:"agg_token_text_as_uuid,omitempty"`
	AggregateTokenOID    uint32 `json:"aggregate_token_oid,omitempty"`
	UUIDOID              uint32 `json:"uuid_oid"`
}

type dataset struct {
	DirectSHA256  string `json:"direct_sha256"`
	ProvSQLSHA256 string `json:"provsql_sha256"`
	Rows          int    `json:"fingerprint_rows"`
	Equal         bool   `json:"equal"`
}

type sample struct {
	WorkloadID           string  `json:"workload_id"`
	Scale                int64   `json:"scale"`
	Iteration            int     `json:"iteration"`
	WorkloadOrder        int     `json:"workload_order_within_iteration"`
	Order                int     `json:"order_within_iteration"`
	System               string  `json:"system"`
	DurationMS           float64 `json:"duration_ms"`
	Rows                 int     `json:"rows"`
	ResultSHA256         string  `json:"result_sha256"`
	AggregateTokens      *int    `json:"aggregate_tokens,omitempty"`
	RepresentationFields *int    `json:"provenance_representation_fields,omitempty"`
	RowTokens            *int    `json:"row_tokens,omitempty"`
	RepresentationSHA256 string  `json:"provenance_representation_sha256,omitempty"`
	RootTypesVerified    *bool   `json:"root_types_verified,omitempty"`
	GatesBefore          *int64  `json:"gates_before,omitempty"`
	GatesAfter           *int64  `json:"gates_after,omitempty"`
	GateDelta            *int64  `json:"gate_delta,omitempty"`
	ArtifactBytesBefore  *int64  `json:"artifact_bytes_before,omitempty"`
	ArtifactBytesAfter   *int64  `json:"artifact_bytes_after,omitempty"`
	ArtifactByteDelta    *int64  `json:"artifact_byte_delta,omitempty"`
}

type summary struct {
	WorkloadID string        `json:"workload_id"`
	Scale      int64         `json:"scale"`
	System     string        `json:"system"`
	Samples    int           `json:"samples"`
	DurationMS distribution  `json:"duration_ms"`
	GateDelta  *distribution `json:"gate_delta,omitempty"`
	ByteDelta  *distribution `json:"artifact_byte_delta,omitempty"`
}

type distribution struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

type gate struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
	Status      string `json:"status"`
	Evidence    any    `json:"evidence,omitempty"`
}

type reportBinding struct {
	ConfigSHA256     string `json:"config_sha256"`
	ExecutableSHA256 string `json:"executable_sha256"`
}

type provSQLMetrics struct {
	Gates         int64
	ArtifactBytes int64
}

type queryResult struct {
	Rows                 int
	ResultHash           string
	AggregateTokens      int
	RepresentationFields int
	RowTokens            int
	RepresentationHash   string
	RootTypesVerified    bool
	Duration             time.Duration
	Before               provSQLMetrics
	After                provSQLMetrics
	HasMetrics           bool
}

func main() {
	var configPath, configEvidencePath, outputPath string
	var validateOnly bool
	flag.StringVar(&configPath, "config", "", "path to the strict JSON experiment configuration")
	flag.StringVar(&configEvidencePath, "config-evidence", "", "new path for the exact configuration bytes")
	flag.StringVar(&outputPath, "output", "", "new report path; existing files are never overwritten")
	flag.BoolVar(&validateOnly, "validate-only", false, "validate configuration without contacting PostgreSQL")
	flag.Parse()

	if strings.TrimSpace(configPath) == "" {
		fatal(errors.New("-config is required"))
	}
	raw, cfg, err := readConfig(configPath)
	if err != nil {
		fatal(err)
	}
	if validateOnly {
		fmt.Printf("valid provenance baseline config: %s\n", cfg.CampaignID)
		return
	}
	if strings.TrimSpace(outputPath) == "" {
		fatal(errors.New("-output is required unless -validate-only is used"))
	}
	if strings.TrimSpace(configEvidencePath) == "" || filepath.Clean(configEvidencePath) == filepath.Clean(outputPath) {
		fatal(errors.New("a distinct -config-evidence path is required unless -validate-only is used"))
	}
	if err := writeExclusive(configEvidencePath, raw, 0o600); err != nil {
		fatal(fmt.Errorf("preserve exact config: %w", err))
	}
	if err := reserveOutput(outputPath); err != nil {
		fatal(err)
	}

	result := newReport(cfg, raw)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.StatementTimeoutMS)*time.Millisecond*time.Duration((cfg.Warmups+cfg.Runs)*len(cfg.Workloads)*4+20))
	defer cancel()
	if err := run(ctx, cfg, &result); err != nil {
		result.Status = "failed"
		result.Errors = append(result.Errors, err.Error())
	}
	result.FinishedAt = time.Now().UTC()
	if err := writeReport(outputPath, result); err != nil {
		fatal(err)
	}
	if result.Status != "complete_measured_campaign" {
		fatal(fmt.Errorf("campaign failed; retained evidence at %s: %s", outputPath, strings.Join(result.Errors, "; ")))
	}
}

func readConfig(path string) ([]byte, config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, config{}, fmt.Errorf("read config: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var cfg config
	if err := decoder.Decode(&cfg); err != nil {
		return nil, config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return nil, config{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, config{}, err
	}
	return raw, cfg, nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("config contains trailing JSON")
		}
		return fmt.Errorf("decode trailing config data: %w", err)
	}
	return nil
}

func validateConfig(cfg config) error {
	if cfg.SchemaVersion != reportSchema || strings.TrimSpace(cfg.CampaignID) == "" {
		return errors.New("config requires schema_version=1 and a campaign_id")
	}
	if cfg.DataCacheStrategy != "warm" || cfg.CircuitStrategy != "novel_nonce" {
		return errors.New("only data_cache_strategy=warm with circuit_strategy=novel_nonce is supported")
	}
	if cfg.Warmups < 1 || cfg.Warmups > 100 || cfg.Runs < 1 || cfg.Runs > 10000 {
		return errors.New("warmups must be 1..100 and runs must be 1..10000")
	}
	if cfg.StatementTimeoutMS < 100 || cfg.StatementTimeoutMS > 3_600_000 {
		return errors.New("statement_timeout_ms must be 100..3600000")
	}
	if !envName.MatchString(cfg.DirectDSNEnv) || !envName.MatchString(cfg.ProvSQLDSNEnv) || cfg.DirectDSNEnv == cfg.ProvSQLDSNEnv {
		return errors.New("distinct uppercase direct_dsn_env and provsql_dsn_env names are required")
	}
	if strings.TrimSpace(cfg.ExpectedProvSQLVersion) == "" || !hex40.MatchString(cfg.ExpectedProvSQLCommit) {
		return errors.New("expected ProvSQL version and 40-character peeled source commit are required")
	}
	if strings.TrimSpace(cfg.DatasetFingerprintSQL) == "" || len(cfg.Workloads) == 0 {
		return errors.New("dataset_fingerprint_sql and at least one workload are required")
	}
	seen := make(map[string]struct{}, len(cfg.Workloads))
	type nonceRange struct{ start, end int64 }
	ranges := make([]nonceRange, 0, len(cfg.Workloads))
	for _, one := range cfg.Workloads {
		if strings.TrimSpace(one.ID) == "" || one.Scale <= 0 || one.ExpectedRows <= 0 ||
			one.ProvenanceCarriers < 1 || one.ProvenanceCarriers > 100 || one.NonceStart <= 0 ||
			strings.TrimSpace(one.CarrierGateType) == "" || strings.TrimSpace(one.RowGateType) == "" || strings.TrimSpace(one.SQL) == "" {
			return fmt.Errorf("workload %q is incomplete", one.ID)
		}
		if _, duplicate := seen[one.ID]; duplicate {
			return fmt.Errorf("duplicate workload id %q", one.ID)
		}
		seen[one.ID] = struct{}{}
		end := one.NonceStart + int64(cfg.Warmups+cfg.Runs) - 1
		if end < one.NonceStart {
			return fmt.Errorf("workload %q novelty nonce range overflows", one.ID)
		}
		for _, existing := range ranges {
			if one.NonceStart <= existing.end && existing.start <= end {
				return fmt.Errorf("workload %q novelty nonce range overlaps another workload", one.ID)
			}
		}
		ranges = append(ranges, nonceRange{one.NonceStart, end})
	}
	return nil
}

func newReport(cfg config, configRaw []byte) report {
	configSum := sha256.Sum256(configRaw)
	return report{
		SchemaVersion: reportSchema,
		Status:        "running",
		Boundary: boundary{
			ID: boundaryID,
			Included: []string{
				"the same SQL text and parameters on both PostgreSQL builds",
				"ordered SQL result production and complete client drain",
				"ProvSQL query rewriting and persistent row/aggregate circuit representation generation",
			},
			Excluded: []string{
				"ProvSQL semiring, probability, and Shapley evaluation",
				"TaskGate authorization, historical set difference, budget check, ledger commit, receipt, and result release",
			},
			Comparability:    "contextual overhead comparison only; ProvSQL circuits and TaskGate typed row/cell-oriented FactSets have different representations and granularity",
			MissingSemantics: "N/A: ProvSQL has no task-root ledger, replay-zero-novelty rule, or atomic exposure-budget settlement",
		},
		Campaign: campaign{ID: cfg.CampaignID, DataCacheStrategy: cfg.DataCacheStrategy, CircuitStrategy: cfg.CircuitStrategy, Warmups: cfg.Warmups,
			PostgreSQLProtocol: "simple_query_text_for_both_systems", Runs: cfg.Runs, OrderSeed: cfg.OrderSeed,
			WorkloadOrderingStrategy: "seeded_random_per_iteration", BaselineOrderingStrategy: "seeded_random_per_pair",
			StatementTimeoutMS: cfg.StatementTimeoutMS},
		StartedAt: time.Now().UTC(),
		Provenance: reportBinding{ConfigSHA256: hex.EncodeToString(configSum[:]),
			ExecutableSHA256: executableSHA256()},
		Limitations: []string{
			"This artifact does not rank ProvSQL against TaskGate end to end.",
			"The first column is canonical visible output; configured extra columns preserve ProvSQL aggregate-token representations and are drained but excluded from the visible-result hash.",
			"ProvSQL circuit mmap allocation is persistent and page/capacity granular; byte deltas are descriptive, not marginal per-result sizes.",
			"Container memory is added only by the outer run/finalize driver; this command never invents a peak-memory value.",
		},
	}
}

func run(ctx context.Context, cfg config, result *report) error {
	directDSN := strings.TrimSpace(os.Getenv(cfg.DirectDSNEnv))
	provDSN := strings.TrimSpace(os.Getenv(cfg.ProvSQLDSNEnv))
	if directDSN == "" || provDSN == "" {
		return fmt.Errorf("both %s and %s must be set", cfg.DirectDSNEnv, cfg.ProvSQLDSNEnv)
	}
	direct, err := pgx.Connect(ctx, directDSN)
	if err != nil {
		return fmt.Errorf("connect direct PostgreSQL: %w", err)
	}
	defer direct.Close(context.Background())
	prov, err := pgx.Connect(ctx, provDSN)
	if err != nil {
		return fmt.Errorf("connect ProvSQL PostgreSQL: %w", err)
	}
	defer prov.Close(context.Background())
	if err := configureSession(ctx, direct, cfg.StatementTimeoutMS, false); err != nil {
		return fmt.Errorf("configure direct PostgreSQL session: %w", err)
	}
	if err := configureSession(ctx, prov, cfg.StatementTimeoutMS, true); err != nil {
		return fmt.Errorf("configure ProvSQL session: %w", err)
	}

	result.Systems.Direct, err = inspectSystem(ctx, direct, false, "")
	if err != nil {
		return err
	}
	result.Systems.ProvSQL, err = inspectSystem(ctx, prov, true, cfg.ExpectedProvSQLCommit)
	if err != nil {
		return err
	}
	versionsEqual := result.Systems.Direct.PostgreSQLVersion == result.Systems.ProvSQL.PostgreSQLVersion &&
		result.Systems.Direct.PostgreSQLVersionNum == result.Systems.ProvSQL.PostgreSQLVersionNum
	result.Gates = append(result.Gates, gate{ID: "same_postgresql_version", Requirement: "direct and ProvSQL servers report the same PostgreSQL version", Status: passFail(versionsEqual), Evidence: result.Systems})
	timeoutOK := result.Systems.Direct.StatementTimeoutMS == int64(cfg.StatementTimeoutMS) &&
		result.Systems.ProvSQL.StatementTimeoutMS == int64(cfg.StatementTimeoutMS)
	result.Gates = append(result.Gates, gate{ID: "statement_timeout_enforced", Requirement: "both sessions enforce the configured per-statement PostgreSQL timeout", Status: passFail(timeoutOK), Evidence: map[string]any{
		"configured_ms": cfg.StatementTimeoutMS,
		"direct_ms":     result.Systems.Direct.StatementTimeoutMS,
		"provsql_ms":    result.Systems.ProvSQL.StatementTimeoutMS,
	}})
	deterministicSessions := result.Systems.Direct.MaxParallelWorkers == 0 && result.Systems.ProvSQL.MaxParallelWorkers == 0 &&
		result.Systems.Direct.ClientMinMessages == "error" && result.Systems.ProvSQL.ClientMinMessages == "error" &&
		result.Systems.Direct.LogMinMessages == "error" && result.Systems.ProvSQL.LogMinMessages == "error" &&
		result.Systems.ProvSQL.AggTokenTextAsUUID && result.Systems.ProvSQL.AggregateTokenOID != 0 &&
		result.Systems.Direct.UUIDOID != 0 && result.Systems.Direct.UUIDOID == result.Systems.ProvSQL.UUIDOID
	result.Gates = append(result.Gates, gate{ID: "deterministic_sessions", Requirement: "both sessions disable parallel gather and warning I/O; ProvSQL emits aggregate-root UUIDs", Status: passFail(deterministicSessions), Evidence: result.Systems})
	extensionOK := result.Systems.ProvSQL.ExtensionVersion == cfg.ExpectedProvSQLVersion && result.Systems.ProvSQL.SharedPreload
	result.Gates = append(result.Gates, gate{ID: "pinned_provsql", Requirement: "configured ProvSQL release is loaded through shared_preload_libraries", Status: passFail(extensionOK), Evidence: result.Systems.ProvSQL})
	if !versionsEqual || !timeoutOK || !deterministicSessions || !extensionOK {
		return errors.New("system preflight failed")
	}

	directFingerprint, rows, err := fingerprint(ctx, direct, cfg.DatasetFingerprintSQL, false)
	if err != nil {
		return fmt.Errorf("fingerprint direct dataset: %w", err)
	}
	provFingerprint, provRows, err := fingerprint(ctx, prov, cfg.DatasetFingerprintSQL, true)
	if err != nil {
		return fmt.Errorf("fingerprint ProvSQL dataset: %w", err)
	}
	result.Dataset = dataset{DirectSHA256: directFingerprint, ProvSQLSHA256: provFingerprint,
		Rows: rows, Equal: rows == provRows && directFingerprint == provFingerprint}
	result.Gates = append(result.Gates, gate{ID: "identical_dataset", Requirement: "both systems expose the same canonical dataset stream", Status: passFail(result.Dataset.Equal), Evidence: result.Dataset})
	if !result.Dataset.Equal {
		return errors.New("dataset fingerprints differ")
	}

	rng := rand.New(rand.NewSource(cfg.OrderSeed)) // #nosec G404 -- experimental ordering, not security.
	seenRepresentations := make(map[string]struct{}, len(cfg.Workloads)*(cfg.Warmups+cfg.Runs))
	novelExecutions := 0
	for iteration := -cfg.Warmups; iteration < cfg.Runs; iteration++ {
		for workloadPosition, workloadIndex := range rng.Perm(len(cfg.Workloads)) {
			work := cfg.Workloads[workloadIndex]
			nonce := work.NonceStart + int64(iteration+cfg.Warmups)
			order := []string{"direct_postgresql", "provsql"}
			if rng.Intn(2) == 1 {
				order[0], order[1] = order[1], order[0]
			}
			paired := make(map[string]queryResult, 2)
			for position, name := range order {
				var one queryResult
				if name == "direct_postgresql" {
					one, err = executeQuery(ctx, direct, work, nonce, false, result.Systems.Direct)
				} else {
					one, err = executeQuery(ctx, prov, work, nonce, true, result.Systems.ProvSQL)
				}
				if err != nil {
					return fmt.Errorf("%s workload %s iteration %d: %w", name, work.ID, iteration, err)
				}
				paired[name] = one
				if iteration >= 0 {
					result.Samples = append(result.Samples, makeSample(work, iteration, workloadPosition, position, name, one))
				}
			}
			directResult, provResult := paired["direct_postgresql"], paired["provsql"]
			expectedFields := work.ExpectedRows * (work.ProvenanceCarriers + 1)
			expectedAggregateTokens := work.ExpectedRows * work.ProvenanceCarriers
			if directResult.Rows != work.ExpectedRows || provResult.Rows != work.ExpectedRows ||
				directResult.ResultHash != provResult.ResultHash || provResult.RowTokens != work.ExpectedRows ||
				provResult.AggregateTokens != expectedAggregateTokens || provResult.RepresentationFields != expectedFields ||
				!provResult.RootTypesVerified {
				return fmt.Errorf("workload %s iteration %d result/provenance mismatch: direct rows=%d provsql rows=%d aggregate_tokens=%d row_tokens=%d representation_fields=%d expected_rows=%d expected_aggregate_tokens=%d expected_fields=%d roots_verified=%t direct_hash=%s provsql_hash=%s",
					work.ID, iteration, directResult.Rows, provResult.Rows, provResult.AggregateTokens,
					provResult.RowTokens, provResult.RepresentationFields, work.ExpectedRows,
					expectedAggregateTokens, expectedFields, provResult.RootTypesVerified,
					directResult.ResultHash, provResult.ResultHash)
			}
			if provResult.After.Gates <= provResult.Before.Gates {
				return fmt.Errorf("workload %s iteration %d nonce %d did not generate a novel ProvSQL circuit", work.ID, iteration, nonce)
			}
			if _, duplicate := seenRepresentations[provResult.RepresentationHash]; duplicate {
				return fmt.Errorf("workload %s iteration %d nonce %d reused a provenance representation", work.ID, iteration, nonce)
			}
			seenRepresentations[provResult.RepresentationHash] = struct{}{}
			novelExecutions++
		}
	}

	result.Summaries = summarize(result.Samples)
	expectedSamples := len(cfg.Workloads) * cfg.Runs * 2
	complete := len(result.Samples) == expectedSamples
	result.Gates = append(result.Gates,
		gate{ID: "result_equivalence", Requirement: "every paired query has an identical ordered canonical visible result and verified aggregate/row provenance roots", Status: "pass", Evidence: map[string]any{"paired_iterations": len(cfg.Workloads) * cfg.Runs}},
		gate{ID: "novel_circuit_generation", Requirement: "each unique nonce produces a new, nonempty ProvSQL circuit representation", Status: "pass", Evidence: map[string]any{"all_executions_including_warmup": novelExecutions, "measured_executions": len(cfg.Workloads) * cfg.Runs}},
		gate{ID: "sample_completeness", Requirement: "all configured measured samples are present", Status: passFail(complete), Evidence: map[string]int{"expected": expectedSamples, "actual": len(result.Samples)}})
	if !complete {
		return errors.New("sample count is incomplete")
	}
	result.Status = "complete_measured_campaign"
	return nil
}

func configureSession(ctx context.Context, conn *pgx.Conn, milliseconds int, provenance bool) error {
	// milliseconds has already been range-validated, so formatting it into SET
	// cannot alter SQL structure. SET applies to every subsequent statement in
	// this session, including fingerprints, workload queries, and metric reads.
	statements := []string{
		fmt.Sprintf("SET statement_timeout = %d", milliseconds),
		"SET max_parallel_workers_per_gather = 0",
		"SET client_min_messages = error",
		"SET log_min_messages = error",
	}
	if provenance {
		statements = append(statements, "SET provsql.aggtoken_text_as_uuid = on")
	}
	for _, statement := range statements {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func inspectSystem(ctx context.Context, conn *pgx.Conn, withProvSQL bool, sourceCommit string) (system, error) {
	var result system
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&result.PostgreSQLVersion); err != nil {
		return system{}, fmt.Errorf("inspect PostgreSQL version: %w", err)
	}
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')").Scan(&result.PostgreSQLVersionNum); err != nil {
		return system{}, fmt.Errorf("inspect PostgreSQL version number: %w", err)
	}
	if err := conn.QueryRow(ctx, "SELECT setting::bigint FROM pg_settings WHERE name='statement_timeout'").Scan(&result.StatementTimeoutMS); err != nil {
		return system{}, fmt.Errorf("inspect PostgreSQL statement timeout: %w", err)
	}
	if err := conn.QueryRow(ctx, "SELECT setting::bigint FROM pg_settings WHERE name='max_parallel_workers_per_gather'").Scan(&result.MaxParallelWorkers); err != nil {
		return system{}, fmt.Errorf("inspect max_parallel_workers_per_gather: %w", err)
	}
	if err := conn.QueryRow(ctx, "SELECT current_setting('client_min_messages'), current_setting('log_min_messages'), 'uuid'::regtype::oid").Scan(&result.ClientMinMessages, &result.LogMinMessages, &result.UUIDOID); err != nil {
		return system{}, fmt.Errorf("inspect deterministic session settings: %w", err)
	}
	if !withProvSQL {
		return result, nil
	}
	if err := conn.QueryRow(ctx, "SELECT extversion FROM pg_extension WHERE extname='provsql'").Scan(&result.ExtensionVersion); err != nil {
		return system{}, fmt.Errorf("inspect ProvSQL extension: %w", err)
	}
	var preload string
	if err := conn.QueryRow(ctx, "SELECT current_setting('shared_preload_libraries')").Scan(&preload); err != nil {
		return system{}, fmt.Errorf("inspect shared_preload_libraries: %w", err)
	}
	for _, entry := range strings.Split(preload, ",") {
		if strings.TrimSpace(entry) == "provsql" {
			result.SharedPreload = true
		}
	}
	result.SourceCommit = sourceCommit
	if err := conn.QueryRow(ctx, "SELECT current_setting('provsql.aggtoken_text_as_uuid')::boolean, 'provsql.agg_token'::regtype::oid").Scan(&result.AggTokenTextAsUUID, &result.AggregateTokenOID); err != nil {
		return system{}, fmt.Errorf("inspect ProvSQL aggregate-token settings: %w", err)
	}
	return result, nil
}

func fingerprint(ctx context.Context, conn *pgx.Conn, sql string, disableProvSQL bool) (string, int, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback(context.Background())
	if disableProvSQL {
		if _, err := tx.Exec(ctx, "SET LOCAL provsql.active = off"); err != nil {
			return "", 0, err
		}
	}
	rows, err := tx.Query(ctx, sql)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	values := make([]string, 0, 1024)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return "", 0, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", 0, err
	}
	return orderedHash(values), len(values), nil
}

func executeQuery(ctx context.Context, conn *pgx.Conn, work workload, nonce int64, provenance bool, inspected system) (queryResult, error) {
	result := queryResult{}
	var err error
	if provenance {
		result.Before, err = readProvSQLMetrics(ctx, conn)
		if err != nil {
			return queryResult{}, err
		}
		result.HasMetrics = true
	}
	started := time.Now()
	// ProvSQL's planner hook appends its hidden token column after parse/describe.
	// The extended protocol cannot bind per-column result formats to that changed
	// target list. Use the same simple text protocol on both systems so transport
	// differences cannot masquerade as provenance overhead.
	rows, err := conn.Query(ctx, work.SQL, pgx.QueryExecModeSimpleProtocol, work.Scale, nonce)
	if err != nil {
		return queryResult{}, err
	}
	expectedColumns := 1 + work.ProvenanceCarriers
	if provenance {
		expectedColumns++ // ProvSQL's hidden row-circuit token.
	}
	fields := rows.FieldDescriptions()
	if len(fields) != expectedColumns {
		rows.Close()
		return queryResult{}, fmt.Errorf("query returned %d columns, expected %d", len(fields), expectedColumns)
	}
	if provenance {
		for index := 1; index <= work.ProvenanceCarriers; index++ {
			if fields[index].DataTypeOID != inspected.AggregateTokenOID {
				rows.Close()
				return queryResult{}, fmt.Errorf("provenance carrier column %d has OID %d, expected agg_token OID %d", index, fields[index].DataTypeOID, inspected.AggregateTokenOID)
			}
		}
		hidden := fields[len(fields)-1]
		if hidden.DataTypeOID != inspected.UUIDOID || hidden.Name != "provsql" {
			rows.Close()
			return queryResult{}, fmt.Errorf("hidden provenance column name/OID is %q/%d, expected provsql/%d", hidden.Name, hidden.DataTypeOID, inspected.UUIDOID)
		}
	}
	values := make([]string, 0, 8)
	aggregateRoots := make([]string, 0, 8)
	rowRoots := make([]string, 0, 8)
	for rows.Next() {
		rowValues := make([]string, expectedColumns)
		destinations := make([]any, expectedColumns)
		for index := range rowValues {
			destinations[index] = &rowValues[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			rows.Close()
			return queryResult{}, err
		}
		values = append(values, rowValues[0])
		if provenance {
			for _, root := range rowValues[1 : 1+work.ProvenanceCarriers] {
				if !uuid.MatchString(root) {
					rows.Close()
					return queryResult{}, fmt.Errorf("aggregate provenance root is not a canonical UUID: %q", root)
				}
				aggregateRoots = append(aggregateRoots, root)
			}
			rowRoot := rowValues[len(rowValues)-1]
			if !uuid.MatchString(rowRoot) {
				rows.Close()
				return queryResult{}, fmt.Errorf("row provenance root is not a canonical UUID: %q", rowRoot)
			}
			rowRoots = append(rowRoots, rowRoot)
			result.RowTokens++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return queryResult{}, err
	}
	rows.Close()
	result.Duration = time.Since(started)
	result.Rows = len(values)
	result.ResultHash = orderedHash(values)
	if provenance {
		result.AggregateTokens = len(aggregateRoots)
		result.RepresentationFields = len(aggregateRoots) + len(rowRoots)
		representations := make([]string, 0, result.RepresentationFields)
		for _, root := range aggregateRoots {
			representations = append(representations, "aggregate:"+root)
		}
		for _, root := range rowRoots {
			representations = append(representations, "row:"+root)
		}
		result.RepresentationHash = canonicalHash(representations)
		result.After, err = readProvSQLMetrics(ctx, conn)
		if err != nil {
			return queryResult{}, err
		}
		if err := validateRootTypes(ctx, conn, aggregateRoots, work.CarrierGateType); err != nil {
			return queryResult{}, err
		}
		if err := validateRootTypes(ctx, conn, rowRoots, work.RowGateType); err != nil {
			return queryResult{}, err
		}
		result.RootTypesVerified = true
	}
	return result, nil
}

func validateRootTypes(ctx context.Context, conn *pgx.Conn, roots []string, expected string) error {
	for _, root := range roots {
		var actual string
		if err := conn.QueryRow(ctx, "SELECT provsql.get_gate_type($1::uuid)::text", root).Scan(&actual); err != nil {
			return fmt.Errorf("inspect provenance root %s: %w", root, err)
		}
		if actual != expected {
			return fmt.Errorf("provenance root %s has gate type %q, expected %q", root, actual, expected)
		}
	}
	return nil
}

func readProvSQLMetrics(ctx context.Context, conn *pgx.Conn) (provSQLMetrics, error) {
	const query = `
SELECT provsql.get_nb_gates(),
       COALESCE(sum((pg_stat_file(
           'base/' || d.oid::text || '/' || f.name,
           true
       )).size), 0)::bigint
FROM pg_database AS d
CROSS JOIN unnest(ARRAY[
  'provsql_gates.mmap', 'provsql_wires.mmap', 'provsql_mapping.mmap',
  'provsql_extra.mmap', 'provsql_table_info.mmap'
]) AS f(name)
WHERE d.datname = current_database()
GROUP BY provsql.get_nb_gates()`
	var result provSQLMetrics
	if err := conn.QueryRow(ctx, query).Scan(&result.Gates, &result.ArtifactBytes); err != nil {
		return provSQLMetrics{}, fmt.Errorf("read ProvSQL circuit metrics: %w", err)
	}
	return result, nil
}

func makeSample(work workload, iteration, workloadOrder, order int, systemName string, result queryResult) sample {
	one := sample{WorkloadID: work.ID, Scale: work.Scale, Iteration: iteration, WorkloadOrder: workloadOrder, Order: order,
		System: systemName, DurationMS: float64(result.Duration) / float64(time.Millisecond), Rows: result.Rows,
		ResultSHA256: result.ResultHash}
	if result.HasMetrics {
		aggregateTokens, fields, rowTokens, rootsVerified := result.AggregateTokens, result.RepresentationFields, result.RowTokens, result.RootTypesVerified
		gateDelta := result.After.Gates - result.Before.Gates
		byteDelta := result.After.ArtifactBytes - result.Before.ArtifactBytes
		one.AggregateTokens = &aggregateTokens
		one.RepresentationFields = &fields
		one.RowTokens = &rowTokens
		one.RepresentationSHA256 = result.RepresentationHash
		one.RootTypesVerified = &rootsVerified
		one.GatesBefore, one.GatesAfter, one.GateDelta = &result.Before.Gates, &result.After.Gates, &gateDelta
		one.ArtifactBytesBefore, one.ArtifactBytesAfter, one.ArtifactByteDelta =
			&result.Before.ArtifactBytes, &result.After.ArtifactBytes, &byteDelta
	}
	return one
}

func canonicalHash(values []string) string {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	return framedHash(ordered)
}

func orderedHash(values []string) string {
	return framedHash(values)
}

func framedHash(values []string) string {
	hash := sha256.New()
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(len(values)))
	_, _ = hash.Write(encoded[:])
	for _, value := range values {
		binary.BigEndian.PutUint64(encoded[:], uint64(len(value)))
		_, _ = hash.Write(encoded[:])
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func summarize(samples []sample) []summary {
	type key struct {
		workload string
		scale    int64
		system   string
	}
	groups := make(map[key][]sample)
	for _, one := range samples {
		groups[key{one.WorkloadID, one.Scale, one.System}] = append(groups[key{one.WorkloadID, one.Scale, one.System}], one)
	}
	keys := make([]key, 0, len(groups))
	for one := range groups {
		keys = append(keys, one)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].scale != keys[j].scale {
			return keys[i].scale < keys[j].scale
		}
		if keys[i].workload != keys[j].workload {
			return keys[i].workload < keys[j].workload
		}
		return keys[i].system < keys[j].system
	})
	result := make([]summary, 0, len(keys))
	for _, oneKey := range keys {
		group := groups[oneKey]
		durations := make([]float64, 0, len(group))
		gates := make([]float64, 0, len(group))
		bytes := make([]float64, 0, len(group))
		for _, one := range group {
			durations = append(durations, one.DurationMS)
			if one.GateDelta != nil {
				gates = append(gates, float64(*one.GateDelta))
			}
			if one.ArtifactByteDelta != nil {
				bytes = append(bytes, float64(*one.ArtifactByteDelta))
			}
		}
		item := summary{WorkloadID: oneKey.workload, Scale: oneKey.scale, System: oneKey.system,
			Samples: len(group), DurationMS: describe(durations)}
		if len(gates) != 0 {
			gateDistribution, byteDistribution := describe(gates), describe(bytes)
			item.GateDelta = &gateDistribution
			item.ByteDelta = &byteDistribution
		}
		result = append(result, item)
	}
	return result
}

func describe(values []float64) distribution {
	if len(values) == 0 {
		return distribution{}
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	var sum float64
	for _, value := range ordered {
		sum += value
	}
	return distribution{Count: len(ordered), Min: ordered[0], P50: quantile(ordered, 0.50),
		P95: quantile(ordered, 0.95), Max: ordered[len(ordered)-1], Mean: sum / float64(len(ordered))}
}

func quantile(sortedValues []float64, probability float64) float64 {
	if len(sortedValues) == 1 {
		return sortedValues[0]
	}
	position := probability * float64(len(sortedValues)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sortedValues[lower]
	}
	return sortedValues[lower] + (position-float64(lower))*(sortedValues[upper]-sortedValues[lower])
}

func executableSHA256() string {
	path, err := os.Executable()
	if err != nil {
		return "unavailable"
	}
	file, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func passFail(value bool) string {
	if value {
		return "pass"
	}
	return "fail"
}

func reserveOutput(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("reserve output: %w", err)
	}
	return file.Close()
}

func writeExclusive(path string, value []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeReport(path string, result report) error {
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
