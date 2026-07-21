package gateway

import (
	"context"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

const testSummarySQL = "SELECT month, total_amount FROM expense_summary"

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
		"task_id": "task-encoding-failure", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded with a JSON-unsupported result")
	}

	record := requireSingleSettledQuery(t, harness, "task-encoding-failure")
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
		"task_id": "task-finalization-failure", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded despite forced result finalization failure")
	}

	record := requireSingleSettledQuery(t, harness, "task-finalization-failure")
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
FOR EACH ROW WHEN (NEW.status = 'COMPLETED')
EXECUTE FUNCTION force_query_settlement_failure_fn()`); err != nil {
		t.Fatalf("create settlement failure trigger: %v", err)
	}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-settlement-retry", "sql": testSummarySQL,
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
	record := requireSingleSettledQuery(t, harness, "task-settlement-retry")
	requireChargedUsage(t, record, 1, 2, resultFinalizationFailed)
}

func TestConnectorErrorSettlesReturnedUsageAndStableCode(t *testing.T) {
	harness := newGatewayHarness(t)
	harness.createActiveSummaryTask(t, "task-connector-failure")
	harness.connector.result = dataconnector.Result{
		Rows:         [][]any{{"partial"}, {"partial-2"}},
		RowCount:     2,
		DatabaseTime: 9 * time.Millisecond,
	}
	harness.connector.queryErr = &dataconnector.Error{Code: dataconnector.CodeQueryTimeout}

	_, err := callGatewayTool(harness.service, harness.alice, "query_sql", map[string]any{
		"task_id": "task-connector-failure", "sql": testSummarySQL,
	})
	if err == nil {
		t.Fatal("query_sql succeeded despite connector error")
	}

	record := requireSingleSettledQuery(t, harness, "task-connector-failure")
	requireChargedUsage(t, record, 2, 9, string(dataconnector.CodeQueryTimeout))
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
		"task_id": "task-expiry-bound", "sql": testSummarySQL,
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
		"task_id": "task-expiry-bound", "sql": testSummarySQL,
	})
	requireToolCode(t, err, apierr.CodeTaskNotActive)
	if len(harness.connector.requests) != 1 {
		t.Fatalf("expired grant reached connector: %d calls", len(harness.connector.requests))
	}
}

func requireSingleSettledQuery(t *testing.T, harness *gatewayHarness, taskID string) control.QueryRecord {
	t.Helper()
	records, err := harness.store.ListQueries(context.Background(), taskID, 10)
	if err != nil {
		t.Fatalf("list queries: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("query records = %d, want 1", len(records))
	}
	if records[0].Status != control.QueryCompleted {
		t.Fatalf("query status = %s, want %s", records[0].Status, control.QueryCompleted)
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
