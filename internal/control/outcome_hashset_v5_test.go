package control

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestV5SettlementAndSemanticReplayPostgres(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 61))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v5_replay", expires)
	task, err := store.GetTask(context.Background(), "task_v5_replay")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.GetPrincipal(context.Background(), task.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	limits := &PredicateFootprintLimitsV1{Version: exposure.PredicateFootprintVersion,
		MaxRawLiteralsPerQuery: 20000, MaxUniqueAtomsPerQuery: 10000,
		MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 8 << 20}
	callback := ApprovalCallback{EventID: "oa_v5_task", RawPayload: []byte(`{"decision":"approved"}`),
		ExpectedState: TaskAwaitingApproval, NewState: TaskActive, Response: []byte(`{"ok":true}`),
		Event: ApprovalEvent{TaskID: task.ID, Actor: "bob", Decision: "approved", Payload: []byte(`{"route":"manual"}`)},
		Grant: &TaskGrant{TaskID: task.ID, Subject: principal.Subject, Purpose: "V5 analysis",
			ApprovedProducts: []string{"expense_summary"}, ApprovedColumns: map[string][]string{"expense_summary": {"month", "amount"}},
			MandatoryScope: []byte(`{"department":"sales"}`), SensitivityCeiling: "internal",
			Budget: BudgetLimits{Queries: 10, Rows: 100, DBMS: 1000}, Exposure: ExposureGrant{
				Limits:         ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 20},
				ProfileVersion: exposure.ProfileV5, PredicateFootprint: limits}, ExpiresAt: expires,
			CatalogVersion: "catalog-v1", CatalogDigest: controlTestDigest, DatasourceID: "taskgate-test-expenses",
			SchemaDigest: controlTestDigest, ApprovalReceipt: "receipt_v5"}}
	if _, err := store.ApplyApprovalCallback(context.Background(), callback); err != nil {
		t.Fatalf("approve V5 task: %v", err)
	}

	contextDigest := strings.Repeat("1", 64)
	atom, err := exposure.NewPredicateAtomFactV5(exposure.PredicateAtomFactV5{PredicateContextSHA256: contextDigest,
		SemanticProductID: "expense_summary", StableRole: "expense_summary", PublicFieldID: "month",
		SQLType: "integer", Operator: "EQ", CanonicalLiteral: "i:1"})
	if err != nil {
		t.Fatal(err)
	}
	setDigest, err := exposure.PredicateSetHashV1([]exposure.FactID{atom})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("2", 64),
		ResultObservationSHA256: strings.Repeat("3", 64), VisibleRows: 1, PredicateContextSHA256: contextDigest,
		PredicateSetSHA256: setDigest, PredicateAtomCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	observation := testOrdinalObservation(t, 0, 0, "unused")
	observation.ProfileVersion = exposure.ProfileV5
	observation.Outcome.DynamicFacts = []OrdinalDynamicFact{
		dynamicV5FactForTest(t, atom, OrdinalDynamicPredicateAtom),
		dynamicV5FactForTest(t, composite, OrdinalDynamicCompositeOutcome),
	}
	reserve := func(queryID, requestID string) BudgetReservation {
		value, reserveErr := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
			QueryID: queryID, TaskID: task.ID, RequestID: requestID, Actor: "alice", RequestDigest: "digest-" + requestID,
			SQLFingerprint: "fingerprint-" + requestID, CatalogVersion: "catalog-v1", RequestedRows: 1, RequestedDBMS: 10,
			Exposure: &ExposureReservationRequest{ProfileVersion: exposure.ProfileV5, EstimatedReleaseFacts: 1,
				EstimatedInfluenceFacts: 2, EstimatedOutcomeFacts: 2}}))
		if reserveErr != nil {
			t.Fatalf("reserve V5 query: %v", reserveErr)
		}
		return value
	}
	first := reserve("query_v5_first", "request-v5-first")
	cacheKey := strings.Repeat("9", 64)
	if _, _, firstMetrics, err := store.FinalizeOrdinalQueryMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: first.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
	}, []byte(`{"rows":[[10]]}`), &OrdinalMaterializationPublish{CacheKeySHA256: cacheKey,
		ProfileVersion: exposure.ProfileV5}, nil); err != nil {
		t.Fatalf("settle V5 query: %v", err)
	} else if firstMetrics.OutcomeRadix.RootCardinality != 0 || firstMetrics.OutcomeRadix.CandidateCardinality != 2 ||
		firstMetrics.OutcomeRadix.BlocksLoaded != 0 || firstMetrics.OutcomeRadix.LeavesChanged == 0 ||
		firstMetrics.OutcomeRadix.CASAttempts != 1 || firstMetrics.OutcomeRadix.CASConflicts != 0 || firstMetrics.OutcomeRadix.CASRetries != 0 {
		t.Fatalf("initial V5 radix telemetry = %+v", firstMetrics.OutcomeRadix)
	}
	firstCharge, err := store.GetExposureCharge(context.Background(), first.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if firstCharge.ActualPredicateAtomCount != 1 || firstCharge.ActualOutcomeFacts != 2 ||
		firstCharge.ChargedPredicateAtomCount != 1 || firstCharge.ChargedOutcomeFacts != 2 ||
		firstCharge.CompositeOutcomeSHA256 == "" {
		t.Fatalf("V5 first charge = %+v", firstCharge)
	}
	events, err := store.ListAuditEvents(context.Background(), AuditFilter{QueryID: first.QueryID,
		EventType: "QUERY_V5_EXPOSURE_SETTLED"})
	if err != nil || len(events) != 1 {
		t.Fatalf("V5 settlement audit = %+v, %v", events, err)
	}
	var auditPayload map[string]any
	if err := json.Unmarshal(events[0].Payload, &auditPayload); err != nil {
		t.Fatal(err)
	}
	if auditPayload["outcome_set_sha256"] != firstCharge.OutcomeSetSHA256 ||
		auditPayload["root_outcome_set_sha256"] == "" {
		t.Fatalf("V5 audit outcome identities = %#v; candidate=%s", auditPayload, firstCharge.OutcomeSetSHA256)
	}
	materialization, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: task.ID, GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet, ProfileVersion: exposure.ProfileV5})
	if err != nil || materialization.ProfileVersion != exposure.ProfileV5 {
		t.Fatalf("lookup V5 materialization: %+v, %v", materialization, err)
	}
	secondAtom, err := exposure.NewPredicateAtomFactV5(exposure.PredicateAtomFactV5{PredicateContextSHA256: contextDigest,
		SemanticProductID: "expense_summary", StableRole: "expense_summary", PublicFieldID: "month",
		SQLType: "integer", Operator: "EQ", CanonicalLiteral: "i:2"})
	if err != nil {
		t.Fatal(err)
	}
	secondSetDigest, err := exposure.PredicateSetHashV1([]exposure.FactID{secondAtom})
	if err != nil {
		t.Fatal(err)
	}
	secondComposite, err := exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("4", 64),
		ResultObservationSHA256: strings.Repeat("5", 64), VisibleRows: 1, PredicateContextSHA256: contextDigest,
		PredicateSetSHA256: secondSetDigest, PredicateAtomCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	secondObservation := observation
	secondObservation.Outcome.DynamicFacts = []OrdinalDynamicFact{
		dynamicV5FactForTest(t, secondAtom, OrdinalDynamicPredicateAtom),
		dynamicV5FactForTest(t, secondComposite, OrdinalDynamicCompositeOutcome),
	}
	novelSecond := reserve("query_v5_second", "request-v5-second")
	if _, _, _, err := store.FinalizeOrdinalQueryMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: novelSecond.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &secondObservation,
	}, []byte(`{"rows":[[20]]}`), nil, nil); err != nil {
		t.Fatalf("settle second V5 query: %v", err)
	}
	secondCharge, err := store.GetExposureCharge(context.Background(), novelSecond.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	secondEvents, err := store.ListAuditEvents(context.Background(), AuditFilter{QueryID: novelSecond.QueryID,
		EventType: "QUERY_V5_EXPOSURE_SETTLED"})
	if err != nil || len(secondEvents) != 1 {
		t.Fatalf("second V5 settlement audit = %+v, %v", secondEvents, err)
	}
	if err := json.Unmarshal(secondEvents[0].Payload, &auditPayload); err != nil {
		t.Fatal(err)
	}
	if auditPayload["outcome_set_sha256"] != secondCharge.OutcomeSetSHA256 ||
		auditPayload["root_outcome_set_sha256"] == secondCharge.OutcomeSetSHA256 {
		t.Fatalf("second V5 audit conflated candidate and root identities: %#v", auditPayload)
	}
	second := reserve("query_v5_replay", "request-v5-replay")
	reference := OrdinalObservationReference{ObservationSHA256: firstCharge.ObservationSHA256,
		DictionarySetDigest: testOrdinalSet}
	if _, _, err := store.FinalizeOrdinalQueryWithReceipt(context.Background(), BudgetSettlement{
		QueryID: second.QueryID, Rows: 1, DBMS: 1, OrdinalObservationRef: &reference,
	}, []byte(`{"rows":[[10]]}`), nil, nil); err != nil {
		t.Fatalf("settle V5 semantic replay: %v", err)
	}
	replayCharge, err := store.GetExposureCharge(context.Background(), second.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if replayCharge.ChargedReleaseFacts != 0 || replayCharge.ChargedInfluenceFacts != 0 ||
		replayCharge.ChargedPredicateAtomCount != 0 || replayCharge.ChargedOutcomeFacts != 0 ||
		replayCharge.ObservationSHA256 != firstCharge.ObservationSHA256 {
		t.Fatalf("V5 replay charge = %+v", replayCharge)
	}
}

func TestOutcomeHashSetV5PostgresLoadsOnlyTouchedBranches(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 62))
	const rootSize = 100000
	hashes := make([]string, rootSize)
	for index := range hashes {
		hashes[index] = outcomeTestHash(index)
	}
	root, err := BuildOutcomeHashSetV5(hashes)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistV5SetObjectsTx(ctx, tx, root, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit 100K root object graph: %v", err)
	}
	tx, err = beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	newHash := outcomeTestHash(rootSize + 1)
	candidate := candidateFromHashesV5(t, []string{newHash})
	merged, novel, metrics, err := differenceAndUnionV5Tx(ctx, tx, root.Set.SetSHA256, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(novel) != 1 || merged.Set.Cardinality != rootSize+1 || metrics.RootCardinality != rootSize ||
		root.Set.BlockCount != 256 || metrics.CandidateCardinality != 1 || metrics.BlocksLoaded != 1 ||
		metrics.LeavesLoaded != 1 || metrics.HashesLoaded != 2 || metrics.BlocksReused != 255 ||
		metrics.LeavesChanged != 1 || len(merged.Blocks) != 1 {
		t.Fatalf("incremental V5 merge loaded too much or rebuilt the wrong graph: metrics=%+v leaves=%d blocks=%d root_blocks=%d",
			metrics, len(merged.Leaves), len(merged.Blocks), root.Set.BlockCount)
	}
	if err := persistV5SetObjectsTx(ctx, tx, merged, time.Now()); err != nil {
		t.Fatal(err)
	}
	replay, replayNovel, replayMetrics, err := differenceAndUnionV5Tx(ctx, tx, merged.Set.SetSHA256, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayNovel) != 0 || replay.Set.SetSHA256 != merged.Set.SetSHA256 || len(replay.Leaves) != 0 ||
		len(replay.Blocks) != 0 || replayMetrics.HashesLoaded > outcomeLeafChunkSize*2 ||
		replayMetrics.BlocksReused != int64(merged.Set.BlockCount) {
		t.Fatalf("incremental V5 replay rebuilt immutable objects: novelty=%d metrics=%+v leaves=%d blocks=%d",
			len(replayNovel), replayMetrics, len(replay.Leaves), len(replay.Blocks))
	}
}

