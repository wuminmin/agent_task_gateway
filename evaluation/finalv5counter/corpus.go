// Package finalv5counter is the (a) comparator-arms corpus: the frozen
// 100-step adaptive RLS trace executed under four a-priori budgeters - the
// exact three-dimension floors, a cumulative row counter, a query counter,
// and a release-set-only counter - in three frozen orderings. Every per-step
// accept/refuse outcome is derived a priori by greedy set simulation over the
// independent oracle observations; the pilot's job is to show the real
// system realizing exactly this table.
package finalv5counter

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
)

const (
	CorpusID      = "taskgate-final-v5-counter-comparator-corpus-v1"
	SchemaVersion = 1
	WorkloadID    = "counter-comparator-v1"
	// Product is the counter experiment's own immutable projection of the
	// frozen ten-row expense fixture.
	Product = "final_v5_counter_expense_detail"

	// The a-priori budgets: the same 70 percent floor recipe applied to each
	// arm's own counter (docs/p8, frozen 2026-09-01 before implementation).
	ExactMaxRelease    int64 = 7
	ExactMaxDependency int64 = 12
	ExactMaxOutcome    int64 = 18
	RowsCounterBudget  int64 = 175 // floor(0.7 * 250 released rows)
	QueryCounterBudget int64 = 70  // floor(0.7 * 100 steps)
	// EffectivelyUnlimited disables a dimension without disabling its
	// accounting.
	EffectivelyUnlimited int64 = 1000000
	// ResourceRows/ResourceQueries are the non-experimental resource ceilings
	// of the arms that do not meter that resource.
	ResourceRows    int64 = 500
	ResourceQueries int64 = 110

	ShuffleSeed int64 = 20260901
)

// Arms in execution order; each is one budget profile.
var Arms = [4]string{"exact", "rows", "queries", "release"}

// Orderings in execution order.
var Orderings = [3]string{"natural", "shuffled-v1", "novelty-first-v1"}

// ArmProfiles binds each arm to its Catalog budget profile.
var ArmProfiles = map[string]string{
	"exact": "final-v5-counter-exact-v1", "rows": "final-v5-counter-rows-v1",
	"queries": "final-v5-counter-queries-v1", "release": "final-v5-counter-release-v1",
}

// StepOutcome is one step's a-priori expectation under one arm and ordering.
type StepOutcome struct {
	Position     int    `json:"position"`     // 1-based position in the ordering
	SourceIndex  int    `json:"source_index"` // the step's index in the natural corpus
	StepID       string `json:"step_id"`
	Accepted     bool   `json:"accepted"`
	RefusalKind  string `json:"refusal_kind,omitempty"` // exposure | resource | archived
	ReleasedRows int64  `json:"released_rows"`
	NovelRelease int64  `json:"novel_release"`
	NovelDep     int64  `json:"novel_dependency"`
	NovelOutcome int64  `json:"novel_outcome"`
}

// ArmTrace is the full a-priori outcome table of one arm x ordering cell.
type ArmTrace struct {
	Arm              string        `json:"arm"`
	Ordering         string        `json:"ordering"`
	BudgetProfile    string        `json:"budget_profile"`
	Steps            []StepOutcome `json:"steps"`
	FirstRefusal     int           `json:"first_refusal,omitempty"`
	AcceptedSteps    int           `json:"accepted_steps"`
	RefusedSteps     int           `json:"refused_steps"`
	DistinctRelease  int64         `json:"distinct_release"`
	DistinctDep      int64         `json:"distinct_dependency"`
	DistinctOutcome  int64         `json:"distinct_outcome"`
	ReleasedRowTotal int64         `json:"released_row_total"`
}

// Manifest is the frozen corpus document.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	CorpusID      string `json:"corpus_id"`
	WorkloadID    string `json:"workload_id"`
	Product       string `json:"product"`
	RLSCorpusID   string `json:"rls_corpus_id"`
	// Orderings maps each ordering to its permutation of natural indices.
	OrderingIndexes map[string][]int `json:"ordering_indexes"`
	Traces          []ArmTrace       `json:"traces"`
}

//go:embed corpus-v1.json
var corpusBytes []byte

func CorpusSHA256() string {
	digest := sha256.Sum256(corpusBytes)
	return hex(digest[:])
}

func hex(value []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 2*len(value))
	for index, b := range value {
		out[2*index], out[2*index+1] = digits[b>>4], digits[b&0xf]
	}
	return string(out)
}

