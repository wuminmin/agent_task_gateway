package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const (
	v5FactDomain              = "TASKGATE-FACT-V5\x00"
	v5PredicateSetDomain      = "TASKGATE-PREDICATE-SET-V1\x00"
	v5ObservationDigestDomain = "TASKGATE-V5-OBSERVATION-V1\x00"
)

type normalizedV5Observation struct {
	dictionarySet string
	release       normalizedOrdinalSet
	influence     normalizedOrdinalSet
	outcome       OutcomeHashSetObjectsV5
	facts         []OrdinalDynamicFact
	digest        string
	contextDigest string
	predicateSet  string
	atomCount     int64
	compositeHash string
}

type decodedV5Fact struct {
	kind          string
	context       string
	setDigest     string
	atomCount     int64
	compositeHash string
}

func ensureV5ExposureHeadTx(ctx context.Context, tx *sql.Tx, taskID string, grant ExposureGrant, now time.Time) error {
	if grant.ProfileVersion != exposure.ProfileV5 {
		return fmt.Errorf("V5 root head requires %s", exposure.ProfileV5)
	}
	if err := activateV4CutoverTx(ctx, tx, taskID, now); err != nil {
		return err
	}
	var rootTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM tasks WHERE id=$1 FOR SHARE`, taskID).Scan(&rootTaskID); err != nil {
		return err
	}
	var profile string
	var limits ExposureLimits
	var predicate PredicateFootprintLimitsV1
	err := tx.QueryRowContext(ctx, `SELECT profile_version,max_release_facts,max_influence_facts,max_outcome_facts
 ,predicate_profile_version,max_raw_literals_per_query,max_unique_atoms_per_query,
 max_atom_payload_bytes,max_total_atom_payload_bytes
FROM v5_exposure_root_heads WHERE root_task_id=$1 FOR UPDATE`, rootTaskID).
		Scan(&profile, &limits.ReleaseFacts, &limits.InfluenceFacts, &limits.OutcomeFacts, &predicate.Version,
			&predicate.MaxRawLiteralsPerQuery, &predicate.MaxUniqueAtomsPerQuery, &predicate.MaxAtomPayloadBytes,
			&predicate.MaxTotalAtomPayloadBytes)
	if err == nil {
		if profile != exposure.ProfileV5 || grant.Limits.ReleaseFacts > limits.ReleaseFacts ||
			grant.Limits.InfluenceFacts > limits.InfluenceFacts || grant.Limits.OutcomeFacts > limits.OutcomeFacts ||
			grant.PredicateFootprint == nil || grant.PredicateFootprint.Version != predicate.Version ||
			grant.PredicateFootprint.MaxRawLiteralsPerQuery > predicate.MaxRawLiteralsPerQuery ||
			grant.PredicateFootprint.MaxUniqueAtomsPerQuery > predicate.MaxUniqueAtomsPerQuery ||
			grant.PredicateFootprint.MaxAtomPayloadBytes > predicate.MaxAtomPayloadBytes ||
			grant.PredicateFootprint.MaxTotalAtomPayloadBytes > predicate.MaxTotalAtomPayloadBytes {
			return errors.New("delegated V5 exposure grant expands or changes its root head")
		}
		return nil
	}
	if !isNoRows(err) {
		return err
	}
	if rootTaskID != taskID {
		return errors.New("delegated task has no V5 root head")
	}
	var v4Exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM v4_exposure_root_heads WHERE root_task_id=$1)`, rootTaskID).Scan(&v4Exists); err != nil {
		return err
	}
	if v4Exists {
		return errors.New("V4 root cannot be reinterpreted as V5")
	}
	predicate = *grant.PredicateFootprint
	_, err = tx.ExecContext(ctx, `INSERT INTO v5_exposure_root_heads(
 root_task_id,profile_version,max_release_facts,max_influence_facts,max_outcome_facts,
 predicate_profile_version,max_raw_literals_per_query,max_unique_atoms_per_query,
 max_atom_payload_bytes,max_total_atom_payload_bytes,updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, rootTaskID, exposure.ProfileV5, grant.Limits.ReleaseFacts,
		grant.Limits.InfluenceFacts, grant.Limits.OutcomeFacts, predicate.Version, predicate.MaxRawLiteralsPerQuery,
		predicate.MaxUniqueAtomsPerQuery, predicate.MaxAtomPayloadBytes, predicate.MaxTotalAtomPayloadBytes, dbTime(now))
	return err
}

func getV5ExposureLedger(ctx context.Context, source rowQueryer, taskID string) (ExposureLedgerSnapshot, error) {
	var result ExposureLedgerSnapshot
	var updated time.Time
	err := source.QueryRowContext(ctx, `SELECT h.root_task_id,h.profile_version,
 h.max_release_facts,h.max_influence_facts,h.max_outcome_facts,
 h.used_release_facts,h.used_influence_facts,h.used_outcome_facts,h.updated_at
FROM tasks t JOIN v5_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).
		Scan(&result.RootTaskID, &result.ProfileVersion, &result.Limits.ReleaseFacts,
			&result.Limits.InfluenceFacts, &result.Limits.OutcomeFacts, &result.Used.ReleaseFacts,
			&result.Used.InfluenceFacts, &result.Used.OutcomeFacts, &updated)
	if err != nil {
		return ExposureLedgerSnapshot{}, err
	}
	result.UpdatedAt = dbTime(updated)
	return result, nil
}

func reserveV5ExposureTx(ctx context.Context, tx *sql.Tx, queryID, taskID string,
	request *ExposureReservationRequest, now time.Time) (*ExposureReservation, error) {
	var rootTaskID, profile string
	if err := tx.QueryRowContext(ctx, `SELECT t.root_task_id,h.profile_version FROM tasks t
JOIN v5_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).
		Scan(&rootTaskID, &profile); err != nil {
		return nil, err
	}
	if request == nil || request.ProfileVersion != exposure.ProfileV5 || profile != exposure.ProfileV5 ||
		request.EstimatedReleaseFacts < 0 || request.EstimatedInfluenceFacts < 0 || request.EstimatedOutcomeFacts <= 0 {
		return nil, errors.New("invalid V5 exposure reservation")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO v5_query_exposure_reservations(query_id,task_id,root_task_id,
 profile_version,status,estimated_release_facts,estimated_influence_facts,estimated_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,'RESERVED',$5,$6,$7,$8)`, queryID, taskID, rootTaskID, profile,
		request.EstimatedReleaseFacts, request.EstimatedInfluenceFacts, request.EstimatedOutcomeFacts, dbTime(now))
	if err != nil {
		return nil, err
	}
	return &ExposureReservation{QueryID: queryID, TaskID: taskID, RootTaskID: rootTaskID, ProfileVersion: profile,
		EstimatedReleaseFacts: request.EstimatedReleaseFacts, EstimatedInfluenceFacts: request.EstimatedInfluenceFacts,
		EstimatedOutcomeFacts: request.EstimatedOutcomeFacts}, nil
}

func normalizeV5ObservationTx(ctx context.Context, tx *sql.Tx, queryID string,
	observation *OrdinalExposureObservation, now time.Time) (normalizedV5Observation, error) {
	if observation == nil || observation.ProfileVersion != exposure.ProfileV5 || !validSHA256Hex(observation.DictionarySetDigest) {
		return normalizedV5Observation{}, errors.New("invalid V5 observation identity")
	}
	var setCatalog, queryCatalog string
	err := tx.QueryRowContext(ctx, `SELECT dictionary_set.catalog_digest,query.catalog_digest
FROM v4_dictionary_sets dictionary_set CROSS JOIN query_records query
WHERE dictionary_set.dictionary_set_digest=$1 AND query.id=$2`, observation.DictionarySetDigest, queryID).
		Scan(&setCatalog, &queryCatalog)
	if err != nil {
		return normalizedV5Observation{}, err
	}
	if setCatalog != queryCatalog {
		return normalizedV5Observation{}, errors.New("V5 dictionary set Catalog does not match the query")
	}
	release, err := normalizeOrdinalSetTx(ctx, tx, queryID, observation.DictionarySetDigest,
		observation.Release, OrdinalDynamicDerivedRelease, map[string]bool{"BASE_CELL": true}, now)
	if err != nil {
		return normalizedV5Observation{}, fmt.Errorf("release set: %w", err)
	}
	influence, err := normalizeOrdinalSetTx(ctx, tx, queryID, observation.DictionarySetDigest,
		observation.Influence, "", map[string]bool{"BASE_ROW": true, "BASE_CELL": true}, now)
	if err != nil {
		return normalizedV5Observation{}, fmt.Errorf("influence set: %w", err)
	}
	if !observation.Outcome.Static.IsEmpty() || len(observation.Outcome.DynamicFacts) == 0 {
		return normalizedV5Observation{}, errors.New("V5 outcome must be a non-empty dynamic set")
	}
	facts, metadata, err := normalizeV5OutcomeFacts(observation.Outcome.DynamicFacts)
	if err != nil {
		return normalizedV5Observation{}, err
	}
	hashes := make([]string, len(facts))
	for index := range facts {
		hashes[index] = facts[index].SHA256
	}
	outcome, err := BuildOutcomeHashSetV5(hashes)
	if err != nil {
		return normalizedV5Observation{}, err
	}
	normalized := normalizedV5Observation{dictionarySet: observation.DictionarySetDigest, release: release,
		influence: influence, outcome: outcome, facts: facts, contextDigest: metadata.context,
		predicateSet: metadata.setDigest, atomCount: metadata.atomCount, compositeHash: metadata.compositeHash}
	normalized.digest = v5ObservationDigest(normalized)
	if observation.ObservationSHA256 != "" && observation.ObservationSHA256 != normalized.digest {
		return normalizedV5Observation{}, errors.New("V5 observation digest mismatch")
	}
	return normalized, nil
}

