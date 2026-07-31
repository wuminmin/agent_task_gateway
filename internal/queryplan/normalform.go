package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	NormalFormVersion = "taskgate-query-normal-form-v3"
	normalFormDomain  = "TASKGATE-QUERY-NORMAL-FORM-V3\x00"
	attestedCollation = "postgresql-deterministic-exact-v1"
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
	Collations      []NormalizedCollation `json:"collations,omitempty"`
	Timezone        string                `json:"timezone"`
	NumericMode     string                `json:"numeric_mode"`
}

type NormalizedAggregate struct {
	Expression string `json:"expression"`
	SQLType    string `json:"sql_type"`
}

type NormalizedFilter struct {
	Column  string          `json:"column"`
	SQLType string          `json:"sql_type"`
	Op      string          `json:"op"`
	Value   json.RawMessage `json:"value"`
}

type NormalizedOrder struct {
	Expression string `json:"expression"`
	Direction  string `json:"direction"`
}

type NormalizedCollation struct {
	Column  string `json:"column"`
	Name    string `json:"name"`
	Version string `json:"version"`
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
	if err := validateProductSQLProfileV2(product); err != nil {
		return NormalForm{}, err
	}
	result := NormalForm{
		Version: NormalFormVersion, Profile: "taskgate-exposure-v2",
		SourceNamespace: product.SourceNamespace, Snapshot: product.Snapshot, LineageDigest: product.LineageDigest,
		BagSemantics: "postgresql-bag", NullLogic: "postgresql-three-valued",
		Collation: attestedCollation, Timezone: "UTC", NumericMode: "postgresql-exact",
		Limit: plan.Limit, Offset: plan.Offset,
	}
	for column, sqlType := range product.ColumnTypes {
		typeName, _ := exposure.CanonicalSQLTypeV2(sqlType)
		if !isCollatableSQLTypeV2(typeName) {
			continue
		}
		result.Collations = append(result.Collations, NormalizedCollation{Column: canonicalField(product.SourceNamespace, column),
			Name: product.ColumnCollations[column], Version: product.CollationVersions[column]})
	}
	sort.Slice(result.Collations, func(i, j int) bool { return result.Collations[i].Column < result.Collations[j].Column })
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
		outputType, err := exposure.CanonicalSQLTypeV2(aggregateOutputType(function, product.ColumnTypes[aggregate.Column]))
		if err != nil {
			return NormalForm{}, err
		}
		result.Aggregates = append(result.Aggregates, NormalizedAggregate{Expression: expression, SQLType: outputType})
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
		typeName, err := exposure.CanonicalSQLTypeV2(product.ColumnTypes[filter.Column])
		if err != nil {
			return NormalForm{}, err
		}
		result.Filters = append(result.Filters, NormalizedFilter{Column: canonicalField(product.SourceNamespace, filter.Column), SQLType: typeName, Op: op, Value: value})
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
	canonical, err := exposure.CanonicalSQLTypeV2(input)
	if err == nil {
		input = canonical
	} else {
		input = strings.ToLower(strings.TrimSpace(input))
	}
	switch function {
	case "count":
		return "bigint"
	case "sum":
		// SUM is admitted only over the exact/integer fragment; float SUM is
		// excluded because IEEE-754 addition is order-dependent and the typed
		// normal form does not fix a physical aggregation order. This mirrors
		// expectedAggregateOutputTypeV2 in the exposure algebra.
		switch input {
		case "smallint", "integer":
			return "bigint"
		case "bigint":
			return "numeric"
		case "numeric":
			return "numeric"
		}
	case "min", "max":
		switch input {
		case "smallint", "integer", "bigint", "numeric", "real", "double precision",
			"date", "time without time zone", "timestamp with time zone", "timestamp without time zone",
			"text", "character", "character varying":
			return input
		}
	}
	return ""
}

