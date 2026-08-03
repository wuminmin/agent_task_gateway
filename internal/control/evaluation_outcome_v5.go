package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// OutcomeRadixEvaluationRequest is the narrow, evaluation-only entry point to
// the production PostgreSQL V5 radix persistence and incremental merge path.
// Callers generate and independently oracle-check the exact member sets. This
// hook deliberately accepts hashes only; it cannot fabricate query receipts,
// TaskGate results, or pass/fail samples.
type OutcomeRadixEvaluationRequest struct {
	RootHashes      []string
	CandidateHashes []string
}

type OutcomeRadixStorageSnapshot struct {
	Objects int64
	Bytes   int64
}

type OutcomeRadixEvaluationResult struct {
	BackendTransactionID uint64
	RootSetSHA256        string
	UnionSetSHA256       string
	ReplayUnionSHA256    string
	// ObservedUnionMemberSHA256 is computed from a complete, independently
	// read-back traversal of the committed root -> block -> leaf graph. It
	// uses the evaluation ordinary-member oracle domain rather than the
	// production Merkle root domain.
	ObservedUnionMemberSHA256 string
	RootCardinality           int64
	CandidateCardinality      int64
	UnionCardinality          int64
	NovelCardinality          int64
	ChangedObjects            int64
	ReplayChangedObjects      int64
	StorageBefore             OutcomeRadixStorageSnapshot
	StorageAfter              OutcomeRadixStorageSnapshot
	Telemetry                 OutcomeRadixTelemetryV5
	MeasuredDuration          time.Duration
}

