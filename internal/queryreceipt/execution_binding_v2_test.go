package queryreceipt

import (
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// executionCompiler is the typed compiler identity every fixture preparation is
// sealed against, and every fixture target record names. The binding requires
// the three to agree.
func executionCompiler(t *testing.T) preparedbinding.CompilerIdentityV1 {
	t.Helper()
	sealed, err := preparedbinding.CompilerIdentityV1{
		QueryPlanVersion: "queryplan-v7", QueryPlanSHA256: fixedDigest("c2"),
		PolicyRendererVersion: "sqlpolicy-v3", PolicyRendererSHA256: fixedDigest("a3"),
	}.Seal()
	if err != nil {
		t.Fatalf("seal compiler identity: %v", err)
	}
	return sealed
}

// preparedOperationFixture is a preparation under expanded evidence, sealed
// against the prepared-target digests the fixture's target records carry.
func preparedOperationFixture(t *testing.T, hasCompanion bool) preparedbinding.PreparedOperationBindingV1 {
	t.Helper()
	binding := preparedbinding.PreparedOperationBindingV1{
		// Grouped is false so that UsesExpandedEvidence -- the disjunction of
		// Grouped and ExpandedEvidence -- is decided by hasCompanion alone. The
		// disjunction itself is proved in internal/querybinding, where it can be
		// varied without every receipt fixture having to re-derive its limits.
		HasCompanion: hasCompanion, ExpandedEvidence: hasCompanion,
		VisibleFieldCount: 4, FactFieldCount: 2, ProvenanceFieldCount: 3,
		VisibleFieldsSHA256:      fixedDigest("11"),
		FactFieldsSHA256:         fixedDigest("12"),
		ProvenanceFieldsSHA256:   fixedDigest("13"),
		PreparationInputsSHA256:  fixedDigest("14"),
		GrantSHA256:              fixedDigest("15"),
		CatalogSHA256:            fixedDigest("16"),
		SnapshotBindingSetSHA256: fixedDigest("17"),
		PlanSHA256:               fixedDigest("18"),
		CompilerIdentitySHA256:   executionCompiler(t).SHA256,
		PolicyGrantSHA256:        fixedDigest("19"),
		NormalFormSHA256:         fixedDigest("1a"),
		OrdinalProgramSHA256:     fixedDigest("1b"),
		DictionarySetSHA256:      fixedDigest("1c"),
		SourcePublicationsSHA256: fixedDigest("1e"),
		PredicateFootprintSHA256: fixedDigest("26"),
		EstimatedBaseFacts:       4096,
		VisibleTargetSHA256:      fixedDigest("a4"),
	}
	if hasCompanion {
		binding.CompanionTargetSHA256 = fixedDigest("b4")
	}
	sealed, err := binding.Seal()
	if err != nil {
		t.Fatalf("seal prepared operation binding: %v", err)
	}
	return sealed
}

// validV10ArtifactReceipt is the artifact-delivery shape: the result was written
// to a registered object.
func validV10ArtifactReceipt(t *testing.T) QueryReceiptV1 {
	t.Helper()
	return validExecutionReceipt(t)
}

// validV10InlineReceipt is the shape an artifact-bearing receipt could not
// represent: a governed operation that returns its rows in the response and
// registers no result object. This is what a Scale or ProvSQL TaskGate produces.
func validV10InlineReceipt(t *testing.T) QueryReceiptV1 {
	t.Helper()
	receipt := validExecutionReceipt(t)
	receipt.ResultDeliveryMode = DeliveryInline
	receipt.ArtifactIntent = nil
	return receipt
}

func TestV10SignsAndVerifiesInBothDeliveryModes(t *testing.T) {
	for name, build := range map[string]func(*testing.T) QueryReceiptV1{
		"artifact": validV10ArtifactReceipt,
		"inline":   validV10InlineReceipt,
	} {
		t.Run(name, func(t *testing.T) {
			receipt := build(t)
			if err := receipt.ValidateUnsigned(); err != nil {
				t.Fatalf("a well-formed %s V10 receipt was rejected: %v", name, err)
			}
			signer := DemoSigner([]byte("v10-" + name))
			signed, err := signer.Sign(receipt)
			if err != nil {
				t.Fatalf("sign V10: %v", err)
			}
			keyring, err := NewKeyring(signer, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := keyring.Verify(signed); err != nil {
				t.Fatalf("a signed %s V10 receipt did not verify: %v", name, err)
			}
		})
	}
}

// The rule the whole version exists for: an inline delivery must not be forced
// to register a result object, and an artifact delivery must not be allowed to
// omit one.
func TestV10DeliveryModeDecidesTheArtifactIntent(t *testing.T) {
	t.Run("artifact mode without an intent is refused", func(t *testing.T) {
		receipt := validV10ArtifactReceipt(t)
		receipt.ArtifactIntent = nil
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("an artifact-mode V10 with no artifact intent was accepted")
		}
	})
	t.Run("inline mode with an intent is refused", func(t *testing.T) {
		receipt := validV10InlineReceipt(t)
		receipt.ArtifactIntent = validV10ArtifactReceipt(t).ArtifactIntent
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("an inline V10 carrying an artifact intent was accepted")
		}
	})
	t.Run("an unnamed or unknown mode is refused", func(t *testing.T) {
		for _, mode := range []ResultDeliveryMode{"", "parquet", "ARTIFACT", "inline "} {
			receipt := validV10ArtifactReceipt(t)
			receipt.ResultDeliveryMode = mode
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatalf("result_delivery_mode %q was accepted", mode)
			}
		}
	})
	t.Run("a query that did not complete delivered nothing", func(t *testing.T) {
		for _, status := range []string{StatusReleased, StatusFailed, StatusIndeterminate} {
			receipt := validV10ArtifactReceipt(t)
			receipt.Status = status
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatalf("a %s query claiming an artifact delivery was accepted", status)
			}
		}
	})
	t.Run("a completed query delivered something", func(t *testing.T) {
		receipt := validV10ArtifactReceipt(t)
		receipt.ResultDeliveryMode = DeliveryNone
		receipt.ArtifactIntent = nil
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a completed query claiming it delivered no result was accepted")
		}
	})
}

