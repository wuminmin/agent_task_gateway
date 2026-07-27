package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

// RelationalCompilation is the paired SQL and trusted metadata consumed by
// the online positive-output dependency path. ProvenanceSQL returns positive
// source members, never a representative chosen after UNION DISTINCT.
type RelationalCompilation struct {
	VisibleSQL       string
	ProvenanceSQL    string
	VisibleFields    []string
	InternalFields   []string
	ProvenanceFields []string
	Products         []string
	Kind             string
	ExpandedEvidence bool
	Sources          []RelationalSource
	JoinPredicates   []JoinPredicate
	UnionColumns     []string
	OutputAliases    map[string]string
}

type RelationalSource struct {
	Product        string
	Role           string
	Filters        []Filter
	EvidenceFields []string
	EvidenceAlias  map[string]string
	Branch         int
}

// CompileRelational lowers the public closed grammar to one visible statement
// and a same-snapshot provenance companion. Product contains Catalog metadata
// plus the task-approved columns, so every identifier and type is checked
// before SQL exists.
func CompileRelational(plan QueryPlan, products map[string]Product) (RelationalCompilation, error) {
	if plan.From == nil || plan.Product != "" {
		return RelationalCompilation{}, errors.New("relational QueryPlan requires from and forbids legacy product")
	}
	if plan.Limit != 0 || plan.Offset != 0 || len(plan.OrderBy) != 0 {
		return RelationalCompilation{}, errors.New("pagination is outside the online Join/Union fragment")
	}
	if len(plan.Columns)+len(plan.Aggregates) == 0 {
		return RelationalCompilation{}, errors.New("empty select list")
	}
	if countFromMembers(*plan.From) != 1 {
		return RelationalCompilation{}, errors.New("from must contain exactly one operator")
	}

	var result RelationalCompilation
	result.OutputAliases = make(map[string]string)
	var fields map[string]relationalField
	var fromSQL, provenanceFrom string
	switch {
	case plan.From.Scan != nil:
		compiled, schema, err := compileExplicitScan(*plan.From.Scan, products)
		if err != nil {
			return result, err
		}
		compiled.source.EvidenceFields = evidenceFields(*plan.From.Scan, products[plan.From.Scan.Product], schema, plan, nil, nil)
		compiled.source.EvidenceAlias = evidenceAliases(plan.From.Scan.Role, compiled.source.EvidenceFields)
		result.Kind, result.Sources = "scan", []RelationalSource{compiled.source}
		fields, fromSQL, provenanceFrom = schema, compiled.sql, compiled.sql
	case plan.From.Join != nil:
		joinResult, schema, visibleFrom, provenanceSQL, err := compileJoin(*plan.From.Join, plan, products)
		if err != nil {
			return result, err
		}
		result.Kind, result.Sources, result.JoinPredicates = "join", joinResult.sources, append([]JoinPredicate(nil), plan.From.Join.On...)
		fields, fromSQL, provenanceFrom = schema, visibleFrom, provenanceSQL
	case plan.From.UnionDistinct != nil:
		unionResult, schema, visibleFrom, provenanceSQL, err := compileUnion(*plan.From.UnionDistinct, plan.Filters, products)
		if err != nil {
			return result, err
		}
		result.Kind, result.Sources, result.UnionColumns = "union_distinct", unionResult.sources, append([]string(nil), plan.From.UnionDistinct.Columns...)
		result.ExpandedEvidence = true
		fields, fromSQL, provenanceFrom = schema, visibleFrom, provenanceSQL
	}

	selectSQL, internal, visible, aliases, err := compileRelationalSelect(plan, fields, fromSQL)
	if err != nil {
		return RelationalCompilation{}, err
	}
	result.VisibleSQL = selectSQL
	result.OutputAliases = aliases
	result.InternalFields = internal
	result.VisibleFields = visible
	if result.Kind == "scan" {
		// Explicit scan has no set operator membership expansion. Its companion
		// is built below from all evidence fields just like the join companion.
		provenanceFrom = fromSQL
	}
	if result.Kind != "union_distinct" {
		provenanceSelect := make([]string, 0)
		seenAliases := make(map[string]struct{})
		for _, source := range result.Sources {
			for _, field := range source.EvidenceFields {
				alias := source.EvidenceAlias[field]
				if len(alias) > 63 {
					return RelationalCompilation{}, errors.New("provenance alias exceeds the PostgreSQL identifier limit")
				}
				if _, duplicate := seenAliases[alias]; duplicate {
					return RelationalCompilation{}, errors.New("provenance aliases collide")
				}
				seenAliases[alias] = struct{}{}
				provenanceSelect = append(provenanceSelect, qualified(source.Role, field)+" AS "+quoteIdentifier(alias))
				result.ProvenanceFields = append(result.ProvenanceFields, alias)
			}
		}
		result.ProvenanceSQL = "SELECT " + strings.Join(provenanceSelect, ", ") + " FROM " + provenanceFrom
		if len(plan.Filters) > 0 {
			where, whereErr := compileQualifiedFilters(plan.Filters, fields)
			if whereErr != nil {
				return RelationalCompilation{}, whereErr
			}
			result.ProvenanceSQL += " WHERE " + where
		}
	} else {
		result.ProvenanceSQL = provenanceFrom
		result.ProvenanceFields = append(result.ProvenanceFields, "tg_branch")
		for _, field := range result.Sources[0].EvidenceFields {
			alias := result.Sources[0].EvidenceAlias[field]
			if len(alias) > 63 {
				return RelationalCompilation{}, errors.New("provenance alias exceeds the PostgreSQL identifier limit")
			}
			result.ProvenanceFields = append(result.ProvenanceFields, alias)
		}
	}
	if len(plan.GroupBy) == 0 && len(plan.Aggregates) == 0 && (result.Kind == "scan" || result.Kind == "join") {
		orders := make([]string, 0)
		for _, source := range result.Sources {
			product := products[source.Product]
			for _, key := range product.StableEntityKey {
				orders = append(orders, qualified(source.Role, key)+" ASC")
			}
		}
		if len(orders) == 0 {
			return RelationalCompilation{}, errors.New("relational row delivery requires Catalog stable entity keys")
		}
		orderSQL := " ORDER BY " + strings.Join(orders, ", ")
		result.VisibleSQL += orderSQL
		result.ProvenanceSQL += orderSQL
	}
	result.Products = relationalProducts(result.Sources)
	return result, nil
}

