package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

func TestBuildScansCompilesAndWritesInputsSequentially(t *testing.T) {
	first, _ := approvedFixture(t, "offline_first", 10)
	second, _ := approvedFixture(t, "offline_second", 20)
	inputDirectory := t.TempDir()
	firstPath := writeInput(t, inputDirectory, first)
	secondPath := writeInput(t, inputDirectory, second)
	outputDirectory := t.TempDir()

	var scanned []string
	scanner := func(_ context.Context, input snapshotbundle.CompilerInput, dsn string) (snapshotbundle.CompilerInput, error) {
		if dsn != "postgres://scanner.example/test" {
			t.Fatalf("scanner DSN = %q", dsn)
		}
		if len(scanned) == 1 {
			if _, err := os.Stat(filepath.Join(outputDirectory, first.PublicationName,
				first.PublicationName+".bundle.json")); err != nil {
				t.Fatalf("second scan began before first publication was written: %v", err)
			}
		}
		scanned = append(scanned, input.PublicationName)
		return input, nil
	}
	var stdout bytes.Buffer
	err := run(context.Background(), []string{"build", "-input", firstPath, "-input", secondPath,
		"-output-dir", outputDirectory}, func(name string) string {
		if name == "SNAPSHOT_POSTGRES_DSN" {
			return "postgres://scanner.example/test"
		}
		return ""
	}, &stdout, buildDependencies{scan: scanner})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scanned, []string{first.PublicationName, second.PublicationName}) {
		t.Fatalf("scan order = %v", scanned)
	}
	report := decodeReport(t, stdout.Bytes())
	if report.Mode != "build" || len(report.Publications) != 2 ||
		report.Publications[0].PublicationName != first.PublicationName ||
		report.Publications[1].PublicationName != second.PublicationName ||
		report.TotalArtifactBytes <= 0 || report.HotArtifactBytes <= 0 {
		t.Fatalf("unexpected build report: %#v", report)
	}
	for _, input := range []snapshotbundle.CompilerInput{first, second} {
		if _, err := os.Stat(filepath.Join(outputDirectory, input.PublicationName,
			input.PublicationName+".bundle.json")); err != nil {
			t.Fatalf("publication %s was not written: %v", input.PublicationName, err)
		}
	}
}

func TestBuildRequiresSnapshotPostgresDSNBeforeScanning(t *testing.T) {
	input, _ := approvedFixture(t, "offline_no_dsn", 30)
	inputPath := writeInput(t, t.TempDir(), input)
	called := false
	err := run(context.Background(), []string{"build", "-input", inputPath, "-output-dir", t.TempDir()},
		func(string) string { return "" }, &bytes.Buffer{}, buildDependencies{scan: func(context.Context,
			snapshotbundle.CompilerInput, string) (snapshotbundle.CompilerInput, error) {
			called = true
			return snapshotbundle.CompilerInput{}, nil
		}})
	if err == nil || !strings.Contains(err.Error(), "SNAPSHOT_POSTGRES_DSN") || called {
		t.Fatalf("err = %v, scanner called = %v", err, called)
	}
}

func TestActivateVerifiesApprovedBundlesInInputOrder(t *testing.T) {
	first, firstBundle := approvedFixture(t, "offline_activate_a", 40)
	second, secondBundle := approvedFixture(t, "offline_activate_b", 50)
	artifactDirectory := t.TempDir()
	if _, err := firstBundle.Write(artifactDirectory); err != nil {
		t.Fatal(err)
	}
	if _, err := secondBundle.Write(artifactDirectory); err != nil {
		t.Fatal(err)
	}
	inputDirectory := t.TempDir()
	firstPath := writeInput(t, inputDirectory, first)
	secondPath := writeInput(t, inputDirectory, second)
	report := verifyThenActivate(t, artifactDirectory, secondPath, firstPath)
	if report.Mode != "activate" || len(report.Publications) != 2 ||
		report.Publications[0].PublicationName != second.PublicationName ||
		report.Publications[1].PublicationName != first.PublicationName {
		t.Fatalf("activation order/report = %#v", report)
	}
	wantTotal := bundleBytes(t, firstBundle) + bundleBytes(t, secondBundle)
	wantHot := int64(len(firstBundle.Hot) + len(secondBundle.Hot))
	if report.TotalArtifactBytes != wantTotal || report.HotArtifactBytes != wantHot {
		t.Fatalf("artifact bytes = %d/%d, want %d/%d", report.TotalArtifactBytes,
			report.HotArtifactBytes, wantTotal, wantHot)
	}
}

