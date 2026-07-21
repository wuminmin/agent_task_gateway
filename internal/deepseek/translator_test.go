package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTranslateIntentRepairsOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer test" {
			t.Fatalf("authorization = %q", got)
		}
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		content := `{"objective":1}`
		if calls.Load() == 2 {
			content = `{"objective":"月度费用","data_products":["expense_summary"]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}}})
	}))
	defer server.Close()

	client := New("test", server.URL, "model", server.Client())
	intent, err := client.TranslateIntent(context.Background(), "查询费用", `{"products":["expense_summary"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Objective != "月度费用" || calls.Load() != 2 {
		t.Fatalf("intent=%+v calls=%d", intent, calls.Load())
	}
}

func TestTranslateIntentClosesAfterRepairFailure(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": `{}`}}}})
	}))
	defer server.Close()

	_, err := New("test", server.URL, "model", server.Client()).TranslateIntent(context.Background(), "x", "catalog")
	if err == nil || !strings.Contains(err.Error(), "INVALID_MODEL_OUTPUT") {
		t.Fatalf("expected closed failure, got %v", err)
	}
}

func TestMissingKeyOnlyDisablesModel(t *testing.T) {
	t.Parallel()
	_, err := New("", "", "", nil).TranslateQuery(context.Background(), "x", "catalog")
	if err == nil || !strings.Contains(err.Error(), "MODEL_UNAVAILABLE") {
		t.Fatalf("expected model unavailable, got %v", err)
	}
}
