// Command final-v5-attestation-footprint qualifies the PostgreSQL-internal
// Attestation footprint.
//
// It measures the internal (toplevel=false) statements one complete
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
// Stage N4 supersedes the Stage N1 shape. N1 measured the right property but
// could not carry it as a contract: it recorded no ExpectedSchema identity, kept
// the entry count in a free-text relation_kind label, and bound no PostgreSQL
// image. Every trial here records the ExpectedSchema digest -- computed by the
// same catalogschema.Digest the Gateway uses, so the identity is in the
// production space -- along with the entry count as an integer and the immutable
// image the measurement ran against.
//
// It emits one experiment.AttestationFootprintV1 per distinct ExpectedSchema,
// and refuses to emit any if a scope's trials disagree. A footprint is valid only
// for the ExpectedSchema, environment and image it names; nothing here scales one
// measurement to another schema.
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
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

const (
	scopePreflight   = "preflight"
	scopeTransaction = "transaction"
)

type structuralEntry struct {
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	TopLevel        bool   `json:"toplevel"`
	Calls           int64  `json:"calls"`
	QueryID         string `json:"queryid"`
}

type trial struct {
	Scope string `json:"scope"`
	// ExpectedSchemaDigest is the catalogschema identity of the schema attested
	// against. It is what makes this trial bindable to a production
	// ExpectedSchema, and its absence is why the Stage N1 record could not serve
	// as a contract.
	ExpectedSchemaDigest string `json:"expected_schema_digest"`
	// ExpectedSchemaEntries is E, recorded as an integer rather than implied by
	// a label.
	ExpectedSchemaEntries int64 `json:"expected_schema_entries"`
	// RelationKinds is one kind per ExpectedSchema entry, in entry order. It is
	// an independent dimension from the entry count; N1 conflated the two.
	RelationKinds []string          `json:"relation_kinds"`
	Warmth        string            `json:"warmth"`
	Repetition    int               `json:"repetition"`
	TopLevel      []structuralEntry `json:"toplevel_entries"`
	Internal      []structuralEntry `json:"internal_entries"`
	TotalDelta    int64             `json:"total_delta"`
	// AttestationsPerTrial is how many complete Attestations the trial actually
	// performed. dataconnector.New attests once at startup, so every trial
	// contains that one plus the scope under test; the per-attestation
	// multiplicity is the trial count divided by this.
	AttestationsPerTrial   int64             `json:"attestations_per_trial"`
	InternalPerAttestation []structuralEntry `json:"internal_per_attestation"`
}

// schemaStability records, per ExpectedSchema and scope, whether every trial
// produced the same internal footprint.
type schemaStability struct {
	ExpectedSchemaDigest  string `json:"expected_schema_digest"`
	ExpectedSchemaEntries int64  `json:"expected_schema_entries"`
	PreflightStable       bool   `json:"preflight_footprint_stable"`
	TransactionStable     bool   `json:"transaction_footprint_stable"`
	Trials                int    `json:"trials"`
}

