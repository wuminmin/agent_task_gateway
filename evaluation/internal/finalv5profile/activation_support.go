package finalv5profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ActivationSupportRecord is the record name of the per-profile activation
// support manifest.
const ActivationSupportRecord = "taskgate-final-v5-profile-activation-support-v1"

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ActivationSupport separates two questions that a single global constant
// previously conflated.
//
// ActivationImplementationAvailable is a property of the *harness*: an
// activator, a per-profile artifact materializer, a Gateway activation
// diagnostic, a profile-bound restart and the C12/C14/C15 mechanisms all exist.
// It says nothing about any particular profile.
//
// The per-profile ActivationSupported entries are properties of *profiles*:
// this profile has completed a real live activation smoke under the current
// contract, Catalog, Publication bundle and activation evidence. A profile the
// harness could in principle activate, but never has, is not supported.
type ActivationSupport struct {
	SchemaVersion int    `json:"schema_version"`
	Record        string `json:"record"`
	// ContractRelease pins the contract the smoke ran under. Support does not
	// carry across a contract release.
	ContractRelease string `json:"contract_release"`
	// ProfileRegistrySHA256 is informational: the registry is regenerated from
	// this manifest, so requiring the manifest to pin the regenerated registry
	// would be circular. Cross-checking is done per profile instead.
	ProfileRegistrySHA256 string `json:"profile_registry_sha256"`

	ActivationImplementationAvailable bool `json:"activation_implementation_available"`

	ActivationSmokeManifestSHA256        string `json:"activation_smoke_manifest_sha256"`
	OutsideProductRouteMatrixSHA256      string `json:"outside_product_route_matrix_sha256"`
	SemanticCacheIsolationEvidenceSHA256 string `json:"semantic_cache_isolation_evidence_sha256"`
	// The two composed evidence records must themselves have passed. A profile
	// cannot be called activation-supported on a deployment whose route matrix
	// or semantic-cache isolation evidence failed.
	OutsideProductRouteMatrixStatus      string `json:"outside_product_route_matrix_status"`
	SemanticCacheIsolationStatus         string `json:"semantic_cache_isolation_status"`
	SemanticCacheCatalogBound            bool   `json:"semantic_cache_catalog_bound"`
	OutsideProductRouteMatrixFailedCount int    `json:"outside_product_route_matrix_failed_probe_count"`

	Profiles []ProfileActivationSupport `json:"profiles"`
}

// ProfileActivationSupport is one profile's activation support claim. Every
// true claim carries the exact identity it was proven against, so a later
// Catalog, closure or contract change silently invalidates it rather than
// silently inheriting it.
type ProfileActivationSupport struct {
	ProfileID             string   `json:"profile_id"`
	ProfileAlias          string   `json:"profile_alias"`
	CatalogSHA256         string   `json:"catalog_sha256"`
	ClosureSHA256         string   `json:"closure_sha256"`
	SchemaAttestation     string   `json:"profile_schema_attestation,omitempty"`
	PublicationIdentities []string `json:"publication_identities,omitempty"`
	ActivationSupported   bool     `json:"activation_supported"`
	ActivationSmokePassed bool     `json:"activation_smoke_passed"`
	// ActivationEvidenceSHA256 lists the redacted digests of all current-release
	// PASS activation evidence documents behind the claim. A run with a
	// switch-back can legitimately contribute more than one.
	ActivationEvidenceSHA256 []string `json:"activation_evidence_sha256"`
	// Reason is empty for a supported profile and is a structured explanation
	// for an unsupported one.
	Reason  string   `json:"reason"`
	Blocked []string `json:"blocked_by,omitempty"`
}

