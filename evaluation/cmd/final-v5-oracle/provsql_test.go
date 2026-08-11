package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

func TestProvSQLFixedDatasetShapesAgreeWithFrozenProducts(t *testing.T) {
	shapes, err := provSQLDatasetStreamShapes()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProvSQLDatasetStreamShapes(shapes); err != nil {
		t.Fatal(err)
	}
	if len(shapes) != 3 || shapes[0].ProductID != finalv5oracle.ProvSQLOrdersProductID ||
		shapes[1].ProductID != finalv5oracle.ProvSQLLineitemProductID ||
		shapes[2].ProductID != finalv5oracle.ProvSQLNonceProductID {
		t.Fatalf("fixed ProvSQL query shapes = %+v", shapes)
	}
	changed, err := provSQLDatasetStreamShapes()
	if err != nil {
		t.Fatal(err)
	}
	changed[0].Columns[0].Name = "changed"
	if err := validateProvSQLDatasetStreamShapes(changed); err == nil {
		t.Fatal("typed Product/query-shape guard accepted a changed column")
	}
	changed, err = provSQLDatasetStreamShapes()
	if err != nil {
		t.Fatal(err)
	}
	changed[1].Columns[2].PostgreSQLOID = 25
	if err := validateProvSQLDatasetStreamShapes(changed); err == nil {
		t.Fatal("typed Product/query-shape guard accepted a changed PostgreSQL OID")
	}
}

func TestProvSQLCommandsRejectCredentialAndSQLArgumentsBeforeDatabaseAccess(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
		args []string
	}{
		{name: "dataset DSN", run: runProvSQLDatasetAgreement, args: []string{"--dsn", "postgres://secret"}},
		{name: "dataset SQL", run: runProvSQLDatasetAgreement, args: []string{"--sql", "SELECT 1"}},
		{name: "generator SQL file", run: runProvSQLManifests, args: []string{"--sql-file", "query.sql"}},
		{name: "verifier DSN", run: runVerifyProvSQLManifests, args: []string{"--dsn", "postgres://secret"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := test.run(test.args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit code = %d, stderr=%q", code, stderr.String())
			}
			if stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), "postgres://secret") {
				t.Fatalf("credential/SQL rejection output stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestProvSQLManifestFlagsRequireReviewedHashesBeforeDatabaseAccess(t *testing.T) {
	want := finalv5oracle.FrozenProvSQLManifestSpecHashes()
	cases := [][]string{
		nil,
		{"--dataset-spec-sha256", want.Dataset, "--catalog-spec-sha256", want.Catalog,
			"--query-spec-sha256", strings.Repeat("0", 64), "--normalization-spec-sha256", want.Normalization},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := runProvSQLManifests(args, &stdout, &stderr); code != 2 {
			t.Fatalf("args=%q exit code=%d stderr=%q", args, code, stderr.String())
		}
		if stdout.Len() != 0 || stderr.Len() == 0 {
			t.Fatalf("args=%q stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestProvSQLManifestInstallIsAtomicClosedAndSiblingSafe(t *testing.T) {
	values, artifacts := trackedProvSQLManifestArtifacts(t)
	root := t.TempDir()
	scaleSibling := filepath.Join(root, "scale", "dependency-e2e", "sentinel.txt")
	provSQLSibling := filepath.Join(root, "provsql", "outcome-future", "sentinel.txt")
	for _, sibling := range []string{scaleSibling, provSQLSibling} {
		if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sibling, []byte("sibling material\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := installProvSQLManifests(root, artifacts); err != nil {
		t.Fatal(err)
	}
	installed, err := readProvSQLManifestSet(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 105 {
		t.Fatalf("installed %d ProvSQL manifests; expected 105", len(installed))
	}
	for relative, want := range values {
		if !bytes.Equal(installed[relative], want) {
			t.Fatalf("installed manifest %s changed bytes", relative)
		}
	}
	for _, sibling := range []string{scaleSibling, provSQLSibling} {
		value, err := os.ReadFile(sibling)
		if err != nil || string(value) != "sibling material\n" {
			t.Fatalf("sibling %s changed: value=%q err=%v", sibling, value, err)
		}
	}
	if err := installProvSQLManifests(root, artifacts); err != nil {
		t.Fatalf("idempotent install failed: %v", err)
	}

	first := artifacts[0].RelativePath
	drifted := filepath.Join(root, filepath.FromSlash(first))
	if err := os.WriteFile(drifted, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installProvSQLManifests(root, artifacts); err == nil {
		t.Fatal("installer overwrote a drifted ProvSQL manifest tree")
	}
	if value, err := os.ReadFile(drifted); err != nil || string(value) != "{}\n" {
		t.Fatalf("failed install changed drifted bytes: value=%q err=%v", value, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".final-v5-oracle-provsql-") {
			t.Fatalf("atomic installer left staging directory %q", entry.Name())
		}
	}
}

func TestReadProvSQLManifestSetRejectsMissingExtraAndSymlinkEntries(t *testing.T) {
	_, artifacts := trackedProvSQLManifestArtifacts(t)
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, []finalv5oracle.ProvSQLManifestArtifact)
	}{
		{name: "missing", mutate: func(t *testing.T, root string, artifacts []finalv5oracle.ProvSQLManifestArtifact) {
			t.Helper()
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(artifacts[0].RelativePath))); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra file", mutate: func(t *testing.T, root string, artifacts []finalv5oracle.ProvSQLManifestArtifact) {
			t.Helper()
			extra := filepath.Join(root, filepath.Dir(filepath.FromSlash(artifacts[0].RelativePath)), "extra.json")
			if err := os.WriteFile(extra, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra directory", mutate: func(t *testing.T, root string, _ []finalv5oracle.ProvSQLManifestArtifact) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(root, "provsql", "nonce-join-group", "unexpected"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "expected path symlink", mutate: func(t *testing.T, root string, artifacts []finalv5oracle.ProvSQLManifestArtifact) {
			t.Helper()
			expected := filepath.Join(root, filepath.FromSlash(artifacts[0].RelativePath))
			outside := filepath.Join(t.TempDir(), "same-manifest.json")
			value, err := finalv5oracle.CanonicalManifest(artifacts[0].Manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(outside, value, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(expected); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, expected); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := installProvSQLManifests(root, artifacts); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, root, artifacts)
			if _, err := readProvSQLManifestSet(root); err == nil {
				t.Fatal("closed-set reader accepted a missing, extra, or symlink entry")
			}
		})
	}
}

func TestProvSQLManifestInstallAndReaderRejectSymlinkParent(t *testing.T) {
	_, artifacts := trackedProvSQLManifestArtifacts(t)
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "provsql")); err != nil {
		t.Fatal(err)
	}
	if err := installProvSQLManifests(root, artifacts); err == nil {
		t.Fatal("installer followed a symlinked ProvSQL output parent")
	}
	if _, err := readProvSQLManifestSet(root); err == nil {
		t.Fatal("reader followed a symlinked ProvSQL input parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected symlink parent received %d entries", len(entries))
	}
}

func TestProvSQLManifestInstallRejectsIncompleteOrUnboundArtifactsBeforeWriting(t *testing.T) {
	_, artifacts := trackedProvSQLManifestArtifacts(t)
	cases := map[string][]finalv5oracle.ProvSQLManifestArtifact{
		"missing": append([]finalv5oracle.ProvSQLManifestArtifact(nil), artifacts[:104]...),
		"extra":   append(append([]finalv5oracle.ProvSQLManifestArtifact(nil), artifacts...), artifacts[0]),
	}
	badSHA := append([]finalv5oracle.ProvSQLManifestArtifact(nil), artifacts...)
	badSHA[0].SHA256 = finalv5oracle.ProvSQLDatasetSpecSHA256
	cases["bad SHA"] = badSHA
	traversal := append([]finalv5oracle.ProvSQLManifestArtifact(nil), artifacts...)
	traversal[0].RelativePath = "../outside.json"
	cases["path traversal"] = traversal
	unbound := append([]finalv5oracle.ProvSQLManifestArtifact(nil), artifacts...)
	unbound[0].Manifest.BindingKey = artifacts[1].Manifest.BindingKey
	cases["unbound identity"] = unbound
	for name, changed := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := installProvSQLManifests(root, changed); err == nil {
				t.Fatal("installer accepted an incomplete or unbound artifact batch")
			}
			if _, err := os.Stat(filepath.Join(root, "provsql", "nonce-join-group")); !os.IsNotExist(err) {
				t.Fatalf("failed validation left a ProvSQL target behind: %v", err)
			}
		})
	}
}

