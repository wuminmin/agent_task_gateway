package ordinal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

func snapshotTestSpec(canonicalFieldID string, reverse bool) SnapshotSpec {
	rows := []SnapshotRow{
		{EntityKey: "row-b", Values: map[string]any{"amount": "20.00"}},
		{EntityKey: "row-a", Values: map[string]any{"amount": "10"}},
	}
	if reverse {
		rows[0], rows[1] = rows[1], rows[0]
	}
	return SnapshotSpec{
		SourceID: "business", SourceNamespace: "travel.expense", Snapshot: "snapshot-1",
		SchemaDigest: testDigest("1"),
		Fields:       []SnapshotField{{Name: "amount", CanonicalFieldID: canonicalFieldID, SQLType: "numeric"}}, Rows: rows,
	}
}

func TestSnapshotCompilerSupportsRawAndQualifiedAliasesFromOneInputColumn(t *testing.T) {
	spec := snapshotTestSpec("", false)
	spec.Fields = []SnapshotField{
		{Name: "amount", CanonicalFieldID: "amount", SQLType: "numeric"},
		{Name: "amount", CanonicalFieldID: "expense.amount", SQLType: "numeric"},
	}
	artifact, err := CompileSnapshotArtifact(spec)
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	row, found := artifact.Hot.LookupEntity("row-a")
	if !found || len(row.Cells) != 2 {
		t.Fatalf("aliased row = %#v, found=%v", row, found)
	}
	for _, fieldID := range []string{"amount", "expense.amount"} {
		ref, exists := row.Cells[fieldID]
		fact, expandErr := artifact.Cold.Expand(ref)
		if !exists || expandErr != nil || fact.Field != fieldID || fact.CanonicalValue != "n:10" {
			t.Fatalf("alias %q ref=%#v fact=%#v exists=%v err=%v", fieldID, ref, fact, exists, expandErr)
		}
	}
}

func TestCompileEmptySnapshotRetainsVerifiedZeroCardinalitySegments(t *testing.T) {
	spec := snapshotTestSpec("", false)
	spec.Rows = nil
	artifact, err := CompileSnapshotArtifact(spec)
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact(empty): %v", err)
	}
	if artifact.Hot.RowCount() != 0 {
		t.Fatalf("empty snapshot row count = %d", artifact.Hot.RowCount())
	}
	manifest := artifact.Hot.Manifest()
	if len(manifest.Segments) != 2 || manifest.Segments[0].FactCount != 0 || manifest.Segments[1].FactCount != 0 {
		t.Fatalf("empty snapshot segments = %#v", manifest.Segments)
	}
	hotBytes, _ := artifact.Hot.MarshalBinary()
	if _, err := ParseHotDictionary(hotBytes, artifact.Hot.ManifestDigest()); err != nil {
		t.Fatalf("parse empty hot artifact: %v", err)
	}
	coldBytes, _ := artifact.Cold.MarshalBinary()
	if _, err := ParseColdDictionary(coldBytes, artifact.Hot.ManifestDigest()); err != nil {
		t.Fatalf("parse empty cold artifact: %v", err)
	}
}

