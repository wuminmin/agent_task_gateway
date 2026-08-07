package queryreceipt

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/querybinding"
)

func fixedDigest(seed string) string { return strings.Repeat(seed, 64/len(seed)) }

// validExecutionReceipt is a paired-novel execution under expanded evidence: the
// companion's policy limit is its evidence rows plus one, so a truncated
// companion result is distinguishable from a complete one.
//
// It is the artifact-delivery shape. The inline shape differs only in the signed
// delivery mode and the absent intent, and is built from this one.
func validExecutionReceipt(t *testing.T) QueryReceiptV1 {
	t.Helper()
	receipt := validArtifactReceipt(t)
	receipt.Version = Version
	receipt.ResultDeliveryMode = DeliveryArtifact
	// Sign() normally fills this. It is set here so ValidateUnsigned is exercised
	// on an otherwise complete receipt, and a negative case below therefore fails
	// for the reason it names rather than for a missing key id.
	receipt.GatewayKeyID = "gateway-demo-ed25519-v1"

	ledger, err := querybinding.ExposureLedgerBeforeV1{
		ProfileVersion: receipt.Exposure.ProfileVersion,
		RootTaskID:     receipt.Exposure.RootTaskID,
		RootEpoch:      receipt.Exposure.RootEpoch,
		Limits: querybinding.FactVector{ReleaseFacts: 500, InfluenceFacts: 4, OutcomeFacts: 10,
			PredicateAtoms: 25, CompositeOutcomes: 5},
		Used: querybinding.FactVector{ReleaseFacts: 100, InfluenceFacts: 0, OutcomeFacts: 2,
			PredicateAtoms: 5, CompositeOutcomes: 1},
		Remaining: querybinding.FactVector{ReleaseFacts: 400, InfluenceFacts: 4, OutcomeFacts: 8,
			PredicateAtoms: 20, CompositeOutcomes: 4},
		RemainingRows:        10,
		UsesExpandedEvidence: true,
		HasExposureContext:   true,
	}.Seal()
	if err != nil {
		t.Fatalf("seal exposure ledger pre-state: %v", err)
	}
	receipt.ExposureLedgerBefore = &ledger

	budgetDigest, err := BudgetStateSHA256(receipt.BudgetBefore)
	if err != nil {
		t.Fatalf("budget digest: %v", err)
	}
	compiler := executionCompiler(t)
	companion := querybinding.TargetRecordV1{
		Role: querybinding.RoleCompanion, Authorized: true, Executed: true,
		ExactSQLSHA256: fixedDigest("b1"), StrictASTSHA256: fixedDigest("b2"),
		RowLimit: 5, PolicyFingerprint: "companion-fingerprint",
		PolicyRendererVersion:       compiler.PolicyRendererVersion,
		PolicyRendererDigest:        compiler.PolicyRendererSHA256,
		PreparedTargetBindingSHA256: fixedDigest("b4"),
	}
	binding, err := querybinding.QueryExecutionBindingV2{
		PathKind:                   querybinding.PathPairedNovel,
		PreparedOperation:          preparedOperationFixture(t, true),
		Compiler:                   compiler,
		ExposureProfileVersion:     ledger.ProfileVersion,
		VisibleRowLimit:            10,
		CompanionEvidenceRows:      4,
		CompanionPolicyRows:        5,
		BudgetBeforeSHA256:         budgetDigest,
		ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible: querybinding.TargetRecordV1{
			Role: querybinding.RoleVisible, Authorized: true, Executed: true,
			ExactSQLSHA256: fixedDigest("a1"), StrictASTSHA256: fixedDigest("a2"),
			RowLimit: 10, PolicyFingerprint: receipt.SQLFingerprint,
			PolicyRendererVersion:       compiler.PolicyRendererVersion,
			PolicyRendererDigest:        compiler.PolicyRendererSHA256,
			PreparedTargetBindingSHA256: fixedDigest("a4"),
		},
		Companion: &companion,
	}.Seal()
	if err != nil {
		t.Fatalf("seal execution binding: %v", err)
	}
	receipt.ExecutionBindingV2 = &binding
	return receipt
}

