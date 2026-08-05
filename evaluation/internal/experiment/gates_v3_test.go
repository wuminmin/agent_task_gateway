package experiment

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv9"
)

// gateCase is one complete, honest acceptance input: a signed V9, the verifier
// that accepts it, the Adapter's carried evidence and the finalizer's trusted
// inputs. Every gate below starts from a case that PASSES and then breaks
// exactly one thing, so a rejection can only be attributed to that thing.
type gateCase struct {
	receipt  queryreceipt.QueryReceiptV1
	verifier ReceiptVerifierV3
	carried  CarriedEvidenceV3
	trusted  TrustedInputsV3
}

func (c gateCase) finalize() (FinalizationV3, error) {
	return FinalizeTaskGateObservationV3(c.receipt, c.verifier, c.carried, c.trusted)
}

func trustedFrom(inputs IndependentInputsV3) TrustedInputsV3 {
	return TrustedInputsV3{
		CatalogPath: inputs.CatalogPath, Footprint: inputs.Footprint,
		PostgreSQL: inputs.PostgreSQL, OperationID: inputs.OperationID,
		ContractIdentity: inputs.ContractIdentity,
		VisibleSQL:       inputs.VisibleSQL, CompanionSQL: inputs.CompanionSQL,
		SettlementWroteExecutionBindingRow: true,
	}
}

// carriedFor builds the carried evidence a correct Adapter produces for one path
// kind, mirroring exactly what FinalizeObservationV3 derives.
//
// honestCarriedEvidence is paired-novel-shaped: it always names an
// ExpectedSchema and a footprint on the operation, and always carries a
// companion statement. Both are presence-coupled to the path, so a replay built
// that way fails on the coupling rather than on the property under test.
func carriedFor(t *testing.T, inputs IndependentInputsV3) CarriedEvidenceV3 {
	t.Helper()
	dimensions, known := dimensionsFor(inputs.PathKind)
	if !known {
		t.Fatalf("path_kind %q has no dimensions", inputs.PathKind)
	}
	footprintDigest, err := inputs.Footprint.SHA256()
	if err != nil {
		t.Fatalf("footprint digest: %v", err)
	}
	// Attestation bindings are presence-coupled to the path in both directions:
	// a path that attests must name what against and under which qualification,
	// and one that does not must claim neither.
	operation := OperationIdentity{
		OperationID: inputs.OperationID, PathKind: inputs.PathKind,
		ContractIdentity: inputs.ContractIdentity,
	}
	if dimensions.requiresSchema {
		operation.ExpectedSchemaDigest = inputs.Footprint.ExpectedSchemaDigest
		operation.AttestationFootprintSHA256 = footprintDigest
	}
	plan, err := planFor(inputs.PathKind, inputs.Footprint.ExpectedSchemaEntries,
		inputs.Footprint.ExpectedSchemaDigest, inputs.Footprint)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	targets, err := deriveTargets(inputs, StrictASTDigest)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	manifest, err := BuildClassifierManifest(inputs.Footprint, targets)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	classifier, err := CompileClassifier(operation, manifest)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	var rows []ObserverStructuralRow
	for _, entry := range manifest.Entries {
		calls := plan.Expected()[entry.Class]
		if calls == 0 {
			continue
		}
		rows = append(rows, ObserverStructuralRow{
			StrictASTSHA256: entry.StrictASTSHA256, TopLevel: entry.RequiredTopLevel, Calls: calls,
		})
	}
	carried := CarriedEvidenceV3{
		Arm: ArmTaskGate, Operation: operation, Plan: plan,
		ClassifierManifestSHA256: classifier.ManifestSHA256(),
		ClassifierBindingSHA256:  classifier.BindingSHA256(),
		Window: ObserverWindowV2{
			Before: snapshotOf(t, "before", nil), After: snapshotOf(t, "after", rows),
		},
	}
	if dimensions.visible > 0 {
		strict, _ := StrictASTDigest(inputs.VisibleSQL)
		carried.VisibleStatement = physicalquery.StatementIdentity{
			ExactSHA256: physicalquery.ExactDigest(inputs.VisibleSQL), StrictASTSHA256: strict,
			RowLimit: fixture.VisibleRowLimit, Fingerprint: "fixture-visible-fingerprint",
		}
		carried.VisiblePreparedTargetBindingSHA256 = fixture.PreparedTargetBinding(querybinding.RoleVisible)
	}
	if dimensions.companion > 0 {
		strict, _ := StrictASTDigest(inputs.CompanionSQL)
		carried.CompanionStatement = &physicalquery.StatementIdentity{
			ExactSHA256: physicalquery.ExactDigest(inputs.CompanionSQL), StrictASTSHA256: strict,
			RowLimit: fixture.CompanionRowLimit, Fingerprint: "fixture-companion-fingerprint",
		}
		carried.CompanionPreparedTargetBindingSHA256 = fixture.PreparedTargetBinding(querybinding.RoleCompanion)
	}
	return carried
}