func TestActivateRejectsInternallyInvalidArtifactsWithReboundTransportDigests(t *testing.T) {
	for _, kind := range []string{"hot", "cold", "sidecar"} {
		t.Run(kind, func(t *testing.T) {
			input, bundle := approvedFixture(t, "offline_tamper_"+kind, 60)
			artifactDirectory := t.TempDir()
			publicationDirectory, err := bundle.Write(artifactDirectory)
			if err != nil {
				t.Fatal(err)
			}
			descriptor := bundle.Manifest.Hot
			switch kind {
			case "cold":
				descriptor = bundle.Manifest.Cold
			case "sidecar":
				descriptor = bundle.Manifest.Sidecar
			}
			artifactPath := filepath.Join(publicationDirectory, descriptor.Name)
			flipFileByte(t, artifactPath)
			descriptor.SHA256 = fileSHA256(t, artifactPath)
			switch kind {
			case "hot":
				bundle.Manifest.Hot = descriptor
			case "cold":
				bundle.Manifest.Cold = descriptor
			case "sidecar":
				bundle.Manifest.Sidecar = descriptor
			}
			manifestBytes, err := bundle.ManifestJSON()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(publicationDirectory,
				bundle.Manifest.PublicationName+".bundle.json"), manifestBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			inputPath := writeInput(t, t.TempDir(), input)
			err = run(context.Background(), []string{"verify", "-input", inputPath,
				"-artifact-dir", artifactDirectory, "-receipt", filepath.Join(t.TempDir(), "receipt.json")},
				func(string) string { return "" }, &bytes.Buffer{}, immutableTestDependencies())
			if err == nil {
				t.Fatal("internally invalid artifact with rebound transport digest was accepted")
			}
			wantBoundary := map[string]string{"hot": "parse HOT artifact", "cold": "verify COLD envelope",
				"sidecar": "verify sidecar"}[kind]
			if !strings.Contains(err.Error(), wantBoundary) {
				t.Fatalf("%s error = %v", kind, err)
			}
		})
	}
}

func TestActivateRejectsInputDigestMismatchAndUnexpectedPublication(t *testing.T) {
	input, bundle := approvedFixture(t, "offline_binding", 70)
	artifactDirectory := t.TempDir()
	if _, err := bundle.Write(artifactDirectory); err != nil {
		t.Fatal(err)
	}

	t.Run("digest mismatch", func(t *testing.T) {
		mismatched := input
		mismatched.ExpectedDigests.ManifestDigest = strings.Repeat("0", 64)
		inputPath := writeInput(t, t.TempDir(), mismatched)
		err := run(context.Background(), []string{"verify", "-input", inputPath,
			"-artifact-dir", artifactDirectory, "-receipt", filepath.Join(t.TempDir(), "receipt.json")},
			func(string) string { return "" }, &bytes.Buffer{}, immutableTestDependencies())
		if err == nil || !strings.Contains(err.Error(), "does not match approved compiler input") {
			t.Fatalf("digest mismatch err = %v", err)
		}
	})

	t.Run("unexpected publication", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(artifactDirectory, "unexpected"), 0o700); err != nil {
			t.Fatal(err)
		}
		inputPath := writeInput(t, t.TempDir(), input)
		err := run(context.Background(), []string{"verify", "-input", inputPath,
			"-artifact-dir", artifactDirectory, "-receipt", filepath.Join(t.TempDir(), "receipt.json")},
			func(string) string { return "" }, &bytes.Buffer{}, immutableTestDependencies())
		if err == nil || !strings.Contains(err.Error(), "exactly the requested publications") {
			t.Fatalf("unexpected publication err = %v", err)
		}
	})
}

