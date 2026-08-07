package gateway

import (
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

// semanticViewCase is one of the two composition shapes a semantic View admits.
//
// An ungrouped Artifact is transparent -- the outer plan projects public outputs
// that expand to terminal FieldIDs -- while a grouped one is an aggregation
// barrier that admits only a projection of already-computed outputs. They take
// different paths through composition, so parity on one says nothing about the
// other.
// The profile is part of the case because it selects which halves of the moved
// derivation run at all: V2 compiles neither an ordinal program nor a predicate
// footprint, V4 compiles the program and binds sidecars, and only V5 builds the
// footprint from the outer plan's filters. Comparing the V2 shapes alone would
// have left the ordinal binding and the whole V5 predicate construction --
// which is most of what T1c-S2 moved -- unexercised.
type semanticViewCase struct {
	name      string
	aggregate bool
	columns   []string
	profile   string
	// filters are the agent's own outer-plan filters, which under V5 become the
	// predicate footprint's caller bindings.
	filters []queryplan.Filter
}

func semanticViewCases() []semanticViewCase {
	projection := []string{"amount"}
	barrier := []string{"request_count", "business_unit", "total_amount"}
	unitFilter := []queryplan.Filter{{Column: "business_unit", Op: "=", Value: "销售部"}}
	return []semanticViewCase{
		{name: "projection", columns: projection, profile: exposure.ProfileV2},
		{name: "aggregate_barrier", aggregate: true, columns: barrier, profile: exposure.ProfileV2},
		{name: "projection_v4", columns: projection, profile: exposure.ProfileV4},
		{name: "aggregate_barrier_v4", aggregate: true, columns: barrier, profile: exposure.ProfileV4},
		{name: "projection_v5_filtered", columns: projection, profile: exposure.ProfileV5,
			filters: unitFilter},
		{name: "aggregate_barrier_v5", aggregate: true, columns: barrier, profile: exposure.ProfileV5},
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
	if test.profile != exposure.ProfileV2 {
		// V4 onward the terminal Product must resolve to a verified immutable
		// snapshot publication, so the Gateway needs its registry.
		installSemanticRuntimeSnapshotRegistry(t, fixture.service)
	}
	fixture.grant.Exposure.ProfileVersion = test.profile
	if test.profile == exposure.ProfileV5 {
		fixture.grant.Exposure.PredicateFootprint = &control.PredicateFootprintLimitsV1{
			MaxRawLiteralsPerQuery: 64, MaxUniqueAtomsPerQuery: 64,
			MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 65536,
		}
	}
	outer := queryplan.QueryPlan{Product: fixture.root.Name, Columns: test.columns,
		Filters: append([]queryplan.Filter(nil), test.filters...)}
	composition, err := viewcompiler.ComposeQueryPlan(fixture.root.Name, outer, fixture.artifact)
	if err != nil {
		t.Fatalf("compose semantic View plan: %v", err)
	}
	return semanticViewParity{fixture: fixture, outer: outer, composition: composition}
}

// semanticViewInputsFor is the production mapping, called rather than repeated.
//
// While this harness owned its own copy, a passing comparison proved that
// PrepareSemanticView agreed with the Gateway given inputs the harness built --
// which is not the claim anyone wanted, and which stopped being true the moment
// production began pinning the grant to the View. What matters is agreement
// given the inputs production actually builds, and the only way to assert that
// is to build them the same way production does.
func semanticViewInputsFor(t *testing.T, parity semanticViewParity) physicalquery.SemanticViewPreparationInputsV1 {
	t.Helper()
	fixture := parity.fixture
	inputs, err := fixture.service.semanticViewPreparationInputs(
		fixture.grant, fixture.artifact, parity.composition, fixture.binding, parity.outer)
	if err != nil {
		t.Fatalf("build semantic View preparation inputs: %v", err)
	}
	return inputs
}

// extractedViewShapeOf reads the same members productionShapeOf reads, from an
// independently prepared operation instead of from the Gateway's context.
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
	shape.PreparedOperationSHA256 = binding.SHA256
	visible, err := binding.TargetSHA256(preparedbinding.RoleVisible)
	if err != nil {
		t.Fatalf("read the prepared visible target: %v", err)
	}
	shape.VisibleTargetSHA256 = visible
	if binding.HasCompanion {
		companion, companionErr := binding.TargetSHA256(preparedbinding.RoleCompanion)
		if companionErr != nil {
			t.Fatalf("read the prepared companion target: %v", companionErr)
		}
		shape.CompanionTargetSHA256 = companion
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

// viewMutation is one change to a semantic View preparation's inputs, together
// with the outcome it must produce.
//
// The outcome is declared, not "either of two things". A mutation asserted to
// move the binding must actually prepare and produce a different sealed digest,
// and one asserted to fail closed must be refused. Accepting either would make
// the case vacuous: a mutation that happened to be rejected earlier for an
// unrelated reason would pass while proving nothing about the member it names.
type viewMutation struct {
	name string
	// failsClosed is true when preparation must refuse the mutated inputs, and
	// false when it must prepare something with a different sealed binding.
	failsClosed bool
	apply       func(*physicalquery.SemanticViewPreparationInputsV1)
}

// mutateProduct rewrites one Catalog product inside the prepared view.
func mutateProduct(inputs *physicalquery.SemanticViewPreparationInputsV1,
	name string, change func(p *catalog.Product)) {
	product := inputs.Catalog.Products[name]
	change(&product)
	products := make(map[string]catalog.Product, len(inputs.Catalog.Products))
	for key, value := range inputs.Catalog.Products {
		products[key] = value
	}
	products[name] = product
	inputs.Catalog.Products = products
}

func mutateScope(inputs *physicalquery.SemanticViewPreparationInputsV1,
	name string, change func(s *catalog.Scope)) {
	scopes := append([]catalog.Scope(nil), inputs.Catalog.Scopes...)
	for index := range scopes {
		if scopes[index].Name == name {
			change(&scopes[index])
		}
	}
	inputs.Catalog.Scopes = scopes
}

func semanticViewMutations(terminal string) []viewMutation {
	return []viewMutation{
		// These two fail closed rather than moving the binding, and that is the
		// stronger outcome. Production pins the grant to the View it was resolved
		// against, so changing the View the inputs name while the grant still names
		// the old one is not a different preparation -- it is a grant and a View
		// that disagree, which preparation refuses rather than reconciles. Their
		// effect on the sealed binding is covered by the artifact, composition and
		// registry-revision members below, which move it.
		{name: "view binding digest", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				i.ViewBindingDigest = strings.Repeat("1", 64)
			}},
		{name: "expected registry revision", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				i.ExpectedRevisionDigest = strings.Repeat("2", 64)
			}},
		// The coherent version of the two above: a task bound to a DIFFERENT View,
		// with the grant and the inputs agreeing about which. That is a real
		// operation, and it must not share an identity with this one.
		{name: "the view the grant and the inputs both name",
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				i.ViewBindingDigest = strings.Repeat("1", 64)
				i.Grant.ViewBindingDigest = strings.Repeat("1", 64)
			}},
		{name: "the registry revision the grant and the inputs both name",
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				i.ExpectedRevisionDigest = strings.Repeat("2", 64)
				i.Grant.ViewRegistryRevision = strings.Repeat("2", 64)
			}},
		{name: "artifact canonical plan digest", apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
			i.Artifact.CanonicalPlanDigest = strings.Repeat("3", 64)
		}},
		{name: "artifact interface digest", apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
			i.Artifact.InterfaceDigest = strings.Repeat("4", 64)
		}},
		{name: "composition visible field name", apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
			fields := append([]string(nil), i.Composition.VisibleFields...)
			fields[0] = fields[0] + "_renamed"
			i.Composition.VisibleFields = fields
		}},
		{name: "artifact output mapping", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				outputs := append([]viewcompiler.Output(nil), i.Artifact.Outputs...)
				for index := range outputs {
					if outputs[index].Name == "business_unit" {
						outputs[index].FieldID = "nonexistent_role.nonexistent_column"
					}
				}
				i.Artifact.Outputs = outputs
			}},
		{name: "outer plan filter", apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
			i.OuterPlan.Filters = append(append([]queryplan.Filter(nil), i.OuterPlan.Filters...),
				queryplan.Filter{Column: "business_unit", Op: "=", Value: "研发部"})
		}},
		{name: "unrelated catalog scope policy", apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
			scopes := append([]catalog.Scope(nil), i.Catalog.Scopes...)
			scopes = append(scopes, catalog.Scope{Name: "zz_unrelated", Type: catalog.ScopeTypeEnum,
				AllowedValues: []string{"a", "b"}})
			i.Catalog.Scopes = scopes
		}},
		{name: "terminal product source", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				mutateProduct(i, terminal, func(p *catalog.Product) { p.Source = "another_source" })
			}},
		{name: "terminal product sensitivity", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				mutateProduct(i, terminal, func(p *catalog.Product) {
					p.Sensitivity = domain.SensitivityRestricted
				})
			}},
		{name: "terminal stable relation role", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				mutateProduct(i, terminal, func(p *catalog.Product) { p.StableRelationRole = "other_role" })
			}},
		{name: "propagated scope policy", failsClosed: true,
			apply: func(i *physicalquery.SemanticViewPreparationInputsV1) {
				mutateScope(i, "department", func(s *catalog.Scope) {
					s.AllowedValues = append(append([]string(nil), s.AllowedValues...), "zzz_extra")
				})
			}},
	}
}

