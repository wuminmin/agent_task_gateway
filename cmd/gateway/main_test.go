package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestApprovalReceiptVerifierFromEnvLoadsKeyring(t *testing.T) {
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x6a}, ed25519.SeedSize))
	publicKey := privateKey.Public().(ed25519.PublicKey)
	t.Setenv("OA_RECEIPT_KEYRING_JSON", fmt.Sprintf(
		`[{"key_id":"oa-keyring-test","public_key":%q,"valid_from":"2026-07-01T00:00:00Z","retired_at":"2026-08-01T00:00:00Z"}]`,
		base64.StdEncoding.EncodeToString(publicKey),
	))
	verifier, err := approvalReceiptVerifierFromEnv()
	if err != nil {
		t.Fatalf("approvalReceiptVerifierFromEnv: %v", err)
	}
	signer, err := approval.NewEd25519ReceiptSigner("oa-keyring-test", privateKey)
	if err != nil {
		t.Fatalf("NewEd25519ReceiptSigner: %v", err)
	}
	issuedAt := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	receipt, err := signer.SignReceipt(testApprovalReceipt(issuedAt))
	if err != nil {
		t.Fatalf("SignReceipt: %v", err)
	}
	if err := verifier.VerifyReceipt(receipt); err != nil {
		t.Fatalf("VerifyReceipt: %v", err)
	}

	late, err := signer.SignReceipt(testApprovalReceipt(time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("SignReceipt late: %v", err)
	}
	if err := verifier.VerifyReceipt(late); !errors.Is(err, approval.ErrReceiptKeyNotValid) {
		t.Fatalf("late receipt error = %v, want %v", err, approval.ErrReceiptKeyNotValid)
	}
}

func TestQueryReceiptPublicKeyBundleFromEnvPublishesActiveAndHistoricalKeys(t *testing.T) {
	oldSigner := testQueryReceiptSigner(t, "gateway-2026-q2", 0x21)
	activeSigner := testQueryReceiptSigner(t, "gateway-2026-q3", 0x22)
	t.Setenv("GATEWAY_RECEIPT_KEYRING_JSON", fmt.Sprintf(
		`[`+
			`{"key_id":%q,"public_key":%q,"valid_from":"2026-06-01T00:00:00Z","retired_at":"2026-07-15T00:00:00Z"},`+
			`{"key_id":%q,"public_key":%q,"valid_from":"2026-07-01T00:00:00Z"}`+
			`]`,
		oldSigner.KeyID(), base64.StdEncoding.EncodeToString(oldSigner.PublicKey()),
		activeSigner.KeyID(), base64.StdEncoding.EncodeToString(activeSigner.PublicKey()),
	))
	bundle, err := queryReceiptPublicKeyBundleFromEnv(activeSigner, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("queryReceiptPublicKeyBundleFromEnv: %v", err)
	}
	if bundle.ActiveKeyID != activeSigner.KeyID() || len(bundle.Keys) != 2 {
		t.Fatalf("unexpected query receipt bundle: %+v", bundle)
	}
	verifier, err := bundle.Verifier()
	if err != nil {
		t.Fatalf("bundle Verifier: %v", err)
	}
	oldReceipt, err := oldSigner.Sign(testQueryReceipt(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign old receipt: %v", err)
	}
	if err := verifier.Verify(oldReceipt); err != nil {
		t.Fatalf("old receipt did not verify from published bundle: %v", err)
	}
	activeReceipt, err := activeSigner.Sign(testQueryReceipt(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign active receipt: %v", err)
	}
	if err := verifier.Verify(activeReceipt); err != nil {
		t.Fatalf("active receipt did not verify from published bundle: %v", err)
	}

	mismatchedSigner := testQueryReceiptSigner(t, activeSigner.KeyID(), 0x23)
	if _, err := queryReceiptPublicKeyBundleFromEnv(mismatchedSigner, time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)); !errors.Is(err, queryreceipt.ErrInvalidKey) {
		t.Fatalf("mismatched active public key error = %v, want %v", err, queryreceipt.ErrInvalidKey)
	}
}

func TestQueryReceiptKeyringHandlerPublishesVerifierBundle(t *testing.T) {
	signer := testQueryReceiptSigner(t, "gateway-publish-test", 0x24)
	bundle, err := queryreceipt.NewPublicKeyBundle(signer.KeyID(), []queryreceipt.VerifyingKey{
		{KeyID: signer.KeyID(), PublicKey: signer.PublicKey()},
	}, time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewPublicKeyBundle: %v", err)
	}
	router := chi.NewRouter()
	router.Get("/.well-known/taskgate/query-receipt-keyring.json", queryReceiptKeyringHandler(bundle))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/taskgate/query-receipt-keyring.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content type = %q", contentType)
	}
	privateSeed := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, ed25519.SeedSize))
	if strings.Contains(recorder.Body.String(), privateSeed) {
		t.Fatal("published query receipt key bundle leaked private seed material")
	}
	var decoded queryreceipt.PublicKeyBundleV1
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode published bundle: %v", err)
	}
	verifier, err := decoded.Verifier()
	if err != nil {
		t.Fatalf("published bundle Verifier: %v", err)
	}
	receipt, err := signer.Sign(testQueryReceipt(time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign receipt: %v", err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatalf("Verify from published bundle: %v", err)
	}
}

