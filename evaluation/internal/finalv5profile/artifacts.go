package finalv5profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ArtifactMaterializerVersion identifies the per-profile artifact layout.
const ArtifactMaterializerVersion = "taskgate-final-v5-profile-artifact-materializer-v1"

// ArtifactManifestName is the record written beside a materialized profile.
const ArtifactManifestName = "profile-artifact-manifest.json"

// publicationBundleFiles are the four files the Gateway loader requires in a
// publication directory. The loader rejects anything else, so the materializer
// must produce exactly these and nothing more.
func publicationBundleFiles(publication string) []string {
	return []string{
		publication + ".bundle.json",
		publication + ".hot.tgord",
		publication + ".cold.tgord",
		publication + ".sidecar.ndjson",
	}
}

// ArtifactFile is one materialized bundle file.
type ArtifactFile struct {
	RelativePath string `json:"relative_path"`
	Publication  string `json:"publication"`
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
	Mode         string `json:"mode"`
}

// ArtifactManifest describes one profile's exact artifact directory. It carries
// no secret, DSN, token, SQL, task identity, object key or business data.
type ArtifactManifest struct {
	SchemaVersion       int            `json:"schema_version"`
	MaterializerVersion string         `json:"materializer_version"`
	ContractRelease     string         `json:"contract_release"`
	ProfileID           string         `json:"profile_id"`
	ProfileAlias        string         `json:"profile_alias"`
	ClosureSHA256       string         `json:"closure_sha256"`
	CatalogSHA256       string         `json:"catalog_sha256"`
	PublicationSetSHA   string         `json:"publication_set_sha256"`
	SourceRootSHA256    string         `json:"source_artifact_root_sha256"`
	DirectorySHA256     string         `json:"profile_artifact_directory_sha256"`
	CreatedAt           string         `json:"created_at"`
	Publications        []string       `json:"publications"`
	Files               []ArtifactFile `json:"files"`
	TotalFiles          int            `json:"total_files"`
	TotalBytes          int64          `json:"total_bytes"`
	ExpectedHotBytes    int64          `json:"expected_hot_bytes"`
	MaxHotLimitBytes    int64          `json:"max_hot_limit_bytes"`
}

// artifactFileMode keeps a materialized bundle read-only for the runtime that
// mounts it. The Gateway only reads these files.
const artifactFileMode os.FileMode = 0o444

