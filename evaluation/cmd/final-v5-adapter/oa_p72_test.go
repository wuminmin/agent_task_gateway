package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/oademo"
)

// p72Counts records what the adapter actually asks the OA for. The two numbers
// that matter are logins (each one costs a cost-10 bcrypt server side) and
// draft-list renders (GET /tasks walks every draft the OA holds, so one per
// action made a campaign quadratic).
type p72Counts struct {
	logins    atomic.Int64
	draftList atomic.Int64
}

func (counts *p72Counts) wrap(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/login" {
			counts.logins.Add(1)
		}
		if r.Method == http.MethodGet && r.URL.Path == "/tasks" {
			counts.draftList.Add(1)
		}
		inner.ServeHTTP(w, r)
	})
}

type p72Fixture struct {
	counts    *p72Counts
	alice     oaAccount
	bob       oaAccount
	client    *approval.Client
	callbacks chan adapterOACallback
	clock     *atomic.Value
	start     time.Time
}

func newP72Fixture(t *testing.T) *p72Fixture {
	t.Helper()
	callbackSecret := strings.Repeat("c", 32)
	serviceToken := "p72-service-token"
	start := time.Date(2026, 8, 24, 0, 4, 0, 0, time.UTC)
	clock := &atomic.Value{}
	clock.Store(start)
	callbacks := make(chan adapterOACallback, 64)
	callbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var event adapterOACallback
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			http.Error(w, "invalid callback", http.StatusBadRequest)
			return
		}
		callbacks <- event
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(callbackServer.Close)

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
	counts := &p72Counts{}
	oaServer := httptest.NewServer(counts.wrap(oa.Handler()))
	t.Cleanup(oaServer.Close)

	client, err := approval.NewClient(oaServer.URL, serviceToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &p72Fixture{
		counts:    counts,
		alice:     newOAAccount(oaServer.URL, "alice", strings.Repeat("a", 32), 5*time.Second),
		bob:       newOAAccount(oaServer.URL, "bob", strings.Repeat("b", 32), 5*time.Second),
		client:    client,
		callbacks: callbacks,
		clock:     clock,
		start:     start,
	}
}

func (fixture *p72Fixture) draft(t *testing.T, taskID string) string {
	t.Helper()
	manifest := expiryRegressionManifest(taskID)
	digest, err := approval.ManifestDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.client.CreateDraft(t.Context(), approval.DraftRequest{
		Manifest: manifest, ManifestDigest: digest, ApprovalMode: "manual", Approver: "bob",
	})
	if err != nil {
		t.Fatal(err)
	}
	return created.DraftID
}

// TestP72OneLoginServesManyActionsAndNeverListsDrafts pins the two costs P72
// removes. Re-logging in per action cost a bcrypt every time, and proving the
// session live with GET /tasks cost a full draft scan every time; neither is
// needed, because the action's own GET already reveals a dead session.
func TestP72OneLoginServesManyActionsAndNeverListsDrafts(t *testing.T) {
	fixture := newP72Fixture(t)
	const actions = 6
	for i := 0; i < actions; i++ {
		draftID := fixture.draft(t, "task-p72-reuse-"+strings.Repeat("x", i+1))
		if err := oaAction(t.Context(), fixture.alice, draftID, "submit", ""); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		waitAdapterOACallback(t, fixture.callbacks, "submitted")
	}
	if got := fixture.counts.logins.Load(); got != 1 {
		t.Fatalf("%d actions should share one login, got %d logins", actions, got)
	}
	// The OA answers a successful POST /login with 303 to /tasks and the client
	// follows it, so each login still costs one draft-list render. What matters
	// is that the count tracks logins rather than actions: before P72 every
	// action paid two of these (the post-login redirect plus the explicit
	// probe), which is what made a campaign quadratic.
	if got := fixture.counts.draftList.Load(); got != fixture.counts.logins.Load() {
		t.Fatalf("draft-list renders must track logins, got %d renders for %d logins",
			got, fixture.counts.logins.Load())
	}
	if got := fixture.counts.draftList.Load(); got >= actions {
		t.Fatalf("draft-list renders must not scale with actions, got %d for %d actions", got, actions)
	}
}