// pairedNovelCase binds a signed paired-novel V9 to the carried evidence a
// correct Adapter would produce for it. The carried statement identities are
// aligned with what the fixture signs, because that is the honest case: a real
// Adapter reads them off the receipt.
func pairedNovelCase(t *testing.T) gateCase {
	t.Helper()
	inputs := finalizerInputs(t)
	carried := carriedFor(t, inputs)

	companionTarget := fixture.Target{
		ExactSQLSHA256:  carried.CompanionStatement.ExactSHA256,
		StrictASTSHA256: carried.CompanionStatement.StrictASTSHA256,
	}
	receipt, err := fixture.PairedNovel(fixture.Options{
		Visible: fixture.Target{
			ExactSQLSHA256:  carried.VisibleStatement.ExactSHA256,
			StrictASTSHA256: carried.VisibleStatement.StrictASTSHA256,
		},
		Companion: &companionTarget,
	})
	if err != nil {
		t.Fatalf("build signed V9: %v", err)
	}
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return gateCase{receipt: receipt, verifier: verifier, carried: carried, trusted: trustedFrom(inputs)}
}

// The honest case must pass, or every rejection below proves nothing.
func TestGateBaselineHonestCaseIsAccepted(t *testing.T) {
	if _, err := pairedNovelCase(t).finalize(); err != nil {
		t.Fatalf("the honest paired-novel case was rejected: %v", err)
	}
}

// mutateTarget applies fn to one role's signed target record and re-signs.
func (c gateCase) mutateTarget(t *testing.T, role querybinding.TargetRole,
	fn func(*querybinding.TargetRecordV1)) (gateCase, error) {
	t.Helper()
	mutated, err := fixture.Mutate(c.receipt, func(b *querybinding.QueryExecutionBindingV1) {
		if role == querybinding.RoleVisible {
			fn(&b.Visible)
			b.VisibleRowLimit = b.Visible.RowLimit
			return
		}
		fn(b.Companion)
		b.CompanionPolicyRows = b.Companion.RowLimit
	})
	if err != nil {
		return gateCase{}, err
	}
	c.receipt = mutated
	return c, nil
}

// targetMutations are the six ways a signed target can be wrong. Each is applied
// independently to a case that otherwise passes.
func targetMutations() map[string]func(*querybinding.TargetRecordV1) {
	return map[string]func(*querybinding.TargetRecordV1){
		"prepared binding": func(r *querybinding.TargetRecordV1) {
			r.PreparedTargetBindingSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("another prepared target")))
		},
		"exact digest": func(r *querybinding.TargetRecordV1) {
			r.ExactSQLSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("another exact statement")))
		},
		"strict digest": func(r *querybinding.TargetRecordV1) {
			r.StrictASTSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte("another structural identity")))
		},
		"row limit": func(r *querybinding.TargetRecordV1) { r.RowLimit++ },
		"role": func(r *querybinding.TargetRecordV1) {
			if r.Role == querybinding.RoleVisible {
				r.Role = querybinding.RoleCompanion
				return
			}
			r.Role = querybinding.RoleVisible
		},
		"policy fingerprint": func(r *querybinding.TargetRecordV1) {
			r.PolicyFingerprint = "another-fingerprint"
		},
	}
}

