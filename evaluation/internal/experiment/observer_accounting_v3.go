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

// GatewayPathKind is the closed set of execution paths a governed operation can
// take. It is authoritative: Validate pins every dimension from the path kind
// rather than accepting any numerically self-consistent combination, so a
// hybrid that no code path can produce cannot be expressed at all.
type GatewayPathKind string

const (
	// PathPairedNovel is Exposure V4/V5 novel execution: one preflight
	// attestation, one QueryPairStream transaction, visible plus companion.
	PathPairedNovel GatewayPathKind = "paired_novel"
	// PathSemanticReplay is served from the semantic cache under a new request
	// ID. The cache lookup happens after datasourceEvidence, so the preflight
	// attestation still runs.
	PathSemanticReplay GatewayPathKind = "semantic_replay"
	// PathIdempotentReplay is an exact request-ID replay, returning before
	// datasourceEvidence and touching Business PostgreSQL not at all.
	PathIdempotentReplay GatewayPathKind = "idempotent_replay"
	// PathSingleQuery is Connector.Query: one transaction, one target
	// statement, no provenance companion and no representation pin.
	PathSingleQuery GatewayPathKind = "single_query"
)

// pathDimensions is the exact dimension tuple each path kind must carry. It is
// the single source of truth for Validate and for the named constructors, so
// the two cannot drift apart.
type pathDimensions struct {
	preflight, single, paired, visible, companion int64
	requiresSchema                                bool
}

