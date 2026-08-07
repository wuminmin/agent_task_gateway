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
	receipt, err := signer.Sign(validReceipt())
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

func TestExposureV3BindsOutcomeCharge(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-v5-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := signer.Sign(exposureV3Receipt())
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

func TestExposureV4BindsOrdinalLedgerEvidence(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-v6-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := signer.Sign(exposureV4Receipt())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ExposureEvidenceV1){
		"dictionary": func(value *ExposureEvidenceV1) { value.DictionarySetSHA256 = fmt.Sprintf("%064x", 2) },
		"release":    func(value *ExposureEvidenceV1) { value.ReleaseSetSHA256 = fmt.Sprintf("%064x", 3) },
		"epoch":      func(value *ExposureEvidenceV1) { value.RootEpoch++ },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := receipt
			copyExposure := *receipt.Exposure
			tampered.Exposure = &copyExposure
			mutate(tampered.Exposure)
			if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("tamper error = %v, want invalid signature", err)
			}
		})
	}
}

func TestExposureV5BindsPredicateAndCompositeEvidence(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-v7-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := signer.Sign(exposureV5Receipt())
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ExposureEvidenceV1){
		"predicate set": func(value *ExposureEvidenceV1) { value.PredicateSetSHA256 = fmt.Sprintf("%064x", 20) },
		"atom count":    func(value *ExposureEvidenceV1) { value.ActualPredicateAtomCount++ },
		"composite":     func(value *ExposureEvidenceV1) { value.CompositeOutcomeSHA256 = fmt.Sprintf("%064x", 21) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := receipt
			copyExposure := *receipt.Exposure
			tampered.Exposure = &copyExposure
			mutate(tampered.Exposure)
			if err := verifier.Verify(tampered); err == nil {
				t.Fatal("tampered V7 receipt was accepted")
			}
		})
	}
}

func TestArtifactDeliveryBindsCompleteArtifactIntent(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-v8-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	receipt := validArtifactReceipt(t)
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.Verify(signed); err != nil {
		t.Fatal(err)
	}

	tampered := signed
	changed := *signed.ArtifactIntent
	changed.ResultID = "result-another"
	changed, err = BuildArtifactIntent(changed)
	if err != nil {
		t.Fatal(err)
	}
	tampered.ArtifactIntent = &changed
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("re-sealed artifact tamper error = %v, want invalid signature", err)
	}

	tampered = signed
	changed = *signed.ArtifactIntent
	changed.ResultMetadataSHA256 = fmt.Sprintf("%064x", 98)
	changed, err = BuildArtifactIntent(changed)
	if err != nil {
		t.Fatal(err)
	}
	tampered.ArtifactIntent = &changed
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("re-sealed result metadata tamper error = %v, want invalid signature", err)
	}

	missingMetadata := receipt
	missingIntent := *receipt.ArtifactIntent
	missingIntent.ResultMetadataSHA256 = ""
	missingMetadata.ArtifactIntent = &missingIntent
	if _, err := signer.Sign(missingMetadata); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("missing result metadata digest error = %v, want invalid receipt", err)
	}

	legacy := exposureV5Receipt()
	legacy.ArtifactIntent = receipt.ArtifactIntent
	if _, err := signer.Sign(legacy); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("V7 artifact intent error = %v, want invalid receipt", err)
	}

	broken := receipt
	copyIntent := *receipt.ArtifactIntent
	copyIntent.ObjectSHA256 = fmt.Sprintf("%064x", 99)
	broken.ArtifactIntent = &copyIntent
	if _, err := signer.Sign(broken); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("stale intent digest error = %v, want invalid receipt", err)
	}
}

