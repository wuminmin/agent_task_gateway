// Package agenttasks replays deterministic database-agent task traces and
// scores released answer assertions against gold answers independently of the
// planner's utility calculation.
package agenttasks

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

//go:embed corpus.json
var corpusJSON []byte

type corpus struct {
	SchemaVersion       int      `json:"schema_version"`
	Seed                int64    `json:"seed"`
	RequirementsPerTask int      `json:"requirements_per_task"`
	Objectives          []string `json:"objectives"`
	BudgetProfiles      []string `json:"budget_profiles"`
}

type candidate struct {
	Input        exposure.EffectCandidate
	AnswerTokens []string
}

type task struct {
	ID              string
	Objective       string
	Profile         string
	ReleaseBudget   int64
	InfluenceBudget int64
	History         exposure.Observation
	Gold            map[string][]string
	Candidates      []candidate
}

type PolicyResult struct {
	Policy                 string  `json:"policy"`
	Tasks                  int     `json:"tasks"`
	TaskSuccesses          int     `json:"task_successes"`
	TaskSuccessRate        float64 `json:"task_success_rate"`
	MeanAnswerCompleteness float64 `json:"mean_answer_completeness"`
	BudgetViolations       int     `json:"budget_violations"`
}

type ProfileResult struct {
	Profile         string `json:"profile"`
	Tasks           int    `json:"tasks"`
	ExactSuccesses  int    `json:"exact_successes"`
	GreedySuccesses int    `json:"greedy_successes"`
}

type Report struct {
	SchemaVersion       int             `json:"schema_version"`
	Status              string          `json:"status"`
	CorpusSHA256        string          `json:"corpus_sha256"`
	Seed                int64           `json:"seed"`
	Objectives          int             `json:"objectives"`
	BudgetProfiles      int             `json:"budget_profiles"`
	Tasks               int             `json:"tasks"`
	RequirementsPerTask int             `json:"requirements_per_task"`
	CandidatesPerTask   int             `json:"candidates_per_task"`
	Scorer              string          `json:"scorer"`
	Policies            []PolicyResult  `json:"policies"`
	Profiles            []ProfileResult `json:"profiles"`
}

type scored struct {
	Completeness float64
	Success      bool
	Violation    bool
}

// Run evaluates the complete objective x budget-profile matrix. The external
// scorer sees only selected answer-token payloads and gold tokens; it never
// reads the utility values used by OptimizeEffects.
func Run() (Report, error) {
	var spec corpus
	if err := json.Unmarshal(corpusJSON, &spec); err != nil {
		return Report{}, err
	}
	if spec.SchemaVersion != 1 || spec.Seed != 20260725 || spec.RequirementsPerTask != 4 ||
		len(spec.Objectives) < 20 || len(spec.BudgetProfiles) < 5 {
		return Report{}, fmt.Errorf("invalid agent-task corpus")
	}
	tasks := make([]task, 0, len(spec.Objectives)*len(spec.BudgetProfiles))
	for objectiveIndex, objective := range spec.Objectives {
		for _, profile := range spec.BudgetProfiles {
			one, err := buildTask(spec, objectiveIndex, objective, profile)
			if err != nil {
				return Report{}, err
			}
			tasks = append(tasks, one)
		}
	}

	policies := []string{"taskgate_exact", "utility_greedy", "taskgate_exact_no_history"}
	results := make(map[string]*PolicyResult, len(policies))
	for _, policy := range policies {
		results[policy] = &PolicyResult{Policy: policy, Tasks: len(tasks)}
	}
	profileResults := make(map[string]*ProfileResult, len(spec.BudgetProfiles))
	for _, profile := range spec.BudgetProfiles {
		profileResults[profile] = &ProfileResult{Profile: profile}
	}
	for _, one := range tasks {
		perProfile := profileResults[one.Profile]
		perProfile.Tasks++
		for _, policy := range policies {
			selected, err := selectCandidates(one, policy)
			if err != nil {
				return Report{}, fmt.Errorf("task %s policy %s: %w", one.ID, policy, err)
			}
			value, err := scoreTask(one, selected, policy != "taskgate_exact_no_history")
			if err != nil {
				return Report{}, err
			}
			result := results[policy]
			result.MeanAnswerCompleteness += value.Completeness
			if value.Success {
				result.TaskSuccesses++
			}
			if value.Violation {
				result.BudgetViolations++
			}
			if policy == "taskgate_exact" && value.Success {
				perProfile.ExactSuccesses++
			}
			if policy == "utility_greedy" && value.Success {
				perProfile.GreedySuccesses++
			}
		}
	}

	report := Report{SchemaVersion: 1, Status: "complete",
		CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(corpusJSON)), Seed: spec.Seed,
		Objectives: len(spec.Objectives), BudgetProfiles: len(spec.BudgetProfiles), Tasks: len(tasks),
		RequirementsPerTask: spec.RequirementsPerTask, CandidatesPerTask: spec.RequirementsPerTask * 2,
		Scorer: "gold-answer-token-recall-with-per-requirement-threshold-v1"}
	for _, policy := range policies {
		result := results[policy]
		result.TaskSuccessRate = float64(result.TaskSuccesses) / float64(result.Tasks)
		result.MeanAnswerCompleteness /= float64(result.Tasks)
		report.Policies = append(report.Policies, *result)
	}
	for _, profile := range spec.BudgetProfiles {
		report.Profiles = append(report.Profiles, *profileResults[profile])
	}
	return report, nil
}

