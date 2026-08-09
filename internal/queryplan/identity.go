package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// CompilerVersion names the compiler a prepared statement was produced by.
//
// It is part of the signed Query Execution Binding: a plan digest says what was
// asked for, and the compiler identity says what turned it into SQL. Two
// compilers can agree on a plan and disagree on the statement, and without this
// the executed bytes would move for a reason nothing recorded.
const CompilerVersion = "taskgate-query-compiler-v7"

const compilerIdentityDomain = "TASKGATE-QUERY-COMPILER-IDENTITY-V1"

// CompilerSHA256 is the compiler's behavioural identity.
//
// It is computed rather than declared, and it is computed from what the compiler
// actually does: frozen single-product and relational probe plans are compiled,
// normalized and footprinted, and the outputs are hashed together with the
// frozen contract versions. A change covered by those probes therefore moves
// this digest even when every version constant is untouched -- exactly the case
// a declared constant would miss.
//
// TestCompilerIdentityIsPinnedToItsSource keeps the value honest by pinning it;
// the failure tells the reader to bump CompilerVersion deliberately rather than
// to update the expectation.
func CompilerSHA256() (string, error) { return compilerIdentity() }

var compilerIdentity = sync.OnceValues(computeCompilerIdentity)

// compilerIdentityProbe is deliberately small and deliberately frozen. It
// exercises projection, aggregation, filtering, grouping, ordering and paging.
// compilerIdentityRelationalProbe separately covers role-qualified relational
// SQL and the V5 predicate footprint, including a repeated Product under two
// UNION branch roles. compilerIdentityGroupedRelationalProbe binds the grouped
// relational statement pair, complete group-key delivery order, NUMERIC text
// result encoding, semantic identity, and ordinal dependency program.
func compilerIdentityProbe() (QueryPlan, Product) {
	plan := QueryPlan{
		Product:    "expense",
		Columns:    []string{"month"},
		Aggregates: []Aggregate{{Function: "sum", Column: "amount", Alias: "total"}},
		Filters:    []Filter{{Column: "department", Op: "=", Value: "sales"}},
		GroupBy:    []string{"month"},
		OrderBy:    []Order{{Column: "month", Direction: "asc"}},
		Limit:      10,
	}
	product := Product{
		Name: "expense",
		Columns: map[string]struct{}{
			"month": {}, "amount": {}, "department": {},
		},
		AllowedAggregates: map[string]struct{}{"sum": {}},
		ColumnTypes: map[string]string{
			"month": "text", "amount": "numeric", "department": "text",
		},
		// The V2 normal form refuses a collatable column with no attested
		// collation, so the probe declares one for both text columns.
		ColumnCollations: map[string]string{"month": "C", "department": "C"},
		CollationVersions: map[string]string{
			"month":      "taskgate-compiler-identity-collation-v1",
			"department": "taskgate-compiler-identity-collation-v1",
		},
		SourceNamespace: "taskgate-compiler-identity",
		Snapshot:        "taskgate-compiler-identity-probe",
		StableRole:      "expense",
		StableEntityKey: []string{"month"},
	}
	return plan, product
}

func compilerIdentityRelationalProbe() (QueryPlan, map[string]Product) {
	_, product := compilerIdentityProbe()
	plan := QueryPlan{From: &From{UnionDistinct: &UnionDistinct{
		Role: "expense", Columns: []string{"month", "department"},
		Left: Scan{Product: product.Name, Role: "left_branch", Filters: []Filter{{
			Column: "department", Op: "=", Value: "sales",
		}}},
		Right: Scan{Product: product.Name, Role: "right_branch", Filters: []Filter{{
			Column: "department", Op: "=", Value: "engineering",
		}}},
	}}, Columns: []string{"expense.month"}}
	products := map[string]Product{product.Name: product}
	return plan, products
}

func compilerIdentityGroupedRelationalProbe() (QueryPlan, map[string]Product) {
	lines := Product{
		Name: "identity_lines",
		Columns: map[string]struct{}{
			"id": {}, "order_id": {}, "amount": {},
		},
		AllowedAggregates: map[string]struct{}{"sum": {}},
		ColumnTypes: map[string]string{
			"id": "bigint", "order_id": "bigint", "amount": "numeric",
		},
		SourceNamespace: "taskgate.compiler.identity.lines",
		Snapshot:        "taskgate-compiler-identity-grouped-probe",
		StableRole:      "identity_lines",
		StableEntityKey: []string{"id"},
	}
	orders := Product{
		Name: "identity_orders",
		Columns: map[string]struct{}{
			"id": {}, "status": {}, "partition_key": {},
		},
		AllowedAggregates: map[string]struct{}{"sum": {}},
		ColumnTypes: map[string]string{
			"id": "bigint", "status": "bigint", "partition_key": "integer",
		},
		SourceNamespace: "taskgate.compiler.identity.orders",
		Snapshot:        "taskgate-compiler-identity-grouped-probe",
		StableRole:      "identity_orders",
		StableEntityKey: []string{"id"},
	}
	plan := QueryPlan{
		From: &From{JoinMany: &JoinMany{
			Sources: []Scan{
				{Product: lines.Name, Role: lines.StableRole},
				{Product: orders.Name, Role: orders.StableRole},
			},
			On: []JoinPredicate{{
				Left: lines.StableRole + ".order_id", Right: orders.StableRole + ".id",
			}},
		}},
		Columns: []string{
			orders.StableRole + ".status",
			orders.StableRole + ".partition_key",
		},
		Aggregates: []Aggregate{{
			Function: "sum", Column: lines.StableRole + ".amount", Alias: "price",
			ResultEncoding: NumericTextResultEncoding,
		}},
		GroupBy: []string{
			orders.StableRole + ".status",
			orders.StableRole + ".partition_key",
		},
		OrderBy: []Order{
			{Column: orders.StableRole + ".status", Direction: "asc"},
			{Column: orders.StableRole + ".partition_key", Direction: "desc"},
		},
	}
	return plan, map[string]Product{lines.Name: lines, orders.Name: orders}
}

