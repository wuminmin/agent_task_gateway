package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
)

// SourceIdentity is one source-built dependency, bound to the commit.
type SourceIdentity struct {
	Path string `json:"path"`
	// WorkingTreeSHA256 is the digest of the bytes actually used.
	WorkingTreeSHA256 string `json:"working_tree_sha256"`
	// CommittedSHA256 is the digest of the same path at the recorded commit.
	// Qualification refuses unless the two are equal, which is what proves the
	// bytes that ran belong to the commit the evidence names.
	CommittedSHA256 string `json:"committed_sha256"`
}

// QualificationProvenance binds a qualification run to a clean, published tree.
//
// Without it a footprint could be measured from an edited working tree and still
// name a commit, and nothing in the evidence would show the difference.
type QualificationProvenance struct {
	Commit             string           `json:"commit"`
	Origin             string           `json:"origin_commit"`
	Branch             string           `json:"branch"`
	WorktreeClean      bool             `json:"worktree_clean"`
	HeadEqualsOrigin   bool             `json:"head_equals_origin"`
	SourceIdentities   []SourceIdentity `json:"source_identities"`
	SourceManifestHash string           `json:"source_manifest_sha256"`
}

// ProfileBinding is the exact profile material a qualification is bound to.
type ProfileBinding struct {
	ProfileID string `json:"profile_id"`
	Alias     string `json:"profile_alias"`
	// RegistryRelease and RegistrySHA256 pin the Profile Registry the profile
	// was read from.
	RegistryRelease string `json:"profile_registry_release"`
	RegistrySHA256  string `json:"profile_registry_sha256"`
	// CatalogSHA256 is the digest of the exact loaded Profile Catalog bytes.
	CatalogSHA256 string `json:"catalog_sha256"`
	// ExpectedSchemaDigest and ExpectedSchemaEntries come from
	// catalogschema.Build over that Catalog.
	ExpectedSchemaDigest  string `json:"expected_schema_digest"`
	ExpectedSchemaEntries int64  `json:"expected_schema_entries"`
	// ArtifactManifestSHA256 digests the per-profile artifact manifest the
	// Gateway was started against.
	ArtifactManifestSHA256 string `json:"profile_artifact_manifest_sha256"`
	// ArtifactDirectorySHA256 is the manifest's own directory digest.
	ArtifactDirectorySHA256 string `json:"profile_artifact_directory_sha256"`
}

// requiredSourcePaths are the source-built dependencies whose bytes decide what a
// qualification measures. A change to any of them changes the measurement, so
// each is digested and bound to the commit.
func requiredSourcePaths() []string {
	return []string{
		"evaluation/cmd/final-v5-attestation-footprint/main.go",
		"evaluation/cmd/final-v5-attestation-footprint/provenance.go",
		"evaluation/final-v5-wsl2/scripts/qualify-attestation-footprint.sh",
		"evaluation/internal/experiment/attestation_footprint.go",
		"evaluation/internal/experiment/strict_ast.go",
		"internal/approval/protocol.go",
		"internal/catalogschema/catalogschema.go",
		"internal/dataconnector/pins.go",
		"internal/dataconnector/postgres.go",
		"internal/dataconnector/statements.go",
		"internal/sqlidentity/strict_ast.go",
		"evaluation/final-v5-wsl2/compose.observer-v3.yaml",
	}
}

