package experiment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"taskbound.local/agent-data-gateway/internal/preparedbinding"
)

// TaskGateRejectionV1Version identifies the credential-free, closed rejection
// record emitted only when FinalizeTaskGateObservationV3 is reached and refuses
// an operation. The operator-facing error and its chain never enter this type.
const TaskGateRejectionV1Version = "taskgate-rejection-v1"

const (
	// pg_stat_statements.max is pinned to 10,000 in the measured deployment.
	// Three typed differences retain every structural key, plus the total and a
	// fail-closed omitted count if a future environment exceeds that boundary.
	maxTaskGateRejectionUnexpectedRows = 10_000
	maxTaskGateRejectionDifferences    = 3*maxTaskGateRejectionUnexpectedRows + 3
	maxTaskGateRejectionCount          = int64(1<<63 - 1)
)

type rejectionPhase uint8

const (
	rejectionPhaseInvalid rejectionPhase = iota
	rejectionPhaseReceiptAuthentication
	rejectionPhaseObserverTicket
	rejectionPhaseTrustedMaterial
	rejectionPhaseContractIdentification
	rejectionPhaseExecutionReproduction
	rejectionPhaseSignedConsistency
	rejectionPhaseSchemaQualification
	rejectionPhaseClassifierDerivation
	rejectionPhaseCarriedConsistency
	rejectionPhaseObserverInterval
	rejectionPhaseClosedWorldAccounting
	rejectionPhaseRuntimeIdentity
	rejectionPhaseCount
)

var rejectionPhaseNames = [...]string{
	"",
	"receipt_authentication",
	"observer_ticket",
	"trusted_material",
	"contract_identification",
	"execution_reproduction",
	"signed_consistency",
	"schema_qualification",
	"classifier_derivation",
	"carried_consistency",
	"observer_interval",
	"closed_world_accounting",
	"runtime_identity",
}

type rejectionGate uint8

const (
	rejectionGateInvalid rejectionGate = iota
	rejectionGateFinalizerInstance
	rejectionGateReceiptCurrentVersion
	rejectionGateReceiptDocument
	rejectionGateReceiptSignature
	rejectionGateReceiptDocumentIdentity
	rejectionGateExecutionBinding
	rejectionGateExposureAccounting
	rejectionGateObserverTicket
	rejectionGateControlRequestState
	rejectionGateDeploymentPostgreSQLIdentity
	rejectionGateReplayReturnedReceipt
	rejectionGateProfileMaterial
	rejectionGateQualifiedFootprintResolve
	rejectionGateCandidateResolution
	rejectionGateCandidateSelectorCount
	rejectionGateCandidateMatchCount
	rejectionGateReplayEvidence
	rejectionGateSettlementExecutionBinding
	rejectionGateSignedTargets
	rejectionGateCarriedTargets
	rejectionGateSignedReproducedTargets
	rejectionGateMaterialPresence
	rejectionGateFrozenMaterial
	rejectionGateCompilerIdentity
	rejectionGateCatalogMaterial
	rejectionGateOperationPreparation
	rejectionGatePreparedBindingMembers
	rejectionGatePreparedStatements
	rejectionGateSignedPrestate
	rejectionGateStatementAuthorization
	rejectionGateDerivedLimits
	rejectionGateCompanionPresence
	rejectionGateExpectedSchema
	rejectionGateFootprintQualification
	rejectionGateControlPlan
	rejectionGateClassifierTargets
	rejectionGateOperationIdentity
	rejectionGateClassifierManifest
	rejectionGateClassifierBinding
	rejectionGateCarriedOperation
	rejectionGateCarriedPlan
	rejectionGateCarriedClassifierManifest
	rejectionGateCarriedClassifierBinding
	rejectionGateStatementIdentity
	rejectionGateObserverSnapshotInterval
	rejectionGateClassifierCommitment
	rejectionGateCensusMonotonicity
	rejectionGateUnexpectedStructuralStatements
	rejectionGateClosedWorldClasses
	rejectionGateInternalStatementMultiset
	rejectionGateObserverTotal
	rejectionGateIdempotentBusinessTotal
	rejectionGatePostgreSQLQualificationRuntime
	rejectionGateFinalizerInternal
	rejectionGateCount
)

var rejectionGateNames = [...]string{
	"",
	"finalizer_instance",
	"receipt_current_version",
	"receipt_document",
	"receipt_signature",
	"receipt_document_identity",
	"execution_binding",
	"exposure_accounting",
	"observer_ticket",
	"control_request_state",
	"deployment_postgresql_identity",
	"replay_returned_receipt",
	"profile_material",
	"qualified_footprint_resolve",
	"candidate_resolution",
	"candidate_selector_count",
	"candidate_match_count",
	"replay_evidence",
	"settlement_execution_binding",
	"signed_targets",
	"carried_targets",
	"signed_reproduced_targets",
	"material_presence",
	"frozen_material",
	"compiler_identity",
	"catalog_material",
	"operation_preparation",
	"prepared_binding_members",
	"prepared_statements",
	"signed_prestate",
	"statement_authorization",
	"derived_limits",
	"companion_presence",
	"expected_schema",
	"footprint_qualification",
	"control_plan",
	"classifier_targets",
	"operation_identity",
	"classifier_manifest",
	"classifier_binding",
	"carried_operation",
	"carried_plan",
	"carried_classifier_manifest",
	"carried_classifier_binding",
	"statement_identity",
	"observer_snapshot_interval",
	"classifier_commitment",
	"census_monotonicity",
	"unexpected_structural_statements",
	"closed_world_classes",
	"internal_statement_multiset",
	"observer_total",
	"idempotent_business_total",
	"postgresql_qualification_runtime",
	"finalizer_internal",
}

