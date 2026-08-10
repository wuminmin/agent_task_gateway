package finalv5linker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const fixtureRole = "candidate"

type countingIndex struct {
	ordinal.SnapshotIndex
	hashCalls int
}

func (index *countingIndex) Hash(ref ordinal.FactRef) ([sha256.Size]byte, error) {
	index.hashCalls++
	return index.SnapshotIndex.Hash(ref)
}

type countingPayloads struct {
	base        CanonicalPayloadIndex
	calls       int
	mutate      *ordinal.FactRef
	unavailable *ordinal.FactRef
}

func (payloads *countingPayloads) CanonicalPayload(ref ordinal.FactRef) ([]byte, error) {
	payloads.calls++
	if payloads.unavailable != nil && *payloads.unavailable == ref {
		return nil, errors.New("fixture payload unavailable")
	}
	value, err := payloads.base.CanonicalPayload(ref)
	if err != nil {
		return nil, err
	}
	if payloads.mutate != nil && *payloads.mutate == ref {
		value[len(value)-1] ^= 1
	}
	return value, nil
}

type linkFixture struct {
	oracleFacts []finalv5oracle.CanonicalFact
	allFacts    []finalv5oracle.CanonicalFact
	refs        []ordinal.FactRef
	hot         *ordinal.HotDictionary
	cold        *ordinal.ColdDictionary
	index       *countingIndex
	payloads    *countingPayloads
	request     Request
}

func TestLinkMatchesEverySemanticMemberAndReportsPublicationIdentities(t *testing.T) {
	fixture := newLinkFixture(t)
	report, err := Link(fixture.request)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !report.Match || !report.OrdinalSetEqual {
		t.Fatalf("report does not match: %+v", report.Mismatches)
	}
	if report.ActualOrdinalSource != ActualSetSourceProductionFactSet {
		t.Fatalf("actual ordinal source = %q, want production FactSet", report.ActualOrdinalSource)
	}
	if report.OracleSemantic.SetSHA256 != report.ActualSemantic.SetSHA256 ||
		report.OracleSemantic.Cardinality != 2 || report.ActualSemantic.Cardinality != 2 {
		t.Fatalf("semantic summaries differ: oracle=%+v actual=%+v", report.OracleSemantic, report.ActualSemantic)
	}
	if report.ExpectedOrdinalSetSHA256 != report.ActualOrdinalSetSHA256 ||
		report.ExpectedOrdinalCardinality != 2 || report.ActualOrdinalCardinality != 2 {
		t.Fatalf("ordinal summaries differ: %+v", report)
	}
	if report.DictionarySet.CatalogDigest != fixture.request.CatalogSHA256 ||
		len(report.DictionarySet.Members) != 1 || report.DictionarySetSHA256 == "" {
		t.Fatalf("dictionary-set identity is incomplete: %+v", report.DictionarySet)
	}
	if len(report.Dictionaries) != 1 {
		t.Fatalf("dictionary identity count = %d, want 1", len(report.Dictionaries))
	}
	identity := report.Dictionaries[0]
	manifest := fixture.index.Manifest()
	if identity.PublicationName != "fixture-publication" || identity.SourceID != manifest.SourceID ||
		identity.SourceNamespace != manifest.SourceNamespace || identity.Snapshot != manifest.Snapshot ||
		identity.SchemaSHA256 != manifest.SchemaDigest || identity.DictionarySHA256 != manifest.DictionaryDigest ||
		identity.SidecarSHA256 != manifest.SidecarDigest || identity.ColdPayloadSHA256 != manifest.ColdPayloadDigest ||
		identity.HotIndexSHA256 != manifest.HotIndexDigest || identity.FactCount != 3 {
		t.Fatalf("dictionary identity differs from HOT manifest: %+v", identity)
	}
	if fixture.index.hashCalls < 5 {
		t.Fatalf("HOT Hash calls = %d, want full three-member scan plus actual stream", fixture.index.hashCalls)
	}
	if fixture.payloads.calls != 2 {
		t.Fatalf("COLD CanonicalPayload calls = %d, want every oracle member", fixture.payloads.calls)
	}
}

