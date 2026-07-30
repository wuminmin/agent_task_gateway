package v4oracle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

const (
	maximumPointRelease   = uint64(12)
	maximumPointInfluence = uint64(1_035_000)
	maximumPointOutcome   = uint64(1)
	maximumPointRows      = int64(225_000)
	snapshotID            = "exposure-scale-2026-v4-narrow-1"
	ordersNamespace       = "evaluation.scale_orders"
	lineitemNamespace     = "evaluation.scale_lineitem"
	hybridSetDomain       = "TASKGATE-V4-HYBRID-SET-V1\x00"
	observationDomain     = "TASKGATE-V4-OBSERVATION-V1\x00"
)

type resultEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	Status        string         `json:"status"`
	Acceptance    string         `json:"acceptance"`
	Samples       []resultSample `json:"samples"`
}

type resultSample struct {
	Phase    string          `json:"phase"`
	Status   string          `json:"status"`
	Exposure *resultExposure `json:"exposure"`
}

type resultExposure struct {
	ProfileVersion      string `json:"profile_version"`
	ActualRelease       int64  `json:"actual_release_facts"`
	ActualInfluence     int64  `json:"actual_influence_facts"`
	ActualOutcome       int64  `json:"actual_outcome_facts"`
	ObservationSHA256   string `json:"observation_sha256"`
	DictionarySetSHA256 string `json:"dictionary_set_digest"`
	ReleaseSetSHA256    string `json:"release_set_sha256"`
	InfluenceSetSHA256  string `json:"influence_set_sha256"`
	OutcomeSetSHA256    string `json:"outcome_set_sha256"`
}

type committedObservation struct {
	Profile        string
	DictionarySet  string
	Release        committedSet
	Influence      committedSet
	Outcome        committedSet
	SHA256         string
	ReleaseCount   int64
	InfluenceCount int64
	OutcomeCount   int64
}

type committedSet struct {
	SHA256       string
	Static       ordinal.BitmapSet
	Dynamic      []dynamicFact
	StaticCount  int64
	DynamicCount int64
}

type dynamicFact struct {
	SHA256  string
	Kind    string
	Payload []byte
}

type publicationArtifact struct {
	Member   ordinal.DictionarySetMember
	Bundle   snapshotbundle.BundleManifest
	ColdPath string
}

type expectedGroup struct {
	status        int16
	revenue       pgtype.Numeric
	linePositions int64
	items         int64
	witness       map[string]*externalSorter
}

