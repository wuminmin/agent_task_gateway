// Command final-v5-attestation-footprint is the Stage N1 diagnosis probe.
//
// It measures the PostgreSQL-internal (toplevel=false) statements one complete
// datasource/schema Attestation causes under the frozen deployment, separately
// for the two scopes the Connector actually uses:
//
//	preflight    Connector.Attestation, against the pool, outside any transaction
//	transaction  the attestation Connector.Query performs inside its transaction
//
// The two are measured separately on purpose. They call the same Go function,
// but that is not evidence that PostgreSQL produces the same internal footprint
// for both: transaction scope and plan caching are properties of the server, not
// of the caller.
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
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

type structuralEntry struct {
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	TopLevel        bool   `json:"toplevel"`
	Calls           int64  `json:"calls"`
	QueryID         string `json:"queryid"`
}

type trial struct {
	Scope        string            `json:"scope"`
	RelationKind string            `json:"relation_kind"`
	Warmth       string            `json:"warmth"`
	Repetition   int               `json:"repetition"`
	TopLevel     []structuralEntry `json:"toplevel_entries"`
	Internal     []structuralEntry `json:"internal_entries"`
	TotalDelta   int64             `json:"total_delta"`
	// AttestationsPerTrial is how many complete Attestations the trial actually
	// performed. dataconnector.New attests once at startup, so every trial
	// contains that one plus the scope under test; the per-attestation
	// multiplicity is the trial count divided by this.
	AttestationsPerTrial   int64             `json:"attestations_per_trial"`
	InternalPerAttestation []structuralEntry `json:"internal_per_attestation"`
}

