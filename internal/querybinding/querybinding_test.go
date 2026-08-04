package querybinding

import (
	"encoding/json"
	"strings"
	"testing"
)

func digestOf(seed string) string { return strings.Repeat(seed, 64/len(seed)) }

func testLedger(t *testing.T) ExposureLedgerBeforeV1 {
	t.Helper()
	sealed, err := ExposureLedgerBeforeV1{
		ProfileVersion: "taskgate-exposure-v5",
		RootTaskID:     "task-root-1",
		RootEpoch:      3,
		Limits: FactVector{ReleaseFacts: 500, InfluenceFacts: 40, OutcomeFacts: 10,
			PredicateAtoms: 25, CompositeOutcomes: 5},
		Used: FactVector{ReleaseFacts: 100, InfluenceFacts: 8, OutcomeFacts: 2,
			PredicateAtoms: 5, CompositeOutcomes: 1},
		Remaining: FactVector{ReleaseFacts: 400, InfluenceFacts: 32, OutcomeFacts: 8,
			PredicateAtoms: 20, CompositeOutcomes: 4},
		RemainingRows:        1000,
		UsesExpandedEvidence: true,
		HasExposureContext:   true,
	}.Seal()
	if err != nil {
		t.Fatalf("seal exposure ledger pre-state: %v", err)
	}
	return sealed
}

func testTarget(role TargetRole, rowLimit int64, executed bool) TargetRecordV1 {
	seed := "a"
	if role == RoleCompanion {
		seed = "b"
	}
	return TargetRecordV1{
		Role: role, Authorized: true, Executed: executed,
		ExactSQLSHA256:              digestOf(seed + "1"),
		StrictASTSHA256:             digestOf(seed + "2"),
		RowLimit:                    rowLimit,
		PolicyFingerprint:           "fp-" + string(role),
		PolicyRendererVersion:       "sqlpolicy-v3",
		PolicyRendererDigest:        digestOf(seed + "3"),
		PreparedTargetBindingSHA256: digestOf(seed + "4"),
	}
}

// pairedNovel is the shape of a Result-heavy paired-novel execution: expanded
// evidence, so the companion's policy limit is its evidence rows plus one.
func pairedNovel(t *testing.T) QueryExecutionBindingV1 {
	t.Helper()
	ledger := testLedger(t)
	companion := testTarget(RoleCompanion, 33, true)
	sealed, err := QueryExecutionBindingV1{
		PathKind:                       PathPairedNovel,
		PreparedOperationBindingSHA256: digestOf("c"),
		ExposureProfileVersion:         ledger.ProfileVersion,
		UsesExpandedEvidence:           true,
		VisibleRowLimit:                200,
		CompanionEvidenceRows:          32,
		CompanionPolicyRows:            33,
		BudgetBeforeSHA256:             digestOf("d"),
		ExposureLedgerBeforeSHA256:     ledger.SHA256,
		PlanSHA256:                     digestOf("e"),
		CompilerVersion:                "queryplan-v7",
		CompilerSHA256:                 digestOf("f"),
		Visible:                        testTarget(RoleVisible, 200, true),
		Companion:                      &companion,
	}.Seal()
	if err != nil {
		t.Fatalf("seal paired-novel binding: %v", err)
	}
	return sealed
}

func TestExposureLedgerBeforeSealsAndValidates(t *testing.T) {
	ledger := testLedger(t)
	if err := ledger.Validate(); err != nil {
		t.Fatalf("a well-formed pre-state was rejected: %v", err)
	}
	if len(ledger.SHA256) != 64 {
		t.Fatalf("the pre-state digest is %q", ledger.SHA256)
	}
}

// The three vectors must agree. Remaining is what a caller would have to forge
// to widen a row limit, so it must not be independently assertable.
func TestExposureLedgerBeforeRequiresRemainingToEqualLimitsMinusUsed(t *testing.T) {
	ledger := testLedger(t)
	ledger.Remaining.InfluenceFacts = 999
	if err := ledger.Validate(); err == nil {
		t.Fatal("a pre-state whose remaining vector contradicted its limits was accepted")
	}
	if _, err := ledger.Seal(); err == nil {
		t.Fatal("an incoherent pre-state sealed cleanly")
	}
}

func TestExposureLedgerBeforeFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*ExposureLedgerBeforeV1){
		"no profile":     func(l *ExposureLedgerBeforeV1) { l.ProfileVersion = "" },
		"no root task":   func(l *ExposureLedgerBeforeV1) { l.RootTaskID = "" },
		"negative epoch": func(l *ExposureLedgerBeforeV1) { l.RootEpoch = -1 },
		"negative rows":  func(l *ExposureLedgerBeforeV1) { l.RemainingRows = -1 },
		"negative limit": func(l *ExposureLedgerBeforeV1) { l.Limits.ReleaseFacts = -1 },
		"wrong version":  func(l *ExposureLedgerBeforeV1) { l.Version = "taskgate-exposure-ledger-before-v0" },
		"expanded without context": func(l *ExposureLedgerBeforeV1) {
			l.HasExposureContext = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := testLedger(t)
			mutate(&ledger)
			if err := ledger.Validate(); err == nil {
				t.Fatal("an invalid exposure pre-state validated")
			}
		})
	}
}

// The digest must cover every member, or a mutated pre-state would still verify.
func TestExposureLedgerBeforeDigestCoversEveryMember(t *testing.T) {
	base := testLedger(t)
	for name, mutate := range map[string]func(*ExposureLedgerBeforeV1){
		"profile":   func(l *ExposureLedgerBeforeV1) { l.ProfileVersion = "taskgate-exposure-v4" },
		"root task": func(l *ExposureLedgerBeforeV1) { l.RootTaskID = "task-root-2" },
		"epoch":     func(l *ExposureLedgerBeforeV1) { l.RootEpoch = 4 },
		"limits": func(l *ExposureLedgerBeforeV1) {
			l.Limits.InfluenceFacts, l.Remaining.InfluenceFacts = 41, 33
		},
		"used": func(l *ExposureLedgerBeforeV1) {
			l.Used.ReleaseFacts, l.Remaining.ReleaseFacts = 101, 399
		},
		"remaining rows": func(l *ExposureLedgerBeforeV1) { l.RemainingRows = 999 },
		"expanded flag":  func(l *ExposureLedgerBeforeV1) { l.UsesExpandedEvidence = false },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			// The digest is left at its original value on purpose: a member that
			// can change without changing the digest is a member the signature
			// does not protect.
			if err := mutated.Validate(); err == nil {
				t.Fatalf("mutating %s left the pre-state valid; the digest does not cover it", name)
			}
			resealed, err := mutated.Seal()
			if err != nil {
				t.Fatalf("reseal: %v", err)
			}
			if resealed.SHA256 == base.SHA256 {
				t.Fatalf("mutating %s did not change the pre-state digest", name)
			}
		})
	}
}

func TestExecutionBindingSealsAndValidates(t *testing.T) {
	binding := pairedNovel(t)
	if err := binding.Validate(); err != nil {
		t.Fatalf("a well-formed paired-novel binding was rejected: %v", err)
	}
	if !binding.Equal(binding) {
		t.Fatal("a binding is not equal to itself")
	}
}

