package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

type capturingApproval struct {
	mu     sync.Mutex
	drafts map[string]approval.DraftRequest
}

func newCapturingApproval() *capturingApproval {
	return &capturingApproval{drafts: make(map[string]approval.DraftRequest)}
}

func (adapter *capturingApproval) CreateDraft(_ context.Context,
	request approval.DraftRequest) (approval.DraftResponse, error) {
	if err := approval.ValidateAuthorizationSnapshot(request); err != nil {
		return approval.DraftResponse{}, err
	}
	taskID := request.Manifest.TaskID
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if _, duplicate := adapter.drafts[taskID]; duplicate {
		return approval.DraftResponse{}, errors.New("OA draft already exists for task")
	}
	adapter.drafts[taskID] = request
	return approval.DraftResponse{
		DraftID: "rq5-draft-" + taskID, State: "draft", URL: "https://oa.invalid/rq5/" + taskID,
	}, nil
}

func (adapter *capturingApproval) take(taskID string) (approval.DraftRequest, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	draft, found := adapter.drafts[taskID]
	if !found {
		return approval.DraftRequest{}, errors.New("OA draft was not captured")
	}
	delete(adapter.drafts, taskID)
	return draft, nil
}

type callbackEvent struct {
	EventID         string                      `json:"event_id"`
	TaskID          string                      `json:"task_id"`
	DraftID         string                      `json:"draft_id"`
	Status          string                      `json:"status"`
	Actor           string                      `json:"actor"`
	OccurredAt      time.Time                   `json:"occurred_at"`
	CatalogVersion  string                      `json:"catalog_version"`
	CallbackContext string                      `json:"callback_context,omitempty"`
	ManifestDigest  string                      `json:"manifest_digest"`
	ApprovedGrant   *approval.TaskGrantCoreV1   `json:"approved_grant,omitempty"`
	ApprovalReceipt *approval.ApprovalReceiptV1 `json:"approval_receipt,omitempty"`
}

func requestAndApprove(ctx context.Context, service *gateway.Service, store *control.Store,
	adapter *capturingApproval, principal mcp.Principal, secret, parentTaskID string) (control.Task, error) {
	arguments := map[string]any{
		"objective":     "Verify immutable daily reporting publication routing",
		"data_products": []string{"daily_lineitem"},
		"columns": map[string][]string{"daily_lineitem": {
			"dataset_partition", "l_orderkey", "l_linenumber", "l_extendedprice",
		}},
		"scopes": map[string]any{"dataset_partition": "1"},
	}
	if parentTaskID != "" {
		arguments["parent_task_id"] = parentTaskID
	}
	result, err := callTool(ctx, service, principal, "request_data_task", arguments)
	if err != nil {
		return control.Task{}, err
	}
	taskID, ok := result["task_id"].(string)
	if !ok || taskID == "" {
		return control.Task{}, errors.New("request_data_task returned no task_id")
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		return control.Task{}, err
	}
	draft, err := adapter.take(taskID)
	if err != nil {
		return control.Task{}, err
	}
	now := time.Now().UTC()
	submitted := callbackEvent{
		EventID: "rq5-submit-" + taskID, TaskID: taskID, DraftID: task.ApprovalRef,
		Status: "submitted", Actor: principal.Subject, OccurredAt: now,
		CatalogVersion: task.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest,
	}
	if err := sendSignedCallback(ctx, service, secret, submitted); err != nil {
		return control.Task{}, fmt.Errorf("submit OA callback: %w", err)
	}

	core, err := domain.CoreFromManifest(draft.Manifest, draft.ManifestDigest, now)
	if err != nil {
		return control.Task{}, err
	}
	callbackStatus := "approved"
	decision := approval.ApprovalDecisionApprove
	if parentTaskID != "" {
		parentGrant, err := store.GetGrant(ctx, parentTaskID)
		if err != nil {
			return control.Task{}, err
		}
		parentProtocol, err := approval.DecodeTaskGrantV1(parentGrant.ApprovalReceipt)
		if err != nil || approval.VerifyTaskGrantV1(approval.DemoReceiptVerifier([]byte(secret)), parentProtocol) != nil {
			return control.Task{}, errors.New("parent signed grant is invalid before delegated approval")
		}
		// A delegated draft is bounded at request time. Real OA processing moves
		// the receipt timestamp forward, so approving the unchanged TTL could
		// extend the child's absolute expiry beyond its parent. Exercise the
		// public narrowed-callback path and recompute the remaining signed TTL.
		if err := parentProtocol.Core.CheckDelegation(core); err != nil {
			remainingTTLMS := parentProtocol.Core.ExpiresAt.Sub(now).Milliseconds()
			if remainingTTLMS <= 0 || remainingTTLMS >= core.Budget.TaskTTLMS {
				return control.Task{}, fmt.Errorf("delegated grant cannot be safely narrowed: %w", err)
			}
			core.Budget.TaskTTLMS = remainingTTLMS
			core.ExpiresAt = now.Add(time.Duration(remainingTTLMS) * time.Millisecond)
			if err := parentProtocol.Core.CheckDelegation(core); err != nil {
				return control.Task{}, fmt.Errorf("narrowed delegated grant exceeds parent: %w", err)
			}
			callbackStatus = "narrowed"
			decision = approval.ApprovalDecisionNarrow
		}
	}
	coreDigest, err := approval.GrantCoreDigest(core)
	if err != nil {
		return control.Task{}, err
	}
	receipt, err := approval.DemoReceiptSigner([]byte(secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "rq5-receipt-" + taskID,
		TaskID: taskID, Decision: decision,
		ManifestDigest: draft.ManifestDigest, ApprovedGrantDigest: coreDigest,
		ApproverID: "bob", IssuedAt: now,
	})
	if err != nil {
		return control.Task{}, err
	}
	approved := callbackEvent{
		EventID: "rq5-approve-" + taskID, TaskID: taskID, DraftID: task.ApprovalRef,
		Status: callbackStatus, Actor: "bob", OccurredAt: now,
		CatalogVersion: task.CatalogVersion, CallbackContext: draft.Manifest.CallbackContext,
		ManifestDigest: draft.ManifestDigest, ApprovedGrant: &core, ApprovalReceipt: &receipt,
	}
	if err := sendSignedCallback(ctx, service, secret, approved); err != nil {
		return control.Task{}, fmt.Errorf("approve OA callback: %w", err)
	}
	active, err := store.GetTask(ctx, taskID)
	if err != nil {
		return control.Task{}, err
	}
	if active.State != control.TaskActive {
		return control.Task{}, fmt.Errorf("approved task state = %s", active.State)
	}
	return active, nil
}

func sendSignedCallback(ctx context.Context, service *gateway.Service, secret string, event callbackEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	request := httptest.NewRequest(http.MethodPost, "/callbacks/oa", bytes.NewReader(raw)).WithContext(ctx)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-OA-Event-ID", event.EventID)
	request.Header.Set("X-OA-Timestamp", timestamp)
	request.Header.Set("X-OA-Signature", approval.Sign([]byte(secret), timestamp, raw))
	recorder := httptest.NewRecorder()
	service.OACallbackHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"ok":true}` {
		return fmt.Errorf("callback status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	return nil
}

func callTool(ctx context.Context, service *gateway.Service, principal mcp.Principal,
	name string, arguments any) (map[string]any, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	result, err := service.CallTool(ctx, principal, name, raw)
	if err != nil {
		return nil, err
	}
	structured, ok := result.Structured.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool %s returned %T", name, result.Structured)
	}
	return structured, nil
}
