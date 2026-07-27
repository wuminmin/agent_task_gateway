package exposureeval

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

//go:embed policy_scenarios.json
var policyScenariosJSON []byte

type PolicyCalibrationSummary struct {
	Status         string                  `json:"status"`
	ManifestSHA256 string                  `json:"manifest_sha256"`
	UtilityMetric  string                  `json:"utility_metric"`
	FixtureRows    int                     `json:"fixture_rows"`
	BudgetPercents []int                   `json:"budget_percentages"`
	Scenarios      []PolicyScenarioSummary `json:"scenarios"`
}

type PolicyScenarioSummary struct {
	ID                          string              `json:"id"`
	Label                       string              `json:"label"`
	PolicyContext               string              `json:"policy_context"`
	Goals                       int                 `json:"goals"`
	FullReleaseFacts            int                 `json:"full_release_facts"`
	FullDependencyFacts         int                 `json:"full_dependency_facts"`
	ReleaseBreakdown            FactClassBreakdown  `json:"release_breakdown"`
	DependencyBreakdown         FactClassBreakdown  `json:"dependency_breakdown"`
	AggregateDependencyFacts    int                 `json:"aggregate_dependency_facts"`
	SensitivityWeightedFullCost int                 `json:"sensitivity_weighted_full_cost"`
	Curve                       []PolicyBudgetPoint `json:"budget_utility_curve"`
}

type FactClassBreakdown struct {
	Rows            int `json:"row_facts"`
	OrdinaryFields  int `json:"ordinary_field_facts"`
	SensitiveFields int `json:"sensitive_field_facts"`
	Derived         int `json:"derived_facts"`
}

type PolicyBudgetPoint struct {
	Percent          int      `json:"percent_of_full_dual_budget"`
	ReleaseBudget    int      `json:"release_budget"`
	DependencyBudget int      `json:"dependency_budget"`
	GoalsCompleted   int      `json:"goals_completed"`
	UtilityPercent   int      `json:"utility_percent"`
	SelectedGoals    []string `json:"selected_goals"`
}

type policyManifest struct {
	SchemaVersion  int              `json:"schema_version"`
	Fixture        string           `json:"fixture"`
	UtilityMetric  string           `json:"utility_metric"`
	BudgetPercents []int            `json:"budget_percentages"`
	Scenarios      []policyScenario `json:"scenarios"`
}

type policyScenario struct {
	ID              string       `json:"id"`
	Label           string       `json:"label"`
	PolicyContext   string       `json:"policy_context"`
	SensitiveFields []string     `json:"sensitive_fields"`
	Steps           []policyStep `json:"steps"`
}

type policyStep struct {
	ID        string `json:"id"`
	Operation string `json:"operation"`
	Aggregate bool   `json:"aggregate"`
}

type policyEffect struct {
	step       policyStep
	release    map[string]exposure.FactID
	dependency map[string]exposure.FactID
}

