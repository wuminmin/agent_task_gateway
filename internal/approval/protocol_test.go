package approval

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

func TestAuthorizationManifestV1RFC8785FixedVector(t *testing.T) {
	manifest := testManifest()
	canonical, err := CanonicalJSON(manifest)
	if err != nil {
		t.Fatalf("canonicalize manifest: %v", err)
	}
	wantCanonical := `{"agent_id":"agent:research-01","approved_columns":{"expense_detail":["amount","receipt_no"],"expense_summary":["month","total_amount"]},"budget":{"max_db_ms":15000,"max_queries":3,"max_result_rows":50,"per_query_timeout_ms":5000,"task_ttl_ms":900000},"callback_context":"callback-vector-001","catalog_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","catalog_version":"catalog-v1","datasource_id":"taskgate-test-expenses","declared_objective":"Compare H1 expenses for 销售部","human_subject":"oidc:alice@example.com","mandatory_scope":{"department":["销售部"],"expense_date":{"from":"2026-01-01","to":"2026-06-30"}},"nonce":"000102030405060708090a0b0c0d0e0f","products":["expense_detail","expense_summary"],"schema_digest":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789","sensitivity":"high","task_id":"task-vector-001","version":"1"}`
	if string(canonical) != wantCanonical {
		t.Fatalf("canonical manifest mismatch\n got: %s\nwant: %s", canonical, wantCanonical)
	}
	digest, err := ManifestDigest(manifest)
	if err != nil {
		t.Fatalf("digest manifest: %v", err)
	}
	const wantDigest = "d9217433a20331bddcd2e697e73401211988b0eeca51fdd4420f56681804e056"
	if digest != wantDigest {
		t.Fatalf("manifest digest = %s, want %s", digest, wantDigest)
	}
}

func TestAuthorizationManifestDigestStableAndTamperEvident(t *testing.T) {
	first := testManifest()
	second := testManifest()
	second.ApprovedColumns = map[string][]string{
		"expense_summary": {"month", "total_amount"},
		"expense_detail":  {"amount", "receipt_no"},
	}
	second.MandatoryScope = map[string]any{
		"expense_date": map[string]any{"to": "2026-06-30", "from": "2026-01-01"},
		"department":   []any{"销售部"},
	}
	firstDigest, err := ManifestDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := ManifestDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("equivalent object maps produced different digests: %s != %s", firstDigest, secondDigest)
	}
	request := DraftRequest{Manifest: first, ManifestDigest: firstDigest, ApprovalMode: "manual", Approver: "bob"}
	if err := ValidateAuthorizationSnapshot(request); err != nil {
		t.Fatalf("validate manifest: %v", err)
	}
	automatic := request
	automatic.ApprovalMode = "auto"
	automatic.Approver = ""
	if err := ValidateAuthorizationSnapshot(automatic); err == nil {
		t.Fatal("automatic approval request was accepted")
	}
	request.Manifest.Budget.MaxResultRows++
	if err := ValidateAuthorizationSnapshot(request); !errors.Is(err, ErrProtocolDigestMatch) {
		t.Fatalf("tampered manifest error = %v, want digest mismatch", err)
	}
}