func TestSignatureBindsExposureEvidence(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-secret"))
	verifier, err := NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	receipt, err := signer.Sign(exposureV1Receipt())
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifier.Verify(receipt); err != nil {
		t.Fatalf("Verify: %v", err)
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
		exposureV1Receipt(),
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
			*receipt = validReceipt()
			receipt.SignedAt = nil
		}},
		{name: "v3 signed before terminal evidence", mutate: func(receipt *QueryReceiptV1) {
			*receipt = validReceipt()
			signedAt := receipt.CompletedAt.Add(-time.Nanosecond)
			receipt.SignedAt = &signedAt
		}},
		{name: "exposure names a profile with no stated shape", mutate: func(receipt *QueryReceiptV1) {
			*receipt = exposureV1Receipt()
			receipt.Exposure.ProfileVersion = "taskgate-exposure-v6"
		}},
		{name: "exposure charge exceeds actual", mutate: func(receipt *QueryReceiptV1) {
			*receipt = exposureV1Receipt()
			receipt.Exposure.ChargedInfluenceFacts = receipt.Exposure.ActualInfluenceFacts + 1
		}},
		{name: "outcome profile with no outcome", mutate: func(receipt *QueryReceiptV1) {
			*receipt = exposureV3Receipt()
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

	oldReceipt, err := oldSigner.Sign(validReceiptAt(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign old receipt: %v", err)
	}
	if err := keyring.Verify(oldReceipt); err != nil {
		t.Fatalf("old receipt signed before retirement did not verify: %v", err)
	}

	newReceipt, err := keyring.Sign(validReceiptAt(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign new receipt: %v", err)
	}
	if err := keyring.Verify(newReceipt); err != nil {
		t.Fatalf("active-key receipt did not verify: %v", err)
	}

	tooLate, err := oldSigner.Sign(validReceiptAt(oldRetiredAt.Add(time.Nanosecond)))
	if err != nil {
		t.Fatalf("sign post-retirement receipt: %v", err)
	}
	if err := keyring.Verify(tooLate); !errors.Is(err, ErrKeyNotValid) {
		t.Fatalf("post-retirement receipt error = %v, want %v", err, ErrKeyNotValid)
	}

	tooEarly, err := oldSigner.Sign(validReceiptAt(validFrom.Add(-time.Nanosecond)))
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

	oldReceipt, err := oldSigner.Sign(validReceiptAt(time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign old receipt: %v", err)
	}
	if err := verifier.Verify(oldReceipt); err != nil {
		t.Fatalf("old receipt from bundle verifier: %v", err)
	}
	newReceipt, err := newSigner.Sign(validReceiptAt(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("sign new receipt: %v", err)
	}
	if err := verifier.Verify(newReceipt); err != nil {
		t.Fatalf("new receipt from bundle verifier: %v", err)
	}
	late, err := oldSigner.Sign(validReceiptAt(oldRetiredAt.Add(time.Nanosecond)))
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
	receipt := validReceipt()
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

func TestArtifactDeliveryVerifiesTerminalAndArtifactInclusion(t *testing.T) {
	signer := DemoSigner([]byte("unit-test-v8-inclusion"))
	receipt := validArtifactReceipt(t)
	baseIntent := *receipt.ArtifactIntent

	terminal := auditchain.Event{
		Sequence: 1, EventID: "audit-terminal", TaskID: receipt.TaskID, QueryID: receipt.QueryID,
		Actor: "alice", EventType: "QUERY_COMPLETED", Payload: []byte(`{"status":"COMPLETED"}`),
		OccurredAt: time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC), PreviousHash: auditchain.GenesisHash,
	}
	terminal.CurrentHash = mustAuditHash(t, terminal.PreviousHash, terminal)
	registration := auditchain.Event{
		Sequence: 2, EventID: "audit-registration", TaskID: receipt.TaskID, QueryID: receipt.QueryID,
		Actor: "alice", EventType: "QUERY_RESULT_OBJECT_REGISTERED",
		Payload:    mustJSONForTest(t, artifactRegistrationPayload(baseIntent)),
		OccurredAt: terminal.OccurredAt.Add(time.Millisecond), PreviousHash: terminal.CurrentHash,
	}
	registration.CurrentHash = mustAuditHash(t, registration.PreviousHash, registration)
	receipt.AuditSequence = terminal.Sequence
	receipt.PreviousAuditHash = terminal.PreviousHash
	receipt.AuditHash = terminal.CurrentHash
	baseIntent.RegistrationAuditSequence = registration.Sequence
	baseIntent.RegistrationPreviousAuditHash = registration.PreviousHash
	baseIntent.RegistrationAuditHash = registration.CurrentHash
	baseIntent, err := BuildArtifactIntent(baseIntent)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ArtifactIntent = &baseIntent
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatal(err)
	}
	terminalProof := auditchain.InclusionProof{
		TerminalEvent: terminal, SuccessorEvents: []auditchain.Event{registration},
		Checkpoint: auditchain.Checkpoint{Sequence: registration.Sequence, Hash: registration.CurrentHash},
	}
	registrationProof := auditchain.InclusionProof{
		TerminalEvent: registration, PredecessorEvent: &terminal,
		Checkpoint: auditchain.Checkpoint{Sequence: registration.Sequence, Hash: registration.CurrentHash},
	}
	if err := VerifyArtifactIntentInclusion(signed, terminalProof, registrationProof); err != nil {
		t.Fatalf("VerifyArtifactIntentInclusion: %v", err)
	}

	wrongPayload := registration
	wrongPayload.Payload = []byte(`{"status":"PENDING"}`)
	wrongPayload.CurrentHash = mustAuditHash(t, wrongPayload.PreviousHash, wrongPayload)
	wrongIntent := baseIntent
	wrongIntent.RegistrationAuditHash = wrongPayload.CurrentHash
	wrongIntent, err = BuildArtifactIntent(wrongIntent)
	if err != nil {
		t.Fatal(err)
	}
	wrongReceipt := receipt
	wrongReceipt.ArtifactIntent = &wrongIntent
	wrongSigned, err := signer.Sign(wrongReceipt)
	if err != nil {
		t.Fatal(err)
	}
	wrongRegistrationProof := auditchain.InclusionProof{
		TerminalEvent: wrongPayload, PredecessorEvent: &terminal,
		Checkpoint: auditchain.Checkpoint{Sequence: wrongPayload.Sequence, Hash: wrongPayload.CurrentHash},
	}
	terminalOnlyProof := auditchain.InclusionProof{
		TerminalEvent: terminal,
		Checkpoint:    auditchain.Checkpoint{Sequence: terminal.Sequence, Hash: terminal.CurrentHash},
	}
	if err := VerifyArtifactIntentInclusion(wrongSigned, terminalOnlyProof, wrongRegistrationProof); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("registration payload mismatch error = %v, want invalid receipt", err)
	}
}

func TestArtifactDeliveryVerifiesAvailabilityInclusion(t *testing.T) {
	receipt := validArtifactReceipt(t)
	intent := *receipt.ArtifactIntent
	registration := auditchain.Event{
		Sequence: intent.RegistrationAuditSequence, EventID: "audit-registration",
		TaskID: receipt.TaskID, QueryID: receipt.QueryID, Actor: "gateway",
		EventType: "QUERY_RESULT_OBJECT_REGISTERED", Payload: []byte(`{"status":"PENDING"}`),
		OccurredAt: receipt.CompletedAt.Add(time.Millisecond), PreviousHash: receipt.AuditHash,
	}
	registration.CurrentHash = mustAuditHash(t, registration.PreviousHash, registration)
	intent.RegistrationAuditHash = registration.CurrentHash
	intent, err := BuildArtifactIntent(intent)
	if err != nil {
		t.Fatal(err)
	}
	receipt.ArtifactIntent = &intent
	event := auditchain.Event{
		Sequence: registration.Sequence + 1, EventID: "audit-available", TaskID: receipt.TaskID, QueryID: receipt.QueryID,
		Actor: "gateway", EventType: "QUERY_RESULT_CONSUMED",
		Payload: mustJSONForTest(t, map[string]any{
			"result_id": intent.ResultID, "result_sha256": intent.ParquetSHA256,
			"object_sha256": intent.ObjectSHA256, "format": intent.Format,
			"status": "AVAILABLE", "consumed_at": "2026-07-22T01:00:00Z",
		}),
		OccurredAt: registration.OccurredAt.Add(time.Millisecond), PreviousHash: registration.CurrentHash,
	}
	event.CurrentHash = mustAuditHash(t, event.PreviousHash, event)
	proof := auditchain.InclusionProof{
		TerminalEvent: event, PredecessorEvent: &registration,
		Checkpoint: auditchain.Checkpoint{Sequence: event.Sequence, Hash: event.CurrentHash},
	}
	if err := VerifyArtifactAvailabilityInclusion(receipt, proof); err != nil {
		t.Fatalf("VerifyArtifactAvailabilityInclusion: %v", err)
	}
	tampered := event
	tampered.Payload = mustJSONForTest(t, map[string]any{
		"result_id": intent.ResultID, "result_sha256": intent.ParquetSHA256,
		"object_sha256": fmt.Sprintf("%064x", 99), "format": intent.Format,
		"status": "AVAILABLE", "consumed_at": "2026-07-22T01:00:00Z",
	})
	tampered.CurrentHash = mustAuditHash(t, tampered.PreviousHash, tampered)
	tamperedProof := auditchain.InclusionProof{
		TerminalEvent: tampered, PredecessorEvent: &registration,
		Checkpoint: auditchain.Checkpoint{Sequence: tampered.Sequence, Hash: tampered.CurrentHash},
	}
	if err := VerifyArtifactAvailabilityInclusion(receipt, tamperedProof); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("tampered availability payload error = %v, want invalid receipt", err)
	}

	malformedTime := event
	malformedTime.Payload = mustJSONForTest(t, map[string]any{
		"result_id": intent.ResultID, "result_sha256": intent.ParquetSHA256,
		"object_sha256": intent.ObjectSHA256, "format": intent.Format,
		"status": "AVAILABLE", "consumed_at": "not-a-timestamp",
	})
	malformedTime.CurrentHash = mustAuditHash(t, malformedTime.PreviousHash, malformedTime)
	malformedTimeProof := auditchain.InclusionProof{
		TerminalEvent: malformedTime, PredecessorEvent: &registration,
		Checkpoint: auditchain.Checkpoint{Sequence: malformedTime.Sequence, Hash: malformedTime.CurrentHash},
	}
	if err := VerifyArtifactAvailabilityInclusion(receipt, malformedTimeProof); !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("malformed consumed_at error = %v, want invalid receipt", err)
	}

	for _, registrationSequence := range []int64{event.Sequence, event.Sequence + 1} {
		outOfOrderReceipt := receipt
		outOfOrderIntent := intent
		outOfOrderIntent.RegistrationAuditSequence = registrationSequence
		outOfOrderIntent, err = BuildArtifactIntent(outOfOrderIntent)
		if err != nil {
			t.Fatal(err)
		}
		outOfOrderReceipt.ArtifactIntent = &outOfOrderIntent
		if err := VerifyArtifactAvailabilityInclusion(outOfOrderReceipt, proof); !errors.Is(err, ErrInvalidReceipt) {
			t.Fatalf("availability sequence %d after registration %d error = %v, want invalid receipt",
				event.Sequence, registrationSequence, err)
		}
	}
}

// validReceipt is the smallest receipt this build signs: a completed operation
// that returned its rows inline and accounted no exposure.
//
// Every other fixture below is this one plus the evidence a condition adds. They
// are not a version ladder -- there is one version -- they are the experiment
// conditions the same receipt describes.
func validReceipt() QueryReceiptV1 {
	return validReceiptAt(time.Date(2026, 7, 22, 0, 0, 0, int(time.Millisecond), time.UTC))
}

func validReceiptAt(signedAt time.Time) QueryReceiptV1 {
	digest := sha256.Sum256([]byte("evidence"))
	hexDigest := fmt.Sprintf("%x", digest)
	signedAt = signedAt.UTC()
	return QueryReceiptV1{
		Version: Version, ReceiptID: "query-1", TaskID: "task-1", QueryID: "query-1", RequestID: "request-1",
		ManifestDigest: hexDigest, GrantDigest: hexDigest, CatalogDigest: hexDigest, CatalogVersion: "catalog-v1",
		DatasourceID: "taskgate-test-expenses", SchemaDigest: hexDigest,
		RequestDigest: hexDigest, SQLFingerprint: "select-fingerprint", PolicyDecision: "ALLOW",
		BudgetBefore:   BudgetStateV1{Limits: BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100}, Used: BudgetVectorV1{}},
		BudgetReserved: BudgetVectorV1{Queries: 1, Rows: 5, DBMS: 50},
		BudgetCharged:  BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		BudgetAfter:    BudgetStateV1{Limits: BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100}, Used: BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2}},
		RowCount:       1, DatabaseMS: 2, ResultHash: hexDigest, Status: "COMPLETED",
		CreatedAt: signedAt.Add(-2 * time.Millisecond), CompletedAt: signedAt.Add(-time.Millisecond),
		SignedAt: &signedAt, AuditSequence: 7,
		PreviousAuditHash: hexDigest, AuditHash: hexDigest,
		ResultDeliveryMode: DeliveryInline,
	}
}

