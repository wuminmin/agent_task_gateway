// Package finalv5rls owns the source-controlled PostgreSQL-RLS fixture,
// deterministic 100-query adaptive corpus, and independent semantic oracle.
// It deliberately imports no production Gateway, Control, exposure, Catalog,
// SQL lowering, or query-plan package.
package finalv5rls

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
)

const (
	CorpusID      = "taskgate-final-v5-rls-corpus-v1"
	DatasetID     = "travel-demo-2026-v1"
	SchemaVersion = 1
	TraceLength   = 100
	PolicyRole    = "final_v5_rls_reader"
	PolicySchema  = "final_v5_rls"
	PolicyTable   = "expense_detail"
	PolicyName    = "final_v5_sales_scope"

	// PolicyInvisibleReceipt is physically present in the frozen ten-row
	// fixture but belongs to a department outside PolicyDepartment.  It is the
	// source-controlled FORCE RLS negative control and is deliberately excluded
	// from the 100-query legal trace.
	PolicyInvisibleReceipt = "TR-2026-0005"

	UnlimitedProduct = "final_v5_rls_unlimited_expense_detail"
	BoundedProduct   = "final_v5_rls_bounded_expense_detail"
	UnlimitedProfile = "final-v5-rls-unlimited-v1"
	BoundedProfile   = "final-v5-rls-bounded-70-v1"

	// The bounded Catalog profile is the exact floor(70% * full-union)
	// contract. Tests recompute these values from the independent trace before
	// any bounded execution is allowed.
	BoundedMaxReleaseFacts    int64 = 7
	BoundedMaxDependencyFacts int64 = 12
	BoundedMaxOutcomeFacts    int64 = 18

	// Updated only when the exact embedded manifest or expanded trace changes.
	CorpusSHA256 = "39e86d75f10be47847e7db155f28c564c43f4d50d765a99605028b3fc6535b3b"
	TraceSHA256  = "b3694e18f701026ca3aa12148824e3077e1d916f4b711516a000d43e6251a4a5"
)

//go:embed corpus-v1.json
var corpusBytes []byte

type Manifest struct {
	SchemaVersion    int          `json:"schema_version"`
	CorpusID         string       `json:"corpus_id"`
	DatasetID        string       `json:"dataset_id"`
	Seed             int64        `json:"seed"`
	PolicyRole       string       `json:"policy_role"`
	PolicyDepartment string       `json:"policy_department"`
	Rows             []FixtureRow `json:"rows"`
	Schedule         Schedule     `json:"schedule"`
}

type FixtureRow struct {
	ReceiptNo    string `json:"receipt_no"`
	EmployeeName string `json:"employee_name"`
	Department   string `json:"department"`
	Amount       int64  `json:"amount"`
}

type Schedule struct {
	EqualityReceipts      []string `json:"equality_receipts"`
	PaginationOffsets     []int64  `json:"pagination_offsets"`
	PaginationCycles      int      `json:"pagination_cycles"`
	EquivalentThresholds  []int64  `json:"equivalent_thresholds"`
	EquivalentCycles      int      `json:"equivalent_cycles"`
	AggregateThresholds   []int64  `json:"aggregate_thresholds"`
	AggregateCycles       int      `json:"aggregate_cycles"`
	AdaptiveSteps         int      `json:"adaptive_steps"`
	AdaptiveOddThreshold  int64    `json:"adaptive_odd_threshold"`
	AdaptiveEvenThreshold int64    `json:"adaptive_even_threshold"`
}

type Decision struct {
	PreviousStep  int    `json:"previous_step,omitempty"`
	PreviousValue int64  `json:"previous_value,omitempty"`
	Rule          string `json:"rule,omitempty"`
	Threshold     int64  `json:"threshold,omitempty"`
}

// BoundedStop is derived from the complete frozen trace, never from a live
// rejection. Before is the last legal prefix and Candidate is the first prefix
// that exceeds exactly one full-trace 70% budget dimension.
type BoundedStop struct {
	Index             int
	SuccessfulQueries int
	Dimension         string
	ErrorReason       string
	Before            finalv5oracle.TraceUnion
	Candidate         finalv5oracle.TraceUnion
	Full              finalv5oracle.TraceUnion
}

