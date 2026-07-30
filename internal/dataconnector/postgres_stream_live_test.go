package dataconnector

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

type recordingProvenanceSink struct {
	columns    []Column
	rows       [][]any
	beginCalls int
	rowCalls   int
	beginErr   error
	rowErrAt   int
	rowErr     error
	cancel     context.CancelFunc
}

type timedProvenanceSink struct {
	visibleDelay time.Duration
	beginDelay   time.Duration
	rowDelay     time.Duration
}

type failingVisibleSink struct {
	*recordingProvenanceSink
	err error
}

func (sink *failingVisibleSink) VisibleResult(context.Context, Result) error {
	return sink.err
}

func (sink *timedProvenanceSink) VisibleResult(context.Context, Result) error {
	time.Sleep(sink.visibleDelay)
	return nil
}

func (sink *timedProvenanceSink) Begin(context.Context, []Column) error {
	time.Sleep(sink.beginDelay)
	return nil
}

func (sink *timedProvenanceSink) Row(context.Context, []any) error {
	time.Sleep(sink.rowDelay)
	return nil
}

func (sink *recordingProvenanceSink) Begin(_ context.Context, columns []Column) error {
	sink.beginCalls++
	sink.columns = append([]Column(nil), columns...)
	return sink.beginErr
}

func (sink *recordingProvenanceSink) Row(ctx context.Context, values []any) error {
	sink.rowCalls++
	if sink.cancel != nil {
		sink.cancel()
		return ctx.Err()
	}
	if sink.rowErrAt > 0 && sink.rowCalls == sink.rowErrAt {
		return sink.rowErr
	}
	sink.rows = append(sink.rows, append([]any(nil), values...))
	return nil
}

func TestQueryPairStreamBoundsRowsAndPreservesMetadata(t *testing.T) {
	connector := newStreamTestConnector(t, 2)
	sink := &recordingProvenanceSink{}

	result, err := connector.QueryPairStream(context.Background(), QueryPairStreamRequest{
		Visible: QueryRequest{
			SQL:              `SELECT n::bigint AS visible_n FROM pg_catalog.generate_series(1, 3) AS n`,
			StatementTimeout: time.Second,
			MaxRows:          10,
		},
		Provenance: QueryRequest{
			SQL:              `SELECT n::bigint AS provenance_n FROM pg_catalog.generate_series(1, 3) AS n`,
			StatementTimeout: time.Second,
			MaxRows:          10,
		},
		ProvenanceSink: sink,
	})
	if err != nil {
		t.Fatalf("QueryPairStream() error = %v", err)
	}

	if result.Visible.RowCount != 2 || !result.Visible.Truncated {
		t.Fatalf("visible bounds = count %d, truncated %t", result.Visible.RowCount, result.Visible.Truncated)
	}
	if !reflect.DeepEqual(result.Visible.Rows, [][]any{{int64(1)}, {int64(2)}}) {
		t.Fatalf("visible rows = %#v", result.Visible.Rows)
	}
	if result.Provenance.RowCount != 2 || !result.Provenance.Truncated {
		t.Fatalf("provenance bounds = count %d, truncated %t", result.Provenance.RowCount, result.Provenance.Truncated)
	}
	if sink.beginCalls != 1 || sink.rowCalls != 2 {
		t.Fatalf("sink calls = begin %d, row %d", sink.beginCalls, sink.rowCalls)
	}
	if !reflect.DeepEqual(sink.rows, [][]any{{int64(1)}, {int64(2)}}) {
		t.Fatalf("streamed rows = %#v", sink.rows)
	}
	if !reflect.DeepEqual(sink.columns, result.Provenance.Columns) {
		t.Fatalf("sink columns = %#v, result columns = %#v", sink.columns, result.Provenance.Columns)
	}
	if len(result.Provenance.Columns) != 1 || result.Provenance.Columns[0].Name != "provenance_n" || result.Provenance.Columns[0].DataTypeOID != 20 {
		t.Fatalf("provenance columns = %#v", result.Provenance.Columns)
	}
	if result.Visible.DatabaseTime <= 0 || result.Provenance.DatabaseTime <= 0 {
		t.Fatalf("database timings = visible %s, provenance %s", result.Visible.DatabaseTime, result.Provenance.DatabaseTime)
	}
}

