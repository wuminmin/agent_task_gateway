package experiment

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// ReceiptVerifierV3 verifies a Query Receipt's signature.
//
// It is an interface so the finalizer depends on the act of verification rather
// than on one key source. It is not optional: a nil verifier is rejected, because
// an unverified receipt is a document the Adapter could have written.
type ReceiptVerifierV3 interface {
	Verify(queryreceipt.QueryReceiptV1) error
}

// TrustedInputsV3 is what the finalizer side supplies to acceptance. Every field
// is obtained without asking the Adapter.
//
// PathKind is deliberately absent. It is read from the Gateway's own signed
// execution binding inside the wrapper, because a path kind supplied alongside
// the evidence is a claim about the evidence rather than a property of it.
// The executed statements are likewise absent: the wrapper reproduces them from
// Material through internal/physicalquery and compares them with the signed
// bytes, rather than accepting them from a caller.
// CatalogPath, Footprint and Material describe an execution against Business
// PostgreSQL. An exact request-ID replay performs none, so for it all three must
// be EMPTY: they are not ignored on that path, they are rejected, because
// supplying them would mean finalizing a replay against schema and qualification
// material belonging to some other request.
type TrustedInputsV3 struct {
	// CatalogPath is the activated Profile Catalog. The ExpectedSchema is built
	// from it rather than accepted as a digest. Empty on an idempotent replay.
	CatalogPath string
	// Footprint is the qualified Attestation footprint from its own retained
	// qualification evidence. Zero on an idempotent replay.
	Footprint AttestationFootprintV2
	// PostgreSQL is the runtime identity read from the running deployment.
	PostgreSQL PostgreSQLRuntimeIdentity
	// OperationID and ContractIdentity come from the frozen workload contract.
	OperationID      string
	ContractIdentity string
	// Material is the frozen contract material the finalizer prepares the
	// operation from. Required on every executing path and forbidden on an
	// idempotent replay, which prepared nothing.
	//
	// It replaced a pair of string fields that claimed to hold "the statements
	// the finalizer reproduced" and held whatever the caller put there. Taking
	// the material instead means the wrapper does the reproducing, so there is no
	// longer a way to reach acceptance while skipping it.
	Material *FrozenOperationMaterialV3
	// StrictAST computes structural identities; nil uses the package default.
	StrictAST physicalquery.StrictASTDigester

	// Replay, when present, is the Control Store's typed evidence that THIS
	// request was an exact request-ID replay. It is how the idempotent path is
	// recognised at all.
	//
	// An idempotent replay returns the ORIGINAL receipt unchanged, so the
	// binding it carries describes the original execution and names that
	// original path kind -- QueryExecutionBindingV2.Validate rejects a binding
	// that claims idempotent_replay outright, precisely because no new binding
	// is produced. Reading the path kind off the returned receipt would
	// therefore misclassify every idempotent replay as the execution it is
	// replaying, and expect a visible and a companion statement from a request
	// that never reached Business PostgreSQL.
	//
	// So the replay is established from the store rather than from the document.
	Replay *IdempotentReplayEvidenceV1
	// SettlementWroteExecutionBindingRow is whether the settlement transaction
	// for THIS request wrote an execution binding row, observed in the control
	// store. A novel settlement must have written one; a replay must not.
	SettlementWroteExecutionBindingRow bool
}

// IdempotentReplayEvidenceV1 is the Control Store's account of an exact
// request-ID replay. Every member is read finalizer-side from the store or from
// the transport; none of it may be supplied by the Adapter, which is the party
// whose claim is being checked.
//
// The receipt documents are carried as raw stored bytes rather than as decoded
// structs. Comparing two already-decoded values compares what the decoder chose
// to keep: an unknown member, a field ordering, a numeric formatting or a
// dropped whitespace difference all survive re-encoding as equality. Gate 22
// asks whether the ORIGINAL DOCUMENT came back, and only the bytes answer that.
type IdempotentReplayEvidenceV1 struct {
	// TaskID, RequestID and RequestDigest identify the replayed request, and
	// must match the receipt that came back.
	TaskID        string
	RequestID     string
	RequestDigest string
	// OriginalQueryID is the query the store settled under this request ID.
	OriginalQueryID string
	// PersistedReceiptJSON is the receipt document as the Control Store holds
	// it, byte for byte.
	PersistedReceiptJSON []byte
	// ReturnedReceiptJSON is the document the Gateway returned for this request,
	// byte for byte as received.
	ReturnedReceiptJSON []byte
	// PersistedReceiptSHA256 and PersistedSignature are the stored receipt's
	// identity, so a rewritten row is detectable even if it re-encodes to the
	// same shape.
	PersistedReceiptSHA256 string
	PersistedSignature     string
	// The three absences that make the replay idempotent. Each is observed in
	// the control store for THIS request.
	WroteNewQueryRow            bool
	WroteNewExecutionBindingRow bool
	WroteNewReservation         bool
}