func validateProductSQLProfileV2(product Product) error {
	if len(product.Columns) == 0 || len(product.ColumnTypes) == 0 {
		return errors.New("V2 normal form requires a typed product schema")
	}
	for column := range product.Columns {
		if !safeIdentifier(column) || strings.TrimSpace(product.ColumnTypes[column]) == "" {
			return fmt.Errorf("V2 approved column %q lacks a safe typed identity", column)
		}
	}
	for _, column := range product.StableEntityKey {
		if !safeIdentifier(column) || strings.TrimSpace(product.ColumnTypes[column]) == "" {
			return fmt.Errorf("V2 entity-key column %q lacks a safe typed identity", column)
		}
	}
	for _, column := range product.RequiredEvidence {
		if !safeIdentifier(column) || strings.TrimSpace(product.ColumnTypes[column]) == "" {
			return fmt.Errorf("V2 required evidence column %q lacks a safe typed identity", column)
		}
	}
	for column, declaredType := range product.ColumnTypes {
		if !safeIdentifier(column) {
			return fmt.Errorf("V2 column %q has an invalid identifier", column)
		}
		typeName, err := exposure.CanonicalSQLTypeV2(declaredType)
		if err != nil {
			return fmt.Errorf("V2 column %q: %w", column, err)
		}
		collation := strings.TrimSpace(product.ColumnCollations[column])
		collationVersion := strings.TrimSpace(product.CollationVersions[column])
		if isCollatableSQLTypeV2(typeName) {
			if collation == "" || collationVersion == "" {
				return fmt.Errorf("V2 collatable column %q requires an exact attested collation name and version", column)
			}
		} else if collation != "" || collationVersion != "" {
			return fmt.Errorf("V2 non-collatable column %q cannot declare a collation profile", column)
		}
	}
	return nil
}

