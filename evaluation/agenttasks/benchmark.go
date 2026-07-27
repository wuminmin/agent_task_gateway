// Package agenttasks replays deterministic, non-isomorphic database-agent task
// traces and scores released answer assertions against gold answers that are
// INDEPENDENT of the planner's utility signal.
//
// Earlier versions generated 120 traces that were all structurally isomorphic,
// and the planner utility was identical to gold-token coverage, so the external
// scorer was a tautology of the utility. This package fixes both:
//
//  1. Tasks are non-isomorphic. Each objective is expanded from a kind spec
//     (aggregate_summary, comparison, anomaly, coverage_constrained, delegated)
//     with varying requirement counts, candidate menus, and budget regimes.
//  2. Gold is decoupled from utility. Each candidate has a query-coverage q
//     (what the planner maximizes) and a gold-reveal count (what the scorer
//     measures). The "broad" class has q = 1.0 but reveals only 2 of 3 gold
//     tokens, so maximizing q is not the same as maximizing gold recall.
//  3. Cost overlap is real. Candidates share a pool of facts, so the exact
//     overlap-aware planner can fit representation sets the additive scalar
//     proxy rejects; that is the constructive separation.
package agenttasks

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

//go:embed corpus.json
var corpusJSON []byte

type candidateClass struct {
	GoldReveal        int     `json:"gold_reveal"`
	QueryCoverage     float64 `json:"query_coverage"`
	SharedFacts       int     `json:"shared_facts"`
	RequirementFacts  int     `json:"requirement_facts"`
}

type kindSpec struct {
	Kind            string   `json:"kind"`
	Count           int      `json:"count"`
	Requirements    []int    `json:"requirements"`
	Classes         []string `json:"classes"`
	ReleaseBudget   []int    `json:"release_budget"`
	InfluenceBudget []int    `json:"influence_budget"`
	History         string   `json:"history,omitempty"`
}

type corpusSpec struct {
	SchemaVersion          int                       `json:"schema_version"`
	Seed                   int64                     `json:"seed"`
	GoldTokensPerReq       int                       `json:"gold_tokens_per_requirement"`
	SuccessThreshold       int                       `json:"success_threshold"`
	SharedFactPool         int                       `json:"shared_fact_pool"`
	PerRequirementFacts    int                       `json:"per_requirement_facts"`
	Classes                map[string]candidateClass `json:"candidate_classes"`
	Kinds                  []kindSpec                `json:"kinds"`
}

type candidate struct {
	Input        exposure.EffectCandidate
	AnswerTokens []string // gold tokens this candidate reveals (used only by the scorer)
}

type task struct {
	ID              string
	Kind            string
	ReleaseBudget   int64
	InfluenceBudget int64
	History         exposure.Observation
	Gold            map[string][]string // requirement -> ground-truth gold tokens (independent of candidates)
	Candidates      []candidate
}

type objectiveInstance struct {
	Kind            string
	Requirements    int
	Classes         []string
	ReleaseBudget   int64
	InfluenceBudget int64
	History         string
}

type PolicyResult struct {
	Policy                 string  `json:"policy"`
	Tasks                  int     `json:"tasks"`
	TaskSuccesses          int     `json:"task_successes"`
	TaskSuccessRate        float64 `json:"task_success_rate"`
	MeanAnswerCompleteness float64 `json:"mean_answer_completeness"`
	BudgetViolations       int     `json:"budget_violations"`
}

type KindResult struct {
	Kind            string         `json:"kind"`
	Tasks           int            `json:"tasks"`
	ExactSuccesses  int            `json:"exact_successes"`
	GreedySuccesses int            `json:"greedy_successes"`
	AdditiveSuccess map[string]int `json:"additive_successes,omitempty"`
}

type Report struct {
	SchemaVersion       int           `json:"schema_version"`
	Status              string        `json:"status"`
	CorpusSHA256        string        `json:"corpus_sha256"`
	Seed                int64         `json:"seed"`
	Objectives          int           `json:"objectives"`
	Kinds               int           `json:"kinds"`
	Tasks               int           `json:"tasks"`
	GoldTokensPerReq    int           `json:"gold_tokens_per_requirement"`
	SuccessThreshold    int           `json:"success_threshold"`
	Scorer              string        `json:"scorer"`
	UtilitySignal       string        `json:"utility_signal"`
	Policies            []PolicyResult `json:"policies"`
	Results             []KindResult   `json:"results"`
}

