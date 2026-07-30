package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

const (
	testOrdinalDictionary = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRowSegment        = "base-row:expense_detail"
	testCellSegment       = "base-cell:expense_detail:amount"
)

var testOrdinalSetManifest = func() ordinal.DictionarySetManifest {
	manifest, err := ordinal.NewDictionarySetManifest(controlTestDigest, ordinal.DictionarySetMember{
		PublicationName: "expense-v4", DictionaryDigest: testOrdinalDictionary,
		ManifestDigest: strings.Repeat("1", 64),
	})
	if err != nil {
		panic(err)
	}
	return manifest
}()

var testOrdinalSet = func() string {
	digest, err := OrdinalDictionarySetDigest(testOrdinalSetManifest)
	if err != nil {
		panic(err)
	}
	return digest
}()

func TestOrdinalSettlementAndReferenceReplayAvoidsBitmapWork(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 31))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_replay", expires)
	approveOrdinalTask(t, store, "task_v4_replay", expires,
		ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 20})

	observation := testOrdinalObservation(t, 0, 0, "outcome-one")
	first := reserveOrdinalQuery(t, store, "task_v4_replay", "query_v4_first", "request-v4-first")
	cacheKey := strings.Repeat("9", 64)
	var receiptExposure ExposureCharge
	receiptBuilder := func(evidence QueryReceipt) (SaveQueryReceiptRequest, error) {
		if evidence.Exposure == nil {
			return SaveQueryReceiptRequest{}, errors.New("V6 receipt builder received no exposure evidence")
		}
		receiptExposure = *evidence.Exposure
		return SaveQueryReceiptRequest{QueryID: evidence.Query.ID, Version: "6", GatewayKeyID: "gateway-v6-test",
			Signature: "v6-test-signature", TerminalAuditSequence: evidence.Audit.Sequence,
			TerminalAuditHash: evidence.Audit.CurrentHash, ReceiptJSON: []byte(`{"version":"6"}`)}, nil
	}
	if _, receipt, metrics, err := store.FinalizeOrdinalQueryMeasuredWithReceipt(context.Background(), BudgetSettlement{
		QueryID: first.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
	}, []byte(`{"rows":[[10]]}`), &OrdinalMaterializationPublish{CacheKeySHA256: cacheKey}, receiptBuilder); err != nil {
		t.Fatalf("FinalizeOrdinalQueryWithReceipt novel: %v", err)
	} else if metrics.SettlementStore <= 0 || metrics.ExposureReservationLock <= 0 || metrics.ExposureLedgerLock <= 0 {
		t.Fatalf("measured ordinal finalize omitted settlement timings: %+v", metrics)
	} else if receipt.Version != "6" {
		t.Fatalf("ordinal finalize persisted receipt version %q", receipt.Version)
	}
	if receiptExposure.DictionarySetDigest != testOrdinalSet || !validSHA256Hex(receiptExposure.ReleaseSetSHA256) ||
		!validSHA256Hex(receiptExposure.InfluenceSetSHA256) || !validSHA256Hex(receiptExposure.OutcomeSetSHA256) ||
		receiptExposure.ActualReleaseFacts != 1 || receiptExposure.ActualInfluenceFacts != 2 ||
		receiptExposure.ActualOutcomeFacts != 1 || receiptExposure.ChargedReleaseFacts != 1 ||
		receiptExposure.ChargedInfluenceFacts != 2 || receiptExposure.ChargedOutcomeFacts != 1 || receiptExposure.RootEpoch != 1 {
		t.Fatalf("V6 receipt builder received incomplete ordinal evidence: %+v", receiptExposure)
	}
	firstCharge, err := store.GetExposureCharge(context.Background(), first.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if firstCharge.ProfileVersion != exposure.ProfileV4 || firstCharge.ActualReleaseFacts != 1 ||
		firstCharge.ActualInfluenceFacts != 2 || firstCharge.ActualOutcomeFacts != 1 ||
		firstCharge.ChargedReleaseFacts != 1 || firstCharge.ChargedInfluenceFacts != 2 ||
		firstCharge.ChargedOutcomeFacts != 1 || firstCharge.RootEpoch != 1 {
		t.Fatalf("first V4 charge = %+v", firstCharge)
	}

	materialization, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: "task_v4_replay", GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet,
	})
	if err != nil {
		t.Fatalf("LookupOrdinalMaterialization: %v", err)
	}
	if materialization.SourceQueryID != first.QueryID ||
		materialization.Observation.ObservationSHA256 != firstCharge.ObservationSHA256 {
		t.Fatalf("materialization = %+v", materialization)
	}
	if _, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: "task_v4_replay", GrantDigest: strings.Repeat("f", 64),
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-grant cache lookup = %v, want not found", err)
	}

	before := ordinalContentCounts(t, store)
	second := reserveOrdinalQuery(t, store, "task_v4_replay", "query_v4_replay", "request-v4-replay")
	reference := OrdinalObservationReference{ObservationSHA256: firstCharge.ObservationSHA256,
		DictionarySetDigest: testOrdinalSet}
	if _, _, err := store.FinalizeOrdinalQueryWithReceipt(context.Background(), BudgetSettlement{
		QueryID: second.QueryID, Rows: 1, DBMS: 1, OrdinalObservationRef: &reference,
	}, []byte(`{"rows":[[10]]}`), nil, nil); err != nil {
		t.Fatalf("FinalizeOrdinalQueryWithReceipt reference replay: %v", err)
	}
	after := ordinalContentCounts(t, store)
	if after != before {
		t.Fatalf("semantic replay touched bitmap/dictionary content: before=%+v after=%+v", before, after)
	}
	secondCharge, err := store.GetExposureCharge(context.Background(), second.QueryID)
	if err != nil {
		t.Fatal(err)
	}
	if secondCharge.ChargedReleaseFacts != 0 || secondCharge.ChargedInfluenceFacts != 0 ||
		secondCharge.ChargedOutcomeFacts != 0 || secondCharge.RootEpoch != firstCharge.RootEpoch ||
		secondCharge.ObservationSHA256 != firstCharge.ObservationSHA256 {
		t.Fatalf("reference replay charge = %+v", secondCharge)
	}
	head, err := store.GetOrdinalRootHead(context.Background(), "task_v4_replay")
	if err != nil {
		t.Fatal(err)
	}
	if head.Epoch != 1 || head.Used != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 2, OutcomeFacts: 1}) {
		t.Fatalf("root head after replay = %+v", head)
	}
	var refs int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM v4_query_observations
