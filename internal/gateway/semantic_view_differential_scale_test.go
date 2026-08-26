//go:build taskgate_scale

// These cases prepare an ordinal-program plan, and preparation resolves every
// snapshot publication the Catalog declares (preparation_inputs.go:180). Five of
// the seven are scanned out of the Business database, which measured 25.84 GB
// peak on a 30 GB host, so they belong on the taskgate_scale lane rather than
// holding the acceptance run open.

package gateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/mcp"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

func TestSemanticViewPredicateFootprintIgnoresScopeJSONEncoding(t *testing.T) {
	var selected semanticViewCase
	for _, test := range semanticViewCases() {
		if test.name == "projection_v5_filtered" {
			selected = test
			break
		}
	}
	if selected.name == "" {
		t.Fatal("the semantic View V5 filtered fixture is missing")
	}
	inputs := semanticViewInputsFor(t, resolveSemanticViewCase(t, selected))
	var decoded any
	if err := json.Unmarshal(inputs.Grant.MandatoryScope, &decoded); err != nil {
		t.Fatalf("decode semantic View mandatory scope: %v", err)
	}
	compact, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("compact semantic View mandatory scope: %v", err)
	}
	spaced, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatalf("indent semantic View mandatory scope: %v", err)
	}
	if string(compact) == string(spaced) {
		t.Fatal("semantic View scope fixtures are not byte-distinct")
	}
	compactInputs, spacedInputs := inputs, inputs
	compactInputs.Grant.MandatoryScope = compact
	spacedInputs.Grant.MandatoryScope = spaced
	compactPrepared, err := physicalquery.PrepareSemanticView(compactInputs)
	if err != nil {
		t.Fatalf("prepare semantic View with compact scope: %v", err)
	}
	spacedPrepared, err := physicalquery.PrepareSemanticView(spacedInputs)
	if err != nil {
		t.Fatalf("prepare semantic View with spaced scope: %v", err)
	}
	if err := compactPrepared.Binding().RequireSame(spacedPrepared.Binding()); err != nil {
		t.Fatalf("semantic View full prepared bindings differ by scope encoding: %v", err)
	}
	compactFootprint, err := compactPrepared.PredicateFootprint()
	if err != nil {
		t.Fatalf("read compact semantic View footprint: %v", err)
	}
	spacedFootprint, err := spacedPrepared.PredicateFootprint()
	if err != nil {
		t.Fatalf("read spaced semantic View footprint: %v", err)
	}
	if !reflect.DeepEqual(compactFootprint, spacedFootprint) {
		t.Fatalf("semantic View footprint differs by scope encoding\ncompact: %+v\nspaced:  %+v",
			compactFootprint, spacedFootprint)
	}
}

// TestSemanticViewPredicateLimitMutationsFailClosed covers the one member the
// structural mutations cannot reach.
//
// Predicate limits only bind under V5, which needs the snapshot registry and so
// the database. A limit the footprint exceeds must be a refusal rather than a
// footprint quietly prepared outside it, and a limit that merely differs must
// still move the binding, since the limits are part of the grant identity.
func TestSemanticViewPredicateLimitMutationsFailClosed(t *testing.T) {
	parity := resolveSemanticViewCase(t, semanticViewCase{
		name: "projection_v5_filtered", columns: []string{"amount"}, profile: exposure.ProfileV5,
		filters: []queryplan.Filter{{Column: "business_unit", Op: "=", Value: "销售部"}},
	})
	baseline, err := physicalquery.PrepareSemanticView(semanticViewInputsFor(t, parity))
	if err != nil {
		t.Fatalf("baseline V5 preparation: %v", err)
	}
	if footprint, _ := baseline.PredicateFootprint(); footprint == nil {
		t.Fatal("the V5 baseline built no predicate footprint, so the limits bind nothing")
	}

	t.Run("payload limit the footprint exceeds", func(t *testing.T) {
		inputs := semanticViewInputsFor(t, parity)
		inputs.Grant.PredicateLimits.MaxAtomPayloadBytes = 8
		if _, err := physicalquery.PrepareSemanticView(inputs); err == nil {
			t.Fatal("a footprint exceeding the atom payload limit was prepared rather than refused")
		}
	})
	t.Run("a wider limit still moves the binding", func(t *testing.T) {
		inputs := semanticViewInputsFor(t, parity)
		inputs.Grant.PredicateLimits.MaxTotalAtomPayloadBytes *= 2
		prepared, err := physicalquery.PrepareSemanticView(inputs)
		if err != nil {
			t.Fatalf("a wider limit was refused: %v", err)
		}
		if prepared.Binding().SHA256 == baseline.Binding().SHA256 {
			t.Fatal("changing a predicate limit did not move the sealed binding")
		}
	})
}