type scored struct {
	Completeness float64
	Success      bool
	Violation    bool
}

// Run evaluates the full non-isomorphic objective set under every policy. The
// scorer sees only the gold tokens each selected candidate reveals; it never
// reads the query-coverage values the planner optimizes.
func Run() (Report, error) {
	var spec corpusSpec
	if err := json.Unmarshal(corpusJSON, &spec); err != nil {
		return Report{}, err
	}
	if err := validateCorpus(spec); err != nil {
		return Report{}, err
	}
	objectives := generateObjectives(spec)
	tasks := make([]task, 0, len(objectives))
	for index, objective := range objectives {
		one, err := buildTask(spec, objective, index)
		if err != nil {
			return Report{}, err
		}
		tasks = append(tasks, one)
	}

	policies := []string{"taskgate_exact", "utility_greedy", "additive_cost",
		"taskgate_exact_no_history", "random_first", "single_candidate"}
	results := make(map[string]*PolicyResult, len(policies))
	for _, policy := range policies {
		results[policy] = &PolicyResult{Policy: policy, Tasks: len(tasks)}
	}
	kindResults := make(map[string]*KindResult)
	for _, objective := range objectives {
		if _, ok := kindResults[objective.Kind]; !ok {
			kindResults[objective.Kind] = &KindResult{Kind: objective.Kind, AdditiveSuccess: make(map[string]int)}
		}
	}
	random := rand.New(rand.NewSource(spec.Seed))

	for _, one := range tasks {
		perKind := kindResults[one.Kind]
		perKind.Tasks++
		for _, policy := range policies {
			selected, err := selectCandidates(one, policy, random)
			if err != nil {
				return Report{}, fmt.Errorf("task %s policy %s: %w", one.ID, policy, err)
			}
			value := scoreTask(one, selected)
			result := results[policy]
			result.MeanAnswerCompleteness += value.Completeness
			if value.Success {
				result.TaskSuccesses++
			}
			if value.Violation {
				result.BudgetViolations++
			}
			if policy == "taskgate_exact" && value.Success {
				perKind.ExactSuccesses++
			}
			if policy == "utility_greedy" && value.Success {
				perKind.GreedySuccesses++
			}
			if policy == "additive_cost" && value.Success {
				perKind.AdditiveSuccess[one.Kind]++
			}
		}
	}

	report := Report{SchemaVersion: 2, Status: "complete",
		CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(corpusJSON)), Seed: spec.Seed,
		Objectives: len(objectives), Kinds: len(kindResults), Tasks: len(tasks),
		GoldTokensPerReq: spec.GoldTokensPerReq, SuccessThreshold: spec.SuccessThreshold,
		Scorer:      "independent-gold-token-recall-with-per-requirement-threshold-v2",
		UtilitySignal: "query-coverage-decoupled-from-gold-reveal"}
	for _, policy := range policies {
		result := results[policy]
		result.TaskSuccessRate = float64(result.TaskSuccesses) / float64(result.Tasks)
		result.MeanAnswerCompleteness /= float64(result.Tasks)
		report.Policies = append(report.Policies, *result)
	}
	kinds := make([]string, 0, len(kindResults))
	for kind := range kindResults {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		report.Results = append(report.Results, *kindResults[kind])
	}
	return report, nil
}

func validateCorpus(spec corpusSpec) error {
	if spec.SchemaVersion != 2 || spec.Seed != 20260725 || spec.GoldTokensPerReq != 3 ||
		spec.SuccessThreshold != 2 || spec.SharedFactPool != 4 || len(spec.Classes) == 0 || len(spec.Kinds) == 0 {
		return fmt.Errorf("invalid agent-task corpus")
	}
	for _, kind := range spec.Kinds {
		if kind.Count <= 0 || len(kind.Requirements) == 0 || len(kind.Classes) == 0 ||
			len(kind.ReleaseBudget) == 0 || len(kind.InfluenceBudget) == 0 {
			return fmt.Errorf("kind %s is incomplete", kind.Kind)
		}
		for _, class := range kind.Classes {
			if _, ok := spec.Classes[class]; !ok {
				return fmt.Errorf("kind %s references unknown class %q", kind.Kind, class)
			}
		}
	}
	return nil
}

