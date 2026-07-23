// Package exposureeval provides deterministic, source-controlled experiments
// for TaskGate's exposure semantics. It deliberately does not manufacture
// latency or task-success measurements that require an external deployment.
package exposureeval

import (
	"bytes"
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

type corpus struct {
	SchemaVersion    int               `json:"schema_version"`
	ProfileVersion   string            `json:"profile_version"`
	RewriteTrials    int               `json:"rewrite_trials"`
	Relations        []relationFixture `json:"relations"`
	GroundTruth      []groundTruthCase `json:"ground_truth"`
	Adversarial      []adversarialCase `json:"adversarial_cases"`
	PlannerScenarios []plannerScenario `json:"planner_scenarios"`
}

type relationFixture struct {
	Name     string             `json:"name"`
	Product  string             `json:"product"`
	Snapshot string             `json:"snapshot"`
	Fields   []string           `json:"fields"`
	Rows     []exposure.BaseRow `json:"rows"`
}

type groundTruthCase struct {
	ID                 string `json:"id"`
	Operation          string `json:"operation"`
	ReleaseFacts       int    `json:"release_facts"`
	InfluenceFacts     int    `json:"influence_facts"`
	ReleaseSetSHA256   string `json:"release_set_sha256"`
	InfluenceSetSHA256 string `json:"influence_set_sha256"`
}

type adversarialCase struct {
	ID        string `json:"id"`
	Execution string `json:"execution"`
}

type plannerScenario struct {
	ID              string                  `json:"id"`
	ReleaseBudget   int64                   `json:"release_budget"`
	InfluenceBudget int64                   `json:"influence_budget"`
	Weights         exposure.UtilityWeights `json:"weights"`
	ExpectedIDs     []string                `json:"expected_ids"`
	Candidates      []exposure.Candidate    `json:"candidates"`
}

type ValidationSummary struct {
	Cases  int `json:"cases"`
	Passed int `json:"passed"`
}

type RewriteSummary struct {
	GeneratedPairs int `json:"generated_pairs"`
	Mismatches     int `json:"mismatches"`
}

type AdversarialSummary struct {
	Cases                   int      `json:"cases"`
	DeterministicCases      int      `json:"deterministic_cases"`
	DeterministicPassed     int      `json:"deterministic_passed"`
	PostgresIntegrationIDs  []string `json:"postgres_integration_ids"`
	PostgresIntegrationNote string   `json:"postgres_integration_note"`
}

type ChargeVector struct {
	Release   int `json:"release"`
	Influence int `json:"influence"`
}

type BaselineSummary struct {
	QueryCount          int          `json:"query_count"`
	ReturnedRows        int          `json:"returned_rows"`
	SerializedBytes     int          `json:"serialized_bytes"`
	WeightedCells       int          `json:"weighted_cells"`
	ProvenanceNoHistory int          `json:"provenance_no_history"`
	FullFirst           ChargeVector `json:"full_first"`
	FullReplay          ChargeVector `json:"full_replay"`
}

type PlannerResult struct {
	ID             string   `json:"id"`
	SelectedIDs    []string `json:"selected_ids"`
	OptimalUtility float64  `json:"optimal_utility"`
	GreedyUtility  float64  `json:"greedy_utility"`
}

type PlannerSummary struct {
	Scenarios int             `json:"scenarios"`
	Passed    int             `json:"passed"`
	Results   []PlannerResult `json:"results"`
}

type Report struct {
	SchemaVersion  int                `json:"schema_version"`
	ProfileVersion string             `json:"profile_version"`
	CorpusSHA256   string             `json:"corpus_sha256"`
	RewriteSeed    int64              `json:"rewrite_seed"`
	RQ1            ValidationSummary  `json:"rq1_ground_truth"`
	RQ2            RewriteSummary     `json:"rq2_rewrite_invariance"`
	RQ3            AdversarialSummary `json:"rq3_anti_arbitrage"`
	RQ4Status      string             `json:"rq4_runtime_overhead_status"`
	Baselines      BaselineSummary    `json:"charge_baselines"`
	RQ5            PlannerSummary     `json:"rq5_budget_aware_planning"`
}

func Run() (Report, error) {
	fixtures, err := loadCorpus()
	if err != nil {
		return Report{}, err
	}
	relations, err := buildRelations(fixtures.Relations)
	if err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: 1, ProfileVersion: fixtures.ProfileVersion,
		CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(corpusJSON)), RewriteSeed: 20260723,
		RQ4Status: "not_measured_requires_external_postgresql_campaign"}
	for _, testCase := range fixtures.GroundTruth {
		observation, _, err := evaluateOperation(fixtures.ProfileVersion, relations, testCase.Operation)
		if err != nil {
			return Report{}, fmt.Errorf("ground truth %s: %w", testCase.ID, err)
		}
		releaseDigest, err := factSetSHA256(observation.Release)
		if err != nil {
			return Report{}, fmt.Errorf("ground truth %s release: %w", testCase.ID, err)
		}
		influenceDigest, err := factSetSHA256(observation.Influence)
		if err != nil {
			return Report{}, fmt.Errorf("ground truth %s influence: %w", testCase.ID, err)
		}
		report.RQ1.Cases++
		if len(observation.Release) != testCase.ReleaseFacts || len(observation.Influence) != testCase.InfluenceFacts ||
			releaseDigest != testCase.ReleaseSetSHA256 || influenceDigest != testCase.InfluenceSetSHA256 {
			return Report{}, fmt.Errorf("ground truth %s = (%d,%d,%s,%s), want (%d,%d,%s,%s)", testCase.ID,
				len(observation.Release), len(observation.Influence), releaseDigest, influenceDigest,
				testCase.ReleaseFacts, testCase.InfluenceFacts, testCase.ReleaseSetSHA256, testCase.InfluenceSetSHA256)
		}
		report.RQ1.Passed++
	}
	report.RQ2, err = runRewriteTrials(fixtures.ProfileVersion, relations["expenses"], fixtures.RewriteTrials)
	if err != nil {
		return Report{}, err
	}
	report.RQ3, err = runAdversarial(fixtures, relations)
	if err != nil {
		return Report{}, err
	}
	report.Baselines, err = baselineSummary(fixtures.ProfileVersion, relations)
	if err != nil {
		return Report{}, err
	}
	report.RQ5, err = runPlannerScenarios(fixtures.PlannerScenarios)
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

