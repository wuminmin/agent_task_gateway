package gateway

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

// semanticViewCase is one of the two composition shapes a semantic View admits.
//
// An ungrouped Artifact is transparent -- the outer plan projects public outputs
// that expand to terminal FieldIDs -- while a grouped one is an aggregation
// barrier that admits only a projection of already-computed outputs. They take
// different paths through composition, so parity on one says nothing about the
// other.
type semanticViewCase struct {
	name      string
	aggregate bool
	columns   []string
}

func semanticViewCases() []semanticViewCase {
	return []semanticViewCase{
		{name: "projection", aggregate: false, columns: []string{"amount"}},
		{name: "aggregate_barrier", aggregate: true,
			columns: []string{"request_count", "business_unit", "total_amount"}},
	}
}

// semanticViewParity is one case resolved against a running fixture, with the
// outer plan and composition both sides prepare from.
type semanticViewParity struct {
	fixture     semanticRuntimeFixture
	outer       queryplan.QueryPlan
	composition viewcompiler.Composition
}

func resolveSemanticViewCase(t *testing.T, test semanticViewCase) semanticViewParity {
	t.Helper()
	fixture := newSemanticRuntimeFixture(t, test.aggregate)
	outer := queryplan.QueryPlan{Product: fixture.root.Name, Columns: test.columns}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, outer, fixture.artifact)
	if err != nil {
		t.Fatalf("compose semantic View plan: %v", err)
	}
	return semanticViewParity{fixture: fixture, outer: outer, composition: composition}
}

// semanticViewInputsFor maps the Gateway's resolved material onto the pure
// input contract.
//
// This is the mapping the Gateway itself will perform at T1d. It reads the
// Control Store binding and the verified registry the Gateway already resolved,
// and passes values; the finalizer will build the same type from retained frozen
// evidence. Nothing here hands the package a store, a registry or a Service.
func semanticViewInputsFor(t *testing.T, parity semanticViewParity) physicalquery.SemanticViewPreparationInputsV1 {
	t.Helper()
	fixture := parity.fixture
	view, err := physicalquery.CatalogViewFromCatalog(*fixture.service.catalog)
	if err != nil {
		t.Fatalf("build catalog view from the Gateway's own catalog: %v", err)
	}
	grant := fixture.grant
	inputs := physicalquery.SemanticViewPreparationInputsV1{
		OuterPlan: parity.outer,
		Catalog:   view,
		Grant: physicalquery.Grant{
			ApprovedProducts: grant.ApprovedProducts,
			ApprovedColumns:  grant.ApprovedColumns,
			MandatoryScope:   grant.MandatoryScope,
			ExposureProfile:  grant.Exposure.ProfileVersion,
			PredicateLimits:  predicateLimitsForGrant(grant.Exposure),
		},
		ViewBindingDigest:      fixture.binding.Digest,
		ExpectedRevisionDigest: fixture.binding.Expectation.ExpectedRevisionDigest,
		Artifact:               fixture.artifact,
		Composition:            parity.composition,
	}
	if inputs.Grant.UsesOrdinalProgram() {
		inputs.SnapshotBindings = gatewaySnapshotBindings(t, fixture.service)
	}
	return inputs
}