type relationalField struct {
	ID                   string
	Role                 string
	Column               string
	SQLType              string
	Collation            string
	CollationVersion     string
	Product              string
	AggregatePermissions map[string]struct{}
}

type compiledScan struct {
	sql    string
	source RelationalSource
}

type compiledRelation struct{ sources []RelationalSource }

func compileExplicitScan(scan Scan, products map[string]Product) (compiledScan, map[string]relationalField, error) {
	product, present := products[scan.Product]
	if !present || product.Name != scan.Product {
		return compiledScan{}, nil, fmt.Errorf("scan product %q is not approved", scan.Product)
	}
	if !safeIdentifier(scan.Product) || !safeRole(scan.Role) {
		return compiledScan{}, nil, errors.New("scan product or role is invalid")
	}
	if product.SourceNamespace == "" || product.Snapshot == "" || len(product.StableEntityKey) == 0 {
		return compiledScan{}, nil, errors.New("scan product lacks V2 namespace, snapshot, or stable entity key")
	}
	if err := validateProductSQLProfileV2(product); err != nil {
		return compiledScan{}, nil, err
	}
	if scan.Role != product.StableRole {
		return compiledScan{}, nil, errors.New("scan role must equal the Catalog stable relation role")
	}
	schema := make(map[string]relationalField, len(product.Columns))
	for column := range product.Columns {
		id := scan.Role + "." + column
		schema[id] = fieldFromProduct(id, scan.Role, column, product)
	}
	if _, err := compileScanFilters(scan, product); err != nil {
		return compiledScan{}, nil, err
	}
	sql := quoteIdentifier(scan.Product) + " AS " + quoteIdentifier(scan.Role)
	if len(scan.Filters) > 0 {
		where, _ := compileLeafFilters(scan, product)
		// A subquery keeps a leaf predicate local when this scan later becomes a
		// grammar building block; explicit top-level scan uses the same form.
		selected := make(map[string]struct{}, len(product.Columns)+len(product.StableEntityKey))
		for column := range product.Columns {
			selected[column] = struct{}{}
		}
		for _, column := range product.StableEntityKey {
			selected[column] = struct{}{}
		}
		selects := make([]string, 0, len(selected))
		for _, column := range sortedColumns(selected) {
			selects = append(selects, quoteIdentifier(column))
		}
		sql = "(SELECT " + strings.Join(selects, ", ") + " FROM " + quoteIdentifier(scan.Product) + " WHERE " + where + ") AS " + quoteIdentifier(scan.Role)
	}
	return compiledScan{sql: sql, source: RelationalSource{Product: scan.Product, Role: scan.Role, Filters: cloneFilters(scan.Filters)}}, schema, nil
}

