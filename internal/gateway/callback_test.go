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

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
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
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest,
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

	approvedCore, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatalf("build approved core: %v", err)
	}
	approvedDigest, err := approval.GrantCoreDigest(approvedCore)
	if err != nil {
		t.Fatalf("hash approved core: %v", err)
	}
	approvedReceipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "oa-receipt-1", TaskID: taskID,
		Decision: approval.ApprovalDecisionApprove, ManifestDigest: draft.ManifestDigest,
		ApprovedGrantDigest: approvedDigest, ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatalf("sign approval receipt: %v", err)
	}
	approved := oaCallbackEvent{
		EventID: "oa-approve-1", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "approved", Actor: "bob", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovedGrant: &approvedCore, ApprovalReceipt: &approvedReceipt,
	}
	tamperedApproval := approved
	tamperedApproval.EventID = "oa-approve-tampered"
	tamperedApproval.ManifestDigest = strings.Repeat("0", 64)
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
	finalGrant, err := approval.DecodeTaskGrantV1(grant.ApprovalReceipt)
	if err != nil {
		t.Fatalf("decode persisted final grant: %v", err)
	}
	if finalGrant.ApprovalReceipt.ReceiptID != approvedReceipt.ReceiptID || grant.Subject != harness.alice.Subject || grant.CatalogVersion != harness.catalog.CatalogVersion {
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

func TestDelegatedTaskSharesRootExposureAndStopsWithParent(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createExposureV5SummaryTask(t, "task-family-root", control.ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 5})
	bob := mcp.Principal{ID: "principal-bob-agent", Subject: "bob-agent", Role: "query"}
	if err := harness.store.CreatePrincipal(context.Background(), control.Principal{
		ID: bob.ID, Subject: bob.Subject, Role: bob.Role, CreatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create delegated principal: %v", err)
	}

	request := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective":      "delegate the approved monthly summary",
		"parent_task_id": "task-family-root", "delegate_principal_id": bob.ID,
		"data_products": []string{"expense_summary"},
		"columns":       map[string][]string{"expense_summary": {"month", "total_amount"}},
		"scopes":        map[string]any{"department": []any{"销售部"}},
	})
	// expense_summary resolves through the default low route now that the frozen
	// benchmark relations share it. Only the profile name moved: the assertions
	// below still require every delegated limit to be the parent's, because
	// constrainDelegatedBudget intersects rather than inherits.
	if request["budget_source"] != "catalog_profile_intersect_parent_grant" || request["budget_profile"] != "final-v5-baseline-low-v1" {
		t.Fatalf("delegated budget provenance = %#v", request)
	}
	delegatedBudget, ok := request["budget"].(map[string]any)
	if !ok || delegatedBudget["max_release_facts"] != int64(20) || delegatedBudget["max_influence_facts"] != int64(20) || delegatedBudget["max_outcome_facts"] != int64(5) {
		t.Fatalf("delegated budget was not intersected with the parent grant: %#v", request["budget"])
	}
	if delegatedBudget["exposure_profile_version"] != exposure.ProfileV5 || delegatedBudget["predicate_footprint"] == nil {
		t.Fatalf("delegated task changed its parent root-ledger semantics: %#v", request["budget"])
	}
	childID := request["task_id"].(string)
	child, err := harness.store.GetTask(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child task: %v", err)
	}
	if child.RootTaskID != "task-family-root" || child.ParentTaskID != "task-family-root" || child.PrincipalID != bob.ID {
		t.Fatalf("child lineage = %+v", child)
	}
	draft := harness.approval.requests[len(harness.approval.requests)-1]
	if draft.Manifest.RootTaskID != "task-family-root" || draft.Manifest.ParentTaskID != "task-family-root" ||
		draft.Manifest.HumanSubject != harness.alice.Subject || draft.Manifest.AgentID != bob.ID {
		t.Fatalf("delegated manifest lineage = %+v", draft.Manifest)
	}

	submitted := oaCallbackEvent{
		EventID: "oa-family-submit", TaskID: childID, DraftID: child.ApprovalRef,
		Status: "submitted", Actor: harness.alice.Subject, OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest,
	}
	if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
		t.Fatalf("delegated submit = %d %s", response.Code, response.Body.String())
	}
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatalf("build delegated core: %v", err)
	}
	coreDigest, err := approval.GrantCoreDigest(core)
	if err != nil {
		t.Fatalf("hash delegated core: %v", err)
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "oa-family-receipt", TaskID: childID,
		Decision: approval.ApprovalDecisionApprove, ManifestDigest: draft.ManifestDigest,
		ApprovedGrantDigest: coreDigest, ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatalf("sign delegated approval: %v", err)
	}
	approved := oaCallbackEvent{
		EventID: "oa-family-approve", TaskID: childID, DraftID: child.ApprovalRef,
		Status: "approved", Actor: "bob", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovedGrant: &core, ApprovalReceipt: &receipt,
	}
	if response := sendGatewayCallback(t, harness, approved, ""); response.Code != http.StatusOK {
		t.Fatalf("delegated approval = %d %s", response.Code, response.Body.String())
	}

	indexes := harness.installCatalogV4SnapshotRegistry(t)
	ordinalPlan := queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
	bound := prepareOrdinalForTest(t, harness, childID, ordinalPlan)
	entityKey, err := exposure.ComposeCanonicalKeyV2("base-entity",
		"travel.expense_summary",
		"month", "text", "s:2026-01",
		"department", "text", "s:销售部",
		"expense_type", "text", "s:机票",
	)
	if err != nil {
		t.Fatalf("compose V4 fixture entity key: %v", err)
	}
	handle, found := indexes["expense-summary-v1"].LookupRowHandle(entityKey)
	if !found {
		t.Fatalf("V4 fixture entity %q has no row handle", entityKey)
	}
	provenanceValues := map[string]any{
		"month": "2026-01", "department": "销售部", "expense_type": "机票", "total_amount": "1680.00",
		bound.Program.Sources[0].HandleAlias: uint64(handle),
	}
	provenanceColumns := make([]dataconnector.Column, 0, len(bound.ProvenanceFields))
	provenanceRow := make([]any, 0, len(bound.ProvenanceFields))
	for _, field := range bound.ProvenanceFields {
		value, present := provenanceValues[field]
		if !present {
			t.Fatalf("V4 fixture has no provenance value for %q", field)
		}
		provenanceColumns = append(provenanceColumns, dataconnector.Column{Name: field})
		provenanceRow = append(provenanceRow, value)
	}
	harness.connector.result = dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month"}, {Name: "total_amount"}},
		Rows:    [][]any{{"2026-01", "1680.00"}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}
	harness.connector.provenanceResult = dataconnector.Result{
		Columns: provenanceColumns, Rows: [][]any{provenanceRow}, RowCount: 1, DatabaseTime: time.Millisecond,
	}
	plan := map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}}
	rootResult := mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": "task-family-root", "request_id": "family-root-query", "plan": plan,
	})
	childResult := mustCallGatewayTool(t, harness.service, bob, "execute_plan", map[string]any{
		"task_id": childID, "request_id": "family-child-query", "plan": plan,
	})
	if rootResult["exposure"].(control.ExposureCharge).ChargedReleaseFacts == 0 ||
		childResult["exposure"].(control.ExposureCharge).ChargedReleaseFacts != 0 ||
		childResult["exposure"].(control.ExposureCharge).RootTaskID != "task-family-root" {
		t.Fatalf("family exposure was not conserved: root=%+v child=%+v", rootResult["exposure"], childResult["exposure"])
	}

	mustCallGatewayTool(t, harness.service, harness.alice, "revoke_task", map[string]any{
		"task_id": "task-family-root", "reason": "delegation test",
	})
	_, err = callGatewayTool(harness.service, bob, "execute_plan", map[string]any{
		"task_id": childID, "request_id": "family-child-after-revoke", "plan": plan,
	})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
}

