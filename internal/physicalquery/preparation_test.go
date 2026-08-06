package physicalquery

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// ---------------------------------------------------------------- fixtures

func testGrant() Grant {
	return Grant{
		ApprovedProducts: []string{"expense", "headcount"},
		ApprovedColumns: map[string][]string{
			"expense":   {"month", "amount", "department"},
			"headcount": {"month", "people"},
		},
		MandatoryScope:  json.RawMessage(`{"department":["sales"]}`),
		ExposureProfile: exposure.ProfileV5,
	}
}

func testPublication(name string) catalog.SnapshotPublication {
	return catalog.SnapshotPublication{
		Name: name, Source: "warehouse", SourceNamespace: "reporting",
		Snapshot:         "2026-08-01",
		ManifestDigest:   strings.Repeat("a", 64),
		DictionaryDigest: strings.Repeat("b", 64),
		SidecarDigest:    strings.Repeat("c", 64),
		OrdinalSidecar:   "taskgate_ordinal." + name + "_v1",
	}
}

func testBinding(name string) SnapshotBinding {
	publication := testPublication(name)
	return SnapshotBinding{
		PublicationName: publication.Name, DictionaryDigest: publication.DictionaryDigest,
		ManifestDigest: publication.ManifestDigest, SidecarDigest: publication.SidecarDigest,
		SourceNamespace: publication.SourceNamespace, Snapshot: publication.Snapshot,
		OrdinalSidecar: publication.OrdinalSidecar, RowCount: 100000,
	}
}

func testCatalogView() CatalogView {
	return CatalogView{
		Digest: strings.Repeat("d", 64), Version: "catalog-v4",
		Products: map[string]catalog.Product{
			"expense":   {Name: "expense"},
			"headcount": {Name: "headcount"},
		},
		SnapshotPublications: []catalog.SnapshotPublication{
			testPublication("expense"), testPublication("headcount"),
		},
	}
}

func testInputs() PreparationInputs {
	return PreparationInputs{
		Plan:  queryplan.QueryPlan{Product: "expense", Columns: []string{"month"}, Limit: 10},
		Grant: testGrant(), Catalog: testCatalogView(),
		SnapshotBindings: map[string]SnapshotBinding{
			"expense": testBinding("expense"), "headcount": testBinding("headcount"),
		},
	}
}

func testDictionarySet(t *testing.T) *ordinal.DictionarySetManifest {
	t.Helper()
	members := make([]ordinal.DictionarySetMember, 0, 2)
	for _, name := range []string{"expense", "headcount"} {
		publication := testPublication(name)
		members = append(members, ordinal.DictionarySetMember{
			PublicationName: publication.Name, DictionaryDigest: publication.DictionaryDigest,
			ManifestDigest: publication.ManifestDigest,
		})
	}
	set, err := ordinal.NewDictionarySetManifest(testCatalogView().Digest, members...)
	if err != nil {
		t.Fatalf("dictionary set: %v", err)
	}
	return &set
}

func testOrdinalProgram() *queryplan.OrdinalProgram {
	return &queryplan.OrdinalProgram{
		Version: "taskgate-ordinal-program-v1", Kind: "scan",
		Sources: []queryplan.OrdinalSource{{
			Product: "expense", SourceAlias: "tg_src_0", SourceNamespace: "reporting",
			Snapshot: "2026-08-01", Role: "base", HandleAlias: "tg_handle_0", HandleRequired: true,
			Branch: -1,
			EntityKey: []queryplan.OrdinalFieldBinding{
				{Column: "month", ProvenanceAlias: "tg_key_0"},
			},
			EvidenceFields: []queryplan.OrdinalFieldBinding{
				{Column: "amount", ProvenanceAlias: "tg_fact_0"},
			},
		}},
		Visible:              []queryplan.OrdinalVisibleSpec{{}},
		WitnessRules:         []queryplan.OrdinalWitnessRule{{}},
		CanonicalExpressions: []string{`"expense"."month"`},
		SnapshotBundle: []queryplan.OrdinalSnapshotBinding{
			{SourceNamespace: "reporting", Snapshot: "2026-08-01"},
		},
	}
}

func testPredicateFootprint() *queryplan.PredicateFootprint {
	return &queryplan.PredicateFootprint{
		Version: "taskgate-predicate-footprint-v1", ContextSHA256: strings.Repeat("1", 64),
		Atoms:           []exposure.FactID{{Profile: exposure.ProfileV5, CanonicalValue: "sales"}},
		AtomSetSHA256:   strings.Repeat("2", 64),
		RawLiteralCount: 1, UniqueAtomCount: 1,
	}
}

// testDraft is the derivation output the fixtures seal. It is deliberately a
// fully populated one -- companion, ordinal program, dictionary set, source
// publications and predicate footprint all present -- so that every runtime
// member has something for Validate to disagree with.
func testDraft() OperationDraft {
	return OperationDraft{
		VisibleSQL:    `SELECT "month" FROM "reporting"."expense" LIMIT 10`,
		CompanionSQL:  `SELECT "row_handle" FROM "taskgate_ordinal"."expense_v1" LIMIT 11`,
		VisibleFields: []string{"month"}, FactFields: []string{"month"},
		ProvenanceFields: []string{"tg_handle_0"},
		Grouped:          false, ExpandedEvidence: true,
		PolicyGrant: sqlpolicy.Grant{Products: []sqlpolicy.ProductGrant{{
			LogicalName: "expense", PhysicalSchema: "reporting", PhysicalView: "expense",
			ApprovedColumns:  []string{"month", "amount", "department"},
			AllowedOperators: []string{"=", "IN"},
			MandatoryScope: []sqlpolicy.ScopePredicate{
				{Column: "department", Operator: "IN", Values: []string{"sales"}},
			},
		}}},
		SidecarGrants: []sqlpolicy.ProductGrant{{
			LogicalName: "tg_sidecar_expense", PhysicalSchema: "taskgate_ordinal",
			PhysicalView: "expense_v1", ApprovedColumns: []string{"row_handle", "month"},
			AllowedOperators: []string{"="},
		}},
		SourcePublications: map[string]string{"tg_src_0": "expense"},
		EstimatedBaseFacts: 4200,
	}
}

