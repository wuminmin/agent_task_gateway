package experiment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

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
// VisibleSQL and CompanionSQL are the statements the finalizer reproduced
// through internal/physicalquery from signed pre-state; they are compared with
// the signed bytes rather than trusted.
type TrustedInputsV3 struct {
	// CatalogPath is the activated Profile Catalog. The ExpectedSchema is built
	// from it rather than accepted as a digest.
	CatalogPath string
	// Footprint is the qualified Attestation footprint from its own retained
	// qualification evidence.
	Footprint AttestationFootprintV2
	// PostgreSQL is the runtime identity read from the running deployment.
	PostgreSQL PostgreSQLRuntimeIdentity
	// OperationID and ContractIdentity come from the frozen workload contract.
	OperationID      string
	ContractIdentity string
	// VisibleSQL and CompanionSQL are independently reproduced statements.
	VisibleSQL   string
	CompanionSQL string
	// StrictAST computes structural identities; nil uses the package default.
	StrictAST physicalquery.StrictASTDigester

	// IdempotentReplayOf is the originally settled receipt, read from the
	// control store finalizer-side, when this request was an exact request-ID
	// replay. It is how the idempotent path is recognised at all.
	//
	// An idempotent replay returns the ORIGINAL receipt unchanged, so the
	// binding it carries describes the original execution and names that
	// original path kind -- QueryExecutionBindingV1.Validate rejects a binding
	// that claims idempotent_replay outright, precisely because no new binding
	// is produced. Reading the path kind off the returned receipt would
	// therefore misclassify every idempotent replay as the execution it is
	// replaying, and expect a visible and a companion statement from a request
	// that never reached Business PostgreSQL.
	//
	// So the replay is established from the store rather than from the
	// document, and the returned receipt is required to equal the stored one
	// exactly.
	IdempotentReplayOf *queryreceipt.QueryReceiptV1
	// NewExecutionBindingRowWritten is whether the settlement transaction for
	// THIS request wrote an execution binding row, observed in the control
	// store. An idempotent replay must not have written one.
	NewExecutionBindingRowWritten bool
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

// FinalizeTaskGateObservationV3 is the production acceptance entry point, and
// the only one. Every TaskGate call site reaches acceptance through it.
//
// The order is the point. The receipt is verified first, the path kind and the
// signed target records are read from it, the Adapter's carried evidence is
// compared against those signed records field by field, and only then is
// FinalizeObservationV3 asked to derive everything else independently. At no
// point does an Adapter-supplied value feed a derivation, and the Adapter's own
// verdict is never read at all -- there is no parameter through which it could
// be passed.
func FinalizeTaskGateObservationV3(receipt queryreceipt.QueryReceiptV1, verifier ReceiptVerifierV3,
	carried CarriedEvidenceV3, trusted TrustedInputsV3) (FinalizationV3, error) {
	var result FinalizationV3

	// 1. The receipt, before anything is read out of it.
	if verifier == nil {
		return result, errors.New("finalization requires a receipt verifier; an unverified receipt is not evidence")
	}
	if receipt.Version != queryreceipt.VersionV9 {
		return result, fmt.Errorf("receipt is V%s; v3 finalization requires a signed V9 execution binding", receipt.Version)
	}
	if err := receipt.Validate(); err != nil {
		return result, fmt.Errorf("V9 receipt does not validate: %w", err)
	}
	if err := verifier.Verify(receipt); err != nil {
		return result, fmt.Errorf("verify V9 receipt: %w", err)
	}
	binding := receipt.ExecutionBinding
	if binding == nil || receipt.ExposureLedgerBefore == nil {
		return result, errors.New("V9 receipt carries no execution binding or signed exposure pre-state")
	}
	if err := binding.Validate(); err != nil {
		return result, fmt.Errorf("signed execution binding does not validate: %w", err)
	}

	// 2. The path kind. For an idempotent replay it comes from the control
	// store, because the returned receipt is the original one and describes the
	// original execution; for every other path it comes from the Gateway's
	// signature. Neither source is the Adapter.
	var pathKind GatewayPathKind
	if trusted.IdempotentReplayOf != nil {
		if err := requireIdempotentReplay(receipt, trusted); err != nil {
			return result, err
		}
		pathKind = PathIdempotentReplay
	} else {
		if trusted.NewExecutionBindingRowWritten != true {
			return result, errors.New("a non-replay settlement wrote no execution binding row")
		}
		derived, err := pathKindForBinding(binding.PathKind)
		if err != nil {
			return result, err
		}
		pathKind = derived
		// 3. The signed target records must match the path the Gateway signed,
		// and the Adapter's carried statement identities must match those
		// records. An idempotent replay settles no target of its own, so there
		// is nothing here to check for it.
		if err := requireSignedTargets(pathKind, *binding, carried); err != nil {
			return result, err
		}
	}

	// 4. Everything else is derived independently.
	inputs := IndependentInputsV3{
		CatalogPath: trusted.CatalogPath, Footprint: trusted.Footprint,
		PostgreSQL: trusted.PostgreSQL, PathKind: pathKind,
		OperationID: trusted.OperationID, ContractIdentity: trusted.ContractIdentity,
		VisibleSQL: trusted.VisibleSQL, CompanionSQL: trusted.CompanionSQL,
		StrictAST: trusted.StrictAST,
	}
	return FinalizeObservationV3(carried, inputs)
}

// requireIdempotentReplay checks the two properties that make a replay idempotent:
// the receipt came back unchanged, and the settlement created nothing.
//
// "Unchanged" is compared over the canonical encoding of both documents rather
// than field by field. A field-by-field comparison has to be extended whenever
// the receipt grows a member, and the one time it is forgotten is the one time a
// replay could differ from its original without saying so.
func requireIdempotentReplay(receipt queryreceipt.QueryReceiptV1, trusted TrustedInputsV3) error {
	if trusted.NewExecutionBindingRowWritten {
		return errors.New("an idempotent replay wrote a new execution binding row; " +
			"the original receipt is returned unchanged and nothing is settled")
	}
	returned, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode the replayed receipt: %w", err)
	}
	original, err := json.Marshal(*trusted.IdempotentReplayOf)
	if err != nil {
		return fmt.Errorf("encode the originally settled receipt: %w", err)
	}
	if !bytes.Equal(returned, original) {
		return errors.New("the replayed receipt is not the originally settled receipt byte for byte")
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
func requireSignedTargets(pathKind GatewayPathKind, binding querybinding.QueryExecutionBindingV1,
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
		if err := requireCarriedMatchesSigned("visible", binding.Visible, carried.VisibleStatement); err != nil {
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
			return fmt.Errorf("path_kind %s settles a companion statement but none was carried", pathKind)
		}
		if err := requireCarriedMatchesSigned("companion", *binding.Companion, *carried.CompanionStatement); err != nil {
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
func requireCarriedMatchesSigned(role string, signed querybinding.TargetRecordV1,
	statement physicalquery.StatementIdentity) error {
	if signed.ExactSQLSHA256 != statement.ExactSHA256 {
		return fmt.Errorf("the carried %s statement is %s, the Gateway signed %s; the executed bytes differ",
			role, shortDigest(statement.ExactSHA256), shortDigest(signed.ExactSQLSHA256))
	}
	if signed.StrictASTSHA256 != statement.StrictASTSHA256 {
		return fmt.Errorf("the carried %s statement has structural identity %s, the Gateway signed %s",
			role, shortDigest(statement.StrictASTSHA256), shortDigest(signed.StrictASTSHA256))
	}
	if signed.RowLimit != statement.RowLimit {
		return fmt.Errorf("the carried %s statement was rendered with row limit %d, the Gateway signed %d",
			role, statement.RowLimit, signed.RowLimit)
	}
	if signed.PolicyFingerprint != statement.Fingerprint {
		return fmt.Errorf("the carried %s statement carries policy fingerprint %s, the Gateway signed %s",
			role, shortDigest(statement.Fingerprint), shortDigest(signed.PolicyFingerprint))
	}
	return nil
}
