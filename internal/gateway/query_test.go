package gateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/semanticcache"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

const (
	testSummarySQL      = "SELECT month, total_amount FROM expense_summary"
	testExposureProfile = "taskgate-exposure-v1"
)

func TestReceiptSigningClampsRegressedWallClock(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-receipt-clock")
	result := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-receipt-clock", "request_id": "receipt-clock-1", "sql": testSummarySQL,
	})
	evidence, err := harness.store.GetQueryReceipt(t.Context(), result["query_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	request, err := BuildQueryReceiptRequest(evidence, harness.service.queryReceiptSigner,
		evidence.Query.CreatedAt.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Query.CompletedAt == nil || request.SignedAt.Before(*evidence.Query.CompletedAt) {
		t.Fatalf("signed_at %s precedes completed_at %v", request.SignedAt, evidence.Query.CompletedAt)
	}
}

func TestBuildQueryReceiptRequestEmitsV8ArtifactIntent(t *testing.T) {
	digest64 := fmt.Sprintf("%064x", 1)
	created := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	completed := created.Add(time.Millisecond)
	after := control.BudgetSnapshot{
		TaskID: "task-v8", Limits: control.BudgetLimits{Queries: 10, Rows: 100, DBMS: 1000},
		Usage: control.BudgetUsage{UsedQueries: 1, UsedRows: 1, UsedDBMS: 2}, UpdatedAt: completed,
	}
	record := control.QueryRecord{
		ID: "query-v8", TaskID: "task-v8", RequestID: "request-v8", Actor: "alice",
		RequestDigest: digest64, SQLFingerprint: "fingerprint-v8", CatalogVersion: "catalog-v8",
		CatalogDigest: digest64, DatasourceID: "datasource-v8", SchemaDigest: digest64,
		ManifestDigest: digest64, GrantDigest: digest64, PolicyDecision: "ALLOW",
		Status: control.QueryCompleted, ReservedRows: 5, ReservedDBMS: 50,
		ResultRows: 1, ResultDBMS: 2, ChargedQueries: 1, ChargedRows: 1, ChargedDBMS: 2,
		BudgetBefore: control.BudgetSnapshot{
			TaskID: "task-v8", Limits: control.BudgetLimits{Queries: 10, Rows: 100, DBMS: 1000}, UpdatedAt: created,
		},
		BudgetAfter: &after, ResultSHA256: digest64, CreatedAt: created, CompletedAt: &completed,
	}
	terminal := control.AuditEvent{Sequence: 7, PreviousHash: digest64, CurrentHash: digest64}
	registration := control.AuditEvent{Sequence: 8, PreviousHash: digest64, CurrentHash: fmt.Sprintf("%064x", 2)}
	expires := completed.Add(time.Hour)
	artifact := control.ResultArtifact{
		ResultID: "result-v8", QueryID: record.ID, TaskID: record.TaskID, KeyID: "artifact-key-v8",
		Format: "parquet", Encryption: "chunked-aes-gcm-v1",
		StagingKey: "private/staging/result-v8", ObjectKey: "private/results/result-v8",
		ParquetSHA256: digest64, ObjectSHA256: fmt.Sprintf("%064x", 3),
		ParquetSize: 128, ObjectSize: 160, RowCount: 1, ColumnCount: 1,
		SchemaJSON: []byte(`[{"name":"amount"}]`), ACLJSON: []byte(`{"subject":"alice"}`),
		ResultMetadataJSON: []byte(`{"display_columns":["amount"],"limited":false}`),
		Status:             control.ResultArtifactAvailable, ExpiresAt: &expires,
	}
	exposureCharge := control.ExposureCharge{
		QueryID: record.ID, RootTaskID: record.TaskID, ProfileVersion: exposure.ProfileV5,
		ActualOutcomeFacts: 2, ChargedOutcomeFacts: 2,
		ActualPredicateAtomCount: 1, ChargedPredicateAtomCount: 1,
		CompositeOutcomeSHA256: fmt.Sprintf("%064x", 4), PredicateContextSHA256: fmt.Sprintf("%064x", 5),
		PredicateSetSHA256: fmt.Sprintf("%064x", 6), ObservationSHA256: fmt.Sprintf("%064x", 7),
		DictionarySetDigest: fmt.Sprintf("%064x", 8), ReleaseSetSHA256: fmt.Sprintf("%064x", 9),
		InfluenceSetSHA256: fmt.Sprintf("%064x", 10), OutcomeSetSHA256: fmt.Sprintf("%064x", 11), RootEpoch: 1,
	}
	signer := queryreceipt.DemoSigner([]byte("gateway-v8-builder-test"))
	request, err := BuildQueryReceiptRequest(control.QueryReceipt{
		Query: record, Audit: terminal, Exposure: &exposureCharge,
		Artifact: &artifact, ArtifactRegistrationAudit: &registration,
	}, signer, completed)
	if err != nil {
		t.Fatal(err)
	}
	var signed queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(request.ReceiptJSON, &signed); err != nil {
		t.Fatal(err)
	}
	if signed.Version != queryreceipt.VersionV8 || signed.ArtifactIntent == nil ||
		signed.ArtifactIntent.Status != queryreceipt.ArtifactStatusPending {
		t.Fatalf("V8 artifact receipt = %+v", signed)
	}
	if signed.ArtifactIntent.ResultMetadataSHA256 != digest(string(artifact.ResultMetadataJSON)) {
		t.Fatalf("result metadata digest = %q, want complete metadata binding", signed.ArtifactIntent.ResultMetadataSHA256)
	}
	if strings.Contains(string(request.ReceiptJSON), artifact.ObjectKey) ||
		strings.Contains(string(request.ReceiptJSON), artifact.StagingKey) {
		t.Fatalf("V8 receipt leaked physical object key: %s", request.ReceiptJSON)
	}
	verifier, err := queryreceipt.NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(signed); err != nil {
		t.Fatalf("verify V8 builder output: %v", err)
	}
}

func TestExposurePlanHidesMeteringKeysAndDeduplicatesReplay(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureSummaryTask(t, "task-exposure-plan", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}, {Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}},
		Rows:    [][]any{{"2026-01", json.Number("123.45"), "销售部", "机票"}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}, {Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{"销售部", "机票", "2026-01", json.Number("123.45")}}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	arguments := map[string]any{
		"task_id": "task-exposure-plan", "request_id": "exposure-request-1",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	}
	first := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", arguments)
	columns := first["columns"].([]dataconnector.Column)
	rows := first["rows"].([][]any)
	if len(columns) != 2 || columns[0].Name != "month" || columns[1].Name != "total_amount" || len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("metering keys leaked into result: columns=%+v rows=%+v", columns, rows)
	}
	charge := first["exposure"].(control.ExposureCharge)
	if charge.ActualReleaseFacts != 2 || charge.ChargedReleaseFacts != 2 || charge.ActualInfluenceFacts != 5 || charge.ChargedInfluenceFacts != 5 {
		t.Fatalf("first exposure charge = %+v", charge)
	}
	components := first["component_ms"].(map[string]float64)
	for _, name := range []string{"exposure_derivation", "exposure_reservation_lock", "exposure_ledger_lock", "exposure_fact_store"} {
		if value, present := components[name]; !present || value < 0 {
			t.Fatalf("exposure component timing %q = %v, present=%v", name, value, present)
		}
	}
	receipt := first["receipt"].(map[string]any)
	if receipt["version"] != "4" || receipt["exposure"] == nil {
		t.Fatalf("exposure receipt is not signed as V4: %#v", receipt)
	}
	if len(harness.connector.requests) != 2 {
		t.Fatalf("paired query calls = %d, want visible and provenance", len(harness.connector.requests))
	}
	if harness.connector.requests[0].SQL != harness.connector.requests[1].SQL ||
		harness.connector.requests[0].MaxRows != harness.connector.requests[1].MaxRows {
		t.Fatalf("non-grouped pair selected different bounded row sets: visible=%+v provenance=%+v",
			harness.connector.requests[0], harness.connector.requests[1])
	}

	replay := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", arguments)
	if replay["idempotent_replay"] != true || len(harness.connector.requests) != 2 {
		t.Fatalf("replay executed again: replay=%+v calls=%d", replay, len(harness.connector.requests))
	}

	arguments["request_id"] = "exposure-request-2"
	second := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", arguments)
	secondCharge := second["exposure"].(control.ExposureCharge)
	if secondCharge.ChargedReleaseFacts != 0 || secondCharge.ChargedInfluenceFacts != 0 {
		t.Fatalf("same facts were charged twice: %+v", secondCharge)
	}
}

