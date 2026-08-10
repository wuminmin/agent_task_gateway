package experiment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type DependencyScaleSpec struct {
	CandidateFacts int64
	ExistingFacts  int64
	OverlapPercent int
	OverlapFacts   int64
	UnionFacts     int64
}

const dependencyScaleFactsPerRetainedRow int64 = 5

// DependencyScaleSummaryRole is a fixed role-domain token, not a FactSet
// digest. C1 must independently summarize each role.
type DependencyScaleSummaryRole string

const (
	DependencyScaleCandidateSummaryRole DependencyScaleSummaryRole = "candidate"
	DependencyScaleExistingSummaryRole  DependencyScaleSummaryRole = "existing"
	DependencyScaleUnionSummaryRole     DependencyScaleSummaryRole = "union"
)

// DependencyScaleQueryIdentity is the fixed aggregate-shape token that keeps
// history and candidate distinct. It is not a normalized SQL digest.
type DependencyScaleQueryIdentity string

const (
	DependencyScaleCandidateQueryIdentity DependencyScaleQueryIdentity = "count(*)"
	DependencyScaleHistoryQueryIdentity   DependencyScaleQueryIdentity = "sum(metric)"
)

// DependencyScaleMemberRankInterval is a half-open/half-closed source-row
// interval (LowerExclusive, UpperInclusive]. It describes fixed contract
// algebra only; it does not parse or prepare SQL.
type DependencyScaleMemberRankInterval struct {
	LowerExclusive int64
	UpperInclusive int64
}

func (interval DependencyScaleMemberRankInterval) Rows() int64 {
	if interval.UpperInclusive <= interval.LowerExclusive {
		return 0
	}
	return interval.UpperInclusive - interval.LowerExclusive
}

func (interval DependencyScaleMemberRankInterval) IntersectionRows(other DependencyScaleMemberRankInterval) int64 {
	lower := max(interval.LowerExclusive, other.LowerExclusive)
	upper := min(interval.UpperInclusive, other.UpperInclusive)
	if upper <= lower {
		return 0
	}
	return upper - lower
}

// DependencyScaleSetState names the cardinality, source interval, and summary
// role of one fixed set. SummaryRole is a domain label for a future independent
// summary, not a generated digest.
type DependencyScaleSetState struct {
	DependencyCardinality int64
	MemberRanks           DependencyScaleMemberRankInterval
	SummaryRole           DependencyScaleSummaryRole
}

type DependencyScaleQueryState struct {
	DependencyScaleSetState
	QueryIdentity DependencyScaleQueryIdentity
}

// DependencyScaleState is the pre-run algebra for the novel operation in one
// frozen Dependency Scale cell. History is always present, including at zero
// overlap. RootBefore is the complete existing set and RootAfter is the union.
type DependencyScaleState struct {
	Candidate              DependencyScaleQueryState
	History                DependencyScaleQueryState
	Union                  DependencyScaleSetState
	RootBefore             DependencyScaleSetState
	RootAfter              DependencyScaleSetState
	ActualDependencyFacts  int64
	ChargedDependencyFacts int64
}

// NovelState returns only fixed evaluation contract algebra for a spec returned
// by ParseDependencyScale. It performs no SQL parsing, statement preparation,
// Fact derivation, or digest generation.
func (spec DependencyScaleSpec) NovelState() DependencyScaleState {
	m := spec.CandidateFacts / dependencyScaleFactsPerRetainedRow
	k := spec.OverlapFacts / dependencyScaleFactsPerRetainedRow
	candidate := DependencyScaleQueryState{
		DependencyScaleSetState: DependencyScaleSetState{
			DependencyCardinality: spec.CandidateFacts,
			MemberRanks:           DependencyScaleMemberRankInterval{LowerExclusive: 0, UpperInclusive: m},
			SummaryRole:           DependencyScaleCandidateSummaryRole,
		},
		QueryIdentity: DependencyScaleCandidateQueryIdentity,
	}
	history := DependencyScaleQueryState{
		DependencyScaleSetState: DependencyScaleSetState{
			DependencyCardinality: spec.ExistingFacts,
			MemberRanks: DependencyScaleMemberRankInterval{
				LowerExclusive: m - k,
				UpperInclusive: 2*m - k,
			},
			SummaryRole: DependencyScaleExistingSummaryRole,
		},
		QueryIdentity: DependencyScaleHistoryQueryIdentity,
	}
	union := DependencyScaleSetState{
		DependencyCardinality: spec.UnionFacts,
		MemberRanks: DependencyScaleMemberRankInterval{
			LowerExclusive: 0,
			UpperInclusive: 2*m - k,
		},
		SummaryRole: DependencyScaleUnionSummaryRole,
	}
	return DependencyScaleState{
		Candidate:              candidate,
		History:                history,
		Union:                  union,
		RootBefore:             history.DependencyScaleSetState,
		RootAfter:              union,
		ActualDependencyFacts:  spec.CandidateFacts,
		ChargedDependencyFacts: spec.CandidateFacts - spec.OverlapFacts,
	}
}