func compileJoin(join Join, plan QueryPlan, products map[string]Product) (compiledRelation, map[string]relationalField, string, string, error) {
	left, leftSchema, err := compileExplicitScan(join.Left, products)
	if err != nil {
		return compiledRelation{}, nil, "", "", err
	}
	right, rightSchema, err := compileExplicitScan(join.Right, products)
	if err != nil {
		return compiledRelation{}, nil, "", "", err
	}
	if join.Left.Role == join.Right.Role || len(join.On) == 0 {
		return compiledRelation{}, nil, "", "", errors.New("join requires distinct roles and at least one equality")
	}
	schema := make(map[string]relationalField, len(leftSchema)+len(rightSchema))
	for id, field := range leftSchema {
		schema[id] = field
	}
	for id, field := range rightSchema {
		schema[id] = field
	}
	seen := make(map[string]struct{}, len(join.On))
	predicates := make([]string, 0, len(join.On))
	for _, predicate := range join.On {
		leftField, leftOK := leftSchema[predicate.Left]
		rightField, rightOK := rightSchema[predicate.Right]
		if !leftOK || !rightOK {
			return compiledRelation{}, nil, "", "", fmt.Errorf("join predicate must reference left then right role")
		}
		key := predicate.Left + "\x00" + predicate.Right
		if _, duplicate := seen[key]; duplicate {
			return compiledRelation{}, nil, "", "", errors.New("duplicate join predicate")
		}
		seen[key] = struct{}{}
		if leftField.SQLType != rightField.SQLType || leftField.SQLType == "json" ||
			(leftField.Collation != rightField.Collation || leftField.CollationVersion != rightField.CollationVersion) {
			return compiledRelation{}, nil, "", "", errors.New("join keys require identical types and deterministic collation profiles")
		}
		predicates = append(predicates, fieldSQL(leftField)+" = "+fieldSQL(rightField))
	}
	if _, err := compileQualifiedFilters(plan.Filters, schema); err != nil {
		return compiledRelation{}, nil, "", "", err
	}
	left.source.EvidenceFields = evidenceFields(join.Left, products[join.Left.Product], schema, plan, join.On, nil)
	right.source.EvidenceFields = evidenceFields(join.Right, products[join.Right.Product], schema, plan, join.On, nil)
	left.source.EvidenceAlias = evidenceAliases(join.Left.Role, left.source.EvidenceFields)
	right.source.EvidenceAlias = evidenceAliases(join.Right.Role, right.source.EvidenceFields)
	from := left.sql + " INNER JOIN " + right.sql + " ON " + strings.Join(predicates, " AND ")
	return compiledRelation{sources: []RelationalSource{left.source, right.source}}, schema, from, from, nil
}