func isCollatableSQLTypeV2(sqlType string) bool {
	return sqlType == "text" || sqlType == "character" || sqlType == "character varying"
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
// and proofs. The production SQL compiler lowers its single-source subset and
// a fail-closed finite JoinMany/two-branch Union subset; the algebra itself
// remains closed under nesting.
type AlgebraPlanV2 struct {
	Op              string
	SourceNamespace string
	Snapshot        string
	LineageDigest   string
	StableRole      string
	Schema          []AlgebraFieldV2
	Input           *AlgebraPlanV2
	Left            *AlgebraPlanV2
	Right           *AlgebraPlanV2
	Fields          []string
	Predicates      []NormalizedFilter
	JoinPredicates  []AlgebraJoinPredicateV2
	GroupBy         []string
	Aggregates      []AlgebraAggregateV2
	OrderBy         []NormalizedOrder
	StableOrderKey  []string
	Limit           int
	Offset          int
	UnionAll        bool
}

type AlgebraFieldV2 struct {
	ID                     string `json:"id"`
	SQLType                string `json:"sql_type"`
	Collation              string `json:"collation,omitempty"`
	CollationVersion       string `json:"collation_version,omitempty"`
	CollationDeterministic bool   `json:"collation_deterministic,omitempty"`
}

type AlgebraJoinPredicateV2 struct {
	LeftField  string `json:"left_field"`
	RightField string `json:"right_field"`
}

type AlgebraAggregateV2 struct {
	Function   string `json:"function"`
	Field      string `json:"field"`
	OutputType string `json:"output_type"`
}

type AlgebraNormalFormV2 struct {
	Canonical json.RawMessage `json:"canonical"`
	SHA256    string          `json:"sha256"`
}

// NormalizeAlgebraV2 canonicalizes exactly Scan, Select, Project, Join,
// Union-distinct, Group and Page. Join/Union children are digest ordered,
// duplicate set-valued Union branches are idempotent, and UNION ALL is
// fail-closed.
func NormalizeAlgebraV2(plan AlgebraPlanV2) (AlgebraNormalFormV2, error) {
	node, err := normalizeAlgebraNodeV2(plan)
	if err != nil {
		return AlgebraNormalFormV2{}, err
	}
	digest := sha256.Sum256(append([]byte(normalFormDomain+"ALGEBRA\x00"), node.Canonical...))
	return AlgebraNormalFormV2{Canonical: node.Canonical, SHA256: hex.EncodeToString(digest[:])}, nil
}

type normalizedAlgebraNodeV2 struct {
	Canonical json.RawMessage
	Schema    []AlgebraFieldV2
	// TupleDistinct is a conservative proof that the node cannot contain two
	// equal full typed tuples. It is deliberately not inferred for Scan.
	TupleDistinct bool
}

func normalizeAlgebraNodeV2(plan AlgebraPlanV2) (normalizedAlgebraNodeV2, error) {
	op := strings.ToLower(strings.TrimSpace(plan.Op))
	switch op {
	case "scan":
		if invalidAlgebraTokenV2(plan.SourceNamespace) || invalidAlgebraTokenV2(plan.Snapshot) || invalidAlgebraTokenV2(plan.StableRole) {
			return normalizedAlgebraNodeV2{}, errors.New("V2 scan normal form requires namespace, snapshot, and stable role")
		}
		schema, err := normalizeAlgebraSchemaV2(plan.Schema)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		scan := map[string]any{"op": "scan", "namespace": plan.SourceNamespace, "snapshot": plan.Snapshot, "role": plan.StableRole, "schema": schema}
		if plan.LineageDigest != "" {
			if len(plan.LineageDigest) != sha256.Size*2 {
				return normalizedAlgebraNodeV2{}, errors.New("V2 scan lineage digest must be lowercase SHA-256")
			}
			if _, decodeErr := hex.DecodeString(plan.LineageDigest); decodeErr != nil || plan.LineageDigest != strings.ToLower(plan.LineageDigest) {
				return normalizedAlgebraNodeV2{}, errors.New("V2 scan lineage digest must be lowercase SHA-256")
			}
			scan["lineage_digest"] = plan.LineageDigest
		}
		canonical, err := json.Marshal(scan)
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: schema}, err
	case "select":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		if len(plan.Predicates) == 0 {
			return normalizedAlgebraNodeV2{}, errors.New("V2 selection requires at least one predicate")
		}
		predicates := make([]NormalizedFilter, 0, len(plan.Predicates))
		for _, predicate := range plan.Predicates {
			normalized, normalizeErr := normalizeAlgebraFilterV2(predicate, input.Schema)
			if normalizeErr != nil {
				return normalizedAlgebraNodeV2{}, normalizeErr
			}
			predicates = append(predicates, normalized)
		}
		sort.Slice(predicates, func(i, j int) bool {
			left, _ := json.Marshal(predicates[i])
			right, _ := json.Marshal(predicates[j])
			return string(left) < string(right)
		})
		canonical, err := json.Marshal(map[string]any{"op": "select", "input": input.Canonical, "predicates": predicates})
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: input.Schema, TupleDistinct: input.TupleDistinct}, err
	case "project":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		fields, err := normalizedAlgebraFieldSetV2(plan.Fields, "projection")
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		if len(fields) == 0 {
			return normalizedAlgebraNodeV2{}, errors.New("V2 projection requires at least one field")
		}
		fieldIndex := algebraFieldIndexV2(input.Schema)
		schema := make([]AlgebraFieldV2, 0, len(fields))
		for _, field := range fields {
			definition, present := fieldIndex[field]
			if !present {
				return normalizedAlgebraNodeV2{}, fmt.Errorf("V2 projection references unknown field %q", field)
			}
			schema = append(schema, definition)
		}
		canonical, err := json.Marshal(map[string]any{"op": "project", "input": input.Canonical, "fields": fields})
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: schema, TupleDistinct: input.TupleDistinct && sameAlgebraSchemaV2(input.Schema, schema)}, err
	case "join":
		if plan.Left == nil || plan.Right == nil {
			return normalizedAlgebraNodeV2{}, errors.New("V2 join normal form requires two inputs")
		}
		left, err := normalizeAlgebraNodeV2(*plan.Left)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		right, err := normalizeAlgebraNodeV2(*plan.Right)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		predicates, err := normalizeJoinPredicatesV2(plan.JoinPredicates, left.Schema, right.Schema)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		schema, err := joinAlgebraSchemasV2(left.Schema, right.Schema)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		if string(right.Canonical) < string(left.Canonical) {
			left, right = right, left
		}
		canonical, err := json.Marshal(map[string]any{"op": op, "left": left.Canonical, "right": right.Canonical, "predicates": predicates})
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: schema, TupleDistinct: left.TupleDistinct && right.TupleDistinct}, err
	case "union":
		if plan.Left == nil || plan.Right == nil {
			return normalizedAlgebraNodeV2{}, errors.New("V2 union normal form requires two inputs")
		}
		if plan.UnionAll {
			return normalizedAlgebraNodeV2{}, errors.New("UNION ALL is outside taskgate-exposure-v2")
		}
		left, err := normalizeAlgebraNodeV2(*plan.Left)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		right, err := normalizeAlgebraNodeV2(*plan.Right)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		if !sameAlgebraSchemaV2(left.Schema, right.Schema) {
			return normalizedAlgebraNodeV2{}, errors.New("V2 UNION DISTINCT inputs require the same typed schema")
		}
		if string(right.Canonical) < string(left.Canonical) {
			left, right = right, left
		}
		if string(left.Canonical) == string(right.Canonical) && left.TupleDistinct {
			return left, nil
		}
		canonical, err := json.Marshal(map[string]any{"op": op, "left": left.Canonical, "right": right.Canonical})
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: left.Schema, TupleDistinct: true}, err
	case "group":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		groupFields, err := normalizedAlgebraFieldSetV2(plan.GroupBy, "group")
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		if len(groupFields)+len(plan.Aggregates) == 0 {
			return normalizedAlgebraNodeV2{}, errors.New("V2 group requires a key or aggregate")
		}
		index := algebraFieldIndexV2(input.Schema)
		schema := make([]AlgebraFieldV2, 0, len(groupFields)+len(plan.Aggregates))
		for _, field := range groupFields {
			definition, present := index[field]
			if !present {
				return normalizedAlgebraNodeV2{}, fmt.Errorf("V2 group references unknown field %q", field)
			}
			schema = append(schema, definition)
		}
		aggregates, aggregateSchema, err := normalizeAlgebraAggregatesV2(plan.Aggregates, index)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		schema = append(schema, aggregateSchema...)
		sort.Slice(schema, func(i, j int) bool { return schema[i].ID < schema[j].ID })
		canonical, err := json.Marshal(map[string]any{"op": "group", "input": input.Canonical, "group_by": groupFields, "aggregates": aggregates})
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: schema, TupleDistinct: true}, err
	case "page":
		input, err := requiredAlgebraInputV2(plan.Input)
		if err != nil {
			return normalizedAlgebraNodeV2{}, err
		}
		if plan.Limit < 0 || plan.Offset < 0 {
			return normalizedAlgebraNodeV2{}, errors.New("V2 page bounds cannot be negative")
		}
		index := algebraFieldIndexV2(input.Schema)
		orders := make([]NormalizedOrder, 0, len(plan.OrderBy)+len(plan.StableOrderKey))
		seen := make(map[string]struct{}, len(orders))
		for _, order := range plan.OrderBy {
			expression := strings.ToLower(strings.TrimSpace(order.Expression))
			if _, present := index[expression]; !present {
				return normalizedAlgebraNodeV2{}, fmt.Errorf("V2 page orders by unknown field %q", expression)
			}
			direction := strings.ToUpper(strings.TrimSpace(order.Direction))
			if direction == "" {
				direction = "ASC"
			}
			if direction != "ASC" && direction != "DESC" {
				return normalizedAlgebraNodeV2{}, errors.New("V2 page has an invalid order direction")
			}
			if _, duplicate := seen[expression]; duplicate {
				return normalizedAlgebraNodeV2{}, fmt.Errorf("V2 page repeats order field %q", expression)
			}
			orders = append(orders, NormalizedOrder{Expression: expression, Direction: direction})
			seen[expression] = struct{}{}
		}
		for _, key := range plan.StableOrderKey {
			key = strings.ToLower(strings.TrimSpace(key))
			if _, present := index[key]; !present {
				return normalizedAlgebraNodeV2{}, fmt.Errorf("V2 page stable key %q is absent", key)
			}
			if _, present := seen[key]; !present {
				orders = append(orders, NormalizedOrder{Expression: key, Direction: "ASC"})
				seen[key] = struct{}{}
			}
		}
		if len(orders) == 0 {
			return normalizedAlgebraNodeV2{}, errors.New("V2 page requires a canonical total order")
		}
		canonical, err := json.Marshal(map[string]any{"op": "page", "input": input.Canonical, "order_by": orders, "limit": plan.Limit, "offset": plan.Offset})
		return normalizedAlgebraNodeV2{Canonical: canonical, Schema: input.Schema, TupleDistinct: input.TupleDistinct}, err
	default:
		return normalizedAlgebraNodeV2{}, fmt.Errorf("operator %q is outside taskgate-exposure-v2", plan.Op)
	}
}