func TestRetentionConfigFromEnvParsesDurations(t *testing.T) {
	t.Setenv("GATEWAY_RESULT_RETENTION_TTL", "48h")
	t.Setenv("GATEWAY_RESULT_RETENTION_SWEEP_INTERVAL", "900")
	t.Setenv("GATEWAY_ADMIN_TOKEN", "admin-secret")
	config, err := retentionConfigFromEnv()
	if err != nil {
		t.Fatalf("retentionConfigFromEnv: %v", err)
	}
	if config.ResultTTL != 48*time.Hour || config.SweepInterval != 15*time.Minute || config.AdminToken != "admin-secret" {
		t.Fatalf("unexpected retention config: %+v", config)
	}

	t.Setenv("GATEWAY_RESULT_RETENTION_TTL", "-1s")
	if _, err := retentionConfigFromEnv(); err == nil {
		t.Fatal("negative retention TTL unexpectedly accepted")
	}
}

func TestGatewayConnectorMaxRowsV4DefaultAndOverride(t *testing.T) {
	t.Setenv("GATEWAY_CONNECTOR_MAX_ROWS", "")
	value, err := positiveInt64Env("GATEWAY_CONNECTOR_MAX_ROWS", defaultGatewayConnectorMaxRows)
	if err != nil || value != 1_200_000 {
		t.Fatalf("V4 connector row default = %d, %v; want 1200000", value, err)
	}

	t.Setenv("GATEWAY_CONNECTOR_MAX_ROWS", "1500000")
	value, err = positiveInt64Env("GATEWAY_CONNECTOR_MAX_ROWS", defaultGatewayConnectorMaxRows)
	if err != nil || value != 1_500_000 {
		t.Fatalf("connector row override = %d, %v; want 1500000", value, err)
	}

	t.Setenv("GATEWAY_CONNECTOR_MAX_ROWS", "0")
	if _, err := positiveInt64Env("GATEWAY_CONNECTOR_MAX_ROWS", defaultGatewayConnectorMaxRows); err == nil {
		t.Fatal("zero connector row ceiling unexpectedly accepted")
	}
}

