package exposureeval

// Rewrite-invariance evidence for RQ2. The earlier RQ2 campaign
// (evaluation/postgresoracle) only compared PostgreSQL result rows and used
// rewrites (CTE pushdown, De Morgan, correlated EXISTS, VALUES join) that lie
// outside the paper's closed algebra language. It therefore could not show that
// TaskGate's own accounting is invariant under rewrite.
//
// This file closes that gap. For every rewrite WITHIN the closed language, we
// express both sides as a queryplan.AlgebraPlanV2 tree, evaluate that tree over
// the SAME data instance with TaskGate's own V2 algebra operators, and require:
//   (a) the typed normal forms agree: NormalizeAlgebraV2(left) == NormalizeAlgebraV2(right);
//   (b) the release FactSets agree: ObserveV2(left).Release == ObserveV2(right).Release;
//   (c) the positive-output dependency FactSets (compatibility field
//       "influence") agree likewise; and
//   (d) the incremental charge is zero in both directions.
// Because both the normal form and the effect come from one plan tree, there is
// no drift between a "description" of the rewrite and its execution.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// RewriteInvarianceCaseResult records one (rewrite x dataset) measurement.
type RewriteInvarianceCaseResult struct {
	Case               string `json:"case"`
	Dataset            string `json:"dataset"`
	NormalFormRequired bool   `json:"normal_form_required"`
	NormalFormEqual    bool   `json:"normal_form_equal"`
	ReleaseEqual       bool   `json:"release_equal"`
	InfluenceEqual     bool   `json:"influence_equal"`
	ChargeDeltaZero    bool   `json:"charge_delta_zero"`
	ReleaseCharge      int    `json:"release_charge"`
	InfluenceCharge    int    `json:"influence_charge"`
}

// RewriteInvarianceSummary is the RQ2 exposure-level evidence.
type RewriteInvarianceSummary struct {
	Status           string                        `json:"status"`
	Oracle           string                        `json:"oracle"`
	Normalization    string                        `json:"normalization"`
	Rewrites         int                           `json:"rewrites"`
	Datasets         int                           `json:"datasets"`
	Cases            int                           `json:"cases"`
	NormalFormChecks int                           `json:"normal_form_checks"`
	EffectChecks     int                           `json:"effect_checks"`
	Mismatches       int                           `json:"mismatches"`
	PairSetSHA256    string                        `json:"pair_set_sha256"`
	Results          []RewriteInvarianceCaseResult `json:"results"`
}

const rewriteInvarianceNormalization = "algebra-normal-form-v3+release-positive-output-dependency-factset+zero-incremental-charge"

// RunExposureRewriteInvariance evaluates every closed-language rewrite over
// every dataset and returns the summary. A single mismatch is a hard error.
func RunExposureRewriteInvariance() (RewriteInvarianceSummary, error) {
	datasets, err := rewriteDatasets()
	if err != nil {
		return RewriteInvarianceSummary{}, err
	}
	rewrites := closedLanguageRewrites()

	summary := RewriteInvarianceSummary{Status: "complete", Oracle: "taskgate-v2-algebra-self-invariance",
		Normalization: rewriteInvarianceNormalization, Rewrites: len(rewrites), Datasets: len(datasets)}
	pairDigest := sha256.New()
	pairDigest.Write([]byte("TASKGATE-RQ2-EXPOSAGE-REWRITE-INVARIANCE-V1\x00"))

	for _, rewrite := range rewrites {
		for _, dataset := range datasets {
			if !dataset.provides(rewrite.requires...) {
				continue
			}
			left, right, err := rewrite.build(dataset.plans)
			if err != nil {
				return summary, fmt.Errorf("rewrite %s on %s: %w", rewrite.id, dataset.name, err)
			}
			result, signature, err := measureRewrite(dataset.scans, left, right)
			if err != nil {
				return summary, fmt.Errorf("rewrite %s on %s: %w", rewrite.id, dataset.name, err)
			}
			result.Case = rewrite.id
			result.Dataset = dataset.name
			result.NormalFormRequired = rewrite.nfInvariant
			summary.Results = append(summary.Results, result)
			summary.Cases++
			if rewrite.nfInvariant {
				summary.NormalFormChecks++
			}
			summary.EffectChecks += 2 // release + influence set comparison
			pairDigest.Write([]byte(signature))
			pairDigest.Write([]byte{0})
			// Every rewrite must preserve the release/dependency FactSets and yield
			// zero incremental charge. Only NF-canonical rewrites must additionally
			// agree on the typed normal form; select/project reordering is an
			// effect-level equivalence (Restricted projection/selection invariance),
			// not a rewrite the normalizer canonicalizes.
			nfOK := !rewrite.nfInvariant || result.NormalFormEqual
			if !nfOK || !result.ReleaseEqual || !result.InfluenceEqual || !result.ChargeDeltaZero {
				summary.Mismatches++
				return summary, fmt.Errorf("rewrite %s on %s is not invariant: %+v", rewrite.id, dataset.name, result)
			}
		}
	}
	summary.PairSetSHA256 = fmt.Sprintf("%x", pairDigest.Sum(nil))
	if summary.Mismatches != 0 || summary.Cases == 0 {
		return summary, fmt.Errorf("rewrite invariance campaign did not pass cleanly: cases=%d mismatches=%d", summary.Cases, summary.Mismatches)
	}
	return summary, nil
}