func compileUnion(union UnionDistinct, outerFilters []Filter, products map[string]Product) (compiledRelation, map[string]relationalField, string, string, error) {
	if !safeRole(union.Role) || union.Left.Role == union.Right.Role || union.Left.Product != union.Right.Product {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT requires one product, distinct branch roles, and a valid output role")
	}
	product, present := products[union.Left.Product]
	if !present {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT product is not approved")
	}
	if union.Role != product.StableRole {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT output role must equal the Catalog stable relation role")
	}
	if len(union.Columns) == 0 {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT requires its complete deduplication schema")
	}
	columns := uniqueOrdered(union.Columns)
	if len(columns) != len(union.Columns) {
		return compiledRelation{}, nil, "", "", errors.New("UNION DISTINCT repeats a deduplication column")
	}
	schema := make(map[string]relationalField, len(columns))
	for _, column := range columns {
		if err := allowedColumn(column, product); err != nil {
			return compiledRelation{}, nil, "", "", err
		}
		id := union.Role + "." + column
		schema[id] = fieldFromProduct(id, union.Role, column, product)
	}
	if _, err := compileQualifiedFilters(outerFilters, schema); err != nil {
		return compiledRelation{}, nil, "", "", err
	}
	for _, scan := range []Scan{union.Left, union.Right} {
		if _, err := compileScanFilters(scan, product); err != nil {
			return compiledRelation{}, nil, "", "", err
		}
	}
	evidenceSet := make(map[string]struct{})
	for _, column := range columns {
		evidenceSet[column] = struct{}{}
	}
	for _, column := range product.StableEntityKey {
		evidenceSet[column] = struct{}{}
	}
	for _, scan := range []Scan{union.Left, union.Right} {
		for _, filter := range scan.Filters {
			evidenceSet[filter.Column] = struct{}{}
		}
	}
	evidence := sortedColumns(evidenceSet)
	branchesVisible := make([]string, 0, 2)
	branchesProvenance := make([]string, 0, 2)
	sources := []RelationalSource{
		{Product: union.Left.Product, Role: union.Left.Role, Filters: cloneFilters(union.Left.Filters), EvidenceFields: evidence, EvidenceAlias: evidenceAliases(union.Role, evidence), Branch: 0},
		{Product: union.Right.Product, Role: union.Right.Role, Filters: cloneFilters(union.Right.Filters), EvidenceFields: evidence, EvidenceAlias: evidenceAliases(union.Role, evidence), Branch: 1},
	}
	for branchIndex, scan := range []Scan{union.Left, union.Right} {
		visibleSelect := make([]string, 0, len(columns))
		for _, column := range columns {
			visibleSelect = append(visibleSelect, qualified(scan.Role, column)+" AS "+quoteIdentifier(column))
		}
		whereParts := make([]string, 0, 2)
		if len(scan.Filters) > 0 {
			leaf, _ := compileLeafFilters(scan, product)
			whereParts = append(whereParts, leaf)
		}
		// Applying a literal predicate before DISTINCT is equivalent to applying
		// it after DISTINCT and keeps every delivered tuple member in provenance.
		if len(outerFilters) > 0 {
			mapped := make([]Filter, 0, len(outerFilters))
			for _, filter := range outerFilters {
				_, column, _ := splitFieldID(filter.Column)
				filter.Column = column
				mapped = append(mapped, filter)
			}
			outer, _ := compileLeafFilterList(mapped, scan.Role, product)
			whereParts = append(whereParts, outer)
		}
		where := ""
		if len(whereParts) > 0 {
			where = " WHERE " + strings.Join(whereParts, " AND ")
		}
		branchesVisible = append(branchesVisible, "SELECT "+strings.Join(visibleSelect, ", ")+" FROM "+quoteIdentifier(scan.Product)+" AS "+quoteIdentifier(scan.Role)+where)
		provSelect := []string{strconv.Itoa(branchIndex) + " AS " + quoteIdentifier("tg_branch")}
		for _, column := range evidence {
			provSelect = append(provSelect, qualified(scan.Role, column)+" AS "+quoteIdentifier(evidenceAliases(union.Role, evidence)[column]))
		}
		branchesProvenance = append(branchesProvenance, "SELECT "+strings.Join(provSelect, ", ")+" FROM "+quoteIdentifier(scan.Product)+" AS "+quoteIdentifier(scan.Role)+where)
	}
	from := "(" + strings.Join(branchesVisible, " UNION ") + ") AS " + quoteIdentifier(union.Role)
	provenance := strings.Join(branchesProvenance, " UNION ALL ")
	return compiledRelation{sources: sources}, schema, from, provenance, nil
}

