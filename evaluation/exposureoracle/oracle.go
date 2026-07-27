// Package exposureoracle is an independent reference model for the RQ1
// ground-truth matrix. It intentionally imports none of TaskGate's exposure,
// query-plan, gateway, or policy packages.
package exposureoracle

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

const (
	OracleID = "independent-rq1-relational-oracle-v1"
	profile  = "taskgate-exposure-v2"
)

//go:embed oracle.go
var oracleSource []byte

type SnapshotBinding struct {
	SourceNamespace string `json:"source_namespace"`
	Snapshot        string `json:"snapshot"`
}

// Fact mirrors the public semantic payload, not the production implementation.
// The reference model constructs these objects and their hashes independently.
type Fact struct {
	Profile              string            `json:"profile,omitempty"`
	Kind                 string            `json:"kind,omitempty"`
	Snapshot             string            `json:"snapshot,omitempty"`
	EntityKey            string            `json:"entity_key,omitempty"`
	Field                string            `json:"field,omitempty"`
	SourceNamespace      string            `json:"source_namespace,omitempty"`
	SQLType              string            `json:"sql_type,omitempty"`
	CanonicalValue       string            `json:"canonical_value,omitempty"`
	SnapshotBundle       []SnapshotBinding `json:"snapshot_bundle,omitempty"`
	OutputRowKey         string            `json:"output_row_key,omitempty"`
	NormalizedExpression string            `json:"normalized_expression,omitempty"`
	WitnessCommitment    string            `json:"witness_commitment,omitempty"`
}

type Observation struct {
	Release   map[string]Fact
	Influence map[string]Fact
}

type corpus struct {
	Relations []relation `json:"relations"`
}

type relation struct {
	Name            string  `json:"name"`
	SourceNamespace string  `json:"source_namespace"`
	Snapshot        string  `json:"snapshot"`
	Fields          []field `json:"fields"`
	Rows            []row   `json:"rows"`
}

type field struct {
	ID      string `json:"id"`
	SQLType string `json:"sql_type"`
}

type row struct {
	EntityKey string         `json:"entity_key"`
	Values    map[string]any `json:"values"`
}

func SourceSHA256() string { return hexDigest(oracleSource) }

// Hash independently computes the durable ledger index for a semantic Fact.
func Hash(fact Fact) string { return hashFact(fact) }

func DatasetShape(corpusJSON []byte) (relations, rows int, err error) {
	parsed, err := parseCorpus(corpusJSON)
	if err != nil {
		return 0, 0, err
	}
	for _, current := range parsed.Relations {
		rows += len(current.Rows)
	}
	return len(parsed.Relations), rows, nil
}