type report struct {
	SchemaVersion              int                               `json:"schema_version"`
	Record                     string                            `json:"record"`
	DiagnosisID                string                            `json:"diagnosis_id"`
	PublicationEligible        bool                              `json:"publication_eligible"`
	CapabilityChanging         bool                              `json:"capability_changing"`
	ActivationSupportChanging  bool                              `json:"activation_support_changing"`
	FormalCampaign             bool                              `json:"formal_campaign"`
	MeasurementEnvironment     experiment.MeasurementEnvironment `json:"measurement_environment"`
	StatsReset                 string                            `json:"stats_reset"`
	Dealloc                    int64                             `json:"dealloc"`
	ExpectedSchemaDigest       string                            `json:"expected_schema_digest"`
	QueryIDPortabilityCaveat   string                            `json:"queryid_portability_caveat"`
	Trials                     []trial                           `json:"trials"`
	PreflightFootprintStable   bool                              `json:"preflight_footprint_stable"`
	TransactionFootprintStable bool                              `json:"transaction_footprint_stable"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-attestation-footprint:", err)
		os.Exit(1)
	}
}

func run() error {
	var dsn, adminDSN, out, plainView, matView, datasourceID, database, reader string
	var majorVersion int
	flag.StringVar(&dsn, "gateway-reader-dsn", "", "gateway_reader DSN for the Connector under test")
	flag.StringVar(&adminDSN, "admin-dsn", "", "superuser DSN used only to read and reset pg_stat_statements")
	flag.StringVar(&out, "out", "", "diagnosis report path")
	flag.StringVar(&plainView, "plain-view", "", "schema.view of a plain view")
	flag.StringVar(&matView, "materialized-view", "", "schema.view of a materialized relation")
	flag.StringVar(&datasourceID, "datasource-id", "", "expected datasource identity")
	flag.StringVar(&database, "database", "travel_demo", "expected database")
	flag.StringVar(&reader, "reader-role", "gateway_reader", "expected role")
	flag.IntVar(&majorVersion, "postgresql-major", 16, "expected PostgreSQL major version")
	flag.Parse()
	if dsn == "" || adminDSN == "" || out == "" || plainView == "" || matView == "" {
		return errors.New("gateway-reader-dsn, admin-dsn, out, plain-view and materialized-view are required")
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("connect admin: %w", err)
	}
	defer admin.Close(context.Background())

	environment, statsReset, dealloc, err := readEnvironment(ctx, admin)
	if err != nil {
		return err
	}
	if err := environment.Validate(); err != nil {
		return err
	}

	document := report{
		SchemaVersion: 1, Record: "taskgate-final-v5-attestation-footprint-diagnosis-v1",
		DiagnosisID:         strings.TrimSpace(os.Getenv("DIAGNOSIS_ID")),
		PublicationEligible: false, CapabilityChanging: false,
		ActivationSupportChanging: false, FormalCampaign: false,
		MeasurementEnvironment: environment, StatsReset: statsReset, Dealloc: dealloc,
		QueryIDPortabilityCaveat: "queryid is PostgreSQL-version and installation specific; " +
			"it is recorded for deployment-local diagnosis only and is not a portable identity",
	}

	// The complete expected attestation is required: a zero value makes
	// liveIdentity return early without issuing the datasource identity read,
	// which would measure a footprint production never produces.
	attestation := dataconnector.ExpectedAttestation{
		DatasourceID: datasourceID, Database: database, User: reader,
		PostgreSQLMajorVersion: majorVersion,
	}

	// The E=2 case decides whether the internal footprint is per Attestation or
	// per ExpectedSchema entry. At E=1 the two are indistinguishable, and the
	// whole N3 multiplicity rule turns on which it is.
	multiView, err := buildExpected(ctx, admin, []string{plainView, matView})
	if err != nil {
		return err
	}
	for _, scope := range []string{"preflight", "transaction"} {
		measured, err := measure(ctx, admin, dsn, attestation, multiView, scope)
		if err != nil {
			return fmt.Errorf("two_entry/%s: %w", scope, err)
		}
		measured.Scope, measured.RelationKind = scope, "two_entry_expected_schema"
		measured.Warmth, measured.Repetition = "warm", 0
		measured.AttestationsPerTrial = 2
		for _, entry := range measured.Internal {
			measured.InternalPerAttestation = append(measured.InternalPerAttestation, structuralEntry{
				StrictASTSHA256: entry.StrictASTSHA256, TopLevel: entry.TopLevel,
				Calls: entry.Calls / measured.AttestationsPerTrial, QueryID: entry.QueryID})
		}
		document.Trials = append(document.Trials, measured)
	}

	// Cold means the first invocation after a fresh deployment; warm repetitions
	// follow immediately. Both are measured because a footprint that changed
	// between them would make any qualified value meaningless.
	for _, relation := range []struct{ kind, name string }{
		{"plain_view", plainView}, {"materialized_view", matView},
	} {
		schema, view, ok := strings.Cut(relation.name, ".")
		if !ok {
			return fmt.Errorf("relation %q is not schema.view", relation.name)
		}
		columns, err := readColumns(ctx, admin, schema, view)
		if err != nil {
			return err
		}
		expected := []dataconnector.ViewSchema{{Schema: schema, View: view, Columns: columns}}
		for repetition := 0; repetition < 3; repetition++ {
			warmth := "warm"
			if repetition == 0 {
				warmth = "cold"
			}
			for _, scope := range []string{"preflight", "transaction"} {
				measured, err := measure(ctx, admin, dsn, attestation, expected, scope)
				if err != nil {
					return fmt.Errorf("%s/%s/%s: %w", relation.kind, scope, warmth, err)
				}
				measured.Scope, measured.RelationKind = scope, relation.kind
				measured.Warmth, measured.Repetition = warmth, repetition
				// One Attestation from dataconnector.New, one from the scope
				// under test.
				measured.AttestationsPerTrial = 2
				for _, entry := range measured.Internal {
					if entry.Calls%measured.AttestationsPerTrial != 0 {
						return fmt.Errorf("%s/%s/%s: internal key %s observed %d calls across %d attestations, "+
							"which is not a whole multiplicity", relation.kind, scope, warmth,
							entry.StrictASTSHA256, entry.Calls, measured.AttestationsPerTrial)
					}
					measured.InternalPerAttestation = append(measured.InternalPerAttestation, structuralEntry{
						StrictASTSHA256: entry.StrictASTSHA256, TopLevel: entry.TopLevel,
						Calls: entry.Calls / measured.AttestationsPerTrial, QueryID: entry.QueryID,
					})
				}
				document.Trials = append(document.Trials, measured)
			}
		}
	}

	document.PreflightFootprintStable = stable(document.Trials, "preflight")
	document.TransactionFootprintStable = stable(document.Trials, "transaction")

	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("attestation footprint diagnosis: %d trials, preflight stable=%t, transaction stable=%t\n",
		len(document.Trials), document.PreflightFootprintStable, document.TransactionFootprintStable)
	return nil
}

// measure resets pg_stat_statements, performs exactly one Attestation in the
// requested scope through the production Connector, and returns the structural
// delta split by toplevel.
func measure(ctx context.Context, admin *pgx.Conn, dsn string, attestation dataconnector.ExpectedAttestation,
	expected []dataconnector.ViewSchema, scope string) (trial, error) {
	if _, err := admin.Exec(ctx, `SELECT public.pg_stat_statements_reset()`); err != nil {
		return trial{}, fmt.Errorf("reset pg_stat_statements: %w", err)
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 10 * time.Second,
		MaxRows: 10, MaxConnections: 1, ExpectedSchema: expected,
		ExpectedAttestation: attestation,
	})
	if err != nil {
		return trial{}, fmt.Errorf("open connector: %w", err)
	}
	defer connector.Close()

	switch scope {
	case "preflight":
		// Against the pool, outside any transaction.
		if _, err := connector.Attestation(ctx); err != nil {
			return trial{}, fmt.Errorf("preflight attestation: %w", err)
		}
	case "transaction":
		// Connector.Query performs the attestation inside its own governed
		// transaction, which is the scope the measured operations use.
		if _, err := connector.Query(ctx, dataconnector.QueryRequest{
			SQL: `SELECT 1::bigint AS probe`, MaxRows: 1, StatementTimeout: 30 * time.Second,
		}); err != nil {
			return trial{}, fmt.Errorf("transactional attestation: %w", err)
		}
	default:
		return trial{}, fmt.Errorf("unknown scope %q", scope)
	}
	return readDelta(ctx, admin)
}

// readDelta classifies the whole gateway_reader reading. Query text is digested
// in process and discarded; nothing textual is returned.
func readDelta(ctx context.Context, admin *pgx.Conn) (trial, error) {
	rows, err := admin.Query(ctx, `
SELECT s.query, s.toplevel, s.calls, s.queryid::text
FROM public.pg_stat_statements s
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND s.userid = (SELECT oid FROM pg_roles WHERE rolname = 'gateway_reader')`)
	if err != nil {
		return trial{}, err
	}
	defer rows.Close()
	var measured trial
	for rows.Next() {
		var text, queryID string
		var topLevel bool
		var calls int64
		if err := rows.Scan(&text, &topLevel, &calls, &queryID); err != nil {
			return trial{}, err
		}
		digest, digestErr := experiment.StrictASTDigest(text)
		if digestErr != nil {
			// Never echo the statement: report a safe code and the local
			// queryid so the operator can look it up themselves.
			return trial{}, fmt.Errorf("strict AST digest failed for queryid %s (statement withheld)", queryID)
		}
		entry := structuralEntry{StrictASTSHA256: digest, TopLevel: topLevel, Calls: calls, QueryID: queryID}
		measured.TotalDelta += calls
		if topLevel {
			measured.TopLevel = append(measured.TopLevel, entry)
		} else {
			measured.Internal = append(measured.Internal, entry)
		}
	}
	if err := rows.Err(); err != nil {
		return trial{}, err
	}
	sortEntries(measured.TopLevel)
	sortEntries(measured.Internal)
	return measured, nil
}

func sortEntries(entries []structuralEntry) {
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].StrictASTSHA256 < entries[right].StrictASTSHA256
	})
}

// stable reports whether every trial of one scope produced the same internal
// footprint. Deliberately compares the exact multiset of structural keys and
// multiplicities -- a union across repetitions would hide instability, which is
// the failure this whole diagnosis exists to detect.
func stable(trials []trial, scope string) bool {
	var reference string
	for _, measured := range trials {
		if measured.Scope != scope || measured.RelationKind == "two_entry_expected_schema" {
			continue
		}
		signature := internalSignature(measured)
		if reference == "" {
			reference = signature
			continue
		}
		if signature != reference {
			return false
		}
	}
	return reference != ""
}

func internalSignature(measured trial) string {
	parts := make([]string, 0, len(measured.InternalPerAttestation))
	for _, entry := range measured.InternalPerAttestation {
		parts = append(parts, fmt.Sprintf("%s:%d", entry.StrictASTSHA256, entry.Calls))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func readEnvironment(ctx context.Context, admin *pgx.Conn) (experiment.MeasurementEnvironment, string, int64, error) {
	var environment experiment.MeasurementEnvironment
	var versionNum, statsReset string
	var dealloc int64
	if err := admin.QueryRow(ctx, `
SELECT current_setting('server_version_num'), current_setting('pg_stat_statements.track'),
       current_setting('pg_stat_statements.track_utility'), current_setting('pg_stat_statements.track_planning'),
       (SELECT stats_reset::text FROM public.pg_stat_statements_info),
       (SELECT dealloc FROM public.pg_stat_statements_info)`).
		Scan(&versionNum, &environment.Track, &environment.TrackUtility, &environment.TrackPlanning,
			&statsReset, &dealloc); err != nil {
		return environment, "", 0, err
	}
	if _, err := fmt.Sscanf(versionNum, "%d", &environment.PostgreSQLVersionNum); err != nil {
		return environment, "", 0, err
	}
	return environment, statsReset, dealloc, nil
}

// buildExpected assembles a multi-entry ExpectedSchema in the given order.
func buildExpected(ctx context.Context, admin *pgx.Conn, relations []string) ([]dataconnector.ViewSchema, error) {
	expected := make([]dataconnector.ViewSchema, 0, len(relations))
	for _, relation := range relations {
		schema, view, ok := strings.Cut(relation, ".")
		if !ok {
			return nil, fmt.Errorf("relation %q is not schema.view", relation)
		}
		columns, err := readColumns(ctx, admin, schema, view)
		if err != nil {
			return nil, err
		}
		expected = append(expected, dataconnector.ViewSchema{Schema: schema, View: view, Columns: columns})
	}
	return expected, nil
}

func readColumns(ctx context.Context, admin *pgx.Conn, schema, view string) ([]dataconnector.SchemaColumn, error) {
	rows, err := admin.Query(ctx, `
SELECT attr.attname, format_type(attr.atttypid, NULL)
FROM pg_namespace ns
JOIN pg_class cls ON cls.relnamespace = ns.oid
JOIN pg_attribute attr ON attr.attrelid = cls.oid AND attr.attnum > 0 AND NOT attr.attisdropped
WHERE ns.nspname = $1 AND cls.relname = $2
ORDER BY attr.attnum`, schema, view)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []dataconnector.SchemaColumn
	for rows.Next() {
		var column dataconnector.SchemaColumn
		if err := rows.Scan(&column.Name, &column.PostgreSQLType); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}
