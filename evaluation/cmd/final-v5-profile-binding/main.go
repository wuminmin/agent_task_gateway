// Command final-v5-profile-binding resolves one cleared deployment profile into
// the exact ProfileBinding the experiment Runner sends to every Adapter
// operation.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-profile-binding:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("final-v5-profile-binding", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registry := flags.String("registry", "", "source-controlled profile registry")
	alias := flags.String("alias", "", "registered profile alias")
	datasetBindingSHA256 := flags.String("dataset-binding-sha256", "", "SHA-256 of the deployment Dataset Binding")
	rq5CatalogFamily := flags.String("rq5-catalog-family", "", "source-controlled RQ5 dynamic Catalog family descriptor")
	rq5BuildManifest := flags.String("rq5-build-manifest", "", "sealed RQ5 source build manifest")
	rq5BuildManifestSHA256 := flags.String("rq5-build-manifest-sha256", "", "expected SHA-256 of the RQ5 source build manifest")
	submissionCommit := flags.String("submission-commit", "", "fixed full submission commit for RQ5 identity")
	rq5GeneratorSHA256 := flags.String("rq5-generator-sha256", "", "generator SHA-256 independently read from the sealed manifest")
	rq5ConfigSHA256 := flags.String("rq5-config-sha256", "", "config SHA-256 independently read from the sealed manifest")
	out := flags.String("out", "", "create-exclusive ProfileBinding JSON output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	if strings.TrimSpace(*registry) == "" || strings.TrimSpace(*alias) == "" ||
		strings.TrimSpace(*datasetBindingSHA256) == "" || strings.TrimSpace(*out) == "" {
		return errors.New("registry, alias, dataset-binding-sha256 and out are required")
	}

	var binding *experiment.ProfileBinding
	var err error
	if strings.TrimSpace(*rq5CatalogFamily) == "" {
		for name, value := range map[string]string{"rq5-build-manifest": *rq5BuildManifest,
			"rq5-build-manifest-sha256": *rq5BuildManifestSHA256, "submission-commit": *submissionCommit,
			"rq5-generator-sha256": *rq5GeneratorSHA256, "rq5-config-sha256": *rq5ConfigSHA256} {
			if strings.TrimSpace(value) != "" {
				return fmt.Errorf("%s requires rq5-catalog-family", name)
			}
		}
		binding, err = experiment.ResolveProfileBinding(*registry, *alias, *datasetBindingSHA256)
	} else {
		for name, value := range map[string]string{"rq5-build-manifest": *rq5BuildManifest,
			"rq5-build-manifest-sha256": *rq5BuildManifestSHA256, "submission-commit": *submissionCommit,
			"rq5-generator-sha256": *rq5GeneratorSHA256, "rq5-config-sha256": *rq5ConfigSHA256} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required with rq5-catalog-family", name)
			}
		}
		var resolved experiment.RQ5ProfileBindingResolution
		resolved, err = experiment.ResolveRQ5ProfileBinding(experiment.RQ5ProfileBindingRequest{
			RegistryPath: *registry, ProfileAlias: *alias, DatasetBindingSHA256: *datasetBindingSHA256,
			CatalogFamilyPath: *rq5CatalogFamily, BuildManifestPath: *rq5BuildManifest,
			BuildManifestSHA256: *rq5BuildManifestSHA256, SubmissionCommit: *submissionCommit,
			GeneratorSHA256: *rq5GeneratorSHA256, ConfigSHA256: *rq5ConfigSHA256})
		binding = resolved.Binding
	}
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return fmt.Errorf("encode profile binding: %w", err)
	}
	return writeExclusive(strings.TrimSpace(*out), append(payload, '\n'))
}

func writeExclusive(path string, payload []byte) error {
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create profile binding output: %w", err)
	}
	if _, err := output.Write(payload); err != nil {
		_ = output.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write profile binding output: %w", err)
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close profile binding output: %w", err)
	}
	return nil
}