// sealedTestOperation builds a coherent prepared operation the only way one can
// be built: every digest derived by the constructor, nothing asserted.
func sealedTestOperation(t *testing.T) PreparedOperation {
	t.Helper()
	draft := testDraft()
	draft.OrdinalProgram = testOrdinalProgram()
	draft.DictionarySet = testDictionarySet(t)
	draft.PredicateFootprint = testPredicateFootprint()
	return sealOperation(t, testInputs(), draft)
}

func sealOperation(t *testing.T, inputs PreparationInputs, draft OperationDraft) PreparedOperation {
	t.Helper()
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatalf("compiler identity: %v", err)
	}
	prepared, err := NewPreparedOperation(inputs, compiler, draft)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return prepared
}

// ---------------------------------------------------------------- sealing

func TestASealedOperationValidates(t *testing.T) {
	if err := sealedTestOperation(t).Validate(); err != nil {
		t.Fatalf("a sealed prepared operation was rejected: %v", err)
	}
}

// The defect the hardening fixes: under the previous shape any 64 hex characters
// passed a format check, so the binding a whole comparison rests on could be
// asserted rather than derived.
func TestASuppliedBindingDigestIsRejected(t *testing.T) {
	prepared := sealedTestOperation(t)
	prepared.binding.SHA256 = strings.Repeat("f", 64)
	if err := prepared.binding.Validate(); err == nil {
		t.Fatal("a binding digest that was written rather than sealed was accepted")
	}
	if err := prepared.Validate(); err == nil {
		t.Fatal("a prepared operation with an asserted binding digest was accepted")
	}
}

// Every member must be covered by the seal, or mutating it would leave the
// digest valid.
func TestTheSealCoversEveryBindingMember(t *testing.T) {
	for name, mutate := range map[string]func(*PreparedOperationBindingV1){
		"has companion":          func(b *PreparedOperationBindingV1) { b.HasCompanion = false },
		"grouped":                func(b *PreparedOperationBindingV1) { b.Grouped = true },
		"expanded evidence":      func(b *PreparedOperationBindingV1) { b.ExpandedEvidence = false },
		"visible field count":    func(b *PreparedOperationBindingV1) { b.VisibleFieldCount = 2 },
		"fact field count":       func(b *PreparedOperationBindingV1) { b.FactFieldCount = 2 },
		"provenance field count": func(b *PreparedOperationBindingV1) { b.ProvenanceFieldCount = 2 },
		"visible fields":         func(b *PreparedOperationBindingV1) { b.VisibleFieldsSHA256 = strings.Repeat("1", 64) },
		"fact fields":            func(b *PreparedOperationBindingV1) { b.FactFieldsSHA256 = strings.Repeat("2", 64) },
		"provenance fields":      func(b *PreparedOperationBindingV1) { b.ProvenanceFieldsSHA256 = strings.Repeat("3", 64) },
		"preparation inputs":     func(b *PreparedOperationBindingV1) { b.PreparationInputsSHA256 = strings.Repeat("4", 64) },
		"grant":                  func(b *PreparedOperationBindingV1) { b.GrantSHA256 = strings.Repeat("5", 64) },
		"catalog":                func(b *PreparedOperationBindingV1) { b.CatalogSHA256 = strings.Repeat("6", 64) },
		"snapshot binding set":   func(b *PreparedOperationBindingV1) { b.SnapshotBindingSetSHA256 = strings.Repeat("7", 64) },
		"plan":                   func(b *PreparedOperationBindingV1) { b.PlanSHA256 = strings.Repeat("8", 64) },
		"compiler identity":      func(b *PreparedOperationBindingV1) { b.CompilerIdentitySHA256 = strings.Repeat("9", 64) },
		"policy grant":           func(b *PreparedOperationBindingV1) { b.PolicyGrantSHA256 = strings.Repeat("1", 64) },
		"source publications":    func(b *PreparedOperationBindingV1) { b.SourcePublicationsSHA256 = strings.Repeat("2", 64) },
		"ordinal program":        func(b *PreparedOperationBindingV1) { b.OrdinalProgramSHA256 = strings.Repeat("a", 64) },
		"dictionary set":         func(b *PreparedOperationBindingV1) { b.DictionarySetSHA256 = strings.Repeat("b", 64) },
		"sidecar grants":         func(b *PreparedOperationBindingV1) { b.SidecarGrantsSHA256 = strings.Repeat("c", 64) },
		"view binding":           func(b *PreparedOperationBindingV1) { b.ViewBindingSHA256 = strings.Repeat("d", 64) },
		"view registry revision": func(b *PreparedOperationBindingV1) { b.ViewRegistryRevisionSHA256 = strings.Repeat("e", 64) },
		"predicate footprint":    func(b *PreparedOperationBindingV1) { b.PredicateFootprintSHA256 = strings.Repeat("f", 64) },
		"estimated base facts":   func(b *PreparedOperationBindingV1) { b.EstimatedBaseFacts = 1 },
		"visible target":         func(b *PreparedOperationBindingV1) { b.VisibleTargetSHA256 = strings.Repeat("0", 64) },
		"companion target":       func(b *PreparedOperationBindingV1) { b.CompanionTargetSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			binding := sealedTestOperation(t).Binding()
			mutate(&binding)
			if err := binding.Validate(); err == nil {
				t.Fatalf("the seal ignored a changed %s", name)
			}
		})
	}
}

