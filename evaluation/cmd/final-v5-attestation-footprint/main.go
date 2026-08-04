// Command final-v5-attestation-footprint qualifies the PostgreSQL-internal
// Attestation footprint for one activated Profile Catalog.
//
// It measures the internal (toplevel=false) statements one Attestation causes,
// separately for each of the four scopes the Connector actually reaches:
//
//	constructor_or_cold_pool     dataconnector.New, while opening the pool
//	explicit_preflight_pool      Connector.Attestation, against the pool
//	single_query_transaction     inside a Connector.Query transaction
//	paired_query_transaction     inside a Connector.QueryPairStream transaction
//
// They are never merged. They reach the same Go code, but that is not evidence
// that PostgreSQL emits the same internal statements: transaction state,
// snapshot isolation and plan caching are properties of the server. In
// particular the Artifact paired-novel path must consume the footprint measured
// through QueryPairStream, not one measured through Query.
//
// # ExpectedSchema identity
//
// The qualification identity comes from catalogschema.Build over the activated
// Profile Catalog -- the same construction the Gateway performs at startup.
//
// It is deliberately NOT reconstructed by reading live PostgreSQL catalogs. A
// live read of column name and type omits collation, collation version and
// collation determinism, all of which catalogschema.Digest covers, so a
// reconstructed schema digests differently from the one production holds. A
// footprint qualified against that digest would be bound to an ExpectedSchema
// the Gateway never has. The live relations are still read, but only to verify
// the Catalog against the deployed database, never to define the identity.
//
// # Measurement sequence
//
// pg_stat_statements is reset exactly once per qualification. Every measurement
// is the delta between two adjacent cumulative snapshots, each isolating exactly
// one Attestation in one scope. Nothing is divided by an assumed number of
// attestations per trial.
//
// Every adjacent interval binds and verifies stats_reset, dealloc, the
// measurement environment, the postmaster start time and that the total call
// delta equals the sum of the structural row deltas. A reset, an eviction, a
// server restart or an unaccounted statement inside the window invalidates the
// interval rather than silently skewing a count.
//
// Nothing here is called "cold" because pg_stat_statements was reset; resetting
// the view does not make a server cold. The constructor interval is the first
// relevant access after a fresh deployment, which is the only sense in which any
// measurement here is a first access, and it is named for what it is.
//
// It is diagnosis only. It changes no capability, no activation support and no
// contract state, and it produces no publication-eligible sample. Raw and
// normalized SQL are inspected in process and never written out; only structural
// digests and counts are emitted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

// structuralKey is the classifier key: structural identity plus toplevel.
type structuralKey struct {
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	TopLevel        bool   `json:"toplevel"`
}

type structuralEntry struct {
	structuralKey
	Calls   int64  `json:"calls"`
	QueryID string `json:"queryid"`
}

// snapshot is one cumulative reading of the whole gateway_reader census plus the
// invariants every interval is bound to.
type snapshot struct {
	Index int    `json:"index"`
	Label string `json:"label"`
	// Calls is the cumulative call count per structural key.
	calls map[structuralKey]int64
	// queryIDs is deployment-local diagnosis only.
	queryIDs map[structuralKey]string

	Total                int64                             `json:"total_calls"`
	StatsReset           string                            `json:"stats_reset"`
	Dealloc              int64                             `json:"dealloc"`
	PostmasterStartTime  string                            `json:"postmaster_start_time"`
	Environment          experiment.MeasurementEnvironment `json:"measurement_environment"`
	PgStatStatementsRows int64                             `json:"pg_stat_statements_rows"`
}

// measuredInterval is one adjacent-snapshot delta attributable to exactly one
// Attestation in one scope.
type measuredInterval struct {
	Index int                         `json:"index"`
	Scope experiment.AttestationScope `json:"scope"`
	// Repetition counts occurrences of this scope within the qualification, from
	// zero. It carries no warmth claim.
	Repetition int    `json:"repetition"`
	FromLabel  string `json:"from_snapshot"`
	ToLabel    string `json:"to_snapshot"`
	// Attestations is how many Attestations this interval isolates. It is always
	// one by construction: the sequence brackets each Attestation individually
	// rather than dividing a combined delta.
	Attestations int64             `json:"attestations"`
	TopLevel     []structuralEntry `json:"toplevel_delta"`
	Internal     []structuralEntry `json:"internal_delta"`
	TotalDelta   int64             `json:"total_delta"`
	// StructuralSum must equal TotalDelta, proving no call was counted outside a
	// structural row this probe classified.
	StructuralSum int64 `json:"structural_sum"`
}