func factSetSHA256(facts []exposure.FactID) (string, error) {
	set, err := exposure.NewFactSet(facts...)
	if err != nil {
		return "", err
	}
	hashes := make([]string, 0, len(set))
	for hash := range set {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	digest := sha256.New()
	_, _ = digest.Write([]byte("TASKGATE-EXPOSURE-FACT-SET-V1\x00"))
	for _, hash := range hashes {
		_, _ = digest.Write([]byte(hash))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}

func loadCorpus() (corpus, error) {
	var result corpus
	decoder := json.NewDecoder(bytes.NewReader(corpusJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return corpus{}, err
	}
	if result.SchemaVersion != 1 || result.ProfileVersion == "" || result.RewriteTrials < 1 ||
		len(result.Relations) == 0 || len(result.GroundTruth) == 0 || len(result.PlannerScenarios) == 0 {
		return corpus{}, fmt.Errorf("invalid exposure evaluation corpus")
	}
	for _, testCase := range result.GroundTruth {
		if len(testCase.ReleaseSetSHA256) != sha256.Size*2 || len(testCase.InfluenceSetSHA256) != sha256.Size*2 {
			return corpus{}, fmt.Errorf("ground truth %s is missing exact set digests", testCase.ID)
		}
	}
	return result, nil
}

func buildRelations(fixtures []relationFixture) (map[string]exposure.Relation, error) {
	result := make(map[string]exposure.Relation, len(fixtures))
	for _, fixture := range fixtures {
		if fixture.Name == "" {
			return nil, fmt.Errorf("relation fixture name is required")
		}
		relation, err := exposure.NewBaseRelation(fixture.Product, fixture.Snapshot, fixture.Fields, fixture.Rows)
		if err != nil {
			return nil, err
		}
		result[fixture.Name] = relation
	}
	return result, nil
}

func evaluateOperation(profile string, relations map[string]exposure.Relation, operation string) (exposure.Observation, exposure.Relation, error) {
	expenses := relations["expenses"]
	var relation exposure.Relation
	var err error
	switch operation {
	case "projection_amount":
		relation, err = exposure.Project(expenses, "amount")
	case "selection_sales_amount":
		relation, err = exposure.Select(expenses, []string{"department"}, func(row exposure.Row) bool {
			return row.Cells["department"].Value == "sales"
		})
		if err == nil {
			relation, err = exposure.Project(relation, "amount")
		}
	case "group_sum_count":
		relation, err = exposure.Aggregate(expenses, []string{"department"}, []exposure.AggregateSpec{
			{Function: "sum", Field: "amount", Alias: "total"},
			{Function: "count", Field: "*", Alias: "items"},
		})
	case "global_count":
		relation, err = exposure.Aggregate(expenses, nil, []exposure.AggregateSpec{{Function: "count", Field: "*", Alias: "items"}})
	case "department_join":
		relation, err = exposure.Join(relations["departments"], expenses, "department", "department")
		if err == nil {
			observation, observeErr := exposure.Observe(profile, relation, "left.manager", "right.amount")
			return observation, relation, observeErr
		}
	default:
		return exposure.Observation{}, exposure.Relation{}, fmt.Errorf("unknown operation %q", operation)
	}
	if err != nil {
		return exposure.Observation{}, exposure.Relation{}, err
	}
	observation, err := exposure.Observe(profile, relation)
	return observation, relation, err
}

func runRewriteTrials(profile string, base exposure.Relation, trials int) (RewriteSummary, error) {
	random := rand.New(rand.NewSource(20260723))
	result := RewriteSummary{}
	for index := 0; index < trials; index++ {
		target := "sales"
		if random.Intn(2) == 0 {
			target = "rnd"
		}
		fields := []string{"department", "amount"}
		if random.Intn(2) == 0 {
			fields[0], fields[1] = fields[1], fields[0]
		}
		selected, err := exposure.Select(base, []string{"department"}, func(row exposure.Row) bool {
			return row.Cells["department"].Value == target
		})
		if err != nil {
			return result, err
		}
		left, err := exposure.Project(selected, fields...)
		if err != nil {
			return result, err
		}
		projected, err := exposure.Project(base, fields...)
		if err != nil {
			return result, err
		}
		right, err := exposure.Select(projected, []string{"department"}, func(row exposure.Row) bool {
			return row.Cells["department"].Value == target
		})
		if err != nil {
			return result, err
		}
		leftObservation, _ := exposure.Observe(profile, left)
		rightObservation, _ := exposure.Observe(profile, right)
		result.GeneratedPairs++
		if !sameObservation(leftObservation, rightObservation) {
			result.Mismatches++
		}

		pageSize := 1 + random.Intn(len(base.Rows))
		var pages []exposure.Observation
		for offset := 0; offset < len(base.Rows); offset += pageSize {
			page, _ := exposure.Page(base, offset, pageSize)
			observation, _ := exposure.Observe(profile, page)
			pages = append(pages, observation)
		}
		merged, err := exposure.MergeObservations(profile, pages...)
		if err != nil {
			return result, err
		}
		full, _ := exposure.Observe(profile, base)
		result.GeneratedPairs++
		if !sameObservation(full, merged) {
			result.Mismatches++
		}
	}
	return result, nil
}

func runAdversarial(fixtures corpus, relations map[string]exposure.Relation) (AdversarialSummary, error) {
	result := AdversarialSummary{Cases: len(fixtures.Adversarial),
		PostgresIntegrationNote: "run_go_test_race_with_control_test_postgres_dsn"}
	full, _ := exposure.Observe(fixtures.ProfileVersion, relations["expenses"])
	for _, testCase := range fixtures.Adversarial {
		if testCase.Execution == "postgres_integration" {
			result.PostgresIntegrationIDs = append(result.PostgresIntegrationIDs, testCase.ID)
			continue
		}
		result.DeterministicCases++
		passed := false
		switch testCase.ID {
		case "join_multiplicity":
			observation, _, err := evaluateOperation(fixtures.ProfileVersion, relations, "department_join")
			passed = err == nil && len(observation.Influence) == 18
		case "split_merge":
			departments, _ := exposure.Project(relations["expenses"], "department")
			amounts, _ := exposure.Project(relations["expenses"], "amount")
			left, _ := exposure.Observe(fixtures.ProfileVersion, departments)
			right, _ := exposure.Observe(fixtures.ProfileVersion, amounts)
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, left, right)
			passed = err == nil && sameObservation(full, merged)
		case "overlapping_pagination":
			first, _ := exposure.Page(relations["expenses"], 0, 3)
			second, _ := exposure.Page(relations["expenses"], 2, 3)
			left, _ := exposure.Observe(fixtures.ProfileVersion, first)
			right, _ := exposure.Observe(fixtures.ProfileVersion, second)
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, left, right)
			passed = err == nil && sameObservation(full, merged)
		case "cache_retry":
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, full, full, full)
			passed = err == nil && sameObservation(full, merged)
		case "snapshot_update":
			before, _ := exposure.NewFact("expense_detail", "snapshot-1", "r1", "amount", 10)
			after, _ := exposure.NewFact("expense_detail", "snapshot-2", "r1", "amount", 10)
			beforeHash, _ := before.Hash()
			afterHash, _ := after.Hash()
			passed = beforeHash != afterHash
		}
		if !passed {
			return result, fmt.Errorf("adversarial case %s failed", testCase.ID)
		}
		result.DeterministicPassed++
	}
	sort.Strings(result.PostgresIntegrationIDs)
	return result, nil
}