// exposureV1Receipt accounts under a profile with no outcome dimension.
func exposureV1Receipt() QueryReceiptV1 {
	receipt := validReceipt()
	digest := sha256.Sum256([]byte("normalized exposure observation"))
	receipt.Exposure = &ExposureEvidenceV1{
		RootTaskID: "task-root", ProfileVersion: "taskgate-exposure-v1",
		ActualReleaseFacts: 3, ActualInfluenceFacts: 7,
		ChargedReleaseFacts: 2, ChargedInfluenceFacts: 5,
		ObservationSHA256: fmt.Sprintf("%x", digest),
	}
	return receipt
}

// exposureV3Receipt adds the outcome dimension.
func exposureV3Receipt() QueryReceiptV1 {
	receipt := exposureV1Receipt()
	receipt.Exposure.ProfileVersion = "taskgate-exposure-v3"
	receipt.Exposure.ActualOutcomeFacts = 1
	receipt.Exposure.ChargedOutcomeFacts = 1
	return receipt
}

// exposureV4Receipt adds the ordinal ledger identities and the root epoch.
func exposureV4Receipt() QueryReceiptV1 {
	receipt := exposureV3Receipt()
	receipt.Exposure.ProfileVersion = "taskgate-exposure-v4"
	receipt.Exposure.DictionarySetSHA256 = fmt.Sprintf("%064x", 10)
	receipt.Exposure.ReleaseSetSHA256 = fmt.Sprintf("%064x", 11)
	receipt.Exposure.InfluenceSetSHA256 = fmt.Sprintf("%064x", 12)
	receipt.Exposure.OutcomeSetSHA256 = fmt.Sprintf("%064x", 13)
	receipt.Exposure.RootEpoch = 7
	return receipt
}