func TestCompileSnapshotIsDeterministicAndSplitsHotCold(t *testing.T) {
	first, err := CompileSnapshotArtifact(snapshotTestSpec("", false))
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact(first): %v", err)
	}
	second, err := CompileSnapshotArtifact(snapshotTestSpec("", true))
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact(second): %v", err)
	}
	if first.Hot.DictionaryDigest() != second.Hot.DictionaryDigest() || first.Hot.ManifestDigest() != second.Hot.ManifestDigest() {
		t.Fatalf("row order changed dictionary: %s/%s vs %s/%s", first.Hot.DictionaryDigest(), first.Hot.ManifestDigest(),
			second.Hot.DictionaryDigest(), second.Hot.ManifestDigest())
	}
	handle, found := first.Hot.LookupRowHandle("row-a")
	if !found || handle != 1 {
		t.Fatalf("canonical row-a handle = %d, found=%v; want 1,true", handle, found)
	}
	row, found := first.Hot.LookupRow(handle)
	if !found || row.EntityKey != "row-a" || row.Cells["amount"].SegmentID != "cell:amount" {
		t.Fatalf("unexpected hot row index: %#v, found=%v", row, found)
	}
	fact, err := first.Cold.Expand(row.Cells["amount"])
	if err != nil {
		t.Fatalf("cold Expand: %v", err)
	}
	want, _ := exposure.NewBaseCellFactV2("travel.expense", "snapshot-1", "row-a", "amount", "numeric", "10.0")
	gotHash, _ := fact.Hash()
	wantHash, _ := want.Hash()
	if gotHash != wantHash {
		t.Fatalf("compiled FactID hash = %s, want existing semantics %s", gotHash, wantHash)
	}
	hotHash, err := first.Hot.Hash(row.Cells["amount"])
	if err != nil || hex.EncodeToString(hotHash[:]) != wantHash {
		t.Fatalf("hot hash = %x, err=%v, want %s", hotHash, err, wantHash)
	}
	payload, _ := first.Cold.CanonicalPayload(row.Cells["amount"])
	wantPayload, _ := want.CanonicalPayload()
	if !bytes.Equal(payload, wantPayload) {
		t.Fatal("cold artifact did not preserve the canonical FactID payload")
	}
}

func TestSnapshotCompilerPreservesRawAndRoleQualifiedFieldSemantics(t *testing.T) {
	for _, test := range []struct {
		name      string
		canonical string
	}{
		{"one product scan uses raw field", ""},
		{"relational scan uses stable-role field", "expense.amount"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact, err := CompileSnapshotArtifact(snapshotTestSpec(test.canonical, false))
			if err != nil {
				t.Fatalf("CompileSnapshotArtifact: %v", err)
			}
			fieldID := test.canonical
			if fieldID == "" {
				fieldID = "amount"
			}
			row, found := artifact.Hot.LookupEntity("row-b")
			if !found {
				t.Fatal("hot lookup missed row-b")
			}
			ref, found := row.Cells[fieldID]
			if !found {
				t.Fatalf("hot row missed canonical field %q: %#v", fieldID, row.Cells)
			}
			fact, err := artifact.Cold.Expand(ref)
			if err != nil || fact.Field != fieldID {
				t.Fatalf("expanded field = %q, err=%v, want %q", fact.Field, err, fieldID)
			}
			legacy, _ := exposure.NewBaseCellFactV2("travel.expense", "snapshot-1", "row-b", fieldID, "numeric", "20")
			legacyHash, _ := legacy.Hash()
			compiledHash, _ := fact.Hash()
			if compiledHash != legacyHash {
				t.Fatalf("compiler changed legacy FactID: %s != %s", compiledHash, legacyHash)
			}
		})
	}
}