type rejectionFailureKind uint8

const (
	rejectionFailureInvalid rejectionFailureKind = iota
	rejectionFailureMissing
	rejectionFailureInvalidValue
	rejectionFailureMismatch
	rejectionFailureAmbiguous
	rejectionFailureUnavailable
	rejectionFailureKindCount
)

var rejectionFailureKindNames = [...]string{
	"", "missing", "invalid", "mismatch", "ambiguous", "unavailable",
}

type rejectionSource uint8

const (
	rejectionSourceInvalid rejectionSource = iota
	rejectionSourceFinalizerVerifier
	rejectionSourceGatewayReceipt
	rejectionSourceObserverTicket
	rejectionSourceControlStore
	rejectionSourceDeploymentRuntime
	rejectionSourceFrozenContract
	rejectionSourceActivatedProfile
	rejectionSourceRetainedQualification
	rejectionSourceFinalizerDerivation
	rejectionSourceCarriedEvidence
	rejectionSourceObserverWindow
	rejectionSourceClassifierPlan
	rejectionSourceCount
)

var rejectionSourceNames = [...]string{
	"",
	"finalizer_verifier",
	"gateway_receipt",
	"observer_ticket",
	"control_store",
	"deployment_runtime",
	"frozen_contract",
	"activated_profile",
	"retained_qualification",
	"finalizer_derivation",
	"carried_evidence",
	"observer_window",
	"classifier_plan",
}

type rejectionPathKind uint8

const (
	rejectionPathInvalid rejectionPathKind = iota
	rejectionPathPairedNovel
	rejectionPathSingleQuery
	rejectionPathSemanticReplay
	rejectionPathIdempotentReplay
	rejectionPathCount
)

var rejectionPathNames = [...]string{
	"", "paired_novel", "single_query", "semantic_replay", "idempotent_replay",
}

type rejectionTargetRole uint8

const (
	rejectionTargetRoleInvalid rejectionTargetRole = iota
	rejectionTargetRoleVisible
	rejectionTargetRoleCompanion
	rejectionTargetRoleCount
)

var rejectionTargetRoleNames = [...]string{"", "visible", "companion"}

type rejectionStatementClass uint8

const (
	rejectionStatementClassInvalid rejectionStatementClass = iota
	rejectionStatementTransactionBegin
	rejectionStatementTransactionCommit
	rejectionStatementSafetySessionPin
	rejectionStatementRepresentationPin
	rejectionStatementTimeoutPin
	rejectionStatementDatasourceIdentity
	rejectionStatementViewColumnAttestation
	rejectionStatementViewDefinitionAttestation
	rejectionStatementPostgreSQLInternalAttestation
	rejectionStatementTargetedVisible
	rejectionStatementTargetedCompanion
	rejectionStatementUnexpected
	rejectionStatementClassCount
)

var rejectionStatementClassNames = [...]string{
	"",
	"transaction_begin",
	"transaction_commit",
	"safety_session_pin",
	"representation_pin",
	"statement_timeout_pin",
	"datasource_identity",
	"view_column_attestation",
	"view_definition_attestation",
	"postgresql_internal_attestation",
	"targeted_visible",
	"targeted_companion",
	"unexpected",
}

type rejectionDifferenceKind uint8

const (
	rejectionDifferenceInvalid rejectionDifferenceKind = iota
	rejectionDifferenceBool
	rejectionDifferenceCount
	rejectionDifferenceSHA256
	rejectionDifferenceEnum
	rejectionDifferenceKindCount
)

var rejectionDifferenceKindNames = [...]string{
	"", "bool", "count", "lowercase_sha256", "enum",
}

type rejectionDifferenceField uint8

const (
	rejectionDifferenceFieldInvalid rejectionDifferenceField = iota
	rejectionDifferenceExpectedCount
	rejectionDifferenceActualCount
	rejectionDifferenceCandidateCount
	rejectionDifferenceRefusedCandidateCount
	rejectionDifferenceMatchedCandidateCount
	rejectionDifferencePreparedMember
	rejectionDifferenceUnexpectedStatementSHA256
	rejectionDifferenceUnexpectedStatementTopLevel
	rejectionDifferenceUnexpectedStatementCalls
	rejectionDifferenceOmittedStatementCount
	rejectionDifferenceExpectedSHA256
	rejectionDifferenceActualSHA256
	rejectionDifferenceExpectedBool
	rejectionDifferenceActualBool
	rejectionDifferenceFieldCount
)

var rejectionDifferenceFieldNames = [...]string{
	"",
	"expected_count",
	"actual_count",
	"candidate_count",
	"refused_candidate_count",
	"matched_candidate_count",
	"prepared_member",
	"unexpected_statement_sha256",
	"unexpected_statement_toplevel",
	"unexpected_statement_calls",
	"omitted_statement_count",
	"expected_sha256",
	"actual_sha256",
	"expected_bool",
	"actual_bool",
}

