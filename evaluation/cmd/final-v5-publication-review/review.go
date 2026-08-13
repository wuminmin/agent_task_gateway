package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5linker"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	reviewVersion          = "taskgate-final-v5-exposure-scale-publication-review-v1"
	reviewStatus           = "REVIEW_CANDIDATE"
	notGenerated           = "NOT_GENERATED"
	publicationName        = "final-v5-exposure-scale-v1"
	reviewCatalogVersion   = "2026-08-11.final-v5-exposure-scale-review-v1"
	reviewCatalogSource    = "travel_demo"
	reviewSourceRelation   = "reporting.final_v5_exposure_scale"
	reviewSidecar          = "taskgate_ordinal.final_v5_exposure_scale_v1"
	reviewDatasourceID     = "taskgate-demo-travel"
	reviewDatabase         = "travel_demo"
	reviewDatabaseUser     = "gateway_reader"
	reviewPostgreSQLMajor  = 16
	reviewSemanticRole     = "union"
	reviewScaleAnchor      = "1035000-overlap-0"
	reviewScaleUnionSHA    = "4354d04853af871bc62d3eba9d51b2da2b1abe9e8e365bcf307e3d7ccd50f02f"
	reviewScaleNovelSHA    = "c0dc2d9f588bd57a2bcbded3fbdb85abf09f29d2cb3551bf75a7d1346a304cb9"
	reviewScaleReplaySHA   = "3d1037c6dbfb1f4a33793b062f100bf35e71b2818a164e14eb0569178f323679"
	reviewManifestMaxBytes = 1 << 20
	reviewSetMemoryMembers = 64 * 1024
	reviewSetSampleMembers = 8
	provSQLManifestSetSHA  = "c7b2a8db21a8c2516ee667780d53e5a485be8e12a16907463727a69e8a2b7f8d"
	scaleManifestSetSHA    = "024e19897d8cd035de8419ffe5c8952b3e43cb194359b62d3d0af6002f18adf1"
)

type generateOptions struct {
	RepositoryRoot  string
	OutputDirectory string
	ArtifactRoot    string
	DSN             string
}

type manifestSetReview struct {
	Files           int    `json:"files"`
	Verifier        string `json:"verifier"`
	PathRoot        string `json:"path_root"`
	AggregateRecord string `json:"aggregate_record"`
	AggregateSHA256 string `json:"aggregate_sha256"`
}

type databaseReview struct {
	DatasourceID                  string `json:"datasource_id"`
	Database                      string `json:"database"`
	User                          string `json:"user"`
	PostgreSQLMajorVersion        int    `json:"postgresql_major_version"`
	SchemaSHA256                  string `json:"schema_sha256"`
	QueryExecMode                 string `json:"query_exec_mode"`
	SessionCount                  int    `json:"session_count"`
	MaximumPreparedStatementCount int64  `json:"maximum_prepared_statement_count"`
}

type reviewFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type manifestReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type scaleUnionReview struct {
	Role        string              `json:"role"`
	Scale       string              `json:"scale"`
	Cardinality int64               `json:"cardinality"`
	SetSHA256   string              `json:"set_sha256"`
	Manifests   []manifestReference `json:"manifests"`
}

type reviewReport struct {
	Version             string                        `json:"version"`
	Status              string                        `json:"status"`
	AuthorApproved      bool                          `json:"author_approved"`
	OutcomeIdentity     string                        `json:"outcome_identity"`
	SetAlgebra          string                        `json:"set_algebra"`
	ProvSQLManifestSet  manifestSetReview             `json:"provsql_manifest_set"`
	ScaleManifestSet    manifestSetReview             `json:"scale_manifest_set"`
	ScaleUnion          scaleUnionReview              `json:"scale_union"`
	Database            databaseReview                `json:"database"`
	Publication         snapshotbundle.BundleManifest `json:"publication"`
	SemanticOrdinalLink finalv5linker.Report          `json:"semantic_ordinal_link"`
	Files               []reviewFile                  `json:"files"`
}

type sourceAttestation struct {
	DatasourceID           string
	Database               string
	User                   string
	PostgreSQLMajorVersion int
	SchemaSHA256           string
	PreparedStatements     int64
}