func TestOACallbackNarrowingIsEnforcedBeforeGrantPersistence(t *testing.T) {
	harness := newGatewayHarness(t)
	requestTask := func(objective string) (string, control.Task, approval.DraftRequest) {
		result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
			"objective": objective, "data_products": []string{"expense_detail"},
			"columns": map[string][]string{"expense_detail": {"receipt_no", "employee_name", "amount"}},
			"scopes":  map[string]any{"department": []any{"销售部"}},
		})
		taskID := result["task_id"].(string)
		task, err := harness.store.GetTask(context.Background(), taskID)
		if err != nil {
			t.Fatal(err)
		}
		draft := harness.approval.requests[len(harness.approval.requests)-1]
		submitted := oaCallbackEvent{
			EventID: "submit-" + taskID, TaskID: taskID, DraftID: task.ApprovalRef,
			Status: "submitted", Actor: harness.alice.Subject, OccurredAt: harness.clock.value,
			CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
			ManifestDigest: draft.ManifestDigest,
		}
		if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
			t.Fatalf("submit callback = %d %s", response.Code, response.Body.String())
		}
		return taskID, task, draft
	}

	taskID, task, draft := requestTask("narrow employee expense detail")
	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatal(err)
	}
	core.ApprovedColumns = map[string][]string{"expense_detail": {"amount", "receipt_no"}}
	core.Budget.MaxQueries--
	core.Budget.PerQueryTimeoutMS = 2_000
	core.Budget.TaskTTLMS = 5 * 60 * 1000
	core.ExpiresAt = harness.clock.value.Add(5 * time.Minute)
	narrowed := signedFinalCallback(t, harness, task, draft, "narrowed", "bob", core)
	response := sendGatewayCallback(t, harness, narrowed, "")
	if response.Code != http.StatusOK {
		t.Fatalf("narrow callback = %d %s", response.Code, response.Body.String())
	}
	grant, err := harness.store.GetGrant(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.ApprovedColumns["expense_detail"]) != 2 || grant.Budget.Queries != core.Budget.MaxQueries || !grant.ExpiresAt.Equal(core.ExpiresAt) {
		t.Fatalf("narrowed grant was not persisted: %+v", grant)
	}

	widenTaskID, widenTask, widenDraft := requestTask("attempt widened employee expense detail")
	widenedCore, err := domain.CoreFromManifest(widenDraft.Manifest, widenDraft.ManifestDigest, harness.clock.value)
	if err != nil {
		t.Fatal(err)
	}
	widenedCore.ApprovedColumns["expense_detail"] = append(widenedCore.ApprovedColumns["expense_detail"], "department")
	widened := signedFinalCallback(t, harness, widenTask, widenDraft, "narrowed", "bob", widenedCore)
	response = sendGatewayCallback(t, harness, widened, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("widen callback = %d, want conflict; body=%s", response.Code, response.Body.String())
	}
	if _, err := harness.store.GetGrant(context.Background(), widenTaskID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("widening created a grant: %v", err)
	}
}