// V8 keeps working exactly as before. Adding V9 must not have moved any V8
// signature, so a V8 receipt signs and verifies unchanged.
func TestV8RemainsValidUnderItsOwnSemantics(t *testing.T) {
	receipt := validArtifactReceipt(t)
	signer := DemoSigner([]byte("v8-unchanged"))
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("sign V8: %v", err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(signed); err != nil {
		t.Fatalf("a signed V8 receipt did not verify: %v", err)
	}
}

// A receipt that describes no execution must not be able to acquire one.
//
// The absence is itself signed -- the payload carries a null binding and a null
// pre-state -- so stapling one on breaks the signature. ValidateUnsigned catches
// the incoherent halves before that: a binding without the pre-state its limits
// derive from, or a pre-state describing no execution.
func TestAReceiptWithoutExecutionEvidenceCannotAcquireIt(t *testing.T) {
	plain := validArtifactReceipt(t)
	if plain.ExecutionBindingV2 != nil || plain.ExposureLedgerBefore != nil {
		t.Fatal("the baseline already carries execution evidence")
	}
	plain.GatewayKeyID = "gateway-demo-ed25519-v1"
	if err := plain.ValidateUnsigned(); err != nil {
		t.Fatalf("the baseline does not validate, so the cases below prove nothing: %v", err)
	}

	executing := validExecutionReceipt(t)
	for name, staple := range map[string]func(*QueryReceiptV1){
		"a binding with no pre-state": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2 = executing.ExecutionBindingV2
		},
		"a pre-state with no binding": func(r *QueryReceiptV1) {
			r.ExposureLedgerBefore = executing.ExposureLedgerBefore
		},
	} {
		t.Run(name, func(t *testing.T) {
			stapled := plain
			staple(&stapled)
			if err := stapled.ValidateUnsigned(); err == nil {
				t.Fatalf("a receipt carrying %s was accepted", name)
			}
		})
	}

	// And stapling both is refused by the signature rather than by the shape,
	// because both together are a coherent shape -- just not the one that was
	// signed.
	t.Run("both, past the signature", func(t *testing.T) {
		signer := DemoSigner([]byte("staple"))
		keyring, err := NewKeyring(signer, nil)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(plain)
		if err != nil {
			t.Fatalf("sign the baseline: %v", err)
		}
		if err := keyring.Verify(signed); err != nil {
			t.Fatalf("the signed baseline does not verify: %v", err)
		}
		stapled := signed
		stapled.ExecutionBindingV2 = executing.ExecutionBindingV2
		stapled.ExposureLedgerBefore = executing.ExposureLedgerBefore
		if err := keyring.Verify(stapled); err == nil {
			t.Fatal("a receipt that acquired execution evidence after signing still verified")
		}
	})
}

func TestExecutionReceiptSignsAndVerifies(t *testing.T) {
	receipt := validExecutionReceipt(t)
	if err := receipt.ValidateUnsigned(); err != nil {
		t.Fatalf("a well-formed execution receipt was rejected: %v", err)
	}
	signer := DemoSigner([]byte("v9"))
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(signed); err != nil {
		t.Fatalf("a signed execution receipt did not verify: %v", err)
	}
}

// V9 requires both structures. A receipt claiming the version without them is
// asserting an execution binding it does not have.
func TestExecutionReceiptRequiresBothSignedStructures(t *testing.T) {
	for name, mutate := range map[string]func(*QueryReceiptV1){
		"no execution binding": func(r *QueryReceiptV1) { r.ExecutionBindingV2 = nil },
		"no ledger pre-state":  func(r *QueryReceiptV1) { r.ExposureLedgerBefore = nil },
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validExecutionReceipt(t)
			mutate(&receipt)
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatal("a V9 receipt without its signed execution evidence was accepted")
			}
		})
	}
}