func generateReview(ctx context.Context, options generateOptions) (reviewReport, error) {
	var result reviewReport
	if ctx == nil {
		return result, errors.New("publication review context is required")
	}
	repositoryRoot, err := filepath.Abs(strings.TrimSpace(options.RepositoryRoot))
	if err != nil {
		return result, errors.New("resolve repository root")
	}
	if err := requireRepositoryRoot(repositoryRoot); err != nil {
		return result, err
	}
	if strings.TrimSpace(options.DSN) == "" {
		return result, errors.New("a live Business PostgreSQL DSN is required")
	}
	if err := requireFreshOutputDirectory(options.OutputDirectory); err != nil {
		return result, err
	}
	if err := requirePrivateArtifactRoot(options.ArtifactRoot); err != nil {
		return result, err
	}

	setOptions := finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: reviewSetMemoryMembers,
		CaptureMembers:     reviewSetSampleMembers,
		TempDir:            options.ArtifactRoot,
	}
	oracleRoot := filepath.Join(repositoryRoot, "evaluation", "final-v5-wsl2", "oracle-manifests")
	// D1's complete 105-file closure is verified before the first database
	// connection. A schema decode or selected-file check is not sufficient.
	provSQLValues, err := readManifestSubtree(oracleRoot, "provsql")
	if err != nil {
		return result, fmt.Errorf("read ProvSQL manifest closed set: %w", err)
	}
	provSQLArtifacts, err := finalv5oracle.VerifyProvSQLNonceJoinManifestSet(provSQLValues, setOptions)
	if err != nil {
		return result, fmt.Errorf("verify ProvSQL manifest closed set: %w", err)
	}
	provSQLReview, err := summarizeProvSQLManifestSet(provSQLValues)
	if err != nil {
		return result, err
	}
	if len(provSQLArtifacts) != 105 || provSQLReview.Files != 105 {
		return result, errors.New("ProvSQL verifier did not return the exact 105-file set")
	}

	scaleValues, err := readManifestSubtree(oracleRoot, "scale")
	if err != nil {
		return result, fmt.Errorf("read Scale manifest closed set: %w", err)
	}
	scaleArtifacts, err := finalv5oracle.VerifyExposureScaleDependencyManifestSet(scaleValues, setOptions)
	if err != nil {
		return result, fmt.Errorf("verify Scale manifest closed set: %w", err)
	}
	scaleReview, err := summarizeScaleManifestSet(scaleValues)
	if err != nil {
		return result, err
	}
	if len(scaleArtifacts) != 24 || scaleReview.Files != 24 {
		return result, errors.New("Scale verifier did not return the exact 24-file set")
	}
	unionReview, err := reviewedScaleUnion(scaleArtifacts)
	if err != nil {
		return result, fmt.Errorf("bind reviewed exposure-scale union: %w", err)
	}

	before, err := attestExposureSource(ctx, options.DSN)
	if err != nil {
		return result, fmt.Errorf("attest exposure-scale source before compilation: %w", err)
	}
	candidate := exposureCompilerInput(before.SchemaSHA256)

	calibrationInput, err := snapshotbundle.ScanPostgresSnapshot(ctx, candidate, options.DSN)
	if err != nil {
		return result, fmt.Errorf("scan exposure-scale calibration snapshot: %w", err)
	}
	calibrationRoot := filepath.Join(options.ArtifactRoot, "calibration")
	calibration, err := snapshotbundle.CompileOwnedToDirectory(&calibrationInput, calibrationRoot,
		snapshotbundle.DefaultPublicationLimits())
	if err != nil {
		return result, fmt.Errorf("compile exposure-scale calibration publication: %w", err)
	}
	candidate.ExpectedDigests = expectedDigests(calibration.Manifest)

	reviewedInput, err := snapshotbundle.ScanPostgresSnapshot(ctx, candidate, options.DSN)
	if err != nil {
		return result, fmt.Errorf("scan exposure-scale reviewed snapshot: %w", err)
	}
	reviewedRoot := filepath.Join(options.ArtifactRoot, "reviewed")
	reviewed, err := snapshotbundle.CompileOwnedToDirectory(&reviewedInput, reviewedRoot,
		snapshotbundle.DefaultPublicationLimits())
	if err != nil {
		return result, fmt.Errorf("compile exposure-scale reviewed publication: %w", err)
	}
	if !reflect.DeepEqual(calibration.Manifest, reviewed.Manifest) {
		return result, errors.New("calibration and reviewed exposure-scale bundle manifests differ")
	}
	after, err := attestExposureSource(ctx, options.DSN)
	if err != nil {
		return result, fmt.Errorf("attest exposure-scale source after compilation: %w", err)
	}
	if before != after || before.PreparedStatements != 0 {
		return result, errors.New("exposure-scale source attestation changed across publication compilation")
	}

	hot, cold, err := reopenReviewedPublication(reviewed)
	if err != nil {
		return result, err
	}

	compilerInputBytes, err := marshalJSONDocument(candidate)
	if err != nil {
		return result, fmt.Errorf("encode reviewed compiler input: %w", err)
	}
	decodedInput, err := snapshotbundle.DecodeCompilerInput(bytes.NewReader(compilerInputBytes))
	if err != nil || !reflect.DeepEqual(decodedInput, candidate) {
		return result, fmt.Errorf("redecode reviewed compiler input: %w", errors.Join(err, errors.New("compiler input differs after decode")))
	}
	bundleBytes, err := marshalJSONDocument(reviewed.Manifest)
	if err != nil {
		return result, fmt.Errorf("encode reviewed bundle manifest: %w", err)
	}
	decodedBundle, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(bundleBytes))
	if err != nil || !reflect.DeepEqual(decodedBundle, reviewed.Manifest) {
		return result, fmt.Errorf("redecode reviewed bundle manifest: %w", errors.Join(err, errors.New("bundle manifest differs after decode")))
	}
	catalogBytes, err := exposureCatalogCandidate(before.SchemaSHA256, reviewed.Manifest)
	if err != nil {
		return result, err
	}
	parsedCatalog, err := catalog.Parse(catalogBytes)
	if err != nil {
		return result, fmt.Errorf("parse dedicated exposure-scale Catalog candidate: %w", err)
	}

	reviewedUniverse, err := finalv5linker.ReviewPublications(parsedCatalog.SHA256, finalv5linker.Publication{
		Name: publicationName, Index: hot, Payloads: cold,
	})
	if err != nil {
		return result, fmt.Errorf("review exposure-scale publication closure: %w", err)
	}
	actualUniverse, err := reviewedUniverse.FullBitmapSet()
	if err != nil {
		return result, fmt.Errorf("construct reviewed exposure-scale ordinal universe: %w", err)
	}
	reviewedOrdinalDigest, err := actualUniverse.Digest()
	if err != nil {
		return result, fmt.Errorf("digest reviewed exposure-scale ordinal universe: %w", err)
	}
	linkReport, err := reviewedUniverse.Link(finalv5linker.SetRequest{
		Role:        reviewSemanticRole,
		OracleFacts: exposurePublicationFactStream,
		Expected: finalv5linker.SemanticExpectation{
			Cardinality: unionReview.Cardinality,
			SetSHA256:   unionReview.SetSHA256,
		},
		Actual:                   actualUniverse,
		ActualSource:             finalv5linker.ActualSetSourceReviewedPublicationUniverse,
		ReviewedOrdinalSetSHA256: reviewedOrdinalDigest,
		Options:                  finalv5linker.Options{Set: setOptions},
	})
	if err != nil {
		return result, fmt.Errorf("link exposure-scale semantic Facts to reviewed ordinals: %w", err)
	}
	if !linkReport.Match || !linkReport.OrdinalSetEqual || linkReport.ExpectedOrdinalCardinality != uint64(finalv5oracle.ExposureScaleMaximumDatasetFacts) ||
		linkReport.ActualOrdinalCardinality != uint64(finalv5oracle.ExposureScaleMaximumDatasetFacts) {
		return result, errors.New("exposure-scale publication linker did not close the complete dictionary universe")
	}
	// ExpectedOrdinals is an in-process convenience view and is deliberately
	// excluded from JSON. Clear it before round-trip equality checks so the
	// review document is represented solely by its portable digest identity.
	linkReport.ExpectedOrdinals = ordinal.BitmapSet{}

	files := []reviewFile{
		reviewFileFor("compiler-input.json", compilerInputBytes),
		reviewFileFor(publicationName+".bundle.json", bundleBytes),
		reviewFileFor("catalog.yaml", catalogBytes),
	}
	result = reviewReport{
		Version: reviewVersion, Status: reviewStatus, AuthorApproved: false,
		OutcomeIdentity: notGenerated, SetAlgebra: notGenerated,
		ProvSQLManifestSet: provSQLReview, ScaleManifestSet: scaleReview, ScaleUnion: unionReview,
		Database: databaseReview{
			DatasourceID: before.DatasourceID, Database: before.Database, User: before.User,
			PostgreSQLMajorVersion: before.PostgreSQLMajorVersion, SchemaSHA256: before.SchemaSHA256,
			QueryExecMode: "simple_protocol", SessionCount: 4, MaximumPreparedStatementCount: 0,
		},
		Publication: reviewed.Manifest, SemanticOrdinalLink: linkReport, Files: files,
	}
	if err := validateReviewReport(result); err != nil {
		return reviewReport{}, fmt.Errorf("validate generated review report: %w", err)
	}
	reviewBytes, err := marshalJSONDocument(result)
	if err != nil {
		return reviewReport{}, fmt.Errorf("encode review report: %w", err)
	}
	decodedReview, err := decodeReviewReport(bytes.NewReader(reviewBytes))
	if err != nil || !reflect.DeepEqual(decodedReview, result) {
		return reviewReport{}, fmt.Errorf("redecode generated review report: %w",
			errors.Join(err, errors.New("review report differs after decode")))
	}
	if err := writeReviewDirectory(options.OutputDirectory, map[string][]byte{
		"compiler-input.json":            compilerInputBytes,
		publicationName + ".bundle.json": bundleBytes,
		"catalog.yaml":                   catalogBytes,
		"review.json":                    reviewBytes,
	}); err != nil {
		return reviewReport{}, err
	}
	reopened, err := validateReviewDirectory(options.OutputDirectory)
	if err != nil || !reflect.DeepEqual(reopened, result) {
		_ = os.RemoveAll(options.OutputDirectory)
		return reviewReport{}, fmt.Errorf("reopen complete review directory: %w",
			errors.Join(err, errors.New("on-disk review directory differs from generated report")))
	}
	return result, nil
}

