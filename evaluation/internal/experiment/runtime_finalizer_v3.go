package experiment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// This file is the production boundary of v3 acceptance, and it exists to
// resolve a contradiction between two guards that were both right.
//
// One says every TaskGate workload must reach v3 finalization. The other says an
// Adapter may never name TrustedInputsV3, because an Adapter that could build
// the finalizer's inputs would be supplying the material its own claim is
// checked against. Taken together they say the Adapter must call something that
// is not the wrapper: a finalizer-side entry point that takes evidence and
// constructs its own answer.
//
// That entry point is RuntimeFinalizerV3. The division is:
//
//	the Adapter submits          -- a signed receipt, what it observed, and a
//	                                case number;
//	the finalizer fetches        -- the frozen contract, the Catalog, the
//	                                artifacts, the qualification, the runtime
//	                                identity and the Control Store's account of
//	                                the request;
//	the private core adjudicates -- and never sees a caller-supplied input.
//
// The public surface takes no trusted material of any kind. That is the whole
// design: not a rule about how callers should behave, but a shape in which the
// wrong call cannot be written.
//
// # Why the construction surface is closed too
//
// Keeping trusted material out of the REQUEST is only half of it. The finalizer
// reads its material through collaborators, so whoever constructs the finalizer
// chooses where that material comes from -- and an Adapter that assembled its
// own finalizer would have picked the archive it is then judged against. That is
// the same defect one level up, and a guard on the request type would not see
// it.
//
// So the collaborators, their types and the constructor are all package-private.
// An Adapter cannot name them, cannot implement one that would be accepted, and
// cannot call the constructor; it receives an already-opened *RuntimeFinalizerV3
// and can do nothing with it but submit evidence. The exported surface of this
// package offers no way to decide what the finalizer reads.
//
// The deployment constructor that wires the real registries -- frozen contracts,
// activated profiles, retained qualification evidence, the running database and
// the Control Store -- belongs in this package and lands with the first cutover,
// which is the first caller that needs a real one. It is deliberately not a stub
// here: an artifact that ships a resolver nothing has ever executed is worse
// evidence than one that ships none.

// FrozenContractSelectorV3 narrows the search for the frozen contract this
// operation was defined by.
//
// It is a HINT and carries no authority. The finalizer prepares every candidate
// the selector admits and keeps the one whose preparation equals the one the
// Gateway signed, so a selector naming the wrong cell does not finalize the
// wrong contract -- it finalizes nothing. That is why an Adapter may supply it:
// being wrong about it can only cause a rejection, never a wrong acceptance.
type FrozenContractSelectorV3 struct {
	ExperimentID string
	WorkloadID   string
	Scale        string
	Mode         string
	// BindingKey narrows a public protocol cell to one exact private operation
	// variant when that cell deliberately covers more than one executable
	// statement. Like the four public coordinates it is only a hint: the
	// finalizer independently loads and validates the private binding, prepares
	// every admitted candidate, and accepts only the one the Gateway signed.
	BindingKey string
}

func (selector FrozenContractSelectorV3) String() string {
	coordinate := strings.Join([]string{selector.ExperimentID, selector.WorkloadID,
		selector.Scale, selector.Mode}, "/")
	if selector.BindingKey == "" {
		return coordinate
	}
	return coordinate + "#binding-key=" + selector.BindingKey
}

// frozenOperationCandidateV3 is one operation the frozen contracts define.
//
// The plan and the approved surface come from the contract, never from the
// request. ProfileID names which activated deployment profile the operation runs
// under, resolved separately, so a contract cannot also decide which Catalog it
// is attested against.
type frozenOperationCandidateV3 struct {
	OperationID      string
	ContractIdentity string
	// OutcomeCandidate is the independent ordinary-set oracle for the exact
	// Scale Outcome candidate. It is private contract material: the Adapter
	// cannot supply it and the Gateway's production radix set cannot derive it.
	// Nil means this frozen operation is outside the strict Scale binding.
	OutcomeCandidate *OutcomeCandidateExpectationV1
	// ScaleDependency is the independent role-bound semantic oracle for the
	// exact dependency Scale cell. It never contains a production set digest.
	ScaleDependency *ScaleDependencySetExpectationV1
	// BindingKey is empty for one-operation public cells. A non-empty key names
	// one independently validated private variant beneath a public cell and is
	// compared only as a selector hint.
	BindingKey string
	ProfileID  string
	// PathKind is the execution path the contract defines for this cell. It is
	// needed BEFORE the operation runs, to pre-register the classification, and
	// it is a contract fact rather than an observation: a cell that then takes a
	// different path fails finalization, which is the correct outcome and not a
	// defect in the commitment.
	PathKind GatewayPathKind
	Plan     queryplan.QueryPlan
	Grant    physicalquery.Grant
}

