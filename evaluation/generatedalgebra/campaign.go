package generatedalgebra

import (
	"fmt"
	"math/rand"
	"sort"

	"taskbound.local/agent-data-gateway/evaluation/exposureoracle"
)

// Fixtures returns random expenses/departments relations: departments is the
// dimension (one row per department), expenses the fact table with occasional
// NULL departments and NULL amounts.
func Fixtures(rng *rand.Rand, expenseRows int) (Relation, Relation) {
	names := []string{"sales", "rnd", "ops", "legal", "hr"}
	departments := Relation{Name: "departments", SourceNamespace: "travel.department", Snapshot: "snapshot-gen", StableRole: "department",
		Fields: []Field{{ID: "department.department", SQLType: "text"}, {ID: "department.manager", SQLType: "text"}}}
	for i, name := range names {
		departments.Rows = append(departments.Rows, Row{EntityKey: fmt.Sprintf("d%02d", i+1),
			Values: map[string]any{"department.department": name, "department.manager": "m-" + name}})
	}
	expenses := Relation{Name: "expenses", SourceNamespace: "travel.expense", Snapshot: "snapshot-gen", StableRole: "expense",
		Fields: []Field{{ID: "expense.department", SQLType: "text"}, {ID: "expense.amount", SQLType: "numeric"}, {ID: "expense.days", SQLType: "bigint"}}}
	for i := 0; i < expenseRows; i++ {
		var department any = names[rng.Intn(len(names))]
		if rng.Intn(10) == 0 {
			department = nil
		}
		var amount any = int64(rng.Intn(50) * 5)
		if rng.Intn(12) == 0 {
			amount = nil
		}
		expenses.Rows = append(expenses.Rows, Row{EntityKey: fmt.Sprintf("r%03d", i+1),
			Values: map[string]any{"expense.department": department, "expense.amount": amount, "expense.days": int64(rng.Intn(9) + 1)}})
	}
	return expenses, departments
}

// GeneratePlan draws one plan from the closed fragment.
func GeneratePlan(rng *rand.Rand) Plan {
	plan := Plan{Join: rng.Intn(3) == 0}
	names := []string{"sales", "rnd", "ops", "legal", "hr", "none"}
	numericOps := []string{"<", "<=", ">", ">=", "=", "<>"}
	for i := rng.Intn(3); i > 0; i-- {
		switch rng.Intn(3) {
		case 0:
			plan.Predicates = append(plan.Predicates, Predicate{Field: "expense.department", Op: []string{"=", "<>"}[rng.Intn(2)], Literal: names[rng.Intn(len(names))]})
		case 1:
			plan.Predicates = append(plan.Predicates, Predicate{Field: "expense.amount", Op: numericOps[rng.Intn(len(numericOps))], Literal: int64(rng.Intn(50) * 5)})
		default:
			plan.Predicates = append(plan.Predicates, Predicate{Field: "expense.days", Op: numericOps[rng.Intn(len(numericOps))], Literal: int64(rng.Intn(9) + 1)})
		}
	}
	visible := []string{"expense.department", "expense.amount", "expense.days"}
	if plan.Join {
		visible = append(visible, "department.manager", "department.department")
	}
	switch rng.Intn(3) {
	case 0:
		plan.Kind = "project"
		rng.Shuffle(len(visible), func(i, j int) { visible[i], visible[j] = visible[j], visible[i] })
		plan.Project = append([]string(nil), visible[:1+rng.Intn(len(visible))]...)
		sort.Strings(plan.Project)
		if !plan.Join && rng.Intn(2) == 0 {
			page := [2]int{rng.Intn(6), 1 + rng.Intn(8)}
			plan.Page = &page
		}
	case 1:
		plan.Kind = "global"
		plan.Aggregates = randomAggregates(rng)
		plan.Having = randomHaving(rng, plan.Aggregates)
	default:
		plan.Kind = "group"
		plan.GroupField = "expense.department"
		if plan.Join && rng.Intn(2) == 0 {
			plan.GroupField = "department.manager"
		}
		plan.GroupKeyVisible = rng.Intn(4) != 0
		plan.Aggregates = randomAggregates(rng)
		plan.Having = randomHaving(rng, plan.Aggregates)
	}
	return plan
}

