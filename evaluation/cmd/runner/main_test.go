package main

import (
	"reflect"
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

	tasks, err := tasksForCell(pool, "tpch_sf1", 2)
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

	if _, err := tasksForCell(pool, "tpch_sf1", 1); err == nil || !strings.Contains(err.Error(), "exactly 1") {
		t.Fatalf("missing exact-count failure, got %v", err)
	}
	if _, err := tasksForCell(pool, "tpch_sf1", 3); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("missing duplicate failure, got %v", err)
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