// Evaluate derives the complete release and positive-output dependency sets
// from the source fixture without calling production exposure code. Influence
// is retained only as the compatibility field name.
func Evaluate(corpusJSON []byte, operation string) (Observation, error) {
	parsed, err := parseCorpus(corpusJSON)
	if err != nil {
		return Observation{}, err
	}
	expenses, err := namedRelation(parsed, "expenses")
	if err != nil {
		return Observation{}, err
	}
	switch operation {
	case "projection_amount":
		return project(expenses, expenses.Rows, "expense.amount")
	case "projection_department":
		return project(expenses, expenses.Rows, "expense.department")
	case "projection_pair":
		return project(expenses, expenses.Rows, "expense.department", "expense.amount")
	case "selection_sales_amount":
		return selectAndProject(expenses, "sales", "expense.amount")
	case "selection_rnd_amount":
		return selectAndProject(expenses, "rnd", "expense.amount")
	case "selection_ops_amount":
		return selectAndProject(expenses, "ops", "expense.amount")
	case "selection_legal_amount":
		return selectAndProject(expenses, "legal", "expense.amount")
	case "selection_missing_amount":
		return selectAndProject(expenses, "missing", "expense.amount")
	case "selection_positive_boundary":
		return selectAndProject(expenses, "sales", "expense.amount")
	case "group_sum_count":
		return groupSumCount(expenses)
	case "group_hidden_key_sum":
		return groupHiddenKeySum(expenses)
	case "global_count":
		return globalAggregate(expenses, "count", "*", "bigint", strconv.Itoa(len(expenses.Rows)))
	case "global_count_column_null":
		return globalAggregate(expenses, "count", "expense.amount", "bigint", int64(11))
	case "global_sum":
		return globalAggregate(expenses, "sum", "expense.amount", "numeric", sumRows(expenses.Rows, "expense.amount"))
	case "global_min_all_inputs":
		return globalAggregate(expenses, "min", "expense.amount", "numeric", "0")
	case "global_max_all_inputs":
		return globalAggregate(expenses, "max", "expense.amount", "numeric", "40")
	case "department_join":
		departments, relationErr := namedRelation(parsed, "departments")
		if relationErr != nil {
			return Observation{}, relationErr
		}
		return joinDepartments(departments, expenses)
	case "page_first_four":
		return project(expenses, page(expenses.Rows, 0, 4), "expense.department", "expense.amount")
	case "page_middle_five":
		return project(expenses, page(expenses.Rows, 3, 5), "expense.department", "expense.amount")
	case "page_order_boundary":
		return pageOrderBoundary(expenses)
	case "union_hidden_distinct":
		return unionHiddenDistinct(expenses)
	default:
		return Observation{}, fmt.Errorf("independent oracle: unknown operation %q", operation)
	}
}

func parseCorpus(raw []byte) (corpus, error) {
	var result corpus
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return corpus{}, fmt.Errorf("independent oracle corpus: %w", err)
	}
	if len(result.Relations) == 0 {
		return corpus{}, fmt.Errorf("independent oracle corpus has no relations")
	}
	return result, nil
}

func namedRelation(parsed corpus, name string) (relation, error) {
	for _, current := range parsed.Relations {
		if current.Name == name {
			return current, nil
		}
	}
	return relation{}, fmt.Errorf("independent oracle: relation %q is missing", name)
}

func newObservation() Observation {
	return Observation{Release: make(map[string]Fact), Influence: make(map[string]Fact)}
}

func add(set map[string]Fact, fact Fact) { set[Key(fact)] = fact }

// Key is an implementation-independent equality key for a semantic Fact.
func Key(fact Fact) string {
	fact.SnapshotBundle = append([]SnapshotBinding(nil), fact.SnapshotBundle...)
	sort.Slice(fact.SnapshotBundle, func(i, j int) bool {
		if fact.SnapshotBundle[i].SourceNamespace != fact.SnapshotBundle[j].SourceNamespace {
			return fact.SnapshotBundle[i].SourceNamespace < fact.SnapshotBundle[j].SourceNamespace
		}
		return fact.SnapshotBundle[i].Snapshot < fact.SnapshotBundle[j].Snapshot
	})
	encoded, _ := json.Marshal(fact)
	return string(encoded)
}

func project(source relation, rows []row, fields ...string) (Observation, error) {
	result := newObservation()
	for _, current := range rows {
		add(result.Influence, baseRow(source, current))
		for _, fieldID := range fields {
			fact, err := baseCell(source, current, fieldID)
			if err != nil {
				return Observation{}, err
			}
			add(result.Release, fact)
			add(result.Influence, fact)
		}
	}
	return result, nil
}

func selectAndProject(source relation, target string, fields ...string) (Observation, error) {
	result := newObservation()
	for _, current := range source.Rows {
		department, ok := current.Values["expense.department"].(string)
		if !ok || department != target {
			continue
		}
		add(result.Influence, baseRow(source, current))
		predicate, err := baseCell(source, current, "expense.department")
		if err != nil {
			return Observation{}, err
		}
		add(result.Influence, predicate)
		for _, fieldID := range fields {
			fact, cellErr := baseCell(source, current, fieldID)
			if cellErr != nil {
				return Observation{}, cellErr
			}
			add(result.Release, fact)
			add(result.Influence, fact)
		}
	}
	return result, nil
}

