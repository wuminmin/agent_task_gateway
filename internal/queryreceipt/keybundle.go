package queryreceipt

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
)

const PublicKeyBundleVersion = "taskgate-query-receipt-keyring/v1"

type PublicKeyBundleV1 struct {
	Version     string                 `json:"version"`
	ActiveKeyID string                 `json:"active_key_id"`
	PublishedAt time.Time              `json:"published_at"`
	Keys        []PublicKeyBundleKeyV1 `json:"keys"`
}

type PublicKeyBundleKeyV1 struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	ValidFrom string `json:"valid_from,omitempty"`
	RetiredAt string `json:"retired_at,omitempty"`
}

func NewPublicKeyBundle(activeKeyID string, keys []VerifyingKey, publishedAt time.Time) (PublicKeyBundleV1, error) {
	activeKeyID = strings.TrimSpace(activeKeyID)
	if activeKeyID == "" || len(keys) == 0 || publishedAt.IsZero() {
		return PublicKeyBundleV1{}, ErrInvalidKey
	}
	normalized := make([]VerifyingKey, 0, len(keys))
	seenActive := false
	for _, key := range keys {
		clean, err := normalizeVerifyingKey(key)
		if err != nil {
			return PublicKeyBundleV1{}, err
		}
		if clean.KeyID == activeKeyID {
			seenActive = true
		}
		normalized = append(normalized, clean)
	}
	if !seenActive {
		return PublicKeyBundleV1{}, ErrUnknownKey
	}
	if _, err := NewVerifierWithKeyring(normalized); err != nil {
		return PublicKeyBundleV1{}, err
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].KeyID < normalized[j].KeyID })
	bundle := PublicKeyBundleV1{
		Version: PublicKeyBundleVersion, ActiveKeyID: activeKeyID, PublishedAt: publishedAt.UTC(), Keys: make([]PublicKeyBundleKeyV1, 0, len(normalized)),
	}
	for _, key := range normalized {
		bundle.Keys = append(bundle.Keys, PublicKeyBundleKeyV1{
			KeyID: key.KeyID, PublicKey: base64.StdEncoding.EncodeToString(key.PublicKey),
			ValidFrom: bundleTime(key.ValidFrom), RetiredAt: bundleTime(key.RetiredAt),
		})
	}
	return bundle, nil
}

func (bundle PublicKeyBundleV1) Verifier() (*Verifier, error) {
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	keys := make([]VerifyingKey, 0, len(bundle.Keys))
	for _, entry := range bundle.Keys {
		publicKey, err := decodeBundlePublicKey(entry.PublicKey)
		if err != nil {
			return nil, err
		}
		validFrom, err := parseBundleTime(entry.ValidFrom)
		if err != nil {
			return nil, err
		}
		retiredAt, err := parseBundleTime(entry.RetiredAt)
		if err != nil {
			return nil, err
		}
		keys = append(keys, VerifyingKey{KeyID: entry.KeyID, PublicKey: publicKey, ValidFrom: validFrom, RetiredAt: retiredAt})
	}
	return NewVerifierWithKeyring(keys)
}

func (bundle PublicKeyBundleV1) Validate() error {
	if bundle.Version != PublicKeyBundleVersion || strings.TrimSpace(bundle.ActiveKeyID) == "" ||
		bundle.PublishedAt.IsZero() || len(bundle.Keys) == 0 {
		return ErrInvalidKey
	}
	seenActive := false
	seenKeys := make(map[string]struct{}, len(bundle.Keys))
	for _, entry := range bundle.Keys {
		keyID := strings.TrimSpace(entry.KeyID)
		if keyID == "" {
			return ErrInvalidKey
		}
		if _, duplicate := seenKeys[keyID]; duplicate {
			return ErrInvalidKey
		}
		seenKeys[keyID] = struct{}{}
		if keyID == bundle.ActiveKeyID {
			seenActive = true
		}
		if _, err := decodeBundlePublicKey(entry.PublicKey); err != nil {
			return err
		}
		validFrom, err := parseBundleTime(entry.ValidFrom)
		if err != nil {
			return err
		}
		retiredAt, err := parseBundleTime(entry.RetiredAt)
		if err != nil {
			return err
		}
		if !validFrom.IsZero() && !retiredAt.IsZero() && retiredAt.Before(validFrom) {
			return ErrInvalidKey
		}
	}
	if !seenActive {
		return ErrUnknownKey
	}
	return nil
}

func bundleTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func parseBundleTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}
	return parsed.UTC(), nil
}

func decodeBundlePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	}
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}
	return ed25519.PublicKey(raw), nil
}