// Verify performs a read-only, repeatable-read audit. Spool files are created
// below Config.SpoolParent and removed on return; committed system state is
// never mutated.
func Verify(ctx context.Context, business, control *pgxpool.Pool, cfg Config) (Report, error) {
	started := time.Now().UTC()
	report := Report{SchemaVersion: ReportSchema, OracleID: OracleID, Status: "running", StartedAt: started,
		Boundary: Boundary{
			ExpectedSource:     "independent row-wise reconstruction from frozen reporting.scale_orders and reporting.scale_lineitem",
			ActualSource:       "committed Control-PG containers plus independently streamed COLD FactIDs",
			Algorithm:          "bounded external merge sort by full FactHash; exact canonical-payload and witness-multiplicity comparison",
			IndependenceScope:  "derivation-independent; shares the versioned canonical FactID specification and encoder with TaskGate",
			EvidenceValidation: "strict duplicate-key/trailing JSON rejection plus full-file SHA-256 binding of source-controlled V4 results",
			HotPathCalls:       0,
		},
	}
	fail := func(err error) (Report, error) {
		report.Status = "fail"
		report.FinishedAt = time.Now().UTC()
		report.Errors = append(report.Errors, err.Error())
		report.Gates = buildGates(report, err)
		return report, err
	}
	if business == nil || control == nil {
		return fail(errors.New("oracle requires Business and Control PostgreSQL pools"))
	}
	if cfg.SortMemoryBytes == 0 {
		cfg.SortMemoryBytes = defaultSortMemory
	}
	if cfg.SortMemoryBytes < 1<<20 || strings.TrimSpace(cfg.ArtifactDir) == "" || strings.TrimSpace(cfg.SpoolParent) == "" ||
		strings.TrimSpace(cfg.RepositoryRoot) == "" {
		return fail(errors.New("oracle requires artifact/spool/repository directories and at least 1 MiB sort memory"))
	}
	report.Resources.SortMemoryLimitBytes = cfg.SortMemoryBytes
	report.Provenance.OraclePackageSHA256 = SourceSHA256()
	if !validDigest(report.Provenance.OraclePackageSHA256) {
		return fail(errors.New("embedded oracle source binding is unavailable"))
	}
	sourceDigest, sourceFiles, err := repositorySourceDigest(cfg.RepositoryRoot)
	if err != nil {
		return fail(fmt.Errorf("bind oracle repository source scope: %w", err))
	}
	report.Provenance.RepositorySourceSHA256 = sourceDigest
	report.Provenance.RepositorySourceFiles = sourceFiles
	report.Provenance.ExecutableSHA256 = executableSHA256()
	if !validDigest(report.Provenance.ExecutableSHA256) {
		return fail(errors.New("oracle executable source binding is unavailable"))
	}

	evidence, evidenceSHA, err := readResultEvidence(cfg.ResultsPath)
	if err != nil {
		return fail(err)
	}
	report.Provenance.ResultsSHA256 = evidenceSHA
	identity, err := maximumPointIdentity(evidence)
	if err != nil {
		return fail(err)
	}
	report.Observation = Observation{SHA256: identity.ObservationSHA256,
		DictionarySetSHA256: identity.DictionarySetSHA256, ReleaseSetSHA256: identity.ReleaseSetSHA256,
		InfluenceSetSHA256: identity.InfluenceSetSHA256, OutcomeSetSHA256: identity.OutcomeSetSHA256}

	controlTx, err := control.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fail(fmt.Errorf("begin Control oracle transaction: %w", err))
	}
	defer controlTx.Rollback(context.Background())
	committed, members, err := loadCommittedObservation(ctx, controlTx, identity)
	if err != nil {
		return fail(err)
	}
	artifacts, err := resolveArtifacts(cfg.ArtifactDir, members)
	if err != nil {
		return fail(err)
	}
	if err := controlTx.Commit(ctx); err != nil {
		return fail(fmt.Errorf("commit read-only Control oracle transaction: %w", err))
	}

	spoolDir, err := os.MkdirTemp(cfg.SpoolParent, "taskgate-v4-million-oracle-")
	if err != nil {
		return fail(fmt.Errorf("create oracle spool: %w", err))
	}
	defer os.RemoveAll(spoolDir)
	resourceTracker := &sortResourceTracker{}
	expectedInfluence, err := newExternalSorter(spoolDir, "expected-influence", cfg.SortMemoryBytes, resourceTracker)
	if err != nil {
		return fail(err)
	}
	actualInfluence, err := newExternalSorter(spoolDir, "actual-influence", cfg.SortMemoryBytes, resourceTracker)
	if err != nil {
		return fail(err)
	}

	businessTx, err := business.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fail(fmt.Errorf("begin Business oracle transaction: %w", err))
	}
	defer businessTx.Rollback(context.Background())
	groups, err := loadExpectedGroups(ctx, businessTx, spoolDir, cfg.SortMemoryBytes, resourceTracker)
	if err != nil {
		return fail(err)
	}
	rows, err := generateExpectedFacts(ctx, businessTx, groups, expectedInfluence)
	if err != nil {
		return fail(err)
	}
	report.Resources.BusinessRows = rows
	if rows != maximumPointRows {
		return fail(fmt.Errorf("Business maximum point has %d joined rows, want %d", rows, maximumPointRows))
	}
	if err := businessTx.Commit(ctx); err != nil {
		return fail(fmt.Errorf("commit read-only Business oracle transaction: %w", err))
	}

	for _, artifact := range artifacts {
		scan, scanErr := scanColdDictionary(artifact.ColdPath, artifact.Member.ManifestDigest,
			artifact.Member.DictionaryDigest, artifact.Bundle.Cold.SHA256, func(fact coldFact) error {
				if !committed.Influence.Static.Contains(fact.Ref) {
					return nil
				}
				return actualInfluence.Add(factRecord{Hash: fact.Hash, Multiplicity: 1, Payload: fact.Payload})
			})
		if scanErr != nil {
			return fail(fmt.Errorf("scan COLD publication %s: %w", artifact.Member.PublicationName, scanErr))
		}
		report.Resources.ColdFactsScanned += scan.Facts
		report.Provenance.Artifacts = append(report.Provenance.Artifacts, ArtifactBinding{
			PublicationName: artifact.Member.PublicationName, DictionarySHA256: artifact.Member.DictionaryDigest,
			ManifestSHA256: artifact.Member.ManifestDigest, ArtifactSHA256: scan.SHA256, Bytes: artifact.Bundle.Cold.Bytes})
	}

	expectedIterator, err := expectedInfluence.Finish()
	if err != nil {
		return fail(err)
	}
	defer expectedIterator.Close()
	actualIterator, err := actualInfluence.Finish()
	if err != nil {
		return fail(err)
	}
	defer actualIterator.Close()
	comparison, err := compareFactStreams(expectedIterator, actualIterator)
	report.Facts.ExpectedInfluence = comparison.Matched + comparison.Missing
	report.Facts.MatchedInfluence = comparison.Matched
	report.Facts.HashMismatches += comparison.HashMismatches
	report.Facts.CanonicalPayloadMismatches += comparison.PayloadMismatches
	report.Facts.MissingFacts += comparison.Missing
	report.Facts.ExtraFacts += comparison.Extra
	report.Facts.InfluenceChunkSHA256 = comparison.ChunkSHA256
	if err != nil {
		return fail(fmt.Errorf("Influence FactID mismatch: %w", err))
	}
	report.Facts.ActualInfluence = committed.Influence.Static.Cardinality() + uint64(len(committed.Influence.Dynamic))
	report.Facts.FactHashMatches += comparison.Matched
	report.Facts.CanonicalPayloadMatches += comparison.Matched
	if comparison.Matched != maximumPointInfluence {
		return fail(fmt.Errorf("matched Influence facts=%d, want %d", comparison.Matched, maximumPointInfluence))
	}

	releases, witnessSummary, err := buildExpectedReleases(groups)
	if err != nil {
		return fail(err)
	}
	report.Witnesses = witnessSummary
	report.Facts.ExpectedRelease = uint64(len(releases))
	report.Facts.ActualRelease = uint64(committed.Release.StaticCount + committed.Release.DynamicCount)
	matchedRelease, err := compareDynamicFacts(releases, committed.Release.Dynamic, "DERIVED_RELEASE")
	if err != nil {
		return fail(fmt.Errorf("Release FactID mismatch: %w", err))
	}
	report.Facts.MatchedRelease = matchedRelease
	report.Witnesses.MatchedCommitments = int(matchedRelease)
	report.Witnesses.CommitmentMismatches = report.Witnesses.DerivedFacts - int(matchedRelease)
	report.Facts.FactHashMatches += matchedRelease
	report.Facts.CanonicalPayloadMatches += matchedRelease

	normalForm, err := maximumPointNormalForm()
	if err != nil {
		return fail(err)
	}
	report.Observation.NormalFormSHA256 = normalForm
	outcomeDigest, err := exposure.ReleaseOutcomeDigest(releases, 3)
	if err != nil {
		return fail(err)
	}
	outcome, err := exposure.NewOutcomeFactV3(queryplan.NormalFormVersion, normalForm, outcomeDigest, 3)
	if err != nil {
		return fail(err)
	}
	report.Facts.ExpectedOutcome = 1
	report.Facts.ActualOutcome = uint64(committed.Outcome.StaticCount + committed.Outcome.DynamicCount)
	matchedOutcome, err := compareDynamicFacts([]exposure.FactID{outcome}, committed.Outcome.Dynamic, "OUTCOME")
	if err != nil {
		return fail(fmt.Errorf("Outcome FactID mismatch: %w", err))
	}
	report.Facts.MatchedOutcome = matchedOutcome
	report.Facts.FactHashMatches += matchedOutcome
	report.Facts.CanonicalPayloadMatches += matchedOutcome

	empty, err := ordinal.NewBitmapSet()
	if err != nil {
		return fail(fmt.Errorf("construct empty static effect set: %w", err))
	}
	expectedReleaseDynamic, err := dynamicFacts(releases, "DERIVED_RELEASE")
	if err != nil {
		return fail(err)
	}
	expectedOutcomeDynamic, err := dynamicFacts([]exposure.FactID{outcome}, "OUTCOME")
	if err != nil {
		return fail(err)
	}
	releaseDigest, err := hybridSetDigest(committed.DictionarySet, empty, expectedReleaseDynamic)
	if err != nil || releaseDigest != committed.Release.SHA256 {
		return fail(errors.New("independent Release effect digest differs from committed set"))
	}
	outcomeSetDigest, err := hybridSetDigest(committed.DictionarySet, empty, expectedOutcomeDynamic)
	if err != nil || outcomeSetDigest != committed.Outcome.SHA256 {
		return fail(errors.New("independent Outcome effect digest differs from committed set"))
	}
	influenceDigest, err := hybridSetDigest(committed.DictionarySet, committed.Influence.Static, nil)
	if err != nil || influenceDigest != committed.Influence.SHA256 {
		return fail(errors.New("expanded Influence effect digest differs from committed set"))
	}
	recomputed := observationDigest(committed.DictionarySet, releaseDigest, influenceDigest, outcomeSetDigest,
		int64(len(releases)), int64(comparison.Matched), 1)
	report.Observation.RecomputedSHA256 = recomputed
	if recomputed != committed.SHA256 || recomputed != identity.ObservationSHA256 {
		return fail(errors.New("recomputed observation identity differs from results/Control PG"))
	}

	for _, sorter := range append([]*externalSorter{expectedInfluence, actualInfluence}, witnessSorters(groups)...) {
		report.Resources.SpoolBytes += sorter.spool
		report.Resources.SortRuns += len(sorter.runs)
		report.Resources.SortRunSHA256 = append(report.Resources.SortRunSHA256, sorter.runDigests...)
	}
	report.Resources.MaximumResidentRecords = resourceTracker.maximumRecords
	report.Resources.PeakRSSBytes = resourceTracker.peakRSS
	sorterCount := int64(2 + len(witnessSorters(groups)))
	if cfg.SortMemoryBytes > math.MaxInt64/sorterCount {
		return fail(errors.New("external-sort buffer bound overflows int64"))
	}
	report.Resources.TheoreticalBufferBoundBytes = cfg.SortMemoryBytes * sorterCount
	report.Facts.TotalCompared = report.Facts.MatchedRelease + report.Facts.MatchedInfluence + report.Facts.MatchedOutcome
	if report.Facts.ExpectedRelease != maximumPointRelease || report.Facts.ActualRelease != maximumPointRelease ||
		report.Facts.ExpectedInfluence != maximumPointInfluence || report.Facts.ActualInfluence != maximumPointInfluence ||
		report.Facts.ExpectedOutcome != maximumPointOutcome || report.Facts.ActualOutcome != maximumPointOutcome ||
		report.Facts.TotalCompared != maximumPointRelease+maximumPointInfluence+maximumPointOutcome ||
		report.Facts.FactHashMatches != report.Facts.TotalCompared ||
		report.Facts.CanonicalPayloadMatches != report.Facts.TotalCompared || report.Facts.HashMismatches != 0 ||
		report.Facts.CanonicalPayloadMismatches != 0 || report.Facts.MissingFacts != 0 || report.Facts.ExtraFacts != 0 ||
		report.Witnesses.DerivedFacts != int(maximumPointRelease) ||
		report.Witnesses.MatchedCommitments != int(maximumPointRelease) || report.Witnesses.CommitmentMismatches != 0 {
		return fail(errors.New("maximum-point exact FactID/witness contract is incomplete"))
	}
	if resourceTracker.currentRecords != 0 || report.Resources.SortRuns == 0 || report.Resources.SpoolBytes == 0 ||
		report.Resources.MaximumResidentRecords == 0 || report.Resources.PeakRSSBytes == 0 {
		return fail(errors.New("bounded external-sort resource accounting is incomplete"))
	}
	report.Status = "pass"
	report.FinishedAt = time.Now().UTC()
	report.Gates = buildGates(report, nil)
	return report, nil
}

