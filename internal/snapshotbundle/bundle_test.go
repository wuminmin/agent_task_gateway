package snapshotbundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/ordinal"
)

func TestCompileWriteAndVerifySnapshotBundle(t *testing.T) {
	input := testCompilerInput()
	bundle, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if bundle.Manifest.RowCount != 2 || bundle.Manifest.ManifestDigest == "" ||
		bundle.Manifest.DictionaryManifest.DictionaryDigest == "" {
		t.Fatalf("unexpected bundle manifest: %#v", bundle.Manifest)
	}
	hot, err := ordinal.ParseHotDictionary(bundle.Hot, bundle.Manifest.ManifestDigest)
	if err != nil {
		t.Fatalf("ParseHotDictionary: %v", err)
	}
	if _, err := ordinal.ParseColdDictionary(bundle.Cold, bundle.Manifest.ManifestDigest); err != nil {
		t.Fatalf("ParseColdDictionary: %v", err)
	}
	expectation := SidecarExpectation{PublicationName: input.PublicationName, OrdinalSidecar: input.OrdinalSidecar,
		SourceNamespace: input.Snapshot.SourceNamespace, ManifestDigest: bundle.Manifest.ManifestDigest,
		SidecarDigest: bundle.Manifest.DictionaryManifest.SidecarDigest}
	if err := VerifySidecarNDJSON(bytes.NewReader(bundle.Sidecar), hot, expectation); err != nil {
		t.Fatalf("VerifySidecarNDJSON: %v", err)
	}

	base := t.TempDir()
	publicationDirectory, err := bundle.Write(base)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if publicationDirectory != filepath.Join(base, input.PublicationName) {
		t.Fatalf("publication directory = %q", publicationDirectory)
	}
	manifestFile, err := os.Open(filepath.Join(publicationDirectory, input.PublicationName+".bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeBundleManifest(manifestFile)
	_ = manifestFile.Close()
	if err != nil || decoded.ManifestDigest != bundle.Manifest.ManifestDigest {
		t.Fatalf("DecodeBundleManifest = %#v, %v", decoded, err)
	}
	if _, err := bundle.Write(base); err == nil {
		t.Fatal("Write overwrote an existing immutable publication")
	}
	if got, err := bundle.WriteIdempotent(base); err != nil || got != publicationDirectory {
		t.Fatalf("WriteIdempotent identical = %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(publicationDirectory, bundle.Manifest.Hot.Name), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.WriteIdempotent(base); err == nil || !strings.Contains(err.Error(), "not identical") {
		t.Fatalf("WriteIdempotent accepted tampered publication: %v", err)
	}
}

func TestWriteIdempotentRejectsAdditionalArtifact(t *testing.T) {
	bundle, err := Compile(testCompilerInput())
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	directory, err := bundle.Write(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unexpected"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.WriteIdempotent(base); err == nil || !strings.Contains(err.Error(), "not identical") {
		t.Fatalf("WriteIdempotent accepted additional artifact: %v", err)
	}
}

func TestCompilerRecomputesEntityKeysAndExpectedDigests(t *testing.T) {
	input := testCompilerInput()
	input.Snapshot.Rows[0].EntityKey = strings.Repeat("f", 64)
	if _, err := Compile(input); err == nil || !strings.Contains(err.Error(), "entity key assertion") {
		t.Fatalf("forged entity key error = %v", err)
	}

	input = testCompilerInput()
	input.ExpectedDigests.ManifestDigest = strings.Repeat("a", 64)
	if _, err := Compile(input); err == nil || !strings.Contains(err.Error(), "expected manifest digest") {
		t.Fatalf("expected digest mismatch = %v", err)
	}
}

func TestCompilerAllowsOnePhysicalFieldInDistinctCanonicalSegments(t *testing.T) {
	input := testCompilerInput()
	input.Snapshot.Fields = append(input.Snapshot.Fields,
		SnapshotField{Name: "amount", CanonicalFieldID: "amount", SQLType: "numeric"})
	bundle, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile role/raw segments: %v", err)
	}
	segments := make(map[string]struct{})
	for _, segment := range bundle.Manifest.DictionaryManifest.Segments {
		segments[segment.ID] = struct{}{}
	}
	for _, expected := range []string{"cell:amount", "cell:expense.amount"} {
		if _, found := segments[expected]; !found {
			t.Fatalf("compiled segments miss %q: %#v", expected, segments)
		}
	}
}

func TestSidecarVerificationRejectsNonCanonicalOrRemappedRows(t *testing.T) {
	bundle, err := Compile(testCompilerInput())
	if err != nil {
		t.Fatal(err)
	}
	hot, _ := ordinal.ParseHotDictionary(bundle.Hot, bundle.Manifest.ManifestDigest)
	expectation := SidecarExpectation{PublicationName: bundle.Manifest.PublicationName,
		OrdinalSidecar: bundle.Manifest.OrdinalSidecar, SourceNamespace: bundle.Manifest.DictionaryManifest.SourceNamespace,
		ManifestDigest: bundle.Manifest.ManifestDigest, SidecarDigest: bundle.Manifest.DictionaryManifest.SidecarDigest}
	lines := bytes.Split(bytes.TrimSuffix(bundle.Sidecar, []byte{'\n'}), []byte{'\n'})
	var row SidecarRow
	if err := decodeStrictJSON(bytes.NewReader(lines[1]), &row); err != nil {
		t.Fatal(err)
	}
	row.RowHandle = 2
	lines[1], _ = json.Marshal(row)
	tampered := append(bytes.Join(lines, []byte{'\n'}), '\n')
	if err := VerifySidecarNDJSON(bytes.NewReader(tampered), hot, expectation); err == nil {
		t.Fatal("remapped row handle passed sidecar verification")
	}

	spaced := bytes.Replace(bundle.Sidecar, []byte(`{"type":"header"`), []byte(`{ "type":"header"`), 1)
	if err := VerifySidecarNDJSON(bytes.NewReader(spaced), hot, expectation); err == nil {
		t.Fatal("non-canonical NDJSON passed verification")
	}
}

func TestDecodeCompilerInputIsStrictAndByteaIsExplicit(t *testing.T) {
	raw := `{"version":"taskgate-snapshot-index-input-v1","unknown":true}`
	if _, err := DecodeCompilerInput(strings.NewReader(raw)); err == nil {
		t.Fatal("unknown compiler input field was accepted")
	}
	decoded, err := normalizeJSONValue("bytea", "base64:AQI=")
	if err != nil || !bytes.Equal(decoded.([]byte), []byte{1, 2}) {
		t.Fatalf("normalize bytea = %#v, %v", decoded, err)
	}
	if _, err := normalizeJSONValue("bytea", "AQI="); err == nil {
		t.Fatal("ambiguous bytea JSON was accepted")
	}
}

func TestCompilerRejectsUnrestrictedSourceRelation(t *testing.T) {
	input := testCompilerInput()
	input.SourceRelation = "public.expense_detail"
	if _, err := Compile(input); err == nil || !strings.Contains(err.Error(), "identity is incomplete") {
		t.Fatalf("Compile accepted unrestricted source relation: %v", err)
	}
	if _, _, err := splitSourceRelation("reporting.expense_detail;drop_table"); err == nil {
		t.Fatal("source relation parser accepted non-identifier content")
	}
}

func TestFrozenSourceRelationRequiresNOLOGINOwnerAndSafeReader(t *testing.T) {
	safe := frozenSourceRelationState{
		RelationKind: "m", Populated: true, OwnerName: "snapshot_owner", CanSelect: true,
	}
	if err := validateFrozenSourceRelation(safe); err != nil {
		t.Fatalf("safe frozen relation rejected: %v", err)
	}
	loginOwner := safe
	loginOwner.OwnerCanLogin = true
	if err := validateFrozenSourceRelation(loginOwner); err == nil || !strings.Contains(err.Error(), "NOLOGIN") {
		t.Fatalf("LOGIN owner accepted: %v", err)
	}
	unsafeReader := safe
	unsafeReader.CanWrite = true
	if err := validateFrozenSourceRelation(unsafeReader); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("writable scanner accepted: %v", err)
	}
}

func TestCompilerByteaLazyCopyDoesNotMutateCallerRows(t *testing.T) {
	input := testCompilerInput()
	input.Snapshot.Fields = append(input.Snapshot.Fields, SnapshotField{Name: "attachment", SQLType: "bytea"})
	for index := range input.Snapshot.Rows {
		input.Snapshot.Rows[index].Values["attachment"] = "base64:AQI="
	}
	originalMap := input.Snapshot.Rows[0].Values

	spec, _, rowsByEntity, err := prepareCompilerInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if got := originalMap["attachment"]; got != "base64:AQI=" {
		t.Fatalf("compiler mutated caller bytea transport value: %#v", got)
	}
	if len(spec.Rows) != 2 {
		t.Fatalf("prepared rows = %d, want 2", len(spec.Rows))
	}
	for _, row := range spec.Rows {
		decoded, ok := row.Values["attachment"].([]byte)
		if !ok || !bytes.Equal(decoded, []byte{1, 2}) {
			t.Fatalf("prepared bytea = %#v", row.Values["attachment"])
		}
		keyValues, ok := rowsByEntity[row.EntityKey]
		if !ok || len(keyValues) != 1 {
			t.Fatalf("compact sidecar key row = %#v", keyValues)
		}
	}
	if _, err := Compile(input); err != nil {
		t.Fatalf("compile bytea snapshot: %v", err)
	}
}

func TestCompileOwnedMatchesCompileAndReleasesInputRows(t *testing.T) {
	baselineInput := testCompilerInput()
	baseline, err := Compile(baselineInput)
	if err != nil {
		t.Fatal(err)
	}
	ownedInput := testCompilerInput()
	owned, err := CompileOwned(&ownedInput)
	if err != nil {
		t.Fatal(err)
	}
	if ownedInput.Snapshot.Rows != nil {
		t.Fatal("CompileOwned retained caller snapshot rows")
	}
	if !reflect.DeepEqual(owned.Manifest, baseline.Manifest) || !bytes.Equal(owned.Hot, baseline.Hot) ||
		!bytes.Equal(owned.Cold, baseline.Cold) || !bytes.Equal(owned.Sidecar, baseline.Sidecar) {
		t.Fatal("owned compilation changed canonical publication bytes")
	}
}

func TestCompileOwnedToDirectoryMatchesInMemoryOracle(t *testing.T) {
	oracle, err := Compile(testCompilerInput())
	if err != nil {
		t.Fatal(err)
	}
	input := testCompilerInput()
	base := t.TempDir()
	written, err := CompileOwnedToDirectory(&input, base, PublicationLimits{
		MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.Snapshot.Rows != nil {
		t.Fatal("streaming compiler retained caller snapshot rows")
	}
	if !reflect.DeepEqual(written.Manifest, oracle.Manifest) || written.Directory != filepath.Join(base, oracle.Manifest.PublicationName) {
		t.Fatalf("written publication differs: %#v", written)
	}
	manifestJSON, err := oracle.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := map[string][]byte{
		oracle.Manifest.Hot.Name: oracle.Hot, oracle.Manifest.Cold.Name: oracle.Cold,
		oracle.Manifest.Sidecar.Name:                     oracle.Sidecar,
		oracle.Manifest.PublicationName + ".bundle.json": manifestJSON,
	}
	var total int64
	for name, want := range wantFiles {
		got, err := os.ReadFile(filepath.Join(written.Directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("streamed file %q differs from in-memory oracle", name)
		}
		total += int64(len(got))
	}
	if written.Bytes != total {
		t.Fatalf("written bytes = %d, want %d", written.Bytes, total)
	}
}

func TestCompileOwnedToDirectoryCleansStagingAfterFailure(t *testing.T) {
	for _, failedStage := range []string{"hot", "cold", "sidecar", "manifest"} {
		t.Run(failedStage, func(t *testing.T) {
			input := testCompilerInput()
			base := t.TempDir()
			_, err := compileOwnedToDirectory(input, base, PublicationLimits{
				MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes),
			}, func(stage string) error {
				if stage == failedStage {
					return errors.New("fault")
				}
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "after "+failedStage) {
				t.Fatalf("injected failure = %v", err)
			}
			entries, readErr := os.ReadDir(base)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed publication left staging or activation files: %#v", entries)
			}
		})
	}
}

func TestCompileOwnedToDirectoryRenameNeverReplacesConcurrentFinal(t *testing.T) {
	input := testCompilerInput()
	base := t.TempDir()
	final := filepath.Join(base, input.PublicationName)
	_, err := compileOwnedToDirectory(input, base, PublicationLimits{
		MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes),
	}, func(stage string) error {
		if stage != "manifest" {
			return nil
		}
		if err := os.Mkdir(final, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(final, "owner"), []byte("concurrent"), 0o600)
	})
	if err == nil || !strings.Contains(err.Error(), "without replacement") {
		t.Fatalf("no-replace publication error = %v", err)
	}
	owner, readErr := os.ReadFile(filepath.Join(final, "owner"))
	if readErr != nil || string(owner) != "concurrent" {
		t.Fatalf("concurrent final was replaced: %q, %v", owner, readErr)
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != input.PublicationName {
		t.Fatalf("staging cleanup after no-replace failure = %#v", entries)
	}
}

func TestCompileOwnedToDirectoryExistingIdenticalIsStrict(t *testing.T) {
	base := t.TempDir()
	input := testCompilerInput()
	limits := DefaultPublicationLimits()
	first, err := CompileOwnedToDirectory(&input, base, limits)
	if err != nil {
		t.Fatal(err)
	}
	limits.AllowExistingIdentical = true
	input = testCompilerInput()
	second, err := CompileOwnedToDirectory(&input, base, limits)
	if err != nil || !reflect.DeepEqual(second.Manifest, first.Manifest) || second.Directory != first.Directory {
		t.Fatalf("identical publication = %#v, %v", second, err)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 1 || entries[0].Name() != first.Manifest.PublicationName {
		t.Fatalf("idempotent publication left staging: %#v, %v", entries, err)
	}
	if err := os.Chmod(first.Directory, 0o777); err != nil {
		t.Fatal(err)
	}
	input = testCompilerInput()
	if _, err := CompileOwnedToDirectory(&input, base, limits); err == nil ||
		!strings.Contains(err.Error(), "unsafe existing publication") {
		t.Fatalf("writable existing publication accepted: %v", err)
	}
	if err := os.Chmod(first.Directory, 0o700); err != nil {
		t.Fatal(err)
	}

	hotPath := filepath.Join(first.Directory, first.Manifest.Hot.Name)
	if err := os.Chmod(hotPath, 0o666); err != nil {
		t.Fatal(err)
	}
	input = testCompilerInput()
	if _, err := CompileOwnedToDirectory(&input, base, limits); err == nil ||
		!strings.Contains(err.Error(), "unsafe artifact permissions") {
		t.Fatalf("writable existing artifact accepted: %v", err)
	}
	if err := os.Chmod(hotPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hotPath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	input = testCompilerInput()
	if _, err := CompileOwnedToDirectory(&input, base, limits); err == nil ||
		!strings.Contains(err.Error(), "not identical") {
		t.Fatalf("tampered existing publication accepted: %v", err)
	}
	tampered, err := os.ReadFile(hotPath)
	if err != nil || string(tampered) != "tampered" {
		t.Fatalf("tampered existing publication was overwritten: %q, %v", tampered, err)
	}
	entries, err = os.ReadDir(base)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed identical check left staging: %#v, %v", entries, err)
	}
}

func TestCompileOwnedToDirectoryEnforcesExactHotAndTotalBounds(t *testing.T) {
	oracle, err := Compile(testCompilerInput())
	if err != nil {
		t.Fatal(err)
	}
	manifestJSON, err := oracle.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	total := int64(len(oracle.Hot) + len(oracle.Cold) + len(oracle.Sidecar) + len(manifestJSON))
	hot := int64(len(oracle.Hot))
	for name, limits := range map[string]PublicationLimits{
		"hot-b-minus-1":   {MaxBytes: total + 1, MaxHotBytes: hot - 1},
		"total-b-minus-1": {MaxBytes: total - 1, MaxHotBytes: hot},
	} {
		t.Run(name, func(t *testing.T) {
			input := testCompilerInput()
			base := t.TempDir()
			if _, err := CompileOwnedToDirectory(&input, base, limits); !errors.Is(err, errPublishedSizeLimit) {
				t.Fatalf("boundary failure = %v, want size limit", err)
			}
			entries, readErr := os.ReadDir(base)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("bounded failure left publication: %#v, %v", entries, readErr)
			}
		})
	}
	for name, limits := range map[string]PublicationLimits{
		"exact-b":  {MaxBytes: total, MaxHotBytes: hot},
		"b-plus-1": {MaxBytes: total + 1, MaxHotBytes: hot + 1},
	} {
		t.Run(name, func(t *testing.T) {
			input := testCompilerInput()
			written, err := CompileOwnedToDirectory(&input, t.TempDir(), limits)
			if err != nil || written.Bytes != total {
				t.Fatalf("boundary success = %#v, %v; want bytes=%d", written, err, total)
			}
		})
	}
	input := testCompilerInput()
	if _, err := CompileOwnedToDirectory(&input, t.TempDir(), PublicationLimits{
		MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes) + 1,
	}); err == nil || !strings.Contains(err.Error(), "limits are invalid") {
		t.Fatalf("core accepted HOT cap above 160 MiB: %v", err)
	}
}

func TestCompileOwnedToDirectoryRejectsSymlinkedBaseAndStagedArtifact(t *testing.T) {
	realBase := t.TempDir()
	symlink := filepath.Join(t.TempDir(), "artifacts")
	if err := os.Symlink(realBase, symlink); err != nil {
		t.Fatal(err)
	}
	input := testCompilerInput()
	if _, err := CompileOwnedToDirectory(&input, symlink, PublicationLimits{
		MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes),
	}); err == nil || !strings.Contains(err.Error(), "not a regular directory") {
		t.Fatalf("symlinked base accepted: %v", err)
	}

	input = testCompilerInput()
	base := t.TempDir()
	_, err := compileOwnedToDirectory(input, base, PublicationLimits{
		MaxBytes: int64(maxPublishedBytes), MaxHotBytes: int64(maxHotPublishedBytes),
	}, func(stage string) error {
		if stage != "hot" {
			return nil
		}
		entries, err := os.ReadDir(base)
		if err != nil || len(entries) != 1 {
			return errors.New("staging directory not found")
		}
		hotPath := filepath.Join(base, entries[0].Name(), input.PublicationName+".hot.tgord")
		if err := os.Remove(hotPath); err != nil {
			return err
		}
		return os.Symlink("/dev/null", hotPath)
	})
	if err == nil || !strings.Contains(err.Error(), "expected regular file") {
		t.Fatalf("symlinked staged HOT accepted: %v", err)
	}
	entries, readErr := os.ReadDir(base)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("symlink failure left staging: %#v, %v", entries, readErr)
	}
}

