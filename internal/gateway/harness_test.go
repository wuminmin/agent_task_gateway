package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/deepseek"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

type gatewayTestClock struct{ value time.Time }

func (clock *gatewayTestClock) Now() time.Time { return clock.value }

type fakeTranslator struct {
	intent         deepseek.TaskIntent
	intentErr      error
	queryPlan      deepseek.QueryPlan
	queryErr       error
	intentCalls    int
	queryCalls     int
	intentCatalogs []string
	queryCatalogs  []string
}

func (translator *fakeTranslator) TranslateIntent(_ context.Context, _ string, logicalCatalog string) (deepseek.TaskIntent, error) {
	translator.intentCalls++
	translator.intentCatalogs = append(translator.intentCatalogs, logicalCatalog)
	return translator.intent, translator.intentErr
}

func (translator *fakeTranslator) TranslateQuery(_ context.Context, _ string, logicalCatalog string) (deepseek.QueryPlan, error) {
	translator.queryCalls++
	translator.queryCatalogs = append(translator.queryCatalogs, logicalCatalog)
	return translator.queryPlan, translator.queryErr
}

type fakeApproval struct {
	requests []approval.DraftRequest
	response approval.DraftResponse
	err      error
}

func (adapter *fakeApproval) CreateDraft(_ context.Context, request approval.DraftRequest) (approval.DraftResponse, error) {
	adapter.requests = append(adapter.requests, request)
	if adapter.err != nil {
		return approval.DraftResponse{}, adapter.err
	}
	response := adapter.response
	if response.DraftID == "" {
		response.DraftID = fmt.Sprintf("draft-%d", len(adapter.requests))
	}
	if response.URL == "" {
		response.URL = "http://oa.test/drafts/" + response.DraftID
	}
	if response.State == "" {
		response.State = "draft"
	}
	return response, nil
}

type fakeConnector struct {
	requests          []dataconnector.QueryRequest
	deadlineRemaining []time.Duration
	result            dataconnector.Result
	queryErr          error
	pingErr           error
}

func (connector *fakeConnector) Query(ctx context.Context, request dataconnector.QueryRequest) (dataconnector.Result, error) {
	connector.requests = append(connector.requests, request)
	if deadline, ok := ctx.Deadline(); ok {
		connector.deadlineRemaining = append(connector.deadlineRemaining, time.Until(deadline))
	}
	return connector.result, connector.queryErr
}

func (connector *fakeConnector) Ping(context.Context) error { return connector.pingErr }

type gatewayHarness struct {
	service    *Service
	store      *control.Store
	catalog    *catalog.Catalog
	translator *fakeTranslator
	approval   *fakeApproval
	connector  *fakeConnector
	clock      *gatewayTestClock
	alice      mcp.Principal
	carol      mcp.Principal
	secret     string
}

func newGatewayHarness(t *testing.T) *gatewayHarness {
	t.Helper()
	ctx := context.Background()
	loadedCatalog, err := catalog.Load(filepath.Join("..", "..", "config", "catalog.yaml"))
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatalf("create result cipher: %v", err)
	}
	clock := &gatewayTestClock{value: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)}
	store, err := control.Open(ctx, filepath.Join(t.TempDir(), "gateway.db")+`?_foreign_keys=on&_busy_timeout=5000`, cipher, control.WithClock(clock))
	if err != nil {
		t.Fatalf("open control store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	alice := mcp.Principal{ID: "principal-alice", Subject: "alice", Role: "query"}
	carol := mcp.Principal{ID: "principal-carol", Subject: "carol", Role: "auditor"}
	for _, principal := range []mcp.Principal{alice, carol} {
		if err := store.CreatePrincipal(ctx, control.Principal{
			ID: principal.ID, Subject: principal.Subject, Role: principal.Role, CreatedAt: clock.value,
		}); err != nil {
			t.Fatalf("create principal %s: %v", principal.Subject, err)
		}
	}

	translator := &fakeTranslator{intent: deepseek.TaskIntent{
		Objective: "summarize travel expenses", DataProducts: []string{"expense_summary"},
		Columns: map[string][]string{"expense_summary": {"month", "total_amount"}},
		Scopes:  map[string]any{"department": []any{"销售部"}},
	}}
	approvalAdapter := &fakeApproval{}
	connector := &fakeConnector{result: dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{"sensitive-row", 123.45}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}}
	secret := "gateway-callback-test-secret"
	background, cancelBackground := context.WithCancel(context.Background())
	t.Cleanup(cancelBackground)
	service, err := New(Config{
		Catalog: loadedCatalog, Store: store, Approval: approvalAdapter, Translator: translator,
		Connector: connector, CallbackSecret: secret,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.Now, Background: background,
	})
	if err != nil {
		t.Fatalf("create gateway service: %v", err)
	}
	return &gatewayHarness{
		service: service, store: store, catalog: loadedCatalog, translator: translator,
		approval: approvalAdapter, connector: connector, clock: clock,
		alice: alice, carol: carol, secret: secret,
	}
}

