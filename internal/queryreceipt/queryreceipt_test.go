package queryreceipt

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/auditchain"
)

func TestQueryReceiptSignatureBindsEveryEvidenceField(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	receipt, err := signer.Sign(validV3Receipt())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	tampered := receipt
	tampered.RequestID = "another-request"
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered receipt error = %v", err)
	}
	tampered = receipt
	tampered.SchemaDigest = fmt.Sprintf("%064x", 2)
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("schema tamper error = %v", err)
	}
	tampered = receipt
	signedAt := tampered.SignedAt.Add(time.Millisecond)
	tampered.SignedAt = &signedAt
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("signed_at tamper error = %v", err)
	}
	tampered = receipt
	tampered.GatewayKeyID = "retired-or-unknown"
	if err := verifier.Verify(tampered); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error = %v", err)
	}
	tampered = receipt
	tampered.Signature = "not-base64url"
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("malformed signature error = %v", err)
	}
}

func TestQueryReceiptV5BindsOutcomeCharge(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-v5-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := signer.Sign(validV5Receipt())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatal(err)
	}
	tampered := receipt
	tampered.Exposure = &(*receipt.Exposure)
	tampered.Exposure.ChargedOutcomeFacts = 0
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("outcome tamper error = %v, want invalid signature", err)
	}
}

func TestQueryReceiptV1VerificationRemainsCompatible(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	receipt := validReceipt()
	receipt.Version = VersionV1
	receipt.DatasourceID = ""
	receipt.SchemaDigest = ""
	receipt.SignedAt = nil
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("Sign V1: %v", err)
	}
	if err := verifier.Verify(signed); err != nil {
		t.Fatalf("Verify V1: %v", err)
	}
}

func TestQueryReceiptV2VerificationRemainsCompatible(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	receipt := validReceipt()
	receipt.Version = VersionV2
	receipt.SignedAt = nil
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("Sign V2: %v", err)
	}
	if err := verifier.Verify(signed); err != nil {
		t.Fatalf("Verify V2: %v", err)
	}
}

func TestQueryReceiptV4SignatureBindsExposureEvidence(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	receipt, err := signer.Sign(validV4Receipt())
	if err != nil {
		t.Fatalf("Sign V4: %v", err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatalf("Verify V4: %v", err)
	}

	tampered := receipt
	exposureCopy := *receipt.Exposure
	tampered.Exposure = &exposureCopy
	tampered.Exposure.ChargedReleaseFacts++
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("exposure charge tamper error = %v", err)
	}

	tampered = receipt
	exposureCopy = *receipt.Exposure
	tampered.Exposure = &exposureCopy
	tampered.Exposure.ObservationSHA256 = fmt.Sprintf("%064x", 2)
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("observation digest tamper error = %v", err)
	}
}

func TestQueryReceiptSemanticValidation(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	for _, receipt := range []QueryReceiptV1{
		validReceipt(),
		validV4Receipt(),
		validReleasedReceipt(StatusReleased, "AUTHORIZATION_EXPIRED"),
		validFailedReceipt(),
		validIndeterminateReceipt(),
	} {
		signed, err := signer.Sign(receipt)
		if err != nil {
			t.Fatalf("Sign valid %s receipt: %v", receipt.Status, err)
		}
		if err := verifier.Verify(signed); err != nil {
			t.Fatalf("Verify valid %s receipt: %v", receipt.Status, err)
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*QueryReceiptV1)
	}{
		{name: "charge exceeds reservation", mutate: func(receipt *QueryReceiptV1) { receipt.BudgetCharged.Rows = receipt.BudgetReserved.Rows + 1 }},
		{name: "after used mismatch", mutate: func(receipt *QueryReceiptV1) { receipt.BudgetAfter.Used.Rows++ }},
		{name: "reservation not released", mutate: func(receipt *QueryReceiptV1) { receipt.BudgetAfter.Reserved.Queries = 1 }},
		{name: "completed missing result hash", mutate: func(receipt *QueryReceiptV1) { receipt.ResultHash = "" }},
		{name: "released charged budget", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validReleasedReceipt(StatusReleased, "AUTHORIZATION_EXPIRED")
			receipt.BudgetCharged.Queries = 1
		}},
		{name: "failed missing error", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validFailedReceipt()
			receipt.ErrorCode = ""
		}},
		{name: "failed with result hash", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validFailedReceipt()
			receipt.ResultHash = validReceipt().ResultHash
		}},
		{name: "indeterminate partial charge", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validIndeterminateReceipt()
			receipt.BudgetCharged.Rows--
		}},
		{name: "unsupported status", mutate: func(receipt *QueryReceiptV1) { receipt.Status = "MAYBE_DONE" }},
		{name: "v3 missing signed_at", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validV3Receipt()
			receipt.SignedAt = nil
		}},
		{name: "v3 signed before terminal evidence", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validV3Receipt()
			signedAt := receipt.CompletedAt.Add(-time.Nanosecond)
			receipt.SignedAt = &signedAt
		}},
		{name: "v4 missing exposure", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validV4Receipt()
			receipt.Exposure = nil
		}},
		{name: "v3 carries exposure", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validV4Receipt()
			receipt.Version = VersionV3
		}},
		{name: "exposure charge exceeds actual", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validV4Receipt()
			receipt.Exposure.ChargedInfluenceFacts = receipt.Exposure.ActualInfluenceFacts + 1
		}},
		{name: "v5 missing outcome", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validV5Receipt()
			receipt.Exposure.ActualOutcomeFacts = 0
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			receipt := validReceipt()
			test.mutate(&receipt)
			if _, err := signer.Sign(receipt); !errors.Is(err, ErrInvalidReceipt) {
				t.Fatalf("Sign invalid receipt error = %v, want %v", err, ErrInvalidReceipt)
			}
		})
	}
}