// The binding must name the pre-state and budget the receipt actually carries,
// or it could have been derived against a state nothing here describes.
func TestExecutionReceiptBindsItsPreStateAndBudget(t *testing.T) {
	t.Run("pre-state digest", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		binding := *receipt.ExecutionBindingV2
		binding.ExposureLedgerBeforeSHA256 = fixedDigest("9")
		resealed, err := binding.Seal()
		if err != nil {
			t.Fatal(err)
		}
		receipt.ExecutionBindingV2 = &resealed
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a binding naming another exposure pre-state was accepted")
		}
	})

	t.Run("budget digest", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		binding := *receipt.ExecutionBindingV2
		binding.BudgetBeforeSHA256 = fixedDigest("9")
		resealed, err := binding.Seal()
		if err != nil {
			t.Fatal(err)
		}
		receipt.ExecutionBindingV2 = &resealed
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a binding naming another budget pre-state was accepted")
		}
	})

	// Changing the receipt's own budget_before must invalidate the binding too:
	// the digest is what ties them together in both directions.
	t.Run("budget mutated on the receipt", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		receipt.BudgetBefore.Limits.Rows = 999
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("mutating budget_before left the execution binding valid")
		}
	})
}

// The row limits must be reproducible from the signed pre-state. A limit the
// pre-state cannot derive is a limit nothing authorized.
func TestExecutionReceiptRowLimitsMustReproduceFromThePreState(t *testing.T) {
	for name, mutate := range map[string]func(*querybinding.QueryExecutionBindingV2){
		"visible limit exceeds the budget": func(b *querybinding.QueryExecutionBindingV2) {
			b.VisibleRowLimit = 11
			b.Visible.RowLimit = 11
		},
		"companion evidence rows invented": func(b *querybinding.QueryExecutionBindingV2) {
			b.CompanionEvidenceRows = 3
			b.CompanionPolicyRows = 4
			b.Companion.RowLimit = 4
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validExecutionReceipt(t)
			binding := *receipt.ExecutionBindingV2
			companion := *binding.Companion
			binding.Companion = &companion
			mutate(&binding)
			resealed, err := binding.Seal()
			if err != nil {
				t.Fatalf("reseal: %v", err)
			}
			receipt.ExecutionBindingV2 = &resealed
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatal("a row limit the signed pre-state cannot derive was accepted")
			}
		})
	}
}

// The signature must cover every member of the binding and the pre-state. Each
// case mutates a signed receipt and requires verification to fail.
func TestExecutionReceiptSignatureCoversTheCompleteExecutionBinding(t *testing.T) {
	signer := DemoSigner([]byte("execution-binding-coverage"))
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signer.Sign(validExecutionReceipt(t))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := keyring.Verify(base); err != nil {
		t.Fatalf("baseline verification failed: %v", err)
	}

	for name, mutate := range map[string]func(*QueryReceiptV1){
		"exact digest": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Visible.ExactSQLSHA256 = fixedDigest("9")
		},
		"strict digest": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Visible.StrictASTSHA256 = fixedDigest("9")
		},
		"companion exact digest": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Companion.ExactSQLSHA256 = fixedDigest("9")
		},
		"companion strict digest": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Companion.StrictASTSHA256 = fixedDigest("9")
		},
		"visible row limit": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.VisibleRowLimit = 9
			r.ExecutionBindingV2.Visible.RowLimit = 9
		},
		"companion row limit": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.CompanionPolicyRows = 6
			r.ExecutionBindingV2.Companion.RowLimit = 6
		},
		"executed flag": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Companion.Executed = false
		},
		"path kind": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.PathKind = querybinding.PathSemanticReplay
		},
		"prepared plan identity": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.PreparedOperation.PlanSHA256 = fixedDigest("9")
		},
		"prepared compiler identity": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.PreparedOperation.CompilerIdentitySHA256 = fixedDigest("9")
		},
		"policy fingerprint": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Visible.PolicyFingerprint = "other-fingerprint"
		},
		"prepared target binding": func(r *QueryReceiptV1) {
			r.ExecutionBindingV2.Visible.PreparedTargetBindingSHA256 = fixedDigest("9")
		},
		"pre-state limits": func(r *QueryReceiptV1) {
			r.ExposureLedgerBefore.Limits.InfluenceFacts = 40
			r.ExposureLedgerBefore.Remaining.InfluenceFacts = 40
		},
		"pre-state root epoch": func(r *QueryReceiptV1) {
			r.ExposureLedgerBefore.RootEpoch = 99
		},
		"pre-state remaining rows": func(r *QueryReceiptV1) {
			r.ExposureLedgerBefore.RemainingRows = 999
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			binding := *base.ExecutionBindingV2
			companion := *base.ExecutionBindingV2.Companion
			binding.Companion = &companion
			ledger := *base.ExposureLedgerBefore
			mutated.ExecutionBindingV2 = &binding
			mutated.ExposureLedgerBefore = &ledger
			mutate(&mutated)
			if err := keyring.Verify(mutated); err == nil {
				t.Fatalf("mutating the %s left the signature valid", name)
			}
		})
	}
}

