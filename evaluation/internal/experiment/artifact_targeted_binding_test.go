package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	contractfs "taskbound.local/agent-data-gateway/evaluation/final-v5-wsl2"
	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
)

const (
	artifactTargetedTestCommit = "0123456789abcdef0123456789abcdef01234567"
	artifactTargetedTestDSN    = "postgres://binding-user:DSN-SECRET-MUST-NOT-APPEAR@127.0.0.1:65535/business?sslmode=disable"
	artifactTargetedRawProbe   = "RAW-PROBE-RESULT-MUST-NOT-APPEAR"
)

type artifactTargetedBindingFixture struct {
	input      ArtifactTargetedBindingInput
	deps       artifactTargetedBindingDependencies
	probeCalls int
}

func newArtifactTargetedBindingFixture(t *testing.T) *artifactTargetedBindingFixture {
	t.Helper()
	root := repositoryRootForDeployment(t)
	qualificationRoot := filepath.Join(root, "evaluation", "final-v5-wsl2", "raw", retainedQualificationRun)
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

func buildArtifactTargetedFixture(t *testing.T, fixture *artifactTargetedBindingFixture) ArtifactTargetedDeploymentBinding {
	t.Helper()
	binding, err := buildArtifactTargetedDeploymentBinding(context.Background(), fixture.input, fixture.deps)
	if err != nil {
		t.Fatalf("build the Artifact targeted binding: %v", err)
	}
	return binding
}

func TestArtifactTargetedBindingBuildsTheSixFrozenCellsWithoutPostgreSQL(t *testing.T) {
	fixture := newArtifactTargetedBindingFixture(t)
	binding := buildArtifactTargetedFixture(t, fixture)
	if fixture.probeCalls != 1 {
		t.Fatalf("dataset probe calls = %d, want exactly 1", fixture.probeCalls)
	}

	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatalf("load the embedded Contract Index independently: %v", err)
	}
	probeSQL, err := runtime.DatasetProbeSQL()
	if err != nil {
		t.Fatalf("load the indexed dataset probe independently: %v", err)
	}
	if binding.SchemaVersion != 1 || binding.Record != ArtifactTargetedDeploymentBindingVersion ||
		binding.SubmissionCommit != artifactTargetedTestCommit || binding.ContractRelease != runtime.ContractRelease() ||
		binding.ContractIndexSHA256 != runtime.IndexSHA256() ||
		binding.DatasetProbeSQLSHA256 != sha256String(probeSQL) ||
		binding.DatasetProbeSHA256 != sha256String(artifactTargetedRawProbe) {
		t.Fatalf("binding header or probe identity is not derived from the embedded runtime: %+v", binding)
	}
	wantSelected := []string{"100x4", "10k-x16", "100k-x16"}
	if !reflect.DeepEqual(binding.SelectedScales, wantSelected) {
		t.Fatalf("selected scales = %v, want frozen order %v", binding.SelectedScales, wantSelected)
	}
	if len(binding.ArtifactCells) != len(frozenArtifactTargetedScales) {
		t.Fatalf("binding carries %d Artifact cells, want 6", len(binding.ArtifactCells))
	}
	wantRows := []int64{100, 10_000, 100_000, 100, 10_000, 100_000}
	wantColumns := []int{4, 4, 4, 16, 16, 16}
	for index, got := range binding.ArtifactCells {
		cell, err := runtime.ArtifactCell(frozenArtifactTargetedScales[index], "novel")
		if err != nil {
			t.Fatalf("load Artifact cell %d independently: %v", index, err)
		}
		query, err := runtime.QueryContract(cell)
		if err != nil {
			t.Fatalf("load Artifact query %s independently: %v", cell.Identity, err)
		}
		manifest, manifestSHA, err := runtime.OracleManifest(cell)
		if err != nil {
			t.Fatalf("load Artifact oracle %s independently: %v", cell.Identity, err)
		}
		if got.Cell != cell.Identity || got.Rows != wantRows[index] || got.Columns != wantColumns[index] ||
			got.Rows != query.Rows || got.Columns != query.Columns || got.SpecID != cell.SpecID ||
			got.BDGTemplateSHA256 != query.BDG.TemplateSHA256 || got.BDGSQLSHA256 != query.BDG.SQLSHA256 ||
			got.DirectTemplateSHA256 != query.Direct.TemplateSHA256 || got.DirectSQLSHA256 != query.Direct.SQLSHA256 ||
			got.NormalizationSHA256 != query.NormalizationSHA256 ||
			got.OracleManifestPath != cell.OracleManifestPath || got.OracleManifestSHA256 != manifestSHA ||
			got.NormalizedSchemaSHA256 != manifest.Expected.NormalizedSchemaSHA256 ||
			got.CanonicalResultSHA256 != manifest.Expected.CanonicalResultSHA256 {
			t.Fatalf("cell %d is not the exact embedded query/oracle identity:\n got %+v", index, got)
		}
	}
	for name, path := range map[string]string{
		"profile_registry_sha256":          fixture.input.ProfileRegistryPath,
		"catalog_sha256":                   fixture.input.CatalogPath,
		"attestation_qualification_sha256": fixture.input.QualificationPath,
		"postgresql_identity_sha256":       fixture.input.PostgreSQLIdentityPath,
	} {
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256Hex(payload)
		var got string
		switch name {
		case "profile_registry_sha256":
			got = binding.ProfileRegistrySHA256
		case "catalog_sha256":
			got = binding.Profile.CatalogSHA256
		case "attestation_qualification_sha256":
			got = binding.AttestationQualificationSHA256
		case "postgresql_identity_sha256":
			got = binding.PostgreSQLIdentitySHA256
		}
		if got != want {
			t.Fatalf("%s = %s, want exact source bytes %s", name, got, want)
		}
	}
	if binding.Profile.ProfileAlias != artifactDeploymentProfileAlias ||
		binding.Profile.ProfileID != "profile-a86cd4df5cad6e26" ||
		!binding.Profile.ActivationSupported || !binding.Profile.ActivationSmokePassed ||
		!binding.Profile.TargetedRunEligible {
		t.Fatalf("binding does not carry the cleared Result-heavy identity: %+v", binding.Profile)
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("the constructed binding does not validate: %v", err)
	}

	canonical, err := CanonicalArtifactTargetedDeploymentBinding(binding)
	if err != nil {
		t.Fatalf("canonicalize the constructed binding: %v", err)
	}
	if bytes.HasSuffix(canonical, []byte("\n")) {
		t.Fatal("canonical binding has a trailing newline")
	}
	decoded, err := DecodeArtifactTargetedDeploymentBinding(canonical)
	if err != nil || !reflect.DeepEqual(decoded, binding) {
		t.Fatalf("canonical binding did not round trip: decoded=%+v err=%v", decoded, err)
	}
	assertArtifactTargetedOutputContainsNoSecrets(t, canonical, fixture, probeSQL)
}