func readResultEvidence(path string) (resultEnvelope, string, error) {
	var result resultEnvelope
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 1<<30 {
		return result, "", errors.New("V4 results are not a bounded regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return result, "", err
	}
	if err := validateUniqueJSON(raw); err != nil {
		return result, "", fmt.Errorf("V4 results JSON is not structurally strict: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&result); err != nil {
		return result, "", errors.New("V4 results are not valid schema-1 JSON")
	}
	if err := requireJSONEOF(decoder); err != nil || result.SchemaVersion != 1 ||
		result.Status != "complete_measured_campaign" || result.Acceptance != "pass" {
		return result, "", errors.New("V4 results are not a completed schema-1 campaign")
	}
	digest := sha256.Sum256(raw)
	return result, hex.EncodeToString(digest[:]), nil
}

func validateUniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := validateUniqueJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateUniqueJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := validateUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := validateUniqueJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func maximumPointIdentity(result resultEnvelope) (resultExposure, error) {
	var selected *resultExposure
	count := 0
	for _, sample := range result.Samples {
		one := sample.Exposure
		if sample.Phase != "novel" || sample.Status != "measured" || one == nil ||
			one.ActualRelease != int64(maximumPointRelease) || one.ActualInfluence != int64(maximumPointInfluence) ||
			one.ActualOutcome != int64(maximumPointOutcome) {
			continue
		}
		if one.ProfileVersion != exposure.ProfileV4 {
			return resultExposure{}, errors.New("maximum-point sample is not V4")
		}
		for _, digest := range []string{one.ObservationSHA256, one.DictionarySetSHA256, one.ReleaseSetSHA256,
			one.InfluenceSetSHA256, one.OutcomeSetSHA256} {
			if !validDigest(digest) {
				return resultExposure{}, errors.New("maximum-point sample has a malformed digest")
			}
		}
		if selected == nil {
			copy := *one
			selected = &copy
		} else if selected.ObservationSHA256 != one.ObservationSHA256 || selected.DictionarySetSHA256 != one.DictionarySetSHA256 ||
			selected.ReleaseSetSHA256 != one.ReleaseSetSHA256 || selected.InfluenceSetSHA256 != one.InfluenceSetSHA256 ||
			selected.OutcomeSetSHA256 != one.OutcomeSetSHA256 {
			return resultExposure{}, errors.New("maximum-point samples disagree on exact effect identity")
		}
		count++
	}
	if selected == nil || count == 0 {
		return resultExposure{}, errors.New("V4 results contain no measured maximum-point novel sample")
	}
	return *selected, nil
}

func loadCommittedObservation(ctx context.Context, tx pgx.Tx, expected resultExposure) (committedObservation, []ordinal.DictionarySetMember, error) {
	result := committedObservation{SHA256: expected.ObservationSHA256}
	err := tx.QueryRow(ctx, `SELECT profile_version,dictionary_set_digest,release_set_sha256,influence_set_sha256,
outcome_set_sha256,actual_release_facts,actual_influence_facts,actual_outcome_facts
FROM v4_observations WHERE observation_sha256=$1`, expected.ObservationSHA256).Scan(&result.Profile, &result.DictionarySet,
		&result.Release.SHA256, &result.Influence.SHA256, &result.Outcome.SHA256,
		&result.ReleaseCount, &result.InfluenceCount, &result.OutcomeCount)
	if err != nil {
		return result, nil, fmt.Errorf("load committed observation: %w", err)
	}
	if result.Profile != exposure.ProfileV4 || result.DictionarySet != expected.DictionarySetSHA256 ||
		result.Release.SHA256 != expected.ReleaseSetSHA256 || result.Influence.SHA256 != expected.InfluenceSetSHA256 ||
		result.Outcome.SHA256 != expected.OutcomeSetSHA256 || result.ReleaseCount != int64(maximumPointRelease) ||
		result.InfluenceCount != int64(maximumPointInfluence) || result.OutcomeCount != int64(maximumPointOutcome) {
		return result, nil, errors.New("Control observation differs from digest-bound results")
	}
	var manifestRaw []byte
	if err := tx.QueryRow(ctx, `SELECT manifest_json FROM v4_dictionary_sets WHERE dictionary_set_digest=$1`, result.DictionarySet).Scan(&manifestRaw); err != nil {
		return result, nil, err
	}
	var manifest ordinal.DictionarySetManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return result, nil, err
	}
	computed, err := manifest.Digest()
	if err != nil || computed != result.DictionarySet {
		return result, nil, errors.New("Control dictionary-set manifest digest mismatch")
	}
	for _, target := range []*committedSet{&result.Release, &result.Influence, &result.Outcome} {
		value, err := loadCommittedSet(ctx, tx, result.DictionarySet, target.SHA256)
		if err != nil {
			return result, nil, err
		}
		*target = value
	}
	return result, manifest.Members, nil
}

func loadCommittedSet(ctx context.Context, tx pgx.Tx, dictionarySet, setSHA string) (committedSet, error) {
	result := committedSet{SHA256: setSHA}
	var storedDictionary string
	if err := tx.QueryRow(ctx, `SELECT dictionary_set_digest,static_cardinality,dynamic_cardinality
FROM v4_bitmap_sets WHERE set_sha256=$1`, setSHA).Scan(&storedDictionary, &result.StaticCount, &result.DynamicCount); err != nil {
		return result, err
	}
	if storedDictionary != dictionarySet || result.StaticCount < 0 || result.DynamicCount < 0 {
		return result, errors.New("committed bitmap set header is corrupt")
	}
	rows, err := tx.Query(ctx, `SELECT mapping.dictionary_digest,mapping.segment_id,mapping.high16,mapping.cardinality,
container.portable_payload,container.container_sha256
FROM v4_bitmap_set_containers mapping JOIN v4_bitmap_containers container
ON container.container_sha256=mapping.container_sha256
WHERE mapping.set_sha256=$1 ORDER BY mapping.dictionary_digest,mapping.segment_id,mapping.high16`, setSHA)
	if err != nil {
		return result, err
	}
	var containers []ordinal.PortableContainer
	for rows.Next() {
		var dictionary, segment, digest string
		var high int32
		var cardinality int64
		var payload []byte
		if err := rows.Scan(&dictionary, &segment, &high, &cardinality, &payload, &digest); err != nil {
			rows.Close()
			return result, err
		}
		if high < 0 || high > math.MaxUint16 || cardinality <= 0 {
			rows.Close()
			return result, errors.New("committed bitmap container metadata is corrupt")
		}
		containers = append(containers, ordinal.PortableContainer{Key: ordinal.ContainerKey{DictionaryDigest: dictionary,
			SegmentID: segment, High16: uint16(high)}, Bitmap: payload, Cardinality: uint64(cardinality), Digest: digest})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	result.Static, err = ordinal.ParsePortableContainers(containers)
	if err != nil || result.Static.Cardinality() != uint64(result.StaticCount) {
		return result, errors.New("committed bitmap containers are noncanonical or have wrong cardinality")
	}
	dynamicRows, err := tx.Query(ctx, `SELECT fact.fact_sha256,fact.fact_kind,fact.canonical_payload
FROM v4_bitmap_set_dynamic_facts member JOIN v4_dynamic_facts fact ON fact.fact_sha256=member.fact_sha256
WHERE member.set_sha256=$1 ORDER BY fact.fact_sha256`, setSHA)
	if err != nil {
		return result, err
	}
	for dynamicRows.Next() {
		var fact dynamicFact
		if err := dynamicRows.Scan(&fact.SHA256, &fact.Kind, &fact.Payload); err != nil {
			dynamicRows.Close()
			return result, err
		}
		result.Dynamic = append(result.Dynamic, fact)
	}
	if err := dynamicRows.Err(); err != nil {
		dynamicRows.Close()
		return result, err
	}
	dynamicRows.Close()
	if int64(len(result.Dynamic)) != result.DynamicCount {
		return result, errors.New("committed dynamic fact cardinality mismatch")
	}
	computed, err := hybridSetDigest(dictionarySet, result.Static, result.Dynamic)
	if err != nil || computed != setSHA {
		return result, errors.New("committed hybrid set digest mismatch")
	}
	return result, nil
}

func resolveArtifacts(root string, members []ordinal.DictionarySetMember) ([]publicationArtifact, error) {
	result := make([]publicationArtifact, 0, len(members))
	for _, member := range members {
		directory := filepath.Join(root, member.PublicationName)
		manifestPath := filepath.Join(directory, member.PublicationName+".bundle.json")
		info, err := os.Lstat(manifestPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 4<<20 {
			return nil, fmt.Errorf("publication %s has no bounded bundle manifest", member.PublicationName)
		}
		file, err := os.Open(manifestPath)
		if err != nil {
			return nil, err
		}
		bundle, decodeErr := snapshotbundle.DecodeBundleManifest(file)
		closeErr := file.Close()
		if decodeErr != nil || closeErr != nil || bundle.PublicationName != member.PublicationName ||
			bundle.ManifestDigest != member.ManifestDigest || bundle.DictionaryManifest.DictionaryDigest != member.DictionaryDigest {
			return nil, fmt.Errorf("publication %s bundle does not match dictionary-set member", member.PublicationName)
		}
		result = append(result, publicationArtifact{Member: member, Bundle: bundle, ColdPath: filepath.Join(directory, bundle.Cold.Name)})
	}
	return result, nil
}

func loadExpectedGroups(ctx context.Context, tx pgx.Tx, spoolDir string, limit int64,
	tracker *sortResourceTracker) ([]*expectedGroup, error) {
	rows, err := tx.Query(ctx, `SELECT o.o_orderstatus,sum(l.l_extendedprice),sum(l.l_linenumber)::bigint,count(*)::bigint
FROM reporting.scale_orders o JOIN reporting.scale_lineitem l ON l.l_orderkey=o.o_orderkey
WHERE o.dataset_partition=1 AND l.dataset_partition=1 AND l.l_orderkey<=45000
GROUP BY o.o_orderstatus ORDER BY o.o_orderstatus`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []*expectedGroup
	for rows.Next() {
		group := &expectedGroup{witness: make(map[string]*externalSorter)}
		if err := rows.Scan(&group.status, &group.revenue, &group.linePositions, &group.items); err != nil {
			return nil, err
		}
		for _, output := range []string{"status", "revenue", "line_positions", "items"} {
			sorter, err := newExternalSorter(spoolDir, fmt.Sprintf("witness-%d-%s", group.status, output), limit, tracker)
			if err != nil {
				return nil, err
			}
			group.witness[output] = sorter
		}
		result = append(result, group)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(result) != 3 {
		return nil, fmt.Errorf("maximum-point group count=%d, want 3", len(result))
	}
	return result, nil
}

func generateExpectedFacts(ctx context.Context, tx pgx.Tx, groups []*expectedGroup, influence *externalSorter) (int64, error) {
	byStatus := make(map[int16]*expectedGroup, len(groups))
	for _, group := range groups {
		byStatus[group.status] = group
	}
	rows, err := tx.Query(ctx, `SELECT o.o_orderkey,o.o_orderstatus,l.l_linenumber,l.l_extendedprice
FROM reporting.scale_orders o JOIN reporting.scale_lineitem l ON l.l_orderkey=o.o_orderkey
WHERE o.dataset_partition=1 AND l.dataset_partition=1 AND l.l_orderkey<=45000
ORDER BY o.o_orderkey,l.l_linenumber`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var count, previousOrder int64
	var orderRow, orderKey, orderStatus exposure.FactID
	for rows.Next() {
		var order int64
		var status int16
		var lineNumber int32
		var price pgtype.Numeric
		if err := rows.Scan(&order, &status, &lineNumber, &price); err != nil {
			return count, err
		}
		group := byStatus[status]
		if group == nil {
			return count, fmt.Errorf("joined row has unknown group status %d", status)
		}
		if order != previousOrder {
			orderEntity, err := entityKey(ordersNamespace, []entityComponent{{"o_orderkey", "bigint", order}})
			if err != nil {
				return count, err
			}
			orderRow, err = exposure.NewBaseRowFactV2(ordersNamespace, snapshotID, orderEntity)
			if err != nil {
				return count, fmt.Errorf("construct order row FactID: %w", err)
			}
			orderKey, err = exposure.NewBaseCellFactV2(ordersNamespace, snapshotID, orderEntity,
				"scale_orders.o_orderkey", "bigint", order)
			if err != nil {
				return count, fmt.Errorf("construct order-key FactID: %w", err)
			}
			orderStatus, err = exposure.NewBaseCellFactV2(ordersNamespace, snapshotID, orderEntity,
				"scale_orders.o_orderstatus", "smallint", status)
			if err != nil {
				return count, fmt.Errorf("construct order-status FactID: %w", err)
			}
			for _, fact := range []exposure.FactID{orderRow, orderKey, orderStatus} {
				if err := addFact(influence, fact, 1); err != nil {
					return count, err
				}
			}
			previousOrder = order
		}
		lineEntity, err := entityKey(lineitemNamespace, []entityComponent{{"l_orderkey", "bigint", order},
			{"l_linenumber", "integer", lineNumber}})
		if err != nil {
			return count, err
		}
		lineRow, err := exposure.NewBaseRowFactV2(lineitemNamespace, snapshotID, lineEntity)
		if err != nil {
			return count, fmt.Errorf("construct line row FactID: %w", err)
		}
		lineKey, err := exposure.NewBaseCellFactV2(lineitemNamespace, snapshotID, lineEntity,
			"scale_lineitem.l_orderkey", "bigint", order)
		if err != nil {
			return count, fmt.Errorf("construct line order-key FactID: %w", err)
		}
		lineNo, err := exposure.NewBaseCellFactV2(lineitemNamespace, snapshotID, lineEntity,
			"scale_lineitem.l_linenumber", "integer", lineNumber)
		if err != nil {
			return count, fmt.Errorf("construct line-number FactID: %w", err)
		}
		linePrice, err := exposure.NewBaseCellFactV2(lineitemNamespace, snapshotID, lineEntity,
			"scale_lineitem.l_extendedprice", "numeric", price)
		if err != nil {
			return count, fmt.Errorf("construct line-price FactID: %w", err)
		}
		for _, fact := range []exposure.FactID{lineRow, lineKey, lineNo, linePrice} {
			if err := addFact(influence, fact, 1); err != nil {
				return count, err
			}
		}
		for _, item := range []struct {
			output       string
			fact         exposure.FactID
			multiplicity uint64
		}{
			{"status", orderStatus, 1}, {"revenue", linePrice, 1}, {"line_positions", lineNo, 1},
			{"items", orderRow, 1}, {"items", lineRow, 1}, {"items", orderKey, 1}, {"items", lineKey, 2},
		} {
			if err := addFact(group.witness[item.output], item.fact, item.multiplicity); err != nil {
				return count, err
			}
		}
		count++
	}
	return count, rows.Err()
}

type entityComponent struct {
	column   string
	typeName string
	value    any
}

func entityKey(namespace string, components []entityComponent) (string, error) {
	values := []string{namespace}
	for _, component := range components {
		canonical, err := exposure.CanonicalSQLValue(component.typeName, component.value)
		if err != nil {
			return "", err
		}
		values = append(values, component.column, component.typeName, canonical)
	}
	return exposure.ComposeCanonicalKeyV2("base-entity", values...)
}

func addFact(sorter *externalSorter, fact exposure.FactID, multiplicity uint64) error {
	hashText, err := fact.Hash()
	if err != nil {
		return err
	}
	hashBytes, err := hex.DecodeString(hashText)
	if err != nil || len(hashBytes) != sha256.Size {
		return errors.New("FactID returned a malformed SHA-256")
	}
	var hashValue [sha256.Size]byte
	copy(hashValue[:], hashBytes)
	payload, err := fact.CanonicalPayload()
	if err != nil {
		return err
	}
	return sorter.Add(factRecord{Hash: hashValue, Multiplicity: multiplicity, Payload: payload})
}

type streamComparison struct {
	Matched, HashMismatches, PayloadMismatches, Missing, Extra uint64
	ChunkSHA256                                                []string
}

func compareFactStreams(expected, actual *mergeIterator) (streamComparison, error) {
	var result streamComparison
	chunk := sha256.New()
	chunkCount := uint64(0)
	flushChunk := func() {
		if chunkCount != 0 {
			result.ChunkSHA256 = append(result.ChunkSHA256, hex.EncodeToString(chunk.Sum(nil)))
			chunk = sha256.New()
			chunkCount = 0
		}
	}
	for {
		left, leftErr := expected.NextCombined()
		right, rightErr := actual.NextCombined()
		if errors.Is(leftErr, io.EOF) && errors.Is(rightErr, io.EOF) {
			flushChunk()
			return result, nil
		}
		if leftErr != nil || rightErr != nil {
			if errors.Is(leftErr, io.EOF) {
				result.Extra++
				return result, fmt.Errorf("actual set has extra FactHash %x", right.Hash)
			}
			if errors.Is(rightErr, io.EOF) {
				result.Missing++
				return result, fmt.Errorf("actual set omits expected FactHash %x", left.Hash)
			}
			return result, errors.Join(leftErr, rightErr)
		}
		if left.Hash != right.Hash {
			result.HashMismatches++
			return result, fmt.Errorf("expected FactHash %x, actual %x", left.Hash, right.Hash)
		}
		if left.Multiplicity != 1 || right.Multiplicity != 1 || !bytes.Equal(left.Payload, right.Payload) {
			result.PayloadMismatches++
			return result, fmt.Errorf("FactHash %x has a payload or set-multiplicity mismatch", left.Hash)
		}
		_, _ = chunk.Write(left.Hash[:])
		payloadHash := sha256.Sum256(left.Payload)
		_, _ = chunk.Write(payloadHash[:])
		result.Matched++
		chunkCount++
		if chunkCount == 65536 {
			flushChunk()
		}
	}
}

func buildExpectedReleases(groups []*expectedGroup) ([]exposure.FactID, WitnessChecks, error) {
	bundle := []exposure.SnapshotBinding{{SourceNamespace: lineitemNamespace, Snapshot: snapshotID},
		{SourceNamespace: ordersNamespace, Snapshot: snapshotID}}
	var result []exposure.FactID
	var summary WitnessChecks
	commitmentHash := sha256.New()
	multiplicityHash := sha256.New()
	for _, group := range groups {
		statusCanonical, err := exposure.CanonicalSQLValue("smallint", group.status)
		if err != nil {
			return nil, summary, err
		}
		rowKey, err := exposure.ComposeCanonicalKeyV2("group-row",
			ordersNamespace+".scale_orders.o_orderstatus\x00smallint\x00"+statusCanonical)
		if err != nil {
			return nil, summary, err
		}
		outputs := []struct {
			name, expression, sqlType string
			value                     any
		}{
			{"status", "group(" + ordersNamespace + ".scale_orders.o_orderstatus)", "smallint", group.status},
			{"revenue", "sum(" + lineitemNamespace + ".scale_lineitem.l_extendedprice)", "numeric", group.revenue},
			{"line_positions", "sum(" + lineitemNamespace + ".scale_lineitem.l_linenumber)", "bigint", group.linePositions},
			{"items", "count(*)", "bigint", group.items},
		}
		for _, output := range outputs {
			commitment, unique, total, err := witnessCommitment(group.witness[output.name])
			if err != nil {
				return nil, summary, err
			}
			fact, err := exposure.NewDerivedFactV2(bundle, rowKey, output.expression, output.sqlType, output.value, commitment)
			if err != nil {
				return nil, summary, err
			}
			result = append(result, fact)
			summary.DerivedFacts++
			summary.ExpectedWitnessItems += unique
			summary.ExpectedTotalMultiplicity += total
			writeStringHash(commitmentHash, commitment)
			writeStringHash(multiplicityHash, output.name)
			writeUint64Hash(multiplicityHash, unique)
			writeUint64Hash(multiplicityHash, total)
		}
	}
	type hashedFact struct {
		hash string
		fact exposure.FactID
	}
	ordered := make([]hashedFact, len(result))
	for index, fact := range result {
		hashText, err := fact.Hash()
		if err != nil {
			return nil, summary, fmt.Errorf("hash derived release FactID: %w", err)
		}
		ordered[index] = hashedFact{hash: hashText, fact: fact}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].hash < ordered[j].hash })
	for index := range ordered {
		result[index] = ordered[index].fact
	}
	summary.CommitmentSetSHA256 = hex.EncodeToString(commitmentHash.Sum(nil))
	summary.MultiplicityStreamSHA256 = hex.EncodeToString(multiplicityHash.Sum(nil))
	return result, summary, nil
}

func witnessCommitment(sorter *externalSorter) (string, uint64, uint64, error) {
	iterator, err := sorter.Finish()
	if err != nil {
		return "", 0, 0, err
	}
	var unique, total uint64
	for {
		record, nextErr := iterator.NextCombined()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			iterator.Close()
			return "", 0, 0, nextErr
		}
		unique++
		if math.MaxUint64-total < record.Multiplicity {
			iterator.Close()
			return "", 0, 0, errors.New("witness total multiplicity overflow")
		}
		total += record.Multiplicity
	}
	iterator.Close()
	iterator, err = sorter.Iterator()
	if err != nil {
		return "", 0, 0, err
	}
	defer iterator.Close()
	hashValue := sha256.New()
	writeStringHash(hashValue, "witness-multiset")
	if unique > math.MaxUint64/2 {
		return "", 0, 0, errors.New("witness item count overflow")
	}
	writeUint64Hash(hashValue, unique*2)
	for {
		record, nextErr := iterator.NextCombined()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return "", 0, 0, nextErr
		}
		writeStringHash(hashValue, hex.EncodeToString(record.Hash[:]))
		writeStringHash(hashValue, fmt.Sprintf("%020d", record.Multiplicity))
	}
	return hex.EncodeToString(hashValue.Sum(nil)), unique, total, nil
}

func maximumPointNormalForm() (string, error) {
	orders := queryplan.AlgebraPlanV2{Op: "scan", SourceNamespace: ordersNamespace, Snapshot: snapshotID,
		StableRole: "scale_orders", Schema: []queryplan.AlgebraFieldV2{
			{ID: "scale_orders.dataset_partition", SQLType: "smallint"},
			{ID: "scale_orders.o_orderkey", SQLType: "bigint"}, {ID: "scale_orders.o_orderstatus", SQLType: "smallint"},
		}}
	lineitems := queryplan.AlgebraPlanV2{Op: "scan", SourceNamespace: lineitemNamespace, Snapshot: snapshotID,
		StableRole: "scale_lineitem", Schema: []queryplan.AlgebraFieldV2{
			{ID: "scale_lineitem.dataset_partition", SQLType: "smallint"},
			{ID: "scale_lineitem.l_orderkey", SQLType: "bigint"}, {ID: "scale_lineitem.l_linenumber", SQLType: "integer"},
			{ID: "scale_lineitem.l_extendedprice", SQLType: "numeric"},
		}}
	join := queryplan.AlgebraPlanV2{Op: "join", Left: &orders, Right: &lineitems,
		JoinPredicates: []queryplan.AlgebraJoinPredicateV2{{LeftField: "scale_orders.o_orderkey", RightField: "scale_lineitem.l_orderkey"}}}
	filterValue := json.RawMessage("45000")
	selected := queryplan.AlgebraPlanV2{Op: "select", Input: &join, Predicates: []queryplan.NormalizedFilter{{
		Column: "scale_lineitem.l_orderkey", SQLType: "bigint", Op: "<=", Value: filterValue}}}
	grouped := queryplan.AlgebraPlanV2{Op: "group", Input: &selected, GroupBy: []string{"scale_orders.o_orderstatus"},
		Aggregates: []queryplan.AlgebraAggregateV2{
			{Function: "sum", Field: "scale_lineitem.l_extendedprice", OutputType: "numeric"},
			{Function: "sum", Field: "scale_lineitem.l_linenumber", OutputType: "bigint"},
			{Function: "count", Field: "*", OutputType: "bigint"},
		}}
	projected := queryplan.AlgebraPlanV2{Op: "project", Input: &grouped, Fields: []string{"scale_orders.o_orderstatus",
		"sum(scale_lineitem.l_extendedprice)", "sum(scale_lineitem.l_linenumber)", "count(*)"}}
	normal, err := queryplan.NormalizeAlgebraV2(projected)
	return normal.SHA256, err
}

func compareDynamicFacts(expected []exposure.FactID, actual []dynamicFact, kind string) (uint64, error) {
	want, err := dynamicFacts(expected, kind)
	if err != nil {
		return 0, err
	}
	if len(want) != len(actual) {
		return 0, fmt.Errorf("dynamic cardinality=%d, want %d", len(actual), len(want))
	}
	for index := range want {
		if want[index].SHA256 != actual[index].SHA256 || want[index].Kind != actual[index].Kind ||
			!bytes.Equal(want[index].Payload, actual[index].Payload) {
			return uint64(index), fmt.Errorf("dynamic fact %d differs", index)
		}
	}
	return uint64(len(want)), nil
}

func dynamicFacts(facts []exposure.FactID, kind string) ([]dynamicFact, error) {
	result := make([]dynamicFact, 0, len(facts))
	for _, fact := range facts {
		hashText, err := fact.Hash()
		if err != nil {
			return nil, err
		}
		payload, err := fact.CanonicalPayload()
		if err != nil {
			return nil, err
		}
		result = append(result, dynamicFact{SHA256: hashText, Kind: kind, Payload: payload})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SHA256 < result[j].SHA256 })
	return result, nil
}

func hybridSetDigest(dictionarySet string, static ordinal.BitmapSet, dynamic []dynamicFact) (string, error) {
	staticDigest, err := static.Digest()
	if err != nil {
		return "", err
	}
	hashValue := sha256.New()
	_, _ = hashValue.Write([]byte(hybridSetDomain))
	writeStringHash(hashValue, dictionarySet)
	writeStringHash(hashValue, staticDigest)
	writeUint64Hash(hashValue, uint64(len(dynamic)))
	for _, fact := range dynamic {
		writeStringHash(hashValue, fact.SHA256)
	}
	return hex.EncodeToString(hashValue.Sum(nil)), nil
}

func observationDigest(dictionarySet, release, influence, outcome string, releaseCount, influenceCount, outcomeCount int64) string {
	hashValue := sha256.New()
	_, _ = hashValue.Write([]byte(observationDomain))
	for _, value := range []string{exposure.ProfileV4, dictionarySet, release, influence, outcome} {
		writeStringHash(hashValue, value)
	}
	for _, value := range []int64{releaseCount, influenceCount, outcomeCount} {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(value))
		_, _ = hashValue.Write(encoded[:])
	}
	return hex.EncodeToString(hashValue.Sum(nil))
}