WHERE query_id IN ($1,$2)`, first.QueryID, second.QueryID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 2 {
		t.Fatalf("query observation refs = %d, want 2", refs)
	}
	if _, err := store.EraseResultEncryptionKey(context.Background(), DefaultResultEncryptionKeyID, "privacy-admin"); err != nil {
		t.Fatalf("erase materialization result key: %v", err)
	}
	if _, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: "task_v4_replay", GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("erased-key materialization lookup = %v, want not found", err)
	}
	evicted, err := store.DeleteUnusableOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: "task_v4_replay", GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet,
	})
	if err != nil || !evicted {
		t.Fatalf("DeleteUnusableOrdinalMaterialization = %v, %v", evicted, err)
	}
	if err := store.DeleteOrdinalMaterialization(context.Background(), "task_v4_replay", cacheKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unusable materialization still exists: %v", err)
	}
}

func TestOrdinalConcurrentMaterializationPublishConvergesEquivalentEvidence(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 39))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_cache_race", expires)
	approveOrdinalTask(t, store, "task_v4_cache_race", expires,
		ExposureLimits{ReleaseFacts: 20, InfluenceFacts: 20, OutcomeFacts: 20})
	queryIDs := []string{"query_v4_cache_race_a", "query_v4_cache_race_b"}
	resultEnvelopes := [][]byte{
		[]byte(`{"rows":[[10]],"component_ms":{"attempt":1}}`),
		[]byte(`{"rows":[[10]],"component_ms":{"attempt":2}}`),
	}
	observation := testOrdinalObservation(t, 0, 0, "cache-race-outcome")
	for index, queryID := range queryIDs {
		reserveOrdinalQuery(t, store, "task_v4_cache_race", queryID, "request-v4-cache-race-"+string(rune('a'+index)))
		if _, _, err := store.FinalizeOrdinalQueryWithReceipt(context.Background(), BudgetSettlement{
			QueryID: queryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
		}, resultEnvelopes[index], nil, nil); err != nil {
			t.Fatalf("prepare completed query %s: %v", queryID, err)
		}
	}
	cacheKey := strings.Repeat("4", 64)
	cacheExpiries := []time.Time{time.Now().UTC().Add(2 * time.Hour), time.Now().UTC().Add(3 * time.Hour)}
	start := make(chan struct{})
	errorsByQuery := make([]error, len(queryIDs))
	var wait sync.WaitGroup
	for index := range queryIDs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, errorsByQuery[index] = store.PublishOrdinalMaterialization(context.Background(), queryIDs[index], OrdinalMaterializationPublish{
				CacheKeySHA256: cacheKey, ExpiresAt: &cacheExpiries[index],
			})
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByQuery {
		if err != nil {
			t.Fatalf("concurrent materialization publish %s: %v", queryIDs[index], err)
		}
	}
	materialization, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: "task_v4_cache_race", GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet,
	})
	if err != nil {
		t.Fatal(err)
	}
	if materialization.SourceQueryID != queryIDs[0] && materialization.SourceQueryID != queryIDs[1] {
		t.Fatalf("converged cache source = %q", materialization.SourceQueryID)
	}
	var charged ExposureLimits
	var resultHashes []string
	for _, queryID := range queryIDs {
		record, err := store.GetQuery(context.Background(), queryID)
		if err != nil || record.Status != QueryCompleted {
			t.Fatalf("concurrent query %s = %+v, %v", queryID, record, err)
		}
		if _, _, err := store.GetEncryptedResult(context.Background(), "task_v4_cache_race", queryID); err != nil {
			t.Fatalf("concurrent result %s: %v", queryID, err)
		}
		resultHashes = append(resultHashes, record.ResultSHA256)
		charge, err := store.GetExposureCharge(context.Background(), queryID)
		if err != nil {
			t.Fatal(err)
		}
		charged.ReleaseFacts += charge.ChargedReleaseFacts
		charged.InfluenceFacts += charge.ChargedInfluenceFacts
		charged.OutcomeFacts += charge.ChargedOutcomeFacts
	}
	if charged != (ExposureLimits{ReleaseFacts: 1, InfluenceFacts: 2, OutcomeFacts: 1}) {
		t.Fatalf("concurrent materialization total novelty = %+v", charged)
	}
	if len(resultHashes) != 2 || resultHashes[0] == resultHashes[1] {
		t.Fatalf("race fixture did not exercise request-local result envelopes: %v", resultHashes)
	}
	var cacheRows, convergedAudits int
	if err := store.db.QueryRowContext(context.Background(), `SELECT
 (SELECT count(*) FROM v4_committed_materializations WHERE cache_key_sha256=$1),
 (SELECT count(*) FROM audit_events WHERE event_type='ORDINAL_MATERIALIZATION_CONVERGED'
    AND query_id IN ($2,$3))`, cacheKey, queryIDs[0], queryIDs[1]).Scan(&cacheRows, &convergedAudits); err != nil {
		t.Fatal(err)
	}
	if cacheRows != 1 || convergedAudits != 1 {
		t.Fatalf("cache convergence rows=%d audits=%d", cacheRows, convergedAudits)
	}

	// Reusing the same semantic key for non-equivalent result evidence is a
	// typed fail-closed conflict and rolls the contender's whole transaction
	// back, including its otherwise-zero exposure reference and result.
	third := reserveOrdinalQuery(t, store, "task_v4_cache_race", "query_v4_cache_conflict", "request-v4-cache-conflict")
	conflictingObservation := testOrdinalObservation(t, 1, 0, "cache-conflict-outcome")
	_, _, err = store.FinalizeOrdinalQueryWithReceipt(context.Background(), BudgetSettlement{
		QueryID: third.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &conflictingObservation,
	}, []byte(`{"rows":[[99]]}`), &OrdinalMaterializationPublish{CacheKeySHA256: cacheKey}, nil)
	if !errors.Is(err, ErrMaterializationConflict) || !errors.Is(err, ErrConflict) {
		t.Fatalf("non-equivalent cache collision = %v, want typed conflict", err)
	}
	if CodeOf(err) != CodeConflict {
		t.Fatalf("typed materialization conflict public code = %s", CodeOf(err))
	}
	rolledBack, err := store.GetQuery(context.Background(), third.QueryID)
	if err != nil || rolledBack.Status != QueryReserved {
		t.Fatalf("cache collision query = %+v, %v", rolledBack, err)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_v4_cache_race", third.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cache collision leaked result: %v", err)
	}
}

func TestOrdinalSnapshotPublicationAdapterAndCanonicalSetDigest(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 36))
	row, err := exposure.NewBaseRowFactV2("expense_detail", "snapshot-adapter", "row-1")
	if err != nil {
		t.Fatal(err)
	}
	cell, err := exposure.NewBaseCellFactV2("expense_detail", "snapshot-adapter", "row-1", "amount", "int8", int64(10))
	if err != nil {
		t.Fatal(err)
	}
	dictionary, err := ordinal.Compile(ordinal.DictionarySpec{SourceID: "adapter-source",
		SourceNamespace: "expense_detail", Snapshot: "snapshot-adapter", SchemaDigest: controlTestDigest,
		Segments: []ordinal.SegmentSpec{
			{ID: "adapter-row", Kind: ordinal.SegmentBaseRow, Facts: []exposure.FactID{row}},
			{ID: "adapter-cell", Kind: ordinal.SegmentBaseCell, Field: "amount", Facts: []exposure.FactID{cell}},
		}})
	if err != nil {
		t.Fatalf("ordinal.Compile: %v", err)
	}
	artifact, err := dictionary.Split()
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	hot, err := artifact.Hot.MarshalBinary()
	if err != nil {
		t.Fatalf("Marshal hot: %v", err)
	}
	cold, err := artifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatalf("Marshal cold: %v", err)
	}
	publication := dictionary.ManifestDigest()
	artifacts := []OrdinalArtifactChunk{{Kind: "HOT", Index: 0, Payload: hot},
		{Kind: "COLD", Index: 0, Payload: cold}}
	if err := store.PutOrdinalSnapshotPublication(context.Background(), publication, dictionary, artifacts); err != nil {
		t.Fatalf("PutOrdinalSnapshotPublication: %v", err)
	}
	if err := store.PutOrdinalSnapshotPublication(context.Background(), publication, dictionary, artifacts); err != nil {
		t.Fatalf("PutOrdinalSnapshotPublication replay: %v", err)
	}
	if err := store.PutOrdinalSnapshotPublication(context.Background(), strings.Repeat("6", 64), dictionary, artifacts); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched Catalog publication manifest = %v, want invalid", err)
	}
	var segments, storedArtifacts int
	if err := store.db.QueryRowContext(context.Background(), `SELECT
 (SELECT count(*) FROM v4_dictionary_segments WHERE dictionary_digest=$1),
 (SELECT count(*) FROM v4_dictionary_artifacts WHERE dictionary_digest=$1)`, dictionary.DictionaryDigest()).
		Scan(&segments, &storedArtifacts); err != nil {
		t.Fatal(err)
	}
	if segments != 2 || storedArtifacts != 2 {
		t.Fatalf("adapter stored segments=%d artifacts=%d", segments, storedArtifacts)
	}
	setManifest, err := ordinal.NewDictionarySetManifest(controlTestDigest, ordinal.DictionarySetMember{
		PublicationName: "adapter-v1", DictionaryDigest: dictionary.DictionaryDigest(),
		ManifestDigest: dictionary.ManifestDigest(),
	})
	if err != nil {
		t.Fatal(err)
	}
	canonicalSet, err := OrdinalDictionarySetDigest(setManifest)
	if err != nil {
		t.Fatal(err)
	}
	ordinalDigest, err := setManifest.Digest()
	if err != nil || canonicalSet != ordinalDigest {
		t.Fatalf("Control/ordinal dictionary set digest drift: control=%s ordinal=%s err=%v", canonicalSet, ordinalDigest, err)
	}
	if err := store.PutOrdinalDictionarySet(context.Background(), setManifest); err != nil {
		t.Fatalf("Put canonical dictionary set: %v", err)
	}
	invalidSet := setManifest
	invalidSet.Version = "taskgate-ordinal-dictionary-set-v0"
	if err := store.PutOrdinalDictionarySet(context.Background(), invalidSet); !errors.Is(err, ErrInvalid) {
		t.Fatalf("noncanonical dictionary set = %v, want invalid", err)
	}
}

func TestOrdinalReferenceMustAlreadyBeCommittedForSameRoot(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 32))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_missing_ref", expires)
	approveOrdinalTask(t, store, "task_v4_missing_ref", expires,
		ExposureLimits{ReleaseFacts: 10, InfluenceFacts: 10, OutcomeFacts: 10})
	reservation := reserveOrdinalQuery(t, store, "task_v4_missing_ref", "query_v4_missing_ref", "request-v4-missing-ref")
	reference := OrdinalObservationReference{ObservationSHA256: strings.Repeat("7", 64), DictionarySetDigest: testOrdinalSet}
	if _, _, err := store.FinalizeOrdinalQueryWithReceipt(context.Background(), BudgetSettlement{
		QueryID: reservation.QueryID, Rows: 1, DBMS: 1, OrdinalObservationRef: &reference,
	}, []byte(`{"rows":[]}`), nil, nil); err == nil {
		t.Fatal("uncommitted observation reference unexpectedly settled")
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_v4_missing_ref", reservation.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed replay result lookup = %v, want not found", err)
	}
	var refs int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM v4_query_observations
WHERE query_id=$1`, reservation.QueryID).Scan(&refs); err != nil || refs != 0 {
		t.Fatalf("failed replay refs = %d, %v", refs, err)
	}
}