func TestOrdinalSemanticAuthorizationDigestIgnoresVolatileResourceCeilings(t *testing.T) {
	manifestDigest := strings.Repeat("a", 64)
	firstVisible := sqlpolicy.Decision{Fingerprint: strings.Repeat("b", 64), RowLimit: 100}
	firstProvenance := sqlpolicy.Decision{Fingerprint: strings.Repeat("c", 64), RowLimit: 5_001}
	first := ordinalSemanticAuthorizationDigest(firstVisible, firstProvenance, manifestDigest)

	// Both ceilings can change between distinct requests: the visible ceiling
	// follows the remaining row budget, and a non-grouped V4 provenance query
	// uses that same ceiling. Neither change may partition semantic replay.
	secondVisible := firstVisible
	secondVisible.RowLimit = 97
	secondProvenance := firstProvenance
	secondProvenance.RowLimit = 97
	second := ordinalSemanticAuthorizationDigest(secondVisible, secondProvenance, manifestDigest)
	if second != first {
		t.Fatalf("resource ceiling change partitioned semantic identity: %s != %s", second, first)
	}

	changedSQL := secondVisible
	changedSQL.Fingerprint = strings.Repeat("d", 64)
	if got := ordinalSemanticAuthorizationDigest(changedSQL, secondProvenance, manifestDigest); got == first {
		t.Fatal("visible SQL fingerprint change did not partition semantic identity")
	}
	changedProvenance := secondProvenance
	changedProvenance.Fingerprint = strings.Repeat("e", 64)
	if got := ordinalSemanticAuthorizationDigest(secondVisible, changedProvenance, manifestDigest); got == first {
		t.Fatal("provenance SQL fingerprint change did not partition semantic identity")
	}
	if got := ordinalSemanticAuthorizationDigest(secondVisible, secondProvenance, strings.Repeat("f", 64)); got == first {
		t.Fatal("authorization manifest change did not partition semantic identity")
	}
}

func TestOrdinalSemanticAuthorizationDigestV5UsesCanonicalIdentity(t *testing.T) {
	planDigest := strings.Repeat("1", 64)
	manifestDigest := strings.Repeat("2", 64)
	footprint := &queryplan.PredicateFootprint{
		Version:         "taskgate-predicate-footprint-v1",
		ContextSHA256:   strings.Repeat("3", 64),
		AtomSetSHA256:   strings.Repeat("4", 64),
		UniqueAtomCount: 3,
		RawLiteralCount: 4,
		DuplicateCount:  1,
	}
	first := ordinalSemanticAuthorizationDigestV5(planDigest, manifestDigest, footprint)

	// Raw literal multiplicity is request metadata. Canonical plan and atom-set
	// identity remain unchanged after reordering or deduplicating IN members.
	equivalent := *footprint
	equivalent.RawLiteralCount = 3
	equivalent.DuplicateCount = 0
	if got := ordinalSemanticAuthorizationDigestV5(planDigest, manifestDigest, &equivalent); got != first {
		t.Fatalf("equivalent V5 predicate syntax partitioned semantic identity: %s != %s", got, first)
	}

	cases := []struct {
		name      string
		plan      string
		manifest  string
		footprint queryplan.PredicateFootprint
	}{
		{name: "plan", plan: strings.Repeat("5", 64), manifest: manifestDigest, footprint: *footprint},
		{name: "manifest", plan: planDigest, manifest: strings.Repeat("6", 64), footprint: *footprint},
		{name: "context", plan: planDigest, manifest: manifestDigest, footprint: func() queryplan.PredicateFootprint {
			changed := *footprint
			changed.ContextSHA256 = strings.Repeat("7", 64)
			return changed
		}()},
		{name: "atom set", plan: planDigest, manifest: manifestDigest, footprint: func() queryplan.PredicateFootprint {
			changed := *footprint
			changed.AtomSetSHA256 = strings.Repeat("8", 64)
			return changed
		}()},
		{name: "atom count", plan: planDigest, manifest: manifestDigest, footprint: func() queryplan.PredicateFootprint {
			changed := *footprint
			changed.UniqueAtomCount++
			return changed
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ordinalSemanticAuthorizationDigestV5(tc.plan, tc.manifest, &tc.footprint); got == first {
				t.Fatal("semantic identity change did not partition V5 authorization digest")
			}
		})
	}
}

func TestOrdinalSemanticReplayBindingPinsCompilerOrderingAndPaginationVersions(t *testing.T) {
	binding := ordinalSemanticReplayBinding("task-versioned", strings.Repeat("1", 64), strings.Repeat("2", 64),
		strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64))
	if binding.CompilerVersion != queryplan.OrdinalProgramVersion ||
		binding.OrderingVersion != semanticcache.OrderingV1 || binding.PaginationVersion != semanticcache.PaginationV1 {
		t.Fatalf("semantic replay versions are not explicit: %#v", binding)
	}
	first, err := binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	changed := binding
	changed.CompilerVersion = queryplan.OrdinalProgramVersion + "-changed"
	second, err := changed.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("ordinal compiler version change did not partition semantic replay")
	}
}

func TestSQLAndExecutePlanShareV4SemanticReplayAfterConsumedRowBudget(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t)
	taskID := "task-v4-replay-consumed-row-budget"
	approvedColumns := []string{"month", "department", "expense_type", "total_amount"}
	harness.createTaskWithGrantAndExposureProfile(t, taskID, func(core *domain.TaskGrantCoreV1) {
		core.Budget.MaxResultRows = 100
	}, control.ExposureLimits{ReleaseFacts: 100, InfluenceFacts: 100, OutcomeFacts: 10}, exposure.ProfileV4,
		[]string{"expense_summary"}, map[string][]string{"expense_summary": approvedColumns}, domain.SensitivityLow)

	plan := queryplan.QueryPlan{Product: "expense_summary", Columns: approvedColumns,
		OrderBy: []queryplan.Order{{Column: "month", Direction: "asc"},
			{Column: "department", Direction: "asc"}, {Column: "expense_type", Direction: "asc"}}}
	product, found := harness.catalog.LookupProduct(plan.Product)
	if !found {
		t.Fatal("expense_summary product is unavailable")
	}
	approved := stringSetFromSlice(approvedColumns)
	ordinalProduct, err := harness.service.ordinalQueryProduct(product, approved)
	if err != nil {
		t.Fatal(err)
	}
	compilation, err := queryplan.CompileOrdinal(plan, ordinalProduct)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := harness.service.bindOrdinalSidecars(compilation.ProvenanceSQL,
		compilation.ProvenanceFields, compilation.OrdinalProgram)
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{
		{"month": "2026-01", "department": "销售部", "expense_type": "机票", "total_amount": json.Number("1680.00")},
		{"month": "2026-01", "department": "销售部", "expense_type": "酒店", "total_amount": json.Number("880.00")},
		{"month": "2026-02", "department": "销售部", "expense_type": "高铁", "total_amount": json.Number("553.00")},
	}
	visible := scanVisibleResult(t, bound.Program, rows)
	visible.DatabaseTime = 2 * time.Millisecond
	provenanceColumns, positions := ordinalProvenanceColumns(bound.Program)
	provenanceRows := make([][]any, 0, len(rows))
	for _, row := range rows {
		values := make([]any, len(provenanceColumns))
		for _, source := range bound.Program.Sources {
			entityKey := ordinalFixtureEntityKey(t, source, row)
			handle, present := bound.Indexes[source.SourceAlias].LookupRowHandle(entityKey)
			if !present {
				t.Fatalf("snapshot index misses entity %q", entityKey)
			}
			values[positions[source.HandleAlias]] = uint64(handle)
			for _, field := range source.EvidenceFields {
				values[positions[field.ProvenanceAlias]] = row[field.Column]
			}
		}
		provenanceRows = append(provenanceRows, values)
	}
	harness.connector.result = visible
	harness.connector.provenanceResult = dataconnector.Result{Columns: provenanceColumns, Rows: provenanceRows,
		RowCount: int64(len(provenanceRows)), DatabaseTime: time.Millisecond}

	first := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": "v4-row-budget-novel", "plan": plan,
	})
	if first["semantic_replay"] == true || first["row_count"] != int64(3) {
		t.Fatalf("first request was not the three-row novel path: %#v", first)
	}
	if len(harness.connector.requests) != 2 {
		t.Fatalf("novel connector calls = %d, want visible and provenance", len(harness.connector.requests))
	}
	budget, err := harness.store.GetBudget(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := budget.Remaining().Rows; remaining != 97 {
		t.Fatalf("remaining rows after novel = %d, want 97", remaining)
	}

	secondArguments := map[string]any{
		"task_id": taskID, "request_id": "v4-row-budget-semantic-replay",
		"sql": `SELECT month, department, expense_type, total_amount
	FROM expense_summary
	ORDER BY month ASC, department ASC, expense_type ASC`,
	}
	second := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", secondArguments)
	if second["semantic_replay"] != true || second["row_count"] != int64(3) {
		t.Fatalf("cross-entry request did not use semantic replay: %#v", second)
	}
	if second["plan_digest"] != first["plan_digest"] || second["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("cross-entry semantic identity differs: execute_plan=%v query_sql=%v profile=%v",
			first["plan_digest"], second["plan_digest"], second["sql_profile"])
	}
	if len(harness.connector.requests) != 2 {
		t.Fatalf("semantic replay executed connector: calls=%d", len(harness.connector.requests))
	}
	secondReplay := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", secondArguments)
	if secondReplay["idempotent_replay"] != true || secondReplay["plan_digest"] != second["plan_digest"] ||
		secondReplay["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("semantic then idempotent replay lost SQL metadata: %#v", secondReplay)
	}
	charge := second["exposure"].(control.ExposureCharge)
	if charge.ChargedReleaseFacts != 0 || charge.ChargedInfluenceFacts != 0 || charge.ChargedOutcomeFacts != 0 {
		t.Fatalf("semantic replay charged exposure novelty: %+v", charge)
	}
	var materializations, cacheKeys int
	if err := harness.store.DB().QueryRowContext(context.Background(), `
SELECT count(*),count(DISTINCT cache_key_sha256)
FROM v4_committed_materializations WHERE task_id=$1`, taskID).Scan(&materializations, &cacheKeys); err != nil {
		t.Fatal(err)
	}
	if materializations != 1 || cacheKeys != 1 {
		t.Fatalf("semantic replay materializations=%d keys=%d, want 1/1", materializations, cacheKeys)
	}
}