// Swapping the visible and companion statements must not verify.
func TestExecutionReceiptTargetRoleSwapFails(t *testing.T) {
	signer := DemoSigner([]byte("v9-swap"))
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signer.Sign(validExecutionReceipt(t))
	if err != nil {
		t.Fatal(err)
	}

	swapped := base
	binding := *base.ExecutionBindingV2
	visible, companion := base.ExecutionBindingV2.Visible, *base.ExecutionBindingV2.Companion
	binding.Visible.ExactSQLSHA256, binding.Visible.StrictASTSHA256 =
		companion.ExactSQLSHA256, companion.StrictASTSHA256
	swappedCompanion := companion
	swappedCompanion.ExactSQLSHA256, swappedCompanion.StrictASTSHA256 =
		visible.ExactSQLSHA256, visible.StrictASTSHA256
	binding.Companion = &swappedCompanion
	swapped.ExecutionBindingV2 = &binding

	if err := keyring.Verify(swapped); err == nil {
		t.Fatal("a receipt with swapped visible and companion statements verified")
	}
	if err := swapped.ValidateUnsigned(); err == nil {
		t.Fatal("a swapped execution binding validated")
	}
}

// An idempotent replay returns the original signed receipt unchanged, so its
// execution binding must be byte-identical rather than re-derived.
func TestExecutionReceiptReplayReturnsAByteIdenticalExecutionBinding(t *testing.T) {
	signer := DemoSigner([]byte("v9-replay"))
	original, err := signer.Sign(validExecutionReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	// A replay is a round trip through persistence, not a recomputation.
	var replayed QueryReceiptV1
	if err := json.Unmarshal(encoded, &replayed); err != nil {
		t.Fatal(err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(replayed); err != nil {
		t.Fatalf("the replayed receipt did not verify: %v", err)
	}
	if replayed.ExecutionBindingV2.SHA256 != original.ExecutionBindingV2.SHA256 {
		t.Fatal("the replayed execution binding is not the original")
	}
	if !replayed.ExecutionBindingV2.Equal(*original.ExecutionBindingV2) {
		t.Fatal("the replayed execution binding is not equal to the original")
	}
	roundTripped, err := json.Marshal(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if string(roundTripped) != string(encoded) {
		t.Fatal("the replayed receipt is not byte-identical to the original")
	}
	if replayed.Signature != original.Signature {
		t.Fatal("the replay produced a different signature")
	}
}

// Nothing in a V9 receipt may carry SQL: it is retained, replayed and handed to
// a finalizer that must not learn what was queried.
func TestExecutionReceiptCarriesNoSQL(t *testing.T) {
	signer := DemoSigner([]byte("v9-no-sql"))
	signed, err := signer.Sign(validExecutionReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	lowered := strings.ToLower(string(encoded))
	for _, fragment := range []string{
		"select ", " from ", " where ", " join ", "insert ", "update ", "delete ", "--", "/*",
	} {
		if strings.Contains(lowered, fragment) {
			t.Fatalf("the signed V9 receipt carries the SQL fragment %q", fragment)
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		for _, forbidden := range []string{"sql_text", "statement", "query_text", "fact_id", "bitmap"} {
			if strings.Contains(field, forbidden) {
				t.Errorf("the V9 receipt exposes a %q field", field)
			}
		}
	}
}

// The path semantics reach the receipt: a semantic replay that executed a
// target is refused before anything is signed.
func TestExecutionReceiptRefusesASemanticReplayThatExecuted(t *testing.T) {
	receipt := validExecutionReceipt(t)
	binding := *receipt.ExecutionBindingV2
	binding.PathKind = querybinding.PathSemanticReplay
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a semantic replay that executed its targets sealed cleanly")
	}

	// An idempotent replay never creates a binding at all.
	idempotent := *receipt.ExecutionBindingV2
	idempotent.PathKind = querybinding.PathIdempotentReplay
	if _, err := idempotent.Seal(); err == nil {
		t.Fatal("an idempotent replay produced a new execution binding")
	}
}

// --- I2-A0 forward fixes -----------------------------------------------------
//
// The three cases below passed before only because V9 was omitted from the
// version conditions that enforce them. They are not new rules: every version
// since V2 (schema_digest) and V3 (signed_at) has had to satisfy them, and V9
// inherited the requirement without inheriting the check.

func TestExecutionReceiptRequiresSchemaDigest(t *testing.T) {
	for name, digest := range map[string]string{
		"absent":    "",
		"uppercase": strings.ToUpper(fixedDigest("ab")),
		"truncated": fixedDigest("ab")[:32],
		"not hex":   strings.Repeat("z", 64),
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validExecutionReceipt(t)
			if err := receipt.ValidateUnsigned(); err != nil {
				t.Fatalf("the V9 baseline does not validate, so this case proves nothing: %v", err)
			}
			receipt.SchemaDigest = digest
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatalf("a V9 receipt with a %s schema_digest was accepted", name)
			}
		})
	}
}

func TestExecutionReceiptRequiresSignedAtNotBeforeCompletion(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		receipt.SignedAt = nil
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a V9 receipt with no signed_at was accepted")
		}
	})
	t.Run("zero", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		zero := time.Time{}
		receipt.SignedAt = &zero
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a V9 receipt with a zero signed_at was accepted")
		}
	})
	t.Run("precedes completion", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		early := receipt.CompletedAt.Add(-time.Millisecond)
		receipt.SignedAt = &early
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a V9 receipt signed before the evidence it attests was accepted")
		}
	})
	t.Run("at completion", func(t *testing.T) {
		receipt := validExecutionReceipt(t)
		at := receipt.CompletedAt
		receipt.SignedAt = &at
		if err := receipt.ValidateUnsigned(); err != nil {
			t.Fatalf("signing at the completion instant is legitimate but was refused: %v", err)
		}
	})
}

