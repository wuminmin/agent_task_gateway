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
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

type gatewayTestClock struct{ value time.Time }

func (clock *gatewayTestClock) Now() time.Time { return clock.value }

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
	provenanceResult  dataconnector.Result
	queryErr          error
	pingErr           error
	attestation       dataconnector.Attestation
	attestationErr    error
	started           chan struct{}
	release           <-chan struct{}
	startOnce         sync.Once
}

func (connector *fakeConnector) Query(ctx context.Context, request dataconnector.QueryRequest) (dataconnector.Result, error) {
	connector.requests = append(connector.requests, request)
	if deadline, ok := ctx.Deadline(); ok {
		connector.deadlineRemaining = append(connector.deadlineRemaining, time.Until(deadline))
	}
	if connector.started != nil {
		connector.startOnce.Do(func() { close(connector.started) })
	}
	if connector.release != nil {
		select {
		case <-connector.release:
		case <-ctx.Done():
			return dataconnector.Result{}, &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}
		}
	}
	return connector.result, connector.queryErr
}

func (connector *fakeConnector) QueryPair(ctx context.Context, request dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error) {
	connector.requests = append(connector.requests, request.Visible, request.Provenance)
	if deadline, ok := ctx.Deadline(); ok {
		connector.deadlineRemaining = append(connector.deadlineRemaining, time.Until(deadline))
	}
	if connector.started != nil {
		connector.startOnce.Do(func() { close(connector.started) })
	}
	if connector.release != nil {
		select {
		case <-connector.release:
		case <-ctx.Done():
			return dataconnector.QueryPairResult{}, &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}
		}
	}
	if connector.queryErr != nil {
		return dataconnector.QueryPairResult{}, connector.queryErr
	}
	provenance := connector.provenanceResult
	if provenance.Columns == nil {
		provenance = connector.result
	}
	return dataconnector.QueryPairResult{Visible: connector.result, Provenance: provenance}, nil
}

func (connector *fakeConnector) Ping(context.Context) error { return connector.pingErr }

func (connector *fakeConnector) Attestation(context.Context) (dataconnector.Attestation, error) {
	if connector.pingErr != nil {
		return dataconnector.Attestation{}, connector.pingErr
	}
	if connector.attestationErr != nil {
		return dataconnector.Attestation{}, connector.attestationErr
	}
	return connector.attestation, nil
}

type gatewayHarness struct {
	service   *Service
	store     *control.Store
	catalog   *catalog.Catalog
	approval  *fakeApproval
	connector *fakeConnector
	clock     *gatewayTestClock
	alice     mcp.Principal
	carol     mcp.Principal
	secret    string
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
	store, err := control.Open(ctx, testpostgres.SchemaDSN(t), cipher, control.WithClock(clock))
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

	approvalAdapter := &fakeApproval{}
	connector := &fakeConnector{attestation: testCatalogAttestation(t, loadedCatalog), result: dataconnector.Result{
		Columns: []dataconnector.Column{{Name: "month", DataTypeOID: 25}, {Name: "total_amount", DataTypeOID: 1700}},
		Rows:    [][]any{{"sensitive-row", 123.45}}, RowCount: 1, DatabaseTime: 2 * time.Millisecond,
	}}
	secret := "gateway-callback-test-secret"
	background, cancelBackground := context.WithCancel(context.Background())
	t.Cleanup(cancelBackground)
	service, err := New(Config{
		Catalog: loadedCatalog, Store: store, Approval: approvalAdapter,
		Connector: connector, CallbackSecret: secret,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.Now, Background: background,
	})
	if err != nil {
		t.Fatalf("create gateway service: %v", err)
	}
	return &gatewayHarness{
		service: service, store: store, catalog: loadedCatalog,
		approval: approvalAdapter, connector: connector, clock: clock,
		alice: alice, carol: carol, secret: secret,
	}
}

