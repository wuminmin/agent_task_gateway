package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const (
	narrowTaskPoolSchema  = 1
	narrowTaskPoolDataset = "deterministic-tpch-derived-orders-lineitem-v1"
	narrowTaskCount       = 20
	narrowOrderCount      = 45_000
	narrowDirectSQL       = "SELECT o.o_orderstatus, sum(l.l_extendedprice) AS revenue, sum(l.l_linenumber) AS line_positions, count(*) AS items FROM reporting.scale_orders AS o JOIN reporting.scale_lineitem AS l ON l.l_orderkey = o.o_orderkey WHERE o.dataset_partition = 1 AND l.dataset_partition = 1 AND l.l_orderkey <= 45000 GROUP BY o.o_orderstatus ORDER BY o.o_orderstatus"
	narrowPlanContract    = `{"from":{"join":{"left":{"product":"scale_orders","role":"scale_orders"},"right":{"product":"scale_lineitem","role":"scale_lineitem"},"on":[{"left":"scale_orders.o_orderkey","right":"scale_lineitem.l_orderkey"}]}},"columns":["scale_orders.o_orderstatus"],"aggregates":[{"function":"sum","column":"scale_lineitem.l_extendedprice","alias":"revenue"},{"function":"sum","column":"scale_lineitem.l_linenumber","alias":"line_positions"},{"function":"count","column":"*","alias":"items"}],"filters":[{"column":"scale_lineitem.l_orderkey","op":"<=","value":45000}],"group_by":["scale_orders.o_orderstatus"]}`
)

type narrowTaskPool struct {
	SchemaVersion int          `json:"schema_version"`
	Dataset       string       `json:"dataset"`
	Tasks         []narrowTask `json:"tasks"`
}

type narrowTask struct {
	TaskID string `json:"task_id"`
	Trial  int    `json:"trial"`
	Orders int    `json:"orders"`
}

// prepareNarrowConfig consumes the credential-free task-pool artifact emitted
// by exposure-bench scale-bootstrap. The bootstrap operation has already used
// the public request_data_task and OA submit/approve flows and waited for every
// root to become ACTIVE. This step only binds those opaque IDs to the immutable
// maximum-point workload template; it does not execute the measured query.
func prepareNarrowConfig(template config, taskPoolPath string) (config, error) {
	pool, err := readNarrowTaskPool(taskPoolPath)
	if err != nil {
		return config{}, err
	}
	if err := validateNarrowTemplate(template); err != nil {
		return config{}, err
	}
	sort.Slice(pool.Tasks, func(i, j int) bool { return pool.Tasks[i].Trial < pool.Tasks[j].Trial })
	template.Cases[0].TaskIDs = make([]string, 0, len(pool.Tasks))
	for _, task := range pool.Tasks {
		template.Cases[0].TaskIDs = append(template.Cases[0].TaskIDs, task.TaskID)
	}
	return template, nil
}

func readNarrowTaskPool(path string) (narrowTaskPool, error) {
	file, err := os.Open(path)
	if err != nil {
		return narrowTaskPool{}, fmt.Errorf("open narrow task pool: %w", err)
	}
	defer file.Close()
	var pool narrowTaskPool
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pool); err != nil {
		return narrowTaskPool{}, fmt.Errorf("decode narrow task pool: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return narrowTaskPool{}, errors.New("narrow task pool contains trailing JSON")
	}
	if pool.SchemaVersion != narrowTaskPoolSchema || pool.Dataset != narrowTaskPoolDataset {
		return narrowTaskPool{}, errors.New("narrow task pool has an unexpected schema or dataset")
	}
	if len(pool.Tasks) != narrowTaskCount {
		return narrowTaskPool{}, fmt.Errorf("narrow task pool has %d roots, want %d", len(pool.Tasks), narrowTaskCount)
	}
	seenIDs := make(map[string]struct{}, narrowTaskCount)
	seenTrials := make(map[int]struct{}, narrowTaskCount)
	for _, task := range pool.Tasks {
		if strings.TrimSpace(task.TaskID) == "" || task.TaskID != strings.TrimSpace(task.TaskID) {
			return narrowTaskPool{}, errors.New("narrow task pool contains an invalid task ID")
		}
		if _, exists := seenIDs[task.TaskID]; exists {
			return narrowTaskPool{}, errors.New("narrow task pool reuses a task ID")
		}
		seenIDs[task.TaskID] = struct{}{}
		if task.Orders != narrowOrderCount || task.Trial < 1 || task.Trial > narrowTaskCount {
			return narrowTaskPool{}, fmt.Errorf("narrow task %q has orders=%d trial=%d", task.TaskID, task.Orders, task.Trial)
		}
		if _, exists := seenTrials[task.Trial]; exists {
			return narrowTaskPool{}, errors.New("narrow task pool reuses a trial number")
		}
		seenTrials[task.Trial] = struct{}{}
	}
	return pool, nil
}

func validateNarrowTemplate(value config) error {
	if len(value.Cases) != 1 {
		return errors.New("narrow template must contain exactly one workload case")
	}
	one := value.Cases[0]
	if one.ID != "join-group-max-point-overlap-0" || one.Shape != "join_group" ||
		one.TargetOverlapPercent != 0 || one.OverlapDimension != "influence" || len(one.SetupPlans) != 0 {
		return errors.New("narrow template does not describe the fresh-root Join-Group 0% overlap cell")
	}
	if one.Expected.RowCount == nil || *one.Expected.RowCount != 3 ||
		one.Expected.ReleaseFacts == nil || *one.Expected.ReleaseFacts != 12 ||
		one.Expected.InfluenceFacts == nil || *one.Expected.InfluenceFacts != 1_035_000 ||
		one.Expected.OutcomeFacts == nil || *one.Expected.OutcomeFacts != 1 {
		return errors.New("narrow template does not bind the exact maximum-point cardinalities")
	}
	if len(one.TaskIDs) != 0 {
		return errors.New("narrow template task_ids must be empty before provisioning")
	}
	planDigest, err := canonicalJSONDigest(one.Plan)
	if err != nil {
		return fmt.Errorf("narrow plan: %w", err)
	}
	wantPlanDigest, err := canonicalJSONDigest(json.RawMessage(narrowPlanContract))
	if err != nil {
		return fmt.Errorf("internal narrow plan contract: %w", err)
	}
	if planDigest != wantPlanDigest || one.DirectSQL != narrowDirectSQL || len(one.DirectArgs) != 0 {
		return errors.New("narrow template plan and direct SQL do not match the fixed maximum-point contract")
	}
	return nil
}

func canonicalJSONDigest(raw json.RawMessage) (string, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("trailing JSON")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return sha256Hex(encoded), nil
}
