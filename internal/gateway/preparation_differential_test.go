package gateway

import (
	"errors"
	"sort"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// This is T1b-B: the differential half of the parity harness.
//
// T1b-A established what the Gateway's derivation produces and checked it
// against evidence neither implementation makes. This file adds the comparison
// that was missing -- the Gateway's derivation against
// internal/physicalquery.Prepare -- for every shape whose derivation has moved,
// and requires the shapes that have not moved to say so rather than to produce
// something.
//
// The production Gateway still runs its own derivation. Prepare is reachable
// from tests only until every named shape passes here; that is what keeps a
// second implementation out of the running system while it is being verified.

// preparationInputsFor builds the immutable inputs Prepare reads, the way the
// Gateway will build them at T1d.
//
// The snapshot bindings come from the Gateway's own verified registry, which is
// the point of the descriptor: the Gateway resolves them from the registry it
// has already checked against the Catalog, and the finalizer will resolve the
// same values from retained Publication evidence, so neither depends on the
// other's copy.
func preparationInputsFor(t *testing.T, service *Service, test parityCase) physicalquery.PreparationInputs {
	t.Helper()
	resolved := resolveParityCase(t, service, test)
	grant := resolved.grant()

	products := make(map[string]catalog.Product, len(service.catalog.Products))
	for _, product := range service.catalog.Products {
		products[product.Name] = product
	}
	publications := append([]catalog.SnapshotPublication(nil), service.catalog.SnapshotPublications...)
	sort.Slice(publications, func(left, right int) bool {
		return publications[left].Name < publications[right].Name
	})

	inputs := physicalquery.PreparationInputs{
		Plan: resolved.plan,
		Grant: physicalquery.Grant{
			ApprovedProducts: grant.ApprovedProducts,
			ApprovedColumns:  grant.ApprovedColumns,
			MandatoryScope:   grant.MandatoryScope,
			ExposureProfile:  grant.Exposure.ProfileVersion,
			PredicateLimits:  predicateLimitsForGrant(grant.Exposure),
		},
		Catalog: physicalquery.CatalogView{
			Digest: service.catalog.SHA256, Version: service.catalog.CatalogVersion,
			Products: products, SnapshotPublications: publications,
		},
	}
	if inputs.Grant.UsesOrdinalProgram() {
		inputs.SnapshotBindings = gatewaySnapshotBindings(t, service)
	}
	return inputs
}

// gatewaySnapshotBindings reads the verified registry into descriptors.
//
// This is the code the Gateway will keep after the extraction: resolving and
// verifying the artifact stays here, because that is what the Gateway's registry
// is for, and only the resolved values cross into preparation.
func gatewaySnapshotBindings(t *testing.T, service *Service) map[string]physicalquery.SnapshotBinding {
	t.Helper()
	bindings := make(map[string]physicalquery.SnapshotBinding, len(service.catalog.SnapshotPublications))
	for _, publication := range service.catalog.SnapshotPublications {
		index, err := service.snapshotRegistry.Resolve(ordinal.PublicationKey{
			CatalogDigest: service.catalog.SHA256, PublicationName: publication.Name,
		})
		if err != nil {
			t.Fatalf("resolve snapshot publication %s: %v", publication.Name, err)
		}
		manifest := index.Manifest()
		bindings[publication.Name] = physicalquery.SnapshotBinding{
			PublicationName:  publication.Name,
			DictionaryDigest: index.DictionaryDigest(),
			ManifestDigest:   index.ManifestDigest(),
			SidecarDigest:    manifest.SidecarDigest,
			SourceNamespace:  manifest.SourceNamespace,
			Snapshot:         manifest.Snapshot,
			OrdinalSidecar:   publication.OrdinalSidecar,
			RowCount:         index.RowCount(),
		}
	}
	return bindings
}

// extractedShapeOf reads the same properties off a physicalquery preparation.
//
// The two prepared-binding digests are computed with the Gateway's own
// newPreparedOperation rather than read off the physicalquery binding. They are
// different constructions -- the Gateway's is what a Query Execution Binding
// signs today -- so comparing physicalquery's binding against the Gateway's
// would compare two things that were never meant to be equal. What must be
// unchanged is the value the Gateway will sign, and that is what this computes,
// from the new implementation's statements.
func extractedShapeOf(t *testing.T, service *Service, prepared physicalquery.PreparedOperation,
	test parityCase, evidence datasourceEvidence, manifestDigest, grantDigest string) preparationShape {
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
		NormalFormSHA256:     binding.NormalFormSHA256,
		EstimatedBaseFacts:   prepared.EstimatedBaseFacts(),
		SidecarGrants:        prepared.SidecarGrants(),
		SourcePublications:   prepared.SourcePublications(),
		PolicyGrant:          prepared.PolicyGrant(),
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
		return shape
	}

	operation, err := newPreparedOperation(preparedOperation{
		PlanSHA256:                 shape.PlanDigest,
		ExposureProfileVersion:     test.profile,
		GrantDigest:                grantDigest,
		ManifestDigest:             manifestDigest,
		CatalogSHA256:              service.catalog.SHA256,
		DatasourceID:               evidence.DatasourceID,
		SchemaDigest:               evidence.SchemaDigest,
		OrdinalDictionarySetSHA256: shape.DictionarySetDigest,
		SidecarGrantsSHA256:        shape.SidecarGrantsSHA256,
		VisibleSQL:                 shape.VisibleSQL,
		CompanionSQL:               shape.CompanionSQL,
	})
	if err != nil {
		t.Fatalf("build the prepared operation binding: %v", err)
	}
	shape.PreparedOperationSHA256 = operation.digest()
	shape.VisibleTargetSHA256 = operation.visibleTargetSHA
	shape.CompanionTargetSHA256 = operation.companionTargetSHA
	return shape
}