// TestSemanticViewMutationsMoveTheBindingOrFailClosed is the T1c-S3 coverage
// proof and the mutation half of T1c-S4.
//
// It answers the question the spec refuses to let a field name answer: whether
// ViewBindingDigest transitively binds the compiled Artifact, the Composition,
// the outputs, the canonical plan digest, the terminal Product closure and the
// governance envelope. It does not. Holding the binding digest fixed and
// changing the Artifact or the Composition still moves the sealed
// PreparedOperationBindingV1, because those values are measured here rather than
// inferred, which is what the explicit ViewArtifactSHA256,
// ViewCompositionSHA256, TerminalProductClosureSHA256 and
// GovernanceEnvelopeSHA256 members exist for.
//
// The structural mutations run against the V2 projection, which needs no
// database; the predicate-limit mutation needs V5 and lives below.
func TestSemanticViewMutationsMoveTheBindingOrFailClosed(t *testing.T) {
	base := semanticViewCase{name: "projection", columns: []string{"amount"}, profile: exposure.ProfileV2}
	parity := resolveSemanticViewCase(t, base)
	terminal := parity.fixture.terminal.Name

	baseline, err := physicalquery.PrepareSemanticView(semanticViewInputsFor(t, parity))
	if err != nil {
		t.Fatalf("baseline preparation: %v", err)
	}
	sealed := baseline.Binding().SHA256

	for _, mutation := range semanticViewMutations(terminal) {
		t.Run(mutation.name, func(t *testing.T) {
			inputs := semanticViewInputsFor(t, parity)
			mutation.apply(&inputs)
			prepared, err := physicalquery.PrepareSemanticView(inputs)
			if mutation.failsClosed {
				if err == nil {
					t.Fatal("the mutation was accepted; it must fail closed")
				}
				return
			}
			if err != nil {
				t.Fatalf("the mutation was expected to move the binding, but preparation "+
					"was refused, so the case proves nothing about the member it names: %v", err)
			}
			if prepared.Binding().SHA256 == sealed {
				t.Fatal("the mutation did not move the sealed prepared operation binding")
			}
		})
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
