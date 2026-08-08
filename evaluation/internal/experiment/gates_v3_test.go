package experiment

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
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
	return finalizeTaskGateObservationV3Core(c.receipt, c.verifier, c.carried, c.trusted)
}

func trustedFrom(t *testing.T, inputs IndependentInputsV3) TrustedInputsV3 {
	t.Helper()
	trusted := TrustedInputsV3{
		CatalogPath: inputs.CatalogPath, Footprint: inputs.Footprint,
		PostgreSQL: inputs.PostgreSQL, OperationID: inputs.OperationID,
		ContractIdentity:                   inputs.ContractIdentity,
		SettlementWroteExecutionBindingRow: true,
	}
	// The material the finalizer prepares from, not the statements it produced.
	// Supplying the statements is what TrustedInputsV3 used to allow, and it is
	// the reason the reproduction could be skipped.
	if dimensions, _ := dimensionsFor(inputs.PathKind); dimensions.requiresSchema {
		material := reproMaterial(t)
		trusted.Material = &material
	}
	// A path that reaches Business PostgreSQL not at all builds no
	// ExpectedSchema, performs no Attestation and renders no target statement, so
	// it must be finalized with none of that material.
	if dimensions, _ := dimensionsFor(inputs.PathKind); !dimensions.requiresSchema {
		trusted.CatalogPath, trusted.Footprint = "", AttestationFootprintV2{}
	}
	return trusted
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
	// Attestation bindings are presence-coupled to the path in both directions:
	// a path that attests must name what against and under which qualification,
	// and one that does not must claim neither.
	operation := OperationIdentity{
		OperationID: inputs.OperationID, PathKind: inputs.PathKind,
		ContractIdentity: inputs.ContractIdentity,
	}
	var (
		plan              GatewayControlPlanV3
		manifestFootprint *AttestationFootprintV2
		targets           []ClassifierEntry
		err               error
	)
	if dimensions.requiresSchema {
		footprintDigest, digestErr := inputs.Footprint.SHA256()
		if digestErr != nil {
			t.Fatalf("footprint digest: %v", digestErr)
		}
		operation.ExpectedSchemaDigest = inputs.Footprint.ExpectedSchemaDigest
		operation.AttestationFootprintSHA256 = footprintDigest
		plan, err = planFor(inputs.PathKind, inputs.Footprint.ExpectedSchemaEntries,
			inputs.Footprint.ExpectedSchemaDigest, inputs.Footprint)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		targets, err = deriveTargets(inputs, StrictASTDigest)
		if err != nil {
			t.Fatalf("targets: %v", err)
		}
		footprint := inputs.Footprint
		manifestFootprint = &footprint
	} else {
		plan, err = planFor(inputs.PathKind, 0, "", AttestationFootprintV2{})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
	}
	manifest, err := BuildClassifierManifestV2(plan, manifestFootprint, targets)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	classifier, err := CompileClassifierV2(operation, plan, manifest)
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
	// The carried identities are read off the real preparation, because that is
	// what a correct Adapter reads off the receipt. Restating fixture constants
	// here would make the honest case disagree with a signature derived from a
	// real authorization on exactly the two members -- the row limit and the
	// policy fingerprint -- that a structural digest cannot see.
	prep := prepareOperationV3(t)
	if dimensions.visible > 0 {
		carried.VisibleStatement = &physicalquery.StatementIdentity{
			ExactSHA256: prep.Visible.ExactSHA256, StrictASTSHA256: prep.Visible.StrictASTSHA256,
			RowLimit:    prep.Limits.VisibleRowLimit,
			Fingerprint: prep.Fingerprints[querybinding.RoleVisible],
		}
		carried.VisiblePreparedTargetBindingSHA256 = prep.Targets[querybinding.RoleVisible]
	}
	if dimensions.companion > 0 {
		carried.CompanionStatement = &physicalquery.StatementIdentity{
			ExactSHA256: prep.Companion.ExactSHA256, StrictASTSHA256: prep.Companion.StrictASTSHA256,
			RowLimit:    prep.Limits.CompanionPolicyRows,
			Fingerprint: prep.Fingerprints[querybinding.RoleCompanion],
		}
		carried.CompanionPreparedTargetBindingSHA256 = prep.Targets[querybinding.RoleCompanion]
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

	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build signed V9: %v", err)
	}
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return gateCase{receipt: receipt, verifier: verifier, carried: carried, trusted: trustedFrom(t, inputs)}
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
	mutated, err := fixture.Mutate(c.receipt, func(b *querybinding.QueryExecutionBindingV2) {
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

	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.SemanticReplay(options)
	if err != nil {
		t.Fatalf("build semantic replay: %v", err)
	}
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	replay := gateCase{receipt: receipt, verifier: verifier, carried: carried, trusted: trustedFrom(t, inputs)}

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

// Gate 22. An exact request-ID replay returns the original document unchanged,
// settles nothing, and reaches Business PostgreSQL not at all.
//
// The path is finalized under a zero-statement classifier manifest: a real
// document with a real digest, bound to this operation and this all-zero plan,
// whose entry list is empty. That is the strictest closed world available rather
// than a relaxation of one -- with no entry to match, EVERY Business statement
// observed in the window is V3Unexpected, and the all-zero plan accepts none.
//
// This replaces TestGate22IsBlockedByAConflictBetweenTwoV3Invariants, which
// pinned the conflict between the old universal required-class rule and the
// presence coupling that forbids a non-attesting operation from naming a
// qualification. The author resolved it by making the class set path-aware; see
// docs/final_v5_v3_runtime_integration_gates.md.
func TestGate22IdempotentReplayReturnsOriginalReceiptByteForByte(t *testing.T) {
	base := idempotentReplayCase(t)

	finalized, err := base.finalize()
	if err != nil {
		t.Fatalf("an honest idempotent replay was rejected: %v", err)
	}

	// The zero-statement manifest is a document, and it is the one the finalizer
	// independently derived for this operation and this all-zero plan.
	zero := compiledTestManifest(t, PathIdempotentReplay)
	if len(zero.Entries) != 0 {
		t.Fatalf("the idempotent replay manifest declares %d entr(ies)", len(zero.Entries))
	}
	zeroDigest, err := zero.SHA256()
	if err != nil {
		t.Fatalf("zero-statement manifest digest: %v", err)
	}
	if finalized.ClassifierManifestSHA256 != zeroDigest {
		t.Fatalf("the replay finalized under manifest %s, the zero-statement manifest is %s",
			shortDigest(finalized.ClassifierManifestSHA256), shortDigest(zeroDigest))
	}
	if finalized.Operation.PathKind != PathIdempotentReplay {
		t.Fatalf("the replay finalized as path_kind %s", finalized.Operation.PathKind)
	}
	// It attests against nothing, so it names nothing.
	if finalized.ExpectedSchemaDigest != "" || finalized.Operation.AttestationFootprintSHA256 != "" {
		t.Fatal("an idempotent replay finalized with an ExpectedSchema or a qualified footprint")
	}

	// Zero Business delta, and empty rather than merely balanced: a structural
	// row in any class would mean the replay touched Business PostgreSQL.
	if finalized.Delta.Total != 0 {
		t.Fatalf("an idempotent replay moved the Business total by %d", finalized.Delta.Total)
	}
	for _, class := range GatewayStatementClassesV3() {
		if got := finalized.Delta.PerClass[class]; got != 0 {
			t.Errorf("an idempotent replay recorded %d %s statement(s)", got, class)
		}
	}
	if len(finalized.Delta.Internal) != 0 || len(finalized.Delta.Unexpected) != 0 {
		t.Fatal("an idempotent replay produced a non-empty structural delta")
	}

	// The returned document must be the persisted one, byte for byte, and its
	// stored identity unchanged.
	t.Run("a changed returned document", func(t *testing.T) {
		broken := idempotentReplayCase(t)
		replay := *broken.trusted.Replay
		replay.ReturnedReceiptJSON = append(append([]byte(nil), replay.PersistedReceiptJSON...), ' ')
		broken.trusted.Replay = &replay
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a returned document differing from the persisted one by one byte was accepted")
		}
	})
	t.Run("a rewritten persisted document", func(t *testing.T) {
		broken := idempotentReplayCase(t)
		replay := *broken.trusted.Replay
		rewritten := append(append([]byte(nil), replay.PersistedReceiptJSON...), ' ')
		replay.PersistedReceiptJSON, replay.ReturnedReceiptJSON = rewritten, rewritten
		broken.trusted.Replay = &replay
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a rewritten stored document was accepted; its recorded digest no longer describes it")
		}
	})
	t.Run("a changed signature", func(t *testing.T) {
		broken := idempotentReplayCase(t)
		replay := *broken.trusted.Replay
		replay.PersistedSignature = "another-signature"
		broken.trusted.Replay = &replay
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a replayed receipt whose signature differs from the persisted one was accepted")
		}
	})
	t.Run("another request's receipt", func(t *testing.T) {
		for name, mutate := range map[string]func(*IdempotentReplayEvidenceV1){
			"task id":    func(e *IdempotentReplayEvidenceV1) { e.TaskID = "another-task" },
			"request id": func(e *IdempotentReplayEvidenceV1) { e.RequestID = "another-request" },
			"request digest": func(e *IdempotentReplayEvidenceV1) {
				e.RequestDigest = fmt.Sprintf("%x", sha256.Sum256([]byte("another request")))
			},
			"original query id": func(e *IdempotentReplayEvidenceV1) { e.OriginalQueryID = "another-query" },
		} {
			t.Run(name, func(t *testing.T) {
				broken := idempotentReplayCase(t)
				replay := *broken.trusted.Replay
				mutate(&replay)
				broken.trusted.Replay = &replay
				if _, err := broken.finalize(); err == nil {
					t.Fatalf("a replay whose %s does not identify the recorded request was accepted", name)
				}
			})
		}
	})

	// Nothing may have been created. Each row is named separately so a partial
	// replay -- one that reserved budget, say -- fails rather than averaging out.
	t.Run("something was settled", func(t *testing.T) {
		for name, mutate := range map[string]func(*IdempotentReplayEvidenceV1){
			"a new query row":             func(e *IdempotentReplayEvidenceV1) { e.WroteNewQueryRow = true },
			"a new execution binding row": func(e *IdempotentReplayEvidenceV1) { e.WroteNewExecutionBindingRow = true },
			"a new reservation":           func(e *IdempotentReplayEvidenceV1) { e.WroteNewReservation = true },
		} {
			t.Run(name, func(t *testing.T) {
				broken := idempotentReplayCase(t)
				replay := *broken.trusted.Replay
				mutate(&replay)
				broken.trusted.Replay = &replay
				if _, err := broken.finalize(); err == nil {
					t.Fatalf("a replay that wrote %s was accepted", name)
				}
			})
		}
		broken := idempotentReplayCase(t)
		broken.trusted.SettlementWroteExecutionBindingRow = true
		if _, err := broken.finalize(); err == nil {
			t.Fatal("the settlement and replay evidence disagreed about a binding row and were accepted")
		}
	})

	// Any Business statement at all is fatal, whatever it is. Each of these is a
	// structure that is perfectly legitimate on some other path; here none of
	// them can be classified, so each lands in the unexpected sink.
	t.Run("a Business statement in the window", func(t *testing.T) {
		for name, row := range map[string]ObserverStructuralRow{
			"a transaction BEGIN":       structuralRowFor(t, runtimeBeginTemplate, true),
			"a transaction COMMIT":      structuralRowFor(t, runtimeCommitTemplate, true),
			"the safety session pin":    structuralRowFor(t, dataconnector.SafetySessionPinSQL, true),
			"the datasource identity":   structuralRowFor(t, dataconnector.DatasourceIdentitySQL, true),
			"a view column attestation": structuralRowFor(t, dataconnector.ViewColumnAttestationSQL, true),
			"the visible target":        structuralRowFor(t, prepareOperationV3(t).VisibleSQL, true),
			"the companion target":      structuralRowFor(t, prepareOperationV3(t).CompanionSQL, true),
			"a qualified internal key": {
				StrictASTSHA256: testInternalKeyA, TopLevel: false, Calls: 1,
			},
			"an unknown statement": {
				StrictASTSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("something else entirely"))),
				TopLevel:        true, Calls: 1,
			},
		} {
			t.Run(name, func(t *testing.T) {
				broken := idempotentReplayCase(t)
				broken.carried.Window.After = snapshotOf(t, "after", []ObserverStructuralRow{row})
				if _, err := broken.finalize(); err == nil {
					t.Fatalf("an idempotent replay window containing %s was accepted", name)
				}
			})
		}
	})

	// A role total that moved without any structural row is not a smaller
	// version of the same defect: it means the total and the census did not come
	// from one row set, and the atomic invariant rejects the snapshot outright.
	t.Run("a role total with no structural rows", func(t *testing.T) {
		broken := idempotentReplayCase(t)
		broken.carried.Window.After.Total = 1
		if err := broken.carried.Window.After.Validate(); err == nil {
			t.Fatal("a snapshot whose total disagrees with its census validated")
		}
		if _, err := broken.finalize(); err == nil {
			t.Fatal("an idempotent replay whose role total moved with no structural row was accepted")
		}
	})

	// The window must have been measured under one unchanging deployment. The
	// healthcheck digest is part of that identity for a specific reason: the
	// observer-v3 override replaces /health/ready with /health/live, and a probe
	// still reaching /health/ready performs a full Business Attestation on every
	// interval -- readiness attestation is only ever legitimate OUTSIDE the
	// window.
	t.Run("the periodic healthcheck changed inside the window", func(t *testing.T) {
		broken := idempotentReplayCase(t)
		broken.carried.Window.After.Runtime.Gateway.HealthcheckSHA256 = strings.Repeat("9", 64)
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a window whose periodic healthcheck definition changed was accepted")
		}
	})

	// Attestation and target material must not reach an idempotent replay through
	// any door: not the finalizer's inputs, not the Adapter's evidence.
	t.Run("attestation or target material", func(t *testing.T) {
		for name, mutate := range map[string]func(*gateCase){
			"a Profile Catalog": func(c *gateCase) {
				c.trusted.CatalogPath = resultHeavyCatalogPath(t)
			},
			"a qualified footprint": func(c *gateCase) {
				footprint, _ := realSchemaFootprint(t)
				c.trusted.Footprint = footprint
			},
			"frozen preparation material": func(c *gateCase) {
				material := reproMaterial(t)
				c.trusted.Material = &material
			},
			"a carried visible statement": func(c *gateCase) {
				c.carried.VisibleStatement = &physicalquery.StatementIdentity{
					ExactSHA256: physicalquery.ExactDigest(prepareOperationV3(t).VisibleSQL),
				}
			},
			"a carried companion statement": func(c *gateCase) {
				c.carried.CompanionStatement = &physicalquery.StatementIdentity{
					ExactSHA256: physicalquery.ExactDigest(prepareOperationV3(t).CompanionSQL),
				}
			},
			"a carried visible prepared target binding": func(c *gateCase) {
				c.carried.VisiblePreparedTargetBindingSHA256 = fixture.PreparedTargetBinding(querybinding.RoleVisible)
			},
			"a carried companion prepared target binding": func(c *gateCase) {
				c.carried.CompanionPreparedTargetBindingSHA256 = fixture.PreparedTargetBinding(querybinding.RoleCompanion)
			},
			"an ExpectedSchema on the operation": func(c *gateCase) {
				c.carried.Operation.ExpectedSchemaDigest = strings.Repeat("a", 64)
			},
			"a footprint on the operation": func(c *gateCase) {
				c.carried.Operation.AttestationFootprintSHA256 = strings.Repeat("b", 64)
			},
		} {
			t.Run(name, func(t *testing.T) {
				broken := idempotentReplayCase(t)
				mutate(&broken)
				if _, err := broken.finalize(); err == nil {
					t.Fatalf("an idempotent replay carrying %s was accepted", name)
				}
			})
		}
	})

	// And the empty manifest is a contract for THIS path alone.
	t.Run("the empty manifest on an executing path", func(t *testing.T) {
		for _, kind := range []GatewayPathKind{PathPairedNovel, PathSingleQuery, PathSemanticReplay} {
			if _, err := compileTest(t, kind, zero); err == nil {
				t.Fatalf("the zero-statement manifest compiled for %s", kind)
			}
		}
	})
}