type OutcomeMerkleScaleSpec struct {
	RootFacts      int64
	CandidateFacts int64
	OverlapPercent int
	OverlapFacts   int64
}

type ArtifactScaleSpec struct {
	Rows    int64
	Columns int
}

func ParseDependencyScale(value string) (DependencyScaleSpec, error) {
	parts := strings.Split(value, "-overlap-")
	if len(parts) != 2 {
		return DependencyScaleSpec{}, errors.New("dependency scale is not frozen")
	}
	facts, ok := map[string]int64{"10k": 10_000, "100k": 100_000, "1035000": 1_035_000}[parts[0]]
	percent, err := parseFrozenOverlap(parts[1])
	if !ok || err != nil {
		return DependencyScaleSpec{}, errors.New("dependency scale is not frozen")
	}
	overlapFacts := facts * int64(percent) / 100
	return DependencyScaleSpec{
		CandidateFacts: facts,
		ExistingFacts:  facts,
		OverlapPercent: percent,
		OverlapFacts:   overlapFacts,
		UnionFacts:     2*facts - overlapFacts,
	}, nil
}

func ParseOutcomeMerkleScale(value string) (OutcomeMerkleScaleSpec, error) {
	parts := strings.Split(value, "-")
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "x") || !strings.HasPrefix(parts[2], "o") {
		return OutcomeMerkleScaleSpec{}, errors.New("Outcome-Merkle scale is not frozen")
	}
	root, rootOK := map[string]int64{"10k": 10_000, "100k": 100_000, "1m": 1_000_000}[parts[0]]
	candidate, candidateOK := map[string]int64{"x1": 1, "x100": 100, "x10k": 10_000}[parts[1]]
	percent, err := parseFrozenOverlap(strings.TrimPrefix(parts[2], "o"))
	if !rootOK || !candidateOK || err != nil {
		return OutcomeMerkleScaleSpec{}, errors.New("Outcome-Merkle scale is not frozen")
	}
	// x1 cannot represent 50% or 90% exactly. The preregistered label is
	// therefore resolved by the nearest integer, with exact halves rounded up.
	// This rule is retained in every raw sample and enforced by the finalizer.
	overlap := (candidate*int64(percent) + 50) / 100
	return OutcomeMerkleScaleSpec{RootFacts: root, CandidateFacts: candidate,
		OverlapPercent: percent, OverlapFacts: overlap}, nil
}

func ParseArtifactScale(value string) (ArtifactScaleSpec, error) {
	parts := strings.Split(value, "x")
	if len(parts) != 2 {
		return ArtifactScaleSpec{}, errors.New("artifact scale is not frozen")
	}
	rows, rowsOK := map[string]int64{"100": 100, "10k-": 10_000, "100k-": 100_000}[parts[0]]
	columns, err := strconv.Atoi(parts[1])
	if !rowsOK || err != nil || (columns != 4 && columns != 16) {
		return ArtifactScaleSpec{}, errors.New("artifact scale is not frozen")
	}
	return ArtifactScaleSpec{Rows: rows, Columns: columns}, nil
}

func ParseExtremeScale(value string) (int64, error) {
	if facts, ok := map[string]int64{"10m": 10_000_000, "100m": 100_000_000}[value]; ok {
		return facts, nil
	}
	return 0, fmt.Errorf("extreme scale %q is not frozen", value)
}

func parseFrozenOverlap(value string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || (parsed != 0 && parsed != 50 && parsed != 90 && parsed != 100) {
		return 0, errors.New("overlap is not frozen")
	}
	return parsed, nil
}
