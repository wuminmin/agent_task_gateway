package experiment

import (
	"context"
	"errors"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
)

// The finalizer's collaborators, as deterministic test doubles. They stand in
// for a contract registry, a deployment profile, retained qualification
// evidence, the running database and the Control Store.
type (
	stubContracts struct {
		candidates []frozenOperationCandidateV3
		err        error
	}
	stubProfiles  struct{ material profileMaterialV3 }
	stubFootprint struct{ footprint AttestationFootprintV2 }
	stubRuntime   struct{ identity PostgreSQLRuntimeIdentity }
	stubControl   struct{ state requestSettlementStateV3 }
	stubScaleSets struct{}
)

func (s stubContracts) ResolveCandidates(FrozenContractSelectorV3) ([]frozenOperationCandidateV3, error) {
	return s.candidates, s.err
}
func (s stubProfiles) Resolve(string) (profileMaterialV3, error) { return s.material, nil }
func (s stubFootprint) Resolve(string, PostgreSQLRuntimeIdentity) (AttestationFootprintV2, error) {
	return s.footprint, nil
}
func (s stubRuntime) ReadPostgreSQLIdentity(context.Context) (PostgreSQLRuntimeIdentity, error) {
	return s.identity, nil
}
func (s stubControl) ReadRequestState(context.Context, string, string) (requestSettlementStateV3, error) {
	return s.state, nil
}
func (stubScaleSets) Verify(context.Context, profileMaterialV3, ScaleDependencySetExpectationV1,
	DependencyScaleSummaryRole, string) (ScaleDependencySetVerificationV1, error) {
	return ScaleDependencySetVerificationV1{}, errors.New("stub Scale set verifier was not configured")
}

// runtimeFinalizerFor builds a finalizer whose contract registry offers the
// given candidates.
func runtimeFinalizerFor(t *testing.T, candidates ...frozenOperationCandidateV3) *RuntimeFinalizerV3 {
	t.Helper()
	operation := prepareOperationV3(t)
	footprint, _ := realSchemaFootprint(t)
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	finalizer, err := openRuntimeFinalizerV3(verifier,
		stubContracts{candidates: candidates},
		stubProfiles{material: profileMaterialV3{
			CatalogPath:         operation.Material.CatalogPath,
			SnapshotArtifactDir: operation.Material.SnapshotArtifactDir,
		}},
		stubFootprint{footprint: footprint},
		stubRuntime{identity: testRuntimeIdentity()},
		stubControl{state: requestSettlementStateV3{WroteExecutionBindingRow: true}}, stubScaleSets{})
	if err != nil {
		t.Fatalf("open runtime finalizer: %v", err)
	}
	return finalizer
}

// honestCandidate is the frozen operation the receipt actually describes.
func honestCandidate(t *testing.T) frozenOperationCandidateV3 {
	t.Helper()
	operation := prepareOperationV3(t)
	return frozenOperationCandidateV3{
		OperationID: finalizerOperationID, ContractIdentity: finalizerContract,
		ProfileID: "result-heavy", PathKind: PathPairedNovel,
		Plan: operation.Material.Plan, Grant: operation.Material.Grant,
	}
}

// ticketedFinalizationRequestV3 gives tests of the runtime finalizer an honest
// issued ticket without forcing the contract resolver under test to also satisfy
// OpenObserverWindowV3's exactly-one-candidate rule. Dedicated ticket tests below
// exercise the public Open path itself.
func ticketedFinalizationRequestV3(t *testing.T, finalizer *RuntimeFinalizerV3,
	receipt queryreceipt.QueryReceiptV1, carried CarriedEvidenceV3,
	selector FrozenContractSelectorV3) FinalizationRequestV3 {
	t.Helper()
	registered := PreRegisteredObservationV3{
		Operation: carried.Operation, Plan: carried.Plan,
		ClassifierManifestSHA256: carried.ClassifierManifestSHA256,
		ClassifierBindingSHA256:  carried.ClassifierBindingSHA256,
	}
	ticket, err := finalizer.observerWindows.issue(ObserverAttemptV3{
		TaskID: receipt.TaskID, RequestID: receipt.RequestID,
	}, registered)
	if err != nil {
		t.Fatalf("issue test observer window ticket: %v", err)
	}
	carried.Window.Before.ObserverWindowID = ticket.ObserverWindowID
	carried.Window.After.ObserverWindowID = ticket.ObserverWindowID
	carried.Window.Before.ClassifierManifestSHA256 = ticket.ClassifierManifestSHA256
	carried.Window.After.ClassifierManifestSHA256 = ticket.ClassifierManifestSHA256
	return FinalizationRequestV3{
		Receipt: receipt, Carried: carried, ContractSelector: selector,
		ObserverWindowTicket: ticket,
	}
}

