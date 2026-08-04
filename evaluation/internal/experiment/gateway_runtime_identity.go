package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// GatewayRuntimeIdentityV1Version identifies the typed Gateway identity.
const GatewayRuntimeIdentityV1Version = "taskgate-gateway-runtime-identity-v1"

const gatewayRuntimeIdentityDomain = "TASKGATE-FINAL-V5-GATEWAY-RUNTIME-IDENTITY-V1"

// GatewayRuntimeIdentityV1 is the immutable source/build/runtime identity of the
// Gateway that served a measured window.
//
// Every member is carried separately and stays independently inspectable. An
// earlier draft filled a single gateway_source_sha256 by hashing the checkout
// the observer happened to run from, which asserts something the observer cannot
// know: that the running container was built from those bytes. Nothing in the
// evidence distinguished "the image came from this commit" from "someone ran the
// observer in a directory that happens to be at this commit".
//
// AggregateSHA256 is a convenience for comparing two identities in one field. It
// is derived from the members, never a substitute for them: a reviewer must be
// able to check each binding on its own, and an aggregate that is the only
// carried value is an unexplained opaque hash.
type GatewayRuntimeIdentityV1 struct {
	Version string `json:"version"`
	// SubmissionCommit is the published commit the formal image was built from.
	SubmissionCommit string `json:"submission_commit"`
	// CleanTreeAtBuild records that the build refused a dirty worktree. It is
	// carried rather than merely enforced so the evidence states the property.
	CleanTreeAtBuild bool `json:"clean_tree_at_build"`
	// BuildContextSHA256 digests the deterministic tracked-file-only build
	// context: relative path, file mode, file bytes and symlink target.
	BuildContextSHA256 string `json:"build_context_sha256"`
	// SourceManifestSHA256 digests the enumerated source manifest of that same
	// context, computed from the commit tree rather than from the host directory.
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	// BuildTarget is the cmd/ target the image was built for. It must be gateway:
	// the same Dockerfile builds every command in the repository.
	BuildTarget string `json:"build_target"`
	// OCIRevisionLabel is org.opencontainers.image.revision as read back off the
	// built image. It must equal SubmissionCommit.
	OCIRevisionLabel string `json:"oci_revision_label"`
	// LocalImageID is the content-addressed ID of the image that was built.
	LocalImageID string `json:"local_image_id"`
	// ContainerImageID is the image ID the running container was created from.
	ContainerImageID string `json:"container_image_id"`
	// BinarySHA256 is the digest of the Gateway executable inside the image.
	BinarySHA256 string `json:"gateway_binary_sha256"`
	// Platform is the resolved os/architecture.
	Platform string `json:"platform"`
	// HealthcheckSHA256 digests the running periodic healthcheck definition.
	HealthcheckSHA256 string `json:"healthcheck_definition_sha256"`
	// BuilderBaseImage and RuntimeBaseImage are the immutable identities of the
	// two base images the formal build used. Without them the image is only as
	// reproducible as two mutable upstream tags.
	BuilderBaseImage string `json:"builder_base_image"`
	RuntimeBaseImage string `json:"runtime_base_image"`
	// AggregateSHA256 is the canonical digest over every member above.
	AggregateSHA256 string `json:"aggregate_sha256"`
}

// gatewayBuildTarget is the only build target a formal Gateway image may carry.
const gatewayBuildTarget = "gateway"