// runTargetGate applies every mutation to one role and requires each to be
// fatal, whether the signature refuses to be produced at all or acceptance
// rejects the receipt that results.
func runTargetGate(t *testing.T, role querybinding.TargetRole) {
	t.Helper()
	for name, mutate := range targetMutations() {
		t.Run(name, func(t *testing.T) {
			broken, err := pairedNovelCase(t).mutateTarget(t, role, mutate)
			if err != nil {
				// The binding could not even be sealed. That is a stronger
				// result than a rejection: the mutation cannot be signed.
				t.Logf("the mutated binding could not be sealed, which is fatal earlier: %v", err)
				return
			}
			if _, err := broken.finalize(); err == nil {
				t.Fatalf("a %s target with a mutated %s was accepted", role, name)
			}
		})
	}
}

// Gate 18.
func TestGate18WrongVisibleTargetFailsFinalization(t *testing.T) {
	runTargetGate(t, querybinding.RoleVisible)
}

// Gate 18/19 also cover the target's contract identity, which is not a member of
// the signed record: it is derived into the classifier manifest from the
// operation's contract. A carried manifest binding a target to another contract
// must lose to the finalizer's own derivation.
func TestGate18And19RejectAnotherContractIdentityForATarget(t *testing.T) {
	base := pairedNovelCase(t)
	broken := base
	broken.carried.Operation.ContractIdentity = "artifact/another-cell/100x4"
	if _, err := broken.finalize(); err == nil {
		t.Fatal("a target bound to another contract identity was accepted")
	}
}

// Gate 19.
func TestGate19WrongCompanionTargetFailsFinalization(t *testing.T) {
	runTargetGate(t, querybinding.RoleCompanion)
}

// Gate 21. A semantic replay authorizes its targets so the semantic key can be
// derived and executes neither, so the observer must see no target at all.
func TestGate21SemanticReplayAuthorizesWithoutExecuting(t *testing.T) {
	inputs := finalizerInputs(t)
	inputs.PathKind = PathSemanticReplay
	carried := carriedFor(t, inputs)

	companionTarget := fixture.Target{
		ExactSQLSHA256:  fmt.Sprintf("%x", sha256.Sum256([]byte("replay companion"))),
		StrictASTSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("replay companion strict"))),
	}
	receipt, err := fixture.SemanticReplay(fixture.Options{
		Visible: fixture.Target{
			ExactSQLSHA256:  fmt.Sprintf("%x", sha256.Sum256([]byte("replay visible"))),
			StrictASTSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("replay visible strict"))),
		},
		Companion: &companionTarget,
	})
	if err != nil {
		t.Fatalf("build semantic replay: %v", err)
	}
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	replay := gateCase{receipt: receipt, verifier: verifier, carried: carried, trusted: trustedFrom(inputs)}

	finalized, err := replay.finalize()
	if err != nil {
		t.Fatalf("an honest semantic replay was rejected: %v", err)
	}
	// Zero visible and companion delta: nothing ran.
	if got := finalized.Delta.PerClass[V3TargetedVisible]; got != 0 {
		t.Fatalf("semantic replay recorded %d visible target statements", got)
	}
	if got := finalized.Delta.PerClass[V3TargetedCompanion]; got != 0 {
		t.Fatalf("semantic replay recorded %d companion target statements", got)
	}

	// Any executed target must be fatal.
	for _, role := range []querybinding.TargetRole{querybinding.RoleVisible, querybinding.RoleCompanion} {
		t.Run("executed "+string(role), func(t *testing.T) {
			broken, err := replay.mutateTarget(t, role, func(r *querybinding.TargetRecordV1) {
				r.Executed = true
			})
			if err != nil {
				t.Logf("a semantic replay with an executed %s target cannot be signed: %v", role, err)
				return
			}
			if _, err := broken.finalize(); err == nil {
				t.Fatalf("a semantic replay with an executed %s target was accepted", role)
			}
		})
	}

	// And any target statement actually observed must be fatal.
	t.Run("an observed target statement", func(t *testing.T) {
		broken := replay
		rows := append([]ObserverStructuralRow(nil), broken.carried.Window.After.Structural...)
		rows = append(rows, ObserverStructuralRow{
			StrictASTSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("replay visible strict"))),
			TopLevel:        true, Calls: 1,
		})
		broken.carried.Window.After = snapshotOf(t, "after", rows)
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a semantic replay that executed a target statement was accepted")
		}
	})
}

