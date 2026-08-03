package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

type recordingProductionConnector struct {
	queryRequest     dataconnector.QueryRequest
	pairRequest      dataconnector.QueryPairRequest
	streamRequest    dataconnector.QueryPairStreamRequest
	queryResult      dataconnector.Result
	pairResult       dataconnector.QueryPairResult
	streamResult     dataconnector.QueryPairStreamResult
	attestation      dataconnector.Attestation
	queryErr         error
	pairErr          error
	streamErr        error
	pingErr          error
	attestationErr   error
	queryCalls       int
	pairCalls        int
	streamCalls      int
	pingCalls        int
	attestationCalls int
	closeCalls       int
}

func (connector *recordingProductionConnector) Query(_ context.Context,
	request dataconnector.QueryRequest) (dataconnector.Result, error) {
	connector.queryCalls++
	connector.queryRequest = request
	return connector.queryResult, connector.queryErr
}

func (connector *recordingProductionConnector) QueryPair(_ context.Context,
	request dataconnector.QueryPairRequest) (dataconnector.QueryPairResult, error) {
	connector.pairCalls++
	connector.pairRequest = request
	return connector.pairResult, connector.pairErr
}

func (connector *recordingProductionConnector) QueryPairStream(_ context.Context,
	request dataconnector.QueryPairStreamRequest) (dataconnector.QueryPairStreamResult, error) {
	connector.streamCalls++
	connector.streamRequest = request
	return connector.streamResult, connector.streamErr
}

func (connector *recordingProductionConnector) Ping(context.Context) error {
	connector.pingCalls++
	return connector.pingErr
}

func (connector *recordingProductionConnector) Attestation(context.Context) (dataconnector.Attestation, error) {
	connector.attestationCalls++
	return connector.attestation, connector.attestationErr
}

func (connector *recordingProductionConnector) Close() { connector.closeCalls++ }

type recordingProvenanceSink struct{}

func (*recordingProvenanceSink) Begin(context.Context, []dataconnector.Column) error { return nil }
func (*recordingProvenanceSink) Row(context.Context, []any) error                    { return nil }

func TestCountedProductionConnectorPreservesPairInterfacesAndCountsStatements(t *testing.T) {
	inner := &recordingProductionConnector{
		pairResult: dataconnector.QueryPairResult{
			Visible: dataconnector.Result{RowCount: 3}, Provenance: dataconnector.Result{RowCount: 5},
		},
		streamResult: dataconnector.QueryPairStreamResult{
			Visible:         dataconnector.Result{RowCount: 7},
			Provenance:      dataconnector.StreamResult{RowCount: 11},
			VisibleSinkTime: 13 * time.Millisecond,
		},
	}
	connector := &countedProductionConnector{inner: inner}
	if connector.Count() != 0 {
		t.Fatal("a semantic replay with no connector invocation was counted")
	}

	pairRequest := dataconnector.QueryPairRequest{
		Visible:    dataconnector.QueryRequest{SQL: "SELECT visible", MaxRows: 17},
		Provenance: dataconnector.QueryRequest{SQL: "SELECT provenance", MaxRows: 19},
	}
	pair, err := connector.QueryPair(t.Context(), pairRequest)
	if err != nil || !reflect.DeepEqual(pair, inner.pairResult) ||
		!reflect.DeepEqual(inner.pairRequest, pairRequest) || inner.pairCalls != 1 {
		t.Fatalf("QueryPair forwarding = %#v, %v, request %#v, calls %d", pair, err,
			inner.pairRequest, inner.pairCalls)
	}
	if connector.Count() != 2 {
		t.Fatalf("successful QueryPair count = %d, want 2", connector.Count())
	}

	connector.Reset()
	streamRequest := dataconnector.QueryPairStreamRequest{
		Visible:        dataconnector.QueryRequest{SQL: "SELECT streamed_visible", MaxRows: 23},
		Provenance:     dataconnector.QueryRequest{SQL: "SELECT streamed_provenance", MaxRows: 29},
		ProvenanceSink: &recordingProvenanceSink{},
	}
	streamed, err := connector.QueryPairStream(t.Context(), streamRequest)
	if err != nil || !reflect.DeepEqual(streamed, inner.streamResult) ||
		!reflect.DeepEqual(inner.streamRequest, streamRequest) || inner.streamCalls != 1 {
		t.Fatalf("QueryPairStream forwarding = %#v, %v, request %#v, calls %d", streamed, err,
			inner.streamRequest, inner.streamCalls)
	}
	if connector.Count() != 2 {
		t.Fatalf("successful QueryPairStream count = %d, want 2", connector.Count())
	}
}

func TestCountedProductionConnectorForwardsErrorsWithoutFallback(t *testing.T) {
	pairErr := errors.New("pair failed")
	streamErr := errors.New("stream failed")
	inner := &recordingProductionConnector{pairErr: pairErr, streamErr: streamErr}
	connector := &countedProductionConnector{inner: inner}

	if _, err := connector.QueryPair(t.Context(), dataconnector.QueryPairRequest{}); !errors.Is(err, pairErr) {
		t.Fatalf("QueryPair error = %v, want original error", err)
	}
	if _, err := connector.QueryPairStream(t.Context(), dataconnector.QueryPairStreamRequest{}); !errors.Is(err, streamErr) {
		t.Fatalf("QueryPairStream error = %v, want original error", err)
	}
	if inner.queryCalls != 0 || inner.pairCalls != 1 || inner.streamCalls != 1 || connector.Count() != 4 {
		t.Fatalf("error forwarding fabricated a fallback: query=%d pair=%d stream=%d count=%d",
			inner.queryCalls, inner.pairCalls, inner.streamCalls, connector.Count())
	}
}

func TestCountedProductionConnectorForwardsBaseOperations(t *testing.T) {
	queryErr := errors.New("query failed")
	pingErr := errors.New("ping failed")
	attestationErr := errors.New("attestation failed")
	inner := &recordingProductionConnector{queryErr: queryErr, pingErr: pingErr,
		attestation: dataconnector.Attestation{Database: "bound"}, attestationErr: attestationErr}
	connector := &countedProductionConnector{inner: inner}
	request := dataconnector.QueryRequest{SQL: "SELECT base", MaxRows: 31}

	if _, err := connector.Query(t.Context(), request); !errors.Is(err, queryErr) ||
		!reflect.DeepEqual(inner.queryRequest, request) || connector.Count() != 1 {
		t.Fatalf("Query forwarding = %v, request %#v, count %d", err, inner.queryRequest, connector.Count())
	}
	if err := connector.Ping(t.Context()); !errors.Is(err, pingErr) {
		t.Fatalf("Ping error = %v, want original error", err)
	}
	attestation, err := connector.Attestation(t.Context())
	if !errors.Is(err, attestationErr) || attestation != inner.attestation {
		t.Fatalf("Attestation forwarding = %#v, %v", attestation, err)
	}
	connector.Close()
	if connector.Count() != 1 || inner.pingCalls != 1 || inner.attestationCalls != 1 || inner.closeCalls != 1 {
		t.Fatalf("base forwarding counters = connector %d, ping %d, attestation %d, close %d",
			connector.Count(), inner.pingCalls, inner.attestationCalls, inner.closeCalls)
	}
}
