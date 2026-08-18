package experiment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5linker"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const exposureScalePublicationV1 = "final-v5-exposure-scale-v1"

type deploymentScaleDependencySetVerifierV1 struct {
	dsn       string
	mu        sync.Mutex
	byProfile map[string]*finalv5linker.ReviewedUniverse
}

func newDeploymentScaleDependencySetVerifierV1(dsn string) *deploymentScaleDependencySetVerifierV1 {
	return &deploymentScaleDependencySetVerifierV1{dsn: dsn,
		byProfile: make(map[string]*finalv5linker.ReviewedUniverse)}
}

func (verifier *deploymentScaleDependencySetVerifierV1) Verify(ctx context.Context,
	profile profileMaterialV3, expectation ScaleDependencySetExpectationV1,
	role DependencyScaleSummaryRole, productionSetSHA256 string) (ScaleDependencySetVerificationV1, error) {
	var result ScaleDependencySetVerificationV1
	expected, err := expectation.forRole(role)
	if err != nil {
		return result, err
	}
	universe, err := verifier.reviewedUniverse(profile)
	if err != nil {
		return result, err
	}
	actual, dictionarySet, err := verifier.readProductionSet(ctx, productionSetSHA256)
	if err != nil {
		return result, fmt.Errorf("read production Scale dependency set: %w", err)
	}
	stream, err := scaleDependencyFactStream(expectation.Scale, role)
	if err != nil {
		return result, err
	}
	report, linkErr := universe.Link(finalv5linker.SetRequest{
		Role: string(role), OracleFacts: stream,
		Expected: finalv5linker.SemanticExpectation{Cardinality: expected.Cardinality, SetSHA256: expected.SetSHA256},
		Actual:   actual, ActualSource: finalv5linker.ActualSetSourceProductionFactSet,
	})
	result = ScaleDependencySetVerificationV1{
		Version: ScaleDependencySetVerificationV1Version, Role: role, Match: report.Match,
		ExpectedCardinality: expected.Cardinality, ExpectedSemanticSetSHA256: expected.SetSHA256,
		ObservedCardinality:       report.ActualSemantic.Cardinality,
		ObservedSemanticSetSHA256: report.ActualSemantic.SetSHA256,
		ProductionSetSHA256:       productionSetSHA256, ProductionDictionarySHA256: dictionarySet,
		ObservedOrdinalSetSHA256: report.ActualOrdinalSetSHA256,
		ExpectedOrdinalsMissing:  report.Mismatches.ExpectedOrdinalsMissingInActual,
		UnexpectedActualOrdinals: report.Mismatches.UnexpectedActualOrdinals,
	}
	if report.DictionarySetSHA256 != dictionarySet {
		return result, errors.New("production Scale set dictionary closure differs from the activated publication")
	}
	if linkErr != nil {
		return result, fmt.Errorf("link production Scale dependency set to semantic oracle: %w", linkErr)
	}
	if err := result.Validate(); err != nil {
		return result, err
	}
	return result, nil
}

func scaleDependencyFactStream(scale string,
	role DependencyScaleSummaryRole) (finalv5linker.CanonicalFactStream, error) {
	spec, err := ParseDependencyScale(scale)
	if err != nil {
		return nil, err
	}
	state := spec.NovelState()
	var interval DependencyScaleMemberRankInterval
	switch role {
	case DependencyScaleCandidateSummaryRole:
		interval = state.Candidate.MemberRanks
	case DependencyScaleExistingSummaryRole:
		interval = state.History.MemberRanks
	case DependencyScaleUnionSummaryRole:
		interval = state.Union.MemberRanks
	default:
		return nil, fmt.Errorf("Scale dependency role %q is not frozen", role)
	}
	first := interval.LowerExclusive * dependencyScaleFactsPerRetainedRow
	last := interval.UpperInclusive * dependencyScaleFactsPerRetainedRow
	return func(yield func(finalv5oracle.CanonicalFact) error) error {
		return finalv5oracle.StreamExposureScaleFacts(first, last, yield)
	}, nil
}