func decodeReviewReport(reader io.Reader) (reviewReport, error) {
	if reader == nil {
		return reviewReport{}, errors.New("review report reader is required")
	}
	decoder := json.NewDecoder(io.LimitReader(reader, reviewManifestMaxBytes+1))
	decoder.DisallowUnknownFields()
	var report reviewReport
	if err := decoder.Decode(&report); err != nil {
		return reviewReport{}, fmt.Errorf("decode review report: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return reviewReport{}, errors.New("review report has trailing JSON")
	}
	if err := validateReviewReport(report); err != nil {
		return reviewReport{}, err
	}
	return report, nil
}

func validateReviewReport(report reviewReport) error {
	if report.Version != reviewVersion || report.Status != reviewStatus || report.AuthorApproved ||
		report.OutcomeIdentity != notGenerated || report.SetAlgebra != notGenerated {
		return errors.New("review status must remain an unapproved pre-run candidate without Outcome or set algebra")
	}
	if report.ProvSQLManifestSet.Files != 105 ||
		report.ProvSQLManifestSet.Verifier != "VerifyProvSQLNonceJoinManifestSet" ||
		report.ProvSQLManifestSet.PathRoot != "evaluation/final-v5-wsl2/oracle-manifests" ||
		report.ProvSQLManifestSet.AggregateRecord != "<path><TAB><manifest-sha256><LF>" ||
		report.ProvSQLManifestSet.AggregateSHA256 != provSQLManifestSetSHA ||
		report.ScaleManifestSet.Files != 24 ||
		report.ScaleManifestSet.Verifier != "VerifyExposureScaleDependencyManifestSet" ||
		report.ScaleManifestSet.PathRoot != "." ||
		report.ScaleManifestSet.AggregateRecord != "<manifest-sha256><SPACE><SPACE><path><LF>" ||
		report.ScaleManifestSet.AggregateSHA256 != scaleManifestSetSHA {
		return errors.New("review report does not bind both exact oracle manifest closed sets")
	}
	if err := validateScaleUnionReview(report.ScaleUnion); err != nil {
		return err
	}
	database := report.Database
	if database.DatasourceID != reviewDatasourceID || database.Database != reviewDatabase || database.User != reviewDatabaseUser ||
		database.PostgreSQLMajorVersion != reviewPostgreSQLMajor || !isSHA256(database.SchemaSHA256) ||
		database.QueryExecMode != "simple_protocol" || database.SessionCount != 4 ||
		database.MaximumPreparedStatementCount != 0 {
		return errors.New("review report database boundary is incomplete")
	}
	publication := report.Publication
	if err := publication.Validate(); err != nil {
		return fmt.Errorf("review report contains an invalid publication manifest: %w", err)
	}
	if publication.PublicationName != publicationName || publication.CatalogSource != reviewCatalogSource ||
		publication.OrdinalSidecar != reviewSidecar || publication.RowCount != 414_000 ||
		publication.DictionaryManifest.SourceID != reviewDatasourceID ||
		publication.DictionaryManifest.SourceNamespace != finalv5oracle.ExposureScaleSourceNamespace ||
		publication.DictionaryManifest.Snapshot != finalv5oracle.ExposureScaleSnapshot ||
		publication.DictionaryManifest.SchemaDigest != database.SchemaSHA256 ||
		publication.ManifestDigest == "" || publication.DictionaryManifest.DictionaryDigest == "" {
		return errors.New("review report publication identity is incomplete")
	}
	var facts uint64
	for _, segment := range publication.DictionaryManifest.Segments {
		if ^uint64(0)-facts < segment.FactCount {
			return errors.New("reviewed dictionary cardinality overflows")
		}
		facts += segment.FactCount
	}
	if facts != uint64(finalv5oracle.ExposureScaleMaximumDatasetFacts) {
		return fmt.Errorf("reviewed dictionary has %d Facts; expected %d", facts,
			finalv5oracle.ExposureScaleMaximumDatasetFacts)
	}
	link := report.SemanticOrdinalLink
	if link.Version != finalv5linker.Version || !link.Match || !link.OrdinalSetEqual || link.Role != reviewSemanticRole ||
		link.ActualOrdinalSource != finalv5linker.ActualSetSourceReviewedPublicationUniverse ||
		link.CatalogSHA256 == "" || link.ExpectedOrdinalCardinality != facts || link.ActualOrdinalCardinality != facts ||
		link.OracleSemantic.Cardinality != int64(facts) || link.ActualSemantic.Cardinality != int64(facts) ||
		link.OracleSemantic.SetSHA256 != link.ActualSemantic.SetSHA256 ||
		link.ExpectedOrdinalSetSHA256 != link.ActualOrdinalSetSHA256 ||
		link.ReviewedOrdinalSetSHA256 != link.ExpectedOrdinalSetSHA256 ||
		!reflect.DeepEqual(link.Mismatches, finalv5linker.MismatchSummary{}) || len(link.Dictionaries) != 1 ||
		link.Dictionaries[0].PublicationName != publicationName || link.Dictionaries[0].FactCount != facts ||
		link.Dictionaries[0].PayloadVerificationMode != finalv5linker.PayloadVerificationExact ||
		link.PayloadVerification.ExactCanonicalPayloadMembers != int64(facts) ||
		link.PayloadVerification.CachedExactPayloadMembers != 0 ||
		link.PayloadVerification.VerifiedColdClosureMembers != 0 {
		return errors.New("semantic-to-ordinal linker did not close every reviewed member")
	}
	if link.Reviewed.Cardinality != report.ScaleUnion.Cardinality ||
		link.Reviewed.SetSHA256 != report.ScaleUnion.SetSHA256 ||
		link.Reviewed.SetSHA256 != link.OracleSemantic.SetSHA256 ||
		link.OracleSemantic.Role != link.Role || link.ActualSemantic.Role != link.Role ||
		link.OracleSemantic.Stats.InputMembers != int64(facts) || link.OracleSemantic.Stats.DuplicateMembers != 0 ||
		link.ActualSemantic.Stats.InputMembers != int64(facts) || link.ActualSemantic.Stats.DuplicateMembers != 0 {
		return errors.New("semantic-to-ordinal report does not bind its reviewed and computed semantic sets")
	}
	if err := link.DictionarySet.Validate(); err != nil {
		return fmt.Errorf("semantic-to-ordinal dictionary set is invalid: %w", err)
	}
	dictionarySetDigest, err := link.DictionarySet.Digest()
	if err != nil || dictionarySetDigest != link.DictionarySetSHA256 ||
		link.DictionarySet.CatalogDigest != link.CatalogSHA256 || len(link.DictionarySet.Members) != 1 {
		return errors.New("semantic-to-ordinal dictionary-set identity is incomplete")
	}
	member := link.DictionarySet.Members[0]
	if member.PublicationName != publicationName ||
		member.DictionaryDigest != publication.DictionaryManifest.DictionaryDigest ||
		member.ManifestDigest != publication.ManifestDigest {
		return errors.New("semantic-to-ordinal dictionary set differs from the reviewed publication")
	}
	dictionary := link.Dictionaries[0]
	manifest := publication.DictionaryManifest
	if dictionary.SourceID != manifest.SourceID || dictionary.SourceNamespace != manifest.SourceNamespace ||
		dictionary.Snapshot != manifest.Snapshot || dictionary.SchemaSHA256 != manifest.SchemaDigest ||
		dictionary.DictionarySHA256 != manifest.DictionaryDigest || dictionary.ManifestSHA256 != publication.ManifestDigest ||
		dictionary.SidecarSHA256 != manifest.SidecarDigest || dictionary.ColdPayloadSHA256 != manifest.ColdPayloadDigest ||
		dictionary.HotIndexSHA256 != manifest.HotIndexDigest || dictionary.ColdArtifactSHA256 != "" ||
		dictionary.ColdArtifactBytes != 0 {
		return errors.New("semantic-to-ordinal dictionary identity differs from the reviewed publication")
	}
	if len(report.Files) != 3 {
		return errors.New("review report must bind exactly three companion review files")
	}
	wantNames := []string{"catalog.yaml", "compiler-input.json", publicationName + ".bundle.json"}
	seen := make([]string, 0, len(report.Files))
	byName := make(map[string]reviewFile, len(report.Files))
	for _, file := range report.Files {
		if filepath.Base(file.Name) != file.Name || !isSHA256(file.SHA256) || file.Bytes <= 0 {
			return errors.New("review report contains an invalid companion file descriptor")
		}
		if _, duplicate := byName[file.Name]; duplicate {
			return errors.New("review report repeats a companion file descriptor")
		}
		byName[file.Name] = file
		seen = append(seen, file.Name)
	}
	sort.Strings(seen)
	if !reflect.DeepEqual(seen, wantNames) {
		return errors.New("review report companion file set is not closed")
	}
	if link.CatalogSHA256 != byName["catalog.yaml"].SHA256 {
		return errors.New("semantic-to-ordinal Catalog digest differs from the companion Catalog")
	}
	return nil
}

// validateReviewDirectory reopens the complete small review envelope from
// disk. It proves that review.json binds the exact compiler input, Catalog,
// and bundle manifest bytes rather than only round-tripping an in-memory Go
// value. The large HOT/COLD/sidecar files remain descriptor-bound private
// artifacts and are independently reopened during generation.
func validateReviewDirectory(path string) (reviewReport, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return reviewReport{}, errors.New("review directory must be an owner-controlled non-symlink directory")
	}
	want := map[string]bool{
		"catalog.yaml": true, "compiler-input.json": true,
		publicationName + ".bundle.json": true, "review.json": true,
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != len(want) {
		return reviewReport{}, errors.New("review directory does not contain the exact four-file closed set")
	}
	values := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if !want[entry.Name()] || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return reviewReport{}, fmt.Errorf("review directory contains unexpected or non-regular entry %q", entry.Name())
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil || !entryInfo.Mode().IsRegular() || entryInfo.Mode().Perm()&0o022 != 0 ||
			entryInfo.Size() <= 0 || entryInfo.Size() > reviewManifestMaxBytes {
			return reviewReport{}, fmt.Errorf("review file %q is not a bounded owner-controlled regular file", entry.Name())
		}
		value, readErr := os.ReadFile(filepath.Join(path, entry.Name()))
		if readErr != nil || int64(len(value)) != entryInfo.Size() {
			return reviewReport{}, fmt.Errorf("read complete review file %q: %w", entry.Name(), readErr)
		}
		values[entry.Name()] = value
	}
	report, err := decodeReviewReport(bytes.NewReader(values["review.json"]))
	if err != nil {
		return reviewReport{}, err
	}
	descriptors := make(map[string]reviewFile, len(report.Files))
	for _, descriptor := range report.Files {
		descriptors[descriptor.Name] = descriptor
	}
	for name, value := range values {
		if name == "review.json" {
			continue
		}
		descriptor, found := descriptors[name]
		if !found || descriptor.Bytes != int64(len(value)) || descriptor.SHA256 != sha256Hex(value) {
			return reviewReport{}, fmt.Errorf("review companion %q differs from its descriptor", name)
		}
	}
	input, err := snapshotbundle.DecodeCompilerInput(bytes.NewReader(values["compiler-input.json"]))
	if err != nil {
		return reviewReport{}, fmt.Errorf("decode review companion compiler input: %w", err)
	}
	bundle, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(values[publicationName+".bundle.json"]))
	if err != nil {
		return reviewReport{}, fmt.Errorf("decode review companion bundle manifest: %w", err)
	}
	if !reflect.DeepEqual(bundle, report.Publication) {
		return reviewReport{}, errors.New("review companion bundle differs from review.json")
	}
	wantInput := exposureCompilerInput(report.Database.SchemaSHA256)
	wantInput.ExpectedDigests = expectedDigests(bundle)
	if !reflect.DeepEqual(input, wantInput) {
		return reviewReport{}, errors.New("review companion compiler input differs from the fixed exposure-scale candidate")
	}
	wantCatalog, err := exposureCatalogCandidate(report.Database.SchemaSHA256, bundle)
	if err != nil || !bytes.Equal(values["catalog.yaml"], wantCatalog) {
		return reviewReport{}, errors.New("review companion Catalog differs from the fixed exposure-scale candidate")
	}
	parsedCatalog, err := catalog.Parse(values["catalog.yaml"])
	if err != nil || parsedCatalog.SHA256 != report.SemanticOrdinalLink.CatalogSHA256 {
		return reviewReport{}, errors.New("review companion Catalog identity differs from the semantic-to-ordinal report")
	}
	publication, found := parsedCatalog.LookupSnapshotPublication(publicationName)
	if !found || publication.Source != bundle.CatalogSource || publication.OrdinalSidecar != bundle.OrdinalSidecar ||
		publication.SourceNamespace != bundle.DictionaryManifest.SourceNamespace ||
		publication.Snapshot != bundle.DictionaryManifest.Snapshot ||
		publication.DictionaryDigest != bundle.DictionaryManifest.DictionaryDigest ||
		publication.ManifestDigest != bundle.ManifestDigest ||
		publication.SidecarDigest != bundle.DictionaryManifest.SidecarDigest {
		return reviewReport{}, errors.New("review companion Catalog publication differs from the bundle")
	}
	product, found := parsedCatalog.LookupProduct("final_v5_exposure_scale")
	wantFields := []struct{ name, sqlType string }{
		{"member_rank", "bigint"}, {"metric", "numeric"}, {"family_id", "integer"}, {"partition_key", "integer"},
	}
	if !found || len(parsedCatalog.Sources) != 1 || len(parsedCatalog.SnapshotPublications) != 1 ||
		len(parsedCatalog.Products) != 1 || product.Source != reviewCatalogSource ||
		product.ReportingView != reviewSourceRelation || product.Snapshot != bundle.DictionaryManifest.Snapshot ||
		product.SnapshotPublication != publicationName || product.FactNamespace != bundle.DictionaryManifest.SourceNamespace ||
		product.StableRelationRole != "final_v5_exposure_scale" ||
		!reflect.DeepEqual(product.EntityKey, input.EntityKeyFields) || len(product.Fields) != len(wantFields) {
		return reviewReport{}, errors.New("review companion Catalog Product differs from the compiler input")
	}
	for index, want := range wantFields {
		if product.Fields[index].Name != want.name || product.Fields[index].Type != want.sqlType ||
			input.Snapshot.Fields[index].Name != want.name || input.Snapshot.Fields[index].SQLType != want.sqlType {
			return reviewReport{}, errors.New("review companion Catalog fields differ from the compiler input")
		}
	}
	return report, nil
}

