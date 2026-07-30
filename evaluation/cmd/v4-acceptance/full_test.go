package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
)

func TestFullScaleCatalogBindsReviewedDualAliasSnapshotInputs(t *testing.T) {
	root := findRepositoryRoot()
	fixture := filepath.Join(root, "evaluation", "v4-acceptance", "scale-fixture")
	inputs := make(map[string]snapshotbundle.CompilerInput)
	for _, name := range []string{"scale-orders-v4-narrow-1.json", "scale-lineitem-v4-narrow-1.json"} {
		path := filepath.Join(fixture, "snapshots", name)
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var input snapshotbundle.CompilerInput
		decoder := json.NewDecoder(file)
		decoder.DisallowUnknownFields()
		decodeErr := decoder.Decode(&input)
		closeErr := file.Close()
		if decodeErr != nil || closeErr != nil {
			t.Fatalf("decode %s: %v / %v", name, decodeErr, closeErr)
		}
		inputs[input.PublicationName] = input
	}

	for _, catalogName := range []string{"catalog.yaml", "catalog-full.yaml"} {
		logical, err := catalog.Load(filepath.Join(fixture, catalogName))
		if err != nil {
			t.Fatalf("load %s: %v", catalogName, err)
		}
		publications := make(map[string]catalog.SnapshotPublication, len(logical.SnapshotPublications))
		for _, publication := range logical.SnapshotPublications {
			publications[publication.Name] = publication
		}
		for _, product := range logical.Products {
			publication, present := publications[product.SnapshotPublication]
			input, inputPresent := inputs[product.SnapshotPublication]
			if !present || !inputPresent {
				t.Fatalf("%s product %s lacks a reviewed publication input", catalogName, product.Name)
			}
			expected := input.ExpectedDigests
			if input.CatalogSource != publication.Source || input.OrdinalSidecar != publication.OrdinalSidecar ||
				input.Snapshot.SourceNamespace != publication.SourceNamespace || input.Snapshot.Snapshot != publication.Snapshot ||
				expected.SidecarDigest != publication.SidecarDigest || expected.DictionaryDigest != publication.DictionaryDigest ||
				expected.ManifestDigest != publication.ManifestDigest || len(expected.ColdPayloadDigest) != 64 ||
				len(expected.HotIndexDigest) != 64 {
				t.Fatalf("%s publication %s differs from its reviewed compiler input", catalogName, publication.Name)
			}
			aliases := make(map[string]map[string]struct{})
			for _, field := range input.Snapshot.Fields {
				if aliases[field.Name] == nil {
					aliases[field.Name] = make(map[string]struct{})
				}
				aliases[field.Name][field.CanonicalFieldID] = struct{}{}
			}
			for _, field := range product.Fields {
				_, raw := aliases[field.Name][field.Name]
				_, qualified := aliases[field.Name][product.StableRelationRole+"."+field.Name]
				if !raw || !qualified {
					t.Fatalf("%s publication %s field %s lacks raw+role-qualified canonical aliases",
						catalogName, publication.Name, field.Name)
				}
			}
		}
	}
}

