// Package finalv5linker links independent final-v5 semantic Fact expectations
// to reviewed production ordinal publications. It is evaluation-only: the
// package reads already-compiled HOT/COLD artifacts and never prepares or
// derives a query.
package finalv5linker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	// Version identifies the review-report shape and comparison procedure.
	Version = "taskgate-final-v5-semantic-ordinal-link-v1"

	defaultMismatchDetails = 32
)

var (
	ErrInvalidInput      = errors.New("invalid semantic-to-ordinal link input")
	ErrMismatch          = errors.New("semantic-to-ordinal link mismatch")
	ErrUnknownDictionary = errors.New("actual ordinal set names an unknown dictionary")
)

// CanonicalFactStream emits the independent oracle's complete semantic set.
// The callback must not be retained after the stream returns.
type CanonicalFactStream func(yield func(finalv5oracle.CanonicalFact) error) error

// CanonicalPayloadIndex is the COLD half of an ordinal publication. The
// production *ordinal.ColdDictionary satisfies this interface without adding
// another production API.
type CanonicalPayloadIndex interface {
	CanonicalPayload(ordinal.FactRef) ([]byte, error)
}

// Publication pairs the already-verified HOT and COLD halves of one reviewed
// publication. Name is the Catalog publication name.
type Publication struct {
	Name        string
	Index       ordinal.SnapshotIndex
	Payloads    CanonicalPayloadIndex
	ColdClosure *VerifiedColdClosure
}

// PayloadVerificationMode makes the semantic/payload proof boundary explicit
// in every report. Exact mode compares oracle and COLD canonical bytes for each
// linked member. Closure mode relies on a complete streaming production COLD
// verification plus exact oracle-HOT FactHash equality, without retaining the
// multi-gigabyte COLD index for repeated query cells.
type PayloadVerificationMode string

const (
	PayloadVerificationExact       PayloadVerificationMode = "exact_canonical_payload"
	PayloadVerificationColdClosure PayloadVerificationMode = "verified_cold_artifact_closure"
)

// ActualSetSource distinguishes a production FactSet comparison from a
// publication-wide review. Both are useful, but the latter must never be
// presented as evidence that a production execution returned those ordinals.
type ActualSetSource string

const (
	ActualSetSourceProductionFactSet           ActualSetSource = "production_factset"
	ActualSetSourceReviewedPublicationUniverse ActualSetSource = "reviewed_publication_universe"
)

// VerifiedColdClosure is an opaque successful verification result. Only
// VerifyColdClosure can set verified, so callers cannot opt into closure mode
// with an unchecked descriptor literal.
type VerifiedColdClosure struct {
	ManifestSHA256    string `json:"manifest_sha256"`
	DictionarySHA256  string `json:"dictionary_sha256"`
	ColdPayloadSHA256 string `json:"cold_payload_sha256"`
	ArtifactSHA256    string `json:"artifact_sha256"`
	ArtifactBytes     int64  `json:"artifact_bytes"`
	verified          bool
}

// VerifyColdClosure runs the production complete streaming COLD verifier and
// binds its result to the supplied HOT manifest. The reader is consumed once;
// no COLD payload index is retained.
func VerifyColdClosure(reader io.Reader, artifactBytes int64, index ordinal.SnapshotIndex) (VerifiedColdClosure, error) {
	if reader == nil || index == nil {
		return VerifiedColdClosure{}, fmt.Errorf("%w: nil COLD stream or HOT index", ErrInvalidInput)
	}
	manifest := index.Manifest()
	manifestDigest, err := manifest.Digest()
	if err != nil || index.DictionaryDigest() != manifest.DictionaryDigest || index.ManifestDigest() != manifestDigest {
		return VerifiedColdClosure{}, fmt.Errorf("%w: HOT identity disagrees with its manifest", ErrInvalidInput)
	}
	artifactHash := sha256.New()
	if err := ordinal.VerifyColdDictionaryReader(io.TeeReader(reader, artifactHash), artifactBytes, manifestDigest); err != nil {
		return VerifiedColdClosure{}, fmt.Errorf("verify complete COLD artifact: %w", err)
	}
	return VerifiedColdClosure{ManifestSHA256: manifestDigest, DictionarySHA256: manifest.DictionaryDigest,
		ColdPayloadSHA256: manifest.ColdPayloadDigest, ArtifactSHA256: hex.EncodeToString(artifactHash.Sum(nil)),
		ArtifactBytes: artifactBytes, verified: true}, nil
}

// SemanticExpectation is the role-bound commitment already produced by the
// independent oracle manifest.
type SemanticExpectation struct {
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256"`
}

