package gateway

import (
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// This began as T1b-B, the differential half of the parity harness, and became
// something else at T1d.
//
// While the Gateway had its own derivation this compared the two. It no longer
// does: production calls Prepare, so the comparison below is between what the
// production path EXPOSES and what an independent Prepare over the same inputs
// produces. That still fails a Gateway that drops a prepared member, reorders a
// projection, rebuilds a digest or hands the executor a grant preparation did
// not produce -- which is the whole of what the Gateway is still able to get
// wrong about a preparation.
//
// notYetExtracted is empty and the guard below keeps it that way: a shape that
// began refusing would be a shape production can no longer execute at all,
// because there is no second derivation left to fall back to.

// preparationInputsFor builds the immutable inputs Prepare reads.
//
// It calls the production mapping rather than reconstructing it. While this
// harness owned its own copy, a passing comparison proved that Prepare agreed
// with the Gateway's derivation given inputs the harness built -- which is not
// the claim anyone wanted. What matters is that Prepare agrees given the inputs
// production actually builds, and the only way to assert that is to build them
// the same way production does.
func preparationInputsFor(t *testing.T, service *Service, test parityCase) physicalquery.PreparationInputs {
	t.Helper()
	resolved := resolveParityCase(t, service, test)
	inputs, err := service.preparationInputs(resolved.grant(), resolved.plan)
	if err != nil {
		t.Fatalf("build preparation inputs: %v", err)
	}
	return inputs
}

// extractedShapeOf reads the same properties off an independently prepared
// operation.
//
// The three binding digests are the sealed preparation's own and its two
// prepared targets, which is what a Query Execution Binding V2 carries. Under V9
// they had to be recomputed through the Gateway's separate construction, because
// that construction was what the receipt signed; V2 signs the preparation
// itself, so there is one value and both sides read it.
func extractedShapeOf(t *testing.T, prepared physicalquery.PreparedOperation,
	test parityCase) preparationShape {
	t.Helper()
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatalf("the prepared operation refused to hand out its statements: %v", err)
	}
	binding := prepared.Binding()

	shape := preparationShape{
		VisibleSQL: visibleSQL, CompanionSQL: companionSQL,
		VisibleFields:    prepared.VisibleFields(),
		FactFields:       prepared.FactFields(),
		ProvenanceFields: prepared.ProvenanceFields(),
		Grouped:          prepared.Grouped(),
		ExpandedEvidence: prepared.ExpandedEvidence(),
		// usesExpandedEvidence is grouped-or-expanded, and preparation carries
		// both members, so the derived one is recomputed rather than carried.
		UsesExpandedEvidence: prepared.Grouped() || prepared.ExpandedEvidence(),
		PlanDigest:           binding.NormalFormSHA256,
		EstimatedBaseFacts:   prepared.EstimatedBaseFacts(),
		SidecarGrants:        prepared.SidecarGrants(),
		SourcePublications:   prepared.SourcePublications(),
		PolicyGrant:          prepared.PolicyGrant(),
	}
	// The Gateway holds two normal-form members and fills whichever its plan
	// shape uses: a single-product plan normalizes to a NormalForm, a relational
	// one to an AlgebraNormalFormV2. The extraction carries one member, because
	// the profile and the plan shape already determine which normalizer produced
	// it. Placing it in the member the legacy shape uses is what makes the two
	// comparable without either side pretending to a distinction it does not draw.
	if test.plan.From != nil {
		shape.AlgebraNormalFormSHA256 = binding.NormalFormSHA256
	} else {
		shape.NormalFormSHA256 = binding.NormalFormSHA256
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
	if shape.PlanDigest == "" {
		// A non-exposure operation signs no execution binding, so the production
		// side carries no binding identities to compare these against. Filling them
		// here would report a difference where there is nothing to differ.
		return shape
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

// notYetExtracted names the shapes whose derivation still lives in the Gateway.
//
// It is a list that may only shrink. The guard below fails both when a listed
// shape starts preparing -- which means it moved and its parity must now be
// verified -- and when an unlisted one refuses, so the extraction cannot be
// declared complete while a shape is quietly excluded from the comparison.
var notYetExtracted = map[string]string{}

// The extraction must be honest about what it has not moved.
//
// Without this, a shape could be dropped from notYetExtracted and from the
// comparison at once, and the harness would report a complete parity it never
// checked.
func TestEveryNamedShapeIsEitherComparedOrDeclaredPending(t *testing.T) {
	named := map[string]bool{}
	for _, test := range parityCases() {
		named[test.name] = true
	}
	for name := range notYetExtracted {
		if !named[name] {
			t.Errorf("notYetExtracted lists %q, which is not a named parity shape; "+
				"a pending entry that matches no case excludes nothing and hides nothing", name)
		}
	}
	// The semantic View is no longer pending with the table shapes. It reaches
	// preparation through PrepareSemanticView rather than Prepare, so it is not a
	// parityCase and cannot appear in this list; its own differential is
	// TestSemanticViewPreparationMatchesTheGateway, which compares both
	// composition shapes at V2, V4 and V5.
	if len(notYetExtracted) == 0 {
		return
	}
	t.Logf("%d named shape(s) are not yet extracted and are not yet compared: %v",
		len(notYetExtracted), sortedPendingShapes())
}

func sortedPendingShapes() []string {
	names := make([]string, 0, len(notYetExtracted))
	for name := range notYetExtracted {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// The Gateway's Service is not reachable from preparation.
//
// physicalquery must not import internal/gateway, and this states the reason
// rather than leaving it to review: an import edge in that direction would let
// preparation read the registry, the store or the catalog pointer again, which
// is the dependency the whole extraction removes.
func TestPreparationDoesNotDependOnTheGateway(t *testing.T) {
	// The compile-time proof is the import graph; this is the runtime half of it.
	// A Service built with no catalog, no registry and no store cannot influence
	// a preparation, so preparing with one present and one absent must agree.
	service := parityService(t, true)
	test := parityCases()[0]
	inputs := preparationInputsFor(t, service, test)

	withService, err := physicalquery.Prepare(inputs)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	bare := &Service{catalog: service.catalog}
	bareInputs := preparationInputsFor(t, bare, test)
	withoutRegistry, err := physicalquery.Prepare(bareInputs)
	if err != nil {
		t.Fatalf("prepare without a registry: %v", err)
	}
	if err := withService.Binding().RequireSame(withoutRegistry.Binding()); err != nil {
		t.Fatalf("preparation depended on something outside its inputs: %v", err)
	}
}
