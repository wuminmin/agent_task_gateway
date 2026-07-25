// Package exposureeval provides deterministic, source-controlled experiments
// for TaskGate's exposure semantics. It deliberately does not manufacture
// latency or task-success measurements that require an external deployment.
package exposureeval

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"

	"taskbound.local/agent-data-gateway/evaluation/agenttasks"
	"taskbound.local/agent-data-gateway/evaluation/postgresoracle"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

//go:embed corpus.json
var corpusJSON []byte

type corpus struct {
	SchemaVersion  int               `json:"schema_version"`
	ProfileVersion string            `json:"profile_version"`
	RewriteTrials  int               `json:"rewrite_trials"`
	Relations      []relationFixture `json:"relations"`
	GroundTruth    []groundTruthCase `json:"ground_truth"`
	Adversarial    []adversarialCase `json:"adversarial_cases"`
}

type relationFixture struct {
	Name            string               `json:"name"`
	SourceNamespace string               `json:"source_namespace"`
	Snapshot        string               `json:"snapshot"`
	StableRole      string               `json:"stable_role"`
	Fields          []exposure.FieldV2   `json:"fields"`
	Rows            []exposure.BaseRowV2 `json:"rows"`
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

type ValidationSummary struct {
	Cases  int `json:"cases"`
	Passed int `json:"passed"`
}

type RewriteSummary struct {
	GeneratedPairs      int    `json:"generated_pairs"`
	UniqueRewrites      int    `json:"unique_rewrites"`
	RewriteTemplates    int    `json:"rewrite_templates"`
	DifferentialChecks  int    `json:"differential_checks,omitempty"`
	MetamorphicChecks   int    `json:"metamorphic_checks,omitempty"`
	PostgresStatements  int    `json:"postgres_statements,omitempty"`
	Mismatches          int    `json:"mismatches"`
	Oracle              string `json:"oracle"`
	OracleFixtureSHA256 string `json:"oracle_fixture_sha256,omitempty"`
	PostgresVersion     string `json:"postgres_version,omitempty"`
	PostgresMajor       int    `json:"postgres_major,omitempty"`
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
	ID                   string   `json:"id"`
	SelectedIDs          []string `json:"selected_ids"`
	ExactUtility         float64  `json:"exact_utility"`
	AdditiveProxyUtility float64  `json:"additive_proxy_utility"`
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
	RQ5Agent       agenttasks.Report  `json:"rq5_agent_tasks"`
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
	report := Report{SchemaVersion: 2, ProfileVersion: fixtures.ProfileVersion,
		CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(corpusJSON)), RewriteSeed: 20260723,
		RQ4Status: "measured_controlled_local_postgresql_campaign"}
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
	report.RQ5, err = runSharedFactPlannerRegression()
	if err != nil {
		return Report{}, err
	}
	if err := runRandomV2PlannerOracle(&report.RQ5, 500); err != nil {
		return Report{}, err
	}
	report.RQ5Agent, err = agenttasks.Run()
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

// RunPostgreSQL replaces the shared-implementation RQ2 preflight from Run
// with a real PostgreSQL 16 differential/metamorphic campaign whose expected
// rows come from the independent postgresoracle package.
func RunPostgreSQL(ctx context.Context, dsn string) (Report, error) {
	report, err := Run()
	if err != nil {
		return Report{}, err
	}
	summary, err := postgresoracle.Run(ctx, dsn)
	if err != nil {
		return Report{}, err
	}
	report.RQ2 = RewriteSummary{
		GeneratedPairs:      summary.GeneratedAttempts,
		UniqueRewrites:      summary.UniqueRewrites,
		RewriteTemplates:    summary.RewriteTemplates,
		DifferentialChecks:  summary.DifferentialChecks,
		MetamorphicChecks:   summary.MetamorphicChecks,
		PostgresStatements:  summary.PostgresStatements,
		Mismatches:          summary.Mismatches,
		Oracle:              summary.Oracle,
		OracleFixtureSHA256: summary.OracleFixtureSHA256,
		PostgresVersion:     summary.PostgresVersion,
		PostgresMajor:       summary.PostgresMajor,
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
	_, _ = digest.Write([]byte("TASKGATE-EXPOSURE-FACT-SET-V2\x00"))
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
		len(result.Relations) == 0 || len(result.GroundTruth) == 0 {
		return corpus{}, fmt.Errorf("invalid exposure evaluation corpus")
	}
	for _, testCase := range result.GroundTruth {
		if len(testCase.ReleaseSetSHA256) != sha256.Size*2 || len(testCase.InfluenceSetSHA256) != sha256.Size*2 {
			return corpus{}, fmt.Errorf("ground truth %s is missing exact set digests", testCase.ID)
		}
	}
	return result, nil
}

func buildRelations(fixtures []relationFixture) (map[string]exposure.RelationV2, error) {
	result := make(map[string]exposure.RelationV2, len(fixtures))
	for _, fixture := range fixtures {
		if fixture.Name == "" {
			return nil, fmt.Errorf("relation fixture name is required")
		}
		relation, err := exposure.ScanV2(exposure.BaseRelationSpecV2{SourceNamespace: fixture.SourceNamespace,
			Snapshot: fixture.Snapshot, StableRole: fixture.StableRole, Fields: fixture.Fields, Rows: fixture.Rows})
		if err != nil {
			return nil, err
		}
		relation.CanonicalOrder = true // source-controlled fixture order is the stable entity-key order
		result[fixture.Name] = relation
	}
	return result, nil
}

func evaluateOperation(profile string, relations map[string]exposure.RelationV2, operation string) (exposure.Observation, exposure.RelationV2, error) {
	expenses := relations["expenses"]
	var relation exposure.RelationV2
	var err error
	switch operation {
	case "projection_amount":
		relation, err = exposure.ProjectV2(expenses, "expense.amount")
	case "selection_sales_amount":
		relation, err = exposure.SelectV2(expenses, []string{"expense.department"}, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if row.Cells["expense.department"].Value == "sales" {
				return exposure.SQLTrue
			}
			return exposure.SQLFalse
		})
		if err == nil {
			relation, err = exposure.ProjectV2(relation, "expense.amount")
		}
	case "group_sum_count":
		relation, err = exposure.AggregateFromResultsV2(expenses, []string{"expense.department"}, []exposure.AggregateSpecV2{
			{Function: "sum", Field: "expense.amount", OutputID: "total", OutputType: "numeric"},
			{Function: "count", Field: "*", OutputID: "items", OutputType: "bigint"},
		}, []map[string]any{
			{"expense.department": "sales", "total": "40", "items": int64(3)},
			{"expense.department": "rnd", "total": "30", "items": int64(1)},
		})
	case "global_count":
		relation, err = exposure.AggregateFromResultsV2(expenses, nil, []exposure.AggregateSpecV2{
			{Function: "count", Field: "*", OutputID: "items", OutputType: "bigint"},
		}, []map[string]any{{"items": int64(4)}})
	case "department_join":
		relation, err = exposure.JoinV2(relations["departments"], expenses, "department.department", "expense.department")
		if err == nil {
			observation, observeErr := exposure.ObserveV2(relation, "department.manager", "expense.amount")
			return observation, relation, observeErr
		}
	default:
		return exposure.Observation{}, exposure.RelationV2{}, fmt.Errorf("unknown operation %q", operation)
	}
	if err != nil {
		return exposure.Observation{}, exposure.RelationV2{}, err
	}
	observation, err := exposure.ObserveV2(relation)
	return observation, relation, err
}

func runRewriteTrials(profile string, base exposure.RelationV2, trials int) (RewriteSummary, error) {
	random := rand.New(rand.NewSource(20260723))
	result := RewriteSummary{RewriteTemplates: 2, Oracle: "shared-implementation-preflight"}
	unique := make(map[string]struct{})
	for index := 0; index < trials; index++ {
		target := "sales"
		if random.Intn(2) == 0 {
			target = "rnd"
		}
		fields := []string{"expense.department", "expense.amount"}
		if random.Intn(2) == 0 {
			fields[0], fields[1] = fields[1], fields[0]
		}
		selected, err := exposure.SelectV2(base, []string{"expense.department"}, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if row.Cells["expense.department"].Value == target {
				return exposure.SQLTrue
			}
			return exposure.SQLFalse
		})
		if err != nil {
			return result, err
		}
		left, err := exposure.ProjectV2(selected, fields...)
		if err != nil {
			return result, err
		}
		projected, err := exposure.ProjectV2(base, fields...)
		if err != nil {
			return result, err
		}
		right, err := exposure.SelectV2(projected, []string{"expense.department"}, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if row.Cells["expense.department"].Value == target {
				return exposure.SQLTrue
			}
			return exposure.SQLFalse
		})
		if err != nil {
			return result, err
		}
		leftObservation, _ := exposure.ObserveV2(left)
		rightObservation, _ := exposure.ObserveV2(right)
		result.GeneratedPairs++
		unique[fmt.Sprintf("selection_projection:%s:%s,%s", target, fields[0], fields[1])] = struct{}{}
		if !sameObservation(leftObservation, rightObservation) {
			result.Mismatches++
		}

		pageSize := 1 + random.Intn(len(base.Rows))
		var pages []exposure.Observation
		for offset := 0; offset < len(base.Rows); offset += pageSize {
			page, _ := exposure.PageV2(base, offset, pageSize)
			observation, _ := exposure.ObserveV2(page)
			pages = append(pages, observation)
		}
		merged, err := exposure.MergeObservations(profile, pages...)
		if err != nil {
			return result, err
		}
		full, _ := exposure.ObserveV2(base)
		result.GeneratedPairs++
		unique[fmt.Sprintf("page_partition:%d", pageSize)] = struct{}{}
		if !sameObservation(full, merged) {
			result.Mismatches++
		}
	}
	result.UniqueRewrites = len(unique)
	return result, nil
}