func TestQueryPairStreamUsesOneReadOnlyRepeatableReadSnapshot(t *testing.T) {
	connector := newStreamTestConnector(t, 10)
	sink := &recordingProvenanceSink{}
	statement := `SELECT current_setting('transaction_isolation'),
       current_setting('transaction_read_only'),
       pg_catalog.pg_current_snapshot()::text`

	result, err := connector.QueryPairStream(context.Background(), QueryPairStreamRequest{
		Visible:        QueryRequest{SQL: statement, MaxRows: 1},
		Provenance:     QueryRequest{SQL: statement, MaxRows: 1},
		ProvenanceSink: sink,
	})
	if err != nil {
		t.Fatalf("QueryPairStream() error = %v", err)
	}
	if result.Visible.RowCount != 1 || result.Provenance.RowCount != 1 || len(sink.rows) != 1 {
		t.Fatalf("row counts = visible %d, provenance %d, sink %d", result.Visible.RowCount, result.Provenance.RowCount, len(sink.rows))
	}
	visible := result.Visible.Rows[0]
	provenance := sink.rows[0]
	if visible[0] != "repeatable read" || visible[1] != "on" {
		t.Fatalf("transaction settings = isolation %#v, read_only %#v", visible[0], visible[1])
	}
	if provenance[0] != visible[0] || provenance[1] != visible[1] || provenance[2] != visible[2] {
		t.Fatalf("visible and provenance snapshots differ: %#v != %#v", visible, provenance)
	}
}

func TestQueryPairStreamSeparatesVisibleAndProvenanceConsumerTimers(t *testing.T) {
	connector := newStreamTestConnector(t, 10)
	sink := &timedProvenanceSink{
		visibleDelay: 5 * time.Millisecond,
		beginDelay:   5 * time.Millisecond,
		rowDelay:     5 * time.Millisecond,
	}

	result, err := connector.QueryPairStream(context.Background(), QueryPairStreamRequest{
		Visible:        QueryRequest{SQL: `SELECT 1::bigint`, MaxRows: 1},
		Provenance:     QueryRequest{SQL: `SELECT n::bigint FROM pg_catalog.generate_series(1, 2) AS n`, MaxRows: 2},
		ProvenanceSink: sink,
	})
	if err != nil {
		t.Fatalf("QueryPairStream() error = %v", err)
	}
	if result.VisibleSinkTime < sink.visibleDelay {
		t.Fatalf("visible sink time = %s, want at least %s", result.VisibleSinkTime, sink.visibleDelay)
	}
	wantConsumer := sink.beginDelay + 2*sink.rowDelay
	if result.Provenance.ConsumerTime < wantConsumer {
		t.Fatalf("consumer time = %s, want at least %s", result.Provenance.ConsumerTime, wantConsumer)
	}
	if result.Provenance.DatabaseTime < result.Provenance.ConsumerTime {
		t.Fatalf("stream wall time %s is below contained consumer time %s",
			result.Provenance.DatabaseTime, result.Provenance.ConsumerTime)
	}
}

func TestQueryPairStreamVisibleSinkFailureReturnsNoPartialResult(t *testing.T) {
	connector := newStreamTestConnector(t, 10)
	sentinel := errors.New("visible ordinal preparation failed")
	recorder := &recordingProvenanceSink{}
	sink := &failingVisibleSink{recordingProvenanceSink: recorder, err: sentinel}

	result, err := connector.QueryPairStream(context.Background(), QueryPairStreamRequest{
		Visible:        QueryRequest{SQL: `SELECT 1::bigint`, MaxRows: 1},
		Provenance:     QueryRequest{SQL: `SELECT 2::bigint`, MaxRows: 1},
		ProvenanceSink: sink,
	})
	if !IsCode(err, CodeQueryFailed) || !errors.Is(err, sentinel) {
		t.Fatalf("visible sink failure = %v, want %s wrapping sentinel", err, CodeQueryFailed)
	}
	if !reflect.DeepEqual(result, QueryPairStreamResult{}) {
		t.Fatalf("failed visible preparation returned partial result: %#v", result)
	}
	if recorder.beginCalls != 0 || recorder.rowCalls != 0 {
		t.Fatalf("provenance started after visible sink failure: begin=%d rows=%d", recorder.beginCalls, recorder.rowCalls)
	}

	queryResult, queryErr := connector.Query(context.Background(), QueryRequest{SQL: `SELECT 1::bigint`, MaxRows: 1})
	if queryErr != nil || queryResult.RowCount != 1 {
		t.Fatalf("query after visible sink rollback = result %#v, error %v", queryResult, queryErr)
	}
}