// EncodeManifest is the frozen byte encoding.
func EncodeManifest(manifest Manifest) ([]byte, error) {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// Load parses and validates the embedded frozen corpus.
func Load() (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(corpusBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode embedded counter corpus: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID ||
		manifest.WorkloadID != WorkloadID || manifest.Product != Product ||
		manifest.RLSCorpusID != finalv5rls.CorpusID ||
		len(manifest.Traces) != len(Arms)*len(Orderings) {
		return Manifest{}, fmt.Errorf("embedded counter corpus disagrees with the frozen constants")
	}
	return manifest, nil
}

// Trace returns one arm x ordering cell.
func (manifest Manifest) Trace(arm, ordering string) (ArmTrace, error) {
	for _, trace := range manifest.Traces {
		if trace.Arm == arm && trace.Ordering == ordering {
			return trace, nil
		}
	}
	return ArmTrace{}, fmt.Errorf("counter corpus lacks trace %s/%s", arm, ordering)
}

// BuildManifest derives the frozen corpus from the RLS trace and the frozen
// budgets by greedy simulation.
func BuildManifest() (Manifest, error) {
	rls, err := finalv5rls.Load()
	if err != nil {
		return Manifest{}, err
	}
	steps, err := rls.Trace()
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{SchemaVersion: SchemaVersion, CorpusID: CorpusID, WorkloadID: WorkloadID,
		Product: Product, RLSCorpusID: finalv5rls.CorpusID,
		OrderingIndexes: map[string][]int{}}
	orderings := map[string][]int{
		"natural":          naturalOrder(len(steps)),
		"shuffled-v1":      shuffledOrder(len(steps)),
		"novelty-first-v1": noveltyFirstOrder(steps),
	}
	for _, name := range Orderings {
		manifest.OrderingIndexes[name] = orderings[name]
	}
	for _, ordering := range Orderings {
		for _, arm := range Arms {
			trace, err := simulateArm(arm, ordering, orderings[ordering], steps)
			if err != nil {
				return Manifest{}, err
			}
			manifest.Traces = append(manifest.Traces, trace)
		}
	}
	return manifest, nil
}

func naturalOrder(length int) []int {
	order := make([]int, length)
	for index := range order {
		order[index] = index
	}
	return order
}

// shuffledOrder is a deterministic Fisher-Yates permutation driven by a
// SplitMix64 stream seeded with ShuffleSeed; no runtime PRNG library is
// depended on, so the permutation can never drift with a toolchain.
func shuffledOrder(length int) []int {
	order := naturalOrder(length)
	state := uint64(ShuffleSeed)
	next := func() uint64 {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		return z ^ (z >> 31)
	}
	for index := length - 1; index > 0; index-- {
		swap := int(next() % uint64(index+1))
		order[index], order[swap] = order[swap], order[index]
	}
	return order
}

// noveltyFirstOrder sorts steps by their standalone dependency cardinality,
// descending, ties by natural order: the most exposing statements first.
func noveltyFirstOrder(steps []finalv5rls.Step) []int {
	order := naturalOrder(len(steps))
	weight := make([]int, len(steps))
	for index, step := range steps {
		weight[index] = len(step.Oracle.Dependency)
	}
	sort.SliceStable(order, func(a, b int) bool {
		return weight[order[a]] > weight[order[b]]
	})
	return order
}

// simulateArm replays the ordered trace against one arm's budgets with the
// production refusal semantics: exposure crossings refuse without charging
// and without ending the task; resource crossings archive the task, so every
// later step is refused as archived.
func simulateArm(arm, ordering string, order []int, steps []finalv5rls.Step) (ArmTrace, error) {
	profile, known := ArmProfiles[arm]
	if !known {
		return ArmTrace{}, fmt.Errorf("unknown comparator arm %q", arm)
	}
	trace := ArmTrace{Arm: arm, Ordering: ordering, BudgetProfile: profile}
	release, dependency, outcome := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var rowsUsed, queriesUsed int64
	archived := false
	maxRelease, maxDependency, maxOutcome := EffectivelyUnlimited, EffectivelyUnlimited, EffectivelyUnlimited
	maxRows, maxQueries := ResourceRows, ResourceQueries
	switch arm {
	case "exact":
		maxRelease, maxDependency, maxOutcome = ExactMaxRelease, ExactMaxDependency, ExactMaxOutcome
	case "rows":
		maxRows = RowsCounterBudget
	case "queries":
		maxQueries = QueryCounterBudget
	case "release":
		maxRelease = ExactMaxRelease
	}
	for position, sourceIndex := range order {
		step := steps[sourceIndex]
		outcomeRow := StepOutcome{Position: position + 1, SourceIndex: sourceIndex + 1, StepID: step.ID}
		novelRelease := novelCount(release, step.Oracle.Release)
		novelDep := novelCount(dependency, step.Oracle.Dependency)
		novelOutcome := novelCount(outcome, step.Oracle.Outcome)
		stepRows := int64(len(step.ExpectedRows))
		switch {
		case archived:
			outcomeRow.RefusalKind = "archived"
		case queriesUsed+1 > maxQueries || rowsUsed+stepRows > maxRows:
			// A resource crossing consumes the attempt and archives the task.
			outcomeRow.RefusalKind = "resource"
			archived = true
		case int64(len(release))+novelRelease > maxRelease ||
			int64(len(dependency))+novelDep > maxDependency ||
			int64(len(outcome))+novelOutcome > maxOutcome:
			outcomeRow.RefusalKind = "exposure"
		default:
			outcomeRow.Accepted = true
			outcomeRow.ReleasedRows = stepRows
			outcomeRow.NovelRelease, outcomeRow.NovelDep, outcomeRow.NovelOutcome = novelRelease, novelDep, novelOutcome
			admit(release, step.Oracle.Release)
			admit(dependency, step.Oracle.Dependency)
			admit(outcome, step.Oracle.Outcome)
			rowsUsed += stepRows
			trace.ReleasedRowTotal += stepRows
		}
		queriesUsed++
		if !outcomeRow.Accepted {
			trace.RefusedSteps++
			if trace.FirstRefusal == 0 {
				trace.FirstRefusal = position + 1
			}
		} else {
			trace.AcceptedSteps++
		}
		trace.Steps = append(trace.Steps, outcomeRow)
	}
	trace.DistinctRelease = int64(len(release))
	trace.DistinctDep = int64(len(dependency))
	trace.DistinctOutcome = int64(len(outcome))
	return trace, nil
}

func novelCount(seen map[string]bool, members []string) int64 {
	var count int64
	local := map[string]bool{}
	for _, member := range members {
		if !seen[member] && !local[member] {
			local[member] = true
			count++
		}
	}
	return count
}

func admit(seen map[string]bool, members []string) {
	for _, member := range members {
		seen[member] = true
	}
}

// Oracle is re-exported for validators that need per-step observations.
func Oracle(steps []finalv5rls.Step) []finalv5oracle.Observation {
	return finalv5rls.OracleTrace(steps)
}