// V9 must sign under its own domain. Reusing V8's would let a V8 signature be
// presented over a V9 document.
func TestExecutionReceiptSignsUnderItsOwnDomain(t *testing.T) {
	receipt := validExecutionReceipt(t)
	signer := DemoSigner([]byte("v9-domain"))
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(signed); err != nil {
		t.Fatalf("a signed execution receipt did not verify: %v", err)
	}
	relabelled := signed
	relabelled.Version = "9"
	if err := keyring.Verify(relabelled); err == nil {
		t.Fatal("a signature verified over a document relabelled to another version")
	}
}

// The pre-state's remaining_rows is not independently assertable: budget_before
// is signed on the same receipt and already says what the task had left.
func TestExecutionReceiptRemainingRowsMustEqualBudgetBefore(t *testing.T) {
	for name, mutate := range map[string]func(*QueryReceiptV1){
		"pre-state claims more rows than the budget leaves": func(r *QueryReceiptV1) {
			ledger := *r.ExposureLedgerBefore
			ledger.RemainingRows = 20
			resealLedger(t, r, ledger)
		},
		"pre-state claims fewer rows than the budget leaves": func(r *QueryReceiptV1) {
			ledger := *r.ExposureLedgerBefore
			ledger.RemainingRows = 4
			resealLedger(t, r, ledger)
		},
		"budget used moves without the pre-state": func(r *QueryReceiptV1) {
			r.BudgetBefore.Used.Rows = 3
		},
		"budget reserved moves without the pre-state": func(r *QueryReceiptV1) {
			r.BudgetBefore.Reserved.Rows = 2
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validExecutionReceipt(t)
			mutate(&receipt)
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatal("a V9 receipt whose two signed pre-states disagree about the row budget was accepted")
			}
		})
	}
}