// Each stable wire-name table is compile-time locked to its numeric enum. The
// tests additionally require every nonzero slot to be unique and nonempty;
// together those checks prevent a new enum from silently becoming arbitrary or
// unserializable evidence.
var _ [int(rejectionPhaseCount) - len(rejectionPhaseNames)]struct{}
var _ [len(rejectionPhaseNames) - int(rejectionPhaseCount)]struct{}
var _ [int(rejectionGateCount) - len(rejectionGateNames)]struct{}
var _ [len(rejectionGateNames) - int(rejectionGateCount)]struct{}
var _ [int(rejectionFailureKindCount) - len(rejectionFailureKindNames)]struct{}
var _ [len(rejectionFailureKindNames) - int(rejectionFailureKindCount)]struct{}
var _ [int(rejectionSourceCount) - len(rejectionSourceNames)]struct{}
var _ [len(rejectionSourceNames) - int(rejectionSourceCount)]struct{}
var _ [int(rejectionPathCount) - len(rejectionPathNames)]struct{}
var _ [len(rejectionPathNames) - int(rejectionPathCount)]struct{}
var _ [int(rejectionTargetRoleCount) - len(rejectionTargetRoleNames)]struct{}
var _ [len(rejectionTargetRoleNames) - int(rejectionTargetRoleCount)]struct{}
var _ [int(rejectionStatementClassCount) - len(rejectionStatementClassNames)]struct{}
var _ [len(rejectionStatementClassNames) - int(rejectionStatementClassCount)]struct{}
var _ [int(rejectionDifferenceKindCount) - len(rejectionDifferenceKindNames)]struct{}
var _ [len(rejectionDifferenceKindNames) - int(rejectionDifferenceKindCount)]struct{}
var _ [int(rejectionDifferenceFieldCount) - len(rejectionDifferenceFieldNames)]struct{}
var _ [len(rejectionDifferenceFieldNames) - int(rejectionDifferenceFieldCount)]struct{}

// rejectionSHA256 is the only representation of a digest inside a rejection.
// A free-form string cannot be stored in the durable record.
type rejectionSHA256 [32]byte

func parseRejectionSHA256(value string) (rejectionSHA256, error) {
	var result rejectionSHA256
	if len(value) != 64 {
		return result, errors.New("rejection SHA-256 is not 64 lowercase hex characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(decoded) != value {
		return result, errors.New("rejection SHA-256 is not lowercase hexadecimal")
	}
	copy(result[:], decoded)
	return result, nil
}

func (value rejectionSHA256) String() string { return hex.EncodeToString(value[:]) }

// rejectionDifferenceV1 is a sealed tagged union. enumValue is used only for
// the typed prepared-binding member enumeration.
type rejectionDifferenceV1 struct {
	field          rejectionDifferenceField
	ordinal        int64
	hasOrdinal     bool
	kind           rejectionDifferenceKind
	boolValue      bool
	countValue     int64
	sha256Value    rejectionSHA256
	preparedMember preparedbinding.PreparedOperationMember
}

// TaskGateRejectionV1 is opaque by construction. Its stored members are closed
// numeric enums, booleans, bounded counts, typed SHA-256 values and a bounded
// list of the same. It deliberately has no exported field, string, map, raw
// JSON, SQL, path, DSN, task/request/operation identity, or error.
type TaskGateRejectionV1 struct {
	phase          rejectionPhase
	gate           rejectionGate
	failure        rejectionFailureKind
	expectedSource rejectionSource
	actualSource   rejectionSource
	pathKind       rejectionPathKind
	targetRole     rejectionTargetRole
	statementClass rejectionStatementClass
	differences    []rejectionDifferenceV1
}

type taskGateRejectionWireV1 struct {
	Version        string                              `json:"version"`
	Phase          string                              `json:"phase"`
	GateCode       string                              `json:"gate_code"`
	FailureKind    string                              `json:"failure_kind"`
	ExpectedSource string                              `json:"expected_source"`
	ActualSource   string                              `json:"actual_source"`
	PathKind       *string                             `json:"path_kind,omitempty"`
	TargetRole     *string                             `json:"target_role,omitempty"`
	StatementClass *string                             `json:"statement_class,omitempty"`
	Differences    []taskGateRejectionDifferenceWireV1 `json:"differences,omitempty"`
}

type taskGateRejectionDifferenceWireV1 struct {
	Field           string  `json:"field"`
	Ordinal         *int64  `json:"ordinal,omitempty"`
	Bool            *bool   `json:"bool,omitempty"`
	Count           *int64  `json:"count,omitempty"`
	LowercaseSHA256 *string `json:"lowercase_sha256,omitempty"`
	Enum            *string `json:"enum,omitempty"`
}

func (rejection TaskGateRejectionV1) MarshalJSON() ([]byte, error) {
	if err := rejection.Validate(); err != nil {
		return nil, err
	}
	wire := taskGateRejectionWireV1{
		Version:        TaskGateRejectionV1Version,
		Phase:          enumName(rejectionPhaseNames[:], int(rejection.phase)),
		GateCode:       enumName(rejectionGateNames[:], int(rejection.gate)),
		FailureKind:    enumName(rejectionFailureKindNames[:], int(rejection.failure)),
		ExpectedSource: enumName(rejectionSourceNames[:], int(rejection.expectedSource)),
		ActualSource:   enumName(rejectionSourceNames[:], int(rejection.actualSource)),
	}
	if name := enumName(rejectionPathNames[:], int(rejection.pathKind)); name != "" {
		wire.PathKind = &name
	}
	if name := enumName(rejectionTargetRoleNames[:], int(rejection.targetRole)); name != "" {
		wire.TargetRole = &name
	}
	if name := enumName(rejectionStatementClassNames[:], int(rejection.statementClass)); name != "" {
		wire.StatementClass = &name
	}
	for _, difference := range rejection.differences {
		encoded := taskGateRejectionDifferenceWireV1{
			Field: enumName(rejectionDifferenceFieldNames[:], int(difference.field)),
		}
		if difference.hasOrdinal {
			value := difference.ordinal
			encoded.Ordinal = &value
		}
		switch difference.kind {
		case rejectionDifferenceBool:
			value := difference.boolValue
			encoded.Bool = &value
		case rejectionDifferenceCount:
			value := difference.countValue
			encoded.Count = &value
		case rejectionDifferenceSHA256:
			value := difference.sha256Value.String()
			encoded.LowercaseSHA256 = &value
		case rejectionDifferenceEnum:
			value := difference.preparedMember.Code()
			encoded.Enum = &value
		}
		wire.Differences = append(wire.Differences, encoded)
	}
	return json.Marshal(wire)
}

func (rejection *TaskGateRejectionV1) UnmarshalJSON(value []byte) error {
	if rejection == nil {
		return errors.New("cannot decode a rejection into nil")
	}
	if err := rejectTaskGateRejectionNulls(value); err != nil {
		return err
	}
	var wire taskGateRejectionWireV1
	if err := decodeClosedJSON(value, &wire); err != nil {
		return err
	}
	if wire.Version != TaskGateRejectionV1Version {
		return fmt.Errorf("unsupported TaskGate rejection version %q", wire.Version)
	}
	var decoded TaskGateRejectionV1
	var err error
	if decoded.phase, err = parseRejectionPhase(wire.Phase); err != nil {
		return err
	}
	if decoded.gate, err = parseRejectionGate(wire.GateCode); err != nil {
		return err
	}
	if decoded.failure, err = parseRejectionFailureKind(wire.FailureKind); err != nil {
		return err
	}
	if decoded.expectedSource, err = parseRejectionSource(wire.ExpectedSource); err != nil {
		return err
	}
	if decoded.actualSource, err = parseRejectionSource(wire.ActualSource); err != nil {
		return err
	}
	if wire.PathKind != nil {
		if decoded.pathKind, err = parseRejectionPathKind(*wire.PathKind); err != nil {
			return err
		}
	}
	if wire.TargetRole != nil {
		if decoded.targetRole, err = parseRejectionTargetRole(*wire.TargetRole); err != nil {
			return err
		}
	}
	if wire.StatementClass != nil {
		if decoded.statementClass, err = parseRejectionStatementClass(*wire.StatementClass); err != nil {
			return err
		}
	}
	for _, encoded := range wire.Differences {
		difference, decodeErr := decodeRejectionDifference(encoded)
		if decodeErr != nil {
			return decodeErr
		}
		decoded.differences = append(decoded.differences, difference)
	}
	if err := decoded.Validate(); err != nil {
		return err
	}
	*rejection = decoded
	return nil
}

// A rejection wire has no nullable member. Rejecting null before decoding is
// necessary because encoding/json maps null into nil pointers and slices,
// which would otherwise turn a present union arm into an omitted one.
func rejectTaskGateRejectionNulls(value []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if token == nil {
			return errors.New("TaskGate rejection JSON cannot contain null")
		}
	}
}