func TestOrdinalSettlementRejectsWrongSegmentKindAndOutOfBoundsOrdinal(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 37))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	tests := []struct {
		name string
		ref  ordinal.FactRef
	}{
		{name: "release row segment", ref: ordinal.FactRef{DictionaryDigest: testOrdinalDictionary,
			SegmentID: testRowSegment, Ordinal: 0}},
		{name: "ordinal above segment bound", ref: ordinal.FactRef{DictionaryDigest: testOrdinalDictionary,
			SegmentID: testCellSegment, Ordinal: 4}},
	}
	for index, test := range tests {
		taskID := "task_v4_invalid_" + string(rune('a'+index))
		queryID := "query_v4_invalid_" + string(rune('a'+index))
		createAwaitingApprovalTask(t, store, taskID, expires)
		approveOrdinalTask(t, store, taskID, expires,
			ExposureLimits{ReleaseFacts: 10, InfluenceFacts: 10, OutcomeFacts: 10})
		reservation := reserveOrdinalQuery(t, store, taskID, queryID, "request-"+queryID)
		observation := testOrdinalObservation(t, 0, 0, "outcome-"+test.name)
		invalid, err := ordinal.NewBitmapSet(test.ref)
		if err != nil {
			t.Fatal(err)
		}
		observation.Release.Static = invalid
		if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: reservation.QueryID,
			Rows: 1, DBMS: 1, OrdinalExposure: &observation}, []byte(`{"rows":[]}`)); err == nil {
			t.Fatalf("%s unexpectedly settled", test.name)
		}
		head, err := store.GetOrdinalRootHead(context.Background(), taskID)
		if err != nil || head.Epoch != 0 || head.Used != (ExposureLimits{}) {
			t.Fatalf("%s changed root head: %+v, %v", test.name, head, err)
		}
	}
}