func TestGroupedExposureUsesOverflowProbeAndMatchesAlgebraRelease(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureSummaryTask(t, "task-exposure-grouped", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month"}, {Name: "total"}},
		Rows:    [][]any{{"2026-01", float64(30)}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns:  []dataconnector.Column{{Name: "department"}, {Name: "expense_type"}, {Name: "month"}, {Name: "total_amount"}},
		Rows:     [][]any{{"销售部", "机票", "2026-01", int64(10)}, {"销售部", "酒店", "2026-01", int64(20)}},
		RowCount: 2, DatabaseTime: time.Millisecond,
	}
	plan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "total_amount", Alias: "total"}},
		GroupBy:    []string{"month"}}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-exposure-grouped", "request_id": "grouped-overflow-probe", "plan": plan,
	})
	if len(harness.connector.requests) != 2 || harness.connector.requests[1].MaxRows != 20 ||
		!strings.HasSuffix(harness.connector.requests[1].SQL, "LIMIT 21") {
		t.Fatalf("grouped provenance did not use a one-row overflow probe: %+v", harness.connector.requests)
	}

	product, _ := harness.catalog.LookupProduct("expense_summary")
	approved := make(map[string]struct{})
	for _, field := range product.Fields {
		approved[field.Name] = struct{}{}
	}
	exposureContext, err := buildPlanExposureContext(plan, product, approved, map[string]struct{}{"sum": {}})
	if err != nil {
		t.Fatal(err)
	}
	online := result["exposure"].(control.ExposureCharge)
	if online.ActualReleaseFacts != 2 {
		t.Fatalf("grouped release count = %d, want 2", online.ActualReleaseFacts)
	}

	keys := make([]string, 2)
	keys[0], _ = exposure.ComposeKey("2026-01", "销售部", "机票")
	keys[1], _ = exposure.ComposeKey("2026-01", "销售部", "酒店")
	base, err := exposure.NewBaseRelation(product.Name, product.Snapshot, exposureContext.provenanceFields, []exposure.BaseRow{
		{EntityKey: keys[0], Values: map[string]any{"department": "销售部", "expense_type": "机票", "month": "2026-01", "total_amount": int64(10)}},
		{EntityKey: keys[1], Values: map[string]any{"department": "销售部", "expense_type": "酒店", "month": "2026-01", "total_amount": int64(20)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	aggregated, err := exposure.Aggregate(base, []string{"month"}, []exposure.AggregateSpec{{Function: "sum", Field: "total_amount", Alias: "renamed"}})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := exposure.Observe(testExposureProfile, aggregated)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := harness.store.GetExposureCharge(context.Background(), result["query_id"].(string))
	if err != nil || charge.ObservationSHA256 == "" {
		t.Fatalf("grouped charge = %+v, %v", charge, err)
	}
	// deriveObservation is checked directly so canonical source-bound identities,
	// rather than only their count in the persisted charge, are compared.
	derived, err := exposureContext.deriveObservation(harness.connector.result, harness.connector.provenanceResult, testExposureProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !sameExposureFactIDs(t, derived.Release, expected.Release) {
		t.Fatalf("online release differs from algebra:\nonline=%+v\nalgebra=%+v", derived.Release, expected.Release)
	}
}

func TestGroupedPaginationSQLUsesCanonicalGroupKeySuffix(t *testing.T) {
	product := catalog.Product{Name: "expenses", Snapshot: "s1", EntityKey: []string{"a", "b"},
		Fields: []catalog.Field{{Name: "a", Type: "text"}, {Name: "b", Type: "text"}, {Name: "amount", Type: "numeric"}}}
	columns := map[string]struct{}{"a": {}, "b": {}, "amount": {}}
	context, err := buildPlanExposureContext(queryplan.QueryPlan{Product: "expenses", Columns: []string{"b", "a"},
		Aggregates: []queryplan.Aggregate{{Function: "sum", Column: "amount", Alias: "total"}},
		GroupBy:    []string{"b", "a"}, Limit: 5}, product, columns, map[string]struct{}{"sum": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context.mainSQL, `ORDER BY "a" ASC, "b" ASC LIMIT 5`) {
		t.Fatalf("group pagination did not use the canonical key suffix: %s", context.mainSQL)
	}
}

func TestExposureV2OnlinePathSupportsOffset(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-v2-offset", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20})
	page := dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}},
		Rows:    [][]any{{"2026-03", "销售部", "机票"}}, RowCount: 1,
	}
	harness.connector.result = page
	harness.connector.provenanceResult = page
	result := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-offset", "request_id": "v2-offset-page",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month"},
			"order_by": []map[string]any{{"column": "month", "direction": "asc"}}, "limit": 1, "offset": 2},
	})
	if len(harness.connector.requests) != 2 {
		t.Fatalf("paired online calls = %d, want 2", len(harness.connector.requests))
	}
	for _, request := range harness.connector.requests {
		if !strings.Contains(request.SQL, "LIMIT 1 OFFSET 2") {
			t.Fatalf("online pagination SQL omitted offset: %s", request.SQL)
		}
	}
	columns := result["columns"].([]dataconnector.Column)
	if len(columns) != 1 || columns[0].Name != "month" {
		t.Fatalf("hidden pagination evidence leaked: %+v", columns)
	}
}

func TestExposureV2OnlinePathSupportsCountStar(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-v2-count-star", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "rows", DataTypeOID: 20}},
		Rows:    [][]any{{int64(2)}}, RowCount: 1,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}, {Name: "month", DataTypeOID: 25}},
		Rows:    [][]any{{"销售部", "机票", "2026-01"}, {"销售部", "酒店", "2026-01"}}, RowCount: 2,
	}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-count-star", "request_id": "v2-count-star",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{},
			"aggregates": []map[string]any{{"function": "count", "column": "*", "alias": "rows"}}},
	})
	if len(harness.connector.requests) != 2 || !strings.Contains(harness.connector.requests[0].SQL, "count(*)") {
		t.Fatalf("COUNT(*) did not use the paired online path: %+v", harness.connector.requests)
	}
	charge := result["exposure"].(control.ExposureCharge)
	if charge.ActualReleaseFacts != 1 || charge.ActualInfluenceFacts == 0 {
		t.Fatalf("COUNT(*) exposure charge = %+v", charge)
	}
}

func TestExposureV3ChargesDistinctZeroResultPredicates(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV3SummaryTask(t, "task-v3-zero-thresholds", control.ExposureLimits{
		ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 3,
	})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "rows", DataTypeOID: 20}},
		Rows:    [][]any{{int64(0)}}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25},
			{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows: nil, RowCount: 0, DatabaseTime: time.Millisecond,
	}
	call := func(requestID string, threshold int64) control.ExposureCharge {
		result := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
			"task_id": "task-v3-zero-thresholds", "request_id": requestID,
			"plan": map[string]any{"product": "expense_summary", "columns": []string{},
				"aggregates": []map[string]any{{"function": "count", "column": "*", "alias": "rows"}},
				"filters":    []map[string]any{{"column": "total_amount", "op": ">", "value": threshold}}},
		})
		receipt := result["receipt"].(map[string]any)
		if receipt["version"] != queryreceipt.VersionV5 {
			t.Fatalf("V3 receipt version = %v, want %s", receipt["version"], queryreceipt.VersionV5)
		}
		return result["exposure"].(control.ExposureCharge)
	}

	first := call("v3-zero-100", 100)
	second := call("v3-zero-200", 200)
	replay := call("v3-zero-100-again", 100)
	if first.ActualOutcomeFacts != 1 || first.ChargedOutcomeFacts != 1 ||
		second.ActualOutcomeFacts != 1 || second.ChargedOutcomeFacts != 1 {
		t.Fatalf("different zero-result predicates did not each charge an outcome: first=%+v second=%+v", first, second)
	}
	if second.ChargedReleaseFacts != 0 || second.ChargedInfluenceFacts != 0 {
		t.Fatalf("same zero result unexpectedly recharged release/dependency facts: %+v", second)
	}
	if replay.ChargedReleaseFacts != 0 || replay.ChargedInfluenceFacts != 0 || replay.ChargedOutcomeFacts != 0 {
		t.Fatalf("same proposition replay was not free: %+v", replay)
	}
	ledger, err := harness.store.GetExposureLedger(t.Context(), "task-v3-zero-thresholds")
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Used.OutcomeFacts != 2 {
		t.Fatalf("outcome usage = %d, want 2", ledger.Used.OutcomeFacts)
	}
}