// Options bounds review diagnostics and forwards the bounded-memory set
// sorter controls to finalv5oracle.SummarizeSemanticSet.
type Options struct {
	Set                finalv5oracle.StreamSetOptions
	MaxMismatchDetails int
}

// Request contains independent semantic expectations and an explicitly
// sourced ordinal set to compare. ReviewedOrdinalSetSHA256 is optional on
// first generation; when present, it is checked rather than regenerated by
// fiat.
type Request struct {
	CatalogSHA256            string
	Role                     string
	OracleFacts              CanonicalFactStream
	Expected                 SemanticExpectation
	Actual                   ordinal.BitmapSet
	ActualSource             ActualSetSource
	Publications             []Publication
	ReviewedOrdinalSetSHA256 string
	Options                  Options
}

// SetRequest compares one oracle semantic set with one explicitly sourced
// ordinal set after the publication universe has been reviewed and indexed.
type SetRequest struct {
	Role                     string
	OracleFacts              CanonicalFactStream
	Expected                 SemanticExpectation
	Actual                   ordinal.BitmapSet
	ActualSource             ActualSetSource
	ReviewedOrdinalSetSHA256 string
	Options                  Options
}

// ExpectedRequest resolves an independent semantic stream before production
// execution. Compare can consume the result later without rerunning the
// oracle or rescanning HOT/COLD artifacts.
type ExpectedRequest struct {
	Role        string
	OracleFacts CanonicalFactStream
	Expected    SemanticExpectation
	Options     Options
}

// CompareRequest supplies the comparison set, its non-optional provenance,
// and an optional previously reviewed ordinal commitment.
type CompareRequest struct {
	Actual                   ordinal.BitmapSet
	ActualSource             ActualSetSource
	ReviewedOrdinalSetSHA256 string
}

// DictionaryIdentity records every immutable identity the HOT manifest binds,
// including the COLD payload and sidecar roots needed by a reviewer.
type DictionaryIdentity struct {
	PublicationName         string                  `json:"publication_name"`
	SourceID                string                  `json:"source_id"`
	SourceNamespace         string                  `json:"source_namespace"`
	Snapshot                string                  `json:"snapshot"`
	SchemaSHA256            string                  `json:"schema_sha256"`
	DictionarySHA256        string                  `json:"dictionary_sha256"`
	ManifestSHA256          string                  `json:"manifest_sha256"`
	SidecarSHA256           string                  `json:"sidecar_sha256"`
	ColdPayloadSHA256       string                  `json:"cold_payload_sha256"`
	HotIndexSHA256          string                  `json:"hot_index_sha256"`
	FactCount               uint64                  `json:"fact_count"`
	PayloadVerificationMode PayloadVerificationMode `json:"payload_verification_mode"`
	ColdArtifactSHA256      string                  `json:"cold_artifact_sha256,omitempty"`
	ColdArtifactBytes       int64                   `json:"cold_artifact_bytes,omitempty"`
}

// PayloadVerificationSummary states how linked oracle members were checked.
type PayloadVerificationSummary struct {
	ExactCanonicalPayloadMembers int64 `json:"exact_canonical_payload_members"`
	CachedExactPayloadMembers    int64 `json:"cached_exact_payload_members"`
	VerifiedColdClosureMembers   int64 `json:"verified_cold_closure_members"`
}

// MismatchKind is a stable, machine-readable review diagnostic.
type MismatchKind string

const (
	MismatchOracleDuplicate     MismatchKind = "oracle_duplicate"
	MismatchDictionaryMissing   MismatchKind = "dictionary_missing"
	MismatchPayload             MismatchKind = "canonical_payload"
	MismatchPayloadUnavailable  MismatchKind = "canonical_payload_unavailable"
	MismatchActualMissing       MismatchKind = "actual_missing"
	MismatchActualExtra         MismatchKind = "actual_extra"
	MismatchReviewedCardinality MismatchKind = "reviewed_cardinality"
	MismatchReviewedSemantic    MismatchKind = "reviewed_semantic_digest"
	MismatchSemanticSet         MismatchKind = "semantic_set"
	MismatchReviewedOrdinal     MismatchKind = "reviewed_ordinal_digest"
)

// MismatchDetail identifies a mismatching semantic member or commitment. Raw
// canonical payload bytes are deliberately not copied into the report.
type MismatchDetail struct {
	Kind                  MismatchKind     `json:"kind"`
	SemanticSHA256        string           `json:"semantic_sha256,omitempty"`
	Ref                   *ordinal.FactRef `json:"ref,omitempty"`
	ExpectedSHA256        string           `json:"expected_sha256,omitempty"`
	ActualSHA256          string           `json:"actual_sha256,omitempty"`
	ExpectedPayloadSHA256 string           `json:"expected_payload_sha256,omitempty"`
	ActualPayloadSHA256   string           `json:"actual_payload_sha256,omitempty"`
	Message               string           `json:"message,omitempty"`
}