func TestOrdinalSettlementRejectsDictionarySetFromAnotherCatalog(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 38))
	publishOrdinalTestDictionary(t, store)
	foreignManifest, err := ordinal.NewDictionarySetManifest(strings.Repeat("e", 64), ordinal.DictionarySetMember{
		PublicationName: "expense-v4", DictionaryDigest: testOrdinalDictionary,
		ManifestDigest: strings.Repeat("1", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutOrdinalDictionarySet(context.Background(), foreignManifest); err != nil {
		t.Fatal(err)
	}
	foreignDigest, err := foreignManifest.Digest()
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_foreign_catalog", expires)
	approveOrdinalTask(t, store, "task_v4_foreign_catalog", expires,
		ExposureLimits{ReleaseFacts: 10, InfluenceFacts: 10, OutcomeFacts: 10})
	reservation := reserveOrdinalQuery(t, store, "task_v4_foreign_catalog", "query_v4_foreign_catalog", "request-v4-foreign-catalog")
	observation := testOrdinalObservation(t, 0, 0, "foreign-catalog")
	observation.DictionarySetDigest = foreignDigest
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: reservation.QueryID,
		Rows: 1, DBMS: 1, OrdinalExposure: &observation}, []byte(`{"rows":[]}`)); err == nil {
		t.Fatal("foreign-Catalog dictionary set unexpectedly settled")
	}
	head, err := store.GetOrdinalRootHead(context.Background(), "task_v4_foreign_catalog")
	if err != nil || head.Epoch != 0 || head.Used != (ExposureLimits{}) {
		t.Fatalf("foreign-Catalog evidence changed root head: %+v, %v", head, err)
	}
}