func compileRelationalSelect(plan QueryPlan, fields map[string]relationalField, fromSQL string) (string, []string, []string, map[string]string, error) {
	selectNames := make(map[string]struct{})
	visible := make([]string, 0, len(plan.Columns)+len(plan.Aggregates))
	internal := make([]string, 0, len(fields)+len(plan.Aggregates))
	selects := make([]string, 0, len(fields)+len(plan.Aggregates))
	requested := make(map[string]struct{})
	aliases := make(map[string]string, len(fields)+len(plan.Aggregates))
	for _, id := range plan.Columns {
		field, present := fields[id]
		if !present {
			return "", nil, nil, nil, fmt.Errorf("column %q is not in the relation schema", id)
		}
		if _, duplicate := requested[id]; duplicate {
			return "", nil, nil, nil, fmt.Errorf("duplicate select name %q", id)
		}
		requested[id] = struct{}{}
		selectNames[id] = struct{}{}
		visible = append(visible, id)
		_ = field
	}
	grouped := len(plan.GroupBy) > 0 || len(plan.Aggregates) > 0
	selectedFields := append([]string(nil), plan.Columns...)
	if !grouped && plan.From != nil && plan.From.UnionDistinct != nil {
		// Hidden relation fields carry the complete UNION DISTINCT tuple;
		// visibleResult strips them after accounting.
		selectedFields = sortedRelationalFieldIDs(fields)
	}
	if grouped {
		for _, group := range plan.GroupBy {
			if _, present := fields[group]; !present {
				return "", nil, nil, nil, fmt.Errorf("group field %q is absent", group)
			}
			if _, present := requested[group]; !present {
				selectedFields = append(selectedFields, group)
			}
		}
	}
	selectedFields = uniqueOrdered(selectedFields)
	for _, id := range selectedFields {
		field := fields[id]
		alias := relationalOutputAlias(id)
		aliases[id] = alias
		selects = append(selects, fieldSQL(field)+" AS "+quoteIdentifier(alias))
		internal = append(internal, id)
	}
	for _, aggregate := range plan.Aggregates {
		fn := strings.ToLower(strings.TrimSpace(aggregate.Function))
		if !safeIdentifier(aggregate.Alias) || strings.HasPrefix(aggregate.Alias, "tg_") {
			return "", nil, nil, nil, errors.New("aggregate alias is invalid")
		}
		if _, duplicate := selectNames[aggregate.Alias]; duplicate {
			return "", nil, nil, nil, fmt.Errorf("duplicate select name %q", aggregate.Alias)
		}
		argument := "*"
		if aggregate.Column != "*" {
			field, present := fields[aggregate.Column]
			if !present {
				return "", nil, nil, nil, fmt.Errorf("aggregate field %q is absent", aggregate.Column)
			}
			if _, allowed := field.AggregatePermissions[fn]; !allowed {
				return "", nil, nil, nil, fmt.Errorf("aggregate %q is not approved", fn)
			}
			argument = fieldSQL(field)
		} else if fn != "count" {
			return "", nil, nil, nil, fmt.Errorf("aggregate %q does not accept *", fn)
		}
		if aggregate.Column == "*" {
			for _, field := range fields {
				if _, ok := field.AggregatePermissions[fn]; !ok {
					return "", nil, nil, nil, fmt.Errorf("aggregate %q is not approved by every input", fn)
				}
			}
		}
		selects = append(selects, fn+"("+argument+") AS "+quoteIdentifier(aggregate.Alias))
		aliases[aggregate.Alias] = aggregate.Alias
		internal = append(internal, aggregate.Alias)
		visible = append(visible, aggregate.Alias)
		selectNames[aggregate.Alias] = struct{}{}
	}
	if len(selects) == 0 {
		return "", nil, nil, nil, errors.New("empty select list")
	}
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(selects, ", "))
	b.WriteString(" FROM ")
	b.WriteString(fromSQL)
	if len(plan.Filters) > 0 {
		where, err := compileQualifiedFilters(plan.Filters, fields)
		if err != nil {
			return "", nil, nil, nil, err
		}
		b.WriteString(" WHERE ")
		b.WriteString(where)
	}
	if grouped {
		groups := uniqueOrdered(plan.GroupBy)
		if len(groups) != len(plan.GroupBy) {
			return "", nil, nil, nil, errors.New("duplicate group field")
		}
		selectedSet := make(map[string]struct{}, len(plan.GroupBy))
		for _, group := range groups {
			selectedSet[group] = struct{}{}
		}
		for _, column := range plan.Columns {
			if _, ok := selectedSet[column]; !ok {
				return "", nil, nil, nil, fmt.Errorf("selected column %q is not grouped", column)
			}
		}
		if len(groups) > 0 {
			parts := make([]string, 0, len(groups))
			for _, group := range groups {
				parts = append(parts, fieldSQL(fields[group]))
			}
			b.WriteString(" GROUP BY ")
			b.WriteString(strings.Join(parts, ", "))
		} else if len(plan.Columns) > 0 {
			return "", nil, nil, nil, errors.New("selected columns require group_by")
		}
	}
	return b.String(), internal, visible, aliases, nil
}