// MismatchSummary retains exact counts even when Details is bounded.
type MismatchSummary struct {
	OracleDuplicates                int64            `json:"oracle_duplicates"`
	MissingFromDictionaries         int64            `json:"missing_from_dictionaries"`
	CanonicalPayloadMismatches      int64            `json:"canonical_payload_mismatches"`
	CanonicalPayloadUnavailable     int64            `json:"canonical_payload_unavailable"`
	ExpectedOrdinalsMissingInActual uint64           `json:"expected_ordinals_missing_in_actual"`
	UnexpectedActualOrdinals        uint64           `json:"unexpected_actual_ordinals"`
	ReviewedCardinalityMismatch     bool             `json:"reviewed_cardinality_mismatch"`
	ReviewedSemanticDigestMismatch  bool             `json:"reviewed_semantic_digest_mismatch"`
	SemanticSetMismatch             bool             `json:"semantic_set_mismatch"`
	ReviewedOrdinalDigestMismatch   bool             `json:"reviewed_ordinal_digest_mismatch"`
	Details                         []MismatchDetail `json:"details,omitempty"`
	DetailsTruncated                bool             `json:"details_truncated"`
}

// Report contains both independently computed semantic commitments, both
// ordinal commitments, and the complete publication identity closure.
type Report struct {
	Version                    string                         `json:"version"`
	Match                      bool                           `json:"match"`
	Role                       string                         `json:"role"`
	CatalogSHA256              string                         `json:"catalog_sha256"`
	DictionarySet              ordinal.DictionarySetManifest  `json:"dictionary_set"`
	DictionarySetSHA256        string                         `json:"dictionary_set_sha256"`
	Dictionaries               []DictionaryIdentity           `json:"dictionaries"`
	Reviewed                   SemanticExpectation            `json:"reviewed"`
	OracleSemantic             finalv5oracle.StreamSetSummary `json:"oracle_semantic"`
	ActualSemantic             finalv5oracle.StreamSetSummary `json:"actual_semantic"`
	ExpectedOrdinalCardinality uint64                         `json:"expected_ordinal_cardinality"`
	ActualOrdinalCardinality   uint64                         `json:"actual_ordinal_cardinality"`
	ActualOrdinalSource        ActualSetSource                `json:"actual_ordinal_source"`
	ExpectedOrdinalSetSHA256   string                         `json:"expected_ordinal_set_sha256"`
	ActualOrdinalSetSHA256     string                         `json:"actual_ordinal_set_sha256"`
	ReviewedOrdinalSetSHA256   string                         `json:"reviewed_ordinal_set_sha256,omitempty"`
	OrdinalSetEqual            bool                           `json:"ordinal_set_equal"`
	Mismatches                 MismatchSummary                `json:"mismatches"`
	PayloadVerification        PayloadVerificationSummary     `json:"payload_verification"`
	// ExpectedOrdinals is immutable and available to evaluation callers. JSON
	// reports use ExpectedOrdinalSetSHA256 as its portable identity.
	ExpectedOrdinals ordinal.BitmapSet `json:"-"`
}

// LinkError carries the complete bounded diagnostic report while supporting
// errors.Is(err, ErrMismatch).
type LinkError struct {
	Report Report
}

func (e *LinkError) Error() string {
	if e == nil {
		return ErrMismatch.Error()
	}
	m := e.Report.Mismatches
	return fmt.Sprintf("%v: duplicate=%d dictionary_missing=%d payload=%d payload_unavailable=%d actual_missing=%d actual_extra=%d",
		ErrMismatch, m.OracleDuplicates, m.MissingFromDictionaries, m.CanonicalPayloadMismatches,
		m.CanonicalPayloadUnavailable, m.ExpectedOrdinalsMissingInActual, m.UnexpectedActualOrdinals)
}

func (e *LinkError) Unwrap() error { return ErrMismatch }

type indexedFact struct {
	ref ordinal.FactRef
}