// The same mutations must be NAMED by the comparison the finalizer uses, not
// merely detected by the seal. A rejection that cannot say what differs is not
// usable evidence.
func TestRequireSameNamesEveryChangedMember(t *testing.T) {
	base := sealedTestOperation(t).Binding()
	for name, mutate := range map[string]func(*PreparedOperationBindingV1){
		"grouped":              func(b *PreparedOperationBindingV1) { b.Grouped = true },
		"visible field count":  func(b *PreparedOperationBindingV1) { b.VisibleFieldCount = 2 },
		"preparation inputs":   func(b *PreparedOperationBindingV1) { b.PreparationInputsSHA256 = strings.Repeat("4", 64) },
		"grant":                func(b *PreparedOperationBindingV1) { b.GrantSHA256 = strings.Repeat("5", 64) },
		"catalog":              func(b *PreparedOperationBindingV1) { b.CatalogSHA256 = strings.Repeat("6", 64) },
		"snapshot binding set": func(b *PreparedOperationBindingV1) { b.SnapshotBindingSetSHA256 = strings.Repeat("7", 64) },
		"plan":                 func(b *PreparedOperationBindingV1) { b.PlanSHA256 = strings.Repeat("8", 64) },
		"compiler identity":    func(b *PreparedOperationBindingV1) { b.CompilerIdentitySHA256 = strings.Repeat("9", 64) },
		"policy grant":         func(b *PreparedOperationBindingV1) { b.PolicyGrantSHA256 = strings.Repeat("1", 64) },
		"source publications":  func(b *PreparedOperationBindingV1) { b.SourcePublicationsSHA256 = strings.Repeat("2", 64) },
		"estimated base facts": func(b *PreparedOperationBindingV1) { b.EstimatedBaseFacts = 1 },
		"visible target":       func(b *PreparedOperationBindingV1) { b.VisibleTargetSHA256 = strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			resealed, err := mutated.Seal()
			if err != nil {
				t.Fatalf("reseal: %v", err)
			}
			err = base.RequireSame(resealed)
			if err == nil {
				t.Fatalf("the comparison ignored a changed %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("the rejection %q does not name the changed %s", err, name)
			}
		})
	}
}

func TestTwoIdenticalPreparationsAgree(t *testing.T) {
	if err := sealedTestOperation(t).Binding().RequireSame(sealedTestOperation(t).Binding()); err != nil {
		t.Fatalf("two identical preparations disagreed: %v", err)
	}
}

// ------------------------------------------------------- serialization rules

// The runtime object holds executable SQL. Refusing to marshal is the only form
// of this rule a future member cannot weaken by omitting a struct tag.
func TestAPreparedOperationRefusesToSerialize(t *testing.T) {
	if _, err := json.Marshal(sealedTestOperation(t)); err == nil {
		t.Fatal("a PreparedOperation serialized")
	}
	// And through a container, which is how it would actually happen.
	if _, err := json.Marshal(map[string]any{"prepared": sealedTestOperation(t)}); err == nil {
		t.Fatal("a PreparedOperation serialized inside a container")
	}
}

// The durable half must carry no statement and no physical name. This is
// structural rather than a keyword scan: the encoded object may contain only
// members this test knows about.
func TestTheDurableBindingCarriesOnlyIdentities(t *testing.T) {
	encoded, err := json.Marshal(sealedTestOperation(t).Binding())
	if err != nil {
		t.Fatalf("marshal binding: %v", err)
	}
	var members map[string]any
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	allowed := map[string]bool{
		"version": true, "has_companion": true, "grouped": true, "expanded_evidence": true,
		"visible_field_count": true, "fact_field_count": true, "provenance_field_count": true,
		"visible_fields_sha256": true, "fact_fields_sha256": true, "provenance_fields_sha256": true,
		"preparation_inputs_sha256": true, "grant_sha256": true, "catalog_sha256": true,
		"snapshot_binding_set_sha256": true, "plan_sha256": true, "compiler_identity_sha256": true,
		"policy_grant_sha256": true, "ordinal_program_sha256": true, "dictionary_set_sha256": true,
		"sidecar_grants_sha256": true, "source_publications_sha256": true,
		"view_binding_sha256": true, "view_registry_revision_sha256": true,
		"predicate_footprint_sha256": true, "estimated_base_facts": true,
		"visible_target_sha256": true, "companion_target_sha256": true, "sha256": true,
	}
	for name, value := range members {
		if !allowed[name] {
			t.Errorf("the durable binding carries member %q, which this test does not know to be "+
				"SQL-free and physical-name-free; add it deliberately or move it to the runtime object", name)
			continue
		}
		// Every string member is either a version, a digest, or empty.
		text, isText := value.(string)
		if !isText || name == "version" {
			continue
		}
		if text != "" && !validSHA256(text) {
			t.Errorf("durable binding member %q is the free string %q; only digests and the version may be text",
				name, text)
		}
	}
}

// ------------------------------------------------------- input canonicalization