func TestPrepareFullConfigBindsTwentyIndependentRootsPerCase(t *testing.T) {
	template := loadFullTemplate(t)
	pool := validFullTaskPool()
	for left, right := 0, len(pool.Tasks)-1; left < right; left, right = left+1, right-1 {
		pool.Tasks[left], pool.Tasks[right] = pool.Tasks[right], pool.Tasks[left]
	}
	path := writeFullTaskPool(t, pool)
	prepared, err := prepareFullConfig(template, path, validFullPreparationEvidence(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Cases) != 7 || trialCount(prepared.Cases) != fullTaskCount {
		t.Fatalf("prepared matrix has %d cases / %d roots", len(prepared.Cases), trialCount(prepared.Cases))
	}
	seen := make(map[string]struct{}, fullTaskCount)
	for caseIndex, one := range prepared.Cases {
		if len(one.TaskIDs) != fullTrialsPerCase {
			t.Fatalf("case %s has %d roots", one.ID, len(one.TaskIDs))
		}
		firstTrial := caseIndex*fullTrialsPerCase + 1
		lastTrial := firstTrial + fullTrialsPerCase - 1
		if one.TaskIDs[0] != fmt.Sprintf("task-%03d", firstTrial) ||
			one.TaskIDs[len(one.TaskIDs)-1] != fmt.Sprintf("task-%03d", lastTrial) {
			t.Fatalf("case %s received unexpected ordered roots: %#v", one.ID, one.TaskIDs)
		}
		for _, taskID := range one.TaskIDs {
			if _, duplicate := seen[taskID]; duplicate {
				t.Fatalf("prepared matrix reused root %s", taskID)
			}
			seen[taskID] = struct{}{}
		}
	}
	if err := validateConfig(prepared); err != nil {
		t.Fatalf("prepared full config is invalid: %v", err)
	}
	if prepared.EnvironmentManifest == nil || prepared.SmallQueryBaseline == nil || prepared.SmallQueryCandidate == nil {
		t.Fatal("prepared matrix omitted bound environment or small-query evidence")
	}
	if prepared.SmallQueryBaseline.P50MS != 10 || prepared.SmallQueryBaseline.ThroughputQPS != 100 ||
		prepared.SmallQueryCandidate.P50MS != 10.5 || prepared.SmallQueryCandidate.ThroughputQPS != 95 {
		t.Fatalf("prepared small-query metrics were not parsed from evidence: baseline=%#v candidate=%#v",
			prepared.SmallQueryBaseline, prepared.SmallQueryCandidate)
	}
	if prepared.IndexBuild == nil || prepared.IndexBuild.Runs != 1 || prepared.IndexBuild.TimeoutMS != 600_000 ||
		!prepared.IndexBuild.SingleProcess || len(prepared.IndexBuild.ArtifactPaths) != 1 {
		t.Fatalf("prepared offline build contract = %#v", prepared.IndexBuild)
	}
	if prepared.ActivationVerification == nil || prepared.ActivationVerification.Runs != 1 ||
		prepared.ActivationVerification.TimeoutMS != 30_000 ||
		!commandContainsToken(prepared.ActivationVerification.Argv, "{{verification_receipt}}") {
		t.Fatalf("prepared strict activation-verification contract = %#v", prepared.ActivationVerification)
	}
	if prepared.Activation == nil || prepared.Activation.Runs != 3 || prepared.Activation.TimeoutMS != 30_000 ||
		!prepared.Activation.WarmVerified || !commandContainsToken(prepared.Activation.Argv, "{{verification_receipt_sha256}}") {
		t.Fatalf("prepared activation contract = %#v", prepared.Activation)
	}
	if len(prepared.Artifacts.TotalPaths) != 1 || prepared.Artifacts.TotalPaths[0] != fullPublishedArtifactRoot ||
		len(prepared.Artifacts.HotPaths) != 2 {
		t.Fatalf("prepared published artifact paths = %#v", prepared.Artifacts)
	}
}

func TestPrepareFullConfigRejectsUnboundOrNonReplayEvidence(t *testing.T) {
	template := loadFullTemplate(t)
	tasks := writeFullTaskPool(t, validFullTaskPool())
	valid := validFullPreparationEvidence(t)
	t.Run("digest mismatch", func(t *testing.T) {
		evidence := valid
		evidence.Environment.SHA256 = strings.Repeat("0", 64)
		if _, err := prepareFullConfig(template, tasks, evidence); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
			t.Fatalf("digest mismatch error = %v", err)
		}
	})
	t.Run("candidate is novelty hit but not semantic replay", func(t *testing.T) {
		evidence := valid
		evidence.Candidate = writeFullBoundEvidence(t, "candidate-non-replay.json", []byte(`{
  "schema_version":1,
  "status":"smoke",
  "configuration":{"workload":"expense_detail/sales/ordered/limit-1","cache_strategy":"warm","task_concurrency_mode":"delegated_tasks_shared_root"},
  "cells":[{"phase":"full_history_hit","concurrency":1,"samples":20,"throughput_qps":95,"latency_ms":{"p50":10.5},"query_history_hit_rate":1,"fact_history_hit_rate":1,"semantic_replay_hit_rate":0,"actual_facts":100,"charged_facts":0}]
}`))
		if _, err := prepareFullConfig(template, tasks, evidence); err == nil || !strings.Contains(err.Error(), "100% semantic replay") {
			t.Fatalf("non-replay candidate error = %v", err)
		}
	})
	t.Run("candidate charged facts", func(t *testing.T) {
		evidence := valid
		evidence.Candidate = writeFullBoundEvidence(t, "candidate-charged.json", []byte(`{
  "schema_version":1,
  "status":"smoke",
  "configuration":{"workload":"expense_detail/sales/ordered/limit-1","cache_strategy":"warm","task_concurrency_mode":"delegated_tasks_shared_root"},
  "cells":[{"phase":"full_history_hit","concurrency":1,"samples":20,"throughput_qps":95,"latency_ms":{"p50":10.5},"query_history_hit_rate":1,"fact_history_hit_rate":1,"semantic_replay_hit_rate":1,"actual_facts":100,"charged_facts":1}]
}`))
		if _, err := prepareFullConfig(template, tasks, evidence); err == nil || !strings.Contains(err.Error(), "charged_facts=0") {
			t.Fatalf("charged candidate error = %v", err)
		}
	})
}

