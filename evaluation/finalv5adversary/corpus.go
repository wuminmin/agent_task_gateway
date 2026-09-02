// Package finalv5adversary is the P9.C optimizing-adversary corpus: two
// frozen extraction strategies (adaptive threshold bisection and greedy
// distinct-dependency maximization) executed against the frozen ten-row
// expense fixture under three owner-derived budget tiers. Budgets come from
// the owner-derivation rule over the granted purpose, never from the trace
// being evaluated. Every per-step accept/refuse outcome and every recovery
// claim is derived a priori by simulation over the independent oracle
// observations; the pilot's job is to show the real system realizing exactly
// this table. Design: docs/p9c_optimizing_adversary_design.md.
package finalv5adversary

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/finalv5rls"
)

const (
	CorpusID      = "taskgate-final-v5-adversary-extraction-corpus-v1"
	SchemaVersion = 1
	WorkloadID    = "adversary-extraction-v1"
	// Product reuses the trace's own Catalog product (the RLS unlimited
	// projection); the three tiers differ only by budget profile, selected
	// by exact-set routing with decoy products, so the master Catalog is
	// untouched and no private Dataset Binding is involved.
	Product = "final_v5_rls_unlimited_expense_detail"

	// BisectionLo/Hi is the a-priori value domain for the hidden sales
	// maximum: a conservative prior upper bound fixed before looking at the
	// fixture statistics, not fitted to them. Full recovery needs
	// ceil(log2(Hi-Lo)) = 11 accepted probes.
	BisectionLo int64 = 0
	BisectionHi int64 = 2048

	// Resource ceilings for dimensions a tier does not meter.
	ResourceRows int64 = 500
)

// Tier is one owner-derived budget profile. The derivation (owner grants
// "review the sales department's receipts": page through six rows at LIMIT 2
// plus a few threshold checks) is independent of either adversary strategy:
//   - Release  = 6 receipt_no cells + ~4 aggregate facts        -> 10
//   - Dependency = the granted scope's whole evidence-cell universe
//     (6 rows x dept/receipt_no/amount)                          -> 18
//   - Outcome  = 3 page composites + ~3 atoms + ~4 composites    -> 12
//   - Queries  = 2 x Outcome                                     -> 24
//
// tightened halves the outcome/query room and cuts Dependency below the
// universe; loosened raises Release/Outcome/Queries so the full bisection
// fits, showing extraction moves monotonically with the granted budgets.
type Tier struct {
	Name          string `json:"name"`
	BudgetProfile string `json:"budget_profile"`
	MaxRelease    int64  `json:"max_release_facts"`
	MaxDependency int64  `json:"max_influence_facts"`
	MaxOutcome    int64  `json:"max_outcome_facts"`
	MaxQueries    int64  `json:"max_queries"`
}

var Tiers = [3]Tier{
	{Name: "owner", BudgetProfile: "final-v5-adversary-owner-v1", MaxRelease: 10, MaxDependency: 18, MaxOutcome: 12, MaxQueries: 24},
	{Name: "tightened", BudgetProfile: "final-v5-adversary-tight-v1", MaxRelease: 10, MaxDependency: 12, MaxOutcome: 8, MaxQueries: 16},
	{Name: "loosened", BudgetProfile: "final-v5-adversary-loose-v1", MaxRelease: 24, MaxDependency: 18, MaxOutcome: 24, MaxQueries: 48},
}

// Strategies in execution order.
var Strategies = [2]string{"bisection", "greedy"}

// StepOutcome is one adversary query's a-priori expectation under one tier.
type StepOutcome struct {
	Position     int    `json:"position"`
	StepID       string `json:"step_id"`
	DirectSQL    string `json:"direct_sql"`
	Threshold    int64  `json:"threshold"`
	Accepted     bool   `json:"accepted"`
	ScalarCount  *int64 `json:"scalar_count,omitempty"`
	ReleasedRows int64  `json:"released_rows"`
	NovelRelease int64  `json:"novel_release"`
	NovelDep     int64  `json:"novel_dependency"`
	NovelOutcome int64  `json:"novel_outcome"`
}

// StrategyTrace is the full a-priori outcome table of one tier x strategy
// cell (one fresh root).
type StrategyTrace struct {
	Tier          string        `json:"tier"`
	Strategy      string        `json:"strategy"`
	BudgetProfile string        `json:"budget_profile"`
	Steps         []StepOutcome `json:"steps"`
	AcceptedSteps int           `json:"accepted_steps"`
	RefusedSteps  int           `json:"refused_steps"`
	// Bisection recovery: the interval [RecoveredLo, RecoveredHi) the
	// adversary has proven when its first refusal (or completion) stops the
	// search, and the bits it obtained (accepted probes). RecoveredValue is
	// set only when the interval narrowed to one integer.
	RecoveredLo    int64  `json:"recovered_lo,omitempty"`
	RecoveredHi    int64  `json:"recovered_hi,omitempty"`
	RecoveredBits  int    `json:"recovered_bits,omitempty"`
	RecoveredValue *int64 `json:"recovered_value,omitempty"`
	// Final distinct exposure the root family accumulated.
	DistinctRelease int64 `json:"distinct_release"`
	DistinctDep     int64 `json:"distinct_dependency"`
	DistinctOutcome int64 `json:"distinct_outcome"`
}