// generateObjectives expands each kind into Count non-isomorphic objective
// instances, deterministically choosing the requirement count and budget regime
// from the seed so the corpus is reproducible without hand-listing every case.
func generateObjectives(spec corpusSpec) []objectiveInstance {
	random := rand.New(rand.NewSource(spec.Seed))
	pick := func(options []int) int {
		return options[random.Intn(len(options))]
	}
	pickI64 := func(options []int) int64 {
		return int64(options[random.Intn(len(options))])
	}
	var objectives []objectiveInstance
	for _, kind := range spec.Kinds {
		for i := 0; i < kind.Count; i++ {
			objectives = append(objectives, objectiveInstance{
				Kind:            kind.Kind,
				Requirements:    pick(kind.Requirements),
				Classes:         append([]string(nil), kind.Classes...),
				ReleaseBudget:   pickI64(kind.ReleaseBudget),
				InfluenceBudget: pickI64(kind.InfluenceBudget),
				History:         kind.History,
			})
		}
	}
	return objectives
}

func buildTask(spec corpusSpec, objective objectiveInstance, index int) (task, error) {
	id := fmt.Sprintf("agent-task-%02d-%s", index+1, objective.Kind)
	one := task{ID: id, Kind: objective.Kind, ReleaseBudget: objective.ReleaseBudget,
		InfluenceBudget: objective.InfluenceBudget,
		History: exposure.Observation{ProfileVersion: exposure.ProfileV2}, Gold: make(map[string][]string)}
	for r := 0; r < objective.Requirements; r++ {
		requirement := fmt.Sprintf("r%d", r)
		gold := make([]string, spec.GoldTokensPerReq)
		for g := 0; g < spec.GoldTokensPerReq; g++ {
			gold[g] = fmt.Sprintf("%s:g%d", requirement, g)
		}
		one.Gold[requirement] = gold
		for _, classID := range objective.Classes {
			class := spec.Classes[classID]
			candidate, err := makeCandidate(one.ID, requirement, classID, class, spec, gold)
			if err != nil {
				return task{}, err
			}
			one.Candidates = append(one.Candidates, candidate)
		}
	}
	if objective.History == "overlap" {
		// A delegated task family has already accounted the shared facts plus one
		// requirement's facts in a prior task; history makes those replay free.
		one.History.Release = append(one.History.Release, sharedFacts(one.ID, "release", spec.SharedFactPool)...)
		one.History.Influence = append(one.History.Influence, sharedFacts(one.ID, "influence", spec.SharedFactPool)...)
		one.History.Release = append(one.History.Release, reqFacts(one.ID, "release", "r0", 2)...)
		one.History.Influence = append(one.History.Influence, reqFacts(one.ID, "influence", "r0", 2)...)
	}
	return one, nil
}

func makeCandidate(taskID, requirement, classID string, class candidateClass, spec corpusSpec, gold []string) (candidate, error) {
	release := append([]exposure.FactID(nil), sharedFacts(taskID, "release", class.SharedFacts)...)
	release = append(release, reqFacts(taskID, "release", requirement, class.RequirementFacts)...)
	influence := append([]exposure.FactID(nil), sharedFacts(taskID, "influence", class.SharedFacts)...)
	influence = append(influence, reqFacts(taskID, "influence", requirement, class.RequirementFacts)...)
	reveal := class.GoldReveal
	if reveal > len(gold) {
		reveal = len(gold)
	}
	return candidate{
		Input: exposure.EffectCandidate{ID: requirement + ":" + classID, Requirement: requirement,
			AnswerCompleteness: class.QueryCoverage,
			Effect:             exposure.Observation{ProfileVersion: exposure.ProfileV2, Release: release, Influence: influence}},
		AnswerTokens: append([]string(nil), gold[:reveal]...),
	}, nil
}