func TestDecodeBoundsRegistryAndHashOrderedStreaming(t *testing.T) {
	artifact, err := CompileSnapshotArtifact(snapshotTestSpec("expense.amount", false))
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	rowA, _ := artifact.Hot.LookupEntity("row-a")
	rowB, _ := artifact.Hot.LookupEntity("row-b")
	set, err := NewBitmapSet(rowA.Row, rowA.Cells["expense.amount"], rowB.Row, rowB.Cells["expense.amount"])
	if err != nil {
		t.Fatalf("NewBitmapSet: %v", err)
	}
	decoded, err := Decode(set, artifact.Cold)
	if err != nil || len(decoded) != int(set.Cardinality()) {
		t.Fatalf("Decode cardinality = %d, err=%v, want %d", len(decoded), err, set.Cardinality())
	}
	if err := artifact.Hot.ValidateSetBounds(set); err != nil {
		t.Fatalf("ValidateSetBounds(valid): %v", err)
	}
	count, _ := artifact.Hot.SegmentFactCount(rowA.Row.SegmentID)
	invalid, _ := NewBitmapSet(FactRef{DictionaryDigest: artifact.Hot.DictionaryDigest(), SegmentID: rowA.Row.SegmentID, Ordinal: uint32(count)})
	if err := artifact.Hot.ValidateSetBounds(invalid); err == nil {
		t.Fatal("out-of-range ordinal passed fail-closed bounds validation")
	}

	var hashes [][]byte
	if err := artifact.Hot.StreamHashesByFactHash(set, func(_ FactRef, hash [32]byte) error {
		hashes = append(hashes, append([]byte(nil), hash[:]...))
		return nil
	}); err != nil {
		t.Fatalf("StreamHashesByFactHash: %v", err)
	}
	if !sort.SliceIsSorted(hashes, func(i, j int) bool { return bytes.Compare(hashes[i], hashes[j]) < 0 }) {
		t.Fatalf("hash stream is not globally sorted: %x", hashes)
	}

	registry, err := NewRegistry(artifact.Hot)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	key := PublicationKey{CatalogDigest: testDigest("9"), PublicationName: "expense-v1"}
	if err := registry.RegisterPublication(key, artifact.Hot.ManifestDigest(), artifact.Hot); err != nil {
		t.Fatalf("RegisterPublication: %v", err)
	}
	resolved, err := registry.Resolve(key)
	if err != nil || resolved.DictionaryDigest() != artifact.Hot.DictionaryDigest() {
		t.Fatalf("Resolve = %#v, err=%v", resolved, err)
	}
	if _, err := registry.Resolve(PublicationKey{CatalogDigest: testDigest("8"), PublicationName: "expense-v1"}); err == nil {
		t.Fatal("cross-catalog publication lookup was accepted")
	}
}

func TestHotAndColdArtifactsRoundTripAndRejectTampering(t *testing.T) {
	artifact, err := CompileSnapshotArtifact(snapshotTestSpec("expense.amount", false))
	if err != nil {
		t.Fatalf("CompileSnapshotArtifact: %v", err)
	}
	hotBytes, err := artifact.Hot.MarshalBinary()
	if err != nil {
		t.Fatalf("Marshal hot: %v", err)
	}
	secondHotBytes, _ := artifact.Hot.MarshalBinary()
	if !bytes.Equal(hotBytes, secondHotBytes) {
		t.Fatal("hot artifact serialization is nondeterministic")
	}
	loadedHot, err := ParseHotDictionary(hotBytes, artifact.Hot.ManifestDigest())
	if err != nil {
		t.Fatalf("ParseHotDictionary: %v", err)
	}
	row, found := loadedHot.LookupEntity("row-a")
	if !found || row.Cells["expense.amount"].SegmentID == "" {
		t.Fatalf("loaded hot row = %#v, found=%v", row, found)
	}

	withoutEntities, err := artifact.Hot.MarshalBinaryWithoutEntityKeys()
	if err != nil {
		t.Fatalf("MarshalBinaryWithoutEntityKeys: %v", err)
	}
	loadedHandleOnly, err := ParseHotDictionary(withoutEntities, artifact.Hot.ManifestDigest())
	if err != nil {
		t.Fatalf("Parse handle-only hot: %v", err)
	}
	if _, found := loadedHandleOnly.LookupRowHandle("row-a"); found {
		t.Fatal("handle-only hot artifact retained entity-key lookup")
	}
	if handleRow, found := loadedHandleOnly.LookupRow(1); !found || handleRow.Cells["expense.amount"].SegmentID == "" {
		t.Fatalf("handle-only row = %#v, found=%v", handleRow, found)
	}

	coldBytes, err := artifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatalf("Marshal cold: %v", err)
	}
	secondColdBytes, _ := artifact.Cold.MarshalBinary()
	if !bytes.Equal(coldBytes, secondColdBytes) {
		t.Fatal("cold artifact serialization is nondeterministic")
	}
	loadedCold, err := ParseColdDictionary(coldBytes, artifact.Hot.ManifestDigest())
	if err != nil {
		t.Fatalf("ParseColdDictionary: %v", err)
	}
	if _, err := loadedCold.Expand(row.Cells["expense.amount"]); err != nil {
		t.Fatalf("loaded cold Expand: %v", err)
	}

	for name, test := range map[string]struct {
		bytes []byte
		parse func([]byte) error
	}{
		"hot": {hotBytes, func(value []byte) error {
			_, parseErr := ParseHotDictionary(value, artifact.Hot.ManifestDigest())
			return parseErr
		}},
		"cold": {coldBytes, func(value []byte) error {
			_, parseErr := ParseColdDictionary(value, artifact.Hot.ManifestDigest())
			return parseErr
		}},
	} {
		t.Run(name+" tamper", func(t *testing.T) {
			tampered := append([]byte(nil), test.bytes...)
			tampered[len(tampered)/2] ^= 1
			if parseErr := test.parse(tampered); !errors.Is(parseErr, ErrDigestMismatch) {
				t.Fatalf("tamper error = %v, want ErrDigestMismatch", parseErr)
			}
		})
	}
}

