package finalv5contracts

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The Gateway cannot prepare a UNION DISTINCT whose branches carry filters under
// exposure profile V5.
//
// queryplan.PredicateBindings keys its products by product NAME, while a UNION
// branch qualifies its columns by branch ROLE, so a branch filter on
// `partition_key` is looked up as `left_branch.partition_key` and resolves to
// nothing. The preparation fails closed with POLICY_DENIED; V4, which accounts
// no predicate footprint, prepares the same plan. This is pinned from the other
// side by internal/gateway's
// TestUnionBranchFilteredPredicatesFailClosedUnderV5.
//
// The refusal is correct -- a footprint that silently omitted the branch
// predicates would under-count the atoms the query reveals -- but it is a
// capability boundary, and a frozen contract may not depend on a shape the
// runtime cannot execute. This file is what connects the two: it reads the
// frozen cells and says exactly which of them do.

// unionDependentCell is one frozen cell whose BDG arm submits a QueryPlan
// carrying a branch-filtered UNION DISTINCT.
type unionDependentCell struct {
	Contract string
	Workload string
	Scale    string
	Mode     string
	Products []string
	Template string
}

func (found unionDependentCell) String() string {
	return fmt.Sprintf("%s/%s/%s/%s (products %s, %s)",
		found.Contract, found.Workload, found.Scale, found.Mode,
		strings.Join(found.Products, "+"), found.Template)
}

// branchFilteredUnionCells finds every cell whose active BDG arm submits a plan
// with a filtered UNION DISTINCT branch.
//
// Only active BDG arms are considered. A `direct` arm is raw PostgreSQL and does
// not reach the Gateway's preparation at all, so an inactive BDG arm on the same
// cell is not a dependency on the unsupported shape.
func branchFilteredUnionCells(t *testing.T) []unionDependentCell {
	t.Helper()
	root := repositoryRootForTest(t)
	contractDir := filepath.Join(root, "evaluation", "final-v5-wsl2")

	type source struct {
		name  string
		cells []cell
	}
	baseline, _, err := loadBaseline(filepath.Join(contractDir, "contracts", "baseline-v1.json"))
	if err != nil {
		t.Fatalf("load baseline contract: %v", err)
	}
	scale, _, err := loadScale(filepath.Join(contractDir, "contracts", "scale-v1.json"))
	if err != nil {
		t.Fatalf("load scale contract: %v", err)
	}
	artifact, _, err := loadArtifact(filepath.Join(contractDir, "contracts", "artifact-v1.json"))
	if err != nil {
		t.Fatalf("load artifact contract: %v", err)
	}

	var found []unionDependentCell
	for _, contract := range []source{
		{"baseline", baseline.Cells}, {"scale", scale.Cells}, {"artifact", artifact.Cells},
	} {
		for _, entry := range contract.cells {
			var arm struct {
				Active     bool   `json:"active"`
				Entrypoint string `json:"entrypoint"`
				Template   string `json:"template"`
			}
			if err := json.Unmarshal(entry.BDG, &arm); err != nil {
				t.Fatalf("decode BDG arm of %s/%s/%s/%s: %v",
					contract.name, entry.Workload, entry.Scale, entry.Mode, err)
			}
			if !arm.Active || arm.Entrypoint != "execute_plan" || arm.Template == "" {
				continue
			}
			if !strings.HasSuffix(arm.Template, ".json") {
				continue
			}
			if !planTemplateHasBranchFilteredUnion(t, filepath.Join(contractDir, arm.Template)) {
				continue
			}
			var product struct {
				IDs []string `json:"ids"`
			}
			if err := json.Unmarshal(entry.Product, &product); err != nil {
				t.Fatalf("decode product of %s/%s/%s/%s: %v",
					contract.name, entry.Workload, entry.Scale, entry.Mode, err)
			}
			found = append(found, unionDependentCell{
				Contract: contract.name, Workload: entry.Workload, Scale: entry.Scale,
				Mode: entry.Mode, Products: product.IDs, Template: arm.Template,
			})
		}
	}
	sort.Slice(found, func(left, right int) bool {
		return found[left].String() < found[right].String()
	})
	return found
}