// MaterializeProfileArtifacts copies exactly one profile's Publication closure
// out of a verified source artifact directory into its own directory.
//
// It never symlinks and never hardlinks: a symlink escapes the mount boundary,
// and a hardlink would let a later edit of the source silently change an
// already-frozen profile directory. Generation is atomic -- a temporary
// directory is verified in full and then renamed -- so a partially written
// profile can never be mounted.
func MaterializeProfileArtifacts(profile Profile, contractRelease, sourceRoot, destinationRoot string,
	publicationSetSHA256 string) (ArtifactManifest, error) {
	if len(profile.Closure.Publications) == 0 {
		return ArtifactManifest{}, errors.New("profile closure declares no Publication")
	}
	if profile.ID == "" || !safeDirectoryName(profile.ID) {
		return ArtifactManifest{}, fmt.Errorf("profile id %q is not a safe directory name", profile.ID)
	}
	sourceDigest, err := directoryDigest(sourceRoot)
	if err != nil {
		return ArtifactManifest{}, fmt.Errorf("source artifact root: %w", err)
	}
	manifest := ArtifactManifest{SchemaVersion: 1, MaterializerVersion: ArtifactMaterializerVersion,
		ContractRelease: contractRelease, ProfileID: profile.ID, ProfileAlias: profile.Alias,
		ClosureSHA256: profile.Closure.SHA256, CatalogSHA256: profile.CatalogSHA256,
		PublicationSetSHA: publicationSetSHA256, SourceRootSHA256: sourceDigest,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		Publications:     append([]string(nil), profile.Closure.Publications...),
		ExpectedHotBytes: profile.TotalHotBytes, MaxHotLimitBytes: MaxHotBytesPerInstance}
	sort.Strings(manifest.Publications)

	final := filepath.Join(destinationRoot, profile.ID)
	temporary, err := os.MkdirTemp(destinationRoot, ".materializing-"+profile.ID+"-")
	if err != nil {
		return ArtifactManifest{}, err
	}
	defer os.RemoveAll(temporary)
	if err := os.Chmod(temporary, 0o755); err != nil {
		return ArtifactManifest{}, err
	}
	for _, publication := range manifest.Publications {
		sourceDirectory := filepath.Join(sourceRoot, publication)
		if err := requireRealDirectory(sourceDirectory); err != nil {
			return ArtifactManifest{}, fmt.Errorf("source publication %q: %w", publication, err)
		}
		targetDirectory := filepath.Join(temporary, publication)
		if err := os.Mkdir(targetDirectory, 0o755); err != nil {
			return ArtifactManifest{}, err
		}
		// Mkdir applies the caller's umask. The Gateway traverses this directory
		// through a read-only mount under a different UID, so enforce the mode.
		if err := os.Chmod(targetDirectory, 0o755); err != nil {
			return ArtifactManifest{}, err
		}
		for _, name := range publicationBundleFiles(publication) {
			file, err := copyRegularFile(filepath.Join(sourceDirectory, name), filepath.Join(targetDirectory, name))
			if err != nil {
				return ArtifactManifest{}, fmt.Errorf("copy %s/%s: %w", publication, name, err)
			}
			file.Publication = publication
			file.RelativePath = publication + "/" + name
			manifest.Files = append(manifest.Files, file)
			manifest.TotalBytes += file.Bytes
		}
	}
	sort.Slice(manifest.Files, func(left, right int) bool {
		return manifest.Files[left].RelativePath < manifest.Files[right].RelativePath
	})
	manifest.TotalFiles = len(manifest.Files)
	// Re-walk the produced directory rather than trusting the copy loop, so an
	// extra or missing entry is caught before anything is mounted.
	if err := verifyMaterializedTree(temporary, manifest); err != nil {
		return ArtifactManifest{}, err
	}
	manifest.DirectorySHA256, err = directoryDigest(temporary)
	if err != nil {
		return ArtifactManifest{}, err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ArtifactManifest{}, err
	}
	// The manifest lives beside the profile directory, never inside it: the
	// loader rejects any file it does not expect.
	if err := writeSynced(filepath.Join(destinationRoot, profile.ID+"."+ArtifactManifestName), append(encoded, '\n')); err != nil {
		return ArtifactManifest{}, err
	}
	if err := syncDirectory(temporary); err != nil {
		return ArtifactManifest{}, err
	}
	if existing, err := os.Lstat(final); err == nil {
		// Reuse is allowed only when the existing directory is byte-identical.
		if !existing.IsDir() {
			return ArtifactManifest{}, fmt.Errorf("profile artifact path %s is not a directory", final)
		}
		existingDigest, err := directoryDigest(final)
		if err != nil {
			return ArtifactManifest{}, err
		}
		if existingDigest != manifest.DirectorySHA256 {
			return ArtifactManifest{}, fmt.Errorf(
				"profile artifact directory %s already exists with digest %s, expected %s",
				final, existingDigest, manifest.DirectorySHA256)
		}
		return manifest, nil
	}
	if err := os.Rename(temporary, final); err != nil {
		return ArtifactManifest{}, err
	}
	return manifest, syncDirectory(destinationRoot)
}

