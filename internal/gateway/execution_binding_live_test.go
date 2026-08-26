package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
	"taskbound.local/agent-data-gateway/internal/sqlidentity"
)

// v10Harness is an exposure-V5 task with result artifacts enabled.
//
// Result artifacts are no longer what makes a Query Execution Binding signable
// -- V10 states its delivery mode rather than requiring an artifact intent, so
// an inline V5 execution binds too. They are kept here because the artifact
// delivery path is what the rest of these cases exercise; the inline case has
// its own test.
type v10Harness struct {
	*gatewayHarness
	taskID string
}

func newV10Harness(t *testing.T, taskID string) *v10Harness {
	t.Helper()
	harness := newGatewayHarness(t)
	harness.installCatalogV4SnapshotRegistry(t)

	backend := newGatewayArtifactMemoryBackend()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x59}, 32))
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
	harness.service.resultTTL = time.Hour
	harness.service.deliverySigningKey = []byte("v10-execution-binding-test-key")

	harness.createExposureV5SummaryTask(t, taskID, control.ExposureLimits{
		ReleaseFacts: 40, InfluenceFacts: 40, OutcomeFacts: 40,
	})
	// The Connector fixture is installed after the task exists, because it is now
	// built from the program the production preparation compiles for that task's
	// own grant rather than from a program the fixture compiled itself.
	installV10ConnectorFixture(t, harness, taskID)
	return &v10Harness{gatewayHarness: harness, taskID: taskID}
}

// installV10ConnectorFixture makes the fake Connector return a visible row and a
// matching provenance row whose row handles resolve in the live compiled
// snapshot index. Without a resolvable handle the ordinal derivation refuses the
// evidence and nothing reaches settlement.
func installV10ConnectorFixture(t *testing.T, harness *gatewayHarness, taskID string) {
	t.Helper()
	bound := prepareOrdinalForTest(t, harness, taskID, plannedSummaryQuery())
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
}

// plannedSummaryQuery is the plan executePlan submits, as a QueryPlan.
//
// The tool takes it as JSON; a case that re-prepares the executed operation
// needs the same plan as a value. Deriving one from the other would be a second
// place the fixture's query is written down, so it is stated once here and the
// tool call renders it.
func plannedSummaryQuery() queryplan.QueryPlan {
	return queryplan.QueryPlan{Product: "expense_summary", Columns: []string{"month", "total_amount"}}
}

func (harness *v10Harness) executePlan(t *testing.T, requestID string) map[string]any {
	t.Helper()
	return mustCallGatewayTool(t, harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": harness.taskID, "request_id": requestID,
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	})
}

