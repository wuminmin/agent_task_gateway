// Package finalv5binding owns the credential-free, author-reviewed private
// binding contract used by the final-v5 publication campaign.  It deliberately
// does not know how to execute an observer or a dataset probe: both executable
// paths are built from the frozen submission and the probe SQL is compiled
// below from the source-controlled publication query.
package finalv5binding

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

const (
	SectionName = "final_v5_adapter_v1"
	CatalogPath = "config/catalog.yaml"

	// DatasetProbeSQL is the query in
	// evaluation/final-v5-wsl2/sql/datasets/default-fingerprint.sql, without
	// its terminating semicolon/newline.  A repository test compares these
	// bytes so the built adapter and fresh-deployment launcher cannot drift.
	DatasetProbeSQL = `SELECT jsonb_build_object(
  'database', current_database(),
  'expense_detail_rows', (SELECT count(*) FROM reporting.expense_detail),
  'expense_detail_keys', (SELECT md5(string_agg(receipt_no, E'\n' ORDER BY receipt_no)) FROM reporting.expense_detail),
  'expense_summary_rows', (SELECT count(*) FROM reporting.expense_summary),
  'expense_summary_keys', (SELECT md5(string_agg(month || E'\t' || department || E'\t' || expense_type, E'\n' ORDER BY month, department, expense_type)) FROM reporting.expense_summary)
)::text`
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Binding is the validated representation. FileSHA256 identifies exact input
// bytes while SectionSHA256 identifies deterministic JSON bytes for the strict
// adapter section, independent of harmless top-level JSON formatting.
type Binding struct {
	DatasetSHA256 string
	CatalogSHA256 string
	FileSHA256    string
	SectionSHA256 string
	Section       Section
}

type Section struct {
	SchemaVersion int              `json:"schema_version"`
	Scale         *ScaleBinding    `json:"scale"`
	Artifact      *ArtifactBinding `json:"artifact"`
	ProvSQL       *ProvSQLBinding  `json:"provsql"`
}

type BoundTaskRequest struct {
	Objective         string              `json:"objective"`
	DataProducts      []string            `json:"data_products"`
	Columns           map[string][]string `json:"columns"`
	Scopes            map[string][]string `json:"scopes"`
	VisibleRelation   string              `json:"visible_relation"`
	CompanionRelation string              `json:"companion_relation"`
}

type BoundQueryExpectation struct {
	SQL                    string `json:"sql"`
	ExpectedRows           int64  `json:"expected_rows"`
	ExpectedColumns        int    `json:"expected_columns"`
	ExpectedResultSHA256   string `json:"expected_result_sha256"`
	DependencyFacts        int64  `json:"dependency_facts"`
	DependencySetSHA256    string `json:"dependency_set_sha256"`
	ExpectedVisibleCalls   int64  `json:"expected_visible_calls,omitempty"`
	ExpectedCompanionCalls int64  `json:"expected_companion_calls,omitempty"`
}

type DependencyCellBinding struct {
	Task      BoundTaskRequest       `json:"task"`
	Candidate BoundQueryExpectation  `json:"candidate"`
	History   *BoundQueryExpectation `json:"history,omitempty"`
}

type ScaleBinding struct {
	DependencyE2E       map[string]DependencyCellBinding `json:"dependency_e2e"`
	EnableOutcomeMerkle bool                             `json:"enable_outcome_merkle"`
	EnableExtreme       bool                             `json:"enable_extreme,omitempty"`
}

type ArtifactCellBinding struct {
	Task  BoundTaskRequest      `json:"task"`
	Query BoundQueryExpectation `json:"query"`
}

type ArtifactBinding struct {
	ResultHeavy map[string]ArtifactCellBinding `json:"result_heavy"`
}

type ProvSQLBinding struct {
	FixtureVersion                string                           `json:"fixture_version"`
	FixtureSQLSHA256              string                           `json:"fixture_sql_sha256"`
	EnableSQLSHA256               string                           `json:"enable_sql_sha256"`
	DatasetSHA256                 string                           `json:"dataset_sha256"`
	DatasetProbeSQLSHA256         string                           `json:"dataset_probe_sql_sha256"`
	BusinessDatasetProbeSQLSHA256 string                           `json:"business_dataset_probe_sql_sha256"`
	Task                          BoundTaskRequest                 `json:"task"`
	TaskGate                      map[string]BoundQueryExpectation `json:"taskgate"`
}

func DatasetProbeSHA256() string { return shaBytes([]byte(DatasetProbeSQL)) }

// LoadFile reads and validates one bounded non-symlink input file. It performs
// no transient runtime checks, so author review does not depend on a campaign
// path that will only exist after the frozen binaries are built.
func LoadFile(path string) (Binding, error) {
	var result Binding
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() > 4<<20 {
		return result, errors.New("private dataset binding must be a bounded regular 0600 file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	result, err = Parse(value)
	if err != nil {
		return Binding{}, err
	}
	result.FileSHA256 = shaBytes(value)
	return result, nil
}

// LoadPublicationFile additionally proves that every private task/query is
// realizable by the one source-controlled Catalog that the formal Compose
// topology mounts into Gateway. The path is fixed by callers to CatalogPath;
// it is never accepted from the private binding.
func LoadPublicationFile(path, catalogPath string) (Binding, error) {
	binding, err := LoadFile(path)
	if err != nil {
		return Binding{}, err
	}
	if catalogPath != CatalogPath {
		return Binding{}, errors.New("publication Catalog path is source-controlled")
	}
	info, err := os.Lstat(catalogPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() > 4<<20 {
		return Binding{}, errors.New("source-controlled publication Catalog is missing or unsafe")
	}
	value, err := os.ReadFile(catalogPath)
	if err != nil {
		return Binding{}, err
	}
	parsed, err := catalog.Parse(value)
	if err != nil {
		return Binding{}, fmt.Errorf("source-controlled publication Catalog: %w", err)
	}
	if binding.CatalogSHA256 != shaBytes(value) || parsed.SHA256 != binding.CatalogSHA256 {
		return Binding{}, errors.New("reviewed catalog_sha256 differs from source-controlled Catalog bytes")
	}
	if err := ValidateAgainstCatalog(binding, parsed); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

// ValidateAgainstCatalog checks the pre-provisioning boundary the runtime OA
// and Gateway would otherwise discover only after containers had started.
func ValidateAgainstCatalog(binding Binding, source *catalog.Catalog) error {
	if source == nil || source.SHA256 == "" || binding.CatalogSHA256 != source.SHA256 {
		return errors.New("binding/Catalog identity is invalid")
	}
	for scale, cell := range binding.Section.Scale.DependencyE2E {
		queries := []BoundQueryExpectation{cell.Candidate, cell.Candidate}
		if cell.History != nil {
			queries = append(queries, *cell.History)
		}
		if err := validateTaskCatalogCapacity(source, cell.Task, queries); err != nil {
			return fmt.Errorf("scale %s is not realizable by frozen Catalog: %w", scale, err)
		}
	}
	for scale, cell := range binding.Section.Artifact.ResultHeavy {
		if err := validateTaskCatalogCapacity(source, cell.Task, []BoundQueryExpectation{cell.Query}); err != nil {
			return fmt.Errorf("artifact %s is not realizable by frozen Catalog: %w", scale, err)
		}
	}
	for key, query := range binding.Section.ProvSQL.TaskGate {
		if err := validateTaskCatalogCapacity(source, binding.Section.ProvSQL.Task, []BoundQueryExpectation{query}); err != nil {
			return fmt.Errorf("ProvSQL %s is not realizable by frozen Catalog: %w", key, err)
		}
	}
	return nil
}

func validateTaskCatalogCapacity(source *catalog.Catalog, task BoundTaskRequest, queries []BoundQueryExpectation) error {
	const maxSignedInt64 = int64(^uint64(0) >> 1)
	policy, err := source.ResolveTaskPolicy(task.DataProducts)
	if err != nil {
		return fmt.Errorf("exact product approval route: %w", err)
	}
	knownScopes := make(map[string]catalog.Scope, len(source.Scopes))
	allowedTaskScopes := make(map[string]bool)
	for _, one := range source.Scopes {
		knownScopes[one.Name] = one
	}
	visibleProduct := ""
	grant := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, 0, len(policy.Products))}
	for _, product := range policy.Products {
		approved := make(map[string]bool, len(product.Fields))
		for _, field := range product.Fields {
			approved[field.Name] = true
			if _, isScope := knownScopes[field.Name]; isScope {
				allowedTaskScopes[field.Name] = true
			}
		}
		for _, column := range task.Columns[product.Name] {
			if !approved[column] {
				return fmt.Errorf("product %s does not publish approved column %s", product.Name, column)
			}
		}
		for _, scope := range product.Scopes {
			allowedTaskScopes[scope] = true
			if len(task.Scopes[scope]) == 0 {
				return fmt.Errorf("product %s lacks required scope %s", product.Name, scope)
			}
		}
		if product.ReportingView == task.VisibleRelation {
			publication, present := source.LookupSnapshotPublication(product.SnapshotPublication)
			if !present || publication.OrdinalSidecar != task.CompanionRelation {
				return errors.New("visible/companion relations do not identify one published product")
			}
			visibleProduct = product.Name
		}
		schema, view, ok := strings.Cut(product.ReportingView, ".")
		if !ok {
			return errors.New("Catalog reporting view is not canonical")
		}
		mandatory := make([]sqlpolicy.ScopePredicate, 0, len(task.Scopes))
		for scope, values := range task.Scopes {
			if approved[scope] {
				mandatory = append(mandatory, sqlpolicy.ScopePredicate{Column: scope, Operator: sqlpolicy.ScopeIn,
					Values: append([]string(nil), values...)})
			}
		}
		grant.Products = append(grant.Products, sqlpolicy.ProductGrant{LogicalName: product.Name,
			PhysicalSchema: schema, PhysicalView: view, ApprovedColumns: append([]string(nil), task.Columns[product.Name]...),
			AllowedFunctions:  append([]string(nil), product.AllowedFunctions...),
			AllowedAggregates: append([]string(nil), product.AllowedAggregates...),
			AllowedOperators:  append([]string(nil), product.AllowedOperators...), MandatoryScope: mandatory})
	}
	if visibleProduct == "" {
		return errors.New("bound observer relations are outside requested Catalog products")
	}
	for scope, values := range task.Scopes {
		definition, present := knownScopes[scope]
		if !present || !allowedTaskScopes[scope] || !scopeValuesAllowed(definition, values) {
			return fmt.Errorf("scope %s is absent or outside Catalog bounds", scope)
		}
	}
	if len(queries) == 0 || policy.Budget.MaxQueries < int64(len(queries)) {
		return fmt.Errorf("budget max_queries=%d is below required %d", policy.Budget.MaxQueries, len(queries))
	}
	var totalRows, maximumInfluence, totalRelease int64
	engine := sqlpolicy.New(sqlpolicy.Config{})
	for _, query := range queries {
		decision, err := engine.Authorize(sqlpolicy.Request{SQL: query.SQL, Grant: grant, RowLimit: maxInt64(1, query.ExpectedRows)})
		if err != nil || len(decision.ReferencedProducts) == 0 {
			return fmt.Errorf("query is outside Catalog/task grant: %w", err)
		}
		visibleReferenced := false
		for _, product := range decision.ReferencedProducts {
			visibleReferenced = visibleReferenced || product == visibleProduct
		}
		if !visibleReferenced {
			return errors.New("query does not reference the product measured by bound Business SQL observer relations")
		}
		if query.ExpectedRows > maxSignedInt64-totalRows {
			return errors.New("cumulative expected row budget overflows int64")
		}
		totalRows += query.ExpectedRows
		if query.DependencyFacts > maximumInfluence {
			maximumInfluence = query.DependencyFacts
		}
		if query.ExpectedRows > 0 && int64(query.ExpectedColumns) > maxSignedInt64/query.ExpectedRows {
			return errors.New("expected release budget overflows int64")
		}
		release := query.ExpectedRows * int64(query.ExpectedColumns)
		if release > maxSignedInt64-totalRelease {
			return errors.New("cumulative expected release budget overflows int64")
		}
		totalRelease += release
	}
	if policy.Budget.MaxRows < totalRows {
		return fmt.Errorf("cumulative budget max_rows=%d is below required %d", policy.Budget.MaxRows, totalRows)
	}
	if policy.Budget.MaxInfluenceFacts < maximumInfluence {
		return fmt.Errorf("budget max_influence_facts=%d is below required %d", policy.Budget.MaxInfluenceFacts, maximumInfluence)
	}
	if policy.Budget.MaxReleaseFacts < totalRelease {
		return fmt.Errorf("budget max_release_facts=%d is below safe required %d", policy.Budget.MaxReleaseFacts, totalRelease)
	}
	return nil
}

func scopeValuesAllowed(definition catalog.Scope, values []string) bool {
	if len(values) == 0 {
		return false
	}
	switch definition.Type {
	case catalog.ScopeTypeEnum:
		allowed := map[string]bool{}
		seen := map[string]bool{}
		for _, value := range definition.AllowedValues {
			allowed[value] = true
		}
		for _, value := range values {
			if !allowed[value] || seen[value] {
				return false
			}
			seen[value] = true
		}
		return true
	case catalog.ScopeTypeDateRange:
		return len(values) == 2 && values[0] >= definition.Min && values[1] <= definition.Max && values[0] <= values[1]
	default:
		return false
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

// Parse enforces the complete publication matrix. An implementation cannot
// make a capability usable with a partial private oracle or an extra field.
func Parse(value []byte) (Binding, error) {
	var result Binding
	var top map[string]json.RawMessage
	if err := strictJSON(value, &top); err != nil {
		return result, fmt.Errorf("decode dataset binding: %w", err)
	}
	if len(top) != 3 || top["dataset_sha256"] == nil || top["catalog_sha256"] == nil || top[SectionName] == nil {
		return result, errors.New("dataset binding must contain exactly dataset_sha256, catalog_sha256, and final_v5_adapter_v1")
	}
	if err := json.Unmarshal(top["dataset_sha256"], &result.DatasetSHA256); err != nil || !ValidDigest(result.DatasetSHA256) {
		return result, errors.New("dataset binding lacks dataset_sha256")
	}
	if err := json.Unmarshal(top["catalog_sha256"], &result.CatalogSHA256); err != nil || !ValidDigest(result.CatalogSHA256) {
		return result, errors.New("dataset binding lacks catalog_sha256")
	}
	if err := strictJSON(top[SectionName], &result.Section); err != nil {
		return result, fmt.Errorf("decode strict adapter binding section: %w", err)
	}
	if result.Section.SchemaVersion != 1 {
		return result, errors.New("adapter binding section schema is unsupported")
	}
	if err := validateScaleBinding(result.Section.Scale); err != nil {
		return result, err
	}
	if err := validateArtifactBinding(result.Section.Artifact); err != nil {
		return result, err
	}
	if err := ValidateProvSQLBinding(result.Section.ProvSQL); err != nil {
		return result, err
	}
	canonicalSection, err := json.Marshal(result.Section)
	if err != nil {
		return result, err
	}
	result.SectionSHA256 = shaBytes(canonicalSection)
	result.FileSHA256 = shaBytes(value)
	return result, nil
}

func ValidateBoundTask(task BoundTaskRequest) error {
	canonicalRelation := regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`)
	if strings.TrimSpace(task.Objective) == "" || len(task.DataProducts) == 0 || len(task.Columns) == 0 ||
		!canonicalRelation.MatchString(task.VisibleRelation) || !canonicalRelation.MatchString(task.CompanionRelation) ||
		task.VisibleRelation == task.CompanionRelation {
		return errors.New("bound task request is incomplete")
	}
	seen := map[string]bool{}
	for _, product := range task.DataProducts {
		if strings.TrimSpace(product) == "" || seen[product] || len(task.Columns[product]) == 0 {
			return errors.New("bound task products/columns are inconsistent")
		}
		seen[product] = true
		if err := validateUniqueStrings(task.Columns[product]); err != nil {
			return errors.New("bound task contains an empty or duplicate column")
		}
	}
	if len(seen) != len(task.Columns) {
		return errors.New("bound task columns contain an undeclared product")
	}
	for scope, values := range task.Scopes {
		if strings.TrimSpace(scope) == "" || len(values) == 0 || validateUniqueStrings(values) != nil {
			return errors.New("bound task scopes contain an empty name or invalid value")
		}
	}
	return nil
}

func ValidateBoundQuery(query BoundQueryExpectation) error {
	if err := validateReadOnlySQL(query.SQL); err != nil {
		return err
	}
	if query.ExpectedRows < 0 || query.ExpectedColumns <= 0 || !ValidDigest(query.ExpectedResultSHA256) ||
		query.DependencyFacts < 0 || (query.DependencyFacts > 0 && !ValidDigest(query.DependencySetSHA256)) ||
		(query.DependencyFacts == 0 && query.DependencySetSHA256 != "") || query.ExpectedVisibleCalls < 0 ||
		query.ExpectedCompanionCalls < 0 {
		return errors.New("bound query expectation is incomplete")
	}
	return nil
}

func validateBoundQueryForTask(query BoundQueryExpectation, task BoundTaskRequest) error {
	if err := ValidateBoundQuery(query); err != nil {
		return err
	}
	grant := sqlpolicy.Grant{Products: make([]sqlpolicy.ProductGrant, 0, len(task.DataProducts))}
	for _, product := range task.DataProducts {
		grant.Products = append(grant.Products, sqlpolicy.ProductGrant{
			LogicalName: product, PhysicalSchema: "reporting", PhysicalView: product,
			ApprovedColumns: append([]string(nil), task.Columns[product]...),
		})
	}
	if _, err := sqlpolicy.New(sqlpolicy.Config{}).Authorize(sqlpolicy.Request{
		SQL: query.SQL, Grant: grant, RowLimit: int64(^uint64(0) >> 1),
	}); err != nil {
		return fmt.Errorf("bound query exceeds its approved task SQL grant: %w", err)
	}
	return nil
}

func ProvSQLBindingKey(scale string, nonce int64) string {
	return scale + "/" + fmt.Sprintf("%d", nonce)
}

func ValidateProvSQLBinding(binding *ProvSQLBinding) error {
	if binding == nil || binding.FixtureVersion != provsqlfixture.Version ||
		binding.FixtureSQLSHA256 != provsqlfixture.FixtureSQLSHA256() ||
		binding.EnableSQLSHA256 != provsqlfixture.EnableSQLSHA256() ||
		binding.DatasetSHA256 != provsqlfixture.ExpectedDatasetSHA256() ||
		binding.DatasetProbeSQLSHA256 != provsqlfixture.DatasetProbeSQLSHA256() ||
		binding.BusinessDatasetProbeSQLSHA256 != provsqlfixture.BusinessDatasetProbeSQLSHA256() ||
		ValidateBoundTask(binding.Task) != nil || len(binding.TaskGate) != 105 {
		return errors.New("ProvSQL deployment binding differs from the frozen fixture")
	}
	wanted := map[string]bool{}
	for _, scale := range []string{"1k", "10k", "45k"} {
		for _, phase := range []struct {
			warmup bool
			count  int
		}{{warmup: true, count: 5}, {warmup: false, count: 30}} {
			for iteration := 1; iteration <= phase.count; iteration++ {
				nonce, err := provsqlfixture.Nonce(scale, 1, iteration, phase.warmup)
				if err != nil {
					return err
				}
				key := ProvSQLBindingKey(scale, nonce)
				wanted[key] = true
				expected, present := binding.TaskGate[key]
				if !present {
					return errors.New("ProvSQL deployment binding omits a frozen nonce cell")
				}
				if err := validateProvSQLCellExpectation(expected, binding.Task, scale, nonce); err != nil {
					return err
				}
			}
		}
	}
	for key := range binding.TaskGate {
		if !wanted[key] {
			return errors.New("ProvSQL deployment binding contains an unfrozen nonce cell")
		}
	}
	return nil
}

func ValidateProvSQLCellBinding(binding *ProvSQLBinding, scale string, nonce int64) (BoundQueryExpectation, error) {
	if err := ValidateProvSQLBinding(binding); err != nil {
		return BoundQueryExpectation{}, err
	}
	expected, present := binding.TaskGate[ProvSQLBindingKey(scale, nonce)]
	if !present {
		return BoundQueryExpectation{}, errors.New("ProvSQL TaskGate cell lacks its exact frozen query/FactSet oracle")
	}
	if err := validateProvSQLCellExpectation(expected, binding.Task, scale, nonce); err != nil {
		return BoundQueryExpectation{}, err
	}
	return expected, nil
}

func validateScaleBinding(binding *ScaleBinding) error {
	if binding == nil || !binding.EnableOutcomeMerkle || len(binding.DependencyE2E) != 12 {
		return errors.New("scale deployment binding must contain the exact 12 dependency cells and enable Outcome-Merkle")
	}
	wanted := map[string]int64{}
	for _, prefix := range []struct {
		name  string
		facts int64
	}{{"10k", 10_000}, {"100k", 100_000}, {"1035000", 1_035_000}} {
		for _, overlap := range []int64{0, 50, 90, 100} {
			wanted[fmt.Sprintf("%s-overlap-%d", prefix.name, overlap)] = prefix.facts * overlap / 100
		}
	}
	for scale, overlapFacts := range wanted {
		cell, present := binding.DependencyE2E[scale]
		if !present || ValidateBoundTask(cell.Task) != nil || validateBoundQueryForTask(cell.Candidate, cell.Task) != nil {
			return errors.New("scale deployment binding omits or invalidates a frozen dependency cell")
		}
		parts := strings.Split(scale, "-overlap-")
		candidateFacts := map[string]int64{"10k": 10_000, "100k": 100_000, "1035000": 1_035_000}[parts[0]]
		if cell.Candidate.DependencyFacts != candidateFacts || !ValidDigest(cell.Candidate.DependencySetSHA256) {
			return errors.New("candidate binding differs from frozen dependency scale")
		}
		if overlapFacts == 0 {
			if cell.History != nil {
				return errors.New("zero-overlap cell unexpectedly declares history")
			}
		} else if cell.History == nil || validateBoundQueryForTask(*cell.History, cell.Task) != nil ||
			cell.History.DependencyFacts != overlapFacts || !ValidDigest(cell.History.DependencySetSHA256) {
			return errors.New("history binding differs from frozen dependency overlap")
		}
	}
	for scale := range binding.DependencyE2E {
		if _, present := wanted[scale]; !present {
			return errors.New("scale deployment binding contains an unfrozen dependency cell")
		}
	}
	return nil
}

func validateArtifactBinding(binding *ArtifactBinding) error {
	wanted := map[string]struct {
		rows    int64
		columns int
	}{
		"100x4": {100, 4}, "10k-x4": {10_000, 4}, "100k-x4": {100_000, 4},
		"100x16": {100, 16}, "10k-x16": {10_000, 16}, "100k-x16": {100_000, 16},
	}
	if binding == nil || len(binding.ResultHeavy) != len(wanted) {
		return errors.New("artifact deployment binding must contain the exact six result-heavy cells")
	}
	for scale, spec := range wanted {
		cell, present := binding.ResultHeavy[scale]
		if !present || ValidateBoundTask(cell.Task) != nil || validateBoundQueryForTask(cell.Query, cell.Task) != nil ||
			cell.Query.ExpectedRows != spec.rows || cell.Query.ExpectedColumns != spec.columns ||
			cell.Query.DependencyFacts <= 0 || !ValidDigest(cell.Query.DependencySetSHA256) {
			return errors.New("artifact cell binding differs from frozen NxC or Dependency oracle")
		}
		approvedColumns := 0
		for _, columns := range cell.Task.Columns {
			approvedColumns += len(columns)
		}
		if approvedColumns < spec.columns {
			return errors.New("artifact task approves fewer columns than the frozen result width")
		}
	}
	for scale := range binding.ResultHeavy {
		if _, present := wanted[scale]; !present {
			return errors.New("artifact deployment binding contains an unfrozen result-heavy cell")
		}
	}
	return nil
}

func validateProvSQLCellExpectation(expected BoundQueryExpectation, task BoundTaskRequest, scale string, nonce int64) error {
	logical, err := provsqlfixture.LogicalSQL(scale, nonce)
	if err != nil || validateBoundQueryForTask(expected, task) != nil || expected.SQL != logical ||
		expected.ExpectedRows != provsqlfixture.ExpectedRows || expected.ExpectedColumns != provsqlfixture.ExpectedColumns ||
		expected.DependencyFacts <= 0 || !ValidDigest(expected.DependencySetSHA256) ||
		expected.ExpectedVisibleCalls != 1 || expected.ExpectedCompanionCalls != 1 {
		return errors.New("ProvSQL TaskGate cell lacks its exact frozen query/FactSet oracle")
	}
	rows, err := provsqlfixture.ExpectedResultRows(scale)
	if err != nil {
		return err
	}
	resultSHA256, err := canonicalResultHash(rows)
	if err != nil || expected.ExpectedResultSHA256 != resultSHA256 {
		return errors.New("ProvSQL TaskGate result oracle differs from the source fixture")
	}
	return nil
}

func validateReadOnlySQL(sqlText string) error {
	trimmed := strings.TrimSpace(sqlText)
	if trimmed == "" || strings.ContainsRune(trimmed, 0) {
		return errors.New("only one pure SELECT/CTE statement is allowed")
	}
	parsed, err := pg_query.Parse(trimmed)
	if err != nil || len(parsed.GetStmts()) != 1 || parsed.GetStmts()[0].GetStmt().GetSelectStmt() == nil {
		return errors.New("only one pure SELECT/CTE statement is allowed")
	}
	astJSON, err := pg_query.ParseToJSON(trimmed)
	if err != nil {
		return errors.New("only one pure SELECT/CTE statement is allowed")
	}
	var ast any
	decoder := json.NewDecoder(strings.NewReader(astJSON))
	if decoder.Decode(&ast) != nil || containsForbiddenSQLNode(ast) {
		return errors.New("SELECT INTO, locking, and data-modifying CTEs are forbidden")
	}
	return nil
}

func containsForbiddenSQLNode(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch key {
			case "InsertStmt", "UpdateStmt", "DeleteStmt", "MergeStmt", "CopyStmt", "CallStmt", "CreateTableAsStmt":
				return true
			case "intoClause":
				if child != nil {
					return true
				}
			case "lockingClause":
				if list, ok := child.([]any); !ok || len(list) != 0 {
					return true
				}
			}
			if containsForbiddenSQLNode(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsForbiddenSQLNode(child) {
				return true
			}
		}
	}
	return false
}

func validateUniqueStrings(values []string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || seen[value] {
			return errors.New("empty or duplicate value")
		}
		seen[value] = true
	}
	return nil
}

func canonicalResultHash(rows [][]any) (string, error) {
	encoded := make([][]byte, len(rows))
	for index, row := range rows {
		value, err := json.Marshal(row)
		if err != nil {
			return "", err
		}
		encoded[index] = value
	}
	sort.Slice(encoded, func(i, j int) bool { return string(encoded[i]) < string(encoded[j]) })
	h := sha256.New()
	for _, value := range encoded {
		_, _ = fmt.Fprintf(h, "%d:", len(value))
		_, _ = h.Write(value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func strictJSON(value []byte, target any) error {
	if err := rejectDuplicateJSONKeys(value); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(value []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON object key %q", key)
				}
				seen[key] = true
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("invalid JSON delimiter")
		}
	}
	if err := consume(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func ValidDigest(value string) bool {
	return digestPattern.MatchString(value)
}

func shaBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