type qualificationReport struct {
	SchemaVersion             int    `json:"schema_version"`
	Record                    string `json:"record"`
	DiagnosisID               string `json:"diagnosis_id"`
	PublicationEligible       bool   `json:"publication_eligible"`
	CapabilityChanging        bool   `json:"capability_changing"`
	ActivationSupportChanging bool   `json:"activation_support_changing"`
	FormalCampaign            bool   `json:"formal_campaign"`

	CatalogPath           string `json:"catalog_path"`
	CatalogSHA256         string `json:"catalog_sha256"`
	ExpectedSchemaDigest  string `json:"expected_schema_digest"`
	ExpectedSchemaEntries int64  `json:"expected_schema_entries"`
	// ExpectedSchemaRelations names the ordered entries, for review. It is
	// schema-qualified relation naming only; no data and no SQL.
	ExpectedSchemaRelations []string `json:"expected_schema_relations"`
	// LiveSchemaVerified reports that every Catalog entry exists in the deployed
	// database with the same ordered column names and types. The live read
	// verifies the Catalog; it never defines the identity.
	LiveSchemaVerified bool `json:"live_schema_verified"`

	Environment              experiment.MeasurementEnvironment    `json:"measurement_environment"`
	PostgreSQL               experiment.PostgreSQLRuntimeIdentity `json:"postgresql_runtime_identity"`
	QueryIDPortabilityCaveat string                               `json:"queryid_portability_caveat"`

	Snapshots []snapshot         `json:"snapshots"`
	Intervals []measuredInterval `json:"intervals"`

	// ScopeStable reports, per scope, that every repetition produced the same
	// exact internal multiset.
	ScopeStable map[experiment.AttestationScope]bool `json:"scope_stable"`
	// ConstructorMatchesExplicitPreflight is retained evidence for a future
	// revision that wants one portable preflight scope. The two are not merged
	// here.
	ConstructorMatchesExplicitPreflight bool `json:"constructor_matches_explicit_preflight"`

	Footprint        experiment.AttestationFootprintV2 `json:"footprint"`
	FootprintSHA256  string                            `json:"footprint_sha256"`
	PortableSHA256   string                            `json:"portable_footprint_sha256"`
	InternalKeyCount int                               `json:"internal_key_count"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-attestation-footprint:", err)
		os.Exit(1)
	}
}

func run() error {
	var dsn, adminDSN, out, catalogPath, identityPath, datasourceID, database, reader string
	var majorVersion, repetitions int
	flag.StringVar(&dsn, "gateway-reader-dsn", "", "gateway_reader DSN for the Connector under test")
	flag.StringVar(&adminDSN, "admin-dsn", "", "superuser DSN used only to read and reset pg_stat_statements")
	flag.StringVar(&out, "out", "", "qualification report path")
	flag.StringVar(&catalogPath, "catalog", "", "activated Profile Catalog whose ExpectedSchema is qualified")
	flag.StringVar(&identityPath, "postgresql-identity", "",
		"JSON file carrying the complete immutable PostgreSQL runtime identity")
	flag.StringVar(&datasourceID, "datasource-id", "", "expected datasource identity")
	flag.StringVar(&database, "database", "travel_demo", "expected database")
	flag.StringVar(&reader, "reader-role", "gateway_reader", "expected role")
	flag.IntVar(&majorVersion, "postgresql-major", 16, "expected PostgreSQL major version")
	flag.IntVar(&repetitions, "repetitions", 3, "repetitions of each repeatable scope")
	flag.Parse()
	if dsn == "" || adminDSN == "" || out == "" || catalogPath == "" || identityPath == "" {
		return errors.New("gateway-reader-dsn, admin-dsn, out, catalog and postgresql-identity are required")
	}
	if repetitions < 2 {
		return fmt.Errorf("repetitions must be at least 2 so a scope has something to agree with, got %d", repetitions)
	}
	diagnosisID := strings.TrimSpace(os.Getenv("DIAGNOSIS_ID"))
	if diagnosisID == "" {
		return errors.New("DIAGNOSIS_ID must name this non-formal qualification run")
	}

	postgreSQL, err := loadRuntimeIdentity(identityPath)
	if err != nil {
		return err
	}

	// The ExpectedSchema identity comes from the Catalog builder, never from a
	// live catalog read.
	logicalCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		return fmt.Errorf("load catalog %s: %w", catalogPath, err)
	}
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		return fmt.Errorf("build ExpectedSchema from %s: %w", catalogPath, err)
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer admin.Close(context.Background())

	document := qualificationReport{
		SchemaVersion: 3, Record: "taskgate-final-v5-attestation-footprint-qualification-v3",
		DiagnosisID:         diagnosisID,
		PublicationEligible: false, CapabilityChanging: false,
		ActivationSupportChanging: false, FormalCampaign: false,
		CatalogPath: catalogPath, ExpectedSchemaDigest: built.Digest,
		ExpectedSchemaEntries: built.Count, PostgreSQL: postgreSQL,
		QueryIDPortabilityCaveat: "queryid is PostgreSQL-version and installation specific; " +
			"it is recorded for deployment-local diagnosis only and is not a portable identity",
		ScopeStable: map[experiment.AttestationScope]bool{},
	}
	for _, entry := range built.Entries {
		document.ExpectedSchemaRelations = append(document.ExpectedSchemaRelations, entry.Schema+"."+entry.View)
	}

	// The live read verifies the Catalog against the deployed database. A
	// mismatch means the qualification would attest against relations that do
	// not match what the Catalog describes.
	if err := verifyLiveSchema(ctx, admin, built.Entries); err != nil {
		return fmt.Errorf("verify Catalog ExpectedSchema against the deployed database: %w", err)
	}
	document.LiveSchemaVerified = true

	attestation := dataconnector.ExpectedAttestation{
		DatasourceID: datasourceID, Database: database, User: reader,
		PostgreSQLMajorVersion: majorVersion,
	}

	sequence, err := measureSequence(ctx, admin, dsn, attestation, built.Entries, repetitions)
	if err != nil {
		return err
	}
	document.Snapshots, document.Intervals = sequence.snapshots, sequence.intervals
	document.Environment = sequence.environment

	measured := map[experiment.AttestationScope][]experiment.AttestationInternalEntry{}
	for _, scope := range experiment.AttestationScopes() {
		entries, stable, count, err := agreedFootprint(sequence.intervals, scope)
		if err != nil {
			return err
		}
		document.ScopeStable[scope] = stable
		if !stable {
			return fmt.Errorf("ATTESTATION INTERNAL FOOTPRINT NOT STABLE: scope %s disagreed across %d intervals",
				scope, count)
		}
		measured[scope] = entries
	}

	footprint, err := experiment.NewAttestationFootprintV2(built.Digest, built.Count,
		sequence.environment, postgreSQL, diagnosisID, measured)
	if err != nil {
		return err
	}
	document.Footprint = footprint
	document.ConstructorMatchesExplicitPreflight = footprint.ConstructorMatchesExplicitPreflight()
	if document.FootprintSHA256, err = footprint.SHA256(); err != nil {
		return err
	}
	if document.PortableSHA256, err = footprint.PortableSHA256(); err != nil {
		return err
	}
	document.InternalKeyCount = len(footprint.InternalKeys())

	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("attestation footprint qualification for %s\n", catalogPath)
	fmt.Printf("  ExpectedSchema E=%d digest=%s (from catalogschema.Build)\n",
		built.Count, built.Digest[:12])
	fmt.Printf("  %d snapshots, %d intervals, %d internal keys\n",
		len(document.Snapshots), len(document.Intervals), document.InternalKeyCount)
	for _, scope := range experiment.AttestationScopes() {
		bound, _ := footprint.Scope(scope)
		fmt.Printf("  %-28s %d internal calls/attestation over %d keys\n",
			scope, bound.TotalCallsPerAttestation(), len(bound.Internal))
	}
	fmt.Printf("  constructor == explicit preflight: %t\n", document.ConstructorMatchesExplicitPreflight)
	fmt.Printf("  footprint=%s portable=%s\n", document.FootprintSHA256[:12], document.PortableSHA256[:12])
	return nil
}

func loadRuntimeIdentity(path string) (experiment.PostgreSQLRuntimeIdentity, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return experiment.PostgreSQLRuntimeIdentity{}, fmt.Errorf("read PostgreSQL identity: %w", err)
	}
	var identity experiment.PostgreSQLRuntimeIdentity
	if err := json.Unmarshal(payload, &identity); err != nil {
		return experiment.PostgreSQLRuntimeIdentity{}, fmt.Errorf("decode PostgreSQL identity: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return experiment.PostgreSQLRuntimeIdentity{}, fmt.Errorf("PostgreSQL identity: %w", err)
	}
	return identity, nil
}

type sequenceResult struct {
	snapshots   []snapshot
	intervals   []measuredInterval
	environment experiment.MeasurementEnvironment
}

// measureSequence resets pg_stat_statements once and brackets each Attestation
// between adjacent cumulative snapshots.
func measureSequence(ctx context.Context, admin *pgx.Conn, dsn string,
	attestation dataconnector.ExpectedAttestation, expected []dataconnector.ViewSchema,
	repetitions int) (sequenceResult, error) {
	var result sequenceResult
	if _, err := admin.Exec(ctx, `SELECT public.pg_stat_statements_reset()`); err != nil {
		return result, fmt.Errorf("reset pg_stat_statements: %w", err)
	}

	take := func(label string) (snapshot, error) {
		taken, err := readSnapshot(ctx, admin)
		if err != nil {
			return snapshot{}, fmt.Errorf("snapshot %s: %w", label, err)
		}
		taken.Index, taken.Label = len(result.snapshots), label
		result.snapshots = append(result.snapshots, taken)
		return taken, nil
	}

	baseline, err := take("baseline")
	if err != nil {
		return result, err
	}
	if err := baseline.Environment.Validate(); err != nil {
		return result, err
	}
	result.environment = baseline.Environment
	previous := baseline

	// bracket runs one Attestation-bearing action and records the interval it
	// isolates.
	repetitionOf := map[experiment.AttestationScope]int{}
	bracket := func(scope experiment.AttestationScope, label string, action func() error) error {
		if err := action(); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		current, err := take(label)
		if err != nil {
			return err
		}
		interval, err := deltaBetween(previous, current, scope, repetitionOf[scope])
		if err != nil {
			return err
		}
		repetitionOf[scope]++
		result.intervals = append(result.intervals, interval)
		previous = current
		return nil
	}

	// The Connector is constructed once. Its construction Attestation is the
	// first relevant access after a fresh deployment, and it is the only
	// occurrence of that scope: reconstructing the Connector would open a new
	// pool against a server that is no longer at first access.
	var connector *dataconnector.Connector
	if err := bracket(experiment.AttestationScopeConstructorOrColdPool, "connector-construction", func() error {
		opened, err := dataconnector.New(ctx, dataconnector.Config{
			DSN: dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 10 * time.Second,
			MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected,
			ExpectedAttestation: attestation,
		})
		if err != nil {
			return fmt.Errorf("open connector: %w", err)
		}
		connector = opened
		return nil
	}); err != nil {
		return result, err
	}
	defer connector.Close()

	for repetition := 0; repetition < repetitions; repetition++ {
		if err := bracket(experiment.AttestationScopeExplicitPreflightPool,
			fmt.Sprintf("explicit-preflight-%d", repetition), func() error {
				_, err := connector.Attestation(ctx)
				return err
			}); err != nil {
			return result, err
		}
		if err := bracket(experiment.AttestationScopeSingleQueryTransaction,
			fmt.Sprintf("single-query-%d", repetition), func() error {
				_, err := connector.Query(ctx, dataconnector.QueryRequest{
					SQL: probeSQL, MaxRows: 1, StatementTimeout: 30 * time.Second,
				})
				return err
			}); err != nil {
			return result, err
		}
		if err := bracket(experiment.AttestationScopePairedQueryTransaction,
			fmt.Sprintf("paired-query-%d", repetition), func() error {
				_, err := connector.QueryPairStream(ctx, dataconnector.QueryPairStreamRequest{
					Visible:        dataconnector.QueryRequest{SQL: probeSQL, MaxRows: 1, StatementTimeout: 30 * time.Second},
					Provenance:     dataconnector.QueryRequest{SQL: probeSQL, MaxRows: 1, StatementTimeout: 30 * time.Second},
					ProvenanceSink: discardSink{},
				})
				return err
			}); err != nil {
			return result, err
		}
	}
	return result, nil
}

// probeSQL is a constant with no relation dependency, so the measured target
// statements cannot vary with the deployed data.
const probeSQL = `SELECT 1::bigint AS probe`

type discardSink struct{}

func (discardSink) Begin(context.Context, []dataconnector.Column) error { return nil }
func (discardSink) Row(context.Context, []any) error                    { return nil }

// deltaBetween computes one interval and binds every invariant it depends on.
func deltaBetween(from, to snapshot, scope experiment.AttestationScope, repetition int) (measuredInterval, error) {
	interval := measuredInterval{
		Index: to.Index - 1, Scope: scope, Repetition: repetition,
		FromLabel: from.Label, ToLabel: to.Label, Attestations: 1,
	}
	// A reset inside the window would make the cumulative difference negative or
	// nonsensical; an eviction would silently drop counted statements; a restart
	// would discard the whole view. Each invalidates the interval outright.
	if from.StatsReset != to.StatsReset {
		return interval, fmt.Errorf("interval %s->%s: pg_stat_statements was reset inside the window",
			from.Label, to.Label)
	}
	if from.Dealloc != to.Dealloc || to.Dealloc != 0 {
		return interval, fmt.Errorf("interval %s->%s: pg_stat_statements evicted entries (dealloc %d->%d)",
			from.Label, to.Label, from.Dealloc, to.Dealloc)
	}
	if from.PostmasterStartTime != to.PostmasterStartTime {
		return interval, fmt.Errorf("interval %s->%s: PostgreSQL restarted inside the window",
			from.Label, to.Label)
	}
	if from.Environment != to.Environment {
		return interval, fmt.Errorf("interval %s->%s: the measurement environment changed inside the window",
			from.Label, to.Label)
	}
	if err := to.Environment.Validate(); err != nil {
		return interval, fmt.Errorf("interval %s->%s: %w", from.Label, to.Label, err)
	}

	for key, calls := range to.calls {
		delta := calls - from.calls[key]
		if delta < 0 {
			return interval, fmt.Errorf("interval %s->%s: key %s went backwards by %d",
				from.Label, to.Label, key.StrictASTSHA256[:12], -delta)
		}
		if delta == 0 {
			continue
		}
		entry := structuralEntry{structuralKey: key, Calls: delta, QueryID: to.queryIDs[key]}
		interval.StructuralSum += delta
		if key.TopLevel {
			interval.TopLevel = append(interval.TopLevel, entry)
		} else {
			interval.Internal = append(interval.Internal, entry)
		}
	}
	// A key present before and absent after can only mean the view lost rows.
	for key, calls := range from.calls {
		if _, present := to.calls[key]; !present && calls > 0 {
			return interval, fmt.Errorf("interval %s->%s: key %s disappeared from pg_stat_statements",
				from.Label, to.Label, key.StrictASTSHA256[:12])
		}
	}
	interval.TotalDelta = to.Total - from.Total
	if interval.StructuralSum != interval.TotalDelta {
		return interval, fmt.Errorf("interval %s->%s: total delta %d does not equal the structural sum %d; "+
			"a call was counted outside the classified rows",
			from.Label, to.Label, interval.TotalDelta, interval.StructuralSum)
	}
	sortEntries(interval.TopLevel)
	sortEntries(interval.Internal)
	return interval, nil
}

// agreedFootprint returns the single per-Attestation internal footprint every
// interval of one scope produced.
//
// Agreement is the exact multiset of structural keys and multiplicities. A union
// across repetitions would hide instability, which is the failure this whole
// qualification exists to detect.
func agreedFootprint(intervals []measuredInterval, scope experiment.AttestationScope) (
	[]experiment.AttestationInternalEntry, bool, int, error) {
	var reference string
	var entries []experiment.AttestationInternalEntry
	count := 0
	for _, interval := range intervals {
		if interval.Scope != scope {
			continue
		}
		count++
		signature := internalSignature(interval)
		if count == 1 {
			reference = signature
			for _, entry := range interval.Internal {
				entries = append(entries, experiment.AttestationInternalEntry{
					StrictASTSHA256: entry.StrictASTSHA256, CallsPerAttestation: entry.Calls,
				})
			}
			continue
		}
		if signature != reference {
			return nil, false, count, nil
		}
	}
	if count == 0 {
		return nil, false, 0, fmt.Errorf("no interval measured scope %s", scope)
	}
	return entries, true, count, nil
}

func internalSignature(interval measuredInterval) string {
	parts := make([]string, 0, len(interval.Internal))
	for _, entry := range interval.Internal {
		parts = append(parts, fmt.Sprintf("%s:%d", entry.StrictASTSHA256, entry.Calls))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func sortEntries(entries []structuralEntry) {
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].StrictASTSHA256 != entries[right].StrictASTSHA256 {
			return entries[left].StrictASTSHA256 < entries[right].StrictASTSHA256
		}
		return !entries[left].TopLevel && entries[right].TopLevel
	})
}

// readSnapshot takes one cumulative census of gateway_reader plus the invariants
// every interval binds. Query text is digested in process and discarded.
func readSnapshot(ctx context.Context, admin *pgx.Conn) (snapshot, error) {
	var taken snapshot
	taken.calls = map[structuralKey]int64{}
	taken.queryIDs = map[structuralKey]string{}

	var versionNum string
	if err := admin.QueryRow(ctx, `
SELECT current_setting('server_version_num'), current_setting('pg_stat_statements.track'),
       current_setting('pg_stat_statements.track_utility'), current_setting('pg_stat_statements.track_planning'),
       (SELECT stats_reset::text FROM public.pg_stat_statements_info),
       (SELECT dealloc FROM public.pg_stat_statements_info),
       pg_postmaster_start_time()::text`).
		Scan(&versionNum, &taken.Environment.Track, &taken.Environment.TrackUtility,
			&taken.Environment.TrackPlanning, &taken.StatsReset, &taken.Dealloc,
			&taken.PostmasterStartTime); err != nil {
		return taken, err
	}
	if _, err := fmt.Sscanf(versionNum, "%d", &taken.Environment.PostgreSQLVersionNum); err != nil {
		return taken, err
	}

	rows, err := admin.Query(ctx, `
SELECT s.query, s.toplevel, s.calls, s.queryid::text
FROM public.pg_stat_statements s
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND s.userid = (SELECT oid FROM pg_roles WHERE rolname = 'gateway_reader')`)
	if err != nil {
		return taken, err
	}
	defer rows.Close()
	for rows.Next() {
		var text, queryID string
		var topLevel bool
		var calls int64
		if err := rows.Scan(&text, &topLevel, &calls, &queryID); err != nil {
			return taken, err
		}
		digest, digestErr := experiment.StrictASTDigest(text)
		if digestErr != nil {
			// Never echo the statement: report a safe code and the local
			// queryid so the operator can look it up themselves.
			return taken, fmt.Errorf("strict AST digest failed for queryid %s (statement withheld)", queryID)
		}
		key := structuralKey{StrictASTSHA256: digest, TopLevel: topLevel}
		// Two pg_stat_statements rows can normalize to one structural key; their
		// calls are summed rather than one silently replacing the other.
		taken.calls[key] += calls
		taken.queryIDs[key] = queryID
		taken.Total += calls
		taken.PgStatStatementsRows++
	}
	return taken, rows.Err()
}

// verifyLiveSchema confirms every Catalog entry exists in the deployed database
// with the same ordered column names and types.
//
// It deliberately does not compare collation: the live catalog read that would
// be needed is exactly the reconstruction this probe refuses to treat as an
// identity, and the Catalog remains the authority either way.
func verifyLiveSchema(ctx context.Context, admin *pgx.Conn, entries []dataconnector.ViewSchema) error {
	for _, entry := range entries {
		rows, err := admin.Query(ctx, `
SELECT attr.attname, format_type(attr.atttypid, NULL)
FROM pg_namespace ns
JOIN pg_class cls ON cls.relnamespace = ns.oid
JOIN pg_attribute attr ON attr.attrelid = cls.oid AND attr.attnum > 0 AND NOT attr.attisdropped
WHERE ns.nspname = $1 AND cls.relname = $2
ORDER BY attr.attnum`, entry.Schema, entry.View)
		if err != nil {
			return err
		}
		var live []dataconnector.SchemaColumn
		for rows.Next() {
			var column dataconnector.SchemaColumn
			if err := rows.Scan(&column.Name, &column.PostgreSQLType); err != nil {
				rows.Close()
				return err
			}
			live = append(live, column)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(live) != len(entry.Columns) {
			return fmt.Errorf("%s.%s: Catalog describes %d columns, the deployed relation has %d",
				entry.Schema, entry.View, len(entry.Columns), len(live))
		}
		for index, column := range entry.Columns {
			if live[index].Name != column.Name || live[index].PostgreSQLType != column.PostgreSQLType {
				return fmt.Errorf("%s.%s column %d: Catalog describes %s %s, the deployed relation has %s %s",
					entry.Schema, entry.View, index+1, column.Name, column.PostgreSQLType,
					live[index].Name, live[index].PostgreSQLType)
			}
		}
	}
	return nil
}