func dimensionsFor(kind GatewayPathKind) (pathDimensions, bool) {
	switch kind {
	case PathPairedNovel:
		return pathDimensions{preflight: 1, single: 0, paired: 1, visible: 1, companion: 1, requiresSchema: true}, true
	case PathSemanticReplay:
		return pathDimensions{preflight: 1, single: 0, paired: 0, visible: 0, companion: 0, requiresSchema: true}, true
	case PathIdempotentReplay:
		return pathDimensions{preflight: 0, single: 0, paired: 0, visible: 0, companion: 0, requiresSchema: false}, true
	case PathSingleQuery:
		return pathDimensions{preflight: 1, single: 1, paired: 0, visible: 1, companion: 0, requiresSchema: true}, true
	default:
		return pathDimensions{}, false
	}
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
	// PathKind is authoritative. The algebraic checks below are retained as
	// defence in depth, but they cannot by themselves exclude a hybrid that is
	// arithmetically consistent and yet corresponds to no execution path.
	PathKind GatewayPathKind `json:"path_kind"`
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

	// AttestationFootprintSHA256 pins the qualification run whose measured
	// footprint produced NestedInternalCalls. It is empty exactly when the path
	// performs no Attestation.
	AttestationFootprintSHA256 string `json:"attestation_footprint_sha256,omitempty"`
	// NestedInternalCalls is the expected count of the statements PostgreSQL
	// emits inside pg_get_viewdef.
	//
	// It is carried rather than computed from E because it is not derivable from
	// TaskGate source at all: it is a measured property of one server, one
	// ExpectedSchema, one environment and one image. Earlier versions asserted
	// (P+S+Q)*E, which happened to be the measured value but was a rule nothing
	// established. The finalizer does not trust this number -- RequireFootprint
	// recomputes it from the qualified footprint.
	NestedInternalCalls int64 `json:"nested_internal_calls"`

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
//	nested_viewdef_rewrite_lookup = measured, per qualified footprint
//	targeted_visible              = V
//	targeted_companion            = C
//	unexpected                    = 0
//
// The two per-entry attestations are source-derived: the Connector executes one
// exported constant per ExpectedSchema entry, so the shared builder settles their
// multiplicity.
//
// The nested lookup is not. PostgreSQL issues it internally inside
// pg_get_viewdef, it is only observable because track='all' counts nested
// statements, and it must be observed with toplevel=false. No reading of TaskGate
// source establishes how often the server emits it, so it comes from a qualified
// AttestationFootprintV1 rather than from a rule.
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
		V3NestedViewdefRewrite:  plan.NestedInternalCalls,
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
//
// PathKind is authoritative: every dimension is pinned to the tuple that path
// actually produces. The algebraic checks that follow are retained as defence in
// depth -- they would catch a mistake in the dimension table itself -- but they
// are not what excludes a hybrid. A numerically self-consistent combination that
// corresponds to no code path is rejected because its PathKind does not match,
// not because the arithmetic disagrees.
func (plan GatewayControlPlanV3) Validate() error {
	if plan.Version != ObserverAccountingV3Version {
		return fmt.Errorf("gateway control plan version %q is unsupported", plan.Version)
	}
	if err := plan.Environment.Validate(); err != nil {
		return err
	}
	required, known := dimensionsFor(plan.PathKind)
	if !known {
		return fmt.Errorf("gateway control plan path_kind %q is not a derivable execution path", plan.PathKind)
	}
	for _, dimension := range []struct {
		name         string
		actual, want int64
	}{
		{"preflight_attestation_passes", plan.PreflightAttestationPasses, required.preflight},
		{"single_query_transactions", plan.SingleQueryTransactions, required.single},
		{"paired_query_transactions", plan.PairedQueryTransactions, required.paired},
		{"expected_visible_calls", plan.ExpectedVisibleCalls, required.visible},
		{"expected_companion_calls", plan.ExpectedCompanionCall, required.companion},
	} {
		if dimension.actual != dimension.want {
			return fmt.Errorf("path_kind %s requires %s=%d, plan carries %d",
				plan.PathKind, dimension.name, dimension.want, dimension.actual)
		}
	}
	// E and its digest are presence-coupled in both directions: a plan that
	// attests must name what it attested against, and a plan that attests
	// against nothing must not carry a digest.
	switch {
	case required.requiresSchema:
		if plan.ExpectedSchemaEntries <= 0 {
			return fmt.Errorf("path_kind %s attests against no ExpectedSchema entry", plan.PathKind)
		}
		if !validSHA256(plan.ExpectedSchemaDigest) {
			return errors.New("gateway control plan ExpectedSchema digest is not a lowercase SHA-256")
		}
		// A path that attests must name the qualification its nested count came
		// from. Without this a plan could carry any number for a class no source
		// derivation can check.
		if !validSHA256(plan.AttestationFootprintSHA256) {
			return fmt.Errorf("path_kind %s attests but names no qualified Attestation footprint", plan.PathKind)
		}
		if plan.NestedInternalCalls < 0 {
			return fmt.Errorf("gateway control plan expects %d nested internal statements", plan.NestedInternalCalls)
		}
	default:
		if plan.ExpectedSchemaEntries != 0 {
			return fmt.Errorf("path_kind %s reaches no ExpectedSchema entry but claims %d",
				plan.PathKind, plan.ExpectedSchemaEntries)
		}
		if plan.ExpectedSchemaDigest != "" {
			return fmt.Errorf("path_kind %s attests against nothing but carries an ExpectedSchema digest", plan.PathKind)
		}
		if plan.AttestationFootprintSHA256 != "" {
			return fmt.Errorf("path_kind %s performs no Attestation but carries a footprint digest", plan.PathKind)
		}
		if plan.NestedInternalCalls != 0 {
			return fmt.Errorf("path_kind %s performs no Attestation but expects %d nested internal statements",
				plan.PathKind, plan.NestedInternalCalls)
		}
	}

	// Defence in depth. These restate the structural facts the dimension table
	// encodes, so a table edit that broke one of them would fail here too.
	transactions := plan.SingleQueryTransactions + plan.PairedQueryTransactions
	if targets := plan.ExpectedVisibleCalls + plan.ExpectedCompanionCall; targets !=
		plan.SingleQueryTransactions+2*plan.PairedQueryTransactions {
		return fmt.Errorf("gateway control plan settles %d single and %d paired transactions but expects %d target statements",
			plan.SingleQueryTransactions, plan.PairedQueryTransactions, targets)
	}
	if plan.PairedQueryTransactions > 0 && plan.ExpectedCompanionCall != plan.PairedQueryTransactions {
		return fmt.Errorf("gateway control plan pairs %d transactions with %d companion statements",
			plan.PairedQueryTransactions, plan.ExpectedCompanionCall)
	}
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

// planFor builds the plan for one path kind from the single dimension table, so
// a constructor cannot disagree with Validate.
//
// The footprint is not merely stored: the ExpectedSchema the caller names is
// checked against the one the footprint was qualified for, and the nested count
// is derived scope-wise from the measured per-Attestation multiplicities. A
// footprint qualified elsewhere therefore cannot produce a plan at all.
func planFor(kind GatewayPathKind, schemaEntries int64, schemaDigest string,
	footprint AttestationFootprintV1) (GatewayControlPlanV3, error) {
	dimensions, _ := dimensionsFor(kind)
	plan := GatewayControlPlanV3{
		Version: ObserverAccountingV3Version, PathKind: kind,
		PreflightAttestationPasses: dimensions.preflight,
		SingleQueryTransactions:    dimensions.single,
		PairedQueryTransactions:    dimensions.paired,
		ExpectedVisibleCalls:       dimensions.visible,
		ExpectedCompanionCall:      dimensions.companion,
		Environment:                RequiredMeasurementEnvironment(),
	}
	if dimensions.requiresSchema {
		if footprint.ExpectedSchemaDigest != schemaDigest || footprint.ExpectedSchemaEntries != schemaEntries {
			return GatewayControlPlanV3{}, fmt.Errorf(
				"path_kind %s plans against ExpectedSchema %s with %d entries, "+
					"the supplied footprint was qualified for %s with %d entries",
				kind, shortDigest(schemaDigest), schemaEntries,
				shortDigest(footprint.ExpectedSchemaDigest), footprint.ExpectedSchemaEntries)
		}
		if footprint.Environment != plan.Environment {
			return GatewayControlPlanV3{}, fmt.Errorf(
				"path_kind %s plans under PostgreSQL %d but the footprint was qualified under %d",
				kind, plan.Environment.PostgreSQLVersionNum, footprint.Environment.PostgreSQLVersionNum)
		}
		digest, err := footprint.SHA256()
		if err != nil {
			return GatewayControlPlanV3{}, fmt.Errorf("path_kind %s: %w", kind, err)
		}
		nested, err := footprint.InternalCalls(dimensions.preflight, dimensions.single+dimensions.paired)
		if err != nil {
			return GatewayControlPlanV3{}, fmt.Errorf("path_kind %s: %w", kind, err)
		}
		plan.ExpectedSchemaEntries, plan.ExpectedSchemaDigest = schemaEntries, schemaDigest
		plan.AttestationFootprintSHA256, plan.NestedInternalCalls = digest, nested
	}
	if err := plan.Validate(); err != nil {
		return GatewayControlPlanV3{}, err
	}
	return plan, nil
}

// RequireFootprint re-derives the nested expectation from a qualified footprint
// and the live deployment identity, and rejects any disagreement.
//
// This is what lets the finalizer refuse to trust the Adapter. The plan carries
// NestedInternalCalls as a claim; this recomputes it from the footprint the
// digest pins, under the ExpectedSchema, environment and image the run actually
// used. A plan whose nested count was edited, or whose footprint was qualified
// against a different deployment, fails here rather than at acceptance.
func (plan GatewayControlPlanV3) RequireFootprint(footprint AttestationFootprintV1, postgreSQLImageID string) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	dimensions, known := dimensionsFor(plan.PathKind)
	if !known {
		return fmt.Errorf("gateway control plan path_kind %q is not a derivable execution path", plan.PathKind)
	}
	if !dimensions.requiresSchema {
		// The path performs no Attestation; Validate already required the plan to
		// carry no footprint binding, so there is nothing to re-derive.
		return nil
	}
	digest, err := footprint.SHA256()
	if err != nil {
		return err
	}
	if digest != plan.AttestationFootprintSHA256 {
		return fmt.Errorf("gateway control plan was derived under Attestation footprint %s, the supplied qualification is %s",
			shortDigest(plan.AttestationFootprintSHA256), shortDigest(digest))
	}
	if err := footprint.Require(plan.ExpectedSchemaDigest, plan.ExpectedSchemaEntries,
		plan.Environment, postgreSQLImageID); err != nil {
		return err
	}
	nested, err := footprint.InternalCalls(plan.PreflightAttestationPasses,
		plan.SingleQueryTransactions+plan.PairedQueryTransactions)
	if err != nil {
		return err
	}
	if nested != plan.NestedInternalCalls {
		return fmt.Errorf("gateway control plan expects %d nested internal statements, the qualified footprint derives %d",
			plan.NestedInternalCalls, nested)
	}
	return nil
}

// The four derivable paths. Each is a named constructor rather than a set of
// magic numbers at a call site, so a path that does not exist cannot be
// expressed by accident.

// PairedNovelPlanV3 is the Exposure V4/V5 novel path: one preflight
// attestation, one QueryPairStream transaction, one visible and one companion.
func PairedNovelPlanV3(schemaEntries int64, schemaDigest string,
	footprint AttestationFootprintV1) (GatewayControlPlanV3, error) {
	return planFor(PathPairedNovel, schemaEntries, schemaDigest, footprint)
}

// SemanticReplayPlanV3 is a replay served from the semantic cache under a new
// request ID. The cache lookup happens after datasourceEvidence, so the
// preflight attestation still runs; no transaction is opened and no target
// statement executes. Modelling this as all-zero, as v2 did, would reject a
// correct replay.
func SemanticReplayPlanV3(schemaEntries int64, schemaDigest string,
	footprint AttestationFootprintV1) (GatewayControlPlanV3, error) {
	return planFor(PathSemanticReplay, schemaEntries, schemaDigest, footprint)
}

// IdempotentReplayPlanV3 is an exact request-ID replay. It returns before
// datasourceEvidence, so it touches Business PostgreSQL not at all.
// It performs no Attestation, so it is the one path that needs no qualified
// footprint.
func IdempotentReplayPlanV3() (GatewayControlPlanV3, error) {
	return planFor(PathIdempotentReplay, 0, "", AttestationFootprintV1{})
}

// SingleQueryPlanV3 is the Connector.Query path: one preflight attestation, one
// transaction, one target statement, and no representation pin -- Query does
// not issue one.
func SingleQueryPlanV3(schemaEntries int64, schemaDigest string,
	footprint AttestationFootprintV1) (GatewayControlPlanV3, error) {
	return planFor(PathSingleQuery, schemaEntries, schemaDigest, footprint)
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
		"path_kind":                    {plan.PathKind, other.PathKind},
		"preflight_attestation_passes": {plan.PreflightAttestationPasses, other.PreflightAttestationPasses},
		"single_query_transactions":    {plan.SingleQueryTransactions, other.SingleQueryTransactions},
		"paired_query_transactions":    {plan.PairedQueryTransactions, other.PairedQueryTransactions},
		"expected_schema_entries":      {plan.ExpectedSchemaEntries, other.ExpectedSchemaEntries},
		"expected_schema_digest":       {plan.ExpectedSchemaDigest, other.ExpectedSchemaDigest},
		"expected_visible_calls":       {plan.ExpectedVisibleCalls, other.ExpectedVisibleCalls},
		"expected_companion_calls":     {plan.ExpectedCompanionCall, other.ExpectedCompanionCall},
		"attestation_footprint_sha256": {plan.AttestationFootprintSHA256, other.AttestationFootprintSHA256},
		"nested_internal_calls":        {plan.NestedInternalCalls, other.NestedInternalCalls},
		"measurement_environment":      {plan.Environment, other.Environment},
	} {
		if pair[0] != pair[1] {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}
