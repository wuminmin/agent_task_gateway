package querybinding

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// testCompiler is a sealed compiler identity. Every V2 in this file is compiled
// against it, so a test that changes a renderer member is changing exactly one
// thing.
func testCompiler(t *testing.T) preparedbinding.CompilerIdentityV1 {
	t.Helper()
	sealed, err := preparedbinding.CompilerIdentityV1{
		QueryPlanVersion:      "queryplan-v7",
		QueryPlanSHA256:       digestOf("1"),
		PolicyRendererVersion: "sqlpolicy-v3",
		PolicyRendererSHA256:  digestOf("a3"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal compiler identity: %v", err)
	}
	return sealed
}

// testPrepared is a paired semantic-View preparation with every optional member
// populated.
//
// Every member is filled deliberately. A baseline that left the optional
// digests empty would make the mutation sweep below prove only that the members
// it happened to populate are covered, which is the failure mode the sweep
// exists to catch.
func testPrepared(t *testing.T) preparedbinding.PreparedOperationBindingV1 {
	t.Helper()
	compiler := testCompiler(t)
	sealed, err := preparedbinding.PreparedOperationBindingV1{
		HasCompanion: true, Grouped: true, ExpandedEvidence: true,

		VisibleFieldCount: 4, FactFieldCount: 2, ProvenanceFieldCount: 3,
		VisibleFieldsSHA256:    digestOf("11"),
		FactFieldsSHA256:       digestOf("12"),
		ProvenanceFieldsSHA256: digestOf("13"),

		PreparationInputsSHA256:  digestOf("14"),
		GrantSHA256:              digestOf("15"),
		CatalogSHA256:            digestOf("16"),
		SnapshotBindingSetSHA256: digestOf("17"),
		PlanSHA256:               digestOf("18"),
		CompilerIdentitySHA256:   compiler.SHA256,

		PolicyGrantSHA256:          digestOf("19"),
		NormalFormSHA256:           digestOf("1a"),
		OrdinalProgramSHA256:       digestOf("1b"),
		DictionarySetSHA256:        digestOf("1c"),
		SidecarGrantsSHA256:        digestOf("1d"),
		SourcePublicationsSHA256:   digestOf("1e"),
		ViewBindingSHA256:          digestOf("1f"),
		ViewRegistryRevisionSHA256: digestOf("21"),

		ViewArtifactSHA256:           digestOf("22"),
		ViewCompositionSHA256:        digestOf("23"),
		TerminalProductClosureSHA256: digestOf("24"),
		GovernanceEnvelopeSHA256:     digestOf("25"),

		PredicateFootprintSHA256: digestOf("26"),
		EstimatedBaseFacts:       4096,

		VisibleTargetSHA256:   digestOf("a4"),
		CompanionTargetSHA256: digestOf("b4"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal prepared operation binding: %v", err)
	}
	if err := sealed.Validate(); err != nil {
		t.Fatalf("the baseline preparation does not validate: %v", err)
	}
	return sealed
}

// v2Target is testTarget with the renderer members pinned to the compiler
// identity the preparation names.
//
// The V1 fixture gives the two targets different renderer digests, which is not
// a shape any execution can produce: one renderer renders both statements of an
// operation. V1 never noticed because it had nothing to check them against;
// V2 does, so the fixture has to describe a real execution.
func v2Target(t *testing.T, role TargetRole, rowLimit int64, executed bool) TargetRecordV1 {
	t.Helper()
	compiler := testCompiler(t)
	target := testTarget(role, rowLimit, executed)
	target.PolicyRendererVersion = compiler.PolicyRendererVersion
	target.PolicyRendererDigest = compiler.PolicyRendererSHA256
	return target
}

// pairedNovelV2 mirrors pairedNovel: expanded evidence, so the companion's
// policy limit is its evidence rows plus one. The prepared target digests match
// testTarget's, which is what ties the two halves together.
func pairedNovelV2(t *testing.T) QueryExecutionBindingV2 {
	t.Helper()
	ledger := testLedger(t)
	companion := v2Target(t, RoleCompanion, 33, true)
	sealed, err := QueryExecutionBindingV2{
		PathKind:               PathPairedNovel,
		PreparedOperation:      testPrepared(t),
		Compiler:               testCompiler(t),
		ExposureProfileVersion: ledger.ProfileVersion,
		VisibleRowLimit:        200,
		CompanionEvidenceRows:  32,
		CompanionPolicyRows:    33,
		BudgetBeforeSHA256:     digestOf("d"),

		ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible:                    v2Target(t, RoleVisible, 200, true),
		Companion:                  &companion,
	}.Seal()
	if err != nil {
		t.Fatalf("seal paired-novel V2 binding: %v", err)
	}
	return sealed
}

func (binding QueryExecutionBindingV2) withCopiedCompanion() QueryExecutionBindingV2 {
	if binding.Companion != nil {
		companion := *binding.Companion
		binding.Companion = &companion
	}
	return binding
}

func TestExecutionBindingV2SealsAndValidates(t *testing.T) {
	binding := pairedNovelV2(t)
	if err := binding.Validate(); err != nil {
		t.Fatalf("a sealed V2 binding does not validate: %v", err)
	}
	if !validSHA256(binding.SHA256) {
		t.Fatalf("the sealed digest %q is not a SHA-256", binding.SHA256)
	}
	if binding.Version != QueryExecutionBindingV2Version {
		t.Fatalf("Seal left version %q", binding.Version)
	}
	if !binding.UsesExpandedEvidence() {
		t.Fatal("the baseline preparation expands evidence but the binding reports it does not")
	}
}

// The whole point of V2. Every member of the prepared binding must be covered:
// mutating one either moves the V2 digest or fails validation, while the V1
// binding that names the preparation only by digest is left completely unmoved.
func TestV2CoversEveryPreparedBindingMemberThatV1CannotSee(t *testing.T) {
	mutations := map[string]func(*preparedbinding.PreparedOperationBindingV1){
		"HasCompanion": func(b *preparedbinding.PreparedOperationBindingV1) {
			b.HasCompanion = false
			b.CompanionTargetSHA256 = ""
			b.ExpandedEvidence = false
		},
		"Grouped":          func(b *preparedbinding.PreparedOperationBindingV1) { b.Grouped = false },
		"ExpandedEvidence": func(b *preparedbinding.PreparedOperationBindingV1) { b.ExpandedEvidence = false },

		"VisibleFieldCount":    func(b *preparedbinding.PreparedOperationBindingV1) { b.VisibleFieldCount = 5 },
		"FactFieldCount":       func(b *preparedbinding.PreparedOperationBindingV1) { b.FactFieldCount = 3 },
		"ProvenanceFieldCount": func(b *preparedbinding.PreparedOperationBindingV1) { b.ProvenanceFieldCount = 4 },

		"VisibleFieldsSHA256":    func(b *preparedbinding.PreparedOperationBindingV1) { b.VisibleFieldsSHA256 = digestOf("9") },
		"FactFieldsSHA256":       func(b *preparedbinding.PreparedOperationBindingV1) { b.FactFieldsSHA256 = digestOf("9") },
		"ProvenanceFieldsSHA256": func(b *preparedbinding.PreparedOperationBindingV1) { b.ProvenanceFieldsSHA256 = digestOf("9") },

		"PreparationInputsSHA256":  func(b *preparedbinding.PreparedOperationBindingV1) { b.PreparationInputsSHA256 = digestOf("9") },
		"GrantSHA256":              func(b *preparedbinding.PreparedOperationBindingV1) { b.GrantSHA256 = digestOf("9") },
		"CatalogSHA256":            func(b *preparedbinding.PreparedOperationBindingV1) { b.CatalogSHA256 = digestOf("9") },
		"SnapshotBindingSetSHA256": func(b *preparedbinding.PreparedOperationBindingV1) { b.SnapshotBindingSetSHA256 = digestOf("9") },
		"PlanSHA256":               func(b *preparedbinding.PreparedOperationBindingV1) { b.PlanSHA256 = digestOf("9") },
		"CompilerIdentitySHA256":   func(b *preparedbinding.PreparedOperationBindingV1) { b.CompilerIdentitySHA256 = digestOf("9") },

		"PolicyGrantSHA256":          func(b *preparedbinding.PreparedOperationBindingV1) { b.PolicyGrantSHA256 = digestOf("9") },
		"NormalFormSHA256":           func(b *preparedbinding.PreparedOperationBindingV1) { b.NormalFormSHA256 = digestOf("9") },
		"OrdinalProgramSHA256":       func(b *preparedbinding.PreparedOperationBindingV1) { b.OrdinalProgramSHA256 = digestOf("9") },
		"DictionarySetSHA256":        func(b *preparedbinding.PreparedOperationBindingV1) { b.DictionarySetSHA256 = digestOf("9") },
		"SidecarGrantsSHA256":        func(b *preparedbinding.PreparedOperationBindingV1) { b.SidecarGrantsSHA256 = digestOf("9") },
		"SourcePublicationsSHA256":   func(b *preparedbinding.PreparedOperationBindingV1) { b.SourcePublicationsSHA256 = digestOf("9") },
		"ViewBindingSHA256":          func(b *preparedbinding.PreparedOperationBindingV1) { b.ViewBindingSHA256 = digestOf("9") },
		"ViewRegistryRevisionSHA256": func(b *preparedbinding.PreparedOperationBindingV1) { b.ViewRegistryRevisionSHA256 = digestOf("9") },

		"ViewArtifactSHA256":           func(b *preparedbinding.PreparedOperationBindingV1) { b.ViewArtifactSHA256 = digestOf("9") },
		"ViewCompositionSHA256":        func(b *preparedbinding.PreparedOperationBindingV1) { b.ViewCompositionSHA256 = digestOf("9") },
		"TerminalProductClosureSHA256": func(b *preparedbinding.PreparedOperationBindingV1) { b.TerminalProductClosureSHA256 = digestOf("9") },
		"GovernanceEnvelopeSHA256":     func(b *preparedbinding.PreparedOperationBindingV1) { b.GovernanceEnvelopeSHA256 = digestOf("9") },

		"PredicateFootprintSHA256": func(b *preparedbinding.PreparedOperationBindingV1) { b.PredicateFootprintSHA256 = digestOf("9") },
		"EstimatedBaseFacts":       func(b *preparedbinding.PreparedOperationBindingV1) { b.EstimatedBaseFacts = 4097 },

		"VisibleTargetSHA256":   func(b *preparedbinding.PreparedOperationBindingV1) { b.VisibleTargetSHA256 = digestOf("9") },
		"CompanionTargetSHA256": func(b *preparedbinding.PreparedOperationBindingV1) { b.CompanionTargetSHA256 = digestOf("9") },
	}
	requireEveryPreparedMemberMutated(t, mutations)

	baselineV2 := pairedNovelV2(t)
	baselineV1 := pairedNovel(t)
	// The V1 baseline names the same preparation the V2 baseline carries. That is
	// the comparison being made: one signs the digest, the other signs the
	// document.
	if baselineV1.PreparedOperationBindingSHA256 == baselineV2.PreparedOperation.SHA256 {
		t.Fatal("the fixtures accidentally share a prepared digest; rewrite the test")
	}
	prepared := testPrepared(t)
	namedV1, err := rebindV1To(baselineV1, prepared.SHA256)
	if err != nil {
		t.Fatalf("rebind the V1 baseline onto the preparation: %v", err)
	}
	// The V1 receipt material, as a holder would receive it. A V1 binding carries
	// no preparation document, so these bytes are the whole of what a verifier has
	// to work with when someone hands it a preparation alongside.
	namedV1Bytes, err := json.Marshal(namedV1)
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutated := prepared
			mutate(&mutated)
			resealed, sealErr := mutated.Seal()
			if sealErr != nil {
				t.Fatalf("reseal the mutated preparation: %v", sealErr)
			}
			if resealed.SHA256 == prepared.SHA256 {
				t.Fatalf("mutating %s did not move the preparation's own digest; "+
					"PreparedOperationBindingV1.Seal does not cover it", name)
			}

			// V1 is blind by construction. It names the ORIGINAL preparation by
			// digest and carries no document, so a holder handed the MUTATED
			// preparation receives byte-identical V1 material and a binding that
			// still validates. There is no member of V1 the substitution moves.
			//
			// Asserting the bytes rather than a recomputed digest is the point: the
			// question is what a verifier is given, not what it could recompute if
			// it already had the right preparation.
			if err := namedV1.Validate(); err != nil {
				t.Fatalf("the V1 binding stopped validating for a reason it cannot see: %v", err)
			}
			currentV1Bytes, marshalErr := json.Marshal(namedV1)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if !bytes.Equal(currentV1Bytes, namedV1Bytes) {
				t.Fatal("the V1 fixture is not deterministic; the comparison below means nothing")
			}

			// V2 either refuses the mutated preparation or seals to a different
			// digest. Both are acceptable; agreeing with the baseline is not.
			candidate := baselineV2.withCopiedCompanion()
			candidate.PreparedOperation = resealed
			sealedV2, v2Err := candidate.Seal()
			if v2Err != nil {
				return
			}
			if sealedV2.SHA256 == baselineV2.SHA256 {
				t.Fatalf("mutating %s left the V2 digest unchanged and the binding valid; V2 does not cover it", name)
			}
		})
	}
}

