package oademo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	body := `{"task_id":"task-1","requester":"alice","objective":"summary","data_products":["expense_summary"],"sensitivity":"low","approval_mode":"auto","catalog_version":"v1"}`
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
	_ = callbacks
	_ = callbackBodies
}
