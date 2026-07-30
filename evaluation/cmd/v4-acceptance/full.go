package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const (
	fullTaskPoolSchema  = 1
	fullTaskPoolDataset = "deterministic-tpch-derived-orders-lineitem-v1"
	fullTaskCount       = 140
	fullTrialsPerCase   = 20
	fullOrderCount      = 45_000
	fullEvidenceMaxSize = 64 << 20

	fullSmallQueryWorkload       = "expense_detail/sales/ordered/limit-1"
	fullSmallQueryCacheStrategy  = "warm"
	fullSmallQueryTaskMode       = "delegated_tasks_shared_root"
	fullOfflineExecutable        = "/usr/local/bin/v4-offline"
	fullOrdersSnapshotInput      = "/usr/local/share/taskgate/snapshots/scale-orders-v4-narrow-1.json"
	fullLineitemSnapshotInput    = "/usr/local/share/taskgate/snapshots/scale-lineitem-v4-narrow-1.json"
	fullPublishedArtifactRoot    = "/var/lib/taskgate/snapshot-index"
	fullOrdersPublishedHotPath   = "/var/lib/taskgate/snapshot-index/scale-orders-v4-narrow-1/scale-orders-v4-narrow-1.hot.tgord"
	fullLineitemPublishedHotPath = "/var/lib/taskgate/snapshot-index/scale-lineitem-v4-narrow-1/scale-lineitem-v4-narrow-1.hot.tgord"

	fullScanDirectSQL  = "SELECT o_orderkey, o_orderstatus FROM reporting.scale_orders WHERE dataset_partition = 1 AND o_orderkey = 1 ORDER BY o_orderkey"
	fullPageDirectSQL  = "SELECT l_orderkey, l_linenumber, l_extendedprice FROM reporting.scale_lineitem WHERE dataset_partition = 1 AND l_orderkey <= 20 ORDER BY l_orderkey, l_linenumber LIMIT 20 OFFSET 20"
	fullUnionDirectSQL = "SELECT o_orderkey, o_orderstatus FROM reporting.scale_orders WHERE dataset_partition = 1 AND o_orderkey <= 20 UNION SELECT o_orderkey, o_orderstatus FROM reporting.scale_orders WHERE dataset_partition = 1 AND o_orderkey > 20 AND o_orderkey <= 40 ORDER BY o_orderkey, o_orderstatus"
)

type fullTaskPool struct {
	SchemaVersion int        `json:"schema_version"`
	Dataset       string     `json:"dataset"`
	Tasks         []fullTask `json:"tasks"`
}

type fullTask struct {
	TaskID string `json:"task_id"`
	Trial  int    `json:"trial"`
	Orders int    `json:"orders"`
}

type fullCaseContract struct {
	ID                   string
	Shape                string
	TargetOverlapPercent float64
	Plan                 queryplan.QueryPlan
	SetupPlans           []queryplan.QueryPlan
	DirectSQL            string
	Expected             expectedResult
	SmallQuery           bool
}

type fullBoundArtifact struct {
	Path   string
	SHA256 string
}

type fullPreparationEvidence struct {
	Environment fullBoundArtifact
	Baseline    fullBoundArtifact
	Candidate   fullBoundArtifact
}

type fullBenchmarkReport struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Configuration struct {
		Workload            string `json:"workload"`
		CacheStrategy       string `json:"cache_strategy"`
		TaskConcurrencyMode string `json:"task_concurrency_mode"`
	} `json:"configuration"`
	Cells []fullBenchmarkCell `json:"cells"`
}

type fullBenchmarkCell struct {
	Phase                 string   `json:"phase"`
	Concurrency           int      `json:"concurrency"`
	Samples               int      `json:"samples"`
	SamplesPerTrial       int      `json:"samples_per_trial"`
	ThroughputQPS         float64  `json:"throughput_qps"`
	QueryHistoryHitRate   *float64 `json:"query_history_hit_rate"`
	FactHistoryHitRate    *float64 `json:"fact_history_hit_rate"`
	SemanticReplayHitRate *float64 `json:"semantic_replay_hit_rate"`
	ActualFacts           *int64   `json:"actual_facts"`
	ChargedFacts          *int64   `json:"charged_facts"`
	LatencyMS             struct {
		P50 float64 `json:"p50"`
	} `json:"latency_ms"`
}

