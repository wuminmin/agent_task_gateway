//go:build taskgate_scale

// These cases prepare an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Five of
// the seven are scanned out of the Business database, which measured 25.84 GB
// peak on a 30 GB host, so they belong on the taskgate_scale lane rather than
// holding the acceptance run open.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

func TestOrdinalExposureBudgetBPlusOneCommitsCompleteFailureOnly(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t)
	backend := newGatewayArtifactMemoryBackend()
	artifactCipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x45}, 32))
	if err != nil {
		t.Fatal(err)
	}
	artifactTempDir := t.TempDir()
	if err := os.Chmod(artifactTempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifactManager, err := resultartifact.NewManager(backend, artifactCipher, artifactTempDir)
	if err != nil {
		t.Fatal(err)
	}
	harness.service.resultArtifacts = artifactManager
	taskID := "task-v4-exposure-b-plus-one"
	requestID := "v4-exposure-b-plus-one-request"
	// This scan releases two visible base-cell facts. A release ceiling of one
	// therefore makes the attempted observation exactly B+1 in that dimension.
	harness.createExposureV4SummaryTask(t, taskID,
		control.ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 100, OutcomeFacts: 10})

	plan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
	bound := prepareOrdinalForTest(t, harness, taskID, plan)
	row := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票",
		"total_amount": json.Number("1680.00"),
	}
	visible := scanVisibleResult(t, bound.Program, []map[string]any{row})
	for index := range visible.Columns {
		switch visible.Columns[index].Name {
		case "month":
			visible.Columns[index].DataTypeOID = 25
		case "total_amount":
			visible.Columns[index].DataTypeOID = 1700
		default:
			t.Fatalf("unexpected visible artifact column %q", visible.Columns[index].Name)
		}
	}
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
	var artifacts, availableArtifacts, availabilityAudits, failureAudits, receipts int
	if err := harness.store.DB().QueryRowContext(context.Background(), `SELECT
 (SELECT status FROM v4_query_exposure_reservations WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_results WHERE query_id=$1),
 (SELECT count(*) FROM encrypted_query_result_chunks WHERE query_id=$1),
 (SELECT count(*) FROM v4_committed_materializations WHERE source_query_id=$1),
 (SELECT count(*) FROM v4_query_observations WHERE query_id=$1),
 (SELECT count(*) FROM v4_root_observations WHERE first_query_id=$1),
	(SELECT count(*) FROM result_artifacts WHERE query_id=$1),
	(SELECT count(*) FROM result_artifacts WHERE query_id=$1 AND status='AVAILABLE'),
	(SELECT count(*) FROM audit_events WHERE query_id=$1 AND event_type='QUERY_RESULT_CONSUMED'),
 (SELECT count(*) FROM audit_events WHERE query_id=$1 AND event_type='QUERY_FAILED'),
 (SELECT count(*) FROM query_receipts WHERE query_id=$1)`, record.ID).Scan(
		&reservationStatus, &encryptedResults, &encryptedChunks, &materializations,
		&queryObservations, &rootObservations, &artifacts, &availableArtifacts, &availabilityAudits,
		&failureAudits, &receipts); err != nil {
		t.Fatal(err)
	}
	if reservationStatus != "RELEASED" || encryptedResults != 0 || encryptedChunks != 0 ||
		materializations != 0 || queryObservations != 0 || rootObservations != 0 ||
		artifacts != 0 || availableArtifacts != 0 || availabilityAudits != 0 || failureAudits != 1 || receipts != 1 {
		t.Fatalf("B+1 atomic failure evidence: reservation=%s result=%d chunks=%d materialization=%d "+
			"query_observation=%d root_observation=%d artifacts=%d available=%d availability_audit=%d "+
			"failure_audit=%d receipt=%d",
			reservationStatus, encryptedResults, encryptedChunks, materializations, queryObservations,
			rootObservations, artifacts, availableArtifacts, availabilityAudits, failureAudits, receipts)
	}
	objects, err := backend.List(context.Background(), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("B+1 left unpublished artifact objects: %+v", objects)
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
	bound := prepareOrdinalForTest(t, harness, taskID, plan)
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