func exposureCompilerInput(schemaDigest string) snapshotbundle.CompilerInput {
	return snapshotbundle.CompilerInput{
		Version: snapshotbundle.CompilerInputVersion, PublicationName: publicationName,
		CatalogSource: reviewCatalogSource, SourceRelation: reviewSourceRelation, OrdinalSidecar: reviewSidecar,
		EntityKeyFields: []string{"member_rank"},
		Snapshot: snapshotbundle.SnapshotInput{
			SourceID: reviewDatasourceID, SourceNamespace: finalv5oracle.ExposureScaleSourceNamespace,
			Snapshot: finalv5oracle.ExposureScaleSnapshot, SchemaDigest: schemaDigest,
			Fields: []snapshotbundle.SnapshotField{
				{Name: "member_rank", SQLType: "bigint"},
				{Name: "metric", SQLType: "numeric"},
				{Name: "family_id", SQLType: "integer"},
				{Name: "partition_key", SQLType: "integer"},
			},
			Rows: []snapshotbundle.SnapshotRow{},
		},
	}
}

func expectedDigests(manifest snapshotbundle.BundleManifest) snapshotbundle.ExpectedDigests {
	return snapshotbundle.ExpectedDigests{
		SidecarDigest:     manifest.DictionaryManifest.SidecarDigest,
		DictionaryDigest:  manifest.DictionaryManifest.DictionaryDigest,
		ManifestDigest:    manifest.ManifestDigest,
		ColdPayloadDigest: manifest.DictionaryManifest.ColdPayloadDigest,
		HotIndexDigest:    manifest.DictionaryManifest.HotIndexDigest,
	}
}

