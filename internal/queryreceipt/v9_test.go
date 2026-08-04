package queryreceipt

import (
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/querybinding"
)

func fixedDigest(seed string) string { return strings.Repeat(seed, 64/len(seed)) }

// validV9Receipt is a paired-novel execution under expanded evidence: the
// companion's policy limit is its evidence rows plus one, so a truncated
// companion result is distinguishable from a complete one.
func validV9Receipt(t *testing.T) QueryReceiptV1 {
	t.Helper()
	receipt := validV8Receipt(t)
	receipt.Version = VersionV9
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
	companion := querybinding.TargetRecordV1{
		Role: querybinding.RoleCompanion, Authorized: true, Executed: true,
		ExactSQLSHA256: fixedDigest("b1"), StrictASTSHA256: fixedDigest("b2"),
		RowLimit: 5, PolicyFingerprint: "companion-fingerprint",
		PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererDigest: fixedDigest("b3"),
		PreparedTargetBindingSHA256: fixedDigest("b4"),
	}
	binding, err := querybinding.QueryExecutionBindingV1{
		PathKind:                       querybinding.PathPairedNovel,
		PreparedOperationBindingSHA256: fixedDigest("c1"),
		ExposureProfileVersion:         ledger.ProfileVersion,
		UsesExpandedEvidence:           true,
		VisibleRowLimit:                10,
		CompanionEvidenceRows:          4,
		CompanionPolicyRows:            5,
		BudgetBeforeSHA256:             budgetDigest,
		ExposureLedgerBeforeSHA256:     ledger.SHA256,
		PlanSHA256:                     fixedDigest("c2"),
		CompilerVersion:                "queryplan-v7",
		CompilerSHA256:                 fixedDigest("c3"),
		Visible: querybinding.TargetRecordV1{
			Role: querybinding.RoleVisible, Authorized: true, Executed: true,
			ExactSQLSHA256: fixedDigest("a1"), StrictASTSHA256: fixedDigest("a2"),
			RowLimit: 10, PolicyFingerprint: receipt.SQLFingerprint,
			PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererDigest: fixedDigest("a3"),
			PreparedTargetBindingSHA256: fixedDigest("a4"),
		},
		Companion: &companion,
	}.Seal()
	if err != nil {
		t.Fatalf("seal execution binding: %v", err)
	}
	receipt.ExecutionBinding = &binding
	return receipt
}

// V8 keeps working exactly as before. Adding V9 must not have moved any V8
// signature, so a V8 receipt signs and verifies unchanged.
func TestV8RemainsValidUnderItsOwnSemantics(t *testing.T) {
	receipt := validV8Receipt(t)
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

// A V8 receipt carries no execution binding, so it cannot satisfy anything that
// requires one. A holder must not be able to staple one on either: the V8
// signature does not cover those fields.
func TestV8CannotCarryOrSatisfyExecutionEvidence(t *testing.T) {
	v8 := validV8Receipt(t)
	if v8.ExecutionBinding != nil || v8.ExposureLedgerBefore != nil {
		t.Fatal("a V8 receipt carries execution evidence")
	}

	v9 := validV9Receipt(t)
	stapled := validV8Receipt(t)
	stapled.GatewayKeyID = "gateway-demo-ed25519-v1"
	if err := stapled.ValidateUnsigned(); err != nil {
		t.Fatalf("the V8 baseline does not validate, so the negative case below proves nothing: %v", err)
	}
	stapled.ExecutionBinding = v9.ExecutionBinding
	stapled.ExposureLedgerBefore = v9.ExposureLedgerBefore
	if err := stapled.ValidateUnsigned(); err == nil {
		t.Fatal("a V8 receipt carrying an execution binding its signature does not cover was accepted")
	}

	// Downgrading a V9 to V8 must not silently drop the evidence into an
	// unsigned position either.
	downgraded := validV9Receipt(t)
	downgraded.Version = VersionV8
	if err := downgraded.ValidateUnsigned(); err == nil {
		t.Fatal("a V9 receipt relabelled as V8 was accepted")
	}
}

func TestV9SignsAndVerifies(t *testing.T) {
	receipt := validV9Receipt(t)
	if err := receipt.ValidateUnsigned(); err != nil {
		t.Fatalf("a well-formed V9 receipt was rejected: %v", err)
	}
	signer := DemoSigner([]byte("v9"))
	signed, err := signer.Sign(receipt)
	if err != nil {
		t.Fatalf("sign V9: %v", err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Verify(signed); err != nil {
		t.Fatalf("a signed V9 receipt did not verify: %v", err)
	}
}

// V9 requires both structures. A receipt claiming the version without them is
// asserting an execution binding it does not have.
func TestV9RequiresBothSignedStructures(t *testing.T) {
	for name, mutate := range map[string]func(*QueryReceiptV1){
		"no execution binding": func(r *QueryReceiptV1) { r.ExecutionBinding = nil },
		"no ledger pre-state":  func(r *QueryReceiptV1) { r.ExposureLedgerBefore = nil },
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validV9Receipt(t)
			mutate(&receipt)
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatal("a V9 receipt without its signed execution evidence was accepted")
			}
		})
	}
}