// profileMaterialV3 is one activated deployment profile's immutable material.
type profileMaterialV3 struct {
	CatalogPath         string
	SnapshotArtifactDir string
}

// requestSettlementStateV3 is the Control Store's account of one request.
//
// It is read finalizer-side and never accepted from a caller. An Adapter that
// could assert "this was a replay" or "no binding row was written" would be
// deciding how the finalizer interprets the execution, which is the same defect
// as supplying the statements.
type requestSettlementStateV3 struct {
	// Replay is non-nil when the store recorded this request as an exact
	// request-ID replay. Its ReturnedReceiptJSON is filled by the finalizer from
	// the transport, because the store cannot know what came back over the wire.
	Replay *IdempotentReplayEvidenceV1
	// WroteExecutionBindingRow is whether THIS request's settlement transaction
	// wrote an execution binding row.
	WroteExecutionBindingRow bool
}

// The collaborators the finalizer reads its trusted material through.
//
// They are interfaces so that where each value comes from is a deployment
// concern rather than a property of acceptance, and so that a test can supply a
// deterministic one. None of them is reachable from a request: a caller chooses
// nothing about which implementation answers.
type (
	// contractResolverV3 returns every frozen operation a selector admits.
	// Returning candidates rather than one operation is deliberate -- see
	// FrozenContractSelectorV3.
	contractResolverV3 interface {
		ResolveCandidates(FrozenContractSelectorV3) ([]frozenOperationCandidateV3, error)
	}
	// profileMaterialResolverV3 maps a deployment profile to the Catalog and
	// artifacts it was activated with.
	profileMaterialResolverV3 interface {
		Resolve(profileID string) (profileMaterialV3, error)
	}
	// footprintResolverV3 loads the qualified Attestation footprint from its own
	// retained qualification evidence, for this Catalog and this runtime.
	footprintResolverV3 interface {
		Resolve(catalogPath string, runtime PostgreSQLRuntimeIdentity) (AttestationFootprintV2, error)
	}
	// runtimeIdentityReaderV3 reads what the deployment is actually running.
	runtimeIdentityReaderV3 interface {
		ReadPostgreSQLIdentity(context.Context) (PostgreSQLRuntimeIdentity, error)
	}
	// controlEvidenceReaderV3 reads the Control Store's account of one request.
	controlEvidenceReaderV3 interface {
		ReadRequestState(ctx context.Context, taskID, requestID string) (requestSettlementStateV3, error)
	}
)

// RuntimeFinalizerV3 is the production entry point to v3 acceptance.
type RuntimeFinalizerV3 struct {
	verifier   ReceiptVerifierV3
	contracts  contractResolverV3
	profiles   profileMaterialResolverV3
	footprints footprintResolverV3
	runtime    runtimeIdentityReaderV3
	control    controlEvidenceReaderV3
	scaleSets  scaleDependencySetVerifierV1
	// observerWindows owns the ephemeral signing key and the one-use attempt
	// registry. Keeping both behind the already-opened finalizer is what prevents
	// the Adapter from minting or resetting its own preregistration tickets.
	observerWindows *observerWindowTicketsV3
}

