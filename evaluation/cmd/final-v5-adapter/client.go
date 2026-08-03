package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
)

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// mcpCallError preserves the stable, client-safe structured error contract.
// Adapters use Code/Reason for preregistered fail-closed controls; raw text is
// never copied into publication evidence.
type mcpCallError struct {
	Code                  string
	Message               string
	Reason                string
	TraceID               string
	RetryableAfterRewrite bool
}

func (err *mcpCallError) Error() string {
	if err.Code == "" {
		return "MCP tool returned an unstructured error"
	}
	return err.Code + ": " + err.Message
}

type mcpClient struct {
	url   string
	token string
	http  *http.Client
	next  atomic.Int64
}

func (client *mcpClient) call(ctx context.Context, tool string, arguments, output any) error {
	return client.callWithHeaders(ctx, tool, arguments, output, nil)
}

// callWithHeaders is used only by the authenticated concurrency evaluation
// path. The service consumes and strips those private headers before MCP
// dispatch; ordinary adapter calls continue to use call with no additions.
func (client *mcpClient) callWithHeaders(ctx context.Context, tool string, arguments, output any, headers map[string]string) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": client.next.Add(1), "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	for name, value := range headers {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return errors.New("MCP request contains an invalid private header")
		}
		request.Header.Set(name, value)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP HTTP status %d", response.StatusCode)
	}
	var rpc rpcResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return err
	}
	if rpc.Error != nil {
		return fmt.Errorf("MCP RPC error %d", rpc.Error.Code)
	}
	var result toolResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return err
	}
	if result.IsError {
		var structured struct {
			TraceID string `json:"trace_id"`
			Error   struct {
				Code                  string `json:"code"`
				Message               string `json:"message"`
				Reason                string `json:"reason"`
				RetryableAfterRewrite bool   `json:"retryable_after_rewrite"`
			} `json:"error"`
		}
		if len(result.StructuredContent) != 0 && json.Unmarshal(result.StructuredContent, &structured) == nil && structured.Error.Code != "" {
			return &mcpCallError{Code: structured.Error.Code, Message: structured.Error.Message,
				Reason: structured.Error.Reason, TraceID: structured.TraceID,
				RetryableAfterRewrite: structured.Error.RetryableAfterRewrite}
		}
		return errors.New("tool returned an error without structured content")
	}
	if len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null" {
		return errors.New("tool omitted structured content")
	}
	decoder := json.NewDecoder(strings.NewReader(string(result.StructuredContent)))
	decoder.UseNumber()
	return decoder.Decode(output)
}
