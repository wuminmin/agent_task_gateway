// Package queryreceipt defines the persistent, gateway-signed evidence for one
// authorized query. Approval receipts and query receipts intentionally use
// different keys and domain separators.
package queryreceipt

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/auditchain"
)

const (
	VersionV1         = "1"
	VersionV2         = "2"
	VersionV3         = "3"
	VersionV4         = "4"
	VersionV5         = "5"
	VersionV6         = "6"
	VersionV7         = "7"
	VersionV8         = "8"
	signatureDomainV1 = "TASKGATE-QUERY-RECEIPT-V1\x00"
	signatureDomainV2 = "TASKGATE-QUERY-RECEIPT-V2\x00"
	signatureDomainV3 = "TASKGATE-QUERY-RECEIPT-V3\x00"
	signatureDomainV4 = "TASKGATE-QUERY-RECEIPT-V4\x00"
	signatureDomainV5 = "TASKGATE-QUERY-RECEIPT-V5\x00"
	signatureDomainV6 = "TASKGATE-QUERY-RECEIPT-V6\x00"
	signatureDomainV7 = "TASKGATE-QUERY-RECEIPT-V7\x00"
	signatureDomainV8 = "TASKGATE-QUERY-RECEIPT-V8\x00"

	ArtifactIntentVersionV1 = "taskgate-artifact-intent-v1"
	ArtifactStatusPending   = "PENDING"

	StatusCompleted     = "COMPLETED"
	StatusReleased      = "RELEASED"
	StatusFailed        = "FAILED"
	StatusIndeterminate = "INDETERMINATE"
)

