// Command final-v5-activation-support derives the per-profile activation
// support manifest from real activation evidence.
//
// It exists so activation_supported can never be asserted. Every true entry is
// produced by reading a PASS activation evidence document, checking it against
// the profile registry it claims to describe, and recording only redacted
// digests. A profile with no passing evidence is written out as unsupported
// with a structured reason, not omitted.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
)

const (
	registryPath     = "config/profiles/registry.json"
	supportPath      = "config/profiles/activation-support-v1.json"
	routeMatrixPath  = "evaluation/final-v5-wsl2/profiles/outside-product-route-matrix-v1.json"
	isolationPath    = "evaluation/final-v5-wsl2/profiles/semantic-cache-isolation-evidence-v1.json"
	attestationsPath = "config/profiles/schema-attestations-v1.json"
)

func main() {
	var root, evidenceDir string
	var verifyOnly bool
	flag.StringVar(&root, "root", ".", "repository root")
	flag.StringVar(&evidenceDir, "evidence-dir", "", "directory of PASS activation evidence documents")
	flag.BoolVar(&verifyOnly, "verify", false, "regenerate and fail if the committed manifest differs")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("positional arguments are not accepted"))
	}
	if err := run(root, evidenceDir, verifyOnly); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "final-v5-activation-support:", err)
	os.Exit(1)
}

func run(root, evidenceDir string, verifyOnly bool) error {
	registry, err := loadRegistry(filepath.Join(root, registryPath))
	if err != nil {
		return err
	}
	if verifyOnly && evidenceDir == "" {
		// Verification without a run directory re-reads the committed manifest
		// and re-checks it against the current registry. It cannot invent
		// support, so it is safe to run in CI.
		return verifyCommitted(root, registry)
	}
	if evidenceDir == "" {
		return errors.New("evidence-dir is required to generate the manifest")
	}
	matrix, matrixDigest, err := loadJSONWithDigest(filepath.Join(root, routeMatrixPath))
	if err != nil {
		return err
	}
	isolation, isolationDigest, err := loadJSONWithDigest(filepath.Join(root, isolationPath))
	if err != nil {
		return err
	}
	registryDigest, err := fileSHA256(filepath.Join(root, registryPath))
	if err != nil {
		return err
	}
	attestations, err := loadAttestations(filepath.Join(root, attestationsPath))
	if err != nil {
		return err
	}

	evidence, smokeDigest, err := loadEvidence(evidenceDir)
	if err != nil {
		return err
	}

	support := finalv5profile.ActivationSupport{SchemaVersion: 1,
		Record: finalv5profile.ActivationSupportRecord, ContractRelease: registry.ContractRelease,
		ProfileRegistrySHA256:                registryDigest,
		ActivationImplementationAvailable:    true,
		ActivationSmokeManifestSHA256:        smokeDigest,
		OutsideProductRouteMatrixSHA256:      matrixDigest,
		SemanticCacheIsolationEvidenceSHA256: isolationDigest,
		OutsideProductRouteMatrixStatus:      stringField(matrix, "status"),
		SemanticCacheIsolationStatus:         stringField(isolation, "status"),
		SemanticCacheCatalogBound:            boolField(isolation, "semantic_cache_catalog_bound"),
		OutsideProductRouteMatrixFailedCount: intField(matrix, "failed_probe_count"),
	}

	for _, profile := range registry.Profiles {
		support.Profiles = append(support.Profiles,
			claimFor(profile, evidence[profile.ID], registry.ContractRelease, attestations))
	}
	encoded, err := finalv5profile.EncodeActivationSupport(support)
	if err != nil {
		return err
	}
	target := filepath.Join(root, supportPath)
	if verifyOnly {
		existing, readErr := os.ReadFile(target)
		if readErr != nil {
			return fmt.Errorf("read committed activation support manifest: %w", readErr)
		}
		if string(existing) != string(encoded) {
			return errors.New("activation support manifest is not byte-identical to a regeneration")
		}
		fmt.Println("activation support manifest: byte-identical")
		return nil
	}
	if err := os.WriteFile(target, encoded, 0o644); err != nil {
		return err
	}
	supported := 0
	for _, profile := range support.Profiles {
		if profile.ActivationSupported {
			supported++
		}
	}
	fmt.Printf("activation support manifest: %d of %d profiles supported\n", supported, len(support.Profiles))
	return nil
}