func TestQueryPairStreamSinkFailureReturnsNoPartialResult(t *testing.T) {
	connector := newStreamTestConnector(t, 10)
	sentinel := errors.New("ordinal derivation failed")
	sink := &recordingProvenanceSink{rowErrAt: 2, rowErr: sentinel}

	result, err := connector.QueryPairStream(context.Background(), QueryPairStreamRequest{
		Visible:        QueryRequest{SQL: `SELECT 1::bigint`, MaxRows: 1},
		Provenance:     QueryRequest{SQL: `SELECT n::bigint FROM pg_catalog.generate_series(1, 3) AS n`, MaxRows: 3},
		ProvenanceSink: sink,
	})
	if !IsCode(err, CodeQueryFailed) || !errors.Is(err, sentinel) {
		t.Fatalf("sink failure = %v, want %s wrapping sentinel", err, CodeQueryFailed)
	}
	if !reflect.DeepEqual(result, QueryPairStreamResult{}) {
		t.Fatalf("failed stream returned partial result: %#v", result)
	}
	if sink.rowCalls != 2 || !reflect.DeepEqual(sink.rows, [][]any{{int64(1)}}) {
		t.Fatalf("sink prefix = calls %d, rows %#v", sink.rowCalls, sink.rows)
	}

	// A successful query on a single-connection pool proves the failed pair
	// released its transaction instead of leaving the connection busy.
	queryResult, queryErr := connector.Query(context.Background(), QueryRequest{SQL: `SELECT 1::bigint`, MaxRows: 1})
	if queryErr != nil || queryResult.RowCount != 1 {
		t.Fatalf("query after sink rollback = result %#v, error %v", queryResult, queryErr)
	}
}

func TestQueryPairStreamCancellationIsAQueryTimeout(t *testing.T) {
	connector := newStreamTestConnector(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	sink := &recordingProvenanceSink{cancel: cancel}

	result, err := connector.QueryPairStream(ctx, QueryPairStreamRequest{
		Visible:        QueryRequest{SQL: `SELECT 1::bigint`, MaxRows: 1},
		Provenance:     QueryRequest{SQL: `SELECT n::bigint FROM pg_catalog.generate_series(1, 3) AS n`, MaxRows: 3},
		ProvenanceSink: sink,
	})
	if !IsCode(err, CodeQueryTimeout) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream = %v, want %s wrapping context cancellation", err, CodeQueryTimeout)
	}
	if !reflect.DeepEqual(result, QueryPairStreamResult{}) {
		t.Fatalf("cancelled stream returned partial result: %#v", result)
	}
}

func TestQueryPairCompatibilityCollectsStreamedRows(t *testing.T) {
	connector := newStreamTestConnector(t, 2)
	request := QueryPairRequest{
		Visible:    QueryRequest{SQL: `SELECT n::bigint AS n FROM pg_catalog.generate_series(1, 3) AS n`, MaxRows: 10},
		Provenance: QueryRequest{SQL: `SELECT n::bigint AS n FROM pg_catalog.generate_series(1, 3) AS n`, MaxRows: 10},
	}

	result, err := connector.QueryPair(context.Background(), request)
	if err != nil {
		t.Fatalf("QueryPair() error = %v", err)
	}
	want := [][]any{{int64(1)}, {int64(2)}}
	if !reflect.DeepEqual(result.Visible.Rows, want) || !reflect.DeepEqual(result.Provenance.Rows, want) {
		t.Fatalf("compatibility rows = visible %#v, provenance %#v", result.Visible.Rows, result.Provenance.Rows)
	}
	if result.Visible.RowCount != 2 || result.Provenance.RowCount != 2 || !result.Visible.Truncated || !result.Provenance.Truncated {
		t.Fatalf("compatibility bounds = visible %#v, provenance %#v", result.Visible, result.Provenance)
	}
}

func newStreamTestConnector(t *testing.T, maxRows int64) *Connector {
	t.Helper()
	dsn := testpostgres.SchemaDSN(t)
	connector, err := New(context.Background(), Config{
		DSN:              dsn,
		StatementTimeout: time.Second,
		ConnectTimeout:   time.Second,
		MaxRows:          maxRows,
		MaxConnections:   1,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(connector.Close)
	return connector
}
