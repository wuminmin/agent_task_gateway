// Package generatedalgebra runs a generated-plan differential campaign: random
// relations and random plans in the closed positive fragment (Scan, Select,
// Project, two-source equijoin, grouped and global aggregates, Page) are
// evaluated by the production V2 algebra (internal/exposure) and by an
// independent reference evaluator written from the paper's operator rules in
// the semantic-fact vocabulary of evaluation/exposureoracle. Result and
// Dependency sets must agree fact for fact, and every production FactID hash
// must equal the reference hash of its semantic fact.
package generatedalgebra

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"

	"taskbound.local/agent-data-gateway/evaluation/exposureoracle"
)

// Profile is the V2 identity profile carried by base and derived Facts.
const Profile = "taskgate-exposure-v2"

// Relation is the reference evaluator's view of one immutable publication.
type Relation struct {
	Name            string
	SourceNamespace string
	Snapshot        string
	StableRole      string
	Fields          []Field
	Rows            []Row
}

// Field is a typed column identified by its stable, role-qualified ID.
type Field struct {
	ID      string
	SQLType string // text | numeric | bigint
}

// Row is one base row: the Catalog entity key and its typed values (nil = NULL).
type Row struct {
	EntityKey string
	Values    map[string]any
}

// Predicate is one conjunct: Field OP Literal. OP in =, <>, <, <=, >, >=.
type Predicate struct {
	Field   string
	Op      string
	Literal any
}

// Aggregate is one aggregate specification.
type Aggregate struct {
	Function   string // count | sum | min | max
	Field      string // "*" for count(*)
	OutputID   string
	OutputType string
}

// Plan is a generated query in the closed fragment.
type Plan struct {
	Join       bool        // departments JOIN expenses ON department.department = expense.department
	Predicates []Predicate // conjunction over the (joined) relation
	Kind       string      // "project" | "group" | "global"
	Project    []string    // visible fields for project/page
	Page       *[2]int     // offset, limit (project only, single relation)
	GroupField string      // group key for kind=group
	GroupKeyVisible bool
	Aggregates []Aggregate
}

// Observation is the semantic Result and Dependency footprint of a plan.
type Observation struct {
	Release   map[string]exposureoracle.Fact
	Influence map[string]exposureoracle.Fact
}

func newObservation() Observation {
	return Observation{Release: map[string]exposureoracle.Fact{}, Influence: map[string]exposureoracle.Fact{}}
}

func add(set map[string]exposureoracle.Fact, fact exposureoracle.Fact) { set[exposureoracle.Key(fact)] = fact }

func canonicalType(value string) string {
	switch value {
	case "int8":
		return "bigint"
	case "decimal":
		return "numeric"
	}
	return value
}

// CanonicalValue mirrors the wire contract's typed canonical encoding.
func CanonicalValue(sqlType string, value any) (string, error) {
	if value == nil {
		return "null", nil
	}
	switch canonicalType(sqlType) {
	case "text":
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("%T is not text", value)
		}
		return "s:" + text, nil
	case "bigint":
		switch typed := value.(type) {
		case int64:
			return "i:" + strconv.FormatInt(typed, 10), nil
		case int:
			return "i:" + strconv.Itoa(typed), nil
		case string:
			return "i:" + typed, nil
		}
	case "numeric":
		text := ""
		switch typed := value.(type) {
		case string:
			text = typed
		case int:
			text = strconv.Itoa(typed)
		case int64:
			text = strconv.FormatInt(typed, 10)
		}
		if number, ok := new(big.Rat).SetString(text); ok {
			return "n:" + number.RatString(), nil
		}
	}
	return "", fmt.Errorf("unsupported %s value %T", sqlType, value)
}

func composeKey(domain string, values ...string) string {
	var payload bytes.Buffer
	writeString(&payload, domain)
	writeUint64(&payload, uint64(len(values)))
	for _, value := range values {
		writeString(&payload, value)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:])
}

func writeString(buffer *bytes.Buffer, value string) {
	writeUint64(buffer, uint64(len(value)))
	buffer.WriteString(value)
}

func writeUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	buffer.Write(encoded[:])
}

