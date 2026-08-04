package experiment

import (
	"errors"
	"fmt"
	"sort"
)

// ObserverAccountingV3Version identifies the phase-derived closed-world
// statement accounting.
//
// Version 2 (contracts v1.4) is superseded and must not be repaired in place;
// see docs/final_v5_observer_accounting_v14_audit.md. It modelled the paired
// path as two Connector.Query transactions, had no class for the representation
// pin, modelled semantic replay as issuing no Business statement at all, and
// ignored the nested statements PostgreSQL counts under
// pg_stat_statements.track=all.
//
// Version 3 derives every multiplicity from independent phase dimensions rather
// than inferring the transaction count from the number of target statements.
const ObserverAccountingV3Version = "taskgate-final-v5-observer-accounting-v3"

// GatewayStatementClassV3 names one class in the v3 closed world.
type GatewayStatementClassV3 string

const (
	V3TransactionBegin      GatewayStatementClassV3 = "transaction_begin"
	V3TransactionCommit     GatewayStatementClassV3 = "transaction_commit"
	V3SafetySessionPin      GatewayStatementClassV3 = "safety_session_pin"
	V3RepresentationPin     GatewayStatementClassV3 = "representation_pin"
	V3StatementTimeoutPin   GatewayStatementClassV3 = "statement_timeout_pin"
	V3DatasourceIdentity    GatewayStatementClassV3 = "datasource_identity"
	V3ViewColumnAttestation GatewayStatementClassV3 = "view_column_attestation"
	V3ViewDefinitionAttest  GatewayStatementClassV3 = "view_definition_attestation"
	V3NestedViewdefRewrite  GatewayStatementClassV3 = "nested_viewdef_rewrite_lookup"
	V3TargetedVisible       GatewayStatementClassV3 = "targeted_visible"
	V3TargetedCompanion     GatewayStatementClassV3 = "targeted_companion"
	V3Unexpected            GatewayStatementClassV3 = "unexpected"
)

// GatewayStatementClassesV3 is the closed world in canonical order.
func GatewayStatementClassesV3() []GatewayStatementClassV3 {
	return []GatewayStatementClassV3{
		V3TransactionBegin, V3TransactionCommit,
		V3SafetySessionPin, V3RepresentationPin, V3StatementTimeoutPin,
		V3DatasourceIdentity, V3ViewColumnAttestation, V3ViewDefinitionAttest,
		V3NestedViewdefRewrite,
		V3TargetedVisible, V3TargetedCompanion,
		V3Unexpected,
	}
}

// MeasurementEnvironment is the observation environment the multiplicities are valid
// under. It is part of the plan because two of the v3 rules are not universal:
// BEGIN and COMMIT are only counted when track_utility is on, and the nested
// pg_get_viewdef rewrite lookup is only counted when track is 'all'.
//
// Binding it into the plan means a deployment whose settings differ cannot be
// silently accounted with rules derived for a different configuration.
type MeasurementEnvironment struct {
	PostgreSQLVersionNum int64  `json:"postgresql_version_num"`
	Track                string `json:"pg_stat_statements_track"`
	TrackUtility         string `json:"pg_stat_statements_track_utility"`
	TrackPlanning        string `json:"pg_stat_statements_track_planning"`
}

// RequiredMeasurementEnvironment is the formal Final-V5 environment the v3
// multiplicities were derived against, and the only one they are valid for.
func RequiredMeasurementEnvironment() MeasurementEnvironment {
	return MeasurementEnvironment{
		PostgreSQLVersionNum: 160014, Track: "all", TrackUtility: "on", TrackPlanning: "off",
	}
}

// Validate rejects an environment the derived rules do not cover. It is
// deliberately exact rather than a lower bound: the nested-lookup rule is a
// property of one PostgreSQL implementation, not a guarantee across versions.
func (environment MeasurementEnvironment) Validate() error {
	required := RequiredMeasurementEnvironment()
	if environment != required {
		return fmt.Errorf("observer accounting v3 is derived for PostgreSQL %d with track=%s/track_utility=%s/track_planning=%s, "+
			"this deployment reports %d with track=%s/track_utility=%s/track_planning=%s",
			required.PostgreSQLVersionNum, required.Track, required.TrackUtility, required.TrackPlanning,
			environment.PostgreSQLVersionNum, environment.Track, environment.TrackUtility, environment.TrackPlanning)
	}
	return nil
}