// The mode is signed. Flipping it after signing must break the signature, or it
// would be a description a holder could choose.
func TestV10DeliveryModeIsCoveredByTheSignature(t *testing.T) {
	signer := DemoSigner([]byte("v10-mode"))
	signed, err := signer.Sign(validV10InlineReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	flipped := signed
	flipped.ResultDeliveryMode = DeliveryArtifact
	if err := keyring.Verify(flipped); err == nil {
		t.Fatal("flipping the signed delivery mode left the signature valid")
	}
}

// A receipt naming any version but the current one is refused outright, before
// anything is read out of it.
//
// This is the whole of what version checking does now. There is one version and
// no reader for another, so a document naming a different one is not an older
// receipt to be handled leniently -- it has no contract in this build.
func TestOnlyTheCurrentVersionValidates(t *testing.T) {
	for _, version := range []string{"", "1", "8", "9", "11", "10 ", "V10"} {
		receipt := validV10ArtifactReceipt(t)
		receipt.Version = version
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatalf("a receipt naming version %q was accepted", version)
		}
		if IsCurrentVersion(version) {
			t.Fatalf("version %q was reported as current", version)
		}
	}
	if !IsCurrentVersion(Version) {
		t.Fatal("the current version was not reported as current")
	}
}

// The execution binding and its pre-state are two halves of one statement.
// Neither validates without the other, whichever way round it is missing.
func TestTheExecutionBindingAndItsPreStateAreCarriedTogether(t *testing.T) {
	t.Run("binding without the pre-state", func(t *testing.T) {
		receipt := validV10ArtifactReceipt(t)
		receipt.ExposureLedgerBefore = nil
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a receipt carrying a binding but no pre-state was accepted")
		}
	})
	t.Run("pre-state without the binding", func(t *testing.T) {
		receipt := validV10ArtifactReceipt(t)
		receipt.ExecutionBindingV2 = nil
		if err := receipt.ValidateUnsigned(); err == nil {
			t.Fatal("a receipt carrying a pre-state but no binding was accepted")
		}
	})
}

