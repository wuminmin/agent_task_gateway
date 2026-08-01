package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	if _, _, err := store.FinalizeOrdinalQueryWithReceipt(context.Background(), BudgetSettlement{
		QueryID: first.QueryID, Rows: 1, DBMS: 1, OrdinalExposure: &observation,
	}, []byte(`{"rows":[[10]]}`), &OrdinalMaterializationPublish{CacheKeySHA256: cacheKey,
		ProfileVersion: exposure.ProfileV5}, nil); err != nil {
		t.Fatalf("settle V5 query: %v", err)
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
	materialization, err := store.LookupOrdinalMaterialization(context.Background(), OrdinalMaterializationLookup{
		CacheKeySHA256: cacheKey, TaskID: task.ID, GrantDigest: controlTestDigest,
		CatalogDigest: controlTestDigest, DictionarySetDigest: testOrdinalSet, ProfileVersion: exposure.ProfileV5})
	if err != nil || materialization.ProfileVersion != exposure.ProfileV5 {
		t.Fatalf("lookup V5 materialization: %+v, %v", materialization, err)
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
