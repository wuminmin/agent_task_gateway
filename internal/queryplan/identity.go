package queryplan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// CompilerVersion names the compiler a prepared statement was produced by.
//
// It is part of the signed Query Execution Binding: a plan digest says what was
// asked for, and the compiler identity says what turned it into SQL. Two
// compilers can agree on a plan and disagree on the statement, and without this
// the executed bytes would move for a reason nothing recorded.
const CompilerVersion = "taskgate-query-compiler-v5"

const compilerIdentityDomain = "TASKGATE-QUERY-COMPILER-IDENTITY-V1"

// CompilerSHA256 is the compiler's behavioural identity.
//
// It is computed rather than declared, and it is computed from what the compiler
// actually does: a frozen probe plan is compiled, normalized and footprinted,
// and the outputs are hashed together with the frozen contract versions. A
// change to the emitted SQL, to the normal form, or to the predicate footprint
// therefore moves this digest even when every version constant is untouched --
// which is exactly the case a declared constant would miss.
//
// TestCompilerIdentityIsPinnedToItsSource keeps the value honest by pinning it;
// the failure tells the reader to bump CompilerVersion deliberately rather than
// to update the expectation.
func CompilerSHA256() (string, error) { return compilerIdentity() }

var compilerIdentity = sync.OnceValues(computeCompilerIdentity)

// compilerIdentityProbe is deliberately small and deliberately frozen. It
// exercises projection, aggregation, filtering, grouping, ordering and paging --
// the constructs whose rendering the executed statement depends on.
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
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// writeIdentityField frames each member as name, length, value so no two member
// sets can concatenate to one digest input.
func writeIdentityField(hash interface{ Write([]byte) (int, error) }, name, value string) {
	fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", name, len(value), value)
}
