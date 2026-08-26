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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

func TestCanonicalCopySurvivesAvailableTransactionFailureAndRecoversExactlyOnce(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t)
	const taskID = "task-copy-before-available"
	const requestID = "copy-before-available-1"
	// The task is created before the Connector fixture, because the fixture is
	// built from the program production prepares for this task's own grant.
	harness.createExposureV5SummaryTask(t, taskID, control.ExposureLimits{
		ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 20,
	})
	plan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
	bound := prepareOrdinalForTest(t, harness, taskID, plan)
	row := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票",
		"total_amount": json.Number("1680.00"),
	}
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{row["month"], row["total_amount"]}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
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
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	backend := newGatewayArtifactMemoryBackend()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x32}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := resultartifact.NewManager(backend, cipher, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	harness.service.resultArtifacts = manager
	harness.service.deliverySigningKey = []byte("copy-before-available-test")
	availabilityBlocked := true
	harness.service.markArtifactAvailable = func(ctx context.Context, resultID, etag, actor string) (control.ResultArtifact, error) {
		if availabilityBlocked {
			return control.ResultArtifact{}, errors.New("injected AVAILABLE transaction failure")
		}
		return harness.store.MarkResultArtifactAvailable(ctx, resultID, etag, actor)
	}

	arguments := map[string]any{
		"task_id": taskID, "request_id": requestID,
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	}
	_, executeErr := callGatewayTool(harness.service, harness.alice, "execute_plan", arguments)
	if executeErr == nil {
		t.Fatal("AVAILABLE transaction failure returned an available result")
	}
	record, err := harness.store.GetQueryByRequestID(t.Context(), taskID, requestID)
	if err != nil || record.Status != control.QueryCompleted {
		t.Fatalf("durable query after AVAILABLE failure = %+v, lookup=%v, execute=%v", record, err, executeErr)
	}
	artifact, err := harness.store.GetResultArtifactByQuery(t.Context(), record.ID)
	if err != nil || artifact.Status != control.ResultArtifactPending || artifact.ConsumedAt != nil {
		t.Fatalf("logical artifact after AVAILABLE failure = %+v, %v", artifact, err)
	}
	if _, err := backend.Stat(t.Context(), artifact.ObjectKey); err != nil {
		t.Fatalf("canonical object was not created before AVAILABLE failure: %v", err)
	}
	_, copiesAfterFailure := backend.operationCounts()
	if copiesAfterFailure != 1 {
		t.Fatalf("canonical copy calls before recovery = %d, want 1", copiesAfterFailure)
	}
	budgetBefore, err := harness.store.GetBudget(t.Context(), taskID)
	if err != nil || budgetBefore.Usage.UsedQueries != 1 {
		t.Fatalf("budget after AVAILABLE failure = %+v, %v", budgetBefore, err)
	}
	chargeBefore, err := harness.store.GetExposureCharge(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	receiptBefore, err := harness.store.GetQueryReceipt(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receiptBefore.Receipt == nil {
		t.Fatal("no receipt was persisted before artifact recovery")
	}
	var signedBefore queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(receiptBefore.Receipt.ReceiptJSON, &signedBefore); err != nil {
		t.Fatal(err)
	}
	// V10, not V8. This operation is exposure-V5 and it completed, so it has a
	// persisted QueryExecutionBindingV2; a receipt that dropped it and emitted V8
	// would be the silent downgrade the version selection refuses. The artifact
	// intent is still required here because this delivery registered a result
	// object -- V10 states its mode rather than requiring one, and an artifact
	// delivery that omitted the intent would be describing a different delivery.
	if signedBefore.Version != queryreceipt.Version || signedBefore.Exposure == nil ||
		signedBefore.Exposure.ProfileVersion != exposure.ProfileV5 || signedBefore.ArtifactIntent == nil ||
		signedBefore.ExecutionBindingV2 == nil || signedBefore.ExposureLedgerBefore == nil {
		t.Fatalf("crash-window receipt is not explicit V5 + V10 evidence: %+v", signedBefore)
	}
	intentBefore := *signedBefore.ArtifactIntent
	connectorCalls := len(harness.connector.requests)

	for tool, args := range map[string]map[string]any{
		"preview_result":   {"result_id": artifact.ResultID},
		"deliver_result":   {"result_id": artifact.ResultID, "format": "parquet"},
		"get_query_result": {"task_id": taskID, "query_id": record.ID},
	} {
		if _, err := callGatewayTool(harness.service, harness.alice, tool, args); err == nil {
			t.Fatalf("%s exposed a PENDING canonical object", tool)
		}
	}
	pendingAuditReceipt := mustCallGatewayTool(t, harness.service, harness.carol, "get_audit_receipt", map[string]any{
		"receipt_id": record.ID,
	})
	if _, exists := pendingAuditReceipt["availability_event_inclusion"]; exists {
		t.Fatalf("PENDING artifact exposed an availability event proof: %#v", pendingAuditReceipt)
	}
	router := chi.NewRouter()
	router.Handle("/api/v1/results/{result_id}/download", harness.service.ResultDownloadHandler())
	expires := harness.clock.value.Add(time.Minute).Unix()
	token := harness.service.deliverySignature(artifact.ResultID, taskID, expires)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/v1/results/%s/download?task_id=%s&expires=%d&token=%s", artifact.ResultID, taskID, expires, token), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("download status for PENDING canonical object = %d, want 404", response.Code)
	}
	getsBeforeRecovery, copiesBeforeRecovery := backend.operationCounts()

	availabilityBlocked = false
	completed, err := harness.service.ReconcilePendingArtifacts(t.Context())
	if err != nil || completed != 1 {
		t.Fatalf("recover existing canonical object = %d, %v", completed, err)
	}
	getsAfterRecovery, copiesAfterRecovery := backend.operationCounts()
	if copiesAfterRecovery != copiesBeforeRecovery || getsAfterRecovery != getsBeforeRecovery+1 {
		t.Fatalf("recovery operations: gets %d -> %d, copies %d -> %d; want one exact-digest read and no recopy",
			getsBeforeRecovery, getsAfterRecovery, copiesBeforeRecovery, copiesAfterRecovery)
	}
	after, err := harness.store.GetResultArtifactByQuery(t.Context(), record.ID)
	if err != nil || after.Status != control.ResultArtifactAvailable || after.ConsumedAt == nil {
		t.Fatalf("artifact after recovery = %+v, %v", after, err)
	}
	budgetAfter, _ := harness.store.GetBudget(t.Context(), taskID)
	chargeAfter, _ := harness.store.GetExposureCharge(t.Context(), record.ID)
	receiptAfter, _ := harness.store.GetQueryReceipt(t.Context(), record.ID)
	if !reflect.DeepEqual(budgetAfter, budgetBefore) || !reflect.DeepEqual(chargeAfter, chargeBefore) ||
		receiptAfter.Receipt == nil || !reflect.DeepEqual(receiptAfter.Receipt, receiptBefore.Receipt) {
		t.Fatalf("recovery changed settlement evidence")
	}
	var signedAfter queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(receiptAfter.Receipt.ReceiptJSON, &signedAfter); err != nil {
		t.Fatal(err)
	}
	if signedAfter.ArtifactIntent == nil || !reflect.DeepEqual(*signedAfter.ArtifactIntent, intentBefore) {
		t.Fatalf("recovery changed V8 ArtifactIntent: before=%+v after=%+v", intentBefore, signedAfter.ArtifactIntent)
	}
	if len(harness.connector.requests) != connectorCalls {
		t.Fatalf("recovery re-executed Business PostgreSQL: %d -> %d", connectorCalls, len(harness.connector.requests))
	}
	completed, err = harness.service.ReconcilePendingArtifacts(t.Context())
	if err != nil || completed != 0 {
		t.Fatalf("idempotent recovery pass = %d, %v; want no pending work", completed, err)
	}
	settlements, err := harness.store.ListAuditEvents(t.Context(), control.AuditFilter{
		QueryID: record.ID, EventType: "QUERY_V5_EXPOSURE_SETTLED", Limit: 10,
	})
	if err != nil || len(settlements) != 1 {
		t.Fatalf("exposure settlement events = %+v, %v", settlements, err)
	}
	consumed, err := harness.store.ListAuditEvents(t.Context(), control.AuditFilter{
		QueryID: record.ID, EventType: "QUERY_RESULT_CONSUMED", Limit: 10,
	})
	if err != nil || len(consumed) != 1 {
		t.Fatalf("consumption events after recovery = %+v, %v", consumed, err)
	}
	auditReceipt := mustCallGatewayTool(t, harness.service, harness.carol, "get_audit_receipt", map[string]any{
		"receipt_id": record.ID,
	})
	availability, ok := auditReceipt["availability_event_inclusion"].(map[string]any)
	if !ok || availability["terminal_event"] == nil || availability["checkpoint"] == nil {
		t.Fatalf("availability event inclusion helper = %#v", auditReceipt["availability_event_inclusion"])
	}
	// Exercise the same post-restart semantic-replay budget boundary as the
	// Compose acceptance path: the tenth and final allowed query must still
	// return its committed result even though settlement archives the task.
	for queryIndex := 2; queryIndex <= 10; queryIndex++ {
		replayArguments := map[string]any{
			"task_id": taskID, "request_id": fmt.Sprintf("copy-before-available-%d", queryIndex),
			"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
		}
		if _, err := callGatewayTool(harness.service, harness.alice, "execute_plan", replayArguments); err != nil {
			replayRecord, recordErr := harness.store.GetQueryByRequestID(t.Context(), taskID,
				replayArguments["request_id"].(string))
			replayArtifact, artifactErr := harness.store.GetResultArtifactByQuery(t.Context(), replayRecord.ID)
			replayTask, taskErr := harness.store.GetTask(t.Context(), taskID)
			t.Fatalf("final allowed semantic replay %d: %v; record=%+v/%v artifact=%+v/%v task=%+v/%v",
				queryIndex, err, replayRecord, recordErr, replayArtifact, artifactErr, replayTask, taskErr)
		}
	}
	if len(harness.connector.requests) != connectorCalls {
		t.Fatalf("semantic replay budget boundary re-executed Business PostgreSQL: %d -> %d",
			connectorCalls, len(harness.connector.requests))
	}
	archived, err := harness.store.GetTask(t.Context(), taskID)
	if err != nil || archived.State != control.TaskArchived || archived.TerminalReason != control.TerminalBudgetExhausted {
		t.Fatalf("task after final allowed query = %+v, %v", archived, err)
	}
}