func TestOrdinalDynamicCollisionAndOverBudgetRollback(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 33))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_collision", expires)
	approveOrdinalTask(t, store, "task_v4_collision", expires,
		ExposureLimits{ReleaseFacts: 2, InfluenceFacts: 4, OutcomeFacts: 4})

	hash := digestDynamic(OrdinalDynamicDerivedRelease, []byte("payload-a"))
	firstObservation := testOrdinalObservation(t, 0, 0, "shared-outcome")
	firstObservation.Release.DynamicFacts = []OrdinalDynamicFact{{SHA256: hash,
		Kind: OrdinalDynamicDerivedRelease, CanonicalPayload: []byte("payload-a")}}
	first := reserveOrdinalQuery(t, store, "task_v4_collision", "query_v4_collision_first", "request-v4-collision-first")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: first.QueryID,
		Rows: 1, DBMS: 1, OrdinalExposure: &firstObservation}, []byte(`{"ok":1}`)); err != nil {
		t.Fatal(err)
	}
	headBefore, err := store.GetOrdinalRootHead(context.Background(), "task_v4_collision")
	if err != nil {
		t.Fatal(err)
	}
	conflicting := testOrdinalObservation(t, 0, 0, "shared-outcome")
	conflicting.Release.DynamicFacts = []OrdinalDynamicFact{{SHA256: hash,
		Kind: OrdinalDynamicDerivedRelease, CanonicalPayload: []byte("payload-b")}}
	second := reserveOrdinalQuery(t, store, "task_v4_collision", "query_v4_collision_second", "request-v4-collision-second")
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: second.QueryID,
		Rows: 1, DBMS: 1, OrdinalExposure: &conflicting}, []byte(`{"ok":2}`)); err == nil {
		t.Fatal("dynamic hash collision unexpectedly settled")
	}
	headAfter, err := store.GetOrdinalRootHead(context.Background(), "task_v4_collision")
	if err != nil {
		t.Fatal(err)
	}
	if headAfter.Epoch != headBefore.Epoch || headAfter.Used != headBefore.Used {
		t.Fatalf("collision changed root head: before=%+v after=%+v", headBefore, headAfter)
	}
	if _, _, err := store.GetEncryptedResult(context.Background(), "task_v4_collision", second.QueryID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("collision result lookup = %v, want not found", err)
	}
	if _, err := store.ReleaseBudget(context.Background(), second.QueryID, "DYNAMIC_FACT_COLLISION"); err != nil {
		t.Fatalf("release collision reservation: %v", err)
	}

	// A different static Release fact would exceed the remaining root limit.
	overBudget := testOrdinalObservation(t, 1, 0, "shared-outcome")
	third := reserveOrdinalQueryForTask(t, store, "task_v4_collision", "query_v4_over_budget", "request-v4-over-budget", 2)
	contentBefore := ordinalContentCounts(t, store)
	if _, err := store.FinalizeQuery(context.Background(), BudgetSettlement{QueryID: third.QueryID,
		Rows: 1, DBMS: 1, OrdinalExposure: &overBudget}, []byte(`{"ok":3}`)); !errors.Is(err, ErrExposureBudgetExhausted) {
		t.Fatalf("over-budget finalize = %v, want ErrExposureBudgetExhausted", err)
	}
	if contentAfter := ordinalContentCounts(t, store); contentAfter != contentBefore {
		t.Fatalf("over-budget transaction left bitmap evidence: before=%+v after=%+v", contentBefore, contentAfter)
	}
	headFinal, err := store.GetOrdinalRootHead(context.Background(), "task_v4_collision")
	if err != nil || headFinal.Epoch != headBefore.Epoch || headFinal.Used != headBefore.Used {
		t.Fatalf("over-budget changed root head: %+v, %v", headFinal, err)
	}
	var observationRefs int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM v4_query_observations
WHERE query_id=$1`, third.QueryID).Scan(&observationRefs); err != nil || observationRefs != 0 {
		t.Fatalf("over-budget query refs = %d, %v", observationRefs, err)
	}
}

func TestOrdinalRootSettlementRetriesOptimisticCASAndRecomputesAllDimensions(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 34))
	publishOrdinalTestDictionary(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_root", expires)
	approveOrdinalTask(t, store, "task_v4_root", expires,
		ExposureLimits{ReleaseFacts: 4, InfluenceFacts: 8, OutcomeFacts: 4})
	createOrdinalChildTask(t, store, "task_v4_child_a", "task_v4_root", expires,
		ExposureLimits{ReleaseFacts: 4, InfluenceFacts: 8, OutcomeFacts: 4})
	createOrdinalChildTask(t, store, "task_v4_child_b", "task_v4_root", expires,
		ExposureLimits{ReleaseFacts: 4, InfluenceFacts: 8, OutcomeFacts: 4})
	queries := []BudgetReservation{
		reserveOrdinalQuery(t, store, "task_v4_child_a", "query_v4_child_a", "request-v4-child-a"),
		reserveOrdinalQuery(t, store, "task_v4_child_b", "query_v4_child_b", "request-v4-child-b"),
	}
	observations := []OrdinalExposureObservation{
		testOrdinalObservation(t, 0, 0, "outcome-a"),
		testOrdinalObservation(t, 1, 1, "outcome-b"),
	}
	// Hold the head row only long enough to force both optimistic transactions
	// to read epoch zero and queue at their conditional UPDATE. This makes the
	// losing CAS and its full-transaction retry deterministic.
	blocker, err := beginTx(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.ExecContext(context.Background(), `SELECT root_task_id FROM v4_exposure_root_heads
WHERE root_task_id=$1 FOR UPDATE`, "task_v4_root"); err != nil {
		rollback(blocker)
		t.Fatal(err)
	}
	errorsSeen := make([]error, 2)
	metricsSeen := make([]FinalizeQueryMetrics, 2)
	var wait sync.WaitGroup
	for index := range queries {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, metricsSeen[index], errorsSeen[index] = store.FinalizeQueryMeasured(context.Background(), BudgetSettlement{
				QueryID: queries[index].QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observations[index],
			}, []byte(`{"ok":true}`))
		}(index)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND wait_event_type='Lock'
  AND query LIKE '%UPDATE v4_exposure_root_heads SET dictionary_set_digest%'`).Scan(&waiting); err != nil {
			rollback(blocker)
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		if time.Now().After(deadline) {
			rollback(blocker)
			t.Fatal("concurrent ordinal settlements did not both reach the root-head CAS")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	retries := 0
	for index, err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent settlement %d: %v", index, err)
		}
		retries += metricsSeen[index].OrdinalCASRetries
	}
	if retries < 1 {
		t.Fatalf("concurrent V4 settlements reported no CAS retry: %+v", metricsSeen)
	}
	head, err := store.GetOrdinalRootHead(context.Background(), "task_v4_root")
	if err != nil || head.Epoch != 2 || head.Used.ReleaseFacts != 2 || head.Used.InfluenceFacts != 4 || head.Used.OutcomeFacts != 2 {
		t.Fatalf("CAS root head = %+v, %v", head, err)
	}
}