func decodeRejectionDifference(wire taskGateRejectionDifferenceWireV1) (rejectionDifferenceV1, error) {
	var result rejectionDifferenceV1
	field, err := parseRejectionDifferenceField(wire.Field)
	if err != nil {
		return result, err
	}
	result.field = field
	if wire.Ordinal != nil {
		result.hasOrdinal, result.ordinal = true, *wire.Ordinal
	}
	values := 0
	if wire.Bool != nil {
		values++
		result.kind, result.boolValue = rejectionDifferenceBool, *wire.Bool
	}
	if wire.Count != nil {
		values++
		result.kind, result.countValue = rejectionDifferenceCount, *wire.Count
	}
	if wire.LowercaseSHA256 != nil {
		values++
		result.kind = rejectionDifferenceSHA256
		result.sha256Value, err = parseRejectionSHA256(*wire.LowercaseSHA256)
		if err != nil {
			return rejectionDifferenceV1{}, err
		}
	}
	if wire.Enum != nil {
		values++
		result.kind = rejectionDifferenceEnum
		result.preparedMember, err = parsePreparedOperationMember(*wire.Enum)
		if err != nil {
			return rejectionDifferenceV1{}, err
		}
	}
	if values != 1 {
		return rejectionDifferenceV1{}, errors.New("a rejection difference must carry exactly one typed value")
	}
	return result, nil
}

func (rejection TaskGateRejectionV1) Validate() error {
	if enumName(rejectionPhaseNames[:], int(rejection.phase)) == "" ||
		enumName(rejectionGateNames[:], int(rejection.gate)) == "" ||
		enumName(rejectionFailureKindNames[:], int(rejection.failure)) == "" ||
		enumName(rejectionSourceNames[:], int(rejection.expectedSource)) == "" ||
		enumName(rejectionSourceNames[:], int(rejection.actualSource)) == "" {
		return errors.New("TaskGate rejection omits a closed required enum")
	}
	if rejection.phase != phaseForRejectionGate(rejection.gate) {
		return errors.New("TaskGate rejection phase does not own its gate_code")
	}
	if rejection.pathKind != rejectionPathInvalid && enumName(rejectionPathNames[:], int(rejection.pathKind)) == "" {
		return errors.New("TaskGate rejection path_kind is not closed")
	}
	if rejection.targetRole != rejectionTargetRoleInvalid && enumName(rejectionTargetRoleNames[:], int(rejection.targetRole)) == "" {
		return errors.New("TaskGate rejection target_role is not closed")
	}
	if rejection.statementClass != rejectionStatementClassInvalid &&
		enumName(rejectionStatementClassNames[:], int(rejection.statementClass)) == "" {
		return errors.New("TaskGate rejection statement_class is not closed")
	}
	if len(rejection.differences) > maxTaskGateRejectionDifferences {
		return errors.New("TaskGate rejection exceeds its bounded difference list")
	}
	for index, difference := range rejection.differences {
		if err := difference.validate(); err != nil {
			return fmt.Errorf("TaskGate rejection difference %d: %w", index, err)
		}
		if index > 0 && !rejectionDifferenceLess(rejection.differences[index-1], difference) {
			return errors.New("TaskGate rejection differences are not unique and canonically sorted")
		}
		if index > 0 && rejectionDifferenceLogicalKeyEqual(rejection.differences[index-1], difference) &&
			difference.field != rejectionDifferencePreparedMember {
			return errors.New("TaskGate rejection repeats a logical difference field")
		}
	}
	if err := rejection.validateRequiredDifferenceShape(); err != nil {
		return err
	}
	return nil
}