func TestCompileOwnedToDirectoryRejectsWritablePublicationBase(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	input := testCompilerInput()
	if _, err := CompileOwnedToDirectory(&input, base, DefaultPublicationLimits()); err == nil ||
		!strings.Contains(err.Error(), "group- or world-writable") {
		t.Fatalf("writable publication base accepted: %v", err)
	}
}

func TestStreamedFileStopsBeforeWriteBudget(t *testing.T) {
	directory := t.TempDir()
	opened, err := openRegularDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	descriptor, err := writeStreamedFile(opened, "bounded", 3, func(writer io.Writer) error {
		_, writeErr := writer.Write([]byte("four"))
		return writeErr
	})
	if !errors.Is(err, errPublishedSizeLimit) || descriptor != (FileDescriptor{}) {
		t.Fatalf("bounded writer = %#v, %v", descriptor, err)
	}
	info, statErr := os.Stat(filepath.Join(directory, "bounded"))
	if statErr != nil || info.Size() != 0 {
		t.Fatalf("bounded writer crossed limit: size=%v, err=%v", info, statErr)
	}
}

func TestVerifyOpenDirectoryIdentityClosedDescriptorFailsWithoutPanic(t *testing.T) {
	directory := t.TempDir()
	opened, err := openRegularDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verifyOpenDirectoryIdentity(directory, opened); err == nil {
		t.Fatal("closed directory descriptor passed identity verification")
	}
}

