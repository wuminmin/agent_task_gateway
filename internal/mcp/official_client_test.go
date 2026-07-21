package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (transport bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.Header.Set("Authorization", "Bearer "+transport.token)
	return transport.base.RoundTrip(copy)
}

func TestOfficialGoClientProtocolCompatibility(t *testing.T) {
	t.Parallel()
	auth := NewStaticAuthenticator([]TokenIdentity{{
		Token:     "official-client-token",
		Principal: Principal{ID: "alice", Subject: "alice", Role: "query"},
	}})
	server, err := NewServer(auth, testTools{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: bearerTransport{token: "official-client-token", base: http.DefaultTransport},
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "taskbound-test", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: httpServer.URL, HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("official client initialize: %v", err)
	}
	defer session.Close()
	if result := session.InitializeResult(); result == nil || result.ServerInfo == nil || result.ServerInfo.Name != "taskbound-agent-data-gateway" {
		t.Fatalf("unexpected initialize result: %#v", result)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("official client tools/list: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "hello" {
		t.Fatalf("unexpected tools: %#v", tools.Tools)
	}
	call, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "hello", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("official client tools/call: %v", err)
	}
	if call.IsError || call.StructuredContent == nil || len(call.Content) == 0 {
		t.Fatalf("unexpected call result: %#v", call)
	}
}
