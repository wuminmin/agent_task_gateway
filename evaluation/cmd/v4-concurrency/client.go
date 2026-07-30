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
}

type toolCallError struct {
	Code    string
	Message string
}

func (err *toolCallError) Error() string {
	return err.Code + ": " + err.Message
}

type mcpClient struct {
	url   string
	token string
	http  *http.Client
	next  atomic.Int64
}

func (client *mcpClient) call(ctx context.Context, tool string, arguments any, output any) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": client.next.Add(1), "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": arguments},
	})
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
		return fmt.Errorf("MCP HTTP %d", response.StatusCode)
	}
	var rpc rpcResponse
	if err := json.Unmarshal(body, &rpc); err != nil {
		return err
	}
	if rpc.Error != nil {
		return fmt.Errorf("MCP RPC %d: %s", rpc.Error.Code, rpc.Error.Message)
	}
	var result toolResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return err
	}
	if result.IsError {
		var structured struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(result.StructuredContent, &structured); err != nil || structured.Error.Code == "" {
			return errors.New("tool returned an unstructured error")
		}
		return &toolCallError{Code: structured.Error.Code, Message: structured.Error.Message}
	}
	if len(result.StructuredContent) == 0 || string(result.StructuredContent) == "null" {
		return errors.New("tool omitted structuredContent")
	}
	return decodeStructuredContent(result.StructuredContent, output)
}

func decodeStructuredContent(raw json.RawMessage, output any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("tool structuredContent contains trailing JSON")
	}
	return nil
}