func TestQueryReceiptKeyringHonorsOverlapAndRetirement(t *testing.T) {
	oldSigner := testSigner(t, "gateway-2026-q2", 0x22)
	newSigner := testSigner(t, "gateway-2026-q3", 0x33)
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	overlapStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	oldRetiredAt := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	keyring, err := NewKeyring(newSigner, []VerifyingKey{
		{KeyID: oldSigner.KeyID(), PublicKey: oldSigner.PublicKey(), ValidFrom: validFrom, RetiredAt: oldRetiredAt},
		{KeyID: newSigner.KeyID(), PublicKey: newSigner.PublicKey(), ValidFrom: overlapStart},
	})
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}

	oldReceipt, err := oldSigner.Sign(validV3ReceiptAt(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign old receipt: %v", err)
	}
	if err := keyring.Verify(oldReceipt); err != nil {
		t.Fatalf("old receipt signed before retirement did not verify: %v", err)
	}

	newReceipt, err := keyring.Sign(validV3ReceiptAt(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign new receipt: %v", err)
	}
	if err := keyring.Verify(newReceipt); err != nil {
		t.Fatalf("active-key receipt did not verify: %v", err)
	}

	tooLate, err := oldSigner.Sign(validV3ReceiptAt(oldRetiredAt.Add(time.Nanosecond)))
	if err != nil {
		t.Fatalf("sign post-retirement receipt: %v", err)
	}
	if err := keyring.Verify(tooLate); !errors.Is(err, ErrKeyNotValid) {
		t.Fatalf("post-retirement receipt error = %v, want %v", err, ErrKeyNotValid)
	}

	tooEarly, err := oldSigner.Sign(validV3ReceiptAt(validFrom.Add(-time.Nanosecond)))
	if err != nil {
		t.Fatalf("sign pre-validity receipt: %v", err)
	}
	if err := keyring.Verify(tooEarly); !errors.Is(err, ErrKeyNotValid) {
		t.Fatalf("pre-validity receipt error = %v, want %v", err, ErrKeyNotValid)
	}
}

func TestPublicKeyBundleBuildsVerifierForDistributedKeys(t *testing.T) {
	oldSigner := testSigner(t, "gateway-2026-q2", 0x44)
	newSigner := testSigner(t, "gateway-2026-q3", 0x55)
	validFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	overlapStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	oldRetiredAt := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	bundle, err := NewPublicKeyBundle(newSigner.KeyID(), []VerifyingKey{
		{KeyID: newSigner.KeyID(), PublicKey: newSigner.PublicKey(), ValidFrom: overlapStart},
		{KeyID: oldSigner.KeyID(), PublicKey: oldSigner.PublicKey(), ValidFrom: validFrom, RetiredAt: oldRetiredAt},
	}, time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewPublicKeyBundle: %v", err)
	}
	if bundle.Version != PublicKeyBundleVersion || bundle.ActiveKeyID != newSigner.KeyID() || len(bundle.Keys) != 2 {
		t.Fatalf("unexpected bundle: %+v", bundle)
	}
	if bundle.Keys[0].KeyID != oldSigner.KeyID() || bundle.Keys[1].KeyID != newSigner.KeyID() {
		t.Fatalf("bundle keys are not sorted by key ID: %+v", bundle.Keys)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("Marshal bundle: %v", err)
	}
	var decoded PublicKeyBundleV1
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal bundle: %v", err)
	}
	verifier, err := decoded.Verifier()
	if err != nil {
		t.Fatalf("bundle Verifier: %v", err)
	}

	oldReceipt, err := oldSigner.Sign(validV3ReceiptAt(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign old receipt: %v", err)
	}
	if err := verifier.Verify(oldReceipt); err != nil {
		t.Fatalf("old receipt from bundle verifier: %v", err)
	}
	newReceipt, err := newSigner.Sign(validV3ReceiptAt(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign new receipt: %v", err)
	}
	if err := verifier.Verify(newReceipt); err != nil {
		t.Fatalf("new receipt from bundle verifier: %v", err)
	}
	late, err := oldSigner.Sign(validV3ReceiptAt(oldRetiredAt.Add(time.Nanosecond)))
	if err != nil {
		t.Fatalf("sign late old receipt: %v", err)
	}
	if err := verifier.Verify(late); !errors.Is(err, ErrKeyNotValid) {
		t.Fatalf("late receipt error = %v, want %v", err, ErrKeyNotValid)
	}

	decoded.ActiveKeyID = "missing-active"
	if err := decoded.Validate(); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("missing active key error = %v, want %v", err, ErrUnknownKey)
	}
}

func TestQueryReceiptVerifiesAuditInclusionProof(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	events := receiptAuditEvents(t, 5)
	terminal := events[2]
	predecessor := events[1]
	receipt := validV3Receipt()
	receipt.AuditSequence = terminal.Sequence
	receipt.PreviousAuditHash = terminal.PreviousHash
	receipt.AuditHash = terminal.CurrentHash
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	proof := auditchain.InclusionProof{
		TerminalEvent:    terminal,
		PredecessorEvent: &predecessor,
		SuccessorEvents:  append([]auditchain.Event(nil), events[3:]...),
		Checkpoint:       auditchain.Checkpoint{Sequence: events[len(events)-1].Sequence, Hash: events[len(events)-1].CurrentHash},
	}
	if err := VerifyAuditInclusion(signed, proof); err != nil {
		t.Fatalf("VerifyAuditInclusion: %v", err)
	}

	tampered := proof
	tampered.TerminalEvent.QueryID = "another-query"
	if err := VerifyAuditInclusion(signed, tampered); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("query mismatch error = %v, want %v", err, ErrInvalidReceipt)
	}
	tampered = proof
	tampered.TerminalEvent.EventType = "QUERY_BUDGET_RELEASED"
	if err := VerifyAuditInclusion(signed, tampered); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("status mismatch error = %v, want %v", err, ErrInvalidReceipt)
	}
	tampered = proof
	tampered.SuccessorEvents = tampered.SuccessorEvents[:len(tampered.SuccessorEvents)-1]
	if err := VerifyAuditInclusion(signed, tampered); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("truncated path error = %v, want %v", err, ErrInvalidReceipt)
	}
}

