package formalbuild

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// BuildManifestSchemaVersion is the machine-readable formal Gateway build
// manifest consumed by the publication deployment launchers.
const BuildManifestSchemaVersion = 1

// BuildBindings links a formal image to the already-validated deployment
// inputs. They do not enter the build context or image layers; their purpose is
// to make the image-to-Dataset/Profile provenance join explicit and
// independently checkable before Compose mutates a deployment.
type BuildBindings struct {
	DatasetBindingSHA256  string `json:"dataset_binding_sha256,omitempty"`
	ProfileRegistrySHA256 string `json:"profile_registry_sha256,omitempty"`
}

func (bindings BuildBindings) validate() error {
	for name, value := range map[string]string{
		"dataset binding":  bindings.DatasetBindingSHA256,
		"profile registry": bindings.ProfileRegistrySHA256,
	} {
		if value != "" && !validSHA256(value) {
			return fmt.Errorf("%s digest %q is not a lowercase SHA-256", name, value)
		}
	}
	return nil
}

// BuildManifest is the immutable join between a formal build, its exact local
// image ID, and (for campaign builds) the deployment inputs already approved by
// the surrounding launcher.
type BuildManifest struct {
	SchemaVersion        int    `json:"schema_version"`
	SubmissionCommit     string `json:"submission_commit"`
	CleanTreeAtBuild     bool   `json:"clean_tree_at_build"`
	BuildContextSHA256   string `json:"build_context_sha256"`
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	ImageID              string `json:"image_id"`
	ImageTag             string `json:"image_tag"`
	Platform             string `json:"platform"`
	BuildTarget          string `json:"build_target"`
	BuilderBaseImage     string `json:"builder_base_image"`
	RuntimeBaseImage     string `json:"runtime_base_image"`
	BuildBindings
}

// NewBuildManifest constructs a manifest only from a verified formal image and
// the independently materialized source context.
func NewBuildManifest(materialized Context, image ImageInspect, tag string,
	bindings BuildBindings) (BuildManifest, error) {
	if err := RequireFormalImage(image, FromContext(materialized)); err != nil {
		return BuildManifest{}, err
	}
	builder, present := image.Label(LabelBuilderBaseImage)
	if !present {
		return BuildManifest{}, fmt.Errorf("image %s omits the %s label", shortImageID(image.ID), LabelBuilderBaseImage)
	}
	runtime, present := image.Label(LabelRuntimeBaseImage)
	if !present {
		return BuildManifest{}, fmt.Errorf("image %s omits the %s label", shortImageID(image.ID), LabelRuntimeBaseImage)
	}
	manifest := BuildManifest{
		SchemaVersion:        BuildManifestSchemaVersion,
		SubmissionCommit:     materialized.Source.Commit,
		CleanTreeAtBuild:     materialized.Source.CleanTree,
		BuildContextSHA256:   materialized.ContextSHA256,
		SourceManifestSHA256: materialized.SourceManifestSHA256,
		ImageID:              image.ID,
		ImageTag:             strings.TrimSpace(tag),
		Platform:             image.Platform(),
		BuildTarget:          GatewayBuildTarget,
		BuilderBaseImage:     builder,
		RuntimeBaseImage:     runtime,
		BuildBindings:        bindings,
	}
	if err := manifest.Validate(); err != nil {
		return BuildManifest{}, err
	}
	return manifest, nil
}