func TestRetentionAdminEndpointsRequireAuthAndManageHold(t *testing.T) {
	store := openGatewayTestStore(t)
	ctx := context.Background()
	expires := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	if err := store.CreatePrincipal(ctx, control.Principal{
		ID: "principal_admin_retention", Subject: "retention-subject", Role: "query", CreatedAt: expires.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if err := store.CreateTask(ctx, control.Task{
		ID: "task-admin-retention", PrincipalID: "principal_admin_retention", Objective: "retention admin test",
		State: control.TaskAwaitingApproval, CatalogVersion: "catalog-v1",
		RequestContext: []byte(`{}`), ExpiresAt: &expires, CreatedAt: expires.Add(-time.Hour), UpdatedAt: expires.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	router := chi.NewRouter()
	mountRetentionAdmin(router, store, retentionConfig{AdminToken: "admin-secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPut, "/admin/v1/retention/legal-holds/task-admin-retention", strings.NewReader(`{"reason":"case"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	set := httptest.NewRecorder()
	setRequest := httptest.NewRequest(http.MethodPut, "/admin/v1/retention/legal-holds/task-admin-retention", strings.NewReader(`{"reason":"case"}`))
	setRequest.Header.Set("Authorization", "Bearer admin-secret")
	router.ServeHTTP(set, setRequest)
	if set.Code != http.StatusOK {
		t.Fatalf("set hold status = %d, body=%s", set.Code, set.Body.String())
	}
	hold, err := store.GetResultRetentionHold(ctx, "task-admin-retention")
	if err != nil {
		t.Fatalf("GetResultRetentionHold: %v", err)
	}
	if hold.Reason != "case" || hold.CreatedBy != "admin" {
		t.Fatalf("unexpected hold: %+v", hold)
	}

	purge := httptest.NewRecorder()
	purgeRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/retention/purge", strings.NewReader(`{"cutoff":"2026-07-22T12:00:00Z"}`))
	purgeRequest.Header.Set("Authorization", "Bearer admin-secret")
	router.ServeHTTP(purge, purgeRequest)
	if purge.Code != http.StatusOK {
		t.Fatalf("purge status = %d, body=%s", purge.Code, purge.Body.String())
	}
	var purgeBody map[string]any
	if err := json.Unmarshal(purge.Body.Bytes(), &purgeBody); err != nil {
		t.Fatalf("decode purge body: %v", err)
	}
	if purgeBody["purged_results"] != float64(0) {
		t.Fatalf("purged_results = %#v, want 0", purgeBody["purged_results"])
	}

	clear := httptest.NewRecorder()
	clearRequest := httptest.NewRequest(http.MethodDelete, "/admin/v1/retention/legal-holds/task-admin-retention", nil)
	clearRequest.Header.Set("Authorization", "Bearer admin-secret")
	router.ServeHTTP(clear, clearRequest)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear hold status = %d, body=%s", clear.Code, clear.Body.String())
	}
	if _, err := store.GetResultRetentionHold(ctx, "task-admin-retention"); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("hold after clear = %v, want not found", err)
	}

	erase := httptest.NewRecorder()
	eraseRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/result-encryption-keys/"+control.DefaultResultEncryptionKeyID+"/erase", nil)
	eraseRequest.Header.Set("Authorization", "Bearer admin-secret")
	router.ServeHTTP(erase, eraseRequest)
	if erase.Code != http.StatusOK {
		t.Fatalf("erase key status = %d, body=%s", erase.Code, erase.Body.String())
	}
	var eraseBody map[string]any
	if err := json.Unmarshal(erase.Body.Bytes(), &eraseBody); err != nil {
		t.Fatalf("decode erase body: %v", err)
	}
	if eraseBody["key_id"] != control.DefaultResultEncryptionKeyID || eraseBody["status"] != string(control.ResultEncryptionKeyErased) {
		t.Fatalf("unexpected erase response: %#v", eraseBody)
	}
	key, err := store.GetResultEncryptionKey(ctx, control.DefaultResultEncryptionKeyID)
	if err != nil {
		t.Fatalf("GetResultEncryptionKey: %v", err)
	}
	if key.Status != control.ResultEncryptionKeyErased || key.ErasedBy != "admin" {
		t.Fatalf("unexpected erased key: %+v", key)
	}
	events, err := store.ListAuditEvents(ctx, control.AuditFilter{EventType: "RESULT_ENCRYPTION_KEY_ERASED", Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("RESULT_ENCRYPTION_KEY_ERASED events = %d, err=%v", len(events), err)
	}
}

func TestAuditAnchorConfigFromEnvParsesSignerAndInterval(t *testing.T) {
	t.Setenv("GATEWAY_AUDIT_ANCHOR_URL", "https://anchor.example.test/taskgate")
	t.Setenv("GATEWAY_AUDIT_ANCHOR_INTERVAL", "600")
	t.Setenv("GATEWAY_AUDIT_ANCHOR_KEY_ID", "audit-anchor-test")
	t.Setenv("GATEWAY_AUDIT_ANCHOR_PRIVATE_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, ed25519.SeedSize)))
	config, err := auditAnchorConfigFromEnv()
	if err != nil {
		t.Fatalf("auditAnchorConfigFromEnv: %v", err)
	}
	if !config.enabled() || config.URL != "https://anchor.example.test/taskgate" ||
		config.Interval != 10*time.Minute || config.Signer.KeyID() != "audit-anchor-test" {
		t.Fatalf("unexpected audit anchor config: %+v", config)
	}

	t.Setenv("GATEWAY_AUDIT_ANCHOR_INTERVAL", "-1s")
	if _, err := auditAnchorConfigFromEnv(); err == nil {
		t.Fatal("negative audit anchor interval unexpectedly accepted")
	}
	t.Setenv("GATEWAY_AUDIT_ANCHOR_INTERVAL", "5m")
	t.Setenv("GATEWAY_AUDIT_ANCHOR_KEY_ID", "")
	if _, err := auditAnchorConfigFromEnv(); err == nil {
		t.Fatal("missing audit anchor key unexpectedly accepted")
	}
}

func TestPostAuditCheckpointAnchorPostsSignedPayload(t *testing.T) {
	signer := testAuditAnchorSigner(t, "audit-anchor-post-test", 0x32)
	checkpoint := auditchain.Checkpoint{Sequence: 7, Hash: strings.Repeat("a", 64)}
	signedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	verifier, err := auditchain.NewAnchorVerifier([]auditchain.AnchorVerifyingKey{{
		KeyID: signer.KeyID(), PublicKey: signer.PublicKey(), ValidFrom: signedAt.Add(-time.Hour),
	}})
	if err != nil {
		t.Fatalf("NewAnchorVerifier: %v", err)
	}
	var received auditchain.SignedCheckpointAnchorV1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("content type = %q", contentType)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode anchor body: %v", err)
		}
		if r.Header.Get("Idempotency-Key") != received.AnchorID {
			t.Fatalf("idempotency key = %q, want %q", r.Header.Get("Idempotency-Key"), received.AnchorID)
		}
		if err := verifier.Verify(received); err != nil {
			t.Fatalf("Verify posted anchor: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	anchor, err := postAuditCheckpointAnchor(context.Background(), auditAnchorConfig{
		URL: server.URL, Signer: signer, Client: server.Client(),
	}, checkpoint, signedAt)
	if err != nil {
		t.Fatalf("postAuditCheckpointAnchor: %v", err)
	}
	if anchor.Sequence != checkpoint.Sequence || anchor.Hash != checkpoint.Hash || anchor.AnchorID != received.AnchorID {
		t.Fatalf("unexpected anchor: returned=%+v received=%+v", anchor, received)
	}

	failServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "blocked", http.StatusConflict)
	}))
	defer failServer.Close()
	if _, err := postAuditCheckpointAnchor(context.Background(), auditAnchorConfig{
		URL: failServer.URL, Signer: signer, Client: failServer.Client(),
	}, checkpoint, signedAt); err == nil {
		t.Fatal("non-2xx audit anchor sink response unexpectedly succeeded")
	}
}

func testQueryReceiptSigner(t *testing.T, keyID string, fill byte) *queryreceipt.Signer {
	t.Helper()
	signer, err := queryreceipt.NewSigner(keyID, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fill}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer
}

func testAuditAnchorSigner(t *testing.T, keyID string, fill byte) *auditchain.AnchorSigner {
	t.Helper()
	signer, err := auditchain.NewAnchorSigner(keyID, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{fill}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("NewAnchorSigner: %v", err)
	}
	return signer
}

func testQueryReceipt(signedAt time.Time) queryreceipt.QueryReceiptV1 {
	completedAt := signedAt.Add(-time.Second)
	createdAt := completedAt.Add(-time.Minute)
	return queryreceipt.QueryReceiptV1{
		Version:        queryreceipt.VersionV3,
		ReceiptID:      "query-publish-test",
		TaskID:         "task-publish-test",
		QueryID:        "query-publish-test",
		RequestID:      "request-publish-test",
		ManifestDigest: strings.Repeat("1", 64),
		GrantDigest:    strings.Repeat("2", 64),
		CatalogDigest:  strings.Repeat("3", 64),
		CatalogVersion: "catalog-v1",
		DatasourceID:   "taskgate-test",
		SchemaDigest:   strings.Repeat("4", 64),
		RequestDigest:  strings.Repeat("5", 64),
		SQLFingerprint: "select-total",
		PolicyDecision: "ALLOW",
		BudgetBefore: queryreceipt.BudgetStateV1{
			Limits: queryreceipt.BudgetVectorV1{Queries: 3, Rows: 100, DBMS: 1000},
			Used:   queryreceipt.BudgetVectorV1{Queries: 0, Rows: 0, DBMS: 0},
		},
		BudgetReserved: queryreceipt.BudgetVectorV1{Queries: 1, Rows: 10, DBMS: 100},
		BudgetCharged:  queryreceipt.BudgetVectorV1{Queries: 1, Rows: 2, DBMS: 7},
		BudgetAfter: queryreceipt.BudgetStateV1{
			Limits: queryreceipt.BudgetVectorV1{Queries: 3, Rows: 100, DBMS: 1000},
			Used:   queryreceipt.BudgetVectorV1{Queries: 1, Rows: 2, DBMS: 7},
		},
		RowCount:          2,
		DatabaseMS:        7,
		ResultHash:        strings.Repeat("6", 64),
		Status:            queryreceipt.StatusCompleted,
		CreatedAt:         createdAt,
		CompletedAt:       completedAt,
		AuditSequence:     42,
		PreviousAuditHash: strings.Repeat("7", 64),
		AuditHash:         strings.Repeat("8", 64),
		SignedAt:          &signedAt,
	}
}

func openGatewayTestStore(t *testing.T) *control.Store {
	t.Helper()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x9d}, 32))
	if err != nil {
		t.Fatalf("NewAES256GCM: %v", err)
	}
	store, err := control.Open(context.Background(), testpostgres.SchemaDSN(t), cipher, control.WithoutStartupRecovery())
	if err != nil {
		t.Fatalf("control.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testApprovalReceipt(issuedAt time.Time) approval.ApprovalReceiptV1 {
	return approval.ApprovalReceiptV1{
		Version:        domain.ApprovalReceiptV1Version,
		ReceiptID:      "receipt-keyring-test",
		TaskID:         "task-keyring-test",
		Decision:       approval.ApprovalDecisionReject,
		ManifestDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ApproverID:     "bob",
		IssuedAt:       issuedAt,
	}
}