// Two callers legitimately assemble one authorization in different orders. They
// must reach one identity, or the finalizer would disagree with the Gateway for
// a reason that is not about the operation.
func TestSemanticallyEqualInputsInDifferentOrdersAgree(t *testing.T) {
	first := testInputs()
	second := testInputs()
	second.Grant.ApprovedProducts = []string{"headcount", "expense"}
	second.Grant.ApprovedColumns = map[string][]string{
		"headcount": {"people", "month"},
		"expense":   {"department", "month", "amount"},
	}
	second.Grant.MandatoryScope = json.RawMessage(`{ "department" : [ "sales" ] }`)

	left, err := first.SHA256()
	if err != nil {
		t.Fatalf("first inputs digest: %v", err)
	}
	right, err := second.SHA256()
	if err != nil {
		t.Fatalf("second inputs digest: %v", err)
	}
	if left != right {
		t.Fatalf("two orderings of one authorization produced %s and %s", shortDigest(left), shortDigest(right))
	}
}

// And a real change must move it, in every component.
func TestEveryInputComponentMovesTheIdentity(t *testing.T) {
	base, err := testInputs().SHA256()
	if err != nil {
		t.Fatalf("base digest: %v", err)
	}
	for name, mutate := range map[string]func(*PreparationInputs){
		"an added product": func(i *PreparationInputs) {
			i.Grant.ApprovedProducts = append(i.Grant.ApprovedProducts, "salary")
		},
		"an added column": func(i *PreparationInputs) {
			i.Grant.ApprovedColumns["expense"] = append(i.Grant.ApprovedColumns["expense"], "vendor")
		},
		"a changed scope": func(i *PreparationInputs) {
			i.Grant.MandatoryScope = json.RawMessage(`{"department":["finance"]}`)
		},
		"a changed profile":  func(i *PreparationInputs) { i.Grant.ExposureProfile = exposure.ProfileV4 },
		"a changed plan":     func(i *PreparationInputs) { i.Plan.Limit = 11 },
		"a changed catalog":  func(i *PreparationInputs) { i.Catalog.Digest = strings.Repeat("e", 64) },
		"a changed version":  func(i *PreparationInputs) { i.Catalog.Version = "catalog-v5" },
		"a changed sidecar":  func(i *PreparationInputs) { i.SnapshotBindings["expense"] = SnapshotBinding{} },
		"a changed rowcount": func(i *PreparationInputs) { i.SnapshotBindings["expense"] = mutatedRowCount(t) },
	} {
		t.Run(name, func(t *testing.T) {
			inputs := testInputs()
			mutate(&inputs)
			mutated, err := inputs.SHA256()
			if err != nil {
				// A mutation that makes the inputs invalid is a stronger result
				// than one that merely moves the digest.
				return
			}
			if mutated == base {
				t.Fatalf("%s did not move the preparation identity", name)
			}
		})
	}
}

func mutatedRowCount(t *testing.T) SnapshotBinding {
	t.Helper()
	binding := testBinding("expense")
	binding.RowCount = 99
	return binding
}

func TestGrantRejectsAnIncoherentAuthorization(t *testing.T) {
	for name, breakIt := range map[string]func(*Grant){
		"no product":         func(g *Grant) { g.ApprovedProducts = nil },
		"an unnamed product": func(g *Grant) { g.ApprovedProducts = []string{"expense", " "} },
		"a duplicate product": func(g *Grant) {
			g.ApprovedProducts = []string{"expense", "expense", "headcount"}
		},
		"columns for an unapproved product": func(g *Grant) {
			g.ApprovedColumns["salary"] = []string{"amount"}
		},
		"a duplicate column": func(g *Grant) {
			g.ApprovedColumns["expense"] = []string{"month", "month"}
		},
		"an unknown profile":      func(g *Grant) { g.ExposureProfile = "taskgate-exposure-v9" },
		"a malformed view digest": func(g *Grant) { g.ViewBindingDigest = "not-a-digest" },
		"a malformed scope":       func(g *Grant) { g.MandatoryScope = json.RawMessage(`{`) },
	} {
		t.Run(name, func(t *testing.T) {
			grant := testGrant()
			breakIt(&grant)
			if err := grant.Validate(); err == nil {
				t.Fatal("an incoherent preparation grant was accepted")
			}
			if _, err := grant.SHA256(); err == nil {
				t.Fatal("an incoherent preparation grant produced an identity")
			}
		})
	}
}

func TestCatalogViewRejectsAnAmbiguousCatalog(t *testing.T) {
	for name, breakIt := range map[string]func(*CatalogView){
		"a malformed digest": func(v *CatalogView) { v.Digest = "not-a-digest" },
		"no version":         func(v *CatalogView) { v.Version = " " },
		"no product":         func(v *CatalogView) { v.Products = nil },
		"a key that disagrees with the product": func(v *CatalogView) {
			v.Products["expenses"] = catalog.Product{Name: "expense"}
		},
		"publications out of canonical order": func(v *CatalogView) {
			v.SnapshotPublications = []catalog.SnapshotPublication{
				testPublication("headcount"), testPublication("expense"),
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			view := testCatalogView()
			breakIt(&view)
			if err := view.Validate(); err == nil {
				t.Fatal("an ambiguous catalog view was accepted")
			}
		})
	}
}

// A descriptor must describe the artifact, not merely name it: the derivation
// reads the sidecar relation and the row count.
func TestSnapshotBindingRequiresCompleteImmutableMetadata(t *testing.T) {
	for name, breakIt := range map[string]func(*SnapshotBinding){
		"no publication name": func(b *SnapshotBinding) { b.PublicationName = "" },
		"no sidecar relation": func(b *SnapshotBinding) { b.OrdinalSidecar = "" },
		"an unqualified sidecar relation": func(b *SnapshotBinding) {
			b.OrdinalSidecar = "expense_v1"
		},
		"an over-qualified sidecar relation": func(b *SnapshotBinding) {
			b.OrdinalSidecar = "db.taskgate_ordinal.expense_v1"
		},
		"a malformed manifest digest": func(b *SnapshotBinding) { b.ManifestDigest = "short" },
		"no source namespace":         func(b *SnapshotBinding) { b.SourceNamespace = "" },
	} {
		t.Run(name, func(t *testing.T) {
			binding := testBinding("expense")
			breakIt(&binding)
			if err := binding.Validate(); err == nil {
				t.Fatal("an incomplete snapshot descriptor was accepted")
			}
		})
	}
}