// ReviewedUniverse is a reusable, immutable HOT hash-to-ordinal index over a
// fixed Catalog/dictionary closure. Payload verification successes are cached
// by semantic hash, so a 105-cell review scans HOT once and compares a shared
// COLD member at most once.
type ReviewedUniverse struct {
	catalogSHA256       string
	dictionarySet       ordinal.DictionarySetManifest
	dictionarySetSHA256 string
	dictionaries        []DictionaryIdentity
	publications        []Publication
	indexes             []ordinal.SnapshotIndex
	payloads            map[string]CanonicalPayloadIndex
	payloadModes        map[string]PayloadVerificationMode
	knownDictionaries   map[string]bool
	byHash              map[[sha256.Size]byte]indexedFact
	multiIndex          *ordinal.MultiIndex
	payloadVerified     sync.Map // map[[sha256.Size]byte]struct{}
}

// ReviewPublications validates one Catalog/dictionary closure and scans each
// HOT ordinal exactly once. The returned universe is safe for concurrent Link
// calls; all retained production objects are immutable.
func ReviewPublications(catalogSHA256 string, input ...Publication) (*ReviewedUniverse, error) {
	if !validDigest(catalogSHA256) || len(input) == 0 {
		return nil, fmt.Errorf("%w: invalid Catalog or empty publication set", ErrInvalidInput)
	}
	publications := append([]Publication(nil), input...)
	sort.Slice(publications, func(i, j int) bool { return publications[i].Name < publications[j].Name })
	universe := &ReviewedUniverse{catalogSHA256: catalogSHA256, publications: publications,
		indexes:           make([]ordinal.SnapshotIndex, 0, len(publications)),
		payloads:          make(map[string]CanonicalPayloadIndex, len(publications)),
		payloadModes:      make(map[string]PayloadVerificationMode, len(publications)),
		knownDictionaries: make(map[string]bool, len(publications)), byHash: make(map[[sha256.Size]byte]indexedFact)}
	members := make([]ordinal.DictionarySetMember, 0, len(publications))

	for _, publication := range publications {
		if publication.Index == nil || strings.TrimSpace(publication.Name) == "" ||
			publication.Name != strings.TrimSpace(publication.Name) {
			return nil, fmt.Errorf("%w: incomplete publication %q", ErrInvalidInput, publication.Name)
		}
		if (publication.Payloads == nil) == (publication.ColdClosure == nil) {
			return nil, fmt.Errorf("%w: publication %q must select exactly one payload verification mode",
				ErrInvalidInput, publication.Name)
		}
		manifest := publication.Index.Manifest()
		if err := manifest.Validate(); err != nil {
			return nil, fmt.Errorf("%w: publication %q manifest: %v", ErrInvalidInput, publication.Name, err)
		}
		manifestDigest, err := manifest.Digest()
		if err != nil || publication.Index.DictionaryDigest() != manifest.DictionaryDigest ||
			publication.Index.ManifestDigest() != manifestDigest {
			return nil, fmt.Errorf("%w: publication %q HOT identity disagrees with its manifest", ErrInvalidInput, publication.Name)
		}
		if universe.knownDictionaries[manifest.DictionaryDigest] {
			return nil, fmt.Errorf("%w: duplicate dictionary %s", ErrInvalidInput, manifest.DictionaryDigest)
		}
		universe.knownDictionaries[manifest.DictionaryDigest] = true
		if publication.Payloads != nil {
			universe.payloads[manifest.DictionaryDigest] = publication.Payloads
			universe.payloadModes[manifest.DictionaryDigest] = PayloadVerificationExact
		} else {
			closure := publication.ColdClosure
			if !closure.verified || closure.ManifestSHA256 != manifestDigest ||
				closure.DictionarySHA256 != manifest.DictionaryDigest || closure.ColdPayloadSHA256 != manifest.ColdPayloadDigest ||
				!validDigest(closure.ArtifactSHA256) || closure.ArtifactBytes <= sha256.Size {
				return nil, fmt.Errorf("%w: publication %q COLD closure does not match HOT manifest",
					ErrInvalidInput, publication.Name)
			}
			universe.payloadModes[manifest.DictionaryDigest] = PayloadVerificationColdClosure
		}
		universe.indexes = append(universe.indexes, publication.Index)
		members = append(members, ordinal.DictionarySetMember{PublicationName: publication.Name,
			DictionaryDigest: manifest.DictionaryDigest, ManifestDigest: manifestDigest})

		identity := DictionaryIdentity{PublicationName: publication.Name, SourceID: manifest.SourceID,
			SourceNamespace: manifest.SourceNamespace, Snapshot: manifest.Snapshot, SchemaSHA256: manifest.SchemaDigest,
			DictionarySHA256: manifest.DictionaryDigest, ManifestSHA256: manifestDigest, SidecarSHA256: manifest.SidecarDigest,
			ColdPayloadSHA256: manifest.ColdPayloadDigest, HotIndexSHA256: manifest.HotIndexDigest,
			PayloadVerificationMode: universe.payloadModes[manifest.DictionaryDigest]}
		if publication.ColdClosure != nil {
			identity.ColdArtifactSHA256 = publication.ColdClosure.ArtifactSHA256
			identity.ColdArtifactBytes = publication.ColdClosure.ArtifactBytes
		}
		for _, segment := range manifest.Segments {
			count, found := publication.Index.SegmentFactCount(segment.ID)
			if !found || count != segment.FactCount || ^uint64(0)-identity.FactCount < count {
				return nil, fmt.Errorf("%w: publication %q segment %q count disagrees with its manifest",
					ErrInvalidInput, publication.Name, segment.ID)
			}
			identity.FactCount += count
		}
		universe.dictionaries = append(universe.dictionaries, identity)
	}

	var err error
	universe.dictionarySet, err = ordinal.NewDictionarySetManifest(catalogSHA256, members...)
	if err != nil {
		return nil, fmt.Errorf("%w: dictionary-set identity: %v", ErrInvalidInput, err)
	}
	universe.dictionarySetSHA256, err = universe.dictionarySet.Digest()
	if err != nil {
		return nil, fmt.Errorf("%w: dictionary-set digest: %v", ErrInvalidInput, err)
	}
	universe.multiIndex, err = ordinal.NewMultiIndex(universe.indexes...)
	if err != nil {
		return nil, fmt.Errorf("%w: construct multi-index: %v", ErrInvalidInput, err)
	}

	for _, publication := range publications {
		manifest := publication.Index.Manifest()
		for _, segment := range manifest.Segments {
			for ordinalValue := uint64(0); ordinalValue < segment.FactCount; ordinalValue++ {
				ref := ordinal.FactRef{DictionaryDigest: manifest.DictionaryDigest, SegmentID: segment.ID,
					Ordinal: uint32(ordinalValue)}
				hash, hashErr := publication.Index.Hash(ref)
				if hashErr != nil {
					return nil, fmt.Errorf("%w: HOT hash for %+v: %v", ErrInvalidInput, ref, hashErr)
				}
				if previous, duplicate := universe.byHash[hash]; duplicate && previous.ref != ref {
					return nil, fmt.Errorf("%w: semantic hash %s occurs at both %+v and %+v",
						ErrInvalidInput, hex.EncodeToString(hash[:]), previous.ref, ref)
				}
				universe.byHash[hash] = indexedFact{ref: ref}
			}
		}
	}
	return universe, nil
}