// Path semantics are enforced by the binding, so no consumer has to re-derive
// them and get it wrong permissively.
func TestExecutionBindingEnforcesPathSemantics(t *testing.T) {
	t.Run("paired_novel executes both targets", func(t *testing.T) {
		binding := pairedNovel(t)
		binding.Companion.Executed = false
		if _, err := binding.Seal(); err == nil {
			t.Fatal("paired_novel accepted an unexecuted companion")
		}
		binding = pairedNovel(t)
		binding.Companion = nil
		binding.CompanionEvidenceRows, binding.CompanionPolicyRows = 0, 0
		binding.UsesExpandedEvidence = false
		if _, err := binding.Seal(); err == nil {
			t.Fatal("paired_novel accepted a binding with no companion")
		}
	})

	t.Run("single_query has no companion", func(t *testing.T) {
		binding := pairedNovel(t)
		binding.PathKind = PathSingleQuery
		if _, err := binding.Seal(); err == nil {
			t.Fatal("single_query accepted a companion target")
		}
		binding.Companion = nil
		binding.CompanionEvidenceRows, binding.CompanionPolicyRows = 0, 0
		binding.UsesExpandedEvidence = false
		if _, err := binding.Seal(); err != nil {
			t.Fatalf("a well-formed single_query binding was rejected: %v", err)
		}
	})

	t.Run("semantic_replay executes nothing", func(t *testing.T) {
		binding := pairedNovel(t)
		binding.PathKind = PathSemanticReplay
		// Executing under a replay is precisely the failure this rejects: the
		// observer must see a zero target delta.
		if _, err := binding.Seal(); err == nil {
			t.Fatal("semantic_replay accepted an executed visible target")
		}
		binding.Visible.Executed = false
		if _, err := binding.Seal(); err == nil {
			t.Fatal("semantic_replay accepted an executed companion target")
		}
		binding.Companion.Executed = false
		sealed, err := binding.Seal()
		if err != nil {
			t.Fatalf("a semantic_replay that authorized but did not execute was rejected: %v", err)
		}
		// The targets remain authorized: deriving the semantic key requires it.
		if !sealed.Visible.Authorized || !sealed.Companion.Authorized {
			t.Fatal("semantic_replay lost its authorization evidence")
		}
	})

	t.Run("idempotent_replay creates no binding", func(t *testing.T) {
		binding := pairedNovel(t)
		binding.PathKind = PathIdempotentReplay
		if _, err := binding.Seal(); err == nil {
			t.Fatal("idempotent_replay produced a new execution binding")
		}
	})
}

// A target that executed without being authorized is what makes "executed"
// meaningful rather than decorative.
func TestTargetCannotExecuteWithoutAuthorization(t *testing.T) {
	binding := pairedNovel(t)
	binding.Visible.Authorized = false
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a target executed without authorization was accepted")
	}
}

// Under expanded evidence the companion's policy limit is one more than its
// evidence rows, so truncation is detectable. Anywhere else they are equal.
func TestCompanionRowLimitsMustFollowTheExpandedEvidenceRule(t *testing.T) {
	binding := pairedNovel(t)
	binding.CompanionPolicyRows = binding.CompanionEvidenceRows
	binding.Companion.RowLimit = binding.CompanionPolicyRows
	if _, err := binding.Seal(); err == nil {
		t.Fatal("an expanded-evidence companion accepted a policy limit equal to its evidence rows")
	}

	binding = pairedNovel(t)
	binding.UsesExpandedEvidence = false
	binding.CompanionEvidenceRows, binding.CompanionPolicyRows = 33, 33
	if _, err := binding.Seal(); err != nil {
		t.Fatalf("a non-expanded companion with equal limits was rejected: %v", err)
	}

	// An exhausted exposure budget leaves no evidence row at all.
	binding = pairedNovel(t)
	binding.CompanionEvidenceRows, binding.CompanionPolicyRows = 0, 1
	binding.Companion.RowLimit = 1
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a companion with no evidence row was accepted")
	}
}

// The bound limits must equal what the targets actually rendered; otherwise the
// binding could claim one limit while the executed bytes carried another.
func TestBoundLimitsMustMatchTheRenderedTargets(t *testing.T) {
	binding := pairedNovel(t)
	binding.VisibleRowLimit = 199
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a binding whose visible limit differed from its rendered target was accepted")
	}
	binding = pairedNovel(t)
	binding.Companion.RowLimit = 34
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a binding whose companion limit differed from its rendered target was accepted")
	}
}