// idempotentReplayCase is an honest exact request-ID replay: the original
// paired-novel receipt comes back unchanged, the store recorded that nothing was
// settled, and the observer saw no Business statement at all.
func idempotentReplayCase(t *testing.T) gateCase {
	t.Helper()
	inputs := finalizerInputs(t)
	inputs.PathKind = PathIdempotentReplay
	carried := carriedFor(t, inputs)

	// The document that comes back is the ORIGINAL execution's receipt. Its
	// signed binding describes that execution and names its path kind, which is
	// exactly why the replay is established from the Control Store instead.
	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build the original signed V9: %v", err)
	}
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	persisted, err := fixture.PersistedJSON(receipt)
	if err != nil {
		t.Fatalf("persist the original receipt: %v", err)
	}

	trusted := trustedFrom(t, inputs)
	trusted.SettlementWroteExecutionBindingRow = false
	trusted.Replay = &IdempotentReplayEvidenceV1{
		TaskID: receipt.TaskID, RequestID: receipt.RequestID,
		RequestDigest: receipt.RequestDigest, OriginalQueryID: receipt.QueryID,
		PersistedReceiptJSON: persisted,
		// A distinct copy, so a test that mutates one does not silently mutate
		// the other and pass for the wrong reason.
		ReturnedReceiptJSON:    append([]byte(nil), persisted...),
		PersistedReceiptSHA256: fmt.Sprintf("%x", sha256.Sum256(persisted)),
		PersistedSignature:     receipt.Signature,
	}
	return gateCase{receipt: receipt, verifier: verifier, carried: carried, trusted: trusted}
}

// structuralRowFor is one observed statement, keyed the way the observer keys it.
func structuralRowFor(t *testing.T, sql string, topLevel bool) ObserverStructuralRow {
	t.Helper()
	return ObserverStructuralRow{StrictASTSHA256: mustStrictDigest(t, sql), TopLevel: topLevel, Calls: 1}
}

func mustStrictDigest(t *testing.T, sql string) string {
	t.Helper()
	digest, err := StrictASTDigest(sql)
	if err != nil {
		t.Fatalf("strict AST digest: %v", err)
	}
	return digest
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