type report struct {
	SchemaVersion             int                               `json:"schema_version"`
	Record                    string                            `json:"record"`
	DiagnosisID               string                            `json:"diagnosis_id"`
	PublicationEligible       bool                              `json:"publication_eligible"`
	CapabilityChanging        bool                              `json:"capability_changing"`
	ActivationSupportChanging bool                              `json:"activation_support_changing"`
	FormalCampaign            bool                              `json:"formal_campaign"`
	MeasurementEnvironment    experiment.MeasurementEnvironment `json:"measurement_environment"`
	PostgreSQLImageID         string                            `json:"postgresql_image_id"`
	StatsReset                string                            `json:"stats_reset"`
	Dealloc                   int64                             `json:"dealloc"`
	QueryIDPortabilityCaveat  string                            `json:"queryid_portability_caveat"`
	Trials                    []trial                           `json:"trials"`
	Stability                 []schemaStability                 `json:"stability"`
	// Footprints is the qualified output, one per distinct ExpectedSchema.
	Footprints []experiment.AttestationFootprintV1 `json:"footprints"`
	// FootprintDigests parallels Footprints so a consumer can pin one without
	// recomputing.
	FootprintDigests []string `json:"footprint_digests"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-attestation-footprint:", err)
		os.Exit(1)
	}
}

// schemaConfiguration is one ExpectedSchema under test.
type schemaConfiguration struct {
	relations []string
	kinds     []string
	entries   []dataconnector.ViewSchema
	digest    string
}

func run() error {
	var dsn, adminDSN, out, plainView, matView, datasourceID, database, reader, imageID string
	var majorVersion, repetitions int
	flag.StringVar(&dsn, "gateway-reader-dsn", "", "gateway_reader DSN for the Connector under test")
	flag.StringVar(&adminDSN, "admin-dsn", "", "superuser DSN used only to read and reset pg_stat_statements")
	flag.StringVar(&out, "out", "", "diagnosis report path")
	flag.StringVar(&plainView, "plain-view", "", "schema.view of a plain view")
	flag.StringVar(&matView, "materialized-view", "", "schema.view of a materialized relation")
	flag.StringVar(&datasourceID, "datasource-id", "", "expected datasource identity")
	flag.StringVar(&database, "database", "travel_demo", "expected database")
	flag.StringVar(&reader, "reader-role", "gateway_reader", "expected role")
	flag.StringVar(&imageID, "postgresql-image-id", "",
		"immutable sha256: image ID of the PostgreSQL container under test")
	flag.IntVar(&majorVersion, "postgresql-major", 16, "expected PostgreSQL major version")
	flag.IntVar(&repetitions, "repetitions", 3, "repetitions per configuration and scope; the first is cold")
	flag.Parse()
	if dsn == "" || adminDSN == "" || out == "" || plainView == "" || matView == "" {
		return errors.New("gateway-reader-dsn, admin-dsn, out, plain-view and materialized-view are required")
	}
	// The image binding is required rather than optional: a footprint that does
	// not name the server it was measured on cannot be re-checked against the
	// deployment that later consumes it.
	if err := requireImageID(imageID); err != nil {
		return err
	}
	if repetitions < 1 {
		return fmt.Errorf("repetitions must be at least 1, got %d", repetitions)
	}
	diagnosisID := strings.TrimSpace(os.Getenv("DIAGNOSIS_ID"))
	if diagnosisID == "" {
		return errors.New("DIAGNOSIS_ID must name this non-formal qualification run")
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
		SchemaVersion: 2, Record: "taskgate-final-v5-attestation-footprint-qualification-v2",
		DiagnosisID:         diagnosisID,
		PublicationEligible: false, CapabilityChanging: false,
		ActivationSupportChanging: false, FormalCampaign: false,
		MeasurementEnvironment: environment, PostgreSQLImageID: imageID,
		StatsReset: statsReset, Dealloc: dealloc,
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

	// Three configurations. The two single-entry ones separate relation kind
	// from entry count; the two-entry one decides whether the footprint is per
	// Attestation or per ExpectedSchema entry, which at E=1 is indistinguishable.
	// Every configuration runs the full cold/warm and both scopes, so no cell of
	// the matrix rests on a single observation.
	configurations, err := buildConfigurations(ctx, admin, plainView, matView)
	if err != nil {
		return err
	}

	for _, configuration := range configurations {
		for repetition := 0; repetition < repetitions; repetition++ {
			warmth := "warm"
			if repetition == 0 {
				warmth = "cold"
			}
			for _, scope := range []string{scopePreflight, scopeTransaction} {
				measured, err := measure(ctx, admin, dsn, attestation, configuration.entries, scope)
				if err != nil {
					return fmt.Errorf("%s/%s/%s: %w", strings.Join(configuration.kinds, "+"), scope, warmth, err)
				}
				measured.Scope = scope
				measured.ExpectedSchemaDigest = configuration.digest
				measured.ExpectedSchemaEntries = int64(len(configuration.entries))
				measured.RelationKinds = configuration.kinds
				measured.Warmth, measured.Repetition = warmth, repetition
				// One Attestation from dataconnector.New, one from the scope
				// under test.
				measured.AttestationsPerTrial = 2
				for _, entry := range measured.Internal {
					if entry.Calls%measured.AttestationsPerTrial != 0 {
						return fmt.Errorf("%s/%s/%s: internal key %s observed %d calls across %d attestations, "+
							"which is not a whole multiplicity", strings.Join(configuration.kinds, "+"), scope, warmth,
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

	for _, configuration := range configurations {
		stability, footprint, err := qualify(document.Trials, configuration, environment, imageID, diagnosisID)
		if err != nil {
			return err
		}
		document.Stability = append(document.Stability, stability)
		digest, err := footprint.SHA256()
		if err != nil {
			return err
		}
		document.Footprints = append(document.Footprints, footprint)
		document.FootprintDigests = append(document.FootprintDigests, digest)
	}

	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(payload, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("attestation footprint qualification: %d trials over %d ExpectedSchemas, %d footprints\n",
		len(document.Trials), len(configurations), len(document.Footprints))
	for index, footprint := range document.Footprints {
		preflight, _ := footprint.Scope(experiment.AttestationScopePreflight)
		transactional, _ := footprint.Scope(experiment.AttestationScopeTransactional)
		fmt.Printf("  E=%d schema=%s preflight=%d/attestation transactional=%d/attestation footprint=%s\n",
			footprint.ExpectedSchemaEntries, footprint.ExpectedSchemaDigest[:12],
			preflight.TotalCallsPerAttestation(), transactional.TotalCallsPerAttestation(),
			document.FootprintDigests[index][:12])
	}
	return nil
}

func requireImageID(imageID string) error {
	digest, found := strings.CutPrefix(imageID, "sha256:")
	if !found || len(digest) != 64 {
		return fmt.Errorf("postgresql-image-id must be an immutable sha256: identity, got %q", imageID)
	}
	for _, character := range digest {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return fmt.Errorf("postgresql-image-id must be an immutable sha256: identity, got %q", imageID)
		}
	}
	return nil
}

// buildConfigurations assembles the ExpectedSchemas under test, digesting each
// with the same function the Gateway uses.
func buildConfigurations(ctx context.Context, admin *pgx.Conn, plainView, matView string) ([]schemaConfiguration, error) {
	specifications := []struct {
		relations []string
		kinds     []string
	}{
		{[]string{plainView}, []string{"plain_view"}},
		{[]string{matView}, []string{"materialized_view"}},
		{[]string{plainView, matView}, []string{"plain_view", "materialized_view"}},
	}
	configurations := make([]schemaConfiguration, 0, len(specifications))
	for _, specification := range specifications {
		entries, err := buildExpected(ctx, admin, specification.relations)
		if err != nil {
			return nil, err
		}
		configurations = append(configurations, schemaConfiguration{
			relations: specification.relations, kinds: specification.kinds,
			entries: entries, digest: catalogschema.Digest(entries),
		})
	}
	// Two configurations sharing a digest would make their trials
	// indistinguishable when grouped, and the E=1/E=2 comparison meaningless.
	seen := map[string]bool{}
	for _, configuration := range configurations {
		if seen[configuration.digest] {
			return nil, fmt.Errorf("two configurations share ExpectedSchema digest %s", configuration.digest[:12])
		}
		seen[configuration.digest] = true
	}
	return configurations, nil
}

// qualify turns the trials for one ExpectedSchema into a footprint, refusing if
// any scope's trials disagree.
//
// Agreement is over the exact multiset of structural keys and multiplicities.
// A union across repetitions would hide instability, which is the failure this
// whole qualification exists to detect.
func qualify(trials []trial, configuration schemaConfiguration,
	environment experiment.MeasurementEnvironment, imageID, qualificationID string) (
	schemaStability, experiment.AttestationFootprintV1, error) {
	stability := schemaStability{
		ExpectedSchemaDigest:  configuration.digest,
		ExpectedSchemaEntries: int64(len(configuration.entries)),
	}
	measured := map[experiment.AttestationScope][]experiment.AttestationInternalEntry{}
	for _, pair := range []struct {
		scope  string
		mapped experiment.AttestationScope
		stable *bool
	}{
		{scopePreflight, experiment.AttestationScopePreflight, &stability.PreflightStable},
		{scopeTransaction, experiment.AttestationScopeTransactional, &stability.TransactionStable},
	} {
		entries, count, stable, err := agreedFootprint(trials, configuration.digest, pair.scope)
		if err != nil {
			return stability, experiment.AttestationFootprintV1{}, err
		}
		*pair.stable = stable
		stability.Trials += count
		if !stable {
			return stability, experiment.AttestationFootprintV1{}, fmt.Errorf(
				"ATTESTATION INTERNAL FOOTPRINT NOT STABLE: ExpectedSchema %s scope %s disagreed across %d trials",
				configuration.digest[:12], pair.scope, count)
		}
		measured[pair.mapped] = entries
	}
	footprint, err := experiment.NewAttestationFootprintV1(configuration.digest,
		int64(len(configuration.entries)), environment, imageID, qualificationID, measured)
	if err != nil {
		return stability, experiment.AttestationFootprintV1{}, err
	}
	return stability, footprint, nil
}

// agreedFootprint returns the single per-Attestation footprint every trial of one
// ExpectedSchema and scope produced.
func agreedFootprint(trials []trial, schemaDigest, scope string) (
	[]experiment.AttestationInternalEntry, int, bool, error) {
	var reference string
	var entries []experiment.AttestationInternalEntry
	count := 0
	for _, measured := range trials {
		if measured.Scope != scope || measured.ExpectedSchemaDigest != schemaDigest {
			continue
		}
		count++
		signature := internalSignature(measured)
		if count == 1 {
			reference = signature
			for _, entry := range measured.InternalPerAttestation {
				entries = append(entries, experiment.AttestationInternalEntry{
					StrictASTSHA256: entry.StrictASTSHA256, CallsPerAttestation: entry.Calls,
				})
			}
			continue
		}
		if signature != reference {
			return nil, count, false, nil
		}
	}
	if count == 0 {
		return nil, 0, false, fmt.Errorf("no trial measured ExpectedSchema %s in scope %s", schemaDigest[:12], scope)
	}
	return entries, count, true, nil
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
	case scopePreflight:
		// Against the pool, outside any transaction.
		if _, err := connector.Attestation(ctx); err != nil {
			return trial{}, fmt.Errorf("preflight attestation: %w", err)
		}
	case scopeTransaction:
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