// prepareFullConfig binds one disjoint block of twenty scale-bootstrap roots
// to each fixed workload case. The task pool is credential-free evidence that
// the public request/submit/approve flow already produced ACTIVE V4 roots; no
// measured or setup plan is executed during preparation.
func prepareFullConfig(template config, taskPoolPath string, evidence fullPreparationEvidence) (config, error) {
	pool, err := readFullTaskPool(taskPoolPath)
	if err != nil {
		return config{}, err
	}
	if err := validateFullTemplate(template); err != nil {
		return config{}, err
	}
	environmentRaw, environmentReference, err := readFullBoundArtifact("environment", evidence.Environment)
	if err != nil {
		return config{}, err
	}
	if err := validateFullEnvironment(environmentRaw); err != nil {
		return config{}, err
	}
	baselineRaw, baselineReference, err := readFullBoundArtifact("V2 baseline", evidence.Baseline)
	if err != nil {
		return config{}, err
	}
	baselineMetric, err := parseFullSmallQueryEvidence("V2 baseline", baselineRaw, false)
	if err != nil {
		return config{}, err
	}
	candidateRaw, candidateReference, err := readFullBoundArtifact("V4 candidate", evidence.Candidate)
	if err != nil {
		return config{}, err
	}
	candidateMetric, err := parseFullSmallQueryEvidence("V4 candidate", candidateRaw, true)
	if err != nil {
		return config{}, err
	}
	template.EnvironmentManifest = &environmentReference
	template.SmallQueryBaseline = &smallQueryBaseline{ArtifactPath: baselineReference.Path,
		ArtifactSHA256: baselineReference.SHA256, P50MS: baselineMetric.P50MS,
		ThroughputQPS: baselineMetric.ThroughputQPS}
	template.SmallQueryCandidate = &smallQueryBaseline{ArtifactPath: candidateReference.Path,
		ArtifactSHA256: candidateReference.SHA256, P50MS: candidateMetric.P50MS,
		ThroughputQPS: candidateMetric.ThroughputQPS}
	bindFullOfflineContracts(&template)
	sort.Slice(pool.Tasks, func(i, j int) bool { return pool.Tasks[i].Trial < pool.Tasks[j].Trial })
	for caseIndex := range template.Cases {
		first := caseIndex * fullTrialsPerCase
		last := first + fullTrialsPerCase
		template.Cases[caseIndex].TaskIDs = make([]string, 0, fullTrialsPerCase)
		for _, task := range pool.Tasks[first:last] {
			template.Cases[caseIndex].TaskIDs = append(template.Cases[caseIndex].TaskIDs, task.TaskID)
		}
	}
	return template, nil
}

func readFullBoundArtifact(label string, evidence fullBoundArtifact) ([]byte, artifactReference, error) {
	if strings.TrimSpace(evidence.Path) == "" || evidence.Path != strings.TrimSpace(evidence.Path) {
		return nil, artifactReference{}, fmt.Errorf("%s evidence path is required", label)
	}
	if len(evidence.SHA256) != sha256.Size*2 || evidence.SHA256 != strings.ToLower(evidence.SHA256) {
		return nil, artifactReference{}, fmt.Errorf("%s expected SHA-256 is invalid", label)
	}
	decoded, err := hex.DecodeString(evidence.SHA256)
	if err != nil || len(decoded) != sha256.Size {
		return nil, artifactReference{}, fmt.Errorf("%s expected SHA-256 is invalid", label)
	}
	raw, err := os.ReadFile(evidence.Path)
	if err != nil {
		return nil, artifactReference{}, fmt.Errorf("read %s evidence: %w", label, err)
	}
	if len(raw) == 0 || len(raw) > fullEvidenceMaxSize {
		return nil, artifactReference{}, fmt.Errorf("%s evidence size %d is outside (0,%d]", label, len(raw), fullEvidenceMaxSize)
	}
	actual := sha256Hex(raw)
	if actual != evidence.SHA256 {
		return nil, artifactReference{}, fmt.Errorf("%s evidence SHA-256 mismatch: got %s", label, actual)
	}
	return raw, artifactReference{Path: evidence.Path, SHA256: actual}, nil
}