func groupSumCount(source relation) (Observation, error) {
	groups := make(map[string][]row)
	groupValues := make(map[string]any)
	for _, current := range source.Rows {
		canonical, err := canonicalValue("text", current.Values["expense.department"])
		if err != nil {
			return Observation{}, err
		}
		groups[canonical] = append(groups[canonical], current)
		groupValues[canonical] = current.Values["expense.department"]
	}
	result := newObservation()
	bundle := []SnapshotBinding{{SourceNamespace: source.SourceNamespace, Snapshot: source.Snapshot}}
	for canonical, members := range groups {
		component := source.SourceNamespace + ".expense.department\x00text\x00" + canonical
		rowKey := composeKey("group-row", component)
		rowWitness := make([]Fact, 0, len(members))
		groupWitness := make([]Fact, 0, len(members))
		amountWitness := make([]Fact, 0, len(members))
		for _, member := range members {
			rowFact := baseRow(source, member)
			department, err := baseCell(source, member, "expense.department")
			if err != nil {
				return Observation{}, err
			}
			amount, err := baseCell(source, member, "expense.amount")
			if err != nil {
				return Observation{}, err
			}
			rowWitness = append(rowWitness, rowFact)
			groupWitness = append(groupWitness, department)
			amountWitness = append(amountWitness, amount)
			add(result.Influence, rowFact)
			add(result.Influence, department)
			add(result.Influence, amount)
		}
		groupFact, err := derived(bundle, rowKey, "group("+source.SourceNamespace+".expense.department)", "text", groupValues[canonical], groupWitness)
		if err != nil {
			return Observation{}, err
		}
		sumFact, err := derived(bundle, rowKey, "sum("+source.SourceNamespace+".expense.amount)", "numeric", sumRows(members, "expense.amount"), amountWitness)
		if err != nil {
			return Observation{}, err
		}
		countFact, err := derived(bundle, rowKey, "count(*)", "bigint", int64(len(members)), rowWitness)
		if err != nil {
			return Observation{}, err
		}
		add(result.Release, groupFact)
		add(result.Release, sumFact)
		add(result.Release, countFact)
	}
	return result, nil
}

func groupHiddenKeySum(source relation) (Observation, error) {
	groups := make(map[string][]row)
	for _, current := range source.Rows {
		canonical, err := canonicalValue("text", current.Values["expense.department"])
		if err != nil {
			return Observation{}, err
		}
		groups[canonical] = append(groups[canonical], current)
	}
	result := newObservation()
	bundle := []SnapshotBinding{{SourceNamespace: source.SourceNamespace, Snapshot: source.Snapshot}}
	for canonical, members := range groups {
		component := source.SourceNamespace + ".expense.department\x00text\x00" + canonical
		rowKey := composeKey("group-row", component)
		amountWitness := make([]Fact, 0, len(members))
		for _, member := range members {
			rowFact := baseRow(source, member)
			department, err := baseCell(source, member, "expense.department")
			if err != nil {
				return Observation{}, err
			}
			amount, err := baseCell(source, member, "expense.amount")
			if err != nil {
				return Observation{}, err
			}
			amountWitness = append(amountWitness, amount)
			add(result.Influence, rowFact)
			add(result.Influence, department)
			add(result.Influence, amount)
		}
		sumFact, err := derived(bundle, rowKey, "sum("+source.SourceNamespace+".expense.amount)", "numeric", sumRows(members, "expense.amount"), amountWitness)
		if err != nil {
			return Observation{}, err
		}
		add(result.Release, sumFact)
	}
	return result, nil
}