// openRuntimeFinalizerV3 builds the finalizer, refusing any missing
// collaborator.
//
// Every one of them is a source of trusted material, so a nil is not a degraded
// mode to fall back from -- it is a finalizer that would have to take that
// material from somewhere else, and the only other place is the caller.
//
// It is package-private on purpose. Exporting it would let a caller choose which
// registry answers, which is choosing the standard the evidence is judged by;
// see the file comment.
func openRuntimeFinalizerV3(verifier ReceiptVerifierV3, contracts contractResolverV3,
	profiles profileMaterialResolverV3, footprints footprintResolverV3,
	runtime runtimeIdentityReaderV3, control controlEvidenceReaderV3,
	scaleSets scaleDependencySetVerifierV1) (*RuntimeFinalizerV3, error) {
	for name, present := range map[string]bool{
		"a receipt verifier":          verifier != nil,
		"a frozen contract resolver":  contracts != nil,
		"a profile material resolver": profiles != nil,
		"a footprint resolver":        footprints != nil,
		"a runtime identity reader":   runtime != nil,
		"a control evidence reader":   control != nil,
		"a Scale set verifier":        scaleSets != nil,
	} {
		if !present {
			return nil, fmt.Errorf("a runtime finalizer needs %s; without one its trusted material "+
				"could only come from the party being checked", name)
		}
	}
	observerWindows, err := newObserverWindowTicketsV3()
	if err != nil {
		return nil, err
	}
	return &RuntimeFinalizerV3{verifier: verifier, contracts: contracts, profiles: profiles,
		footprints: footprints, runtime: runtime, control: control, scaleSets: scaleSets,
		observerWindows: observerWindows}, nil
}

// VerifyScaleDependencySetV1 resolves the semantic oracle and activated
// publication finalizer-side, then links one immutable production set named by
// the caller. A wrong selector, role or production digest can only reject.
func (finalizer *RuntimeFinalizerV3) VerifyScaleDependencySetV1(ctx context.Context,
	request ScaleDependencySetVerificationRequestV1) (ScaleDependencySetVerificationV1, error) {
	var verification ScaleDependencySetVerificationV1
	if finalizer == nil || finalizer.scaleSets == nil {
		return verification, errors.New("Scale dependency verification requires an opened runtime finalizer")
	}
	if !validSHA256(request.ProductionSetSHA256) {
		return verification, errors.New("Scale dependency verification requires a production set digest")
	}
	candidates, err := finalizer.contracts.ResolveCandidates(request.ContractSelector)
	if err != nil {
		return verification, fmt.Errorf("resolve frozen Scale dependency candidate: %w", err)
	}
	if len(candidates) != 1 || candidates[0].ScaleDependency == nil {
		return verification, fmt.Errorf("Scale dependency verification selector names %d eligible candidates", len(candidates))
	}
	expectation := *candidates[0].ScaleDependency
	if err := expectation.Validate(); err != nil {
		return verification, fmt.Errorf("validate frozen Scale dependency expectation: %w", err)
	}
	if _, err := expectation.forRole(request.Role); err != nil {
		return verification, err
	}
	profile, err := finalizer.profiles.Resolve(candidates[0].ProfileID)
	if err != nil {
		return verification, fmt.Errorf("resolve Scale dependency deployment profile: %w", err)
	}
	return finalizer.scaleSets.Verify(ctx, profile, expectation, request.Role, request.ProductionSetSHA256)
}

// FinalizationRequestV3 is everything an Adapter may submit.
//
// There is no member here through which trusted material could arrive: no plan,
// no grant, no Catalog path, no footprint, no replay claim, no settlement
// claim. What an Adapter can get wrong, it can only get wrong into a rejection.
type FinalizationRequestV3 struct {
	// Receipt is what the Gateway returned. It is verified before anything is
	// read out of it, so submitting it is not the same as being believed.
	Receipt queryreceipt.QueryReceiptV1
	// Carried is what the Adapter observed and claims. Every member of it is
	// compared against the finalizer's own derivation and none of it feeds one.
	Carried CarriedEvidenceV3
	// ContractSelector narrows the frozen contract search. A hint, not an
	// authority -- see FrozenContractSelectorV3.
	ContractSelector FrozenContractSelectorV3
	// ObserverWindowTicket is the finalizer-issued one-use preregistration for
	// this exact task/request attempt. It is verified and atomically consumed
	// after the receipt signature is verified and before any acceptance material
	// is derived.
	ObserverWindowTicket ObserverWindowTicketV3
	// ReturnedReceiptJSON is the response document byte for byte, needed only on
	// the idempotent replay path, where the question is whether the stored
	// document came back unchanged. The bytes come from the transport because
	// nothing else knows what was actually returned; they are compared against
	// the store's, so a rewritten copy fails.
	ReturnedReceiptJSON []byte
}

