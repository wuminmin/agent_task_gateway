package provsqlfixture

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	Version          = "taskgate-final-v5-provsql-fixture-v1"
	ProvSQLVersion   = "1.11.0"
	ProvSQLCommit    = "6388fd06b79b7d247b4ff4dad4959374d0e92358"
	PostgreSQLImage  = "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55"
	ExpectedRows     = int64(3)
	ExpectedColumns  = 4
	CarrierColumns   = 3
	DatasetRowCount  = int64(301000)
	StatementTimeout = 30 * 60 * 1000
)

// FixtureSQL and EnableProvSQLSQL are embedded from the exact files mounted by
// the formal Compose overlay. The evidence therefore binds runtime probes to
// source bytes, rather than to a copied or relabelled historical smoke file.
//
//go:embed fixture.sql
var FixtureSQL string

//go:embed enable-provsql.sql
var EnableProvSQLSQL string

const PhysicalSQL = `SELECT jsonb_build_array(
           grouped.status::bigint,
           grouped.price::text,
           grouped.lines::bigint,
           grouped.members::bigint
       )::text AS row_json,
       grouped.price AS price_provenance,
       grouped.lines AS line_provenance,
       grouped.members AS count_provenance
FROM (
    SELECT o.status,
           sum(l.extendedprice) AS price,
           sum(l.linenumber) AS lines,
           count(*) AS members
    FROM final_v5_provsql.orders AS o
    JOIN final_v5_provsql.lineitem AS l ON l.orderkey = o.orderkey AND l.partition_key = o.partition_key
    JOIN final_v5_provsql.nonce AS nonce ON nonce.partition_key = o.partition_key
    WHERE o.orderkey <= $1 AND nonce.nonce_id = $2
    GROUP BY o.status
) AS grouped
ORDER BY grouped.status`

// DatasetFingerprintSQL returns a typed, totally ordered stream. Both live
// PostgreSQL systems and the TaskGate Business database are hashed by the Go
// adapter with DatasetHasher; no server-provided digest is trusted.
const DatasetFingerprintSQL = `WITH dataset_rows(kind,key1,key2,value) AS (
    SELECT 'lineitem'::text,l.orderkey,l.linenumber::bigint,l.extendedprice::text || '|' || l.partition_key::text
    FROM final_v5_provsql.lineitem AS l
    UNION ALL
    SELECT 'nonce'::text,n.nonce_id,n.partition_key::bigint,''::text
    FROM final_v5_provsql.nonce AS n
    UNION ALL
    SELECT 'order'::text,o.orderkey,o.status,o.partition_key::text
    FROM final_v5_provsql.orders AS o
)
SELECT kind,key1,key2,value
FROM dataset_rows
ORDER BY kind,key1,key2`

const BusinessDatasetFingerprintSQL = `WITH dataset_rows(kind,key1,key2,value) AS (
    SELECT 'lineitem'::text,l.orderkey,l.linenumber::bigint,l.extendedprice::text || '|' || l.partition_key::text
    FROM reporting.provsql_lineitem AS l
    UNION ALL
    SELECT 'nonce'::text,n.nonce_id,n.partition_key::bigint,''::text
    FROM reporting.provsql_nonce AS n
    UNION ALL
    SELECT 'order'::text,o.orderkey,o.status,o.partition_key::text
    FROM reporting.provsql_orders AS o
)
SELECT kind,key1,key2,value
FROM dataset_rows
ORDER BY kind,key1,key2`

type Scale struct {
	Name  string
	Limit int64
	Base  int64
}

var scales = map[string]Scale{
	"1k":  {Name: "1k", Limit: 1000, Base: 0},
	"10k": {Name: "10k", Limit: 10000, Base: 300},
	"45k": {Name: "45k", Limit: 45000, Base: 600},
}

func ParseScale(value string) (Scale, error) {
	result, ok := scales[value]
	if !ok {
		return Scale{}, fmt.Errorf("unsupported ProvSQL scale %q", value)
	}
	return result, nil
}

// Nonce allocates disjoint source-controlled blocks to each scale. The frozen
// ProvSQL profile has exactly one process replicate, five warmups and thirty
// measured iterations. Warmup and measured ranges are deliberately separated
// so runner phase labels can never accidentally reuse a circuit.
func Nonce(scale string, processReplicate, iteration int, warmup bool) (int64, error) {
	spec, err := ParseScale(scale)
	if err != nil {
		return 0, err
	}
	if processReplicate != 1 {
		return 0, errors.New("frozen ProvSQL profile requires one process replicate")
	}
	if warmup {
		if iteration < 1 || iteration > 5 {
			return 0, errors.New("ProvSQL warmup iteration is outside 1..5")
		}
		return spec.Base + int64(iteration), nil
	}
	if iteration < 1 || iteration > 30 {
		return 0, errors.New("ProvSQL measured iteration is outside 1..30")
	}
	return spec.Base + 100 + int64(iteration), nil
}