// The caller resolved an artifact; it must be THIS artifact.
func TestSnapshotBindingMustMatchTheCatalog(t *testing.T) {
	for name, mutate := range map[string]func(*SnapshotBinding){
		"another manifest":   func(b *SnapshotBinding) { b.ManifestDigest = strings.Repeat("9", 64) },
		"another dictionary": func(b *SnapshotBinding) { b.DictionaryDigest = strings.Repeat("9", 64) },
		"another sidecar":    func(b *SnapshotBinding) { b.SidecarDigest = strings.Repeat("9", 64) },
		"another snapshot":   func(b *SnapshotBinding) { b.Snapshot = "2026-08-02" },
		"another relation":   func(b *SnapshotBinding) { b.OrdinalSidecar = "taskgate_ordinal.other_v1" },
	} {
		t.Run(name, func(t *testing.T) {
			binding := testBinding("expense")
			mutate(&binding)
			if err := binding.RequireCatalog(testPublication("expense")); err == nil {
				t.Fatal("a binding for another artifact satisfied the Catalog")
			}
		})
	}
}

// The ordinal dictionary universe is Catalog-wide. An incomplete one would pin a
// root ledger to whichever product it first executed.
func TestOrdinalProfilesRequireTheWholeDictionaryUniverse(t *testing.T) {
	inputs := testInputs()
	delete(inputs.SnapshotBindings, "headcount")
	if err := inputs.Validate(); err == nil {
		t.Fatal("an ordinal profile prepared against an incomplete dictionary universe")
	}

	// And a profile that compiles no ordinal program must supply none at all.
	nonOrdinal := testInputs()
	nonOrdinal.Grant.ExposureProfile = exposure.ProfileV2
	if err := nonOrdinal.Validate(); err == nil {
		t.Fatal("a non-ordinal profile was allowed to carry snapshot bindings")
	}
	nonOrdinal.SnapshotBindings = nil
	if err := nonOrdinal.Validate(); err != nil {
		t.Fatalf("a non-ordinal profile with no snapshot bindings was rejected: %v", err)
	}
}

func TestOneDictionaryDigestCannotResolveToTwoManifests(t *testing.T) {
	inputs := testInputs()
	conflicting := testBinding("headcount")
	conflicting.ManifestDigest = strings.Repeat("9", 64)
	inputs.SnapshotBindings["headcount"] = conflicting
	// The Catalog check catches it first, which is the stronger rejection; force
	// the Catalog to agree so the universe check is what fires.
	view := inputs.Catalog
	publication := testPublication("headcount")
	publication.ManifestDigest = conflicting.ManifestDigest
	view.SnapshotPublications = []catalog.SnapshotPublication{testPublication("expense"), publication}
	inputs.Catalog = view
	if err := inputs.Validate(); err == nil {
		t.Fatal("one dictionary digest resolved to two manifests")
	}
}

// ------------------------------------------------------- compiler identity

func TestCompilerIdentityIsSealedFromTheRunningBinary(t *testing.T) {
	identity, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatalf("local compiler identity: %v", err)
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("the local compiler identity does not validate: %v", err)
	}
	if identity.QueryPlanVersion == "" || identity.PolicyRendererVersion == "" {
		t.Fatal("the compiler identity carries no version")
	}
	// Two calls in one binary must agree; that is what lets the Gateway and the
	// finalizer agree by construction when built from one commit.
	again, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if again.SHA256 != identity.SHA256 {
		t.Fatal("the local compiler identity is not stable within one binary")
	}
	for name, mutate := range map[string]func(*CompilerIdentityV1){
		"queryplan version": func(c *CompilerIdentityV1) { c.QueryPlanVersion = "other" },
		"queryplan digest":  func(c *CompilerIdentityV1) { c.QueryPlanSHA256 = strings.Repeat("1", 64) },
		"renderer version":  func(c *CompilerIdentityV1) { c.PolicyRendererVersion = "other" },
		"renderer digest":   func(c *CompilerIdentityV1) { c.PolicyRendererSHA256 = strings.Repeat("2", 64) },
		"the digest itself": func(c *CompilerIdentityV1) { c.SHA256 = strings.Repeat("3", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := identity
			mutate(&mutated)
			if err := mutated.Validate(); err == nil {
				t.Fatalf("the compiler identity seal ignored a changed %s", name)
			}
		})
	}
}

// ------------------------------------------------------- half coherence