// FinalizeTaskGateObservationV3 is the production acceptance entry point.
//
// It fetches, then adjudicates. Nothing between those two steps consults the
// request except for the receipt whose signature it has verified, the carried
// evidence it will only compare against, and a selector that cannot decide the
// answer.
func (finalizer *RuntimeFinalizerV3) FinalizeTaskGateObservationV3(ctx context.Context,
	request FinalizationRequestV3) (FinalizationV3, error) {
	var result FinalizationV3
	if finalizer == nil {
		return result, rejectTaskGateAt(
			errors.New("v3 finalization requires an opened runtime finalizer"),
			rejectionGateFinalizerInstance, rejectionFailureUnavailable,
			rejectionSourceFinalizerDerivation, rejectionSourceFinalizerDerivation)
	}
	// The signature first, because everything below reads the receipt: the
	// request state is looked up by ITS task and request id, and the contract
	// candidate is chosen by comparing against ITS signed preparation.
	if err := queryreceipt.RequireCurrentVersion(request.Receipt.Version); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("v3 finalization requires the current receipt: %w", err),
			rejectionGateReceiptCurrentVersion, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	if err := request.Receipt.Validate(); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("receipt does not validate: %w", err),
			rejectionGateReceiptDocument, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	if err := finalizer.verifier.Verify(request.Receipt); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("verify receipt: %w", err),
			rejectionGateReceiptSignature, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	if err := finalizer.verifyAndConsumeObserverWindowTicketV3(request); err != nil {
		return result, rejectTaskGateAt(err, rejectionGateObserverTicket,
			rejectionFailureInvalidValue, rejectionSourceObserverTicket, rejectionSourceCarriedEvidence)
	}
	trusted, err := finalizer.deriveTrustedInputs(ctx, request)
	if err != nil {
		return result, rejectTaskGateAt(err, rejectionGateFinalizerInternal,
			rejectionFailureInvalidValue, rejectionSourceFinalizerDerivation,
			rejectionSourceFinalizerDerivation)
	}
	finalized, err := finalizeTaskGateObservationV3Core(
		request.Receipt, finalizer.verifier, request.Carried, trusted)
	if err != nil {
		return result, rejectTaskGateAt(err, rejectionGateFinalizerInternal,
			rejectionFailureInvalidValue, rejectionSourceFinalizerDerivation,
			rejectionSourceFinalizerDerivation)
	}
	return finalized, nil
}

// deriveTrustedInputs assembles the finalizer's own answer.
//
// The order is the argument. The Control Store decides whether this was a
// replay, because only it knows; a replay then needs none of the execution
// material and is refused if it somehow resolves any. Everything else is
// fetched, and the contract is not selected but IDENTIFIED -- by preparing the
// candidates and keeping the one the Gateway's signature agrees with.
func (finalizer *RuntimeFinalizerV3) deriveTrustedInputs(ctx context.Context,
	request FinalizationRequestV3) (TrustedInputsV3, error) {
	var trusted TrustedInputsV3
	state, err := finalizer.control.ReadRequestState(ctx, request.Receipt.TaskID, request.Receipt.RequestID)
	if err != nil {
		return trusted, rejectTaskGateAt(fmt.Errorf("read the Control Store's account of this request: %w", err),
			rejectionGateControlRequestState, rejectionFailureUnavailable,
			rejectionSourceControlStore, rejectionSourceControlStore)
	}
	postgres, err := finalizer.runtime.ReadPostgreSQLIdentity(ctx)
	if err != nil {
		return trusted, rejectTaskGateAt(fmt.Errorf("read the deployment's PostgreSQL identity: %w", err),
			rejectionGateDeploymentPostgreSQLIdentity, rejectionFailureUnavailable,
			rejectionSourceDeploymentRuntime, rejectionSourceDeploymentRuntime)
	}
	trusted.PostgreSQL = postgres
	trusted.SettlementWroteExecutionBindingRow = state.WroteExecutionBindingRow

	if state.Replay != nil {
		// A replay reached Business PostgreSQL not at all, so it resolves no
		// contract, no Catalog and no footprint. The returned bytes are the one
		// thing only the transport knows; the store's copy is what they are
		// compared against.
		replay := *state.Replay
		replay.ReturnedReceiptJSON = request.ReturnedReceiptJSON
		if len(replay.ReturnedReceiptJSON) == 0 {
			return trusted, rejectTaskGateAt(
				errors.New("an idempotent replay is finalized on whether the stored document "+
					"came back unchanged, and the returned bytes were not submitted"),
				rejectionGateReplayReturnedReceipt, rejectionFailureMissing,
				rejectionSourceControlStore, rejectionSourceCarriedEvidence)
		}
		trusted.Replay = &replay
		trusted.OperationID, trusted.ContractIdentity = replay.OriginalQueryID, request.Carried.Operation.ContractIdentity
		return trusted, nil
	}

	candidate, material, err := finalizer.identifyOperation(request)
	if err != nil {
		return trusted, err
	}
	profile, err := finalizer.profiles.Resolve(candidate.ProfileID)
	if err != nil {
		return trusted, rejectTaskGateAt(fmt.Errorf("resolve deployment profile %q: %w", candidate.ProfileID, err),
			rejectionGateProfileMaterial, rejectionFailureUnavailable,
			rejectionSourceActivatedProfile, rejectionSourceActivatedProfile)
	}
	footprint, err := finalizer.footprints.Resolve(profile.CatalogPath, postgres)
	if err != nil {
		return trusted, rejectTaskGateAt(fmt.Errorf("resolve the qualified Attestation footprint: %w", err),
			rejectionGateQualifiedFootprintResolve, rejectionFailureUnavailable,
			rejectionSourceRetainedQualification, rejectionSourceDeploymentRuntime)
	}
	trusted.CatalogPath, trusted.Footprint = profile.CatalogPath, footprint
	trusted.OperationID, trusted.ContractIdentity = candidate.OperationID, candidate.ContractIdentity
	trusted.OutcomeCandidate = cloneOutcomeCandidateExpectationV1(candidate.OutcomeCandidate)
	trusted.Material = &material
	return trusted, nil
}

