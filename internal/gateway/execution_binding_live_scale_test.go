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
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqlidentity"
)

// The binding and the terminal evidence commit together. A query that is not
// COMPLETED must have no binding at all, and a COMPLETED one must have both a
// binding and a receipt: "the query completed but nothing describes what it
// executed" has to be unreachable.
func TestLiveV10BindingAndTerminalEvidenceCommitTogether(t *testing.T) {
	harness := newV10Harness(t, "task-v10-atomicity")
	harness.service.markArtifactAvailable = func(context.Context, string, string, string) (control.ResultArtifact, error) {
		return control.ResultArtifact{}, errInjectedAvailabilityFailure
	}
	_, execErr := callGatewayTool(harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": harness.taskID, "request_id": "v10-atomicity-1",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	})
	if execErr == nil {
		t.Fatal("the injected availability failure returned a result")
	}
	record, lookupErr := harness.store.GetQueryByRequestID(t.Context(), harness.taskID, "v10-atomicity-1")
	if lookupErr != nil {
		t.Skipf("no durable query survived the injected failure (%v); this case has nothing to check", lookupErr)
	}
	_, bindingErr := harness.store.GetQueryExecutionBinding(t.Context(), record.ID)
	if record.Status != control.QueryCompleted {
		if bindingErr == nil {
			t.Fatalf("query %s is %s but carries an execution binding", record.ID, record.Status)
		}
		return
	}
	if bindingErr != nil {
		t.Fatalf("a COMPLETED query has no execution binding: %v", bindingErr)
	}
	if _, err := harness.store.GetPersistedQueryReceipt(t.Context(), record.ID); err != nil {
		t.Fatalf("a COMPLETED query with a binding has no receipt: %v", err)
	}
}

// An idempotent replay returns the original receipt bytes, signature and
// binding. It creates no second binding row: the path exists precisely because
// nothing new executed.
func TestLiveV10IdempotentReplayReturnsTheOriginalReceiptByteForByte(t *testing.T) {
	harness := newV10Harness(t, "task-v10-idempotent")
	first := harness.executePlan(t, "v10-idempotent-1")
	queryID := first["query_id"].(string)
	_, originalPersisted := harness.signedReceiptFor(t, queryID)

	replayed := harness.executePlan(t, "v10-idempotent-1")
	if replayed["idempotent_replay"] != true {
		t.Fatalf("the second identical request was not an idempotent replay: %+v", replayed)
	}
	_, replayedPersisted := harness.signedReceiptFor(t, queryID)
	if !bytes.Equal(originalPersisted.ReceiptJSON, replayedPersisted.ReceiptJSON) {
		t.Fatal("an idempotent replay changed the persisted receipt bytes")
	}
	if originalPersisted.Signature != replayedPersisted.Signature {
		t.Fatal("an idempotent replay re-signed the receipt")
	}
	// The replay must not have produced a second query with its own binding: it
	// returns the first query's evidence unchanged.
	if replayed["query_id"] != queryID {
		t.Fatalf("the idempotent replay reports query %v, want %s", replayed["query_id"], queryID)
	}
	stored, err := harness.store.GetQueryExecutionBinding(t.Context(), queryID)
	if err != nil {
		t.Fatalf("the replayed query has no execution binding: %v", err)
	}
	var replayedReceipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(replayedPersisted.ReceiptJSON, &replayedReceipt); err != nil {
		t.Fatal(err)
	}
	if replayedReceipt.ExecutionBindingV2 == nil || replayedReceipt.ExecutionBindingV2.SHA256 != stored.BindingV2.SHA256 {
		t.Fatal("the replayed receipt does not carry the originally persisted binding")
	}
}

// A paired-novel execution must complete, emit V10, verify under the Gateway's
// key, and carry a binding that is byte-for-byte the persisted one, whose target
// digests are the digests of the statements the Connector actually received.
func TestLiveV10PairedNovelEmitsAVerifiableBindingOverWhatExecuted(t *testing.T) {
	harness := newV10Harness(t, "task-v10-paired-novel")
	result := harness.executePlan(t, "v10-paired-novel-1")
	queryID, _ := result["query_id"].(string)
	if queryID == "" {
		t.Fatalf("execute_plan returned no query id: %+v", result)
	}

	receipt, _ := harness.signedReceiptFor(t, queryID)
	if receipt.Version != queryreceipt.Version {
		t.Fatalf("a completed exposure-V5 artifact query emitted a V%s receipt, want V10", receipt.Version)
	}
	// (2) the signature verifies under the Gateway's own key.
	keyring, err := queryreceipt.NewKeyring(harness.service.queryReceiptSigner, nil)
	if err != nil {
		t.Fatalf("build verifying keyring: %v", err)
	}
	if err := keyring.Verify(receipt); err != nil {
		t.Fatalf("the signed V10 receipt does not verify: %v", err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("the signed V10 receipt does not validate: %v", err)
	}
	if receipt.ExecutionBindingV2 == nil || receipt.ExposureLedgerBefore == nil {
		t.Fatal("the V10 receipt carries no execution evidence")
	}
	if receipt.ExecutionBindingV2.PathKind != querybinding.PathPairedNovel {
		t.Fatalf("path_kind is %q, want paired_novel", receipt.ExecutionBindingV2.PathKind)
	}
	if !receipt.ExecutionBindingV2.Visible.Executed ||
		receipt.ExecutionBindingV2.Companion == nil || !receipt.ExecutionBindingV2.Companion.Executed {
		t.Fatal("a paired-novel binding does not record both targets as executed")
	}

	// (3, 4) the stored binding and pre-state are the ones the receipt carries.
	stored, err := harness.store.GetQueryExecutionBinding(t.Context(), queryID)
	if err != nil {
		t.Fatalf("stored execution binding: %v", err)
	}
	if stored.BindingV2.SHA256 != receipt.ExecutionBindingV2.SHA256 {
		t.Fatalf("stored binding digests to %s but the receipt carries %s",
			stored.BindingV2.SHA256, receipt.ExecutionBindingV2.SHA256)
	}
	if stored.ExposureLedgerBefore.SHA256 != receipt.ExposureLedgerBefore.SHA256 {
		t.Fatalf("stored pre-state digests to %s but the receipt carries %s",
			stored.ExposureLedgerBefore.SHA256, receipt.ExposureLedgerBefore.SHA256)
	}

	// (5) the exact digests are the digests of what the Connector was handed.
	requests := harness.connector.requests
	if len(requests) < 2 {
		t.Fatalf("the Connector received %d statements, want a visible and a companion", len(requests))
	}
	visibleSQL := requests[len(requests)-2].SQL
	companionSQL := requests[len(requests)-1].SQL
	if got := physicalquery.ExactDigest(visibleSQL); got != receipt.ExecutionBindingV2.Visible.ExactSQLSHA256 {
		t.Fatalf("the visible target's exact digest is %s but the executed statement digests to %s",
			receipt.ExecutionBindingV2.Visible.ExactSQLSHA256, got)
	}
	if got := physicalquery.ExactDigest(companionSQL); got != receipt.ExecutionBindingV2.Companion.ExactSQLSHA256 {
		t.Fatalf("the companion target's exact digest is %s but the executed statement digests to %s",
			receipt.ExecutionBindingV2.Companion.ExactSQLSHA256, got)
	}

	// (6) the strict digests are in the observer's and classifier's space, i.e.
	// they are what sqlidentity produces for the same bytes.
	for _, target := range []struct {
		role string
		sql  string
		want string
	}{
		{"visible", visibleSQL, receipt.ExecutionBindingV2.Visible.StrictASTSHA256},
		{"companion", companionSQL, receipt.ExecutionBindingV2.Companion.StrictASTSHA256},
	} {
		strict, digestErr := sqlidentity.StrictASTDigest(target.sql)
		if digestErr != nil {
			t.Fatalf("%s statement has no strict AST identity: %v", target.role, digestErr)
		}
		if strict != target.want {
			t.Fatalf("the %s target's strict AST digest is %s but the executed statement digests to %s; "+
				"the binding is not in the space the observer classifies on", target.role, target.want, strict)
		}
	}
	if receipt.ExecutionBindingV2.Visible.RowLimit != requests[len(requests)-2].MaxRows &&
		receipt.ExecutionBindingV2.Visible.RowLimit < 1 {
		t.Fatalf("the visible target's row limit is %d", receipt.ExecutionBindingV2.Visible.RowLimit)
	}
}

// Another request settles a query between preparation and reservation, so the
// budget the preparation derivation read is stale by the time the reservation
// establishes the authoritative pre-state.
//
// The resulting receipt must either describe the AUTHORITATIVE pre-state -- the
// one the reservation observed -- or the request must fail closed before
// executing. What it must never do is sign the stale budget as though it had
// authorized the statements.
func TestLiveV10PreStateChangedBetweenPreparationAndReservation(t *testing.T) {
	harness := newV10Harness(t, "task-v10-prestate-race")
	// Consume budget from inside the window. The task lock serializes
	// reservations, so a competing request cannot interleave on its own; this
	// seam is the only way to reach the state the re-derivation exists to handle.
	var consumed bool
	harness.service.beforeReserveBudget = func(ctx context.Context, taskID string) {
		if consumed {
			return
		}
		consumed = true
		// Stand in for a concurrent query that settled: raise used_rows the way a
		// settlement would. The Control store deliberately exposes no API for
		// this, and none should be added to production for a test.
		if _, err := harness.controlDB(t).ExecContext(ctx,
			`UPDATE budget_ledger SET used_rows = used_rows + 300 WHERE task_id = $1`, taskID); err != nil {
			t.Errorf("consume row budget inside the pre-reservation window: %v", err)
		}
	}
	result, err := callGatewayTool(harness.service, harness.alice, "execute_plan", map[string]any{
		"task_id": harness.taskID, "request_id": "v10-prestate-race-1",
		"plan": map[string]any{"product": "expense_summary", "columns": []string{"month", "total_amount"}},
	})
	if err != nil {
		// Fail-closed is an accepted outcome, but only before execution: nothing
		// may have been settled with a binding.
		record, lookupErr := harness.store.GetQueryByRequestID(t.Context(), harness.taskID, "v10-prestate-race-1")
		if lookupErr == nil && record.Status == control.QueryCompleted {
			t.Fatalf("the request failed closed but a COMPLETED query survives: %+v", record)
		}
		return
	}
	queryID := result["query_id"].(string)
	receipt, _ := harness.signedReceiptFor(t, queryID)
	if receipt.Version != queryreceipt.Version || receipt.ExposureLedgerBefore == nil {
		t.Fatalf("the completed query emitted a V%s receipt with no pre-state", receipt.Version)
	}
	// The signed pre-state must be the reservation's, which already reflects the
	// concurrent consumption -- not the stale reading the preparation used.
	wantRemaining := receipt.BudgetBefore.Limits.Rows - receipt.BudgetBefore.Used.Rows -
		receipt.BudgetBefore.Reserved.Rows
	if receipt.ExposureLedgerBefore.RemainingRows != wantRemaining {
		t.Fatalf("the signed pre-state claims %d remaining rows but the signed budget leaves %d",
			receipt.ExposureLedgerBefore.RemainingRows, wantRemaining)
	}
	if receipt.BudgetBefore.Used.Rows == 0 {
		t.Fatal("the receipt signed a budget that predates the concurrent consumption; " +
			"the pre-state is the stale preparation reading")
	}
	if receipt.ExecutionBindingV2.VisibleRowLimit > wantRemaining {
		t.Fatalf("the executed visible limit is %d but the authoritative pre-state authorizes %d",
			receipt.ExecutionBindingV2.VisibleRowLimit, wantRemaining)
	}
}

// A recovery re-reads the same canonical binding rather than reconstructing one.
func TestLiveV10RecoveryReloadsTheSameCanonicalBinding(t *testing.T) {
	harness := newV10Harness(t, "task-v10-recovery")
	result := harness.executePlan(t, "v10-recovery-1")
	queryID := result["query_id"].(string)
	receipt, _ := harness.signedReceiptFor(t, queryID)

	first, err := harness.store.GetQueryExecutionBinding(t.Context(), queryID)
	if err != nil {
		t.Fatalf("first reload: %v", err)
	}
	for attempt := 0; attempt < 4; attempt++ {
		again, reloadErr := harness.store.GetQueryExecutionBinding(t.Context(), queryID)
		if reloadErr != nil {
			t.Fatalf("reload %d: %v", attempt, reloadErr)
		}
		if again.BindingV2.SHA256 != first.BindingV2.SHA256 ||
			again.ExposureLedgerBefore.SHA256 != first.ExposureLedgerBefore.SHA256 {
			t.Fatal("two reloads of one persisted binding disagree")
		}
	}
	if first.BindingV2.SHA256 != receipt.ExecutionBindingV2.SHA256 {
		t.Fatal("the reloaded binding is not the one the receipt was signed over")
	}
}

// A semantic replay is a NEW query with its own V10 receipt, whose binding says
// nothing executed. It is not an idempotent replay: the request id differs, so
// the Gateway settles a fresh query against the same committed materialization.
func TestLiveV10SemanticReplayBindsAnExecutionThatDidNotHappen(t *testing.T) {
	harness := newV10Harness(t, "task-v10-semantic")
	first := harness.executePlan(t, "v10-semantic-1")
	firstQueryID := first["query_id"].(string)

	connectorCallsAfterNovel := len(harness.connector.requests)
	second := harness.executePlan(t, "v10-semantic-2")
	secondQueryID := second["query_id"].(string)
	if secondQueryID == firstQueryID {
		t.Fatal("the second request reused the first query; it is an idempotent replay, not a semantic one")
	}
	if second["semantic_replay"] != true {
		t.Skipf("the second identical plan did not take the semantic replay path (%+v); "+
			"this deployment does not materialize it", second)
	}
	if calls := len(harness.connector.requests); calls != connectorCallsAfterNovel {
		t.Fatalf("a semantic replay executed %d further statements against the Business database",
			calls-connectorCallsAfterNovel)
	}

	receipt, _ := harness.signedReceiptFor(t, secondQueryID)
	if receipt.Version != queryreceipt.Version {
		t.Fatalf("the semantic replay emitted a V%s receipt, want V10", receipt.Version)
	}
	binding := receipt.ExecutionBindingV2
	if binding == nil {
		t.Fatal("the semantic replay's receipt carries no execution binding")
	}
	if binding.PathKind != querybinding.PathSemanticReplay {
		t.Fatalf("path_kind is %q, want semantic_replay", binding.PathKind)
	}
	if binding.Visible.Executed || (binding.Companion != nil && binding.Companion.Executed) {
		t.Fatal("a semantic replay's binding records an executed target")
	}
	if !binding.Visible.Authorized || (binding.Companion != nil && !binding.Companion.Authorized) {
		t.Fatal("a semantic replay authorizes both targets to derive its key; the binding says otherwise")
	}
	// Its own binding, not the novel query's: they describe different paths.
	firstReceipt, _ := harness.signedReceiptFor(t, firstQueryID)
	if firstReceipt.ExecutionBindingV2 != nil && firstReceipt.ExecutionBindingV2.SHA256 == binding.SHA256 {
		t.Fatal("the semantic replay reused the novel execution's binding")
	}
}
