package viewcompiler

import (
	"strings"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// CodeCompositionUnsupported identifies a request that cannot be represented
// by flattening a public semantic View query into the closed QueryPlan grammar.
// It is separate from definition compilation failures: the attested View may
// be valid even though an operation above it crosses an aggregation barrier or
// otherwise falls outside the runtime fragment.
const CodeCompositionUnsupported ErrorCode = "VIEW_COMPOSITION_UNSUPPORTED"

// Composition keeps execution grammar separate from result presentation. A
// flattened aggregate plan necessarily stores fields and aggregates in
// separate QueryPlan slices, so VisibleFields preserves the public outer
// order that QueryPlan alone cannot represent. Field entries are stable
// FieldIDs; aggregate entries are their expanded aliases.
type Composition struct {
	Plan          queryplan.QueryPlan `json:"plan"`
	VisibleFields []string            `json:"visible_fields"`
}

// ComposeQueryPlan replaces a single public semantic-View product with its
// attested, expanded plan. It performs no catalog access and never mutates the
// outer plan or Artifact.
//
// An ungrouped Artifact is transparent: public output names in the outer plan
// are replaced by their stable FieldIDs, while the Artifact input and filters
// are retained. A grouped Artifact is an aggregation barrier and admits only
// a direct projection of its already-computed public outputs.
//
// ORDER BY and pagination are deliberately rejected for every semantic View,
// including one-source expansions. Composed plans enter CompileRelational,
// whose online accounting fragment does not admit those operations.
func ComposeQueryPlan(rootProduct string, outer queryplan.QueryPlan, artifact Artifact) (Composition, error) {
	if !catalogIdentifier.MatchString(rootProduct) {
		return Composition{}, compositionReject(artifact, "root product %q is not a canonical product identifier", rootProduct)
	}
	if outer.From != nil || outer.Product != rootProduct {
		return Composition{}, compositionReject(artifact,
			"outer plan must be a single legacy Product %q with no from operator", rootProduct)
	}
	if outer.Limit < 0 || outer.Offset < 0 {
		return Composition{}, compositionReject(artifact, "limit and offset cannot be negative")
	}
	if len(outer.OrderBy) != 0 || outer.Limit != 0 || outer.Offset != 0 {
		return Composition{}, compositionReject(artifact,
			"ORDER BY, LIMIT, and OFFSET are outside semantic View composition")
	}

	index, err := indexCompositionArtifact(artifact)
	if err != nil {
		return Composition{}, err
	}
	if len(outer.Columns)+len(outer.Aggregates) == 0 {
		return Composition{}, compositionReject(artifact, "outer plan has an empty select list")
	}
	if index.barrier {
		return composeBarrierProjection(outer, artifact, index)
	}
	return composeTransparentPlan(outer, artifact, index)
}

type compositionArtifactIndex struct {
	outputs    map[string]Output
	aggregates map[string]queryplan.Aggregate
	barrier    bool
}

func indexCompositionArtifact(artifact Artifact) (compositionArtifactIndex, error) {
	plan := artifact.Plan
	fail := func(format string, args ...any) (compositionArtifactIndex, error) {
		return compositionArtifactIndex{}, compositionReject(artifact, format, args...)
	}
	if plan.Product != "" {
		return fail("compiled View artifact unexpectedly names legacy product %q", plan.Product)
	}
	if plan.From == nil {
		return fail("compiled View artifact has no from operator")
	}
	members := 0
	if plan.From.Scan != nil {
		members++
	}
	if plan.From.Join != nil {
		members++
	}
	if plan.From.JoinMany != nil {
		members++
	}
	if plan.From.UnionDistinct != nil {
		members++
	}
	if members != 1 {
		return fail("compiled View artifact must have exactly one from operator")
	}
	switch {
	case plan.From.Scan != nil:
		// A compiler-produced transparent scan has exactly one governed leaf.
	case plan.From.JoinMany != nil:
		if len(plan.From.JoinMany.Sources) < 2 || len(plan.From.JoinMany.Sources) > queryplan.MaxJoinSources {
			return fail("compiled View artifact join source count is outside 2..%d", queryplan.MaxJoinSources)
		}
	default:
		return fail("compiled View artifact contains an operator outside Scan/JoinMany")
	}
	if len(plan.OrderBy) != 0 || plan.Limit != 0 || plan.Offset != 0 {
		return fail("compiled View artifact unexpectedly contains order or pagination")
	}
	if len(plan.Columns)+len(plan.Aggregates) == 0 || len(artifact.Outputs) != len(plan.Columns)+len(plan.Aggregates) {
		return fail("compiled View artifact output metadata is incomplete")
	}

	result := compositionArtifactIndex{
		outputs:    make(map[string]Output, len(artifact.Outputs)),
		aggregates: make(map[string]queryplan.Aggregate, len(plan.Aggregates)),
		barrier:    len(plan.GroupBy) != 0 || len(plan.Aggregates) != 0,
	}
	seenFields := make(map[string]struct{}, len(plan.Columns))
	for _, field := range plan.Columns {
		if _, _, ok := splitFieldID(field); !ok {
			return fail("compiled View artifact field %q is not a stable FieldID", field)
		}
		if _, duplicate := seenFields[field]; duplicate {
			return fail("compiled View artifact repeats field %q", field)
		}
		seenFields[field] = struct{}{}
	}
	seenGroups := make(map[string]struct{}, len(plan.GroupBy))
	for _, field := range plan.GroupBy {
		if _, _, ok := splitFieldID(field); !ok {
			return fail("compiled View artifact group field %q is not a stable FieldID", field)
		}
		if _, duplicate := seenGroups[field]; duplicate {
			return fail("compiled View artifact repeats group field %q", field)
		}
		seenGroups[field] = struct{}{}
	}
	seenAggregateAliases := make(map[string]struct{}, len(plan.Aggregates))
	for _, aggregate := range plan.Aggregates {
		function := strings.ToLower(strings.TrimSpace(aggregate.Function))
		if !compositionAggregate(function) || !catalogIdentifier.MatchString(aggregate.Alias) || strings.HasPrefix(aggregate.Alias, "tg_") {
			return fail("compiled View artifact aggregate %q is incomplete", aggregate.Alias)
		}
		if aggregate.Column == "*" {
			if function != "count" {
				return fail("compiled View artifact aggregate %q cannot consume *", aggregate.Function)
			}
		} else if _, _, ok := splitFieldID(aggregate.Column); !ok {
			return fail("compiled View artifact aggregate argument %q is not a stable FieldID", aggregate.Column)
		}
		if _, duplicate := seenAggregateAliases[aggregate.Alias]; duplicate {
			return fail("compiled View artifact repeats aggregate alias %q", aggregate.Alias)
		}
		seenAggregateAliases[aggregate.Alias] = struct{}{}
	}

	fieldIndex, aggregateIndex := 0, 0
	for _, output := range artifact.Outputs {
		if !catalogIdentifier.MatchString(output.Name) {
			return fail("compiled View artifact output %q has an invalid public name", output.Name)
		}
		if _, duplicate := result.outputs[output.Name]; duplicate {
			return fail("compiled View artifact repeats public output %q", output.Name)
		}
		switch output.Kind {
		case OutputField:
			if output.FieldID == "" || output.Function != "" || output.Argument != "" || fieldIndex >= len(plan.Columns) ||
				plan.Columns[fieldIndex] != output.FieldID {
				return fail("compiled View artifact field output %q is not bound to its plan field", output.Name)
			}
			fieldIndex++
		case OutputAggregate:
			if output.FieldID != "" || aggregateIndex >= len(plan.Aggregates) {
				return fail("compiled View artifact aggregate output %q is not bound to its plan aggregate", output.Name)
			}
			aggregate := plan.Aggregates[aggregateIndex]
			if output.Name != aggregate.Alias || output.Function != aggregate.Function || output.Argument != aggregate.Column {
				return fail("compiled View artifact aggregate output %q disagrees with its plan aggregate", output.Name)
			}
			result.aggregates[output.Name] = aggregate
			aggregateIndex++
		default:
			return fail("compiled View artifact output %q has unknown kind %q", output.Name, output.Kind)
		}
		result.outputs[output.Name] = output
	}
	if fieldIndex != len(plan.Columns) || aggregateIndex != len(plan.Aggregates) {
		return fail("compiled View artifact output metadata does not cover its plan")
	}
	if !result.barrier && aggregateIndex != 0 {
		return fail("compiled View artifact exposes an aggregate without an aggregation barrier")
	}
	return result, nil
}

func composeTransparentPlan(outer queryplan.QueryPlan, artifact Artifact, index compositionArtifactIndex) (Composition, error) {
	result := cloneCompositionPlan(artifact.Plan)
	result.Product = ""
	result.Columns = nil
	result.Aggregates = nil
	result.GroupBy = nil

	selectNames := make(map[string]struct{}, len(outer.Columns)+len(outer.Aggregates))
	visible := make([]string, 0, len(outer.Columns)+len(outer.Aggregates))
	for _, name := range outer.Columns {
		output, err := compositionFieldOutput(artifact, index, name, "selected column")
		if err != nil {
			return Composition{}, err
		}
		if _, duplicate := selectNames[name]; duplicate {
			return Composition{}, compositionReject(artifact, "outer plan repeats selected output %q", name)
		}
		selectNames[name] = struct{}{}
		result.Columns = append(result.Columns, output.FieldID)
		visible = append(visible, output.FieldID)
	}

	result.Filters = cloneCompositionFilters(artifact.Plan.Filters)
	for _, filter := range outer.Filters {
		output, err := compositionFieldOutput(artifact, index, filter.Column, "filter")
		if err != nil {
			return Composition{}, err
		}
		mapped := filter
		mapped.Column = output.FieldID
		mapped.Value = cloneCompositionValue(filter.Value)
		result.Filters = append(result.Filters, mapped)
	}

	seenGroups := make(map[string]struct{}, len(outer.GroupBy))
	for _, name := range outer.GroupBy {
		output, err := compositionFieldOutput(artifact, index, name, "group field")
		if err != nil {
			return Composition{}, err
		}
		if _, duplicate := seenGroups[output.FieldID]; duplicate {
			return Composition{}, compositionReject(artifact, "outer plan repeats group field %q", name)
		}
		seenGroups[output.FieldID] = struct{}{}
		result.GroupBy = append(result.GroupBy, output.FieldID)
	}

	for _, aggregate := range outer.Aggregates {
		function := strings.ToLower(strings.TrimSpace(aggregate.Function))
		if !compositionAggregate(function) {
			return Composition{}, compositionReject(artifact, "outer aggregate %q is outside COUNT/SUM/MIN/MAX", aggregate.Function)
		}
		if !catalogIdentifier.MatchString(aggregate.Alias) || strings.HasPrefix(aggregate.Alias, "tg_") {
			return Composition{}, compositionReject(artifact, "outer aggregate alias %q is invalid", aggregate.Alias)
		}
		if _, duplicate := selectNames[aggregate.Alias]; duplicate {
			return Composition{}, compositionReject(artifact, "outer plan repeats select name %q", aggregate.Alias)
		}
		selectNames[aggregate.Alias] = struct{}{}
		mapped := aggregate
		if aggregate.Column == "*" {
			if function != "count" {
				return Composition{}, compositionReject(artifact, "outer aggregate %q cannot consume *", aggregate.Function)
			}
		} else {
			output, mapErr := compositionFieldOutput(artifact, index, aggregate.Column, "aggregate argument")
			if mapErr != nil {
				return Composition{}, mapErr
			}
			mapped.Column = output.FieldID
		}
		result.Aggregates = append(result.Aggregates, mapped)
		visible = append(visible, aggregate.Alias)
	}

	grouped := len(result.GroupBy) != 0 || len(result.Aggregates) != 0
	if grouped {
		groups := make(map[string]struct{}, len(result.GroupBy))
		for _, field := range result.GroupBy {
			groups[field] = struct{}{}
		}
		for _, field := range result.Columns {
			if _, present := groups[field]; !present {
				return Composition{}, compositionReject(artifact,
					"selected field %q is not present in outer group_by", field)
			}
		}
	}
	return Composition{Plan: result, VisibleFields: visible}, nil
}

func composeBarrierProjection(outer queryplan.QueryPlan, artifact Artifact, index compositionArtifactIndex) (Composition, error) {
	if len(outer.Filters) != 0 || len(outer.GroupBy) != 0 || len(outer.Aggregates) != 0 {
		return Composition{}, compositionReject(artifact,
			"an aggregated View admits direct projection only; filter, grouping, and reaggregation are forbidden")
	}
	result := cloneCompositionPlan(artifact.Plan)
	result.Product = ""
	result.Columns = nil
	result.Aggregates = nil
	seen := make(map[string]struct{}, len(outer.Columns))
	visible := make([]string, 0, len(outer.Columns))
	for _, name := range outer.Columns {
		if _, duplicate := seen[name]; duplicate {
			return Composition{}, compositionReject(artifact, "outer plan repeats selected output %q", name)
		}
		seen[name] = struct{}{}
		output, present := index.outputs[name]
		if !present {
			return Composition{}, compositionReject(artifact, "selected output %q is not exposed by the View", name)
		}
		switch output.Kind {
		case OutputField:
			result.Columns = append(result.Columns, output.FieldID)
			visible = append(visible, output.FieldID)
		case OutputAggregate:
			aggregate, present := index.aggregates[name]
			if !present {
				return Composition{}, compositionReject(artifact,
					"aggregate output %q has no complete plan binding", name)
			}
			result.Aggregates = append(result.Aggregates, aggregate)
			visible = append(visible, aggregate.Alias)
		default:
			return Composition{}, compositionReject(artifact, "selected output %q has unknown kind", name)
		}
	}
	return Composition{Plan: result, VisibleFields: visible}, nil
}

func compositionFieldOutput(artifact Artifact, index compositionArtifactIndex, name, usage string) (Output, error) {
	output, present := index.outputs[name]
	if !present {
		return Output{}, compositionReject(artifact, "%s %q is not exposed by the View", usage, name)
	}
	if output.Kind != OutputField || output.FieldID == "" {
		return Output{}, compositionReject(artifact, "%s %q resolves to an aggregate output, not a field", usage, name)
	}
	return output, nil
}

func compositionAggregate(function string) bool {
	switch function {
	case "count", "sum", "min", "max":
		return true
	default:
		return false
	}
}

func compositionReject(artifact Artifact, format string, args ...any) *Error {
	return reject(CodeCompositionUnsupported, artifact.Root, format, args...)
}

func cloneCompositionPlan(plan queryplan.QueryPlan) queryplan.QueryPlan {
	result := plan
	result.Columns = cloneCompositionStrings(plan.Columns)
	result.Aggregates = cloneCompositionAggregates(plan.Aggregates)
	result.Filters = cloneCompositionFilters(plan.Filters)
	result.GroupBy = cloneCompositionStrings(plan.GroupBy)
	result.OrderBy = cloneCompositionOrders(plan.OrderBy)
	result.From = cloneCompositionFrom(plan.From)
	return result
}

func cloneCompositionFrom(input *queryplan.From) *queryplan.From {
	if input == nil {
		return nil
	}
	result := &queryplan.From{}
	if input.Scan != nil {
		scan := cloneCompositionScan(*input.Scan)
		result.Scan = &scan
	}
	if input.Join != nil {
		join := *input.Join
		join.Left = cloneCompositionScan(input.Join.Left)
		join.Right = cloneCompositionScan(input.Join.Right)
		join.On = cloneCompositionPredicates(input.Join.On)
		result.Join = &join
	}
	if input.JoinMany != nil {
		join := *input.JoinMany
		if input.JoinMany.Sources != nil {
			join.Sources = make([]queryplan.Scan, len(input.JoinMany.Sources))
			for i, scan := range input.JoinMany.Sources {
				join.Sources[i] = cloneCompositionScan(scan)
			}
		}
		join.On = cloneCompositionPredicates(input.JoinMany.On)
		result.JoinMany = &join
	}
	if input.UnionDistinct != nil {
		union := *input.UnionDistinct
		union.Columns = cloneCompositionStrings(input.UnionDistinct.Columns)
		union.Left = cloneCompositionScan(input.UnionDistinct.Left)
		union.Right = cloneCompositionScan(input.UnionDistinct.Right)
		result.UnionDistinct = &union
	}
	return result
}

func cloneCompositionScan(input queryplan.Scan) queryplan.Scan {
	input.Filters = cloneCompositionFilters(input.Filters)
	return input
}

func cloneCompositionFilters(input []queryplan.Filter) []queryplan.Filter {
	if input == nil {
		return nil
	}
	result := make([]queryplan.Filter, len(input))
	for i, filter := range input {
		result[i] = filter
		result[i].Value = cloneCompositionValue(filter.Value)
	}
	return result
}

func cloneCompositionValue(value any) any {
	switch typed := value.(type) {
	case []any:
		if typed == nil {
			return []any(nil)
		}
		result := make([]any, len(typed))
		for i, item := range typed {
			result[i] = cloneCompositionValue(item)
		}
		return result
	case map[string]any:
		if typed == nil {
			return map[string]any(nil)
		}
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = cloneCompositionValue(item)
		}
		return result
	default:
		return value
	}
}

func cloneCompositionStrings(input []string) []string {
	if input == nil {
		return nil
	}
	return append(make([]string, 0, len(input)), input...)
}

func cloneCompositionAggregates(input []queryplan.Aggregate) []queryplan.Aggregate {
	if input == nil {
		return nil
	}
	return append(make([]queryplan.Aggregate, 0, len(input)), input...)
}

func cloneCompositionOrders(input []queryplan.Order) []queryplan.Order {
	if input == nil {
		return nil
	}
	return append(make([]queryplan.Order, 0, len(input)), input...)
}

func cloneCompositionPredicates(input []queryplan.JoinPredicate) []queryplan.JoinPredicate {
	if input == nil {
		return nil
	}
	return append(make([]queryplan.JoinPredicate, 0, len(input)), input...)
}