// signedReceiptFor reads the receipt from the persisted row, not from the tool
// response. The response is a convenience; the row is the evidence.
func (harness *v10Harness) signedReceiptFor(t *testing.T, queryID string) (queryreceipt.QueryReceiptV1, control.PersistedQueryReceipt) {
	t.Helper()
	persisted, err := harness.store.GetPersistedQueryReceipt(t.Context(), queryID)
	if err != nil {
		t.Fatalf("persisted receipt for %s: %v", queryID, err)
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(persisted.ReceiptJSON, &receipt); err != nil {
		t.Fatalf("decode persisted receipt: %v", err)
	}
	return receipt, persisted
}

// --- 1, 2, 3, 4, 5, 6 --------------------------------------------------------

// --- 7 -----------------------------------------------------------------------

// --- 8 -----------------------------------------------------------------------

// --- 10 ----------------------------------------------------------------------

var errInjectedAvailabilityFailure = errors.New("injected AVAILABLE transaction failure")

// --- 9 -----------------------------------------------------------------------

// --- I2-A1 concurrency -------------------------------------------------------

// A completed query on a task with no exposure grant describes its execution
// too, and describes it in the shape that says it accounted none.
//
// This is the case the contract had no way to express. The binding and its
// exposure pre-state used to travel as a pair, so a plain query could either
// carry a fabricated empty ledger -- a signed claim that a ledger was read when
// none was -- or carry nothing and go undescribed. It went undescribed, which is
// why "every completed query states which physical statements produced its rows"
// was not an invariant before this.
func TestLiveNonExposureCompletedQueryDescribesItsExecution(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-plain-binding")

	result := mustCallGatewayTool(t, harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-plain-binding", "request_id": "plain-binding-1",
		"sql": "SELECT month, total_amount FROM expense_summary",
	})
	queryID, _ := result["query_id"].(string)
	if queryID == "" {
		t.Fatalf("query_sql returned no query id: %+v", result)
	}

	persisted, err := harness.store.GetPersistedQueryReceipt(t.Context(), queryID)
	if err != nil {
		t.Fatalf("persisted receipt: %v", err)
	}
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(persisted.ReceiptJSON, &receipt); err != nil {
		t.Fatalf("decode persisted receipt: %v", err)
	}
	keyring, err := queryreceipt.NewKeyring(harness.service.queryReceiptSigner, nil)
	if err != nil {
		t.Fatalf("build verifying keyring: %v", err)
	}
	if err := keyring.Verify(receipt); err != nil {
		t.Fatalf("the plain receipt does not verify: %v", err)
	}

	binding := receipt.ExecutionBindingV2
	if binding == nil {
		t.Fatal("a completed plain query carries no execution binding")
	}
	// The non-exposure shape, member by member. Each of these is what the
	// receipt's own validator holds it to; asserting them here is what proves
	// production BUILDS the shape rather than merely tolerating it.
	if binding.ExposureProfileVersion != "" || binding.ExposureLedgerBeforeSHA256 != "" {
		t.Fatalf("a plain query's binding names profile %q and pre-state %q",
			binding.ExposureProfileVersion, binding.ExposureLedgerBeforeSHA256)
	}
	if receipt.ExposureLedgerBefore != nil || receipt.Exposure != nil {
		t.Fatal("a plain query's receipt carries exposure evidence")
	}
	if binding.PathKind != querybinding.PathSingleQuery || binding.Companion != nil {
		t.Fatalf("a plain query bound path_kind %q with %d companions",
			binding.PathKind, boolCount(binding.Companion != nil))
	}
	if !binding.Visible.Executed {
		t.Fatal("the visible target is not recorded as executed")
	}
	if receipt.ResultDeliveryMode != queryreceipt.DeliveryInline {
		t.Fatalf("delivery mode is %q, want inline", receipt.ResultDeliveryMode)
	}
	// The executed statement is the one the binding names. This is the whole
	// point of describing the execution: the Connector's bytes and the signed
	// identity are the same statement.
	if len(harness.connector.requests) != 1 {
		t.Fatalf("connector calls = %d, want 1", len(harness.connector.requests))
	}
	executed := harness.connector.requests[0].SQL
	if got := physicalquery.ExactDigest(executed); got != binding.Visible.ExactSQLSHA256 {
		t.Fatalf("the binding names visible statement %s but the Connector received %s",
			binding.Visible.ExactSQLSHA256, got)
	}
	strict, err := sqlidentity.StrictASTDigest(executed)
	if err != nil {
		t.Fatalf("strict AST digest: %v", err)
	}
	if strict != binding.Visible.StrictASTSHA256 {
		t.Fatalf("the binding names strict AST %s but the executed statement is %s",
			binding.Visible.StrictASTSHA256, strict)
	}

	// And the row it is stored under carries no pre-state at all, rather than an
	// empty one.
	stored, err := harness.store.GetQueryExecutionBinding(t.Context(), queryID)
	if err != nil {
		t.Fatalf("stored execution binding: %v", err)
	}
	if stored.ExposureLedgerBefore != nil {
		t.Fatal("the stored row carries an exposure pre-state for a plain query")
	}
	if stored.BindingV2 == nil || stored.BindingV2.SHA256 != binding.SHA256 {
		t.Fatal("the stored binding is not the one the receipt carries")
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