// A finalizer with no collaborator is not a degraded finalizer, it is one whose
// trusted material would have to come from its caller.
func TestOpenRuntimeFinalizerRefusesAMissingCollaborator(t *testing.T) {
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if _, err := openRuntimeFinalizerV3(verifier, nil, stubProfiles{}, stubFootprint{},
		stubRuntime{}, stubControl{}, stubScaleSets{}); err == nil {
		t.Fatal("a finalizer with no contract resolver was opened")
	}
	if _, err := openRuntimeFinalizerV3(nil, stubContracts{}, stubProfiles{}, stubFootprint{},
		stubRuntime{}, stubControl{}, stubScaleSets{}); err == nil {
		t.Fatal("a finalizer with no receipt verifier was opened")
	}
}

// The selector is a hint. A caller that names the wrong cell gets a rejection,
// never a finalization against the contract it named.
//
// This is what lets an Adapter supply the selector at all: it cannot be used to
// choose which standard the evidence is judged by, because the standard is
// chosen by agreeing with the Gateway's signature instead.
func TestTheContractSelectorHasNoAuthority(t *testing.T) {
	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build the signed receipt: %v", err)
	}
	honest := honestCandidate(t)

	// A registry that offers only an operation this receipt does not describe.
	wrong := honest
	wrong.OperationID = "some-other-operation"
	wrong.Plan.Columns = []string{"row_id"}
	finalizer := runtimeFinalizerFor(t, wrong)
	request := ticketedFinalizationRequestV3(t, finalizer, receipt, carriedFor(t, finalizerInputs(t)),
		FrozenContractSelectorV3{ExperimentID: "artifact", WorkloadID: "result-heavy"})
	_, err = finalizer.FinalizeTaskGateObservationV3(context.Background(), request)
	if err == nil {
		t.Fatal("the finalizer accepted a contract whose preparation the Gateway did not sign")
	}

	// And a registry offering the right one alongside the wrong one identifies
	// the right one, without the selector distinguishing them.
	finalizer = runtimeFinalizerFor(t, wrong, honest)
	request = ticketedFinalizationRequestV3(t, finalizer, receipt, carriedFor(t, finalizerInputs(t)),
		FrozenContractSelectorV3{ExperimentID: "artifact", WorkloadID: "result-heavy"})
	if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), request); err != nil {
		t.Fatalf("the finalizer did not identify the operation the receipt describes: %v", err)
	}
}

// Two contract cells that compile to one preparation leave the evidence unable
// to say which ran, and the finalizer says so rather than picking.
func TestAmbiguousContractCandidatesAreRefused(t *testing.T) {
	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build the signed receipt: %v", err)
	}
	first := honestCandidate(t)
	second := first
	second.OperationID = "a-second-cell-compiling-the-same-way"

	finalizer := runtimeFinalizerFor(t, first, second)
	request := ticketedFinalizationRequestV3(t, finalizer, receipt, carriedFor(t, finalizerInputs(t)),
		FrozenContractSelectorV3{ExperimentID: "artifact"})
	_, err = finalizer.FinalizeTaskGateObservationV3(context.Background(), request)
	if err == nil {
		t.Fatal("the finalizer chose between two contracts the evidence cannot distinguish")
	}
}

// A registry that offers nothing is a finalization failure, not a lookup miss:
// the receipt describes an operation no frozen contract defines.
func TestAnUndefinedOperationIsRefused(t *testing.T) {
	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build the signed receipt: %v", err)
	}
	for name, finalizer := range map[string]*RuntimeFinalizerV3{
		"no candidates": runtimeFinalizerFor(t),
		"a resolver that errs": func() *RuntimeFinalizerV3 {
			f := runtimeFinalizerFor(t, honestCandidate(t))
			f.contracts = stubContracts{err: errors.New("registry unavailable")}
			return f
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			request := ticketedFinalizationRequestV3(t, finalizer, receipt,
				carriedFor(t, finalizerInputs(t)), FrozenContractSelectorV3{})
			if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), request); err == nil {
				t.Fatal("acceptance succeeded with no frozen contract behind it")
			}
		})
	}
}