func NonceBindingSHA256(scale string, processReplicate, iteration int, warmup bool) (string, error) {
	nonce, err := Nonce(scale, processReplicate, iteration, warmup)
	if err != nil {
		return "", err
	}
	return SHA256String(strings.Join([]string{Version, "nonce-allocation-v1", scale,
		strconv.Itoa(processReplicate), strconv.Itoa(iteration), strconv.FormatBool(warmup), strconv.FormatInt(nonce, 10)}, "\x00")), nil
}

func LogicalSQL(scale string, nonce int64) (string, error) {
	return logicalSQL(scale, nonce, "provsql_orders", "provsql_lineitem", "provsql_nonce")
}

// BusinessSQL returns the fixed direct PostgreSQL counterpart of LogicalSQL.
// Only the three relation names differ; all projection, join, predicate,
// grouping, and ordering bytes are produced by the same implementation.
func BusinessSQL(scale string, nonce int64) (string, error) {
	return logicalSQL(scale, nonce, "reporting.provsql_orders", "reporting.provsql_lineitem", "reporting.provsql_nonce")
}

func logicalSQL(scale string, nonce int64, orders, lineitem, nonceRelation string) (string, error) {
	spec, err := ParseScale(scale)
	if err != nil {
		return "", err
	}
	if nonce < 1 || nonce > 1000 {
		return "", errors.New("ProvSQL nonce is outside the frozen fixture")
	}
	return fmt.Sprintf(`SELECT o.status::bigint AS status,
       sum(l.extendedprice)::text AS price,
       sum(l.linenumber)::bigint AS lines,
       count(*)::bigint AS members
FROM %s AS o
JOIN %s AS l ON l.orderkey = o.orderkey AND l.partition_key = o.partition_key
JOIN %s AS nonce ON nonce.partition_key = o.partition_key
WHERE o.orderkey <= %d AND nonce.nonce_id = %d
GROUP BY o.status
ORDER BY o.status`, orders, lineitem, nonceRelation, spec.Limit, nonce), nil
}

func FixtureSQLSHA256() string              { return SHA256String(FixtureSQL) }
func EnableSQLSHA256() string               { return SHA256String(EnableProvSQLSQL) }
func PhysicalSQLSHA256() string             { return SHA256String(PhysicalSQL) }
func DatasetProbeSQLSHA256() string         { return SHA256String(DatasetFingerprintSQL) }
func BusinessDatasetProbeSQLSHA256() string { return SHA256String(BusinessDatasetFingerprintSQL) }

func SHA256String(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type DatasetHasher struct {
	hash  interface{ Write([]byte) (int, error) }
	count int64
}

func NewDatasetHasher() *DatasetHasher {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-PROVSQL-DATASET-V1\x00"))
	return &DatasetHasher{hash: hash}
}

func (hasher *DatasetHasher) Add(kind string, key1, key2 int64, value string) {
	var length [8]byte
	fields := []string{kind, strconv.FormatInt(key1, 10), strconv.FormatInt(key2, 10), value}
	for _, field := range fields {
		binary.BigEndian.PutUint64(length[:], uint64(len(field)))
		_, _ = hasher.hash.Write(length[:])
		_, _ = hasher.hash.Write([]byte(field))
	}
	hasher.count++
}

func (hasher *DatasetHasher) Sum() (string, int64) {
	return hex.EncodeToString(hasher.hash.(interface{ Sum([]byte) []byte }).Sum(nil)), hasher.count
}

func ExpectedDatasetSHA256() string {
	hasher := NewDatasetHasher()
	// DatasetFingerprintSQL orders lexicographically by kind, then by keys.
	for orderKey := int64(1); orderKey <= 50000; orderKey++ {
		for line := int64(1); line <= 5; line++ {
			cents := ((orderKey*13 + line*7) % 100000) + 100
			hasher.Add("lineitem", orderKey, line, centsText(cents)+"|1")
		}
	}
	for nonce := int64(1); nonce <= 1000; nonce++ {
		hasher.Add("nonce", nonce, 1, "")
	}
	for orderKey := int64(1); orderKey <= 50000; orderKey++ {
		hasher.Add("order", orderKey, orderKey%3, "1")
	}
	digest, _ := hasher.Sum()
	return digest
}

// ExpectedResultRows returns the exact four visible values emitted by both
// the external PostgreSQL query and TaskGate's released Parquet artifact.
func ExpectedResultRows(scale string) ([][]any, error) {
	spec, err := ParseScale(scale)
	if err != nil {
		return nil, err
	}
	type aggregate struct{ cents, lines, members int64 }
	groups := [3]aggregate{}
	for orderKey := int64(1); orderKey <= spec.Limit; orderKey++ {
		status := orderKey % 3
		for line := int64(1); line <= 5; line++ {
			one := groups[status]
			one.cents += ((orderKey*13 + line*7) % 100000) + 100
			one.lines += line
			one.members++
			groups[status] = one
		}
	}
	rows := make([][]any, 3)
	for status := int64(0); status < 3; status++ {
		one := groups[status]
		rows[status] = []any{status, centsText(one.cents), one.lines, one.members}
	}
	return rows, nil
}

func centsText(value int64) string {
	return fmt.Sprintf("%d.%02d", value/100, value%100)
}