func rejectionDifferenceLogicalKeyEqual(left, right rejectionDifferenceV1) bool {
	return left.field == right.field && left.hasOrdinal == right.hasOrdinal &&
		(!left.hasOrdinal || left.ordinal == right.ordinal)
}

// validateRequiredDifferenceShape makes the three rejection families whose
// detail is part of the evidence claim self-describing on the wire. Production
// constructors cannot rely on a comment to retain every prepared member, the
// complete zero-match census, or every unexpected structural key triple.
func (rejection TaskGateRejectionV1) validateRequiredDifferenceShape() error {
	switch rejection.gate {
	case rejectionGatePreparedBindingMembers:
		if len(rejection.differences) == 0 {
			return errors.New("prepared-binding rejection omits its differing members")
		}
		for _, difference := range rejection.differences {
			if difference.field != rejectionDifferencePreparedMember {
				return errors.New("prepared-binding rejection carries a non-member difference")
			}
		}
	case rejectionGateCandidateMatchCount:
		return rejection.validateCandidateMatchDifferences()
	case rejectionGateUnexpectedStructuralStatements:
		return rejection.validateUnexpectedDifferences()
	}
	return nil
}

func (rejection TaskGateRejectionV1) validateCandidateMatchDifferences() error {
	candidate, hasCandidate := rejection.countDifference(rejectionDifferenceCandidateCount)
	matched, hasMatched := rejection.countDifference(rejectionDifferenceMatchedCandidateCount)
	refused, hasRefused := rejection.countDifference(rejectionDifferenceRefusedCandidateCount)
	if !hasCandidate || !hasMatched {
		return errors.New("candidate-match rejection omits its candidate or matched count")
	}
	for _, difference := range rejection.differences {
		switch difference.field {
		case rejectionDifferenceCandidateCount, rejectionDifferenceMatchedCandidateCount,
			rejectionDifferenceRefusedCandidateCount, rejectionDifferencePreparedMember:
		default:
			return errors.New("candidate-match rejection carries an unrelated difference")
		}
	}
	switch rejection.failure {
	case rejectionFailureMismatch:
		if !hasRefused || candidate < 1 || refused != candidate || matched != 0 {
			return errors.New("zero-match rejection does not carry a complete candidate census")
		}
	case rejectionFailureAmbiguous:
		if hasRefused || candidate < 2 || matched < 2 || matched > candidate || len(rejection.differences) != 2 {
			return errors.New("ambiguous candidate rejection has an invalid candidate census")
		}
	default:
		return errors.New("candidate-match rejection has an invalid failure kind")
	}
	return nil
}

func (rejection TaskGateRejectionV1) validateUnexpectedDifferences() error {
	if rejection.failure != rejectionFailureMismatch {
		return errors.New("unexpected-structural rejection must be a mismatch")
	}
	expected, hasExpected := rejection.countDifference(rejectionDifferenceExpectedCount)
	actual, hasActual := rejection.countDifference(rejectionDifferenceActualCount)
	omitted, hasOmitted := rejection.countDifference(rejectionDifferenceOmittedStatementCount)
	if !hasExpected || expected != 0 || !hasActual || actual < 1 {
		return errors.New("unexpected-structural rejection omits its zero/nonzero census")
	}
	retained := actual
	if retained > maxTaskGateRejectionUnexpectedRows {
		retained = maxTaskGateRejectionUnexpectedRows
	}
	seenSHA := make([]bool, int(retained))
	seenTopLevel := make([]bool, int(retained))
	seenCalls := make([]bool, int(retained))
	for _, difference := range rejection.differences {
		switch difference.field {
		case rejectionDifferenceExpectedCount, rejectionDifferenceActualCount,
			rejectionDifferenceOmittedStatementCount:
			continue
		case rejectionDifferenceUnexpectedStatementSHA256,
			rejectionDifferenceUnexpectedStatementTopLevel,
			rejectionDifferenceUnexpectedStatementCalls:
			if difference.ordinal < 0 || difference.ordinal >= retained {
				return errors.New("unexpected-structural rejection has an out-of-census ordinal")
			}
			index := int(difference.ordinal)
			switch difference.field {
			case rejectionDifferenceUnexpectedStatementSHA256:
				seenSHA[index] = true
			case rejectionDifferenceUnexpectedStatementTopLevel:
				seenTopLevel[index] = true
			case rejectionDifferenceUnexpectedStatementCalls:
				seenCalls[index] = true
			}
		default:
			return errors.New("unexpected-structural rejection carries an unrelated difference")
		}
	}
	for index := range seenSHA {
		if !seenSHA[index] || !seenTopLevel[index] || !seenCalls[index] {
			return errors.New("unexpected-structural rejection omits a structural key member")
		}
	}
	if actual > maxTaskGateRejectionUnexpectedRows {
		if !hasOmitted || omitted != actual-maxTaskGateRejectionUnexpectedRows {
			return errors.New("unexpected-structural rejection has an invalid omitted count")
		}
	} else if hasOmitted {
		return errors.New("unexpected-structural rejection carries a spurious omitted count")
	}
	return nil
}

func (rejection TaskGateRejectionV1) countDifference(field rejectionDifferenceField) (int64, bool) {
	for _, difference := range rejection.differences {
		if difference.field == field {
			return difference.countValue, true
		}
	}
	return 0, false
}

