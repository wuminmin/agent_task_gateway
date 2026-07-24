package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	NormalFormVersion = "taskgate-query-normal-form-v2"
	normalFormDomain  = "TASKGATE-QUERY-NORMAL-FORM-V2\x00"
)

// NormalForm is intentionally a restricted syntactic normal form, not an
// arbitrary SQL equivalence oracle. Output aliases and display order are not
// part of its identity; predicate conjunctions and group expressions are.
type NormalForm struct {
	Version         string                `json:"version"`
	Profile         string                `json:"profile"`
	SourceNamespace string                `json:"source_namespace"`
	Snapshot        string                `json:"snapshot"`
	LineageDigest   string                `json:"lineage_digest,omitempty"`
	Columns         []string              `json:"columns,omitempty"`
	Aggregates      []NormalizedAggregate `json:"aggregates,omitempty"`
	Filters         []NormalizedFilter    `json:"filters,omitempty"`
	GroupBy         []string              `json:"group_by,omitempty"`
	OrderBy         []NormalizedOrder     `json:"order_by,omitempty"`
	Limit           int                   `json:"limit,omitempty"`
	Offset          int                   `json:"offset,omitempty"`
	BagSemantics    string                `json:"bag_semantics"`
	NullLogic       string                `json:"null_logic"`
	Collation       string                `json:"collation"`
	Timezone        string                `json:"timezone"`
	NumericMode     string                `json:"numeric_mode"`
}

type NormalizedAggregate struct {
	Expression string `json:"expression"`
	SQLType    string `json:"sql_type"`
}

type NormalizedFilter struct {
	Column string          `json:"column"`
	Op     string          `json:"op"`
	Value  json.RawMessage `json:"value"`
}

type NormalizedOrder struct {
	Expression string `json:"expression"`
	Direction  string `json:"direction"`
}

// NormalizeV2 applies the explicitly supported V2 rewrite set: aliases are
// erased, qualified field IDs/functions/operators are normalized, AND terms,
// projections, aggregates and group keys are sorted, and pagination receives
// a stable entity/group-key suffix.
func NormalizeV2(plan QueryPlan, product Product) (NormalForm, error) {
	if product.SourceNamespace == "" || product.Snapshot == "" {
		return NormalForm{}, errors.New("V2 normal form requires source namespace and immutable snapshot")
	}
	if _, err := Compile(plan, product); err != nil {
		return NormalForm{}, err
	}
	result := NormalForm{
		Version: NormalFormVersion, Profile: "taskgate-exposure-v2",
		SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot, LineageDigest: product.LineageDigest,
		BagSemantics: "postgresql-bag", NullLogic: "postgresql-three-valued",
		Collation: "snapshot-bound-database-default", Timezone: "UTC", NumericMode: "postgresql-exact",
		Limit: plan.Limit, Offset: plan.Offset,
	}
	for _, field := range sortedUnique(plan.Columns) {
		result.Columns = append(result.Columns, canonicalField(product.SourceNamespace, field))
	}
	aliases := make(map[string]string, len(plan.Aggregates))
	for _, aggregate := range plan.Aggregates {
		function := strings.ToLower(strings.TrimSpace(aggregate.Function))
		column := "*"
		if aggregate.Column != "*" {
			column = canonicalField(product.SourceNamespace, aggregate.Column)
		}
		expression := function + "(" + column + ")"
		aliases[aggregate.Alias] = expression
		result.Aggregates = append(result.Aggregates, NormalizedAggregate{Expression: expression, SQLType: aggregateOutputType(function, product.ColumnTypes[aggregate.Column])})
	}
	sort.Slice(result.Aggregates, func(i, j int) bool {
		if result.Aggregates[i].Expression != result.Aggregates[j].Expression {
			return result.Aggregates[i].Expression < result.Aggregates[j].Expression
		}
		return result.Aggregates[i].SQLType < result.Aggregates[j].SQLType
	})
	for _, filter := range plan.Filters {
		value, err := canonicalJSON(filter.Value)
		if err != nil {
			return NormalForm{}, fmt.Errorf("normalize filter %s: %w", filter.Column, err)
		}
		op := strings.ToUpper(strings.TrimSpace(filter.Op))
		if op == "!=" {
			op = "<>"
		}
		result.Filters = append(result.Filters, NormalizedFilter{Column: canonicalField(product.SourceNamespace, filter.Column), Op: op, Value: value})
	}
	sort.Slice(result.Filters, func(i, j int) bool {
		left, _ := json.Marshal(result.Filters[i])
		right, _ := json.Marshal(result.Filters[j])
		return string(left) < string(right)
	})
	for _, field := range sortedUnique(plan.GroupBy) {
		result.GroupBy = append(result.GroupBy, canonicalField(product.SourceNamespace, field))
	}
	for _, order := range plan.OrderBy {
		expression := aliases[order.Column]
		if expression == "" {
			expression = canonicalField(product.SourceNamespace, order.Column)
		}
		direction := strings.ToUpper(strings.TrimSpace(order.Direction))
		if direction == "" {
			direction = "ASC"
		}
		result.OrderBy = append(result.OrderBy, NormalizedOrder{Expression: expression, Direction: direction})
	}
	if plan.Limit > 0 || plan.Offset > 0 {
		stable := product.StableEntityKey
		if len(plan.GroupBy) > 0 {
			stable = sortedUnique(plan.GroupBy)
		} else if len(plan.Aggregates) > 0 {
			stable = nil // a global aggregate has at most one output row
		}
		ordered := make(map[string]struct{}, len(result.OrderBy))
		for _, order := range result.OrderBy {
			ordered[order.Expression] = struct{}{}
		}
		for _, field := range stable {
			expression := canonicalField(product.SourceNamespace, field)
			if _, present := ordered[expression]; present {
				continue
			}
			result.OrderBy = append(result.OrderBy, NormalizedOrder{Expression: expression, Direction: "ASC"})
			ordered[expression] = struct{}{}
		}
		if len(result.OrderBy) == 0 && len(plan.Aggregates) == 0 {
			return NormalForm{}, errors.New("V2 pagination requires a catalog stable entity key")
		}
	}
	return result, nil
}