// identifyOperation finds which frozen operation this receipt describes, by
// preparing the candidates rather than by believing the selector.
//
// Exactly one candidate must reproduce. Zero means the receipt describes an
// operation no frozen contract defines, which is a finalization failure and not
// a lookup miss. More than one would mean two contract cells compile to the same
// preparation, so the evidence cannot say which ran -- and picking either would
// be the finalizer guessing.
func (finalizer *RuntimeFinalizerV3) identifyOperation(request FinalizationRequestV3) (
	frozenOperationCandidateV3, FrozenOperationMaterialV3, error) {
	candidates, err := finalizer.contracts.ResolveCandidates(request.ContractSelector)
	if err != nil {
		return frozenOperationCandidateV3{}, FrozenOperationMaterialV3{},
			rejectTaskGateAt(fmt.Errorf("resolve frozen contract candidates for %s: %w", request.ContractSelector, err),
				rejectionGateCandidateResolution, rejectionFailureUnavailable,
				rejectionSourceFrozenContract, rejectionSourceFrozenContract)
	}
	if len(candidates) == 0 {
		return frozenOperationCandidateV3{}, FrozenOperationMaterialV3{},
			rejectTaskGateAt(fmt.Errorf("no frozen contract operation matches selector %s", request.ContractSelector),
				rejectionGateCandidateSelectorCount, rejectionFailureMissing,
				rejectionSourceFrozenContract, rejectionSourceCarriedEvidence,
				rejectionCountDifference(rejectionDifferenceExpectedCount, 1),
				rejectionCountDifference(rejectionDifferenceActualCount, 0))
	}
	var (
		matched   []frozenOperationCandidateV3
		materials []FrozenOperationMaterialV3
		refusals  []string
		members   = make(map[preparedbinding.PreparedOperationMember]bool)
	)
	for _, candidate := range candidates {
		profile, profileErr := finalizer.profiles.Resolve(candidate.ProfileID)
		if profileErr != nil {
			refusals = append(refusals, fmt.Sprintf("%s: %v", candidate.OperationID, profileErr))
			continue
		}
		material := FrozenOperationMaterialV3{
			CatalogPath: profile.CatalogPath, SnapshotArtifactDir: profile.SnapshotArtifactDir,
			Plan: candidate.Plan, Grant: candidate.Grant,
		}
		if _, reproduceErr := ReproduceExecutionV3(request.Receipt, material); reproduceErr != nil {
			refusals = append(refusals, fmt.Sprintf("%s: %v", candidate.OperationID, reproduceErr))
			var mismatch *preparedbinding.PreparedOperationMismatchError
			if errors.As(reproduceErr, &mismatch) {
				for _, member := range mismatch.Members() {
					members[member] = true
				}
			}
			continue
		}
		matched = append(matched, candidate)
		materials = append(materials, material)
	}
	switch len(matched) {
	case 1:
		return matched[0], materials[0], nil
	case 0:
		sort.Strings(refusals)
		differences := []rejectionDifferenceV1{
			rejectionCountDifference(rejectionDifferenceCandidateCount, int64(len(candidates))),
			rejectionCountDifference(rejectionDifferenceRefusedCandidateCount, int64(len(refusals))),
			rejectionCountDifference(rejectionDifferenceMatchedCandidateCount, 0),
		}
		preparedMembers := make([]preparedbinding.PreparedOperationMember, 0, len(members))
		for member := range members {
			preparedMembers = append(preparedMembers, member)
		}
		differences = append(differences, rejectionPreparedMemberDifferences(preparedMembers)...)
		return frozenOperationCandidateV3{}, FrozenOperationMaterialV3{},
			rejectTaskGateAt(fmt.Errorf("no frozen contract operation reproduces the signed execution; %d candidate(s) "+
				"were refused: %s", len(refusals), strings.Join(refusals, "; ")),
				rejectionGateCandidateMatchCount, rejectionFailureMismatch,
				rejectionSourceFrozenContract, rejectionSourceGatewayReceipt, differences...)
	default:
		names := make([]string, 0, len(matched))
		for _, candidate := range matched {
			names = append(names, candidate.OperationID)
		}
		sort.Strings(names)
		return frozenOperationCandidateV3{}, FrozenOperationMaterialV3{},
			rejectTaskGateAt(fmt.Errorf("%v all reproduce the signed execution, so the evidence does not say which ran",
				names), rejectionGateCandidateMatchCount, rejectionFailureAmbiguous,
				rejectionSourceFrozenContract, rejectionSourceGatewayReceipt,
				rejectionCountDifference(rejectionDifferenceCandidateCount, int64(len(candidates))),
				rejectionCountDifference(rejectionDifferenceMatchedCandidateCount, int64(len(matched))))
	}
}

