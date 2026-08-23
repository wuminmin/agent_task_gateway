package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/oademo"
)

type adapterOACallback struct {
	Status          string                      `json:"status"`
	OccurredAt      time.Time                   `json:"occurred_at"`
	ApprovedGrant   *approval.TaskGrantCoreV1   `json:"approved_grant,omitempty"`
	ApprovalReceipt *approval.ApprovalReceiptV1 `json:"approval_receipt,omitempty"`
}

func TestOANarrowActionUsesRealFormAndSurvivesApprovalDelay(t *testing.T) {
	callbackSecret := strings.Repeat("c", 32)
	serviceToken := "real-service-token"
	parentIssuedAt := time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC)
	var clock atomic.Value
	clock.Store(parentIssuedAt)
	callbacks := make(chan adapterOACallback, 4)
	callbackServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var event adapterOACallback
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			http.Error(response, "invalid callback", http.StatusBadRequest)
			return
		}
		callbacks <- event
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer callbackServer.Close()

	signer := approval.DemoReceiptSigner([]byte(callbackSecret))
	oa, err := oademo.New(oademo.Config{
		ServiceToken: serviceToken, CallbackSecret: callbackSecret, SessionSecret: strings.Repeat("s", 32),
		CallbackURL: callbackServer.URL, PublicBaseURL: "http://oa.invalid",
		AlicePassword: strings.Repeat("a", 32), BobPassword: strings.Repeat("b", 32),
		Clock: func() time.Time { return clock.Load().(time.Time) }, ReceiptSigner: signer,
	})
	if err != nil {
		t.Fatal(err)
	}
	oaServer := httptest.NewServer(oa.Handler())
	defer oaServer.Close()

	alice := newOAAccount(oaServer.URL, "alice", strings.Repeat("a", 32), 3*time.Second)
	bob := newOAAccount(oaServer.URL, "bob", strings.Repeat("b", 32), 3*time.Second)

	resourceBudget := provisionedBudget{
		MaxQueries: 5, MaxRows: 100, MaxDBMS: 30_000, QueryTimeoutMS: 5_000,
		TaskTTLSeconds: 90, MaxReleaseFacts: 20, MaxInfluenceFacts: 30, MaxOutcomeFacts: 6,
		ExposureProfileVersion: "taskgate-exposure-v5",
		PredicateFootprint: &domain.PredicateFootprintLimitsV1{
			Version: domain.PredicateFootprintV1, MaxRawLiteralsPerQuery: 64,
			MaxUniqueAtomsPerQuery: 8, MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 32768,
		},
	}
	signedBudget := approval.AuthorizationBudgetV1{
		MaxQueries: resourceBudget.MaxQueries, MaxResultRows: resourceBudget.MaxRows,
		MaxDBMS: resourceBudget.MaxDBMS, PerQueryTimeoutMS: resourceBudget.QueryTimeoutMS,
		TaskTTLMS:       resourceBudget.TaskTTLSeconds * 1000,
		MaxReleaseFacts: resourceBudget.MaxReleaseFacts, MaxInfluenceFacts: resourceBudget.MaxInfluenceFacts,
		MaxOutcomeFacts: resourceBudget.MaxOutcomeFacts, ExposureProfileVersion: resourceBudget.ExposureProfileVersion,
		PredicateFootprint: resourceBudget.PredicateFootprint,
	}
	products := []string{"final_v5_attack_expense_detail"}
	columns := map[string][]string{products[0]: {"receipt_no", "amount"}}
	scope := map[string]any{"department": []string{"销售部"}}
	const parentTaskID = "task-parent-delay-regression"
	const childTaskID = "task-child-delay-regression"
	manifest := approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: childTaskID,
		RootTaskID: parentTaskID, ParentTaskID: parentTaskID,
		HumanSubject: "alice", AgentID: "agent:alice", DeclaredObjective: "E delegated terminal ceiling",
		Products: products, ApprovedColumns: columns, MandatoryScope: scope,
		Sensitivity: domain.SensitivityHigh, Budget: signedBudget,
		CatalogVersion: "final-v5-test", CatalogSHA256: strings.Repeat("1", 64),
		DatasourceID: "final-v5-test-source", SchemaDigest: strings.Repeat("2", 64),
		CallbackContext: "adapter-oa-delay-regression", Nonce: strings.Repeat("0", 32),
	}
	manifestDigest, err := approval.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	client, err := approval.NewClient(oaServer.URL, serviceToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := client.CreateDraft(t.Context(), approval.DraftRequest{
		Manifest: manifest, ManifestDigest: manifestDigest, ApprovalMode: "manual", Approver: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := oaAction(t.Context(), alice, draft.DraftID, "submit", ""); err != nil {
		t.Fatal(err)
	}
	waitAdapterOACallback(t, callbacks, "submitted")

	// Reproduce the production race deterministically: Bob decides two seconds
	// after the parent was issued. Reapproving the full 90-second TTL would
	// extend the child two seconds past its parent; the real narrow form leaves
	// a 30-second headroom instead.
	clock.Store(parentIssuedAt.Add(2 * time.Second))
	narrowedTTLMS, err := delegatedNarrowTTLMS(resourceBudget.TaskTTLSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := oaNarrowAction(t.Context(), bob, draft.DraftID, oaNarrowDecision{
		Products: products, Columns: columns, MandatoryScope: scope,
		MaxQueries: resourceBudget.MaxQueries, MaxResultRows: resourceBudget.MaxRows,
		MaxDBMS: resourceBudget.MaxDBMS, QueryTimeoutMS: resourceBudget.QueryTimeoutMS,
		TaskTTLMS: narrowedTTLMS,
	}); err != nil {
		t.Fatal(err)
	}
	decided := waitAdapterOACallback(t, callbacks, "narrowed")
	if decided.ApprovedGrant == nil || decided.ApprovalReceipt == nil {
		t.Fatal("real OA narrow callback omitted its signed grant")
	}
	childProtocol := approval.TaskGrantV1{
		Version: domain.TaskGrantV1Version, Core: *decided.ApprovedGrant,
		ApprovalReceipt: *decided.ApprovalReceipt,
	}
	if err := approval.VerifyTaskGrantV1(approval.DemoReceiptVerifier([]byte(callbackSecret)), childProtocol); err != nil {
		t.Fatalf("verify real OA signed child grant: %v", err)
	}
	childEncoded, err := approval.EncodeTaskGrantV1(childProtocol)
	if err != nil {
		t.Fatal(err)
	}

	parentCore := domain.TaskGrantCoreV1{
		Version: domain.TaskGrantCoreV1Version, TaskID: parentTaskID,
		HumanSubject: manifest.HumanSubject, AgentID: "agent:parent", DeclaredObjective: "E root sequence",
		ApprovedProducts: products, ApprovedColumns: columns, MandatoryScope: scope,
		SensitivityCeiling: manifest.Sensitivity, Budget: signedBudget,
		ExpiresAt:      parentIssuedAt.Add(time.Duration(signedBudget.TaskTTLMS) * time.Millisecond),
		CatalogVersion: manifest.CatalogVersion, CatalogSHA256: manifest.CatalogSHA256,
		DatasourceID: manifest.DatasourceID, SchemaDigest: manifest.SchemaDigest,
		ManifestDigest: strings.Repeat("3", 64),
	}
	parentDigest, err := approval.GrantCoreDigest(parentCore)
	if err != nil {
		t.Fatal(err)
	}
	parentReceipt, err := signer.SignReceipt(approval.ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "receipt-parent-delay-regression",
		TaskID: parentTaskID, Decision: domain.ApprovalDecisionApprove,
		ManifestDigest: parentCore.ManifestDigest, ApprovedGrantDigest: parentDigest,
		ApproverID: "bob", IssuedAt: parentIssuedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	parentEncoded, err := approval.EncodeTaskGrantV1(approval.TaskGrantV1{
		Version: domain.TaskGrantV1Version, Core: parentCore, ApprovalReceipt: parentReceipt,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !decided.ApprovalReceipt.IssuedAt.Add(time.Duration(signedBudget.TaskTTLMS) * time.Millisecond).After(parentCore.ExpiresAt) {
		t.Fatal("test precondition failed: delayed full-TTL approval would not extend parent expiry")
	}
	if err := validateDelegatedSignedGrant(parentEncoded, childEncoded, parentTaskID, childTaskID,
		resourceBudget, narrowedTTLMS); err != nil {
		t.Fatalf("validate narrowed delegated grant after approval delay: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*provisionedBudget)
	}{
		{name: "release", mutate: func(value *provisionedBudget) { value.MaxReleaseFacts++ }},
		{name: "influence", mutate: func(value *provisionedBudget) { value.MaxInfluenceFacts++ }},
		{name: "outcome", mutate: func(value *provisionedBudget) { value.MaxOutcomeFacts++ }},
		{name: "profile", mutate: func(value *provisionedBudget) { value.ExposureProfileVersion = "taskgate-exposure-v4" }},
		{name: "predicate footprint", mutate: func(value *provisionedBudget) {
			copy := *value.PredicateFootprint
			copy.MaxUniqueAtomsPerQuery++
			value.PredicateFootprint = &copy
		}},
	} {
		t.Run("reject non-TTL drift/"+test.name, func(t *testing.T) {
			expected := resourceBudget
			test.mutate(&expected)
			if err := validateDelegatedSignedGrant(parentEncoded, childEncoded, parentTaskID, childTaskID,
				expected, narrowedTTLMS); err == nil {
				t.Fatal("delegated signed grant accepted non-TTL authorization drift")
			}
		})
	}
}

func TestDelegatedNarrowTTLMSFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		seconds   int64
		want      int64
		wantError bool
	}{
		{name: "zero", seconds: 0, wantError: true},
		{name: "less than headroom", seconds: 29, wantError: true},
		{name: "equal to headroom", seconds: 30, wantError: true},
		{name: "one second remains", seconds: 31, want: 1_000},
		{name: "thirty second headroom", seconds: 90, want: 60_000},
		{name: "overflow", seconds: int64(^uint64(0) >> 1), wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := delegatedNarrowTTLMS(test.seconds)
			if test.wantError {
				if err == nil {
					t.Fatalf("delegatedNarrowTTLMS(%d) = %d, want fail closed", test.seconds, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("delegatedNarrowTTLMS(%d) = (%d, %v), want (%d, nil)", test.seconds, got, err, test.want)
			}
		})
	}
}

// TestOAActionsOpenFreshVerifiedSessionsAcrossExpiry is the P69 regression:
// the OA session token dies 8h after login while a campaign runs ~8.5h in one
// adapter process. With per-action fresh sessions, actions after the 8h mark
// must still submit and decide successfully instead of silently landing on
// /login with a final 200.
func TestOAActionsOpenFreshVerifiedSessionsAcrossExpiry(t *testing.T) {
	callbackSecret := strings.Repeat("c", 32)
	serviceToken := "expiry-service-token"
	start := time.Date(2026, 8, 23, 9, 46, 0, 0, time.UTC)
	var clock atomic.Value
	clock.Store(start)
	callbacks := make(chan adapterOACallback, 8)
	callbackServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var event adapterOACallback
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			http.Error(response, "invalid callback", http.StatusBadRequest)
			return
		}
		callbacks <- event
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer callbackServer.Close()

	oa, err := oademo.New(oademo.Config{
		ServiceToken: serviceToken, CallbackSecret: callbackSecret, SessionSecret: strings.Repeat("s", 32),
		CallbackURL: callbackServer.URL, PublicBaseURL: "http://oa.invalid",
		AlicePassword: strings.Repeat("a", 32), BobPassword: strings.Repeat("b", 32),
		Clock:         func() time.Time { return clock.Load().(time.Time) },
		ReceiptSigner: approval.DemoReceiptSigner([]byte(callbackSecret)),
	})
	if err != nil {
		t.Fatal(err)
	}
	oaServer := httptest.NewServer(oa.Handler())
	defer oaServer.Close()

	alice := newOAAccount(oaServer.URL, "alice", strings.Repeat("a", 32), 3*time.Second)
	bob := newOAAccount(oaServer.URL, "bob", strings.Repeat("b", 32), 3*time.Second)
	client, err := approval.NewClient(oaServer.URL, serviceToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	createDraft := func(taskID string) string {
		manifest := expiryRegressionManifest(taskID)
		digest, digestErr := approval.ManifestDigest(manifest)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		draft, draftErr := client.CreateDraft(t.Context(), approval.DraftRequest{
			Manifest: manifest, ManifestDigest: digest, ApprovalMode: "manual", Approver: "bob",
		})
		if draftErr != nil {
			t.Fatal(draftErr)
		}
		return draft.DraftID
	}

	first := createDraft("task-expiry-before")
	if err := oaAction(t.Context(), alice, first, "submit", ""); err != nil {
		t.Fatal(err)
	}
	waitAdapterOACallback(t, callbacks, "submitted")

	// The wedge window: more than the OA's 8h session lifetime elapses while
	// the same adapter process keeps provisioning tasks.
	clock.Store(start.Add(8*time.Hour + time.Minute))

	second := createDraft("task-expiry-after")
	if err := oaAction(t.Context(), alice, second, "submit", ""); err != nil {
		t.Fatalf("submit after the 8h session lifetime: %v", err)
	}
	waitAdapterOACallback(t, callbacks, "submitted")
	if err := oaAction(t.Context(), bob, second, "decision", "approved"); err != nil {
		t.Fatalf("decision after the 8h session lifetime: %v", err)
	}
	approved := waitAdapterOACallback(t, callbacks, "approved")
	if approved.ApprovedGrant == nil || approved.ApprovalReceipt == nil {
		t.Fatal("post-expiry approval callback omitted its signed grant")
	}
}

// TestOAAccountFailsLoudOnBadPassword covers the other silent path: the OA
// answers a failed login with a redirect to /login?error=invalid and a final
// 200, which the pre-P69 client accepted as success.
func TestOAAccountFailsLoudOnBadPassword(t *testing.T) {
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer callbackServer.Close()
	oa, err := oademo.New(oademo.Config{
		ServiceToken: "service", CallbackSecret: strings.Repeat("c", 32), SessionSecret: strings.Repeat("s", 32),
		CallbackURL: callbackServer.URL, PublicBaseURL: "http://oa.invalid",
		AlicePassword: strings.Repeat("a", 32), BobPassword: strings.Repeat("b", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	oaServer := httptest.NewServer(oa.Handler())
	defer oaServer.Close()

	wrong := newOAAccount(oaServer.URL, "alice", "not-the-password", 3*time.Second)
	if _, err := wrong.newSession(t.Context()); err == nil {
		t.Fatal("bad password produced a usable OA session")
	}
	if err := oaAction(t.Context(), wrong, "oa_no_such_draft", "submit", ""); err == nil {
		t.Fatal("bad-password OA action reported success")
	}
}

func expiryRegressionManifest(taskID string) approval.AuthorizationManifestV1 {
	return approval.AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version, TaskID: taskID,
		HumanSubject: "alice", AgentID: "agent:alice", DeclaredObjective: "P69 session expiry regression",
		Products:        []string{"final_v5_attack_expense_detail"},
		ApprovedColumns: map[string][]string{"final_v5_attack_expense_detail": {"receipt_no", "amount"}},
		MandatoryScope:  map[string]any{"department": []string{"销售部"}},
		Sensitivity:     domain.SensitivityHigh,
		Budget: approval.AuthorizationBudgetV1{
			MaxQueries: 5, MaxResultRows: 100, MaxDBMS: 30_000, PerQueryTimeoutMS: 5_000,
			TaskTTLMS: 90_000, MaxReleaseFacts: 20, MaxInfluenceFacts: 30, MaxOutcomeFacts: 6,
			ExposureProfileVersion: "taskgate-exposure-v5",
			PredicateFootprint: &domain.PredicateFootprintLimitsV1{
				Version: domain.PredicateFootprintV1, MaxRawLiteralsPerQuery: 64,
				MaxUniqueAtomsPerQuery: 8, MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 32768,
			},
		},
		CatalogVersion: "final-v5-test", CatalogSHA256: strings.Repeat("1", 64),
		DatasourceID: "final-v5-test-source", SchemaDigest: strings.Repeat("2", 64),
		CallbackContext: "adapter-oa-expiry-regression", Nonce: strings.Repeat("0", 32),
	}
}

func waitAdapterOACallback(t *testing.T, callbacks <-chan adapterOACallback, status string) adapterOACallback {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-callbacks:
			if event.Status == status {
				return event
			}
		case <-deadline.C:
			t.Fatalf("real OA callback %q was not delivered", status)
		}
	}
}
