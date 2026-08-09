package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const (
	testSubmissionCommit = "0123456789abcdef0123456789abcdef01234567"
	testBusinessDSN      = "postgres://gateway_reader:super-secret@127.0.0.1:5432/business?sslmode=disable"
)

var testFrozenArtifactScales = []string{
	"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16",
}

func testArtifactTargetedBinding(selected []string) experiment.ArtifactTargetedDeploymentBinding {
	digest := strings.Repeat("a", 64)
	binding := experiment.ArtifactTargetedDeploymentBinding{
		SchemaVersion:         1,
		Record:                experiment.ArtifactTargetedDeploymentBindingVersion,
		SubmissionCommit:      testSubmissionCommit,
		ContractRelease:       "final-v5-contracts-v1.4",
		ContractIndexSHA256:   digest,
		ProfileRegistrySHA256: digest,
		Profile: experiment.ArtifactTargetedProfileBinding{
			ProfileID:             "profile-aaaaaaaaaaaaaaaa",
			ProfileAlias:          "result-heavy",
			ClosureSHA256:         digest,
			CatalogSHA256:         digest,
			PublicationIdentity:   digest,
			ActivationSupported:   true,
			ActivationSmokePassed: true,
			TargetedRunEligible:   true,
		},
		SelectedScales:                 append([]string(nil), selected...),
		DatasetProbeSQLSHA256:          digest,
		DatasetProbeSHA256:             strings.Repeat("b", 64),
		AttestationQualificationSHA256: digest,
		PostgreSQLIdentitySHA256:       digest,
	}
	for index, scale := range testFrozenArtifactScales {
		rows := []int64{100, 10_000, 100_000, 100, 10_000, 100_000}[index]
		columns := []int{4, 4, 4, 16, 16, 16}[index]
		binding.ArtifactCells = append(binding.ArtifactCells, experiment.ArtifactTargetedCellBinding{
			Cell: finalv5contracts.CellIdentity{
				ExperimentID: finalv5contracts.ArtifactExperimentID,
				WorkloadID:   finalv5contracts.ArtifactWorkloadID,
				Scale:        scale,
				Mode:         "novel",
			},
			SpecID:                 fmt.Sprintf("artifact-spec-%d", index),
			BaselineIdentity:       "baseline/S6/" + scale + "/novel",
			ProductID:              "product-result-heavy",
			PublicationID:          "publication-result-heavy",
			Rows:                   rows,
			Columns:                columns,
			BDGTemplateSHA256:      digest,
			BDGSQLSHA256:           digest,
			DirectTemplateSHA256:   digest,
			DirectSQLSHA256:        digest,
			NormalizationSHA256:    digest,
			OracleManifestPath:     fmt.Sprintf("oracle-manifests/artifact/result-heavy/%s/novel.json", scale),
			OracleManifestSHA256:   digest,
			NormalizedSchemaSHA256: digest,
			CanonicalResultSHA256:  digest,
		})
	}
	return binding
}

func testCLIArgs(out string) []string {
	return []string{
		"--registry", "config/profiles/registry.json",
		"--profile-alias", "result-heavy",
		"--catalog", "config/profiles/result-heavy.catalog.yaml",
		"--selected-scales", "100x4,100k-x16",
		"--attestation-qualification", "retained/attestation-footprint-v2.json",
		"--postgresql-identity", "retained/postgresql-identity.json",
		"--out", out,
	}
}

func testEnvironment(overrides map[string]string) func(string) string {
	values := map[string]string{
		submissionCommitEnv: testSubmissionCommit,
		businessDSNEnv:      testBusinessDSN,
	}
	for name, value := range overrides {
		values[name] = value
	}
	return func(name string) string { return values[name] }
}

func injectArtifactTargetedBuilder(t *testing.T,
	builder func(context.Context, experiment.ArtifactTargetedBindingInput) (
		experiment.ArtifactTargetedDeploymentBinding, error)) {
	t.Helper()
	previous := buildArtifactTargetedDeploymentBinding
	buildArtifactTargetedDeploymentBinding = builder
	t.Cleanup(func() { buildArtifactTargetedDeploymentBinding = previous })
}

func withoutCLIFlag(args []string, name string) []string {
	result := make([]string, 0, len(args)-2)
	for index := 0; index < len(args); index++ {
		if args[index] == name {
			index++
			continue
		}
		result = append(result, args[index])
	}
	return result
}

