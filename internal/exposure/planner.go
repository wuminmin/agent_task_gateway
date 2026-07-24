package exposure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
	"sort"
	"strings"
)

const ExactPlannerVersion = "taskgate-overlap-exact-v2"

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

// EffectCandidate is a server-generated V2 candidate. Effect is deliberately
// absent from the public planner response; callers submit plans and measurable
// utility evidence, while the gateway constructs these FactSets itself.
type EffectCandidate struct {
	ID                 string      `json:"id"`
	Requirement        string      `json:"requirement"`
	AnswerCompleteness float64     `json:"answer_completeness"`
	QueryCoverage      float64     `json:"query_coverage"`
	Effect             Observation `json:"effect"`
	PlanDigest         string      `json:"plan_digest,omitempty"`
}

type CandidateSelection struct {
	ID                 string  `json:"id"`
	Requirement        string  `json:"requirement"`
	PlanDigest         string  `json:"plan_digest,omitempty"`
	EffectDigest       string  `json:"effect_digest"`
	AnswerCompleteness float64 `json:"answer_completeness"`
	QueryCoverage      float64 `json:"query_coverage"`
}

// ExactPlan contains exact novelty costs after subtracting root history. The
// full union Effect is returned to the trusted settlement path, but gateway
// responses should expose only its digest and cardinalities.
type ExactPlan struct {
	PlannerVersion  string               `json:"planner_version"`
	Selected        []CandidateSelection `json:"selected"`
	ReleaseCost     int64                `json:"release_cost"`
	InfluenceCost   int64                `json:"influence_cost"`
	Utility         float64              `json:"utility"`
	UnionEffect     Observation          `json:"-"`
	UnionEffectHash string               `json:"union_effect_digest"`
}

type exactCandidate struct {
	input        EffectCandidate
	release      factBitmap
	influence    factBitmap
	effectHash   string
	releaseSet   FactSet
	influenceSet FactSet
}

type exactState struct {
	selected  []int
	release   factBitmap
	influence factBitmap
	utility   float64
	ids       string
}

type factBitmap []uint64

func newFactBitmap(size int) factBitmap { return make(factBitmap, (size+63)/64) }

func (b factBitmap) clone() factBitmap { return append(factBitmap(nil), b...) }

func (b factBitmap) add(index int) { b[index/64] |= uint64(1) << uint(index%64) }

func (b factBitmap) or(other factBitmap) {
	for index := range b {
		b[index] |= other[index]
	}
}

func (b factBitmap) count() int64 {
	var result int64
	for _, word := range b {
		result += int64(bits.OnesCount64(word))
	}
	return result
}

func (b factBitmap) subsetOf(other factBitmap) bool {
	for index := range b {
		if b[index]&^other[index] != 0 {
			return false
		}
	}
	return true
}