func buildTask(spec corpus, objectiveIndex int, objective, profile string) (task, error) {
	one := task{ID: fmt.Sprintf("agent-task-%02d-%s", objectiveIndex+1, profile), Objective: objective,
		Profile: profile, History: exposure.Observation{ProfileVersion: exposure.ProfileV2}, Gold: make(map[string][]string)}
	for requirementIndex := 0; requirementIndex < spec.RequirementsPerTask; requirementIndex++ {
		requirement := fmt.Sprintf("r%d", requirementIndex)
		one.Gold[requirement] = []string{requirement + ":a", requirement + ":b", requirement + ":c"}
		concise, err := makeCandidate(one.ID, requirement, "concise", 2, 3, one.Gold[requirement][:2])
		if err != nil {
			return task{}, err
		}
		full, err := makeCandidate(one.ID, requirement, "full", 7, 8, one.Gold[requirement])
		if err != nil {
			return task{}, err
		}
		one.Candidates = append(one.Candidates, full, concise)
	}
	switch profile {
	case "balanced":
		one.ReleaseBudget, one.InfluenceBudget = 9, 13
	case "release_tight":
		one.ReleaseBudget, one.InfluenceBudget = 7, 13
	case "influence_tight":
		one.ReleaseBudget, one.InfluenceBudget = 9, 10
	case "ample":
		one.ReleaseBudget, one.InfluenceBudget = 29, 33
	case "history_overlap":
		one.ReleaseBudget, one.InfluenceBudget = 8, 12
		historyRelease := []exposure.FactID{mustFact(one.ID, "release", "shared", 0)}
		historyInfluence := []exposure.FactID{mustFact(one.ID, "influence", "shared", 0)}
		for index := 2; index < 7; index++ {
			historyRelease = append(historyRelease, mustFact(one.ID, "release", "r0", index))
		}
		for index := 3; index < 8; index++ {
			historyInfluence = append(historyInfluence, mustFact(one.ID, "influence", "r0", index))
		}
		one.History.Release, one.History.Influence = historyRelease, historyInfluence
	default:
		return task{}, fmt.Errorf("unknown budget profile %q", profile)
	}
	return one, nil
}