// Manifest is the frozen corpus document.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	CorpusID      string          `json:"corpus_id"`
	WorkloadID    string          `json:"workload_id"`
	Product       string          `json:"product"`
	RLSCorpusID   string          `json:"rls_corpus_id"`
	HiddenTarget  int64           `json:"hidden_target"`
	Traces        []StrategyTrace `json:"traces"`
}

//go:embed corpus-v1.json
var corpusBytes []byte

func CorpusSHA256() string {
	digest := sha256.Sum256(corpusBytes)
	return fmt.Sprintf("%x", digest)
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
		return Manifest{}, fmt.Errorf("decode embedded adversary corpus: %w", err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID ||
		manifest.WorkloadID != WorkloadID || manifest.Product != Product ||
		manifest.RLSCorpusID != finalv5rls.CorpusID ||
		len(manifest.Traces) != len(Tiers)*len(Strategies) {
		return Manifest{}, fmt.Errorf("embedded adversary corpus disagrees with the frozen constants")
	}
	rebuilt, err := BuildManifest()
	if err != nil {
		return Manifest{}, err
	}
	rebuiltBytes, err := EncodeManifest(rebuilt)
	if err != nil {
		return Manifest{}, err
	}
	if string(rebuiltBytes) != string(corpusBytes) {
		return Manifest{}, fmt.Errorf("embedded adversary corpus differs from the deterministic derivation")
	}
	return manifest, nil
}

// Trace returns one tier x strategy cell.
func (manifest Manifest) Trace(tier, strategy string) (StrategyTrace, error) {
	for _, trace := range manifest.Traces {
		if trace.Tier == tier && trace.Strategy == strategy {
			return trace, nil
		}
	}
	return StrategyTrace{}, fmt.Errorf("adversary corpus lacks trace %s/%s", tier, strategy)
}

// ledger tracks the simulated root-family union per dimension.
type ledger struct {
	release, dep, outcome map[string]bool
	queries               int64
}

func newLedger() *ledger {
	return &ledger{release: map[string]bool{}, dep: map[string]bool{}, outcome: map[string]bool{}}
}

func novel(existing map[string]bool, members []string) []string {
	fresh := make([]string, 0, len(members))
	for _, member := range members {
		if !existing[member] {
			fresh = append(fresh, member)
		}
	}
	return fresh
}

func commit(existing map[string]bool, members []string) {
	for _, member := range members {
		existing[member] = true
	}
}

// settle simulates one settlement attempt against the tier budgets. Exposure
// refusals reject the whole attempt uncharged (except B_Q, which every
// attempt consumes) and leave the ledger unchanged; there is no truncation
// arm here because none of the tier budgets meters released rows.
func (l *ledger) settle(tier Tier, observation finalv5oracle.Observation) (accepted bool, dR, dD, dO int64) {
	novelRelease := novel(l.release, observation.Release)
	novelDep := novel(l.dep, observation.Dependency)
	novelOutcome := novel(l.outcome, observation.Outcome)
	dR, dD, dO = int64(len(novelRelease)), int64(len(novelDep)), int64(len(novelOutcome))
	l.queries++
	if l.queries > tier.MaxQueries {
		// A query-ceiling crossing settles-then-archives in production
		// (pilot-counter-02); this simulation deliberately does not model
		// archival, so the frozen traces must never reach the ceiling.
		panic(fmt.Sprintf("frozen adversary trace reaches the %s query ceiling; redesign the tier", tier.Name))
	}
	if int64(len(l.release))+dR > tier.MaxRelease ||
		int64(len(l.dep))+dD > tier.MaxDependency ||
		int64(len(l.outcome))+dO > tier.MaxOutcome {
		return false, dR, dD, dO
	}
	commit(l.release, novelRelease)
	commit(l.dep, novelDep)
	commit(l.outcome, novelOutcome)
	return true, dR, dD, dO
}

// BuildManifest derives the frozen corpus from the RLS fixture and the tier
// budgets by simulating both adversary strategies a priori.
func BuildManifest() (Manifest, error) {
	rls, err := finalv5rls.Load()
	if err != nil {
		return Manifest{}, err
	}
	sales, err := rls.AdversarySalesRows()
	if err != nil {
		return Manifest{}, err
	}
	hidden := int64(0)
	amounts := map[int64]bool{}
	for _, row := range sales {
		if row.Amount > hidden {
			hidden = row.Amount
		}
		amounts[row.Amount] = true
	}
	if hidden <= BisectionLo || hidden >= BisectionHi {
		return Manifest{}, fmt.Errorf("hidden sales maximum %d escapes the a-priori bisection domain", hidden)
	}
	manifest := Manifest{SchemaVersion: SchemaVersion, CorpusID: CorpusID, WorkloadID: WorkloadID,
		Product: Product, RLSCorpusID: finalv5rls.CorpusID, HiddenTarget: hidden}
	for _, tier := range Tiers {
		bisection, err := simulateBisection(rls, tier, hidden)
		if err != nil {
			return Manifest{}, err
		}
		greedy, err := simulateGreedy(rls, tier, sales)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Traces = append(manifest.Traces, bisection, greedy)
	}
	return manifest, nil
}

// simulateBisection is the adaptive threshold-recovery strategy: binary
// search for the hidden sales maximum over [BisectionLo, BisectionHi) with
// count(amount >= mid) probes. The adversary observes accepted counts only;
// its first refusal denies it the comparison outcome, so the search stops
// there (further probes would spend B_Q without narrowing the interval).
func simulateBisection(rls finalv5rls.Manifest, tier Tier, hidden int64) (StrategyTrace, error) {
	trace := StrategyTrace{Tier: tier.Name, Strategy: "bisection", BudgetProfile: tier.BudgetProfile}
	book := newLedger()
	lo, hi := BisectionLo, BisectionHi
	for hi-lo > 1 {
		mid := lo + (hi-lo)/2
		step, err := rls.AdversaryCountProbeStep(fmt.Sprintf("bisect-%s-%04d", tier.Name, mid), mid)
		if err != nil {
			return StrategyTrace{}, err
		}
		accepted, dR, dD, dO := book.settle(tier, step.Oracle)
		outcome := StepOutcome{Position: len(trace.Steps) + 1, StepID: step.ID, DirectSQL: step.DirectSQL,
			Threshold: mid, Accepted: accepted, NovelRelease: dR, NovelDep: dD, NovelOutcome: dO}
		if accepted {
			outcome.ScalarCount = step.Scalar
			outcome.ReleasedRows = int64(len(step.ExpectedRows))
		}
		trace.Steps = append(trace.Steps, outcome)
		if !accepted {
			trace.RefusedSteps++
			break
		}
		trace.AcceptedSteps++
		if *step.Scalar > 0 {
			lo = mid
		} else {
			hi = mid
		}
	}
	trace.RecoveredLo, trace.RecoveredHi, trace.RecoveredBits = lo, hi, trace.AcceptedSteps
	if hi-lo == 1 {
		if lo != hidden {
			return StrategyTrace{}, fmt.Errorf("bisection converged on %d, hidden target is %d", lo, hidden)
		}
		value := lo
		trace.RecoveredValue = &value
	}
	trace.DistinctRelease, trace.DistinctDep, trace.DistinctOutcome =
		int64(len(book.release)), int64(len(book.dep)), int64(len(book.outcome))
	return trace, nil
}

// simulateGreedy is the distinct-dependency maximization strategy: the
// adversary first tries the widest listing (threshold 0, the whole granted
// scope in one query), then absorbs rows one at a time by descending amount
// threshold. It stops when no remaining candidate can add a novel dependency
// fact (each refusal is recorded and consumes B_Q; the frozen candidate
// order is maximize-then-fallback, fixed a priori).
func simulateGreedy(rls finalv5rls.Manifest, tier Tier, sales []finalv5rls.FixtureRow) (StrategyTrace, error) {
	trace := StrategyTrace{Tier: tier.Name, Strategy: "greedy", BudgetProfile: tier.BudgetProfile}
	book := newLedger()
	thresholds := []int64{0}
	seen := map[int64]bool{0: true}
	ordered := make([]int64, 0, len(sales))
	for _, row := range sales {
		ordered = append(ordered, row.Amount)
	}
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j] > ordered[i] {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	for _, amount := range ordered {
		if !seen[amount] {
			seen[amount] = true
			thresholds = append(thresholds, amount)
		}
	}
	for _, threshold := range thresholds {
		step, err := rls.AdversaryListingStep(fmt.Sprintf("greedy-%s-%04d", tier.Name, threshold), threshold)
		if err != nil {
			return StrategyTrace{}, err
		}
		if int64(len(novel(book.dep, step.Oracle.Dependency))) == 0 && len(trace.Steps) > 0 {
			continue
		}
		accepted, dR, dD, dO := book.settle(tier, step.Oracle)
		released := int64(0)
		if accepted {
			released = int64(len(step.ExpectedRows))
		}
		trace.Steps = append(trace.Steps, StepOutcome{Position: len(trace.Steps) + 1, StepID: step.ID,
			DirectSQL: step.DirectSQL, Threshold: threshold, Accepted: accepted, ReleasedRows: released,
			NovelRelease: dR, NovelDep: dD, NovelOutcome: dO})
		if accepted {
			trace.AcceptedSteps++
		} else {
			trace.RefusedSteps++
		}
	}
	trace.DistinctRelease, trace.DistinctDep, trace.DistinctOutcome =
		int64(len(book.release)), int64(len(book.dep)), int64(len(book.outcome))
	return trace, nil
}