// Validate rejects an identity that leaves the running Gateway ambiguous.
func (identity GatewayRuntimeIdentityV1) Validate() error {
	if identity.Version != GatewayRuntimeIdentityV1Version {
		return fmt.Errorf("gateway runtime identity version %q is unsupported; v3 acceptance requires %s",
			identity.Version, GatewayRuntimeIdentityV1Version)
	}
	if !validCommitSHA1(identity.SubmissionCommit) {
		return errors.New("submission_commit is not a full lowercase git object name")
	}
	if !identity.CleanTreeAtBuild {
		return errors.New("the formal image was built from a dirty worktree")
	}
	for name, value := range map[string]string{
		"build_context_sha256":          identity.BuildContextSHA256,
		"source_manifest_sha256":        identity.SourceManifestSHA256,
		"gateway_binary_sha256":         identity.BinarySHA256,
		"healthcheck_definition_sha256": identity.HealthcheckSHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("%s is not a lowercase SHA-256", name)
		}
	}
	if identity.BuildTarget != gatewayBuildTarget {
		return fmt.Errorf("build_target is %q; a formal Gateway image is built for %q",
			identity.BuildTarget, gatewayBuildTarget)
	}
	// The revision label is what a reviewer reads off the image. If it can differ
	// from the commit the build claims, the label proves nothing.
	if identity.OCIRevisionLabel != identity.SubmissionCommit {
		return fmt.Errorf("the image revision label is %s but the build names commit %s",
			shortCommit(identity.OCIRevisionLabel), shortCommit(identity.SubmissionCommit))
	}
	if err := requireImageID(identity.LocalImageID); err != nil {
		return fmt.Errorf("local_image_id: %w", err)
	}
	if err := requireImageID(identity.ContainerImageID); err != nil {
		return fmt.Errorf("container_image_id: %w", err)
	}
	if identity.ContainerImageID != identity.LocalImageID {
		return fmt.Errorf("the running Gateway container was created from %s but the formal build produced %s",
			shortImageID(identity.ContainerImageID), shortImageID(identity.LocalImageID))
	}
	if strings.TrimSpace(identity.Platform) == "" {
		return errors.New("platform is empty; the same image ID can be reached on more than one architecture")
	}
	for name, value := range map[string]string{
		"builder_base_image": identity.BuilderBaseImage,
		"runtime_base_image": identity.RuntimeBaseImage,
	} {
		if err := requireImmutableReference(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	expected, err := identity.computeAggregate()
	if err != nil {
		return err
	}
	if identity.AggregateSHA256 != expected {
		return fmt.Errorf("gateway runtime identity aggregate is %s but its members digest to %s",
			shortDigest(identity.AggregateSHA256), shortDigest(expected))
	}
	return nil
}

// Seal fills AggregateSHA256 from the members and validates the result.
func (identity GatewayRuntimeIdentityV1) Seal() (GatewayRuntimeIdentityV1, error) {
	identity.Version = GatewayRuntimeIdentityV1Version
	identity.AggregateSHA256 = ""
	aggregate, err := identity.computeAggregate()
	if err != nil {
		return GatewayRuntimeIdentityV1{}, err
	}
	identity.AggregateSHA256 = aggregate
	if err := identity.Validate(); err != nil {
		return GatewayRuntimeIdentityV1{}, err
	}
	return identity, nil
}

// computeAggregate digests every member in a fixed order with length-prefixed
// framing, so no two different member sets can produce one digest.
func (identity GatewayRuntimeIdentityV1) computeAggregate() (string, error) {
	hash := sha256.New()
	hash.Write([]byte(gatewayRuntimeIdentityDomain))
	hash.Write([]byte{0})
	members := []struct{ name, value string }{
		{"version", GatewayRuntimeIdentityV1Version},
		{"submission_commit", identity.SubmissionCommit},
		{"clean_tree_at_build", fmt.Sprintf("%t", identity.CleanTreeAtBuild)},
		{"build_context_sha256", identity.BuildContextSHA256},
		{"source_manifest_sha256", identity.SourceManifestSHA256},
		{"build_target", identity.BuildTarget},
		{"oci_revision_label", identity.OCIRevisionLabel},
		{"local_image_id", identity.LocalImageID},
		{"container_image_id", identity.ContainerImageID},
		{"gateway_binary_sha256", identity.BinarySHA256},
		{"platform", identity.Platform},
		{"healthcheck_definition_sha256", identity.HealthcheckSHA256},
		{"builder_base_image", identity.BuilderBaseImage},
		{"runtime_base_image", identity.RuntimeBaseImage},
	}
	for _, member := range members {
		if strings.ContainsRune(member.value, 0) {
			return "", fmt.Errorf("gateway runtime identity member %s contains a NUL byte", member.name)
		}
		fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", member.name, len(member.value), member.value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validCommitSHA1(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9', character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}

func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}