func (verifier *deploymentScaleDependencySetVerifierV1) reviewedUniverse(
	profile profileMaterialV3) (*finalv5linker.ReviewedUniverse, error) {
	key := profile.CatalogPath + "\x00" + profile.SnapshotArtifactDir
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if universe := verifier.byProfile[key]; universe != nil {
		return universe, nil
	}
	logical, err := catalog.Load(profile.CatalogPath)
	if err != nil {
		return nil, fmt.Errorf("load Scale profile Catalog: %w", err)
	}
	if len(logical.SnapshotPublications) != 1 ||
		logical.SnapshotPublications[0].Name != exposureScalePublicationV1 {
		return nil, errors.New("Scale profile must activate exactly the exposure-scale publication")
	}
	if _, err := snapshotBindingsFromArtifactsV3(*logical, profile.SnapshotArtifactDir); err != nil {
		return nil, err
	}
	directory := filepath.Join(profile.SnapshotArtifactDir, exposureScalePublicationV1)
	manifestFile, err := os.Open(filepath.Join(directory, exposureScalePublicationV1+".bundle.json"))
	if err != nil {
		return nil, err
	}
	manifest, decodeErr := snapshotbundle.DecodeBundleManifest(manifestFile)
	closeErr := manifestFile.Close()
	if decodeErr != nil || closeErr != nil {
		return nil, errors.Join(decodeErr, closeErr)
	}
	hotBytes, err := readScaleDescriptor(directory, manifest.Hot)
	if err != nil {
		return nil, err
	}
	hot, err := ordinal.ParseHotDictionary(hotBytes, manifest.ManifestDigest)
	if err != nil {
		return nil, fmt.Errorf("parse Scale HOT dictionary: %w", err)
	}
	coldPath := filepath.Join(directory, manifest.Cold.Name)
	cold, err := os.Open(coldPath)
	if err != nil {
		return nil, err
	}
	info, statErr := cold.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() != manifest.Cold.Bytes {
		cold.Close()
		return nil, errors.Join(statErr, errors.New("Scale COLD artifact differs from its descriptor"))
	}
	closure, verifyErr := finalv5linker.VerifyColdClosure(cold, info.Size(), hot)
	closeErr = cold.Close()
	if verifyErr != nil || closeErr != nil {
		return nil, errors.Join(verifyErr, closeErr)
	}
	if closure.ArtifactSHA256 != manifest.Cold.SHA256 || closure.ArtifactBytes != manifest.Cold.Bytes {
		return nil, errors.New("Scale COLD transport identity differs from its descriptor")
	}
	universe, err := finalv5linker.ReviewPublications(logical.SHA256, finalv5linker.Publication{
		Name: exposureScalePublicationV1, Index: hot, ColdClosure: &closure,
	})
	if err != nil {
		return nil, fmt.Errorf("review activated Scale publication: %w", err)
	}
	verifier.byProfile[key] = universe
	return universe, nil
}

func readScaleDescriptor(directory string, descriptor snapshotbundle.FileDescriptor) ([]byte, error) {
	value, err := os.ReadFile(filepath.Join(directory, descriptor.Name))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(value)
	if int64(len(value)) != descriptor.Bytes || hex.EncodeToString(digest[:]) != descriptor.SHA256 {
		return nil, errors.New("Scale artifact differs from its descriptor")
	}
	return value, nil
}

func (verifier *deploymentScaleDependencySetVerifierV1) readProductionSet(ctx context.Context,
	setSHA256 string) (ordinal.BitmapSet, string, error) {
	var empty ordinal.BitmapSet
	configuration, err := pgx.ParseConfig(verifier.dsn)
	if err != nil {
		return empty, "", errors.New("parse Control PostgreSQL DSN")
	}
	configuration.ConnectTimeout = 10 * time.Second
	configuration.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		return empty, "", fmt.Errorf("connect Control PostgreSQL: %w", err)
	}
	defer connection.Close(context.Background())
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return empty, "", err
	}
	defer tx.Rollback(context.Background())
	var dictionarySet string
	var staticCardinality, dynamicCardinality int64
	if err := tx.QueryRow(ctx, `SELECT dictionary_set_digest,static_cardinality,dynamic_cardinality
FROM v4_bitmap_sets WHERE set_sha256=$1`, setSHA256).
		Scan(&dictionarySet, &staticCardinality, &dynamicCardinality); err != nil {
		return empty, "", err
	}
	if dynamicCardinality != 0 {
		return empty, "", errors.New("Scale dependency set unexpectedly contains dynamic Facts")
	}
	rows, err := tx.Query(ctx, `SELECT mapping.dictionary_digest,mapping.segment_id,mapping.high16,mapping.cardinality,
 container.container_sha256,container.dictionary_digest,container.segment_id,container.high16,
 container.cardinality,container.portable_payload
FROM v4_bitmap_set_containers mapping
JOIN v4_bitmap_containers container ON container.container_sha256=mapping.container_sha256
WHERE mapping.set_sha256=$1
ORDER BY mapping.dictionary_digest,mapping.segment_id,mapping.high16`, setSHA256)
	if err != nil {
		return empty, "", err
	}
	var containers []ordinal.PortableContainer
	for rows.Next() {
		var mapDictionary, mapSegment, digest, blobDictionary, blobSegment string
		var mapHigh, blobHigh int32
		var mapCardinality, blobCardinality int64
		var payload []byte
		if err := rows.Scan(&mapDictionary, &mapSegment, &mapHigh, &mapCardinality, &digest,
			&blobDictionary, &blobSegment, &blobHigh, &blobCardinality, &payload); err != nil {
			rows.Close()
			return empty, "", err
		}
		if mapDictionary != blobDictionary || mapSegment != blobSegment || mapHigh != blobHigh ||
			mapCardinality != blobCardinality || mapHigh < 0 || mapHigh > 65535 || mapCardinality <= 0 {
			rows.Close()
			return empty, "", errors.New("Scale dependency bitmap container mapping is corrupt")
		}
		containers = append(containers, ordinal.PortableContainer{Key: ordinal.ContainerKey{
			DictionaryDigest: mapDictionary, SegmentID: mapSegment, High16: uint16(mapHigh)},
			Bitmap: payload, Cardinality: uint64(mapCardinality), Digest: digest})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return empty, "", err
	}
	rows.Close()
	actual, err := ordinal.ParsePortableContainers(containers)
	if err != nil {
		return empty, "", err
	}
	if actual.Cardinality() != uint64(staticCardinality) {
		return empty, "", errors.New("Scale dependency bitmap cardinality differs from Control metadata")
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, "", err
	}
	return actual, dictionarySet, nil
}
