package gateway

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
)

// legacyDigestOf maps a new PreparedOperation onto the fields Query Receipt V9
// actually signs, and returns the V9 prepared-operation digest.
//
// This is the mapping the Gateway performs today: preparedOperation.PlanSHA256
// carries the NORMAL FORM digest, not the new binding's PlanSHA256, because V9
// identifies a query by its canonical form.
func legacyDigestOf(t *testing.T, prepared physicalquery.PreparedOperation,
	service *Service, viewBinding string) string {
	t.Helper()
	visibleSQL, companionSQL, err := prepared.ExecutableStatements()
	if err != nil {
		t.Fatalf("statements: %v", err)
	}
	binding := prepared.Binding()
	var dictionary string
	if set, setErr := prepared.DictionarySet(); setErr == nil && set != nil {
		if digest, digestErr := set.Digest(); digestErr == nil {
			dictionary = digest
		}
	}
	var sidecars string
	if grants := prepared.SidecarGrants(); len(grants) > 0 {
		sidecars = sidecarGrantsDigest(grants)
	}
	operation, err := newPreparedOperation(preparedOperation{
		PlanSHA256:                 binding.NormalFormSHA256,
		ExposureProfileVersion:     exposure.ProfileV2,
		GrantDigest:                strings.Repeat("b", 64),
		ManifestDigest:             strings.Repeat("a", 64),
		CatalogSHA256:              service.catalog.SHA256,
		DatasourceID:               "coverage-datasource",
		SchemaDigest:               strings.Repeat("9", 64),
		ViewBindingDigest:          viewBinding,
		OrdinalDictionarySetSHA256: dictionary,
		SidecarGrantsSHA256:        sidecars,
		VisibleSQL:                 visibleSQL,
		CompanionSQL:               companionSQL,
	})
	if err != nil {
		t.Fatalf("build legacy prepared operation: %v", err)
	}
	return operation.digest()
}

// TestQueryReceiptV9DoesNotCoverThePreparedOperationBinding is the coverage
// proof the version decision rests on.
//
// Route A of the binding-compatibility question was to show that every member of
// PreparedOperationBindingV1 is already transitively covered by what V9 signs,
// so the new preparation could be adopted under the existing receipt version.
// It is not, and this demonstrates it rather than arguing it: two preparations
// that differ in a load-bearing member of the new binding produce the SAME V9
// prepared-operation digest.
//
// The mutation used is the Artifact's interface digest. It is load-bearing --
// it is part of the compiled View the operation was authorized against -- and it
// changes neither the statements, the normal form, the dictionary set nor the
// sidecar grants, which is the whole of what V9 identifies a prepared operation
// by. V9 therefore cannot distinguish the two, while the new binding does.
//
// The same argument applies to every new member V9 has no field for: the policy
// grant, the ordinal program, the predicate footprint, the source publications,
// the estimated base facts, the view registry revision, the Catalog CONTENT as
// opposed to its claimed digest, and all four semantic View identities. The
// interface digest is simply the cheapest of them to demonstrate.
//
// Route B is therefore required: a forward-versioned QueryExecutionBindingV2 /
// Query Receipt V10 that signs PreparedOperationBindingV1.SHA256 and the two
// prepared target bindings explicitly. Reusing V9 would either leave those
// members unsigned or silently change what an existing V9 field means, and a
// field whose meaning changed under a version that did not is the one outcome
// the extraction must not produce.
func TestQueryReceiptV9DoesNotCoverThePreparedOperationBinding(t *testing.T) {
	parity := resolveSemanticViewCase(t, semanticViewCase{
		name: "projection", columns: []string{"amount"}, profile: exposure.ProfileV2,
	})

	baseline, err := physicalquery.PrepareSemanticView(semanticViewInputsFor(t, parity))
	if err != nil {
		t.Fatalf("baseline preparation: %v", err)
	}
	mutated := semanticViewInputsFor(t, parity)
	mutated.Artifact.InterfaceDigest = strings.Repeat("5", 64)
	changed, err := physicalquery.PrepareSemanticView(mutated)
	if err != nil {
		t.Fatalf("mutated preparation: %v", err)
	}

	if baseline.Binding().SHA256 == changed.Binding().SHA256 {
		t.Fatal("the new binding did not move, so this case cannot show what V9 misses")
	}

	viewBinding := parity.fixture.binding.Digest
	before := legacyDigestOf(t, baseline, parity.fixture.service, viewBinding)
	after := legacyDigestOf(t, changed, parity.fixture.service, viewBinding)
	if before != after {
		t.Fatalf("the V9 prepared-operation digest moved (%s -> %s); if V9 does cover this "+
			"member, the route-A coverage proof should be completed instead of forward-versioning",
			before[:12], after[:12])
	}
	t.Logf("V9 prepared-operation digest is unchanged at %s across a load-bearing "+
		"change to the compiled Artifact; PreparedOperationBindingV1 moved. "+
		"Route B (QueryExecutionBindingV2 / Query Receipt V10) is required.", before[:12])
}