func makeCandidate(taskID, requirement, representation string, releaseCount, influenceCount int, answer []string) (candidate, error) {
	release := []exposure.FactID{mustFact(taskID, "release", "shared", 0)}
	influence := []exposure.FactID{mustFact(taskID, "influence", "shared", 0)}
	for index := 0; index < releaseCount; index++ {
		release = append(release, mustFact(taskID, "release", requirement, index))
	}
	for index := 0; index < influenceCount; index++ {
		influence = append(influence, mustFact(taskID, "influence", requirement, index))
	}
	return candidate{Input: exposure.EffectCandidate{ID: requirement + "-" + representation, Requirement: requirement,
		AnswerCompleteness: float64(len(answer)) / 3,
		Effect:             exposure.Observation{ProfileVersion: exposure.ProfileV2, Release: release, Influence: influence}},
		AnswerTokens: append([]string(nil), answer...)}, nil
}

func mustFact(taskID, family, requirement string, index int) exposure.FactID {
	value := fmt.Sprintf("%s/%s/%s/%d", taskID, family, requirement, index)
	fact, err := exposure.NewBaseCellFactV2("agenttasks.synthetic."+family, "snapshot-v1", value, "value", "text", value)
	if err != nil {
		panic(err)
	}
	return fact
}

func selectCandidates(one task, policy string) ([]string, error) {
	history := one.History
	if policy == "taskgate_exact_no_history" {
		history = exposure.Observation{ProfileVersion: exposure.ProfileV2}
	}
	if policy == "taskgate_exact" || policy == "taskgate_exact_no_history" {
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
	}
	if policy != "utility_greedy" {
		return nil, fmt.Errorf("unknown policy %q", policy)
	}
	ordered := append([]candidate(nil), one.Candidates...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Input.AnswerCompleteness != ordered[j].Input.AnswerCompleteness {
			return ordered[i].Input.AnswerCompleteness > ordered[j].Input.AnswerCompleteness
		}
		return ordered[i].Input.ID < ordered[j].Input.ID
	})
	selectedRequirements := make(map[string]struct{})
	var selected []string
	for _, value := range ordered {
		if _, exists := selectedRequirements[value.Input.Requirement]; exists {
			continue
		}
		trial := append(append([]string(nil), selected...), value.Input.ID)
		release, influence, err := novelty(one, trial, true)
		if err != nil {
			return nil, err
		}
		if release <= one.ReleaseBudget && influence <= one.InfluenceBudget {
			selected = trial
			selectedRequirements[value.Input.Requirement] = struct{}{}
		}
	}
	return selected, nil
}

func scoreTask(one task, selected []string, useHistory bool) (scored, error) {
	answers := make(map[string]struct{})
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	for _, value := range one.Candidates {
		if _, ok := selectedSet[value.Input.ID]; !ok {
			continue
		}
		for _, token := range value.AnswerTokens {
			answers[token] = struct{}{}
		}
	}
	total, matched := 0, 0
	success := true
	for _, gold := range one.Gold {
		total += len(gold)
		perRequirement := 0
		for _, token := range gold {
			if _, ok := answers[token]; ok {
				matched++
				perRequirement++
			}
		}
		if perRequirement < 2 {
			success = false
		}
	}
	release, influence, err := novelty(one, selected, useHistory)
	if err != nil {
		return scored{}, err
	}
	return scored{Completeness: float64(matched) / float64(total), Success: success,
		Violation: release > one.ReleaseBudget || influence > one.InfluenceBudget}, nil
}

func novelty(one task, selected []string, useHistory bool) (int64, int64, error) {
	release, influence := make(exposure.FactSet), make(exposure.FactSet)
	selectedSet := make(map[string]struct{}, len(selected))
	for _, id := range selected {
		selectedSet[id] = struct{}{}
	}
	for _, value := range one.Candidates {
		if _, ok := selectedSet[value.Input.ID]; !ok {
			continue
		}
		currentRelease, err := exposure.NewFactSet(value.Input.Effect.Release...)
		if err != nil {
			return 0, 0, err
		}
		currentInfluence, err := exposure.NewFactSet(value.Input.Effect.Influence...)
		if err != nil {
			return 0, 0, err
		}
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
	return int64(len(release)), int64(len(influence)), nil
}