var (
	ErrInvalidReceipt   = errors.New("invalid query receipt")
	ErrInvalidKey       = errors.New("invalid query receipt key")
	ErrUnknownKey       = errors.New("unknown query receipt key ID")
	ErrKeyNotValid      = errors.New("query receipt key not valid at signing time")
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

type ExposureEvidenceV1 struct {
	RootTaskID                string `json:"root_task_id"`
	ProfileVersion            string `json:"profile_version"`
	ActualReleaseFacts        int64  `json:"actual_release_facts"`
	ActualInfluenceFacts      int64  `json:"actual_influence_facts"`
	ActualOutcomeFacts        int64  `json:"actual_outcome_facts,omitempty"`
	ChargedReleaseFacts       int64  `json:"charged_release_facts"`
	ChargedInfluenceFacts     int64  `json:"charged_influence_facts"`
	ChargedOutcomeFacts       int64  `json:"charged_outcome_facts,omitempty"`
	ObservationSHA256         string `json:"observation_sha256"`
	DictionarySetSHA256       string `json:"dictionary_set_sha256,omitempty"`
	ReleaseSetSHA256          string `json:"release_set_sha256,omitempty"`
	InfluenceSetSHA256        string `json:"influence_set_sha256,omitempty"`
	OutcomeSetSHA256          string `json:"outcome_set_sha256,omitempty"`
	RootEpoch                 int64  `json:"root_epoch,omitempty"`
	PredicateProfileVersion   string `json:"predicate_profile_version,omitempty"`
	PredicateContextSHA256    string `json:"predicate_context_sha256,omitempty"`
	PredicateSetSHA256        string `json:"predicate_set_sha256,omitempty"`
	ActualPredicateAtomCount  int64  `json:"actual_predicate_atom_count"`
	ChargedPredicateAtomCount int64  `json:"charged_predicate_atom_count"`
	CompositeOutcomeSHA256    string `json:"composite_outcome_sha256,omitempty"`
	ActualCompositeCount      int64  `json:"actual_composite_count,omitempty"`
	ChargedCompositeCount     int64  `json:"charged_composite_count,omitempty"`
}

// ArtifactIntentEvidenceV1 is the immutable, privacy-preserving description
// of the PENDING result object registered immediately after query completion.
// Physical object keys and display/query metadata are represented only by
// their SHA-256 digests.
type ArtifactIntentEvidenceV1 struct {
	Version                       string     `json:"version"`
	ResultID                      string     `json:"result_id"`
	Format                        string     `json:"format"`
	Encryption                    string     `json:"encryption"`
	KeyID                         string     `json:"key_id"`
	ParquetSHA256                 string     `json:"parquet_sha256"`
	ObjectSHA256                  string     `json:"object_sha256"`
	ParquetSize                   int64      `json:"parquet_size"`
	ObjectSize                    int64      `json:"object_size"`
	RowCount                      int64      `json:"row_count"`
	ColumnCount                   int64      `json:"column_count"`
	SchemaSHA256                  string     `json:"schema_sha256"`
	ResultMetadataSHA256          string     `json:"result_metadata_sha256"`
	ACLSHA256                     string     `json:"acl_sha256"`
	ObjectKeySHA256               string     `json:"object_key_sha256"`
	StagingKeySHA256              string     `json:"staging_key_sha256"`
	ExpiresAt                     *time.Time `json:"expires_at,omitempty"`
	Status                        string     `json:"status"`
	IntentSHA256                  string     `json:"intent_sha256"`
	RegistrationAuditSequence     int64      `json:"registration_audit_sequence"`
	RegistrationPreviousAuditHash string     `json:"registration_previous_audit_hash"`
	RegistrationAuditHash         string     `json:"registration_audit_hash"`
}

// QueryReceiptV1 contains no raw rows, SQL text, credentials, or physical
// relation names. Signature is unpadded base64url Ed25519.
type QueryReceiptV1 struct {
	Version           string                    `json:"version"`
	ReceiptID         string                    `json:"receipt_id"`
	TaskID            string                    `json:"task_id"`
	QueryID           string                    `json:"query_id"`
	RequestID         string                    `json:"request_id"`
	ManifestDigest    string                    `json:"manifest_digest"`
	GrantDigest       string                    `json:"grant_digest"`
	CatalogDigest     string                    `json:"catalog_digest"`
	CatalogVersion    string                    `json:"catalog_version"`
	DatasourceID      string                    `json:"datasource_id"`
	SchemaDigest      string                    `json:"schema_digest"`
	RequestDigest     string                    `json:"request_digest"`
	SQLFingerprint    string                    `json:"sql_fingerprint"`
	PolicyDecision    string                    `json:"policy_decision"`
	BudgetBefore      BudgetStateV1             `json:"budget_before"`
	BudgetReserved    BudgetVectorV1            `json:"budget_reserved"`
	BudgetCharged     BudgetVectorV1            `json:"budget_charged"`
	BudgetAfter       BudgetStateV1             `json:"budget_after"`
	RowCount          int64                     `json:"row_count"`
	DatabaseMS        int64                     `json:"db_ms"`
	ResultHash        string                    `json:"result_hash"`
	Status            string                    `json:"status"`
	ErrorCode         string                    `json:"error_code"`
	CreatedAt         time.Time                 `json:"created_at"`
	CompletedAt       time.Time                 `json:"completed_at"`
	AuditSequence     int64                     `json:"audit_sequence"`
	PreviousAuditHash string                    `json:"previous_audit_hash"`
	AuditHash         string                    `json:"audit_hash"`
	SignedAt          *time.Time                `json:"signed_at,omitempty"`
	GatewayKeyID      string                    `json:"gateway_key_id"`
	Signature         string                    `json:"signature"`
	Exposure          *ExposureEvidenceV1       `json:"exposure,omitempty"`
	ArtifactIntent    *ArtifactIntentEvidenceV1 `json:"artifact_intent,omitempty"`
}

// BuildArtifactIntent validates the supplied immutable evidence and seals it
// with a digest over every field except intent_sha256 itself.
func BuildArtifactIntent(intent ArtifactIntentEvidenceV1) (ArtifactIntentEvidenceV1, error) {
	intent.IntentSHA256 = ""
	if err := validateArtifactIntentEvidence(intent, false); err != nil {
		return ArtifactIntentEvidenceV1{}, err
	}
	digest, err := artifactIntentSHA256(intent)
	if err != nil {
		return ArtifactIntentEvidenceV1{}, err
	}
	intent.IntentSHA256 = digest
	return intent, nil
}

func (r QueryReceiptV1) validateArtifactIntent() error {
	intent := *r.ArtifactIntent
	if err := validateArtifactIntentEvidence(intent, true); err != nil {
		return err
	}
	if r.ResultHash != intent.ParquetSHA256 || r.RowCount != intent.RowCount {
		return fmt.Errorf("%w: artifact intent does not match query result evidence", ErrInvalidReceipt)
	}
	if intent.RegistrationAuditSequence != r.AuditSequence+1 ||
		intent.RegistrationPreviousAuditHash != r.AuditHash {
		return fmt.Errorf("%w: artifact registration does not follow terminal audit evidence", ErrInvalidReceipt)
	}
	expected, err := artifactIntentSHA256(intent)
	if err != nil {
		return fmt.Errorf("%w: cannot canonicalize artifact intent: %v", ErrInvalidReceipt, err)
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(intent.IntentSHA256)) != 1 {
		return fmt.Errorf("%w: artifact intent digest mismatch", ErrInvalidReceipt)
	}
	return nil
}

func validateArtifactIntentEvidence(intent ArtifactIntentEvidenceV1, requireIntentDigest bool) error {
	if intent.Version != ArtifactIntentVersionV1 || strings.TrimSpace(intent.ResultID) == "" ||
		intent.ResultID != strings.TrimSpace(intent.ResultID) || intent.Format != "parquet" ||
		intent.Encryption != "chunked-aes-gcm-v1" || strings.TrimSpace(intent.KeyID) == "" ||
		intent.KeyID != strings.TrimSpace(intent.KeyID) || intent.ParquetSize < 0 || intent.ObjectSize <= 0 ||
		intent.RowCount < 0 || intent.ColumnCount <= 0 || intent.Status != ArtifactStatusPending ||
		intent.RegistrationAuditSequence <= 0 {
		return fmt.Errorf("%w: artifact intent evidence is missing or invalid", ErrInvalidReceipt)
	}
	for name, value := range map[string]string{
		"parquet_sha256": intent.ParquetSHA256, "object_sha256": intent.ObjectSHA256,
		"schema_sha256": intent.SchemaSHA256, "result_metadata_sha256": intent.ResultMetadataSHA256,
		"acl_sha256":        intent.ACLSHA256,
		"object_key_sha256": intent.ObjectKeySHA256, "staging_key_sha256": intent.StagingKeySHA256,
		"registration_previous_audit_hash": intent.RegistrationPreviousAuditHash,
		"registration_audit_hash":          intent.RegistrationAuditHash,
	} {
		if !isSHA256(value) {
			return fmt.Errorf("%w: artifact %s is not lowercase SHA-256", ErrInvalidReceipt, name)
		}
	}
	if requireIntentDigest && !isSHA256(intent.IntentSHA256) {
		return fmt.Errorf("%w: artifact intent_sha256 is not lowercase SHA-256", ErrInvalidReceipt)
	}
	if intent.ExpiresAt != nil && intent.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: artifact expiry is invalid", ErrInvalidReceipt)
	}
	return nil
}