// A budget that has used and reserved more than its limit describes no state.
// It must be refused rather than clamped to zero remaining.
func TestExecutionReceiptFailsClosedOnOverdrawnBudget(t *testing.T) {
	receipt := validExecutionReceipt(t)
	receipt.BudgetBefore.Used.Rows = 8
	receipt.BudgetBefore.Reserved.Rows = 5
	ledger := *receipt.ExposureLedgerBefore
	ledger.RemainingRows = 0
	resealLedger(t, &receipt, ledger)
	if err := receipt.ValidateUnsigned(); err == nil {
		t.Fatal("an overdrawn budget_before was accepted with remaining_rows clamped to zero")
	}
}

// The ledger identity is not independently assertable either. An epoch change
// resets the accounting, so a pre-state naming a different epoch describes
// limits that did not exist when the query ran.
func TestExecutionReceiptLedgerPreStateMustMatchExposureEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *QueryReceiptV1){
		"root task": func(t *testing.T, r *QueryReceiptV1) {
			ledger := *r.ExposureLedgerBefore
			ledger.RootTaskID = "some-other-root-task"
			resealLedger(t, r, ledger)
		},
		"root epoch later than the charge": func(t *testing.T, r *QueryReceiptV1) {
			ledger := *r.ExposureLedgerBefore
			ledger.RootEpoch = r.Exposure.RootEpoch + 1
			resealLedger(t, r, ledger)
		},
		"profile version": func(t *testing.T, r *QueryReceiptV1) {
			ledger := *r.ExposureLedgerBefore
			ledger.ProfileVersion = "taskgate-exposure-v4"
			resealLedger(t, r, ledger)
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validExecutionReceipt(t)
			mutate(t, &receipt)
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatalf("a receipt whose pre-state names a different %s than its exposure evidence was accepted", name)
			}
		})
	}
}