func TestRunBuildsWritesReopensAndReports(t *testing.T) {
	selected := []string{"100x4", "100k-x16"}
	wantBinding := testArtifactTargetedBinding(selected)
	var captured experiment.ArtifactTargetedBindingInput
	buildCalls := 0
	injectArtifactTargetedBuilder(t, func(_ context.Context,
		input experiment.ArtifactTargetedBindingInput) (experiment.ArtifactTargetedDeploymentBinding, error) {
		buildCalls++
		captured = input
		return wantBinding, nil
	})

	out := filepath.Join(t.TempDir(), "artifact-targeted-binding.json")
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), testCLIArgs(out), testEnvironment(nil), &stdout, &stderr); err != nil {
		t.Fatalf("run command: %v\nstderr: %s", err, stderr.String())
	}
	if buildCalls != 1 {
		t.Fatalf("builder called %d times, want 1", buildCalls)
	}
	wantInput := experiment.ArtifactTargetedBindingInput{
		SubmissionCommit:       testSubmissionCommit,
		ProfileRegistryPath:    "config/profiles/registry.json",
		ProfileAlias:           "result-heavy",
		CatalogPath:            "config/profiles/result-heavy.catalog.yaml",
		BusinessDSN:            testBusinessDSN,
		SelectedScales:         selected,
		QualificationPath:      "retained/attestation-footprint-v2.json",
		PostgreSQLIdentityPath: "retained/postgresql-identity.json",
	}
	if !reflect.DeepEqual(captured, wantInput) {
		t.Fatalf("builder input = %+v, want %+v", captured, wantInput)
	}
	if stderr.Len() != 0 {
		t.Fatalf("successful command wrote stderr: %s", stderr.String())
	}

	info, err := os.Lstat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("output mode = %v, want regular non-symlink 0600", info.Mode())
	}
	payload, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := experiment.CanonicalArtifactTargetedDeploymentBinding(wantBinding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, canonical) {
		t.Fatal("command output is not the core's exact canonical binding")
	}
	if _, err := experiment.DecodeArtifactTargetedDeploymentBinding(payload); err != nil {
		t.Fatalf("reopen canonical output: %v", err)
	}
	if bytes.Contains(payload, []byte(testBusinessDSN)) || bytes.Contains(payload, []byte("super-secret")) {
		t.Fatal("credential-free binding contains its input DSN")
	}

	var report experiment.ArtifactTargetedBindingValidation
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not one validation report: %v\n%s", err, stdout.String())
	}
	digest := sha256.Sum256(payload)
	wantReport := experiment.ArtifactTargetedBindingValidation{
		SchemaVersion:      1,
		Status:             "valid",
		ArtifactCells:      6,
		SelectedCells:      len(selected),
		DatasetProbeSHA256: wantBinding.DatasetProbeSHA256,
		BindingFileSHA256:  fmt.Sprintf("%x", digest),
	}
	if report != wantReport {
		t.Fatalf("validation report = %+v, want %+v", report, wantReport)
	}
	reportJSON, err := json.Marshal(wantReport)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), string(reportJSON)+"\n"; got != want {
		t.Fatalf("stdout = %q, want exactly one report %q", got, want)
	}
}