func TestActivateRequiresAllApprovedDigests(t *testing.T) {
	input, bundle := approvedFixture(t, "offline_missing_digest", 80)
	input.ExpectedDigests.ColdPayloadDigest = ""
	artifactDirectory := t.TempDir()
	if _, err := bundle.Write(artifactDirectory); err != nil {
		t.Fatal(err)
	}
	inputPath := writeInput(t, t.TempDir(), input)
	err := run(context.Background(), []string{"verify", "-input", inputPath,
		"-artifact-dir", artifactDirectory, "-receipt", filepath.Join(t.TempDir(), "receipt.json")},
		func(string) string { return "" }, &bytes.Buffer{}, immutableTestDependencies())
	if err == nil || !strings.Contains(err.Error(), "approved cold payload digest is required") {
		t.Fatalf("missing digest err = %v", err)
	}
}

func approvedFixture(t *testing.T, publicationName string, base int64) (snapshotbundle.CompilerInput,
	snapshotbundle.CompiledBundle) {
	t.Helper()
	input := snapshotbundle.CompilerInput{
		Version: snapshotbundle.CompilerInputVersion, PublicationName: publicationName,
		CatalogSource: "scale_demo", SourceRelation: "reporting." + publicationName,
		OrdinalSidecar: "taskgate_ordinal." + publicationName, EntityKeyFields: []string{"id"},
		Snapshot: snapshotbundle.SnapshotInput{SourceID: "offline_source",
			SourceNamespace: "evaluation." + publicationName, Snapshot: "snapshot-v1",
			SchemaDigest: strings.Repeat("a", 64), Fields: []snapshotbundle.SnapshotField{
				{Name: "id", CanonicalFieldID: publicationName + ".id", SQLType: "bigint"},
				{Name: "amount", CanonicalFieldID: publicationName + ".amount", SQLType: "bigint"},
			}, Rows: []snapshotbundle.SnapshotRow{
				{Values: map[string]any{"id": base + 1, "amount": base + 101}},
				{Values: map[string]any{"id": base + 2, "amount": base + 102}},
			}},
	}
	bundle, err := snapshotbundle.Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	manifest := bundle.Manifest.DictionaryManifest
	input.ExpectedDigests = snapshotbundle.ExpectedDigests{ManifestDigest: bundle.Manifest.ManifestDigest,
		DictionaryDigest: manifest.DictionaryDigest, SidecarDigest: manifest.SidecarDigest,
		ColdPayloadDigest: manifest.ColdPayloadDigest, HotIndexDigest: manifest.HotIndexDigest}
	return input, bundle
}

func writeInput(t *testing.T, directory string, input snapshotbundle.CompilerInput) string {
	t.Helper()
	raw, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, input.PublicationName+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeReport(t *testing.T, raw []byte) commandReport {
	t.Helper()
	var report commandReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != reportSchemaVersion {
		t.Fatalf("schema version = %d", report.SchemaVersion)
	}
	return report
}

func bundleBytes(t *testing.T, bundle snapshotbundle.CompiledBundle) int64 {
	t.Helper()
	manifest, err := bundle.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(bundle.Hot) + len(bundle.Cold) + len(bundle.Sidecar) + len(manifest))
}