// PreRegisteredObservationV3 is the classification one operation is committed to
// before it runs.
//
// # Why the Adapter is given all of it
//
// The observer only needs ClassifierManifestSHA256: that is the digest it seals
// into both snapshots, and the one ObserverWindowV2.Delta later enforces. The
// other three members exist because CarriedEvidenceV3 requires them and the
// Adapter cannot derive them.
//
// That is not an oversight in the carried type. A classifier manifest is built
// from the operation's rendered target statements, and an Adapter that held
// those would be holding the material its own claim is checked against -- which
// is the whole reason the finalizer reproduces them instead. So there is no
// version of this where the Adapter derives its own classification.
//
// What the carried copy is worth is therefore not "a second derivation". It is
// the binding between the classification committed BEFORE the operation ran and
// the operation finalization identifies AFTER it ran, by the Gateway's
// signature. Those two are reached by different routes -- a selector beforehand,
// a signed preparation afterwards -- so requiring them to be equal is a real
// check: an operation that executed something other than the cell whose
// classification was pre-registered produces a different plan, a different
// operation identity and a different binding digest, and finalization rejects
// it. What it is not is evidence that the Adapter derived anything, and nothing
// downstream may read it as such.
type PreRegisteredObservationV3 struct {
	// Operation is the identity the classifier is bound to.
	Operation OperationIdentity
	// Plan is the independently derived control plan the class set came from.
	Plan GatewayControlPlanV3
	// ClassifierManifestSHA256 is what the observer is invoked with and seals.
	ClassifierManifestSHA256 string
	// ClassifierBindingSHA256 ties the operation, the plan and the manifest
	// together; it is the digest a manifest presented for another operation or
	// under another plan cannot reproduce.
	ClassifierBindingSHA256 string
	// ObserverWindowID is a fresh random identity issued for this attempt, not a
	// stable derivative of the frozen operation. It is sealed into both observer
	// snapshots.
	ObserverWindowID string
	// ObserverWindowTicket binds that window and classification to the exact
	// task/request pair and can be consumed once at finalization.
	ObserverWindowTicket ObserverWindowTicketV3
}