func relationalOutputAlias(id string) string {
	digest := sha256.Sum256([]byte("taskgate-relational-output\x00" + id))
	return "tg_o_" + hex.EncodeToString(digest[:8])
}

func countFromMembers(from From) int {
	n := 0
	if from.Scan != nil {
		n++
	}
	if from.Join != nil {
		n++
	}
	if from.UnionDistinct != nil {
		n++
	}
	return n
}
func safeRole(role string) bool { return safeIdentifier(role) && !strings.HasPrefix(role, "tg_") }
func qualified(role, column string) string {
	return quoteIdentifier(role) + "." + quoteIdentifier(column)
}
func fieldSQL(field relationalField) string { return qualified(field.Role, field.Column) }

func fieldFromProduct(id, role, column string, product Product) relationalField {
	typeName, _ := canonicalProductType(product.ColumnTypes[column])
	return relationalField{ID: id, Role: role, Column: column, SQLType: typeName, Collation: product.ColumnCollations[column],
		CollationVersion: product.CollationVersions[column], Product: product.Name, AggregatePermissions: product.AllowedAggregates}
}
func canonicalProductType(value string) (string, error) {
	return exposure.CanonicalSQLTypeV2(value)
}

func compileScanFilters(scan Scan, product Product) (string, error) {
	return compileLeafFilterList(scan.Filters, scan.Role, product)
}
func compileLeafFilters(scan Scan, product Product) (string, error) {
	return compileLeafFilterList(scan.Filters, scan.Role, product)
}
func compileLeafFilterList(filters []Filter, role string, product Product) (string, error) {
	parts := make([]string, 0, len(filters))
	for _, filter := range filters {
		if err := allowedColumn(filter.Column, product); err != nil {
			return "", err
		}
		expression, err := compileFilterExpression(qualified(role, filter.Column), filter)
		if err != nil {
			return "", err
		}
		parts = append(parts, expression)
	}
	return strings.Join(parts, " AND "), nil
}
func compileQualifiedFilters(filters []Filter, fields map[string]relationalField) (string, error) {
	parts := make([]string, 0, len(filters))
	for _, filter := range filters {
		field, present := fields[filter.Column]
		if !present {
			return "", fmt.Errorf("filter field %q is absent", filter.Column)
		}
		expression, err := compileFilterExpression(fieldSQL(field), filter)
		if err != nil {
			return "", err
		}
		parts = append(parts, expression)
	}
	return strings.Join(parts, " AND "), nil
}
func compileFilterExpression(column string, filter Filter) (string, error) {
	op := strings.ToUpper(strings.TrimSpace(filter.Op))
	switch op {
	case "=", "!=", "<>", "<", "<=", ">", ">=", "LIKE":
		if op == "LIKE" {
			if _, ok := filter.Value.(string); !ok {
				return "", errors.New("LIKE requires a string literal")
			}
		}
		literal, err := sqlLiteral(filter.Value)
		if err != nil {
			return "", err
		}
		return column + " " + op + " " + literal, nil
	case "IN", "NOT IN":
		values, ok := filter.Value.([]any)
		if !ok || len(values) == 0 || len(values) > 100 {
			return "", errors.New("IN requires a non-empty JSON array of at most 100 values")
		}
		literals := make([]string, 0, len(values))
		for _, value := range values {
			literal, err := sqlLiteral(value)
			if err != nil {
				return "", err
			}
			literals = append(literals, literal)
		}
		return column + " " + op + " (" + strings.Join(literals, ", ") + ")", nil
	default:
		return "", fmt.Errorf("filter operator %q is not allowed", filter.Op)
	}
}

