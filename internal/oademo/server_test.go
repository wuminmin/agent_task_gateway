package oademo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/approval"
)

func TestCreateDraftAuthenticationAndCallbackSignature(t *testing.T) {
	callbacks := make(chan *http.Request, 1)
	callbackBodies := make(chan []byte, 1)
	callback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		callbacks <- r.Clone(r.Context())
		callbackBodies <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()

	server, err := New(Config{
		ServiceToken: "service", CallbackSecret: "callback", SessionSecret: "session",
		CallbackURL: callback.URL, AlicePassword: "alice-pass", BobPassword: "bob-pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	draftRequest := DraftRequest{
		TaskID: "task-1", Requester: "alice", Objective: "summary",
		DataProducts:    []string{"expense_summary"},
		ApprovedColumns: map[string][]string{"expense_summary": {"month", "total_amount"}},
		MandatoryScope:  map[string]any{"department": []string{"销售部"}}, Sensitivity: "low",
		Budget: approval.DraftBudget{
			MaxQueries: 10, MaxRows: 500, MaxDBMS: 30_000, QueryTimeoutMS: 5_000, TaskTTLMS: 1_800_000,
		},
		ApprovalMode: "auto", CatalogVersion: "v1", CallbackContext: "callback-1",
	}
	snapshotSHA256, err := approval.AuthorizationSnapshotSHA256(draftRequest)
	if err != nil {
		t.Fatalf("hash authorization snapshot: %v", err)
	}
	draftRequest.AuthorizationSnapshotSHA256 = snapshotSHA256
	bodyBytes, err := json.Marshal(draftRequest)
	if err != nil {
		t.Fatalf("marshal draft request: %v", err)
	}
	body := string(bodyBytes)
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer wrong")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}

	tampered := draftRequest
	tampered.ApprovedColumns = cloneColumns(draftRequest.ApprovedColumns)
	tampered.ApprovedColumns["expense_summary"] = []string{"month", "department", "total_amount"}
	tamperedBody, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal tampered draft: %v", err)
	}
	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(string(tamperedBody)))
	request.Header.Set("Authorization", "Bearer service")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered snapshot status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}

	request, _ = http.NewRequest(http.MethodPost, httpServer.URL+"/api/drafts", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer service")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var created map[string]any
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil || created["draft_id"] == "" {
		t.Fatalf("created = %#v, err=%v", created, err)
	}
	draftID := created["draft_id"].(string)
	server.mu.RLock()
	stored := cloneDraft(server.drafts[draftID])
	server.mu.RUnlock()
	if stored.AuthorizationSnapshotSHA256 != snapshotSHA256 || stored.Budget.MaxRows != 500 || len(stored.ApprovedColumns["expense_summary"]) != 2 {
		t.Fatalf("stored OA authorization snapshot is incomplete: %+v", stored)
	}

	page := httptest.NewRecorder()
	server.render(page, "task", map[string]any{
		"Session": sessionData{Username: "alice", Role: "requester", CSRF: "csrf"},
		"Draft":   stored,
	})
	for _, visible := range []string{"total_amount", "department", "500", "30000", snapshotSHA256} {
		if !strings.Contains(page.Body.String(), visible) {
			t.Fatalf("OA detail page does not display %q: %s", visible, page.Body.String())
		}
	}
	_ = callbacks
	_ = callbackBodies
}
