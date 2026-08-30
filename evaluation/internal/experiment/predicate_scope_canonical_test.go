package experiment

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
)

const (
	run04CompactScope = `{"category":["alpha","beta","delta","gamma"]}`
	run04JSONBScope   = `{"category": ["alpha", "beta", "delta", "gamma"]}`

	run04GrantSHA256 = "646a66b82e4e2196481198a64583925acdd1a866d9710835cf2be7aa27602163"
	// Re-pinned 2026-08-29: the only change to the result-heavy profile Catalog
	// since the 2026-08-10 pin is 900fc96 (v1.11 release-freeze reprojection of
	// the default route budget summary-manual-v5 -> final-v5-baseline-low-v1),
	// which moves the Catalog view these inputs embed. The grant digest and the
	// predicate footprint below are unchanged, and the compact/JSONB and
	// source/retained assertions were re-run under the new digest before it was
	// pinned.
	run04PreparationInputsSHA256  = "a10c985473bbc60a636e8ebb271414121536792567df4a344c9a6b3c13fd31f7"
	run04PredicateFootprintSHA256 = "09d696f60327552c01da5a43d240059de493b3a21baae932e341a8bbdd6fb840"
	run04SnapshotRowCount         = uint64(100000)
)

// run04ArtifactInputs rebuilds the exact operation inputs used by P3.3 run 04.
// The operation is lowered from the frozen contract, while the snapshot binding
// takes every identity from the source-controlled Catalog and the row count from
// the immutable run-04 publication bundle. The two historical input digests
// below make accidental fixture drift fail before it can weaken the hard gate.
func run04ArtifactInputs(t *testing.T, scope json.RawMessage) physicalquery.PreparationInputs {
	t.Helper()
	contracts := deploymentContractsForTest(t)
	candidates, err := contracts.ResolveCandidates(FrozenContractSelectorV3{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
		WorkloadID:   finalv5contracts.ArtifactWorkloadID,
		Scale:        "100x4",
		Mode:         "novel",
	})
	if err != nil {
		t.Fatalf("resolve run-04 operation: %v", err)
	}
	if len(candidates) != 1 || candidates[0].OperationID != "artifact/result-heavy/100x4/novel" {
		t.Fatalf("run-04 selector resolved %+v", candidates)
	}
	candidate := candidates[0]
	if string(candidate.Grant.MandatoryScope) != run04CompactScope {
		t.Fatalf("frozen run-04 scope = %s, want %s", candidate.Grant.MandatoryScope, run04CompactScope)
	}
	view, err := physicalquery.CatalogViewFromCatalog(*contracts.catalog)
	if err != nil {
		t.Fatalf("build run-04 Catalog view: %v", err)
	}
	if len(view.SnapshotPublications) != 1 {
		t.Fatalf("run-04 Catalog contains %d snapshot publications, want 1", len(view.SnapshotPublications))
	}
	publication := view.SnapshotPublications[0]
	binding := physicalquery.SnapshotBinding{
		PublicationName:  publication.Name,
		DictionaryDigest: publication.DictionaryDigest,
		ManifestDigest:   publication.ManifestDigest,
		SidecarDigest:    publication.SidecarDigest,
		SourceNamespace:  publication.SourceNamespace,
		Snapshot:         publication.Snapshot,
		OrdinalSidecar:   publication.OrdinalSidecar,
		RowCount:         run04SnapshotRowCount,
	}
	candidate.Grant.MandatoryScope = append(json.RawMessage(nil), scope...)
	inputs := physicalquery.PreparationInputs{
		Plan: candidate.Plan, Grant: candidate.Grant, Catalog: view,
		SnapshotBindings: map[string]physicalquery.SnapshotBinding{publication.Name: binding},
	}
	grantSHA256, err := inputs.Grant.SHA256()
	if err != nil {
		t.Fatalf("digest run-04 grant: %v", err)
	}
	if grantSHA256 != run04GrantSHA256 {
		t.Fatalf("run-04 grant digest = %s, want %s", grantSHA256, run04GrantSHA256)
	}
	inputsSHA256, err := inputs.SHA256()
	if err != nil {
		t.Fatalf("digest run-04 preparation inputs: %v", err)
	}
	if inputsSHA256 != run04PreparationInputsSHA256 {
		t.Fatalf("run-04 preparation inputs digest = %s, want %s", inputsSHA256, run04PreparationInputsSHA256)
	}
	return inputs
}