func TestV4CutoverNeverDeletesOrMixesLegacyLedger(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 35))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_legacy_before_v4", expires)
	approveExposureTask(t, store, "task_legacy_before_v4", expires,
		ExposureLimits{ReleaseFacts: 2, InfluenceFacts: 2})
	if err := store.ValidateV4Cutover(context.Background()); err == nil {
		t.Fatal("V4 cutover accepted a non-empty legacy ledger")
	}
	var legacyLedgers int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM exposure_ledgers`).Scan(&legacyLedgers); err != nil {
		t.Fatal(err)
	}
	if legacyLedgers != 1 {
		t.Fatalf("cutover preflight altered legacy ledgers: %d", legacyLedgers)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO v4_cutover_state(singleton,activated_by_task_id,activated_at)
VALUES (TRUE,$1,CURRENT_TIMESTAMP)`, "task_legacy_before_v4"); err == nil {
		t.Fatal("database trigger accepted V4 activation over legacy ledger")
	}
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM exposure_ledgers`).Scan(&legacyLedgers); err != nil || legacyLedgers != 1 {
		t.Fatalf("failed cutover altered legacy ledger: %d, %v", legacyLedgers, err)
	}
}

func TestV4DeploymentModeIsDurableAndSealsEveryLegacyEntryPoint(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 39))
	firstCatalog := strings.Repeat("a", 64)
	if err := store.EnforceExposureDeploymentMode(context.Background(), firstCatalog, true); err != nil {
		t.Fatalf("activate V4 deployment: %v", err)
	}
	var activatedCatalog string
	if err := store.db.QueryRowContext(context.Background(), `SELECT activated_catalog_digest
FROM v4_cutover_state WHERE singleton`).Scan(&activatedCatalog); err != nil || activatedCatalog != firstCatalog {
		t.Fatalf("V4 activation evidence = %q, %v", activatedCatalog, err)
	}

	// V4 Catalog upgrades are allowed; the one-way deployment mode, rather
	// than one particular Catalog version, is what remains immutable.
	if err := store.EnforceExposureDeploymentMode(context.Background(), strings.Repeat("b", 64), true); err != nil {
		t.Fatalf("re-assert V4 deployment after Catalog upgrade: %v", err)
	}
	if err := store.EnforceExposureDeploymentMode(context.Background(), strings.Repeat("c", 64), false); err == nil {
		t.Fatal("legacy Catalog was accepted after durable V4 activation")
	}
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_after_startup_cutover", expires)
	approveOrdinalTask(t, store, "task_after_startup_cutover", expires,
		ExposureLimits{ReleaseFacts: 2, InfluenceFacts: 4, OutcomeFacts: 2})
	reservation := reserveOrdinalQuery(t, store, "task_after_startup_cutover", "query_after_startup_cutover", "request-after-startup-cutover")
	if _, err := store.ReleaseBudget(context.Background(), reservation.QueryID, "TEST_COMPLETE"); err != nil {
		t.Fatalf("V4 grant/query was blocked by deployment guard: %v", err)
	}

	tests := []struct {
		name      string
		statement string
		message   string
	}{
		{name: "legacy root ledger", statement: `INSERT INTO exposure_ledgers(root_task_id) VALUES ('invalid')`,
			message: "legacy exposure accounting is disabled"},
		{name: "legacy fact", statement: `INSERT INTO exposure_facts(root_task_id) VALUES ('invalid')`,
			message: "legacy exposure accounting is disabled"},
		{name: "legacy query fact", statement: `INSERT INTO query_exposure_facts(root_task_id) VALUES ('invalid')`,
			message: "legacy exposure accounting is disabled"},
		{name: "legacy grant", statement: `INSERT INTO task_grants(task_id) VALUES ('invalid')`,
			message: "non-V4 task grants are disabled"},
		{name: "pre-cutover task query", statement: `INSERT INTO query_records(id,task_id) VALUES ('invalid-query','invalid-task')`,
			message: "queries require a V4 task grant"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.db.ExecContext(context.Background(), test.statement)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("guard error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestV4ActivationSerializesWithConcurrentLegacyWriter(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 40))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_cutover_race", expires)

	legacy, err := beginTx(context.Background(), store.db)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(legacy)
	if _, err := legacy.ExecContext(context.Background(), `SELECT pg_advisory_xact_lock($1)`, v4CutoverAdvisoryLock); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.ExecContext(context.Background(), `INSERT INTO exposure_ledgers
(root_task_id,profile_version,max_release_facts,max_influence_facts,max_outcome_facts,updated_at)
VALUES ($1,$2,1,1,0,CURRENT_TIMESTAMP)`, "task_cutover_race", exposure.ProfileV2); err != nil {
		t.Fatal(err)
	}
	activation := make(chan error, 1)
	go func() {
		activation <- store.EnforceExposureDeploymentMode(context.Background(), strings.Repeat("d", 64), true)
	}()
	select {
	case err := <-activation:
		t.Fatalf("V4 activation did not wait for legacy writer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := legacy.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-activation; err == nil {
		t.Fatal("V4 activation committed beside a concurrent legacy ledger")
	}
	var markers, ledgers int
	if err := store.db.QueryRowContext(context.Background(), `SELECT