func (normal NormalForm) Digest() (string, error) {
	if normal.Version != NormalFormVersion || normal.Profile != "taskgate-exposure-v2" {
		return "", errors.New("invalid V2 normal form")
	}
	encoded, err := json.Marshal(normal)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(normalFormDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func canonicalField(namespace, field string) string {
	return namespace + "." + strings.ToLower(strings.TrimSpace(field))
}

func aggregateOutputType(function, input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	switch function {
	case "count":
		return "bigint"
	case "sum":
		switch input {
		case "smallint", "integer":
			return "bigint"
		case "bigint":
			return "numeric"
		}
	}
	return input
}

func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if _, present := seen[value]; present {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func canonicalJSON(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// AlgebraPlanV2 is the profile-level operator grammar used by the semantics
// and proofs. The production SQL compiler currently lowers its single-source
// scan/select/project/group/page subset; Join and Union normalization is still
// executable and independently testable at the annotated-algebra boundary.
type AlgebraPlanV2 struct {
	Op              string
	SourceNamespace string
	Snapshot        string
	StableRole      string
	Input           *AlgebraPlanV2
	Left            *AlgebraPlanV2
	Right           *AlgebraPlanV2
	Fields          []string
	Predicates      []NormalizedFilter
	JoinPredicates  []string
	GroupBy         []string
	Aggregates      []NormalizedAggregate
	OrderBy         []NormalizedOrder
	StableOrderKey  []string
	Limit           int
	Offset          int
	UnionAll        bool
}

type AlgebraNormalFormV2 struct {
	Canonical json.RawMessage `json:"canonical"`
	SHA256    string          `json:"sha256"`
}

// NormalizeAlgebraV2 canonicalizes exactly Scan, Select, Project, Join,
// Union-distinct, Group and Page. Join/Union children are digest ordered,
// duplicate Union branches are idempotent, and UNION ALL is fail-closed.
func NormalizeAlgebraV2(plan AlgebraPlanV2) (AlgebraNormalFormV2, error) {
	canonical, err := normalizeAlgebraNodeV2(plan)
	if err != nil {
		return AlgebraNormalFormV2{}, err
	}
	digest := sha256.Sum256(append([]byte(normalFormDomain+"ALGEBRA\x00"), canonical...))
	return AlgebraNormalFormV2{Canonical: canonical, SHA256: hex.EncodeToString(digest[:])}, nil
}

func normalizeAlgebraNodeV2(plan AlgebraPlanV2) (json.RawMessage, error) {
	op := strings.ToLower(strings.TrimSpace(plan.Op))
	switch op {
	case "scan":
		if plan.SourceNamespace == "" || plan.Snapshot == "" || plan.StableRole == "" {
			return nil, errors.New("V2 scan normal form requires namespace, snapshot, and stable role")
		}
		return json.Marshal(map[string]any{"op": "scan", "namespace": plan.SourceNamespace, "snapshot": plan.Snapshot, "role": plan.StableRole})
	case "select":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return nil, err
		}
		predicates := append([]NormalizedFilter(nil), plan.Predicates...)
		sort.Slice(predicates, func(i, j int) bool {
			left, _ := json.Marshal(predicates[i])
			right, _ := json.Marshal(predicates[j])
			return string(left) < string(right)
		})
		return json.Marshal(map[string]any{"op": "select", "input": input, "predicates": predicates})
	case "project":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"op": "project", "input": input, "fields": sortedUnique(plan.Fields)})
	case "join", "union":
		if plan.Left == nil || plan.Right == nil {
			return nil, errors.New("V2 binary normal form requires two inputs")
		}
		if op == "union" && plan.UnionAll {
			return nil, errors.New("UNION ALL is outside taskgate-exposure-v2")
		}
		left, err := normalizeAlgebraNodeV2(*plan.Left)
		if err != nil {
			return nil, err
		}
		right, err := normalizeAlgebraNodeV2(*plan.Right)
		if err != nil {
			return nil, err
		}
		if string(right) < string(left) {
			left, right = right, left
		}
		if op == "union" && string(left) == string(right) {
			return left, nil
		}
		predicates := append([]string(nil), plan.JoinPredicates...)
		sort.Strings(predicates)
		return json.Marshal(map[string]any{"op": op, "left": left, "right": right, "predicates": predicates})
	case "group":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return nil, err
		}
		aggregates := append([]NormalizedAggregate(nil), plan.Aggregates...)
		sort.Slice(aggregates, func(i, j int) bool {
			return aggregates[i].Expression+"\x00"+aggregates[i].SQLType < aggregates[j].Expression+"\x00"+aggregates[j].SQLType
		})
		return json.Marshal(map[string]any{"op": "group", "input": input, "group_by": sortedUnique(plan.GroupBy), "aggregates": aggregates})
	case "page":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return nil, err
		}
		if plan.Limit < 0 || plan.Offset < 0 {
			return nil, errors.New("V2 page bounds cannot be negative")
		}
		orders := append([]NormalizedOrder(nil), plan.OrderBy...)
		seen := make(map[string]struct{}, len(orders))
		for _, order := range orders {
			seen[order.Expression] = struct{}{}
		}
		for _, key := range plan.StableOrderKey {
			if _, present := seen[key]; !present {
				orders = append(orders, NormalizedOrder{Expression: key, Direction: "ASC"})
				seen[key] = struct{}{}
			}
		}
		if len(orders) == 0 {
			return nil, errors.New("V2 page requires a canonical total order")
		}
		return json.Marshal(map[string]any{"op": "page", "input": input, "order_by": orders, "limit": plan.Limit, "offset": plan.Offset})
	default:
		return nil, fmt.Errorf("operator %q is outside taskgate-exposure-v2", plan.Op)
	}
}

func requiredAlgebraInputV2(input *AlgebraPlanV2) (json.RawMessage, error) {
	if input == nil {
		return nil, errors.New("V2 unary normal form requires an input")
	}
	return normalizeAlgebraNodeV2(*input)
}