func TestExposureV2GroupedHiddenKeyIsMeteredButNotReleased(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-v2-hidden-group", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total", DataTypeOID: 1700}},
		Rows:    [][]any{{"2026-01", "30"}}, RowCount: 1,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}, {Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows: [][]any{
			{"销售部", "机票", "2026-01", "10"},
			{"销售部", "酒店", "2026-01", "20"},
		}, RowCount: 2,
	}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-hidden-group", "request_id": "v2-hidden-group",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{},
			"aggregates": []map[string]any{{"function": "sum", "column": "total_amount", "alias": "total"}},
			"group_by":   []string{"month"}},
	})
	columns := result["columns"].([]dataconnector.Column)
	rows := result["rows"].([][]any)
	if len(columns) != 1 || columns[0].Name != "total" || len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("hidden group key leaked into result: columns=%+v rows=%+v", columns, rows)
	}
	if len(harness.connector.requests) != 2 ||
		(!strings.Contains(harness.connector.requests[0].SQL, `SELECT "month", sum("total_amount") AS "total"`) &&
			!strings.Contains(harness.connector.requests[0].SQL, `SELECT month, sum(total_amount) AS total`)) {
		t.Fatalf("hidden group key was not selected internally: %+v", harness.connector.requests)
	}
	charge := result["exposure"].(control.ExposureCharge)
	if charge.ActualReleaseFacts != 1 || charge.ActualInfluenceFacts != 6 {
		t.Fatalf("hidden-key grouped exposure = %+v, want release=1 dependency=6", charge)
	}
}

func TestExposureV2OnlineJoinOperandSwapUsesSameLedgerFacts(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createTaskWithGrantAndExposureProfile(t, "task-v2-join", nil,
		control.ExposureLimits{ReleaseFacts: 50, InfluenceFacts: 50}, exposure.ProfileV2,
		[]string{"expense_detail", "expense_summary"}, map[string][]string{
			"expense_detail":  {"amount", "department", "receipt_no"},
			"expense_summary": {"department", "month", "total_amount"},
		}, domain.SensitivityHigh)
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{
			{Name: "expense_detail.receipt_no", DataTypeOID: 25}, {Name: "expense_summary.total_amount", DataTypeOID: 1700},
		},
		Rows: [][]any{{"R-1", "30"}}, RowCount: 1,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{
			{Name: "tg_expense_detail_department", DataTypeOID: 25}, {Name: "tg_expense_detail_receipt_no", DataTypeOID: 25},
			{Name: "tg_expense_summary_department", DataTypeOID: 25}, {Name: "tg_expense_summary_expense_type", DataTypeOID: 25},
			{Name: "tg_expense_summary_month", DataTypeOID: 25},
			{Name: "tg_expense_summary_total_amount", DataTypeOID: 1700},
		},
		Rows: [][]any{{"销售部", "R-1", "销售部", "机票", "2026-01", "30"}}, RowCount: 1,
	}
	plan := map[string]any{
		"from": map[string]any{"join": map[string]any{
			"left":  map[string]any{"product": "expense_detail", "role": "expense_detail"},
			"right": map[string]any{"product": "expense_summary", "role": "expense_summary"},
			"on":    []map[string]any{{"left": "expense_detail.department", "right": "expense_summary.department"}},
		}},
		"columns": []string{"expense_detail.receipt_no", "expense_summary.total_amount"},
	}
	firstArguments := map[string]any{"task_id": "task-v2-join", "request_id": "join-left-right", "plan": plan}
	first := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", firstArguments)
	firstCharge := first["exposure"].(control.ExposureCharge)
	if firstCharge.ActualReleaseFacts != 2 || firstCharge.ActualInfluenceFacts != 6 {
		t.Fatalf("online join charge = %+v, want release=2 dependency=6", firstCharge)
	}
	if columns := first["columns"].([]dataconnector.Column); len(columns) != 2 {
		t.Fatalf("join metering fields leaked: %+v", columns)
	}
	firstReplay := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", firstArguments)
	if firstReplay["idempotent_replay"] != true || firstReplay["plan_digest"] != first["plan_digest"] ||
		firstReplay["output_format"] != "json" {
		t.Fatalf("execute_plan replay lost response metadata: %#v", firstReplay)
	}

	swapped := map[string]any{
		"from": map[string]any{"join": map[string]any{
			"left":  map[string]any{"product": "expense_summary", "role": "expense_summary"},
			"right": map[string]any{"product": "expense_detail", "role": "expense_detail"},
			"on":    []map[string]any{{"left": "expense_summary.department", "right": "expense_detail.department"}},
		}},
		"columns": []string{"expense_detail.receipt_no", "expense_summary.total_amount"},
	}
	second := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-join", "request_id": "join-right-left", "plan": swapped,
	})
	secondCharge := second["exposure"].(control.ExposureCharge)
	if secondCharge.ChargedReleaseFacts != 0 || secondCharge.ChargedInfluenceFacts != 0 {
		t.Fatalf("join operand swap changed ledger facts: %+v", secondCharge)
	}

	// SQL aliases and FROM operand order are presentation syntax. Lowering must
	// erase both and reach the same canonical plan and ledger proposition as the
	// advanced structured entry point.
	sqlArguments := map[string]any{
		"task_id": "task-v2-join", "request_id": "join-sql-entrypoint",
		"sql": `SELECT detail_input.receipt_no, summary_input.total_amount
	FROM expense_summary AS summary_input
	JOIN expense_detail AS detail_input ON summary_input.department = detail_input.department`,
	}
	third := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", sqlArguments)
	thirdCharge := third["exposure"].(control.ExposureCharge)
	if thirdCharge.ChargedReleaseFacts != 0 || thirdCharge.ChargedInfluenceFacts != 0 {
		t.Fatalf("SQL lowering changed cross-entry ledger facts: %+v", thirdCharge)
	}
	if third["plan_digest"] == "" || third["plan_digest"] != first["plan_digest"] {
		t.Fatalf("cross-entry plan digest differs: execute_plan=%v query_sql=%v", first["plan_digest"], third["plan_digest"])
	}
	if third["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("SQL profile = %v, want %s", third["sql_profile"], catalogReportingSQLProfile)
	}
	thirdColumns := third["columns"].([]dataconnector.Column)
	if len(thirdColumns) != 2 || thirdColumns[0].Name != "receipt_no" || thirdColumns[1].Name != "total_amount" {
		t.Fatalf("SQL display columns leaked canonical roles: %+v", thirdColumns)
	}
	for _, request := range harness.connector.requests[len(harness.connector.requests)-2:] {
		if strings.Contains(request.SQL, "summary_input") || strings.Contains(request.SQL, "detail_input") {
			t.Fatalf("raw SQL aliases reached the connector instead of regenerated SQL: %s", request.SQL)
		}
	}
	requestCount := len(harness.connector.requests)
	replayed := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", sqlArguments)
	if replayed["idempotent_replay"] != true || replayed["sql_profile"] != catalogReportingSQLProfile ||
		replayed["plan_digest"] != third["plan_digest"] {
		t.Fatalf("idempotent SQL replay lost canonical metadata: %#v", replayed)
	}
	if _, ok := replayed["query_plan"].(queryplan.QueryPlan); !ok {
		t.Fatalf("idempotent SQL replay lost query plan: %#v", replayed["query_plan"])
	}
	replayedColumns := replayed["columns"].([]dataconnector.Column)
	if len(replayedColumns) != 2 || replayedColumns[0].Name != "receipt_no" || replayedColumns[1].Name != "total_amount" {
		t.Fatalf("idempotent SQL replay lost display columns: %+v", replayedColumns)
	}
	if len(harness.connector.requests) != requestCount {
		t.Fatalf("idempotent SQL replay reached connector: %d -> %d", requestCount, len(harness.connector.requests))
	}
	fetched := mustCallGatewayTool(t, harness.service, harness.alice, "get_query_result", map[string]any{
		"task_id": "task-v2-join", "query_id": third["query_id"],
	})
	fetchedColumns := fetched["columns"].([]dataconnector.Column)
	if len(fetchedColumns) != 2 || fetchedColumns[0].Name != "receipt_no" || fetchedColumns[1].Name != "total_amount" ||
		fetched["sql_profile"] != catalogReportingSQLProfile || fetched["plan_digest"] != third["plan_digest"] {
		t.Fatalf("get_query_result lost SQL response metadata: %#v", fetched)
	}
	if _, ok := fetched["query_plan"].(queryplan.QueryPlan); !ok {
		t.Fatalf("get_query_result lost canonical plan: %#v", fetched["query_plan"])
	}
}

