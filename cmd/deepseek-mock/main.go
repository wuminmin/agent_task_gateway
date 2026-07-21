// Command deepseek-mock provides a deterministic, network-local OpenAI-style
// chat endpoint used exclusively by the Compose acceptance suite. It lets the
// suite exercise the real DeepSeek adapter without using an external model or
// credential.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

type chatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/chat/completions", complete)
	address := os.Getenv("MOCK_ADDR")
	if address == "" {
		address = ":8081"
	}
	slog.Info("DeepSeek integration mock listening", "address", address)
	if err := http.ListenAndServe(address, mux); err != nil {
		slog.Error("DeepSeek integration mock stopped", "error", err)
		os.Exit(1)
	}
}

func complete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request chatRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&request); err != nil || len(request.Messages) < 2 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	system := request.Messages[0].Content
	question := request.Messages[len(request.Messages)-1].Content
	var content any
	if strings.Contains(system, "data_products") {
		content = taskIntent(question)
	} else {
		content = queryPlan(question)
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(encoded)}}},
	})
}

func taskIntent(question string) map[string]any {
	if strings.Contains(strings.ToLower(question), "detail") || strings.Contains(question, "明细") || strings.Contains(question, "拒绝") {
		return map[string]any{
			"objective":     question,
			"data_products": []string{"expense_detail"},
			"columns": map[string]any{"expense_detail": []string{
				"receipt_no", "employee_no", "employee_name", "department", "expense_date",
				"expense_type", "amount", "city", "purpose", "status",
			}},
			"scopes": map[string]any{"department": []string{"销售部"}},
		}
	}
	return map[string]any{
		"objective":        question,
		"data_products":    []string{"expense_summary"},
		"columns":          map[string]any{"expense_summary": []string{"month", "department", "expense_type", "total_amount", "request_count"}},
		"scopes":           map[string]any{"department": []string{"销售部"}},
		"requested_budget": map[string]any{"max_queries": 2, "max_rows": 50},
	}
}

func queryPlan(question string) map[string]any {
	if strings.Contains(strings.ToLower(question), "detail") || strings.Contains(question, "明细") {
		return map[string]any{
			"product": "expense_detail", "columns": []string{"receipt_no", "employee_name", "amount"},
			"order_by": []any{map[string]any{"column": "receipt_no", "direction": "asc"}}, "limit": 20,
		}
	}
	return map[string]any{
		"product": "expense_summary", "columns": []string{"month", "total_amount"},
		"order_by": []any{map[string]any{"column": "month", "direction": "asc"}}, "limit": 20,
	}
}