func randomAggregates(rng *rand.Rand) []Aggregate {
	candidates := []Aggregate{
		{Function: "count", Field: "*", OutputID: "items", OutputType: "bigint"},
		{Function: "sum", Field: "expense.amount", OutputID: "total", OutputType: "numeric"},
		{Function: "min", Field: "expense.amount", OutputID: "minimum", OutputType: "numeric"},
		{Function: "max", Field: "expense.amount", OutputID: "maximum", OutputType: "numeric"},
		{Function: "sum", Field: "expense.days", OutputID: "days", OutputType: "numeric"},
		{Function: "avg", Field: "expense.amount", OutputID: "mean", OutputType: "numeric"},
		{Function: "count", Field: "expense.days", OutputID: "distinct_days", OutputType: "bigint", Distinct: true},
	}
	rng.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	return append([]Aggregate(nil), candidates[:1+rng.Intn(3)]...)
}

// randomHaving attaches a HAVING predicate to one of the plan's aggregates in
// about a third of grouped or global plans; the literal is drawn so that both
// kept and dropped groups occur across fixtures.
func randomHaving(rng *rand.Rand, aggregates []Aggregate) *Having {
	if rng.Intn(3) != 0 {
		return nil
	}
	aggregate := aggregates[rng.Intn(len(aggregates))]
	ops := []string{">", ">=", "<", "<=", "=", "<>"}
	var literal any
	switch aggregate.OutputType {
	case "bigint":
		literal = float64(1 + rng.Intn(4))
	default:
		literal = float64([]int{5, 10, 20, 40, 80}[rng.Intn(5)])
	}
	return &Having{OutputID: aggregate.OutputID, Op: ops[rng.Intn(len(ops))], Literal: literal}
}

// Report summarizes one campaign.
type Report struct {
	Version        string         `json:"version"`
	Seed           int64          `json:"seed"`
	Plans          int            `json:"plans"`
	Fixtures       int            `json:"fixtures"`
	ExpenseRows    [2]int         `json:"expense_rows_min_max"`
	Coverage       map[string]int `json:"coverage"`
	Mismatches     int            `json:"mismatches"`
	HashMismatches int            `json:"hash_mismatches"`
	Failures       []string       `json:"failures,omitempty"`
	Conservation   map[string]int `json:"conservation_checks"`
	ReleaseFacts   int            `json:"release_facts_compared"`
	InfluenceFacts int            `json:"dependency_facts_compared"`
}

func sameSet(left, right map[string]any) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