// The runtime object and its binding must describe one operation.
//
// This is the property the whole extraction rests on. A binding that merely
// seals to its own digest proves the binding is coherent; what the Gateway's
// evidence needs is that the object it is about to hand the Connector is the one
// that binding describes. Every load-bearing runtime member is changed here --
// from inside the package, since from outside there is nothing to change -- and
// each must be caught.
func TestEveryRuntimeMemberIsCoveredByValidate(t *testing.T) {
	for name, breakIt := range map[string]func(*PreparedOperation){
		"the visible statement": func(p *PreparedOperation) {
			p.visibleSQL = `SELECT "month", "amount" FROM "reporting"."expense" LIMIT 10`
		},
		"the companion statement": func(p *PreparedOperation) {
			p.companionSQL = `SELECT "row_handle" FROM "taskgate_ordinal"."headcount_v1" LIMIT 11`
		},
		"a companion the binding does not know about": func(p *PreparedOperation) {
			p.hasCompanion, p.companionSQL = false, ""
		},
		"a projection the binding does not know about": func(p *PreparedOperation) {
			p.visibleFields = append(p.visibleFields, "smuggled")
		},
		"a renamed projection": func(p *PreparedOperation) {
			p.visibleFields = []string{"smuggled"}
		},
		"a reordered projection": func(p *PreparedOperation) {
			p.factFields = []string{"amount", "month"}
			p.visibleFields = []string{"amount", "month"}
		},
		"a renamed provenance field": func(p *PreparedOperation) {
			p.provenanceFields = []string{"tg_handle_1"}
		},
		"a widened policy grant": func(p *PreparedOperation) {
			p.policyGrant.Products[0].ApprovedColumns = append(
				p.policyGrant.Products[0].ApprovedColumns, "salary")
		},
		"a dropped policy scope": func(p *PreparedOperation) {
			p.policyGrant.Products[0].MandatoryScope = nil
		},
		"a repointed policy relation": func(p *PreparedOperation) {
			p.policyGrant.Products[0].PhysicalView = "expense_unfiltered"
		},
		"a sidecar grant the binding does not know about": func(p *PreparedOperation) {
			p.sidecarGrants = nil
		},
		"a widened sidecar grant": func(p *PreparedOperation) {
			p.sidecarGrants[0].ApprovedColumns = append(p.sidecarGrants[0].ApprovedColumns, "salary")
		},
		"a rewritten ordinal program": func(p *PreparedOperation) {
			p.ordinalProgram.Sources[0].EvidenceFields = nil
		},
		"an ordinal program the binding does not know about": func(p *PreparedOperation) {
			p.ordinalProgram = nil
		},
		"a substituted dictionary set": func(p *PreparedOperation) {
			set, err := ordinal.NewDictionarySetManifest(strings.Repeat("e", 64),
				ordinal.DictionarySetMember{PublicationName: "expense",
					DictionaryDigest: strings.Repeat("b", 64), ManifestDigest: strings.Repeat("a", 64)})
			if err != nil {
				t.Fatal(err)
			}
			p.dictionarySet = &set
		},
		"a dictionary set the binding does not know about": func(p *PreparedOperation) {
			p.dictionarySet = nil
		},
		"a redirected source publication": func(p *PreparedOperation) {
			p.sourcePublications["tg_src_0"] = "headcount"
		},
		"an added source publication": func(p *PreparedOperation) {
			p.sourcePublications["tg_src_1"] = "headcount"
		},
		"a rewritten predicate footprint": func(p *PreparedOperation) {
			p.predicateFootprint.UniqueAtomCount = 0
		},
		"a predicate footprint the binding does not know about": func(p *PreparedOperation) {
			p.predicateFootprint = nil
		},
		"a different base-fact estimate": func(p *PreparedOperation) {
			p.estimatedBaseFacts = 1
		},
		"a different exposure shape": func(p *PreparedOperation) { p.grouped = true },
		"a dropped expanded-evidence flag": func(p *PreparedOperation) {
			p.expandedEvidence = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			prepared := sealedTestOperation(t)
			breakIt(&prepared)
			if err := prepared.Validate(); err == nil {
				t.Fatal("a prepared operation whose halves disagree was accepted")
			}
			// And the statements must be unreachable, not merely reportable as
			// wrong. This is what "no caller may execute an unvalidated prepared
			// operation" means in code.
			if _, _, err := prepared.ExecutableStatements(); err == nil {
				t.Fatal("a prepared operation whose halves disagree handed out its SQL")
			}
		})
	}
}

// Nothing load-bearing may be reachable for writing from outside this package.
// The mutation suite above proves Validate catches an in-package edit; this
// proves an out-of-package one cannot be made at all.
func TestAPreparedOperationExposesNoWritableMember(t *testing.T) {
	operationType := reflect.TypeOf(PreparedOperation{})
	for index := 0; index < operationType.NumField(); index++ {
		field := operationType.Field(index)
		if field.IsExported() {
			t.Errorf("PreparedOperation exports member %q; a caller could rewrite it after preparation "+
				"and execute something the sealed binding does not describe", field.Name)
		}
	}
}

// The accessors must copy. Returning the sealed state itself would put the
// mutation back, one indirection further out.
func TestTheAccessorsDoNotHandOutTheSealedState(t *testing.T) {
	prepared := sealedTestOperation(t)

	prepared.VisibleFields()[0] = "smuggled"
	prepared.FactFields()[0] = "smuggled"
	prepared.SidecarGrants()[0].ApprovedColumns[0] = "smuggled"
	prepared.PolicyGrant().Products[0].ApprovedColumns[0] = "smuggled"
	prepared.PolicyGrant().Products[0].MandatoryScope[0].Values[0] = "everything"
	prepared.SourcePublications()["tg_src_0"] = "headcount"

	program, err := prepared.OrdinalProgram()
	if err != nil {
		t.Fatal(err)
	}
	program.Sources = nil
	set, err := prepared.DictionarySet()
	if err != nil {
		t.Fatal(err)
	}
	set.Members = nil
	footprint, err := prepared.PredicateFootprint()
	if err != nil {
		t.Fatal(err)
	}
	footprint.UniqueAtomCount = 99

	if err := prepared.Validate(); err != nil {
		t.Fatalf("writing through an accessor's result reached the sealed state: %v", err)
	}
}