func TestExposureV2OnlineUnionDistinctMetersAllMembersAndHiddenField(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createTaskWithGrantAndExposureProfile(t, "task-v2-union", nil,
		control.ExposureLimits{ReleaseFacts: 50, InfluenceFacts: 50}, exposure.ProfileV2,
		[]string{"expense_summary"}, map[string][]string{
			"expense_summary": {"department", "expense_type", "month"},
		}, domain.SensitivityLow)
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "expense_summary.department", DataTypeOID: 25}, {Name: "expense_summary.month", DataTypeOID: 25}},
		Rows:    [][]any{{"销售部", "2026-01"}}, RowCount: 1,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{
			{Name: "tg_branch", DataTypeOID: 23}, {Name: "tg_expense_summary_department", DataTypeOID: 25},
			{Name: "tg_expense_summary_expense_type", DataTypeOID: 25}, {Name: "tg_expense_summary_month", DataTypeOID: 25},
		},
		Rows: [][]any{
			{int64(0), "销售部", "机票", "2026-01"},
			{int64(1), "销售部", "酒店", "2026-01"},
		}, RowCount: 2,
	}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-v2-union", "request_id": "union-members", "plan": map[string]any{
			"from": map[string]any{"union_distinct": map[string]any{
				"role": "expense_summary", "columns": []string{"department", "month"},
				"left":  map[string]any{"product": "expense_summary", "role": "left_branch", "filters": []map[string]any{{"column": "expense_type", "op": "=", "value": "机票"}}},
				"right": map[string]any{"product": "expense_summary", "role": "right_branch", "filters": []map[string]any{{"column": "expense_type", "op": "=", "value": "酒店"}}},
			}},
			"columns": []string{"expense_summary.department"},
		},
	})
	columns := result["columns"].([]dataconnector.Column)
	if len(columns) != 1 || columns[0].Name != "expense_summary.department" {
		t.Fatalf("hidden UNION dedup field leaked: %+v", columns)
	}
	if len(harness.connector.requests) != 2 || !strings.Contains(harness.connector.requests[0].SQL, " UNION ") ||
		!strings.Contains(harness.connector.requests[1].SQL, "UNION ALL") || !strings.Contains(harness.connector.requests[1].SQL, "LIMIT 51") {
		t.Fatalf("online UNION did not execute distinct/all paired statements: %+v", harness.connector.requests)
	}
	charge := result["exposure"].(control.ExposureCharge)
	if charge.ActualReleaseFacts != 1 || charge.ActualInfluenceFacts != 8 {
		t.Fatalf("online UNION charge = %+v, want release=1 dependency=8", charge)
	}
}

func sameExposureFactIDs(t *testing.T, left, right []exposure.FactID) bool {
	t.Helper()
	leftSet, err := exposure.NewFactSet(left...)
	if err != nil {
		t.Fatal(err)
	}
	rightSet, err := exposure.NewFactSet(right...)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for hash := range leftSet {
		if _, present := rightSet[hash]; !present {
			return false
		}
	}
	return true
}

func TestExposureTaskLowersSQLIntoPairedQueryPlanExecution(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV2SummaryTask(t, "task-exposure-direct", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}, {Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}},
		Rows:    [][]any{{"2026-01", json.Number("123.45"), "销售部", "机票"}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "department", DataTypeOID: 25}, {Name: "expense_type", DataTypeOID: 25}, {Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{"销售部", "机票", "2026-01", json.Number("123.45")}}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	grant, err := harness.store.GetGrant(context.Background(), "task-exposure-direct")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := harness.service.preparePlan(grant, queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.Exposure.deriveObservation(harness.connector.result, harness.connector.provenanceResult, exposure.ProfileV2); err != nil {
		t.Fatalf("fixture does not satisfy V2 paired evidence: %v", err)
	}
	result := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-exposure-direct", "request_id": "direct-request", "sql": testSummarySQL,
	})
	plan, ok := result["query_plan"].(queryplan.QueryPlan)
	if !ok || plan.Product != "expense_summary" || len(plan.Columns) != 2 {
		t.Fatalf("lowered query plan = %#v", result["query_plan"])
	}
	if result["plan_digest"] == "" || result["sql_profile"] != catalogReportingSQLProfile {
		t.Fatalf("lowered response omitted canonical identity: %#v", result)
	}
	if len(harness.connector.requests) != 2 {
		t.Fatalf("exposure SQL connector calls = %d, want visible and provenance", len(harness.connector.requests))
	}
}

func TestSQLLoweringFailureDoesNotReserveOrChargeBudget(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createTaskWithGrantAndExposureProfile(t, "task-v2-lowering-reject", nil,
		control.ExposureLimits{ReleaseFacts: 50, InfluenceFacts: 50}, exposure.ProfileV2,
		[]string{"expense_detail", "expense_summary"}, map[string][]string{
			"expense_detail":  {"amount", "department", "receipt_no"},
			"expense_summary": {"department", "month", "total_amount"},
		}, domain.SensitivityHigh)
	budgetBefore, err := harness.store.GetBudget(context.Background(), "task-v2-lowering-reject")
	if err != nil {
		t.Fatal(err)
	}
	ledgerBefore, err := harness.store.GetExposureLedger(context.Background(), "task-v2-lowering-reject")
	if err != nil {
		t.Fatal(err)
	}

	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-v2-lowering-reject", "request_id": "left-join-reject",
		"sql": `SELECT d.receipt_no, s.total_amount
FROM expense_detail AS d
LEFT JOIN expense_summary AS s ON d.department = s.department`,
	})
	requireToolCode(t, err, apierr.CodeJoinTypeUnsupported)
	if len(harness.connector.requests) != 0 {
		t.Fatalf("rejected SQL reached connector %d times", len(harness.connector.requests))
	}
	records, err := harness.store.ListQueries(context.Background(), "task-v2-lowering-reject", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("lowering rejection created query reservations: %#v", records)
	}
	budgetAfter, err := harness.store.GetBudget(context.Background(), "task-v2-lowering-reject")
	if err != nil {
		t.Fatal(err)
	}
	ledgerAfter, err := harness.store.GetExposureLedger(context.Background(), "task-v2-lowering-reject")
	if err != nil {
		t.Fatal(err)
	}
	if budgetAfter != budgetBefore || ledgerAfter != ledgerBefore {
		t.Fatalf("lowering rejection changed budget: resource before=%+v after=%+v exposure before=%+v after=%+v",
			budgetBefore, budgetAfter, ledgerBefore, ledgerAfter)
	}
}

func TestExposureBudgetRejectsBufferedResultBeforeRelease(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureSummaryTask(t, "task-exposure-over", control.ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 10})
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month"}, {Name: "total_amount"}, {Name: "department"}, {Name: "expense_type"}},
		Rows:    [][]any{{"2026-01", 123.45, "销售部", "机票"}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "department"}, {Name: "expense_type"}, {Name: "month"}, {Name: "total_amount"}},
		Rows:    [][]any{{"销售部", "机票", "2026-01", 123.45}}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	_, err := callGatewayTool(harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-exposure-over", "request_id": "over-request",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	})
	requireToolCode(t, err, apierr.CodeExposureBudgetExhausted)
	record, lookupErr := harness.store.GetQueryByRequestID(context.Background(), "task-exposure-over", "over-request")
	if lookupErr != nil || record.Status != control.QueryFailed || record.ResultSHA256 != "" {
		t.Fatalf("over-budget query = %+v, %v", record, lookupErr)
	}
	if _, _, resultErr := harness.store.GetEncryptedResult(context.Background(), "task-exposure-over", record.ID); resultErr == nil {
		t.Fatal("over-budget buffered result was persisted")
	}
}