func testCatalogAttestation(t *testing.T, loadedCatalog *catalog.Catalog) dataconnector.Attestation {
	t.Helper()
	if len(loadedCatalog.Sources) != 1 {
		t.Fatalf("test catalog sources = %d, want 1", len(loadedCatalog.Sources))
	}
	source := loadedCatalog.Sources[0]
	if source.SchemaDigest == "" {
		t.Fatal("test catalog source is missing schema_digest")
	}
	return dataconnector.Attestation{
		DatasourceID: source.DatasourceID, Database: source.Database, User: source.User,
		PostgreSQLMajorVersion: source.PostgreSQLMajorVersion, SchemaDigest: source.SchemaDigest,
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
	harness.createSummaryTaskWithGrant(t, taskID, nil)
}

func (harness *gatewayHarness) createNarrowedSummaryTask(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1)) {
	t.Helper()
	if narrow == nil {
		t.Fatal("narrowing function is required")
	}
	harness.createSummaryTaskWithGrant(t, taskID, narrow)
}

func (harness *gatewayHarness) createSummaryTaskWithGrant(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1)) {
	harness.createSummaryTaskWithGrantAndExposure(t, taskID, narrow, control.ExposureLimits{})
}

func (harness *gatewayHarness) createExposureSummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposure(t, taskID, nil, limits)
}

func (harness *gatewayHarness) createSummaryTaskWithGrantAndExposure(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1), exposureLimits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, narrow, exposureLimits, "taskgate-exposure-v1")
}

func (harness *gatewayHarness) createExposureV2SummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil, limits, "taskgate-exposure-v2")
}

func (harness *gatewayHarness) createExposureV3SummaryTask(t *testing.T, taskID string, limits control.ExposureLimits) {
	harness.createSummaryTaskWithGrantAndExposureProfile(t, taskID, nil, limits, exposure.ProfileV3)
}

func (harness *gatewayHarness) createSummaryTaskWithGrantAndExposureProfile(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1), exposureLimits control.ExposureLimits, profile string) {
	harness.createTaskWithGrantAndExposureProfile(t, taskID, narrow, exposureLimits, profile,
		[]string{"expense_summary"}, map[string][]string{"expense_summary": {"month", "total_amount"}}, domain.SensitivityLow)
}