// The copies must be faithful as well as detached: a member that the encoder
// drops would be a member the digests do not cover.
func TestTheDeepCopiesAreFaithful(t *testing.T) {
	program := testOrdinalProgram()
	clonedProgram, err := cloneOrdinalProgram(program)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(program, clonedProgram) {
		t.Fatal("copying an ordinal program did not preserve it")
	}
	footprint := testPredicateFootprint()
	clonedFootprint, err := clonePredicateFootprint(footprint)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(footprint, clonedFootprint) {
		t.Fatal("copying a predicate footprint did not preserve it")
	}
}

// Validate closes the runtime-to-binding gap. RequireInputs closes the
// binding-to-inputs one: a binding can name input digests coherently and still
// not be the preparation these inputs produce.
func TestRequireInputsRejectsAnotherPreparationsInputs(t *testing.T) {
	compiler, err := LocalCompilerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	prepared := sealedTestOperation(t)
	if err := prepared.RequireInputs(testInputs(), compiler); err != nil {
		t.Fatalf("an operation was rejected against the inputs it was prepared from: %v", err)
	}
	for name, mutate := range map[string]func(*PreparationInputs){
		"a changed plan":    func(i *PreparationInputs) { i.Plan.Limit = 11 },
		"a changed catalog": func(i *PreparationInputs) { i.Catalog.Digest = strings.Repeat("e", 64) },
		"a changed scope": func(i *PreparationInputs) {
			i.Grant.MandatoryScope = json.RawMessage(`{"department":["finance"]}`)
		},
		"an added product": func(i *PreparationInputs) {
			i.Grant.ApprovedProducts = append(i.Grant.ApprovedProducts, "salary")
		},
		"a changed row count": func(i *PreparationInputs) {
			i.SnapshotBindings["expense"] = mutatedRowCount(t)
		},
	} {
		t.Run(name, func(t *testing.T) {
			inputs := testInputs()
			mutate(&inputs)
			if err := prepared.RequireInputs(inputs, compiler); err == nil {
				t.Fatalf("an operation was accepted against inputs with %s", name)
			}
		})
	}
	other := compiler
	other.QueryPlanSHA256 = strings.Repeat("7", 64)
	if other, err = other.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.RequireInputs(testInputs(), other); err == nil {
		t.Fatal("an operation was accepted against another compiler's identity")
	}
}

// A constructed operation must never be one that cannot validate: the
// constructor is the only entry point, so rejecting there is what makes
// "unvalidated prepared operation" a state that does not exist.
func TestTheConstructorRefusesAnIncoherentDraft(t *testing.T) {
	for name, breakIt := range map[string]func(*OperationDraft){
		"no visible statement": func(d *OperationDraft) { d.VisibleSQL = "" },
		"no visible field":     func(d *OperationDraft) { d.VisibleFields = nil },
		"expanded evidence without a companion": func(d *OperationDraft) {
			d.CompanionSQL = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			draft := testDraft()
			breakIt(&draft)
			compiler, err := LocalCompilerIdentity()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewPreparedOperation(testInputs(), compiler, draft); err == nil {
				t.Fatalf("the constructor accepted a draft with %s", name)
			}
		})
	}
}

// The constructor must copy the draft in. A caller that keeps its own reference
// and writes through it afterwards must not reach what was sealed.
func TestTheConstructorCopiesTheDraft(t *testing.T) {
	draft := testDraft()
	draft.OrdinalProgram = testOrdinalProgram()
	draft.DictionarySet = testDictionarySet(t)
	draft.PredicateFootprint = testPredicateFootprint()
	prepared := sealOperation(t, testInputs(), draft)

	draft.VisibleFields[0] = "smuggled"
	draft.SidecarGrants[0].ApprovedColumns[0] = "smuggled"
	draft.PolicyGrant.Products[0].ApprovedColumns[0] = "smuggled"
	draft.SourcePublications["tg_src_0"] = "headcount"
	draft.OrdinalProgram.Sources = nil
	draft.DictionarySet.Members = nil
	draft.PredicateFootprint.UniqueAtomCount = 99

	if err := prepared.Validate(); err != nil {
		t.Fatalf("writing through the caller's draft reached the sealed operation: %v", err)
	}
}

// An operation with no exposure accounting projects no fact field, and must
// still be representable.
//
// The binding originally required a fact-field digest of every operation, which
// made the unaccounted shape impossible to seal at all -- found by the
// differential harness the first time a plain query reached Prepare. Each field
// digest is now present exactly when its count is non-zero, in both directions.
func TestAnOperationWithNoFactProjectionSeals(t *testing.T) {
	inputs := testInputs()
	inputs.Grant.ExposureProfile = ""
	inputs.SnapshotBindings = nil

	draft := OperationDraft{
		VisibleSQL:    `SELECT "month" FROM "reporting"."expense" LIMIT 10`,
		VisibleFields: []string{"month"},
		PolicyGrant:   testDraft().PolicyGrant,
	}
	prepared := sealOperation(t, inputs, draft)
	binding := prepared.Binding()
	if binding.FactFieldCount != 0 || binding.FactFieldsSHA256 != "" {
		t.Fatalf("an operation with no fact projection carries count %d and digest %q",
			binding.FactFieldCount, binding.FactFieldsSHA256)
	}

	// And the coupling holds in both directions, so a count and a digest cannot
	// drift apart the way a one-sided check would allow.
	for name, mutate := range map[string]func(*PreparedOperationBindingV1){
		"a fact digest with no count":       func(b *PreparedOperationBindingV1) { b.FactFieldsSHA256 = strings.Repeat("1", 64) },
		"a fact count with no digest":       func(b *PreparedOperationBindingV1) { b.FactFieldCount = 2 },
		"a provenance digest with no count": func(b *PreparedOperationBindingV1) { b.ProvenanceFieldsSHA256 = strings.Repeat("2", 64) },
		"a provenance count with no digest": func(b *PreparedOperationBindingV1) { b.ProvenanceFieldCount = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := binding
			mutate(&mutated)
			resealed, err := mutated.Seal()
			if err != nil {
				t.Fatal(err)
			}
			if err := resealed.Validate(); err == nil {
				t.Fatalf("the binding accepted %s", name)
			}
		})
	}
}