// Validate checks the manifest as a self-contained typed document. Agreement
// with the current repository and Docker Engine is checked separately by
// VerifyBuildDeployment.
func (manifest BuildManifest) Validate() error {
	if manifest.SchemaVersion != BuildManifestSchemaVersion {
		return fmt.Errorf("formal Gateway build manifest schema_version=%d, want %d",
			manifest.SchemaVersion, BuildManifestSchemaVersion)
	}
	if !validCommitSHA1(manifest.SubmissionCommit) {
		return errors.New("formal Gateway build manifest has no full submission commit")
	}
	if !manifest.CleanTreeAtBuild {
		return errors.New("formal Gateway build manifest does not attest a clean build tree")
	}
	if !validSHA256(manifest.BuildContextSHA256) || !validSHA256(manifest.SourceManifestSHA256) {
		return errors.New("formal Gateway build manifest has malformed source digests")
	}
	if err := requireImageID(manifest.ImageID); err != nil {
		return fmt.Errorf("formal Gateway build manifest image_id: %w", err)
	}
	if strings.TrimSpace(manifest.ImageTag) == "" {
		return errors.New("formal Gateway build manifest has no informational image tag")
	}
	if manifest.Platform == "" || manifest.Platform == "/" || strings.ContainsAny(manifest.Platform, "\r\n") {
		return errors.New("formal Gateway build manifest has no valid platform")
	}
	if manifest.BuildTarget != GatewayBuildTarget {
		return fmt.Errorf("formal Gateway build manifest names target %q, want %q",
			manifest.BuildTarget, GatewayBuildTarget)
	}
	for name, reference := range map[string]string{
		"builder": manifest.BuilderBaseImage,
		"runtime": manifest.RuntimeBaseImage,
	} {
		if !validDigestReference(referenceDigest(reference)) {
			return fmt.Errorf("formal Gateway build manifest %s base image %q is not digest-pinned", name, reference)
		}
	}
	return manifest.BuildBindings.validate()
}

// Require binds a manifest to the source and deployment inputs independently
// resolved by the caller.
func (manifest BuildManifest) Require(expected ExpectedSource, bindings BuildBindings) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if err := bindings.validate(); err != nil {
		return err
	}
	for _, check := range []struct{ name, got, want string }{
		{"submission commit", manifest.SubmissionCommit, expected.Commit},
		{"build-context digest", manifest.BuildContextSHA256, expected.ContextSHA256},
		{"source-manifest digest", manifest.SourceManifestSHA256, expected.SourceManifestSHA256},
		{"Dataset binding digest", manifest.DatasetBindingSHA256, bindings.DatasetBindingSHA256},
		{"profile registry digest", manifest.ProfileRegistrySHA256, bindings.ProfileRegistrySHA256},
	} {
		if check.got != check.want {
			return fmt.Errorf("formal Gateway build manifest %s is %q, expected %q", check.name, check.got, check.want)
		}
	}
	if !expected.CleanTree {
		return errors.New("formal Gateway build manifest cannot be verified against a dirty source tree")
	}
	return nil
}

// ComposeOverride renders the complete image-only override. The content names
// the immutable image ID rather than the mutable convenience tag.
func (manifest BuildManifest) ComposeOverride() ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	// !reset is load bearing: an ordinary base Compose file carries
	// gateway.build. Merely adding an immutable image and pull_policy leaves that
	// build stanza merged in, so a later `compose up` could recreate the
	// ordinary Dockerfile image if the verified image disappeared locally.
	return []byte("services:\n  gateway:\n    image: \"" + manifest.ImageID +
		"\"\n    pull_policy: never\n    build: !reset null\n"), nil
}