// TestSemanticViewPreparationMatchesTheGateway is the T1c-S4 differential.
//
// Both composition shapes are prepared twice -- once by the Gateway's own
// prepareSemanticViewPlan and once by physicalquery.PrepareSemanticView -- and
// every prepared member is compared, including the exact visible and companion
// statement bytes. Parity on the digests alone would not be parity: two
// statements can agree on a normal form and still differ in what they select.
func TestSemanticViewPreparationMatchesTheGateway(t *testing.T) {
	for _, test := range semanticViewCases() {
		t.Run(test.name, func(t *testing.T) {
			parity := resolveSemanticViewCase(t, test)
			fixture := parity.fixture

			legacyPrepared, err := fixture.service.prepareSemanticViewPlan(
				fixture.grant, fixture.artifact, parity.composition,
				fixture.binding, parity.outer)
			if err != nil {
				if toolErr, ok := err.(*mcp.ToolError); ok {
					t.Fatalf("the Gateway refused to prepare the semantic View: %v details=%v", err, toolErr.Details)
				}
				t.Fatalf("the Gateway refused to prepare the semantic View: %v", err)
			}
			production, err := productionShapeOf(legacyPrepared, parity.composition.Plan)
			if err != nil {
				t.Fatalf("read the Gateway's semantic View shape: %v", err)
			}

			inputs := semanticViewInputsFor(t, parity)
			prepared, err := physicalquery.PrepareSemanticView(inputs)
			if err != nil {
				t.Fatalf("PrepareSemanticView refused a shape the Gateway prepared: %v", err)
			}
			extracted := extractedViewShapeOf(t, prepared)

			// The registry revision is Gateway-only state: the binding carries a
			// digest of it, because an operational string is not evidence, so the
			// two are compared for presence and agreement below rather than byte for
			// byte.
			revision := production.ViewRegistryRevision
			production.ViewRegistryRevision, extracted.ViewRegistryRevision = "", ""
			requireSameShape(t, production, extracted)

			if revision == "" || prepared.Binding().ViewRegistryRevisionSHA256 == "" {
				t.Error("a semantic View preparation carries no registry revision")
			}
			if prepared.Binding().ViewBindingSHA256 != fixture.binding.Digest {
				t.Errorf("the prepared binding names view %q, the resolved binding is %q",
					prepared.Binding().ViewBindingSHA256, fixture.binding.Digest)
			}

			// The profile-selected halves must actually have run. Without this the
			// V4 and V5 cases could agree by both preparing nothing, and the
			// ordinal binding and predicate construction would be reported as
			// covered while never having been exercised.
			if test.profile != exposure.ProfileV2 {
				if extracted.OrdinalProgram == nil {
					t.Error("the V4-onward case compiled no ordinal program, so the comparison covered none")
				}
				if extracted.DictionarySetDigest == "" || len(extracted.SidecarGrants) == 0 {
					t.Error("the V4-onward case bound no dictionary set or sidecar grant")
				}
			}
			if test.profile == exposure.ProfileV5 {
				if extracted.PredicateFootprint == nil {
					t.Fatal("the V5 case built no predicate footprint, so the comparison covered none")
				}
				if len(test.filters) > 0 && extracted.PredicateFootprint.UniqueAtomCount == 0 {
					t.Error("the filtered V5 case produced a footprint with no atoms, " +
						"so the outer-plan filter never reached the predicate binding")
				}
			}
		})
	}
}

// TestSemanticViewTerminalProductsStayPrivate holds the authorization boundary
// the extraction must not have loosened.
//
// The terminal Products the compiled View reads must be in the internal policy
// grant, because the statements are executed against them; they must never be in
// the task's public ApprovedProducts, because that list is what the agent's own
// plan is compiled and authorized against, and a terminal Product there would
// let the agent read the underlying relation directly instead of through the
// View.
func TestSemanticViewTerminalProductsStayPrivate(t *testing.T) {
	for _, test := range semanticViewCases() {
		t.Run(test.name, func(t *testing.T) {
			parity := resolveSemanticViewCase(t, test)
			terminal := parity.fixture.terminal.Name

			prepared, err := physicalquery.PrepareSemanticView(semanticViewInputsFor(t, parity))
			if err != nil {
				t.Fatalf("PrepareSemanticView: %v", err)
			}
			granted := map[string]bool{}
			for _, product := range prepared.PolicyGrant().Products {
				granted[product.LogicalName] = true
			}
			if !granted[terminal] {
				t.Errorf("the internal policy grant does not admit terminal product %q, "+
					"so the expanded statement could not be authorized", terminal)
			}
			// The root stays in the grant. The task was approved against the root
			// relation, and the Gateway admits statements against the task grant
			// extended by the terminal closure, so dropping the root here would
			// narrow what production authorizes. The boundary is the direction that
			// matters: the terminal must not travel outward into the public list.
			if !granted[parity.fixture.root.Name] {
				t.Errorf("the policy grant no longer admits the approved View root %q", parity.fixture.root.Name)
			}
			if contains(parity.fixture.grant.ApprovedProducts, terminal) {
				t.Errorf("terminal product %q reached the task's public approved products", terminal)
			}

			// And the refusal is real: a grant that does list the terminal Product
			// must fail closed rather than prepare a wider surface.
			widened := semanticViewInputsFor(t, parity)
			widened.Grant.ApprovedProducts = append(
				append([]string(nil), widened.Grant.ApprovedProducts...), terminal)
			if _, err := physicalquery.PrepareSemanticView(widened); err == nil {
				t.Error("a task grant listing a terminal Product publicly was accepted")
			}
		})
	}
}