// The canonical digest must cover every member of the binding and of both
// targets, including the role, so a swap or a single-digest edit is detected.
func TestExecutionBindingDigestCoversEveryMember(t *testing.T) {
	base := pairedNovel(t)
	for name, mutate := range map[string]func(*QueryExecutionBindingV1){
		"path kind":         func(b *QueryExecutionBindingV1) { b.PathKind = PathSingleQuery; b.Companion = nil },
		"prepared binding":  func(b *QueryExecutionBindingV1) { b.PreparedOperationBindingSHA256 = digestOf("9") },
		"exposure profile":  func(b *QueryExecutionBindingV1) { b.ExposureProfileVersion = "taskgate-exposure-v4" },
		"budget before":     func(b *QueryExecutionBindingV1) { b.BudgetBeforeSHA256 = digestOf("9") },
		"ledger before":     func(b *QueryExecutionBindingV1) { b.ExposureLedgerBeforeSHA256 = digestOf("9") },
		"plan":              func(b *QueryExecutionBindingV1) { b.PlanSHA256 = digestOf("9") },
		"compiler version":  func(b *QueryExecutionBindingV1) { b.CompilerVersion = "queryplan-v8" },
		"compiler digest":   func(b *QueryExecutionBindingV1) { b.CompilerSHA256 = digestOf("9") },
		"dictionary set":    func(b *QueryExecutionBindingV1) { b.OrdinalDictionarySetSHA256 = digestOf("9") },
		"sidecar grants":    func(b *QueryExecutionBindingV1) { b.SidecarGrantsSHA256 = digestOf("9") },
		"visible exact":     func(b *QueryExecutionBindingV1) { b.Visible.ExactSQLSHA256 = digestOf("9") },
		"visible strict":    func(b *QueryExecutionBindingV1) { b.Visible.StrictASTSHA256 = digestOf("9") },
		"visible renderer":  func(b *QueryExecutionBindingV1) { b.Visible.PolicyRendererVersion = "sqlpolicy-v4" },
		"visible prepared":  func(b *QueryExecutionBindingV1) { b.Visible.PreparedTargetBindingSHA256 = digestOf("9") },
		"visible policy fp": func(b *QueryExecutionBindingV1) { b.Visible.PolicyFingerprint = "fp-other" },
		"companion exact":   func(b *QueryExecutionBindingV1) { b.Companion.ExactSQLSHA256 = digestOf("9") },
		"companion strict":  func(b *QueryExecutionBindingV1) { b.Companion.StrictASTSHA256 = digestOf("9") },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			if mutated.Companion != nil {
				companion := *mutated.Companion
				mutated.Companion = &companion
			}
			mutate(&mutated)
			if err := mutated.Validate(); err == nil {
				t.Fatalf("mutating %s left the binding valid; the digest does not cover it", name)
			}
		})
	}
}

// Swapping the visible and companion targets must not verify. Their identities
// are role-tagged and the roles are digested, so the swap changes the binding.
func TestSwappingVisibleAndCompanionFails(t *testing.T) {
	base := pairedNovel(t)
	swapped := base
	visible := base.Visible
	companion := *base.Companion

	// Swap the statement identities while keeping each record's declared role.
	swapped.Visible = TargetRecordV1{
		Role: RoleVisible, Authorized: true, Executed: true,
		ExactSQLSHA256: companion.ExactSQLSHA256, StrictASTSHA256: companion.StrictASTSHA256,
		RowLimit: visible.RowLimit, PolicyFingerprint: companion.PolicyFingerprint,
		PolicyRendererVersion:       companion.PolicyRendererVersion,
		PolicyRendererDigest:        companion.PolicyRendererDigest,
		PreparedTargetBindingSHA256: companion.PreparedTargetBindingSHA256,
	}
	swappedCompanion := TargetRecordV1{
		Role: RoleCompanion, Authorized: true, Executed: true,
		ExactSQLSHA256: visible.ExactSQLSHA256, StrictASTSHA256: visible.StrictASTSHA256,
		RowLimit: companion.RowLimit, PolicyFingerprint: visible.PolicyFingerprint,
		PolicyRendererVersion:       visible.PolicyRendererVersion,
		PolicyRendererDigest:        visible.PolicyRendererDigest,
		PreparedTargetBindingSHA256: visible.PreparedTargetBindingSHA256,
	}
	swapped.Companion = &swappedCompanion

	if err := swapped.Validate(); err == nil {
		t.Fatal("a binding with swapped visible and companion statements verified")
	}
	resealed, err := swapped.Seal()
	if err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if resealed.Equal(base) {
		t.Fatal("swapping the visible and companion statements produced the same binding digest")
	}
}

// A record whose role contradicts its position must be refused outright.
func TestTargetRoleMustMatchItsPosition(t *testing.T) {
	binding := pairedNovel(t)
	binding.Visible.Role = RoleCompanion
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a companion-roled record was accepted as the visible target")
	}
	binding = pairedNovel(t)
	binding.Companion.Role = RoleVisible
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a visible-roled record was accepted as the companion target")
	}
}