// Gate 22 is OPEN, and this test pins the reason.
//
// The v3 model as it stands cannot finalize an exact request-ID replay at all,
// because two of its invariants are in direct conflict on that path:
//
//   - CompileClassifier presence-couples attestation in both directions. A path
//     that performs no Attestation -- idempotent_replay returns before
//     datasourceEvidence and reaches Business PostgreSQL not at all -- must name
//     neither an ExpectedSchema nor a qualified footprint, and an internal
//     manifest entry naming a qualification the operation does not claim is
//     rejected.
//   - ClassifierManifest.Validate requires an entry for every class in
//     requiredManifestClasses(), which includes postgresql_internal_attestation
//     unconditionally. The only source of internal keys is the footprint.
//
// So the manifest must carry internal keys, and the operation must not claim the
// qualification those keys came from. Nothing satisfies both.
//
// Resolving it is an author decision, because each way out changes what a
// manifest or an operation identity MEANS:
//
//   - make the required-class set path-aware, so a non-attesting operation's
//     closed world excludes the internal class. This relaxes a closed-world rule
//     whose purpose is that no observed statement goes unclassified;
//   - let a non-attesting operation still name the footprint, weakening the
//     presence coupling that keeps a replay distinguishable from an execution;
//   - finalize replays through a separate acceptance path.
//
// Weakening either invariant unilaterally is precisely the quiet relaxation this
// arc exists to prevent, so the conflict is pinned here instead. This test fails
// the moment someone resolves it, which is when the real gate 22 gets written.
func TestGate22IsBlockedByAConflictBetweenTwoV3Invariants(t *testing.T) {
	inputs := finalizerInputs(t)
	inputs.PathKind = PathIdempotentReplay

	dimensions, known := dimensionsFor(PathIdempotentReplay)
	if !known || dimensions.requiresSchema {
		t.Fatal("idempotent_replay is expected to perform no Attestation")
	}

	var internalRequired bool
	for _, class := range requiredManifestClasses() {
		if class == V3PostgreSQLInternalAttestation {
			internalRequired = true
		}
	}
	if !internalRequired {
		t.Fatal("the internal class is no longer unconditionally required; " +
			"the gate 22 conflict may be resolved -- write the real gate")
	}

	// A manifest built without internal keys cannot validate...
	if _, err := BuildClassifierManifest(AttestationFootprintV2{}, nil); err == nil {
		t.Fatal("a manifest with no internal keys now builds; " +
			"the gate 22 conflict may be resolved -- write the real gate")
	}

	// ...and one built WITH them cannot compile against a non-attesting
	// operation, which is the other half of the vice.
	targets, err := deriveTargets(inputs, StrictASTDigest)
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	manifest, err := BuildClassifierManifest(inputs.Footprint, targets)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	operation := OperationIdentity{
		OperationID: inputs.OperationID, PathKind: PathIdempotentReplay,
		ContractIdentity: inputs.ContractIdentity,
	}
	if _, err := CompileClassifier(operation, manifest); err == nil {
		t.Fatal("a non-attesting operation now compiles against a footprint-bound manifest; " +
			"the gate 22 conflict may be resolved -- write the real gate")
	}
}

// The replay evidence contract itself is complete and testable, and is checked
// here so the wrapper's half of gate 22 is not blocked by the conflict above.
func TestIdempotentReplayEvidenceRequiresEveryMember(t *testing.T) {
	persisted := []byte(`{"receipt":"document"}`)
	complete := IdempotentReplayEvidenceV1{
		TaskID: "task-1", RequestID: "request-1", RequestDigest: strings.Repeat("d", 64),
		OriginalQueryID: "query-1", PersistedReceiptJSON: persisted,
		ReturnedReceiptJSON:    append([]byte(nil), persisted...),
		PersistedReceiptSHA256: fmt.Sprintf("%x", sha256.Sum256(persisted)),
		PersistedSignature:     "signature",
	}
	if err := complete.Validate(); err != nil {
		t.Fatalf("complete replay evidence was rejected: %v", err)
	}
	for name, breakIt := range map[string]func(*IdempotentReplayEvidenceV1){
		"no task id":             func(e *IdempotentReplayEvidenceV1) { e.TaskID = "" },
		"no request id":          func(e *IdempotentReplayEvidenceV1) { e.RequestID = "" },
		"no request digest":      func(e *IdempotentReplayEvidenceV1) { e.RequestDigest = "" },
		"no original query id":   func(e *IdempotentReplayEvidenceV1) { e.OriginalQueryID = "" },
		"no persisted digest":    func(e *IdempotentReplayEvidenceV1) { e.PersistedReceiptSHA256 = "" },
		"no persisted signature": func(e *IdempotentReplayEvidenceV1) { e.PersistedSignature = "" },
		"no persisted document":  func(e *IdempotentReplayEvidenceV1) { e.PersistedReceiptJSON = nil },
		"no returned document":   func(e *IdempotentReplayEvidenceV1) { e.ReturnedReceiptJSON = nil },
	} {
		t.Run(name, func(t *testing.T) {
			broken := complete
			breakIt(&broken)
			if err := broken.Validate(); err == nil {
				t.Fatalf("replay evidence with %s was accepted; absence is the substance "+
					"of this evidence and an unpopulated field must not read as a convenient negative", name)
			}
		})
	}
}