// verifyMaterializedTree proves the produced directory is exactly the closure:
// no extra publication, no missing file, no symlink, and every byte intact.
func verifyMaterializedTree(root string, manifest ArtifactManifest) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) != len(manifest.Publications) {
		return fmt.Errorf("materialized directory holds %d publications, the closure declares %d",
			len(entries), len(manifest.Publications))
	}
	declared := map[string]bool{}
	for _, publication := range manifest.Publications {
		declared[publication] = true
	}
	for _, entry := range entries {
		if !declared[entry.Name()] {
			return fmt.Errorf("materialized directory contains undeclared publication %q", entry.Name())
		}
		if err := requireRealDirectory(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	expected := map[string]ArtifactFile{}
	for _, file := range manifest.Files {
		expected[file.RelativePath] = file
	}
	seen := 0
	for _, publication := range manifest.Publications {
		names, err := os.ReadDir(filepath.Join(root, publication))
		if err != nil {
			return err
		}
		if len(names) != len(publicationBundleFiles(publication)) {
			return fmt.Errorf("publication %q holds %d files, the loader requires %d",
				publication, len(names), len(publicationBundleFiles(publication)))
		}
		for _, name := range names {
			relative := publication + "/" + name.Name()
			want, declared := expected[relative]
			if !declared {
				return fmt.Errorf("materialized directory contains unexpected artifact %q", relative)
			}
			info, err := name.Info()
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("materialized artifact %q is not a regular file", relative)
			}
			digest, size, err := fileDigest(filepath.Join(root, relative))
			if err != nil {
				return err
			}
			if digest != want.SHA256 || size != want.Bytes {
				return fmt.Errorf("materialized artifact %q does not match its source bytes", relative)
			}
			seen++
		}
	}
	if seen != len(manifest.Files) {
		return fmt.Errorf("materialized directory holds %d files, the manifest declares %d", seen, len(manifest.Files))
	}
	return nil
}

// VerifyProfileArtifactDirectory re-checks an already materialized directory.
func VerifyProfileArtifactDirectory(root string, manifest ArtifactManifest) error {
	if err := verifyMaterializedTree(root, manifest); err != nil {
		return err
	}
	digest, err := directoryDigest(root)
	if err != nil {
		return err
	}
	if digest != manifest.DirectorySHA256 {
		return fmt.Errorf("profile artifact directory digest %s does not match its manifest %s",
			digest, manifest.DirectorySHA256)
	}
	return nil
}

// directoryDigest is a canonical, length-delimited digest over the sorted
// relative paths and file digests of a tree. Renaming, adding, dropping or
// editing any file changes it.
func directoryDigest(root string) (string, error) {
	type entry struct {
		path   string
		digest string
		size   int64
	}
	var entries []entry
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact %q is not a regular file", path)
		}
		digest, size, err := fileDigest(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{filepath.ToSlash(relative), digest, size})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].path < entries[right].path })
	hash := sha256.New()
	hash.Write([]byte(ArtifactMaterializerVersion + "\x00"))
	fmt.Fprintf(hash, "%d\x00", len(entries))
	for _, item := range entries {
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00%d\x00", len(item.path), item.path, item.digest, item.size)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyRegularFile(source, destination string) (ArtifactFile, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return ArtifactFile{}, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return ArtifactFile{}, fmt.Errorf("source artifact %s is not a regular file", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return ArtifactFile{}, err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, artifactFileMode)
	if err != nil {
		return ArtifactFile{}, err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), input)
	if err != nil {
		output.Close()
		return ArtifactFile{}, err
	}
	if err := output.Sync(); err != nil {
		output.Close()
		return ArtifactFile{}, err
	}
	if err := output.Close(); err != nil {
		return ArtifactFile{}, err
	}
	if written != info.Size() {
		return ArtifactFile{}, fmt.Errorf("copied %d of %d bytes from %s", written, info.Size(), source)
	}
	if err := os.Chmod(destination, artifactFileMode); err != nil {
		return ArtifactFile{}, err
	}
	return ArtifactFile{SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: written,
		Mode: fmt.Sprintf("%04o", artifactFileMode)}, nil
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", path)
	}
	return nil
}