func witnessCommitment(facts []exposureoracle.Fact) string {
	counts := map[string]uint64{}
	for _, fact := range facts {
		counts[exposureoracle.Hash(fact)]++
	}
	hashes := make([]string, 0, len(counts))
	for hash := range counts {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	values := make([]string, 0, 2*len(hashes))
	for _, hash := range hashes {
		values = append(values, hash, fmt.Sprintf("%020d", counts[hash]))
	}
	if len(values) == 0 {
		values = append(values, "empty")
	}
	return composeKey("witness-multiset", values...)
}

func baseRow(source Relation, current Row) exposureoracle.Fact {
	return exposureoracle.Fact{Profile: Profile, Kind: "base-row", SourceNamespace: source.SourceNamespace,
		Snapshot: source.Snapshot, EntityKey: current.EntityKey}
}

func fieldType(source Relation, fieldID string) string {
	for _, field := range source.Fields {
		if field.ID == fieldID {
			return canonicalType(field.SQLType)
		}
	}
	return ""
}

func baseCell(source Relation, current Row, fieldID string) (exposureoracle.Fact, error) {
	typeName := fieldType(source, fieldID)
	value, present := current.Values[fieldID]
	if typeName == "" || !present {
		return exposureoracle.Fact{}, fmt.Errorf("missing field %q", fieldID)
	}
	canonical, err := CanonicalValue(typeName, value)
	if err != nil {
		return exposureoracle.Fact{}, err
	}
	return exposureoracle.Fact{Profile: Profile, Kind: "base-cell", SourceNamespace: source.SourceNamespace,
		Snapshot: source.Snapshot, EntityKey: current.EntityKey, Field: fieldID, SQLType: typeName, CanonicalValue: canonical}, nil
}

func derived(bundle []exposureoracle.SnapshotBinding, rowKey, expression, sqlType string, value any, witness []exposureoracle.Fact) (exposureoracle.Fact, error) {
	canonical, err := CanonicalValue(sqlType, value)
	if err != nil {
		return exposureoracle.Fact{}, err
	}
	return exposureoracle.Fact{Profile: Profile, Kind: "derived", SnapshotBundle: append([]exposureoracle.SnapshotBinding(nil), bundle...),
		OutputRowKey: rowKey, NormalizedExpression: expression, SQLType: canonicalType(sqlType), CanonicalValue: canonical,
		WitnessCommitment: witnessCommitment(witness)}, nil
}

// joinedRow is one row of the (possibly joined) input: the contributing base
// rows per relation and the merged value map.
type joinedRow struct {
	parts  []struct{ rel Relation; row Row }
	values map[string]any
}

func (j joinedRow) cell(fieldID string) (exposureoracle.Fact, error) {
	for _, part := range j.parts {
		if fieldType(part.rel, fieldID) != "" {
			return baseCell(part.rel, part.row, fieldID)
		}
	}
	return exposureoracle.Fact{}, fmt.Errorf("field %q not in joined row", fieldID)
}

func (j joinedRow) rowFacts() []exposureoracle.Fact {
	facts := make([]exposureoracle.Fact, 0, len(j.parts))
	for _, part := range j.parts {
		facts = append(facts, baseRow(part.rel, part.row))
	}
	return facts
}

// Truth is SQL three-valued logic.
type Truth int

const (
	Unknown Truth = iota
	False
	True
)

func compare(sqlType string, left, right any, op string) Truth {
	if left == nil || right == nil {
		return Unknown
	}
	var cmp int
	switch canonicalType(sqlType) {
	case "text":
		l, r := left.(string), right.(string)
		switch {
		case l < r:
			cmp = -1
		case l > r:
			cmp = 1
		}
	default:
		l, r := new(big.Rat), new(big.Rat)
		l.SetString(fmt.Sprint(left))
		r.SetString(fmt.Sprint(right))
		cmp = l.Cmp(r)
	}
	var result bool
	switch op {
	case "=":
		result = cmp == 0
	case "<>":
		result = cmp != 0
	case "<":
		result = cmp < 0
	case "<=":
		result = cmp <= 0
	case ">":
		result = cmp > 0
	case ">=":
		result = cmp >= 0
	}
	if result {
		return True
	}
	return False
}

// Evaluate computes the reference Result/Dependency footprint of plan over the
// expenses relation and, when plan.Join is set, the departments relation.
func Evaluate(expenses, departments Relation, plan Plan) (Observation, error) {
	// 1. source rows (joined pairs on equal non-NULL department values)
	var rows []joinedRow
	if plan.Join {
		for _, expense := range expenses.Rows {
			value, ok := expense.Values["expense.department"].(string)
			if !ok {
				continue
			}
			for _, department := range departments.Rows {
				if department.Values["department.department"] != value {
					continue
				}
				values := map[string]any{}
				for k, v := range expense.Values {
					values[k] = v
				}
				for k, v := range department.Values {
					values[k] = v
				}
				rows = append(rows, joinedRow{parts: []struct{ rel Relation; row Row }{{departments, department}, {expenses, expense}}, values: values})
			}
		}
	} else {
		for _, expense := range expenses.Rows {
			rows = append(rows, joinedRow{parts: []struct{ rel Relation; row Row }{{expenses, expense}}, values: expense.Values})
		}
	}
	result := newObservation()
	// helper: the dependency facts every surviving row contributes (rows,
	// predicate cells, join-key cells)
	contribute := func(row joinedRow) error {
		for _, fact := range row.rowFacts() {
			add(result.Influence, fact)
		}
		if plan.Join {
			for _, key := range []string{"department.department", "expense.department"} {
				fact, err := row.cell(key)
				if err != nil {
					return err
				}
				add(result.Influence, fact)
			}
		}
		for _, predicate := range plan.Predicates {
			fact, err := row.cell(predicate.Field)
			if err != nil {
				return err
			}
			add(result.Influence, fact)
		}
		return nil
	}
	// 2. selection with three-valued logic: a row survives only if every
	// conjunct is TRUE
	var surviving []joinedRow
	for _, row := range rows {
		keep := true
		for _, predicate := range plan.Predicates {
			typ := ""
			for _, part := range row.parts {
				if t := fieldType(part.rel, predicate.Field); t != "" {
					typ = t
				}
			}
			if compare(typ, row.values[predicate.Field], predicate.Literal, predicate.Op) != True {
				keep = false
				break
			}
		}
		if keep {
			surviving = append(surviving, row)
		}
	}
	bundle := []exposureoracle.SnapshotBinding{{SourceNamespace: expenses.SourceNamespace, Snapshot: expenses.Snapshot}}
	if plan.Join {
		bundle = append(bundle, exposureoracle.SnapshotBinding{SourceNamespace: departments.SourceNamespace, Snapshot: departments.Snapshot})
	}
	switch plan.Kind {
	case "project":
		selected := surviving
		if plan.Page != nil {
			// canonical order is the entity-key order of the single relation
			sort.SliceStable(selected, func(i, j int) bool { return selected[i].parts[0].row.EntityKey < selected[j].parts[0].row.EntityKey })
			offset, limit := plan.Page[0], plan.Page[1]
			if offset >= len(selected) {
				selected = nil
			} else {
				end := len(selected)
				if limit > 0 && offset+limit < end {
					end = offset + limit
				}
				selected = selected[offset:end]
			}
		}
		for _, row := range selected {
			if err := contribute(row); err != nil {
				return Observation{}, err
			}
			for _, fieldID := range plan.Project {
				fact, err := row.cell(fieldID)
				if err != nil {
					return Observation{}, err
				}
				add(result.Release, fact)
				add(result.Influence, fact)
			}
		}
	case "global", "group":
		groups := map[string][]joinedRow{}
		groupValues := map[string]any{}
		order := []string{}
		for _, row := range surviving {
			key := "global"
			if plan.Kind == "group" {
				canonical, err := CanonicalValue(groupType(expenses, departments, plan.GroupField), row.values[plan.GroupField])
				if err != nil {
					return Observation{}, err
				}
				key = canonical
			}
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], row)
			groupValues[key] = row.values[plan.GroupField]
		}
		if plan.Kind == "global" && len(groups) == 0 {
			// an empty global aggregate still has one output row over no inputs
			groups["global"] = nil
			order = append(order, "global")
		}
		for _, key := range order {
			members := groups[key]
			var rowKey string
			if plan.Kind == "global" {
				rowKey = composeKey("group-row", "global")
			} else {
				ns := relationOf(expenses, departments, plan.GroupField).SourceNamespace
				rowKey = composeKey("group-row", ns+"."+plan.GroupField+"\x00"+groupType(expenses, departments, plan.GroupField)+"\x00"+key)
			}
			rowWitness := []exposureoracle.Fact{}
			keyWitness := []exposureoracle.Fact{}
			argWitness := map[string][]exposureoracle.Fact{}
			argFields := []string{}
			seenArg := map[string]bool{}
			for _, aggregate := range plan.Aggregates {
				if aggregate.Field != "*" && !seenArg[aggregate.Field] {
					seenArg[aggregate.Field] = true
					argFields = append(argFields, aggregate.Field)
				}
			}
			for _, member := range members {
				if err := contribute(member); err != nil {
					return Observation{}, err
				}
				// The row witness multiset of a surviving row is its base row
				// facts plus the join-key cells and each distinct predicate
				// field's cell (Select and Join merge them into the row).
				witness, err := memberWitness(member, plan)
				if err != nil {
					return Observation{}, err
				}
				rowWitness = append(rowWitness, witness...)
				if plan.Kind == "group" {
					fact, err := member.cell(plan.GroupField)
					if err != nil {
						return Observation{}, err
					}
					add(result.Influence, fact)
					keyWitness = append(keyWitness, fact)
				}
				for _, field := range argFields {
					fact, err := member.cell(field)
					if err != nil {
						return Observation{}, err
					}
					add(result.Influence, fact)
					argWitness[field] = append(argWitness[field], fact)
				}
			}
			if plan.Kind == "group" && plan.GroupKeyVisible {
				ns := relationOf(expenses, departments, plan.GroupField).SourceNamespace
				fact, err := derived(bundle, rowKey, "group("+ns+"."+plan.GroupField+")", groupType(expenses, departments, plan.GroupField), groupValues[key], keyWitness)
				if err != nil {
					return Observation{}, err
				}
				add(result.Release, fact)
			}
			for _, aggregate := range plan.Aggregates {
				var expression string
				var value any
				var witness []exposureoracle.Fact
				if aggregate.Field == "*" {
					expression = aggregate.Function + "(*)"
					value = int64(len(members))
					witness = rowWitness
				} else {
					ns := relationOf(expenses, departments, aggregate.Field).SourceNamespace
					expression = aggregate.Function + "(" + ns + "." + aggregate.Field + ")"
					value = AggregateValue(aggregate.Function, aggregate.Field, members)
					witness = argWitness[aggregate.Field]
				}
				fact, err := derived(bundle, rowKey, expression, aggregate.OutputType, value, witness)
				if err != nil {
					return Observation{}, err
				}
				add(result.Release, fact)
			}
		}
	default:
		return Observation{}, fmt.Errorf("unknown plan kind %q", plan.Kind)
	}
	return result, nil
}