// Link performs a complete semantic-to-ordinal comparison. It scans every HOT
// ordinal exposed by each manifest, verifies every oracle member's COLD bytes,
// builds an expected BitmapSet, and compares it exactly with Actual. Semantic
// summaries are delegated to finalv5oracle; ordinal hashes and set digests are
// delegated to ordinal.MultiIndex and ordinal.BitmapSet.
func Link(request Request) (Report, error) {
	universe, err := ReviewPublications(request.CatalogSHA256, request.Publications...)
	if err != nil {
		return Report{Version: Version, Role: request.Role, CatalogSHA256: request.CatalogSHA256,
			Reviewed: request.Expected, ReviewedOrdinalSetSHA256: request.ReviewedOrdinalSetSHA256}, err
	}
	return universe.Link(SetRequest{Role: request.Role, OracleFacts: request.OracleFacts, Expected: request.Expected,
		Actual: request.Actual, ActualSource: request.ActualSource,
		ReviewedOrdinalSetSHA256: request.ReviewedOrdinalSetSHA256, Options: request.Options})
}

// ExpectedLink is the frozen, pre-production result of semantic-to-ordinal
// resolution. Ordinals is immutable. Report contains only oracle, publication,
// payload, and expected-ordinal fields until Compare completes it.
type ExpectedLink struct {
	Report   Report
	Ordinals ordinal.BitmapSet
	universe *ReviewedUniverse
	options  Options
}