// EvaluateOutcomeRadixPostgresV5 invokes differenceAndUnionV5Tx and
// persistV5SetObjectsTx against the supplied real Control PostgreSQL. Fixture
// construction/persistence is outside MeasuredDuration. The measured boundary
// begins before the merge transaction and ends after commit. An identical
// candidate is then replayed and must not create any new immutable object.
func EvaluateOutcomeRadixPostgresV5(ctx context.Context, dsn string,
	request OutcomeRadixEvaluationRequest) (OutcomeRadixEvaluationResult, error) {
	var result OutcomeRadixEvaluationResult
	if dsn == "" || len(request.RootHashes) == 0 || len(request.CandidateHashes) == 0 {
		return result, errors.New("Outcome radix evaluation requires a DSN and nonempty operands")
	}
	root, err := BuildOutcomeHashSetV5(request.RootHashes)
	if err != nil || root.Set.Cardinality != int64(len(request.RootHashes)) {
		if err == nil {
			err = errors.New("Outcome radix root operand contains duplicates")
		}
		return result, err
	}
	candidate, err := outcomeCandidateFromHashTextsV5(request.CandidateHashes)
	if err != nil || int64(len(candidate.Hashes)) != int64(len(request.CandidateHashes)) {
		if err == nil {
			err = errors.New("Outcome radix candidate operand contains duplicates")
		}
		return result, err
	}
	expected, expectedNovel, err := DifferenceAndUnion(root, candidate)
	if err != nil {
		return result, err
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return result, err
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		return result, err
	}

	fixtureTx, err := beginTx(ctx, db)
	if err != nil {
		return result, err
	}
	if err := persistV5SetObjectsTx(ctx, fixtureTx, root, time.Now()); err != nil {
		rollback(fixtureTx)
		return result, err
	}
	if err := fixtureTx.Commit(); err != nil {
		return result, err
	}
	result.StorageBefore, err = outcomeRadixStorageSnapshot(ctx, db)
	if err != nil {
		return result, err
	}

	measuredStarted := time.Now()
	tx, err := beginTx(ctx, db)
	if err != nil {
		return result, err
	}
	defer rollback(tx)
	var transactionText string
	if err := tx.QueryRowContext(ctx, `SELECT txid_current()::text`).Scan(&transactionText); err != nil {
		return result, err
	}
	result.BackendTransactionID, err = strconv.ParseUint(transactionText, 10, 64)
	if err != nil || result.BackendTransactionID == 0 {
		return result, errors.New("Control PostgreSQL omitted a transaction identity")
	}
	merged, novel, telemetry, err := differenceAndUnionV5Tx(ctx, tx, root.Set.SetSHA256, candidate)
	if err != nil {
		return result, err
	}
	persistStarted := time.Now()
	if err := persistV5SetObjectsTx(ctx, tx, merged, time.Now()); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	telemetry.PersistDuration = time.Since(persistStarted)
	result.MeasuredDuration = time.Since(measuredStarted)
	if merged.Set.SetSHA256 != expected.Set.SetSHA256 || merged.Set.Cardinality != expected.Set.Cardinality ||
		len(novel) != len(expectedNovel) {
		return result, errors.New("persisted Outcome radix merge differs from the full production oracle")
	}
	result.StorageAfter, err = outcomeRadixStorageSnapshot(ctx, db)
	if err != nil {
		return result, err
	}

	replayTx, err := beginTx(ctx, db)
	if err != nil {
		return result, err
	}
	defer rollback(replayTx)
	replay, replayNovel, _, err := differenceAndUnionV5Tx(ctx, replayTx, merged.Set.SetSHA256, candidate)
	if err != nil {
		return result, err
	}
	replayChanged := int64(len(replay.Leaves) + len(replay.Blocks))
	if replay.Set.SetSHA256 != merged.Set.SetSHA256 {
		replayChanged++
	}
	if len(replayNovel) != 0 || replayChanged != 0 {
		return result, errors.New("identical Outcome radix replay changed immutable storage")
	}
	if err := persistV5SetObjectsTx(ctx, replayTx, replay, time.Now()); err != nil {
		return result, err
	}
	if err := replayTx.Commit(); err != nil {
		return result, err
	}
	afterReplay, err := outcomeRadixStorageSnapshot(ctx, db)
	if err != nil {
		return result, err
	}
	if afterReplay != result.StorageAfter {
		return result, errors.New("identical Outcome radix replay grew PostgreSQL storage")
	}
	result.ObservedUnionMemberSHA256, err = outcomeRadixObservedMemberDigest(ctx, db, merged.Set.SetSHA256)
	if err != nil {
		return result, err
	}

	changed := int64(len(merged.Leaves) + len(merged.Blocks))
	if merged.Set.SetSHA256 != root.Set.SetSHA256 {
		changed++
	}
	result.RootSetSHA256 = root.Set.SetSHA256
	result.UnionSetSHA256 = merged.Set.SetSHA256
	result.ReplayUnionSHA256 = replay.Set.SetSHA256
	result.RootCardinality = root.Set.Cardinality
	result.CandidateCardinality = int64(len(candidate.Hashes))
	result.UnionCardinality = merged.Set.Cardinality
	result.NovelCardinality = int64(len(novel))
	result.ChangedObjects = changed
	result.ReplayChangedObjects = replayChanged
	result.Telemetry = telemetry
	return result, nil
}

