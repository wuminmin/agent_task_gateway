package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

func TestEvaluateOutcomeRadixPostgresV5UsesPersistedProductionPathAndZeroGrowthReplay(t *testing.T) {
	dsn := testpostgres.SchemaDSN(t)
	_ = openTestStore(t, dsn, testCipher(t, 93))
	root := make([]string, 100)
	for index := range root {
		root[index] = outcomeTestHash(index)
	}
	candidate := []string{root[0], outcomeTestHash(1000)}
	wantMembers := append(append([]string(nil), root...), candidate[1])
	result, err := EvaluateOutcomeRadixPostgresV5(context.Background(), dsn,
		OutcomeRadixEvaluationRequest{RootHashes: root, CandidateHashes: candidate})
	if err != nil {
		t.Fatal(err)
	}
	if result.BackendTransactionID == 0 || result.RootCardinality != 100 || result.CandidateCardinality != 2 ||
		result.NovelCardinality != 1 || result.UnionCardinality != 101 || result.RootSetSHA256 == result.UnionSetSHA256 ||
		result.UnionSetSHA256 != result.ReplayUnionSHA256 || result.ChangedObjects <= 0 ||
		result.ReplayChangedObjects != 0 || result.Telemetry.BlocksLoaded <= 0 ||
		result.ObservedUnionMemberSHA256 != ordinaryOutcomeMemberDigestForTest(wantMembers) ||
		result.StorageBefore.Objects <= 0 || result.StorageAfter.Objects < result.StorageBefore.Objects {
		t.Fatalf("unexpected persisted evaluation result: %+v", result)
	}
}

func ordinaryOutcomeMemberDigestForTest(values []string) string {
	values = append([]string(nil), values...)
	sort.Strings(values)
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ORDINARY-HASH-SET-ORACLE-V1\x00"))
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestEvaluateOutcomeRadixPostgresV5RejectsDuplicateOrMissingOperandsBeforeBackend(t *testing.T) {
	hash := outcomeTestHash(1)
	for _, request := range []OutcomeRadixEvaluationRequest{
		{},
		{RootHashes: []string{hash, hash}, CandidateHashes: []string{outcomeTestHash(2)}},
		{RootHashes: []string{hash}, CandidateHashes: []string{hash, hash}},
	} {
		if _, err := EvaluateOutcomeRadixPostgresV5(context.Background(), "not-a-real-dsn", request); err == nil {
			t.Fatalf("invalid operands reached/returned from backend: %+v", request)
		}
	}
}
