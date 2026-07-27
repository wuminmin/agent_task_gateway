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
	"sort"

	"taskbound.local/agent-data-gateway/evaluation/exposureoracle"
	"taskbound.local/agent-data-gateway/evaluation/postgresoracle"
	"taskbound.local/agent-data-gateway/internal/exposure"
)

//go:embed corpus.json
var corpusJSON []byte

type corpus struct {
	SchemaVersion  int               `json:"schema_version"`
	ProfileVersion string            `json:"profile_version"`
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
	ID        string `json:"id"`
	Operation string `json:"operation"`
}

type adversarialCase struct {
	ID        string `json:"id"`
	Execution string `json:"execution"`
	Test      string `json:"test,omitempty"`
	Package   string `json:"package,omitempty"`
}

type ValidationSummary struct {
	Cases              int                 `json:"cases"`
	Passed             int                 `json:"passed"`
	DatasetRelations   int                 `json:"dataset_relations"`
	DatasetRows        int                 `json:"dataset_rows"`
	ReleaseFacts       int                 `json:"release_fact_comparisons"`
	InfluenceFacts     int                 `json:"influence_fact_comparisons"`
	Oracle             string              `json:"oracle"`
	OracleSourceSHA256 string              `json:"oracle_source_sha256"`
	Results            []GroundTruthResult `json:"results"`
}

type GroundTruthResult struct {
	ID                 string `json:"id"`
	ReleaseFacts       int    `json:"release_facts"`
	InfluenceFacts     int    `json:"influence_facts"`
	ReleaseSetSHA256   string `json:"release_set_sha256"`
	InfluenceSetSHA256 string `json:"influence_set_sha256"`
}

type RewriteSummary struct {
	GeneratedAttempts     int      `json:"generated_attempts"`
	UniqueNormalizedPairs int      `json:"unique_normalized_pairs"`
	ExecutedUniquePairs   int      `json:"executed_unique_pairs"`
	DuplicateAttempts     int      `json:"duplicate_attempts"`
	RewriteTemplates      int      `json:"rewrite_templates"`
	Scenarios             int      `json:"scenarios,omitempty"`
	FixtureRows           int      `json:"fixture_rows,omitempty"`
	PairNormalization     string   `json:"pair_normalization"`
	PairSetSHA256         string   `json:"pair_set_sha256"`
	PairSignatures        []string `json:"normalized_pair_signatures,omitempty"`
	DifferentialChecks    int      `json:"differential_checks,omitempty"`
	MetamorphicChecks     int      `json:"metamorphic_checks,omitempty"`
	PostgresStatements    int      `json:"postgres_statements,omitempty"`
	Mismatches            int      `json:"mismatches"`
	Oracle                string   `json:"oracle"`
	OracleFixtureSHA256   string   `json:"oracle_fixture_sha256,omitempty"`
	PostgresVersion       string   `json:"postgres_version,omitempty"`
	PostgresMajor         int      `json:"postgres_major,omitempty"`
}

type IntegrationCase struct {
	ID      string `json:"id"`
	Test    string `json:"test"`
	Package string `json:"package"`
}