func TestOwnedSplitIsBytewiseEquivalentAndTransfersOnlyItsSource(t *testing.T) {
	spec := snapshotTestSpec("expense.amount", false)
	copySource, err := CompileSnapshot(spec)
	if err != nil {
		t.Fatal(err)
	}
	copyArtifact, err := copySource.Split()
	if err != nil {
		t.Fatal(err)
	}
	ownedSource, err := CompileSnapshot(spec)
	if err != nil {
		t.Fatal(err)
	}
	segmentID := ownedSource.manifest.Segments[0].ID
	ownedPayload := ownedSource.segments[segmentID][0].payload
	copyPayload := copySource.segments[segmentID][0].payload
	ownedArtifact, err := ownedSource.splitOwned()
	if err != nil {
		t.Fatal(err)
	}
	if len(ownedSource.segments) != 0 {
		t.Fatalf("owned split retained %d source segments", len(ownedSource.segments))
	}
	if len(ownedPayload) == 0 || &ownedPayload[0] != &ownedArtifact.Cold.segments[segmentID][0].payload[0] {
		t.Fatal("owned split cloned rather than transferred the immutable payload")
	}
	if len(copyPayload) == 0 || &copyPayload[0] == &copyArtifact.Cold.segments[segmentID][0].payload[0] {
		t.Fatal("public Split transferred payload ownership from a still-usable dictionary")
	}
	if len(copySource.segments) != len(copySource.manifest.Segments) {
		t.Fatal("public Split consumed its source dictionary")
	}

	copyHot, err := copyArtifact.Hot.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	ownedHot, err := ownedArtifact.Hot.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	copyCold, err := copyArtifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	ownedCold, err := ownedArtifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copyHot, ownedHot) || !bytes.Equal(copyCold, ownedCold) ||
		copyArtifact.Hot.ManifestDigest() != ownedArtifact.Hot.ManifestDigest() {
		t.Fatal("owned split changed canonical artifact bytes or manifest digest")
	}
}

func TestCompileSharedPayloadStillRejectsDuplicateFactPositions(t *testing.T) {
	fact, err := exposure.NewBaseRowFactV2("travel.expense", "snapshot-1", "row-a")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compile(DictionarySpec{
		SourceID: "business", SourceNamespace: "travel.expense", Snapshot: "snapshot-1", SchemaDigest: testDigest("1"),
		Segments: []SegmentSpec{
			{ID: "row-a", Kind: SegmentBaseRow, Facts: []exposure.FactID{fact}},
			{ID: "row-b", Kind: SegmentBaseRow, Facts: []exposure.FactID{fact}},
		},
	})
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrFactCollision) {
		t.Fatalf("duplicate canonical fact error = %v, want duplicate-position ErrInvalid", err)
	}
}