// outcomeRadixObservedMemberDigest reconstructs the complete committed member
// set by following only persisted content-addressed references. It verifies
// every root, block, and leaf digest before hashing the normalized members, so
// an in-memory expected graph cannot stand in for PostgreSQL read-back.
func outcomeRadixObservedMemberDigest(ctx context.Context, db *sql.DB, setDigest string) (string, error) {
	graph := OutcomeHashSetObjectsV5{Leaves: map[string]OutcomeHashLeafV5{}, Blocks: map[string]OutcomeHashBlockV5{}}
	if err := db.QueryRowContext(ctx, `SELECT set_sha256,cardinality,block_count,root_manifest
FROM v5_outcome_hash_sets WHERE set_sha256=$1`, setDigest).Scan(&graph.Set.SetSHA256,
		&graph.Set.Cardinality, &graph.Set.BlockCount, &graph.Set.RootManifest); err != nil {
		return "", fmt.Errorf("read committed Outcome root: %w", err)
	}
	if graph.Set.SetSHA256 != setDigest {
		return "", errors.New("committed Outcome root identity differs from the requested union")
	}
	rootRefs, err := parseV5RootManifestReferences(graph.Set.RootManifest, graph.Set.Cardinality, graph.Set.BlockCount)
	if err != nil || sha256HexV5(graph.Set.RootManifest) != graph.Set.SetSHA256 {
		if err == nil {
			err = errors.New("committed Outcome root digest is invalid")
		}
		return "", err
	}

	blockRefs := make(map[string]outcomeBlockReferenceV5, len(rootRefs))
	blockDigests := make([]string, 0, len(rootRefs))
	for _, ref := range rootRefs {
		if _, duplicate := blockRefs[ref.digest]; duplicate {
			return "", errors.New("committed Outcome root repeats a block reference")
		}
		blockRefs[ref.digest] = ref
		blockDigests = append(blockDigests, ref.digest)
	}
	if len(blockDigests) != 0 {
		rows, queryErr := db.QueryContext(ctx, `SELECT block_sha256,prefix8,cardinality,manifest
FROM v5_outcome_hash_blocks WHERE block_sha256=ANY($1::text[])`, blockDigests)
		if queryErr != nil {
			return "", fmt.Errorf("read committed Outcome blocks: %w", queryErr)
		}
		for rows.Next() {
			var block OutcomeHashBlockV5
			var prefix int
			if scanErr := rows.Scan(&block.BlockSHA256, &prefix, &block.Cardinality, &block.Manifest); scanErr != nil {
				rows.Close()
				return "", scanErr
			}
			if prefix < 0 || prefix > 255 {
				rows.Close()
				return "", errors.New("committed Outcome block has an invalid prefix")
			}
			block.Prefix8 = byte(prefix)
			ref, present := blockRefs[block.BlockSHA256]
			if !present || ref.prefix != block.Prefix8 || ref.cardinality != block.Cardinality ||
				sha256HexV5(block.Manifest) != block.BlockSHA256 {
				rows.Close()
				return "", errors.New("committed Outcome block differs from its root reference")
			}
			if _, duplicate := graph.Blocks[block.BlockSHA256]; duplicate {
				rows.Close()
				return "", errors.New("committed Outcome block query returned a duplicate")
			}
			graph.Blocks[block.BlockSHA256] = block
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return "", rowsErr
		}
		if closeErr := rows.Close(); closeErr != nil {
			return "", closeErr
		}
	}
	if len(graph.Blocks) != len(blockRefs) {
		return "", errors.New("committed Outcome graph is missing a referenced block")
	}

	leafRefs := map[string]outcomeLeafReferenceV5{}
	for _, rootRef := range rootRefs {
		block := graph.Blocks[rootRef.digest]
		refs, parseErr := parseV5BlockManifestReferences(block.Manifest, block.Prefix8, block.Cardinality)
		if parseErr != nil {
			return "", parseErr
		}
		for _, ref := range refs {
			if prior, duplicate := leafRefs[ref.digest]; duplicate && prior != ref {
				return "", errors.New("committed Outcome graph aliases inconsistent leaf metadata")
			}
			leafRefs[ref.digest] = ref
		}
	}
	leafDigests := make([]string, 0, len(leafRefs))
	for digest := range leafRefs {
		leafDigests = append(leafDigests, digest)
	}
	if len(leafDigests) != 0 {
		rows, queryErr := db.QueryContext(ctx, `SELECT leaf_sha256,prefix16,chunk_index,cardinality,codec,payload,uncompressed_size
FROM v5_outcome_hash_leaves WHERE leaf_sha256=ANY($1::text[])`, leafDigests)
		if queryErr != nil {
			return "", fmt.Errorf("read committed Outcome leaves: %w", queryErr)
		}
		for rows.Next() {
			var leaf OutcomeHashLeafV5
			var prefix, chunk, count, size int
			var codec string
			if scanErr := rows.Scan(&leaf.LeafSHA256, &prefix, &chunk, &count, &codec, &leaf.Payload, &size); scanErr != nil {
				rows.Close()
				return "", scanErr
			}
			if prefix < 0 || prefix > 65535 || chunk < 0 || count < 1 || size < 0 {
				rows.Close()
				return "", errors.New("committed Outcome leaf has invalid numeric metadata")
			}
			leaf.Prefix16, leaf.ChunkIndex, leaf.Cardinality = uint16(prefix), uint32(chunk), count
			ref, present := leafRefs[leaf.LeafSHA256]
			decoded, decodedPrefix, decodedChunk, decodeErr := decodeOutcomeLeafV5(leaf.Payload)
			if !present || codec != "RAW" || size != len(leaf.Payload) ||
				sha256HexV5(leaf.Payload) != leaf.LeafSHA256 || ref.prefix16 != leaf.Prefix16 ||
				ref.chunk != leaf.ChunkIndex || ref.cardinality != leaf.Cardinality || decodeErr != nil ||
				decodedPrefix != leaf.Prefix16 || decodedChunk != leaf.ChunkIndex || len(decoded) != leaf.Cardinality {
				rows.Close()
				return "", errors.New("committed Outcome leaf differs from its block reference")
			}
			if _, duplicate := graph.Leaves[leaf.LeafSHA256]; duplicate {
				rows.Close()
				return "", errors.New("committed Outcome leaf query returned a duplicate")
			}
			graph.Leaves[leaf.LeafSHA256] = leaf
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			rows.Close()
			return "", rowsErr
		}
		if closeErr := rows.Close(); closeErr != nil {
			return "", closeErr
		}
	}
	if len(graph.Leaves) != len(leafRefs) {
		return "", errors.New("committed Outcome graph is missing a referenced leaf")
	}
	if err := VerifySetDigest(graph); err != nil {
		return "", fmt.Errorf("verify committed Outcome graph: %w", err)
	}
	members, err := decodeOutcomeMembersV5(graph)
	if err != nil || int64(len(members)) != graph.Set.Cardinality {
		if err == nil {
			err = errors.New("committed Outcome members differ from root cardinality")
		}
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ORDINARY-HASH-SET-ORACLE-V1\x00"))
	var text [sha256.Size * 2]byte
	for _, member := range members {
		hex.Encode(text[:], member[:])
		_, _ = hash.Write(text[:])
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func outcomeCandidateFromHashTextsV5(values []string) (OutcomeCandidateV5, error) {
	candidate := OutcomeCandidateV5{Hashes: make([][sha256.Size]byte, len(values))}
	for index, value := range values {
		hash, err := decodeOutcomeHashV5(value)
		if err != nil {
			return OutcomeCandidateV5{}, err
		}
		candidate.Hashes[index] = hash
	}
	candidate.Hashes = uniqueOutcomeHashesV5(candidate.Hashes)
	return candidate, nil
}

func outcomeRadixStorageSnapshot(ctx context.Context, db *sql.DB) (OutcomeRadixStorageSnapshot, error) {
	var result OutcomeRadixStorageSnapshot
	err := db.QueryRowContext(ctx, `
SELECT (SELECT count(*) FROM v5_outcome_hash_sets) +
       (SELECT count(*) FROM v5_outcome_hash_blocks) +
       (SELECT count(*) FROM v5_outcome_hash_leaves),
       pg_total_relation_size('v5_outcome_hash_sets') +
       pg_total_relation_size('v5_outcome_hash_blocks') +
       pg_total_relation_size('v5_outcome_hash_leaves')`).Scan(&result.Objects, &result.Bytes)
	if err != nil {
		return result, fmt.Errorf("snapshot Outcome radix storage: %w", err)
	}
	if result.Objects <= 0 || result.Bytes <= 0 {
		return result, errors.New("Outcome radix production storage is absent")
	}
	return result, nil
}
