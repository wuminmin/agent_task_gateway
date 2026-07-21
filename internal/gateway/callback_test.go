package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

// Exercise the public callback handler so signature checks happen before any
// control-store mutation.
func TestOACallbackHMACSubmissionApprovalReplayAndBadSignature(t *testing.T) {
	harness := newGatewayHarness(t)
	requestResult := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":     "summarize travel expenses",
		"data_products": []string{"expense_summary"},
		"columns":       map[string][]string{"expense_summary": {"month", "total_amount"}},
		"scopes":        map[string]any{"department": []any{"销售部"}},
	})
	taskID := requestResult["task_id"].(string)
	task, err := harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load requested task: %v", err)
	}
	if len(harness.approval.requests) != 1 {
		t.Fatalf("OA draft calls = %d, want 1", len(harness.approval.requests))
	}
	draft := harness.approval.requests[0]

	submitted := oaCallbackEvent{
		EventID: "oa-submit-1", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "submitted", Actor: "Alice", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.CallbackContext,
		AuthorizationSnapshotSHA256: draft.AuthorizationSnapshotSHA256,
	}
	badSignature := sendGatewayCallback(t, harness, submitted, "v1=00")
	if badSignature.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want %d; body=%s", badSignature.Code, http.StatusUnauthorized, badSignature.Body.String())
	}
	task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("reload task after bad signature: %v", err)
	}
	if task.State != control.TaskAwaitingSubmission {
		t.Fatalf("bad signature changed task state to %s", task.State)
	}

	submitResponse := sendGatewayCallback(t, harness, submitted, "")
	if submitResponse.Code != http.StatusOK || submitResponse.Body.String() != `{"ok":true}` {
		t.Fatalf("submit response = %d %q", submitResponse.Code, submitResponse.Body.String())
	}
	task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("reload submitted task: %v", err)
	}
	if task.State != control.TaskAwaitingApproval {
		t.Fatalf("submitted task state = %s, want %s", task.State, control.TaskAwaitingApproval)
	}

	approved := oaCallbackEvent{
		EventID: "oa-approve-1", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "approved", Actor: "oa-auto", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.CallbackContext,
		AuthorizationSnapshotSHA256: draft.AuthorizationSnapshotSHA256,
		ApprovalReceipt:             "oa-receipt-1",
	}
	tamperedApproval := approved
	tamperedApproval.EventID = "oa-approve-tampered"
	tamperedApproval.AuthorizationSnapshotSHA256 = strings.Repeat("0", 64)
	tamperedResponse := sendGatewayCallback(t, harness, tamperedApproval, "")
	if tamperedResponse.Code != http.StatusConflict {
		t.Fatalf("tampered snapshot status = %d, want %d; body=%s", tamperedResponse.Code, http.StatusConflict, tamperedResponse.Body.String())
	}
	task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("reload task after tampered approval: %v", err)
	}
	if task.State != control.TaskAwaitingApproval {
		t.Fatalf("tampered snapshot changed task state to %s", task.State)
	}
	if _, err := harness.store.GetGrant(context.Background(), taskID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("tampered snapshot created a grant: %v", err)
	}

	originalPendingJSON := append([]byte(nil), task.RequestContext...)
	tamperedPending, err := decodePersistedPending(task)
	if err != nil {
		t.Fatalf("decode pending context for tamper test: %v", err)
	}
	tamperedPending.Columns = cloneColumns(tamperedPending.Columns)
	tamperedPending.Columns["expense_summary"] = append(tamperedPending.Columns["expense_summary"], "department")
	tamperedPendingJSON, err := json.Marshal(tamperedPending)
	if err != nil {
		t.Fatalf("marshal tampered pending context: %v", err)
	}
	if _, err := harness.store.DB().Exec(`UPDATE tasks SET request_context_json=$1 WHERE id=$2`, string(tamperedPendingJSON), taskID); err != nil {
		t.Fatalf("tamper pending context: %v", err)
	}
	tamperedPendingApproval := approved
	tamperedPendingApproval.EventID = "oa-approve-pending-tampered"
	tamperedPendingResponse := sendGatewayCallback(t, harness, tamperedPendingApproval, "")
	if tamperedPendingResponse.Code != http.StatusConflict {
		t.Fatalf("tampered pending status = %d, want %d; body=%s", tamperedPendingResponse.Code, http.StatusConflict, tamperedPendingResponse.Body.String())
	}
	if _, err := harness.store.GetGrant(context.Background(), taskID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("tampered pending context created a grant: %v", err)
	}
	if _, err := harness.store.DB().Exec(`UPDATE tasks SET request_context_json=$1 WHERE id=$2`, string(originalPendingJSON), taskID); err != nil {
		t.Fatalf("restore pending context: %v", err)
	}
	approveResponse := sendGatewayCallback(t, harness, approved, "")
	if approveResponse.Code != http.StatusOK || approveResponse.Body.String() != `{"ok":true}` {
		t.Fatalf("approval response = %d %q", approveResponse.Code, approveResponse.Body.String())
	}
	task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("reload approved task: %v", err)
	}
	if task.State != control.TaskActive {
		t.Fatalf("approved task state = %s, want %s", task.State, control.TaskActive)
	}
	grant, err := harness.store.GetGrant(context.Background(), taskID)
	if err != nil {
		t.Fatalf("load grant: %v", err)
	}
	if grant.ApprovalReceipt != approved.ApprovalReceipt || grant.Subject != harness.alice.Subject || grant.CatalogVersion != harness.catalog.CatalogVersion {
		t.Fatalf("unexpected approved grant: %+v", grant)
	}

	replayResponse := sendGatewayCallback(t, harness, approved, "")
	if replayResponse.Code != http.StatusOK || !bytes.Equal(replayResponse.Body.Bytes(), approveResponse.Body.Bytes()) {
		t.Fatalf("replay response = %d %q, original = %d %q", replayResponse.Code, replayResponse.Body.String(), approveResponse.Code, approveResponse.Body.String())
	}
	// Replays use the newly signed transport timestamp but retain the original
	// event occurrence time. They must still return the stored response after
	// the freshness window and after the live catalog has advanced.
	harness.clock.value = harness.clock.value.Add(6 * time.Minute)
	originalCatalogVersion := harness.catalog.CatalogVersion
	harness.catalog.CatalogVersion = "catalog-v-next"
	lateReplayResponse := sendGatewayCallback(t, harness, approved, "")
	harness.catalog.CatalogVersion = originalCatalogVersion
	if lateReplayResponse.Code != http.StatusOK || !bytes.Equal(lateReplayResponse.Body.Bytes(), approveResponse.Body.Bytes()) {
		t.Fatalf("late replay response = %d %q, original = %d %q", lateReplayResponse.Code, lateReplayResponse.Body.String(), approveResponse.Code, approveResponse.Body.String())
	}
	var callbackCount, approvalEventCount int
	if err := harness.store.DB().QueryRow(`SELECT COUNT(*) FROM callback_idempotency`).Scan(&callbackCount); err != nil {
		t.Fatalf("count callback claims: %v", err)
	}
	if err := harness.store.DB().QueryRow(`SELECT COUNT(*) FROM approval_events WHERE task_id=$1`, taskID).Scan(&approvalEventCount); err != nil {
		t.Fatalf("count approval events: %v", err)
	}
	if callbackCount != 2 || approvalEventCount != 2 {
		t.Fatalf("replay created duplicates: callbacks=%d approval_events=%d", callbackCount, approvalEventCount)
	}
}

func sendGatewayCallback(t *testing.T, harness *gatewayHarness, event oaCallbackEvent, signature string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal callback: %v", err)
	}
	timestamp := strconv.FormatInt(harness.clock.value.Unix(), 10)
	if signature == "" {
		signature = approval.Sign([]byte(harness.secret), timestamp, body)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/oa/callback", bytes.NewReader(body))
	request.Header.Set("X-OA-Event-ID", event.EventID)
	request.Header.Set("X-OA-Timestamp", timestamp)
	request.Header.Set("X-OA-Signature", signature)
	recorder := httptest.NewRecorder()
	harness.service.OACallbackHandler().ServeHTTP(recorder, request)
	return recorder
}
