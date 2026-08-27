// final-v5-gateway-build is the formal Gateway build and verification path.
//
// Ordinary developer builds go on using the plain Dockerfile and `make up`; they
// are convenient and nothing depends on their provenance. Qualification and
// publication use this command instead, because they make claims about which
// source produced the running Gateway and the ordinary path cannot support one.
//
//	build                 materialize a tracked-file-only context from the
//	                      published HEAD, build Dockerfile.formal from it, and
//	                      verify the labels on the result; publication callers
//	                      may also create an immutable deployment manifest and
//	                      image-only Compose override
//	verify-build          revalidate that manifest, override, source, optional
//	                      deployment bindings, and local image before startup
//	verify                inspect a running Gateway container through Docker
//	                      Engine and emit its typed runtime identity
//	record-base-images    resolve the base image tags once and write the digest
//	                      pins for review
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/formalbuild"
)

const (
	// moduleProxyEnv names an optional GOPROXY for the builder. It is read from
	// the environment rather than a flag so every launcher that runs this tool
	// (the campaign scripts included) inherits it without a script change.
	moduleProxyEnv = "TASKGATE_FORMAL_BUILD_GOPROXY"
	buildTimeout   = 30 * time.Minute
	verifyTimeout  = 60 * time.Second
	defaultTag     = "taskgate-final-v5-gateway:formal"
)

func main() {
	if err := run(os.Args, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5 gateway build:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout *os.File) error {
	if len(args) < 2 {
		return errors.New("usage: final-v5-gateway-build build|verify-build|verify|record-base-images [flags]")
	}
	switch args[1] {
	case "build":
		return runBuild(args[2:], getenv, stdout)
	case "verify-build":
		return runVerifyBuild(args[2:], getenv, stdout)
	case "verify":
		return runVerify(args[2:], getenv, stdout)
	case "record-base-images":
		return runRecordBaseImages(args[2:], getenv, stdout)
	default:
		return fmt.Errorf("final-v5-gateway-build does not accept %q", args[1])
	}
}

func runBuild(args []string, getenv func(string) string, stdout *os.File) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root to build the formal context from")
	tag := flags.String("tag", defaultTag, "local tag to give the built image")
	manifestOut := flags.String("manifest-out", "", "create the machine-readable build manifest here")
	composeOverrideOut := flags.String("compose-override-out", "", "create the immutable-ID Compose override here")
	datasetBinding := flags.String("dataset-binding", "", "optional validated deployment Dataset Binding file")
	profileRegistry := flags.String("profile-registry", "", "optional source-controlled profile registry file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if (*manifestOut == "") != (*composeOverrideOut == "") {
		return errors.New("build needs both -manifest-out and -compose-override-out, or neither")
	}
	if *manifestOut == "" && (*datasetBinding != "" || *profileRegistry != "") {
		return errors.New("deployment bindings require -manifest-out and -compose-override-out")
	}
	bindingsBefore, err := loadBuildBindings(*datasetBinding, *profileRegistry)
	if err != nil {
		return err
	}
	if *manifestOut != "" {
		if err := requireAbsentOutput(*manifestOut, "formal Gateway build manifest"); err != nil {
			return err
		}
		if err := requireAbsentOutput(*composeOverrideOut, "formal Gateway Compose override"); err != nil {
			return err
		}
	}

	// Materialized before anything else: a dirty tree or an unpublished commit
	// must fail here, not after a twenty-minute build.
	materialized, err := formalbuild.MaterializeHead(*root)
	if err != nil {
		return err
	}
	pins, err := formalbuild.LoadBaseImagePins(filepath.Join(*root, formalbuild.BaseImagePinPath))
	if err != nil {
		return err
	}
	if err := pins.Pinned(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	engine, err := dialEngine(ctx, getenv)
	if err != nil {
		return err
	}
	if err := requirePinnedBasesPresent(ctx, engine, pins); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "formal Gateway build\n")
	fmt.Fprintf(stdout, "  commit        %s (clean, equals origin/%s)\n",
		materialized.Source.Commit[:12], materialized.Source.Branch)
	fmt.Fprintf(stdout, "  context       %s over %d tracked files\n",
		materialized.ContextSHA256[:12], len(materialized.Entries))
	fmt.Fprintf(stdout, "  manifest      %s\n", materialized.SourceManifestSHA256[:12])

	moduleProxy := strings.TrimSpace(getenv(moduleProxyEnv))
	if moduleProxy != "" {
		fmt.Fprintf(stdout, "  module proxy  %s (transport only; go.sum decides the bytes)\n", moduleProxy)
	}
	image, err := formalbuild.Build(ctx, engine, formalbuild.ExecBuilder, formalbuild.BuildRequest{
		Context: materialized, Pins: pins, Tag: *tag, ModuleProxy: moduleProxy,
	})
	if err != nil {
		return err
	}
	if *manifestOut != "" {
		bindingsAfter, err := loadBuildBindings(*datasetBinding, *profileRegistry)
		if err != nil {
			return err
		}
		if bindingsAfter != bindingsBefore {
			return errors.New("deployment binding inputs changed during the formal Gateway build")
		}
		manifest, err := formalbuild.NewBuildManifest(materialized, image, *tag, bindingsAfter)
		if err != nil {
			return err
		}
		manifest.ModuleProxy = moduleProxy
		if err := formalbuild.WriteBuildDeployment(*manifestOut, *composeOverrideOut, manifest); err != nil {
			return err
		}
		manifestSHA, err := formalbuild.ArtifactSHA256(*manifestOut, "formal Gateway build manifest")
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "  build manifest %s (%s)\n", *manifestOut, manifestSHA)
		fmt.Fprintf(stdout, "  compose image  %s\n", image.ID)
	}
	fmt.Fprintf(stdout, "  image         %s (%s)\n", image.ID, image.Platform())
	fmt.Fprintf(stdout, "  tag           %s\n", *tag)
	fmt.Fprintf(stdout, "formal Gateway build: pass\n")
	return nil
}

