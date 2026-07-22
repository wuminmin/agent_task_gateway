// Package queryreceipt defines the persistent, gateway-signed evidence for one
// authorized query. Approval receipts and query receipts intentionally use
// different keys and domain separators.
package queryreceipt

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
)

const (
	VersionV1       = "1"
	signatureDomain = "TASKGATE-QUERY-RECEIPT-V1\x00"
)

var (
	ErrInvalidReceipt   = errors.New("invalid query receipt")
	ErrInvalidKey       = errors.New("invalid query receipt key")
	ErrUnknownKey       = errors.New("unknown query receipt key ID")
	ErrInvalidSignature = errors.New("invalid query receipt signature")
)

type BudgetVectorV1 struct {
	Queries int64 `json:"queries"`
	Rows    int64 `json:"rows"`
	DBMS    int64 `json:"db_ms"`
}

type BudgetStateV1 struct {
	Limits   BudgetVectorV1 `json:"limits"`
	Used     BudgetVectorV1 `json:"used"`
	Reserved BudgetVectorV1 `json:"reserved"`
}

// QueryReceiptV1 contains no raw rows, SQL text, credentials, or physical
// relation names. Signature is unpadded base64url Ed25519.
type QueryReceiptV1 struct {
	Version           string         `json:"version"`
	ReceiptID         string         `json:"receipt_id"`
	TaskID            string         `json:"task_id"`
	QueryID           string         `json:"query_id"`
	RequestID         string         `json:"request_id"`
	ManifestDigest    string         `json:"manifest_digest"`
	GrantDigest       string         `json:"grant_digest"`
	CatalogDigest     string         `json:"catalog_digest"`
	CatalogVersion    string         `json:"catalog_version"`
	RequestDigest     string         `json:"request_digest"`
	SQLFingerprint    string         `json:"sql_fingerprint"`
	PolicyDecision    string         `json:"policy_decision"`
	BudgetBefore      BudgetStateV1  `json:"budget_before"`
	BudgetReserved    BudgetVectorV1 `json:"budget_reserved"`
	BudgetCharged     BudgetVectorV1 `json:"budget_charged"`
	BudgetAfter       BudgetStateV1  `json:"budget_after"`
	RowCount          int64          `json:"row_count"`
	DatabaseMS        int64          `json:"db_ms"`
	ResultHash        string         `json:"result_hash"`
	Status            string         `json:"status"`
	ErrorCode         string         `json:"error_code"`
	CreatedAt         time.Time      `json:"created_at"`
	CompletedAt       time.Time      `json:"completed_at"`
	AuditSequence     int64          `json:"audit_sequence"`
	PreviousAuditHash string         `json:"previous_audit_hash"`
	AuditHash         string         `json:"audit_hash"`
	GatewayKeyID      string         `json:"gateway_key_id"`
	Signature         string         `json:"signature"`
}

func (r QueryReceiptV1) ValidateUnsigned() error {
	if r.Version != VersionV1 || strings.TrimSpace(r.ReceiptID) == "" ||
		strings.TrimSpace(r.TaskID) == "" || strings.TrimSpace(r.QueryID) == "" ||
		strings.TrimSpace(r.RequestID) == "" || r.ReceiptID != r.QueryID {
		return fmt.Errorf("%w: identity field is missing or inconsistent", ErrInvalidReceipt)
	}
	for name, value := range map[string]string{
		"manifest_digest": r.ManifestDigest, "grant_digest": r.GrantDigest,
		"catalog_digest": r.CatalogDigest, "request_digest": r.RequestDigest,
		"previous_audit_hash": r.PreviousAuditHash, "audit_hash": r.AuditHash,
	} {
		if !isSHA256(value) {
			return fmt.Errorf("%w: %s is not lowercase SHA-256", ErrInvalidReceipt, name)
		}
	}
	if strings.TrimSpace(r.CatalogVersion) == "" || strings.TrimSpace(r.SQLFingerprint) == "" ||
		strings.TrimSpace(r.PolicyDecision) == "" || strings.TrimSpace(r.Status) == "" ||
		strings.TrimSpace(r.GatewayKeyID) == "" || r.CreatedAt.IsZero() || r.CompletedAt.IsZero() ||
		r.AuditSequence <= 0 || r.RowCount < 0 || r.DatabaseMS < 0 {
		return fmt.Errorf("%w: evidence field is missing or invalid", ErrInvalidReceipt)
	}
	if r.CompletedAt.Before(r.CreatedAt) {
		return fmt.Errorf("%w: completion precedes creation", ErrInvalidReceipt)
	}
	for _, vector := range []BudgetVectorV1{
		r.BudgetBefore.Limits, r.BudgetBefore.Used, r.BudgetBefore.Reserved,
		r.BudgetReserved, r.BudgetCharged,
		r.BudgetAfter.Limits, r.BudgetAfter.Used, r.BudgetAfter.Reserved,
	} {
		if vector.Queries < 0 || vector.Rows < 0 || vector.DBMS < 0 {
			return fmt.Errorf("%w: negative budget evidence", ErrInvalidReceipt)
		}
	}
	if r.ResultHash != "" && !isSHA256(r.ResultHash) {
		return fmt.Errorf("%w: result hash is invalid", ErrInvalidReceipt)
	}
	return nil
}

