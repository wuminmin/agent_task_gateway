package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
)

func TestProvSQLBaseFactsUseFrozenEntityAndCanonicalFieldIdentity(t *testing.T) {
	products, err := provSQLDatasetProducts()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		product       benchmarkDatasetProduct
		rowIndex      int64
		keyComponents []string
		fields        []string
	}{
		{products[0], 0, []string{"final_v5.provsql_orders", "orderkey", "bigint", "i:1"},
			[]string{"provsql_orders.orderkey", "provsql_orders.status", "provsql_orders.partition_key"}},
		{products[1], 0, []string{"final_v5.provsql_lineitem", "orderkey", "bigint", "i:1", "linenumber", "integer", "i:1"},
			[]string{"provsql_lineitem.orderkey", "provsql_lineitem.linenumber", "provsql_lineitem.extendedprice", "provsql_lineitem.partition_key"}},
		{products[2], 100, []string{"final_v5.provsql_nonce", "nonce_id", "bigint", "i:101"},
			[]string{"provsql_nonce.nonce_id", "provsql_nonce.partition_key"}},
	}
	for _, test := range tests {
		facts, err := buildProvSQLProductRowFacts(test.product, test.rowIndex)
		if err != nil {
			t.Fatal(err)
		}
		wantKey, err := ComposeOracleCanonicalKeyV2("base-entity", test.keyComponents...)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalFactFields(t, facts[0])[4]; got != wantKey {
			t.Fatalf("%s entity key = %s, want %s", test.product.productID, got, wantKey)
		}
		gotFields := make([]string, 0, len(facts)-1)
		for _, fact := range facts[1:] {
			fields := canonicalFactFields(t, fact)
			gotFields = append(gotFields, fields[5])
		}
		if !reflect.DeepEqual(gotFields, test.fields) {
			t.Fatalf("%s canonical fields = %v, want %v", test.product.productID, gotFields, test.fields)
		}
	}
}

func TestProvSQLDependencyIsExactly29LPlus3AndReleaseIs3x4(t *testing.T) {
	cell, err := ParseProvSQLBindingKey("1k/1")
	if err != nil {
		t.Fatal(err)
	}
	report, err := GenerateProvSQLNonceJoinDependency(cell, StreamSetOptions{
		MaxInMemoryMembers: 512, CaptureMembers: 8, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Candidate.Cardinality != 29_003 || report.Stats.FactEmissions != 29_003 ||
		report.Candidate.Stats.DuplicateMembers != 0 {
		t.Fatalf("ProvSQL candidate report = %+v", report)
	}
	if report.Result.RowCount != 3 || report.Result.ColumnCount != 4 ||
		!validSHA256(report.Result.NormalizedSchemaSHA256) || !validSHA256(report.Result.CanonicalResultSHA256) {
		t.Fatalf("ProvSQL logical result = %+v", report.Result)
	}
	if report.Release.Cardinality != 12 || !validSHA256(report.Release.SetSHA256) {
		t.Fatalf("ProvSQL release = %+v", report.Release)
	}
	if report.Candidate.SetSHA256 != "d261861e25b089e8a1fe069ad6e121ceabf4da6bb60d68bb4f28c45a02d483a0" ||
		report.Result.CanonicalResultSHA256 != "75ceb4164134f6c19b8f9a22d86a2b09fb5e7c2ae6102f58a797ad2c662ed444" ||
		report.Release.SetSHA256 != "2541517890d8d1a7a502bae6876ebcd4958f180712705c83f1c8ff6ec07ada49" {
		t.Fatalf("1k/1 fixed vector moved: dependency=%s result=%s release=%s",
			report.Candidate.SetSHA256, report.Result.CanonicalResultSHA256, report.Release.SetSHA256)
	}
	rows, _, err := provSQLLogicalRows(1_000)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := [][]any{
		{int64(0), "110456.10", int64(4995), int64(1665)},
		{int64(1), "110679.25", int64(5010), int64(1670)},
		{int64(2), "110239.65", int64(4995), int64(1665)},
	}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("1k logical rows = %v, want %v", rows, wantRows)
	}
	t.Logf("dependency=%s result=%s release=%s", report.Candidate.SetSHA256,
		report.Result.CanonicalResultSHA256, report.Release.SetSHA256)
}

func TestProvSQLWeightedWitnessSortsAndBindsMultiplicity(t *testing.T) {
	members := []struct {
		hash string
		mult uint64
	}{
		{hash: hashProvSQLTestMember("c"), mult: 3},
		{hash: hashProvSQLTestMember("a"), mult: 1},
		{hash: hashProvSQLTestMember("b"), mult: 2},
	}
	got, err := summarizeProvSQLWeightedWitness(func(yield func(string, uint64) error) error {
		for _, member := range members {
			if err := yield(member.hash, member.mult); err != nil {
				return err
			}
		}
		return nil
	}, StreamSetOptions{MaxInMemoryMembers: 2, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ordered := append([]struct {
		hash string
		mult uint64
	}(nil), members...)
	for left := 0; left < len(ordered); left++ {
		for right := left + 1; right < len(ordered); right++ {
			if ordered[right].hash < ordered[left].hash {
				ordered[left], ordered[right] = ordered[right], ordered[left]
			}
		}
	}
	target := sha256.New()
	oracleWriteHashString(target, "witness-multiset")
	writeUint64(target, uint64(len(ordered))*2)
	for _, member := range ordered {
		oracleWriteHashString(target, member.hash)
		oracleWriteHashString(target, fmt.Sprintf("%020d", member.mult))
	}
	want := hex.EncodeToString(target.Sum(nil))
	if got != want {
		t.Fatalf("weighted witness = %s, want %s", got, want)
	}
	if _, err := summarizeProvSQLWeightedWitness(func(yield func(string, uint64) error) error {
		_ = yield(members[0].hash, 1)
		return yield(members[0].hash, 2)
	}, StreamSetOptions{MaxInMemoryMembers: 2, TempDir: t.TempDir()}); err == nil {
		t.Fatal("duplicate weighted Fact was accepted")
	}
}

func canonicalFactFields(t *testing.T, fact CanonicalFact) []string {
	t.Helper()
	result := make([]string, 0, 8)
	rest := fact.Payload
	for len(rest) > 0 {
		value, next, ok := oracleReadCanonicalString(rest)
		if !ok {
			t.Fatalf("Fact payload is not a string tuple: %x", fact.Payload)
		}
		result = append(result, value)
		rest = next
	}
	return result
}

func hashProvSQLTestMember(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