// Expected consumes the independent oracle before production execution,
// verifies every linked payload according to the publication's explicit mode,
// and freezes the exact expected ordinal set.
func (universe *ReviewedUniverse) Expected(request ExpectedRequest) (ExpectedLink, error) {
	report := Report{Version: Version, Role: request.Role, Reviewed: request.Expected}
	link := ExpectedLink{Report: report, universe: universe, options: request.Options}
	if universe == nil {
		return link, fmt.Errorf("%w: nil reviewed universe", ErrInvalidInput)
	}
	report.CatalogSHA256 = universe.catalogSHA256
	report.DictionarySet = universe.dictionarySet
	report.DictionarySetSHA256 = universe.dictionarySetSHA256
	report.Dictionaries = append([]DictionaryIdentity(nil), universe.dictionaries...)
	maxDetails := mismatchDetailLimit(request.Options)
	if request.OracleFacts == nil || request.Expected.Cardinality < 0 || !validDigest(request.Expected.SetSHA256) || maxDetails < 0 {
		link.Report = report
		return link, fmt.Errorf("%w: incomplete expected-set request", ErrInvalidInput)
	}

	expectedBuilder := ordinal.NewBuilder()
	var err error
	report.OracleSemantic, err = finalv5oracle.SummarizeSemanticSet(request.Role, func(yield func(string) error) error {
		return request.OracleFacts(func(fact finalv5oracle.CanonicalFact) error {
			if validationErr := finalv5oracle.ValidateCanonicalFact(fact); validationErr != nil {
				return fmt.Errorf("invalid oracle CanonicalFact: %w", validationErr)
			}
			decoded, _ := hex.DecodeString(fact.SHA256)
			var hash [sha256.Size]byte
			copy(hash[:], decoded)
			indexed, found := universe.byHash[hash]
			if !found {
				report.Mismatches.MissingFromDictionaries++
				appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchDictionaryMissing, SemanticSHA256: fact.SHA256})
				return yield(fact.SHA256)
			}
			switch universe.payloadModes[indexed.ref.DictionaryDigest] {
			case PayloadVerificationColdClosure:
				report.PayloadVerification.VerifiedColdClosureMembers++
			case PayloadVerificationExact:
				if _, verified := universe.payloadVerified.Load(hash); verified {
					report.PayloadVerification.CachedExactPayloadMembers++
					break
				}
				report.PayloadVerification.ExactCanonicalPayloadMembers++
				payload, payloadErr := universe.payloads[indexed.ref.DictionaryDigest].CanonicalPayload(indexed.ref)
				if payloadErr != nil {
					report.Mismatches.CanonicalPayloadUnavailable++
					refCopy := indexed.ref
					appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchPayloadUnavailable,
						SemanticSHA256: fact.SHA256, Ref: &refCopy, Message: payloadErr.Error()})
				} else if !bytes.Equal(payload, fact.Payload) {
					report.Mismatches.CanonicalPayloadMismatches++
					refCopy := indexed.ref
					expectedPayloadHash, actualPayloadHash := sha256.Sum256(fact.Payload), sha256.Sum256(payload)
					appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchPayload, SemanticSHA256: fact.SHA256,
						Ref: &refCopy, ExpectedPayloadSHA256: hex.EncodeToString(expectedPayloadHash[:]),
						ActualPayloadSHA256: hex.EncodeToString(actualPayloadHash[:])})
				} else {
					universe.payloadVerified.Store(hash, struct{}{})
				}
			default:
				return errors.New("publication payload verification mode is not available")
			}
			if addErr := expectedBuilder.Add(indexed.ref); addErr != nil {
				return fmt.Errorf("add linked ordinal %+v: %w", indexed.ref, addErr)
			}
			return yield(fact.SHA256)
		})
	}, request.Options.Set)
	if err != nil {
		link.Report = report
		return link, fmt.Errorf("%w: summarize oracle semantic set: %v", ErrInvalidInput, err)
	}
	report.Mismatches.OracleDuplicates = report.OracleSemantic.Stats.DuplicateMembers
	if report.Mismatches.OracleDuplicates != 0 {
		appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchOracleDuplicate,
			Message: fmt.Sprintf("oracle stream repeated %d semantic members", report.Mismatches.OracleDuplicates)})
	}
	link.Ordinals, err = expectedBuilder.Freeze()
	if err != nil {
		link.Report = report
		return link, fmt.Errorf("%w: freeze linked ordinal set: %v", ErrInvalidInput, err)
	}
	report.ExpectedOrdinalCardinality = link.Ordinals.Cardinality()
	report.ExpectedOrdinals = link.Ordinals
	report.ExpectedOrdinalSetSHA256, err = link.Ordinals.Digest()
	if err != nil {
		link.Report = report
		return link, fmt.Errorf("%w: expected ordinal digest: %v", ErrInvalidInput, err)
	}
	if report.OracleSemantic.Cardinality != request.Expected.Cardinality {
		report.Mismatches.ReviewedCardinalityMismatch = true
		appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchReviewedCardinality,
			Message: fmt.Sprintf("oracle cardinality %d; reviewed %d", report.OracleSemantic.Cardinality, request.Expected.Cardinality)})
	}
	if report.OracleSemantic.SetSHA256 != request.Expected.SetSHA256 {
		report.Mismatches.ReviewedSemanticDigestMismatch = true
		appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchReviewedSemantic,
			ExpectedSHA256: request.Expected.SetSHA256, ActualSHA256: report.OracleSemantic.SetSHA256})
	}
	link.Report = report
	if mismatchCount(report.Mismatches) != 0 {
		return link, &LinkError{Report: report}
	}
	return link, nil
}