// The binding must name the pre-state and budget the receipt actually carries,
// or it could have been derived against a state nothing here describes.
func TestV9BindsItsPreStateAndBudget(t *testing.T) {
	t.Run("pre-state digest", func(t *testing.T) {
		receipt := validV9Receipt(t)
		binding := *receipt.ExecutionBinding
		binding.ExposureLedgerBeforeSHA256 = fixedDigest("9")
		resealed, err := binding.Seal()
		if err != nil {
			t.Fatal(err)
		}
		receipt.ExecutionBinding = &resealed
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a binding naming another exposure pre-state was accepted")
		}
	})

	t.Run("budget digest", func(t *testing.T) {
		receipt := validV9Receipt(t)
		binding := *receipt.ExecutionBinding
		binding.BudgetBeforeSHA256 = fixedDigest("9")
		resealed, err := binding.Seal()
		if err != nil {
			t.Fatal(err)
		}
		receipt.ExecutionBinding = &resealed
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a binding naming another budget pre-state was accepted")
		}
	})

	// Changing the receipt's own budget_before must invalidate the binding too:
	// the digest is what ties them together in both directions.
	t.Run("budget mutated on the receipt", func(t *testing.T) {
		receipt := validV9Receipt(t)
		receipt.BudgetBefore.Limits.Rows = 999
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("mutating budget_before left the execution binding valid")
		}
	})
}

// The row limits must be reproducible from the signed pre-state. A limit the
// pre-state cannot derive is a limit nothing authorized.
func TestV9RowLimitsMustReproduceFromThePreState(t *testing.T) {
	for name, mutate := range map[string]func(*querybinding.QueryExecutionBindingV1){
		"visible limit exceeds the budget": func(b *querybinding.QueryExecutionBindingV1) {
			b.VisibleRowLimit = 11
			b.Visible.RowLimit = 11
		},
		"companion evidence rows invented": func(b *querybinding.QueryExecutionBindingV1) {
			b.CompanionEvidenceRows = 3
			b.CompanionPolicyRows = 4
			b.Companion.RowLimit = 4
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validV9Receipt(t)
			binding := *receipt.ExecutionBinding
			companion := *binding.Companion
			binding.Companion = &companion
			mutate(&binding)
			resealed, err := binding.Seal()
			if err != nil {
				t.Fatalf("reseal: %v", err)
			}
			receipt.ExecutionBinding = &resealed
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatal("a row limit the signed pre-state cannot derive was accepted")
			}
		})
	}
}