// A binding with no companion must not collide with one whose companion is
// present: the absent companion is framed explicitly.
func TestAbsentCompanionIsFramedDistinctly(t *testing.T) {
	single := pairedNovel(t)
	single.PathKind = PathSingleQuery
	single.Companion = nil
	single.CompanionEvidenceRows, single.CompanionPolicyRows = 0, 0
	single.UsesExpandedEvidence = false
	sealedSingle, err := single.Seal()
	if err != nil {
		t.Fatalf("seal single_query: %v", err)
	}
	if sealedSingle.Equal(pairedNovel(t)) {
		t.Fatal("a single_query binding collided with a paired_novel one")
	}
}

// Nothing in the binding may carry SQL. The receipt is retained, replayed and
// handed to a finalizer that must not learn what was queried.
func TestNoStructureCarriesSQL(t *testing.T) {
	binding := pairedNovel(t)
	ledger := testLedger(t)
	for name, value := range map[string]any{"binding": binding, "ledger": ledger} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		lowered := strings.ToLower(string(encoded))
		for _, fragment := range []string{"select ", " from ", " where ", "insert ", "update ", "--", "/*"} {
			if strings.Contains(lowered, fragment) {
				t.Fatalf("the %s carries the SQL fragment %q: %s", name, fragment, encoded)
			}
		}
		// The structures have no field that could hold a FactID, a bitmap member
		// or a task payload; this asserts the JSON shape stays that way.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		for field := range fields {
			for _, forbidden := range []string{"fact_id", "fact_ids", "bitmap", "members", "payload", "sql_text"} {
				if strings.Contains(field, forbidden) {
					t.Errorf("the %s exposes a %q field", name, field)
				}
			}
		}
	}
}

// The framing must be unambiguous: two different member sets must not be able to
// produce one digest by concatenation.
func TestCanonicalFramingCannotBeCollided(t *testing.T) {
	left := pairedNovel(t)
	left.CompilerVersion = "ab"
	left.ExposureProfileVersion = "cd"
	sealedLeft, err := left.Seal()
	if err != nil {
		t.Fatal(err)
	}
	right := pairedNovel(t)
	right.CompilerVersion = "a"
	right.ExposureProfileVersion = "bcd"
	sealedRight, err := right.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if sealedLeft.Equal(sealedRight) {
		t.Fatal("two distinct bindings collided; the framing is ambiguous")
	}
}

func TestValidateRejectsMalformedDigestsAndMissingIdentities(t *testing.T) {
	for name, mutate := range map[string]func(*QueryExecutionBindingV1){
		"short plan digest":    func(b *QueryExecutionBindingV1) { b.PlanSHA256 = "abc" },
		"uppercase digest":     func(b *QueryExecutionBindingV1) { b.CompilerSHA256 = strings.ToUpper(digestOf("f")) },
		"no compiler version":  func(b *QueryExecutionBindingV1) { b.CompilerVersion = "" },
		"no exposure profile":  func(b *QueryExecutionBindingV1) { b.ExposureProfileVersion = "" },
		"unknown path":         func(b *QueryExecutionBindingV1) { b.PathKind = "streaming" },
		"bad optional digest":  func(b *QueryExecutionBindingV1) { b.SidecarGrantsSHA256 = "xyz" },
		"no policy renderer":   func(b *QueryExecutionBindingV1) { b.Visible.PolicyRendererVersion = "" },
		"zero row limit":       func(b *QueryExecutionBindingV1) { b.Visible.RowLimit = 0; b.VisibleRowLimit = 0 },
		"bad strict digest":    func(b *QueryExecutionBindingV1) { b.Visible.StrictASTSHA256 = "" },
		"bad prepared binding": func(b *QueryExecutionBindingV1) { b.Visible.PreparedTargetBindingSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			binding := pairedNovel(t)
			companion := *binding.Companion
			binding.Companion = &companion
			mutate(&binding)
			if _, err := binding.Seal(); err == nil {
				t.Fatal("an invalid execution binding sealed cleanly")
			}
		})
	}
}