func exposurePublicationFactStream(yield func(finalv5oracle.CanonicalFact) error) error {
	return finalv5oracle.StreamExposureScaleFacts(0, finalv5oracle.ExposureScaleMaximumDatasetFacts, yield)
}

func reopenReviewedPublication(written snapshotbundle.WrittenBundle) (*ordinal.HotDictionary, *ordinal.ColdDictionary, error) {
	manifestPath := filepath.Join(written.Directory, publicationName+".bundle.json")
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read reviewed bundle manifest: %w", err)
	}
	manifest, err := snapshotbundle.DecodeBundleManifest(bytes.NewReader(manifestBytes))
	if err != nil || !reflect.DeepEqual(manifest, written.Manifest) {
		return nil, nil, fmt.Errorf("verify reviewed bundle manifest: %w", errors.Join(err, errors.New("manifest identity changed")))
	}
	hotBytes, err := readDescriptorFile(written.Directory, manifest.Hot)
	if err != nil {
		return nil, nil, err
	}
	hot, err := ordinal.ParseHotDictionary(hotBytes, manifest.ManifestDigest)
	if err != nil {
		return nil, nil, fmt.Errorf("parse reviewed HOT dictionary: %w", err)
	}
	coldBytes, err := readDescriptorFile(written.Directory, manifest.Cold)
	if err != nil {
		return nil, nil, err
	}
	cold, err := ordinal.ParseColdDictionary(coldBytes, manifest.ManifestDigest)
	if err != nil {
		return nil, nil, fmt.Errorf("parse reviewed COLD dictionary: %w", err)
	}
	sidecarPath := filepath.Join(written.Directory, manifest.Sidecar.Name)
	sidecar, err := os.Open(sidecarPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open reviewed ordinal sidecar: %w", err)
	}
	defer sidecar.Close()
	if err := snapshotbundle.VerifySidecarNDJSON(sidecar, hot, snapshotbundle.SidecarExpectation{
		PublicationName: publicationName, OrdinalSidecar: reviewSidecar,
		SourceNamespace: finalv5oracle.ExposureScaleSourceNamespace,
		ManifestDigest:  manifest.ManifestDigest, SidecarDigest: manifest.DictionaryManifest.SidecarDigest,
	}); err != nil {
		return nil, nil, fmt.Errorf("verify reviewed ordinal sidecar: %w", err)
	}
	if err := verifyDescriptorPath(sidecarPath, manifest.Sidecar); err != nil {
		return nil, nil, err
	}
	return hot, cold, nil
}