// A profile that prepares no ordinal material must seal without it, and must not
// acquire an identity for something it never produced.
func TestAnOperationWithoutOrdinalMaterialSeals(t *testing.T) {
	inputs := testInputs()
	inputs.Grant.ExposureProfile = exposure.ProfileV1
	inputs.SnapshotBindings = nil

	draft := testDraft()
	draft.CompanionSQL, draft.ExpandedEvidence = "", false
	draft.ProvenanceFields, draft.SidecarGrants, draft.SourcePublications = nil, nil, nil
	draft.EstimatedBaseFacts = 0

	prepared := sealOperation(t, inputs, draft)
	binding := prepared.Binding()
	for name, digest := range map[string]string{
		"ordinal program":     binding.OrdinalProgramSHA256,
		"dictionary set":      binding.DictionarySetSHA256,
		"sidecar grants":      binding.SidecarGrantsSHA256,
		"source publications": binding.SourcePublicationsSHA256,
		"predicate footprint": binding.PredicateFootprintSHA256,
		"provenance fields":   binding.ProvenanceFieldsSHA256,
		"companion target":    binding.CompanionTargetSHA256,
	} {
		if digest != "" {
			t.Errorf("an operation that prepared no %s carries a %s digest", name, name)
		}
	}
	if binding.PolicyGrantSHA256 == "" {
		t.Fatal("an operation with no ordinal material carries no policy grant identity; " +
			"every operation is authorized against one")
	}
}

func TestExpandedEvidenceRequiresACompanion(t *testing.T) {
	binding := sealedTestOperation(t).Binding()
	binding.HasCompanion, binding.CompanionTargetSHA256 = false, ""
	resealed, err := binding.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if err := resealed.Validate(); err == nil {
		t.Fatal("expanded evidence was accepted on an operation with no companion")
	}
}

func TestTargetSHA256RefusesAnAbsentCompanion(t *testing.T) {
	binding := sealedTestOperation(t).Binding()
	binding.HasCompanion = false
	if _, err := binding.TargetSHA256(RoleCompanion); err == nil {
		t.Fatal("an absent companion produced a target binding")
	}
	if _, err := binding.TargetSHA256("neither"); err == nil {
		t.Fatal("an unknown role produced a target binding")
	}
}

// A target binding must separate the two roles and the operation they belong to,
// or one statement could be presented as another's target.
func TestPreparedTargetBindingsSeparateRoleAndOperation(t *testing.T) {
	const sql = `SELECT "month" FROM "reporting"."expense"`
	inputs, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	compiler := strings.Repeat("c", 64)

	visible, err := preparedTargetSHA256(RoleVisible, sql, inputs, compiler)
	if err != nil {
		t.Fatal(err)
	}
	companion, err := preparedTargetSHA256(RoleCompanion, sql, inputs, compiler)
	if err != nil {
		t.Fatal(err)
	}
	if visible == companion {
		t.Fatal("the same statement produced one binding in both roles")
	}
	elsewhere, err := preparedTargetSHA256(RoleVisible, sql, other, compiler)
	if err != nil {
		t.Fatal(err)
	}
	if visible == elsewhere {
		t.Fatal("the same statement prepared from other inputs produced the same target binding")
	}
	recompiled, err := preparedTargetSHA256(RoleVisible, sql, inputs, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	if visible == recompiled {
		t.Fatal("the same statement from another compiler produced the same target binding")
	}
	absent, err := preparedTargetSHA256(RoleCompanion, "", inputs, compiler)
	if err != nil {
		t.Fatal(err)
	}
	if absent != "" {
		t.Fatal("an absent statement produced a target binding")
	}
}

// Projection order is part of the identity: the same columns in another order
// return a different result.
func TestFieldListDigestsAreOrderSensitive(t *testing.T) {
	first, err := fieldListSHA256([]string{"month", "amount"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fieldListSHA256([]string{"amount", "month"})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two different projections share a digest")
	}
	empty, err := fieldListSHA256(nil)
	if err != nil {
		t.Fatal(err)
	}
	if empty != "" {
		t.Fatal("an empty projection produced a digest")
	}
}

// Sidecar grants are a set, so their digest must not depend on the order the
// caller happened to assemble them in.
func TestSidecarGrantDigestsAreOrderInsensitive(t *testing.T) {
	left := []sqlpolicy.ProductGrant{
		{LogicalName: "b", ApprovedColumns: []string{"y", "x"}, AllowedOperators: []string{"="}},
		{LogicalName: "a", ApprovedColumns: []string{"row_handle"}, AllowedOperators: []string{"="}},
	}
	right := []sqlpolicy.ProductGrant{
		{LogicalName: "a", ApprovedColumns: []string{"row_handle"}, AllowedOperators: []string{"="}},
		{LogicalName: "b", ApprovedColumns: []string{"x", "y"}, AllowedOperators: []string{"="}},
	}
	first, err := sidecarGrantsSHA256(left)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sidecarGrantsSHA256(right)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("two orderings of one sidecar grant set produced different digests")
	}
}