// OptimizeEffects enumerates skip/select-one choices per requirement and
// keeps only states dominated by exact set inclusion. It therefore accounts
// for candidate overlap and history overlap without using scalar cost sums.
func OptimizeEffects(candidates []EffectCandidate, history Observation, budgetRelease, budgetInfluence int64, weights UtilityWeights) (ExactPlan, error) {
	if budgetRelease < 0 || budgetInfluence < 0 {
		return ExactPlan{}, fmt.Errorf("%w: planner budgets cannot be negative", ErrInvalid)
	}
	if weights.AnswerCompleteness < 0 || weights.QueryCoverage < 0 || weights.AnswerCompleteness+weights.QueryCoverage <= 0 {
		return ExactPlan{}, fmt.Errorf("%w: planner utility weights must be non-negative and non-zero", ErrInvalid)
	}
	normalizedHistory, err := history.Normalize()
	if err != nil {
		return ExactPlan{}, err
	}
	if normalizedHistory.ProfileVersion != ProfileV2 {
		return ExactPlan{}, fmt.Errorf("%w: exact planner requires profile %q", ErrInvalid, ProfileV2)
	}
	historyRelease, _ := NewFactSet(normalizedHistory.Release...)
	historyInfluence, _ := NewFactSet(normalizedHistory.Influence...)

	seenIDs := make(map[string]struct{}, len(candidates))
	allNovelHashes := make(map[string]struct{})
	normalized := make([]EffectCandidate, len(candidates))
	for index, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" || strings.TrimSpace(candidate.Requirement) == "" ||
			candidate.ID != strings.TrimSpace(candidate.ID) || candidate.Requirement != strings.TrimSpace(candidate.Requirement) {
			return ExactPlan{}, fmt.Errorf("%w: candidate id and requirement are required", ErrInvalid)
		}
		if _, duplicate := seenIDs[candidate.ID]; duplicate {
			return ExactPlan{}, fmt.Errorf("%w: duplicate candidate %q", ErrInvalid, candidate.ID)
		}
		seenIDs[candidate.ID] = struct{}{}
		if candidate.AnswerCompleteness < 0 || candidate.AnswerCompleteness > 1 || candidate.QueryCoverage < 0 || candidate.QueryCoverage > 1 {
			return ExactPlan{}, fmt.Errorf("%w: candidate utility evidence is out of range", ErrInvalid)
		}
		effect, normalizeErr := candidate.Effect.Normalize()
		if normalizeErr != nil || effect.ProfileVersion != ProfileV2 {
			return ExactPlan{}, fmt.Errorf("%w: candidate %q has invalid V2 effect", ErrInvalid, candidate.ID)
		}
		candidate.Effect = effect
		if candidate.PlanDigest != "" && !isSHA256(candidate.PlanDigest) {
			return ExactPlan{}, fmt.Errorf("%w: candidate %q plan digest is invalid", ErrInvalid, candidate.ID)
		}
		normalized[index] = candidate
		for _, fact := range effect.Release {
			hash, _ := fact.Hash()
			if _, known := historyRelease[hash]; !known {
				allNovelHashes[hash] = struct{}{}
			}
		}
		for _, fact := range effect.Influence {
			hash, _ := fact.Hash()
			if _, known := historyInfluence[hash]; !known {
				allNovelHashes[hash] = struct{}{}
			}
		}
	}
	hashes := make([]string, 0, len(allNovelHashes))
	for hash := range allNovelHashes {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	dense := make(map[string]int, len(hashes))
	for index, hash := range hashes {
		dense[hash] = index
	}

	prepared := make([]exactCandidate, len(normalized))
	groups := make(map[string][]int)
	for index, candidate := range normalized {
		releaseSet, _ := NewFactSet(candidate.Effect.Release...)
		influenceSet, _ := NewFactSet(candidate.Effect.Influence...)
		prepared[index] = exactCandidate{input: candidate, release: newFactBitmap(len(hashes)), influence: newFactBitmap(len(hashes)),
			releaseSet: releaseSet, influenceSet: influenceSet}
		for hash := range releaseSet {
			if _, known := historyRelease[hash]; !known {
				prepared[index].release.add(dense[hash])
			}
		}
		for hash := range influenceSet {
			if _, known := historyInfluence[hash]; !known {
				prepared[index].influence.add(dense[hash])
			}
		}
		prepared[index].effectHash, err = ObservationDigest(candidate.Effect)
		if err != nil {
			return ExactPlan{}, err
		}
		groups[candidate.Requirement] = append(groups[candidate.Requirement], index)
	}
	requirements := make([]string, 0, len(groups))
	for requirement := range groups {
		requirements = append(requirements, requirement)
	}
	sort.Strings(requirements)
	for _, requirement := range requirements {
		sort.Slice(groups[requirement], func(i, j int) bool {
			return prepared[groups[requirement][i]].input.ID < prepared[groups[requirement][j]].input.ID
		})
	}

	frontier := []exactState{{release: newFactBitmap(len(hashes)), influence: newFactBitmap(len(hashes))}}
	for _, requirement := range requirements {
		next := make([]exactState, 0, len(frontier)*(len(groups[requirement])+1))
		for _, current := range frontier {
			next = append(next, current)
			for _, candidateIndex := range groups[requirement] {
				candidate := prepared[candidateIndex]
				release := current.release.clone()
				release.or(candidate.release)
				influence := current.influence.clone()
				influence.or(candidate.influence)
				if release.count() > budgetRelease || influence.count() > budgetInfluence {
					continue
				}
				selected := append(append([]int(nil), current.selected...), candidateIndex)
				next = append(next, exactState{selected: selected, release: release, influence: influence,
					utility: current.utility + effectCandidateUtility(candidate.input, weights), ids: current.ids + "\x00" + candidate.input.ID})
			}
		}
		frontier = exactFrontier(next)
	}
	best := frontier[0]
	for _, state := range frontier[1:] {
		if betterExactState(state, best) {
			best = state
		}
	}

	selected := make([]CandidateSelection, 0, len(best.selected))
	observations := make([]Observation, 0, len(best.selected))
	for _, index := range best.selected {
		candidate := prepared[index]
		selected = append(selected, CandidateSelection{ID: candidate.input.ID, Requirement: candidate.input.Requirement,
			PlanDigest: candidate.input.PlanDigest, EffectDigest: candidate.effectHash,
			AnswerCompleteness: candidate.input.AnswerCompleteness, QueryCoverage: candidate.input.QueryCoverage})
		observations = append(observations, candidate.input.Effect)
	}
	union, err := MergeObservations(ProfileV2, observations...)
	if err != nil {
		return ExactPlan{}, err
	}
	unionDigest, err := ObservationDigest(union)
	if err != nil {
		return ExactPlan{}, err
	}
	return ExactPlan{PlannerVersion: ExactPlannerVersion, Selected: selected, ReleaseCost: best.release.count(),
		InfluenceCost: best.influence.count(), Utility: best.utility, UnionEffect: union, UnionEffectHash: unionDigest}, nil
}

func effectCandidateUtility(candidate EffectCandidate, weights UtilityWeights) float64 {
	total := weights.AnswerCompleteness + weights.QueryCoverage
	return (candidate.AnswerCompleteness*weights.AnswerCompleteness + candidate.QueryCoverage*weights.QueryCoverage) / total
}

func exactFrontier(states []exactState) []exactState {
	result := make([]exactState, 0, len(states))
	for index, candidate := range states {
		dominated := false
		for otherIndex, other := range states {
			if index == otherIndex {
				continue
			}
			if other.release.subsetOf(candidate.release) && other.influence.subsetOf(candidate.influence) && other.utility >= candidate.utility &&
				(other.utility > candidate.utility || !candidate.release.subsetOf(other.release) || !candidate.influence.subsetOf(other.influence) || other.ids < candidate.ids) {
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

func betterExactState(left, right exactState) bool {
	if left.utility != right.utility {
		return left.utility > right.utility
	}
	if left.release.count() != right.release.count() {
		return left.release.count() < right.release.count()
	}
	if left.influence.count() != right.influence.count() {
		return left.influence.count() < right.influence.count()
	}
	return left.ids < right.ids
}

func ObservationDigest(observation Observation) (string, error) {
	normalized, err := observation.Normalize()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