func runVerifyBuild(args []string, getenv func(string) string, stdout *os.File) error {
	flags := flag.NewFlagSet("verify-build", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root the image must have been built from")
	manifestPath := flags.String("manifest", "", "machine-readable formal build manifest")
	composeOverride := flags.String("compose-override", "", "immutable-ID Compose override")
	datasetBinding := flags.String("dataset-binding", "", "optional validated deployment Dataset Binding file")
	profileRegistry := flags.String("profile-registry", "", "optional source-controlled profile registry file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *manifestPath == "" || *composeOverride == "" {
		return errors.New("verify-build needs -manifest and -compose-override")
	}
	bindings, err := loadBuildBindings(*datasetBinding, *profileRegistry)
	if err != nil {
		return err
	}
	materialized, err := formalbuild.MaterializeHead(*root)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	engine, err := dialEngine(ctx, getenv)
	if err != nil {
		return err
	}
	manifest, err := formalbuild.VerifyBuildDeployment(ctx, engine, *manifestPath, *composeOverride,
		formalbuild.FromContext(materialized), bindings)
	if err != nil {
		return err
	}
	manifestSHA, err := formalbuild.ArtifactSHA256(*manifestPath, "formal Gateway build manifest")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "formal Gateway build deployment: pass\n")
	fmt.Fprintf(stdout, "  image          %s\n", manifest.ImageID)
	fmt.Fprintf(stdout, "  build manifest %s\n", manifestSHA)
	fmt.Fprintf(stdout, "  context        %s\n", manifest.BuildContextSHA256)
	fmt.Fprintf(stdout, "  source manifest %s\n", manifest.SourceManifestSHA256)
	if manifest.DatasetBindingSHA256 != "" {
		fmt.Fprintf(stdout, "  Dataset binding %s\n", manifest.DatasetBindingSHA256)
	}
	if manifest.ProfileRegistrySHA256 != "" {
		fmt.Fprintf(stdout, "  profile registry %s\n", manifest.ProfileRegistrySHA256)
	}
	return nil
}

func runVerify(args []string, getenv func(string) string, stdout *os.File) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root the running Gateway must have been built from")
	project := flags.String("project", "", "Compose project of the running deployment")
	service := flags.String("service", "gateway", "Compose service of the Gateway")
	container := flags.String("container", "", "Gateway container id (overrides -project/-service)")
	out := flags.String("out", "", "write the typed identity JSON here instead of stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *container == "" && *project == "" {
		return errors.New("verify needs either -container or -project")
	}

	materialized, err := formalbuild.MaterializeHead(*root)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	engine, err := dialEngine(ctx, getenv)
	if err != nil {
		return err
	}
	containerID := *container
	if containerID == "" {
		if containerID, err = engine.ResolveService(ctx, *project, *service); err != nil {
			return err
		}
	}

	identity, err := ResolveVerifiedIdentity(ctx, engine, containerID, formalbuild.FromContext(materialized))
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, append(payload, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "formal Gateway identity written to %s\n", *out)
	} else {
		fmt.Fprintf(stdout, "%s\n", payload)
	}
	fmt.Fprintf(stdout, "formal Gateway runtime identity: pass (aggregate %s)\n", identity.AggregateSHA256[:12])
	return nil
}

