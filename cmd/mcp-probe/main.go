// Command mcp-probe verifies a running Gateway with the official Go MCP
// client. It is built and run only by the Compose acceptance suite.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"taskbound.local/agent-data-gateway/internal/control"
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

func main() {
	endpoint := requiredEnv("MCP_ENDPOINT")
	token := requiredEnv("MCP_TOKEN")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	httpClient := &http.Client{Timeout: 10 * time.Second, Transport: bearerTransport{token: token, base: http.DefaultTransport}}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "taskbound-compose-probe", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		fatal("initialize with official MCP client", err)
	}
	defer session.Close()
	initialized := session.InitializeResult()
	if initialized == nil || initialized.ServerInfo == nil || initialized.ServerInfo.Name != "taskbound-agent-data-gateway" {
		fatal("validate initialize result", errors.New("unexpected server info"))
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		fatal("list tools with official MCP client", err)
	}
	found := false
	for _, tool := range listed.Tools {
		if tool.Name == "list_data_products" {
			found = true
			break
		}
	}
	if !found {
		fatal("validate official client tools/list", errors.New("list_data_products missing"))
	}
	called, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "list_data_products", Arguments: map[string]any{}})
	if err != nil {
		fatal("call tool with official MCP client", err)
	}
	if called.IsError || called.StructuredContent == nil || len(called.Content) == 0 {
		fatal("validate official client tools/call", errors.New("unexpected tool result"))
	}
	encoded, err := json.Marshal(called.StructuredContent)
	if err != nil {
		fatal("encode official tool result", err)
	}
	var products struct {
		CatalogVersion string `json:"catalog_version"`
	}
	if err := json.Unmarshal(encoded, &products); err != nil || products.CatalogVersion == "" {
		fatal("read catalog version from official tool result", errors.New("catalog_version missing"))
	}
	seedOtherPrincipalTask(ctx, products.CatalogVersion)
	fmt.Println("ok - official Go MCP client initialized, listed tools, and called /mcp")
}

// seedOtherPrincipalTask creates one real control-plane task for the Compose
// ownership test. The helper container mounts the isolated integration volume;
// no test-only endpoint or identity is added to the production Gateway.
func seedOtherPrincipalTask(ctx context.Context, catalogVersion string) {
	databasePath := os.Getenv("CONTROL_DB_PATH")
	dataKey := os.Getenv("GATEWAY_DATA_KEY")
	if databasePath == "" && dataKey == "" {
		return
	}
	if databasePath == "" || dataKey == "" {
		fatal("seed another principal task", errors.New("CONTROL_DB_PATH and GATEWAY_DATA_KEY must be set together"))
	}
	key, err := control.ParseAES256Key(dataKey)
	if err != nil {
		fatal("parse integration control key", err)
	}
	cipher, err := control.NewAES256GCM(key)
	if err != nil {
		fatal("create integration result cipher", err)
	}
	store, err := control.Open(ctx, databasePath, cipher)
	if err != nil {
		fatal("open integration control store", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	principal := control.Principal{ID: "principal_integration_other", Subject: "integration-other", Role: "query", CreatedAt: now}
	if err := store.CreatePrincipal(ctx, principal); err != nil && !errors.Is(err, control.ErrConflict) {
		fatal("create another integration principal", err)
	}
	requestContext, _ := json.Marshal(map[string]any{"integration_seed": true})
	err = store.CreateTask(ctx, control.Task{
		ID: "task-owned-by-another-subject", PrincipalID: principal.ID,
		Objective: "integration ownership isolation probe", State: control.TaskAwaitingSubmission,
		CatalogVersion: catalogVersion, Sensitivity: "low", RequestedBudget: json.RawMessage(`{}`),
		RequestContext: requestContext, ApprovalRef: "integration-seed", CreatedAt: now, UpdatedAt: now,
	})
	if err != nil && !errors.Is(err, control.ErrConflict) {
		fatal("create another principal task", err)
	}
	// Verify the seed exists before the client performs a non-enumerating read.
	seeded, err := store.GetTask(ctx, "task-owned-by-another-subject")
	if err != nil || seeded.PrincipalID != principal.ID || !bytes.Equal(seeded.RequestedBudget, []byte(`{}`)) {
		fatal("verify another principal task", errors.New("ownership seed was not stored"))
	}
}

func requiredEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fatal("read "+name, errors.New("environment variable is required"))
	}
	return value
}

func fatal(operation string, err error) {
	fmt.Fprintf(os.Stderr, "mcp probe failed: %s: %v\n", operation, err)
	os.Exit(1)
}