func runAdversarial(fixtures corpus, relations map[string]exposure.RelationV2) (AdversarialSummary, error) {
	result := AdversarialSummary{Cases: len(fixtures.Adversarial),
		PostgresIntegrationNote: "run_go_test_race_with_control_test_postgres_dsn"}
	full, _ := exposure.ObserveV2(relations["expenses"])
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
			departments, _ := exposure.ProjectV2(relations["expenses"], "expense.department")
			amounts, _ := exposure.ProjectV2(relations["expenses"], "expense.amount")
			left, _ := exposure.ObserveV2(departments)
			right, _ := exposure.ObserveV2(amounts)
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, left, right)
			passed = err == nil && sameObservation(full, merged)
		case "overlapping_pagination":
			first, _ := exposure.PageV2(relations["expenses"], 0, 3)
			second, _ := exposure.PageV2(relations["expenses"], 2, 3)
			left, _ := exposure.ObserveV2(first)
			right, _ := exposure.ObserveV2(second)
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, left, right)
			passed = err == nil && sameObservation(full, merged)
		case "cache_retry":
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, full, full, full)
			passed = err == nil && sameObservation(full, merged)
		case "snapshot_update":
			before, _ := exposure.NewBaseCellFactV2("travel.expense", "snapshot-1", "r1", "expense.amount", "numeric", "10")
			after, _ := exposure.NewBaseCellFactV2("travel.expense", "snapshot-2", "r1", "expense.amount", "numeric", "10")
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