// Validate rejects replay evidence that is not complete.
//
// Absence is the substance of this evidence, and an absent field and a false
// boolean are indistinguishable once decoded. Requiring every identifier means
// evidence that was never populated fails rather than reading as three
// convenient negatives.
func (evidence IdempotentReplayEvidenceV1) Validate() error {
	for name, value := range map[string]string{
		"task_id":                  evidence.TaskID,
		"request_id":               evidence.RequestID,
		"request_digest":           evidence.RequestDigest,
		"original_query_id":        evidence.OriginalQueryID,
		"persisted receipt sha256": evidence.PersistedReceiptSHA256,
		"persisted signature":      evidence.PersistedSignature,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("idempotent replay evidence carries no %s", name)
		}
	}
	if len(evidence.PersistedReceiptJSON) == 0 {
		return errors.New("idempotent replay evidence carries no persisted receipt document")
	}
	if len(evidence.ReturnedReceiptJSON) == 0 {
		return errors.New("idempotent replay evidence carries no returned receipt document")
	}
	return nil
}

// pathKindForBinding maps the signed binding's path kind onto the accounting's.
//
// The two enumerations are declared in different packages because production
// must not import the evaluation tree. Mapping them explicitly, and failing on
// anything unrecognised, is what keeps them from drifting silently: a new
// production path kind becomes a finalization failure rather than a default.
func pathKindForBinding(kind querybinding.PathKind) (GatewayPathKind, error) {
	switch kind {
	case querybinding.PathPairedNovel:
		return PathPairedNovel, nil
	case querybinding.PathSingleQuery:
		return PathSingleQuery, nil
	case querybinding.PathSemanticReplay:
		return PathSemanticReplay, nil
	case querybinding.PathIdempotentReplay:
		return PathIdempotentReplay, nil
	default:
		return "", fmt.Errorf("signed execution binding carries unknown path_kind %q", kind)
	}
}