func validReceipt() QueryReceiptV1 {
	digest := sha256.Sum256([]byte("evidence"))
	hexDigest := fmt.Sprintf("%x", digest)
	created := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	return QueryReceiptV1{
		Version: VersionV2, ReceiptID: "query-1", TaskID: "task-1", QueryID: "query-1", RequestID: "request-1",
		ManifestDigest: hexDigest, GrantDigest: hexDigest, CatalogDigest: hexDigest, CatalogVersion: "catalog-v1",
		DatasourceID: "taskgate-test-expenses", SchemaDigest: hexDigest,
		RequestDigest: hexDigest, SQLFingerprint: "select-fingerprint", PolicyDecision: "ALLOW",
		BudgetBefore:   BudgetStateV1{Limits: BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100}, Used: BudgetVectorV1{}},
		BudgetReserved: BudgetVectorV1{Queries: 1, Rows: 5, DBMS: 50},
		BudgetCharged:  BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		BudgetAfter:    BudgetStateV1{Limits: BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100}, Used: BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2}},
		RowCount:       1, DatabaseMS: 2, ResultHash: hexDigest, Status: "COMPLETED",
		CreatedAt: created, CompletedAt: created.Add(time.Millisecond), AuditSequence: 7,
		PreviousAuditHash: hexDigest, AuditHash: hexDigest,
	}
}