// Validate rejects a manifest that could not be trusted to set a state. It is
// deliberately strict: this document is the only thing standing between "the
// harness can activate profiles" and "this profile was actually activated".
func (support ActivationSupport) Validate() error {
	if support.SchemaVersion != 1 || support.Record != ActivationSupportRecord {
		return errors.New("activation support manifest identity is not recognised")
	}
	if strings.TrimSpace(support.ContractRelease) == "" {
		return errors.New("activation support manifest does not pin a contract release")
	}
	for label, digest := range map[string]string{
		"profile_registry_sha256":                  support.ProfileRegistrySHA256,
		"activation_smoke_manifest_sha256":         support.ActivationSmokeManifestSHA256,
		"outside_product_route_matrix_sha256":      support.OutsideProductRouteMatrixSHA256,
		"semantic_cache_isolation_evidence_sha256": support.SemanticCacheIsolationEvidenceSHA256,
	} {
		if !digestPattern.MatchString(digest) {
			return fmt.Errorf("activation support manifest %s is not a SHA-256", label)
		}
	}
	if len(support.Profiles) == 0 {
		return errors.New("activation support manifest lists no profiles")
	}
	// A supported profile depends on deployment-wide isolation evidence, so the
	// deployment-wide gates are checked once, here, before any profile is read.
	deploymentClean := support.ActivationImplementationAvailable &&
		support.OutsideProductRouteMatrixStatus == "pass" &&
		support.OutsideProductRouteMatrixFailedCount == 0 &&
		support.SemanticCacheIsolationStatus == "pass" &&
		support.SemanticCacheCatalogBound
	seen := map[string]bool{}
	for _, profile := range support.Profiles {
		if !strings.HasPrefix(profile.ProfileID, "profile-") {
			return fmt.Errorf("activation support entry %q is not a profile ID", profile.ProfileID)
		}
		if seen[profile.ProfileID] {
			return fmt.Errorf("activation support manifest lists %s twice", profile.ProfileID)
		}
		seen[profile.ProfileID] = true
		if !profile.ActivationSupported {
			if strings.TrimSpace(profile.Reason) == "" {
				return fmt.Errorf("unsupported profile %s carries no reason", profile.ProfileID)
			}
			if profile.ActivationSmokePassed {
				return fmt.Errorf("profile %s claims a passed smoke but is not supported", profile.ProfileID)
			}
			continue
		}
		if !deploymentClean {
			return fmt.Errorf("profile %s claims activation support on a deployment whose "+
				"implementation, route matrix or semantic-cache isolation evidence did not pass",
				profile.ProfileID)
		}
		if !profile.ActivationSmokePassed {
			return fmt.Errorf("profile %s claims activation support without a passed smoke", profile.ProfileID)
		}
		if !digestPattern.MatchString(profile.CatalogSHA256) || !digestPattern.MatchString(profile.ClosureSHA256) {
			return fmt.Errorf("profile %s does not pin a Catalog and closure digest", profile.ProfileID)
		}
		if ProfileID(profile.ClosureSHA256) != profile.ProfileID {
			return fmt.Errorf("profile %s is not derived from its own closure digest", profile.ProfileID)
		}
		if len(profile.ActivationEvidenceSHA256) == 0 {
			return fmt.Errorf("profile %s claims activation support with no evidence digest", profile.ProfileID)
		}
		for _, digest := range profile.ActivationEvidenceSHA256 {
			if !digestPattern.MatchString(digest) {
				return fmt.Errorf("profile %s carries a malformed evidence digest", profile.ProfileID)
			}
		}
		if strings.TrimSpace(profile.Reason) != "" {
			return fmt.Errorf("supported profile %s also carries a blocking reason", profile.ProfileID)
		}
	}
	return nil
}

// SupportedProfiles returns the per-profile claims keyed by profile ID, after
// validation. A profile absent from the manifest is not supported; callers must
// treat a missing key as false rather than as unknown.
func (support ActivationSupport) SupportedProfiles() (map[string]ProfileActivationSupport, error) {
	if err := support.Validate(); err != nil {
		return nil, err
	}
	byID := make(map[string]ProfileActivationSupport, len(support.Profiles))
	for _, profile := range support.Profiles {
		byID[profile.ProfileID] = profile
	}
	return byID, nil
}