// The signature must cover every member of the binding and the pre-state. Each
// case mutates a signed receipt and requires verification to fail.
func TestV9SignatureCoversTheCompleteExecutionBinding(t *testing.T) {
	signer := DemoSigner([]byte("v9-coverage"))
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signer.Sign(validV9Receipt(t))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := keyring.Verify(base); err != nil {
		t.Fatalf("baseline verification failed: %v", err)
	}

	for name, mutate := range map[string]func(*QueryReceiptV1){
		"exact digest": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Visible.ExactSQLSHA256 = fixedDigest("9")
		},
		"strict digest": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Visible.StrictASTSHA256 = fixedDigest("9")
		},
		"companion exact digest": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Companion.ExactSQLSHA256 = fixedDigest("9")
		},
		"companion strict digest": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Companion.StrictASTSHA256 = fixedDigest("9")
		},
		"visible row limit": func(r *QueryReceiptV1) {
			r.ExecutionBinding.VisibleRowLimit = 9
			r.ExecutionBinding.Visible.RowLimit = 9
		},
		"companion row limit": func(r *QueryReceiptV1) {
			r.ExecutionBinding.CompanionPolicyRows = 6
			r.ExecutionBinding.Companion.RowLimit = 6
		},
		"executed flag": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Companion.Executed = false
		},
		"path kind": func(r *QueryReceiptV1) {
			r.ExecutionBinding.PathKind = querybinding.PathSemanticReplay
		},
		"plan identity": func(r *QueryReceiptV1) {
			r.ExecutionBinding.PlanSHA256 = fixedDigest("9")
		},
		"compiler identity": func(r *QueryReceiptV1) {
			r.ExecutionBinding.CompilerSHA256 = fixedDigest("9")
		},
		"policy fingerprint": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Visible.PolicyFingerprint = "other-fingerprint"
		},
		"prepared target binding": func(r *QueryReceiptV1) {
			r.ExecutionBinding.Visible.PreparedTargetBindingSHA256 = fixedDigest("9")
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
			binding := *base.ExecutionBinding
			companion := *base.ExecutionBinding.Companion
			binding.Companion = &companion
			ledger := *base.ExposureLedgerBefore
			mutated.ExecutionBinding = &binding
			mutated.ExposureLedgerBefore = &ledger
			mutate(&mutated)
			if err := keyring.Verify(mutated); err == nil {
				t.Fatalf("mutating the %s left the signature valid", name)
			}
		})
	}
}

// Swapping the visible and companion statements must not verify.
func TestV9TargetRoleSwapFails(t *testing.T) {
	signer := DemoSigner([]byte("v9-swap"))
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signer.Sign(validV9Receipt(t))
	if err != nil {
		t.Fatal(err)
	}

	swapped := base
	binding := *base.ExecutionBinding
	visible, companion := base.ExecutionBinding.Visible, *base.ExecutionBinding.Companion
	binding.Visible.ExactSQLSHA256, binding.Visible.StrictASTSHA256 =
		companion.ExactSQLSHA256, companion.StrictASTSHA256
	swappedCompanion := companion
	swappedCompanion.ExactSQLSHA256, swappedCompanion.StrictASTSHA256 =
		visible.ExactSQLSHA256, visible.StrictASTSHA256
	binding.Companion = &swappedCompanion
	swapped.ExecutionBinding = &binding

	if err := keyring.Verify(swapped); err == nil {
		t.Fatal("a receipt with swapped visible and companion statements verified")
	}
	if err := swapped.ValidateUnsigned(); err == nil {
		t.Fatal("a swapped execution binding validated")
	}
}

// An idempotent replay returns the original signed receipt unchanged, so its
// execution binding must be byte-identical rather than re-derived.
func TestV9ReplayReturnsAByteIdenticalExecutionBinding(t *testing.T) {
	signer := DemoSigner([]byte("v9-replay"))
	original, err := signer.Sign(validV9Receipt(t))
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
	if replayed.ExecutionBinding.SHA256 != original.ExecutionBinding.SHA256 {
		t.Fatal("the replayed execution binding is not the original")
	}
	if !replayed.ExecutionBinding.Equal(*original.ExecutionBinding) {
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
func TestV9CarriesNoSQL(t *testing.T) {
	signer := DemoSigner([]byte("v9-no-sql"))
	signed, err := signer.Sign(validV9Receipt(t))
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
func TestV9RefusesASemanticReplayThatExecuted(t *testing.T) {
	receipt := validV9Receipt(t)
	binding := *receipt.ExecutionBinding
	binding.PathKind = querybinding.PathSemanticReplay
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a semantic replay that executed its targets sealed cleanly")
	}

	// An idempotent replay never creates a binding at all.
	idempotent := *receipt.ExecutionBinding
	idempotent.PathKind = querybinding.PathIdempotentReplay
	if _, err := idempotent.Seal(); err == nil {
		t.Fatal("an idempotent replay produced a new execution binding")
	}
}