func prepareRun04Artifact(t *testing.T, scope json.RawMessage) (physicalquery.PreparedOperation, *queryplan.PredicateFootprint) {
	t.Helper()
	prepared, err := physicalquery.Prepare(run04ArtifactInputs(t, scope))
	if err != nil {
		t.Fatalf("prepare run-04 operation: %v", err)
	}
	footprint, err := prepared.PredicateFootprint()
	if err != nil || footprint == nil {
		t.Fatalf("read run-04 predicate footprint: %v / %#v", err, footprint)
	}
	return prepared, footprint
}

func requireRun04SemanticPreimage(t *testing.T, footprint *queryplan.PredicateFootprint) {
	t.Helper()
	if footprint.Version != queryplan.PredicateFootprintVersion || footprint.RawLiteralCount != 5 ||
		footprint.UniqueAtomCount != 5 || footprint.DuplicateCount != 0 || footprint.NullAtomCount != 0 {
		t.Fatalf("run-04 structured footprint header = %+v", footprint)
	}
	expected := []exposure.FactID{
		{Profile: exposure.ProfileV5, Kind: exposure.FactPredicateAtom,
			AtomizerVersion: exposure.PredicateFootprintVersion, SemanticProductID: "final_v5_result_heavy",
			StableRole: "final_v5_result_heavy", PublicFieldID: "category", SQLType: "text",
			CollationName: "en_US.utf8", CollationVersion: "2.36", Operator: "EQ", CanonicalLiteral: "s:alpha"},
		{Profile: exposure.ProfileV5, Kind: exposure.FactPredicateAtom,
			AtomizerVersion: exposure.PredicateFootprintVersion, SemanticProductID: "final_v5_result_heavy",
			StableRole: "final_v5_result_heavy", PublicFieldID: "category", SQLType: "text",
			CollationName: "en_US.utf8", CollationVersion: "2.36", Operator: "EQ", CanonicalLiteral: "s:beta"},
		{Profile: exposure.ProfileV5, Kind: exposure.FactPredicateAtom,
			AtomizerVersion: exposure.PredicateFootprintVersion, SemanticProductID: "final_v5_result_heavy",
			StableRole: "final_v5_result_heavy", PublicFieldID: "category", SQLType: "text",
			CollationName: "en_US.utf8", CollationVersion: "2.36", Operator: "EQ", CanonicalLiteral: "s:delta"},
		{Profile: exposure.ProfileV5, Kind: exposure.FactPredicateAtom,
			AtomizerVersion: exposure.PredicateFootprintVersion, SemanticProductID: "final_v5_result_heavy",
			StableRole: "final_v5_result_heavy", PublicFieldID: "category", SQLType: "text",
			CollationName: "en_US.utf8", CollationVersion: "2.36", Operator: "EQ", CanonicalLiteral: "s:gamma"},
		{Profile: exposure.ProfileV5, Kind: exposure.FactPredicateAtom,
			AtomizerVersion: exposure.PredicateFootprintVersion, SemanticProductID: "final_v5_result_heavy",
			StableRole: "final_v5_result_heavy", PublicFieldID: "row_id", SQLType: "bigint",
			Operator: "LE", CanonicalLiteral: "i:100"},
	}
	actual := append([]exposure.FactID(nil), footprint.Atoms...)
	for index := range actual {
		if actual[index].PredicateContextSHA256 != footprint.ContextSHA256 {
			t.Fatalf("atom %d context = %s, footprint context = %s", index,
				actual[index].PredicateContextSHA256, footprint.ContextSHA256)
		}
		actual[index].PredicateContextSHA256 = ""
	}
	less := func(values []exposure.FactID) func(int, int) bool {
		return func(left, right int) bool {
			leftKey := values[left].PublicFieldID + "\x00" + values[left].Operator + "\x00" + values[left].CanonicalLiteral
			rightKey := values[right].PublicFieldID + "\x00" + values[right].Operator + "\x00" + values[right].CanonicalLiteral
			return leftKey < rightKey
		}
	}
	sort.Slice(actual, less(actual))
	sort.Slice(expected, less(expected))
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("run-04 semantic atoms changed\nactual:   %#v\nexpected: %#v", actual, expected)
	}
}