func TestOrdinalExposureBudgetBPlusOneCommitsCompleteFailureOnly(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t)
	taskID := "task-v4-exposure-b-plus-one"
	requestID := "v4-exposure-b-plus-one-request"
	// This scan releases two visible base-cell facts. A release ceiling of one
	// therefore makes the attempted observation exactly B+1 in that dimension.
	harness.createExposureV4SummaryTask(t, taskID,
		control.ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 100, OutcomeFacts: 10})

	plan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
	product, found := harness.catalog.LookupProduct(plan.Product)
	if !found {
		t.Fatal("expense_summary product is unavailable")
	}
	ordinalProduct, err := harness.service.ordinalQueryProduct(product,
		map[string]struct{}{"month": {}, "total_amount": {}})
	if err != nil {
		t.Fatal(err)
	}
	compilation, err := queryplan.CompileOrdinal(plan, ordinalProduct)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := harness.service.bindOrdinalSidecars(compilation.ProvenanceSQL,
		compilation.ProvenanceFields, compilation.OrdinalProgram)
	if err != nil {
		t.Fatal(err)
	}
	row := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票",
		"total_amount": json.Number("1680.00"),
	}
	visible := scanVisibleResult(t, bound.Program, []map[string]any{row})
	visible.DatabaseTime = 2 * time.Millisecond
	provenanceColumns, positions := ordinalProvenanceColumns(bound.Program)
	provenanceRow := make([]any, len(provenanceColumns))
	for _, source := range bound.Program.Sources {
		entityKey := ordinalFixtureEntityKey(t, source, row)
		handle, present := bound.Indexes[source.SourceAlias].LookupRowHandle(entityKey)
		if !present {
			t.Fatalf("snapshot index misses entity %q", entityKey)
		}
		provenanceRow[positions[source.HandleAlias]] = uint64(handle)
		for _, field := range source.EvidenceFields {
			provenanceRow[positions[field.ProvenanceAlias]] = row[field.Column]
		}
	}
	harness.connector.result = visible
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1, DatabaseTime: time.Millisecond,
	}

	headBefore, err := harness.store.GetOrdinalRootHead(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if headBefore.Limits.ReleaseFacts != 1 || headBefore.Used.ReleaseFacts != 0 {
		t.Fatalf("B+1 test root starts at unexpected boundary: %+v", headBefore)
	}
	contentBefore := gatewayOrdinalContentCounts(t, harness)

	_, err = callGatewayTool(harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": taskID, "request_id": requestID, "plan": plan,
	})
	requireToolCode(t, err, apierr.CodeExposureBudgetExhausted)
	record, err := harness.store.GetQueryByRequestID(context.Background(), taskID, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != control.QueryFailed || record.ErrorCode != string(control.CodeExposureBudgetExhausted) ||
		record.ResultSHA256 != "" {
		t.Fatalf("B+1 terminal query = %+v", record)
	}

	var reservationStatus string
	var encryptedResults, encryptedChunks, materializations, queryObservations, rootObservations int
	var failureAudits, receipts int
	if err := harness.store.DB().QueryRowContext(context.Background(), `SELECT
 (SELECT status FROM v4_query_exposure_reservations WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_results WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1),
 (SELECT count(*) FROM v4_committed_materializations WHERE source_query_id=$1),
 (SELECT count(*) FROM v4_query_observations WHERE query_id=$1),
 (SELECT count(*) FROM v4_root_observations WHERE first_query_id=$1),
 (SELECT count(*) FROM audit_events WHERE query_id=$1 AND event_type='QUERY_FAILED'),
 (SELECT count(*) FROM query_receipts WHERE query_id=$1)`, record.ID).Scan(
		&reservationStatus, &encryptedResults, &encryptedChunks, &materializations,
		&queryObservations, &rootObservations, &failureAudits, &receipts); err != nil {
		t.Fatal(err)
	}
	if reservationStatus != "RELEASED" || encryptedResults != 0 || encryptedChunks != 0 ||
		materializations != 0 || queryObservations != 0 || rootObservations != 0 ||
		failureAudits != 1 || receipts != 1 {
		t.Fatalf("B+1 atomic failure evidence: reservation=%s result=%d chunks=%d materialization=%d "+
			"query_observation=%d root_observation=%d failure_audit=%d receipt=%d",
			reservationStatus, encryptedResults, encryptedChunks, materializations, queryObservations,
			rootObservations, failureAudits, receipts)
	}
	headAfter, err := harness.store.GetOrdinalRootHead(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if headAfter != headBefore {
		t.Fatalf("B+1 changed the root head: before=%+v after=%+v", headBefore, headAfter)
	}
	if contentAfter := gatewayOrdinalContentCounts(t, harness); contentAfter != contentBefore {
		t.Fatalf("B+1 changed ordinal content: before=%v after=%v", contentBefore, contentAfter)
	}
	if err := harness.service.ReadyError(); err != nil {
		t.Fatalf("B+1 left a pending settlement retry: %v", err)
	}
}

func TestRequestIDIsRequiredAndRetriesNeverExecuteTwice(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-idempotent")

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-idempotent", "sql": testSummarySQL,
	})
	requireToolCode(t, err, apierr.CodeInvalidRequest)

	first := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-idempotent", "request_id": "client-request-001", "sql": testSummarySQL,
	})
	components, ok := first["component_ms"].(map[string]float64)
	if !ok {
		t.Fatalf("query response omitted component timings: %#v", first["component_ms"])
	}
	for _, name := range []string{"authorization", "parse_policy", "reserve", "business_postgresql", "connector_overhead", "result_encoding", "encryption", "settle_persist", "receipt_signing"} {
		if value, found := components[name]; !found || value < 0 {
			t.Fatalf("component timing %q = %v, present=%v", name, value, found)
		}
	}
	replay := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-idempotent", "request_id": "client-request-001", "sql": testSummarySQL,
	})
	if first["query_id"] != replay["query_id"] || replay["idempotent_replay"] != true {
		t.Fatalf("retry did not return the first durable result: first=%#v replay=%#v", first, replay)
	}
	if len(harness.connector.requests) != 1 {
		t.Fatalf("connector calls = %d, want exactly one", len(harness.connector.requests))
	}
	firstReceipt, err := approval.CanonicalJSON(first["receipt"])
	if err != nil {
		t.Fatalf("canonical first receipt: %v", err)
	}
	replayReceipt, err := approval.CanonicalJSON(replay["receipt"])
	if err != nil {
		t.Fatalf("canonical replay receipt: %v", err)
	}
	if string(firstReceipt) != string(replayReceipt) {
		t.Fatalf("replay receipt changed:\nfirst=%s\nreplay=%s", firstReceipt, replayReceipt)
	}
	var persistedReceipts int
	if err := harness.store.DB().QueryRowContext(context.Background(), `SELECT count(*) FROM query_receipts WHERE query_id=$1`, first["query_id"]).Scan(&persistedReceipts); err != nil {
		t.Fatalf("count persisted receipts: %v", err)
	}
	if persistedReceipts != 1 {
		t.Fatalf("persisted receipt count = %d, want 1", persistedReceipts)
	}
	record := requireSingleSettledQuery(t, harness, "task-idempotent")
	if record.DatasourceID != harness.connector.attestation.DatasourceID ||
		record.SchemaDigest != harness.connector.attestation.SchemaDigest ||
		record.CatalogDigest != harness.catalog.SHA256 {
		t.Fatalf("query record omitted datasource evidence: %+v", record)
	}
	receipt, ok := first["receipt"].(map[string]any)
	if !ok || receipt["datasource_id"] != record.DatasourceID || receipt["schema_digest"] != record.SchemaDigest {
		t.Fatalf("query receipt omitted datasource evidence: %#v", first["receipt"])
	}
	budget, err := harness.store.GetBudget(context.Background(), "task-idempotent")
	if err != nil {
		t.Fatalf("GetBudget: %v", err)
	}
	if budget.Usage.UsedQueries != 1 {
		t.Fatalf("used queries = %d, want 1", budget.Usage.UsedQueries)
	}

	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-idempotent", "request_id": "client-request-001", "sql": "SELECT month FROM expense_summary",
	})
	requireToolCode(t, err, apierr.CodeConflict)
	if len(harness.connector.requests) != 1 {
		t.Fatalf("conflicting retry reached connector: %d calls", len(harness.connector.requests))
	}

	mustCallGatewayTool(t, harness.service, harness.alice, "revoke_task", map[string]any{
		"task_id": "task-idempotent", "reason": "test revocation",
	})
	afterRevocation := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-idempotent", "request_id": "client-request-001", "sql": testSummarySQL,
	})
	if afterRevocation["query_id"] != first["query_id"] || len(harness.connector.requests) != 1 {
		t.Fatalf("retry after revocation was not observational: %#v", afterRevocation)
	}
	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-idempotent", "request_id": "client-request-002", "sql": testSummarySQL,
	})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
}

func TestOrdinalTimingComponentsExposeCoherentLeafTimers(t *testing.T) {
	components := make(map[string]float64)
	recordOrdinalTimingComponents(components, 17*time.Millisecond, 5*time.Millisecond,
		3*time.Millisecond, 7*time.Millisecond)

	want := map[string]float64{
		"ordinal_stream":              17,
		"ordinal_stream_consumer":     5,
		"ordinal_visible_preparation": 3,
		"ordinal_finish":              7,
		"bitmap_derivation":           15,
	}
	for name, expected := range want {
		if got := components[name]; got != expected {
			t.Fatalf("component %s = %v, want %v", name, got, expected)
		}
	}
}

func TestConnectorOverheadExcludesSeparatelyReportedVisibleConsumer(t *testing.T) {
	if got := connectorOverheadDuration(30*time.Millisecond, 20*time.Millisecond, 3*time.Millisecond); got != 7*time.Millisecond {
		t.Fatalf("connector overhead = %s, want 7ms", got)
	}
	if got := connectorOverheadDuration(20*time.Millisecond, 20*time.Millisecond, 3*time.Millisecond); got != 0 {
		t.Fatalf("negative connector overhead was not clamped: %s", got)
	}
}

func TestSchemaDriftFailsQueryClosedBeforeReservation(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-schema-drift")
	harness.connector.pingErr = &dataconnector.Error{Code: dataconnector.CodeSchemaDrift}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-schema-drift", "request_id": "schema-drift-1", "sql": testSummarySQL,
	})
	requireToolCode(t, err, string(dataconnector.CodeSchemaDrift))
	if len(harness.connector.requests) != 0 {
		t.Fatalf("schema drift reached Query: %d calls", len(harness.connector.requests))
	}
	records, listErr := harness.store.ListQueries(context.Background(), "task-schema-drift", 10)
	if listErr != nil {
		t.Fatalf("ListQueries: %v", listErr)
	}
	if len(records) != 0 {
		t.Fatalf("schema drift consumed a reservation: %#v", records)
	}
}