func TestV10BindingMustAgreeWithTheSignedPreStates(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *QueryReceiptV1){
		"binding names another exposure pre-state": func(t *testing.T, r *QueryReceiptV1) {
			rebuilt := *r.ExecutionBindingV2
			rebuilt.ExposureLedgerBeforeSHA256 = fixedDigest("f")
			sealed, err := rebuilt.Seal()
			if err != nil {
				t.Fatal(err)
			}
			r.ExecutionBindingV2 = &sealed
		},
		"binding names another budget pre-state": func(t *testing.T, r *QueryReceiptV1) {
			rebuilt := *r.ExecutionBindingV2
			rebuilt.BudgetBeforeSHA256 = fixedDigest("f")
			sealed, err := rebuilt.Seal()
			if err != nil {
				t.Fatal(err)
			}
			r.ExecutionBindingV2 = &sealed
		},
		"binding derives under another exposure profile": func(t *testing.T, r *QueryReceiptV1) {
			rebuilt := *r.ExecutionBindingV2
			rebuilt.ExposureProfileVersion = "taskgate-exposure-v4"
			sealed, err := rebuilt.Seal()
			if err != nil {
				t.Fatal(err)
			}
			r.ExecutionBindingV2 = &sealed
		},
		"preparation disagrees with the pre-state about expanded evidence": func(t *testing.T, r *QueryReceiptV1) {
			prepared := r.ExecutionBindingV2.PreparedOperation
			prepared.ExpandedEvidence = false
			resealed, err := prepared.Seal()
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := *r.ExecutionBindingV2
			rebuilt.PreparedOperation = resealed
			rebuilt.CompanionPolicyRows = rebuilt.CompanionEvidenceRows
			companion := *rebuilt.Companion
			companion.RowLimit = rebuilt.CompanionEvidenceRows
			rebuilt.Companion = &companion
			sealed, err := rebuilt.Seal()
			if err != nil {
				t.Fatal(err)
			}
			r.ExecutionBindingV2 = &sealed
		},
		"visible row limit exceeds what the pre-state authorizes": func(t *testing.T, r *QueryReceiptV1) {
			rebuilt := *r.ExecutionBindingV2
			rebuilt.VisibleRowLimit = r.ExposureLedgerBefore.RemainingRows + 1
			visible := rebuilt.Visible
			visible.RowLimit = rebuilt.VisibleRowLimit
			rebuilt.Visible = visible
			sealed, err := rebuilt.Seal()
			if err != nil {
				t.Fatal(err)
			}
			r.ExecutionBindingV2 = &sealed
		},
		"ledger pre-state postdates the settled charge": func(t *testing.T, r *QueryReceiptV1) {
			r.Exposure.RootEpoch = r.ExposureLedgerBefore.RootEpoch - 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := validV10ArtifactReceipt(t)
			mutate(t, &receipt)
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// A receipt must not report more rows than the statement it says executed could
// have returned. V9 had no way to make this check land on the binding; V10 does.
func TestV10RefusesARowCountAboveTheRenderedLimit(t *testing.T) {
	receipt := validV10ArtifactReceipt(t)
	receipt.RowCount = receipt.ExecutionBindingV2.VisibleRowLimit + 1
	receipt.BudgetCharged.Rows = receipt.RowCount
	if err := receipt.ValidateUnsigned(); err == nil {
		t.Fatal("a receipt reporting more rows than its visible statement could return was accepted")
	}
}

// The whole V2 document is signed, not merely its digest. Editing any member of
// the carried preparation must break the signature.
func TestV10SignsTheWholePreparation(t *testing.T) {
	signer := DemoSigner([]byte("v10-prepared"))
	signed, err := signer.Sign(validV10ArtifactReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := NewKeyring(signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*preparedbinding.PreparedOperationBindingV1){
		"policy grant":         func(b *preparedbinding.PreparedOperationBindingV1) { b.PolicyGrantSHA256 = fixedDigest("f") },
		"source publications":  func(b *preparedbinding.PreparedOperationBindingV1) { b.SourcePublicationsSHA256 = fixedDigest("f") },
		"predicate footprint":  func(b *preparedbinding.PreparedOperationBindingV1) { b.PredicateFootprintSHA256 = fixedDigest("f") },
		"estimated base facts": func(b *preparedbinding.PreparedOperationBindingV1) { b.EstimatedBaseFacts = 4097 },
		"preparation inputs":   func(b *preparedbinding.PreparedOperationBindingV1) { b.PreparationInputsSHA256 = fixedDigest("f") },
		"normal form":          func(b *preparedbinding.PreparedOperationBindingV1) { b.NormalFormSHA256 = fixedDigest("f") },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := signed
			binding := *signed.ExecutionBindingV2
			prepared := binding.PreparedOperation
			mutate(&prepared)
			resealed, sealErr := prepared.Seal()
			if sealErr != nil {
				t.Fatal(sealErr)
			}
			binding.PreparedOperation = resealed
			// Reseal the binding too, so the receipt is as internally coherent as a
			// forger could make it. Only the signature is left to catch the edit.
			sealedBinding, bindingErr := binding.Seal()
			if bindingErr != nil {
				t.Fatal(bindingErr)
			}
			tampered.ExecutionBindingV2 = &sealedBinding
			if err := keyring.Verify(tampered); err == nil {
				t.Fatalf("editing the preparation's %s left the V10 signature valid", name)
			}
		})
	}
}

// Audit code must not assume every V10 registered a result object.
func TestV10InlineReceiptsAreRefusedArtifactProofsByName(t *testing.T) {
	inline := validV10InlineReceipt(t)
	if inline.RequiresArtifactInclusionProofs() {
		t.Fatal("an inline V10 was reported as requiring artifact inclusion proofs")
	}
	if inline.CarriesArtifactIntent() {
		t.Fatal("an inline V10 reports an artifact intent")
	}
	// The proofs are empty on purpose: the refusal must come from the receipt
	// describing no result object, before any proof is examined. A verifier that
	// reached the chain first would be deciding on the wrong evidence.
	if err := VerifyArtifactIntentInclusion(inline, auditchain.InclusionProof{},
		auditchain.InclusionProof{}); err == nil {
		t.Fatal("artifact intent inclusion was proved for a receipt that registered no result object")
	}
	if err := VerifyArtifactAvailabilityInclusion(inline, auditchain.InclusionProof{}); err == nil {
		t.Fatal("artifact availability inclusion was proved for a receipt that registered no result object")
	}

	artifact := validV10ArtifactReceipt(t)
	if !artifact.RequiresArtifactInclusionProofs() {
		t.Fatal("an artifact-mode V10 was reported as not requiring artifact inclusion proofs")
	}
	if !artifact.CarriesArtifactIntent() {
		t.Fatal("an artifact-mode V10 reports no artifact intent")
	}
}

func TestV10CarriesNoSQL(t *testing.T) {
	for name, build := range map[string]func(*testing.T) QueryReceiptV1{
		"artifact": validV10ArtifactReceipt, "inline": validV10InlineReceipt,
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(build(t))
			if err != nil {
				t.Fatal(err)
			}
			lowered := strings.ToLower(string(encoded))
			for _, fragment := range []string{"select ", " from ", " where ", "insert ", "update "} {
				if strings.Contains(lowered, fragment) {
					t.Fatalf("the %s V10 receipt carries the SQL fragment %q", name, fragment)
				}
			}
		})
	}
}

// Only a completed query has an execution to describe. Under V8 and V9 this was
// implied by the unconditional artifact intent; V10 lifts that for inline
// delivery, so the requirement has to be stated on the execution evidence
// itself or an inline V10 could report a failure while describing what it ran.
func TestV10RequiresACompletedQueryInBothDeliveryModes(t *testing.T) {
	for name, build := range map[string]func(*testing.T) QueryReceiptV1{
		"artifact": validV10ArtifactReceipt, "inline": validV10InlineReceipt,
	} {
		t.Run(name, func(t *testing.T) {
			receipt := build(t)
			receipt.Status = StatusFailed
			receipt.ResultHash = ""
			receipt.ErrorCode = "QUERY_FAILED"
			if err := receipt.ValidateUnsigned(); err == nil {
				t.Fatalf("a failed %s V10 carrying an execution binding was accepted", name)
			}
		})
	}
}