func globalAggregate(source relation, function, fieldID, outputType string, value any) (Observation, error) {
	result := newObservation()
	witness := make([]Fact, 0, len(source.Rows))
	for _, current := range source.Rows {
		rowFact := baseRow(source, current)
		add(result.Influence, rowFact)
		if fieldID == "*" {
			witness = append(witness, rowFact)
			continue
		}
		cell, err := baseCell(source, current, fieldID)
		if err != nil {
			return Observation{}, err
		}
		witness = append(witness, cell)
		add(result.Influence, cell)
	}
	rowKey := composeKey("group-row", "global")
	expression := function + "(*)"
	if fieldID != "*" {
		expression = function + "(" + source.SourceNamespace + "." + fieldID + ")"
	}
	fact, err := derived([]SnapshotBinding{{SourceNamespace: source.SourceNamespace, Snapshot: source.Snapshot}}, rowKey, expression, outputType, value, witness)
	if err != nil {
		return Observation{}, err
	}
	add(result.Release, fact)
	return result, nil
}

func joinDepartments(departments, expenses relation) (Observation, error) {
	result := newObservation()
	for _, expense := range expenses.Rows {
		value, ok := expense.Values["expense.department"].(string)
		if !ok {
			continue // PostgreSQL equality with NULL is UNKNOWN.
		}
		for _, department := range departments.Rows {
			if department.Values["department.department"] != value {
				continue
			}
			for _, fact := range []Fact{baseRow(expenses, expense), baseRow(departments, department)} {
				add(result.Influence, fact)
			}
			for _, pair := range []struct {
				relation relation
				row      row
				field    string
			}{
				{expenses, expense, "expense.department"},
				{expenses, expense, "expense.amount"},
				{departments, department, "department.department"},
				{departments, department, "department.manager"},
			} {
				fact, err := baseCell(pair.relation, pair.row, pair.field)
				if err != nil {
					return Observation{}, err
				}
				add(result.Influence, fact)
				if pair.field == "expense.amount" || pair.field == "department.manager" {
					add(result.Release, fact)
				}
			}
		}
	}
	return result, nil
}

func unionHiddenDistinct(source relation) (Observation, error) {
	result := newObservation()
	for _, current := range source.Rows {
		if current.EntityKey != "r10" && current.EntityKey != "r12" {
			continue
		}
		add(result.Influence, baseRow(source, current))
		for _, fieldID := range []string{"expense.department", "expense.amount"} {
			fact, err := baseCell(source, current, fieldID)
			if err != nil {
				return Observation{}, err
			}
			add(result.Influence, fact)
			if fieldID == "expense.amount" {
				add(result.Release, fact)
			}
		}
	}
	return result, nil
}

func pageOrderBoundary(source relation) (Observation, error) {
	rows := append([]row(nil), source.Rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		left, leftOK := rows[i].Values["expense.department"].(string)
		right, rightOK := rows[j].Values["expense.department"].(string)
		if leftOK != rightOK {
			return leftOK // NULLS LAST
		}
		if left != right {
			return left < right
		}
		return rows[i].EntityKey < rows[j].EntityKey
	})
	return project(source, page(rows, 0, 1), "expense.amount")
}