func TestOutcomeHashSetV5SamePrefixMultiChunkBoundaries(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 63))
	const rootSize = 8193
	hashTexts := make([]string, rootSize)
	for index := range hashTexts {
		hash := samePrefixOutcomeHash(uint64((index + 1) * 2))
		hashTexts[index] = hex.EncodeToString(hash[:])
	}
	root, err := BuildOutcomeHashSetV5(hashTexts)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	persistTx, err := beginTx(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistV5SetObjectsTx(ctx, persistTx, root, time.Now()); err != nil {
		rollback(persistTx)
		t.Fatal(err)
	}
	if err := persistTx.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name          string
		value         uint64
		changedLeaves int64
	}{
		{name: "insert-first", value: 1, changedLeaves: 3},
		{name: "insert-middle", value: 8193, changedLeaves: 2},
		{name: "insert-last", value: 16387, changedLeaves: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := beginTx(ctx, store.db)
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(tx)
			candidateHash := samePrefixOutcomeHash(test.value)
			candidate := candidateFromHashesV5(t, []string{hex.EncodeToString(candidateHash[:])})
			want, wantNovel, err := DifferenceAndUnion(root, candidate)
			if err != nil {
				t.Fatal(err)
			}
			merged, novel, metrics, err := differenceAndUnionV5Tx(ctx, tx, root.Set.SetSHA256, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if len(wantNovel) != 1 || len(novel) != 1 || novel[0] != candidateHash ||
				merged.Set.SetSHA256 != want.Set.SetSHA256 || merged.Set.Cardinality != rootSize+1 ||
				metrics.LeavesLoaded != 3 || metrics.HashesLoaded != rootSize ||
				metrics.LeavesChanged != test.changedLeaves || len(merged.Blocks) != 1 {
				t.Fatalf("same-prefix merge mismatch: metrics=%+v novel=%d want=%d leaves=%d blocks=%d",
					metrics, len(novel), len(wantNovel), len(merged.Leaves), len(merged.Blocks))
			}
			for _, block := range merged.Blocks {
				refs, err := parseV5BlockManifestReferences(block.Manifest, block.Prefix8, block.Cardinality)
				if err != nil {
					t.Fatal(err)
				}
				if len(refs) != 3 {
					t.Fatalf("leaf reference count = %d, want 3", len(refs))
				}
				for index, ref := range refs {
					if ref.prefix16 != 0x4224 || ref.chunk != uint32(index) {
						t.Fatalf("leaf reference %d = prefix %04x chunk %d", index, ref.prefix16, ref.chunk)
					}
				}
			}
			if err := persistV5SetObjectsTx(ctx, tx, merged, time.Now()); err != nil {
				t.Fatal(err)
			}
			replay, replayNovel, replayMetrics, err := differenceAndUnionV5Tx(ctx, tx, merged.Set.SetSHA256, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if len(replayNovel) != 0 || replay.Set.SetSHA256 != merged.Set.SetSHA256 ||
				len(replay.Leaves) != 0 || len(replay.Blocks) != 0 || replayMetrics.LeavesChanged != 0 {
				t.Fatalf("same-prefix replay changed objects: novelty=%d metrics=%+v", len(replayNovel), replayMetrics)
			}
		})
	}

	t.Run("missing-chunk-fails-closed", func(t *testing.T) {
		tx, err := beginTx(ctx, store.db)
		if err != nil {
			t.Fatal(err)
		}
		defer rollback(tx)
		var originalBlock OutcomeHashBlockV5
		for _, block := range root.Blocks {
			originalBlock = block
		}
		refs, err := parseV5BlockManifestReferences(originalBlock.Manifest, originalBlock.Prefix8,
			originalBlock.Cardinality)
		if err != nil || len(refs) != 3 {
			t.Fatalf("parse original multi-chunk block: refs=%d err=%v", len(refs), err)
		}
		refs[1].digest = strings.Repeat("f", 64)
		corruptManifest, corruptCardinality, err := canonicalOutcomeBlockV5(originalBlock.Prefix8, refs)
		if err != nil {
			t.Fatal(err)
		}
		corruptBlockDigest := sha256HexV5(corruptManifest)
		if _, err := tx.ExecContext(ctx, `INSERT INTO v5_outcome_hash_blocks(
block_sha256,prefix8,cardinality,manifest,created_at) VALUES ($1,$2,$3,$4,$5)`,
			corruptBlockDigest, int(originalBlock.Prefix8), corruptCardinality, corruptManifest, dbTime(time.Now())); err != nil {
			t.Fatal(err)
		}
		corruptRootManifest, err := canonicalOutcomeRootV5(root.Set.Cardinality, []outcomeBlockReferenceV5{{
			prefix: originalBlock.Prefix8, digest: corruptBlockDigest, cardinality: corruptCardinality,
		}})
		if err != nil {
			t.Fatal(err)
		}
		corruptRootDigest := sha256HexV5(corruptRootManifest)
		if _, err := tx.ExecContext(ctx, `INSERT INTO v5_outcome_hash_sets(
set_sha256,cardinality,block_count,root_manifest,created_at) VALUES ($1,$2,1,$3,$4)`,
			corruptRootDigest, root.Set.Cardinality, corruptRootManifest, dbTime(time.Now())); err != nil {
			t.Fatal(err)
		}
		candidateHash := samePrefixOutcomeHash(1)
		candidate := candidateFromHashesV5(t, []string{hex.EncodeToString(candidateHash[:])})
		if _, _, _, err := differenceAndUnionV5Tx(ctx, tx, corruptRootDigest, candidate); err == nil {
			t.Fatal("missing touched chunk was accepted")
		}
	})
}

