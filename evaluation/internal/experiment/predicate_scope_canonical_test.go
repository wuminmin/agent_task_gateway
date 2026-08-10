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
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const (
	run04CompactScope = `{"category":["alpha","beta","delta","gamma"]}`
	run04JSONBScope   = `{"category": ["alpha", "beta", "delta", "gamma"]}`

	run04GrantSHA256              = "646a66b82e4e2196481198a64583925acdd1a866d9710835cf2be7aa27602163"
	run04PreparationInputsSHA256  = "87dbe20076297104b0a5c12f01a80a4515f4e4cad8fa9b208da026b9a9d40c42"
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