func writeSynced(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func syncDirectory(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func safeDirectoryName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	return !strings.HasPrefix(name, ".")
}

// SchemaAttestationVersion identifies the per-profile schema attestation.
const SchemaAttestationVersion = "taskgate-final-v5-profile-schema-attestation-v1"

// SchemaAttestation binds one profile Catalog to the reporting-schema
// attestation of exactly the views its own Product closure declares. The full
// Catalog's attestation covers every Product, so a profile cannot inherit it.
type SchemaAttestation struct {
	ProfileID                    string   `json:"profile_id"`
	ProfileAlias                 string   `json:"profile_alias"`
	ClosureSHA256                string   `json:"closure_sha256"`
	Source                       string   `json:"source"`
	PostgreSQLMajorVersion       int      `json:"postgres_major_version"`
	ProductSetSHA256             string   `json:"product_set_sha256"`
	ReportingViewSetSHA256       string   `json:"reporting_view_set_sha256"`
	ReportingViews               []string `json:"reporting_views"`
	SchemaDigest                 string   `json:"schema_digest"`
	SchemaDigestToolSHA256       string   `json:"schema_digest_tool_sha256"`
	GeneratedFromFreshDeployment bool     `json:"generated_from_fresh_deployment"`
}

// SchemaAttestationRegistry is the source-controlled reviewed attestation set.
// It exists so a profile Catalog can be rebuilt byte-for-byte offline while a
// live activation still re-runs the real attestation against the deployment.
type SchemaAttestationRegistry struct {
	SchemaVersion          int                 `json:"schema_version"`
	AttestationVersion     string              `json:"attestation_version"`
	ContractRelease        string              `json:"contract_release"`
	Source                 string              `json:"source"`
	PostgreSQLMajorVersion int                 `json:"postgres_major_version"`
	GeneratedAt            string              `json:"generated_at"`
	Profiles               []SchemaAttestation `json:"profiles"`
}

// Lookup returns one profile's reviewed attestation.
func (registry SchemaAttestationRegistry) Lookup(profileID string) (SchemaAttestation, bool) {
	for _, attestation := range registry.Profiles {
		if attestation.ProfileID == profileID {
			return attestation, true
		}
	}
	return SchemaAttestation{}, false
}

// Validate rejects a structurally incomplete attestation registry.
func (registry SchemaAttestationRegistry) Validate() error {
	if registry.SchemaVersion != 1 || registry.AttestationVersion != SchemaAttestationVersion {
		return errors.New("schema attestation registry header is invalid")
	}
	seen := map[string]bool{}
	for _, attestation := range registry.Profiles {
		if seen[attestation.ProfileID] {
			return fmt.Errorf("profile %s is attested twice", attestation.ProfileID)
		}
		seen[attestation.ProfileID] = true
		if !validDigest(attestation.SchemaDigest) || !validDigest(attestation.ClosureSHA256) ||
			!validDigest(attestation.ProductSetSHA256) || !validDigest(attestation.ReportingViewSetSHA256) ||
			!validDigest(attestation.SchemaDigestToolSHA256) {
			return fmt.Errorf("profile %s attestation carries a non-digest member", attestation.ProfileID)
		}
		if len(attestation.ReportingViews) == 0 || !attestation.GeneratedFromFreshDeployment {
			return fmt.Errorf("profile %s attestation is incomplete", attestation.ProfileID)
		}
		if attestation.ReportingViewSetSHA256 != CanonicalNameSetSHA256("reporting-view-set", attestation.ReportingViews) {
			return fmt.Errorf("profile %s reporting view set digest does not describe its own views", attestation.ProfileID)
		}
	}
	return nil
}

// CanonicalNameSetSHA256 is a domain-separated, order-independent digest over a
// set of names. It is used for Product and reporting-view sets so a reordering
// cannot change an attestation while an added or dropped member always does.
func CanonicalNameSetSHA256(domain string, names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%s\x00%d\x00", SchemaAttestationVersion, domain, len(sorted))
	for _, name := range sorted {
		fmt.Fprintf(hash, "%d\x00%s\x00", len(name), name)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