func TestDatasourceMismatchFailsQueryClosedBeforeReservation(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-datasource-mismatch")
	harness.connector.attestation.DatasourceID = "taskgate-other-source"

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-datasource-mismatch", "request_id": "datasource-mismatch-1", "sql": testSummarySQL,
	})
	requireToolCode(t, err, string(dataconnector.CodeSchemaDrift))
	if len(harness.connector.requests) != 0 {
		t.Fatalf("datasource mismatch reached Query: %d calls", len(harness.connector.requests))
	}
	records, listErr := harness.store.ListQueries(context.Background(), "task-datasource-mismatch", 10)
	if listErr != nil {
		t.Fatalf("ListQueries: %v", listErr)
	}
	if len(records) != 0 {
		t.Fatalf("datasource mismatch consumed a reservation: %#v", records)
	}
}

func TestPolicyDenialFailsBeforeConnectorAndReservation(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-policy-denied")

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-policy-denied", "request_id": "policy-denied-1",
		"sql": "SELECT employee_name FROM expense_summary",
	})
	requireToolCode(t, err, string(sqlpolicy.CodeColumnNotAllowed))
	if len(harness.connector.requests) != 0 {
		t.Fatalf("policy-denied query reached connector: %d calls", len(harness.connector.requests))
	}
	records, listErr := harness.store.ListQueries(context.Background(), "task-policy-denied", 10)
	if listErr != nil {
		t.Fatalf("ListQueries: %v", listErr)
	}
	if len(records) != 0 {
		t.Fatalf("policy denial consumed a reservation: %#v", records)
	}
}

func TestRevocationBlocksNewQueriesWithoutCancellingInFlightQuery(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-revoke-in-flight")
	started := make(chan struct{})
	release := make(chan struct{})
	harness.connector.started = started
	harness.connector.release = release

	type callResult struct {
		value map[string]any
		err   error
	}
	finished := make(chan callResult, 1)
	go func() {
		value, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
			"task_id": "task-revoke-in-flight", "request_id": "in-flight-1", "sql": testSummarySQL,
		})
		finished <- callResult{value: value, err: err}
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("query did not reach connector")
	}

	revoked := mustCallGatewayTool(t, harness.service, harness.alice, "revoke_task", map[string]any{
		"task_id": "task-revoke-in-flight", "reason": "operator revoked task",
	})
	if revoked["terminal_reason"] != control.TerminalRevoked || revoked["in_flight_queries_cancelled"] != false {
		t.Fatalf("unexpected revocation response: %#v", revoked)
	}
	close(release)
	select {
	case completed := <-finished:
		if completed.err != nil || completed.value["status"] != control.QueryCompleted {
			t.Fatalf("in-flight query was not allowed to settle: value=%#v err=%v", completed.value, completed.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight query did not settle after revocation")
	}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-revoke-in-flight", "request_id": "after-revoke-1", "sql": testSummarySQL,
	})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
}

func TestArchivedTaskResultsStayReadableUntilRetentionPurge(t *testing.T) {
	tests := []struct {
		name    string
		taskID  string
		archive func(t *testing.T, harness *gatewayHarness, taskID string)
	}{
		{
			name:   "revoked",
			taskID: "task-retention-revoked",
			archive: func(t *testing.T, harness *gatewayHarness, taskID string) {
				t.Helper()
				archived := mustCallGatewayTool(t, harness.service, harness.alice, "revoke_task", map[string]any{
					"task_id": taskID, "reason": "operator revoked task",
				})
				if archived["terminal_reason"] != control.TerminalRevoked {
					t.Fatalf("revoked terminal reason = %#v", archived)
				}
			},
		},
		{
			name:   "expired",
			taskID: "task-retention-expired",
			archive: func(t *testing.T, harness *gatewayHarness, taskID string) {
				t.Helper()
				expiredAt := harness.clock.value
				task, err := harness.store.TransitionTask(context.Background(), control.TaskTransition{
					TaskID: taskID, ExpectedFrom: control.TaskActive, To: control.TaskArchived,
					Reason: control.TerminalExpired, Actor: "system", ExpiresAt: &expiredAt,
				})
				if err != nil {
					t.Fatalf("TransitionTask expired: %v", err)
				}
				if task.TerminalReason != control.TerminalExpired {
					t.Fatalf("expired terminal reason = %+v", task)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newGatewayHarness(t)
			harness.createActiveSummaryTask(t, test.taskID)
			query := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
				"task_id": test.taskID, "request_id": test.name + "-query-1", "sql": testSummarySQL,
			})
			queryID, ok := query["query_id"].(string)
			if !ok || queryID == "" {
				t.Fatalf("query result omitted query_id: %#v", query)
			}
			test.archive(t, harness, test.taskID)

			stored := mustCallGatewayTool(t, harness.service, harness.alice, "get_query_result", map[string]any{
				"task_id": test.taskID, "query_id": queryID,
			})
			storedJSON, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("marshal stored result: %v", err)
			}
			if !strings.Contains(string(storedJSON), "sensitive-row") {
				t.Fatalf("archived task did not retain owner result: %s", storedJSON)
			}
			listed := mustCallGatewayTool(t, harness.service, harness.alice, "list_receipts", map[string]any{
				"task_id": test.taskID,
			})
			receipts, ok := listed["receipts"].([]map[string]any)
			if !ok || len(receipts) != 1 || receipts[0]["query_id"] != queryID {
				t.Fatalf("receipt listing before purge = %#v", listed)
			}

			purged, err := harness.store.PurgeEncryptedResultsBefore(context.Background(), harness.clock.value.Add(time.Second))
			if err != nil {
				t.Fatalf("PurgeEncryptedResultsBefore: %v", err)
			}
			if purged != 1 {
				t.Fatalf("purged rows = %d, want 1", purged)
			}
			_, err = callGatewayTool(harness.service, harness.alice, "get_query_result", map[string]any{
				"task_id": test.taskID, "query_id": queryID,
			})
			requireToolCode(t, err, apierr.CodeNotFound)
			afterPurge := mustCallGatewayTool(t, harness.service, harness.alice, "list_receipts", map[string]any{
				"task_id": test.taskID,
			})
			retainedReceipts, ok := afterPurge["receipts"].([]map[string]any)
			if !ok || len(retainedReceipts) != 1 || retainedReceipts[0]["query_id"] != queryID {
				t.Fatalf("receipt listing after purge = %#v", afterPurge)
			}
		})
	}
}

func TestQueryEncodingFailureSettlesActualUsage(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-encoding-failure")
	harness.connector.result = dataconnector.Result{
		Columns:      []dataconnector.Column{{Name: "month", DataTypeOID: 25}},
		Rows:         [][]any{{make(chan int)}},
		RowCount:     1,
		DatabaseTime: 7 * time.Millisecond,
	}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-encoding-failure", "request_id": "encoding-failure-1", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded with a JSON-unsupported result")
	}

	record := requireSingleFailedQuery(t, harness, "task-encoding-failure")
	requireChargedUsage(t, record, 1, 7, resultEncodingFailed)
}

func TestQueryFinalizationFailureSettlesActualUsage(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-finalization-failure")
	if _, err := harness.store.DB().ExecContext(context.Background(), `
CREATE FUNCTION force_result_finalization_failure_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'forced result finalization failure';
END;
$$;
CREATE TRIGGER force_result_finalization_failure
BEFORE INSERT ON encrypted_query_results
FOR EACH ROW EXECUTE FUNCTION force_result_finalization_failure_fn()`); err != nil {
		t.Fatalf("create finalization failure trigger: %v", err)
	}
	harness.connector.result.DatabaseTime = 11 * time.Millisecond

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-finalization-failure", "request_id": "finalization-failure-1", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded despite forced result finalization failure")
	}

	record := requireSingleFailedQuery(t, harness, "task-finalization-failure")
	requireChargedUsage(t, record, 1, 11, resultFinalizationFailed)
}

func TestFailedSettlementMakesServiceUnreadyUntilBackgroundRetrySucceeds(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-settlement-retry")
	if _, err := harness.store.DB().ExecContext(context.Background(), `
CREATE FUNCTION force_query_settlement_failure_fn() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'forced query settlement failure';
END;
$$;
CREATE TRIGGER force_query_settlement_failure
BEFORE UPDATE OF status ON query_records
FOR EACH ROW WHEN (NEW.status IN ('COMPLETED','FAILED'))
EXECUTE FUNCTION force_query_settlement_failure_fn()`); err != nil {
		t.Fatalf("create settlement failure trigger: %v", err)
	}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-settlement-retry", "request_id": "settlement-retry-1", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded despite forced settlement failure")
	}
	if err := harness.service.ReadyError(); err == nil {
		t.Fatal("ReadyError = nil with a pending settlement retry")
	}

	records, err := harness.store.ListQueries(context.Background(), "task-settlement-retry", 10)
	if err != nil {
		t.Fatalf("list reserved query: %v", err)
	}
	if len(records) != 1 || records[0].Status != control.QueryReserved {
		t.Fatalf("queries before retry = %#v, want one RESERVED query", records)
	}
	if _, err := harness.store.DB().ExecContext(context.Background(), `DROP TRIGGER force_query_settlement_failure ON query_records`); err != nil {
		t.Fatalf("drop settlement failure trigger: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for harness.service.ReadyError() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := harness.service.ReadyError(); err != nil {
		t.Fatalf("ReadyError after retry = %v, want nil", err)
	}
	record := requireSingleFailedQuery(t, harness, "task-settlement-retry")
	requireChargedUsage(t, record, 1, 2, resultFinalizationFailed)
}