func TestAuthorizationManifestDigestBindsOptionalViewBinding(t *testing.T) {
	legacy := testManifest()
	legacyDigest, err := ManifestDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	bound := testManifest()
	bound.ViewBindingDigest = strings.Repeat("f", 64)
	boundDigest, err := ManifestDigest(bound)
	if err != nil {
		t.Fatal(err)
	}
	if boundDigest == legacyDigest {
		t.Fatal("view binding digest did not partition manifest identity")
	}
	core, err := domain.CoreFromManifest(bound, boundDigest, time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if core.ViewBindingDigest != bound.ViewBindingDigest {
		t.Fatalf("core view binding digest = %q, want %q", core.ViewBindingDigest, bound.ViewBindingDigest)
	}
}

func TestCanonicalJSONUsesUTF16OrderingAndMinimalEscapes(t *testing.T) {
	value := map[string]any{
		"\ue000": "<>&\u2028", // BMP private use sorts after the surrogate pair.
		"😀":      "\n\t\u0001",
	}
	canonical, err := CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"😀\":\"\\n\\t\\u0001\",\"\ue000\":\"<>&\u2028\"}"
	if string(canonical) != want {
		t.Fatalf("canonical JSON = %q, want %q", canonical, want)
	}
}

func TestCanonicalJSONRejectsInvalidUTF8(t *testing.T) {
	invalid := string([]byte{0xff})
	if _, err := CanonicalJSON(map[string]string{"value": invalid}); !errors.Is(err, ErrCanonicalJSON) {
		t.Fatalf("invalid UTF-8 error = %v, want ErrCanonicalJSON", err)
	}
}

func TestCanonicalJSONRejectsDuplicateObjectNames(t *testing.T) {
	if _, err := CanonicalJSON(json.RawMessage(`{"scope":1,"scope":2}`)); !errors.Is(err, ErrCanonicalJSON) {
		t.Fatalf("duplicate property error = %v, want ErrCanonicalJSON", err)
	}
}

func TestCanonicalJSONUsesECMAScriptNumberSerialization(t *testing.T) {
	canonical, err := CanonicalJSON(json.RawMessage(`[333333333.33333329,1E30,4.50,2e-3,0.000000000000000000000000001,-0]`))
	if err != nil {
		t.Fatal(err)
	}
	want := `[333333333.3333333,1e+30,4.5,0.002,1e-27,0]`
	if string(canonical) != want {
		t.Fatalf("canonical numbers = %s, want %s", canonical, want)
	}
}

func TestApprovalReceiptV1Ed25519RoundTripAndTampering(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	signer, err := NewEd25519ReceiptSigner("oa-test-2026", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewEd25519ReceiptVerifier(map[string]ed25519.PublicKey{
		"oa-test-2026": privateKey.Public().(ed25519.PublicKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	manifestDigest, _ := ManifestDigest(manifest)
	issuedAt := time.Date(2026, 7, 22, 8, 9, 10, 123000000, time.UTC)
	core, err := domain.CoreFromManifest(manifest, manifestDigest, issuedAt)
	if err != nil {
		t.Fatal(err)
	}
	grantDigest, _ := GrantCoreDigest(core)
	receipt, err := signer.SignReceipt(ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "receipt-vector-1",
		TaskID: manifest.TaskID, Decision: ApprovalDecisionApprove,
		ManifestDigest: manifestDigest, ApprovedGrantDigest: grantDigest,
		ApproverID: "bob", IssuedAt: issuedAt,
	})
	if err != nil {
		t.Fatalf("sign receipt: %v", err)
	}
	finalGrant := TaskGrantV1{Version: domain.TaskGrantV1Version, Core: core, ApprovalReceipt: receipt}
	if err := VerifyTaskGrantV1(verifier, finalGrant); err != nil {
		t.Fatalf("verify signed final grant: %v", err)
	}

	tampered := receipt
	tampered.ApproverID = "mallory"
	if err := verifier.VerifyReceipt(tampered); !errors.Is(err, ErrInvalidReceiptSig) {
		t.Fatalf("tampered receipt error = %v, want invalid signature", err)
	}
	tampered = receipt
	tampered.KeyID = "retired-or-unknown"
	if err := verifier.VerifyReceipt(tampered); !errors.Is(err, ErrUnknownReceiptKey) {
		t.Fatalf("unknown key error = %v", err)
	}
	tampered = receipt
	tampered.Signature = strings.Repeat("A", len(receipt.Signature))
	if err := verifier.VerifyReceipt(tampered); !errors.Is(err, ErrInvalidReceiptSig) {
		t.Fatalf("signature tamper error = %v", err)
	}
}

func TestApprovalReceiptKeyringHonorsOverlapAndRetirement(t *testing.T) {
	oldSigner := testReceiptSigner(t, "oa-2026-q2", 0x44)
	newSigner := testReceiptSigner(t, "oa-2026-q3", 0x55)
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	overlapStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	oldRetiredAt := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	verifier, err := NewEd25519ReceiptVerifierWithKeyring([]ReceiptVerifyingKey{
		{KeyID: oldSigner.KeyID(), PublicKey: oldSigner.key.Public().(ed25519.PublicKey), ValidFrom: validFrom, RetiredAt: oldRetiredAt},
		{KeyID: newSigner.KeyID(), PublicKey: newSigner.key.Public().(ed25519.PublicKey), ValidFrom: overlapStart},
	})
	if err != nil {
		t.Fatalf("NewEd25519ReceiptVerifierWithKeyring: %v", err)
	}

	oldGrant := signedTestGrant(t, oldSigner, time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC), "old")
	if err := VerifyTaskGrantV1(verifier, oldGrant); err != nil {
		t.Fatalf("old grant signed before retirement did not verify: %v", err)
	}

	newGrant := signedTestGrant(t, newSigner, time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), "new")
	if err := VerifyTaskGrantV1(verifier, newGrant); err != nil {
		t.Fatalf("active-key grant did not verify: %v", err)
	}

	tooLate := signedTestGrant(t, oldSigner, oldRetiredAt.Add(time.Nanosecond), "late")
	if err := VerifyTaskGrantV1(verifier, tooLate); !errors.Is(err, ErrReceiptKeyNotValid) {
		t.Fatalf("post-retirement grant error = %v, want %v", err, ErrReceiptKeyNotValid)
	}

	tooEarly := signedTestGrant(t, oldSigner, validFrom.Add(-time.Nanosecond), "early")
	if err := VerifyTaskGrantV1(verifier, tooEarly); !errors.Is(err, ErrReceiptKeyNotValid) {
		t.Fatalf("pre-validity grant error = %v, want %v", err, ErrReceiptKeyNotValid)
	}
}

func testReceiptSigner(t *testing.T, keyID string, seedByte byte) *Ed25519ReceiptSigner {
	t.Helper()
	signer, err := NewEd25519ReceiptSigner(keyID, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("NewEd25519ReceiptSigner %s: %v", keyID, err)
	}
	return signer
}

func signedTestGrant(t *testing.T, signer ReceiptSigner, issuedAt time.Time, suffix string) TaskGrantV1 {
	t.Helper()
	manifest := testManifest()
	manifestDigest, err := ManifestDigest(manifest)
	if err != nil {
		t.Fatalf("ManifestDigest: %v", err)
	}
	core, err := domain.CoreFromManifest(manifest, manifestDigest, issuedAt)
	if err != nil {
		t.Fatalf("CoreFromManifest: %v", err)
	}
	grantDigest, err := GrantCoreDigest(core)
	if err != nil {
		t.Fatalf("GrantCoreDigest: %v", err)
	}
	receipt, err := signer.SignReceipt(ApprovalReceiptV1{
		Version: domain.ApprovalReceiptV1Version, ReceiptID: "receipt-" + suffix,
		TaskID: manifest.TaskID, Decision: ApprovalDecisionApprove,
		ManifestDigest: manifestDigest, ApprovedGrantDigest: grantDigest,
		ApproverID: "bob", IssuedAt: issuedAt,
	})
	if err != nil {
		t.Fatalf("SignReceipt: %v", err)
	}
	return TaskGrantV1{Version: domain.TaskGrantV1Version, Core: core, ApprovalReceipt: receipt}
}

func testManifest() AuthorizationManifestV1 {
	return AuthorizationManifestV1{
		Version: domain.AuthorizationManifestV1Version,
		TaskID:  "task-vector-001", HumanSubject: "oidc:alice@example.com", AgentID: "agent:research-01",
		DeclaredObjective: "Compare H1 expenses for 销售部",
		Products:          []string{"expense_detail", "expense_summary"},
		ApprovedColumns: map[string][]string{
			"expense_detail":  {"amount", "receipt_no"},
			"expense_summary": {"month", "total_amount"},
		},
		MandatoryScope: map[string]any{
			"department":   []string{"销售部"},
			"expense_date": map[string]string{"from": "2026-01-01", "to": "2026-06-30"},
		},
		Sensitivity: domain.SensitivityHigh,
		Budget: AuthorizationBudgetV1{
			MaxQueries: 3, MaxResultRows: 50, MaxDBMS: 15_000,
			PerQueryTimeoutMS: 5_000, TaskTTLMS: 900_000,
		},
		CatalogVersion:  "catalog-v1",
		CatalogSHA256:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		DatasourceID:    "taskgate-test-expenses",
		SchemaDigest:    "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		CallbackContext: "callback-vector-001", Nonce: "000102030405060708090a0b0c0d0e0f",
	}
}