// DecodeActivationSupport parses and validates a manifest document.
func DecodeActivationSupport(payload []byte) (ActivationSupport, error) {
	var support ActivationSupport
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&support); err != nil {
		return ActivationSupport{}, fmt.Errorf("decode activation support manifest: %w", err)
	}
	if err := support.Validate(); err != nil {
		return ActivationSupport{}, err
	}
	return support, nil
}

// EncodeActivationSupport renders the manifest deterministically. Regenerating
// it from the same evidence must be byte-identical.
func EncodeActivationSupport(support ActivationSupport) ([]byte, error) {
	if err := support.Validate(); err != nil {
		return nil, err
	}
	sort.Slice(support.Profiles, func(left, right int) bool {
		return support.Profiles[left].ProfileID < support.Profiles[right].ProfileID
	})
	for index := range support.Profiles {
		sort.Strings(support.Profiles[index].ActivationEvidenceSHA256)
		sort.Strings(support.Profiles[index].Blocked)
		sort.Strings(support.Profiles[index].PublicationIdentities)
	}
	encoded, err := json.MarshalIndent(support, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// ActivationSupportFor resolves one profile's claim against a manifest that has
// already been validated. It returns the state and, when the state is false, a
// structured unresolved reason rather than a bare boolean.
func ActivationSupportFor(byID map[string]ProfileActivationSupport, profileID, catalogSHA,
	closureSHA string) (bool, UnresolvedReason) {
	claim, found := byID[profileID]
	if !found {
		return false, UnresolvedReason{State: "activation_supported",
			Code: "profile_absent_from_activation_support_manifest", Subject: profileID,
			Detail: "no live activation smoke has been recorded for this profile"}
	}
	if !claim.ActivationSupported {
		detail := claim.Reason
		if detail == "" {
			detail = "the activation support manifest does not support this profile"
		}
		return false, UnresolvedReason{State: "activation_supported",
			Code: "live_activation_smoke_not_executed", Subject: profileID, Detail: detail}
	}
	// A claim is bound to the identity it was proven against. A regenerated
	// Catalog or a changed closure invalidates it rather than inheriting it.
	if claim.CatalogSHA256 != catalogSHA {
		return false, UnresolvedReason{State: "activation_supported",
			Code: "activation_evidence_catalog_drift", Subject: profileID,
			Detail: "the recorded activation smoke ran against a different profile Catalog digest"}
	}
	if claim.ClosureSHA256 != closureSHA {
		return false, UnresolvedReason{State: "activation_supported",
			Code: "activation_evidence_closure_drift", Subject: profileID,
			Detail: "the recorded activation smoke ran against a different Product closure"}
	}
	return true, UnresolvedReason{}
}

// ApplyActivationSupport sets each profile's activation_supported state from
// the manifest, after profile Catalogs have been materialized so the recorded
// Catalog digest can be cross-checked.
//
// It runs as a separate pass on purpose. Build classifies a profile from its
// closure and the live Catalog alone and leaves activation_supported false; a
// state that means "this profile really was activated" cannot be produced by
// the same code that decides what the profile is.
func ApplyActivationSupport(profiles []Profile, byID map[string]ProfileActivationSupport) {
	for index := range profiles {
		profile := &profiles[index]
		supported, reason := ActivationSupportFor(byID, profile.ID, profile.CatalogSHA256,
			profile.Closure.SHA256)
		profile.Status.ActivationSupported = supported
		profile.Status.ActivationSmokePassed = supported && byID[profile.ID].ActivationSmokePassed
		// Replace whatever placeholder reason Build left behind; there must be
		// exactly one activation_supported reason, and only when it is false.
		kept := profile.Status.UnresolvedReasons[:0]
		for _, existing := range profile.Status.UnresolvedReasons {
			if existing.State != "activation_supported" {
				kept = append(kept, existing)
			}
		}
		profile.Status.UnresolvedReasons = kept
		if !supported {
			profile.Status.UnresolvedReasons = append(profile.Status.UnresolvedReasons, reason)
		}
		sortReasons(profile.Status.UnresolvedReasons)
		profile.Routable = profile.Status.Routable()
		profile.TargetedRunEligible = profile.Status.TargetedRunEligible()
	}
}
