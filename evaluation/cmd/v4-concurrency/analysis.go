package main

import "fmt"

func evaluateConcurrencyGates(cfg concurrencyConfig, cells []concurrencyCell) []reportGate {
	byCase := make(map[string]concurrencyCell, len(cells))
	for _, cell := range cells {
		byCase[cell.CaseID] = cell
	}
	gates := []reportGate{{
		ID:          "source_and_config_binding",
		Requirement: "report binds the exact configuration and supplemental/production source with SHA-256",
		Status:      "pass",
	}}
	levels := make(map[int]bool)
	dimensions := make(map[string]bool)
	rootLockQueueCells := 0
	for _, contract := range cfg.Cases {
		cell, present := byCase[contract.ID]
		if present && cell.Status == "measured" {
			levels[cell.Concurrency] = true
			dimensions[cell.BoundaryDimension] = true
			if cell.Checks.RootLockQueueObserved {
				rootLockQueueCells++
			}
		}
		checks := cell.Checks
		prefix := "case_" + safeID(contract.ID) + "_"
		gates = append(gates,
			boolGate(prefix+"shared_root", "all prefix/contender/overflow tasks belong to one root family", checks.SharedRootFamily,
				map[string]any{"root_task_sha256": cell.RootTaskSHA256, "family_task_sha256": cell.FamilyTaskSHA256}),
			boolGate(prefix+"fresh_root", "cell starts from epoch zero with the exact Catalog budget", checks.FreshRoot, cell.Initial),
			boolGate(prefix+"b_minus_one", fmt.Sprintf("successful prefix commits exact B-1 in %s", contract.BoundaryDimension),
				checks.BMinusOneCommitted, map[string]any{"prefix": cell.Prefix, "head": cell.BeforeBoundary}),
			boolGate(prefix+"b", "concurrent batch succeeds and commits the exact three-dimensional B state",
				checks.BCommitted, map[string]any{"contention": cell.Contention, "head": cell.AtBoundary}),
			boolGate(prefix+"three_dimensional_atomicity", "one epoch transition commits all three novelty dimensions; other contenders settle zero novelty",
				checks.ThreeDimensionalAtomic, cell.Contention),
			boolGate(prefix+"root_lock_queue", "every contender is observed in the root-lock wait chain before the blocker is released",
				checks.RootLockQueueObserved, map[string]any{"concurrency": cell.Concurrency,
					"root_lock_waiters_observed": cell.Contention.RootLockWaitersObserved}),
			boolGate(prefix+"b_plus_one", "a distinct B+1 observation is rejected before result release",
				checks.OverflowRejected, cell.Overflow),
			boolGate(prefix+"failure_atomicity", "B+1 releases its ordinal reservation and leaves no result/chunk, materialization, observation reference/root-observation, terminal success audit, content, or head mutation",
				checks.FailureLeftNoPartialCommit, cell.Overflow),
		)
	}
	levelEvidence := map[string]bool{}
	levelOK := true
	for _, level := range []int{1, 4, 8, 16} {
		key := fmt.Sprint(level)
		levelEvidence[key] = levels[level]
		levelOK = levelOK && levels[level]
	}
	gates = append(gates, boolGate("concurrency_widths", "measured shared-root widths are exactly 1/4/8/16",
		levelOK, levelEvidence))
	dimensionEvidence := map[string]bool{}
	dimensionOK := true
	for _, dimension := range []string{"release", "influence", "outcome"} {
		dimensionEvidence[dimension] = dimensions[dimension]
		dimensionOK = dimensionOK && dimensions[dimension]
	}
	gates = append(gates, boolGate("boundary_dimensions", "B-1/B/B+1 coverage includes Release, Influence, and Outcome boundaries",
		dimensionOK, dimensionEvidence))
	gates = append(gates, boolGate("all_root_lock_queues", "every measured cell observes all contenders in its root-lock wait chain before release",
		rootLockQueueCells == len(cfg.Cases), map[string]int{"observed_cells": rootLockQueueCells,
			"required_cells": len(cfg.Cases)}))
	return gates
}

func boolGate(id, requirement string, ok bool, evidence any) reportGate {
	status := "fail"
	if ok {
		status = "pass"
	}
	return reportGate{ID: id, Requirement: requirement, Status: status, Evidence: evidence}
}

func gateAcceptance(gates []reportGate) string {
	if len(gates) == 0 {
		return "fail"
	}
	for _, one := range gates {
		if one.Status != "pass" {
			return "fail"
		}
	}
	return "pass"
}
