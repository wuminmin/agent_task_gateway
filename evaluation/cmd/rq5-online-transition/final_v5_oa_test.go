package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/oademo"
)

func TestFinalV5OAWorkflowUsesRealLoginSubmitDecisionAndSignedCallbacks(t *testing.T) {
	t.Parallel()
	type observedCallback struct {
		Status          string                      `json:"status"`
		ApprovalReceipt *approval.ApprovalReceiptV1 `json:"approval_receipt"`
	}
	var mu sync.Mutex
	var callbacks []observedCallback
	callbackServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var event observedCallback
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			http.Error(response, "invalid", http.StatusBadRequest)
			return
		}
		mu.Lock()
		callbacks = append(callbacks, event)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer callbackServer.Close()

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	receiptSigner, err := approval.NewReceiptSignerFromBase64("rq5-oa-test-v1",
		base64.StdEncoding.EncodeToString(privateKey))
	if err != nil {
		t.Fatal(err)
	}
	oa, err := oademo.New(oademo.Config{
		ServiceToken: "real-service-token", CallbackSecret: strings.Repeat("c", 32),
		SessionSecret: strings.Repeat("s", 32), CallbackURL: callbackServer.URL,
		PublicBaseURL: "http://oa.invalid", AlicePassword: strings.Repeat("a", 32),
		BobPassword: strings.Repeat("b", 32), ReceiptSigner: receiptSigner,
	})
	if err != nil {
		t.Fatal(err)
	}
	oaServer := httptest.NewServer(oa.Handler())
	defer oaServer.Close()

	workflow, err := newFinalV5OAWorkflow(t.Context(), oaServer.URL, strings.Repeat("a", 32), strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	manifest := approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: "rq5-real-oa-task",
		HumanSubject: "alice", AgentID: "agent:rq5", DeclaredObjective: "verify publication",
		Products:        []string{"daily_lineitem"},
		ApprovedColumns: map[string][]string{"daily_lineitem": {"dataset_partition", "l_orderkey"}},
		MandatoryScope:  map[string]any{"dataset_partition": "1"}, Sensitivity: domain.SensitivityHigh,
		Budget: approval.AuthorizationBudgetV1{MaxQueries: 10, MaxResultRows: 100, MaxDBMS: 30_000,
			PerQueryTimeoutMS: 5_000, TaskTTLMS: 1_800_000},
		CatalogVersion: "rq5-v1", CatalogSHA256: strings.Repeat("1", 64),
		DatasourceID: "rq5-source", SchemaDigest: strings.Repeat("2", 64),
		CallbackContext: "rq5-callback", Nonce: "000102030405060708090a0b0c0d0e0f",
	}
	manifestDigest, err := approval.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	client, err := approval.NewClient(oaServer.URL, "real-service-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := client.CreateDraft(t.Context(), approval.DraftRequest{Manifest: manifest,
		ManifestDigest: manifestDigest, ApprovalMode: "manual", Approver: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if err := workflow.submit(t.Context(), draft.DraftID); err != nil {
		t.Fatal(err)
	}
	if err := workflow.action(t.Context(), workflow.bob, draft.DraftID, "decision", "approved", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		complete := len(callbacks) >= 2
		var first, second observedCallback
		if complete {
			first, second = callbacks[0], callbacks[1]
		}
		mu.Unlock()
		if complete {
			// The OA demo dispatches each callback in its own goroutine with
			// independent retries (internal/oademo/server.go dispatch), so
			// delivery order is deliberately unguaranteed; classify by status.
			var submitted, approved *observedCallback
			for _, callback := range []*observedCallback{&first, &second} {
				switch callback.Status {
				case "submitted":
					submitted = callback
				case "approved":
					approved = callback
				}
			}
			if submitted == nil || approved == nil || approved.ApprovalReceipt == nil ||
				approved.ApprovalReceipt.Signature == "" {
				t.Fatalf("unexpected real OA callbacks: %#v %#v", first, second)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real OA callbacks were not delivered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