// planTemplateHasBranchFilteredUnion reports whether a plan template's
// union_distinct carries a filter on either branch.
//
// The template is decoded generically rather than into queryplan.QueryPlan: a
// template holds unrendered `$parameter` objects where a plan holds literals, so
// it does not decode as a plan, and the question here is structural.
func planTemplateHasBranchFilteredUnion(t *testing.T, path string) bool {
	t.Helper()
	// Every member the template schema carries is named, so the strict decode
	// still rejects one this test does not know about. A new plan-shape member --
	// another set operation, another branch container -- must fail here rather
	// than be skipped as "no union found".
	var template struct {
		TemplateSchemaVersion int             `json:"template_schema_version"`
		Entrypoint            string          `json:"entrypoint"`
		RenderRule            string          `json:"render_rule"`
		Parameters            json.RawMessage `json:"parameters"`
		Plan                  struct {
			Columns []string `json:"columns"`
			From    struct {
				UnionDistinct *struct {
					Role    string         `json:"role"`
					Columns []string       `json:"columns"`
					Left    map[string]any `json:"left"`
					Right   map[string]any `json:"right"`
				} `json:"union_distinct"`
			} `json:"from"`
		} `json:"plan"`
	}
	if _, err := decodeStrictJSONFile(path, &template); err != nil {
		// A template that is not a plan template -- a raw SQL file reached through
		// a .json suffix, say -- is not a dependency on this shape. Anything else
		// is a contract the harness cannot read, which must not pass silently.
		t.Fatalf("decode plan template %s: %v", path, err)
	}
	union := template.Plan.From.UnionDistinct
	if union == nil {
		return false
	}
	for _, branch := range []map[string]any{union.Left, union.Right} {
		filters, present := branch["filters"]
		if !present {
			continue
		}
		if values, ok := filters.([]any); ok && len(values) > 0 {
			return true
		}
	}
	return false
}

// No Artifact cell, and no Scale dependency-e2e cell, may depend on the
// unsupported shape.
//
// These are the two experiment paths the Artifact mainline rests on. If either
// depended on a branch-filtered V5 union, the v3 cutover would be blocked on the
// qualified-column work rather than merely accompanied by it.
func TestNoArtifactOrScaleCellDependsOnBranchFilteredUnion(t *testing.T) {
	for _, found := range branchFilteredUnionCells(t) {
		if found.Contract == "artifact" {
			t.Errorf("Artifact cell %s submits a branch-filtered UNION DISTINCT, "+
				"which no V5 preparation can build a predicate footprint for", found)
		}
		if found.Contract == "scale" && found.Workload == "dependency-e2e" {
			t.Errorf("Scale dependency-e2e cell %s submits a branch-filtered UNION DISTINCT, "+
				"which no V5 preparation can build a predicate footprint for", found)
		}
	}
}

// The set of cells that DO depend on it is pinned exactly.
//
// This is the half that matters. Asserting only that Artifact and Scale are
// clean would let the dependency spread into any other cell without notice, and
// would leave the affected set recorded nowhere. Baseline S5 is the whole of it
// today: six active BDG arms over provsql_orders, all still
// PENDING_IMPLEMENTATION, so nothing has been measured against a shape the
// runtime refuses.
//
// provsql_orders routes to budget profile final-v5-provsql-low-v1, which is
// taskgate-exposure-v5, so these arms cannot execute as written. That is a real
// obligation on the Final-V5 baseline and it is recorded in
// docs/final_v5_artifact_autonomous_status.md rather than left in a test.
//
// When the qualified-column work lands, this list should become empty and the
// test below should be deleted along with the limitation it records.
func TestTheBranchFilteredUnionDependencySetIsExactlyBaselineS5(t *testing.T) {
	want := map[string]bool{
		"baseline/S5/SF1/novel":              true,
		"baseline/S5/SF1/semantic_replay":    true,
		"baseline/S5/SF1/idempotent_replay":  true,
		"baseline/S5/SF10/novel":             true,
		"baseline/S5/SF10/semantic_replay":   true,
		"baseline/S5/SF10/idempotent_replay": true,
	}
	got := map[string]bool{}
	for _, found := range branchFilteredUnionCells(t) {
		key := fmt.Sprintf("%s/%s/%s/%s", found.Contract, found.Workload, found.Scale, found.Mode)
		got[key] = true
		if !want[key] {
			t.Errorf("cell %s newly depends on the branch-filtered UNION DISTINCT shape, "+
				"which no V5 preparation can prepare", found)
		}
		for _, product := range found.Products {
			if !strings.HasPrefix(product, "provsql_") {
				t.Errorf("cell %s depends on the unsupported shape over product %q; "+
					"the recorded dependency is ProvSQL-only", found, product)
			}
		}
	}
	for key := range want {
		if !got[key] {
			t.Errorf("cell %s no longer depends on the branch-filtered UNION DISTINCT shape; "+
				"if the qualified-column work landed, empty this list, delete this test, and "+
				"promote internal/gateway's V5 Union parity case back to the filtered plan", key)
		}
	}
}