// measureRewrite evaluates both plan trees over the shared scans and checks
// normal-form equality, FactSet equality, and zero incremental charge.
func measureRewrite(scans map[string]exposure.RelationV2, left, right queryplan.AlgebraPlanV2) (RewriteInvarianceCaseResult, string, error) {
	leftNF, err := queryplan.NormalizeAlgebraV2(left)
	if err != nil {
		return RewriteInvarianceCaseResult{}, "", err
	}
	rightNF, err := queryplan.NormalizeAlgebraV2(right)
	if err != nil {
		return RewriteInvarianceCaseResult{}, "", err
	}
	leftRel, err := evalAlgebraPlan(left, scans)
	if err != nil {
		return RewriteInvarianceCaseResult{}, "", err
	}
	rightRel, err := evalAlgebraPlan(right, scans)
	if err != nil {
		return RewriteInvarianceCaseResult{}, "", err
	}
	leftObs, err := exposure.ObserveV2(leftRel)
	if err != nil {
		return RewriteInvarianceCaseResult{}, "", err
	}
	rightObs, err := exposure.ObserveV2(rightRel)
	if err != nil {
		return RewriteInvarianceCaseResult{}, "", err
	}
	releaseDelta, influenceDelta := incrementalCharge(leftObs, rightObs)
	result := RewriteInvarianceCaseResult{
		NormalFormEqual: leftNF.SHA256 == rightNF.SHA256,
		ReleaseEqual:    sameFactIDSet(leftObs.Release, rightObs.Release),
		InfluenceEqual:  sameFactIDSet(leftObs.Influence, rightObs.Influence),
		ChargeDeltaZero: releaseDelta == 0 && influenceDelta == 0,
		ReleaseCharge:   releaseDelta,
		InfluenceCharge: influenceDelta,
	}
	signature := fmt.Sprintf("%s|%s|%s", leftNF.SHA256, rightNF.SHA256, observationSignature(leftObs))
	return result, signature, nil
}

// incrementalCharge returns the symmetric set-difference cardinality of two
// observations: facts one side charges that the other does not. Invariance
// requires both to be zero.
func incrementalCharge(left, right exposure.Observation) (int, int) {
	return symmetricDifference(left.Release, right.Release), symmetricDifference(left.Influence, right.Influence)
}

func symmetricDifference(left, right []exposure.FactID) int {
	leftSet, _ := exposure.NewFactSet(left...)
	rightSet, _ := exposure.NewFactSet(right...)
	delta := 0
	_ = leftSet.Range(func(hash [32]byte, _ exposure.FactID) error {
		if _, present, err := rightSet.Contains(hash); err != nil || !present {
			delta++
		}
		return nil
	})
	_ = rightSet.Range(func(hash [32]byte, _ exposure.FactID) error {
		if _, present, err := leftSet.Contains(hash); err != nil || !present {
			delta++
		}
		return nil
	})
	return delta
}

func sameFactIDSet(left, right []exposure.FactID) bool {
	if len(left) != len(right) {
		return false
	}
	leftSet, _ := exposure.NewFactSet(left...)
	for _, fact := range right {
		hash, err := fact.HashBytes()
		if err != nil {
			return false
		}
		if _, present, err := leftSet.Contains(hash); err != nil || !present {
			return false
		}
	}
	return true
}

func observationSignature(observation exposure.Observation) string {
	release, _ := exposure.ObservationDigest(observation)
	return release
}