// Compare completes a pre-run ExpectedLink with an explicitly sourced
// BitmapSet. The actual semantic digest is always recomputed through MultiIndex
// hash streaming and finalv5oracle.SummarizeSemanticSet.
func (universe *ReviewedUniverse) Compare(expected ExpectedLink, request CompareRequest) (Report, error) {
	report := expected.Report
	report.Mismatches.Details = append([]MismatchDetail(nil), expected.Report.Mismatches.Details...)
	report.ReviewedOrdinalSetSHA256 = request.ReviewedOrdinalSetSHA256
	report.ActualOrdinalCardinality = request.Actual.Cardinality()
	report.ActualOrdinalSource = request.ActualSource
	if universe == nil || expected.universe != universe {
		return report, fmt.Errorf("%w: expected set belongs to another reviewed universe", ErrInvalidInput)
	}
	maxDetails := mismatchDetailLimit(expected.options)
	if maxDetails < 0 || !validActualSetSource(request.ActualSource) ||
		(request.ReviewedOrdinalSetSHA256 != "" && !validDigest(request.ReviewedOrdinalSetSHA256)) {
		return report, fmt.Errorf("%w: invalid comparison request", ErrInvalidInput)
	}
	for _, bound := range request.Actual.SegmentBounds() {
		if !universe.knownDictionaries[bound.Segment.DictionaryDigest] {
			return report, fmt.Errorf("%w: %s", ErrUnknownDictionary, bound.Segment.DictionaryDigest)
		}
	}
	if err := universe.multiIndex.ValidateSetBounds(request.Actual); err != nil {
		return report, fmt.Errorf("%w: actual ordinal bounds: %v", ErrInvalidInput, err)
	}

	var err error
	report.ActualOrdinalSetSHA256, err = request.Actual.Digest()
	if err != nil {
		return report, fmt.Errorf("%w: actual ordinal digest: %v", ErrInvalidInput, err)
	}
	report.ActualSemantic, err = finalv5oracle.SummarizeSemanticSet(report.Role, func(yield func(string) error) error {
		return universe.multiIndex.StreamHashesByFactHash(request.Actual, func(_ ordinal.FactRef, hash [sha256.Size]byte) error {
			return yield(hex.EncodeToString(hash[:]))
		})
	}, expected.options.Set)
	if err != nil {
		return report, fmt.Errorf("%w: summarize actual ordinal semantic set: %v", ErrInvalidInput, err)
	}
	if report.OracleSemantic.Cardinality != report.ActualSemantic.Cardinality ||
		report.OracleSemantic.SetSHA256 != report.ActualSemantic.SetSHA256 {
		report.Mismatches.SemanticSetMismatch = true
		appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchSemanticSet,
			ExpectedSHA256: report.OracleSemantic.SetSHA256, ActualSHA256: report.ActualSemantic.SetSHA256})
	}
	if request.ReviewedOrdinalSetSHA256 != "" && report.ExpectedOrdinalSetSHA256 != request.ReviewedOrdinalSetSHA256 {
		report.Mismatches.ReviewedOrdinalDigestMismatch = true
		appendDetail(&report, maxDetails, MismatchDetail{Kind: MismatchReviewedOrdinal,
			ExpectedSHA256: request.ReviewedOrdinalSetSHA256, ActualSHA256: report.ExpectedOrdinalSetSHA256})
	}

	missing := expected.Ordinals.Difference(request.Actual)
	extra := request.Actual.Difference(expected.Ordinals)
	report.Mismatches.ExpectedOrdinalsMissingInActual = missing.Cardinality()
	report.Mismatches.UnexpectedActualOrdinals = extra.Cardinality()
	appendOrdinalDetails(&report, maxDetails, MismatchActualMissing, missing, universe.multiIndex)
	appendOrdinalDetails(&report, maxDetails, MismatchActualExtra, extra, universe.multiIndex)
	report.OrdinalSetEqual = report.Mismatches.MissingFromDictionaries == 0 && expected.Ordinals.Equal(request.Actual)
	report.Match = report.OrdinalSetEqual && mismatchCount(report.Mismatches) == 0
	if !report.Match {
		return report, &LinkError{Report: report}
	}
	return report, nil
}

