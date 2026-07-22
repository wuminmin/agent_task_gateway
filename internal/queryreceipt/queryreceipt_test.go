package queryreceipt

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
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

func validReceipt() QueryReceiptV1 {
	digest := sha256.Sum256([]byte("evidence"))
	hexDigest := fmt.Sprintf("%x", digest)
	created := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	return QueryReceiptV1{
		Version: VersionV1, ReceiptID: "query-1", TaskID: "task-1", QueryID: "query-1", RequestID: "request-1",
		ManifestDigest: hexDigest, GrantDigest: hexDigest, CatalogDigest: hexDigest, CatalogVersion: "catalog-v1",
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