// Step contains every concrete cross-system statement and exact expected
// result. Oracle members are independently derived from the frozen fixture.
type Step struct {
	Index          int                       `json:"index"`
	ID             string                    `json:"id"`
	Family         string                    `json:"family"`
	Variant        string                    `json:"variant"`
	DirectSQL      string                    `json:"direct_sql"`
	ExpectedRows   [][]string                `json:"expected_rows"`
	ExpectedSHA256 string                    `json:"expected_sha256"`
	Scalar         *int64                    `json:"scalar,omitempty"`
	Decision       Decision                  `json:"decision,omitempty"`
	Oracle         finalv5oracle.Observation `json:"oracle"`
}

func (step Step) LogicalSQL(product string) string {
	return strings.Replace(step.DirectSQL, "final_v5_rls.expense_detail", product, 1)
}

// AuthorizationControl is separate from the FORCE RLS probe.  It proves the
// protocol's explicit rejection clause without pretending that a column grant
// error is row-level security.
type AuthorizationControl struct {
	ID        string
	Family    string
	Variant   string
	DirectSQL string
}

func (control AuthorizationControl) LogicalSQL(product string) string {
	return strings.Replace(control.DirectSQL, "final_v5_rls.expense_detail", product, 1)
}

func PolicyAuthorizationControl() AuthorizationControl {
	return AuthorizationControl{
		ID: "authorization-denied-employee-name", Family: "authorization_denied_control",
		Variant: "unapproved-column", DirectSQL: "SELECT employee_name FROM final_v5_rls.expense_detail",
	}
}

