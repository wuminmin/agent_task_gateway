package exposure

import (
	"fmt"
	"sort"
	"strings"
)

type Representation string

const (
	RepresentationRaw         Representation = "raw"
	RepresentationProjection  Representation = "projection"
	RepresentationAggregate   Representation = "aggregate"
	RepresentationGeneralized Representation = "generalized"
)

type Candidate struct {
	ID                 string         `json:"id"`
	Requirement        string         `json:"requirement"`
	Product            string         `json:"product"`
	Representation     Representation `json:"representation"`
	ReleaseCost        int64          `json:"release_cost"`
	InfluenceCost      int64          `json:"influence_cost"`
	AnswerCompleteness float64        `json:"answer_completeness"`
	QueryCoverage      float64        `json:"query_coverage"`
}

type UtilityWeights struct {
	AnswerCompleteness float64 `json:"answer_completeness"`
	QueryCoverage      float64 `json:"query_coverage"`
}

type Plan struct {
	Candidates    []Candidate `json:"candidates"`
	ReleaseCost   int64       `json:"release_cost"`
	InfluenceCost int64       `json:"influence_cost"`
	Utility       float64     `json:"utility"`
}

type plannerState struct {
	plan Plan
	ids  string
}

// Optimize selects at most one representation per requirement. Utility is
// computed only from measurable completeness and query coverage.
func Optimize(candidates []Candidate, budgetRelease, budgetInfluence int64, weights UtilityWeights) (Plan, error) {
	if budgetRelease < 0 || budgetInfluence < 0 {
		return Plan{}, fmt.Errorf("%w: planner budgets cannot be negative", ErrInvalid)
	}
	if weights.AnswerCompleteness < 0 || weights.QueryCoverage < 0 || weights.AnswerCompleteness+weights.QueryCoverage <= 0 {
		return Plan{}, fmt.Errorf("%w: planner utility weights must be non-negative and non-zero", ErrInvalid)
	}
	groups := make(map[string][]Candidate)
	seenIDs := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if err := validateCandidate(candidate); err != nil {
			return Plan{}, err
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return Plan{}, fmt.Errorf("%w: duplicate candidate %q", ErrInvalid, candidate.ID)
		}
		seenIDs[candidate.ID] = struct{}{}
		groups[candidate.Requirement] = append(groups[candidate.Requirement], candidate)
	}
	requirements := make([]string, 0, len(groups))
	for requirement := range groups {
		requirements = append(requirements, requirement)
	}
	sort.Strings(requirements)

	frontier := []plannerState{{}}
	for _, requirement := range requirements {
		next := append([]plannerState(nil), frontier...)
		sort.Slice(groups[requirement], func(i, j int) bool { return groups[requirement][i].ID < groups[requirement][j].ID })
		for _, current := range frontier {
			for _, candidate := range groups[requirement] {
				release := current.plan.ReleaseCost + candidate.ReleaseCost
				influence := current.plan.InfluenceCost + candidate.InfluenceCost
				if release > budgetRelease || influence > budgetInfluence {
					continue
				}
				selected := append(append([]Candidate(nil), current.plan.Candidates...), candidate)
				utility := current.plan.Utility + candidateUtility(candidate, weights)
				ids := current.ids + "\x00" + candidate.ID
				next = append(next, plannerState{plan: Plan{Candidates: selected, ReleaseCost: release, InfluenceCost: influence, Utility: utility}, ids: ids})
			}
		}
		frontier = paretoFrontier(next)
	}
	best := frontier[0]
	for _, candidate := range frontier[1:] {
		if betterPlan(candidate, best) {
			best = candidate
		}
	}
	return best.plan, nil
}

func validateCandidate(candidate Candidate) error {
	if strings.TrimSpace(candidate.ID) == "" || strings.TrimSpace(candidate.Requirement) == "" || strings.TrimSpace(candidate.Product) == "" {
		return fmt.Errorf("%w: candidate id, requirement, and product are required", ErrInvalid)
	}
	switch candidate.Representation {
	case RepresentationRaw, RepresentationProjection, RepresentationAggregate, RepresentationGeneralized:
	default:
		return fmt.Errorf("%w: unknown representation %q", ErrInvalid, candidate.Representation)
	}
	if candidate.ReleaseCost < 0 || candidate.InfluenceCost < 0 || candidate.AnswerCompleteness < 0 || candidate.AnswerCompleteness > 1 || candidate.QueryCoverage < 0 || candidate.QueryCoverage > 1 {
		return fmt.Errorf("%w: candidate costs or measured utility are out of range", ErrInvalid)
	}
	return nil
}

func candidateUtility(candidate Candidate, weights UtilityWeights) float64 {
	total := weights.AnswerCompleteness + weights.QueryCoverage
	return (candidate.AnswerCompleteness*weights.AnswerCompleteness + candidate.QueryCoverage*weights.QueryCoverage) / total
}

func paretoFrontier(states []plannerState) []plannerState {
	result := make([]plannerState, 0, len(states))
	for index, candidate := range states {
		dominated := false
		for otherIndex, other := range states {
			if index == otherIndex {
				continue
			}
			if other.plan.ReleaseCost <= candidate.plan.ReleaseCost && other.plan.InfluenceCost <= candidate.plan.InfluenceCost && other.plan.Utility >= candidate.plan.Utility &&
				(other.plan.ReleaseCost < candidate.plan.ReleaseCost || other.plan.InfluenceCost < candidate.plan.InfluenceCost || other.plan.Utility > candidate.plan.Utility) {
				dominated = true
				break
			}
		}
		if !dominated {
			result = append(result, candidate)
		}
	}
	return result
}

func betterPlan(left, right plannerState) bool {
	if left.plan.Utility != right.plan.Utility {
		return left.plan.Utility > right.plan.Utility
	}
	if left.plan.ReleaseCost != right.plan.ReleaseCost {
		return left.plan.ReleaseCost < right.plan.ReleaseCost
	}
	if left.plan.InfluenceCost != right.plan.InfluenceCost {
		return left.plan.InfluenceCost < right.plan.InfluenceCost
	}
	return left.ids < right.ids
}