func validateFullEnvironment(raw []byte) error {
	var manifest map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("environment evidence is not valid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("environment evidence contains trailing JSON")
	}
	for _, key := range []string{"host", "software", "database", "datasets"} {
		value, ok := manifest[key]
		object, objectOK := value.(map[string]any)
		if !ok || !objectOK || len(object) == 0 {
			return fmt.Errorf("environment evidence requires a nonempty %s object", key)
		}
	}
	return nil
}

type fullSmallQueryMetric struct {
	P50MS         float64
	ThroughputQPS float64
}

func parseFullSmallQueryEvidence(label string, raw []byte, requireSemanticReplay bool) (fullSmallQueryMetric, error) {
	var report fullBenchmarkReport
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&report); err != nil {
		return fullSmallQueryMetric{}, fmt.Errorf("decode %s benchmark evidence: %w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark evidence contains trailing JSON", label)
	}
	if requireSemanticReplay {
		if report.SchemaVersion != 1 && report.SchemaVersion != 2 {
			return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark schema_version must be 1 or 2", label)
		}
		if report.Status != "smoke" && report.Status != "complete_controlled_local_campaign" {
			return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark status %q is not a completed campaign", label, report.Status)
		}
	} else {
		if report.SchemaVersion != 2 || report.Status != "complete_controlled_local_campaign" {
			return fullSmallQueryMetric{}, errors.New("V2 baseline must be a schema_version=2 complete_controlled_local_campaign results.json")
		}
	}
	if report.Configuration.Workload != fullSmallQueryWorkload ||
		report.Configuration.CacheStrategy != fullSmallQueryCacheStrategy ||
		report.Configuration.TaskConcurrencyMode != fullSmallQueryTaskMode {
		return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark does not use the fixed warm small-query workload", label)
	}
	var selected *fullBenchmarkCell
	for index := range report.Cells {
		one := &report.Cells[index]
		if one.Phase != "full_history_hit" || one.Concurrency != 1 {
			continue
		}
		if selected != nil {
			return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark has duplicate full_history_hit concurrency=1 cells", label)
		}
		selected = one
	}
	if selected == nil {
		return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark omitted full_history_hit concurrency=1", label)
	}
	if selected.Samples <= 0 && selected.SamplesPerTrial <= 0 {
		return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark hit cell has no measured samples", label)
	}
	if !positiveFinite(selected.LatencyMS.P50) || !positiveFinite(selected.ThroughputQPS) {
		return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark hit cell has invalid p50 or throughput", label)
	}
	if !rateIsOne(selected.QueryHistoryHitRate) || !rateIsOne(selected.FactHistoryHitRate) {
		return fullSmallQueryMetric{}, fmt.Errorf("%s benchmark hit cell is not a complete history hit", label)
	}
	if requireSemanticReplay {
		if !rateIsOne(selected.SemanticReplayHitRate) {
			return fullSmallQueryMetric{}, errors.New("V4 candidate hit cell is not 100% semantic replay")
		}
		if selected.ActualFacts == nil || *selected.ActualFacts <= 0 ||
			selected.ChargedFacts == nil || *selected.ChargedFacts != 0 {
			return fullSmallQueryMetric{}, errors.New("V4 candidate hit cell must report actual_facts>0 and charged_facts=0")
		}
	}
	return fullSmallQueryMetric{P50MS: selected.LatencyMS.P50, ThroughputQPS: selected.ThroughputQPS}, nil
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func rateIsOne(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && math.Abs(*value-1) <= 1e-12
}