// A single-query binding renders exactly one row limit into executable SQL. It
// used to be the only limit nothing checked against the state that authorized
// it, because the reproduction returned early when no companion was bound.
func TestExecutionReceiptSingleQueryVisibleLimitIsBoundedByThePreState(t *testing.T) {
	build := func(t *testing.T, visibleRowLimit int64) QueryReceiptV1 {
		t.Helper()
		receipt := validExecutionReceipt(t)
		ledger, err := querybinding.ExposureLedgerBeforeV1{
			ProfileVersion: receipt.Exposure.ProfileVersion,
			RootTaskID:     receipt.Exposure.RootTaskID,
			RootEpoch:      receipt.Exposure.RootEpoch,
			Limits: querybinding.FactVector{ReleaseFacts: 500, InfluenceFacts: 4, OutcomeFacts: 10,
				PredicateAtoms: 25, CompositeOutcomes: 5},
			Used: querybinding.FactVector{ReleaseFacts: 100, InfluenceFacts: 0, OutcomeFacts: 2,
				PredicateAtoms: 5, CompositeOutcomes: 1},
			Remaining: querybinding.FactVector{ReleaseFacts: 400, InfluenceFacts: 4, OutcomeFacts: 8,
				PredicateAtoms: 20, CompositeOutcomes: 4},
			RemainingRows: 10,
		}.Seal()
		if err != nil {
			t.Fatalf("seal single-query pre-state: %v", err)
		}
		receipt.ExposureLedgerBefore = &ledger
		budgetDigest, err := BudgetStateSHA256(receipt.BudgetBefore)
		if err != nil {
			t.Fatal(err)
		}
		compiler := executionCompiler(t)
		binding, err := querybinding.QueryExecutionBindingV2{
			PathKind:                   querybinding.PathSingleQuery,
			PreparedOperation:          preparedOperationFixture(t, false),
			Compiler:                   compiler,
			ExposureProfileVersion:     ledger.ProfileVersion,
			VisibleRowLimit:            visibleRowLimit,
			BudgetBeforeSHA256:         budgetDigest,
			ExposureLedgerBeforeSHA256: ledger.SHA256,
			Visible: querybinding.TargetRecordV1{
				Role: querybinding.RoleVisible, Authorized: true, Executed: true,
				ExactSQLSHA256: fixedDigest("a1"), StrictASTSHA256: fixedDigest("a2"),
				RowLimit: visibleRowLimit, PolicyFingerprint: receipt.SQLFingerprint,
				PolicyRendererVersion:       compiler.PolicyRendererVersion,
				PolicyRendererDigest:        compiler.PolicyRendererSHA256,
				PreparedTargetBindingSHA256: fixedDigest("a4"),
			},
		}.Seal()
		if err != nil {
			t.Fatalf("seal single-query binding: %v", err)
		}
		receipt.ExecutionBindingV2 = &binding
		return receipt
	}

	if err := build(t, 10).ValidateUnsigned(); err != nil {
		t.Fatalf("a single-query binding at the signed remaining rows was refused: %v", err)
	}
	if err := build(t, 11).ValidateUnsigned(); err == nil {
		t.Fatal("a single-query binding whose visible row limit exceeds the signed remaining rows was accepted")
	}
}

func resealLedger(t *testing.T, receipt *QueryReceiptV1, ledger querybinding.ExposureLedgerBeforeV1) {
	t.Helper()
	sealed, err := ledger.Seal()
	if err != nil {
		t.Fatalf("reseal exposure ledger pre-state: %v", err)
	}
	receipt.ExposureLedgerBefore = &sealed
	binding := *receipt.ExecutionBindingV2
	binding.ExposureLedgerBeforeSHA256 = sealed.SHA256
	binding.ExposureProfileVersion = sealed.ProfileVersion
	// The expanded-evidence state lives in the preparation, which is the only
	// place it is written down. Moving it means re-sealing the preparation rather
	// than setting a flag beside it, which is the property V2 was built to have.
	if binding.PreparedOperation.ExpandedEvidence != sealed.UsesExpandedEvidence {
		prepared := binding.PreparedOperation
		prepared.ExpandedEvidence = sealed.UsesExpandedEvidence
		prepared.SHA256 = ""
		resealedPreparation, err := prepared.Seal()
		if err != nil {
			t.Fatalf("reseal prepared operation: %v", err)
		}
		binding.PreparedOperation = resealedPreparation
	}
	resealed, err := binding.Seal()
	if err != nil {
		t.Fatalf("reseal execution binding: %v", err)
	}
	receipt.ExecutionBindingV2 = &resealed
}

// A novel observation advances the root head as it settles, so the pre-state
// epoch is normally one BEHIND the charge's. Rejecting that would reject every
// novel paired execution, which is the case the execution evidence exists to describe.
func TestExecutionReceiptAcceptsAPreStateEpochBehindTheCharge(t *testing.T) {
	receipt := validExecutionReceipt(t)
	ledger := *receipt.ExposureLedgerBefore
	if receipt.Exposure.RootEpoch < 1 {
		t.Fatalf("the fixture charge settled at epoch %d, so this case proves nothing", receipt.Exposure.RootEpoch)
	}
	ledger.RootEpoch = receipt.Exposure.RootEpoch - 1
	resealLedger(t, &receipt, ledger)
	if err := receipt.ValidateUnsigned(); err != nil {
		t.Fatalf("a pre-state read one epoch before its charge settled was refused: %v", err)
	}
}
