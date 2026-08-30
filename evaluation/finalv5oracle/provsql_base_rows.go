package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// ProvSQLBaseRowFactFromKeys builds the canonical base-row Fact of one row of
// the ProvSQL Dataset Product at productIndex (0 orders, 1 lineitem, 2 nonce)
// from the values of its stable-key fields, in field order. It lets an
// external lineage system's base tuples (identified by their key columns) be
// expressed as the same canonical identities the oracle enumerates, without
// consulting any production code.
func ProvSQLBaseRowFactFromKeys(productIndex int, keyValues []any) (CanonicalFact, error) {
	products, err := provSQLDatasetProducts()
	if err != nil {
		return CanonicalFact{}, err
	}
	if productIndex < 0 || productIndex >= len(products) {
		return CanonicalFact{}, fmt.Errorf("ProvSQL Dataset Product index %d is out of range", productIndex)
	}
	product := products[productIndex]
	components := []string{product.sourceNamespace}
	next := 0
	for _, field := range product.fields {
		if !field.StableKey {
			continue
		}
		if next >= len(keyValues) {
			return CanonicalFact{}, fmt.Errorf("ProvSQL Product %s needs more key values", product.productID)
		}
		canonical, err := provSQLCanonicalFactValue(field.SQLType, keyValues[next])
		if err != nil {
			return CanonicalFact{}, err
		}
		components = append(components, field.Name, string(field.SQLType), canonical)
		next++
	}
	if next != len(keyValues) {
		return CanonicalFact{}, fmt.Errorf("ProvSQL Product %s received %d key values for %d key fields", product.productID, len(keyValues), next)
	}
	entityKey, err := ComposeOracleCanonicalKeyV2("base-entity", components...)
	if err != nil {
		return CanonicalFact{}, err
	}
	return BuildV2BaseRowFact(V2BaseRowInput{
		SourceNamespace: product.sourceNamespace, Snapshot: product.snapshot, EntityKey: entityKey,
	})
}

// ProvSQLOracleBaseRowSet streams the oracle's Facts for one frozen scale and
// nonce and summarizes only its base-row members: their count and the SHA-256
// of their sorted Fact digests. It is the target an external base-tuple
// expansion is compared against.
func ProvSQLOracleBaseRowSet(limit, nonce int64) (int64, string, error) {
	var digests []string
	err := StreamProvSQLNonceJoinFacts(limit, nonce, func(fact CanonicalFact) error {
		if fact.Kind == OracleFactKindBaseRow {
			digests = append(digests, fact.SHA256)
		}
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	if len(digests) == 0 {
		return 0, "", errors.New("ProvSQL oracle emitted no base-row facts")
	}
	return int64(len(digests)), FactDigestSetSHA256(digests), nil
}

// FactDigestSetSHA256 hashes a set of Fact digests order-independently.
func FactDigestSetSHA256(digests []string) string {
	sorted := append([]string(nil), digests...)
	sort.Strings(sorted)
	hasher := sha256.New()
	for _, digest := range sorted {
		hasher.Write([]byte(digest))
		hasher.Write([]byte{0})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}