func TestSealArtifactInPlacePreservesCanonicalBytes(t *testing.T) {
	domain := "test-artifact-domain\x00"
	body := make([]byte, len("canonical-body"), len("canonical-body")+sha256.Size)
	copy(body, "canonical-body")
	originalAddress := &body[0]
	hash := sha256.New()
	hash.Write([]byte(domain))
	hash.Write(body)
	expected := append(append([]byte(nil), body...), hash.Sum(nil)...)

	sealed := sealArtifact(domain, body)
	if &sealed[0] != originalAddress {
		t.Fatal("sealArtifact did not use available exclusive capacity")
	}
	if !bytes.Equal(sealed, expected) {
		t.Fatalf("sealed bytes changed: %x != %x", sealed, expected)
	}
	opened, err := openArtifact(domain, sealed)
	if err != nil || !bytes.Equal(opened, []byte("canonical-body")) {
		t.Fatalf("open sealed artifact = %q, %v", opened, err)
	}
}

func TestStreamingColdVerificationMatchesRetainingParser(t *testing.T) {
	artifact, err := CompileSnapshotArtifact(snapshotTestSpec("expense.amount", false))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := artifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	digest := artifact.Hot.ManifestDigest()
	guarded := &boundedReadReader{reader: bytes.NewReader(encoded), maximum: artifactStreamBufferBytes}
	if err := VerifyColdDictionaryReader(guarded, int64(len(encoded)), digest); err != nil {
		t.Fatalf("bounded streaming verification: %v", err)
	}
	if guarded.exceeded {
		t.Fatalf("streaming verifier requested a read larger than %d bytes", artifactStreamBufferBytes)
	}
	envelopeReader := &boundedReadReader{reader: bytes.NewReader(encoded), maximum: artifactStreamBufferBytes}
	envelopeDigest, err := VerifyColdDictionaryEnvelopeReader(envelopeReader, int64(len(encoded)), digest)
	expectedEnvelopeDigest := sha256.Sum256(encoded)
	if err != nil || envelopeDigest != expectedEnvelopeDigest || envelopeReader.exceeded {
		t.Fatalf("bounded envelope verification = %x, %v, oversized=%t", envelopeDigest, err, envelopeReader.exceeded)
	}
	if _, err := VerifyColdDictionaryEnvelopeReader(bytes.NewReader(encoded), int64(len(encoded)), testDigest("f")); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("envelope accepted wrong Catalog manifest: %v", err)
	}

	assertSameClass := func(name string, material []byte, expected string, want error) {
		t.Helper()
		_, parseErr := ParseColdDictionary(material, expected)
		verifyErr := VerifyColdDictionary(material, expected)
		streamErr := VerifyColdDictionaryReader(bytes.NewReader(material), int64(len(material)), expected)
		if (parseErr == nil) != (verifyErr == nil) || (parseErr == nil) != (streamErr == nil) ||
			(want != nil && (!errors.Is(parseErr, want) || !errors.Is(verifyErr, want) || !errors.Is(streamErr, want))) {
			t.Fatalf("%s parser=%v verifier=%v stream=%v, want matching %v classification",
				name, parseErr, verifyErr, streamErr, want)
		}
	}
	assertSameClass("valid", encoded, digest, nil)
	assertSameClass("wrong manifest", encoded, testDigest("f"), ErrDigestMismatch)

	// Change only a canonical payload byte and recompute the transport seal.
	// Both paths must reject before accepting the per-segment summaries.
	canonicalMismatch := mutateFirstColdPayload(t, encoded)
	assertSameClass("canonical payload mismatch", canonicalMismatch, digest, ErrInvalid)

	// Change a canonical value in both its payload and Fact JSON, preserving all
	// length prefixes and the outer seal. Canonical validation succeeds, so the
	// manifest-bound segment/dictionary summaries must catch the change.
	body, err := openArtifact(coldArtifactDomain, encoded)
	if err != nil {
		t.Fatal(err)
	}
	changedBody := bytes.ReplaceAll(append([]byte(nil), body...), []byte("n:10"), []byte("n:11"))
	if bytes.Equal(changedBody, body) {
		t.Fatal("fixture did not contain the expected canonical numeric value")
	}
	assertSameClass("summary mismatch", sealArtifact(coldArtifactDomain, changedBody), digest, ErrDigestMismatch)
}

type boundedReadReader struct {
	reader   *bytes.Reader
	maximum  int
	exceeded bool
}