func baselineSummary(profile string, relations map[string]exposure.Relation) (BaselineSummary, error) {
	observation, relation, err := evaluateOperation(profile, relations, "group_sum_count")
	if err != nil {
		return BaselineSummary{}, err
	}
	values := make([][]any, 0, len(relation.Rows))
	for _, row := range relation.Rows {
		current := make([]any, 0, len(relation.Fields))
		for _, field := range relation.Fields {
			current = append(current, row.Cells[field].Value)
		}
		values = append(values, current)
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return BaselineSummary{}, err
	}
	return BaselineSummary{
		QueryCount: 1, ReturnedRows: len(relation.Rows), SerializedBytes: len(encoded),
		WeightedCells: len(relation.Rows) * len(relation.Fields), ProvenanceNoHistory: len(observation.Influence),
		FullFirst:  ChargeVector{Release: len(observation.Release), Influence: len(observation.Influence)},
		FullReplay: ChargeVector{},
	}, nil
}

func runPlannerScenarios(scenarios []plannerScenario) (PlannerSummary, error) {
	result := PlannerSummary{Scenarios: len(scenarios)}
	for _, scenario := range scenarios {
		plan, err := exposure.Optimize(scenario.Candidates, scenario.ReleaseBudget, scenario.InfluenceBudget, scenario.Weights)
		if err != nil {
			return result, err
		}
		ids := candidateIDs(plan.Candidates)
		if !sameStrings(ids, scenario.ExpectedIDs) {
			return result, fmt.Errorf("planner scenario %s selected %v, want %v", scenario.ID, ids, scenario.ExpectedIDs)
		}
		greedy := greedyPlan(scenario)
		result.Results = append(result.Results, PlannerResult{ID: scenario.ID, SelectedIDs: ids,
			OptimalUtility: plan.Utility, GreedyUtility: greedy.Utility})
		result.Passed++
	}
	return result, nil
}