func Load() (Manifest, error) {
	var manifest Manifest
	digest := sha256.Sum256(corpusBytes)
	if hex.EncodeToString(digest[:]) != CorpusSHA256 {
		return manifest, errors.New("RLS corpus bytes differ from their compiled SHA-256")
	}
	decoder := json.NewDecoder(strings.NewReader(string(corpusBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return manifest, errors.New("RLS corpus contains trailing JSON")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	steps, err := manifest.Trace()
	if err != nil {
		return Manifest{}, err
	}
	encoded, _ := json.Marshal(steps)
	if SHA256(encoded) != TraceSHA256 {
		return Manifest{}, errors.New("expanded RLS trace differs from its compiled SHA-256")
	}
	return manifest, nil
}

func Bytes() []byte { return append([]byte(nil), corpusBytes...) }

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID || manifest.DatasetID != DatasetID ||
		manifest.Seed != 20260801 || manifest.PolicyRole != PolicyRole || manifest.PolicyDepartment != "销售部" || len(manifest.Rows) != 10 {
		return errors.New("RLS corpus identity, seed, policy, or dataset size is invalid")
	}
	seen := map[string]bool{}
	sales := 0
	for index, row := range manifest.Rows {
		if row.ReceiptNo == "" || row.EmployeeName == "" || row.Department == "" || row.Amount < 0 || seen[row.ReceiptNo] {
			return fmt.Errorf("invalid RLS fixture row %d", index+1)
		}
		seen[row.ReceiptNo] = true
		if row.Department == manifest.PolicyDepartment {
			sales++
		}
	}
	s := manifest.Schedule
	if sales != 6 || len(s.EqualityReceipts) != 6 || len(s.PaginationOffsets) != 3 || s.PaginationCycles != 10 ||
		len(s.EquivalentThresholds) != 3 || s.EquivalentCycles != 5 || len(s.AggregateThresholds) != 3 ||
		s.AggregateCycles != 10 || s.AdaptiveSteps != 4 || s.AdaptiveOddThreshold != 880 || s.AdaptiveEvenThreshold != 553 {
		return errors.New("RLS deterministic schedule differs from preregistration")
	}
	for _, receipt := range s.EqualityReceipts {
		if !seen[receipt] || manifest.row(receipt).Department != manifest.PolicyDepartment {
			return errors.New("RLS equality schedule escapes the policy-visible fixture")
		}
	}
	return nil
}

func (manifest Manifest) Trace() ([]Step, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	steps := make([]Step, 0, TraceLength)
	appendStep := func(step Step) {
		step.Index = len(steps) + 1
		step.ExpectedSHA256 = ResultSHA256(step.ExpectedRows)
		steps = append(steps, step)
	}
	for _, receipt := range manifest.Schedule.EqualityReceipts {
		row := manifest.row(receipt)
		appendStep(manifest.rowStep("equality", "canonical", "receipt-"+receipt,
			fmt.Sprintf("SELECT receipt_no FROM final_v5_rls.expense_detail WHERE receipt_no = '%s'", receipt), []FixtureRow{row},
			[]string{outcomeAtom("receipt_no", "=", receipt)}, "row-equality|"+receipt))
	}
	for cycle := 1; cycle <= manifest.Schedule.PaginationCycles; cycle++ {
		for _, offset := range manifest.Schedule.PaginationOffsets {
			rows := manifest.salesRows()
			rows = rows[int(offset):min(int(offset)+2, len(rows))]
			appendStep(manifest.rowStep("pagination", "limit-2-offset", fmt.Sprintf("page-%02d-%d", cycle, offset),
				fmt.Sprintf("SELECT receipt_no FROM final_v5_rls.expense_detail ORDER BY receipt_no ASC LIMIT 2 OFFSET %d", offset), rows,
				nil, fmt.Sprintf("page|2|%d", offset)))
		}
	}
	for cycle := 1; cycle <= manifest.Schedule.EquivalentCycles; cycle++ {
		for _, threshold := range manifest.Schedule.EquivalentThresholds {
			rows := manifest.amountRows(threshold)
			atom := outcomeAtom("amount", ">=", strconv.FormatInt(threshold, 10))
			appendStep(manifest.rowStep("equivalent_predicate", "canonical", fmt.Sprintf("equiv-%02d-%d-canonical", cycle, threshold),
				fmt.Sprintf("SELECT receipt_no FROM final_v5_rls.expense_detail WHERE amount >= %d ORDER BY receipt_no ASC", threshold), rows,
				[]string{atom}, fmt.Sprintf("amount-ge|%d", threshold)))
			appendStep(manifest.rowStep("equivalent_predicate", "reversed", fmt.Sprintf("equiv-%02d-%d-reversed", cycle, threshold),
				fmt.Sprintf("SELECT receipt_no FROM final_v5_rls.expense_detail WHERE %d <= amount ORDER BY receipt_no ASC", threshold), rows,
				[]string{atom}, fmt.Sprintf("amount-ge|%d", threshold)))
		}
	}
	for cycle := 1; cycle <= manifest.Schedule.AggregateCycles; cycle++ {
		for _, threshold := range manifest.Schedule.AggregateThresholds {
			appendStep(manifest.countStep("repeated_aggregation", "canonical", fmt.Sprintf("count-%02d-%d", cycle, threshold), threshold, Decision{}))
		}
	}
	previous := int64(len(manifest.amountRows(manifest.Schedule.AggregateThresholds[len(manifest.Schedule.AggregateThresholds)-1])))
	for adaptive := 1; adaptive <= manifest.Schedule.AdaptiveSteps; adaptive++ {
		threshold := manifest.Schedule.AdaptiveEvenThreshold
		if previous%2 != 0 {
			threshold = manifest.Schedule.AdaptiveOddThreshold
		}
		decision := Decision{PreviousStep: len(steps), PreviousValue: previous,
			Rule: "odd->880;even->553", Threshold: threshold}
		step := manifest.countStep("adaptive_choice", "prior-scalar-parity", fmt.Sprintf("adaptive-%02d", adaptive), threshold, decision)
		appendStep(step)
		previous = *step.Scalar
	}
	if len(steps) != TraceLength {
		return nil, fmt.Errorf("expanded RLS trace has %d steps, want %d", len(steps), TraceLength)
	}
	return steps, nil
}

// PolicyInvisibleStep returns the single read-only negative control.  A
// PostgreSQL SELECT policy does not raise an authorization SQLSTATE for a row
// hidden by RLS; it makes that row invisible.  The exact observable proof is
// therefore a successful statement with an authenticated empty result.  The
// TaskGate arm executes the same predicate under its mandatory Sales scope.
func (manifest Manifest) PolicyInvisibleStep() (Step, error) {
	if err := manifest.Validate(); err != nil {
		return Step{}, err
	}
	if _, err := manifest.PolicyInvisibleRow(); err != nil {
		return Step{}, err
	}
	step := manifest.rowStep(
		"policy_denied_control",
		"force-rls-zero-row",
		"policy-invisible-receipt",
		fmt.Sprintf("SELECT receipt_no FROM final_v5_rls.expense_detail WHERE receipt_no = '%s'", PolicyInvisibleReceipt),
		nil,
		[]string{outcomeAtom("receipt_no", "=", PolicyInvisibleReceipt)},
		"policy-invisible|"+PolicyInvisibleReceipt,
	)
	step.Index = 1
	step.ExpectedSHA256 = ResultSHA256(step.ExpectedRows)
	return step, nil
}

// PolicyInvisibleRow identifies the physical fixture row whose invisibility is
// proved by PolicyInvisibleStep.
func (manifest Manifest) PolicyInvisibleRow() (FixtureRow, error) {
	if err := manifest.Validate(); err != nil {
		return FixtureRow{}, err
	}
	target := manifest.row(PolicyInvisibleReceipt)
	if target.ReceiptNo != PolicyInvisibleReceipt || target.Department == manifest.PolicyDepartment {
		return FixtureRow{}, errors.New("RLS policy-control target is absent or policy-visible")
	}
	return target, nil
}

func (manifest Manifest) rowStep(family, variant, id, sql string, rows []FixtureRow, extraOutcome []string, normalForm string) Step {
	expected := make([][]string, 0, len(rows))
	// Production V5 binds mandatory scope into PredicateContextSHA256; it does
	// not charge the scope clause as a caller-controlled predicate atom.  Each
	// query therefore contributes only its caller filters plus one composite.
	oracle := finalv5oracle.Observation{Outcome: []string{compositeOutcome(normalForm, rows)}}
	oracle.Outcome = append(oracle.Outcome, extraOutcome...)
	for _, row := range rows {
		expected = append(expected, []string{row.ReceiptNo})
		oracle.Release = append(oracle.Release, releaseCell(row.ReceiptNo, "receipt_no", row.ReceiptNo))
		// Production ordinal influence is the set of evidence-field cells.  The
		// row handle identifies those cells but is not a separate influence fact.
		oracle.Dependency = append(oracle.Dependency, dependencyCell(row.ReceiptNo, "department", row.Department), dependencyCell(row.ReceiptNo, "receipt_no", row.ReceiptNo))
		if family == "equivalent_predicate" {
			oracle.Dependency = append(oracle.Dependency, dependencyCell(row.ReceiptNo, "amount", strconv.FormatInt(row.Amount, 10)))
		}
	}
	return Step{ID: id, Family: family, Variant: variant, DirectSQL: sql, ExpectedRows: expected, Oracle: normalizeObservation(oracle)}
}

func (manifest Manifest) countStep(family, variant, id string, threshold int64, decision Decision) Step {
	rows := manifest.amountRows(threshold)
	value := int64(len(rows))
	expected := [][]string{{strconv.FormatInt(value, 10)}}
	oracle := finalv5oracle.Observation{
		Release: []string{releaseAggregate(threshold, value, rows)},
		Outcome: []string{outcomeAtom("amount", ">=", strconv.FormatInt(threshold, 10)), compositeOutcome(fmt.Sprintf("count-ge|%d", threshold), rows)},
	}
	for _, row := range rows {
		oracle.Dependency = append(oracle.Dependency, dependencyCell(row.ReceiptNo, "department", row.Department), dependencyCell(row.ReceiptNo, "amount", strconv.FormatInt(row.Amount, 10)))
	}
	return Step{ID: id, Family: family, Variant: variant,
		DirectSQL:    fmt.Sprintf("SELECT count(*) AS request_count FROM final_v5_rls.expense_detail WHERE amount >= %d", threshold),
		ExpectedRows: expected, Scalar: &value, Decision: decision, Oracle: normalizeObservation(oracle)}
}

func (manifest Manifest) row(receipt string) FixtureRow {
	for _, row := range manifest.Rows {
		if row.ReceiptNo == receipt {
			return row
		}
	}
	return FixtureRow{}
}

func (manifest Manifest) salesRows() []FixtureRow {
	rows := make([]FixtureRow, 0, 6)
	for _, row := range manifest.Rows {
		if row.Department == manifest.PolicyDepartment {
			rows = append(rows, row)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ReceiptNo < rows[j].ReceiptNo })
	return rows
}

func (manifest Manifest) amountRows(threshold int64) []FixtureRow {
	rows := manifest.salesRows()
	result := rows[:0]
	for _, row := range rows {
		if row.Amount >= threshold {
			result = append(result, row)
		}
	}
	return result
}

func Trace(manifest Manifest) ([]Step, error) { return manifest.Trace() }

func OracleTrace(steps []Step) []finalv5oracle.Observation {
	result := make([]finalv5oracle.Observation, len(steps))
	for index, step := range steps {
		result[index] = step.Oracle
	}
	return result
}

// ComputeBoundedStop recomputes the first strict budget crossing from the
// complete source-controlled oracle. A simultaneous multi-dimension crossing
// is rejected because it would not prove one unambiguous terminal cause.
func ComputeBoundedStop(steps []Step) (BoundedStop, error) {
	if len(steps) != TraceLength {
		return BoundedStop{}, fmt.Errorf("bounded RLS stop requires %d frozen steps", TraceLength)
	}
	trace := OracleTrace(steps)
	full, err := finalv5oracle.Evaluate(trace)
	if err != nil {
		return BoundedStop{}, err
	}
	if full.Release.Budget != BoundedMaxReleaseFacts || full.Dependency.Budget != BoundedMaxDependencyFacts ||
		full.Outcome.Budget != BoundedMaxOutcomeFacts {
		return BoundedStop{}, errors.New("bounded RLS Catalog limits differ from the recomputed full-trace 70% floors")
	}
	prefixes, err := finalv5oracle.EvaluatePrefixes(trace)
	if err != nil {
		return BoundedStop{}, err
	}
	for index, candidate := range prefixes {
		crossed := make([]string, 0, 3)
		if candidate.Release.Cardinality > full.Release.Budget {
			crossed = append(crossed, "release")
		}
		if candidate.Dependency.Cardinality > full.Dependency.Budget {
			crossed = append(crossed, "dependency")
		}
		if candidate.Outcome.Cardinality > full.Outcome.Budget {
			crossed = append(crossed, "outcome")
		}
		if len(crossed) == 0 {
			continue
		}
		if index == 0 || len(crossed) != 1 {
			return BoundedStop{}, errors.New("bounded RLS trace lacks one unambiguous legal-prefix crossing")
		}
		reasons := map[string]string{
			"release": "ROOT_RELEASE_CEILING_EXCEEDED", "dependency": "ROOT_DEPENDENCY_CEILING_EXCEEDED",
			"outcome": "ROOT_OUTCOME_CEILING_EXCEEDED",
		}
		return BoundedStop{Index: index + 1, SuccessfulQueries: index, Dimension: crossed[0],
			ErrorReason: reasons[crossed[0]], Before: prefixes[index-1], Candidate: candidate, Full: full}, nil
	}
	return BoundedStop{}, errors.New("bounded RLS trace never crosses its full-trace 70% budget")
}

func ExpectedPoliciesJSON() json.RawMessage {
	value, _ := json.Marshal([]map[string]any{{
		"schema": "final_v5_rls", "table": "expense_detail", "policy": "final_v5_sales_scope",
		"permissive": "PERMISSIVE", "roles": []string{"final_v5_rls_reader"}, "command": "SELECT",
		"qual": "(department = '销售部'::text)", "with_check": nil,
	}})
	return value
}

func ExpectedMembershipJSON() json.RawMessage { return json.RawMessage("[]") }

func ExpectedGrantsJSON() json.RawMessage {
	value, _ := json.Marshal([]map[string]string{
		{"kind": "column", "schema": "final_v5_rls", "relation": "expense_detail", "name": "amount", "privilege": "SELECT"},
		{"kind": "column", "schema": "final_v5_rls", "relation": "expense_detail", "name": "receipt_no", "privilege": "SELECT"},
		{"kind": "schema", "schema": "final_v5_rls", "relation": "", "name": "", "privilege": "USAGE"},
	})
	return value
}

func DatasetSHA256(manifest Manifest) string {
	encoded, _ := json.Marshal(manifest.Rows)
	return domainDigest("TASKGATE-FINAL-V5-RLS-DATASET-V1", string(encoded))
}

func ResultSHA256(rows [][]string) string {
	encoded := make([][]byte, len(rows))
	for index, row := range rows {
		encoded[index], _ = json.Marshal(row)
	}
	sort.Slice(encoded, func(i, j int) bool { return string(encoded[i]) < string(encoded[j]) })
	hash := sha256.New()
	for _, row := range encoded {
		_, _ = fmt.Fprintf(hash, "%d:", len(row))
		_, _ = hash.Write(row)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// VerifiedResultSHA256 matches the repository's canonical typed-result hash.
// Receipt projections are text and COUNT(*) is bigint in both PostgreSQL and
// released Parquet, so this independently freezes their exact SQL types.
func VerifiedResultSHA256(step Step) string {
	rows := make([][]any, len(step.ExpectedRows))
	for rowIndex, row := range step.ExpectedRows {
		rows[rowIndex] = make([]any, len(row))
		for columnIndex, value := range row {
			rows[rowIndex][columnIndex] = value
		}
	}
	if step.Scalar != nil {
		rows = [][]any{{*step.Scalar}}
	}
	encoded := make([][]byte, len(rows))
	for index, row := range rows {
		encoded[index], _ = json.Marshal(row)
	}
	sort.Slice(encoded, func(i, j int) bool { return string(encoded[i]) < string(encoded[j]) })
	hash := sha256.New()
	for _, row := range encoded {
		_, _ = fmt.Fprintf(hash, "%d:", len(row))
		_, _ = hash.Write(row)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TraceResultSHA256(steps []Step, accepted int) string {
	if accepted > len(steps) {
		accepted = len(steps)
	}
	values := make([]string, 0, accepted)
	for _, step := range steps[:accepted] {
		values = append(values, step.ExpectedSHA256)
	}
	return domainDigest("TASKGATE-FINAL-V5-RLS-TRACE-RESULT-V1", strings.Join(values, "\x00"))
}

func SHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func normalizeObservation(input finalv5oracle.Observation) finalv5oracle.Observation {
	input.Release = sortedUnique(input.Release)
	input.Dependency = sortedUnique(input.Dependency)
	input.Outcome = sortedUnique(input.Outcome)
	return input
}

func sortedUnique(values []string) []string {
	sort.Strings(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func releaseCell(receipt, field, value string) string {
	return domainDigest("release-cell", receipt+"\x00"+field+"\x00"+value)
}
func dependencyCell(receipt, field, value string) string {
	return domainDigest("dependency-cell", receipt+"\x00"+field+"\x00"+value)
}
func outcomeAtom(field, op, value string) string {
	return domainDigest("outcome-atom", field+"\x00"+op+"\x00"+value)
}
func compositeOutcome(normalForm string, rows []FixtureRow) string {
	receipts := make([]string, len(rows))
	for index, row := range rows {
		receipts[index] = row.ReceiptNo
	}
	return domainDigest("outcome-composite", normalForm+"\x00"+strings.Join(receipts, ","))
}
func releaseAggregate(threshold, value int64, rows []FixtureRow) string {
	receipts := make([]string, len(rows))
	for index, row := range rows {
		receipts[index] = row.ReceiptNo
	}
	return domainDigest("release-count", fmt.Sprintf("%d\x00%d\x00%s", threshold, value, strings.Join(receipts, ",")))
}
func domainDigest(domain, value string) string {
	digest := sha256.Sum256([]byte("TASKGATE-FINAL-V5-RLS-ORACLE-V1\x00" + domain + "\x00" + value))
	return hex.EncodeToString(digest[:])
}