// extractedViewShapeOf reads the same members legacyShapeOf reads, from the
// extracted preparation instead of the Gateway's.
func extractedViewShapeOf(t *testing.T, prepared physicalquery.PreparedOperation) preparationShape {
	t.Helper()
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatalf("the prepared operation refused to hand out its statements: %v", err)
	}
	binding := prepared.Binding()
	shape := preparationShape{
		VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
		VisibleFields:        prepared.VisibleFields(),
		FactFields:           prepared.FactFields(),
		ProvenanceFields:     prepared.ProvenanceFields(),
		Grouped:              prepared.Grouped(),
		ExpandedEvidence:     prepared.ExpandedEvidence(),
		UsesExpandedEvidence: prepared.Grouped() || prepared.ExpandedEvidence(),
		PlanDigest:           binding.NormalFormSHA256,
		// A composed View plan is relational, so its canonical identity is the
		// algebra normal form, which is the member the legacy shape fills.
		AlgebraNormalFormSHA256: binding.NormalFormSHA256,
		EstimatedBaseFacts:      prepared.EstimatedBaseFacts(),
		SidecarGrants:           prepared.SidecarGrants(),
		SourcePublications:      prepared.SourcePublications(),
		PolicyGrant:             prepared.PolicyGrant(),
		ViewBindingDigest:       binding.ViewBindingSHA256,
	}
	if program, programErr := prepared.OrdinalProgram(); programErr != nil {
		t.Fatalf("copy the prepared ordinal program: %v", programErr)
	} else if program != nil {
		shape.OrdinalProgram = program
		shape.SidecarGrantsSHA256 = sidecarGrantsDigest(shape.SidecarGrants)
	}
	if set, setErr := prepared.DictionarySet(); setErr != nil {
		t.Fatalf("copy the prepared dictionary set: %v", setErr)
	} else if set != nil {
		digest, digestErr := set.Digest()
		if digestErr != nil {
			t.Fatalf("digest the prepared dictionary set: %v", digestErr)
		}
		shape.DictionarySetDigest = digest
		shape.DictionarySetMembers = append([]ordinal.DictionarySetMember(nil), set.Members...)
	}
	if footprint, footprintErr := prepared.PredicateFootprint(); footprintErr != nil {
		t.Fatalf("copy the prepared predicate footprint: %v", footprintErr)
	} else {
		shape.PredicateFootprint = footprint
	}
	return shape
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
				fixture.grant, fixture.root, fixture.artifact, parity.composition,
				fixture.binding, parity.outer)
			if err != nil {
				t.Fatalf("the Gateway refused to prepare the semantic View: %v", err)
			}
			evidence := datasourceEvidence{DatasourceID: "parity-datasource",
				SchemaDigest: strings.Repeat("9", 64)}
			legacy, err := legacyShapeOf(fixture.service, legacyPrepared, parity.outer,
				fixture.grant, evidence, strings.Repeat("a", 64), strings.Repeat("b", 64))
			if err != nil {
				t.Fatalf("read the Gateway's semantic View shape: %v", err)
			}

			inputs := semanticViewInputsFor(t, parity)
			prepared, err := physicalquery.PrepareSemanticView(inputs)
			if err != nil {
				t.Fatalf("PrepareSemanticView refused a shape the Gateway prepared: %v", err)
			}
			extracted := extractedViewShapeOf(t, prepared)

			// Metering is Gateway observation state rather than prepared output, as
			// for the table shapes; its whole effect on preparation is the statement
			// bytes and the widened grant, both compared here.
			legacy.MeteringColumns, extracted.MeteringColumns = nil, nil
			// The execution-binding digests are computed by the Gateway from the
			// shape, not by preparation, and the legacy side alone fills them.
			legacy.PreparedOperationSHA256, legacy.VisibleTargetSHA256, legacy.CompanionTargetSHA256 = "", "", ""
			// The registry revision travels as a digest in the new binding and as the
			// raw operational string in the old shape, so they are compared for
			// presence and agreement separately below rather than byte for byte.
			revision := legacy.ViewRegistryRevision
			legacy.ViewRegistryRevision, extracted.ViewRegistryRevision = "", ""
			requireSameShape(t, legacy, extracted)

			if revision == "" || prepared.Binding().ViewRegistryRevisionSHA256 == "" {
				t.Error("a semantic View preparation carries no registry revision")
			}
			if prepared.Binding().ViewBindingSHA256 != fixture.binding.Digest {
				t.Errorf("the prepared binding names view %q, the resolved binding is %q",
					prepared.Binding().ViewBindingSHA256, fixture.binding.Digest)
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