func greedyPlan(scenario plannerScenario) exposure.Plan {
	candidates := append([]exposure.Candidate(nil), scenario.Candidates...)
	sort.Slice(candidates, func(i, j int) bool {
		left := measuredUtility(candidates[i], scenario.Weights)
		right := measuredUtility(candidates[j], scenario.Weights)
		if left != right {
			return left > right
		}
		return candidates[i].ID < candidates[j].ID
	})
	selectedRequirements := make(map[string]struct{})
	var result exposure.Plan
	for _, candidate := range candidates {
		if _, selected := selectedRequirements[candidate.Requirement]; selected ||
			result.ReleaseCost+candidate.ReleaseCost > scenario.ReleaseBudget ||
			result.InfluenceCost+candidate.InfluenceCost > scenario.InfluenceBudget {
			continue
		}
		selectedRequirements[candidate.Requirement] = struct{}{}
		result.Candidates = append(result.Candidates, candidate)
		result.ReleaseCost += candidate.ReleaseCost
		result.InfluenceCost += candidate.InfluenceCost
		result.Utility += measuredUtility(candidate, scenario.Weights)
	}
	return result
}

func measuredUtility(candidate exposure.Candidate, weights exposure.UtilityWeights) float64 {
	total := weights.AnswerCompleteness + weights.QueryCoverage
	return (candidate.AnswerCompleteness*weights.AnswerCompleteness + candidate.QueryCoverage*weights.QueryCoverage) / total
}

func candidateIDs(candidates []exposure.Candidate) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		result = append(result, candidate.ID)
	}
	sort.Strings(result)
	return result
}

func sameStrings(left, right []string) bool {
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sort.Strings(leftCopy)
	sort.Strings(rightCopy)
	if len(leftCopy) != len(rightCopy) {
		return false
	}
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func sameObservation(left, right exposure.Observation) bool {
	if left.ProfileVersion != right.ProfileVersion {
		return false
	}
	return sameFacts(left.Release, right.Release) && sameFacts(left.Influence, right.Influence)
}

func sameFacts(left, right []exposure.FactID) bool {
	if len(left) != len(right) {
		return false
	}
	hashes := make(map[string]struct{}, len(left))
	for _, fact := range left {
		hash, err := fact.Hash()
		if err != nil {
			return false
		}
		hashes[hash] = struct{}{}
	}
	for _, fact := range right {
		hash, err := fact.Hash()
		if err != nil {
			return false
		}
		if _, ok := hashes[hash]; !ok {
			return false
		}
	}
	return true
}