func normalizeV5OutcomeFacts(input []OrdinalDynamicFact) ([]OrdinalDynamicFact, decodedV5Fact, error) {
	byHash := make(map[string]OrdinalDynamicFact, len(input))
	atoms := make([]string, 0, len(input))
	var composite decodedV5Fact
	compositeCount := 0
	for _, fact := range input {
		if !validSHA256Hex(fact.SHA256) || len(fact.CanonicalPayload) == 0 ||
			(fact.Kind != OrdinalDynamicPredicateAtom && fact.Kind != OrdinalDynamicCompositeOutcome) {
			return nil, decodedV5Fact{}, errors.New("invalid V5 dynamic outcome fact")
		}
		material := append([]byte(v5FactDomain), fact.CanonicalPayload...)
		digest := sha256.Sum256(material)
		if hex.EncodeToString(digest[:]) != fact.SHA256 {
			return nil, decodedV5Fact{}, errors.New("V5 fact hash does not commit its payload")
		}
		decoded, err := decodeV5FactPayload(fact.CanonicalPayload)
		if err != nil {
			return nil, decodedV5Fact{}, err
		}
		if fact.Kind == OrdinalDynamicPredicateAtom {
			if decoded.kind != string(exposure.FactPredicateAtom) {
				return nil, decodedV5Fact{}, errors.New("V5 atom kind disagrees with payload")
			}
			atoms = append(atoms, fact.SHA256)
			if composite.context == "" {
				composite.context = decoded.context
			} else if compositeCount == 0 && composite.context != decoded.context {
				return nil, decodedV5Fact{}, errors.New("V5 atoms use different predicate contexts")
			}
		} else {
			if decoded.kind != string(exposure.FactCompositeOutcome) {
				return nil, decodedV5Fact{}, errors.New("V5 composite kind disagrees with payload")
			}
			compositeCount++
			composite = decoded
			composite.compositeHash = fact.SHA256
		}
		fact.CanonicalPayload = append([]byte(nil), fact.CanonicalPayload...)
		if previous, duplicate := byHash[fact.SHA256]; duplicate {
			if previous.Kind != fact.Kind || !bytes.Equal(previous.CanonicalPayload, fact.CanonicalPayload) {
				return nil, decodedV5Fact{}, errors.New("V5 outcome fact SHA-256 collision")
			}
			continue
		}
		byHash[fact.SHA256] = fact
	}
	if compositeCount != 1 {
		return nil, decodedV5Fact{}, errors.New("V5 outcome requires exactly one composite")
	}
	atomSet := v5PredicateHashSet(atoms)
	if composite.atomCount != int64(len(uniqueStringsV5(atoms))) || composite.setDigest != atomSet {
		return nil, decodedV5Fact{}, errors.New("V5 composite predicate set binding mismatch")
	}
	for _, fact := range input {
		if fact.Kind != OrdinalDynamicPredicateAtom {
			continue
		}
		decoded, _ := decodeV5FactPayload(fact.CanonicalPayload)
		if decoded.context != composite.context {
			return nil, decodedV5Fact{}, errors.New("V5 atom/composite context mismatch")
		}
	}
	result := make([]OrdinalDynamicFact, 0, len(byHash))
	for _, fact := range byHash {
		result = append(result, fact)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SHA256 < result[j].SHA256 })
	return result, composite, nil
}

func decodeV5FactPayload(payload []byte) (decodedV5Fact, error) {
	reader := bytes.NewReader(payload)
	kind, err := readV5String(reader)
	if err != nil {
		return decodedV5Fact{}, err
	}
	profile, err := readV5String(reader)
	if err != nil || profile != exposure.ProfileV5 {
		return decodedV5Fact{}, errors.New("invalid V5 fact profile")
	}
	result := decodedV5Fact{kind: kind}
	switch kind {
	case string(exposure.FactPredicateAtom):
		atomizer, readErr := readV5String(reader)
		if readErr != nil || atomizer != exposure.PredicateFootprintVersion {
			return decodedV5Fact{}, errors.New("invalid V5 atomizer version")
		}
		result.context, err = readV5String(reader)
		values := make([]string, 9)
		for index := range values {
			if err == nil {
				values[index], err = readV5String(reader)
			}
		}
		if err == nil {
			fact, validateErr := exposure.NewPredicateAtomFactV5(exposure.PredicateAtomFactV5{
				PredicateContextSHA256: result.context, SemanticProductID: values[0], StableRole: values[1],
				PublicFieldID: values[2], ResolvedExpressionSHA256: values[3], SQLType: values[4],
				CollationName: values[5], CollationVersion: values[6], Operator: values[7], CanonicalLiteral: values[8],
			})
			if validateErr != nil {
				return decodedV5Fact{}, validateErr
			}
			canonical, _ := fact.CanonicalPayload()
			if !bytes.Equal(canonical, payload) {
				return decodedV5Fact{}, errors.New("non-canonical V5 atom payload")
			}
		}
	case string(exposure.FactCompositeOutcome):
		values := make([]string, 3)
		for index := range values {
			if err == nil {
				values[index], err = readV5String(reader)
			}
		}
		var visibleRows uint64
		if err == nil {
			visibleRows, err = readV5Uint64(reader)
		}
		var predicateProfile string
		if err == nil {
			predicateProfile, err = readV5String(reader)
		}
		if err != nil || predicateProfile != exposure.PredicateFootprintVersion {
			return decodedV5Fact{}, errors.New("invalid V5 composite predicate profile")
		}
		result.context, err = readV5String(reader)
		if err == nil {
			result.setDigest, err = readV5String(reader)
		}
		var count uint64
		if err == nil {
			count, err = readV5Uint64(reader)
			if count > math.MaxInt64 || visibleRows > math.MaxInt64 {
				return decodedV5Fact{}, errors.New("V5 composite cardinality exceeds int64")
			}
			result.atomCount = int64(count)
		}
		if err == nil {
			fact, validateErr := exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
				QueryNormalFormVersion: values[0], QueryNormalFormSHA256: values[1], ResultObservationSHA256: values[2],
				VisibleRows: int64(visibleRows), PredicateContextSHA256: result.context,
				PredicateSetSHA256: result.setDigest, PredicateAtomCount: result.atomCount,
			})
			if validateErr != nil || values[0] != "taskgate-query-normal-form-v4" {
				return decodedV5Fact{}, errors.New("invalid V5 composite payload")
			}
			canonical, _ := fact.CanonicalPayload()
			if !bytes.Equal(canonical, payload) {
				return decodedV5Fact{}, errors.New("non-canonical V5 composite payload")
			}
		}
	default:
		return decodedV5Fact{}, errors.New("unknown V5 outcome fact kind")
	}
	if err != nil || reader.Len() != 0 || !validSHA256Hex(result.context) {
		return decodedV5Fact{}, errors.New("invalid V5 fact canonical payload")
	}
	return result, nil
}

func readV5String(reader *bytes.Reader) (string, error) {
	length, err := readV5Uint64(reader)
	if err != nil || length > uint64(reader.Len()) {
		return "", io.ErrUnexpectedEOF
	}
	value := make([]byte, int(length))
	_, err = io.ReadFull(reader, value)
	return string(value), err
}

func readV5Uint64(reader *bytes.Reader) (uint64, error) {
	var encoded [8]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint64(encoded[:]), nil
}