func artifactIntentSHA256(intent ArtifactIntentEvidenceV1) (string, error) {
	unsigned := map[string]any{
		"version": intent.Version, "result_id": intent.ResultID, "format": intent.Format,
		"encryption": intent.Encryption, "key_id": intent.KeyID,
		"parquet_sha256": intent.ParquetSHA256, "object_sha256": intent.ObjectSHA256,
		"parquet_size": intent.ParquetSize, "object_size": intent.ObjectSize,
		"row_count": intent.RowCount, "column_count": intent.ColumnCount,
		"schema_sha256": intent.SchemaSHA256, "result_metadata_sha256": intent.ResultMetadataSHA256,
		"acl_sha256":        intent.ACLSHA256,
		"object_key_sha256": intent.ObjectKeySHA256, "staging_key_sha256": intent.StagingKeySHA256,
		"status": intent.Status, "registration_audit_sequence": intent.RegistrationAuditSequence,
		"registration_previous_audit_hash": intent.RegistrationPreviousAuditHash,
		"registration_audit_hash":          intent.RegistrationAuditHash,
	}
	if intent.ExpiresAt != nil {
		unsigned["expires_at"] = intent.ExpiresAt
	}
	canonical, err := approval.CanonicalJSON(unsigned)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (r QueryReceiptV1) ValidateUnsigned() error {
	if r.Version != VersionV1 && r.Version != VersionV2 && r.Version != VersionV3 && r.Version != VersionV4 && r.Version != VersionV5 && r.Version != VersionV6 && r.Version != VersionV7 && r.Version != VersionV8 {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidReceipt, r.Version)
	}
	if strings.TrimSpace(r.ReceiptID) == "" || strings.TrimSpace(r.TaskID) == "" ||
		strings.TrimSpace(r.QueryID) == "" || strings.TrimSpace(r.RequestID) == "" || r.ReceiptID != r.QueryID {
		return fmt.Errorf("%w: identity field is missing or inconsistent", ErrInvalidReceipt)
	}
	digests := map[string]string{
		"manifest_digest": r.ManifestDigest, "grant_digest": r.GrantDigest,
		"catalog_digest":      r.CatalogDigest,
		"request_digest":      r.RequestDigest,
		"previous_audit_hash": r.PreviousAuditHash, "audit_hash": r.AuditHash,
	}
	if r.Version == VersionV2 || r.Version == VersionV3 || r.Version == VersionV4 || r.Version == VersionV5 || r.Version == VersionV6 || r.Version == VersionV7 || r.Version == VersionV8 {
		digests["schema_digest"] = r.SchemaDigest
	}
	for name, value := range digests {
		if !isSHA256(value) {
			return fmt.Errorf("%w: %s is not lowercase SHA-256", ErrInvalidReceipt, name)
		}
	}
	if (r.Version == VersionV2 || r.Version == VersionV3 || r.Version == VersionV4 || r.Version == VersionV5 || r.Version == VersionV6 || r.Version == VersionV7 || r.Version == VersionV8) && strings.TrimSpace(r.DatasourceID) == "" {
		return fmt.Errorf("%w: datasource_id is required", ErrInvalidReceipt)
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
	if r.Version == VersionV3 || r.Version == VersionV4 || r.Version == VersionV5 || r.Version == VersionV6 || r.Version == VersionV7 || r.Version == VersionV8 {
		if r.SignedAt == nil || r.SignedAt.IsZero() {
			return fmt.Errorf("%w: signed_at is required", ErrInvalidReceipt)
		}
		if r.SignedAt.UTC().Before(r.CompletedAt.UTC()) {
			return fmt.Errorf("%w: signed_at precedes terminal evidence", ErrInvalidReceipt)
		}
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
	if r.Exposure != nil {
		exposure := r.Exposure
		if (r.Version != VersionV4 && r.Version != VersionV5 && r.Version != VersionV6 && r.Version != VersionV7 && r.Version != VersionV8) || strings.TrimSpace(exposure.RootTaskID) == "" ||
			strings.TrimSpace(exposure.ProfileVersion) == "" || !isSHA256(exposure.ObservationSHA256) ||
			exposure.ActualReleaseFacts < 0 || exposure.ActualInfluenceFacts < 0 || exposure.ActualOutcomeFacts < 0 ||
			exposure.ChargedReleaseFacts < 0 || exposure.ChargedInfluenceFacts < 0 || exposure.ChargedOutcomeFacts < 0 ||
			exposure.ChargedReleaseFacts > exposure.ActualReleaseFacts ||
			exposure.ChargedInfluenceFacts > exposure.ActualInfluenceFacts || exposure.ChargedOutcomeFacts > exposure.ActualOutcomeFacts {
			return fmt.Errorf("%w: exposure evidence is invalid", ErrInvalidReceipt)
		}
		if (r.Version == VersionV4 && (exposure.ActualOutcomeFacts != 0 || exposure.ChargedOutcomeFacts != 0)) ||
			(r.Version == VersionV5 && (exposure.ProfileVersion != "taskgate-exposure-v3" || exposure.ActualOutcomeFacts != 1)) ||
			(r.Version == VersionV6 && (exposure.ProfileVersion != "taskgate-exposure-v4" || exposure.ActualOutcomeFacts != 1 ||
				exposure.RootEpoch <= 0 || !isSHA256(exposure.DictionarySetSHA256) || !isSHA256(exposure.ReleaseSetSHA256) ||
				!isSHA256(exposure.InfluenceSetSHA256) || !isSHA256(exposure.OutcomeSetSHA256)) ||
				((r.Version == VersionV7 || r.Version == VersionV8) && (exposure.ProfileVersion != "taskgate-exposure-v5" ||
					exposure.PredicateProfileVersion != "taskgate-predicate-footprint-v1" ||
					exposure.ActualCompositeCount != 1 || exposure.ChargedCompositeCount < 0 || exposure.ChargedCompositeCount > 1 ||
					exposure.ActualPredicateAtomCount < 0 || exposure.ChargedPredicateAtomCount < 0 ||
					exposure.ChargedPredicateAtomCount > exposure.ActualPredicateAtomCount ||
					exposure.ActualOutcomeFacts != exposure.ActualPredicateAtomCount+1 ||
					exposure.ChargedOutcomeFacts != exposure.ChargedPredicateAtomCount+exposure.ChargedCompositeCount ||
					exposure.RootEpoch <= 0 || !isSHA256(exposure.DictionarySetSHA256) || !isSHA256(exposure.ReleaseSetSHA256) ||
					!isSHA256(exposure.InfluenceSetSHA256) || !isSHA256(exposure.OutcomeSetSHA256) ||
					!isSHA256(exposure.PredicateContextSHA256) || !isSHA256(exposure.PredicateSetSHA256) ||
					!isSHA256(exposure.CompositeOutcomeSHA256)))) {
			return fmt.Errorf("%w: receipt version and outcome evidence disagree", ErrInvalidReceipt)
		}
	} else if r.Version == VersionV4 || r.Version == VersionV5 || r.Version == VersionV6 || r.Version == VersionV7 || r.Version == VersionV8 {
		return fmt.Errorf("%w: V4/V5/V6/V7/V8 requires exposure evidence", ErrInvalidReceipt)
	}
	if r.Version != VersionV8 {
		if r.ArtifactIntent != nil {
			return fmt.Errorf("%w: V1-V7 must not carry artifact intent", ErrInvalidReceipt)
		}
	} else {
		if r.Status != StatusCompleted || r.ArtifactIntent == nil {
			return fmt.Errorf("%w: V8 requires completed artifact intent evidence", ErrInvalidReceipt)
		}
		if err := r.validateArtifactIntent(); err != nil {
			return err
		}
	}
	if err := r.validateBudgetSemantics(); err != nil {
		return err
	}
	if err := r.validateStatusSemantics(); err != nil {
		return err
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
	if receipt.SignedAt != nil {
		signedAt := receipt.SignedAt.UTC()
		receipt.SignedAt = &signedAt
	}
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

type VerifyingKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	ValidFrom time.Time
	RetiredAt time.Time
}

type Keyring struct {
	active   *Signer
	verifier *Verifier
}

func NewKeyring(active *Signer, historical []VerifyingKey) (*Keyring, error) {
	if active == nil || active.KeyID() == "" || active.PublicKey() == nil {
		return nil, ErrInvalidKey
	}
	keys := append([]VerifyingKey(nil), historical...)
	activePublicKey := active.PublicKey()
	foundActive := false
	for _, key := range keys {
		if key.KeyID == active.KeyID() {
			foundActive = true
			if len(key.PublicKey) == ed25519.PublicKeySize && !bytes.Equal(key.PublicKey, activePublicKey) {
				return nil, ErrInvalidKey
			}
			break
		}
	}
	if !foundActive {
		keys = append(keys, VerifyingKey{KeyID: active.KeyID(), PublicKey: activePublicKey})
	}
	verifier, err := NewVerifierWithKeyring(keys)
	if err != nil {
		return nil, err
	}
	return &Keyring{active: active, verifier: verifier}, nil
}

func (k *Keyring) KeyID() string {
	if k == nil || k.active == nil {
		return ""
	}
	return k.active.KeyID()
}

func (k *Keyring) PublicKey() ed25519.PublicKey {
	if k == nil || k.active == nil {
		return nil
	}
	return k.active.PublicKey()
}

func (k *Keyring) Sign(receipt QueryReceiptV1) (QueryReceiptV1, error) {
	if k == nil || k.active == nil {
		return QueryReceiptV1{}, ErrInvalidKey
	}
	return k.active.Sign(receipt)
}

func (k *Keyring) Verify(receipt QueryReceiptV1) error {
	if k == nil || k.verifier == nil {
		return ErrInvalidKey
	}
	return k.verifier.Verify(receipt)
}

func (k *Keyring) Verifier() *Verifier {
	if k == nil {
		return nil
	}
	return k.verifier
}

func VerifyAuditInclusion(receipt QueryReceiptV1, proof auditchain.InclusionProof) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	terminal := proof.TerminalEvent
	if terminal.QueryID != receipt.QueryID ||
		terminal.Sequence != receipt.AuditSequence ||
		terminal.PreviousHash != receipt.PreviousAuditHash ||
		terminal.CurrentHash != receipt.AuditHash {
		return fmt.Errorf("%w: audit terminal event does not match receipt", ErrInvalidReceipt)
	}
	if !statusMatchesTerminalAuditEvent(receipt.Status, terminal.EventType) {
		return fmt.Errorf("%w: terminal audit event does not match receipt status", ErrInvalidReceipt)
	}
	if err := auditchain.VerifyInclusion(proof); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	return nil
}

// VerifyArtifactIntentInclusion proves both the terminal completion event and
// the immediately following artifact registration event against audit
// checkpoints, then checks the registration payload against the signed intent.
func VerifyArtifactIntentInclusion(receipt QueryReceiptV1, terminalProof, registrationProof auditchain.InclusionProof) error {
	if receipt.Version != VersionV8 || receipt.ArtifactIntent == nil {
		return fmt.Errorf("%w: artifact inclusion requires a V8 receipt", ErrInvalidReceipt)
	}
	if err := VerifyAuditInclusion(receipt, terminalProof); err != nil {
		return err
	}
	intent := receipt.ArtifactIntent
	registration := registrationProof.TerminalEvent
	if registration.EventType != "QUERY_RESULT_OBJECT_REGISTERED" ||
		registration.TaskID != receipt.TaskID || registration.QueryID != receipt.QueryID ||
		registration.Sequence != intent.RegistrationAuditSequence ||
		registration.PreviousHash != intent.RegistrationPreviousAuditHash ||
		registration.CurrentHash != intent.RegistrationAuditHash {
		return fmt.Errorf("%w: artifact registration event does not match receipt", ErrInvalidReceipt)
	}
	if err := auditchain.VerifyInclusion(registrationProof); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	expected, err := json.Marshal(artifactRegistrationPayload(*intent))
	if err != nil {
		return fmt.Errorf("%w: cannot encode artifact registration evidence: %v", ErrInvalidReceipt, err)
	}
	expected, err = auditchain.NormalizePayload(expected)
	if err != nil {
		return fmt.Errorf("%w: cannot normalize artifact registration evidence: %v", ErrInvalidReceipt, err)
	}
	actual, err := auditchain.NormalizePayload(registration.Payload)
	if err != nil || !bytes.Equal(actual, expected) {
		return fmt.Errorf("%w: artifact registration payload does not match intent", ErrInvalidReceipt)
	}
	return nil
}

// VerifyArtifactAvailabilityInclusion verifies the dedicated proof for the
// QUERY_RESULT_CONSUMED event that crosses the logical AVAILABLE boundary.
// The event is deliberately not part of the immutable V8 receipt because it
// occurs after settlement; its payload must nevertheless match that receipt's
// signed artifact intent exactly.
func VerifyArtifactAvailabilityInclusion(receipt QueryReceiptV1, availabilityProof auditchain.InclusionProof) error {
	if receipt.Version != VersionV8 || receipt.ArtifactIntent == nil {
		return fmt.Errorf("%w: artifact availability inclusion requires a V8 receipt", ErrInvalidReceipt)
	}
	event := availabilityProof.TerminalEvent
	intent := receipt.ArtifactIntent
	if event.EventType != "QUERY_RESULT_CONSUMED" || event.TaskID != receipt.TaskID ||
		event.QueryID != receipt.QueryID {
		return fmt.Errorf("%w: artifact availability event does not match receipt", ErrInvalidReceipt)
	}
	if err := auditchain.VerifyInclusion(availabilityProof); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReceipt, err)
	}
	var payload struct {
		ResultID     string `json:"result_id"`
		ResultSHA256 string `json:"result_sha256"`
		ObjectSHA256 string `json:"object_sha256"`
		Format       string `json:"format"`
		Status       string `json:"status"`
		ConsumedAt   string `json:"consumed_at"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil ||
		payload.ResultID != intent.ResultID || payload.ResultSHA256 != intent.ParquetSHA256 ||
		payload.ObjectSHA256 != intent.ObjectSHA256 || payload.Format != intent.Format ||
		payload.Status != "AVAILABLE" || strings.TrimSpace(payload.ConsumedAt) == "" {
		return fmt.Errorf("%w: artifact availability payload does not match intent", ErrInvalidReceipt)
	}
	return nil
}

func artifactRegistrationPayload(intent ArtifactIntentEvidenceV1) map[string]any {
	// The event payload is the artifact projection of the intent. The audit
	// sequence/hash and intent_sha256 are intentionally excluded because they
	// are only known after this event has itself been hashed.
	payload := map[string]any{
		"version": intent.Version, "result_id": intent.ResultID, "format": intent.Format,
		"encryption": intent.Encryption, "key_id": intent.KeyID,
		"parquet_sha256": intent.ParquetSHA256, "object_sha256": intent.ObjectSHA256,
		"parquet_size": intent.ParquetSize, "object_size": intent.ObjectSize,
		"row_count": intent.RowCount, "column_count": intent.ColumnCount,
		"schema_sha256": intent.SchemaSHA256, "result_metadata_sha256": intent.ResultMetadataSHA256,
		"acl_sha256":        intent.ACLSHA256,
		"object_key_sha256": intent.ObjectKeySHA256, "staging_key_sha256": intent.StagingKeySHA256,
		"status": intent.Status,
	}
	if intent.ExpiresAt != nil {
		payload["expires_at"] = intent.ExpiresAt
	}
	return payload
}

type Verifier struct{ keys map[string]VerifyingKey }

func NewVerifier(keys map[string]ed25519.PublicKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidKey
	}
	keyring := make([]VerifyingKey, 0, len(keys))
	for keyID, key := range keys {
		keyring = append(keyring, VerifyingKey{KeyID: keyID, PublicKey: key})
	}
	return NewVerifierWithKeyring(keyring)
}

func NewVerifierWithKeyring(keys []VerifyingKey) (*Verifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidKey
	}
	copyKeys := make(map[string]VerifyingKey, len(keys))
	for _, key := range keys {
		normalized, err := normalizeVerifyingKey(key)
		if err != nil {
			return nil, err
		}
		if _, duplicate := copyKeys[normalized.KeyID]; duplicate {
			return nil, ErrInvalidKey
		}
		copyKeys[normalized.KeyID] = normalized
	}
	return &Verifier{keys: copyKeys}, nil
}

func (v *Verifier) Verify(receipt QueryReceiptV1) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	if v == nil {
		return ErrInvalidKey
	}
	key, ok := v.keys[receipt.GatewayKeyID]
	if !ok {
		return ErrUnknownKey
	}
	if err := key.validFor(receipt); err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(receipt.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrInvalidSignature
	}
	payload, err := signingPayload(receipt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key.PublicKey, payload, signature) {
		return ErrInvalidSignature
	}
	return nil
}

func normalizeVerifyingKey(key VerifyingKey) (VerifyingKey, error) {
	key.KeyID = strings.TrimSpace(key.KeyID)
	if key.KeyID == "" || len(key.PublicKey) != ed25519.PublicKeySize {
		return VerifyingKey{}, ErrInvalidKey
	}
	if !key.ValidFrom.IsZero() {
		key.ValidFrom = key.ValidFrom.UTC()
	}
	if !key.RetiredAt.IsZero() {
		key.RetiredAt = key.RetiredAt.UTC()
		if !key.ValidFrom.IsZero() && key.RetiredAt.Before(key.ValidFrom) {
			return VerifyingKey{}, ErrInvalidKey
		}
	}
	key.PublicKey = append(ed25519.PublicKey(nil), key.PublicKey...)
	return key, nil
}

func (key VerifyingKey) validFor(receipt QueryReceiptV1) error {
	if receipt.SignedAt == nil {
		if !key.ValidFrom.IsZero() || !key.RetiredAt.IsZero() {
			return fmt.Errorf("%w: receipt lacks signed_at for keyring policy", ErrInvalidReceipt)
		}
		return nil
	}
	signedAt := receipt.SignedAt.UTC()
	if !key.ValidFrom.IsZero() && signedAt.Before(key.ValidFrom) {
		return ErrKeyNotValid
	}
	if !key.RetiredAt.IsZero() && signedAt.After(key.RetiredAt) {
		return ErrKeyNotValid
	}
	return nil
}

func (r QueryReceiptV1) validateBudgetSemantics() error {
	if !sameVector(r.BudgetBefore.Limits, r.BudgetAfter.Limits) {
		return fmt.Errorf("%w: budget limits changed across receipt", ErrInvalidReceipt)
	}
	if !validBudgetState(r.BudgetBefore) || !validBudgetState(r.BudgetAfter) {
		return fmt.Errorf("%w: budget state exceeds limits", ErrInvalidReceipt)
	}
	if r.BudgetReserved.Queries != 1 || r.BudgetReserved.Rows <= 0 || r.BudgetReserved.DBMS <= 0 {
		return fmt.Errorf("%w: invalid reservation vector", ErrInvalidReceipt)
	}
	if !vectorLTE(r.BudgetCharged, r.BudgetReserved) {
		return fmt.Errorf("%w: charge exceeds reservation", ErrInvalidReceipt)
	}
	if !sameVector(r.BudgetAfter.Used, addVector(r.BudgetBefore.Used, r.BudgetCharged)) {
		return fmt.Errorf("%w: budget usage transition is invalid", ErrInvalidReceipt)
	}
	if !sameVector(r.BudgetAfter.Reserved, r.BudgetBefore.Reserved) {
		return fmt.Errorf("%w: reservation was not released", ErrInvalidReceipt)
	}
	if r.RowCount != r.BudgetCharged.Rows || r.DatabaseMS != r.BudgetCharged.DBMS {
		return fmt.Errorf("%w: result metrics do not match charged budget", ErrInvalidReceipt)
	}
	return nil
}

func (r QueryReceiptV1) validateStatusSemantics() error {
	switch r.Status {
	case StatusCompleted:
		if r.ResultHash == "" || r.ErrorCode != "" || r.BudgetCharged.Queries != 1 ||
			r.RowCount < 0 || r.DatabaseMS <= 0 {
			return fmt.Errorf("%w: invalid completed receipt evidence", ErrInvalidReceipt)
		}
	case StatusReleased:
		if r.ResultHash != "" || !sameVector(r.BudgetCharged, BudgetVectorV1{}) ||
			r.RowCount != 0 || r.DatabaseMS != 0 {
			return fmt.Errorf("%w: invalid released receipt evidence", ErrInvalidReceipt)
		}
	case StatusFailed:
		if r.ResultHash != "" || strings.TrimSpace(r.ErrorCode) == "" ||
			r.BudgetCharged.Queries != 1 || r.RowCount < 0 || r.DatabaseMS <= 0 {
			return fmt.Errorf("%w: failed receipt requires an error code", ErrInvalidReceipt)
		}
	case StatusIndeterminate:
		if r.ResultHash != "" || strings.TrimSpace(r.ErrorCode) == "" ||
			!sameVector(r.BudgetCharged, r.BudgetReserved) {
			return fmt.Errorf("%w: invalid indeterminate receipt evidence", ErrInvalidReceipt)
		}
	default:
		return fmt.Errorf("%w: unsupported terminal status %q", ErrInvalidReceipt, r.Status)
	}
	return nil
}

func statusMatchesTerminalAuditEvent(status, eventType string) bool {
	switch status {
	case StatusCompleted:
		return eventType == "QUERY_COMPLETED"
	case StatusReleased:
		return eventType == "QUERY_BUDGET_RELEASED"
	case StatusFailed:
		return eventType == "QUERY_FAILED"
	case StatusIndeterminate:
		return eventType == "QUERY_INDETERMINATE" || eventType == "QUERY_INTERRUPTED"
	default:
		return false
	}
}

func validBudgetState(state BudgetStateV1) bool {
	if state.Limits.Queries <= 0 || state.Limits.Rows <= 0 || state.Limits.DBMS <= 0 {
		return false
	}
	return state.Used.Queries+state.Reserved.Queries <= state.Limits.Queries &&
		state.Used.Rows+state.Reserved.Rows <= state.Limits.Rows &&
		state.Used.DBMS+state.Reserved.DBMS <= state.Limits.DBMS
}

func sameVector(left, right BudgetVectorV1) bool {
	return left.Queries == right.Queries && left.Rows == right.Rows && left.DBMS == right.DBMS
}

func vectorLTE(left, right BudgetVectorV1) bool {
	return left.Queries <= right.Queries && left.Rows <= right.Rows && left.DBMS <= right.DBMS
}

func addVector(left, right BudgetVectorV1) BudgetVectorV1 {
	return BudgetVectorV1{Queries: left.Queries + right.Queries, Rows: left.Rows + right.Rows, DBMS: left.DBMS + right.DBMS}
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
	domain := signatureDomainV1
	if receipt.Version == VersionV2 || receipt.Version == VersionV3 || receipt.Version == VersionV4 || receipt.Version == VersionV5 || receipt.Version == VersionV6 || receipt.Version == VersionV7 || receipt.Version == VersionV8 {
		domain = signatureDomainV2
		unsigned["datasource_id"] = receipt.DatasourceID
		unsigned["schema_digest"] = receipt.SchemaDigest
	}
	if receipt.Version == VersionV3 {
		domain = signatureDomainV3
		unsigned["signed_at"] = receipt.SignedAt
	}
	if receipt.Version == VersionV4 {
		domain = signatureDomainV4
		unsigned["signed_at"] = receipt.SignedAt
		unsigned["exposure"] = receipt.Exposure
	}
	if receipt.Version == VersionV5 {
		domain = signatureDomainV5
		unsigned["signed_at"] = receipt.SignedAt
		unsigned["exposure"] = receipt.Exposure
	}
	if receipt.Version == VersionV6 {
		domain = signatureDomainV6
		unsigned["signed_at"] = receipt.SignedAt
		unsigned["exposure"] = receipt.Exposure
	}
	if receipt.Version == VersionV7 {
		domain = signatureDomainV7
		unsigned["signed_at"] = receipt.SignedAt
		unsigned["exposure"] = receipt.Exposure
	}
	if receipt.Version == VersionV8 {
		domain = signatureDomainV8
		unsigned["signed_at"] = receipt.SignedAt
		unsigned["exposure"] = receipt.Exposure
		unsigned["artifact_intent"] = receipt.ArtifactIntent
	}
	canonical, err := approval.CanonicalJSON(unsigned)
	if err != nil {
		return nil, err
	}
	return append([]byte(domain), canonical...), nil
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && subtle.ConstantTimeEq(int32(len(decoded)), sha256.Size) == 1
}
