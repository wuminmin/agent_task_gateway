package sqlpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// RendererVersion names the renderer that produced an executable statement.
//
// The compiler decides what the statement means; this package decides what
// bytes are sent. A renderer change alters the executed statement without
// altering the plan, so the signed Query Execution Binding records both.
const RendererVersion = "taskgate-sqlpolicy-renderer-v3"

const rendererIdentityDomain = "TASKGATE-SQLPOLICY-RENDERER-IDENTITY-V1"

// RendererSHA256 is the renderer's behavioural identity.
//
// It digests what the renderer actually emits for a frozen probe grant rather
// than a constant somebody remembered to bump. Identifier quoting, literal
// escaping, predicate ordering, CTE framing and LIMIT placement all reach the
// executed bytes, and all of them move this digest.
//
// TestRendererIdentityIsPinnedToItsSource pins the value. A failure there means
// the rendered bytes changed: bump RendererVersion deliberately rather than
// updating the expectation, because every binding signed under the old value
// describes statements this renderer no longer produces.
func RendererSHA256() (string, error) { return rendererIdentity() }

var rendererIdentity = sync.OnceValues(computeRendererIdentity)

// rendererIdentityProbe exercises every construct the renderer can emit: two
// products so the CTE separator appears, quoted identifiers needing escape, and
// one predicate of each shape -- scalar, set-valued and null.
func rendererIdentityProbe() (referenced []string, products map[string]ProductGrant) {
	products = map[string]ProductGrant{
		"expense": {
			LogicalName: "expense", PhysicalSchema: "reporting", PhysicalView: "expense_v1",
			ApprovedColumns: []string{"month", "amount", "department"},
			MandatoryScope: []ScopePredicate{
				{Column: "department", Operator: ScopeIn, Values: []string{"sales", "o'brien"}},
				{Column: "amount", Operator: ScopeGreaterEqual, Values: []string{"0"}},
				{Column: "voided_at", Operator: ScopeIsNull},
			},
		},
		"headcount": {
			LogicalName: "headcount", PhysicalSchema: "reporting", PhysicalView: "head\"count",
			ApprovedColumns: []string{"month", "people"},
			MandatoryScope: []ScopePredicate{
				{Column: "region", Operator: ScopeNotEqual, Values: []string{"emea"}},
			},
		},
	}
	// Sorted, as Authorize passes them.
	return []string{"expense", "headcount"}, products
}

func computeRendererIdentity() (string, error) {
	referenced, products := rendererIdentityProbe()
	rendered, err := renderExecutable(
		`SELECT "month", sum("amount") AS "total" FROM "expense" GROUP BY "month"`,
		referenced, products, 137)
	if err != nil {
		return "", fmt.Errorf("renderer identity probe does not render: %w", err)
	}
	hash := sha256.New()
	writeRendererField(hash, "domain", rendererIdentityDomain)
	writeRendererField(hash, "renderer_version", RendererVersion)
	writeRendererField(hash, "probe_rendered", rendered)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeRendererField(hash interface{ Write([]byte) (int, error) }, name, value string) {
	fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", name, len(value), value)
}