func TestArtifactTargetedBindingSelectedScalesAreAClosedOrderedSubset(t *testing.T) {
	valid := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "one", in: []string{"10k-x16"}, want: []string{"10k-x16"}},
		{name: "caller order is normalized", in: []string{"100k-x16", "100x4", "100k-x4"},
			want: []string{"100x4", "100k-x4", "100k-x16"}},
		{name: "all", in: []string{"100k-x16", "10k-x16", "100x16", "100k-x4", "10k-x4", "100x4"},
			want: append([]string(nil), frozenArtifactTargetedScales[:]...)},
	}
	for _, testCase := range valid {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := normalizeArtifactTargetedScales(testCase.in)
			if err != nil || !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("normalize %v = %v, %v; want %v", testCase.in, got, err, testCase.want)
			}
		})
	}
	for _, testCase := range []struct {
		name string
		in   []string
	}{
		{name: "empty"},
		{name: "unknown", in: []string{"1m-x16"}},
		{name: "duplicate", in: []string{"100x4", "100x4"}},
		{name: "leading whitespace", in: []string{" 100x4"}},
		{name: "empty member", in: []string{""}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got, err := normalizeArtifactTargetedScales(testCase.in); err == nil {
				t.Fatalf("invalid selected scales %v normalized to %v", testCase.in, got)
			}
		})
	}

	fixture := newArtifactTargetedBindingFixture(t)
	binding := buildArtifactTargetedFixture(t, fixture)
	binding.SelectedScales[0], binding.SelectedScales[1] = binding.SelectedScales[1], binding.SelectedScales[0]
	if err := binding.Validate(); err == nil || !strings.Contains(err.Error(), "frozen order") {
		t.Fatalf("a reordered record was not rejected: %v", err)
	}
}