func validV3Receipt() QueryReceiptV1 {
	return validV3ReceiptAt(time.Date(2026, 7, 22, 0, 0, 0, int(time.Millisecond), time.UTC))
}

func validV3ReceiptAt(signedAt time.Time) QueryReceiptV1 {
	receipt := validReceipt()
	receipt.Version = VersionV3
	receipt.CreatedAt = signedAt.Add(-2 * time.Millisecond)
	receipt.CompletedAt = signedAt.Add(-time.Millisecond)
	signedAt = signedAt.UTC()
	receipt.SignedAt = &signedAt
	return receipt
}

func validV4Receipt() QueryReceiptV1 {
	receipt := validV3Receipt()
	receipt.Version = VersionV4
	digest := sha256.Sum256([]byte("normalized exposure observation"))
	receipt.Exposure = &ExposureEvidenceV1{
		RootTaskID: "task-root", ProfileVersion: "taskgate-exposure-v1",
		ActualReleaseFacts: 3, ActualInfluenceFacts: 7,
		ChargedReleaseFacts: 2, ChargedInfluenceFacts: 5,
		ObservationSHA256: fmt.Sprintf("%x", digest),
	}
	return receipt
}

func validV5Receipt() QueryReceiptV1 {
	receipt := validV4Receipt()
	receipt.Version = VersionV5
	receipt.Exposure.ProfileVersion = "taskgate-exposure-v3"
	receipt.Exposure.ActualOutcomeFacts = 1
	receipt.Exposure.ChargedOutcomeFacts = 1
	return receipt
}

func testSigner(t *testing.T, keyID string, seedByte byte) *Signer {
	t.Helper()
	signer, err := NewSigner(keyID, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("NewSigner %s: %v", keyID, err)
	}
	return signer
}

func receiptAuditEvents(t *testing.T, count int) []auditchain.Event {
	t.Helper()
	events := make([]auditchain.Event, 0, count)
	previous := auditchain.GenesisHash
	start := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	for index := 0; index < count; index++ {
		event := auditchain.Event{
			Sequence:   int64(index + 1),
			EventID:    fmt.Sprintf("receipt-audit-%d", index+1),
			TaskID:     "task-1",
			QueryID:    "query-1",
			Actor:      "alice",
			EventType:  "QUERY_COMPLETED",
			Payload:    []byte(fmt.Sprintf(`{"index":%d}`, index)),
			OccurredAt: start.Add(time.Duration(index) * time.Millisecond),
		}
		event.PreviousHash = previous
		current, err := auditchain.Hash(previous, event)
		if err != nil {
			t.Fatalf("Hash audit event %d: %v", index, err)
		}
		event.CurrentHash = current
		events = append(events, event)
		previous = current
	}
	return events
}

func validReleasedReceipt(status, errorCode string) QueryReceiptV1 {
	receipt := validReceipt()
	receipt.Status = status
	receipt.ErrorCode = errorCode
	receipt.ResultHash = ""
	receipt.RowCount = 0
	receipt.DatabaseMS = 0
	receipt.BudgetCharged = BudgetVectorV1{}
	receipt.BudgetAfter = receipt.BudgetBefore
	return receipt
}

func validFailedReceipt() QueryReceiptV1 {
	receipt := validReceipt()
	receipt.Status = StatusFailed
	receipt.ErrorCode = "RESULT_ENCODING_FAILED"
	receipt.ResultHash = ""
	return receipt
}

func validIndeterminateReceipt() QueryReceiptV1 {
	receipt := validReceipt()
	receipt.Status = StatusIndeterminate
	receipt.ErrorCode = "GATEWAY_RESTART"
	receipt.ResultHash = ""
	receipt.RowCount = receipt.BudgetReserved.Rows
	receipt.DatabaseMS = receipt.BudgetReserved.DBMS
	receipt.BudgetCharged = receipt.BudgetReserved
	receipt.BudgetAfter.Used = addVector(receipt.BudgetBefore.Used, receipt.BudgetCharged)
	receipt.BudgetAfter.Reserved = receipt.BudgetBefore.Reserved
	return receipt
}
