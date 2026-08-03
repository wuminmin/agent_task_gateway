// Package finalv5oracle is an independent trace-union and budget oracle for
// the final V5 RLS/attack experiments. It deliberately imports no production
// exposure, control, gateway, query-plan, or SQL-policy package.
package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

const OracleID = "taskgate-final-v5-independent-trace-union-oracle-v1"

type Observation struct {
	Release    []string `json:"release"`
	Dependency []string `json:"dependency"`
	Outcome    []string `json:"outcome"`
}

type Dimension struct {
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256"`
	Budget      int64  `json:"budget"`
}

type TraceUnion struct {
	Oracle     string    `json:"oracle"`
	Queries    int       `json:"queries"`
	Release    Dimension `json:"release"`
	Dependency Dimension `json:"dependency"`
	Outcome    Dimension `json:"outcome"`
}

// EvaluatePrefixes returns the exact independent union after every query.
// It intentionally recomputes from the source trace instead of consuming any
// production ledger member or response FactID.
func EvaluatePrefixes(trace []Observation) ([]TraceUnion, error) {
	if len(trace) == 0 {
		return nil, errors.New("independent oracle trace is empty")
	}
	result := make([]TraceUnion, len(trace))
	for index := range trace {
		prefix, err := Evaluate(trace[:index+1])
		if err != nil {
			return nil, err
		}
		result[index] = prefix
	}
	return result, nil
}

// Evaluate computes U_R/U_D/U_O and the preregistered floor(70% * U)
// budgets. Every nonzero dimension receives a minimum budget of one.
func Evaluate(trace []Observation) (TraceUnion, error) {
	if len(trace) == 0 {
		return TraceUnion{}, errors.New("independent oracle trace is empty")
	}
	release, dependency, outcome := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for queryIndex, observation := range trace {
		for name, input := range map[string]struct {
			values []string
			set    map[string]bool
		}{"release": {observation.Release, release}, "dependency": {observation.Dependency, dependency}, "outcome": {observation.Outcome, outcome}} {
			for _, digest := range input.values {
				if !validSHA256(digest) {
					return TraceUnion{}, fmt.Errorf("query %d has invalid %s FactID", queryIndex+1, name)
				}
				input.set[digest] = true
			}
		}
	}
	return TraceUnion{Oracle: OracleID, Queries: len(trace), Release: dimension(release), Dependency: dimension(dependency), Outcome: dimension(outcome)}, nil
}

func dimension(set map[string]bool) Dimension {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	digest := sha256.New()
	_, _ = digest.Write([]byte("TASKGATE-FINAL-V5-ORACLE-SET-V1\x00"))
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
	cardinality := int64(len(values))
	budget := cardinality * 70 / 100
	if cardinality > 0 && budget == 0 {
		budget = 1
	}
	return Dimension{Cardinality: cardinality, SetSHA256: hex.EncodeToString(digest.Sum(nil)), Budget: budget}
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == fmt.Sprintf("%x", decoded)
}