// claimFor builds one profile's entry. A claim is only ever positive when a
// PASS evidence document exists that names this exact profile, closure, Catalog
// and contract, and that was not itself publication eligible.
func claimFor(profile finalv5profile.Profile, documents []evidenceDocument, contract string,
	attestations map[string]string) finalv5profile.ProfileActivationSupport {
	claim := finalv5profile.ProfileActivationSupport{ProfileID: profile.ID,
		ProfileAlias: profile.Alias, CatalogSHA256: profile.CatalogSHA256,
		ClosureSHA256: profile.Closure.SHA256, SchemaAttestation: attestations[profile.ID]}
	for _, publication := range profile.Closure.Publications {
		claim.PublicationIdentities = append(claim.PublicationIdentities, publication)
	}

	var blocked []string
	if !profile.Status.ClosureComplete {
		blocked = append(blocked, "closure_incomplete")
	}
	if !profile.Status.CatalogMaterializable {
		blocked = append(blocked, "catalog_materializable_false")
	}
	if !profile.Status.LiveRouteAvailable {
		blocked = append(blocked, "live_route_available_false",
			"no_approval_route_for_exact_product_closure")
	}
	// Carry the specific structured codes the registry already derived for the
	// three states that gate activation support, so the manifest names the
	// actual blocker rather than only its category. targeted_validation_passed
	// is deliberately excluded: it is what a targeted run establishes *after*
	// activation support, never a precondition of it.
	gating := map[string]bool{"closure_complete": true, "catalog_materializable": true,
		"live_route_available": true}
	for _, reason := range profile.Status.UnresolvedReasons {
		if !gating[reason.State] || contains(blocked, reason.Code) {
			continue
		}
		blocked = append(blocked, reason.Code)
	}

	var digests []string
	for _, document := range documents {
		if document.evidence.Status != "pass" || !document.evidence.ActivationSmokePassed {
			continue
		}
		if document.evidence.PublicationEligible {
			// Publication-eligible evidence belongs to a Campaign, not to a
			// pilot smoke, and must never be recycled into a readiness state.
			continue
		}
		if document.evidence.ContractRelease != contract ||
			document.evidence.ProfileID != profile.ID ||
			document.evidence.ClosureSHA256 != profile.Closure.SHA256 ||
			document.evidence.CatalogSHA256 != profile.CatalogSHA256 {
			continue
		}
		digests = append(digests, document.digest)
	}
	sort.Strings(digests)
	if len(digests) == 0 {
		blocked = append(blocked, "live_activation_smoke_not_executed")
		sort.Strings(blocked)
		claim.Blocked = blocked
		claim.Reason = strings.Join(blocked, "; ")
		return claim
	}
	if len(blocked) != 0 {
		// Evidence exists but the profile is not otherwise cleared. Refuse the
		// claim rather than letting one passing state paper over another.
		sort.Strings(blocked)
		claim.Blocked = blocked
		claim.Reason = strings.Join(blocked, "; ")
		return claim
	}
	claim.ActivationSupported = true
	claim.ActivationSmokePassed = true
	claim.ActivationEvidenceSHA256 = digests
	return claim
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

type evidenceDocument struct {
	digest   string
	evidence finalv5profile.ActivationEvidence
}

// loadEvidence reads every activation evidence document in the run directory
// and returns them grouped by profile, plus a digest over the whole set so the
// manifest is bound to the exact smoke it was derived from.
func loadEvidence(directory string) (map[string][]evidenceDocument, string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, "", fmt.Errorf("read evidence directory: %w", err)
	}
	byProfile := map[string][]evidenceDocument{}
	var lines []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, "", err
		}
		var evidence finalv5profile.ActivationEvidence
		if err := json.Unmarshal(payload, &evidence); err != nil {
			return nil, "", fmt.Errorf("decode %s: %w", entry.Name(), err)
		}
		digest := sha256.Sum256(payload)
		document := evidenceDocument{digest: hex.EncodeToString(digest[:]), evidence: evidence}
		byProfile[evidence.ProfileID] = append(byProfile[evidence.ProfileID], document)
		// Only the digest and the profile identity enter the manifest input.
		// No token, DSN, SQL, task ID, row, object key or Parquet byte does.
		lines = append(lines, evidence.ProfileID+" "+document.digest)
	}
	if len(lines) == 0 {
		return nil, "", errors.New("evidence directory holds no activation evidence")
	}
	sort.Strings(lines)
	set := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return byProfile, hex.EncodeToString(set[:]), nil
}