func evidenceFields(scan Scan, product Product, schema map[string]relationalField, plan QueryPlan, joins []JoinPredicate, union []string) []string {
	set := make(map[string]struct{})
	for _, field := range product.StableEntityKey {
		set[field] = struct{}{}
	}
	for _, filter := range scan.Filters {
		set[filter.Column] = struct{}{}
	}
	for _, filter := range plan.Filters {
		if field, ok := schema[filter.Column]; ok && field.Role == scan.Role {
			set[field.Column] = struct{}{}
		}
	}
	dependencyIDs := append(append([]string(nil), plan.Columns...), plan.GroupBy...)
	for _, aggregate := range plan.Aggregates {
		if aggregate.Column != "*" {
			dependencyIDs = append(dependencyIDs, aggregate.Column)
		}
	}
	for _, id := range dependencyIDs {
		if field, ok := schema[id]; ok && field.Role == scan.Role {
			set[field.Column] = struct{}{}
		}
	}
	for _, predicate := range joins {
		for _, id := range []string{predicate.Left, predicate.Right} {
			if field, ok := schema[id]; ok && field.Role == scan.Role {
				set[field.Column] = struct{}{}
			}
		}
	}
	for _, field := range union {
		set[field] = struct{}{}
	}
	return sortedColumns(set)
}
func evidenceAliases(role string, fields []string) map[string]string {
	result := make(map[string]string, len(fields))
	for _, field := range fields {
		result[field] = "tg_" + role + "_" + field
	}
	return result
}
func sortedColumns(set map[string]struct{}) []string {
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
func sortedRelationalFieldIDs(fields map[string]relationalField) []string {
	result := make([]string, 0, len(fields))
	for id := range fields {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
func uniqueOrdered(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
func splitFieldID(id string) (string, string, bool) {
	parts := strings.Split(id, ".")
	if len(parts) != 2 || !safeRole(parts[0]) || !safeIdentifier(parts[1]) {
		return "", "", false
	}
	return parts[0], parts[1], true
}
func cloneFilters(filters []Filter) []Filter { return append([]Filter(nil), filters...) }
func relationalProducts(sources []RelationalSource) []string {
	set := make(map[string]struct{})
	for _, source := range sources {
		set[source.Product] = struct{}{}
	}
	return sortedColumns(set)
}

// RelationalProductNames returns the logical products named by the public
// closed grammar without accepting or normalizing any SQL identifiers.
func RelationalProductNames(plan QueryPlan) ([]string, error) {
	if plan.From == nil || countFromMembers(*plan.From) != 1 {
		return nil, errors.New("relational QueryPlan has no unique from operator")
	}
	var names []string
	switch {
	case plan.From.Scan != nil:
		names = []string{plan.From.Scan.Product}
	case plan.From.Join != nil:
		names = []string{plan.From.Join.Left.Product, plan.From.Join.Right.Product}
	case plan.From.UnionDistinct != nil:
		names = []string{plan.From.UnionDistinct.Left.Product, plan.From.UnionDistinct.Right.Product}
	}
	for _, name := range names {
		if !safeIdentifier(name) {
			return nil, errors.New("relational QueryPlan has an invalid product")
		}
	}
	return sortedColumns(func() map[string]struct{} {
		set := make(map[string]struct{}, len(names))
		for _, name := range names {
			set[name] = struct{}{}
		}
		return set
	}()), nil
}
