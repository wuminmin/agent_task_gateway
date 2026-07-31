// Package viewbinding defines the task-scoped semantic View contract that is
// signed by OA and compared before every new query. It is deliberately
// independent from PostgreSQL OIDs, SQL aliases, and intermediate View names.
package viewbinding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

const Version = "taskgate-task-view-binding-v1"

const digestDomain = "TASKGATE-TASK-VIEW-BINDING-V1\x00"

// ProductContract is the semantic evidence for one product in the original
// task request. Expandable Views use compiler digests; ordinary products use
// domain-separated opaque digests so OA narrowing cannot detach the surviving
// subset from the exact product set that was initially signed. Definition
// revisions remain separate diagnostic/cache evidence: task invalidation is
// based on expanded semantics, dependencies, and interface.
type ProductContract struct {
	Product             string `json:"product"`
	CanonicalPlanDigest string `json:"canonical_plan_digest"`
	DependencyDigest    string `json:"dependency_digest"`
	InterfaceDigest     string `json:"interface_digest"`
}

// Set is canonical after New. Product order supplied by callers is irrelevant.
type Set struct {
	Version  string            `json:"version"`
	Products []ProductContract `json:"products"`
}

func New(contracts []ProductContract) (Set, error) {
	if len(contracts) == 0 {
		return Set{}, errors.New("View binding requires at least one product")
	}
	result := Set{Version: Version, Products: append([]ProductContract(nil), contracts...)}
	sort.Slice(result.Products, func(i, j int) bool { return result.Products[i].Product < result.Products[j].Product })
	for index, contract := range result.Products {
		if strings.TrimSpace(contract.Product) == "" || !validDigest(contract.CanonicalPlanDigest) ||
			!validDigest(contract.DependencyDigest) || !validDigest(contract.InterfaceDigest) {
			return Set{}, errors.New("View binding contains an invalid product contract")
		}
		if index > 0 && result.Products[index-1].Product == contract.Product {
			return Set{}, errors.New("View binding contains a duplicate product")
		}
	}
	return result, nil
}

func (set Set) Digest() (string, error) {
	canonical, err := New(set.Products)
	if err != nil || set.Version != Version {
		if err != nil {
			return "", err
		}
		return "", errors.New("unsupported View binding version")
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(digestDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