func sharedFacts(taskID, family string, count int) []exposure.FactID {
	result := make([]exposure.FactID, 0, count)
	for i := 0; i < count; i++ {
		result = append(result, mustFact(taskID, family, "shared", i))
	}
	return result
}

func reqFacts(taskID, family, requirement string, count int) []exposure.FactID {
	result := make([]exposure.FactID, 0, count)
	for i := 0; i < count; i++ {
		result = append(result, mustFact(taskID, family, requirement, i))
	}
	return result
}

func mustFact(taskID, family, requirement string, index int) exposure.FactID {
	value := fmt.Sprintf("%s/%s/%s/%d", taskID, family, requirement, index)
	fact, err := exposure.NewBaseCellFactV2("agenttasks.synthetic."+family, "snapshot-v1", value, "value", "text", value)
	if err != nil {
		panic(err)
	}
	return fact
}

// selectCandidates applies a policy and returns the chosen candidate IDs.
func selectCandidates(one task, policy string, random *rand.Rand) ([]string, error) {
	switch policy {
	case "taskgate_exact", "taskgate_exact_no_history":
		history := one.History
		if policy == "taskgate_exact_no_history" {
			history = exposure.Observation{ProfileVersion: exposure.ProfileV2}
		}
		inputs := make([]exposure.EffectCandidate, 0, len(one.Candidates))
		for _, value := range one.Candidates {
			inputs = append(inputs, value.Input)
		}
		plan, err := exposure.OptimizeEffects(inputs, history, one.ReleaseBudget, one.InfluenceBudget,
			exposure.UtilityWeights{AnswerCompleteness: 1})
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(plan.Selected))
		for _, value := range plan.Selected {
			ids = append(ids, value.ID)
		}
		return ids, nil
	case "utility_greedy":
		return greedyByUtility(one, true), nil
	case "additive_cost":
		return additiveCostProxy(one), nil
	case "random_first":
		return randomFirst(one, random), nil
	case "single_candidate":
		return singleCandidate(one), nil
	default:
		return nil, fmt.Errorf("unknown policy %q", policy)
	}
}

// greedyByUtility picks candidates by descending query coverage, accepting one
// per requirement only if the exact history-aware union still fits the budget.
// It is overlap-aware in cost but myopic in selection order (no backtracking).
func greedyByUtility(one task, useHistory bool) []string {
	ordered := append([]candidate(nil), one.Candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Input.AnswerCompleteness != ordered[j].Input.AnswerCompleteness {
			return ordered[i].Input.AnswerCompleteness > ordered[j].Input.AnswerCompleteness
		}
		return ordered[i].Input.ID < ordered[j].Input.ID
	})
	chosen := make(map[string]struct{})
	var selected []string
	for _, value := range ordered {
		if _, exists := chosen[value.Input.Requirement]; exists {
			continue
		}
		trial := append(append([]string(nil), selected...), value.Input.ID)
		release, influence := exactNovelty(one, trial, useHistory)
		if release <= one.ReleaseBudget && influence <= one.InfluenceBudget {
			selected = trial
			chosen[value.Input.Requirement] = struct{}{}
		}
	}
	return selected
}

// additiveCostProxy is the scalar strawman: one budget B = release+influence,
// cost(c) = |release(c)| + |influence(c)|. It additive-counts shared facts, so
// it rejects overlapping sets the exact planner accepts and may breach the true
// dual budget.
func additiveCostProxy(one task) []string {
	budget := one.ReleaseBudget + one.InfluenceBudget
	ordered := append([]candidate(nil), one.Candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		costI := float64(len(ordered[i].Input.Effect.Release)) + float64(len(ordered[i].Input.Effect.Influence))
		costJ := float64(len(ordered[j].Input.Effect.Release)) + float64(len(ordered[j].Input.Effect.Influence))
		utiI := ordered[i].Input.AnswerCompleteness / costI
		utiJ := ordered[j].Input.AnswerCompleteness / costJ
		if utiI != utiJ {
			return utiI > utiJ
		}
		return ordered[i].Input.ID < ordered[j].Input.ID
	})
	chosen := make(map[string]struct{})
	var selected []string
	running := int64(0)
	for _, value := range ordered {
		if _, exists := chosen[value.Input.Requirement]; exists {
			continue
		}
		cost := int64(len(value.Input.Effect.Release)) + int64(len(value.Input.Effect.Influence))
		if running+cost <= budget {
			selected = append(selected, value.Input.ID)
			chosen[value.Input.Requirement] = struct{}{}
			running += cost
		}
	}
	return selected
}