func requiredAlgebraInputV2(input *AlgebraPlanV2) (normalizedAlgebraNodeV2, error) {
	if input == nil {
		return normalizedAlgebraNodeV2{}, errors.New("V2 unary normal form requires an input")
	}
	return normalizeAlgebraNodeV2(*input)
}

func normalizeAlgebraSchemaV2(input []AlgebraFieldV2) ([]AlgebraFieldV2, error) {
	if len(input) == 0 {
		return nil, errors.New("V2 scan requires a non-empty typed schema")
	}
	result := make([]AlgebraFieldV2, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, field := range input {
		field.ID = strings.ToLower(strings.TrimSpace(field.ID))
		if invalidAlgebraTokenV2(field.ID) {
			return nil, errors.New("V2 schema has an invalid field ID")
		}
		typeName, err := exposure.CanonicalSQLTypeV2(field.SQLType)
		if err != nil {
			return nil, err
		}
		field.SQLType = typeName
		field.Collation = strings.TrimSpace(field.Collation)
		field.CollationVersion = strings.TrimSpace(field.CollationVersion)
		if isCollatableSQLTypeV2(typeName) && (field.Collation == "" || field.CollationVersion == "" || !field.CollationDeterministic) {
			return nil, fmt.Errorf("V2 field %q requires an exact collation name and version", field.ID)
		}
		if !isCollatableSQLTypeV2(typeName) && (field.Collation != "" || field.CollationVersion != "" || field.CollationDeterministic) {
			return nil, fmt.Errorf("V2 field %q is not collatable", field.ID)
		}
		if _, duplicate := seen[field.ID]; duplicate {
			return nil, fmt.Errorf("V2 schema repeats field %q", field.ID)
		}
		seen[field.ID] = struct{}{}
		result = append(result, field)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func normalizeAlgebraFilterV2(filter NormalizedFilter, schema []AlgebraFieldV2) (NormalizedFilter, error) {
	filter.Column = strings.ToLower(strings.TrimSpace(filter.Column))
	definition, present := algebraFieldIndexV2(schema)[filter.Column]
	if !present {
		return NormalizedFilter{}, fmt.Errorf("V2 predicate references unknown field %q", filter.Column)
	}
	if filter.SQLType != "" {
		typeName, err := exposure.CanonicalSQLTypeV2(filter.SQLType)
		if err != nil || typeName != definition.SQLType {
			return NormalizedFilter{}, fmt.Errorf("V2 predicate type disagrees with field %q", filter.Column)
		}
	}
	filter.SQLType = definition.SQLType
	filter.Op = strings.ToUpper(strings.TrimSpace(filter.Op))
	if filter.Op == "!=" {
		filter.Op = "<>"
	}
	switch filter.Op {
	case "=", "<>", "<", "<=", ">", ">=", "LIKE", "IN", "NOT IN":
	default:
		return NormalizedFilter{}, fmt.Errorf("V2 predicate operator %q is outside the grammar", filter.Op)
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(filter.Value)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return NormalizedFilter{}, fmt.Errorf("V2 predicate literal: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return NormalizedFilter{}, errors.New("V2 predicate literal has trailing content")
	}
	if filter.Op == "LIKE" {
		if !isCollatableSQLTypeV2(definition.SQLType) {
			return NormalizedFilter{}, errors.New("V2 LIKE requires a collatable string field")
		}
		if _, ok := decoded.(string); !ok {
			return NormalizedFilter{}, errors.New("V2 LIKE requires a string literal")
		}
	}
	if filter.Op == "IN" || filter.Op == "NOT IN" {
		values, ok := decoded.([]any)
		if !ok || len(values) == 0 || len(values) > 100 {
			return NormalizedFilter{}, errors.New("V2 IN requires a non-empty array of at most 100 typed literals")
		}
		for _, value := range values {
			if _, err := exposure.CanonicalSQLValue(definition.SQLType, value); err != nil {
				return NormalizedFilter{}, fmt.Errorf("V2 predicate literal disagrees with field %q: %w", filter.Column, err)
			}
		}
	} else if _, err := exposure.CanonicalSQLValue(definition.SQLType, decoded); err != nil {
		return NormalizedFilter{}, fmt.Errorf("V2 predicate literal disagrees with field %q: %w", filter.Column, err)
	}
	canonical, err := canonicalJSON(decoded)
	if err != nil {
		return NormalizedFilter{}, err
	}
	filter.Value = canonical
	return filter, nil
}

func normalizeJoinPredicatesV2(input []AlgebraJoinPredicateV2, left, right []AlgebraFieldV2) ([]map[string]string, error) {
	if len(input) == 0 {
		return nil, errors.New("V2 equijoin requires at least one equality")
	}
	leftIndex, rightIndex := algebraFieldIndexV2(left), algebraFieldIndexV2(right)
	result := make([]map[string]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, predicate := range input {
		leftField := strings.ToLower(strings.TrimSpace(predicate.LeftField))
		rightField := strings.ToLower(strings.TrimSpace(predicate.RightField))
		leftDefinition, leftPresent := leftIndex[leftField]
		rightDefinition, rightPresent := rightIndex[rightField]
		if !leftPresent || !rightPresent || leftDefinition.SQLType != rightDefinition.SQLType ||
			leftDefinition.Collation != rightDefinition.Collation || leftDefinition.CollationVersion != rightDefinition.CollationVersion ||
			leftDefinition.CollationDeterministic != rightDefinition.CollationDeterministic {
			return nil, errors.New("V2 join equality must reference same-typed fields from opposite inputs")
		}
		fields := []string{leftField, rightField}
		sort.Strings(fields)
		key := fields[0] + "\x00" + fields[1] + "\x00" + leftDefinition.SQLType
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("V2 join repeats an equality")
		}
		seen[key] = struct{}{}
		result = append(result, map[string]string{"field_a": fields[0], "field_b": fields[1], "sql_type": leftDefinition.SQLType})
	}
	sort.Slice(result, func(i, j int) bool {
		leftValue, _ := json.Marshal(result[i])
		rightValue, _ := json.Marshal(result[j])
		return string(leftValue) < string(rightValue)
	})
	return result, nil
}

func normalizeAlgebraAggregatesV2(input []AlgebraAggregateV2, fields map[string]AlgebraFieldV2) ([]map[string]string, []AlgebraFieldV2, error) {
	result := make([]map[string]string, 0, len(input))
	schema := make([]AlgebraFieldV2, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, aggregate := range input {
		function := strings.ToLower(strings.TrimSpace(aggregate.Function))
		if function != "count" && function != "sum" && function != "min" && function != "max" {
			return nil, nil, fmt.Errorf("V2 aggregate %q is outside the grammar", aggregate.Function)
		}
		field := strings.ToLower(strings.TrimSpace(aggregate.Field))
		inputType := ""
		inputDefinition := AlgebraFieldV2{}
		if field == "*" {
			if function != "count" {
				return nil, nil, fmt.Errorf("V2 aggregate %q cannot consume *", function)
			}
		} else {
			definition, present := fields[field]
			if !present {
				return nil, nil, fmt.Errorf("V2 aggregate references unknown field %q", field)
			}
			inputType = definition.SQLType
			inputDefinition = definition
		}
		outputType, err := exposure.CanonicalSQLTypeV2(aggregate.OutputType)
		if err != nil {
			return nil, nil, err
		}
		expectedType := aggregateOutputType(function, inputType)
		if expectedType != outputType {
			return nil, nil, fmt.Errorf("V2 aggregate %s(%s) output type must be %q", function, field, expectedType)
		}
		expression := function + "(" + field + ")"
		if _, duplicate := seen[expression]; duplicate {
			return nil, nil, fmt.Errorf("V2 group repeats aggregate %q", expression)
		}
		seen[expression] = struct{}{}
		result = append(result, map[string]string{"function": function, "field": field, "output_type": outputType})
		outputField := AlgebraFieldV2{ID: expression, SQLType: outputType}
		if isCollatableSQLTypeV2(outputType) {
			outputField.Collation = inputDefinition.Collation
			outputField.CollationVersion = inputDefinition.CollationVersion
			outputField.CollationDeterministic = inputDefinition.CollationDeterministic
		}
		schema = append(schema, outputField)
	}
	sort.Slice(result, func(i, j int) bool {
		leftValue, _ := json.Marshal(result[i])
		rightValue, _ := json.Marshal(result[j])
		return string(leftValue) < string(rightValue)
	})
	return result, schema, nil
}

func algebraFieldIndexV2(schema []AlgebraFieldV2) map[string]AlgebraFieldV2 {
	result := make(map[string]AlgebraFieldV2, len(schema))
	for _, field := range schema {
		result[field.ID] = field
	}
	return result
}

func joinAlgebraSchemasV2(left, right []AlgebraFieldV2) ([]AlgebraFieldV2, error) {
	result := append(append([]AlgebraFieldV2(nil), left...), right...)
	seen := make(map[string]struct{}, len(result))
	for _, field := range result {
		if _, duplicate := seen[field.ID]; duplicate {
			return nil, errors.New("V2 join requires globally role-qualified field IDs")
		}
		seen[field.ID] = struct{}{}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func sameAlgebraSchemaV2(left, right []AlgebraFieldV2) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func invalidAlgebraTokenV2(value string) bool {
	return strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n\t")
}

func normalizedAlgebraFieldSetV2(values []string, operator string) ([]string, error) {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if invalidAlgebraTokenV2(value) {
			return nil, fmt.Errorf("V2 %s has an invalid field ID", operator)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("V2 %s repeats field %q", operator, value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}