// GatewayControlPlanV3 is the phase-derived expectation.
//
// The dimensions are independent by construction. In particular the transaction
// counts are NOT derived from the target-statement counts: a paired transaction
// settles two target statements, a semantic replay settles none while still
// performing a preflight attestation, and an exact idempotent replay returns
// before reaching Business PostgreSQL at all.
type GatewayControlPlanV3 struct {
	Version string `json:"version"`
	// PreflightAttestationPasses is Connector.Attestation against the pool,
	// outside any transaction, before the semantic-cache lookup.
	PreflightAttestationPasses int64 `json:"preflight_attestation_passes"`
	// SingleQueryTransactions is Connector.Query: one transaction settling one
	// target statement, with no representation pin.
	SingleQueryTransactions int64 `json:"single_query_transactions"`
	// PairedQueryTransactions is Connector.QueryPairStream: one transaction
	// settling the visible statement and its provenance companion.
	PairedQueryTransactions int64 `json:"paired_query_transactions"`
	// ExpectedSchemaEntries is the exact number of ordered ExpectedSchema
	// entries the Gateway Connector holds, not a count of distinct reporting
	// views; see ExpectedSchemaDigest.
	ExpectedSchemaEntries int64  `json:"expected_schema_entries"`
	ExpectedSchemaDigest  string `json:"expected_schema_digest"`
	ExpectedVisibleCalls  int64  `json:"expected_visible_calls"`
	ExpectedCompanionCall int64  `json:"expected_companion_calls"`

	Environment MeasurementEnvironment `json:"measurement_environment"`
}

// Expected derives the per-class multiplicity from the phase dimensions.
//
//	transaction_begin             = S + Q
//	transaction_commit            = S + Q
//	safety_session_pin            = S + Q
//	representation_pin            = Q          (paired path only)
//	statement_timeout_pin         = V + C      (one before each target)
//	datasource_identity           = P + S + Q
//	view_column_attestation       = (P + S + Q) * E
//	view_definition_attestation   = (P + S + Q) * E
//	nested_viewdef_rewrite_lookup = (P + S + Q) * E
//	targeted_visible              = V
//	targeted_companion            = C
//	unexpected                    = 0
//
// The nested lookup is not a Gateway statement. PostgreSQL issues it internally
// inside pg_get_viewdef, and it is only observable because track='all' counts
// nested statements; it must also be observed with toplevel=false.
func (plan GatewayControlPlanV3) Expected() map[GatewayStatementClassV3]int64 {
	transactions := plan.SingleQueryTransactions + plan.PairedQueryTransactions
	attestations := plan.PreflightAttestationPasses + transactions
	perEntry := attestations * plan.ExpectedSchemaEntries
	return map[GatewayStatementClassV3]int64{
		V3TransactionBegin:      transactions,
		V3TransactionCommit:     transactions,
		V3SafetySessionPin:      transactions,
		V3RepresentationPin:     plan.PairedQueryTransactions,
		V3StatementTimeoutPin:   plan.ExpectedVisibleCalls + plan.ExpectedCompanionCall,
		V3DatasourceIdentity:    attestations,
		V3ViewColumnAttestation: perEntry,
		V3ViewDefinitionAttest:  perEntry,
		V3NestedViewdefRewrite:  perEntry,
		V3TargetedVisible:       plan.ExpectedVisibleCalls,
		V3TargetedCompanion:     plan.ExpectedCompanionCall,
		V3Unexpected:            0,
	}
}

// ExpectedTotal is the whole closed-world expectation.
func (plan GatewayControlPlanV3) ExpectedTotal() int64 {
	var total int64
	for _, class := range GatewayStatementClassesV3() {
		total += plan.Expected()[class]
	}
	return total
}

// Validate rejects a plan that does not describe a derivable execution path.
func (plan GatewayControlPlanV3) Validate() error {
	if plan.Version != ObserverAccountingV3Version {
		return fmt.Errorf("gateway control plan version %q is unsupported", plan.Version)
	}
	if err := plan.Environment.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]int64{
		"preflight_attestation_passes": plan.PreflightAttestationPasses,
		"single_query_transactions":    plan.SingleQueryTransactions,
		"paired_query_transactions":    plan.PairedQueryTransactions,
		"expected_schema_entries":      plan.ExpectedSchemaEntries,
		"expected_visible_calls":       plan.ExpectedVisibleCalls,
		"expected_companion_calls":     plan.ExpectedCompanionCall,
	} {
		if value < 0 {
			return fmt.Errorf("gateway control plan %s is negative", name)
		}
	}
	transactions := plan.SingleQueryTransactions + plan.PairedQueryTransactions
	// A transaction can only be accounted for if the Connector held a schema
	// expectation to attest against, and likewise a preflight pass.
	if plan.ExpectedSchemaEntries == 0 && (transactions > 0 || plan.PreflightAttestationPasses > 0) {
		return errors.New("gateway control plan attests against no ExpectedSchema entry")
	}
	if plan.ExpectedSchemaEntries > 0 && !validSHA256(plan.ExpectedSchemaDigest) {
		return errors.New("gateway control plan ExpectedSchema digest is not SHA-256")
	}
	// A single-query transaction settles exactly one target statement and a
	// paired transaction exactly two, so the target counts are constrained by
	// the transaction mix rather than defining it.
	if targets := plan.ExpectedVisibleCalls + plan.ExpectedCompanionCall; targets !=
		plan.SingleQueryTransactions+2*plan.PairedQueryTransactions {
		return fmt.Errorf("gateway control plan settles %d single and %d paired transactions but expects %d target statements",
			plan.SingleQueryTransactions, plan.PairedQueryTransactions, targets)
	}
	// A paired transaction settles exactly one visible and one companion.
	if plan.PairedQueryTransactions > 0 && plan.ExpectedCompanionCall != plan.PairedQueryTransactions {
		return fmt.Errorf("gateway control plan pairs %d transactions with %d companion statements",
			plan.PairedQueryTransactions, plan.ExpectedCompanionCall)
	}
	// A single-query transaction has no provenance companion.
	if plan.PairedQueryTransactions == 0 && plan.ExpectedCompanionCall != 0 {
		return errors.New("gateway control plan expects a companion statement without a paired transaction")
	}
	// Reaching Business PostgreSQL at all requires a preflight attestation: the
	// semantic-cache lookup happens after datasourceEvidence.
	if plan.PreflightAttestationPasses == 0 && transactions > 0 {
		return errors.New("gateway control plan opens a transaction without a preflight attestation")
	}
	return nil
}