func TestArtifactTargetedBindingRequiresOneValidLiveDatasetProbe(t *testing.T) {
	t.Run("missing Business DSN", func(t *testing.T) {
		fixture := newArtifactTargetedBindingFixture(t)
		fixture.input.BusinessDSN = ""
		if _, err := buildArtifactTargetedDeploymentBinding(
			context.Background(), fixture.input, fixture.deps); err == nil {
			t.Fatal("a missing live Business PostgreSQL probe input was accepted")
		}
		if fixture.probeCalls != 0 {
			t.Fatalf("probe ran %d times with no Business DSN", fixture.probeCalls)
		}
	})

	for _, testCase := range []struct {
		name   string
		digest string
		err    error
	}{
		{name: "probe execution failed", err: errors.New("probe unavailable")},
		{name: "missing result", digest: ""},
		{name: "malformed result", digest: "not-a-sha256"},
		{name: "placeholder result", digest: strings.Repeat("0", 64)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newArtifactTargetedBindingFixture(t)
			calls := 0
			fixture.deps.probe = func(context.Context, *finalv5contracts.Runtime, string) (string, error) {
				calls++
				return testCase.digest, testCase.err
			}
			if _, err := buildArtifactTargetedDeploymentBinding(
				context.Background(), fixture.input, fixture.deps); err == nil {
				t.Fatal("a missing, failed or invalid live dataset probe was accepted")
			}
			if calls != 1 {
				t.Fatalf("invalid probe path ran %d times, want exactly 1", calls)
			}
		})
	}
}

func TestArtifactTargetedBindingRejectsSingleByteSourceDrift(t *testing.T) {
	for _, testCase := range []struct {
		name string
		path func(ArtifactTargetedBindingInput) string
	}{
		{name: "registry", path: func(input ArtifactTargetedBindingInput) string { return input.ProfileRegistryPath }},
		{name: "Catalog", path: func(input ArtifactTargetedBindingInput) string { return input.CatalogPath }},
		{name: "qualification", path: func(input ArtifactTargetedBindingInput) string { return input.QualificationPath }},
		{name: "PostgreSQL identity", path: func(input ArtifactTargetedBindingInput) string { return input.PostgreSQLIdentityPath }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newArtifactTargetedBindingFixture(t)
			original := buildArtifactTargetedFixture(t, fixture)
			output := filepath.Join(t.TempDir(), "binding.json")
			if err := WriteArtifactTargetedDeploymentBinding(output, original); err != nil {
				t.Fatalf("write the pre-drift binding: %v", err)
			}
			appendArtifactTargetedSingleByte(t, testCase.path(fixture.input), '\n')

			current, err := buildArtifactTargetedDeploymentBinding(context.Background(), fixture.input, fixture.deps)
			if err != nil {
				return
			}
			if reflect.DeepEqual(current, original) {
				t.Fatal("a one-byte source drift reproduced the old binding")
			}
			if _, err := ValidateArtifactTargetedDeploymentBindingFile(output, current); err == nil {
				t.Fatal("the pre-drift binding file validated against one-byte-drifted sources")
			}
		})
	}
}

func TestArtifactTargetedBindingRejectsContractAndOracleMutation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		path string
	}{
		{name: "indexed Artifact contract", path: "contracts/artifact-v1.json"},
		{name: "Artifact oracle manifest", path: "oracle-manifests/artifact/result-heavy/100x4/novel.json"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newArtifactTargetedBindingFixture(t)
			files := artifactTargetedEmbeddedContractFS(t)
			original, present := files[testCase.path]
			if !present {
				t.Fatalf("embedded contract tree has no %s", testCase.path)
			}
			files[testCase.path] = &fstest.MapFile{Data: append(append([]byte(nil), original.Data...), '\n')}
			fixture.deps.loadRuntime = func() (*finalv5contracts.Runtime, error) {
				return finalv5contracts.LoadRuntimeFS(files)
			}
			if _, err := buildArtifactTargetedDeploymentBinding(context.Background(), fixture.input, fixture.deps); err == nil {
				t.Fatalf("one-byte mutation of %s was accepted", testCase.path)
			}
		})
	}
}