// finalizeTaskGateObservationV3Core adjudicates one operation.
//
// It is package-private, and that is the boundary. It takes TrustedInputsV3,
// which is the finalizer's own answer, so any caller able to reach it could
// supply that answer -- and the party whose claim is being checked is a caller
// like any other once the function is exported. RuntimeFinalizerV3 is the public
// entry point; it constructs the trusted inputs and then calls this.
//
// The order is the point. The receipt is verified first, the path kind and the
// signed target records are read from it, the Adapter's carried evidence is
// compared against those signed records field by field, and only then is
// FinalizeObservationV3 asked to derive everything else independently. At no
// point does an Adapter-supplied value feed a derivation, and the Adapter's own
// verdict is never read at all -- there is no parameter through which it could
// be passed.
func finalizeTaskGateObservationV3Core(receipt queryreceipt.QueryReceiptV1, verifier ReceiptVerifierV3,
	carried CarriedEvidenceV3, trusted TrustedInputsV3) (FinalizationV3, error) {
	var result FinalizationV3

	// 1. The receipt, before anything is read out of it.
	if verifier == nil {
		return result, rejectTaskGateAt(
			errors.New("finalization requires a receipt verifier; an unverified receipt is not evidence"),
			rejectionGateFinalizerInstance, rejectionFailureUnavailable,
			rejectionSourceFinalizerVerifier, rejectionSourceFinalizerVerifier)
	}
	if err := queryreceipt.RequireCurrentVersion(receipt.Version); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("v3 finalization requires the current receipt: %w", err),
			rejectionGateReceiptCurrentVersion, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	if err := receipt.Validate(); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("receipt does not validate: %w", err),
			rejectionGateReceiptDocument, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	if err := verifier.Verify(receipt); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("verify receipt: %w", err),
			rejectionGateReceiptSignature, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	receiptSHA256, err := queryreceipt.DocumentSHA256(receipt)
	if err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("identify verified receipt document: %w", err),
			rejectionGateReceiptDocumentIdentity, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	binding := receipt.ExecutionBindingV2
	if binding == nil {
		return result, rejectTaskGateAt(errors.New("receipt describes no execution; a completed query states which "+
			"physical statements produced its rows"), rejectionGateExecutionBinding,
			rejectionFailureMissing, rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	if err := binding.Validate(); err != nil {
		return result, rejectTaskGateAt(fmt.Errorf("signed execution binding does not validate: %w", err),
			rejectionGateExecutionBinding, rejectionFailureInvalidValue,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}
	// The exposure pre-state is required because these are exposure workloads,
	// not because carrying a binding implies carrying one. The receipt now admits
	// a completed operation that accounted no exposure -- it signs an empty
	// profile and no ledger -- and such an operation produces no exposure
	// evidence for this finalization to be about. Coupling the two would have
	// reported it as a malformed receipt rather than as the wrong workload.
	if binding.ExposureProfileVersion == "" || receipt.ExposureLedgerBefore == nil {
		return result, rejectTaskGateAt(errors.New("this receipt accounts no exposure; v3 finalization is about "+
			"exposure-accounted operations and has nothing to derive for one"),
			rejectionGateExposureAccounting, rejectionFailureMissing,
			rejectionSourceFinalizerVerifier, rejectionSourceGatewayReceipt)
	}

	// 2. The path kind. For an idempotent replay it comes from the control
	// store, because the returned receipt is the original one and describes the
	// original execution; for every other path it comes from the Gateway's
	// signature. Neither source is the Adapter.
	var pathKind GatewayPathKind
	if trusted.Replay != nil {
		if err := requireIdempotentReplay(receipt, trusted); err != nil {
			return result, rejectTaskGateAt(err, rejectionGateReplayEvidence,
				rejectionFailureMismatch, rejectionSourceControlStore, rejectionSourceGatewayReceipt)
		}
		pathKind = PathIdempotentReplay
	} else {
		if !trusted.SettlementWroteExecutionBindingRow {
			return result, rejectTaskGateAt(
				errors.New("a non-replay settlement wrote no execution binding row"),
				rejectionGateSettlementExecutionBinding, rejectionFailureMissing,
				rejectionSourceControlStore, rejectionSourceGatewayReceipt)
		}
		derived, err := pathKindForBinding(binding.PathKind)
		if err != nil {
			return result, rejectTaskGateAt(err, rejectionGateSettlementExecutionBinding,
				rejectionFailureInvalidValue, rejectionSourceControlStore, rejectionSourceGatewayReceipt)
		}
		pathKind = derived
		// 3. The signed target records must match the path the Gateway signed,
		// and the Adapter's carried statement identities must match those
		// records. An idempotent replay settles no target of its own, so there
		// is nothing here to check for it.
		if err := requireSignedTargets(pathKind, *binding, carried); err != nil {
			return result, rejectTaskGateAt(err, rejectionGateSignedTargets,
				rejectionFailureMismatch, rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence)
		}
	}

	// 4. The statements, reproduced rather than received. This is the step the
	// string fields used to stand in for: the finalizer prepares the operation
	// from frozen material, requires the sealed result to be the preparation the
	// Gateway signed, and authorizes it against the receipt's own pre-state. What
	// comes back is compared with the signature by everything below.
	reproduced, err := reproduceForPath(receipt, pathKind, trusted)
	if err != nil {
		return result, rejectTaskGateAt(err, rejectionGateFrozenMaterial,
			rejectionFailureInvalidValue, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt)
	}
	if dimensions, _ := dimensionsFor(pathKind); dimensions.requiresSchema {
		if err := requireReproducedMatchesSigned(pathKind, *binding, reproduced); err != nil {
			return result, rejectTaskGateAt(err, rejectionGateSignedReproducedTargets,
				rejectionFailureMismatch, rejectionSourceFinalizerDerivation, rejectionSourceGatewayReceipt)
		}
	}

	// 5. Everything else is derived independently.
	inputs := IndependentInputsV3{
		CatalogPath: trusted.CatalogPath, Footprint: trusted.Footprint,
		PostgreSQL: trusted.PostgreSQL, PathKind: pathKind,
		OperationID: trusted.OperationID, ContractIdentity: trusted.ContractIdentity,
		VisibleSQL: reproduced.VisibleSQL, CompanionSQL: reproduced.CompanionSQL,
		StrictAST: trusted.StrictAST,
	}
	finalized, err := FinalizeObservationV3(carried, inputs)
	if err != nil {
		return result, err
	}
	// A replay that reached Business PostgreSQL is not a replay. The derived
	// idempotent plan already expects zero in every class, so Accept has checked
	// this class by class; restating it on the total is what makes the failure
	// say the thing gate 22 is about rather than naming one class.
	if pathKind == PathIdempotentReplay && finalized.Delta.Total != 0 {
		return result, rejectTaskGateAt(fmt.Errorf("an idempotent replay moved the observer Business total by %d; "+
			"it must reach Business PostgreSQL not at all", finalized.Delta.Total),
			rejectionGateIdempotentBusinessTotal, rejectionFailureMismatch,
			rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
			rejectionCountDifference(rejectionDifferenceExpectedCount, 0),
			rejectionCountDifference(rejectionDifferenceActualCount, finalized.Delta.Total))
	}
	finalized.ReceiptSHA256 = receiptSHA256
	return finalized, nil
}

// requireIdempotentReplay checks the properties that make a replay idempotent:
// the stored document came back unchanged, its identity is unchanged, and the
// settlement created nothing.
//
// The document comparison is over raw stored bytes. Comparing two decoded
// structs would compare what the decoder chose to keep -- an unknown member, a
// re-ordered field, a reformatted number all survive re-encoding as equality --
// and the question gate 22 asks is whether the original document came back.
func requireIdempotentReplay(receipt queryreceipt.QueryReceiptV1, trusted TrustedInputsV3) error {
	evidence := *trusted.Replay
	if err := evidence.Validate(); err != nil {
		return rejectTaskGateAt(err, rejectionGateReplayEvidence,
			rejectionFailureInvalidValue, rejectionSourceControlStore, rejectionSourceControlStore)
	}
	if trusted.SettlementWroteExecutionBindingRow {
		return rejectTaskGateAt(errors.New("the trusted settlement evidence and the replay evidence disagree "+
			"about whether an execution binding row was written"), rejectionGateReplayEvidence,
			rejectionFailureMismatch, rejectionSourceControlStore, rejectionSourceControlStore,
			rejectionBoolDifference(rejectionDifferenceExpectedBool, false),
			rejectionBoolDifference(rejectionDifferenceActualBool, true))
	}
	// Nothing may have been created. Each of these is a separate row a replay
	// must not produce, and naming them separately is what makes a partial
	// replay -- one that reserved budget, say -- fail rather than average out.
	for name, written := range map[string]bool{
		"a new query row":             evidence.WroteNewQueryRow,
		"a new execution binding row": evidence.WroteNewExecutionBindingRow,
		"a new reservation":           evidence.WroteNewReservation,
	} {
		if written {
			return rejectTaskGateAt(fmt.Errorf("an idempotent replay wrote %s; the original receipt is "+
				"returned unchanged and nothing is settled", name), rejectionGateReplayEvidence,
				rejectionFailureMismatch, rejectionSourceControlStore, rejectionSourceControlStore,
				rejectionBoolDifference(rejectionDifferenceExpectedBool, false),
				rejectionBoolDifference(rejectionDifferenceActualBool, true))
		}
	}
	// The returned document must be the stored one, byte for byte.
	if !bytes.Equal(evidence.ReturnedReceiptJSON, evidence.PersistedReceiptJSON) {
		return rejectTaskGateAt(
			errors.New("the returned receipt document is not the persisted document byte for byte"),
			rejectionGateReplayEvidence, rejectionFailureMismatch,
			rejectionSourceControlStore, rejectionSourceCarriedEvidence)
	}
	// And the stored document's own identity must be unchanged, so a row
	// rewritten to re-encode identically is still detectable.
	storedDigest := fmt.Sprintf("%x", sha256.Sum256(evidence.PersistedReceiptJSON))
	if storedDigest != evidence.PersistedReceiptSHA256 {
		return rejectTaskGateAt(
			fmt.Errorf("the persisted receipt document digests to %s but the store records %s",
				shortDigest(storedDigest), shortDigest(evidence.PersistedReceiptSHA256)),
			rejectionGateReplayEvidence, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceControlStore,
			rejectionSHA256Pair(storedDigest, evidence.PersistedReceiptSHA256)...)
	}
	if receipt.Signature != evidence.PersistedSignature {
		return rejectTaskGateAt(
			errors.New("the replayed receipt's signature differs from the persisted signature"),
			rejectionGateReplayEvidence, rejectionFailureMismatch,
			rejectionSourceControlStore, rejectionSourceGatewayReceipt)
	}
	// The receipt that came back must be the one this request settled.
	if receipt.TaskID != evidence.TaskID || receipt.RequestID != evidence.RequestID ||
		receipt.RequestDigest != evidence.RequestDigest || receipt.QueryID != evidence.OriginalQueryID {
		return rejectTaskGateAt(
			errors.New("the replayed receipt does not identify the request the store recorded"),
			rejectionGateReplayEvidence, rejectionFailureMismatch,
			rejectionSourceControlStore, rejectionSourceGatewayReceipt)
	}
	return nil
}

// requireSignedTargets checks the signed binding against the path it claims, and
// the carried evidence against the signed binding.
//
// The two directions are separate on purpose. The first says the Gateway's own
// document is internally coherent for the path it names -- a semantic replay
// that reports an executed target is rejected here even if the Adapter carried
// nothing at all. The second says the Adapter transcribed that document exactly.
func requireSignedTargets(pathKind GatewayPathKind, binding querybinding.QueryExecutionBindingV2,
	carried CarriedEvidenceV3) error {
	visible, companion, known := requiredTargets(pathKind)
	if !known {
		return fmt.Errorf("path_kind %q is not a derivable execution path", pathKind)
	}
	// A path that executes nothing must still not have signed an executed
	// target. Authorization is allowed without execution -- that is exactly what
	// a semantic replay does -- but never the reverse.
	executes := pathKind == PathPairedNovel || pathKind == PathSingleQuery

	if visible > 0 {
		if binding.Visible.Role != querybinding.RoleVisible {
			return fmt.Errorf("the signed visible target carries role %q", binding.Visible.Role)
		}
		if err := requireTargetExecution("visible", binding.Visible, executes); err != nil {
			return err
		}
		if carried.VisibleStatement == nil {
			return rejectTaskGateTargetAt(
				fmt.Errorf("path_kind %s settles a visible statement but none was carried", pathKind),
				rejectionGateCarriedTargets, rejectionFailureMissing,
				rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence,
				rejectionTargetRoleVisible)
		}
		if err := requireCarriedMatchesSigned(rejectionTargetRoleVisible, binding.Visible, *carried.VisibleStatement,
			carried.VisiblePreparedTargetBindingSHA256); err != nil {
			return err
		}
	}
	switch {
	case companion > 0:
		if binding.Companion == nil {
			return fmt.Errorf("path_kind %s settles a companion statement but the binding signs none", pathKind)
		}
		if binding.Companion.Role != querybinding.RoleCompanion {
			return fmt.Errorf("the signed companion target carries role %q", binding.Companion.Role)
		}
		if err := requireTargetExecution("companion", *binding.Companion, executes); err != nil {
			return err
		}
		if carried.CompanionStatement == nil {
			return rejectTaskGateTargetAt(
				fmt.Errorf("path_kind %s settles a companion statement but none was carried", pathKind),
				rejectionGateCarriedTargets, rejectionFailureMissing,
				rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence,
				rejectionTargetRoleCompanion)
		}
		if err := requireCarriedMatchesSigned(rejectionTargetRoleCompanion, *binding.Companion, *carried.CompanionStatement,
			carried.CompanionPreparedTargetBindingSHA256); err != nil {
			return err
		}
	case binding.Companion != nil:
		// A replay authorizes its companion in order to derive the semantic key,
		// so the record legitimately exists; it must not claim execution.
		if err := requireTargetExecution("companion", *binding.Companion, false); err != nil {
			return err
		}
	}
	return nil
}

// requireTargetExecution enforces the authorized/executed pair for the path.
//
// Both directions matter. A target that was never authorized cannot have been
// executed, and a path that reaches Business PostgreSQL not at all must not have
// signed an execution. Collapsing the two flags would make a semantic replay
// indistinguishable from a novel execution, which is the distinction the replay
// gates exist to test.
func requireTargetExecution(role string, target querybinding.TargetRecordV1, executes bool) error {
	if !target.Authorized {
		return fmt.Errorf("the signed %s target was never authorized", role)
	}
	if target.Executed != executes {
		return fmt.Errorf("the signed %s target reports executed=%t on a path that executes=%t",
			role, target.Executed, executes)
	}
	return nil
}

// requireCarriedMatchesSigned compares every field the Adapter could have got
// wrong against the Gateway's signature.
//
// The row limit and the policy fingerprint are checked because neither is
// visible in a structural digest: a budget difference normalizes to a
// placeholder, and a renderer or policy change alters the executed bytes without
// altering the plan. The prepared target binding is checked because it is what
// stops one target's statement being presented as another's.
func requireCarriedMatchesSigned(targetRole rejectionTargetRole, signed querybinding.TargetRecordV1,
	statement physicalquery.StatementIdentity, preparedTargetBinding string) error {
	role := enumName(rejectionTargetRoleNames[:], int(targetRole))
	if role == "" {
		return rejectTaskGateAt(errors.New("carried target has no closed target role"),
			rejectionGateCarriedTargets, rejectionFailureInvalidValue,
			rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence)
	}
	if signed.ExactSQLSHA256 != statement.ExactSHA256 {
		return rejectTaskGateTargetAt(
			fmt.Errorf("the carried %s statement is %s, the Gateway signed %s; the executed bytes differ",
				role, shortDigest(statement.ExactSHA256), shortDigest(signed.ExactSQLSHA256)),
			rejectionGateCarriedTargets, rejectionFailureMismatch,
			rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence, targetRole,
			rejectionSHA256Pair(signed.ExactSQLSHA256, statement.ExactSHA256)...)
	}
	if signed.StrictASTSHA256 != statement.StrictASTSHA256 {
		return rejectTaskGateTargetAt(
			fmt.Errorf("the carried %s statement has structural identity %s, the Gateway signed %s",
				role, shortDigest(statement.StrictASTSHA256), shortDigest(signed.StrictASTSHA256)),
			rejectionGateCarriedTargets, rejectionFailureMismatch,
			rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence, targetRole,
			rejectionSHA256Pair(signed.StrictASTSHA256, statement.StrictASTSHA256)...)
	}
	if signed.RowLimit != statement.RowLimit {
		differences := []rejectionDifferenceV1(nil)
		if signed.RowLimit >= 0 && statement.RowLimit >= 0 {
			differences = append(differences,
				rejectionCountDifference(rejectionDifferenceExpectedCount, signed.RowLimit),
				rejectionCountDifference(rejectionDifferenceActualCount, statement.RowLimit))
		}
		return rejectTaskGateTargetAt(
			fmt.Errorf("the carried %s statement was rendered with row limit %d, the Gateway signed %d",
				role, statement.RowLimit, signed.RowLimit),
			rejectionGateCarriedTargets, rejectionFailureMismatch,
			rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence, targetRole, differences...)
	}
	if signed.PolicyFingerprint != statement.Fingerprint {
		return rejectTaskGateTargetAt(
			fmt.Errorf("the carried %s statement carries policy fingerprint %s, the Gateway signed %s",
				role, shortDigest(statement.Fingerprint), shortDigest(signed.PolicyFingerprint)),
			rejectionGateCarriedTargets, rejectionFailureMismatch,
			rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence, targetRole,
			rejectionSHA256Pair(signed.PolicyFingerprint, statement.Fingerprint)...)
	}
	if signed.PreparedTargetBindingSHA256 != preparedTargetBinding {
		return rejectTaskGateTargetAt(fmt.Errorf("the carried %s target is prepared as %s, the Gateway signed %s; "+
			"a statement cannot be presented as another target's",
			role, shortDigest(preparedTargetBinding), shortDigest(signed.PreparedTargetBindingSHA256)),
			rejectionGateCarriedTargets, rejectionFailureMismatch,
			rejectionSourceGatewayReceipt, rejectionSourceCarriedEvidence, targetRole,
			rejectionSHA256Pair(signed.PreparedTargetBindingSHA256, preparedTargetBinding)...)
	}
	return nil
}

// requireReproducedMatchesSigned closes the missing signed-to-reproduced (S-R)
// edge: each target identity the Gateway signed must equal the identity emitted
// by the finalizer's own physicalquery authorization. On semantic replay this is
// the only comparison of authorized-but-not-executed target statement identity.
//
// The comparison is deliberately driven by what the binding actually signed,
// not by requiredTargets or dimensionsFor target counts. The original hole
// existed because those counts describe how many statements a path EXECUTES;
// semantic replay executes zero while still signing two authorized targets, so
// a count-driven loop skips exactly the records this edge exists to check.
func requireReproducedMatchesSigned(pathKind GatewayPathKind,
	binding querybinding.QueryExecutionBindingV2, reproduced ReproducedExecutionV3) error {
	compare := func(role string, signed querybinding.TargetRecordV1,
		actual physicalquery.StatementIdentity) error {
		if signed.ExactSQLSHA256 != actual.ExactSHA256 {
			return fmt.Errorf("path_kind %s: the finalizer itself reproduced %s exact SQL SHA-256 %s, "+
				"but the Gateway signed %s", pathKind, role,
				shortDigest(actual.ExactSHA256), shortDigest(signed.ExactSQLSHA256))
		}
		if signed.StrictASTSHA256 != actual.StrictASTSHA256 {
			return fmt.Errorf("path_kind %s: the finalizer itself reproduced %s strict AST SHA-256 %s, "+
				"but the Gateway signed %s", pathKind, role,
				shortDigest(actual.StrictASTSHA256), shortDigest(signed.StrictASTSHA256))
		}
		// Keep row limit on the direct S-R edge even though Gate 31 cannot light
		// this branch: its raised-limit mutations fail requireReproducibleLimits
		// or querybinding.validateLimits while sealing, and any otherwise coherent
		// signed limit mismatch loses to requireDerivedLimitsSignedV3 before this
		// call. Those invariants live in different packages and may evolve
		// independently, so direct comparator tests retain this defense in depth.
		if signed.RowLimit != actual.RowLimit {
			return fmt.Errorf("path_kind %s: the finalizer itself reproduced %s row limit %d, "+
				"but the Gateway signed %d", pathKind, role, actual.RowLimit, signed.RowLimit)
		}
		if signed.PolicyFingerprint != actual.Fingerprint {
			return fmt.Errorf("path_kind %s: the finalizer itself reproduced %s policy fingerprint %s, "+
				"but the Gateway signed %s", pathKind, role,
				shortDigest(actual.Fingerprint), shortDigest(signed.PolicyFingerprint))
		}
		return nil
	}

	if err := compare("visible", binding.Visible, reproduced.Visible); err != nil {
		return err
	}
	if (binding.Companion != nil) != (reproduced.Companion != nil) {
		return fmt.Errorf("path_kind %s: the finalizer itself reproduced companion target presence=%t, "+
			"but the Gateway signed presence=%t", pathKind,
			reproduced.Companion != nil, binding.Companion != nil)
	}
	if binding.Companion != nil {
		if err := compare("companion", *binding.Companion, *reproduced.Companion); err != nil {
			return err
		}
	}
	return nil
}

// reproduceForPath runs the finalizer's own preparation for the paths that have
// one, and requires the material to be absent for the paths that do not.
//
// The absence is checked rather than tolerated, for the same reason CatalogPath
// and Footprint are: an idempotent replay reached Business PostgreSQL not at
// all, so material offered to finalize one describes a different request, and
// preparing from it would produce statements this request never ran.
func reproduceForPath(receipt queryreceipt.QueryReceiptV1, pathKind GatewayPathKind,
	trusted TrustedInputsV3) (ReproducedExecutionV3, error) {
	dimensions, known := dimensionsFor(pathKind)
	if !known {
		return ReproducedExecutionV3{}, rejectTaskGateAt(
			fmt.Errorf("path_kind %q is not a derivable execution path", pathKind),
			rejectionGateMaterialPresence, rejectionFailureInvalidValue,
			rejectionSourceFinalizerDerivation, rejectionSourceGatewayReceipt)
	}
	if !dimensions.requiresSchema {
		if trusted.Material != nil {
			return ReproducedExecutionV3{}, rejectTaskGateAt(
				fmt.Errorf("path_kind %s prepares no operation, but frozen "+
					"preparation material was supplied to finalize it", pathKind),
				rejectionGateMaterialPresence, rejectionFailureInvalidValue,
				rejectionSourceFinalizerDerivation, rejectionSourceFrozenContract)
		}
		return ReproducedExecutionV3{}, nil
	}
	if trusted.Material == nil {
		return ReproducedExecutionV3{}, rejectTaskGateAt(
			fmt.Errorf("path_kind %s executes prepared statements, but no "+
				"frozen material was supplied for the finalizer to reproduce them from", pathKind),
			rejectionGateMaterialPresence, rejectionFailureMissing,
			rejectionSourceFinalizerDerivation, rejectionSourceFrozenContract)
	}
	return ReproduceExecutionV3(receipt, *trusted.Material)
}