func bindFullOfflineContracts(value *config) {
	value.IndexBuild = &commandMetric{
		Argv: []string{fullOfflineExecutable, "build", "-input", fullOrdersSnapshotInput,
			"-input", fullLineitemSnapshotInput, "-output-dir", "{{run_dir}}"},
		Runs: 1, TimeoutMS: 600_000, SingleProcess: true,
		ArtifactPaths: []string{"{{run_dir}}"},
		HotPaths: []string{
			"{{run_dir}}/scale-orders-v4-narrow-1/scale-orders-v4-narrow-1.hot.tgord",
			"{{run_dir}}/scale-lineitem-v4-narrow-1/scale-lineitem-v4-narrow-1.hot.tgord",
		},
	}
	value.ActivationVerification = &commandMetric{
		Argv: []string{fullOfflineExecutable, "verify", "-input", fullOrdersSnapshotInput,
			"-input", fullLineitemSnapshotInput, "-artifact-dir", fullPublishedArtifactRoot,
			"-receipt", "{{verification_receipt}}"},
		Runs: 1, TimeoutMS: 30_000, SingleProcess: true,
	}
	value.Activation = &commandMetric{
		Argv: []string{fullOfflineExecutable, "activate", "-input", fullOrdersSnapshotInput,
			"-input", fullLineitemSnapshotInput, "-artifact-dir", fullPublishedArtifactRoot,
			"-receipt", "{{verification_receipt}}", "-receipt-sha256", "{{verification_receipt_sha256}}"},
		Runs: 3, TimeoutMS: 30_000, SingleProcess: true, WarmVerified: true,
	}
	value.Artifacts = artifactConfig{
		TotalPaths: []string{fullPublishedArtifactRoot},
		HotPaths:   []string{fullOrdersPublishedHotPath, fullLineitemPublishedHotPath},
	}
}

func readFullTaskPool(path string) (fullTaskPool, error) {
	file, err := os.Open(path)
	if err != nil {
		return fullTaskPool{}, fmt.Errorf("open full task pool: %w", err)
	}
	defer file.Close()
	var pool fullTaskPool
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pool); err != nil {
		return fullTaskPool{}, fmt.Errorf("decode full task pool: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fullTaskPool{}, errors.New("full task pool contains trailing JSON")
	}
	if pool.SchemaVersion != fullTaskPoolSchema || pool.Dataset != fullTaskPoolDataset {
		return fullTaskPool{}, errors.New("full task pool has an unexpected schema or dataset")
	}
	if len(pool.Tasks) != fullTaskCount {
		return fullTaskPool{}, fmt.Errorf("full task pool has %d roots, want %d", len(pool.Tasks), fullTaskCount)
	}
	seenIDs := make(map[string]struct{}, fullTaskCount)
	seenTrials := make(map[int]struct{}, fullTaskCount)
	for _, task := range pool.Tasks {
		if strings.TrimSpace(task.TaskID) == "" || task.TaskID != strings.TrimSpace(task.TaskID) {
			return fullTaskPool{}, errors.New("full task pool contains an invalid task ID")
		}
		if _, exists := seenIDs[task.TaskID]; exists {
			return fullTaskPool{}, errors.New("full task pool reuses a task ID")
		}
		seenIDs[task.TaskID] = struct{}{}
		if task.Orders != fullOrderCount || task.Trial < 1 || task.Trial > fullTaskCount {
			return fullTaskPool{}, fmt.Errorf("full task %q has orders=%d trial=%d", task.TaskID, task.Orders, task.Trial)
		}
		if _, exists := seenTrials[task.Trial]; exists {
			return fullTaskPool{}, errors.New("full task pool reuses a trial number")
		}
		seenTrials[task.Trial] = struct{}{}
	}
	return pool, nil
}