func keys[T any](m map[string]T) map[string]any {
	out := make(map[string]any, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

// Run executes the campaign: fixtures × plans differential comparison plus
// conservation checks (split projection, page partition, page overlap).
func Run(seed int64, fixtures, plansPerFixture int) Report {
	rng := rand.New(rand.NewSource(seed))
	report := Report{Version: "taskgate-generated-algebra-v1", Seed: seed, Fixtures: fixtures, Coverage: map[string]int{}, Conservation: map[string]int{}, ExpenseRows: [2]int{1 << 30, 0}}
	for f := 0; f < fixtures; f++ {
		rows := 5 + rng.Intn(46)
		if rows < report.ExpenseRows[0] {
			report.ExpenseRows[0] = rows
		}
		if rows > report.ExpenseRows[1] {
			report.ExpenseRows[1] = rows
		}
		expenses, departments := Fixtures(rng, rows)
		for p := 0; p < plansPerFixture; p++ {
			plan := GeneratePlan(rng)
			report.Plans++
			report.Coverage[plan.Kind]++
			if plan.Having != nil {
				report.Coverage["having"]++
			}
			for _, aggregate := range plan.Aggregates {
				if aggregate.Function == "avg" {
					report.Coverage["avg"]++
				}
				if aggregate.Distinct {
					report.Coverage["count_distinct"]++
				}
			}
			if plan.Join {
				report.Coverage["join"]++
			}
			if len(plan.Predicates) > 0 {
				report.Coverage["select"]++
			}
			if plan.Page != nil {
				report.Coverage["page"]++
			}
			reference, err := Evaluate(expenses, departments, plan)
			if err != nil {
				report.Failures = append(report.Failures, fmt.Sprintf("reference: %v (%+v)", err, plan))
				continue
			}
			production, err := EvaluateProduction(expenses, departments, plan)
			if err != nil {
				report.HashMismatches++
				report.Failures = append(report.Failures, fmt.Sprintf("production: %v (%+v)", err, plan))
				continue
			}
			report.ReleaseFacts += len(reference.Release)
			report.InfluenceFacts += len(reference.Influence)
			if !sameSet(keys(reference.Release), keys(production.Release)) || !sameSet(keys(reference.Influence), keys(production.Influence)) {
				report.Mismatches++
				if len(report.Failures) < 20 {
					report.Failures = append(report.Failures, fmt.Sprintf("mismatch: reference R=%d D=%d production R=%d D=%d (%+v) diff=%s", len(reference.Release), len(reference.Influence), len(production.Release), len(production.Influence), plan, diffKeys(reference, production)))
				}
			}
			// conservation on projection plans without page: split projection
			// and page partition against the complete query (production side)
			if plan.Kind == "project" && plan.Page == nil && len(plan.Project) >= 2 && !plan.Join {
				unionR, unionD := map[string]any{}, map[string]any{}
				for _, field := range plan.Project {
					part := plan
					part.Project = []string{field}
					obs, err := EvaluateProduction(expenses, departments, part)
					if err != nil {
						report.Failures = append(report.Failures, fmt.Sprintf("split: %v", err))
						continue
					}
					for k := range obs.Release {
						unionR[k] = struct{}{}
					}
					for k := range obs.Influence {
						unionD[k] = struct{}{}
					}
				}
				if sameSet(unionR, keys(production.Release)) && sameSet(unionD, keys(production.Influence)) {
					report.Conservation["split_projection_equals_complete"]++
				} else {
					report.Conservation["split_projection_MISMATCH"]++
				}
				// page partition with page size 3 over canonical order
				pageR, pageD := map[string]any{}, map[string]any{}
				for offset := 0; offset < 60; offset += 3 {
					part := plan
					page := [2]int{offset, 3}
					part.Page = &page
					obs, err := EvaluateProduction(expenses, departments, part)
					if err != nil {
						break
					}
					if len(obs.Release) == 0 && len(obs.Influence) == 0 {
						break
					}
					for k := range obs.Release {
						pageR[k] = struct{}{}
					}
					for k := range obs.Influence {
						pageD[k] = struct{}{}
					}
				}
				if sameSet(pageR, keys(production.Release)) && sameSet(pageD, keys(production.Influence)) {
					report.Conservation["page_partition_equals_complete"]++
				} else {
					report.Conservation["page_partition_MISMATCH"]++
				}
			}
		}
	}
	return report
}

func diffKeys(reference, production Observation) string {
	var out []string
	for name, pair := range map[string][2]map[string]exposureoracle.Fact{"release": {reference.Release, production.Release}, "dependency": {reference.Influence, production.Influence}} {
		for key, fact := range pair[0] {
			if _, ok := pair[1][key]; !ok && len(out) < 4 {
				out = append(out, fmt.Sprintf("%s reference-only kind=%s field=%s expr=%s value=%s witness=%.8s", name, fact.Kind, fact.Field, fact.NormalizedExpression, fact.CanonicalValue, fact.WitnessCommitment))
			}
		}
		for key, fact := range pair[1] {
			if _, ok := pair[0][key]; !ok && len(out) < 8 {
				out = append(out, fmt.Sprintf("%s production-only kind=%s field=%s expr=%s value=%s witness=%.8s", name, fact.Kind, fact.Field, fact.NormalizedExpression, fact.CanonicalValue, fact.WitnessCommitment))
			}
		}
	}
	return fmt.Sprint(out)
}
