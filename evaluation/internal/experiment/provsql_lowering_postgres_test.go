package experiment

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// TestExactProvSQLPreparedPairAgainstPostgreSQL executes the shared production
// preparation for one exact frozen variant against the digest-pinned Business
// PostgreSQL test database. It is not a live deployment or publication result:
// the private binding is the explicitly synthetic resolver fixture, while the
// SQL, Catalog, retained ordinal publications, compiler and paired execution
// path are the production ones.
func TestExactProvSQLPreparedPairAgainstPostgreSQL(t *testing.T) {
	dsn := os.Getenv("BUSINESS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for the exact ProvSQL preparation test")
	}
	contracts := provSQLCapableContractsForTest(t)
	binding := provSQLBindingForResolverTest(t)
	key := finalv5binding.ProvSQLBindingKey("1k", 101)
	candidates, err := contracts.resolveProvSQLCandidatesV3(FrozenContractSelectorV3{
		ExperimentID: "provsql", WorkloadID: "nonce-join-group", Scale: "1k", Mode: "taskgate", BindingKey: key,
	}, binding)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("resolve exact ProvSQL candidate: count=%d err=%v", len(candidates), err)
	}
	material := frozenOperationMaterialV3ForCandidate(candidates[0], profileMaterialV3{
		CatalogPath: contracts.live.CatalogPath, SnapshotArtifactDir: retainedSnapshotArtifacts(t),
	})
	logicalCatalog, err := catalog.Load(material.CatalogPath)
	if err != nil {
		t.Fatalf("load ProvSQL profile Catalog: %v", err)
	}
	view, err := physicalquery.CatalogViewFromCatalog(*logicalCatalog)
	if err != nil {
		t.Fatalf("build ProvSQL Catalog view: %v", err)
	}
	snapshots, err := snapshotBindingsFromArtifactsV3(*logicalCatalog, material.SnapshotArtifactDir)
	if err != nil {
		t.Fatalf("bind retained ProvSQL publications: %v", err)
	}
	prepared, err := physicalquery.Prepare(physicalquery.PreparationInputs{
		Plan: material.Plan, Grant: material.Grant, Catalog: view, SnapshotBindings: snapshots,
	})
	if err != nil {
		t.Fatalf("prepare exact ProvSQL candidate: %v", err)
	}
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatalf("read exact ProvSQL statement pair: %v", err)
	}
	derivation, err := physicalquery.Derive(sqlpolicy.New(sqlpolicy.Config{}), StrictASTDigest,
		physicalquery.Request{VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
			Grant: prepared.PolicyGrant(), State: physicalquery.LedgerPreState{
				RemainingRows: provsqlfixture.ExpectedRows, InfluenceFacts: 10_000,
				UsesExpandedEvidence: prepared.Binding().UsesExpandedEvidence(), HasExposureContext: true,
			}})
	if err != nil {
		t.Fatalf("derive exact ProvSQL executable pair: %v", err)
	}
	if derivation.CompanionDecision == nil {
		t.Fatal("exact ProvSQL preparation produced no companion statement")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 20 * time.Second, ConnectTimeout: 5 * time.Second,
		MaxRows: 10_001, MaxConnections: 1, ApplicationName: "taskgate-p2.6a-provsql-exact-test",
	})
	if err != nil {
		t.Fatalf("open Business PostgreSQL: %v", err)
	}
	defer connector.Close()
	pair, err := connector.QueryPair(ctx, dataconnector.QueryPairRequest{
		Visible: dataconnector.QueryRequest{SQL: derivation.VisibleDecision.SQL,
			StatementTimeout: 20 * time.Second, MaxRows: provsqlfixture.ExpectedRows + 1},
		Provenance: dataconnector.QueryRequest{SQL: derivation.CompanionDecision.SQL,
			StatementTimeout: 20 * time.Second, MaxRows: derivation.Limits.CompanionPolicyRows},
	})
	if err != nil {
		t.Fatalf("execute exact ProvSQL visible/companion pair: %v", err)
	}
	if pair.Visible.Truncated || pair.Provenance.Truncated {
		t.Fatalf("exact ProvSQL pair truncated: visible=%v companion=%v", pair.Visible.Truncated, pair.Provenance.Truncated)
	}
	expectedRows, err := provsqlfixture.ExpectedResultRows("1k")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pair.Visible.Rows, expectedRows) {
		t.Fatalf("exact ProvSQL visible rows differ from the frozen oracle: got=%#v want=%#v",
			pair.Visible.Rows, expectedRows)
	}
	queryProducts := make(map[string]queryplan.Product, len(material.Grant.ApprovedColumns))
	for productID, approvedColumns := range material.Grant.ApprovedColumns {
		product, present := logicalCatalog.LookupProduct(productID)
		if !present {
			t.Fatalf("exact ProvSQL grant names absent product %q", productID)
		}
		approved := make(map[string]struct{}, len(approvedColumns))
		for _, column := range approvedColumns {
			approved[column] = struct{}{}
		}
		queryProducts[productID] = physicalquery.QueryProductFromCatalog(product, approved)
	}
	compiled, err := queryplan.CompileRelational(material.Plan, queryProducts)
	if err != nil {
		t.Fatalf("compile exact ProvSQL result schema: %v", err)
	}
	statusAlias := compiled.OutputAliases[material.Plan.Columns[0]]
	wantColumns := []dataconnector.Column{
		{Name: statusAlias, DataTypeOID: 20},
		{Name: "price", DataTypeOID: 25},
		{Name: "lines", DataTypeOID: 20},
		{Name: "members", DataTypeOID: 20},
	}
	if !reflect.DeepEqual(pair.Visible.Columns, wantColumns) {
		t.Fatalf("exact ProvSQL internal visible schema = %#v, want bigint/text/bigint/bigint %#v",
			pair.Visible.Columns, wantColumns)
	}
	lowered, err := sqllowering.Lower(binding.Section.ProvSQL.TaskGate[key].SQL, queryProducts)
	if err != nil {
		t.Fatalf("lower exact ProvSQL display schema: %v", err)
	}
	if !reflect.DeepEqual(lowered.DisplayColumns, []string{"status", "price", "lines", "members"}) {
		t.Fatalf("exact ProvSQL public display schema = %#v", lowered.DisplayColumns)
	}
	gotSHA, err := CanonicalResultHash(pair.Visible.Rows)
	if err != nil {
		t.Fatalf("hash exact ProvSQL result: %v", err)
	}
	wantSHA, err := CanonicalResultHash(expectedRows)
	if err != nil {
		t.Fatal(err)
	}
	if gotSHA != wantSHA || pair.Visible.RowCount != provsqlfixture.ExpectedRows ||
		len(pair.Visible.Columns) != provsqlfixture.ExpectedColumns || pair.Provenance.RowCount == 0 {
		t.Fatalf("exact ProvSQL pair disagrees with its oracle: sha=%s want=%s visible=%d/%d companion=%d",
			gotSHA, wantSHA, pair.Visible.RowCount, len(pair.Visible.Columns), pair.Provenance.RowCount)
	}
}