// collectProvenance refuses a qualification that cannot be tied to a clean,
// published commit.
func collectProvenance(root string) (QualificationProvenance, error) {
	var provenance QualificationProvenance
	git := func(arguments ...string) (string, error) {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w", strings.Join(arguments, " "), err)
		}
		return strings.TrimSpace(string(output)), nil
	}

	status, err := git("status", "--porcelain")
	if err != nil {
		return provenance, err
	}
	provenance.WorktreeClean = status == ""
	if !provenance.WorktreeClean {
		return provenance, fmt.Errorf("qualification refuses a dirty worktree; %d path(s) differ",
			len(strings.Split(status, "\n")))
	}
	if provenance.Commit, err = git("rev-parse", "HEAD"); err != nil {
		return provenance, err
	}
	if provenance.Branch, err = git("rev-parse", "--abbrev-ref", "HEAD"); err != nil {
		return provenance, err
	}
	provenance.Origin, err = git("rev-parse", "origin/"+provenance.Branch)
	if err != nil {
		return provenance, fmt.Errorf("qualification requires a published branch: %w", err)
	}
	provenance.HeadEqualsOrigin = provenance.Commit == provenance.Origin
	if !provenance.HeadEqualsOrigin {
		return provenance, fmt.Errorf("qualification refuses an unpublished commit: HEAD %s, origin/%s %s",
			provenance.Commit[:12], provenance.Branch, provenance.Origin[:12])
	}

	for _, path := range requiredSourcePaths() {
		working, err := os.ReadFile(root + "/" + path)
		if err != nil {
			return provenance, fmt.Errorf("read source dependency %s: %w", path, err)
		}
		committed, err := exec.Command("git", "-C", root, "show", provenance.Commit+":"+path).Output()
		if err != nil {
			return provenance, fmt.Errorf("read %s at %s: %w", path, provenance.Commit[:12], err)
		}
		identity := SourceIdentity{
			Path: path, WorkingTreeSHA256: digestBytes(working), CommittedSHA256: digestBytes(committed),
		}
		// A clean worktree should make these equal; checking anyway means a
		// stale build or a path outside git cannot slip through.
		if identity.WorkingTreeSHA256 != identity.CommittedSHA256 {
			return provenance, fmt.Errorf("%s does not match its bytes at %s", path, provenance.Commit[:12])
		}
		provenance.SourceIdentities = append(provenance.SourceIdentities, identity)
	}
	sort.Slice(provenance.SourceIdentities, func(left, right int) bool {
		return provenance.SourceIdentities[left].Path < provenance.SourceIdentities[right].Path
	})
	hash := sha256.New()
	hash.Write([]byte("TASKGATE-FINAL-V5-QUALIFICATION-SOURCE-MANIFEST-V1\x00"))
	for _, identity := range provenance.SourceIdentities {
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00", len(identity.Path), identity.Path, identity.CommittedSHA256)
	}
	provenance.SourceManifestHash = hex.EncodeToString(hash.Sum(nil))
	return provenance, nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// bindProfile resolves and validates every profile identity the qualification is
// bound to.
func bindProfile(root, registryPath, catalogPath, profileID, artifactManifestPath string) (
	ProfileBinding, catalogschema.Result, error) {
	var binding ProfileBinding

	registryBytes, err := os.ReadFile(root + "/" + registryPath)
	if err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("read profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(registryBytes, &registry); err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("decode profile registry: %w", err)
	}
	binding.RegistrySHA256 = digestBytes(registryBytes)
	binding.RegistryRelease = registry.ContractRelease
	if binding.RegistryRelease == "" {
		return binding, catalogschema.Result{}, errors.New("profile registry names no contract release")
	}

	var found bool
	for _, profile := range registry.Profiles {
		if profile.ID == profileID {
			binding.ProfileID, binding.Alias, found = profile.ID, profile.Alias, true
			break
		}
	}
	if !found {
		return binding, catalogschema.Result{}, fmt.Errorf("profile %q is absent from the registry", profileID)
	}

	catalogBytes, err := os.ReadFile(catalogPath)
	if err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("read profile catalog: %w", err)
	}
	binding.CatalogSHA256 = digestBytes(catalogBytes)

	logicalCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("load profile catalog: %w", err)
	}
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("build ExpectedSchema: %w", err)
	}
	binding.ExpectedSchemaDigest, binding.ExpectedSchemaEntries = built.Digest, built.Count

	manifestBytes, err := os.ReadFile(artifactManifestPath)
	if err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("read profile artifact manifest: %w", err)
	}
	binding.ArtifactManifestSHA256 = digestBytes(manifestBytes)
	var manifestSet struct {
		Profiles map[string]struct {
			DirectorySHA256 string `json:"profile_artifact_directory_sha256"`
			CatalogSHA256   string `json:"catalog_sha256"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(manifestBytes, &manifestSet); err != nil {
		return binding, catalogschema.Result{}, fmt.Errorf("decode profile artifact manifest: %w", err)
	}
	entry, present := manifestSet.Profiles[profileID]
	if !present {
		return binding, catalogschema.Result{}, fmt.Errorf(
			"the profile artifact manifest does not cover profile %q", profileID)
	}
	binding.ArtifactDirectorySHA256 = entry.DirectorySHA256
	if binding.ArtifactDirectorySHA256 == "" {
		return binding, catalogschema.Result{}, errors.New("the profile artifact manifest carries no directory digest")
	}
	// The materializer digests the Catalog it built the artifacts from. If that
	// disagrees with the Catalog this qualification loaded, the Gateway was
	// started against artifacts belonging to a different Catalog -- which no
	// statement count would reveal.
	if entry.CatalogSHA256 != binding.CatalogSHA256 {
		return binding, catalogschema.Result{}, fmt.Errorf(
			"the profile artifacts were materialized from Catalog %s but this qualification loaded %s",
			entry.CatalogSHA256[:12], binding.CatalogSHA256[:12])
	}
	return binding, built, nil
}