(SELECT count(*) FROM v4_cutover_state),(SELECT count(*) FROM exposure_ledgers)`).Scan(&markers, &ledgers); err != nil {
		t.Fatal(err)
	}
	if markers != 0 || ledgers != 1 {
		t.Fatalf("serialized cutover state markers=%d legacy_ledgers=%d", markers, ledgers)
	}
}

func TestV4ActivationRejectsInflightLegacyResourceQuery(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 41))
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_cutover_inflight", expires)
	approveTask(t, store, "task_cutover_inflight", expires, BudgetLimits{Queries: 2, Rows: 10, DBMS: 100})
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: "query_cutover_inflight", TaskID: "task_cutover_inflight", RequestID: "request-cutover-inflight",
		Actor: "alice", RequestDigest: "request-digest", SQLFingerprint: "sql-fingerprint",
		CatalogVersion: "catalog-v1", RequestedRows: 1, RequestedDBMS: 10,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnforceExposureDeploymentMode(context.Background(), strings.Repeat("e", 64), true); err == nil {
		t.Fatal("V4 activation accepted an in-flight legacy resource-only query")
	}
	var markerCount int
	if err := store.db.QueryRowContext(context.Background(), `SELECT count(*) FROM v4_cutover_state`).Scan(&markerCount); err != nil || markerCount != 0 {
		t.Fatalf("failed activation marker count = %d, %v", markerCount, err)
	}
	if _, err := store.ReleaseBudget(context.Background(), reservation.QueryID, "CUTOVER_DRAINED"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnforceExposureDeploymentMode(context.Background(), strings.Repeat("e", 64), true); err != nil {
		t.Fatalf("V4 activation after draining legacy query: %v", err)
	}
}

func TestOrdinalStartupRecoveryReleasesUncommittedExposureReservation(t *testing.T) {
	path := testpostgres.SchemaDSN(t)
	cipher := testCipher(t, 38)
	store := openTestStore(t, path, cipher)
	expires := time.Now().UTC().Add(time.Hour)
	createAwaitingApprovalTask(t, store, "task_v4_recovery", expires)
	approveOrdinalTask(t, store, "task_v4_recovery", expires,
		ExposureLimits{ReleaseFacts: 10, InfluenceFacts: 10, OutcomeFacts: 10})
	reservation := reserveOrdinalQuery(t, store, "task_v4_recovery", "query_v4_recovery", "request-v4-recovery")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(context.Background(), path, cipher)
	if err != nil {
		t.Fatalf("restart V4 store: %v", err)
	}
	defer restarted.Close()
	record, err := restarted.GetQuery(context.Background(), reservation.QueryID)
	if err != nil || record.Status != QueryIndeterminate {
		t.Fatalf("recovered query = %+v, %v", record, err)
	}
	var exposureStatus string
	if err := restarted.db.QueryRowContext(context.Background(), `SELECT status FROM v4_query_exposure_reservations
