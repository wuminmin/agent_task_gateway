package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	contractfs "taskbound.local/agent-data-gateway/evaluation/final-v5-wsl2"
	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
)

const (
	artifactTargetedTestCommit = "0123456789abcdef0123456789abcdef01234567"
	artifactTargetedTestDSN    = "postgres://binding-user:DSN-SECRET-MUST-NOT-APPEAR@127.0.0.1:65535/business?sslmode=disable"
	artifactTargetedRawProbe   = "RAW-PROBE-RESULT-MUST-NOT-APPEAR"
)

type artifactTargetedBindingFixture struct {
	input        ArtifactTargetedBindingInput
	deps         artifactTargetedBindingDependencies
	datasetCalls int
	probeCalls   int
}

func newArtifactTargetedBindingFixture(t *testing.T) *artifactTargetedBindingFixture {
	t.Helper()
	root := repositoryRootForDeployment(t)
	qualificationRoot := artifactTargetedQualificationDirectory(t)
	temporary := t.TempDir()

	copySource := func(name, source string) string {
		t.Helper()
		payload, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read real %s fixture: %v", name, err)
		}
		destination := filepath.Join(temporary, filepath.Base(source))
		if err := os.WriteFile(destination, payload, 0o600); err != nil {
			t.Fatalf("copy real %s fixture: %v", name, err)
		}
		return destination
	}

	fixture := &artifactTargetedBindingFixture{}
	fixture.input = ArtifactTargetedBindingInput{
		SubmissionCommit:       artifactTargetedTestCommit,
		ProfileRegistryPath:    copySource("profile registry", filepath.Join(root, "config", "profiles", "registry.json")),
		ProfileAlias:           artifactDeploymentProfileAlias,
		CatalogPath:            copySource("profile Catalog", filepath.Join(root, "config", "profiles", "result-heavy.catalog.yaml")),
		BusinessDSN:            artifactTargetedTestDSN,
		SelectedScales:         []string{"100k-x16", "100x4", "10k-x16"},
		QualificationPath:      copySource("qualification", filepath.Join(qualificationRoot, "attestation-footprint-v2.json")),
		PostgreSQLIdentityPath: copySource("PostgreSQL identity", filepath.Join(qualificationRoot, "postgresql-identity.json")),
	}
	fixture.deps = artifactTargetedBindingDependencies{
		loadRuntime: finalv5contracts.LoadRuntime,
		dataset: func(ctx context.Context, dsn string) (finalv5dataset.BenchmarkAgreement, error) {
			fixture.datasetCalls++
			if ctx == nil {
				t.Fatal("fake Dataset verifier received a nil context")
			}
			if dsn != artifactTargetedTestDSN {
				t.Fatal("fake Dataset verifier received a different DSN")
			}
			return artifactTargetedTestDatasetAgreement(t)
		},
		probe: func(ctx context.Context, runtime *finalv5contracts.Runtime, dsn string) (string, error) {
			fixture.probeCalls++
			if ctx == nil {
				t.Fatal("fake probe received a nil context")
			}
			if runtime == nil {
				t.Fatal("fake probe received no verified runtime")
			}
			if dsn != artifactTargetedTestDSN {
				t.Fatalf("fake probe received a different DSN")
			}
			probeSQL, err := runtime.DatasetProbeSQL()
			if err != nil {
				t.Fatalf("load the executable dataset probe: %v", err)
			}
			if strings.Contains(probeSQL, `\set`) {
				t.Fatal("the fake probe received a psql metacommand instead of executable SQL")
			}
			return sha256String(artifactTargetedRawProbe), nil
		},
	}
	return fixture
}

func artifactTargetedTestDatasetAgreement(t *testing.T) (finalv5dataset.BenchmarkAgreement, error) {
	t.Helper()
	reference, err := finalv5oracle.BenchmarkDatasetFingerprint()
	if err != nil {
		return finalv5dataset.BenchmarkAgreement{}, err
	}
	definitions, err := finalv5dataset.ProductDefinitions()
	if err != nil {
		return finalv5dataset.BenchmarkAgreement{}, err
	}
	products := make([]finalv5dataset.PostgreSQLProduct, len(definitions))
	for index, definition := range definitions {
		products[index] = definition.PostgreSQLProduct
	}
	return finalv5dataset.BenchmarkAgreement{
		Version: finalv5dataset.BenchmarkAgreementVersion, Products: products,
		Reference: reference, Observed: reference, PreparedStatementCount: 0, Agreed: true,
	}, nil
}

func buildArtifactTargetedFixture(t *testing.T, fixture *artifactTargetedBindingFixture) ArtifactTargetedDeploymentBinding {
	t.Helper()
	binding, err := buildArtifactTargetedDeploymentBinding(context.Background(), fixture.input, fixture.deps)
	if err != nil {
		t.Fatalf("build the Artifact targeted binding: %v", err)
	}
	return binding
}

func artifactTargetedEmbeddedContractFS(t *testing.T) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(contractfs.FS, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		payload, err := fs.ReadFile(contractfs.FS, path)
		if err != nil {
			return err
		}
		files[path] = &fstest.MapFile{Data: append([]byte(nil), payload...), Mode: 0o444}
		return nil
	})
	if err != nil {
		t.Fatalf("clone the embedded contract filesystem: %v", err)
	}
	return files
}

func appendArtifactTargetedSingleByte(t *testing.T, path string, value byte) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte{value}); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 || !bytes.Equal(after[:len(before)], before) || after[len(before)] != value {
		t.Fatal("source drift fixture did not change exactly one appended byte")
	}
}

func assertArtifactTargetedOutputContainsNoSecrets(t *testing.T, output []byte,
	fixture *artifactTargetedBindingFixture, probeSQL string) {
	t.Helper()
	escapedProbeSQL, err := json.Marshal(probeSQL)
	if err != nil {
		t.Fatal(err)
	}
	for name, forbidden := range map[string]string{
		"DSN":                            artifactTargetedTestDSN,
		"DSN password":                   "DSN-SECRET-MUST-NOT-APPEAR",
		"raw probe result":               artifactTargetedRawProbe,
		"raw probe SQL":                  probeSQL,
		"JSON-escaped raw probe SQL":     string(escapedProbeSQL),
		"profile registry input path":    fixture.input.ProfileRegistryPath,
		"Catalog input path":             fixture.input.CatalogPath,
		"qualification input path":       fixture.input.QualificationPath,
		"PostgreSQL identity input path": fixture.input.PostgreSQLIdentityPath,
	} {
		if forbidden != "" && bytes.Contains(output, []byte(forbidden)) {
			t.Fatalf("serialized output contains %s", name)
		}
	}
}