// exposureV5Receipt adds the predicate footprint and the composite outcome.
func exposureV5Receipt() QueryReceiptV1 {
	receipt := exposureV4Receipt()
	receipt.Exposure.ProfileVersion = "taskgate-exposure-v5"
	receipt.Exposure.PredicateProfileVersion = "taskgate-predicate-footprint-v1"
	receipt.Exposure.PredicateContextSHA256 = fmt.Sprintf("%064x", 14)
	receipt.Exposure.PredicateSetSHA256 = fmt.Sprintf("%064x", 15)
	receipt.Exposure.ActualPredicateAtomCount = 1
	receipt.Exposure.ChargedPredicateAtomCount = 1
	receipt.Exposure.CompositeOutcomeSHA256 = fmt.Sprintf("%064x", 16)
	receipt.Exposure.ActualCompositeCount = 1
	receipt.Exposure.ChargedCompositeCount = 1
	receipt.Exposure.ActualOutcomeFacts = 2
	receipt.Exposure.ChargedOutcomeFacts = 2
	return receipt
}

// validArtifactReceipt delivered its result to a registered result object.
func validArtifactReceipt(t *testing.T) QueryReceiptV1 {
	t.Helper()
	receipt := exposureV5Receipt()
	receipt.ResultDeliveryMode = DeliveryArtifact
	expires := receipt.CompletedAt.Add(time.Hour)
	intent, err := BuildArtifactIntent(ArtifactIntentEvidenceV1{
		Version: ArtifactIntentVersionV1, ResultID: "result-query-1", Format: "parquet",
		Encryption: "chunked-aes-gcm-v1", KeyID: "result-key-1",
		ParquetSHA256: receipt.ResultHash, ObjectSHA256: fmt.Sprintf("%064x", 22),
		ParquetSize: 128, ObjectSize: 160, RowCount: receipt.RowCount, ColumnCount: 2,
		SchemaSHA256: fmt.Sprintf("%064x", 23), ResultMetadataSHA256: fmt.Sprintf("%064x", 24),
		ACLSHA256:       fmt.Sprintf("%064x", 25),
		ObjectKeySHA256: fmt.Sprintf("%064x", 26), StagingKeySHA256: fmt.Sprintf("%064x", 27),
		ExpiresAt: &expires, Status: ArtifactStatusPending,
		RegistrationAuditSequence:     receipt.AuditSequence + 1,
		RegistrationPreviousAuditHash: receipt.AuditHash, RegistrationAuditHash: fmt.Sprintf("%064x", 28),
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt.ArtifactIntent = &intent
	return receipt
}

func mustAuditHash(t *testing.T, previous string, event auditchain.Event) string {
	t.Helper()
	value, err := auditchain.Hash(previous, event)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustJSONForTest(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
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
	// A query that did not complete has no result to have delivered.
	receipt.ResultDeliveryMode = DeliveryNone
	return receipt
}

func validFailedReceipt() QueryReceiptV1 {
	receipt := validReceipt()
	receipt.Status = StatusFailed
	receipt.ErrorCode = "RESULT_ENCODING_FAILED"
	receipt.ResultHash = ""
	// A query that did not complete has no result to have delivered.
	receipt.ResultDeliveryMode = DeliveryNone
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
	// A query that did not complete has no result to have delivered.
	receipt.ResultDeliveryMode = DeliveryNone
	return receipt
}