func (difference rejectionDifferenceV1) validate() error {
	if enumName(rejectionDifferenceFieldNames[:], int(difference.field)) == "" ||
		enumName(rejectionDifferenceKindNames[:], int(difference.kind)) == "" {
		return errors.New("unknown difference field or kind")
	}
	if difference.hasOrdinal &&
		(difference.ordinal < 0 || difference.ordinal >= maxTaskGateRejectionUnexpectedRows) {
		return errors.New("difference ordinal is outside the bounded count domain")
	}
	unexpected := difference.field == rejectionDifferenceUnexpectedStatementSHA256 ||
		difference.field == rejectionDifferenceUnexpectedStatementTopLevel ||
		difference.field == rejectionDifferenceUnexpectedStatementCalls
	if unexpected != difference.hasOrdinal {
		return errors.New("only unexpected-statement differences carry an ordinal")
	}
	switch difference.field {
	case rejectionDifferencePreparedMember:
		if difference.kind != rejectionDifferenceEnum || difference.preparedMember.Code() == "" {
			return errors.New("prepared_member requires a closed prepared-member enum")
		}
	case rejectionDifferenceUnexpectedStatementSHA256,
		rejectionDifferenceExpectedSHA256, rejectionDifferenceActualSHA256:
		if difference.kind != rejectionDifferenceSHA256 {
			return errors.New("SHA-256 difference field has the wrong tagged value")
		}
	case rejectionDifferenceUnexpectedStatementTopLevel,
		rejectionDifferenceExpectedBool, rejectionDifferenceActualBool:
		if difference.kind != rejectionDifferenceBool {
			return errors.New("boolean difference field has the wrong tagged value")
		}
	default:
		if difference.kind != rejectionDifferenceCount || difference.countValue < 0 ||
			difference.countValue > maxTaskGateRejectionCount {
			return errors.New("count difference is outside the bounded count domain")
		}
	}
	return nil
}

func phaseForRejectionGate(gate rejectionGate) rejectionPhase {
	switch gate {
	case rejectionGateFinalizerInstance, rejectionGateReceiptCurrentVersion,
		rejectionGateReceiptDocument, rejectionGateReceiptSignature,
		rejectionGateReceiptDocumentIdentity, rejectionGateExecutionBinding,
		rejectionGateExposureAccounting:
		return rejectionPhaseReceiptAuthentication
	case rejectionGateObserverTicket:
		return rejectionPhaseObserverTicket
	case rejectionGateControlRequestState, rejectionGateDeploymentPostgreSQLIdentity,
		rejectionGateReplayReturnedReceipt, rejectionGateProfileMaterial,
		rejectionGateQualifiedFootprintResolve:
		return rejectionPhaseTrustedMaterial
	case rejectionGateCandidateResolution, rejectionGateCandidateSelectorCount,
		rejectionGateCandidateMatchCount:
		return rejectionPhaseContractIdentification
	case rejectionGateMaterialPresence, rejectionGateFrozenMaterial,
		rejectionGateCompilerIdentity, rejectionGateCatalogMaterial,
		rejectionGateOperationPreparation, rejectionGatePreparedBindingMembers,
		rejectionGatePreparedStatements, rejectionGateSignedPrestate,
		rejectionGateStatementAuthorization, rejectionGateDerivedLimits,
		rejectionGateCompanionPresence:
		return rejectionPhaseExecutionReproduction
	case rejectionGateReplayEvidence, rejectionGateSettlementExecutionBinding,
		rejectionGateSignedTargets, rejectionGateCarriedTargets,
		rejectionGateSignedReproducedTargets:
		return rejectionPhaseSignedConsistency
	case rejectionGateExpectedSchema, rejectionGateFootprintQualification:
		return rejectionPhaseSchemaQualification
	case rejectionGateControlPlan, rejectionGateClassifierTargets,
		rejectionGateOperationIdentity, rejectionGateClassifierManifest,
		rejectionGateClassifierBinding:
		return rejectionPhaseClassifierDerivation
	case rejectionGateCarriedOperation, rejectionGateCarriedPlan,
		rejectionGateCarriedClassifierManifest, rejectionGateCarriedClassifierBinding,
		rejectionGateStatementIdentity:
		return rejectionPhaseCarriedConsistency
	case rejectionGateObserverSnapshotInterval, rejectionGateClassifierCommitment,
		rejectionGateCensusMonotonicity:
		return rejectionPhaseObserverInterval
	case rejectionGateUnexpectedStructuralStatements, rejectionGateClosedWorldClasses,
		rejectionGateInternalStatementMultiset, rejectionGateObserverTotal,
		rejectionGateIdempotentBusinessTotal:
		return rejectionPhaseClosedWorldAccounting
	case rejectionGatePostgreSQLQualificationRuntime, rejectionGateFinalizerInternal:
		// finalizer_internal is the fail-closed identity of the running
		// finalizer/taxonomy implementation itself. It never reclassifies a
		// known gate; rejectTaskGateAt preserves an existing typed marker.
		return rejectionPhaseRuntimeIdentity
	default:
		return rejectionPhaseInvalid
	}
}

func newTaskGateRejectionV1(gate rejectionGate, failure rejectionFailureKind,
	expected, actual rejectionSource, differences ...rejectionDifferenceV1) TaskGateRejectionV1 {
	result := TaskGateRejectionV1{
		phase: phaseForRejectionGate(gate), gate: gate, failure: failure,
		expectedSource: expected, actualSource: actual,
		differences: append([]rejectionDifferenceV1(nil), differences...),
	}
	sort.Slice(result.differences, func(left, right int) bool {
		return rejectionDifferenceLess(result.differences[left], result.differences[right])
	})
	return result
}

