// Command final-v5-artifact-targeted-binding creates the credential-free
// deployment binding used by the non-publication Artifact targeted runner.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const (
	submissionCommitEnv = "TASKGATE_SUBMISSION_COMMIT"
	businessDSNEnv      = "TASKGATE_FINAL_V5_BUSINESS_DSN"
)

// The builder is injected by command-package tests so they can exercise the
// complete Build -> create-exclusive Write -> reopen Validate orchestration
// without connecting to a live Business PostgreSQL deployment.
var buildArtifactTargetedDeploymentBinding = experiment.BuildArtifactTargetedDeploymentBinding

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-artifact-targeted-binding:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string,
	stdout, stderr io.Writer) error {
	if ctx == nil {
		return errors.New("Artifact-targeted binding command requires a context")
	}
	if getenv == nil {
		return errors.New("Artifact-targeted binding command requires an environment reader")
	}
	if stdout == nil {
		return errors.New("Artifact-targeted binding command requires a report writer")
	}
	if stderr == nil {
		stderr = io.Discard
	}

	flags := flag.NewFlagSet("final-v5-artifact-targeted-binding", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registry := flags.String("registry", "", "source-controlled profile registry")
	profileAlias := flags.String("profile-alias", "", "cleared Result-heavy profile alias")
	catalog := flags.String("catalog", "", "source-controlled Result-heavy profile Catalog")
	selectedScales := flags.String("selected-scales", "", "comma-separated frozen Artifact scale subset")
	qualification := flags.String("attestation-qualification", "", "retained attestation qualification document")
	postgresqlIdentity := flags.String("postgresql-identity", "", "retained PostgreSQL identity document")
	out := flags.String("out", "", "create-exclusive Artifact-targeted binding output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}

	for _, required := range []struct {
		name  string
		value string
	}{
		{"registry", *registry},
		{"profile-alias", *profileAlias},
		{"catalog", *catalog},
		{"selected-scales", *selectedScales},
		{"attestation-qualification", *qualification},
		{"postgresql-identity", *postgresqlIdentity},
		{"out", *out},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("--%s is required", required.name)
		}
	}

	submissionCommit := strings.TrimSpace(getenv(submissionCommitEnv))
	if submissionCommit == "" {
		return fmt.Errorf("%s is required", submissionCommitEnv)
	}
	businessDSN := strings.TrimSpace(getenv(businessDSNEnv))
	if businessDSN == "" {
		return fmt.Errorf("%s is required", businessDSNEnv)
	}

	input := experiment.ArtifactTargetedBindingInput{
		SubmissionCommit:       submissionCommit,
		ProfileRegistryPath:    strings.TrimSpace(*registry),
		ProfileAlias:           strings.TrimSpace(*profileAlias),
		CatalogPath:            strings.TrimSpace(*catalog),
		BusinessDSN:            businessDSN,
		SelectedScales:         strings.Split(*selectedScales, ","),
		QualificationPath:      strings.TrimSpace(*qualification),
		PostgreSQLIdentityPath: strings.TrimSpace(*postgresqlIdentity),
	}
	binding, err := buildArtifactTargetedDeploymentBinding(ctx, input)
	if err != nil {
		// A database driver error can contain its connection string. Do not wrap
		// the builder error: stderr must never become another copy of a DSN or
		// password. Detailed source validation remains covered by the core tests.
		return errors.New("build Artifact-targeted deployment binding failed")
	}

	outputPath := strings.TrimSpace(*out)
	if err := experiment.WriteArtifactTargetedDeploymentBinding(outputPath, binding); err != nil {
		return err
	}
	report, err := experiment.ValidateArtifactTargetedDeploymentBindingFile(outputPath, binding)
	if err != nil {
		return fmt.Errorf("reopen and validate Artifact-targeted deployment binding: %w", err)
	}
	encoder := jsonEncoder(stdout)
	return encoder.Encode(report)
}

// jsonEncoder is kept small so successful stdout has one purpose and one
// shape: exactly one validation report JSON value followed by a newline.
func jsonEncoder(output io.Writer) *json.Encoder {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder
}