func validateFullTemplate(value config) error {
	if value.EnvironmentManifest != nil || value.SmallQueryBaseline != nil || value.SmallQueryCandidate != nil {
		return errors.New("full template must not hard-code environment or small-query evidence")
	}
	contracts := fullCaseContracts()
	if len(value.Cases) != len(contracts) {
		return fmt.Errorf("full template must contain exactly %d workload cases", len(contracts))
	}
	for index, contract := range contracts {
		one := value.Cases[index]
		if one.ID != contract.ID || one.Shape != contract.Shape ||
			one.TargetOverlapPercent != contract.TargetOverlapPercent || one.OverlapDimension != "influence" {
			return fmt.Errorf("full template case %d does not match the fixed %s contract", index, contract.ID)
		}
		if len(one.TaskIDs) != 0 {
			return fmt.Errorf("full template case %s task_ids must be empty before provisioning", one.ID)
		}
		if len(one.SetupPlans) != len(contract.SetupPlans) {
			return fmt.Errorf("full template case %s has %d setup plans, want %d", one.ID,
				len(one.SetupPlans), len(contract.SetupPlans))
		}
		if err := matchFullPlan(one.ID+" measured plan", one.Plan, contract.Plan); err != nil {
			return err
		}
		for setupIndex, setup := range one.SetupPlans {
			if err := matchFullPlan(fmt.Sprintf("%s setup %d", one.ID, setupIndex+1), setup,
				contract.SetupPlans[setupIndex]); err != nil {
				return err
			}
		}
		if one.DirectSQL != contract.DirectSQL || len(one.DirectArgs) != 0 {
			return fmt.Errorf("full template case %s direct SQL does not match its fixed contract", one.ID)
		}
		if !equalExpectedResult(one.Expected, contract.Expected) || one.SmallQuery != contract.SmallQuery {
			return fmt.Errorf("full template case %s expected result or small-query label drifted", one.ID)
		}
	}
	return nil
}

