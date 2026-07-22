package auditchain

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	AnchorVersionV1  = "taskgate-audit-checkpoint-anchor/v1"
	anchorDomainV1   = "TASKGATE-AUDIT-CHECKPOINT-ANCHOR-V1\x00"
	anchorIDPrefixV1 = "audit-checkpoint:"
)

var (
	ErrInvalidAnchor          = errors.New("invalid audit checkpoint anchor")
	ErrInvalidAnchorKey       = errors.New("invalid audit checkpoint anchor key")
	ErrUnknownAnchorKey       = errors.New("unknown audit checkpoint anchor key ID")
	ErrAnchorKeyNotValid      = errors.New("audit checkpoint anchor key not valid at signing time")
	ErrInvalidAnchorSignature = errors.New("invalid audit checkpoint anchor signature")
)

type SignedCheckpointAnchorV1 struct {
	Version      string    `json:"version"`
	AnchorID     string    `json:"anchor_id"`
	Sequence     int64     `json:"sequence"`
	Hash         string    `json:"hash"`
	SignedAt     time.Time `json:"signed_at"`
	GatewayKeyID string    `json:"gateway_key_id"`
	Signature    string    `json:"signature"`
}

type AnchorSigner struct {
	keyID string
	key   ed25519.PrivateKey
}

type AnchorVerifyingKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
	ValidFrom time.Time
	RetiredAt time.Time
}

type AnchorVerifier struct {
	keys map[string]AnchorVerifyingKey
}

func NewAnchorSigner(keyID string, key ed25519.PrivateKey) (*AnchorSigner, error) {
	if strings.TrimSpace(keyID) == "" || len(key) != ed25519.PrivateKeySize {
		return nil, ErrInvalidAnchorKey
	}
	return &AnchorSigner{keyID: strings.TrimSpace(keyID), key: append(ed25519.PrivateKey(nil), key...)}, nil
}

func NewAnchorSignerFromBase64(keyID, encoded string) (*AnchorSigner, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		keyBytes, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil {
		return nil, ErrInvalidAnchorKey
	}
	if len(keyBytes) == ed25519.SeedSize {
		keyBytes = ed25519.NewKeyFromSeed(keyBytes)
	}
	return NewAnchorSigner(keyID, ed25519.PrivateKey(keyBytes))
}

func (s *AnchorSigner) KeyID() string {
	if s == nil {
		return ""
	}
	return s.keyID
}

func (s *AnchorSigner) PublicKey() ed25519.PublicKey {
	if s == nil || len(s.key) != ed25519.PrivateKeySize {
		return nil
	}
	return append(ed25519.PublicKey(nil), s.key.Public().(ed25519.PublicKey)...)
}

func (s *AnchorSigner) SignCheckpoint(checkpoint Checkpoint, signedAt time.Time) (SignedCheckpointAnchorV1, error) {
	if s == nil || len(s.key) != ed25519.PrivateKeySize || s.keyID == "" {
		return SignedCheckpointAnchorV1{}, ErrInvalidAnchorKey
	}
	anchor := SignedCheckpointAnchorV1{
		Version:      AnchorVersionV1,
		AnchorID:     CheckpointAnchorID(checkpoint),
		Sequence:     checkpoint.Sequence,
		Hash:         checkpoint.Hash,
		SignedAt:     signedAt.UTC(),
		GatewayKeyID: s.keyID,
	}
	if err := anchor.ValidateUnsigned(); err != nil {
		return SignedCheckpointAnchorV1{}, err
	}
	payload, err := anchorSigningPayload(anchor)
	if err != nil {
		return SignedCheckpointAnchorV1{}, err
	}
	anchor.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.key, payload))
	return anchor, nil
}

func NewAnchorVerifier(keys []AnchorVerifyingKey) (*AnchorVerifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidAnchorKey
	}
	keyring := make(map[string]AnchorVerifyingKey, len(keys))
	for _, key := range keys {
		normalized, err := normalizeAnchorVerifyingKey(key)
		if err != nil {
			return nil, err
		}
		if _, exists := keyring[normalized.KeyID]; exists {
			return nil, ErrInvalidAnchorKey
		}
		keyring[normalized.KeyID] = normalized
	}
	return &AnchorVerifier{keys: keyring}, nil
}