// randomFirst selects candidates in a deterministic random order, accepting one
// per requirement when the exact union fits. It is a non-strategic baseline.
func randomFirst(one task, random *rand.Rand) []string {
	ordered := append([]candidate(nil), one.Candidates...)
	random.Shuffle(len(ordered), func(i, j int) { ordered[i], ordered[j] = ordered[j], ordered[i] })
	chosen := make(map[string]struct{})
	var selected []string
	for _, value := range ordered {
		if _, exists := chosen[value.Input.Requirement]; exists {
			continue
		}
		trial := append(append([]string(nil), selected...), value.Input.ID)
		release, influence := exactNovelty(one, trial, true)
		if release <= one.ReleaseBudget && influence <= one.InfluenceBudget {
			selected = trial
			chosen[value.Input.Requirement] = struct{}{}
		}
	}
	return selected
}

// singleCandidate picks only the single highest-coverage candidate, so at most
// one requirement can be satisfied. It is a degenerate lower-bound baseline.
func singleCandidate(one task) []string {
	ordered := append([]candidate(nil), one.Candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Input.AnswerCompleteness != ordered[j].Input.AnswerCompleteness {
			return ordered[i].Input.AnswerCompleteness > ordered[j].Input.AnswerCompleteness
		}
		return ordered[i].Input.ID < ordered[j].Input.ID
	})
	if len(ordered) == 0 {
		return nil
	}
	return []string{ordered[0].Input.ID}
}

// scoreTask is the INDEPENDENT external scorer. It sees only the gold tokens
// each selected candidate reveals and the per-requirement threshold; it never
// reads the query-coverage values the planner optimized.
func scoreTask(one task, selected []string) scored {
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	revealed := make(map[string]struct{})
	for _, value := range one.Candidates {
		if _, ok := selectedSet[value.Input.ID]; !ok {
			continue
		}
		for _, token := range value.AnswerTokens {
			revealed[token] = struct{}{}
		}
	}
	total, matched := 0, 0
	success := true
	coveredRequirements := make(map[string]struct{}, len(selected))
	for _, value := range one.Candidates {
		if _, ok := selectedSet[value.Input.ID]; ok {
			coveredRequirements[value.Input.Requirement] = struct{}{}
		}
	}
	for requirement, gold := range one.Gold {
		total += len(gold)
		perRequirement := 0
		for _, token := range gold {
			if _, ok := revealed[token]; ok {
				matched++
				perRequirement++
			}
		}
		_, covered := coveredRequirements[requirement]
		if !covered || perRequirement < 2 {
			success = false
		}
	}
	release, influence := exactNovelty(one, selected, true)
	return scored{Completeness: float64(matched) / float64(total), Success: success,
		Violation: release > one.ReleaseBudget || influence > one.InfluenceBudget}
}

// exactNovelty computes the history-aware release/influence cardinality of a
// selected candidate set using TaskGate's FactSet (set union, then subtract
// history), exactly as settlement would charge.
func exactNovelty(one task, selected []string, useHistory bool) (int64, int64) {
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	release, influence := make(exposure.FactSet), make(exposure.FactSet)
	for _, value := range one.Candidates {
		if _, ok := selectedSet[value.Input.ID]; !ok {
			continue
		}
		currentRelease, _ := exposure.NewFactSet(value.Input.Effect.Release...)
		currentInfluence, _ := exposure.NewFactSet(value.Input.Effect.Influence...)
		release.Merge(currentRelease)
		influence.Merge(currentInfluence)
	}
	if useHistory {
		historyRelease, _ := exposure.NewFactSet(one.History.Release...)
		historyInfluence, _ := exposure.NewFactSet(one.History.Influence...)
		for hash := range historyRelease {
			delete(release, hash)
		}
		for hash := range historyInfluence {
			delete(influence, hash)
		}
	}
	return int64(len(release)), int64(len(influence))
}