func matchFullPlan(label string, actual json.RawMessage, expected queryplan.QueryPlan) error {
	var decoded queryplan.QueryPlan
	decoder := json.NewDecoder(strings.NewReader(string(actual)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s contains trailing JSON", label)
	}
	actualJSON, err := json.Marshal(decoded)
	if err != nil {
		return fmt.Errorf("%s normalized plan: %w", label, err)
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return fmt.Errorf("%s internal contract: %w", label, err)
	}
	actualDigest, err := canonicalJSONDigest(actualJSON)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	expectedDigest, err := canonicalJSONDigest(expectedJSON)
	if err != nil {
		return fmt.Errorf("%s internal canonical contract: %w", label, err)
	}
	if actualDigest != expectedDigest {
		return fmt.Errorf("%s does not match its fixed QueryPlan contract", label)
	}
	return nil
}

func equalExpectedResult(left, right expectedResult) bool {
	return equalOptionalInt64(left.RowCount, right.RowCount) &&
		equalOptionalInt64(left.ReleaseFacts, right.ReleaseFacts) &&
		equalOptionalInt64(left.InfluenceFacts, right.InfluenceFacts) &&
		equalOptionalInt64(left.OutcomeFacts, right.OutcomeFacts)
}

func equalOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func fullCaseContracts() []fullCaseContract {
	maxExpected := expectedResult{RowCount: fullInt64(3), ReleaseFacts: fullInt64(12),
		InfluenceFacts: fullInt64(1_035_000), OutcomeFacts: fullInt64(1)}
	maxPlan := fullMaxJoinGroupPlan([]queryplan.Filter{{Column: "scale_lineitem.l_orderkey", Op: "<=", Value: 45_000}})
	setup90 := make([]queryplan.QueryPlan, 0, 5)
	for _, bounds := range [][2]int{{1, 8_100}, {9_001, 17_100}, {18_001, 26_100}, {27_001, 35_100}, {36_001, 44_100}} {
		setup90 = append(setup90, fullMaxJoinGroupPlan([]queryplan.Filter{
			{Column: "scale_lineitem.l_orderkey", Op: ">=", Value: bounds[0]},
			{Column: "scale_lineitem.l_orderkey", Op: "<=", Value: bounds[1]},
		}))
	}
	return []fullCaseContract{
		{ID: "join-group-max-point-overlap-0", Shape: "join_group", TargetOverlapPercent: 0,
			Plan: maxPlan, DirectSQL: narrowDirectSQL, Expected: maxExpected},
		{ID: "join-group-max-point-overlap-50", Shape: "join_group", TargetOverlapPercent: 50,
			Plan: maxPlan, SetupPlans: []queryplan.QueryPlan{fullMaxJoinGroupPlan([]queryplan.Filter{
				{Column: "scale_lineitem.l_orderkey", Op: "<=", Value: 22_500},
			})}, DirectSQL: narrowDirectSQL, Expected: maxExpected},
		{ID: "join-group-max-point-overlap-90", Shape: "join_group", TargetOverlapPercent: 90,
			Plan: maxPlan, SetupPlans: setup90, DirectSQL: narrowDirectSQL, Expected: maxExpected},
		{ID: "join-group-max-point-overlap-100", Shape: "join_group", TargetOverlapPercent: 100,
			Plan: maxPlan, SetupPlans: []queryplan.QueryPlan{fullMaxJoinGroupPlan([]queryplan.Filter{
				{Column: "scale_lineitem.l_orderkey", Op: ">=", Value: 1},
				{Column: "scale_lineitem.l_orderkey", Op: "<=", Value: 45_000},
			})}, DirectSQL: narrowDirectSQL, Expected: maxExpected},
		{ID: "scan-scale-overlap-0", Shape: "scan", TargetOverlapPercent: 0,
			Plan: queryplan.QueryPlan{Product: "scale_orders", Columns: []string{"o_orderkey", "o_orderstatus"},
				Filters: []queryplan.Filter{{Column: "o_orderkey", Op: "=", Value: 1}},
				OrderBy: []queryplan.Order{{Column: "o_orderkey", Direction: "asc"}}},
			DirectSQL: fullScanDirectSQL, Expected: expectedResult{RowCount: fullInt64(1), OutcomeFacts: fullInt64(1)},
			SmallQuery: true},
		{ID: "page-scale-overlap-0", Shape: "page", TargetOverlapPercent: 0,
			Plan: queryplan.QueryPlan{Product: "scale_lineitem", Columns: []string{"l_orderkey", "l_linenumber", "l_extendedprice"},
				Filters: []queryplan.Filter{{Column: "l_orderkey", Op: "<=", Value: 20}},
				OrderBy: []queryplan.Order{{Column: "l_orderkey", Direction: "asc"},
					{Column: "l_linenumber", Direction: "asc"}}, Limit: 20, Offset: 20},
			DirectSQL: fullPageDirectSQL, Expected: expectedResult{RowCount: fullInt64(20), OutcomeFacts: fullInt64(1)},
			SmallQuery: true},
		{ID: "union-scale-overlap-0", Shape: "union", TargetOverlapPercent: 0,
			Plan: fullUnionPlan(), DirectSQL: fullUnionDirectSQL,
			Expected: expectedResult{RowCount: fullInt64(40), OutcomeFacts: fullInt64(1)}, SmallQuery: true},
	}
}

func fullMaxJoinGroupPlan(filters []queryplan.Filter) queryplan.QueryPlan {
	return queryplan.QueryPlan{
		From: &queryplan.From{Join: &queryplan.Join{
			Left:  queryplan.Scan{Product: "scale_orders", Role: "scale_orders"},
			Right: queryplan.Scan{Product: "scale_lineitem", Role: "scale_lineitem"},
			On:    []queryplan.JoinPredicate{{Left: "scale_orders.o_orderkey", Right: "scale_lineitem.l_orderkey"}},
		}},
		Columns: []string{"scale_orders.o_orderstatus"},
		Aggregates: []queryplan.Aggregate{
			{Function: "sum", Column: "scale_lineitem.l_extendedprice", Alias: "revenue"},
			{Function: "sum", Column: "scale_lineitem.l_linenumber", Alias: "line_positions"},
			{Function: "count", Column: "*", Alias: "items"},
		},
		Filters: filters, GroupBy: []string{"scale_orders.o_orderstatus"},
	}
}

func fullUnionPlan() queryplan.QueryPlan {
	return queryplan.QueryPlan{
		From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
			Role: "scale_orders", Columns: []string{"o_orderkey", "o_orderstatus"},
			Left: queryplan.Scan{Product: "scale_orders", Role: "scale_orders_left",
				Filters: []queryplan.Filter{{Column: "o_orderkey", Op: "<=", Value: 20}}},
			Right: queryplan.Scan{Product: "scale_orders", Role: "scale_orders_right",
				Filters: []queryplan.Filter{{Column: "o_orderkey", Op: ">", Value: 20},
					{Column: "o_orderkey", Op: "<=", Value: 40}}},
		}},
		Columns: []string{"scale_orders.o_orderkey", "scale_orders.o_orderstatus"},
	}
}

func fullInt64(value int64) *int64 { return &value }