// TestP72ExpiryCostsExactlyOneExtraLogin proves the session is replaced when
// the OA drops it, and only then: the whole point is that recovery is driven
// by the observed redirect, not by re-authenticating on every action.
func TestP72ExpiryCostsExactlyOneExtraLogin(t *testing.T) {
	fixture := newP72Fixture(t)
	before := fixture.draft(t, "task-p72-before")
	if err := oaAction(t.Context(), fixture.alice, before, "submit", ""); err != nil {
		t.Fatal(err)
	}
	waitAdapterOACallback(t, fixture.callbacks, "submitted")
	if got := fixture.counts.logins.Load(); got != 1 {
		t.Fatalf("expected one login before expiry, got %d", got)
	}

	// Cross the OA's 8h session lifetime, the P69 wedge window.
	fixture.clock.Store(fixture.start.Add(8*time.Hour + time.Minute))

	for i := 0; i < 3; i++ {
		draftID := fixture.draft(t, "task-p72-after-"+strings.Repeat("y", i+1))
		if err := oaAction(t.Context(), fixture.alice, draftID, "submit", ""); err != nil {
			t.Fatalf("submit %d after expiry: %v", i, err)
		}
		waitAdapterOACallback(t, fixture.callbacks, "submitted")
	}
	if got := fixture.counts.logins.Load(); got != 2 {
		t.Fatalf("expiry should cost exactly one extra login, got %d total", got)
	}
	if got := fixture.counts.draftList.Load(); got != 2 {
		t.Fatalf("only the two logins may render the draft list, got %d", got)
	}
}

// TestP72ConcurrentExpiryDoesNotStampede covers the failure mode a naive
// re-login introduces: at 500-way concurrency every in-flight action notices
// the same dead session at once, and without the generation check each would
// start its own bcrypt login.
func TestP72ConcurrentExpiryDoesNotStampede(t *testing.T) {
	fixture := newP72Fixture(t)
	const workers = 24
	drafts := make([]string, workers)
	for i := range drafts {
		drafts[i] = fixture.draft(t, "task-p72-race-"+strings.Repeat("z", i+1))
	}
	warm := fixture.draft(t, "task-p72-race-warm")
	if err := oaAction(t.Context(), fixture.alice, warm, "submit", ""); err != nil {
		t.Fatal(err)
	}
	waitAdapterOACallback(t, fixture.callbacks, "submitted")

	fixture.clock.Store(fixture.start.Add(8*time.Hour + time.Minute))

	var group sync.WaitGroup
	errs := make(chan error, workers)
	for _, draftID := range drafts {
		group.Add(1)
		go func(id string) {
			defer group.Done()
			if err := oaAction(t.Context(), fixture.alice, id, "submit", ""); err != nil {
				errs <- err
			}
		}(draftID)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent submit across expiry: %v", err)
	}
	for range drafts {
		waitAdapterOACallback(t, fixture.callbacks, "submitted")
	}
	if got := fixture.counts.logins.Load(); got != 2 {
		t.Fatalf("%d concurrent actions across one expiry should cost one re-login (2 total), got %d",
			workers, got)
	}
}

// TestP72FailsLoudWhenSessionCannotBeRestored keeps the P69 invariant across
// the new retry: a session the OA will not honour must surface as an error,
// never as a silent success, even after the one permitted re-login. The OA
// demo cannot express "login works but the draft routes still refuse", so this
// uses a minimal stand-in that answers every draft route with the login
// redirect while /login itself keeps serving a usable form.
func TestP72FailsLoudWhenSessionCannotBeRestored(t *testing.T) {
	var logins atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login" && r.Method == http.MethodPost:
			logins.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/login":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<form><input name="csrf" value="tok-p72" /></form>`))
		default:
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
	}))
	defer server.Close()

	account := newOAAccount(server.URL, "alice", strings.Repeat("a", 32), 5*time.Second)
	err := oaAction(t.Context(), account, "draft-p72-unrestorable", "submit", "")
	if err == nil {
		t.Fatal("an unrestorable session must fail loudly, not report success")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Fatalf("error should name the exhausted retry, got %v", err)
	}
	if got := logins.Load(); got != 2 {
		t.Fatalf("exactly one re-login may be attempted, got %d logins", got)
	}
}
