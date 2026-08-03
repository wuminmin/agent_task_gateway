package finalv5oracle

import (
	"encoding/hex"
	"fmt"
	"sort"
)

const SetAlgebraVersion = "TASKGATE-FINAL-V5-SET-ALGEBRA-V1"

// SetAlgebraResult contains exact ordinary-set identities for the two inputs
// and the three derived sets. Each digest has a distinct role domain, so equal
// member bytes cannot be substituted between candidate, existing, overlap,
// novel, and union fields.
type SetAlgebraResult struct {
	CandidateCardinality int64  `json:"candidate_cardinality"`
	CandidateSetSHA256   string `json:"candidate_set_sha256"`
	ExistingCardinality  int64  `json:"existing_cardinality"`
	ExistingSetSHA256    string `json:"existing_set_sha256"`
	OverlapCardinality   int64  `json:"overlap_cardinality"`
	OverlapSetSHA256     string `json:"overlap_set_sha256"`
	NovelCardinality     int64  `json:"novel_cardinality"`
	NovelSetSHA256       string `json:"novel_set_sha256"`
	UnionCardinality     int64  `json:"union_cardinality"`
	UnionSetSHA256       string `json:"union_set_sha256"`
}

// EvaluateSetAlgebra independently computes candidate intersection existing,
// candidate difference existing, and candidate union existing. Inputs are
// canonical lowercase SHA-256 member identities. A duplicate inside either
// input is an error; duplicates across the inputs are the intended overlap.
func EvaluateSetAlgebra(candidate, existing []string) (SetAlgebraResult, error) {
	candidateSet, err := exactDigestSet("candidate", candidate)
	if err != nil {
		return SetAlgebraResult{}, err
	}
	existingSet, err := exactDigestSet("existing", existing)
	if err != nil {
		return SetAlgebraResult{}, err
	}

	overlap := make([]string, 0)
	novel := make([]string, 0, len(candidateSet))
	unionMap := make(map[string]bool, len(candidateSet)+len(existingSet))
	for value := range existingSet {
		unionMap[value] = true
	}
	for value := range candidateSet {
		unionMap[value] = true
		if existingSet[value] {
			overlap = append(overlap, value)
		} else {
			novel = append(novel, value)
		}
	}
	union := make([]string, 0, len(unionMap))
	for value := range unionMap {
		union = append(union, value)
	}

	candidateValues := sortedSetValues(candidateSet)
	existingValues := sortedSetValues(existingSet)
	sort.Strings(overlap)
	sort.Strings(novel)
	sort.Strings(union)
	return SetAlgebraResult{
		CandidateCardinality: int64(len(candidateValues)), CandidateSetSHA256: digestSetRole("candidate", candidateValues),
		ExistingCardinality: int64(len(existingValues)), ExistingSetSHA256: digestSetRole("existing", existingValues),
		OverlapCardinality: int64(len(overlap)), OverlapSetSHA256: digestSetRole("overlap", overlap),
		NovelCardinality: int64(len(novel)), NovelSetSHA256: digestSetRole("novel", novel),
		UnionCardinality: int64(len(union)), UnionSetSHA256: digestSetRole("union", union),
	}, nil
}

func exactDigestSet(role string, values []string) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for index, value := range values {
		if !validSHA256(value) {
			return nil, fmt.Errorf("%s member %d is not a canonical SHA-256", role, index+1)
		}
		if result[value] {
			return nil, fmt.Errorf("%s contains duplicate member %s", role, value)
		}
		result[value] = true
	}
	return result, nil
}

func sortedSetValues(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func digestSetRole(role string, values []string) string {
	if !validSetRole(role) {
		panic("invalid final-V5 set-algebra role")
	}
	target := newDomainHash(SetAlgebraVersion + "/" + role)
	writeUint64(target, uint64(len(values)))
	for _, value := range values {
		decoded, err := hex.DecodeString(value)
		if err != nil {
			panic("validated final-V5 set member became invalid")
		}
		writeFramed(target, decoded)
	}
	return digestHex(target)
}

func validSetRole(role string) bool {
	switch role {
	case "candidate", "existing", "overlap", "novel", "union":
		return true
	default:
		return false
	}
}