// An unverifiable receipt is refused before any material is fetched, so a forged
// document cannot even cause a contract lookup.
func TestAnUnverifiableReceiptNeverReachesTheRegistry(t *testing.T) {
	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build the signed receipt: %v", err)
	}
	receipt.Signature = "not-the-signature-that-was-made"
	finalizer := runtimeFinalizerFor(t, honestCandidate(t))
	request := ticketedFinalizationRequestV3(t, finalizer, receipt,
		carriedFor(t, finalizerInputs(t)), FrozenContractSelectorV3{})
	if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), request); err == nil {
		t.Fatal("an unverifiable receipt was finalized")
	}
}

// The property the pre-registration rests on: the classification committed
// BEFORE the operation ran is the one finalization derives AFTER it ran.
//
// If these two could differ, the digest sealed into the observer window would be
// a commitment to nothing -- every sample would fail, or worse, the finalizer
// would have to accept whichever one it liked. They agree because the manifest
// keys on structural statement identity, which is invariant to the row limits
// the authorizer renders from the operation's real ledger state.
func TestThePreRegisteredClassificationIsTheOneFinalizationDerives(t *testing.T) {
	honest := honestCandidate(t)
	finalizer := runtimeFinalizerFor(t, honest)

	committed, err := finalizer.OpenObserverWindowV3(context.Background(),
		FrozenContractSelectorV3{ExperimentID: "artifact", WorkloadID: "result-heavy"},
		ObserverAttemptV3{TaskID: "task-preregister-classification", RequestID: "request-preregister-classification"})
	if err != nil {
		t.Fatalf("pre-register the classification: %v", err)
	}

	// What finalization independently derives for the same operation, from the
	// operation's real pre-state rather than a nominal one.
	//
	// The whole commitment is compared, not only the manifest digest. The Adapter
	// carries every member of it into CarriedEvidenceV3 and finalization compares
	// every member against its own derivation, so a member that agreed only by
	// not being checked here would fail at acceptance instead.
	derived := carriedFor(t, finalizerInputs(t))
	for _, member := range []struct {
		name               string
		committed, derived any
	}{
		{"operation identity", committed.Operation, derived.Operation},
		{"classifier manifest", committed.ClassifierManifestSHA256, derived.ClassifierManifestSHA256},
		{"classifier binding", committed.ClassifierBindingSHA256, derived.ClassifierBindingSHA256},
	} {
		if member.committed != member.derived {
			t.Errorf("the %s committed before the window is %v but finalization derives %v;\n"+
				"the commitment sealed into the observer window would be to a different standard",
				member.name, member.committed, member.derived)
		}
	}
	if !committed.Plan.Equal(derived.Plan) {
		t.Errorf("the control plan committed before the window differs from the derived one on %v",
			derived.Plan.MismatchedFields(committed.Plan))
	}
}

// Pre-registration needs the selector to name one operation, because there is no
// signature yet to identify it by. An ambiguous selector is refused rather than
// resolved, so a window is never opened against a classification that might
// belong to another cell.
func TestPreRegistrationRefusesAnAmbiguousSelector(t *testing.T) {
	first := honestCandidate(t)
	second := first
	second.OperationID = "a-second-cell"
	finalizer := runtimeFinalizerFor(t, first, second)
	if _, err := finalizer.OpenObserverWindowV3(context.Background(),
		FrozenContractSelectorV3{ExperimentID: "artifact"},
		ObserverAttemptV3{TaskID: "task-ambiguous", RequestID: "request-ambiguous"}); err == nil {
		t.Fatal("a classification was pre-registered for a selector naming two operations")
	}
	if _, err := runtimeFinalizerFor(t).OpenObserverWindowV3(context.Background(),
		FrozenContractSelectorV3{ExperimentID: "artifact"},
		ObserverAttemptV3{TaskID: "task-absent", RequestID: "request-absent"}); err == nil {
		t.Fatal("a classification was pre-registered for a selector naming none")
	}
}