func (r *boundedReadReader) Read(payload []byte) (int, error) {
	if len(payload) > r.maximum {
		r.exceeded = true
		payload = payload[:r.maximum]
	}
	return r.reader.Read(payload)
}

func mutateFirstColdPayload(t *testing.T, encoded []byte) []byte {
	t.Helper()
	body, err := openArtifact(coldArtifactDomain, encoded)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), body...)
	reader := artifactReader{Reader: bytes.NewReader(changed)}
	if _, err := reader.readFixed(len(coldArtifactMagic)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readBytes(maxManifestBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readString(sha256.Size * 2); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readUint64(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.readString(maxArtifactString); err != nil {
		t.Fatal(err)
	}
	count, err := reader.readUint64()
	if err != nil || count == 0 {
		t.Fatalf("first cold segment count = %d, %v", count, err)
	}
	payloadLength, err := reader.readUint64()
	if err != nil || payloadLength == 0 || payloadLength > uint64(reader.Len()) {
		t.Fatalf("first cold payload length = %d, %v", payloadLength, err)
	}
	payloadOffset := len(changed) - reader.Len()
	changed[payloadOffset] ^= 1
	return sealArtifact(coldArtifactDomain, changed)
}

func TestDictionarySetManifestHasOneSharedCanonicalDigest(t *testing.T) {
	members := []DictionarySetMember{
		{PublicationName: "orders-v1", DictionaryDigest: testDigest("1"), ManifestDigest: testDigest("2")},
		{PublicationName: "lineitem-v1", DictionaryDigest: testDigest("3"), ManifestDigest: testDigest("4")},
	}
	first, err := NewDictionarySetManifest(testDigest("a"), members...)
	if err != nil {
		t.Fatalf("NewDictionarySetManifest: %v", err)
	}
	second, err := NewDictionarySetManifest(testDigest("a"), members[1], members[0])
	if err != nil {
		t.Fatalf("NewDictionarySetManifest(reversed): %v", err)
	}
	firstDigest, _ := first.Digest()
	secondDigest, _ := second.Digest()
	if firstDigest != secondDigest || first.Members[0].PublicationName != "lineitem-v1" {
		t.Fatalf("dictionary set canonicalization failed: %#v %s != %#v %s", first, firstDigest, second, secondDigest)
	}
	if _, err := NewDictionarySetManifest(testDigest("a"), members[0], members[0]); err == nil {
		t.Fatal("duplicate dictionary set member was accepted")
	}
}

func TestMultiIndexStreamsJoinWitnessAcrossDictionaries(t *testing.T) {
	left, err := CompileSnapshotArtifact(snapshotTestSpec("left.amount", false))
	if err != nil {
		t.Fatalf("compile left: %v", err)
	}
	rightSpec := snapshotTestSpec("right.amount", false)
	rightSpec.SourceID = "warehouse"
	rightSpec.SourceNamespace = "travel.warehouse"
	right, err := CompileSnapshotArtifact(rightSpec)
	if err != nil {
		t.Fatalf("compile right: %v", err)
	}
	leftRow, _ := left.Hot.LookupEntity("row-a")
	rightRow, _ := right.Hot.LookupEntity("row-a")
	witness, _ := NewBitmapSet(leftRow.Row, leftRow.Cells["left.amount"], rightRow.Row, rightRow.Cells["right.amount"])
	multi, err := NewMultiIndex(left.Hot, right.Hot)
	if err != nil {
		t.Fatalf("NewMultiIndex: %v", err)
	}
	var previous []byte
	count := 0
	if err := multi.StreamHashesByFactHash(witness, func(_ FactRef, hash [32]byte) error {
		if previous != nil && bytes.Compare(previous, hash[:]) >= 0 {
			t.Fatalf("multi-index hash stream is not strictly sorted")
		}
		previous = append(previous[:0], hash[:]...)
		count++
		return nil
	}); err != nil || count != 4 {
		t.Fatalf("multi-index stream count=%d err=%v", count, err)
	}
}