// requireEveryPreparedMemberMutated fails when a member is added to
// PreparedOperationBindingV1 without a mutation case above.
//
// Without it the sweep would quietly shrink in coverage exactly when the durable
// binding grows -- which is the moment the coverage matters most.
func requireEveryPreparedMemberMutated(t *testing.T,
	mutations map[string]func(*preparedbinding.PreparedOperationBindingV1)) {
	t.Helper()
	// Version and SHA256 are derived by Seal from the members, never supplied, so
	// there is no independent mutation of them to make.
	derived := map[string]bool{"Version": true, "SHA256": true}
	structure := reflect.TypeOf(preparedbinding.PreparedOperationBindingV1{})
	var missing []string
	for index := 0; index < structure.NumField(); index++ {
		name := structure.Field(index).Name
		if derived[name] {
			continue
		}
		if _, covered := mutations[name]; !covered {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("PreparedOperationBindingV1 gained members with no mutation case: %v", missing)
	}
	for name := range mutations {
		if _, exists := structure.FieldByName(name); !exists && !derived[name] {
			t.Fatalf("the mutation sweep names %q, which PreparedOperationBindingV1 does not have", name)
		}
	}
}

func rebindV1To(base QueryExecutionBindingV1, preparedSHA256 string) (QueryExecutionBindingV1, error) {
	rebound := base
	if rebound.Companion != nil {
		companion := *rebound.Companion
		rebound.Companion = &companion
	}
	rebound.PreparedOperationBindingSHA256 = preparedSHA256
	return rebound.Seal()
}

// A preparation whose member was edited without resealing must be refused
// outright. This is the other half of the coverage: the digest catches a
// resealed edit, and Validate catches an unresealed one.
func TestV2RejectsAPreparationEditedWithoutResealing(t *testing.T) {
	binding := pairedNovelV2(t)
	binding.PreparedOperation.EstimatedBaseFacts = 4097
	if err := binding.Validate(); err == nil {
		t.Fatal("a preparation edited without resealing was accepted")
	}
}

func TestV2RequiresTheTargetsToBeThePreparedOnes(t *testing.T) {
	for name, mutate := range map[string]func(*QueryExecutionBindingV2){
		"visible rendered from another prepared target": func(b *QueryExecutionBindingV2) {
			b.Visible.PreparedTargetBindingSHA256 = digestOf("9")
		},
		"companion rendered from another prepared target": func(b *QueryExecutionBindingV2) {
			b.Companion.PreparedTargetBindingSHA256 = digestOf("9")
		},
		"companion present but never prepared": func(b *QueryExecutionBindingV2) {
			prepared := b.PreparedOperation
			prepared.HasCompanion = false
			prepared.CompanionTargetSHA256 = ""
			prepared.ExpandedEvidence = false
			resealed, err := prepared.Seal()
			if err != nil {
				panic(err)
			}
			b.PreparedOperation = resealed
		},
		"companion prepared but not bound": func(b *QueryExecutionBindingV2) {
			b.PathKind = PathSingleQuery
			b.Companion = nil
			b.CompanionEvidenceRows = 0
			b.CompanionPolicyRows = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := pairedNovelV2(t).withCopiedCompanion()
			mutate(&mutated)
			if _, err := mutated.Seal(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestV2RequiresTheRendererThatCompiledThePreparation(t *testing.T) {
	for name, mutate := range map[string]func(*QueryExecutionBindingV2){
		"visible renderer version":   func(b *QueryExecutionBindingV2) { b.Visible.PolicyRendererVersion = "sqlpolicy-v4" },
		"visible renderer digest":    func(b *QueryExecutionBindingV2) { b.Visible.PolicyRendererDigest = digestOf("9") },
		"companion renderer version": func(b *QueryExecutionBindingV2) { b.Companion.PolicyRendererVersion = "sqlpolicy-v4" },
		"companion renderer digest":  func(b *QueryExecutionBindingV2) { b.Companion.PolicyRendererDigest = digestOf("9") },
		"compiler identity is not the prepared one": func(b *QueryExecutionBindingV2) {
			other, err := preparedbinding.CompilerIdentityV1{
				QueryPlanVersion: "queryplan-v8", QueryPlanSHA256: digestOf("9"),
				PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererSHA256: digestOf("a3"),
			}.Seal()
			if err != nil {
				panic(err)
			}
			b.Compiler = other
		},
		"compiler identity digest was supplied rather than sealed": func(b *QueryExecutionBindingV2) {
			b.Compiler.SHA256 = digestOf("9")
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := pairedNovelV2(t).withCopiedCompanion()
			mutate(&mutated)
			if _, err := mutated.Seal(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

func TestV2EnforcesPathSemantics(t *testing.T) {
	t.Run("idempotent replay creates no binding", func(t *testing.T) {
		binding := pairedNovelV2(t).withCopiedCompanion()
		binding.PathKind = PathIdempotentReplay
		if _, err := binding.Seal(); err == nil {
			t.Fatal("an idempotent_replay binding was sealed")
		}
	})
	t.Run("semantic replay executes nothing", func(t *testing.T) {
		binding := pairedNovelV2(t).withCopiedCompanion()
		binding.PathKind = PathSemanticReplay
		if _, err := binding.Seal(); err == nil {
			t.Fatal("a semantic_replay binding with executed targets was sealed")
		}
		binding.Visible.Executed = false
		binding.Companion.Executed = false
		replay, err := binding.Seal()
		if err != nil {
			t.Fatalf("a coherent semantic replay was refused: %v", err)
		}
		if replay.Visible.Executed || replay.Companion.Executed {
			t.Fatal("the sealed replay claims an execution")
		}
	})
	t.Run("paired novel executes both", func(t *testing.T) {
		binding := pairedNovelV2(t).withCopiedCompanion()
		binding.Companion.Executed = false
		if _, err := binding.Seal(); err == nil {
			t.Fatal("a paired_novel binding with an unexecuted companion was sealed")
		}
	})
}

// A single-query V2: no companion prepared, no companion bound, no expanded
// evidence. This is the shape an inline-delivery Scale or ProvSQL operation
// produces, and it must be representable.
func TestV2RepresentsASingleQueryOperation(t *testing.T) {
	compiler := testCompiler(t)
	prepared, err := preparedbinding.PreparedOperationBindingV1{
		HasCompanion: false, Grouped: false, ExpandedEvidence: false,
		VisibleFieldCount:       3,
		VisibleFieldsSHA256:     digestOf("11"),
		PreparationInputsSHA256: digestOf("14"),
		GrantSHA256:             digestOf("15"),
		CatalogSHA256:           digestOf("16"),
		PlanSHA256:              digestOf("18"),
		CompilerIdentitySHA256:  compiler.SHA256,
		PolicyGrantSHA256:       digestOf("19"),
		NormalFormSHA256:        digestOf("1a"),
		VisibleTargetSHA256:     digestOf("a4"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal the single-query preparation: %v", err)
	}
	ledger := testLedger(t)
	binding, err := QueryExecutionBindingV2{
		PathKind:               PathSingleQuery,
		PreparedOperation:      prepared,
		Compiler:               compiler,
		ExposureProfileVersion: ledger.ProfileVersion,
		VisibleRowLimit:        200,
		BudgetBeforeSHA256:     digestOf("d"),

		ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible:                    v2Target(t, RoleVisible, 200, true),
	}.Seal()
	if err != nil {
		t.Fatalf("a single-query V2 was refused: %v", err)
	}
	if binding.UsesExpandedEvidence() {
		t.Fatal("a companion-less operation reports expanded evidence")
	}
}

func TestV2CompanionRowLimitsFollowTheExpandedEvidenceRule(t *testing.T) {
	// Expanded evidence: policy rows are evidence rows plus one.
	binding := pairedNovelV2(t).withCopiedCompanion()
	binding.CompanionPolicyRows = 32
	binding.Companion.RowLimit = 32
	if _, err := binding.Seal(); err == nil {
		t.Fatal("an expanded-evidence companion whose policy limit did not exceed its evidence rows was sealed")
	}

	// Without expanded evidence the two are equal, and the preparation is the
	// only place that state is written down.
	unexpanded := pairedNovelV2(t).withCopiedCompanion()
	prepared := unexpanded.PreparedOperation
	prepared.ExpandedEvidence = false
	resealed, err := prepared.Seal()
	if err != nil {
		t.Fatal(err)
	}
	unexpanded.PreparedOperation = resealed
	if _, err := unexpanded.Seal(); err == nil {
		t.Fatal("an unexpanded companion kept the plus-one policy limit and was sealed")
	}
	unexpanded.CompanionPolicyRows = 32
	unexpanded.Companion.RowLimit = 32
	if _, err := unexpanded.Seal(); err != nil {
		t.Fatalf("a coherent unexpanded companion was refused: %v", err)
	}
}

func TestV2DigestCoversItsOwnMembers(t *testing.T) {
	base := pairedNovelV2(t)
	for name, mutate := range map[string]func(*QueryExecutionBindingV2){
		"path kind": func(b *QueryExecutionBindingV2) {
			b.PathKind = PathSingleQuery
			b.Companion = nil
			b.CompanionEvidenceRows, b.CompanionPolicyRows = 0, 0
		},
		"exposure profile":  func(b *QueryExecutionBindingV2) { b.ExposureProfileVersion = "taskgate-exposure-v4" },
		"visible row limit": func(b *QueryExecutionBindingV2) { b.VisibleRowLimit = 199; b.Visible.RowLimit = 199 },
		"companion evidence rows": func(b *QueryExecutionBindingV2) {
			b.CompanionEvidenceRows = 31
			b.CompanionPolicyRows = 32
			b.Companion.RowLimit = 32
		},
		"budget before":     func(b *QueryExecutionBindingV2) { b.BudgetBeforeSHA256 = digestOf("9") },
		"ledger before":     func(b *QueryExecutionBindingV2) { b.ExposureLedgerBeforeSHA256 = digestOf("9") },
		"visible exact":     func(b *QueryExecutionBindingV2) { b.Visible.ExactSQLSHA256 = digestOf("9") },
		"visible strict":    func(b *QueryExecutionBindingV2) { b.Visible.StrictASTSHA256 = digestOf("9") },
		"visible policy fp": func(b *QueryExecutionBindingV2) { b.Visible.PolicyFingerprint = "fp-other" },
		"companion exact":   func(b *QueryExecutionBindingV2) { b.Companion.ExactSQLSHA256 = digestOf("9") },
		"companion strict":  func(b *QueryExecutionBindingV2) { b.Companion.StrictASTSHA256 = digestOf("9") },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base.withCopiedCompanion()
			mutate(&mutated)
			if err := mutated.Validate(); err == nil {
				t.Fatalf("mutating %s left the binding valid; the digest does not cover it", name)
			}
		})
	}
}

// V2 carries a whole preparation now, so the no-SQL property has to be asserted
// over the larger document rather than inherited from V1's.
func TestV2CarriesNoSQLAndNoNames(t *testing.T) {
	encoded, err := json.Marshal(pairedNovelV2(t))
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(string(encoded))
	for _, fragment := range []string{"select ", " from ", " where ", "insert ", "update ", "--", "/*"} {
		if strings.Contains(lowered, fragment) {
			t.Fatalf("the V2 binding carries the SQL fragment %q: %s", fragment, encoded)
		}
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	prepared := map[string]json.RawMessage{}
	if err := json.Unmarshal(document["prepared_operation"], &prepared); err != nil {
		t.Fatal(err)
	}
	for field := range prepared {
		for _, forbidden := range []string{"fact_id", "bitmap", "members", "payload", "sql_text", "column", "relation"} {
			if strings.Contains(field, forbidden) {
				t.Errorf("the carried preparation exposes a %q field", field)
			}
		}
	}
}

// V2 must not reintroduce the identities the preparation already carries. Two
// sources for one fact is the defect the whole redesign removes, and a later
// change that "helpfully" adds plan_sha256 back for readability would restore it.
func TestV2DoesNotRepeatPreparedIdentities(t *testing.T) {
	structure := reflect.TypeOf(QueryExecutionBindingV2{})
	for _, forbidden := range []string{
		"PlanSHA256", "CompilerVersion", "CompilerSHA256", "OrdinalDictionarySetSHA256",
		"SidecarGrantsSHA256", "PreparedOperationBindingSHA256", "UsesExpandedEvidence",
	} {
		if _, exists := structure.FieldByName(forbidden); exists {
			t.Errorf("QueryExecutionBindingV2 carries %s, which PreparedOperationBindingV1 already contains",
				forbidden)
		}
	}
}