func readDescriptorFile(directory string, descriptor snapshotbundle.FileDescriptor) ([]byte, error) {
	path := filepath.Join(directory, descriptor.Name)
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read reviewed artifact %s: %w", descriptor.Name, err)
	}
	if int64(len(value)) != descriptor.Bytes || sha256Hex(value) != descriptor.SHA256 {
		return nil, fmt.Errorf("reviewed artifact %s differs from its descriptor", descriptor.Name)
	}
	return value, nil
}

func verifyDescriptorPath(path string, descriptor snapshotbundle.FileDescriptor) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != descriptor.Bytes {
		return errors.New("reviewed artifact is not the descriptor-bound regular file")
	}
	target := sha256.New()
	if _, err := io.Copy(target, file); err != nil {
		return err
	}
	if hex.EncodeToString(target.Sum(nil)) != descriptor.SHA256 {
		return errors.New("reviewed artifact transport digest mismatch")
	}
	return nil
}

func attestExposureSource(ctx context.Context, dsn string) (sourceAttestation, error) {
	var result sourceAttestation
	configuration, err := pgx.ParseConfig(strings.TrimSpace(dsn))
	if err != nil {
		return result, errors.New("parse Business PostgreSQL DSN")
	}
	configuration.ConnectTimeout = 10 * time.Second
	configuration.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	if configuration.RuntimeParams == nil {
		configuration.RuntimeParams = make(map[string]string)
	}
	configuration.RuntimeParams["application_name"] = "taskgate-final-v5-publication-review"
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		return result, errors.New("connect fixed exposure-scale source")
	}
	defer connection.Close(context.Background())
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, errors.New("begin exposure-scale source attestation")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	for _, setting := range [][2]string{
		{"search_path", "pg_catalog"}, {"statement_timeout", "600000ms"}, {"lock_timeout", "5s"},
		{"idle_in_transaction_session_timeout", "10min"}, {"TimeZone", "UTC"}, {"extra_float_digits", "3"},
	} {
		if _, err := tx.Exec(ctx, `SELECT pg_catalog.set_config($1, $2, true)`,
			pgx.QueryExecModeSimpleProtocol, setting[0], setting[1]); err != nil {
			return result, errors.New("set exposure-scale attestation transaction policy")
		}
	}
	var serverVersion string
	if err := tx.QueryRow(ctx, dataconnector.DatasourceIdentitySQL, pgx.QueryExecModeSimpleProtocol).Scan(
		&result.DatasourceID, &result.Database, &result.User, &serverVersion,
	); err != nil {
		return result, errors.New("read exposure-scale datasource identity")
	}
	serverVersionNumber, err := strconv.Atoi(serverVersion)
	if err != nil {
		return result, errors.New("parse exposure-scale PostgreSQL version")
	}
	result.PostgreSQLMajorVersion = serverVersionNumber / 10000
	if result.DatasourceID != reviewDatasourceID || result.Database != reviewDatabase || result.User != reviewDatabaseUser ||
		result.PostgreSQLMajorVersion != reviewPostgreSQLMajor {
		return result, errors.New("exposure-scale datasource identity differs from the fixed review target")
	}
	rows, err := tx.Query(ctx, dataconnector.ViewColumnAttestationSQL, pgx.QueryExecModeSimpleProtocol,
		"reporting", "final_v5_exposure_scale")
	if err != nil {
		return result, errors.New("read exposure-scale view columns")
	}
	var columns []dataconnector.SchemaColumn
	for rows.Next() {
		var column dataconnector.SchemaColumn
		if err := rows.Scan(&column.Name, &column.PostgreSQLType, &column.Collation,
			&column.CollationVersion, &column.CollationDeterministic); err != nil {
			rows.Close()
			return result, errors.New("scan exposure-scale view column")
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, errors.New("iterate exposure-scale view columns")
	}
	rows.Close()
	expectedColumns := []dataconnector.SchemaColumn{
		{Name: "member_rank", PostgreSQLType: "bigint", CollationDeterministic: true},
		{Name: "metric", PostgreSQLType: "numeric", CollationDeterministic: true},
		{Name: "family_id", PostgreSQLType: "integer", CollationDeterministic: true},
		{Name: "partition_key", PostgreSQLType: "integer", CollationDeterministic: true},
	}
	if !reflect.DeepEqual(columns, expectedColumns) {
		return result, fmt.Errorf("exposure-scale view columns are %+v; expected %+v", columns, expectedColumns)
	}
	var definition string
	if err := tx.QueryRow(ctx, dataconnector.ViewDefinitionAttestationSQL, pgx.QueryExecModeSimpleProtocol,
		"reporting", "final_v5_exposure_scale").Scan(&definition); err != nil {
		return result, errors.New("read exposure-scale view definition")
	}
	result.SchemaSHA256, err = dataconnector.SchemaDigest([]dataconnector.ViewSchema{{
		Schema: "reporting", View: "final_v5_exposure_scale", Definition: definition, Columns: columns,
	}})
	if err != nil {
		return result, errors.New("digest exposure-scale reporting surface")
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_statements`, pgx.QueryExecModeSimpleProtocol).
		Scan(&result.PreparedStatements); err != nil {
		return result, errors.New("read exposure-scale prepared-statement state")
	}
	if result.PreparedStatements != 0 {
		return result, fmt.Errorf("exposure-scale review session contains %d prepared statements; expected zero",
			result.PreparedStatements)
	}
	if err := tx.Commit(ctx); err != nil {
		return result, errors.New("commit exposure-scale source attestation")
	}
	committed = true
	return result, nil
}

func exposureCatalogCandidate(schemaDigest string, manifest snapshotbundle.BundleManifest) ([]byte, error) {
	if schemaDigest == "" || manifest.PublicationName != publicationName {
		return nil, errors.New("dedicated Catalog candidate requires the reviewed exposure-scale identities")
	}
	value := fmt.Sprintf(`# REVIEW_CANDIDATE; author_approved=false; runtime_activation=forbidden
catalog_version: %q

sources:
  - name: travel_demo
    datasource_id: taskgate-demo-travel
    type: postgres
    address: business-postgres
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    schema_digest: %s
    secretRef: GATEWAY_DB_PASSWORD

snapshot_publications:
  - name: final-v5-exposure-scale-v1
    source: travel_demo
    source_namespace: final_v5.exposure_scale
    snapshot: final-v5-exposure-scale-2026-v1
    ordinal_sidecar: taskgate_ordinal.final_v5_exposure_scale_v1
    sidecar_digest: %s
    dictionary_digest: %s
    manifest_digest: %s

scopes:
  - name: partition_key
    type: enum
    description: Frozen exposure-scale partition key
    allowed_values: ["1"]

products:
  - name: final_v5_exposure_scale
    source: travel_demo
    reporting_view: reporting.final_v5_exposure_scale
    description: Final-V5 controlled exposure and dependency scale relation
    sensitivity: low
    snapshot: final-v5-exposure-scale-2026-v1
    snapshot_publication: final-v5-exposure-scale-v1
    entity_key: [member_rank]
    fact_namespace: final_v5.exposure_scale
    stable_relation_role: final_v5_exposure_scale
    scopes: [partition_key]
    allowed_functions: []
    allowed_operators: ["=", ">", "<="]
    allowed_aggregates: [sum, count]
    fields:
      - name: member_rank
        type: bigint
        description: Stable deterministic exposure member rank
      - name: metric
        type: numeric
        description: Deterministic aggregate metric
      - name: family_id
        type: integer
        description: Fixed benchmark family identifier
      - name: partition_key
        type: integer
        description: Mandatory frozen partition key

approval_routes:
  - sensitivity: low
    mode: manual
    approver: final-v5-reviewer
    budget_profile: final-v5-exposure-scale-review-v1

budget_profiles:
  - name: final-v5-exposure-scale-review-v1
    max_queries: 128
    max_rows: 100000
    max_db_time: 30m
    query_timeout: 30m
    task_ttl: 2h
    max_release_facts: 2000000
    max_influence_facts: 3000000
    max_outcome_facts: 128
    exposure_profile_version: taskgate-exposure-v5
    predicate_footprint:
      version: taskgate-predicate-footprint-v1
      max_raw_literals_per_query: 64
      max_unique_atoms_per_query: 16
      max_atom_payload_bytes: 4096
      max_total_atom_payload_bytes: 65536
`, reviewCatalogVersion, schemaDigest, manifest.DictionaryManifest.SidecarDigest,
		manifest.DictionaryManifest.DictionaryDigest, manifest.ManifestDigest)
	bytes := []byte(value)
	parsed, err := catalog.Parse(bytes)
	if err != nil {
		return nil, fmt.Errorf("validate dedicated exposure-scale Catalog candidate: %w", err)
	}
	publication, found := parsed.LookupSnapshotPublication(publicationName)
	if !found || publication.DictionaryDigest != manifest.DictionaryManifest.DictionaryDigest ||
		publication.ManifestDigest != manifest.ManifestDigest || publication.SidecarDigest != manifest.DictionaryManifest.SidecarDigest {
		return nil, errors.New("dedicated Catalog candidate differs from the reviewed publication")
	}
	return bytes, nil
}

func readManifestSubtree(root, subtree string) (map[string][]byte, error) {
	base := filepath.Join(root, subtree)
	values := make(map[string][]byte)
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == base {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("manifest tree contains symlink %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > reviewManifestMaxBytes {
			return fmt.Errorf("manifest tree contains invalid file %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(relative, "..") {
			return errors.New("manifest path escapes the reviewed root")
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		values[filepath.ToSlash(relative)] = value
		return nil
	})
	return values, err
}

func summarizeProvSQLManifestSet(values map[string][]byte) (manifestSetReview, error) {
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	target := sha256.New()
	for _, path := range paths {
		if strings.ContainsAny(path, "\x00\r\n\t") || !strings.HasPrefix(path, "provsql/") {
			return manifestSetReview{}, errors.New("manifest set path is not canonical")
		}
		_, _ = fmt.Fprintf(target, "%s\t%s\n", path, sha256Hex(values[path]))
	}
	return manifestSetReview{
		Files: len(paths), Verifier: "VerifyProvSQLNonceJoinManifestSet",
		PathRoot:        "evaluation/final-v5-wsl2/oracle-manifests",
		AggregateRecord: "<path><TAB><manifest-sha256><LF>",
		AggregateSHA256: hex.EncodeToString(target.Sum(nil)),
	}, nil
}

func summarizeScaleManifestSet(values map[string][]byte) (manifestSetReview, error) {
	paths := make([]string, 0, len(values))
	for path := range values {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	target := sha256.New()
	for _, path := range paths {
		if strings.ContainsAny(path, "\x00\r\n\t") || !strings.HasPrefix(path, "scale/") {
			return manifestSetReview{}, errors.New("Scale manifest set path is not canonical")
		}
		repositoryPath := filepath.ToSlash(filepath.Join("evaluation", "final-v5-wsl2", "oracle-manifests", path))
		_, _ = fmt.Fprintf(target, "%s  %s\n", sha256Hex(values[path]), repositoryPath)
	}
	return manifestSetReview{
		Files: len(paths), Verifier: "VerifyExposureScaleDependencyManifestSet",
		PathRoot: ".", AggregateRecord: "<manifest-sha256><SPACE><SPACE><path><LF>",
		AggregateSHA256: hex.EncodeToString(target.Sum(nil)),
	}, nil
}

func reviewedScaleUnion(artifacts []finalv5oracle.ExposureScaleManifestArtifact) (scaleUnionReview, error) {
	result := scaleUnionReview{
		Role: reviewSemanticRole, Scale: reviewScaleAnchor,
		Cardinality: int64(finalv5oracle.ExposureScaleMaximumDatasetFacts), SetSHA256: reviewScaleUnionSHA,
	}
	for _, artifact := range artifacts {
		manifest := artifact.Manifest
		if manifest.Scale != reviewScaleAnchor {
			continue
		}
		if manifest.Mode != finalv5oracle.ExposureScaleModeNovel &&
			manifest.Mode != finalv5oracle.ExposureScaleModeSemanticReplay {
			return scaleUnionReview{}, errors.New("reviewed Scale union has an unsupported mode")
		}
		if manifest.Expected.UnionCardinality == nil ||
			*manifest.Expected.UnionCardinality != result.Cardinality ||
			manifest.Expected.UnionSetSHA256 != result.SetSHA256 {
			return scaleUnionReview{}, errors.New("reviewed Scale union commitments differ across modes")
		}
		result.Manifests = append(result.Manifests, manifestReference{Path: artifact.RelativePath, SHA256: artifact.SHA256})
	}
	sort.Slice(result.Manifests, func(i, j int) bool { return result.Manifests[i].Path < result.Manifests[j].Path })
	if err := validateScaleUnionReview(result); err != nil {
		return scaleUnionReview{}, err
	}
	return result, nil
}

func validateScaleUnionReview(review scaleUnionReview) error {
	want := []manifestReference{
		{Path: "scale/dependency-e2e/1035000-overlap-0/novel.json", SHA256: reviewScaleNovelSHA},
		{Path: "scale/dependency-e2e/1035000-overlap-0/semantic_replay.json", SHA256: reviewScaleReplaySHA},
	}
	if review.Role != reviewSemanticRole || review.Scale != reviewScaleAnchor ||
		review.Cardinality != int64(finalv5oracle.ExposureScaleMaximumDatasetFacts) ||
		review.SetSHA256 != reviewScaleUnionSHA || !reflect.DeepEqual(review.Manifests, want) {
		return errors.New("review report does not bind the two sealed maximum zero-overlap Scale union commitments")
	}
	return nil
}

func requireRepositoryRoot(root string) error {
	for _, relative := range []string{"go.mod", filepath.Join("docs", "codex_publication_execution_plan.md")} {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("repository root omits %s", relative)
		}
	}
	return nil
}

func requireFreshOutputDirectory(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("review output directory is required")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("review output directory already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() {
		return errors.New("review output parent directory does not exist")
	}
	return nil
}

func requirePrivateArtifactRoot(path string) error {
	info, err := os.Lstat(strings.TrimSpace(path))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("artifact root must be an existing private non-symlink directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		return errors.New("artifact root must be readable and empty")
	}
	return nil
}

func writeReviewDirectory(path string, files map[string][]byte) (resultErr error) {
	if err := os.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("create review output directory: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(path)
		}
	}()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if filepath.Base(name) != name || strings.ContainsAny(name, "\x00/\\") {
			return errors.New("review material file name is invalid")
		}
		file, err := os.OpenFile(filepath.Join(path, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		value := files[name]
		written, writeErr := file.Write(value)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil || written != len(value) {
			return errors.Join(writeErr, closeErr, errors.New("short review-material write"))
		}
	}
	complete = true
	return nil
}

func marshalJSONDocument(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func reviewFileFor(name string, value []byte) reviewFile {
	return reviewFile{Name: name, SHA256: sha256Hex(value), Bytes: int64(len(value))}
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