func TestReviewedUniverseReusesHotIndexAndSeparatesExpectedFromCompare(t *testing.T) {
	fixture := newLinkFixture(t)
	universe, err := ReviewPublications(fixture.request.CatalogSHA256, fixture.request.Publications...)
	if err != nil {
		t.Fatalf("ReviewPublications: %v", err)
	}
	if fixture.index.hashCalls != 3 {
		t.Fatalf("HOT scan calls = %d, want one complete three-member scan", fixture.index.hashCalls)
	}
	expected, err := universe.Expected(ExpectedRequest{Role: fixtureRole, OracleFacts: factStream(fixture.oracleFacts),
		Expected: fixture.request.Expected})
	if err != nil {
		t.Fatalf("Expected: %v", err)
	}
	if expected.Ordinals.Cardinality() != 2 || expected.Report.ExpectedOrdinalSetSHA256 == "" ||
		expected.Report.ActualOrdinalSetSHA256 != "" {
		t.Fatalf("pre-run expected link is incomplete or contains an actual result: %+v", expected.Report)
	}
	if fixture.index.hashCalls != 3 || fixture.payloads.calls != 2 {
		t.Fatalf("Expected rescanned HOT or omitted COLD: hash=%d payload=%d", fixture.index.hashCalls, fixture.payloads.calls)
	}
	report, err := universe.Compare(expected, CompareRequest{
		Actual: fixture.request.Actual, ActualSource: ActualSetSourceProductionFactSet,
	})
	if err != nil || !report.Match {
		t.Fatalf("Compare: report=%+v err=%v", report.Mismatches, err)
	}
	if fixture.index.hashCalls != 5 {
		t.Fatalf("actual MultiIndex stream Hash calls = %d, want 3 scan + 2 actual", fixture.index.hashCalls)
	}
	second, err := universe.Expected(ExpectedRequest{Role: fixtureRole, OracleFacts: factStream(fixture.oracleFacts),
		Expected: fixture.request.Expected})
	if err != nil {
		t.Fatalf("second Expected: %v", err)
	}
	if fixture.index.hashCalls != 5 || fixture.payloads.calls != 2 ||
		second.Report.PayloadVerification.CachedExactPayloadMembers != 2 {
		t.Fatalf("reusable cache counts: hash=%d payload=%d verification=%+v", fixture.index.hashCalls,
			fixture.payloads.calls, second.Report.PayloadVerification)
	}
	full, err := universe.FullBitmapSet()
	if err != nil || full.Cardinality() != 3 || !full.Equal(bitmap(t, fixture.refs...)) {
		t.Fatalf("FullBitmapSet cardinality=%d err=%v", full.Cardinality(), err)
	}
}

func TestVerifiedColdClosureAvoidsResidentPayloadIndex(t *testing.T) {
	fixture := newLinkFixture(t)
	encoded, err := fixture.cold.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary COLD: %v", err)
	}
	closure, err := VerifyColdClosure(bytes.NewReader(encoded), int64(len(encoded)), fixture.hot)
	if err != nil {
		t.Fatalf("VerifyColdClosure: %v", err)
	}
	wantArtifact := sha256.Sum256(encoded)
	if closure.ArtifactSHA256 != hex.EncodeToString(wantArtifact[:]) || !closure.verified {
		t.Fatalf("COLD closure identity = %+v", closure)
	}
	universe, err := ReviewPublications(fixture.request.CatalogSHA256, Publication{
		Name: "fixture-publication", Index: fixture.index, ColdClosure: &closure,
	})
	if err != nil {
		t.Fatalf("ReviewPublications with COLD closure: %v", err)
	}
	report, err := universe.Link(SetRequest{Role: fixtureRole, OracleFacts: factStream(fixture.oracleFacts),
		Expected: fixture.request.Expected, Actual: fixture.request.Actual,
		ActualSource: ActualSetSourceProductionFactSet})
	if err != nil || !report.Match {
		t.Fatalf("closure-backed Link: report=%+v err=%v", report.Mismatches, err)
	}
	if report.PayloadVerification.VerifiedColdClosureMembers != 2 ||
		report.PayloadVerification.ExactCanonicalPayloadMembers != 0 || fixture.payloads.calls != 0 ||
		len(report.Dictionaries) != 1 || report.Dictionaries[0].PayloadVerificationMode != PayloadVerificationColdClosure ||
		report.Dictionaries[0].ColdArtifactSHA256 != closure.ArtifactSHA256 {
		t.Fatalf("closure verification report = %+v identities=%+v payload calls=%d",
			report.PayloadVerification, report.Dictionaries, fixture.payloads.calls)
	}
	unchecked := closure
	unchecked.verified = false
	if _, err := ReviewPublications(fixture.request.CatalogSHA256, Publication{
		Name: "fixture-publication", Index: fixture.index, ColdClosure: &unchecked,
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unchecked closure err = %v, want ErrInvalidInput", err)
	}
}