func requireRun04BindingsSame(t *testing.T, left, right physicalquery.PreparedOperation) {
	t.Helper()
	leftBinding, rightBinding := left.Binding(), right.Binding()
	if !reflect.DeepEqual(leftBinding, rightBinding) {
		t.Fatalf("scope spellings produced different full prepared bindings\nleft:  %+v\nright: %+v",
			leftBinding, rightBinding)
	}
	if err := leftBinding.RequireSame(rightBinding); err != nil {
		t.Fatalf("left-to-right full prepared binding comparison: %v", err)
	}
	if err := rightBinding.RequireSame(leftBinding); err != nil {
		t.Fatalf("right-to-left full prepared binding comparison: %v", err)
	}
	leftExecution := querybinding.QueryExecutionBindingV2{PreparedOperation: leftBinding}
	rightExecution := querybinding.QueryExecutionBindingV2{PreparedOperation: rightBinding}
	if err := leftExecution.RequirePreparedSame(rightBinding); err != nil {
		t.Fatalf("left-to-right finalizer comparison: %v", err)
	}
	if err := rightExecution.RequirePreparedSame(leftBinding); err != nil {
		t.Fatalf("right-to-left finalizer comparison: %v", err)
	}
}

func TestRun04PreparedBindingIsIndependentOfScopeJSONEncoding(t *testing.T) {
	compact, compactFootprint := prepareRun04Artifact(t, json.RawMessage(run04CompactScope))
	jsonb, jsonbFootprint := prepareRun04Artifact(t, json.RawMessage(run04JSONBScope))

	requireRun04BindingsSame(t, compact, jsonb)
	if !reflect.DeepEqual(compactFootprint, jsonbFootprint) {
		t.Fatalf("scope spellings produced different structured footprints\ncompact: %+v\njsonb:   %+v",
			compactFootprint, jsonbFootprint)
	}
	requireRun04SemanticPreimage(t, compactFootprint)
	requireRun04SemanticPreimage(t, jsonbFootprint)
	if compact.Binding().PredicateFootprintSHA256 == run04PredicateFootprintSHA256 {
		t.Fatalf("post-fix footprint retained pre-fix digest %s", run04PredicateFootprintSHA256)
	}
	t.Logf("post-fix full binding=%s footprint=%s context=%s predicate-set=%s",
		compact.Binding().SHA256, compact.Binding().PredicateFootprintSHA256,
		compactFootprint.ContextSHA256, compactFootprint.AtomSetSHA256)
}