func rejectionCountDifference(field rejectionDifferenceField, value int64) rejectionDifferenceV1 {
	return rejectionDifferenceV1{field: field, kind: rejectionDifferenceCount, countValue: value}
}

func rejectionBoolDifference(field rejectionDifferenceField, value bool) rejectionDifferenceV1 {
	return rejectionDifferenceV1{field: field, kind: rejectionDifferenceBool, boolValue: value}
}

func rejectionSHA256Difference(field rejectionDifferenceField, value string) (rejectionDifferenceV1, error) {
	digest, err := parseRejectionSHA256(value)
	return rejectionDifferenceV1{field: field, kind: rejectionDifferenceSHA256, sha256Value: digest}, err
}

func rejectionSHA256Pair(expected, actual string) []rejectionDifferenceV1 {
	want, wantErr := rejectionSHA256Difference(rejectionDifferenceExpectedSHA256, expected)
	got, gotErr := rejectionSHA256Difference(rejectionDifferenceActualSHA256, actual)
	if wantErr != nil || gotErr != nil {
		return nil
	}
	return []rejectionDifferenceV1{want, got}
}

func rejectionPreparedMemberDifferences(members []preparedbinding.PreparedOperationMember) []rejectionDifferenceV1 {
	result := make([]rejectionDifferenceV1, 0, len(members))
	for _, member := range members {
		if member.Code() == "" {
			continue
		}
		result = append(result, rejectionDifferenceV1{
			field: rejectionDifferencePreparedMember, kind: rejectionDifferenceEnum,
			preparedMember: member,
		})
	}
	return result
}

func rejectionUnexpectedDifferences(rows []ObserverStructuralRow) []rejectionDifferenceV1 {
	maxRows := maxTaskGateRejectionUnexpectedRows
	retained := rows
	if len(retained) > maxRows {
		retained = retained[:maxRows]
	}
	result := []rejectionDifferenceV1{
		rejectionCountDifference(rejectionDifferenceExpectedCount, 0),
		rejectionCountDifference(rejectionDifferenceActualCount, int64(len(rows))),
	}
	for index, row := range retained {
		digest, err := rejectionSHA256Difference(rejectionDifferenceUnexpectedStatementSHA256,
			row.StrictASTSHA256)
		if err != nil || row.Calls < 0 || row.Calls > maxTaskGateRejectionCount {
			continue
		}
		ordinal := int64(index)
		digest.hasOrdinal, digest.ordinal = true, ordinal
		result = append(result, digest,
			rejectionDifferenceV1{field: rejectionDifferenceUnexpectedStatementTopLevel,
				hasOrdinal: true, ordinal: ordinal, kind: rejectionDifferenceBool, boolValue: row.TopLevel},
			rejectionDifferenceV1{field: rejectionDifferenceUnexpectedStatementCalls,
				hasOrdinal: true, ordinal: ordinal, kind: rejectionDifferenceCount, countValue: row.Calls})
	}
	if len(rows) > len(retained) {
		result = append(result, rejectionCountDifference(rejectionDifferenceOmittedStatementCount,
			int64(len(rows)-len(retained))))
	}
	return result
}

func rejectionDifferenceLess(left, right rejectionDifferenceV1) bool {
	leftField := enumName(rejectionDifferenceFieldNames[:], int(left.field))
	rightField := enumName(rejectionDifferenceFieldNames[:], int(right.field))
	if leftField != rightField {
		return leftField < rightField
	}
	if left.hasOrdinal != right.hasOrdinal {
		return !left.hasOrdinal
	}
	if left.ordinal != right.ordinal {
		return left.ordinal < right.ordinal
	}
	if left.kind != right.kind {
		return left.kind < right.kind
	}
	switch left.kind {
	case rejectionDifferenceBool:
		return !left.boolValue && right.boolValue
	case rejectionDifferenceCount:
		return left.countValue < right.countValue
	case rejectionDifferenceSHA256:
		return bytes.Compare(left.sha256Value[:], right.sha256Value[:]) < 0
	case rejectionDifferenceEnum:
		return left.preparedMember.Code() < right.preparedMember.Code()
	default:
		return false
	}
}

// taskGateRejectionError retains the operator-facing cause only in memory. Its
// safe rejection record is copied out explicitly and is the only part an
// Adapter may attach to a Sample.
type taskGateRejectionError struct {
	cause     error
	rejection TaskGateRejectionV1
}

func (rejected *taskGateRejectionError) Error() string {
	if rejected == nil || rejected.cause == nil {
		return "TaskGate finalization rejected"
	}
	return rejected.cause.Error()
}

func (rejected *taskGateRejectionError) Unwrap() error {
	if rejected == nil {
		return nil
	}
	return rejected.cause
}

func withTaskGateRejection(err error, rejection TaskGateRejectionV1) error {
	if err == nil {
		return nil
	}
	var existing *taskGateRejectionError
	if errors.As(err, &existing) {
		return err
	}
	if validateErr := rejection.Validate(); validateErr != nil {
		rejection = newTaskGateRejectionV1(rejectionGateFinalizerInternal,
			rejectionFailureInvalidValue, rejectionSourceFinalizerDerivation,
			rejectionSourceFinalizerDerivation)
	}
	return &taskGateRejectionError{cause: err, rejection: rejection}
}

func rejectTaskGateAt(err error, gate rejectionGate, failure rejectionFailureKind,
	expected, actual rejectionSource, differences ...rejectionDifferenceV1) error {
	return withTaskGateRejection(err,
		newTaskGateRejectionV1(gate, failure, expected, actual, differences...))
}

