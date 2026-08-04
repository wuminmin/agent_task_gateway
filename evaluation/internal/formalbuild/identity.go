package formalbuild

import (
	"context"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// The labels Dockerfile.formal stamps onto a formal Gateway image.
const (
	LabelRevision         = "org.opencontainers.image.revision"
	LabelBuildContext     = "taskgate.build_context_sha256"
	LabelSourceManifest   = "taskgate.source_manifest_sha256"
	LabelBuildTarget      = "taskgate.build_target"
	LabelBuilderBaseImage = "taskgate.builder_base_image"
	LabelRuntimeBaseImage = "taskgate.runtime_base_image"
	LabelFormalBuild      = "taskgate.formal_build"
)

// GatewayBuildTarget is the only target a formal Gateway image may be built for.
// The same Dockerfile builds every command in the repository, so without this
// an image running some other cmd/ binary would satisfy every other check.
const GatewayBuildTarget = "gateway"

// Paths the formal image records its own provenance at.
const (
	binaryPath           = "/usr/local/bin/app"
	binaryDigestPath     = "/usr/local/share/taskgate/gateway-binary-sha256"
	sourceCommitPath     = "/usr/local/share/taskgate/source-commit"
	buildContextPath     = "/usr/local/share/taskgate/build-context-sha256"
	sourceManifestPath   = "/usr/local/share/taskgate/source-manifest-sha256"
	buildTargetPathInImg = "/usr/local/share/taskgate/build-target"
)

// ExpectedSource is what the verifier computed for itself from the repository.
//
// None of it is read off the image. That is the point: the image's labels are
// the claim, these are the independent computation, and the verification is the
// comparison. A verifier that took the expected digest from the label it is
// checking would accept any self-consistent image.
type ExpectedSource struct {
	Commit               string
	ContextSHA256        string
	SourceManifestSHA256 string
	CleanTree            bool
}

// FromContext derives the expectation from a materialized build context.
func FromContext(materialized Context) ExpectedSource {
	return ExpectedSource{
		Commit:               materialized.Source.Commit,
		ContextSHA256:        materialized.ContextSHA256,
		SourceManifestSHA256: materialized.SourceManifestSHA256,
		CleanTree:            materialized.Source.CleanTree,
	}
}

// ResolveGatewayIdentity inspects the running Gateway through Docker Engine and
// returns its typed, sealed identity.
//
// Every binding required before a snapshot may be emitted is checked here, and
// each failure names the specific thing that did not match rather than reporting
// that an aggregate digest differed. An operator who is told "the revision label
// is abc but the build names def" can act; one told "identity mismatch" cannot.
func ResolveGatewayIdentity(ctx context.Context, engine Engine, containerID string,
	expected ExpectedSource, healthcheckSHA256 string) (experiment.GatewayRuntimeIdentityV1, error) {
	var identity experiment.GatewayRuntimeIdentityV1
	if !validCommitSHA1(expected.Commit) {
		return identity, fmt.Errorf("the expected submission commit %q is not a full object name", expected.Commit)
	}
	if !validSHA256(expected.ContextSHA256) || !validSHA256(expected.SourceManifestSHA256) {
		return identity, fmt.Errorf("the expected build-context and source-manifest digests must both be computed "+
			"before the running Gateway can be verified against commit %s", shortCommit(expected.Commit))
	}
	if !expected.CleanTree {
		return identity, fmt.Errorf("commit %s was materialized from a dirty worktree; a formal Gateway image "+
			"cannot be verified against it", shortCommit(expected.Commit))
	}
	if !validSHA256(healthcheckSHA256) {
		return identity, fmt.Errorf("the resolved formal healthcheck digest %q is not a lowercase SHA-256",
			healthcheckSHA256)
	}

	container, err := engine.ContainerInspect(ctx, containerID)
	if err != nil {
		return identity, err
	}
	if !container.State.Running {
		return identity, fmt.Errorf("the Gateway container %s is not running", shortImageID(containerID))
	}
	if err := requireImageID(container.Image); err != nil {
		return identity, fmt.Errorf("the Gateway container reports image %q: %w", container.Image, err)
	}

	// Inspected by ID, not by tag. A tag can be moved between the build and the
	// measurement, so resolving one here would verify a different image than the
	// container is running.
	image, err := engine.ImageInspect(ctx, container.Image)
	if err != nil {
		return identity, err
	}
	if image.ID != container.Image {
		return identity, fmt.Errorf("the Gateway container was created from %s but that reference inspects to %s",
			shortImageID(container.Image), shortImageID(image.ID))
	}

	if _, present := image.Label(LabelFormalBuild); !present {
		return identity, fmt.Errorf("the running Gateway image %s carries no %s label; it was not produced by the "+
			"formal build path and its provenance is the ordinary Dockerfile's COPY . .",
			shortImageID(image.ID), LabelFormalBuild)
	}
	labels := map[string]string{}
	for _, name := range []string{
		LabelRevision, LabelBuildContext, LabelSourceManifest,
		LabelBuildTarget, LabelBuilderBaseImage, LabelRuntimeBaseImage,
	} {
		value, present := image.Label(name)
		if !present {
			return identity, fmt.Errorf("the running Gateway image %s omits the %s label",
				shortImageID(image.ID), name)
		}
		if strings.TrimSpace(value) == "" {
			return identity, fmt.Errorf("the running Gateway image %s carries an empty %s label; the formal build "+
				"was invoked without its build argument", shortImageID(image.ID), name)
		}
		labels[name] = value
	}

	if labels[LabelRevision] != expected.Commit {
		return identity, fmt.Errorf("the running Gateway image names revision %s but the verified source is %s",
			shortCommit(labels[LabelRevision]), shortCommit(expected.Commit))
	}
	if labels[LabelBuildContext] != expected.ContextSHA256 {
		return identity, fmt.Errorf("the running Gateway image names build context %s but commit %s materializes "+
			"to %s; the image was not built from this source",
			shortDigest(labels[LabelBuildContext]), shortCommit(expected.Commit), shortDigest(expected.ContextSHA256))
	}
	if labels[LabelSourceManifest] != expected.SourceManifestSHA256 {
		return identity, fmt.Errorf("the running Gateway image names source manifest %s but commit %s manifests "+
			"to %s", shortDigest(labels[LabelSourceManifest]), shortCommit(expected.Commit),
			shortDigest(expected.SourceManifestSHA256))
	}
	if labels[LabelBuildTarget] != GatewayBuildTarget {
		return identity, fmt.Errorf("the running Gateway image was built for target %q, not %q",
			labels[LabelBuildTarget], GatewayBuildTarget)
	}

	// The in-image provenance is read back and required to agree with the
	// labels. Labels are metadata a retag can rewrite; these files are content
	// inside the layer the binary lives in.
	recorded, err := readImageProvenance(ctx, engine, containerID)
	if err != nil {
		return identity, err
	}
	for _, check := range []struct{ name, fromImage, fromLabel string }{
		{"submission commit", recorded[sourceCommitPath], labels[LabelRevision]},
		{"build context digest", recorded[buildContextPath], labels[LabelBuildContext]},
		{"source manifest digest", recorded[sourceManifestPath], labels[LabelSourceManifest]},
		{"build target", recorded[buildTargetPathInImg], labels[LabelBuildTarget]},
	} {
		if check.fromImage != check.fromLabel {
			return identity, fmt.Errorf("the running Gateway records %s %q inside the image but its label says %q; "+
				"the image was relabelled after it was built", check.name, check.fromImage, check.fromLabel)
		}
	}

	binaryDigest, err := gatewayBinaryDigest(ctx, engine, containerID)
	if err != nil {
		return identity, err
	}
	if recorded[binaryDigestPath] != binaryDigest {
		return identity, fmt.Errorf("the Gateway binary digests to %s but the image recorded %s at build time; "+
			"the executable was replaced after the image was built",
			shortDigest(binaryDigest), shortDigest(recorded[binaryDigestPath]))
	}

	for _, base := range []struct{ name, reference string }{
		{"builder", labels[LabelBuilderBaseImage]},
		{"runtime", labels[LabelRuntimeBaseImage]},
	} {
		if !validDigestReference(referenceDigest(base.reference)) {
			return identity, fmt.Errorf("the running Gateway image names a %s base image %q that is not pinned by "+
				"digest; the image is only as reproducible as a mutable upstream tag",
				base.name, base.reference)
		}
	}

	sealed, err := experiment.GatewayRuntimeIdentityV1{
		SubmissionCommit:     expected.Commit,
		CleanTreeAtBuild:     true,
		BuildContextSHA256:   expected.ContextSHA256,
		SourceManifestSHA256: expected.SourceManifestSHA256,
		BuildTarget:          GatewayBuildTarget,
		OCIRevisionLabel:     labels[LabelRevision],
		LocalImageID:         image.ID,
		ContainerImageID:     container.Image,
		BinarySHA256:         binaryDigest,
		Platform:             image.Platform(),
		HealthcheckSHA256:    healthcheckSHA256,
		BuilderBaseImage:     labels[LabelBuilderBaseImage],
		RuntimeBaseImage:     labels[LabelRuntimeBaseImage],
	}.Seal()
	if err != nil {
		return identity, fmt.Errorf("seal Gateway runtime identity: %w", err)
	}
	return sealed, nil
}

// ResolvePostgreSQLIdentity inspects a running PostgreSQL container and returns
// its immutable identity.
//
// The observer resolves this for itself rather than accepting it from the
// Adapter or the environment. A runtime identity supplied by the party being
// measured is not evidence about the deployment; it is a claim about it.
func ResolvePostgreSQLIdentity(ctx context.Context, engine Engine,
	containerID string) (experiment.PostgreSQLRuntimeIdentity, error) {
	var identity experiment.PostgreSQLRuntimeIdentity
	container, err := engine.ContainerInspect(ctx, containerID)
	if err != nil {
		return identity, err
	}
	if !container.State.Running {
		return identity, fmt.Errorf("the PostgreSQL container %s is not running", shortImageID(containerID))
	}
	image, err := engine.ImageInspect(ctx, container.Image)
	if err != nil {
		return identity, err
	}
	repoDigest, err := soleRepoDigest(image, container.Config.Image)
	if err != nil {
		return identity, err
	}
	identity = experiment.PostgreSQLRuntimeIdentity{
		ImageReference:   container.Config.Image,
		RepoDigest:       repoDigest,
		LocalImageID:     image.ID,
		ContainerImageID: container.Image,
		Platform:         image.Platform(),
	}
	if err := identity.Validate(); err != nil {
		return experiment.PostgreSQLRuntimeIdentity{}, fmt.Errorf("PostgreSQL runtime identity: %w", err)
	}
	return identity, nil
}

// soleRepoDigest returns the one registry digest for the repository the
// deployment named.
//
// Exactly one, never the first of several: an image carrying two digests for one
// repository cannot say which bytes the deployment is running, and picking one
// would make the evidence depend on map ordering.
func soleRepoDigest(image ImageInspect, reference string) (string, error) {
	repository, _, found := strings.Cut(reference, "@")
	if !found {
		repository, _, _ = strings.Cut(reference, ":")
	}
	if repository == "" {
		return "", fmt.Errorf("the deployment names image %q, which has no repository", reference)
	}
	var digests []string
	for _, repoDigest := range image.RepoDigests {
		name, digest, split := strings.Cut(repoDigest, "@")
		if !split || name != repository {
			continue
		}
		digests = append(digests, repository+"@"+digest)
	}
	switch len(digests) {
	case 1:
		return digests[0], nil
	case 0:
		return "", fmt.Errorf("image %s carries no registry digest for %q; it was built or loaded rather than "+
			"pulled, so there is nothing immutable to bind", shortImageID(image.ID), repository)
	default:
		return "", fmt.Errorf("image %s carries %d registry digests for %q; the running server is ambiguous",
			shortImageID(image.ID), len(digests), repository)
	}
}

// referenceDigest extracts the digest half of a repository@digest reference.
func referenceDigest(reference string) string {
	_, digest, found := strings.Cut(reference, "@")
	if !found {
		return ""
	}
	return digest
}

// readImageProvenance reads the provenance files the formal build wrote.
//
// One exec reads all of them with an explicit marker per file, so a missing file
// is a distinguishable failure rather than a short read that happens to parse.
func readImageProvenance(ctx context.Context, engine Engine, containerID string) (map[string]string, error) {
	paths := []string{sourceCommitPath, buildContextPath, sourceManifestPath, buildTargetPathInImg, binaryDigestPath}
	var script strings.Builder
	script.WriteString("set -eu\n")
	for _, path := range paths {
		fmt.Fprintf(&script, "printf 'TASKGATE_FILE %s\\n'\ncat %s\n", path, path)
	}
	raw, err := engine.Exec(ctx, containerID, []string{"sh", "-c", script.String()})
	if err != nil {
		return nil, fmt.Errorf("read formal build provenance from the running Gateway "+
			"(an image without it was not produced by the formal build path): %w", err)
	}
	values := map[string]string{}
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for index, path := range paths {
		marker := "TASKGATE_FILE " + path + "\n"
		start := strings.Index(normalized, marker)
		if start < 0 || strings.Count(normalized, marker) != 1 {
			return nil, fmt.Errorf("the running Gateway did not report %s exactly once", path)
		}
		start += len(marker)
		end := len(normalized)
		if index+1 < len(paths) {
			next := strings.Index(normalized[start:], "TASKGATE_FILE "+paths[index+1]+"\n")
			if next < 0 {
				return nil, fmt.Errorf("the running Gateway reported %s out of order", paths[index+1])
			}
			end = start + next
		}
		value := strings.TrimSpace(normalized[start:end])
		if value == "" {
			return nil, fmt.Errorf("the running Gateway reported an empty %s", path)
		}
		values[path] = value
	}
	return values, nil
}

// gatewayBinaryDigest digests the executable the container is actually running.
func gatewayBinaryDigest(ctx context.Context, engine Engine, containerID string) (string, error) {
	raw, err := engine.Exec(ctx, containerID,
		[]string{"sh", "-c", "set -eu\nsha256sum " + binaryPath + " | cut -d' ' -f1"})
	if err != nil {
		return "", fmt.Errorf("digest the running Gateway binary: %w", err)
	}
	digest := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if !validSHA256(digest) {
		return "", fmt.Errorf("the running Gateway binary digest %q is not a lowercase SHA-256", digest)
	}
	return digest, nil
}