func TestDemoSnapshotCandidateDigestsRemainUnshardedAndStable(t *testing.T) {
	for _, name := range []string{"expense-detail-v1.json", "expense-summary-v1.json"} {
		t.Run(name, func(t *testing.T) {
			inputFile, err := os.Open(filepath.Join("..", "..", "config", "snapshots", name))
			if err != nil {
				t.Fatal(err)
			}
			input, err := DecodeCompilerInput(inputFile)
			closeErr := inputFile.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			bundle, err := Compile(input)
			if err != nil {
				t.Fatalf("compile pinned candidate rows: %v", err)
			}
			if bundle.Manifest.ManifestDigest != input.ExpectedDigests.ManifestDigest ||
				bundle.Manifest.DictionaryManifest.DictionaryDigest != input.ExpectedDigests.DictionaryDigest ||
				bundle.Manifest.DictionaryManifest.HotIndexDigest != input.ExpectedDigests.HotIndexDigest {
				t.Fatal("small publication digest changed after adding prefix sharding")
			}
			for _, segment := range bundle.Manifest.DictionaryManifest.Segments {
				expectedID := "row"
				if segment.Kind == "base-cell" {
					expectedID = "cell:" + segment.Field
				}
				if segment.ID != expectedID || segment.Shard != 0 {
					t.Fatalf("small publication unexpectedly sharded segment %#v", segment)
				}
			}
		})
	}
}

func testCompilerInput() CompilerInput {
	return CompilerInput{
		Version: CompilerInputVersion, PublicationName: "expense-detail-v1", CatalogSource: "travel_demo",
		OrdinalSidecar: "taskgate_ordinal.expense_detail_v1", EntityKeyFields: []string{"receipt_no"},
		Snapshot: SnapshotInput{SourceID: "travel-demo", SourceNamespace: "travel.expense_receipt",
			Snapshot: "travel-demo-2026-v1", SchemaDigest: strings.Repeat("1", 64),
			Fields: []SnapshotField{
				{Name: "receipt_no", SQLType: "text", Collation: "C", CollationVersion: "builtin"},
				{Name: "amount", CanonicalFieldID: "expense.amount", SQLType: "numeric"},
			},
			Rows: []SnapshotRow{
				{Values: map[string]any{"receipt_no": "R-002", "amount": json.Number("20.00")}},
				{Values: map[string]any{"receipt_no": "R-001", "amount": json.Number("10.0")}},
			}},
	}
}