func (v *AnchorVerifier) Verify(anchor SignedCheckpointAnchorV1) error {
	if err := anchor.Validate(); err != nil {
		return err
	}
	if v == nil {
		return ErrInvalidAnchorKey
	}
	key, ok := v.keys[anchor.GatewayKeyID]
	if !ok {
		return ErrUnknownAnchorKey
	}
	if err := key.validFor(anchor); err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(anchor.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrInvalidAnchorSignature
	}
	payload, err := anchorSigningPayload(anchor)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key.PublicKey, payload, signature) {
		return ErrInvalidAnchorSignature
	}
	return nil
}

func (anchor SignedCheckpointAnchorV1) ValidateUnsigned() error {
	if anchor.Version != AnchorVersionV1 ||
		strings.TrimSpace(anchor.AnchorID) == "" ||
		strings.TrimSpace(anchor.GatewayKeyID) == "" ||
		anchor.Sequence < 0 ||
		anchor.SignedAt.IsZero() {
		return ErrInvalidAnchor
	}
	if err := validateHash(anchor.Hash, "checkpoint hash"); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidAnchor, err)
	}
	checkpoint := Checkpoint{Sequence: anchor.Sequence, Hash: anchor.Hash}
	if anchor.AnchorID != CheckpointAnchorID(checkpoint) {
		return fmt.Errorf("%w: anchor ID does not match checkpoint", ErrInvalidAnchor)
	}
	if anchor.Sequence == 0 && anchor.Hash != GenesisHash {
		return fmt.Errorf("%w: genesis checkpoint hash mismatch", ErrInvalidAnchor)
	}
	return nil
}

func (anchor SignedCheckpointAnchorV1) Validate() error {
	if err := anchor.ValidateUnsigned(); err != nil {
		return err
	}
	if strings.TrimSpace(anchor.Signature) == "" {
		return fmt.Errorf("%w: signature is required", ErrInvalidAnchor)
	}
	return nil
}

func CheckpointAnchorID(checkpoint Checkpoint) string {
	return fmt.Sprintf("%s%d:%s", anchorIDPrefixV1, checkpoint.Sequence, checkpoint.Hash)
}

type anchorSigningMaterial struct {
	Version      string `json:"version"`
	AnchorID     string `json:"anchor_id"`
	Sequence     int64  `json:"sequence"`
	Hash         string `json:"hash"`
	SignedAt     string `json:"signed_at"`
	GatewayKeyID string `json:"gateway_key_id"`
}

func anchorSigningPayload(anchor SignedCheckpointAnchorV1) ([]byte, error) {
	material, err := json.Marshal(anchorSigningMaterial{
		Version:      anchor.Version,
		AnchorID:     anchor.AnchorID,
		Sequence:     anchor.Sequence,
		Hash:         anchor.Hash,
		SignedAt:     FormatTime(anchor.SignedAt),
		GatewayKeyID: anchor.GatewayKeyID,
	})
	if err != nil {
		return nil, err
	}
	return append([]byte(anchorDomainV1), material...), nil
}

func normalizeAnchorVerifyingKey(key AnchorVerifyingKey) (AnchorVerifyingKey, error) {
	key.KeyID = strings.TrimSpace(key.KeyID)
	if key.KeyID == "" || len(key.PublicKey) != ed25519.PublicKeySize {
		return AnchorVerifyingKey{}, ErrInvalidAnchorKey
	}
	if !key.ValidFrom.IsZero() {
		key.ValidFrom = key.ValidFrom.UTC()
	}
	if !key.RetiredAt.IsZero() {
		key.RetiredAt = key.RetiredAt.UTC()
		if !key.ValidFrom.IsZero() && key.RetiredAt.Before(key.ValidFrom) {
			return AnchorVerifyingKey{}, ErrInvalidAnchorKey
		}
	}
	key.PublicKey = append(ed25519.PublicKey(nil), key.PublicKey...)
	return key, nil
}

func (key AnchorVerifyingKey) validFor(anchor SignedCheckpointAnchorV1) error {
	signedAt := anchor.SignedAt.UTC()
	if !key.ValidFrom.IsZero() && signedAt.Before(key.ValidFrom) {
		return ErrAnchorKeyNotValid
	}
	if !key.RetiredAt.IsZero() && signedAt.After(key.RetiredAt) {
		return ErrAnchorKeyNotValid
	}
	return nil
}