// Gate 25. The Adapter's verdict has no acceptance authority, and the wrapper
// gives it no way to arrive: there is no verdict parameter. What the Adapter
// carries is compared, never believed.
func TestGate25AdapterVerdictIsNeverConsulted(t *testing.T) {
	for name, claimPass := range map[string]func(*gateCase){
		"a bad plan": func(c *gateCase) {
			c.carried.Plan.PathKind = PathSingleQuery
		},
		"a bad target": func(c *gateCase) {
			c.carried.VisibleStatement.ExactSHA256 = strings.Repeat("c", 64)
		},
		"a bad delta": func(c *gateCase) {
			rows := append([]ObserverStructuralRow(nil), c.carried.Window.After.Structural...)
			rows = append(rows, ObserverStructuralRow{
				StrictASTSHA256: strings.Repeat("e7", 32), TopLevel: true, Calls: 3,
			})
			c.carried.Window.After = snapshotOf(t, "after", rows)
		},
	} {
		t.Run(name, func(t *testing.T) {
			broken := pairedNovelCase(t)
			// The Adapter asserts the sample passed. There is nowhere to put that
			// assertion, which is the point: acceptance cannot read it.
			claimPass(&broken)
			if _, err := broken.finalize(); err == nil {
				t.Fatalf("evidence carrying %s was accepted; an Adapter verdict must have no authority", name)
			}
		})
	}
}

// Gate 7. A cumulative regression is one failure; a key that vanishes between
// snapshots is the other, and it is the one a per-row scan of the after
// snapshot alone would miss.
func TestGate7DisappearingStructuralKeyFails(t *testing.T) {
	base := pairedNovelCase(t)
	rows := base.carried.Window.After.Structural
	if len(rows) < 2 {
		t.Fatalf("precondition: the honest window has only %d structural rows", len(rows))
	}
	// The before snapshot has counts the after snapshot no longer reports.
	before := snapshotOf(t, "before", rows)
	after := snapshotOf(t, "after", rows[1:])
	base.carried.Window = ObserverWindowV2{Before: before, After: after}
	if _, err := base.finalize(); err == nil {
		t.Fatal("a structural key that disappeared inside the window was accepted")
	}
}

// Gate 29. v1.4/v2 accounting evidence cannot satisfy v3 acceptance. The two
// version strings are distinct constants, and the v3 plan validator requires its
// own; a carried plan wearing the older version is rejected before any number is
// compared.
func TestGate29LegacyV14AccountingCannotSatisfyV3(t *testing.T) {
	if ObserverAccountingVersion == ObserverAccountingV3Version {
		t.Fatal("the v1.4/v2 and v3 accounting versions are the same string")
	}
	base := pairedNovelCase(t)
	base.carried.Plan.Version = ObserverAccountingVersion
	if _, err := base.finalize(); err == nil {
		t.Fatal("a carried plan wearing the v1.4/v2 accounting version was accepted")
	}

	// And a v1 observer snapshot cannot enter a v3 window.
	legacy := pairedNovelCase(t)
	legacy.carried.Window.After.Version = "taskgate-final-v5-observer-snapshot-v1"
	if _, err := legacy.finalize(); err == nil {
		t.Fatal("a v1 observer snapshot was accepted on the v3 path")
	}
}