func callGatewayTool(service *Service, principal mcp.Principal, name string, arguments any) (map[string]any, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	result, err := service.CallTool(context.Background(), principal, name, raw)
	if err != nil {
		return nil, err
	}
	structured, ok := result.Structured.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool %s returned %T, want map[string]any", name, result.Structured)
	}
	return structured, nil
}

func mustCallGatewayTool(t *testing.T, service *Service, principal mcp.Principal, name string, arguments any) map[string]any {
	t.Helper()
	result, err := callGatewayTool(service, principal, name, arguments)
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func requireToolCode(t *testing.T, err error, code string) {
	t.Helper()
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) {
		t.Fatalf("error = %T %v, want *mcp.ToolError", err, err)
	}
	if toolErr.Code != code {
		t.Fatalf("tool error code = %q, want %q (error: %v)", toolErr.Code, code, err)
	}
}

func (harness *gatewayHarness) createActiveSummaryTask(t *testing.T, taskID string) {
	t.Helper()
	budget := domain.Budget{
		MaxQueries: 10, MaxRows: 500, MaxDBTime: 30 * time.Second,
		PerQueryTimeout: 5 * time.Second, TaskTTL: 30 * time.Minute,
	}
	pendingValue := pendingContext{
		Products:       []string{"expense_summary"},
		Columns:        map[string][]string{"expense_summary": {"month", "total_amount"}},
		MandatoryScope: map[string]any{"department": []any{"销售部"}}, Budget: budget, Sensitivity: domain.SensitivityLow,
		ApprovalMode: domain.ApprovalModeAuto, CallbackContext: "seed-context-" + taskID,
	}
	snapshotRequest := approval.DraftRequest{
		TaskID: taskID, Requester: harness.alice.Subject, Objective: "summarize travel expenses",
		DataProducts: pendingValue.Products, ApprovedColumns: pendingValue.Columns,
		MandatoryScope: pendingValue.MandatoryScope, Sensitivity: string(pendingValue.Sensitivity),
		Budget: approval.DraftBudget{
			MaxQueries: budget.MaxQueries, MaxRows: budget.MaxRows, MaxDBMS: budget.MaxDBTime.Milliseconds(),
			QueryTimeoutMS: budget.PerQueryTimeout.Milliseconds(), TaskTTLMS: budget.TaskTTL.Milliseconds(),
		},
		ApprovalMode: string(pendingValue.ApprovalMode), CatalogVersion: harness.catalog.CatalogVersion,
		CallbackContext: pendingValue.CallbackContext,
	}
	snapshotSHA256, err := approval.AuthorizationSnapshotSHA256(snapshotRequest)
	if err != nil {
		t.Fatalf("hash pending context: %v", err)
	}
	pending, err := json.Marshal(persistedPendingContext{
		pendingContext: pendingValue, AuthorizationSnapshotSHA256: snapshotSHA256,
	})
	if err != nil {
		t.Fatalf("marshal pending context: %v", err)
	}
	if err := harness.store.CreateTask(context.Background(), control.Task{
		ID: taskID, PrincipalID: harness.alice.ID, Objective: "summarize travel expenses",
		State: control.TaskAwaitingApproval, CatalogVersion: harness.catalog.CatalogVersion,
		Sensitivity: string(domain.SensitivityLow), RequestedBudget: json.RawMessage(`{}`),
		RequestContext: pending, ApprovalRef: "seed-draft-" + taskID,
		CreatedAt: harness.clock.value, UpdatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create active-task seed: %v", err)
	}
	expiresAt := harness.clock.value.Add(budget.TaskTTL)
	_, err = harness.store.ApplyApprovalCallback(context.Background(), control.ApprovalCallback{
		EventID: "seed-event-" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		Event: control.ApprovalEvent{
			TaskID: taskID, Actor: "oa-auto", Decision: "approved",
			Payload: json.RawMessage(`{"source":"test"}`), CreatedAt: harness.clock.value,
		},
		ExpectedState: control.TaskAwaitingApproval, NewState: control.TaskActive,
		Grant: &control.TaskGrant{
			TaskID: taskID, Subject: harness.alice.Subject, Purpose: "summarize travel expenses",
			ApprovedProducts: []string{"expense_summary"},
			ApprovedColumns:  map[string][]string{"expense_summary": {"month", "total_amount"}},
			MandatoryScope:   json.RawMessage(`{"department":["销售部"]}`), SensitivityCeiling: string(domain.SensitivityLow),
			Budget:    control.BudgetLimits{Queries: 10, Rows: 500, DBMS: 30_000},
			ExpiresAt: expiresAt, CatalogVersion: harness.catalog.CatalogVersion,
			ApprovalReceipt: "seed-receipt-" + taskID, CreatedAt: harness.clock.value,
		},
		Response: []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatalf("approve active-task seed: %v", err)
	}
}
