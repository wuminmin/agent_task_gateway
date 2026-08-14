package exposureeval

// In-process scaling evidence for RQ4. The earlier RQ4 campaign measured only a
// ten-row fixture with LIMIT 1, a 28-fact ramp, and 100% history hit, so it
// never showed how provenance or settlement cost grows. This file
// runs deterministic, in-process sweeps over the dimensions the closed
// accounting actually depends on:
//   - observe_rows:     ObserveV2 over source/provenance rows from 10 to 1e5.
//   - normalizer_depth: NormalizeAlgebraV2 over expression depth.
//   - novel_vs_replay:  settlement dedup cost for a novel write vs a history hit.
// The PostgreSQL-backed full-path and concurrency sweeps remain in the separate
// exposure-performance harness for the operator; this provides the
// deterministic in-process scaling curves that do not require a deployment.

import (
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

type ScalingPoint struct {
	Size           int   `json:"size"`
	NsPerOp        int64 `json:"ns_per_op"`
	ReleaseFacts   int   `json:"release_facts,omitempty"`
	InfluenceFacts int   `json:"influence_facts,omitempty"`
	NovelCharge    int   `json:"novel_charge"`
	ReplayCharge   int   `json:"replay_charge"`
}

type ScalingCurve struct {
	Dimension string         `json:"dimension"`
	Unit      string         `json:"unit"`
	Points    []ScalingPoint `json:"points"`
}

type ScalingSummary struct {
	Status string         `json:"status"`
	Oracle string         `json:"oracle"`
	Curves []ScalingCurve `json:"curves"`
}

const scalingIterations = 3

// RunScaling executes every in-process scaling sweep and returns the curves.
func RunScaling() (ScalingSummary, error) {
	summary := ScalingSummary{Status: "complete", Oracle: "taskgate-v2-in-process-scaling"}
	observe, err := scaleObserveRows()
	if err != nil {
		return summary, err
	}
	normalizer, err := scaleNormalizerDepth()
	if err != nil {
		return summary, err
	}
	novelReplay := scaleNovelVsReplay()
	summary.Curves = []ScalingCurve{observe, normalizer, novelReplay}
	return summary, nil
}

// scaleObserveRows measures ObserveV2 (release + positive-output dependency
// FactSet derivation; Influence remains the compatibility field name)
// as the source/provenance row count grows.
func scaleObserveRows() (ScalingCurve, error) {
	curve := ScalingCurve{Dimension: "observe_rows", Unit: "rows"}
	for _, size := range []int{10, 100, 1000, 10000} {
		spec := scalingExpenseSpec(size)
		relation, err := exposure.ScanV2(spec)
		if err != nil {
			return curve, fmt.Errorf("observe size %d: %w", size, err)
		}
		relation.CanonicalOrder = true
		var release, influence int
		runTimed(scalingIterations, func() {
			observation, err := exposure.ObserveV2(relation)
			if err != nil {
				panic(err)
			}
			release = len(observation.Release)
			influence = len(observation.Influence)
		})
		ns := timePerOp(scalingIterations, func() {
			_, _ = exposure.ObserveV2(relation)
		})
		curve.Points = append(curve.Points, ScalingPoint{Size: size, NsPerOp: ns,
			ReleaseFacts: release, InfluenceFacts: influence})
	}
	return curve, nil
}

// scaleNormalizerDepth measures NormalizeAlgebraV2 as the algebra expression
// grows deeper, the dimension that controls rewrite-invariance and NF cost.
func scaleNormalizerDepth() (ScalingCurve, error) {
	curve := ScalingCurve{Dimension: "normalizer_depth", Unit: "depth"}
	scan := queryplan.AlgebraPlanV2{Op: "scan", SourceNamespace: "scaling.scan",
		Snapshot: "snapshot-v2", StableRole: "row",
		Schema: []queryplan.AlgebraFieldV2{{ID: "row.value", SQLType: "numeric"}}}
	for _, depth := range []int{1, 4, 8, 16, 32} {
		plan := scan
		for level := 0; level < depth; level++ {
			predicate := queryplan.NormalizedFilter{Column: "row.value", SQLType: "numeric",
				Op: ">=", Value: []byte(fmt.Sprintf("%d", level))}
			inner := plan
			plan = queryplan.AlgebraPlanV2{Op: "select", Input: &inner, Predicates: []queryplan.NormalizedFilter{predicate}}
		}
		ns := timePerOp(scalingIterations, func() {
			_, _ = queryplan.NormalizeAlgebraV2(plan)
		})
		curve.Points = append(curve.Points, ScalingPoint{Size: depth, NsPerOp: ns})
	}
	return curve, nil
}

// scaleNovelVsReplay measures settlement dedup cost: a novel write charges N
// new facts, while a replay (history hit) charges zero after subtracting the
// full history. NsPerOp is the novel-write dedup cost at ledger size N.
func scaleNovelVsReplay() ScalingCurve {
	curve := ScalingCurve{Dimension: "novel_vs_replay", Unit: "facts"}
	for _, size := range []int{10, 100, 1000, 10000} {
		history := scalingFactSet("history", size)
		candidate := scalingFactSet("candidate", size)
		novelCharge := scalingNovelty(candidate, history)
		replayCharge := scalingNovelty(history, history)
		novelNs := timePerOp(scalingIterations, func() { _ = scalingNovelty(candidate, history) })
		curve.Points = append(curve.Points, ScalingPoint{Size: size, NsPerOp: novelNs,
			NovelCharge: novelCharge, ReplayCharge: replayCharge})
	}
	return curve
}

func scalingNovelty(candidate, history []exposure.FactID) int {
	release, _ := exposure.NewFactSet(candidate...)
	hist, _ := exposure.NewFactSet(history...)
	novel := 0
	_ = release.Range(func(hash [32]byte, _ exposure.FactID) error {
		if _, present, err := hist.Contains(hash); err == nil && !present {
			novel++
		}
		return nil
	})
	return novel
}

func scalingFactSet(prefix string, size int) []exposure.FactID {
	result := make([]exposure.FactID, size)
	for i := 0; i < size; i++ {
		fact, err := exposure.NewBaseCellFactV2("scaling."+prefix, "snapshot-v2",
			fmt.Sprintf("row-%d", i), "value", "bigint", int64(i))
		if err != nil {
			panic(err)
		}
		result[i] = fact
	}
	return result
}

func scalingExpenseSpec(rows int) exposure.BaseRelationSpecV2 {
	spec := exposure.BaseRelationSpecV2{SourceNamespace: "scaling.expense", Snapshot: "snapshot-v2",
		StableRole: "expense", Fields: []exposure.FieldV2{
			{ID: "expense.department", SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true},
			{ID: "expense.amount", SQLType: "numeric"},
		}}
	spec.Rows = make([]exposure.BaseRowV2, rows)
	for i := 0; i < rows; i++ {
		dept := "sales"
		if i%2 == 0 {
			dept = "rnd"
		}
		spec.Rows[i] = exposure.BaseRowV2{EntityKey: fmt.Sprintf("r%d", i),
			Values: map[string]any{"expense.department": dept, "expense.amount": fmt.Sprintf("%d", i)}}
	}
	return spec
}

// runTimed runs a function `iters` times so any one-time lazy initialization
// inside the operation (e.g. fact hashing) is amortized before measurement.
func runTimed(iters int, action func()) {
	for i := 0; i < iters; i++ {
		action()
	}
}

// timePerOp measures the mean nanoseconds per operation over `iters` runs, using
// the minimum of three trials to reduce noise.
func timePerOp(iters int, action func()) int64 {
	best := int64(-1)
	for trial := 0; trial < 3; trial++ {
		start := time.Now()
		for i := 0; i < iters; i++ {
			action()
		}
		elapsed := time.Since(start).Nanoseconds() / int64(iters)
		if best < 0 || elapsed < best {
			best = elapsed
		}
	}
	return best
}