func rejectTaskGateTargetAt(err error, gate rejectionGate, failure rejectionFailureKind,
	expected, actual rejectionSource, role rejectionTargetRole,
	differences ...rejectionDifferenceV1) error {
	rejection := newTaskGateRejectionV1(gate, failure, expected, actual, differences...)
	rejection.targetRole = role
	return withTaskGateRejection(err, rejection)
}

func rejectTaskGateStatementClassAt(err error, gate rejectionGate, failure rejectionFailureKind,
	expected, actual rejectionSource, class GatewayStatementClassV3,
	differences ...rejectionDifferenceV1) error {
	rejection := newTaskGateRejectionV1(gate, failure, expected, actual, differences...)
	mapped, ok := rejectionStatementClassForGateway(class)
	if !ok {
		return withTaskGateRejection(err, newTaskGateRejectionV1(rejectionGateFinalizerInternal,
			rejectionFailureInvalidValue, rejectionSourceFinalizerDerivation,
			rejectionSourceFinalizerDerivation))
	}
	rejection.statementClass = mapped
	return withTaskGateRejection(err, rejection)
}

func rejectionStatementClassForGateway(class GatewayStatementClassV3) (rejectionStatementClass, bool) {
	switch class {
	case V3TransactionBegin:
		return rejectionStatementTransactionBegin, true
	case V3TransactionCommit:
		return rejectionStatementTransactionCommit, true
	case V3SafetySessionPin:
		return rejectionStatementSafetySessionPin, true
	case V3RepresentationPin:
		return rejectionStatementRepresentationPin, true
	case V3StatementTimeoutPin:
		return rejectionStatementTimeoutPin, true
	case V3DatasourceIdentity:
		return rejectionStatementDatasourceIdentity, true
	case V3ViewColumnAttestation:
		return rejectionStatementViewColumnAttestation, true
	case V3ViewDefinitionAttest:
		return rejectionStatementViewDefinitionAttestation, true
	case V3PostgreSQLInternalAttestation:
		return rejectionStatementPostgreSQLInternalAttestation, true
	case V3TargetedVisible:
		return rejectionStatementTargetedVisible, true
	case V3TargetedCompanion:
		return rejectionStatementTargetedCompanion, true
	case V3Unexpected:
		return rejectionStatementUnexpected, true
	default:
		return rejectionStatementClassInvalid, false
	}
}

// TaskGateRejectionFromError returns a deep copy only for errors produced by
// the finalizer's typed rejection boundary. It never parses Error or its chain.
func TaskGateRejectionFromError(err error) (*TaskGateRejectionV1, bool) {
	var rejected *taskGateRejectionError
	if !errors.As(err, &rejected) || rejected == nil {
		return nil, false
	}
	copy := rejected.rejection
	copy.differences = append([]rejectionDifferenceV1(nil), rejected.rejection.differences...)
	return &copy, true
}

func rejectionFromPreparedMismatch(err error) (TaskGateRejectionV1, bool) {
	var mismatch *preparedbinding.PreparedOperationMismatchError
	if !errors.As(err, &mismatch) {
		return TaskGateRejectionV1{}, false
	}
	return newTaskGateRejectionV1(rejectionGatePreparedBindingMembers,
		rejectionFailureMismatch, rejectionSourceFrozenContract, rejectionSourceGatewayReceipt,
		rejectionPreparedMemberDifferences(mismatch.Members())...), true
}

func enumName(names []string, value int) string {
	if value <= 0 || value >= len(names) {
		return ""
	}
	return names[value]
}

func parseEnum(name, label string, names []string) (int, error) {
	for value := 1; value < len(names); value++ {
		if names[value] == name {
			return value, nil
		}
	}
	return 0, fmt.Errorf("unknown TaskGate rejection %s %q", label, name)
}

func parseRejectionPhase(value string) (rejectionPhase, error) {
	parsed, err := parseEnum(value, "phase", rejectionPhaseNames[:])
	return rejectionPhase(parsed), err
}

func parseRejectionGate(value string) (rejectionGate, error) {
	parsed, err := parseEnum(value, "gate_code", rejectionGateNames[:])
	return rejectionGate(parsed), err
}

func parseRejectionFailureKind(value string) (rejectionFailureKind, error) {
	parsed, err := parseEnum(value, "failure_kind", rejectionFailureKindNames[:])
	return rejectionFailureKind(parsed), err
}

func parseRejectionSource(value string) (rejectionSource, error) {
	parsed, err := parseEnum(value, "source", rejectionSourceNames[:])
	return rejectionSource(parsed), err
}

func parseRejectionPathKind(value string) (rejectionPathKind, error) {
	parsed, err := parseEnum(value, "path_kind", rejectionPathNames[:])
	return rejectionPathKind(parsed), err
}

func parseRejectionTargetRole(value string) (rejectionTargetRole, error) {
	parsed, err := parseEnum(value, "target_role", rejectionTargetRoleNames[:])
	return rejectionTargetRole(parsed), err
}

func parseRejectionStatementClass(value string) (rejectionStatementClass, error) {
	parsed, err := parseEnum(value, "statement_class", rejectionStatementClassNames[:])
	return rejectionStatementClass(parsed), err
}

func parseRejectionDifferenceField(value string) (rejectionDifferenceField, error) {
	parsed, err := parseEnum(value, "difference field", rejectionDifferenceFieldNames[:])
	return rejectionDifferenceField(parsed), err
}

func parsePreparedOperationMember(value string) (preparedbinding.PreparedOperationMember, error) {
	for _, member := range preparedbinding.PreparedOperationMembers() {
		if member.Code() == value {
			return member, nil
		}
	}
	return 0, fmt.Errorf("unknown TaskGate rejection prepared_member %q", value)
}

func decodeClosedJSON(value []byte, target any) error {
	return StrictJSON(value, target)
}
