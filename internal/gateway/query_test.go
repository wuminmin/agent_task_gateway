package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

const testSummarySQL = "SELECT month, total_amount FROM expense_summary"

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

func requireSingleSettledQuery(t *testing.T, harness *gatewayHarness, taskID string) control.QueryRecord {
	t.Helper()
	record := requireSingleQuery(t, harness, taskID)
	if record.Status != control.QueryCompleted {
		t.Fatalf("query status = %s, want %s", record.Status, control.QueryCompleted)
	}
	return record
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
