package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestTasksForCellRequiresExactDistinctPool(t *testing.T) {
	pool := taskPoolFile{
		SchemaVersion: schemaVersion,
		Experiments: map[string]map[string][]string{
			"tpch_sf1": {
				"2": {"task-a", "task-b"},
				"3": {"task-a", "task-a", "task-c"},
			},
		},
	}

	tasks, err := tasksForCell(pool, "tpch_sf1", 2, taskConcurrencyDistinct)
	if err != nil {
		t.Fatalf("valid pool rejected: %v", err)
	}
	if !reflect.DeepEqual(tasks, []string{"task-a", "task-b"}) {
		t.Fatalf("unexpected tasks: %#v", tasks)
	}
	tasks[0] = "mutated"
	if pool.Experiments["tpch_sf1"]["2"][0] != "task-a" {
		t.Fatal("tasksForCell returned the task-pool backing slice")
	}

	if _, err := tasksForCell(pool, "tpch_sf1", 1, taskConcurrencyDistinct); err == nil || !strings.Contains(err.Error(), "exactly 1") {
		t.Fatalf("missing exact-count failure, got %v", err)
	}
	if _, err := tasksForCell(pool, "tpch_sf1", 3, taskConcurrencyDistinct); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("missing duplicate failure, got %v", err)
	}
}

func TestTasksForCellSupportsSameTaskMode(t *testing.T) {
	pool := taskPoolFile{
		SchemaVersion: schemaVersion,
		Experiments: map[string]map[string][]string{
			"tpch_sf1": {
				"4": {"shared-task"},
			},
		},
	}

	tasks, err := tasksForCell(pool, "tpch_sf1", 4, taskConcurrencySameTask)
	if err != nil {
		t.Fatalf("valid same-task pool rejected: %v", err)
	}
	if !reflect.DeepEqual(tasks, []string{"shared-task", "shared-task", "shared-task", "shared-task"}) {
		t.Fatalf("same-task pool was not expanded per worker: %#v", tasks)
	}
	if err := reserveUniqueTasks(map[string]string{}, tasks, "tpch/c4"); err != nil {
		t.Fatalf("same-cell duplicate should not be cross-cell reuse: %v", err)
	}
}

func TestBuildCellScheduleIsSeededCompletePermutation(t *testing.T) {
	cfg := suiteConfig{
		Seed:          42,
		BaselineOrder: append([]string(nil), requiredBaselines...),
		Concurrency:   []int{1, 2},
		Experiments: []experiment{
			{ID: "tpch_sf1"},
			{ID: "tpcds_sf1"},
		},
	}

	first := buildCellSchedule(cfg)
	second := buildCellSchedule(cfg)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same seed produced different schedules:\n%#v\n%#v", first, second)
	}
	expectedCells := len(cfg.Experiments) * len(cfg.Concurrency) * len(cfg.BaselineOrder)
	if len(first) != expectedCells {
		t.Fatalf("schedule length=%d, want %d", len(first), expectedCells)
	}
	seen := make(map[[3]string]bool)
	for index, item := range first {
		if item.Order != index+1 {
			t.Fatalf("order at index %d = %d", index, item.Order)
		}
		key := [3]string{item.Experiment, item.Baseline, strconv.Itoa(item.Concurrency)}
		if seen[key] {
			t.Fatalf("duplicate scheduled cell: %#v", item)
		}
		seen[key] = true
	}
}

func TestLoadSuiteRequiresColdCacheResetEnv(t *testing.T) {
	config := map[string]any{
		"schema_version":           schemaVersion,
		"name":                     "cold-cache-test",
		"mode":                     "smoke",
		"seed":                     42,
		"workload_lineage":         "TPC-derived",
		"ordering_strategy":        orderingSeededRandom,
		"cache_strategy":           cacheStrategyCold,
		"task_concurrency_mode":    taskConcurrencyDistinct,
		"environment_manifest_env": "EVAL_ENVIRONMENT_MANIFEST",
		"warmup_runs_per_worker":   1,
		"measured_runs_per_worker": 1,
		"concurrency":              []int{1},
		"baseline_order":           append([]string(nil), requiredBaselines...),
		"max_result_rows":          10,
		"statement_timeout":        "1s",
		"require_resource_metrics": false,
		"experiments": []map[string]any{{
			"id":                      "tpch_sf1",
			"family":                  "tpch",
			"scale_factor":            1,
			"workload":                "../workloads/tpch/manifest.json",
			"direct_dsn_env":          "DIRECT_DSN",
			"native_dsn_env":          "NATIVE_DSN",
			"ast_url_env":             "AST_URL",
			"taskgate_url_env":        "TASKGATE_URL",
			"taskgate_token_env":      "TASKGATE_TOKEN",
			"taskgate_tasks_file_env": "TASKGATE_TASKS",
			"metrics_probe_env":       map[string]string{},
		}},
	}
	path := writeTempConfig(t, config)
	if _, _, err := loadSuite(path); err == nil || !strings.Contains(err.Error(), "cache_reset_env") {
		t.Fatalf("cold cache config without reset env was not rejected: %v", err)
	}

	experiments := config["experiments"].([]map[string]any)
	experiments[0]["cache_reset_env"] = map[string]string{
		baselineDirect:       "RESET_DIRECT",
		baselineNativeView:   "RESET_NATIVE",
		baselineASTOnly:      "RESET_AST",
		baselineFullTaskGate: "RESET_FULL",
	}
	path = writeTempConfig(t, config)
	if _, _, err := loadSuite(path); err != nil {
		t.Fatalf("cold cache config with reset env was rejected: %v", err)
	}
}

func TestReserveUniqueTasksRejectsReuseAcrossCells(t *testing.T) {
	seen := make(map[string]string)
	if err := reserveUniqueTasks(seen, []string{"task-a", "task-b"}, "sf1/tpch/c2"); err != nil {
		t.Fatalf("first cell rejected: %v", err)
	}
	err := reserveUniqueTasks(seen, []string{"task-c", "task-a"}, "sf10/tpch/c2")
	if err == nil || !strings.Contains(err.Error(), "sf1/tpch/c2") || !strings.Contains(err.Error(), "sf10/tpch/c2") {
		t.Fatalf("cross-cell reuse was not diagnosed: %v", err)
	}
}

func writeTempConfig(t *testing.T, value map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "suite.json")
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRepositoryRelativePath(t *testing.T) {
	path, err := repositoryRelativePath("/workspace/evaluation/config/sf1.json")
	if err != nil || path != "evaluation/config/sf1.json" {
		t.Fatalf("repository path rejected: path=%q err=%v", path, err)
	}
	for _, invalid := range []string{"relative/path", "/tmp/probe", "/workspace/../tmp/probe", "/workspace"} {
		if _, err := repositoryRelativePath(invalid); err == nil {
			t.Errorf("repositoryRelativePath accepted %q", invalid)
		}
	}
}

func TestProvenancePatterns(t *testing.T) {
	if !sha256Pattern.MatchString(strings.Repeat("a", 64)) {
		t.Fatal("valid lowercase SHA-256 rejected")
	}
	for _, invalid := range []string{strings.Repeat("a", 63), strings.Repeat("A", 64), "replace-me"} {
		if sha256Pattern.MatchString(invalid) {
			t.Errorf("invalid SHA-256 accepted: %q", invalid)
		}
	}
	if !campaignIDPattern.MatchString("full-20260722T010203Z") || campaignIDPattern.MatchString("../escape") {
		t.Fatal("campaign identifier validation is not path-safe")
	}
}