func TestOACallbackRejectRequiresValidSignedReceipt(t *testing.T) {
	harness := newGatewayHarness(t)
	result := mustCallGatewayTool(t, harness.service, harness.alice, "request_data_task", map[string]any{
		"objective": "review employee expenses", "data_products": []string{"expense_detail"},
		"columns": map[string][]string{"expense_detail": {"receipt_no", "amount"}},
		"scopes":  map[string]any{"department": []any{"销售部"}},
	})
	taskID := result["task_id"].(string)
	task, err := harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	draft := harness.approval.requests[0]
	submitted := oaCallbackEvent{
		EventID: "reject-submit", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "submitted", Actor: harness.alice.Subject, OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest,
	}
	if response := sendGatewayCallback(t, harness, submitted, ""); response.Code != http.StatusOK {
		t.Fatalf("submit callback = %d %s", response.Code, response.Body.String())
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "receipt-reject", TaskID: taskID,
		Decision: approval.ApprovalDecisionReject, ManifestDigest: draft.ManifestDigest,
		ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected := oaCallbackEvent{
		EventID: "reject-final", TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "rejected", Actor: "bob", OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovalReceipt: &receipt,
	}
	tampered := rejected
	tampered.EventID = "reject-tampered"
	tamperedReceipt := receipt
	tamperedReceipt.ApproverID = "mallory"
	tampered.ApprovalReceipt = &tamperedReceipt
	if response := sendGatewayCallback(t, harness, tampered, ""); response.Code != http.StatusConflict {
		t.Fatalf("tampered rejection = %d, want conflict; body=%s", response.Code, response.Body.String())
	}
	if response := sendGatewayCallback(t, harness, rejected, ""); response.Code != http.StatusOK {
		t.Fatalf("rejection callback = %d %s", response.Code, response.Body.String())
	}
	task, err = harness.store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != control.TaskArchived || task.TerminalReason != control.TerminalRejected {
		t.Fatalf("rejected task state = %+v", task)
	}
	if _, err := harness.store.GetGrant(context.Background(), taskID); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("rejection created a grant: %v", err)
	}
}

func signedFinalCallback(t *testing.T, harness *gatewayHarness, task control.Task, draft approval.DraftRequest, status, actor string, core approval.TaskGrantCoreV1) oaCallbackEvent {
	t.Helper()
	decision := approval.ApprovalDecisionApprove
	if status == "narrowed" {
		decision = approval.ApprovalDecisionNarrow
	}
	digest, err := approval.GrantCoreDigest(core)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "receipt-" + task.ID,
		TaskID: task.ID, Decision: decision, ManifestDigest: draft.ManifestDigest,
		ApprovedGrantDigest: digest, ApproverID: actor, IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatal(err)
	}
	return oaCallbackEvent{
		EventID: "decision-" + task.ID, TaskID: task.ID, DraftID: task.ApprovalRef,
		Status: status, Actor: actor, OccurredAt: harness.clock.value,
		CatalogVersion: harness.catalog.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovedGrant: &core, ApprovalReceipt: &receipt,
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