// Link is the single-call convenience wrapper around Expected then Compare.
func (universe *ReviewedUniverse) Link(request SetRequest) (Report, error) {
	expected, expectedErr := universe.Expected(ExpectedRequest{Role: request.Role, OracleFacts: request.OracleFacts,
		Expected: request.Expected, Options: request.Options})
	if expectedErr != nil && !errors.Is(expectedErr, ErrMismatch) {
		return expected.Report, expectedErr
	}
	report, err := universe.Compare(expected, CompareRequest{Actual: request.Actual,
		ActualSource:             request.ActualSource,
		ReviewedOrdinalSetSHA256: request.ReviewedOrdinalSetSHA256})
	if err != nil {
		return report, err
	}
	if expectedErr != nil {
		return report, &LinkError{Report: report}
	}
	return report, nil
}

func mismatchDetailLimit(options Options) int {
	if options.MaxMismatchDetails == 0 {
		return defaultMismatchDetails
	}
	return options.MaxMismatchDetails
}

// FullBitmapSet returns the exact ordinal universe of all reviewed HOT
// dictionaries without rescanning hashes. It is useful for whole-publication
// review material; ordinary query comparisons should pass the production set.
func (universe *ReviewedUniverse) FullBitmapSet() (ordinal.BitmapSet, error) {
	if universe == nil || len(universe.indexes) == 0 {
		return ordinal.BitmapSet{}, fmt.Errorf("%w: nil or empty reviewed universe", ErrInvalidInput)
	}
	builder := ordinal.NewBuilder()
	for _, index := range universe.indexes {
		manifest := index.Manifest()
		for _, segment := range manifest.Segments {
			count, found := index.SegmentFactCount(segment.ID)
			if !found || count != segment.FactCount {
				return ordinal.BitmapSet{}, fmt.Errorf("%w: dictionary universe segment %q count", ErrInvalidInput, segment.ID)
			}
			for ordinalValue := uint64(0); ordinalValue < count; ordinalValue++ {
				if err := builder.Add(ordinal.FactRef{DictionaryDigest: manifest.DictionaryDigest,
					SegmentID: segment.ID, Ordinal: uint32(ordinalValue)}); err != nil {
					return ordinal.BitmapSet{}, fmt.Errorf("%w: dictionary universe ordinal: %v", ErrInvalidInput, err)
				}
			}
		}
	}
	result, err := builder.Freeze()
	if err != nil {
		return ordinal.BitmapSet{}, fmt.Errorf("%w: freeze dictionary universe: %v", ErrInvalidInput, err)
	}
	return result, nil
}

func appendOrdinalDetails(report *Report, maximum int, kind MismatchKind, set ordinal.BitmapSet, index *ordinal.MultiIndex) {
	remaining := maximum - len(report.Mismatches.Details)
	if remaining <= 0 {
		if !set.IsEmpty() {
			report.Mismatches.DetailsTruncated = true
		}
		return
	}
	visited := 0
	set.Refs(func(ref ordinal.FactRef) bool {
		hash, err := index.Hash(ref)
		refCopy := ref
		detail := MismatchDetail{Kind: kind, Ref: &refCopy}
		if err != nil {
			detail.Message = err.Error()
		} else {
			detail.SemanticSHA256 = hex.EncodeToString(hash[:])
		}
		appendDetail(report, maximum, detail)
		visited++
		return visited < remaining
	})
	if set.Cardinality() > uint64(visited) {
		report.Mismatches.DetailsTruncated = true
	}
}

func appendDetail(report *Report, maximum int, detail MismatchDetail) {
	if len(report.Mismatches.Details) < maximum {
		report.Mismatches.Details = append(report.Mismatches.Details, detail)
		return
	}
	report.Mismatches.DetailsTruncated = true
}

func mismatchCount(value MismatchSummary) int64 {
	count := value.OracleDuplicates + value.MissingFromDictionaries + value.CanonicalPayloadMismatches +
		value.CanonicalPayloadUnavailable + int64(value.ExpectedOrdinalsMissingInActual) + int64(value.UnexpectedActualOrdinals)
	for _, mismatch := range []bool{value.ReviewedCardinalityMismatch, value.ReviewedSemanticDigestMismatch,
		value.SemanticSetMismatch, value.ReviewedOrdinalDigestMismatch} {
		if mismatch {
			count++
		}
	}
	return count
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validActualSetSource(value ActualSetSource) bool {
	return value == ActualSetSourceProductionFactSet || value == ActualSetSourceReviewedPublicationUniverse
}