func witnessSorters(groups []*expectedGroup) []*externalSorter {
	var result []*externalSorter
	for _, group := range groups {
		for _, name := range []string{"status", "revenue", "line_positions", "items"} {
			result = append(result, group.witness[name])
		}
	}
	return result
}

func buildGates(report Report, failure error) []Gate {
	status := "pass"
	reason := ""
	if failure != nil {
		status, reason = "fail", failure.Error()
	}
	return []Gate{
		{ID: "independent_boundary", Requirement: "oracle performs zero calls to V4 bitmap derivation", Status: status,
			Evidence: map[string]any{"hot_path_calls": report.Boundary.HotPathCalls, "oracle_id": report.OracleID}, Reason: reason},
		{ID: "million_fact_identity", Requirement: "all 1,035,000 Influence FactHashes and canonical payloads match", Status: gateStatus(failure == nil && report.Facts.MatchedInfluence == maximumPointInfluence),
			Evidence: map[string]any{"matched": report.Facts.MatchedInfluence, "expected": maximumPointInfluence}, Reason: reason},
		{ID: "derived_witness_identity", Requirement: "all 12 derived releases have independently reconstructed witness multiplicities and commitments", Status: gateStatus(failure == nil && report.Witnesses.MatchedCommitments == 12),
			Evidence: report.Witnesses, Reason: reason},
		{ID: "outcome_and_observation_identity", Requirement: "Outcome FactID and V4 observation/effect identity match Control PG and results", Status: gateStatus(failure == nil && report.Facts.MatchedOutcome == 1 && report.Observation.RecomputedSHA256 == report.Observation.SHA256),
			Evidence: report.Observation, Reason: reason},
		{ID: "bounded_external_merge", Requirement: "oracle uses configured bounded-memory external merge sorting", Status: status,
			Evidence: report.Resources, Reason: reason},
	}
}

func gateStatus(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