// ResolveVerifiedIdentity reads the running healthcheck, requires it to be the
// approved definition, and resolves the typed Gateway identity against it.
//
// The healthcheck is resolved here rather than accepted as a parameter so the
// identity cannot be sealed around a probe nobody looked at.
func ResolveVerifiedIdentity(ctx context.Context, engine formalbuild.Engine, containerID string,
	expected formalbuild.ExpectedSource) (experiment.GatewayRuntimeIdentityV1, error) {
	container, err := engine.ContainerInspect(ctx, containerID)
	if err != nil {
		return experiment.GatewayRuntimeIdentityV1{}, err
	}
	healthcheck, err := container.Healthcheck()
	if err != nil {
		return experiment.GatewayRuntimeIdentityV1{}, err
	}
	if err := healthcheck.Validate(); err != nil {
		return experiment.GatewayRuntimeIdentityV1{}, err
	}
	return formalbuild.ResolveGatewayIdentity(ctx, engine, containerID, expected, healthcheck.SHA256())
}

func runRecordBaseImages(args []string, getenv func(string) string, stdout *os.File) error {
	flags := flag.NewFlagSet("record-base-images", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root holding the pin document")
	if err := flags.Parse(args); err != nil {
		return err
	}
	path := filepath.Join(*root, formalbuild.BaseImagePinPath)
	pins, err := formalbuild.LoadBaseImagePins(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	engine, err := dialEngine(ctx, getenv)
	if err != nil {
		return err
	}
	for index, pin := range pins.Images {
		digest, err := formalbuild.ResolveBaseImageDigest(ctx, engine, formalbuild.ExecBuilder, pin.Tag)
		if err != nil {
			return err
		}
		if pin.Digest != "" && pin.Digest != digest {
			// Reported rather than silently rewritten: a moved pin changes which
			// bytes every future formal image is built from, and that is a source
			// change a reviewer must see.
			fmt.Fprintf(stdout, "  %-8s %s\n    was %s\n    now %s  (UPSTREAM RETAG)\n",
				pin.Role, pin.Tag, pin.Digest, digest)
		} else {
			fmt.Fprintf(stdout, "  %-8s %s\n    %s\n", pin.Role, pin.Tag, digest)
		}
		pins.Images[index].Digest = digest
	}
	if err := formalbuild.WriteBaseImagePins(path, pins); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "recorded base image pins in %s; review and commit them\n", path)
	return nil
}

// requirePinnedBasesPresent proves each pinned digest is available locally
// before the build starts.
//
// --pull=false keeps the build from resolving a tag over the network, so a
// missing pin would otherwise surface as an opaque builder error partway in.
func requirePinnedBasesPresent(ctx context.Context, engine formalbuild.Engine, pins formalbuild.BaseImagePins) error {
	for _, pin := range pins.Images {
		reference, err := pin.Reference()
		if err != nil {
			return err
		}
		if _, err := engine.ImageInspect(ctx, reference); err != nil {
			return fmt.Errorf("the pinned %s base image %s is not present locally; pull it before the formal "+
				"build so the build itself never resolves a mutable tag: %w", pin.Role, reference, err)
		}
	}
	return nil
}

func dialEngine(ctx context.Context, getenv func(string) string) (*formalbuild.HTTPEngine, error) {
	socket, err := formalbuild.DockerSocket(getenv)
	if err != nil {
		return nil, err
	}
	return formalbuild.NewHTTPEngine(ctx, socket)
}

func loadBuildBindings(datasetBinding, profileRegistry string) (formalbuild.BuildBindings, error) {
	datasetSHA, err := formalbuild.RegularFileSHA256(datasetBinding, "Dataset Binding")
	if err != nil {
		return formalbuild.BuildBindings{}, err
	}
	registrySHA, err := formalbuild.RegularFileSHA256(profileRegistry, "profile registry")
	if err != nil {
		return formalbuild.BuildBindings{}, err
	}
	return formalbuild.BuildBindings{
		DatasetBindingSHA256:  datasetSHA,
		ProfileRegistrySHA256: registrySHA,
	}, nil
}

func requireAbsentOutput(path, label string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s path is empty", label)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s already exists: %s", label, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s output: %w", label, err)
	}
	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("%s parent directory: %w", label, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s parent is not a directory", label)
	}
	return nil
}