func (r QueryReceiptV1) Validate() error {
	if err := r.ValidateUnsigned(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Signature) == "" {
		return fmt.Errorf("%w: signature is required", ErrInvalidReceipt)
	}
	return nil
}

type Signer struct {
	keyID string
	key   ed25519.PrivateKey
}

func NewSigner(keyID string, key ed25519.PrivateKey) (*Signer, error) {
	if strings.TrimSpace(keyID) == "" || len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalidKey
	}
	return &Signer{keyID: keyID, key: append(ed25519.PrivateKey(nil), key...)}, nil
}

func NewSignerFromBase64(keyID, encoded string) (*Signer, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		keyBytes, err = base64.RawURLEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, ErrInvalidKey
	}
	if len(keyBytes) == ed25519.SeedSize {
		keyBytes = ed25519.NewKeyFromSeed(keyBytes)
	}
	return NewSigner(keyID, ed25519.PrivateKey(keyBytes))
}

// DemoSigner is only for deterministic local tests. Deployments should load a
// dedicated private key through NewSignerFromBase64.
func DemoSigner(secret []byte) *Signer {
	material := append([]byte("TASKGATE-DEMO-GATEWAY-ED25519-V1\x00"), secret...)
	seed := sha256.Sum256(material)
	signer, err := NewSigner("gateway-demo-ed25519-v1", ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		panic(err)
	}
	return signer
}

func (s *Signer) KeyID() string { return s.keyID }

func (s *Signer) PublicKey() ed25519.PublicKey {
	if s == nil || len(s.key) != ed25519.PrivateKeySize {
		return nil
	}
	return append(ed25519.PublicKey(nil), s.key.Public().(ed25519.PublicKey)...)
}

func (s *Signer) Sign(receipt QueryReceiptV1) (QueryReceiptV1, error) {
	if s == nil || len(s.key) != ed25519.PrivateKeySize || s.keyID == "" {
		return QueryReceiptV1{}, ErrInvalidKey
	}
	if receipt.GatewayKeyID != "" && receipt.GatewayKeyID != s.keyID {
		return QueryReceiptV1{}, ErrInvalidKey
	}
	receipt.GatewayKeyID = s.keyID
	receipt.Signature = ""
	if err := receipt.ValidateUnsigned(); err != nil {
		return QueryReceiptV1{}, err
	}
	payload, err := signingPayload(receipt)
	if err != nil {
		return QueryReceiptV1{}, err
	}
	receipt.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, payload))
	return receipt, nil
}

type Verifier struct{ keys map[string]ed25519.PublicKey }

func NewVerifier(keys map[string]ed25519.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidKey
	}
	copyKeys := make(map[string]ed25519.PublicKey, len(keys))
	for keyID, key := range keys {
		if strings.TrimSpace(keyID) == "" || len(key) != ed25519.PublicKeySize {
			return nil, ErrInvalidKey
		}
		copyKeys[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	return &Verifier{keys: copyKeys}, nil
}

func (v *Verifier) Verify(receipt QueryReceiptV1) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	key, ok := v.keys[receipt.GatewayKeyID]
	if !ok {
		return ErrUnknownKey
	}
	signature, err := base64.RawURLEncoding.DecodeString(receipt.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrInvalidSignature
	}
	payload, err := signingPayload(receipt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, payload, signature) {
		return ErrInvalidSignature
	}
	return nil
}

func signingPayload(receipt QueryReceiptV1) ([]byte, error) {
	unsigned := map[string]any{
		"version": receipt.Version, "receipt_id": receipt.ReceiptID,
		"task_id": receipt.TaskID, "query_id": receipt.QueryID, "request_id": receipt.RequestID,
		"manifest_digest": receipt.ManifestDigest, "grant_digest": receipt.GrantDigest,
		"catalog_digest": receipt.CatalogDigest, "catalog_version": receipt.CatalogVersion,
		"request_digest": receipt.RequestDigest, "sql_fingerprint": receipt.SQLFingerprint,
		"policy_decision": receipt.PolicyDecision, "budget_before": receipt.BudgetBefore,
		"budget_reserved": receipt.BudgetReserved, "budget_charged": receipt.BudgetCharged,
		"budget_after": receipt.BudgetAfter, "row_count": receipt.RowCount, "db_ms": receipt.DatabaseMS,
		"result_hash": receipt.ResultHash, "status": receipt.Status, "error_code": receipt.ErrorCode,
		"created_at": receipt.CreatedAt, "completed_at": receipt.CompletedAt,
		"audit_sequence": receipt.AuditSequence, "previous_audit_hash": receipt.PreviousAuditHash,
		"audit_hash": receipt.AuditHash, "gateway_key_id": receipt.GatewayKeyID,
	}
	canonical, err := approval.CanonicalJSON(unsigned)
	if err != nil {
		return nil, err
	}
	return append([]byte(signatureDomain), canonical...), nil
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && subtle.ConstantTimeEq(int32(len(decoded)), sha256.Size) == 1
}