func relationOf(expenses, departments Relation, fieldID string) Relation {
	if fieldType(departments, fieldID) != "" {
		return departments
	}
	return expenses
}

func groupType(expenses, departments Relation, fieldID string) string {
	if t := fieldType(departments, fieldID); t != "" {
		return t
	}
	return fieldType(expenses, fieldID)
}

// AggregateValue computes a PostgreSQL-compatible aggregate over the members'
// argument values (NULLs ignored; sum/min/max NULL when no input): the value
// is rendered as the decimal string the production side receives.
func AggregateValue(function, fieldID string, members []joinedRow) any {
	var values []*big.Rat
	for _, member := range members {
		value := member.values[fieldID]
		if value == nil {
			continue
		}
		number := new(big.Rat)
		number.SetString(fmt.Sprint(value))
		values = append(values, number)
	}
	switch function {
	case "count":
		return int64(len(values))
	case "sum":
		if len(values) == 0 {
			return nil
		}
		total := new(big.Rat)
		for _, value := range values {
			total.Add(total, value)
		}
		return total.RatString()
	case "min", "max":
		if len(values) == 0 {
			return nil
		}
		best := values[0]
		for _, value := range values[1:] {
			if (function == "min" && value.Cmp(best) < 0) || (function == "max" && value.Cmp(best) > 0) {
				best = value
			}
		}
		return best.RatString()
	}
	return nil
}

// memberWitness is the row witness multiset production carries for one
// surviving (possibly joined) row: base row facts, both join-key cells, and
// one cell per distinct predicate field.
func memberWitness(member joinedRow, plan Plan) ([]exposureoracle.Fact, error) {
	witness := append([]exposureoracle.Fact(nil), member.rowFacts()...)
	if plan.Join {
		for _, key := range []string{"department.department", "expense.department"} {
			fact, err := member.cell(key)
			if err != nil {
				return nil, err
			}
			witness = append(witness, fact)
		}
	}
	seen := map[string]bool{}
	for _, predicate := range plan.Predicates {
		if seen[predicate.Field] {
			continue
		}
		seen[predicate.Field] = true
		fact, err := member.cell(predicate.Field)
		if err != nil {
			return nil, err
		}
		witness = append(witness, fact)
	}
	return witness, nil
}