func v5PredicateHashSet(input []string) string {
	values := uniqueStringsV5(input)
	hash := sha256.New()
	hash.Write([]byte(v5PredicateSetDomain))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(values)))
	hash.Write(count[:])
	for _, value := range values {
		decoded, _ := hex.DecodeString(value)
		hash.Write(decoded)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func uniqueStringsV5(input []string) []string {
	result := append([]string(nil), input...)
	sort.Strings(result)
	write := 0
	for _, value := range result {
		if write != 0 && result[write-1] == value {
			continue
		}
		result[write] = value
		write++
	}
	return result[:write]
}

func v5ObservationDigest(value normalizedV5Observation) string {
	hash := sha256.New()
	hash.Write([]byte(v5ObservationDigestDomain))
	for _, part := range []string{exposure.ProfileV5, value.dictionarySet, value.release.digest,
		value.influence.digest, value.outcome.Set.SetSHA256, value.contextDigest, value.predicateSet,
		value.compositeHash} {
		writeOrdinalDigestPart(hash, part)
	}
	for _, count := range []int64{value.release.cardinality(), value.influence.cardinality(),
		value.outcome.Set.Cardinality, value.atomCount} {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(count))
		hash.Write(encoded[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func emptyV5OutcomeSet() (OutcomeHashSetObjectsV5, error) { return BuildOutcomeHashSetV5(nil) }

func emptyOrdinalSetV5() normalizedOrdinalSet {
	empty, _ := ordinal.NewBitmapSet()
	return normalizedOrdinalSet{static: empty}
}

func persistV5OutcomeTx(ctx context.Context, tx *sql.Tx, queryID, catalogDigest string,
	value normalizedV5Observation, now time.Time) error {
	if len(value.facts) == 0 {
		return errors.New("V5 outcome fact set is empty")
	}
	hashes := make([]string, len(value.facts))
	kinds := make([]string, len(value.facts))
	payloads := make([][]byte, len(value.facts))
	contexts := make([]string, len(value.facts))
	for index, fact := range value.facts {
		hashes[index], kinds[index], payloads[index] = fact.SHA256, fact.Kind, fact.CanonicalPayload
		decoded, err := decodeV5FactPayload(fact.CanonicalPayload)
		if err != nil {
			return err
		}
		contexts[index] = decoded.context
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO v5_outcome_facts(
 fact_sha256,fact_kind,canonical_payload,predicate_context_sha256,first_catalog_digest,first_query_id,first_seen_at)
SELECT input.fact_sha256,input.fact_kind,input.canonical_payload,input.predicate_context_sha256,$5,$6,$7
FROM unnest($1::text[],$2::text[],$3::bytea[],$4::text[])
 AS input(fact_sha256,fact_kind,canonical_payload,predicate_context_sha256)
ON CONFLICT (fact_sha256) DO NOTHING`, hashes, kinds, payloads, contexts, catalogDigest, queryID, dbTime(now))
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT fact_sha256,fact_kind,canonical_payload,predicate_context_sha256
FROM v5_outcome_facts WHERE fact_sha256=ANY($1::text[])`, hashes)
	if err != nil {
		return err
	}
	stored := make(map[string]OrdinalDynamicFact, len(hashes))
	storedContexts := make(map[string]string, len(hashes))
	for rows.Next() {
		var fact OrdinalDynamicFact
		var contextDigest string
		if err := rows.Scan(&fact.SHA256, &fact.Kind, &fact.CanonicalPayload, &contextDigest); err != nil {
			rows.Close()
			return err
		}
		stored[fact.SHA256], storedContexts[fact.SHA256] = fact, contextDigest
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for index, fact := range value.facts {
		got, present := stored[fact.SHA256]
		if !present || got.Kind != fact.Kind || !bytes.Equal(got.CanonicalPayload, fact.CanonicalPayload) ||
			storedContexts[fact.SHA256] != contexts[index] {
			return fmt.Errorf("V5 outcome fact hash collision for %s", fact.SHA256)
		}
	}
	if err := persistV5SetObjectsTx(ctx, tx, value.outcome, now); err != nil {
		return err
	}
	for _, set := range []normalizedOrdinalSet{value.release, value.influence} {
		if err := persistOrdinalSetTx(ctx, tx, value.dictionarySet, set, now); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO v5_observations(
 observation_sha256,profile_version,dictionary_set_digest,release_set_sha256,influence_set_sha256,outcome_set_sha256,
 predicate_context_sha256,predicate_set_sha256,predicate_atom_count,composite_outcome_sha256,
 actual_release_facts,actual_influence_facts,actual_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
ON CONFLICT (observation_sha256) DO NOTHING`, value.digest, exposure.ProfileV5, value.dictionarySet,
		value.release.digest, value.influence.digest, value.outcome.Set.SetSHA256, value.contextDigest,
		value.predicateSet, value.atomCount, value.compositeHash, value.release.cardinality(),
		value.influence.cardinality(), value.outcome.Set.Cardinality, dbTime(now))
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 0 {
		var profile, dictionarySet, releaseSet, influenceSet, outcomeSet, contextDigest, predicateSet, composite string
		var atomCount, releaseCount, influenceCount, outcomeCount int64
		if err := tx.QueryRowContext(ctx, `SELECT profile_version,dictionary_set_digest,release_set_sha256,
 influence_set_sha256,outcome_set_sha256,predicate_context_sha256,predicate_set_sha256,predicate_atom_count,
 composite_outcome_sha256,actual_release_facts,actual_influence_facts,actual_outcome_facts
FROM v5_observations WHERE observation_sha256=$1`, value.digest).Scan(&profile, &dictionarySet, &releaseSet,
			&influenceSet, &outcomeSet, &contextDigest, &predicateSet, &atomCount, &composite,
			&releaseCount, &influenceCount, &outcomeCount); err != nil {
			return err
		}
		if profile != exposure.ProfileV5 || dictionarySet != value.dictionarySet || releaseSet != value.release.digest ||
			influenceSet != value.influence.digest || outcomeSet != value.outcome.Set.SetSHA256 ||
			contextDigest != value.contextDigest || predicateSet != value.predicateSet || atomCount != value.atomCount ||
			composite != value.compositeHash || releaseCount != value.release.cardinality() ||
			influenceCount != value.influence.cardinality() || outcomeCount != value.outcome.Set.Cardinality {
			return errors.New("V5 observation SHA-256 collision")
		}
	}
	return nil
}

func persistV5SetObjectsTx(ctx context.Context, tx *sql.Tx, objects OutcomeHashSetObjectsV5, now time.Time) error {
	if err := verifyOutcomeRootV5(objects.Set); err != nil {
		return err
	}
	for _, leaf := range objects.Leaves {
		decoded, prefix, chunk, err := decodeOutcomeLeafV5(leaf.Payload)
		if err != nil || sha256HexV5(leaf.Payload) != leaf.LeafSHA256 || prefix != leaf.Prefix16 ||
			chunk != leaf.ChunkIndex || len(decoded) != leaf.Cardinality {
			return errors.New("invalid V5 leaf submitted for persistence")
		}
	}
	for _, block := range objects.Blocks {
		if sha256HexV5(block.Manifest) != block.BlockSHA256 {
			return errors.New("invalid V5 block submitted for persistence")
		}
		if _, err := parseV5BlockManifestReferences(block.Manifest, block.Prefix8, block.Cardinality); err != nil {
			return err
		}
	}
	if len(objects.Leaves) != 0 {
		digests := make([]string, 0, len(objects.Leaves))
		prefixes := make([]int32, 0, len(objects.Leaves))
		chunks := make([]int32, 0, len(objects.Leaves))
		counts := make([]int32, 0, len(objects.Leaves))
		payloads := make([][]byte, 0, len(objects.Leaves))
		sizes := make([]int32, 0, len(objects.Leaves))
		ordered := make([]OutcomeHashLeafV5, 0, len(objects.Leaves))
		for _, leaf := range objects.Leaves {
			ordered = append(ordered, leaf)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].LeafSHA256 < ordered[j].LeafSHA256 })
		for _, leaf := range ordered {
			digests = append(digests, leaf.LeafSHA256)
			prefixes = append(prefixes, int32(leaf.Prefix16))
			chunks = append(chunks, int32(leaf.ChunkIndex))
			counts = append(counts, int32(leaf.Cardinality))
			payloads = append(payloads, leaf.Payload)
			sizes = append(sizes, int32(len(leaf.Payload)))
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO v5_outcome_hash_leaves(
 leaf_sha256,prefix16,chunk_index,cardinality,codec,payload,uncompressed_size,created_at)
SELECT input.leaf_sha256,input.prefix16,input.chunk_index,input.cardinality,'RAW',input.payload,input.size,$7
FROM unnest($1::text[],$2::integer[],$3::integer[],$4::integer[],$5::bytea[],$6::integer[])
 AS input(leaf_sha256,prefix16,chunk_index,cardinality,payload,size)
ON CONFLICT (leaf_sha256) DO NOTHING`, digests, prefixes, chunks, counts, payloads, sizes, dbTime(now))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT leaf_sha256,prefix16,chunk_index,cardinality,codec,payload,uncompressed_size
FROM v5_outcome_hash_leaves WHERE leaf_sha256=ANY($1::text[])`, digests)
		if err != nil {
			return err
		}
		seen := make(map[string]OutcomeHashLeafV5, len(digests))
		for rows.Next() {
			var leaf OutcomeHashLeafV5
			var prefix, chunk, count, size int
			var codec string
			if err := rows.Scan(&leaf.LeafSHA256, &prefix, &chunk, &count, &codec, &leaf.Payload, &size); err != nil {
				rows.Close()
				return err
			}
			leaf.Prefix16, leaf.ChunkIndex, leaf.Cardinality = uint16(prefix), uint32(chunk), count
			if codec != "RAW" || size != len(leaf.Payload) {
				rows.Close()
				return errors.New("unsupported or corrupt V5 leaf codec")
			}
			seen[leaf.LeafSHA256] = leaf
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for digest, leaf := range objects.Leaves {
			if stored, present := seen[digest]; !present || !sameOutcomeLeafV5(stored, leaf) {
				return fmt.Errorf("V5 leaf digest collision for %s", digest)
			}
		}
	}
	if len(objects.Blocks) != 0 {
		digests := make([]string, 0, len(objects.Blocks))
		prefixes := make([]int32, 0, len(objects.Blocks))
		counts := make([]int64, 0, len(objects.Blocks))
		manifests := make([][]byte, 0, len(objects.Blocks))
		for _, block := range objects.Blocks {
			digests = append(digests, block.BlockSHA256)
			prefixes = append(prefixes, int32(block.Prefix8))
			counts = append(counts, block.Cardinality)
			manifests = append(manifests, block.Manifest)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO v5_outcome_hash_blocks(block_sha256,prefix8,cardinality,manifest,created_at)
SELECT input.digest,input.prefix,input.cardinality,input.manifest,$5
FROM unnest($1::text[],$2::integer[],$3::bigint[],$4::bytea[])
 AS input(digest,prefix,cardinality,manifest)
ON CONFLICT (block_sha256) DO NOTHING`, digests, prefixes, counts, manifests, dbTime(now))
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT block_sha256,prefix8,cardinality,manifest
FROM v5_outcome_hash_blocks WHERE block_sha256=ANY($1::text[])`, digests)
		if err != nil {
			return err
		}
		seen := make(map[string]OutcomeHashBlockV5, len(digests))
		for rows.Next() {
			var block OutcomeHashBlockV5
			var prefix int
			if err := rows.Scan(&block.BlockSHA256, &prefix, &block.Cardinality, &block.Manifest); err != nil {
				rows.Close()
				return err
			}
			block.Prefix8 = byte(prefix)
			seen[block.BlockSHA256] = block
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for digest, block := range objects.Blocks {
			if stored, present := seen[digest]; !present || !sameOutcomeBlockV5(stored, block) {
				return fmt.Errorf("V5 block digest collision for %s", digest)
			}
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO v5_outcome_hash_sets(set_sha256,cardinality,block_count,root_manifest,created_at)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (set_sha256) DO NOTHING`, objects.Set.SetSHA256,
		objects.Set.Cardinality, objects.Set.BlockCount, objects.Set.RootManifest, dbTime(now))
	if err != nil {
		return err
	}
	if inserted, _ := result.RowsAffected(); inserted == 0 {
		var cardinality int64
		var blockCount int
		var manifest []byte
		if err := tx.QueryRowContext(ctx, `SELECT cardinality,block_count,root_manifest FROM v5_outcome_hash_sets WHERE set_sha256=$1`,
			objects.Set.SetSHA256).Scan(&cardinality, &blockCount, &manifest); err != nil {
			return err
		}
		if cardinality != objects.Set.Cardinality || blockCount != objects.Set.BlockCount || !bytes.Equal(manifest, objects.Set.RootManifest) {
			return errors.New("V5 outcome set SHA-256 collision")
		}
	}
	return nil
}

// differenceAndUnionV5Tx performs an exact union while reading only radix
// branches addressed by candidate prefixes. The returned object maps contain
// only new leaves and changed blocks; unchanged references remain committed by
// the returned root manifest and are not re-submitted to PostgreSQL.
func differenceAndUnionV5Tx(ctx context.Context, tx *sql.Tx, rootDigest string,
	candidate OutcomeCandidateV5) (OutcomeHashSetObjectsV5, [][sha256.Size]byte, OutcomeRadixTelemetryV5, error) {
	candidates := uniqueOutcomeHashesV5(candidate.Hashes)
	telemetry := OutcomeRadixTelemetryV5{CandidateCardinality: int64(len(candidates))}
	if rootDigest == "" {
		built, err := buildOutcomeHashSetFromBinaryV5(candidates)
		if err != nil {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, err
		}
		telemetry.LeavesChanged = int64(len(built.Leaves))
		return built, candidates, telemetry, nil
	}

	var root OutcomeSetV5
	if err := tx.QueryRowContext(ctx, `SELECT set_sha256,cardinality,block_count,root_manifest
FROM v5_outcome_hash_sets WHERE set_sha256=$1`, rootDigest).Scan(&root.SetSHA256, &root.Cardinality,
		&root.BlockCount, &root.RootManifest); err != nil {
		return OutcomeHashSetObjectsV5{}, nil, telemetry, err
	}
	if err := verifyOutcomeRootV5(root); err != nil {
		return OutcomeHashSetObjectsV5{}, nil, telemetry, err
	}
	rootRefs, err := parseV5RootManifestReferences(root.RootManifest, root.Cardinality, root.BlockCount)
	if err != nil {
		return OutcomeHashSetObjectsV5{}, nil, telemetry, err
	}
	telemetry.RootCardinality = root.Cardinality

	byPrefix := make(map[uint16][][sha256.Size]byte)
	touchedBlocks := make(map[byte]bool)
	for _, hash := range candidates {
		prefix := binary.BigEndian.Uint16(hash[:2])
		byPrefix[prefix] = append(byPrefix[prefix], hash)
		touchedBlocks[byte(prefix>>8)] = true
	}
	rootByPrefix := make(map[byte]outcomeBlockReferenceV5, len(rootRefs))
	for _, ref := range rootRefs {
		rootByPrefix[ref.prefix] = ref
	}

	requestedBlocks := make([]string, 0, len(touchedBlocks))
	for prefix := range touchedBlocks {
		if ref, present := rootByPrefix[prefix]; present {
			requestedBlocks = append(requestedBlocks, ref.digest)
		}
	}
	loadedBlocks := make(map[byte]OutcomeHashBlockV5, len(requestedBlocks))
	blockLeafRefs := make(map[byte][]outcomeLeafReferenceV5, len(requestedBlocks))
	if len(requestedBlocks) != 0 {
		rows, queryErr := tx.QueryContext(ctx, `SELECT block_sha256,prefix8,cardinality,manifest
FROM v5_outcome_hash_blocks WHERE block_sha256=ANY($1::text[])`, requestedBlocks)
		if queryErr != nil {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, queryErr
		}
		for rows.Next() {
			var block OutcomeHashBlockV5
			var prefix int
			if scanErr := rows.Scan(&block.BlockSHA256, &prefix, &block.Cardinality, &block.Manifest); scanErr != nil {
				rows.Close()
				return OutcomeHashSetObjectsV5{}, nil, telemetry, scanErr
			}
			block.Prefix8 = byte(prefix)
			ref, present := rootByPrefix[block.Prefix8]
			if !present || ref.digest != block.BlockSHA256 || ref.cardinality != block.Cardinality ||
				sha256HexV5(block.Manifest) != block.BlockSHA256 {
				rows.Close()
				return OutcomeHashSetObjectsV5{}, nil, telemetry, errors.New("V5 touched block disagrees with root manifest")
			}
			refs, parseErr := parseV5BlockManifestReferences(block.Manifest, block.Prefix8, block.Cardinality)
			if parseErr != nil {
				rows.Close()
				return OutcomeHashSetObjectsV5{}, nil, telemetry, parseErr
			}
			loadedBlocks[block.Prefix8], blockLeafRefs[block.Prefix8] = block, refs
		}
		if closeErr := rows.Close(); closeErr != nil {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, closeErr
		}
		if len(loadedBlocks) != len(requestedBlocks) {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, errors.New("V5 outcome set is missing a touched block")
		}
	}
	telemetry.BlocksLoaded = int64(len(loadedBlocks))

	requestedLeaves := make(map[string]outcomeLeafReferenceV5)
	for prefix := range byPrefix {
		for _, ref := range blockLeafRefs[byte(prefix>>8)] {
			if ref.prefix16 == prefix {
				requestedLeaves[ref.digest] = ref
			}
		}
	}
	loadedLeaves := make(map[string]OutcomeHashLeafV5, len(requestedLeaves))
	if len(requestedLeaves) != 0 {
		digests := make([]string, 0, len(requestedLeaves))
		for digest := range requestedLeaves {
			digests = append(digests, digest)
		}
		rows, queryErr := tx.QueryContext(ctx, `SELECT leaf_sha256,prefix16,chunk_index,cardinality,codec,payload,uncompressed_size
FROM v5_outcome_hash_leaves WHERE leaf_sha256=ANY($1::text[])`, digests)
		if queryErr != nil {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, queryErr
		}
		for rows.Next() {
			var leaf OutcomeHashLeafV5
			var prefix, chunk, size int
			var codec string
			if scanErr := rows.Scan(&leaf.LeafSHA256, &prefix, &chunk, &leaf.Cardinality, &codec, &leaf.Payload, &size); scanErr != nil {
				rows.Close()
				return OutcomeHashSetObjectsV5{}, nil, telemetry, scanErr
			}
			leaf.Prefix16, leaf.ChunkIndex = uint16(prefix), uint32(chunk)
			ref, present := requestedLeaves[leaf.LeafSHA256]
			decoded, decodedPrefix, decodedChunk, decodeErr := decodeOutcomeLeafV5(leaf.Payload)
			if !present || codec != "RAW" || size != len(leaf.Payload) || sha256HexV5(leaf.Payload) != leaf.LeafSHA256 ||
				ref.prefix16 != leaf.Prefix16 || ref.chunk != leaf.ChunkIndex || ref.cardinality != leaf.Cardinality ||
				decodeErr != nil || decodedPrefix != leaf.Prefix16 || decodedChunk != leaf.ChunkIndex || len(decoded) != leaf.Cardinality {
				rows.Close()
				return OutcomeHashSetObjectsV5{}, nil, telemetry, errors.New("V5 touched leaf disagrees with block manifest")
			}
			telemetry.HashesLoaded += int64(len(decoded))
			loadedLeaves[leaf.LeafSHA256] = leaf
		}
		if closeErr := rows.Close(); closeErr != nil {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, closeErr
		}
		if len(loadedLeaves) != len(requestedLeaves) {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, errors.New("V5 outcome set is missing a touched leaf")
		}
	}
	telemetry.LeavesLoaded = int64(len(loadedLeaves))

	changedLeaves := make(map[string]OutcomeHashLeafV5)
	novel := make([][sha256.Size]byte, 0, len(candidates))
	replacementRefs := make(map[uint16][]outcomeLeafReferenceV5, len(byPrefix))
	for prefix, additions := range byPrefix {
		oldMembers := make([][sha256.Size]byte, 0)
		oldDigests := make(map[string]bool)
		for _, ref := range blockLeafRefs[byte(prefix>>8)] {
			if ref.prefix16 != prefix {
				continue
			}
			oldDigests[ref.digest] = true
			decoded, _, _, _ := decodeOutcomeLeafV5(loadedLeaves[ref.digest].Payload)
			oldMembers = append(oldMembers, decoded...)
		}
		merged, prefixNovel := mergeOutcomeHashesV5(oldMembers, additions)
		novel = append(novel, prefixNovel...)
		refs := make([]outcomeLeafReferenceV5, 0, (len(merged)+outcomeLeafChunkSize-1)/outcomeLeafChunkSize)
		for start, chunk := 0, uint32(0); start < len(merged); start, chunk = start+outcomeLeafChunkSize, chunk+1 {
			end := start + outcomeLeafChunkSize
			if end > len(merged) {
				end = len(merged)
			}
			payload := canonicalOutcomeLeafV5(prefix, chunk, merged[start:end])
			digest := sha256HexV5(payload)
			leaf := OutcomeHashLeafV5{LeafSHA256: digest, Prefix16: prefix, ChunkIndex: chunk,
				Cardinality: end - start, Payload: payload}
			refs = append(refs, outcomeLeafReferenceV5{prefix16: prefix, chunk: chunk, digest: digest, cardinality: end - start})
			if !oldDigests[digest] {
				changedLeaves[digest] = leaf
			}
		}
		replacementRefs[prefix] = refs
	}
	sortOutcomeHashesV5(novel)
	telemetry.LeavesChanged = int64(len(changedLeaves))

	changedBlocks := make(map[string]OutcomeHashBlockV5)
	replacements := make(map[byte]outcomeBlockReferenceV5, len(touchedBlocks))
	for prefix := range touchedBlocks {
		refs := make([]outcomeLeafReferenceV5, 0)
		seenPrefix := make(map[uint16]bool)
		for _, ref := range blockLeafRefs[prefix] {
			if replacement, touched := replacementRefs[ref.prefix16]; touched {
				if !seenPrefix[ref.prefix16] {
					refs = append(refs, replacement...)
					seenPrefix[ref.prefix16] = true
				}
				continue
			}
			refs = append(refs, ref)
		}
		for prefix16, replacement := range replacementRefs {
			if byte(prefix16>>8) == prefix && !seenPrefix[prefix16] {
				refs = append(refs, replacement...)
			}
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].prefix16 != refs[j].prefix16 {
				return refs[i].prefix16 < refs[j].prefix16
			}
			return refs[i].chunk < refs[j].chunk
		})
		manifest, cardinality, buildErr := canonicalOutcomeBlockV5(prefix, refs)
		if buildErr != nil {
			return OutcomeHashSetObjectsV5{}, nil, telemetry, buildErr
		}
		digest := sha256HexV5(manifest)
		ref := outcomeBlockReferenceV5{prefix: prefix, digest: digest, cardinality: cardinality}
		replacements[prefix] = ref
		if old, present := rootByPrefix[prefix]; !present || old.digest != digest {
			changedBlocks[digest] = OutcomeHashBlockV5{BlockSHA256: digest, Prefix8: prefix,
				Cardinality: cardinality, Manifest: manifest}
		}
	}

	newRootRefs := make([]outcomeBlockReferenceV5, 0, len(rootRefs)+len(touchedBlocks))
	seenBlock := make(map[byte]bool)
	for _, ref := range rootRefs {
		if replacement, touched := replacements[ref.prefix]; touched {
			newRootRefs = append(newRootRefs, replacement)
			seenBlock[ref.prefix] = true
			if replacement.digest == ref.digest {
				telemetry.BlocksReused++
			}
		} else {
			newRootRefs = append(newRootRefs, ref)
			telemetry.BlocksReused++
		}
	}
	for prefix, replacement := range replacements {
		if !seenBlock[prefix] {
			newRootRefs = append(newRootRefs, replacement)
		}
	}
	sort.Slice(newRootRefs, func(i, j int) bool { return newRootRefs[i].prefix < newRootRefs[j].prefix })
	newCardinality := root.Cardinality + int64(len(novel))
	rootManifest, err := canonicalOutcomeRootV5(newCardinality, newRootRefs)
	if err != nil {
		return OutcomeHashSetObjectsV5{}, nil, telemetry, err
	}
	result := OutcomeHashSetObjectsV5{Set: OutcomeSetV5{SetSHA256: sha256HexV5(rootManifest),
		Cardinality: newCardinality, BlockCount: len(newRootRefs), RootManifest: rootManifest},
		Leaves: changedLeaves, Blocks: changedBlocks}
	return result, novel, telemetry, nil
}

func mergeOutcomeHashesV5(existing, additions [][sha256.Size]byte) ([][sha256.Size]byte, [][sha256.Size]byte) {
	existing = uniqueOutcomeHashesV5(existing)
	additions = uniqueOutcomeHashesV5(additions)
	merged := make([][sha256.Size]byte, 0, len(existing)+len(additions))
	novel := make([][sha256.Size]byte, 0, len(additions))
	left, right := 0, 0
	for left < len(existing) || right < len(additions) {
		switch {
		case left == len(existing):
			merged, novel = append(merged, additions[right]), append(novel, additions[right])
			right++
		case right == len(additions):
			merged = append(merged, existing[left])
			left++
		default:
			comparison := bytes.Compare(existing[left][:], additions[right][:])
			if comparison < 0 {
				merged, left = append(merged, existing[left]), left+1
			} else if comparison > 0 {
				merged, novel, right = append(merged, additions[right]), append(novel, additions[right]), right+1
			} else {
				merged, left, right = append(merged, existing[left]), left+1, right+1
			}
		}
	}
	return merged, novel
}

func parseV5RootManifestReferences(payload []byte, cardinality int64, blockCount int) ([]outcomeBlockReferenceV5, error) {
	reader := bytes.NewReader(payload)
	domain := make([]byte, len(outcomeSetDomainV5))
	if _, err := io.ReadFull(reader, domain); err != nil || string(domain) != outcomeSetDomainV5 {
		return nil, errors.New("invalid V5 root manifest domain")
	}
	profile, err := readV5U32String(reader)
	if err != nil || profile != exposure.ProfileV5 {
		return nil, errors.New("invalid V5 root manifest profile")
	}
	storedCardinality, err := readV5Uint64(reader)
	if err != nil || storedCardinality > math.MaxInt64 || int64(storedCardinality) != cardinality {
		return nil, errors.New("invalid V5 root manifest cardinality")
	}
	count, err := readV5Uint32(reader)
	if err != nil || int(count) != blockCount || count > 256 {
		return nil, errors.New("invalid V5 root manifest block count")
	}
	result := make([]outcomeBlockReferenceV5, 0, count)
	lastPrefix := -1
	var total int64
	for index := uint32(0); index < count; index++ {
		prefix, err := reader.ReadByte()
		if err != nil || int(prefix) <= lastPrefix {
			return nil, errors.New("invalid V5 root manifest block order")
		}
		lastPrefix = int(prefix)
		digest := make([]byte, sha256.Size)
		if _, err := io.ReadFull(reader, digest); err != nil {
			return nil, err
		}
		storedCount, err := readV5Uint64(reader)
		if err != nil || storedCount == 0 || storedCount > math.MaxInt64 {
			return nil, errors.New("invalid V5 root block cardinality")
		}
		ref := outcomeBlockReferenceV5{prefix: prefix, digest: hex.EncodeToString(digest), cardinality: int64(storedCount)}
		result = append(result, ref)
		if ref.cardinality > math.MaxInt64-total {
			return nil, errors.New("V5 root cardinality overflow")
		}
		total += ref.cardinality
	}
	if reader.Len() != 0 {
		return nil, errors.New("V5 root manifest has trailing bytes")
	}
	if total != cardinality {
		return nil, errors.New("V5 root block cardinality disagrees with set")
	}
	return result, nil
}

func parseV5BlockManifestReferences(payload []byte, prefix byte, cardinality int64) ([]outcomeLeafReferenceV5, error) {
	reader := bytes.NewReader(payload)
	domain := make([]byte, len(outcomeBlockDomainV5))
	if _, err := io.ReadFull(reader, domain); err != nil || string(domain) != outcomeBlockDomainV5 {
		return nil, errors.New("invalid V5 block manifest domain")
	}
	storedPrefix, err := reader.ReadByte()
	if err != nil || storedPrefix != prefix {
		return nil, errors.New("invalid V5 block prefix")
	}
	count, err := readV5Uint32(reader)
	const encodedLeafReferenceSize = 1 + 4 + sha256.Size + 4
	if err != nil || count == 0 || uint64(count) > uint64(reader.Len()/encodedLeafReferenceSize) {
		return nil, errors.New("invalid V5 block leaf count")
	}
	result := make([]outcomeLeafReferenceV5, 0, count)
	var total int64
	var lastPrefix int = -1
	var lastChunk uint32
	for index := uint32(0); index < count; index++ {
		low, err := reader.ReadByte()
		if err != nil {
			return nil, err
		}
		chunk, err := readV5Uint32(reader)
		if err != nil {
			return nil, err
		}
		prefix16 := uint16(prefix)<<8 | uint16(low)
		if int(prefix16) < lastPrefix || (int(prefix16) == lastPrefix && chunk != lastChunk+1) ||
			(int(prefix16) > lastPrefix && chunk != 0) {
			return nil, errors.New("invalid V5 block leaf order")
		}
		lastPrefix, lastChunk = int(prefix16), chunk
		digest := make([]byte, sha256.Size)
		if _, err := io.ReadFull(reader, digest); err != nil {
			return nil, err
		}
		leafCount, err := readV5Uint32(reader)
		if err != nil || leafCount == 0 || leafCount > outcomeLeafChunkSize {
			return nil, errors.New("invalid V5 block leaf cardinality")
		}
		total += int64(leafCount)
		result = append(result, outcomeLeafReferenceV5{prefix16: prefix16, chunk: chunk,
			digest: hex.EncodeToString(digest), cardinality: int(leafCount)})
	}
	if reader.Len() != 0 || total != cardinality {
		return nil, errors.New("invalid V5 block manifest cardinality")
	}
	return result, nil
}

func readV5Uint32(reader *bytes.Reader) (uint32, error) {
	var encoded [4]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(encoded[:]), nil
}

func readV5U32String(reader *bytes.Reader) (string, error) {
	length, err := readV5Uint32(reader)
	if err != nil || uint64(length) > uint64(reader.Len()) {
		return "", io.ErrUnexpectedEOF
	}
	value := make([]byte, length)
	_, err = io.ReadFull(reader, value)
	return string(value), err
}

func settleV5ExposureMeasuredTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string,
	observation *OrdinalExposureObservation) (*ExposureCharge, exposureSettlementMetrics, error) {
	var metrics exposureSettlementMetrics
	var reservation ExposureReservation
	var status, storedDigest string
	var actual, charged ExposureLimits
	var storedEpoch int64
	started := time.Now()
	err := tx.QueryRowContext(ctx, `SELECT query_id,task_id,root_task_id,profile_version,
 estimated_release_facts,estimated_influence_facts,estimated_outcome_facts,status,
 actual_release_facts,actual_influence_facts,actual_outcome_facts,
 charged_release_facts,charged_influence_facts,charged_outcome_facts,observation_sha256,root_epoch
FROM v5_query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).
		Scan(&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
			&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &reservation.EstimatedOutcomeFacts,
			&status, &actual.ReleaseFacts, &actual.InfluenceFacts, &actual.OutcomeFacts,
			&charged.ReleaseFacts, &charged.InfluenceFacts, &charged.OutcomeFacts, &storedDigest, &storedEpoch)
	metrics.ReservationLock = time.Since(started)
	if err != nil {
		return nil, metrics, err
	}
	normalized, err := normalizeV5ObservationTx(ctx, tx, queryID, observation, now)
	if err != nil {
		return nil, metrics, err
	}
	if normalized.outcome.Set.Cardinality != reservation.EstimatedOutcomeFacts {
		return nil, metrics, errors.New("V5 outcome cardinality differs from its pre-execution reservation")
	}
	if status == exposureSettled {
		if storedDigest != normalized.digest {
			return nil, metrics, errors.New("query already settled with different V5 exposure evidence")
		}
		charge, chargeErr := getV5ExposureCharge(ctx, tx, queryID)
		return &charge, metrics, chargeErr
	}
	if status != exposureReserved {
		return nil, metrics, fmt.Errorf("V5 exposure reservation is %s", status)
	}
	storeStarted := time.Now()
	var catalogDigest string
	if err := tx.QueryRowContext(ctx, `SELECT catalog_digest FROM query_records WHERE id=$1`, queryID).Scan(&catalogDigest); err != nil {
		return nil, metrics, err
	}
	if err := persistV5OutcomeTx(ctx, tx, queryID, catalogDigest, normalized, now); err != nil {
		return nil, metrics, err
	}
	metrics.FactStore = time.Since(storeStarted)

	var head OrdinalRootHead
	started = time.Now()
	err = tx.QueryRowContext(ctx, `SELECT root_task_id,profile_version,COALESCE(dictionary_set_digest,''),epoch,
 max_release_facts,max_influence_facts,max_outcome_facts,
 used_release_facts,used_influence_facts,used_outcome_facts,
 COALESCE(release_set_sha256,''),COALESCE(influence_set_sha256,''),COALESCE(outcome_set_sha256,''),updated_at
FROM v5_exposure_root_heads WHERE root_task_id=$1`, reservation.RootTaskID).
		Scan(&head.RootTaskID, &head.ProfileVersion, &head.DictionarySetDigest, &head.Epoch,
			&head.Limits.ReleaseFacts, &head.Limits.InfluenceFacts, &head.Limits.OutcomeFacts,
			&head.Used.ReleaseFacts, &head.Used.InfluenceFacts, &head.Used.OutcomeFacts,
			&head.ReleaseSetSHA256, &head.InfluenceSetSHA256, &head.OutcomeSetSHA256, &head.UpdatedAt)
	metrics.LedgerLock = time.Since(started)
	if err != nil {
		return nil, metrics, err
	}
	if head.ProfileVersion != exposure.ProfileV5 ||
		(head.DictionarySetDigest != "" && head.DictionarySetDigest != normalized.dictionarySet) {
		return nil, metrics, errors.New("V5 root head dictionary/profile mismatch")
	}
	rootRelease, rootInfluence := emptyOrdinalSetV5(), emptyOrdinalSetV5()
	if head.ReleaseSetSHA256 != "" {
		var dictionary string
		rootRelease, dictionary, err = loadOrdinalSetTx(ctx, tx, head.ReleaseSetSHA256)
		if err != nil || dictionary != normalized.dictionarySet {
			return nil, metrics, errors.New("V5 release root set dictionary mismatch")
		}
		rootInfluence, dictionary, err = loadOrdinalSetTx(ctx, tx, head.InfluenceSetSHA256)
		if err != nil || dictionary != normalized.dictionarySet {
			return nil, metrics, errors.New("V5 influence root set dictionary mismatch")
		}
	}
	deltaRelease, err := differenceOrdinalSet(normalized.dictionarySet, normalized.release, rootRelease)
	if err != nil {
		return nil, metrics, err
	}
	deltaInfluence, err := differenceOrdinalSet(normalized.dictionarySet, normalized.influence, rootInfluence)
	if err != nil {
		return nil, metrics, err
	}
	candidateHashes := make([][sha256.Size]byte, 0, len(normalized.facts))
	factKind := make(map[string]string, len(normalized.facts))
	for _, fact := range normalized.facts {
		hash, _ := decodeOutcomeHashV5(fact.SHA256)
		candidateHashes = append(candidateHashes, hash)
		factKind[fact.SHA256] = fact.Kind
	}
	mergedOutcome, novelHashes, radixMetrics, err := differenceAndUnionV5Tx(ctx, tx, head.OutcomeSetSHA256,
		OutcomeCandidateV5{Hashes: candidateHashes})
	if err != nil {
		return nil, metrics, err
	}
	metrics.OutcomeRadix = radixMetrics
	newRelease, newInfluence, newOutcome := deltaRelease.cardinality(), deltaInfluence.cardinality(), int64(len(novelHashes))
	var chargedAtoms, chargedComposite int64
	for _, hash := range novelHashes {
		switch factKind[OutcomeHashTextV5(hash)] {
		case OrdinalDynamicPredicateAtom:
			chargedAtoms++
		case OrdinalDynamicCompositeOutcome:
			chargedComposite++
		default:
			return nil, metrics, errors.New("V5 novelty references an unknown outcome fact")
		}
	}
	var taskLimits ExposureLimits
	if err := tx.QueryRowContext(ctx, `SELECT max_release_facts,max_influence_facts,max_outcome_facts
FROM task_grants WHERE task_id=$1`, reservation.TaskID).
		Scan(&taskLimits.ReleaseFacts, &taskLimits.InfluenceFacts, &taskLimits.OutcomeFacts); err != nil {
		return nil, metrics, err
	}
	if exceedsOrdinalLimit(head.Used, head.Limits, taskLimits, ExposureLimits{
		ReleaseFacts: newRelease, InfluenceFacts: newInfluence, OutcomeFacts: newOutcome}) {
		return nil, metrics, ErrExposureBudgetExhausted
	}
	mergedRelease, err := unionOrdinalSet(normalized.dictionarySet, rootRelease, normalized.release)
	if err != nil {
		return nil, metrics, err
	}
	mergedInfluence, err := unionOrdinalSet(normalized.dictionarySet, rootInfluence, normalized.influence)
	if err != nil {
		return nil, metrics, err
	}
	for _, set := range []normalizedOrdinalSet{mergedRelease, mergedInfluence} {
		if err := persistOrdinalSetTx(ctx, tx, normalized.dictionarySet, set, now); err != nil {
			return nil, metrics, err
		}
	}
	if err := persistV5SetObjectsTx(ctx, tx, mergedOutcome, now); err != nil {
		return nil, metrics, err
	}
	rootEpoch := head.Epoch
	var result sql.Result
	if newRelease != 0 || newInfluence != 0 || newOutcome != 0 {
		result, err = tx.ExecContext(ctx, `UPDATE v5_exposure_root_heads SET dictionary_set_digest=$1,epoch=epoch+1,
 used_release_facts=used_release_facts+$2,used_influence_facts=used_influence_facts+$3,
 used_outcome_facts=used_outcome_facts+$4,release_set_sha256=$5,influence_set_sha256=$6,
 outcome_set_sha256=$7,updated_at=$8
WHERE root_task_id=$9 AND epoch=$10
 AND used_release_facts+$2 <= max_release_facts
 AND used_influence_facts+$3 <= max_influence_facts
 AND used_outcome_facts+$4 <= max_outcome_facts`, normalized.dictionarySet, newRelease, newInfluence,
			newOutcome, mergedRelease.digest, mergedInfluence.digest, mergedOutcome.Set.SetSHA256, dbTime(now),
			reservation.RootTaskID, head.Epoch)
		if err != nil {
			return nil, metrics, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return nil, metrics, ErrOrdinalCASConflict
		}
		rootEpoch = head.Epoch + 1
	} else if err := tx.QueryRowContext(ctx, `SELECT epoch FROM v5_exposure_root_heads WHERE root_task_id=$1`,
		reservation.RootTaskID).Scan(&rootEpoch); err != nil {
		return nil, metrics, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO v5_root_observations(
 root_task_id,observation_sha256,first_query_id,first_epoch,first_seen_at)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (root_task_id,observation_sha256) DO NOTHING`,
		reservation.RootTaskID, normalized.digest, queryID, rootEpoch, dbTime(now)); err != nil {
		return nil, metrics, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO v5_query_observations(query_id,root_task_id,
 observation_sha256,root_epoch,charged_release_facts,charged_influence_facts,
 charged_predicate_atom_count,charged_composite_count,charged_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, queryID, reservation.RootTaskID, normalized.digest,
		rootEpoch, newRelease, newInfluence, chargedAtoms, chargedComposite, newOutcome, dbTime(now)); err != nil {
		return nil, metrics, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE v5_query_exposure_reservations SET status='SETTLED',
 actual_release_facts=$1,actual_influence_facts=$2,actual_outcome_facts=$3,actual_predicate_atom_count=$4,
 charged_release_facts=$5,charged_influence_facts=$6,charged_outcome_facts=$7,charged_predicate_atom_count=$8,
 observation_sha256=$9,predicate_context_sha256=$10,predicate_set_sha256=$11,composite_outcome_sha256=$12,
 root_epoch=$13,settled_at=$14 WHERE query_id=$15 AND status='RESERVED'`, normalized.release.cardinality(),
		normalized.influence.cardinality(), normalized.outcome.Set.Cardinality, normalized.atomCount,
		newRelease, newInfluence, newOutcome, chargedAtoms, normalized.digest, normalized.contextDigest,
		normalized.predicateSet, normalized.compositeHash, rootEpoch, dbTime(now), queryID)
	if err != nil {
		return nil, metrics, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, metrics, errors.New("V5 exposure reservation changed concurrently")
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: reservation.TaskID, QueryID: queryID,
		Actor: "system", EventType: "QUERY_V5_EXPOSURE_SETTLED", OccurredAt: now,
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID, "profile_version": exposure.ProfileV5,
			"dictionary_set_digest": normalized.dictionarySet, "predicate_context_sha256": normalized.contextDigest,
			"predicate_set_sha256": normalized.predicateSet, "actual_predicate_atom_count": normalized.atomCount,
			"charged_predicate_atom_count": chargedAtoms, "charged_composite_count": chargedComposite,
			"actual_outcome_facts": normalized.outcome.Set.Cardinality, "charged_outcome_facts": newOutcome,
			"outcome_set_sha256":      normalized.outcome.Set.SetSHA256,
			"root_outcome_set_sha256": mergedOutcome.Set.SetSHA256, "root_epoch": rootEpoch,
			"outcome_radix": radixMetrics})})
	if err != nil {
		return nil, metrics, err
	}
	return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID, ProfileVersion: exposure.ProfileV5,
		ActualReleaseFacts: normalized.release.cardinality(), ActualInfluenceFacts: normalized.influence.cardinality(),
		ActualOutcomeFacts: normalized.outcome.Set.Cardinality, ActualPredicateAtomCount: normalized.atomCount,
		ChargedReleaseFacts: newRelease, ChargedInfluenceFacts: newInfluence, ChargedOutcomeFacts: newOutcome,
		ChargedPredicateAtomCount: chargedAtoms, CompositeOutcomeSHA256: normalized.compositeHash,
		PredicateContextSHA256: normalized.contextDigest, PredicateSetSHA256: normalized.predicateSet,
		ObservationSHA256: normalized.digest, DictionarySetDigest: normalized.dictionarySet,
		ReleaseSetSHA256: normalized.release.digest, InfluenceSetSHA256: normalized.influence.digest,
		OutcomeSetSHA256: normalized.outcome.Set.SetSHA256, RootEpoch: rootEpoch}, metrics, nil
}

func getV5ExposureCharge(ctx context.Context, source rowQueryer, queryID string) (ExposureCharge, error) {
	var charge ExposureCharge
	var status string
	err := source.QueryRowContext(ctx, `SELECT reservation.query_id,reservation.root_task_id,reservation.profile_version,
 reservation.status,reservation.actual_release_facts,reservation.actual_influence_facts,reservation.actual_outcome_facts,
 reservation.actual_predicate_atom_count,reservation.charged_release_facts,reservation.charged_influence_facts,
 reservation.charged_outcome_facts,reservation.charged_predicate_atom_count,reservation.observation_sha256,
 reservation.predicate_context_sha256,reservation.predicate_set_sha256,reservation.composite_outcome_sha256,
 reservation.root_epoch,COALESCE(observation.dictionary_set_digest,''),COALESCE(observation.release_set_sha256,''),
 COALESCE(observation.influence_set_sha256,''),COALESCE(observation.outcome_set_sha256,'')
FROM v5_query_exposure_reservations reservation
LEFT JOIN v5_observations observation ON observation.observation_sha256=reservation.observation_sha256
WHERE reservation.query_id=$1`, queryID).Scan(&charge.QueryID, &charge.RootTaskID, &charge.ProfileVersion,
		&status, &charge.ActualReleaseFacts, &charge.ActualInfluenceFacts, &charge.ActualOutcomeFacts,
		&charge.ActualPredicateAtomCount, &charge.ChargedReleaseFacts, &charge.ChargedInfluenceFacts,
		&charge.ChargedOutcomeFacts, &charge.ChargedPredicateAtomCount, &charge.ObservationSHA256,
		&charge.PredicateContextSHA256, &charge.PredicateSetSHA256, &charge.CompositeOutcomeSHA256,
		&charge.RootEpoch, &charge.DictionarySetDigest, &charge.ReleaseSetSHA256, &charge.InfluenceSetSHA256,
		&charge.OutcomeSetSHA256)
	if err != nil {
		return ExposureCharge{}, err
	}
	if status != exposureSettled {
		return ExposureCharge{}, sql.ErrNoRows
	}
	return charge, nil
}

// settleV5ObservationRefMeasuredTx reauthorizes a distinct request while
// reusing an immutable observation already committed by the same root. The
// monotonic exact hash set makes all three exposure deltas provably zero.
func settleV5ObservationRefMeasuredTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string,
	reference *OrdinalObservationReference) (*ExposureCharge, exposureSettlementMetrics, error) {
	var metrics exposureSettlementMetrics
	if reference == nil || !validSHA256Hex(reference.ObservationSHA256) || !validSHA256Hex(reference.DictionarySetDigest) {
		return nil, metrics, errors.New("invalid V5 observation reference")
	}
	var reservation ExposureReservation
	var status, storedDigest string
	var actual, charged ExposureLimits
	started := time.Now()
	err := tx.QueryRowContext(ctx, `SELECT query_id,task_id,root_task_id,profile_version,
 estimated_release_facts,estimated_influence_facts,estimated_outcome_facts,status,
 actual_release_facts,actual_influence_facts,actual_outcome_facts,
 charged_release_facts,charged_influence_facts,charged_outcome_facts,observation_sha256
FROM v5_query_exposure_reservations WHERE query_id=$1 FOR UPDATE`, queryID).Scan(
		&reservation.QueryID, &reservation.TaskID, &reservation.RootTaskID, &reservation.ProfileVersion,
		&reservation.EstimatedReleaseFacts, &reservation.EstimatedInfluenceFacts, &reservation.EstimatedOutcomeFacts,
		&status, &actual.ReleaseFacts, &actual.InfluenceFacts, &actual.OutcomeFacts,
		&charged.ReleaseFacts, &charged.InfluenceFacts, &charged.OutcomeFacts, &storedDigest)
	metrics.ReservationLock = time.Since(started)
	if err != nil {
		return nil, metrics, err
	}
	var dictionarySet, releaseSet, influenceSet, outcomeSet, contextDigest, predicateSet, compositeHash string
	var profile, setCatalog, queryCatalog string
	var releaseCount, influenceCount, outcomeCount, atomCount, firstEpoch, rootEpoch int64
	err = tx.QueryRowContext(ctx, `SELECT observation.profile_version,observation.dictionary_set_digest,
 observation.release_set_sha256,observation.influence_set_sha256,observation.outcome_set_sha256,
 observation.predicate_context_sha256,observation.predicate_set_sha256,observation.predicate_atom_count,
 observation.composite_outcome_sha256,observation.actual_release_facts,observation.actual_influence_facts,
 observation.actual_outcome_facts,seen.first_epoch,head.epoch,dictionary.catalog_digest,query.catalog_digest
FROM v5_root_observations seen
JOIN v5_observations observation ON observation.observation_sha256=seen.observation_sha256
JOIN v5_exposure_root_heads head ON head.root_task_id=seen.root_task_id
JOIN v4_dictionary_sets dictionary ON dictionary.dictionary_set_digest=observation.dictionary_set_digest
JOIN query_records query ON query.id=$3
WHERE seen.root_task_id=$1 AND seen.observation_sha256=$2`, reservation.RootTaskID,
		reference.ObservationSHA256, queryID).Scan(&profile, &dictionarySet, &releaseSet, &influenceSet,
		&outcomeSet, &contextDigest, &predicateSet, &atomCount, &compositeHash, &releaseCount,
		&influenceCount, &outcomeCount, &firstEpoch, &rootEpoch, &setCatalog, &queryCatalog)
	if err != nil {
		return nil, metrics, err
	}
	if profile != exposure.ProfileV5 || reservation.ProfileVersion != exposure.ProfileV5 ||
		dictionarySet != reference.DictionarySetDigest || setCatalog != queryCatalog ||
		outcomeCount != atomCount+1 || reservation.EstimatedOutcomeFacts != outcomeCount || firstEpoch > rootEpoch {
		return nil, metrics, errors.New("V5 observation reference dictionary/profile mismatch")
	}
	if status == exposureSettled {
		if storedDigest != reference.ObservationSHA256 || actual.ReleaseFacts != releaseCount ||
			actual.InfluenceFacts != influenceCount || actual.OutcomeFacts != outcomeCount {
			return nil, metrics, errors.New("query already settled with different V5 replay evidence")
		}
		charge, chargeErr := getV5ExposureCharge(ctx, tx, queryID)
		return &charge, metrics, chargeErr
	}
	if status != exposureReserved {
		return nil, metrics, fmt.Errorf("V5 exposure reservation is %s", status)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO v5_query_observations(query_id,root_task_id,observation_sha256,
 root_epoch,charged_release_facts,charged_influence_facts,charged_predicate_atom_count,
 charged_composite_count,charged_outcome_facts,created_at)
VALUES ($1,$2,$3,$4,0,0,0,0,0,$5)`, queryID, reservation.RootTaskID,
		reference.ObservationSHA256, rootEpoch, dbTime(now))
	if err != nil {
		return nil, metrics, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE v5_query_exposure_reservations SET status='SETTLED',
 actual_release_facts=$1,actual_influence_facts=$2,actual_outcome_facts=$3,
 actual_predicate_atom_count=$4,charged_release_facts=0,charged_influence_facts=0,
 charged_outcome_facts=0,charged_predicate_atom_count=0,observation_sha256=$5,
 predicate_context_sha256=$6,predicate_set_sha256=$7,composite_outcome_sha256=$8,
 root_epoch=$9,settled_at=$10 WHERE query_id=$11 AND status='RESERVED'`, releaseCount, influenceCount,
		outcomeCount, atomCount, reference.ObservationSHA256, contextDigest, predicateSet, compositeHash,
		rootEpoch, dbTime(now), queryID)
	if err != nil {
		return nil, metrics, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, metrics, errors.New("V5 replay reservation changed concurrently")
	}
	_, err = appendAuditTx(ctx, tx, AuditEvent{TaskID: reservation.TaskID, QueryID: queryID,
		Actor: "system", EventType: "QUERY_V5_SEMANTIC_REPLAY", OccurredAt: now,
		Payload: mustJSON(map[string]any{"root_task_id": reservation.RootTaskID,
			"profile_version": exposure.ProfileV5, "dictionary_set_digest": dictionarySet,
			"observation_sha256": reference.ObservationSHA256, "predicate_context_sha256": contextDigest,
			"predicate_set_sha256": predicateSet, "actual_predicate_atom_count": atomCount,
			"actual_outcome_facts": outcomeCount, "charged_outcome_facts": 0, "root_epoch": rootEpoch})})
	if err != nil {
		return nil, metrics, err
	}
	return &ExposureCharge{QueryID: queryID, RootTaskID: reservation.RootTaskID, ProfileVersion: exposure.ProfileV5,
		ActualReleaseFacts: releaseCount, ActualInfluenceFacts: influenceCount, ActualOutcomeFacts: outcomeCount,
		ActualPredicateAtomCount: atomCount, ObservationSHA256: reference.ObservationSHA256,
		PredicateContextSHA256: contextDigest, PredicateSetSHA256: predicateSet,
		CompositeOutcomeSHA256: compositeHash, DictionarySetDigest: dictionarySet,
		ReleaseSetSHA256: releaseSet, InfluenceSetSHA256: influenceSet, OutcomeSetSHA256: outcomeSet,
		RootEpoch: rootEpoch}, metrics, nil
}

func releaseV5ExposureReservationTx(ctx context.Context, tx *sql.Tx, now time.Time, queryID string) error {
	var taskID, rootTaskID, status string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,root_task_id,status FROM v5_query_exposure_reservations
WHERE query_id=$1 FOR UPDATE`, queryID).Scan(&taskID, &rootTaskID, &status); err != nil {
		return err
	}
	if status == exposureReleased {
		return nil
	}
	if status != exposureReserved {
		return fmt.Errorf("cannot release %s V5 exposure reservation", status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE v5_query_exposure_reservations SET status='RELEASED',settled_at=$1
WHERE query_id=$2 AND status='RESERVED'`, dbTime(now), queryID); err != nil {
		return err
	}
	_, err := appendAuditTx(ctx, tx, AuditEvent{TaskID: taskID, QueryID: queryID, Actor: "system",
		EventType: "QUERY_V5_EXPOSURE_RELEASED", Payload: mustJSON(map[string]any{"root_task_id": rootTaskID}),
		OccurredAt: now})
	return err
}