func TestLinkRejectsMissingAndExtraActualOrdinals(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fixture := newLinkFixture(t)
		fixture.request.Actual = bitmap(t, fixture.refs[0])
		report, err := Link(fixture.request)
		if !errors.Is(err, ErrMismatch) {
			t.Fatalf("Link err = %v, want ErrMismatch", err)
		}
		if report.Mismatches.ExpectedOrdinalsMissingInActual != 1 || report.Mismatches.UnexpectedActualOrdinals != 0 ||
			!report.Mismatches.SemanticSetMismatch || report.OrdinalSetEqual {
			t.Fatalf("missing mismatch report = %+v", report.Mismatches)
		}
		assertDetail(t, report, MismatchActualMissing, fixture.oracleFacts[1].SHA256)
	})

	t.Run("extra", func(t *testing.T) {
		fixture := newLinkFixture(t)
		fixture.request.Actual = bitmap(t, fixture.refs...)
		report, err := Link(fixture.request)
		if !errors.Is(err, ErrMismatch) {
			t.Fatalf("Link err = %v, want ErrMismatch", err)
		}
		if report.Mismatches.ExpectedOrdinalsMissingInActual != 0 || report.Mismatches.UnexpectedActualOrdinals != 1 ||
			!report.Mismatches.SemanticSetMismatch || report.OrdinalSetEqual {
			t.Fatalf("extra mismatch report = %+v", report.Mismatches)
		}
		assertDetail(t, report, MismatchActualExtra, fixture.allFacts[2].SHA256)
	})
}

func TestLinkRejectsCanonicalPayloadMismatch(t *testing.T) {
	fixture := newLinkFixture(t)
	fixture.payloads.mutate = &fixture.refs[1]
	report, err := Link(fixture.request)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Link err = %v, want ErrMismatch", err)
	}
	if report.Mismatches.CanonicalPayloadMismatches != 1 || !report.OrdinalSetEqual ||
		report.OracleSemantic.SetSHA256 != report.ActualSemantic.SetSHA256 {
		t.Fatalf("payload mismatch report = %+v", report.Mismatches)
	}
	detail := assertDetail(t, report, MismatchPayload, fixture.oracleFacts[1].SHA256)
	if detail.Ref == nil || *detail.Ref != fixture.refs[1] || detail.ExpectedPayloadSHA256 == detail.ActualPayloadSHA256 {
		t.Fatalf("payload mismatch detail = %+v", detail)
	}
}

func TestLinkRejectsUnavailableCanonicalPayload(t *testing.T) {
	fixture := newLinkFixture(t)
	fixture.payloads.unavailable = &fixture.refs[0]
	report, err := Link(fixture.request)
	if !errors.Is(err, ErrMismatch) || report.Mismatches.CanonicalPayloadUnavailable != 1 {
		t.Fatalf("Link err/report = %v / %+v", err, report.Mismatches)
	}
	assertDetail(t, report, MismatchPayloadUnavailable, fixture.oracleFacts[0].SHA256)
}

func TestLinkRejectsDuplicateOracleMember(t *testing.T) {
	fixture := newLinkFixture(t)
	fixture.request.OracleFacts = factStream([]finalv5oracle.CanonicalFact{
		fixture.oracleFacts[0], fixture.oracleFacts[0], fixture.oracleFacts[1],
	})
	report, err := Link(fixture.request)
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Link err = %v, want ErrMismatch", err)
	}
	if report.Mismatches.OracleDuplicates != 1 || report.OracleSemantic.Stats.DuplicateMembers != 1 ||
		!report.OrdinalSetEqual || report.Mismatches.SemanticSetMismatch {
		t.Fatalf("duplicate mismatch report = %+v; oracle=%+v", report.Mismatches, report.OracleSemantic)
	}
	assertDetail(t, report, MismatchOracleDuplicate, "")
}