// The four derivable paths. Each is a named constructor rather than a set of
// magic numbers at a call site, so a path that does not exist cannot be
// expressed by accident.

// PairedNovelPlanV3 is the Exposure V4/V5 novel path: one preflight
// attestation, one QueryPairStream transaction, one visible and one companion.
func PairedNovelPlanV3(schemaEntries int64, schemaDigest string) GatewayControlPlanV3 {
	return GatewayControlPlanV3{
		Version: ObserverAccountingV3Version, PreflightAttestationPasses: 1,
		SingleQueryTransactions: 0, PairedQueryTransactions: 1,
		ExpectedSchemaEntries: schemaEntries, ExpectedSchemaDigest: schemaDigest,
		ExpectedVisibleCalls: 1, ExpectedCompanionCall: 1,
		Environment: RequiredMeasurementEnvironment(),
	}
}

// SemanticReplayPlanV3 is a replay served from the semantic cache under a new
// request ID. The cache lookup happens after datasourceEvidence, so the
// preflight attestation still runs; no transaction is opened and no target
// statement executes. Modelling this as all-zero, as v2 did, would reject a
// correct replay.
func SemanticReplayPlanV3(schemaEntries int64, schemaDigest string) GatewayControlPlanV3 {
	return GatewayControlPlanV3{
		Version: ObserverAccountingV3Version, PreflightAttestationPasses: 1,
		SingleQueryTransactions: 0, PairedQueryTransactions: 0,
		ExpectedSchemaEntries: schemaEntries, ExpectedSchemaDigest: schemaDigest,
		ExpectedVisibleCalls: 0, ExpectedCompanionCall: 0,
		Environment: RequiredMeasurementEnvironment(),
	}
}

// IdempotentReplayPlanV3 is an exact request-ID replay. It returns before
// datasourceEvidence, so it touches Business PostgreSQL not at all.
func IdempotentReplayPlanV3() GatewayControlPlanV3 {
	return GatewayControlPlanV3{
		Version: ObserverAccountingV3Version, Environment: RequiredMeasurementEnvironment(),
	}
}

// SingleQueryPlanV3 is the Connector.Query path: one preflight attestation, one
// transaction, one target statement, and no representation pin -- Query does
// not issue one.
func SingleQueryPlanV3(schemaEntries int64, schemaDigest string) GatewayControlPlanV3 {
	return GatewayControlPlanV3{
		Version: ObserverAccountingV3Version, PreflightAttestationPasses: 1,
		SingleQueryTransactions: 1, PairedQueryTransactions: 0,
		ExpectedSchemaEntries: schemaEntries, ExpectedSchemaDigest: schemaDigest,
		ExpectedVisibleCalls: 1, ExpectedCompanionCall: 0,
		Environment: RequiredMeasurementEnvironment(),
	}
}

// Equal reports whether two plans are identical in every dimension. The
// finalizer requires this against its own independently derived plan before it
// will look at any observed count.
func (plan GatewayControlPlanV3) Equal(other GatewayControlPlanV3) bool { return plan == other }

// MismatchedFields names exactly which dimensions two plans disagree on, so a
// finalizer rejection says what the Adapter got wrong.
func (plan GatewayControlPlanV3) MismatchedFields(other GatewayControlPlanV3) []string {
	var fields []string
	for name, pair := range map[string][2]any{
		"version":                      {plan.Version, other.Version},
		"preflight_attestation_passes": {plan.PreflightAttestationPasses, other.PreflightAttestationPasses},
		"single_query_transactions":    {plan.SingleQueryTransactions, other.SingleQueryTransactions},
		"paired_query_transactions":    {plan.PairedQueryTransactions, other.PairedQueryTransactions},
		"expected_schema_entries":      {plan.ExpectedSchemaEntries, other.ExpectedSchemaEntries},
		"expected_schema_digest":       {plan.ExpectedSchemaDigest, other.ExpectedSchemaDigest},
		"expected_visible_calls":       {plan.ExpectedVisibleCalls, other.ExpectedVisibleCalls},
		"expected_companion_calls":     {plan.ExpectedCompanionCall, other.ExpectedCompanionCall},
		"measurement_environment":      {plan.Environment, other.Environment},
	} {
		if pair[0] != pair[1] {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}