func baselineSummary(profile string, relations map[string]exposure.RelationV2) (BaselineSummary, error) {
	observation, relation, err := evaluateOperation(profile, relations, "group_sum_count")
	if err != nil {
		return BaselineSummary{}, err
	}
	values := make([][]any, 0, len(relation.Rows))
	for _, row := range relation.Rows {
		current := make([]any, 0, len(relation.Fields))
		for _, field := range relation.Fields {
			current = append(current, row.Cells[field.ID].Value)
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

func runSharedFactPlannerRegression() (PlannerSummary, error) {
	fact, err := exposure.NewBaseCellFactV2("evaluation.planner", "snapshot-v2", "shared", "value", "text", "shared")
	if err != nil {
		return PlannerSummary{}, err
	}
	effect := exposure.Observation{ProfileVersion: exposure.ProfileV2, Release: []exposure.FactID{fact}, Influence: []exposure.FactID{fact}}
	plan, err := exposure.OptimizeEffects([]exposure.EffectCandidate{
		{ID: "requirement-a", Requirement: "a", AnswerCompleteness: 1, Effect: effect},
		{ID: "requirement-b", Requirement: "b", AnswerCompleteness: 1, Effect: effect},
	}, exposure.Observation{ProfileVersion: exposure.ProfileV2}, 1, 1, exposure.UtilityWeights{AnswerCompleteness: 1})
	if err != nil {
		return PlannerSummary{}, err
	}
	ids := make([]string, 0, len(plan.Selected))
	for _, selected := range plan.Selected {
		ids = append(ids, selected.ID)
	}
	if len(ids) != 2 || plan.ReleaseCost != 1 || plan.InfluenceCost != 1 || plan.Utility != 2 {
		return PlannerSummary{}, fmt.Errorf("shared-fact regression selected %v with (%d,%d,%v)",
			ids, plan.ReleaseCost, plan.InfluenceCost, plan.Utility)
	}
	return PlannerSummary{Scenarios: 1, Passed: 1, Results: []PlannerResult{{
		ID: "shared-fact-budget-one", SelectedIDs: ids, ExactUtility: plan.Utility, AdditiveProxyUtility: 1,
	}}}, nil
}

func runRandomV2PlannerOracle(summary *PlannerSummary, trials int) error {
	random := rand.New(rand.NewSource(20260724))
	pool := make([]exposure.FactID, 8)
	for index := range pool {
		fact, err := exposure.NewBaseCellFactV2("evaluation.oracle", "snapshot-v2", fmt.Sprintf("row-%d", index), "value", "bigint", int64(index))
		if err != nil {
			return err
		}
		pool[index] = fact
	}
	for trial := 0; trial < trials; trial++ {
		var candidates []exposure.EffectCandidate
		for requirement := 0; requirement < 3; requirement++ {
			for option := 0; option < 2; option++ {
				candidates = append(candidates, exposure.EffectCandidate{
					ID: fmt.Sprintf("c-%d-%d", requirement, option), Requirement: fmt.Sprintf("r-%d", requirement),
					AnswerCompleteness: float64(1+random.Intn(5)) / 5,
					Effect: exposure.Observation{ProfileVersion: exposure.ProfileV2,
						Release: randomV2Facts(random, pool), Influence: randomV2Facts(random, pool)},
				})
			}
		}
		history := exposure.Observation{ProfileVersion: exposure.ProfileV2,
			Release: randomV2Facts(random, pool), Influence: randomV2Facts(random, pool)}
		releaseBudget, influenceBudget := int64(random.Intn(7)), int64(random.Intn(7))
		plan, err := exposure.OptimizeEffects(candidates, history, releaseBudget, influenceBudget,
			exposure.UtilityWeights{AnswerCompleteness: 1})
		if err != nil {
			return err
		}
		oracle := bruteForceV2Utility(candidates, history, releaseBudget, influenceBudget)
		if plan.Utility != oracle {
			return fmt.Errorf("V2 random planner trial %d utility %v, oracle %v", trial, plan.Utility, oracle)
		}
		summary.Scenarios++
		summary.Passed++
	}
	summary.Results = append(summary.Results, PlannerResult{ID: fmt.Sprintf("v2-random-bruteforce-%d", trials)})
	return nil
}

func randomV2Facts(random *rand.Rand, pool []exposure.FactID) []exposure.FactID {
	var result []exposure.FactID
	for _, fact := range pool {
		if random.Intn(3) == 0 {
			result = append(result, fact)
		}
	}
	return result
}

func bruteForceV2Utility(candidates []exposure.EffectCandidate, history exposure.Observation, releaseBudget, influenceBudget int64) float64 {
	historyRelease, _ := exposure.NewFactSet(history.Release...)
	historyInfluence, _ := exposure.NewFactSet(history.Influence...)
	best := float64(0)
	for mask := 0; mask < 1<<len(candidates); mask++ {
		requirements := make(map[string]struct{})
		release, influence := make(exposure.FactSet), make(exposure.FactSet)
		utility, valid := float64(0), true
		for index, candidate := range candidates {
			if mask&(1<<index) == 0 {
				continue
			}
			if _, duplicate := requirements[candidate.Requirement]; duplicate {
				valid = false
				break
			}
			requirements[candidate.Requirement] = struct{}{}
			candidateRelease, _ := exposure.NewFactSet(candidate.Effect.Release...)
			candidateInfluence, _ := exposure.NewFactSet(candidate.Effect.Influence...)
			release.Merge(candidateRelease)
			influence.Merge(candidateInfluence)
			utility += candidate.AnswerCompleteness
		}
		if !valid {
			continue
		}
		for hash := range historyRelease {
			delete(release, hash)
		}
		for hash := range historyInfluence {
			delete(influence, hash)
		}
		if int64(len(release)) <= releaseBudget && int64(len(influence)) <= influenceBudget && utility > best {
			best = utility
		}
	}
	return best
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