func TestProvSQLDatasetAgreementAgainstPostgreSQLRequiresLiveDSN(t *testing.T) {
	if strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN")) == "" &&
		strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BUSINESS_DSN")) == "" {
		t.Fatal("requires BUSINESS_TEST_POSTGRES_DSN or TASKGATE_FINAL_V5_BUSINESS_DSN; this live test must not skip")
	}
	agreement, err := liveProvSQLDatasetAgreement(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !agreement.Agreed || agreement.PreparedStatementCount != 0 ||
		!reflect.DeepEqual(agreement.Reference, agreement.Observed) ||
		agreement.Reference.ProductCount != 3 || agreement.Reference.RowCount != finalv5oracle.ProvSQLDatasetRows ||
		len(agreement.Reference.Products) != 3 {
		t.Fatalf("live ProvSQL typed Dataset agreement = %+v", agreement)
	}
	encoded, err := json.Marshal(agreement)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"BUSINESS_TEST_POSTGRES_DSN", "TASKGATE_FINAL_V5_BUSINESS_DSN"} {
		if secret := strings.TrimSpace(os.Getenv(name)); secret != "" && bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("credential-free agreement exposed %s", name)
		}
	}
	t.Logf("rows=%d products=%d reference_sha256=%s observed_sha256=%s prepared_statement_count=%d agreed=%t",
		agreement.Observed.RowCount, agreement.Observed.ProductCount, agreement.Reference.SHA256,
		agreement.Observed.SHA256, agreement.PreparedStatementCount, agreement.Agreed)
}

func trackedProvSQLManifestArtifacts(t *testing.T) (map[string][]byte, []finalv5oracle.ProvSQLManifestArtifact) {
	t.Helper()
	sourceRoot := filepath.Join("..", "..", "final-v5-wsl2", "oracle-manifests")
	values, err := readProvSQLManifestSet(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(values))
	for relative := range values {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	artifacts := make([]finalv5oracle.ProvSQLManifestArtifact, 0, len(paths))
	for _, relative := range paths {
		manifest, err := finalv5oracle.DecodeManifest(values[relative])
		if err != nil {
			t.Fatal(err)
		}
		digest, err := finalv5oracle.ManifestSHA256(manifest)
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, finalv5oracle.ProvSQLManifestArtifact{
			RelativePath: relative, SHA256: digest, Manifest: manifest,
		})
	}
	if len(artifacts) != 105 {
		t.Fatalf("tracked ProvSQL manifest count = %d, want 105", len(artifacts))
	}
	return values, artifacts
}