func TestLinkRejectsOracleMemberMissingFromDictionaries(t *testing.T) {
	fixture := newLinkFixture(t)
	missing, err := finalv5oracle.BuildV2BaseRowFact(finalv5oracle.V2BaseRowInput{
		SourceNamespace: "fixture.source", Snapshot: "snapshot-1", EntityKey: "missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.request.OracleFacts = factStream(append(append([]finalv5oracle.CanonicalFact(nil), fixture.oracleFacts...), missing))
	fixture.request.Expected = semanticExpectation(t, fixtureRole,
		append(append([]finalv5oracle.CanonicalFact(nil), fixture.oracleFacts...), missing))
	report, err := Link(fixture.request)
	if !errors.Is(err, ErrMismatch) || report.Mismatches.MissingFromDictionaries != 1 || report.OrdinalSetEqual {
		t.Fatalf("Link err/report = %v / %+v", err, report.Mismatches)
	}
	assertDetail(t, report, MismatchDictionaryMissing, missing.SHA256)
}

func TestLinkRejectsUnknownActualDictionary(t *testing.T) {
	fixture := newLinkFixture(t)
	unknown := ordinal.FactRef{DictionaryDigest: digest("unknown dictionary"), SegmentID: "unknown", Ordinal: 0}
	fixture.request.Actual = bitmap(t, append(append([]ordinal.FactRef(nil), fixture.refs[:2]...), unknown)...)
	_, err := Link(fixture.request)
	if !errors.Is(err, ErrUnknownDictionary) {
		t.Fatalf("Link err = %v, want ErrUnknownDictionary", err)
	}
}

func TestLinkRejectsUnstatedActualSetSource(t *testing.T) {
	fixture := newLinkFixture(t)
	fixture.request.ActualSource = ""
	_, err := Link(fixture.request)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Link err = %v, want ErrInvalidInput", err)
	}
}

func TestLinkRejectsReviewedDigestMismatch(t *testing.T) {
	t.Run("semantic", func(t *testing.T) {
		fixture := newLinkFixture(t)
		fixture.request.Expected.SetSHA256 = digest("wrong reviewed semantic digest")
		report, err := Link(fixture.request)
		if !errors.Is(err, ErrMismatch) || !report.Mismatches.ReviewedSemanticDigestMismatch ||
			!report.OrdinalSetEqual || report.Mismatches.SemanticSetMismatch {
			t.Fatalf("Link err/report = %v / %+v", err, report.Mismatches)
		}
		assertDetail(t, report, MismatchReviewedSemantic, "")
	})

	t.Run("ordinal", func(t *testing.T) {
		fixture := newLinkFixture(t)
		fixture.request.ReviewedOrdinalSetSHA256 = digest("wrong reviewed ordinal digest")
		report, err := Link(fixture.request)
		if !errors.Is(err, ErrMismatch) || !report.Mismatches.ReviewedOrdinalDigestMismatch || !report.OrdinalSetEqual {
			t.Fatalf("Link err/report = %v / %+v", err, report.Mismatches)
		}
		assertDetail(t, report, MismatchReviewedOrdinal, "")
	})
}

func newLinkFixture(t *testing.T) linkFixture {
	t.Helper()
	oracleRow, err := finalv5oracle.BuildV2BaseRowFact(finalv5oracle.V2BaseRowInput{
		SourceNamespace: "fixture.source", Snapshot: "snapshot-1", EntityKey: "row-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	oracleValue, err := finalv5oracle.BuildV2BaseCellFact(finalv5oracle.V2BaseCellInput{
		SourceNamespace: "fixture.source", Snapshot: "snapshot-1", EntityKey: "row-1", Field: "value",
		SQLType: "integer", CanonicalValue: "i:7",
	})
	if err != nil {
		t.Fatal(err)
	}
	oracleExtra, err := finalv5oracle.BuildV2BaseCellFact(finalv5oracle.V2BaseCellInput{
		SourceNamespace: "fixture.source", Snapshot: "snapshot-1", EntityKey: "row-1", Field: "extra",
		SQLType: "integer", CanonicalValue: "i:9",
	})
	if err != nil {
		t.Fatal(err)
	}
	productionRow, err := exposure.NewBaseRowFactV2("fixture.source", "snapshot-1", "row-1")
	if err != nil {
		t.Fatal(err)
	}
	productionValue, err := exposure.NewBaseCellFactV2("fixture.source", "snapshot-1", "row-1", "value", "integer", int64(7))
	if err != nil {
		t.Fatal(err)
	}
	productionExtra, err := exposure.NewBaseCellFactV2("fixture.source", "snapshot-1", "row-1", "extra", "integer", int64(9))
	if err != nil {
		t.Fatal(err)
	}
	dictionary, err := ordinal.Compile(ordinal.DictionarySpec{
		SourceID: "fixture", SourceNamespace: "fixture.source", Snapshot: "snapshot-1", SchemaDigest: digest("schema"),
		Segments: []ordinal.SegmentSpec{
			{ID: "row", Kind: ordinal.SegmentBaseRow, Facts: []exposure.FactID{productionRow}},
			{ID: "value", Kind: ordinal.SegmentBaseCell, Field: "value", Facts: []exposure.FactID{productionValue}},
			{ID: "extra", Kind: ordinal.SegmentBaseCell, Field: "extra", Facts: []exposure.FactID{productionExtra}},
		},
	})
	if err != nil {
		t.Fatalf("compile dictionary: %v", err)
	}
	refs := make([]ordinal.FactRef, 3)
	for index, fact := range []exposure.FactID{productionRow, productionValue, productionExtra} {
		ref, found, lookupErr := dictionary.Lookup(fact)
		if lookupErr != nil || !found {
			t.Fatalf("lookup fact %d: found=%t err=%v", index, found, lookupErr)
		}
		refs[index] = ref
	}
	artifact, err := dictionary.Split()
	if err != nil {
		t.Fatalf("split dictionary: %v", err)
	}
	index := &countingIndex{SnapshotIndex: artifact.Hot}
	payloads := &countingPayloads{base: artifact.Cold}
	oracleFacts := []finalv5oracle.CanonicalFact{oracleRow, oracleValue}
	actual := bitmap(t, refs[:2]...)
	return linkFixture{
		oracleFacts: oracleFacts, allFacts: []finalv5oracle.CanonicalFact{oracleRow, oracleValue, oracleExtra},
		refs: refs, hot: artifact.Hot, cold: artifact.Cold, index: index, payloads: payloads,
		request: Request{CatalogSHA256: digest("catalog"), Role: fixtureRole, OracleFacts: factStream(oracleFacts),
			Expected: semanticExpectation(t, fixtureRole, oracleFacts), Actual: actual,
			ActualSource: ActualSetSourceProductionFactSet,
			Publications: []Publication{{Name: "fixture-publication", Index: index, Payloads: payloads}}},
	}
}

func semanticExpectation(t *testing.T, role string, facts []finalv5oracle.CanonicalFact) SemanticExpectation {
	t.Helper()
	summary, err := finalv5oracle.SummarizeSemanticSet(role, func(yield func(string) error) error {
		for _, fact := range facts {
			if err := yield(fact.SHA256); err != nil {
				return err
			}
		}
		return nil
	}, finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 8})
	if err != nil {
		t.Fatalf("summarize semantic expectation: %v", err)
	}
	return SemanticExpectation{Cardinality: summary.Cardinality, SetSHA256: summary.SetSHA256}
}

func factStream(facts []finalv5oracle.CanonicalFact) CanonicalFactStream {
	return func(yield func(finalv5oracle.CanonicalFact) error) error {
		for _, fact := range facts {
			if err := yield(fact); err != nil {
				return err
			}
		}
		return nil
	}
}

func bitmap(t *testing.T, refs ...ordinal.FactRef) ordinal.BitmapSet {
	t.Helper()
	set, err := ordinal.NewBitmapSet(refs...)
	if err != nil {
		t.Fatalf("NewBitmapSet: %v", err)
	}
	return set
}

func assertDetail(t *testing.T, report Report, kind MismatchKind, semanticHash string) MismatchDetail {
	t.Helper()
	for _, detail := range report.Mismatches.Details {
		if detail.Kind == kind && (semanticHash == "" || detail.SemanticSHA256 == semanticHash) {
			return detail
		}
	}
	t.Fatalf("report has no %q detail for %q: %+v", kind, semanticHash, report.Mismatches.Details)
	return MismatchDetail{}
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