func TestRun04PreparedBindingMatchesPostgreSQLJSONBRoundTrip(t *testing.T) {
	dsn := os.Getenv("CONTROL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CONTROL_TEST_POSTGRES_DSN is required for the run-04 JSONB round-trip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to control PostgreSQL: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	var version int
	var roundTrip string
	if err := connection.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::integer, ($1::text)::jsonb::text`,
		run04CompactScope).Scan(&version, &roundTrip); err != nil {
		t.Fatalf("round-trip run-04 scope through PostgreSQL JSONB: %v", err)
	}
	if version != 160014 {
		t.Fatalf("PostgreSQL server_version_num = %d, want 160014", version)
	}
	if roundTrip != run04JSONBScope || roundTrip == run04CompactScope {
		t.Fatalf("PostgreSQL JSONB rendering = %q, want %q and byte-different from compact", roundTrip, run04JSONBScope)
	}
	compact, compactFootprint := prepareRun04Artifact(t, json.RawMessage(run04CompactScope))
	postgres, postgresFootprint := prepareRun04Artifact(t, json.RawMessage(roundTrip))
	requireRun04BindingsSame(t, compact, postgres)
	if !reflect.DeepEqual(compactFootprint, postgresFootprint) {
		t.Fatal("PostgreSQL JSONB round-trip changed the structured predicate footprint")
	}
}

// The retained statements are regression fixtures transcribed from the run-05
// diagnosis pg_stat_statements capture. They are not new evidence. The source
// side is rebuilt from the exact frozen 100x4 operation, then passed through the
// same Derive call the Gateway uses to sign its visible and companion targets.
func TestRun05SourceAndRetainedStructuralIdentitiesAgree(t *testing.T) {
	prepared, err := physicalquery.Prepare(run04ArtifactInputs(t, json.RawMessage(run04CompactScope)))
	if err != nil {
		t.Fatal(err)
	}
	visible, companion, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatal(err)
	}
	derived, err := physicalquery.Derive(sqlpolicy.New(sqlpolicy.Config{}), StrictASTDigest,
		physicalquery.Request{
			VisibleSQL: visible, CompanionSQL: companion, Grant: prepared.PolicyGrant(),
			State: fixture.PreState(prepared.Binding().UsesExpandedEvidence()),
		})
	if err != nil {
		t.Fatal(err)
	}

	const retainedVisible = `WITH "final_v5_result_heavy" AS (
  SELECT "amount", "category", "event_date", "row_id"
  FROM "reporting"."final_v5_result_heavy"
  WHERE "category" IN ($1, $2, $3, $4)
)
SELECT *
FROM (
SELECT row_id, category, amount, event_date FROM final_v5_result_heavy WHERE category IN ($5, $6, $7, $8) AND row_id <= $9 ORDER BY row_id ASC
) AS "__taskbound_result"
LIMIT $10`
	const retainedCompanion = `WITH "final_v5_result_heavy" AS (
  SELECT "amount", "category", "event_date", "row_id"
  FROM "reporting"."final_v5_result_heavy"
  WHERE "category" IN ($1, $2, $3, $4)
),
"ordinal_sidecar_1be2bf8b1a3ce28d" AS (
  SELECT "row_handle", "row_id"
  FROM "taskgate_ordinal"."final_v5_result_heavy_v1"
)
SELECT *
FROM (
SELECT tg_v4_provenance.row_id AS row_id, tg_v4_provenance.category AS category, tg_v4_provenance.amount AS amount, tg_v4_provenance.event_date AS event_date, tg_v4_sidecar_0.row_handle AS tg_h_a6ee88e89f9997c9 FROM (SELECT row_id, category, amount, event_date FROM final_v5_result_heavy WHERE category IN ($5, $6, $7, $8) AND row_id <= $9 ORDER BY row_id ASC) tg_v4_provenance JOIN ordinal_sidecar_1be2bf8b1a3ce28d tg_v4_sidecar_0 ON tg_v4_provenance.row_id = tg_v4_sidecar_0.row_id ORDER BY tg_v4_provenance.row_id ASC
) AS "__taskbound_result"
LIMIT $10`
	const retainedViewDefinition = `WITH taskgate_schema_digest_path AS (
	SELECT set_config($3, $4, $5)
)
SELECT pg_get_viewdef(format($6, $1::text, $2::text)::regclass, $7)
FROM taskgate_schema_digest_path`

	for _, testCase := range []struct {
		name             string
		source, retained string
	}{
		{"visible", derived.VisibleDecision.SQL, retainedVisible},
		{"companion", derived.CompanionDecision.SQL, retainedCompanion},
		{"view definition", dataconnector.ViewDefinitionAttestationSQL, retainedViewDefinition},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source, err := StrictASTDigest(testCase.source)
			if err != nil {
				t.Fatalf("source digest: %v", err)
			}
			retained, err := StrictASTDigest(testCase.retained)
			if err != nil {
				t.Fatalf("retained digest: %v", err)
			}
			if source != retained {
				t.Fatalf("source digest %s != retained digest %s", source, retained)
			}
			t.Logf("source and retained strict AST digest = %s", source)
		})
	}
}