func runPolicyCalibration(profile string, fixtures corpus, relations map[string]exposure.RelationV2) (PolicyCalibrationSummary, error) {
	var manifest policyManifest
	if err := json.Unmarshal(policyScenariosJSON, &manifest); err != nil {
		return PolicyCalibrationSummary{}, fmt.Errorf("policy scenario manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 || manifest.Fixture != "evaluation/exposure/corpus.json" ||
		manifest.UtilityMetric != "fraction of predeclared workflow goals admitted" ||
		len(manifest.Scenarios) != 3 || len(manifest.BudgetPercents) < 4 {
		return PolicyCalibrationSummary{}, fmt.Errorf("policy scenario manifest is incomplete")
	}
	rows := 0
	for _, relation := range fixtures.Relations {
		rows += len(relation.Rows)
	}
	result := PolicyCalibrationSummary{
		Status:         "complete_deterministic_policy_calibration",
		ManifestSHA256: fmt.Sprintf("%x", sha256.Sum256(policyScenariosJSON)),
		UtilityMetric:  manifest.UtilityMetric,
		FixtureRows:    rows,
		BudgetPercents: append([]int(nil), manifest.BudgetPercents...),
	}
	seenScenarios := make(map[string]struct{}, len(manifest.Scenarios))
	for _, scenario := range manifest.Scenarios {
		if strings.TrimSpace(scenario.ID) == "" || strings.TrimSpace(scenario.Label) == "" ||
			strings.TrimSpace(scenario.PolicyContext) == "" || len(scenario.Steps) < 3 {
			return PolicyCalibrationSummary{}, fmt.Errorf("policy scenario is incomplete")
		}
		if _, duplicate := seenScenarios[scenario.ID]; duplicate {
			return PolicyCalibrationSummary{}, fmt.Errorf("duplicate policy scenario %q", scenario.ID)
		}
		seenScenarios[scenario.ID] = struct{}{}
		sensitive := make(map[string]struct{}, len(scenario.SensitiveFields))
		for _, field := range scenario.SensitiveFields {
			sensitive[field] = struct{}{}
		}
		effects := make([]policyEffect, 0, len(scenario.Steps))
		seenSteps := make(map[string]struct{}, len(scenario.Steps))
		for _, step := range scenario.Steps {
			if strings.TrimSpace(step.ID) == "" {
				return PolicyCalibrationSummary{}, fmt.Errorf("policy scenario %s has an empty goal", scenario.ID)
			}
			if _, duplicate := seenSteps[step.ID]; duplicate {
				return PolicyCalibrationSummary{}, fmt.Errorf("policy scenario %s repeats goal %s", scenario.ID, step.ID)
			}
			seenSteps[step.ID] = struct{}{}
			observation, _, err := evaluateOperation(profile, relations, step.Operation)
			if err != nil {
				return PolicyCalibrationSummary{}, fmt.Errorf("policy scenario %s goal %s: %w", scenario.ID, step.ID, err)
			}
			release, err := factsByHash(observation.Release)
			if err != nil {
				return PolicyCalibrationSummary{}, err
			}
			dependency, err := factsByHash(observation.Influence)
			if err != nil {
				return PolicyCalibrationSummary{}, err
			}
			effects = append(effects, policyEffect{step: step, release: release, dependency: dependency})
		}
		fullRelease, fullDependency := unionEffects(effects, (1<<len(effects))-1)
		aggregateDependency := make(map[string]exposure.FactID)
		for _, effect := range effects {
			if effect.step.Aggregate {
				mergeFacts(aggregateDependency, effect.dependency)
			}
		}
		summary := PolicyScenarioSummary{
			ID:                       scenario.ID,
			Label:                    scenario.Label,
			PolicyContext:            scenario.PolicyContext,
			Goals:                    len(effects),
			FullReleaseFacts:         len(fullRelease),
			FullDependencyFacts:      len(fullDependency),
			ReleaseBreakdown:         classifyFacts(fullRelease, sensitive),
			DependencyBreakdown:      classifyFacts(fullDependency, sensitive),
			AggregateDependencyFacts: len(aggregateDependency),
		}
		// The weighted number is a sensitivity-analysis contrast only: ordinary
		// facts have weight one and scenario-tagged sensitive cells weight five.
		// It is deliberately not used by TaskGate admission.
		summary.SensitivityWeightedFullCost = len(fullRelease) + len(fullDependency) +
			4*(summary.ReleaseBreakdown.SensitiveFields+summary.DependencyBreakdown.SensitiveFields)
		for _, percent := range manifest.BudgetPercents {
			if percent <= 0 || percent > 100 {
				return PolicyCalibrationSummary{}, fmt.Errorf("invalid policy budget percentage %d", percent)
			}
			releaseBudget := int(math.Ceil(float64(len(fullRelease)*percent) / 100.0))
			dependencyBudget := int(math.Ceil(float64(len(fullDependency)*percent) / 100.0))
			mask := bestPolicySubset(effects, releaseBudget, dependencyBudget)
			selected := make([]string, 0, len(effects))
			for index, effect := range effects {
				if mask&(1<<index) != 0 {
					selected = append(selected, effect.step.ID)
				}
			}
			sort.Strings(selected)
			summary.Curve = append(summary.Curve, PolicyBudgetPoint{
				Percent: percent, ReleaseBudget: releaseBudget, DependencyBudget: dependencyBudget,
				GoalsCompleted: len(selected), UtilityPercent: 100 * len(selected) / len(effects), SelectedGoals: selected,
			})
		}
		result.Scenarios = append(result.Scenarios, summary)
	}
	return result, nil
}

func factsByHash(facts []exposure.FactID) (map[string]exposure.FactID, error) {
	result := make(map[string]exposure.FactID, len(facts))
	for _, fact := range facts {
		hash, err := fact.Hash()
		if err != nil {
			return nil, fmt.Errorf("policy fact hash: %w", err)
		}
		result[hash] = fact
	}
	return result, nil
}

func mergeFacts(target, source map[string]exposure.FactID) {
	for hash, fact := range source {
		target[hash] = fact
	}
}

func unionEffects(effects []policyEffect, mask int) (map[string]exposure.FactID, map[string]exposure.FactID) {
	release := make(map[string]exposure.FactID)
	dependency := make(map[string]exposure.FactID)
	for index, effect := range effects {
		if mask&(1<<index) == 0 {
			continue
		}
		mergeFacts(release, effect.release)
		mergeFacts(dependency, effect.dependency)
	}
	return release, dependency
}

func bestPolicySubset(effects []policyEffect, releaseBudget, dependencyBudget int) int {
	bestMask := 0
	bestGoals := -1
	bestFacts := int(^uint(0) >> 1)
	for mask := 0; mask < 1<<len(effects); mask++ {
		release, dependency := unionEffects(effects, mask)
		if len(release) > releaseBudget || len(dependency) > dependencyBudget {
			continue
		}
		goals := 0
		for current := mask; current != 0; current >>= 1 {
			goals += current & 1
		}
		facts := len(release) + len(dependency)
		if goals > bestGoals || (goals == bestGoals && facts < bestFacts) ||
			(goals == bestGoals && facts == bestFacts && mask < bestMask) {
			bestMask, bestGoals, bestFacts = mask, goals, facts
		}
	}
	return bestMask
}

func classifyFacts(facts map[string]exposure.FactID, sensitive map[string]struct{}) FactClassBreakdown {
	var result FactClassBreakdown
	for _, fact := range facts {
		switch fact.Kind {
		case exposure.FactBaseRow:
			result.Rows++
		case exposure.FactBaseCell:
			if _, ok := sensitive[fact.Field]; ok {
				result.SensitiveFields++
			} else {
				result.OrdinaryFields++
			}
		case exposure.FactDerived:
			result.Derived++
		}
	}
	return result
}