func TestConnectorAmbiguityChargesReservationAndUsesStableCode(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-connector-failure")
	harness.connector.result = dataconnector.Result{
		Rows:         [][]any{{"partial"}, {"partial-2"}},
		RowCount:     2,
		DatabaseTime: 9 * time.Millisecond,
	}
	harness.connector.queryErr = &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-connector-failure", "request_id": "connector-failure-1", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded despite connector error")
	}

	record := requireSingleQuery(t, harness, "task-connector-failure")
	if record.Status != control.QueryIndeterminate {
		t.Fatalf("query status = %s, want %s", record.Status, control.QueryIndeterminate)
	}
	requireChargedUsage(t, record, 500, 5000, string(dataconnector.CodeQueryTimeout))
	replay := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-connector-failure", "request_id": "connector-failure-1", "sql": testSummarySQL,
	})
	if replay["status"] != control.QueryIndeterminate || replay["idempotent_replay"] != true || len(harness.connector.requests) != 1 {
		t.Fatalf("indeterminate request was not terminally replayed: %#v calls=%d", replay, len(harness.connector.requests))
	}
}

func TestDefinitePreExecutionConnectorFailureReleasesAndNeverRetriesRequestID(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-connection-failure")
	harness.connector.queryErr = &dataconnector.Error{Code: dataconnector.CodeConnection}

	arguments := map[string]any{
		"task_id": "task-connection-failure", "request_id": "connection-failure-1", "sql": testSummarySQL,
	}
	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", arguments)
	requireToolCode(t, err, string(dataconnector.CodeConnection))
	record := requireSingleQuery(t, harness, "task-connection-failure")
	if record.Status != control.QueryReleased || record.ChargedQueries != 0 || record.ChargedRows != 0 || record.ChargedDBMS != 0 {
		t.Fatalf("definite pre-execution failure was charged: %+v", record)
	}
	replay := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", arguments)
	if replay["status"] != control.QueryReleased || replay["idempotent_replay"] != true {
		t.Fatalf("released request did not replay its status: %#v", replay)
	}
	if len(harness.connector.requests) != 1 {
		t.Fatalf("released request was executed again: %d connector calls", len(harness.connector.requests))
	}
	budget, err := harness.store.GetBudget(context.Background(), "task-connection-failure")
	if err != nil || budget.Usage != (control.BudgetUsage{}) {
		t.Fatalf("released request changed used budget: %+v, %v", budget, err)
	}
}

func TestGrantExpiryBoundsStatementAndQueryTimeoutsAndRejectsExpiredGrant(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-expiry-bound")
	grant, err := harness.store.GetGrant(context.Background(), "task-expiry-bound")
	if err != nil {
		t.Fatalf("get grant: %v", err)
	}
	grantWindow := 2 * time.Second
	harness.clock.value = grant.ExpiresAt.Add(-grantWindow)

	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-expiry-bound", "request_id": "expiry-bound-1", "sql": testSummarySQL,
	})
	if err != nil {
		t.Fatalf("query within grant lifetime: %v", err)
	}
	if len(harness.connector.requests) != 1 || len(harness.connector.deadlineRemaining) != 1 {
		t.Fatalf("connector calls = %d, deadlines = %d, want one each", len(harness.connector.requests), len(harness.connector.deadlineRemaining))
	}
	statementTimeout := harness.connector.requests[0].StatementTimeout
	if statementTimeout <= 0 || statementTimeout > grantWindow {
		t.Fatalf("statement timeout = %v, want within remaining %v grant", statementTimeout, grantWindow)
	}
	queryTimeout := harness.connector.deadlineRemaining[0]
	if queryTimeout <= 0 || queryTimeout > grantWindow {
		t.Fatalf("query context timeout = %v, want within remaining %v grant", queryTimeout, grantWindow)
	}

	harness.clock.value = grant.ExpiresAt
	_, err = callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-expiry-bound", "request_id": "expiry-bound-2", "sql": testSummarySQL,
	})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
	if len(harness.connector.requests) != 1 {
		t.Fatalf("expired grant reached connector: %d calls", len(harness.connector.requests))
	}
}

func TestNarrowedSignedGrantControlsReservationAndStatementTimeout(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createNarrowedSummaryTask(t, "task-narrowed-execution", func(core *domain.TaskGrantCoreV1) {
		core.Budget.MaxQueries = 2
		core.Budget.MaxResultRows = 7
		core.Budget.MaxDBMS = 1_500
		core.Budget.PerQueryTimeoutMS = 750
	})

	result := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-narrowed-execution", "request_id": "narrowed-execution-1", "sql": testSummarySQL,
	})
	if result["status"] != control.QueryCompleted {
		t.Fatalf("narrowed query status = %#v, want %s", result["status"], control.QueryCompleted)
	}
	if len(harness.connector.requests) != 1 {
		t.Fatalf("connector calls = %d, want 1", len(harness.connector.requests))
	}
	request := harness.connector.requests[0]
	if request.MaxRows != 7 || request.StatementTimeout != 750*time.Millisecond {
		t.Fatalf("connector bounds = (%d rows, %v), want (7 rows, 750ms)", request.MaxRows, request.StatementTimeout)
	}
	record := requireSingleSettledQuery(t, harness, "task-narrowed-execution")
	if record.ReservedRows != 7 || record.ReservedDBMS != 750 {
		t.Fatalf("reservation = (%d rows, %dms), want (7 rows, 750ms)", record.ReservedRows, record.ReservedDBMS)
	}
}

func TestQuerySettlementPreservesObservedDBMSWhenClamped(t *testing.T) {
	reservation := control.BudgetReservation{QueryID: "query-clamped", AllowedRows: 10, AllowedDBMS: 500}
	settlement := querySettlement("query-clamped", dataconnector.Result{
		RowCount: 10, DatabaseTime: 1500 * time.Millisecond, Truncated: true,
	}, time.Now(), reservation)
	if settlement.Rows != 10 || settlement.DBMS != reservation.AllowedDBMS || settlement.ObservedDBMS != 1500 {
		t.Fatalf("querySettlement = %+v, want rows=10 charged DBMS=500 observed DBMS=1500", settlement)
	}
}

func requireSingleSettledQuery(t *testing.T, harness *gatewayHarness, taskID string) control.QueryRecord {
	t.Helper()
	record := requireSingleQuery(t, harness, taskID)
	if record.Status != control.QueryCompleted {
		t.Fatalf("query status = %s, want %s", record.Status, control.QueryCompleted)
	}
	return record
}

func gatewayOrdinalContentCounts(t *testing.T, harness *gatewayHarness) [4]int64 {
	t.Helper()
	var counts [4]int64
	if err := harness.store.DB().QueryRowContext(context.Background(), `SELECT
 (SELECT count(*) FROM v4_bitmap_containers),
 (SELECT count(*) FROM v4_bitmap_sets),
 (SELECT count(*) FROM v4_dynamic_facts),
 (SELECT count(*) FROM v4_observations)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func requireSingleFailedQuery(t *testing.T, harness *gatewayHarness, taskID string) control.QueryRecord {
	t.Helper()
	record := requireSingleQuery(t, harness, taskID)
	if record.Status != control.QueryFailed {
		t.Fatalf("query status = %s, want %s", record.Status, control.QueryFailed)
	}
	return record
}

func requireSingleQuery(t *testing.T, harness *gatewayHarness, taskID string) control.QueryRecord {
	t.Helper()
	records, err := harness.store.ListQueries(context.Background(), taskID, 10)
	if err != nil {
		t.Fatalf("list queries: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("query records = %d, want 1", len(records))
	}
	return records[0]
}

func requireChargedUsage(t *testing.T, record control.QueryRecord, rows, databaseMS int64, errorCode string) {
	t.Helper()
	if record.ResultRows != rows || record.ResultDBMS != databaseMS {
		t.Fatalf("result usage = (%d rows, %dms), want (%d rows, %dms)", record.ResultRows, record.ResultDBMS, rows, databaseMS)
	}
	if record.ChargedQueries != 1 || record.ChargedRows != rows || record.ChargedDBMS != databaseMS {
		t.Fatalf("charged usage = (%d queries, %d rows, %dms), want (1, %d, %dms)", record.ChargedQueries, record.ChargedRows, record.ChargedDBMS, rows, databaseMS)
	}
	if record.ErrorCode != errorCode {
		t.Fatalf("error code = %q, want %q", record.ErrorCode, errorCode)
	}
}