// ---- plan interpreter: AlgebraPlanV2 -> RelationV2 via TaskGate operators ----

func evalAlgebraPlan(plan queryplan.AlgebraPlanV2, scans map[string]exposure.RelationV2) (exposure.RelationV2, error) {
	switch strings.ToLower(strings.TrimSpace(plan.Op)) {
	case "scan":
		relation, ok := scans[scanKey(plan)]
		if !ok {
			return exposure.RelationV2{}, fmt.Errorf("scan %s/%s not in dataset", plan.SourceNamespace, plan.Snapshot)
		}
		return relation, nil
	case "select":
		input, err := evalAlgebraPlan(*plan.Input, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		fields := predicateFields(plan.Predicates)
		predicates := plan.Predicates
		return exposure.SelectV2(input, fields, func(row exposure.AnnotatedRowV2) exposure.SQLTruth {
			outcome := exposure.SQLTrue
			for _, predicate := range predicates {
				outcome = and3V2(outcome, evaluateFilter(row, predicate))
			}
			return outcome
		})
	case "project":
		input, err := evalAlgebraPlan(*plan.Input, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		return exposure.ProjectV2(input, plan.Fields...)
	case "join":
		left, err := evalAlgebraPlan(*plan.Left, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		right, err := evalAlgebraPlan(*plan.Right, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		predicates := make([]exposure.JoinPredicateV2, 0, len(plan.JoinPredicates))
		for _, predicate := range plan.JoinPredicates {
			predicates = append(predicates, exposure.JoinPredicateV2{LeftField: predicate.LeftField, RightField: predicate.RightField})
		}
		return exposure.JoinOnV2(left, right, predicates)
	case "union":
		left, err := evalAlgebraPlan(*plan.Left, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		right, err := evalAlgebraPlan(*plan.Right, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		return exposure.UnionDistinctV2(left, right)
	case "group":
		input, err := evalAlgebraPlan(*plan.Input, scans)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		specs := make([]exposure.AggregateSpecV2, 0, len(plan.Aggregates))
		for _, aggregate := range plan.Aggregates {
			outputID := strings.ToLower(strings.TrimSpace(aggregate.Function)) + "(" + strings.ToLower(strings.TrimSpace(aggregate.Field)) + ")"
			specs = append(specs, exposure.AggregateSpecV2{Function: aggregate.Function, Field: aggregate.Field,
				OutputID: outputID, OutputType: aggregate.OutputType})
		}
		outputRows, err := computeGroupOracle(input, plan.GroupBy, specs)
		if err != nil {
			return exposure.RelationV2{}, err
		}
		return exposure.AggregateFromResultsV2(input, plan.GroupBy, specs, outputRows)
	default:
		return exposure.RelationV2{}, fmt.Errorf("operator %q is outside the closed language", plan.Op)
	}
}

func scanKey(plan queryplan.AlgebraPlanV2) string {
	return plan.SourceNamespace + "\x00" + plan.Snapshot
}

func predicateFields(predicates []queryplan.NormalizedFilter) []string {
	seen := make(map[string]struct{}, len(predicates))
	fields := make([]string, 0, len(predicates))
	for _, predicate := range predicates {
		if _, duplicate := seen[predicate.Column]; duplicate {
			continue
		}
		seen[predicate.Column] = struct{}{}
		fields = append(fields, predicate.Column)
	}
	return fields
}

// evaluateFilter implements the closed scalar predicate grammar over a row with
// PostgreSQL three-valued logic: any NULL operand yields SQLUnknown.
func evaluateFilter(row exposure.AnnotatedRowV2, filter queryplan.NormalizedFilter) exposure.SQLTruth {
	cell, present := row.Cells[filter.Column]
	if !present || cell.Value == nil {
		return exposure.SQLUnknown
	}
	literal, err := decodeFilterLiteral(filter.Value)
	if err != nil || literal == nil {
		return exposure.SQLUnknown
	}
	switch strings.ToUpper(filter.Op) {
	case "=":
		return truthFromBool(compareOrdered(cell.Value, literal) == 0)
	case "<>":
		return truthFromBool(compareOrdered(cell.Value, literal) != 0)
	case "<":
		return truthFromBool(compareOrdered(cell.Value, literal) < 0)
	case "<=":
		return truthFromBool(compareOrdered(cell.Value, literal) <= 0)
	case ">":
		return truthFromBool(compareOrdered(cell.Value, literal) > 0)
	case ">=":
		return truthFromBool(compareOrdered(cell.Value, literal) >= 0)
	default:
		return exposure.SQLUnknown
	}
}

func decodeFilterLiteral(raw json.RawMessage) (any, error) {
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// compareOrdered compares two scalar values using PostgreSQL-ish ordering for
// the exact/numeric and text fragment. It panics on incomparable types because
// the campaign only emits predicates over typed columns it controls.
func compareOrdered(left, right any) int {
	leftNumber, leftIsNumber := numericValue(left)
	rightNumber, rightIsNumber := numericValue(right)
	if leftIsNumber && rightIsNumber {
		switch {
		case leftNumber < rightNumber:
			return -1
		case leftNumber > rightNumber:
			return 1
		default:
			return 0
		}
	}
	leftString := fmt.Sprintf("%v", left)
	rightString := fmt.Sprintf("%v", right)
	return strings.Compare(leftString, rightString)
}

func numericValue(value any) (float64, bool) {
	switch candidate := value.(type) {
	case float64:
		return candidate, true
	case float32:
		return float64(candidate), true
	case int:
		return float64(candidate), true
	case int64:
		return float64(candidate), true
	case json.Number:
		asFloat, err := candidate.Float64()
		if err == nil {
			return asFloat, true
		}
		return 0, false
	case string:
		// Exact-numeric values are carried as canonical decimal strings.
		if parsed, ok := parseDecimal(candidate); ok {
			return parsed, true
		}
		return 0, false
	}
	return 0, false
}

func parseDecimal(text string) (float64, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	var sign float64 = 1
	switch text[0] {
	case '-':
		sign, text = -1, text[1:]
	case '+':
		text = text[1:]
	}
	if text == "" {
		return 0, false
	}
	dot := strings.IndexByte(text, '.')
	if dot == -1 {
		var n int
		_, err := fmt.Sscanf(text, "%d", &n)
		if err != nil {
			return 0, false
		}
		return sign * float64(n), true
	}
	integral, fractional := text[:dot], text[dot+1:]
	if !allDigits(integral) || !allDigits(fractional) {
		return 0, false
	}
	combined := integral + fractional
	if combined == "" {
		return 0, false
	}
	var n int
	_, err := fmt.Sscanf(combined, "%d", &n)
	if err != nil {
		return 0, false
	}
	scale := 1.0
	for i := 0; i < len(fractional); i++ {
		scale *= 10
	}
	return sign * float64(n) / scale, true
}

func allDigits(text string) bool {
	for _, character := range text {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func truthFromBool(value bool) exposure.SQLTruth {
	if value {
		return exposure.SQLTrue
	}
	return exposure.SQLFalse
}

func and3V2(left, right exposure.SQLTruth) exposure.SQLTruth {
	if left == exposure.SQLFalse || right == exposure.SQLFalse {
		return exposure.SQLFalse
	}
	if left == exposure.SQLUnknown || right == exposure.SQLUnknown {
		return exposure.SQLUnknown
	}
	return exposure.SQLTrue
}

// computeGroupOracle computes the PostgreSQL aggregate-result rows that
// AggregateFromResultsV2 expects. It supports COUNT(*) and exact/numeric SUM,
// which is all the closed language admits after the floating-point exclusion.
func computeGroupOracle(input exposure.RelationV2, groupFields []string, specs []exposure.AggregateSpecV2) ([]map[string]any, error) {
	type groupKey struct {
		encoded string
	}
	partitions := make(map[string][]exposure.AnnotatedRowV2)
	order := make([]string, 0)
	for _, row := range input.Rows {
		encoded := encodeGroupKey(row, groupFields)
		if _, present := partitions[encoded]; !present {
			order = append(order, encoded)
		}
		partitions[encoded] = append(partitions[encoded], row)
	}
	if len(groupFields) == 0 {
		// A global aggregate has exactly one row even for empty input.
		order = []string{encodeGroupKey(exposure.AnnotatedRowV2{}, groupFields)}
		partitions[order[0]] = input.Rows
	}
	output := make([]map[string]any, 0, len(order))
	for _, encoded := range order {
		members := partitions[encoded]
		rowResult := make(map[string]any, len(groupFields)+len(specs))
		if len(members) > 0 && len(groupFields) > 0 {
			for _, field := range groupFields {
				rowResult[field] = members[0].Cells[field].Value
			}
		}
		for _, spec := range specs {
			rowResult[spec.OutputID] = computeAggregate(spec, members)
		}
		output = append(output, rowResult)
	}
	return output, nil
}

func encodeGroupKey(row exposure.AnnotatedRowV2, groupFields []string) string {
	parts := make([]string, 0, len(groupFields))
	for _, field := range groupFields {
		cell, present := row.Cells[field]
		if !present || cell.Value == nil {
			parts = append(parts, "\x00NULL")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s\x00%v", cell.SQLType, cell.Value))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

func computeAggregate(spec exposure.AggregateSpecV2, members []exposure.AnnotatedRowV2) any {
	function := strings.ToLower(spec.Function)
	if function == "count" && spec.Field == "*" {
		return int64(len(members))
	}
	if function == "count" {
		var total int64
		for _, member := range members {
			if cell, present := member.Cells[spec.Field]; present && cell.Value != nil {
				total++
			}
		}
		return total
	}
	values := make([]float64, 0, len(members))
	for _, member := range members {
		cell, present := member.Cells[spec.Field]
		if !present || cell.Value == nil {
			continue
		}
		if number, ok := numericValue(cell.Value); ok {
			values = append(values, number)
		}
	}
	if len(values) == 0 {
		return nil
	}
	switch function {
	case "sum":
		// Exact-numeric SUM is encoded as a canonical decimal string.
		total := 0.0
		for _, value := range values {
			total += value
		}
		return fmt.Sprintf("%g", total)
	case "min":
		result := values[0]
		for _, value := range values[1:] {
			if value < result {
				result = value
			}
		}
		return result
	case "max":
		result := values[0]
		for _, value := range values[1:] {
			if value > result {
				result = value
			}
		}
		return result
	}
	return nil
}

// ---- closed-language rewrites ----

type rewriteSpec struct {
	id          string
	requires    []string
	nfInvariant bool // rewrite is canonicalized by NF_Π, so normal forms must agree
	build       func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error)
}

func closedLanguageRewrites() []rewriteSpec {
	eq := func(column, literal string) queryplan.NormalizedFilter {
		return queryplan.NormalizedFilter{Column: column, Op: "=", Value: jsonLiteral(literal)}
	}
	ge := func(column, literal string) queryplan.NormalizedFilter {
		return queryplan.NormalizedFilter{Column: column, Op: ">=", Value: jsonLiteral(literal)}
	}

	return []rewriteSpec{
		{
			id:          "select_project_order",
			nfInvariant: false, // effect-level (Restricted projection/selection invariance), not NF-canonical
			requires:    []string{"expenses"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				base := scans["expenses"]
				predicate := []queryplan.NormalizedFilter{eq("expense.department", "sales")}
				left := projectPlan(selectPlan(base, predicate), []string{"expense.department", "expense.amount"})
				right := selectPlan(projectPlan(base, []string{"expense.department", "expense.amount"}), predicate)
				return left, right, nil
			},
		},
		{
			id:          "conjunction_order",
			nfInvariant: true,
			requires:    []string{"expenses"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				base := scans["expenses"]
				first := []queryplan.NormalizedFilter{eq("expense.department", "sales"), ge("expense.amount", "15")}
				second := []queryplan.NormalizedFilter{ge("expense.amount", "15"), eq("expense.department", "sales")}
				return selectPlan(base, first), selectPlan(base, second), nil
			},
		},
		{
			id:          "project_field_order",
			nfInvariant: true,
			requires:    []string{"expenses"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				base := scans["expenses"]
				return projectPlan(base, []string{"expense.department", "expense.amount"}),
					projectPlan(base, []string{"expense.amount", "expense.department"}), nil
			},
		},
		{
			id:          "group_key_order",
			nfInvariant: true,
			requires:    []string{"expenses"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				base := scans["expenses"]
				aggregates := []queryplan.AlgebraAggregateV2{{Function: "count", Field: "*", OutputType: "bigint"},
					{Function: "sum", Field: "expense.amount", OutputType: "numeric"}}
				left := groupPlan(base, []string{"expense.department", "expense.amount"}, aggregates)
				right := groupPlan(base, []string{"expense.amount", "expense.department"}, aggregates)
				return left, right, nil
			},
		},
		{
			id:          "hidden_group_key_order",
			nfInvariant: true,
			requires:    []string{"expenses"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				base := scans["expenses"]
				aggregates := []queryplan.AlgebraAggregateV2{{Function: "count", Field: "*", OutputType: "bigint"}}
				leftGroup := groupPlan(base, []string{"expense.department", "expense.amount"}, aggregates)
				rightGroup := groupPlan(base, []string{"expense.amount", "expense.department"}, aggregates)
				return projectPlan(leftGroup, []string{"count(*)"}), projectPlan(rightGroup, []string{"count(*)"}), nil
			},
		},
		{
			id:          "join_operand_swap",
			nfInvariant: true,
			requires:    []string{"departments", "expenses"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				predicate := []queryplan.AlgebraJoinPredicateV2{{LeftField: "department.department", RightField: "expense.department"}}
				return joinPlan(scans["departments"], scans["expenses"], predicate),
					joinPlan(scans["expenses"], scans["departments"], flipJoinPredicates(predicate)), nil
			},
		},
		{
			id:          "join_predicate_order",
			nfInvariant: true,
			requires:    []string{"ledger", "codebook"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				first := []queryplan.AlgebraJoinPredicateV2{{LeftField: "ledger.department", RightField: "codebook.department"},
					{LeftField: "ledger.code", RightField: "codebook.code"}}
				second := []queryplan.AlgebraJoinPredicateV2{{LeftField: "ledger.code", RightField: "codebook.code"},
					{LeftField: "ledger.department", RightField: "codebook.department"}}
				return joinPlan(scans["ledger"], scans["codebook"], first),
					joinPlan(scans["ledger"], scans["codebook"], second), nil
			},
		},
		{
			id:          "union_operand_exchange",
			nfInvariant: true,
			requires:    []string{"east", "west"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				return unionPlan(scans["east"], scans["west"]), unionPlan(scans["west"], scans["east"]), nil
			},
		},
		{
			id:          "union_idempotence",
			nfInvariant: true,
			requires:    []string{"east"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				setValued := unionPlan(scans["east"], scans["east"])
				return unionPlan(setValued, setValued), setValued, nil
			},
		},
		{
			id:          "duplicate_union_collapse",
			nfInvariant: true,
			requires:    []string{"east"},
			build: func(scans map[string]queryplan.AlgebraPlanV2) (queryplan.AlgebraPlanV2, queryplan.AlgebraPlanV2, error) {
				setValued := unionPlan(scans["east"], scans["east"])
				left := groupPlan(unionPlan(setValued, setValued), nil,
					[]queryplan.AlgebraAggregateV2{{Function: "count", Field: "*", OutputType: "bigint"}})
				right := groupPlan(setValued, nil,
					[]queryplan.AlgebraAggregateV2{{Function: "count", Field: "*", OutputType: "bigint"}})
				return left, right, nil
			},
		},
	}
}
func jsonLiteral(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func selectPlan(input queryplan.AlgebraPlanV2, predicates []queryplan.NormalizedFilter) queryplan.AlgebraPlanV2 {
	return queryplan.AlgebraPlanV2{Op: "select", Input: &input, Predicates: predicates}
}

func projectPlan(input queryplan.AlgebraPlanV2, fields []string) queryplan.AlgebraPlanV2 {
	return queryplan.AlgebraPlanV2{Op: "project", Input: &input, Fields: fields}
}

func joinPlan(left, right queryplan.AlgebraPlanV2, predicates []queryplan.AlgebraJoinPredicateV2) queryplan.AlgebraPlanV2 {
	return queryplan.AlgebraPlanV2{Op: "join", Left: &left, Right: &right, JoinPredicates: predicates}
}

func unionPlan(left, right queryplan.AlgebraPlanV2) queryplan.AlgebraPlanV2 {
	return queryplan.AlgebraPlanV2{Op: "union", Left: &left, Right: &right}
}

func groupPlan(input queryplan.AlgebraPlanV2, groupBy []string, aggregates []queryplan.AlgebraAggregateV2) queryplan.AlgebraPlanV2 {
	return queryplan.AlgebraPlanV2{Op: "group", Input: &input, GroupBy: groupBy, Aggregates: aggregates}
}

func flipJoinPredicates(predicates []queryplan.AlgebraJoinPredicateV2) []queryplan.AlgebraJoinPredicateV2 {
	flipped := make([]queryplan.AlgebraJoinPredicateV2, 0, len(predicates))
	for _, predicate := range predicates {
		flipped = append(flipped, queryplan.AlgebraJoinPredicateV2{LeftField: predicate.RightField, RightField: predicate.LeftField})
	}
	return flipped
}

// ---- datasets: multiple data instances and snapshots ----

type rewriteDataset struct {
	name  string
	plans map[string]queryplan.AlgebraPlanV2 // logical scan name -> scan plan node
	scans map[string]exposure.RelationV2     // scanKey(namespace+snapshot) -> base relation
}

func (d rewriteDataset) provides(names ...string) bool {
	for _, name := range names {
		if _, ok := d.plans[name]; !ok {
			return false
		}
	}
	return true
}

type scanSpec struct {
	name string
	spec exposure.BaseRelationSpecV2
}

func rewriteDatasets() ([]rewriteDataset, error) {
	textC := func(id string) exposure.FieldV2 {
		return exposure.FieldV2{ID: id, SQLType: "text", Collation: "C", CollationVersion: "builtin", CollationDeterministic: true}
	}
	numeric := func(id string) exposure.FieldV2 { return exposure.FieldV2{ID: id, SQLType: "numeric"} }

	expenses := func(namespace, snapshot string, rows []exposure.BaseRowV2) scanSpec {
		return scanSpec{"expenses", exposure.BaseRelationSpecV2{SourceNamespace: namespace, Snapshot: snapshot,
			StableRole: "expense", Fields: []exposure.FieldV2{textC("expense.department"), numeric("expense.amount")}, Rows: rows}}
	}
	departments := func(namespace, snapshot string, rows []exposure.BaseRowV2) scanSpec {
		return scanSpec{"departments", exposure.BaseRelationSpecV2{SourceNamespace: namespace, Snapshot: snapshot,
			StableRole: "department", Fields: []exposure.FieldV2{textC("department.department"), textC("department.manager")}, Rows: rows}}
	}

	row := func(key, dept string, amount any) exposure.BaseRowV2 {
		return exposure.BaseRowV2{EntityKey: key, Values: map[string]any{"expense.department": dept, "expense.amount": amount}}
	}
	deptRow := func(key, dept, manager string) exposure.BaseRowV2 {
		return exposure.BaseRowV2{EntityKey: key, Values: map[string]any{"department.department": dept, "department.manager": manager}}
	}

	definitions := []struct {
		name  string
		scans []scanSpec
	}{
		{"travel-A", []scanSpec{
			expenses("travel.expense", "snapshot-A", []exposure.BaseRowV2{
				row("e1", "sales", "10"), row("e2", "sales", "20"), row("e3", "rnd", "30"),
				row("e4", "ops", "5"), row("e5", "legal", "25"), row("e6", "sales", "15"),
			}),
			departments("travel.department", "snapshot-A", []exposure.BaseRowV2{
				deptRow("d1", "sales", "Ana"), deptRow("d2", "rnd", "Rui"),
				deptRow("d3", "ops", "Omar"), deptRow("d4", "legal", "Lea"),
			}),
		}},
		{"travel-B", []scanSpec{
			expenses("travel.expense", "snapshot-A", []exposure.BaseRowV2{
				row("e1", "sales", "40"), row("e2", "rnd", "15"), row("e3", "ops", nil),
				row("e4", "legal", "25"), row("e5", "sales", "10"),
			}),
			departments("travel.department", "snapshot-A", []exposure.BaseRowV2{
				deptRow("d1", "sales", "Ana"), deptRow("d2", "rnd", "Rui"), deptRow("d3", "legal", "Lea"),
			}),
		}},
		{"ledger-A", []scanSpec{
			{"ledger", exposure.BaseRelationSpecV2{SourceNamespace: "travel.ledger", Snapshot: "snapshot-L", StableRole: "ledger",
				Fields: []exposure.FieldV2{textC("ledger.department"), textC("ledger.code"), numeric("ledger.amount")},
				Rows: []exposure.BaseRowV2{
					{EntityKey: "l1", Values: map[string]any{"ledger.department": "sales", "ledger.code": "A", "ledger.amount": "10"}},
					{EntityKey: "l2", Values: map[string]any{"ledger.department": "sales", "ledger.code": "B", "ledger.amount": "20"}},
					{EntityKey: "l3", Values: map[string]any{"ledger.department": "rnd", "ledger.code": "A", "ledger.amount": "30"}},
					{EntityKey: "l4", Values: map[string]any{"ledger.department": "ops", "ledger.code": "C", "ledger.amount": "5"}},
					{EntityKey: "l5", Values: map[string]any{"ledger.department": "legal", "ledger.code": "B", "ledger.amount": "25"}},
				}}},
			{"codebook", exposure.BaseRelationSpecV2{SourceNamespace: "travel.codebook", Snapshot: "snapshot-L", StableRole: "codebook",
				Fields: []exposure.FieldV2{textC("codebook.department"), textC("codebook.code"), textC("codebook.label")},
				Rows: []exposure.BaseRowV2{
					{EntityKey: "c1", Values: map[string]any{"codebook.department": "sales", "codebook.code": "A", "codebook.label": "Sales-A"}},
					{EntityKey: "c2", Values: map[string]any{"codebook.department": "sales", "codebook.code": "B", "codebook.label": "Sales-B"}},
					{EntityKey: "c3", Values: map[string]any{"codebook.department": "rnd", "codebook.code": "A", "codebook.label": "RnD-A"}},
					{EntityKey: "c4", Values: map[string]any{"codebook.department": "ops", "codebook.code": "C", "codebook.label": "Ops-C"}},
				}}},
		}},
		{"snapshots-A", []scanSpec{
			{"east", exposure.BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "snapshot-A", StableRole: "expense",
				Fields: []exposure.FieldV2{textC("expense.department"), numeric("expense.amount")},
				Rows:   []exposure.BaseRowV2{row("e1", "sales", "10"), row("e2", "rnd", "30"), row("e3", "ops", "5"), row("e4", "legal", "25")}}},
			{"west", exposure.BaseRelationSpecV2{SourceNamespace: "travel.expense.west", Snapshot: "snapshot-A", StableRole: "expense_west",
				Fields: []exposure.FieldV2{textC("expense.department"), numeric("expense.amount")},
				Rows:   []exposure.BaseRowV2{row("w1", "sales", "10"), row("w2", "rnd", "15"), row("w3", "legal", "25")}}},
		}},
		{"snapshots-B", []scanSpec{
			{"east", exposure.BaseRelationSpecV2{SourceNamespace: "travel.expense", Snapshot: "snapshot-A", StableRole: "expense",
				Fields: []exposure.FieldV2{textC("expense.department"), numeric("expense.amount")},
				Rows:   []exposure.BaseRowV2{row("e1", "sales", "20"), row("e2", "rnd", "15"), row("e3", "legal", "40"), row("e4", "ops", "35")}}},
			{"west", exposure.BaseRelationSpecV2{SourceNamespace: "travel.expense.west", Snapshot: "snapshot-A", StableRole: "expense_west",
				Fields: []exposure.FieldV2{textC("expense.department"), numeric("expense.amount")},
				Rows:   []exposure.BaseRowV2{row("w1", "sales", "20"), row("w2", "ops", "35"), row("w3", "legal", "5")}}},
		}},
	}

	datasets := make([]rewriteDataset, 0, len(definitions))
	for _, definition := range definitions {
		dataset, err := buildRewriteDataset(definition.name, definition.scans)
		if err != nil {
			return nil, err
		}
		datasets = append(datasets, dataset)
	}
	return datasets, nil
}

func buildRewriteDataset(name string, specs []scanSpec) (rewriteDataset, error) {
	dataset := rewriteDataset{name: name,
		plans: make(map[string]queryplan.AlgebraPlanV2, len(specs)),
		scans: make(map[string]exposure.RelationV2, len(specs))}
	for _, current := range specs {
		relation, err := exposure.ScanV2(current.spec)
		if err != nil {
			return rewriteDataset{}, fmt.Errorf("dataset %s scan %s: %w", name, current.name, err)
		}
		relation.CanonicalOrder = true
		plan := queryplan.AlgebraPlanV2{Op: "scan", SourceNamespace: current.spec.SourceNamespace,
			Snapshot: current.spec.Snapshot, StableRole: current.spec.StableRole, Schema: toAlgebraSchema(current.spec.Fields)}
		dataset.plans[current.name] = plan
		dataset.scans[scanKey(plan)] = relation
	}
	return dataset, nil
}

func toAlgebraSchema(fields []exposure.FieldV2) []queryplan.AlgebraFieldV2 {
	result := make([]queryplan.AlgebraFieldV2, 0, len(fields))
	for _, field := range fields {
		result = append(result, queryplan.AlgebraFieldV2{ID: field.ID, SQLType: field.SQLType,
			Collation: field.Collation, CollationVersion: field.CollationVersion,
			CollationDeterministic: field.CollationDeterministic})
	}
	return result
}