func page(rows []row, offset, limit int) []row {
	if offset >= len(rows) {
		return nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

func baseRow(source relation, current row) Fact {
	return Fact{Profile: profile, Kind: "base-row", SourceNamespace: source.SourceNamespace,
		Snapshot: source.Snapshot, EntityKey: current.EntityKey}
}

func baseCell(source relation, current row, fieldID string) (Fact, error) {
	typeName := ""
	for _, currentField := range source.Fields {
		if currentField.ID == fieldID {
			typeName = canonicalType(currentField.SQLType)
			break
		}
	}
	value, present := current.Values[fieldID]
	if typeName == "" || !present {
		return Fact{}, fmt.Errorf("independent oracle: missing field %q", fieldID)
	}
	canonical, err := canonicalValue(typeName, value)
	if err != nil {
		return Fact{}, err
	}
	return Fact{Profile: profile, Kind: "base-cell", SourceNamespace: source.SourceNamespace,
		Snapshot: source.Snapshot, EntityKey: current.EntityKey, Field: fieldID,
		SQLType: typeName, CanonicalValue: canonical}, nil
}

func derived(bundle []SnapshotBinding, rowKey, expression, sqlType string, value any, witness []Fact) (Fact, error) {
	canonical, err := canonicalValue(sqlType, value)
	if err != nil {
		return Fact{}, err
	}
	return Fact{Profile: profile, Kind: "derived", SnapshotBundle: append([]SnapshotBinding(nil), bundle...),
		OutputRowKey: rowKey, NormalizedExpression: expression, SQLType: canonicalType(sqlType),
		CanonicalValue: canonical, WitnessCommitment: witnessCommitment(witness)}, nil
}

func witnessCommitment(facts []Fact) string {
	counts := make(map[string]uint64)
	for _, fact := range facts {
		counts[hashFact(fact)]++
	}
	hashes := make([]string, 0, len(counts))
	for hash := range counts {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	values := make([]string, 0, len(hashes)*2)
	for _, hash := range hashes {
		values = append(values, hash, fmt.Sprintf("%020d", counts[hash]))
	}
	if len(values) == 0 {
		values = append(values, "empty")
	}
	return composeKey("witness-multiset", values...)
}

func hashFact(fact Fact) string {
	var payload bytes.Buffer
	writeString(&payload, fact.Kind)
	writeString(&payload, fact.Profile)
	switch fact.Kind {
	case "base-row":
		writeString(&payload, fact.SourceNamespace)
		writeString(&payload, fact.Snapshot)
		writeString(&payload, fact.EntityKey)
	case "base-cell":
		writeString(&payload, fact.SourceNamespace)
		writeString(&payload, fact.Snapshot)
		writeString(&payload, fact.EntityKey)
		writeString(&payload, fact.Field)
		writeString(&payload, fact.SQLType)
		writeString(&payload, fact.CanonicalValue)
	case "derived":
		bindings := append([]SnapshotBinding(nil), fact.SnapshotBundle...)
		sort.Slice(bindings, func(i, j int) bool { return bindings[i].SourceNamespace < bindings[j].SourceNamespace })
		writeUint64(&payload, uint64(len(bindings)))
		for _, binding := range bindings {
			writeString(&payload, binding.SourceNamespace)
			writeString(&payload, binding.Snapshot)
		}
		writeString(&payload, fact.OutputRowKey)
		writeString(&payload, fact.NormalizedExpression)
		writeString(&payload, fact.SQLType)
		writeString(&payload, fact.CanonicalValue)
		writeString(&payload, fact.WitnessCommitment)
	}
	encoded := append([]byte("TASKGATE-FACT-V2\x00"), payload.Bytes()...)
	return hexDigest(encoded)
}

func composeKey(domain string, values ...string) string {
	var payload bytes.Buffer
	writeString(&payload, domain)
	writeUint64(&payload, uint64(len(values)))
	for _, value := range values {
		writeString(&payload, value)
	}
	return hexDigest(payload.Bytes())
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

func canonicalType(value string) string {
	normalized := strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
	switch normalized {
	case "int8":
		return "bigint"
	case "decimal":
		return "numeric"
	default:
		return normalized
	}
}

func canonicalValue(sqlType string, value any) (string, error) {
	if value == nil {
		return "null", nil
	}
	switch canonicalType(sqlType) {
	case "text":
		text, ok := value.(string)
		if !ok {
			return "", fmt.Errorf("independent oracle: %T is not text", value)
		}
		return "s:" + text, nil
	case "bigint":
		switch typed := value.(type) {
		case json.Number:
			return "i:" + string(typed), nil
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
		case json.Number:
			text = string(typed)
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
	return "", fmt.Errorf("independent oracle: unsupported %s value %T", sqlType, value)
}

func sumRows(rows []row, fieldID string) string {
	total := new(big.Rat)
	for _, current := range rows {
		value := current.Values[fieldID]
		if value == nil {
			continue
		}
		text := fmt.Sprint(value)
		if number, ok := new(big.Rat).SetString(text); ok {
			total.Add(total, number)
		}
	}
	return total.RatString()
}

func hexDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
