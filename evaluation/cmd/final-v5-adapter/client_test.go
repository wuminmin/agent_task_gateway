package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

func TestMCPClientPreservesStructuredAttackRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"structuredContent":{"trace_id":"trace-1","error":{"code":"SQL_NOT_LOWERABLE","message":"safe message","reason":"SET_OPERATION_UNSUPPORTED","retryable_after_rewrite":true}},"content":[{"type":"text","text":"must not be parsed as evidence"}]}}`))
	}))
	defer server.Close()

	client := &mcpClient{url: server.URL, token: "test", http: server.Client()}
	err := client.call(context.Background(), "query_sql", map[string]any{"sql": "SELECT 1 UNION SELECT 1"}, &struct{}{})
	var structured *mcpCallError
	if !errors.As(err, &structured) {
		t.Fatalf("structured rejection collapsed to %T: %v", err, err)
	}
	if structured.Code != "SQL_NOT_LOWERABLE" || structured.Message != "safe message" ||
		structured.Reason != "SET_OPERATION_UNSUPPORTED" || structured.TraceID != "trace-1" ||
		!structured.RetryableAfterRewrite {
		t.Fatalf("structured rejection drifted: %+v", structured)
	}
}

func TestMCPClientRejectsUnstructuredErrorAsInvalidEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"EXPOSURE_BUDGET_EXHAUSTED"}]}}`))
	}))
	defer server.Close()

	client := &mcpClient{url: server.URL, token: "test", http: server.Client()}
	err := client.call(context.Background(), "query_sql", map[string]any{"sql": "SELECT 1"}, &struct{}{})
	var structured *mcpCallError
	if err == nil || errors.As(err, &structured) {
		t.Fatalf("unstructured text was accepted as stable attack evidence: %v", err)
	}
}

func TestMCPClientPlacesConcurrencyBindingOnActualQueryRequest(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	round, participant := stringsOfLengthForClientTest("a", 64), stringsOfLengthForClientTest("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get(gatewayapp.ConcurrencyRoundHeader) != round ||
			request.Header.Get(gatewayapp.ConcurrencyParticipantHeader) != participant ||
			request.Header.Get(gatewayapp.ConcurrencyAuthorizationHeader) != token {
			t.Errorf("actual MCP request omitted concurrency binding: %+v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":false,"structuredContent":{"query_id":"query"}}}`))
	}))
	defer server.Close()
	client := &mcpClient{url: server.URL, token: "ordinary-user-token", http: server.Client()}
	var response struct {
		QueryID string `json:"query_id"`
	}
	err := client.callWithHeaders(context.Background(), "query_sql", map[string]any{
		"task_id": "one-root", "request_id": "unique-request", "sql": "SELECT 1",
	}, &response, map[string]string{
		gatewayapp.ConcurrencyRoundHeader: round, gatewayapp.ConcurrencyParticipantHeader: participant,
		gatewayapp.ConcurrencyAuthorizationHeader: token,
	})
	if err != nil || response.QueryID != "query" {
		t.Fatalf("bound MCP query failed: response=%+v err=%v", response, err)
	}
}

func TestMCPClientRejectsHeaderInjectionBeforeNetwork(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	client := &mcpClient{url: server.URL, token: "ordinary-user-token", http: server.Client()}
	err := client.callWithHeaders(context.Background(), "query_sql", map[string]any{"sql": "SELECT 1"}, &struct{}{},
		map[string]string{gatewayapp.ConcurrencyRoundHeader: "safe\r\ninjected: value"})
	if err == nil || called {
		t.Fatalf("private-header injection reached network: called=%v err=%v", called, err)
	}
}

func stringsOfLengthForClientTest(value string, count int) string {
	result := ""
	for index := 0; index < count; index++ {
		result += value
	}
	return result
}