type IntegrationEvidence struct {
	Status         string `json:"status"`
	Artifact       string `json:"artifact,omitempty"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	Executed       int    `json:"executed"`
	Passed         int    `json:"passed"`
	Failed         int    `json:"failed"`
}

type AdversarialSummary struct {
	Cases               int                 `json:"cases"`
	DeterministicCases  int                 `json:"deterministic_cases"`
	DeterministicPassed int                 `json:"deterministic_passed"`
	IntegrationManifest []IntegrationCase   `json:"postgres_integration_manifest"`
	PostgresIntegration IntegrationEvidence `json:"postgres_integration"`
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

type Report struct {
	SchemaVersion  int                      `json:"schema_version"`
	ProfileVersion string                   `json:"profile_version"`
	CorpusSHA256   string                   `json:"corpus_sha256"`
	RQ1            ValidationSummary        `json:"rq1_ground_truth"`
	RQ2            RewriteSummary           `json:"rq2_rewrite_invariance"`
	RQ2Exposure    RewriteInvarianceSummary `json:"rq2_exposure_invariance"`
	RQ3            AdversarialSummary       `json:"rq3_anti_arbitrage"`
	RQ4Status      string                   `json:"rq4_runtime_overhead_status"`
	RQ4Scaling     ScalingSummary           `json:"rq4_scaling"`
	RQ5Policy      PolicyCalibrationSummary `json:"rq5_policy_calibration"`
	Baselines      BaselineSummary          `json:"charge_baselines"`
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
	relationsCount, rowsCount, err := exposureoracle.DatasetShape(corpusJSON)
	if err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: 5, ProfileVersion: fixtures.ProfileVersion,
		CorpusSHA256: fmt.Sprintf("%x", sha256.Sum256(corpusJSON)),
		RQ4Status:    "measured_controlled_local_postgresql_campaign"}
	report.RQ1 = ValidationSummary{DatasetRelations: relationsCount, DatasetRows: rowsCount,
		Oracle: exposureoracle.OracleID, OracleSourceSHA256: exposureoracle.SourceSHA256()}
	for _, testCase := range fixtures.GroundTruth {
		observation, _, err := evaluateOperation(fixtures.ProfileVersion, relations, testCase.Operation)
		if err != nil {
			return Report{}, fmt.Errorf("ground truth %s: %w", testCase.ID, err)
		}
		expected, err := exposureoracle.Evaluate(corpusJSON, testCase.Operation)
		if err != nil {
			return Report{}, fmt.Errorf("independent ground truth %s: %w", testCase.ID, err)
		}
		actualRelease, err := oracleFactSet(observation.Release)
		if err != nil {
			return Report{}, fmt.Errorf("ground truth %s release hash: %w", testCase.ID, err)
		}
		actualInfluence, err := oracleFactSet(observation.Influence)
		if err != nil {
			return Report{}, fmt.Errorf("ground truth %s influence hash: %w", testCase.ID, err)
		}
		report.RQ1.Cases++
		if !sameOracleSet(actualRelease, expected.Release) || !sameOracleSet(actualInfluence, expected.Influence) {
			return Report{}, fmt.Errorf("ground truth %s differs from independent oracle: release=%d/%d influence=%d/%d",
				testCase.ID, len(actualRelease), len(expected.Release), len(actualInfluence), len(expected.Influence))
		}
		releaseDigest := oracleSetSHA256(expected.Release)
		influenceDigest := oracleSetSHA256(expected.Influence)
		report.RQ1.ReleaseFacts += len(expected.Release)
		report.RQ1.InfluenceFacts += len(expected.Influence)
		report.RQ1.Results = append(report.RQ1.Results, GroundTruthResult{ID: testCase.ID,
			ReleaseFacts: len(expected.Release), InfluenceFacts: len(expected.Influence),
			ReleaseSetSHA256: releaseDigest, InfluenceSetSHA256: influenceDigest})
		report.RQ1.Passed++
	}
	report.RQ2, err = runRewriteTrials(fixtures.ProfileVersion, relations["expenses"])
	if err != nil {
		return Report{}, err
	}
	report.RQ2Exposure, err = RunExposureRewriteInvariance()
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
	report.RQ4Scaling, err = RunScaling()
	if err != nil {
		return Report{}, err
	}
	report.RQ5Policy, err = runPolicyCalibration(fixtures.ProfileVersion, fixtures, relations)
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
		GeneratedAttempts:     summary.GeneratedAttempts,
		UniqueNormalizedPairs: summary.UniqueNormalizedPairs,
		ExecutedUniquePairs:   summary.ExecutedUniquePairs,
		DuplicateAttempts:     summary.DuplicateAttempts,
		RewriteTemplates:      summary.RewriteTemplates,
		Scenarios:             summary.Scenarios,
		FixtureRows:           summary.FixtureRows,
		PairNormalization:     summary.PairNormalization,
		PairSetSHA256:         summary.PairSetSHA256,
		PairSignatures:        summary.PairSignatures,
		DifferentialChecks:    summary.DifferentialChecks,
		MetamorphicChecks:     summary.MetamorphicChecks,
		PostgresStatements:    summary.PostgresStatements,
		Mismatches:            summary.Mismatches,
		Oracle:                summary.Oracle,
		OracleFixtureSHA256:   summary.OracleFixtureSHA256,
		PostgresVersion:       summary.PostgresVersion,
		PostgresMajor:         summary.PostgresMajor,
	}
	return report, nil
}

func oracleFactSet(facts []exposure.FactID) (map[string]exposureoracle.Fact, error) {
	result := make(map[string]exposureoracle.Fact, len(facts))
	for _, fact := range facts {
		bindings := make([]exposureoracle.SnapshotBinding, 0, len(fact.SnapshotBundle))
		for _, binding := range fact.SnapshotBundle {
			bindings = append(bindings, exposureoracle.SnapshotBinding{SourceNamespace: binding.SourceNamespace, Snapshot: binding.Snapshot})
		}
		converted := exposureoracle.Fact{Profile: fact.Profile, Kind: string(fact.Kind), Snapshot: fact.Snapshot,
			EntityKey: fact.EntityKey, Field: fact.Field, SourceNamespace: fact.SourceNamespace,
			SQLType: fact.SQLType, CanonicalValue: fact.CanonicalValue, SnapshotBundle: bindings,
			OutputRowKey: fact.OutputRowKey, NormalizedExpression: fact.NormalizedExpression,
			WitnessCommitment: fact.WitnessCommitment}
		productionHash, err := fact.Hash()
		if err != nil {
			return nil, err
		}
		if oracleHash := exposureoracle.Hash(converted); oracleHash != productionHash {
			return nil, fmt.Errorf("independent hash %s differs from production hash %s", oracleHash, productionHash)
		}
		result[exposureoracle.Key(converted)] = converted
	}
	return result, nil
}

func sameOracleSet(left, right map[string]exposureoracle.Fact) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, present := right[key]; !present {
			return false
		}
	}
	return true
}

func oracleSetSHA256(set map[string]exposureoracle.Fact) string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	digest := sha256.New()
	_, _ = digest.Write([]byte("TASKGATE-INDEPENDENT-ORACLE-FACT-SET-V1\x00"))
	for _, key := range keys {
		_, _ = digest.Write([]byte(key))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
}

func loadCorpus() (corpus, error) {
	var result corpus
	decoder := json.NewDecoder(bytes.NewReader(corpusJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return corpus{}, err
	}
	if result.SchemaVersion != 2 || result.ProfileVersion == "" ||
		len(result.Relations) == 0 || len(result.GroundTruth) == 0 {
		return corpus{}, fmt.Errorf("invalid exposure evaluation corpus")
	}
	for _, testCase := range result.GroundTruth {
		if testCase.ID == "" || testCase.Operation == "" {
			return corpus{}, fmt.Errorf("ground truth case is incomplete")
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
	case "projection_department":
		relation, err = exposure.ProjectV2(expenses, "expense.department")
	case "projection_pair":
		relation, err = exposure.ProjectV2(expenses, "expense.department", "expense.amount")
	case "selection_sales_amount", "selection_rnd_amount", "selection_ops_amount", "selection_legal_amount", "selection_missing_amount", "selection_positive_boundary":
		targets := map[string]string{
			"selection_sales_amount": "sales", "selection_rnd_amount": "rnd", "selection_ops_amount": "ops",
			"selection_legal_amount": "legal", "selection_missing_amount": "missing", "selection_positive_boundary": "sales",
		}
		target := targets[operation]
		relation, err = exposure.SelectV2(expenses, []string{"expense.department"}, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if value, ok := row.Cells["expense.department"].Value.(string); ok && value == target {
				return exposure.SQLTrue
			}
			if row.Cells["expense.department"].Value == nil {
				return exposure.SQLUnknown
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
			{"expense.department": "sales", "total": "80", "items": int64(5)},
			{"expense.department": "rnd", "total": "45", "items": int64(2)},
			{"expense.department": "ops", "total": "40", "items": int64(2)},
			{"expense.department": "legal", "total": "25", "items": int64(2)},
			{"expense.department": nil, "total": "40", "items": int64(1)},
		})
	case "group_hidden_key_sum":
		relation, err = exposure.AggregateFromResultsV2(expenses, []string{"expense.department"}, []exposure.AggregateSpecV2{
			{Function: "sum", Field: "expense.amount", OutputID: "total", OutputType: "numeric"},
		}, []map[string]any{
			{"expense.department": "sales", "total": "80"},
			{"expense.department": "rnd", "total": "45"},
			{"expense.department": "ops", "total": "40"},
			{"expense.department": "legal", "total": "25"},
			{"expense.department": nil, "total": "40"},
		})
		if err == nil {
			observation, observeErr := exposure.ObserveV2(relation, "total")
			return observation, relation, observeErr
		}
	case "global_count":
		relation, err = exposure.AggregateFromResultsV2(expenses, nil, []exposure.AggregateSpecV2{
			{Function: "count", Field: "*", OutputID: "items", OutputType: "bigint"},
		}, []map[string]any{{"items": int64(12)}})
	case "global_count_column_null":
		relation, err = exposure.AggregateFromResultsV2(expenses, nil, []exposure.AggregateSpecV2{
			{Function: "count", Field: "expense.amount", OutputID: "items", OutputType: "bigint"},
		}, []map[string]any{{"items": int64(11)}})
	case "global_sum":
		relation, err = exposure.AggregateFromResultsV2(expenses, nil, []exposure.AggregateSpecV2{
			{Function: "sum", Field: "expense.amount", OutputID: "total", OutputType: "numeric"},
		}, []map[string]any{{"total": "230"}})
	case "global_min_all_inputs":
		relation, err = exposure.AggregateFromResultsV2(expenses, nil, []exposure.AggregateSpecV2{
			{Function: "min", Field: "expense.amount", OutputID: "minimum", OutputType: "numeric"},
		}, []map[string]any{{"minimum": "0"}})
	case "global_max_all_inputs":
		relation, err = exposure.AggregateFromResultsV2(expenses, nil, []exposure.AggregateSpecV2{
			{Function: "max", Field: "expense.amount", OutputID: "maximum", OutputType: "numeric"},
		}, []map[string]any{{"maximum": "40"}})
	case "department_join":
		relation, err = exposure.JoinV2(relations["departments"], expenses, "department.department", "expense.department")
		if err == nil {
			observation, observeErr := exposure.ObserveV2(relation, "department.manager", "expense.amount")
			return observation, relation, observeErr
		}
	case "page_first_four":
		relation, err = exposure.PageV2(expenses, 0, 4)
	case "page_middle_five":
		relation, err = exposure.PageV2(expenses, 3, 5)
	case "page_order_boundary":
		relation = expenses
		relation.Rows = append([]exposure.AnnotatedRowV2(nil), expenses.Rows...)
		sort.SliceStable(relation.Rows, func(i, j int) bool {
			left, leftOK := relation.Rows[i].Cells["expense.department"].Value.(string)
			right, rightOK := relation.Rows[j].Cells["expense.department"].Value.(string)
			if leftOK != rightOK {
				return leftOK // PostgreSQL ASC NULLS LAST
			}
			if left != right {
				return left < right
			}
			return relation.Rows[i].Key < relation.Rows[j].Key
		})
		relation.CanonicalOrder = true
		relation, err = exposure.PageV2(relation, 0, 1)
		if err == nil {
			observation, observeErr := exposure.ObserveV2(relation, "expense.amount")
			return observation, relation, observeErr
		}
	case "union_hidden_distinct":
		left, selectErr := exposure.SelectV2(expenses, nil, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if row.Key == "r10" {
				return exposure.SQLTrue
			}
			return exposure.SQLFalse
		})
		if selectErr != nil {
			return exposure.Observation{}, exposure.RelationV2{}, selectErr
		}
		right, selectErr := exposure.SelectV2(expenses, nil, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			if row.Key == "r12" {
				return exposure.SQLTrue
			}
			return exposure.SQLFalse
		})
		if selectErr != nil {
			return exposure.Observation{}, exposure.RelationV2{}, selectErr
		}
		relation, err = exposure.UnionDistinctV2(left, right)
		if err == nil {
			observation, observeErr := exposure.ObserveV2(relation, "expense.amount")
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

func runRewriteTrials(profile string, base exposure.RelationV2) (RewriteSummary, error) {
	result := RewriteSummary{RewriteTemplates: 2, Oracle: "shared-implementation-preflight",
		PairNormalization: "semantic-parameter-key-v1", FixtureRows: len(base.Rows)}
	unique := make(map[string]struct{})
	for _, target := range []string{"sales", "rnd"} {
		for _, fields := range [][]string{{"expense.department", "expense.amount"}, {"expense.amount", "expense.department"}} {
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
			result.GeneratedAttempts++
			unique[fmt.Sprintf("selection_projection:%s:%s,%s", target, fields[0], fields[1])] = struct{}{}
			if !sameObservation(leftObservation, rightObservation) {
				result.Mismatches++
			}
		}
	}
	for pageSize := 1; pageSize <= len(base.Rows); pageSize++ {
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
		result.GeneratedAttempts++
		unique[fmt.Sprintf("page_partition:%d", pageSize)] = struct{}{}
		if !sameObservation(full, merged) {
			result.Mismatches++
		}
	}
	result.UniqueNormalizedPairs = len(unique)
	result.ExecutedUniquePairs = len(unique)
	result.DuplicateAttempts = result.GeneratedAttempts - result.UniqueNormalizedPairs
	result.MetamorphicChecks = result.ExecutedUniquePairs
	result.PairSetSHA256 = stringSetSHA256("TASKGATE-PREFLIGHT-PAIR-SET-V1\x00", unique)
	return result, nil
}

func runAdversarial(fixtures corpus, relations map[string]exposure.RelationV2) (AdversarialSummary, error) {
	result := AdversarialSummary{Cases: len(fixtures.Adversarial),
		PostgresIntegration: IntegrationEvidence{Status: "not_run"}}
	full, _ := exposure.ObserveV2(relations["expenses"])
	for _, testCase := range fixtures.Adversarial {
		if testCase.Execution == "postgres_integration" {
			if testCase.ID == "" || testCase.Test == "" || testCase.Package == "" {
				return result, fmt.Errorf("PostgreSQL integration case is incomplete")
			}
			result.IntegrationManifest = append(result.IntegrationManifest, IntegrationCase{ID: testCase.ID, Test: testCase.Test, Package: testCase.Package})
			continue
		}
		result.DeterministicCases++
		passed := false
		switch testCase.ID {
		case "join_multiplicity":
			observation, _, err := evaluateOperation(fixtures.ProfileVersion, relations, "department_join")
			passed = err == nil && len(observation.Influence) == 45
		case "split_merge":
			departments, _ := exposure.ProjectV2(relations["expenses"], "expense.department")
			amounts, _ := exposure.ProjectV2(relations["expenses"], "expense.amount")
			left, _ := exposure.ObserveV2(departments)
			right, _ := exposure.ObserveV2(amounts)
			merged, err := exposure.MergeObservations(fixtures.ProfileVersion, left, right)
			passed = err == nil && sameObservation(full, merged)
		case "overlapping_pagination":
			first, _ := exposure.PageV2(relations["expenses"], 0, 8)
			second, _ := exposure.PageV2(relations["expenses"], 6, 6)
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
	sort.Slice(result.IntegrationManifest, func(i, j int) bool { return result.IntegrationManifest[i].ID < result.IntegrationManifest[j].ID })
	return result, nil
}

func stringSetSHA256(domain string, set map[string]struct{}) string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
	return fmt.Sprintf("%x", digest.Sum(nil))
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