func flipFileByte(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		t.Fatalf("stat artifact: %v", err)
	}
	offset := info.Size() / 2
	var value [1]byte
	if _, err := file.ReadAt(value[:], offset); err != nil {
		t.Fatal(err)
	}
	value[0] ^= 0xff
	if _, err := file.WriteAt(value[:], offset); err != nil {
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func immutableTestDependencies() buildDependencies {
	return buildDependencies{requireImmutable: func(string) error { return nil }}
}

func verifyThenActivate(t *testing.T, artifactDirectory string, inputPaths ...string) commandReport {
	t.Helper()
	receiptPath := filepath.Join(t.TempDir(), "verification-receipt.json")
	verifyArgs := []string{"verify"}
	for _, inputPath := range inputPaths {
		verifyArgs = append(verifyArgs, "-input", inputPath)
	}
	verifyArgs = append(verifyArgs, "-artifact-dir", artifactDirectory, "-receipt", receiptPath)
	var verifyStdout bytes.Buffer
	if err := run(context.Background(), verifyArgs, func(string) string { return "" }, &verifyStdout,
		immutableTestDependencies()); err != nil {
		t.Fatalf("strict verify: %v", err)
	}
	verifyReport := decodeReport(t, verifyStdout.Bytes())
	if verifyReport.Mode != "verify" || !digestPattern.MatchString(verifyReport.VerificationReceiptSHA256) {
		t.Fatalf("strict verification report = %#v", verifyReport)
	}
	activateArgs := []string{"activate"}
	for _, inputPath := range inputPaths {
		activateArgs = append(activateArgs, "-input", inputPath)
	}
	activateArgs = append(activateArgs, "-artifact-dir", artifactDirectory, "-receipt", receiptPath,
		"-receipt-sha256", verifyReport.VerificationReceiptSHA256)
	var activateStdout bytes.Buffer
	if err := run(context.Background(), activateArgs, func(string) string { return "" }, &activateStdout,
		immutableTestDependencies()); err != nil {
		t.Fatalf("warm activate: %v", err)
	}
	return decodeReport(t, activateStdout.Bytes())
}

func TestWarmActivationRejectsArtifactOrReceiptChangeAfterStrictVerification(t *testing.T) {
	for _, kind := range []string{"manifest", "hot", "cold", "sidecar", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			input, bundle := approvedFixture(t, "offline_warm_tamper_"+kind, 90)
			artifactDirectory := t.TempDir()
			publicationDirectory, err := bundle.Write(artifactDirectory)
			if err != nil {
				t.Fatal(err)
			}
			inputPath := writeInput(t, t.TempDir(), input)
			receiptPath := filepath.Join(t.TempDir(), "receipt.json")
			var verified bytes.Buffer
			if err := run(context.Background(), []string{"verify", "-input", inputPath,
				"-artifact-dir", artifactDirectory, "-receipt", receiptPath}, func(string) string { return "" },
				&verified, immutableTestDependencies()); err != nil {
				t.Fatal(err)
			}
			verificationReport := decodeReport(t, verified.Bytes())
			path := receiptPath
			switch kind {
			case "manifest":
				path = filepath.Join(publicationDirectory, bundle.Manifest.PublicationName+".bundle.json")
			case "hot":
				path = filepath.Join(publicationDirectory, bundle.Manifest.Hot.Name)
			case "cold":
				path = filepath.Join(publicationDirectory, bundle.Manifest.Cold.Name)
			case "sidecar":
				path = filepath.Join(publicationDirectory, bundle.Manifest.Sidecar.Name)
			}
			flipFileByte(t, path)
			err = run(context.Background(), []string{"activate", "-input", inputPath,
				"-artifact-dir", artifactDirectory, "-receipt", receiptPath, "-receipt-sha256",
				verificationReport.VerificationReceiptSHA256}, func(string) string { return "" }, &bytes.Buffer{},
				immutableTestDependencies())
			if err == nil {
				t.Fatalf("warm activation accepted changed %s", kind)
			}
		})
	}
}

func TestParseCommandRejectsAmbiguousModes(t *testing.T) {
	_, err := parseCommand([]string{"build", "-input", "one", "-output-dir", "out", "-artifact-dir", "artifacts"})
	if !errors.Is(err, errUsage) {
		t.Fatalf("ambiguous build err = %v", err)
	}
	_, err = parseCommand([]string{"activate", "-input", "one", "-artifact-dir", "artifacts", "trailing"})
	if !errors.Is(err, errUsage) {
		t.Fatalf("trailing activate err = %v", err)
	}
	_, err = parseCommand([]string{"activate", "-input", "one", "-artifact-dir", "artifacts",
		"-receipt", "receipt.json", "-receipt-sha256", strings.Repeat("a", 64)})
	if err != nil {
		t.Fatalf("complete warm activation contract was rejected: %v", err)
	}
}