func computeCompilerIdentity() (string, error) {
	plan, product := compilerIdentityProbe()
	compiled, err := Compile(plan, product)
	if err != nil {
		return "", fmt.Errorf("compiler identity probe does not compile: %w", err)
	}
	normalV2, err := NormalizeV2(plan, product)
	if err != nil {
		return "", fmt.Errorf("compiler identity probe does not normalize under V2: %w", err)
	}
	normalV2Digest, err := normalV2.Digest()
	if err != nil {
		return "", fmt.Errorf("compiler identity probe normal form does not digest: %w", err)
	}
	normalV4, err := NormalizeV4(plan, product)
	if err != nil {
		return "", fmt.Errorf("compiler identity probe does not normalize under V4: %w", err)
	}
	normalV4Digest, err := normalV4.Digest()
	if err != nil {
		return "", fmt.Errorf("compiler identity probe V4 normal form does not digest: %w", err)
	}
	relationalPlan, relationalProducts := compilerIdentityRelationalProbe()
	relational, err := CompileRelational(relationalPlan, relationalProducts)
	if err != nil {
		return "", fmt.Errorf("compiler identity relational probe does not compile: %w", err)
	}
	predicateProducts, err := PredicateProductsForSources(relationalProducts, relational.Sources)
	if err != nil {
		return "", fmt.Errorf("compiler identity relational sources do not bind: %w", err)
	}
	footprint, err := BuildPredicateFootprint(relationalPlan, PredicateBindings{
		CatalogSHA256: strings.Repeat("a", 64), Products: predicateProducts,
	},
		strings.Repeat("b", 64), PredicateLimits{})
	if err != nil {
		return "", fmt.Errorf("compiler identity relational probe does not footprint: %w", err)
	}
	footprintBytes, err := json.Marshal(footprint)
	if err != nil {
		return "", fmt.Errorf("compiler identity predicate footprint does not encode: %w", err)
	}
	groupedPlan, groupedProducts := compilerIdentityGroupedRelationalProbe()
	grouped, err := CompileRelational(groupedPlan, groupedProducts)
	if err != nil {
		return "", fmt.Errorf("compiler identity grouped relational probe does not compile: %w", err)
	}
	groupedSemantic, err := SemanticNormalFormV4(groupedPlan, grouped, groupedProducts)
	if err != nil {
		return "", fmt.Errorf("compiler identity grouped relational probe does not normalize: %w", err)
	}
	groupedOrdinalJSON, err := grouped.OrdinalProgram.CanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("compiler identity grouped relational ordinal program does not encode: %w", err)
	}
	groupedOrdinalDigest, err := grouped.OrdinalProgram.Digest()
	if err != nil {
		return "", fmt.Errorf("compiler identity grouped relational ordinal program does not digest: %w", err)
	}
	hash := sha256.New()
	writeIdentityField(hash, "domain", compilerIdentityDomain)
	writeIdentityField(hash, "compiler_version", CompilerVersion)
	writeIdentityField(hash, "normal_form_version", NormalFormVersion)
	writeIdentityField(hash, "normal_form_version_v4", NormalFormVersionV4)
	writeIdentityField(hash, "ordinal_program_version", OrdinalProgramVersion)
	writeIdentityField(hash, "predicate_footprint_version", PredicateFootprintVersion)
	writeIdentityField(hash, "probe_sql", compiled)
	writeIdentityField(hash, "probe_normal_form_v2", normalV2Digest)
	writeIdentityField(hash, "probe_normal_form_v4", normalV4Digest)
	writeIdentityField(hash, "probe_relational_visible_sql", relational.VisibleSQL)
	writeIdentityField(hash, "probe_relational_provenance_sql", relational.ProvenanceSQL)
	writeIdentityField(hash, "probe_predicate_footprint", string(footprintBytes))
	writeIdentityField(hash, "probe_grouped_relational_visible_sql", grouped.VisibleSQL)
	writeIdentityField(hash, "probe_grouped_relational_provenance_sql", grouped.ProvenanceSQL)
	writeIdentityField(hash, "probe_grouped_relational_semantic_v4", groupedSemantic.SHA256)
	writeIdentityField(hash, "probe_grouped_relational_ordinal_program", string(groupedOrdinalJSON))
	writeIdentityField(hash, "probe_grouped_relational_ordinal_program_sha256", groupedOrdinalDigest)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// writeIdentityField frames each member as name, length, value so no two member
// sets can concatenate to one digest input.
func writeIdentityField(hash interface{ Write([]byte) (int, error) }, name, value string) {
	fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", name, len(value), value)
}