// notYetExtracted names the shapes whose derivation still lives in the Gateway.
//
// It is a list that may only shrink. The guard below fails both when a listed
// shape starts preparing -- which means it moved and its parity must now be
// verified -- and when an unlisted one refuses, so the extraction cannot be
// declared complete while a shape is quietly excluded from the comparison.
var notYetExtracted = map[string]string{
	"relational_join":           "online relational Join",
	"relational_union":          "online relational Union",
	"duplicate_product_binding": "online relational Union",
}

// Every extracted shape must prepare identically in both implementations.
func TestExtractedPreparationMatchesTheGateway(t *testing.T) {
	for _, test := range parityCases() {
		t.Run(test.name, func(t *testing.T) {
			service, legacy := prepareParityCase(t, test)
			resolved := resolveParityCase(t, service, test)
			evidence, manifestDigest, grantDigest := parityEvidence(t, service, resolved.products)

			inputs := preparationInputsFor(t, service, test)
			prepared, err := physicalquery.Prepare(inputs)
			if _, pending := notYetExtracted[test.name]; pending {
				if err == nil {
					t.Fatalf("shape %q prepared, but it is listed as not yet extracted; "+
						"remove it from notYetExtracted so its parity is verified", test.name)
				}
				if !errors.Is(err, physicalquery.ErrShapeNotExtracted) {
					t.Fatalf("shape %q was refused for a reason other than being unextracted: %v",
						test.name, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("shape %q is not listed as pending but Prepare refused it: %v", test.name, err)
			}

			// The prepared object must also be the one its binding describes, and
			// must be provably prepared from these inputs. A parity pass over an
			// object that failed either check would be comparing the right bytes
			// carried by the wrong evidence.
			compiler, err := physicalquery.LocalCompilerIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if err := prepared.RequireInputs(inputs, compiler); err != nil {
				t.Fatalf("the prepared operation does not bind these inputs: %v", err)
			}

			extracted := extractedShapeOf(t, service, prepared, resolved, evidence, manifestDigest, grantDigest)
			// The metering projection is deliberately not compared. It is Gateway
			// observation state rather than prepared output -- the half that reads
			// it stays behind -- and its whole effect on what is prepared is the
			// compiled statement bytes and the widened policy grant, both of which
			// are compared.
			legacy.MeteringColumns, extracted.MeteringColumns = nil, nil
			requireSameShape(t, legacy, extracted)
		})
	}
}

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
	if len(notYetExtracted) == 0 {
		return
	}
	// While anything is pending, the semantic View is pending with it: its
	// derivation reaches Prepare through a different entry point that does not
	// exist yet, and TestTheSemanticViewShapePrepares still exercises only the
	// Gateway's own path.
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

// A shape the extraction cannot prepare must fail closed, not prepare
// something else.
//
// This is the same property internal/gateway's V5 union test states about the
// legacy path, asserted against the new one so the two cannot diverge on it.
func TestTheExtractionRefusesTheBranchFilteredUnionUnderV5(t *testing.T) {
	service := parityService(t, true)
	test := parityCase{
		name: "union_branch_filtered", profile: "taskgate-exposure-v5",
		products: []string{"expense_summary"},
		columns:  map[string][]string{"expense_summary": summaryColumns},
		plan: queryplan.QueryPlan{From: &queryplan.From{UnionDistinct: &queryplan.UnionDistinct{
			Role: "expense_summary", Columns: []string{"department", "month"},
			Left: queryplan.Scan{Product: "expense_summary", Role: "left_branch",
				Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "机票"}}},
			Right: queryplan.Scan{Product: "expense_summary", Role: "right_branch",
				Filters: []queryplan.Filter{{Column: "expense_type", Op: "=", Value: "酒店"}}},
		}}, Columns: []string{"expense_summary.department"}},
		needsRegistry: true,
	}
	if _, err := physicalquery.Prepare(preparationInputsFor(t, service, test)); err == nil {
		t.Fatal("the extraction prepared a branch-filtered V5 union")
	}
}

// Preparation must not reach a registry, a store or a clock.
//
// The inputs are values, so a preparation that produced the same statements from
// them twice is reproducible by anyone holding them -- which is the property the
// finalizer's independent reconstruction rests on. Preparing twice from one
// input set and requiring byte equality is the cheapest statement of it.
func TestPreparationIsReproducibleFromItsInputsAlone(t *testing.T) {
	service := parityService(t, true)
	for _, test := range parityCases() {
		if _, pending := notYetExtracted[test.name]; pending {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			inputs := preparationInputsFor(t, service, test)
			first, err := physicalquery.Prepare(inputs)
			if err != nil {
				t.Fatalf("first preparation: %v", err)
			}
			second, err := physicalquery.Prepare(preparationInputsFor(t, service, test))
			if err != nil {
				t.Fatalf("second preparation: %v", err)
			}
			if err := first.Binding().RequireSame(second.Binding()); err != nil {
				t.Fatalf("two preparations from one input set disagree: %v", err)
			}
		})
	}
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
