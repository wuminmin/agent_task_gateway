package finalv5profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const profileRoutingIdentityVersion = "taskgate-final-v5-profile-routing-identity-v1"

// ProfileRoutingIdentitySHA256 binds the part of the profile registry that a
// live route actually serves. ActivationSupported and the other readiness
// fields are deliberately excluded: they are generated from live evidence,
// so including them would make that evidence circular. CatalogSHA256 binds the
// complete per-profile route and budget policy; Closure.SHA256 independently
// binds the structural Product/Publication/Source/Scope set.
func ProfileRoutingIdentitySHA256(payload []byte) (string, error) {
	var registry Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return "", fmt.Errorf("decode profile registry routing identity: %w", err)
	}
	if registry.SchemaVersion != 1 || strings.TrimSpace(registry.RegistryVersion) == "" ||
		strings.TrimSpace(registry.ContractRelease) == "" || len(registry.Profiles) == 0 {
		return "", errors.New("profile registry routing identity is incomplete")
	}
	type routingProfile struct {
		ProfileID     string `json:"profile_id"`
		Alias         string `json:"alias"`
		ClosureSHA256 string `json:"closure_sha256"`
		CatalogSHA256 string `json:"catalog_sha256"`
		CatalogPath   string `json:"catalog_path"`
	}
	type routingIdentity struct {
		Version         string           `json:"version"`
		RegistryVersion string           `json:"registry_version"`
		ContractRelease string           `json:"contract_release"`
		Profiles        []routingProfile `json:"profiles"`
	}
	identity := routingIdentity{Version: profileRoutingIdentityVersion,
		RegistryVersion: registry.RegistryVersion, ContractRelease: registry.ContractRelease}
	seen := map[string]bool{}
	for _, profile := range registry.Profiles {
		if profile.ID == "" || profile.Alias == "" || profile.CatalogPath == "" ||
			!lowerSHA256(profile.CatalogSHA256) || !lowerSHA256(profile.Closure.SHA256) || seen[profile.ID] {
			return "", errors.New("profile registry has an incomplete or duplicate routing identity")
		}
		seen[profile.ID] = true
		identity.Profiles = append(identity.Profiles, routingProfile{ProfileID: profile.ID, Alias: profile.Alias,
			ClosureSHA256: profile.Closure.SHA256, CatalogSHA256: profile.CatalogSHA256,
			CatalogPath: profile.CatalogPath})
	}
	sort.Slice(identity.Profiles, func(left, right int) bool {
		return identity.Profiles[left].ProfileID < identity.Profiles[right].ProfileID
	})
	canonical, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func lowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}