// WriteBuildDeployment writes the manifest and its exact Compose override with
// create-exclusive mode-0600 semantics. A partial first output is removed if
// the second cannot be created.
func WriteBuildDeployment(manifestPath, overridePath string, manifest BuildManifest) error {
	if strings.TrimSpace(manifestPath) == "" || strings.TrimSpace(overridePath) == "" {
		return errors.New("formal Gateway build manifest and Compose override paths are both required")
	}
	if filepath.Clean(manifestPath) == filepath.Clean(overridePath) {
		return errors.New("formal Gateway build manifest and Compose override paths must differ")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	override, err := manifest.ComposeOverride()
	if err != nil {
		return err
	}
	if err := writeExclusiveRegular(manifestPath, payload); err != nil {
		return fmt.Errorf("write formal Gateway build manifest: %w", err)
	}
	if err := writeExclusiveRegular(overridePath, override); err != nil {
		if removeErr := os.Remove(manifestPath); removeErr != nil {
			return fmt.Errorf("write formal Gateway Compose override: %v (also could not remove partial manifest: %v)",
				err, removeErr)
		}
		return fmt.Errorf("write formal Gateway Compose override: %w", err)
	}
	return nil
}

// LoadBuildManifest reads one strict, mode-0600, non-symlink manifest.
func LoadBuildManifest(path string) (BuildManifest, error) {
	var manifest BuildManifest
	payload, err := readPrivateRegular(path, "formal Gateway build manifest")
	if err != nil {
		return manifest, err
	}
	if err := experiment.StrictJSON(payload, &manifest); err != nil {
		return BuildManifest{}, fmt.Errorf("decode formal Gateway build manifest: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return BuildManifest{}, err
	}
	return manifest, nil
}

// VerifyBuildDeployment proves that the private manifest, exact image-only
// override, current source, deployment bindings, and Docker image all agree.
func VerifyBuildDeployment(ctx context.Context, engine Engine, manifestPath, overridePath string,
	expected ExpectedSource, bindings BuildBindings) (BuildManifest, error) {
	manifest, err := LoadBuildManifest(manifestPath)
	if err != nil {
		return BuildManifest{}, err
	}
	if err := manifest.Require(expected, bindings); err != nil {
		return BuildManifest{}, err
	}
	override, err := readPrivateRegular(overridePath, "formal Gateway Compose override")
	if err != nil {
		return BuildManifest{}, err
	}
	wantOverride, err := manifest.ComposeOverride()
	if err != nil {
		return BuildManifest{}, err
	}
	if !bytes.Equal(override, wantOverride) {
		return BuildManifest{}, errors.New("formal Gateway Compose override does not exactly name the manifest image ID")
	}
	image, err := engine.ImageInspect(ctx, manifest.ImageID)
	if err != nil {
		return BuildManifest{}, fmt.Errorf("formal Gateway manifest image is unavailable: %w", err)
	}
	if image.ID != manifest.ImageID {
		return BuildManifest{}, fmt.Errorf("formal Gateway manifest names image %s but Engine returned %s",
			shortImageID(manifest.ImageID), shortImageID(image.ID))
	}
	if err := RequireFormalImage(image, expected); err != nil {
		return BuildManifest{}, err
	}
	for _, check := range []struct{ name, got, want string }{
		{"platform", image.Platform(), manifest.Platform},
		{"builder base image", labelValue(image, LabelBuilderBaseImage), manifest.BuilderBaseImage},
		{"runtime base image", labelValue(image, LabelRuntimeBaseImage), manifest.RuntimeBaseImage},
	} {
		if check.got != check.want {
			return BuildManifest{}, fmt.Errorf("formal Gateway image %s is %q, build manifest records %q",
				check.name, check.got, check.want)
		}
	}
	return manifest, nil
}

// RegularFileSHA256 hashes a regular non-symlink file and rejects replacement
// while it is being read. Private-file permission checks remain the launcher's
// responsibility because the tracked profile registry is intentionally 0644.
func RegularFileSHA256(path, label string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", label, err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is not a regular non-symlink file", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", label, err)
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(path)
	if copyErr != nil {
		return "", fmt.Errorf("hash %s: %w", label, copyErr)
	}
	if statErr != nil || lstatErr != nil || closeErr != nil {
		return "", fmt.Errorf("recheck %s after hashing", label)
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s changed while it was being hashed", label)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// ArtifactSHA256 returns the digest of one already-validated private output.
func ArtifactSHA256(path, label string) (string, error) {
	payload, err := readPrivateRegular(path, label)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func labelValue(image ImageInspect, name string) string {
	value, _ := image.Label(name)
	return value
}

func writeExclusiveRegular(path string, payload []byte) (resultErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return nil
}

func readPrivateRegular(path, label string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", label, err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%s must be a regular non-symlink mode-0600 file", label)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
	opened, statErr := file.Stat()
	closeErr := file.Close()
	after, lstatErr := os.Lstat(path)
	if readErr != nil {
		return nil, fmt.Errorf("read %s: %w", label, readErr)
	}
	if len(payload) > 1<<20 {
		return nil, fmt.Errorf("%s exceeds 1 MiB", label)
	}
	if statErr != nil || closeErr != nil || lstatErr != nil ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) ||
		!after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || after.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%s changed while it was being read", label)
	}
	return payload, nil
}