func verifyCommitted(root string, registry finalv5profile.Registry) error {
	payload, err := os.ReadFile(filepath.Join(root, supportPath))
	if os.IsNotExist(err) {
		// No manifest means no profile has a recorded live activation smoke
		// under this contract release. That is a legitimate state -- it is
		// exactly where a fresh contract release starts -- so verification
		// checks that the registry agrees rather than demanding a file.
		for _, profile := range registry.Profiles {
			if profile.Status.ActivationSupported {
				return fmt.Errorf("profile %s claims activation support with no manifest", profile.Alias)
			}
		}
		fmt.Printf("activation support manifest: absent; %d registry profiles are unsupported\n",
			len(registry.Profiles))
		return nil
	}
	if err != nil {
		return fmt.Errorf("read activation support manifest: %w", err)
	}
	support, err := finalv5profile.DecodeActivationSupport(payload)
	if err != nil {
		return err
	}
	if support.ContractRelease != registry.ContractRelease {
		return fmt.Errorf("activation support manifest pins contract %s, the registry is %s",
			support.ContractRelease, registry.ContractRelease)
	}
	byID, err := support.SupportedProfiles()
	if err != nil {
		return err
	}
	for _, profile := range registry.Profiles {
		supported, reason := finalv5profile.ActivationSupportFor(byID, profile.ID,
			profile.CatalogSHA256, profile.Closure.SHA256)
		if supported != profile.Status.ActivationSupported {
			return fmt.Errorf("profile %s registry activation_supported=%t, manifest derives %t (%s)",
				profile.Alias, profile.Status.ActivationSupported, supported, reason.Code)
		}
	}
	fmt.Printf("activation support manifest: consistent with %d registry profiles\n", len(registry.Profiles))
	return nil
}

func loadRegistry(path string) (finalv5profile.Registry, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return finalv5profile.Registry{}, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(value, &registry); err != nil {
		return finalv5profile.Registry{}, fmt.Errorf("decode profile registry: %w", err)
	}
	if len(registry.Profiles) == 0 {
		return finalv5profile.Registry{}, errors.New("profile registry is empty")
	}
	return registry, nil
}

func loadAttestations(path string) (map[string]string, error) {
	value, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var registry finalv5profile.SchemaAttestationRegistry
	if err := json.Unmarshal(value, &registry); err != nil {
		return nil, fmt.Errorf("decode schema attestations: %w", err)
	}
	byID := map[string]string{}
	for _, attestation := range registry.Profiles {
		byID[attestation.ProfileID] = attestation.SchemaDigest
	}
	return byID, nil
}

func loadJSONWithDigest(path string) (map[string]any, string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	digest := sha256.Sum256(payload)
	return document, hex.EncodeToString(digest[:]), nil
}

func fileSHA256(path string) (string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func stringField(document map[string]any, key string) string {
	if value, ok := document[key].(string); ok {
		return value
	}
	return ""
}

func boolField(document map[string]any, key string) bool {
	value, _ := document[key].(bool)
	return value
}

func intField(document map[string]any, key string) int {
	if value, ok := document[key].(float64); ok {
		return int(value)
	}
	return -1
}