func samePrefixOutcomeHash(value uint64) [sha256.Size]byte {
	var result [sha256.Size]byte
	result[0], result[1] = 0x42, 0x24
	binary.BigEndian.PutUint64(result[sha256.Size-8:], value)
	return result
}

func outcomeTestHash(index int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("outcome-member-%08d", index)))
	return hex.EncodeToString(digest[:])
}

func dynamicV5FactForTest(t *testing.T, fact exposure.FactID, kind string) OrdinalDynamicFact {
	t.Helper()
	hash, err := fact.Hash()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fact.CanonicalPayload()
	if err != nil {
		t.Fatal(err)
	}
	return OrdinalDynamicFact{SHA256: hash, Kind: kind, CanonicalPayload: payload}
}

func TestNormalizeV5OutcomeFactsValidatesAtomCompositeBinding(t *testing.T) {
	contextDigest := strings.Repeat("1", 64)
	atom, err := exposure.NewPredicateAtomFactV5(exposure.PredicateAtomFactV5{
		PredicateContextSHA256: contextDigest, SemanticProductID: "orders", StableRole: "orders",
		PublicFieldID: "id", SQLType: "bigint", Operator: "EQ", CanonicalLiteral: "i:1"})
	if err != nil {
		t.Fatal(err)
	}
	setDigest, err := exposure.PredicateSetHashV1([]exposure.FactID{atom})
	if err != nil {
		t.Fatal(err)
	}
	composite, err := exposure.NewCompositeOutcomeFactV5(exposure.CompositeOutcomeFactV5{
		QueryNormalFormVersion: "taskgate-query-normal-form-v4", QueryNormalFormSHA256: strings.Repeat("2", 64),
		ResultObservationSHA256: strings.Repeat("3", 64), PredicateContextSHA256: contextDigest,
		PredicateSetSHA256: setDigest, PredicateAtomCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	facts := []OrdinalDynamicFact{dynamicV5FactForTest(t, composite, OrdinalDynamicCompositeOutcome),
		dynamicV5FactForTest(t, atom, OrdinalDynamicPredicateAtom)}
	normalized, metadata, err := normalizeV5OutcomeFacts(facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 2 || metadata.atomCount != 1 || metadata.context != contextDigest || metadata.setDigest != setDigest {
		t.Fatalf("V5 metadata = %+v", metadata)
	}
	bad := composite
	bad.PredicateAtomCount = 0
	if _, _, err := normalizeV5OutcomeFacts([]OrdinalDynamicFact{
		dynamicV5FactForTest(t, bad, OrdinalDynamicCompositeOutcome), facts[1]}); err == nil {
		t.Fatal("mismatched composite was accepted")
	}
}

func candidateFromHashesV5(t *testing.T, hashes []string) OutcomeCandidateV5 {
	t.Helper()
	result := OutcomeCandidateV5{}
	for _, text := range hashes {
		hash, err := decodeOutcomeHashV5(text)
		if err != nil {
			t.Fatal(err)
		}
		result.Hashes = append(result.Hashes, hash)
	}
	return result
}

func TestOutcomeHashSetV5ExactDifferenceUnionAndReplay(t *testing.T) {
	root, err := BuildOutcomeHashSetV5([]string{outcomeTestHash(1), outcomeTestHash(2)})
	if err != nil {
		t.Fatal(err)
	}
	candidate := candidateFromHashesV5(t, []string{outcomeTestHash(2), outcomeTestHash(3), outcomeTestHash(3)})
	union, novel, err := DifferenceAndUnion(root, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(novel) != 1 || OutcomeHashTextV5(novel[0]) != outcomeTestHash(3) || union.Set.Cardinality != 3 {
		t.Fatalf("unexpected exact union: novelty=%v cardinality=%d", novel, union.Set.Cardinality)
	}
	replay, replayNovel, err := DifferenceAndUnion(union, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayNovel) != 0 || replay.Set.SetSHA256 != union.Set.SetSHA256 {
		t.Fatal("semantic replay changed the exact outcome set")
	}
}

func TestOutcomeHashSetV5DeterministicAndTamperEvident(t *testing.T) {
	input := make([]string, 0, 10001)
	for index := 0; index < 10000; index++ {
		input = append(input, outcomeTestHash(index))
	}
	input = append(input, input[0])
	first, err := BuildOutcomeHashSetV5(input)
	if err != nil {
		t.Fatal(err)
	}
	for left, right := 0, len(input)-1; left < right; left, right = left+1, right-1 {
		input[left], input[right] = input[right], input[left]
	}
	second, err := BuildOutcomeHashSetV5(input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Set.Cardinality != 10000 || first.Set.SetSHA256 != second.Set.SetSHA256 {
		t.Fatal("V5 set is not permutation/duplicate invariant")
	}
	for digest, block := range first.Blocks {
		block.Manifest = append([]byte(nil), block.Manifest...)
		block.Manifest[len(block.Manifest)-1] ^= 1
		first.Blocks[digest] = block
		break
	}
	if err := VerifySetDigest(first); err == nil {
		t.Fatal("tampered V5 block was accepted")
	}
}