WHERE query_id=$1`, reservation.QueryID).Scan(&exposureStatus); err != nil {
		t.Fatal(err)
	}
	if exposureStatus != exposureReleased {
		t.Fatalf("recovered V4 exposure reservation status = %s, want RELEASED", exposureStatus)
	}
	head, err := restarted.GetOrdinalRootHead(context.Background(), "task_v4_recovery")
	if err != nil || head.Epoch != 0 || head.Used != (ExposureLimits{}) {
		t.Fatalf("recovery changed root head = %+v, %v", head, err)
	}
}

func publishOrdinalTestDictionary(t *testing.T, store *Store) {
	t.Helper()
	rowPayload := []byte("test-row-dictionary-chunk")
	cellPayload := []byte("test-cell-dictionary-chunk")
	rowHash := sha256.Sum256(rowPayload)
	cellHash := sha256.Sum256(cellPayload)
	manifest := OrdinalDictionaryManifest{
		Digest: testOrdinalDictionary, ManifestDigest: strings.Repeat("1", 64), PublicationDigest: strings.Repeat("1", 64),
		DatasourceID: "taskgate-test-expenses", SourceNamespace: "expense_detail", SnapshotID: "snapshot-v4",
		FactCount: 8, ManifestJSON: []byte(`{"version":"test-v4"}`),
		Segments: []OrdinalDictionarySegment{
			{ID: testRowSegment, FactKind: "BASE_ROW", OrdinalCount: 4, Digest: strings.Repeat("d", 64),
				Chunks: []OrdinalDictionaryChunk{{Index: 0, SHA256: hex.EncodeToString(rowHash[:]),
					Compression: "NONE", Payload: rowPayload, UncompressedBytes: int64(len(rowPayload)), FactCount: 4}}},
			{ID: testCellSegment, FactKind: "BASE_CELL", FieldName: "amount", OrdinalCount: 4,
				Digest: strings.Repeat("f", 64), Chunks: []OrdinalDictionaryChunk{{Index: 0,
					SHA256: hex.EncodeToString(cellHash[:]), Compression: "NONE", Payload: cellPayload,
					UncompressedBytes: int64(len(cellPayload)), FactCount: 4}}},
		},
	}
	if err := store.putOrdinalDictionary(context.Background(), manifest); err != nil {
		t.Fatalf("putOrdinalDictionary: %v", err)
	}
	if err := store.PutOrdinalDictionarySet(context.Background(), testOrdinalSetManifest); err != nil {
		t.Fatalf("PutOrdinalDictionarySet: %v", err)
	}
}

func approveOrdinalTask(t *testing.T, store *Store, taskID string, expires time.Time, limits ExposureLimits) {
	t.Helper()
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := store.GetPrincipal(context.Background(), task.PrincipalID)
	if err != nil {
		t.Fatal(err)
	}
	callback := ApprovalCallback{EventID: "oa_v4_" + taskID, RawPayload: []byte(`{"decision":"approved"}`),
		ExpectedState: TaskAwaitingApproval, NewState: TaskActive, Response: []byte(`{"ok":true}`),
		Event: ApprovalEvent{TaskID: taskID, Actor: "bob", Decision: "approved", Payload: []byte(`{"route":"manual"}`)},
		Grant: &TaskGrant{TaskID: taskID, Subject: principal.Subject, Purpose: "V4 analysis",
			ApprovedProducts: []string{"expense_summary"}, ApprovedColumns: map[string][]string{"expense_summary": {"month", "amount"}},
			MandatoryScope: []byte(`{"department":"sales"}`), SensitivityCeiling: "internal",
			Budget: BudgetLimits{Queries: 10, Rows: 100, DBMS: 1000}, Exposure: ExposureGrant{Limits: limits,
				ProfileVersion: exposure.ProfileV4}, ExpiresAt: expires, CatalogVersion: "catalog-v1",
			CatalogDigest: controlTestDigest, DatasourceID: "taskgate-test-expenses", SchemaDigest: controlTestDigest,
			ApprovalReceipt: "receipt_" + taskID}}
	if _, err := store.ApplyApprovalCallback(context.Background(), callback); err != nil {
		t.Fatalf("approve V4 task: %v", err)
	}
}

func createOrdinalChildTask(t *testing.T, store *Store, taskID, parentID string, expires time.Time, limits ExposureLimits) {
	t.Helper()
	parent, err := store.GetTask(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(context.Background(), Task{ID: taskID, PrincipalID: parent.PrincipalID,
		Objective: "delegated V4 analysis", State: TaskAwaitingApproval, CatalogVersion: parent.CatalogVersion,
		RequestContext: []byte(`{}`), ParentTaskID: parentID, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	approveOrdinalTask(t, store, taskID, expires, limits)
}

func reserveOrdinalQuery(t *testing.T, store *Store, taskID, queryID, requestID string) BudgetReservation {
	t.Helper()
	return reserveOrdinalQueryForTask(t, store, taskID, queryID, requestID, 1)
}

func reserveOrdinalQueryForTask(t *testing.T, store *Store, taskID, queryID, requestID string, estimateRelease int64) BudgetReservation {
	t.Helper()
	reservation, err := store.ReserveBudget(context.Background(), testReserveRequest(ReserveRequest{
		QueryID: queryID, TaskID: taskID, RequestID: requestID, Actor: "alice", RequestDigest: "digest-" + requestID,
		SQLFingerprint: "fingerprint-" + requestID, CatalogVersion: "catalog-v1", RequestedRows: 1, RequestedDBMS: 10,
		Exposure: &ExposureReservationRequest{ProfileVersion: exposure.ProfileV4, EstimatedReleaseFacts: estimateRelease,
			EstimatedInfluenceFacts: 2, EstimatedOutcomeFacts: 1},
	}))
	if err != nil {
		t.Fatalf("ReserveBudget V4: %v", err)
	}
	return reservation
}

func testOrdinalObservation(t *testing.T, releaseOrdinal, influenceOrdinal uint32, outcomePayload string) OrdinalExposureObservation {
	t.Helper()
	release, err := ordinal.NewBitmapSet(ordinal.FactRef{DictionaryDigest: testOrdinalDictionary,
		SegmentID: testCellSegment, Ordinal: releaseOrdinal})
	if err != nil {
		t.Fatal(err)
	}
	influence, err := ordinal.NewBitmapSet(
		ordinal.FactRef{DictionaryDigest: testOrdinalDictionary, SegmentID: testRowSegment, Ordinal: influenceOrdinal},
		ordinal.FactRef{DictionaryDigest: testOrdinalDictionary, SegmentID: testCellSegment, Ordinal: influenceOrdinal})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := ordinal.NewBitmapSet()
	if err != nil {
		t.Fatal(err)
	}
	return OrdinalExposureObservation{ProfileVersion: exposure.ProfileV4, DictionarySetDigest: testOrdinalSet,
		Release: OrdinalHybridSet{Static: release}, Influence: OrdinalHybridSet{Static: influence},
		Outcome: OrdinalHybridSet{Static: empty, DynamicFacts: []OrdinalDynamicFact{{
			SHA256: digestDynamic(OrdinalDynamicOutcome, []byte(outcomePayload)), Kind: OrdinalDynamicOutcome, CanonicalPayload: []byte(outcomePayload),
		}}}}
}

func digestDynamic(kind string, payload []byte) string {
	domain := dynamicFactDomainV2
	if kind == OrdinalDynamicOutcome {
		domain = dynamicFactDomainV3
	}
	material := append([]byte(domain), payload...)
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:])
}

type ordinalCounts struct{ Containers, Sets, DynamicFacts, Observations int }

func ordinalContentCounts(t *testing.T, store *Store) ordinalCounts {
	t.Helper()
	var result ordinalCounts
	if err := store.db.QueryRowContext(context.Background(), `SELECT
 (SELECT count(*) FROM v4_bitmap_containers),
 (SELECT count(*) FROM v4_bitmap_sets),
 (SELECT count(*) FROM v4_dynamic_facts),
 (SELECT count(*) FROM v4_observations)`).Scan(&result.Containers, &result.Sets, &result.DynamicFacts, &result.Observations); err != nil {
		t.Fatal(err)
	}
	return result
}