func TestRunRejectsMissingOrUnexpectedArgumentsAndEnvironment(t *testing.T) {
	for _, test := range []struct {
		name       string
		removeFlag string
		appendArgs []string
		env        map[string]string
		wantError  string
	}{
		{name: "registry", removeFlag: "--registry", wantError: "--registry"},
		{name: "profile alias", removeFlag: "--profile-alias", wantError: "--profile-alias"},
		{name: "Catalog", removeFlag: "--catalog", wantError: "--catalog"},
		{name: "selected scales", removeFlag: "--selected-scales", wantError: "--selected-scales"},
		{name: "qualification", removeFlag: "--attestation-qualification", wantError: "--attestation-qualification"},
		{name: "PostgreSQL identity", removeFlag: "--postgresql-identity", wantError: "--postgresql-identity"},
		{name: "output", removeFlag: "--out", wantError: "--out"},
		{name: "positional", appendArgs: []string{"unexpected"}, wantError: "positional"},
		{name: "unknown flag", appendArgs: []string{"--unexpected"}, wantError: "flag provided but not defined"},
		{name: "submission commit env", env: map[string]string{submissionCommitEnv: ""}, wantError: submissionCommitEnv},
		{name: "Business DSN env", env: map[string]string{businessDSNEnv: ""}, wantError: businessDSNEnv},
	} {
		t.Run(test.name, func(t *testing.T) {
			buildCalls := 0
			injectArtifactTargetedBuilder(t, func(context.Context,
				experiment.ArtifactTargetedBindingInput) (experiment.ArtifactTargetedDeploymentBinding, error) {
				buildCalls++
				return experiment.ArtifactTargetedDeploymentBinding{}, errors.New("builder must not be called")
			})

			out := filepath.Join(t.TempDir(), "artifact-targeted-binding.json")
			args := testCLIArgs(out)
			if test.removeFlag != "" {
				args = withoutCLIFlag(args, test.removeFlag)
			}
			args = append(args, test.appendArgs...)
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), args, testEnvironment(test.env), &stdout, &stderr)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want one containing %q", err, test.wantError)
			}
			if buildCalls != 0 {
				t.Fatalf("invalid invocation called builder %d times", buildCalls)
			}
			if stdout.Len() != 0 {
				t.Fatalf("invalid invocation wrote stdout: %s", stdout.String())
			}
			if _, statErr := os.Lstat(out); !os.IsNotExist(statErr) {
				t.Fatalf("invalid invocation left output behind: %v", statErr)
			}
		})
	}
}

func TestRunRefusesExistingAndSymlinkOutput(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*testing.T, string) string
	}{
		{
			name: "regular file",
			configure: func(t *testing.T, directory string) string {
				path := filepath.Join(directory, "artifact-targeted-binding.json")
				if err := os.WriteFile(path, []byte("unchanged\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "symlink",
			configure: func(t *testing.T, directory string) string {
				target := filepath.Join(directory, "target.json")
				if err := os.WriteFile(target, []byte("unchanged\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(directory, "artifact-targeted-binding.json")
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := testArtifactTargetedBinding([]string{"100x4", "100k-x16"})
			buildCalls := 0
			injectArtifactTargetedBuilder(t, func(context.Context,
				experiment.ArtifactTargetedBindingInput) (experiment.ArtifactTargetedDeploymentBinding, error) {
				buildCalls++
				return binding, nil
			})

			directory := t.TempDir()
			out := test.configure(t, directory)
			var stdout bytes.Buffer
			err := run(context.Background(), testCLIArgs(out), testEnvironment(nil), &stdout, io.Discard)
			if err == nil {
				t.Fatal("command replaced an existing output")
			}
			if buildCalls != 1 {
				t.Fatalf("Build -> Write ordering called builder %d times, want 1", buildCalls)
			}
			if stdout.Len() != 0 {
				t.Fatalf("refused output emitted a validation report: %s", stdout.String())
			}
			info, statErr := os.Lstat(out)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if test.name == "symlink" && info.Mode()&os.ModeSymlink == 0 {
				t.Fatal("refused output replaced the symlink")
			}
			payloadPath := out
			if info.Mode()&os.ModeSymlink != 0 {
				payloadPath, statErr = filepath.EvalSymlinks(out)
				if statErr != nil {
					t.Fatal(statErr)
				}
			}
			payload, readErr := os.ReadFile(payloadPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(payload) != "unchanged\n" {
				t.Fatal("refused output changed existing bytes")
			}
		})
	}
}

func TestRunNeverReturnsBuilderDSNOrPassword(t *testing.T) {
	injectArtifactTargetedBuilder(t, func(_ context.Context,
		input experiment.ArtifactTargetedBindingInput) (experiment.ArtifactTargetedDeploymentBinding, error) {
		return experiment.ArtifactTargetedDeploymentBinding{},
			fmt.Errorf("dial failed for %s with password super-secret", input.BusinessDSN)
	})

	out := filepath.Join(t.TempDir(), "artifact-targeted-binding.json")
	var stdout bytes.Buffer
	err := run(context.Background(), testCLIArgs(out), testEnvironment(nil), &stdout, io.Discard)
	if err == nil {
		t.Fatal("builder failure was accepted")
	}
	for _, secret := range []string{testBusinessDSN, "super-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("command error leaked %q: %v", secret, err)
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("builder failure wrote stdout: %s", stdout.String())
	}
	if _, statErr := os.Lstat(out); !os.IsNotExist(statErr) {
		t.Fatalf("builder failure left output behind: %v", statErr)
	}
}