func TestArtifactTargetedBindingDecoderRequiresExactCanonicalJSON(t *testing.T) {
	fixture := newArtifactTargetedBindingFixture(t)
	binding := buildArtifactTargetedFixture(t, fixture)
	canonical, err := CanonicalArtifactTargetedDeploymentBinding(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeArtifactTargetedDeploymentBinding(canonical); err != nil {
		t.Fatalf("exact canonical JSON was rejected: %v", err)
	}

	mutations := map[string][]byte{
		"unknown field":    append([]byte(`{"unknown":true,`), canonical[1:]...),
		"duplicate field":  append([]byte(`{"schema_version":1,`), canonical[1:]...),
		"trailing newline": append(append([]byte(nil), canonical...), '\n'),
		"leading space":    append([]byte{' '}, canonical...),
	}
	for name, payload := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeArtifactTargetedDeploymentBinding(payload); err == nil {
				t.Fatal("non-canonical or structurally unsafe JSON was accepted")
			}
		})
	}
}

func TestArtifactTargetedBindingFilesAreExclusiveRegularMode0600(t *testing.T) {
	fixture := newArtifactTargetedBindingFixture(t)
	binding := buildArtifactTargetedFixture(t, fixture)
	probeSQLRuntime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatal(err)
	}
	probeSQL, err := probeSQLRuntime.DatasetProbeSQL()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("create validate and refuse replacement", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "binding.json")
		if err := WriteArtifactTargetedDeploymentBinding(path, binding); err != nil {
			t.Fatalf("write binding: %v", err)
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("binding output mode/type = %v, err=%v", info, err)
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := CanonicalArtifactTargetedDeploymentBinding(binding)
		if err != nil || !bytes.Equal(payload, canonical) {
			t.Fatalf("output is not the exact canonical bytes: err=%v", err)
		}
		report, err := ValidateArtifactTargetedDeploymentBindingFile(path, binding)
		if err != nil {
			t.Fatalf("validate safe binding: %v", err)
		}
		if report.SchemaVersion != 1 || report.Status != "valid" || report.ArtifactCells != 6 ||
			report.SelectedCells != len(binding.SelectedScales) ||
			report.DatasetProbeSHA256 != binding.DatasetProbeSHA256 || report.BindingFileSHA256 != sha256Hex(payload) {
			t.Fatalf("validation report = %+v", report)
		}
		reportBytes, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		assertArtifactTargetedOutputContainsNoSecrets(t, append(payload, reportBytes...), fixture, probeSQL)

		before := append([]byte(nil), payload...)
		if err := WriteArtifactTargetedDeploymentBinding(path, binding); err == nil {
			t.Fatal("create-exclusive writer replaced an existing binding")
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("failed replacement changed existing bytes: err=%v", err)
		}
	})

	t.Run("wrong mode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "binding.json")
		if err := WriteArtifactTargetedDeploymentBinding(path, binding); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateArtifactTargetedDeploymentBindingFile(path, binding); err == nil {
			t.Fatal("a non-0600 binding validated")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		directory := t.TempDir()
		target := filepath.Join(directory, "target.json")
		link := filepath.Join(directory, "binding.json")
		if err := WriteArtifactTargetedDeploymentBinding(target, binding); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateArtifactTargetedDeploymentBindingFile(link, binding); err == nil {
			t.Fatal("a symlink binding validated")
		}
		if err := WriteArtifactTargetedDeploymentBinding(link, binding); err == nil {
			t.Fatal("create-exclusive writer followed or replaced a symlink")
		}
		after, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("symlink refusal changed its target: err=%v", err)
		}
	})

	t.Run("unknown field on disk", func(t *testing.T) {
		canonical, err := CanonicalArtifactTargetedDeploymentBinding(binding)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "binding.json")
		unsafe := append([]byte(`{"unknown":true,`), canonical[1:]...)
		if err := os.WriteFile(path, unsafe, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateArtifactTargetedDeploymentBindingFile(path, binding); err == nil {
			t.Fatal("a binding with an unknown field validated")
		}
	})
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