func (harness *gatewayHarness) createTaskWithGrantAndExposureProfile(t *testing.T, taskID string, narrow func(*domain.TaskGrantCoreV1), exposureLimits control.ExposureLimits, profile string, products []string, columns map[string][]string, sensitivity domain.Sensitivity) {
	t.Helper()
	budget := domain.Budget{
		MaxQueries: 10, MaxRows: 500, MaxDBTime: 30 * time.Second,
		PerQueryTimeout: 5 * time.Second, TaskTTL: 30 * time.Minute,
	}
	if exposureLimits.ReleaseFacts > 0 || exposureLimits.InfluenceFacts > 0 {
		budget.MaxReleaseFacts = exposureLimits.ReleaseFacts
		budget.MaxInfluenceFacts = exposureLimits.InfluenceFacts
		budget.MaxOutcomeFacts = exposureLimits.OutcomeFacts
		budget.ExposureProfileVersion = profile
	}
	pendingValue := pendingContext{
		Products:       append([]string(nil), products...),
		Columns:        columns,
		MandatoryScope: map[string]any{"department": []any{"销售部"}}, Budget: budget, Sensitivity: sensitivity,
		DatasourceID: harness.connector.attestation.DatasourceID, SchemaDigest: harness.connector.attestation.SchemaDigest,
		ApprovalMode: domain.ApprovalModeManual, Approver: "bob", CallbackContext: "seed-context-" + taskID,
	}
	manifest := approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: taskID,
		HumanSubject: harness.alice.Subject, AgentID: harness.alice.ID,
		DeclaredObjective: "summarize travel expenses", Products: pendingValue.Products,
		ApprovedColumns: pendingValue.Columns, MandatoryScope: pendingValue.MandatoryScope,
		Sensitivity: pendingValue.Sensitivity,
		Budget: approval.AuthorizationBudgetV1{
			MaxQueries: budget.MaxQueries, MaxResultRows: budget.MaxRows, MaxDBMS: budget.MaxDBTime.Milliseconds(),
			PerQueryTimeoutMS: budget.PerQueryTimeout.Milliseconds(), TaskTTLMS: budget.TaskTTL.Milliseconds(),
			MaxReleaseFacts: budget.MaxReleaseFacts, MaxInfluenceFacts: budget.MaxInfluenceFacts,
			MaxOutcomeFacts:        budget.MaxOutcomeFacts,
			ExposureProfileVersion: budget.ExposureProfileVersion,
		},
		CatalogVersion: harness.catalog.CatalogVersion, CatalogSHA256: harness.catalog.SHA256,
		DatasourceID:    harness.connector.attestation.DatasourceID,
		SchemaDigest:    harness.connector.attestation.SchemaDigest,
		CallbackContext: pendingValue.CallbackContext, Nonce: "00000000000000000000000000000001",
	}
	manifestDigest, err := approval.ManifestDigest(manifest)
	if err != nil {
		t.Fatalf("hash authorization manifest: %v", err)
	}
	pending, err := json.Marshal(persistedPendingContext{
		pendingContext: pendingValue, Manifest: manifest, ManifestDigest: manifestDigest,
	})
	if err != nil {
		t.Fatalf("marshal pending context: %v", err)
	}
	if err := harness.store.CreateTask(context.Background(), control.Task{
		ID: taskID, PrincipalID: harness.alice.ID, Objective: "summarize travel expenses",
		State: control.TaskAwaitingApproval, CatalogVersion: harness.catalog.CatalogVersion,
		Sensitivity: string(sensitivity), RequestedBudget: json.RawMessage(`{}`),
		RequestContext: pending, ApprovalRef: "seed-draft-" + taskID,
		CreatedAt: harness.clock.value, UpdatedAt: harness.clock.value,
	}); err != nil {
		t.Fatalf("create active-task seed: %v", err)
	}
	core, err := domain.CoreFromManifest(manifest, manifestDigest, harness.clock.value)
	if err != nil {
		t.Fatalf("build grant core: %v", err)
	}
	decision := approval.ApprovalDecisionApprove
	if narrow != nil {
		narrow(&core)
		if err := core.Validate(); err != nil {
			t.Fatalf("validate narrowed grant core: %v", err)
		}
		decision = approval.ApprovalDecisionNarrow
	}
	coreDigest, err := approval.GrantCoreDigest(core)
	if err != nil {
		t.Fatalf("hash grant core: %v", err)
	}
	receipt, err := approval.DemoReceiptSigner([]byte(harness.secret)).SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "seed-receipt-" + taskID,
		TaskID: taskID, Decision: decision, ManifestDigest: manifestDigest,
		ApprovedGrantDigest: coreDigest, ApproverID: "bob", IssuedAt: harness.clock.value,
	})
	if err != nil {
		t.Fatalf("sign approval receipt: %v", err)
	}
	finalGrantJSON, err := approval.EncodeTaskGrantV1(approval.TaskGrantV1{
		Version: domain.TaskGrantV1Version, Core: core, ApprovalReceipt: receipt,
	})
	if err != nil {
		t.Fatalf("encode final grant: %v", err)
	}
	_, err = harness.store.ApplyApprovalCallback(context.Background(), control.ApprovalCallback{
		EventID: "seed-event-" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		Event: control.ApprovalEvent{
			TaskID: taskID, Actor: "bob", Decision: "approved",
			Payload: json.RawMessage(`{"source":"test"}`), CreatedAt: harness.clock.value,
		},
		ExpectedState: control.TaskAwaitingApproval, NewState: control.TaskActive,
		Grant: &control.TaskGrant{
			TaskID: taskID, Subject: harness.alice.Subject, Purpose: "summarize travel expenses",
			ApprovedProducts: append([]string(nil), products...),
			ApprovedColumns:  columns,
			MandatoryScope:   json.RawMessage(`{"department":["销售部"]}`), SensitivityCeiling: string(sensitivity),
			Budget: control.BudgetLimits{
				Queries: core.Budget.MaxQueries, Rows: core.Budget.MaxResultRows, DBMS: core.Budget.MaxDBMS,
			},
			Exposure: control.ExposureGrant{
				Limits: control.ExposureLimits{ReleaseFacts: core.Budget.MaxReleaseFacts,
					InfluenceFacts: core.Budget.MaxInfluenceFacts, OutcomeFacts: core.Budget.MaxOutcomeFacts},
				ProfileVersion: core.Budget.ExposureProfileVersion,
			},
			ExpiresAt: core.ExpiresAt, CatalogVersion: harness.catalog.CatalogVersion,
			CatalogDigest: core.CatalogSHA256, DatasourceID: core.DatasourceID, SchemaDigest: core.SchemaDigest,
			ApprovalReceipt: finalGrantJSON, CreatedAt: harness.clock.value,
		},
		Response: []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatalf("approve active-task seed: %v", err)
	}
}