// OpenObserverWindowV3 is the pre-registration step, and it runs BEFORE the
// operation does.
//
// # Why the classification is committed before the observations exist
//
// The observer is invoked with the digest of the classifier manifest its window
// will be judged against, and it seals that digest into the snapshot. So the
// question "which statements counted as expected" is answered before there are
// any statements to look at.
//
// Without that, the manifest could be built after the window closed -- from a
// derivation that has already seen what ran. Nothing in the resulting evidence
// would distinguish "the observations matched the classification" from "the
// classification was chosen to match the observations", and that is the standard
// objection to any measurement whose analysis is fixed after the data.
//
// It is the FINALIZER that computes it, for the same reason it computes
// everything else: a manifest the measured party chose is a standard the
// measured party set.
//
// # Why a nominal pre-state is sound here
//
// The manifest keys on the STRUCTURAL identity of each target statement, and
// structure is invariant to the row limits the authorizer renders -- the limits
// are literals, which the strict AST digest normalizes away. So the statements
// can be derived against a nominal ledger state before the real one exists.
//
// This is not taken on trust. FinalizeObservationV3 rebuilds the manifest from
// the operation's REAL pre-state and requires the carried digest to equal it, so
// an assumption that turned out to be false shows up as a rejected sample rather
// than as a commitment that quietly meant nothing.
//
// # Why the selector decides here, and does not later
//
// There is no receipt yet, so there is no signature to identify the operation
// by; the selector has to name exactly one candidate. That is a real difference
// from finalization, where a wrong selector cannot cause a wrong acceptance --
// and it does not weaken the result, because a selector that named the wrong
// cell produces a manifest that the finalization then rebuilds differently and
// rejects.
func (finalizer *RuntimeFinalizerV3) OpenObserverWindowV3(ctx context.Context,
	selector FrozenContractSelectorV3, attempt ObserverAttemptV3) (PreRegisteredObservationV3, error) {
	var registered PreRegisteredObservationV3
	if finalizer == nil {
		return registered, errors.New("opening an observer window requires an opened runtime finalizer")
	}
	candidates, err := finalizer.contracts.ResolveCandidates(selector)
	if err != nil {
		return registered, fmt.Errorf("resolve frozen contract candidates for %s: %w", selector, err)
	}
	if len(candidates) != 1 {
		return registered, fmt.Errorf("pre-registering a classification requires the selector to name exactly "+
			"one frozen operation; %s names %d", selector, len(candidates))
	}
	candidate := candidates[0]
	profile, err := finalizer.profiles.Resolve(candidate.ProfileID)
	if err != nil {
		return registered, fmt.Errorf("resolve deployment profile %q: %w", candidate.ProfileID, err)
	}
	postgres, err := finalizer.runtime.ReadPostgreSQLIdentity(ctx)
	if err != nil {
		return registered, fmt.Errorf("read the deployment's PostgreSQL identity: %w", err)
	}
	footprint, err := finalizer.footprints.Resolve(profile.CatalogPath, postgres)
	if err != nil {
		return registered, fmt.Errorf("resolve the qualified Attestation footprint: %w", err)
	}
	material := frozenOperationMaterialV3ForCandidate(candidate, profile)
	visibleSQL, companionSQL, err := prepareNominalStatementsV3(material)
	if err != nil {
		return registered, err
	}
	registered, err = preRegisterObservationV3(candidate, profile, footprint, visibleSQL, companionSQL)
	if err != nil {
		return PreRegisteredObservationV3{}, err
	}
	ticket, err := finalizer.observerWindows.issue(attempt, registered)
	if err != nil {
		return PreRegisteredObservationV3{}, fmt.Errorf("issue observer window ticket: %w", err)
	}
	registered.ObserverWindowID = ticket.ObserverWindowID
	registered.ObserverWindowTicket = ticket
	return registered, nil
}

func frozenOperationMaterialV3ForCandidate(candidate frozenOperationCandidateV3,
	profile profileMaterialV3) FrozenOperationMaterialV3 {
	return FrozenOperationMaterialV3{
		CatalogPath: profile.CatalogPath, SnapshotArtifactDir: profile.SnapshotArtifactDir,
		Plan: candidate.Plan, Grant: candidate.Grant,
	}
}