func TestParsePublishedV2BaselineResults(t *testing.T) {
	path := filepath.Join(findRepositoryRoot(), "evaluation", "exposure-performance", "results.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metric, err := parseFullSmallQueryEvidence("V2 baseline", raw, false)
	if err != nil {
		t.Fatalf("published V2 baseline is not accepted: %v", err)
	}
	if !positiveFinite(metric.P50MS) || !positiveFinite(metric.ThroughputQPS) {
		t.Fatalf("published V2 metric = %#v", metric)
	}
}

func TestReadFullTaskPoolRejectsIncompleteOrAliasedRoots(t *testing.T) {
	t.Run("wrong cardinality", func(t *testing.T) {
		pool := validFullTaskPool()
		pool.Tasks = pool.Tasks[:fullTaskCount-1]
		if _, err := readFullTaskPool(writeFullTaskPool(t, pool)); err == nil {
			t.Fatal("139-root full pool was accepted")
		}
	})
	t.Run("duplicate trial", func(t *testing.T) {
		pool := validFullTaskPool()
		pool.Tasks[fullTaskCount-1].Trial = pool.Tasks[0].Trial
		if _, err := readFullTaskPool(writeFullTaskPool(t, pool)); err == nil {
			t.Fatal("duplicate full-pool trial was accepted")
		}
	})
	t.Run("duplicate task", func(t *testing.T) {
		pool := validFullTaskPool()
		pool.Tasks[fullTaskCount-1].TaskID = pool.Tasks[0].TaskID
		if _, err := readFullTaskPool(writeFullTaskPool(t, pool)); err == nil {
			t.Fatal("duplicate full-pool task ID was accepted")
		}
	})
}

func TestCheckedInFullTemplateMatchesPinnedSevenCaseMatrix(t *testing.T) {
	template := loadFullTemplate(t)
	if err := validateFullTemplate(template); err != nil {
		t.Fatal(err)
	}
	if len(template.Cases[2].SetupPlans) != 5 {
		t.Fatalf("90%% overlap has %d setup segments, want 5", len(template.Cases[2].SetupPlans))
	}
	shapes := map[string]bool{}
	overlaps := map[float64]bool{}
	for _, one := range template.Cases {
		shapes[one.Shape] = true
		overlaps[one.TargetOverlapPercent] = true
	}
	for _, shape := range []string{"scan", "join_group", "union", "page"} {
		if !shapes[shape] {
			t.Fatalf("full template omits shape %s", shape)
		}
	}
	for _, overlap := range []float64{0, 50, 90, 100} {
		if !overlaps[overlap] {
			t.Fatalf("full template omits overlap %.0f", overlap)
		}
	}
}

func TestFullTemplatePlansCompileAgainstScaleProducts(t *testing.T) {
	template := loadFullTemplate(t)
	products := fullTestQueryProducts()
	for _, one := range template.Cases {
		plans := append([]json.RawMessage{one.Plan}, one.SetupPlans...)
		for planIndex, raw := range plans {
			plan := decodeFullQueryPlan(t, raw)
			if plan.From == nil {
				product, found := products[plan.Product]
				if !found {
					t.Fatalf("case %s plan %d uses non-scale product %q", one.ID, planIndex, plan.Product)
				}
				if _, err := queryplan.CompileOrdinal(plan, product); err != nil {
					t.Fatalf("compile case %s plan %d: %v", one.ID, planIndex, err)
				}
				continue
			}
			if _, err := queryplan.CompileRelational(plan, products); err != nil {
				t.Fatalf("compile relational case %s plan %d: %v", one.ID, planIndex, err)
			}
		}
	}
	union := decodeFullQueryPlan(t, template.Cases[6].Plan)
	if union.From == nil || union.From.UnionDistinct == nil ||
		union.From.UnionDistinct.Role != "scale_orders" ||
		union.From.UnionDistinct.Left.Role == union.From.UnionDistinct.Right.Role {
		t.Fatalf("UNION AST lost its Catalog stable output role or distinct branch roles: %#v", union.From)
	}
}

func validFullTaskPool() fullTaskPool {
	pool := fullTaskPool{SchemaVersion: fullTaskPoolSchema, Dataset: fullTaskPoolDataset}
	for trial := 1; trial <= fullTaskCount; trial++ {
		pool.Tasks = append(pool.Tasks, fullTask{TaskID: fmt.Sprintf("task-%03d", trial), Trial: trial,
			Orders: fullOrderCount})
	}
	return pool
}

func writeFullTaskPool(t *testing.T, pool fullTaskPool) string {
	t.Helper()
	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func validFullPreparationEvidence(t *testing.T) fullPreparationEvidence {
	t.Helper()
	return fullPreparationEvidence{
		Environment: writeFullBoundEvidence(t, "environment.json", []byte(`{
  "schema_version":1,
  "host":{"cpu":"test"},
  "software":{"gateway":"test"},
  "database":{"postgresql":"16"},
  "datasets":{"scale_orders":45000,"scale_lineitem":225000}
}`)),
		Baseline: writeFullBoundEvidence(t, "baseline.json", []byte(`{
  "schema_version":2,
  "status":"complete_controlled_local_campaign",
  "configuration":{"workload":"expense_detail/sales/ordered/limit-1","cache_strategy":"warm","task_concurrency_mode":"delegated_tasks_shared_root"},
  "cells":[{"phase":"full_history_hit","concurrency":1,"samples_per_trial":200,"throughput_qps":100,"latency_ms":{"p50":10},"query_history_hit_rate":1,"fact_history_hit_rate":1}]
}`)),
		Candidate: writeFullBoundEvidence(t, "candidate.json", []byte(`{
  "schema_version":1,
  "status":"smoke",
  "configuration":{"workload":"expense_detail/sales/ordered/limit-1","cache_strategy":"warm","task_concurrency_mode":"delegated_tasks_shared_root"},
  "cells":[{"phase":"full_history_hit","concurrency":1,"samples":200,"throughput_qps":95,"latency_ms":{"p50":10.5},"query_history_hit_rate":1,"fact_history_hit_rate":1,"semantic_replay_hit_rate":1,"actual_facts":1000,"charged_facts":0}]
}`)),
	}
}

func writeFullBoundEvidence(t *testing.T, name string, raw []byte) fullBoundArtifact {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return fullBoundArtifact{Path: path, SHA256: sha256Hex(raw)}
}

func loadFullTemplate(t *testing.T) config {
	t.Helper()
	path := filepath.Join(findRepositoryRoot(), "evaluation", "v4-acceptance", "full-matrix.template.json")
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var result config
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func decodeFullQueryPlan(t *testing.T, raw json.RawMessage) queryplan.QueryPlan {
	t.Helper()
	var result queryplan.QueryPlan
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func fullTestQueryProducts() map[string]queryplan.Product {
	aggregates := map[string]struct{}{"sum": {}, "count": {}, "min": {}, "max": {}}
	digest := strings.Repeat("a", 64)
	return map[string]queryplan.Product{
		"scale_orders": {
			Name: "scale_orders", StableRole: "scale_orders", SourceNamespace: "evaluation.scale_orders",
			Snapshot: "exposure-scale-2026-v4-narrow-1", StableEntityKey: []string{"o_orderkey"},
			Columns:           map[string]struct{}{"dataset_partition": {}, "o_orderkey": {}, "o_orderstatus": {}},
			ColumnTypes:       map[string]string{"dataset_partition": "smallint", "o_orderkey": "bigint", "o_orderstatus": "smallint"},
			AllowedAggregates: aggregates, RequiredEvidence: []string{"dataset_partition"},
			SnapshotPublication: "scale-orders-v4-narrow-1", SidecarManifestDigest: digest,
		},
		"scale_lineitem": {
			Name: "scale_lineitem", StableRole: "scale_lineitem", SourceNamespace: "evaluation.scale_lineitem",
			Snapshot: "exposure-scale-2026-v4-narrow-1", StableEntityKey: []string{"l_orderkey", "l_linenumber"},
			Columns: map[string]struct{}{"dataset_partition": {}, "l_orderkey": {}, "l_linenumber": {}, "l_extendedprice": {}},
			ColumnTypes: map[string]string{"dataset_partition": "smallint", "l_orderkey": "bigint",
				"l_linenumber": "integer", "l_extendedprice": "numeric"},
			AllowedAggregates: aggregates, RequiredEvidence: []string{"dataset_partition"},
			SnapshotPublication: "scale-lineitem-v4-narrow-1", SidecarManifestDigest: digest,
		},
	}
}
