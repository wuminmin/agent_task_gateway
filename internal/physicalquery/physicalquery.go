// Package physicalquery derives the exact physical statements one governed
// operation executes, together with the runtime row limits they are rendered
// with.
//
// It exists so that the Gateway and the evaluation share one derivation. The
// observer classifies a target statement by its structural identity, and the
// finalizer independently rebuilds that identity from frozen contracts; if the
// evaluation reimplemented the derivation, the two would drift and the drift
// would look like a measurement result.
//
// # Why the limits belong here
//
// internal/sqlpolicy renders the row limit INTO the executable SQL. The
// companion's limit is not a constant: it derives from the authorized visible
// limit, the exposure ledger's InfluenceFacts budget, and whether the operation
// uses expanded evidence. So a shared helper that stopped at queryplan.Compile
// would produce a shape-correct companion whose rendered digest never matched a
// live run. The limit derivation is therefore part of this package, not of its
// callers.
//
// # Why the ledger pre-state is an input
//
// Limits.InfluenceFacts and the remaining row budget are runtime state. An
// Artifact, Scale or ProvSQL operation does not necessarily begin with an unused
// ledger, so the derivation takes the pre-state it ran against rather than
// assuming a fresh one. The finalizer supplies the same pre-state from signed
// evidence and must reach the same statements.
package physicalquery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// LedgerPreState is the exposure state an operation begins from.
//
// Both fields are pre-state, captured before the operation charges anything.
type LedgerPreState struct {
	// RemainingRows is the row budget still available to the task.
	RemainingRows int64
	// InfluenceFacts is the exposure ledger's influence-fact limit.
	InfluenceFacts int64
	// UsesExpandedEvidence is planExposureContext.usesExpandedEvidence():
	// grouped or expanded-evidence plans settle the companion against the
	// influence budget rather than against the visible limit.
	UsesExpandedEvidence bool
	// HasExposureContext distinguishes a governed exposure operation from a
	// plain query. Without an exposure context there is no companion at all and
	// the visible limit is not clamped.
	HasExposureContext bool
}

// Limits is the derived pair of runtime row limits.
type Limits struct {
	// VisibleRowLimit is what the visible statement is authorized with.
	VisibleRowLimit int64
	// CompanionEvidenceRows is the number of evidence rows the companion is
	// expected to yield.
	CompanionEvidenceRows int64
	// CompanionPolicyRows is what the companion statement is authorized with.
	// Under expanded evidence it is one more than the evidence rows, so a
	// truncation is detectable rather than silently indistinguishable from a
	// complete result.
	CompanionPolicyRows int64
}

// ErrExposureBudgetExhausted mirrors the production refusal when no evidence row
// remains. It is returned rather than a generic error so the finalizer can tell
// an exhausted budget from a derivation defect.
var ErrExposureBudgetExhausted = errors.New("exposure budget leaves no evidence row for the companion statement")

// DeriveLimits reproduces the Gateway's runtime row-limit derivation exactly.
//
// authorizedVisibleRowLimit is sqlpolicy.Decision.RowLimit from authorizing the
// visible statement, NOT the requested limit. The policy engine may lower it,
// and production feeds the authorized value into the companion; taking the
// requested value instead would derive a companion that never executed.
func DeriveLimits(state LedgerPreState, authorizedVisibleRowLimit int64) (Limits, error) {
	limits := Limits{VisibleRowLimit: RequestedVisibleRowLimit(state)}
	if !state.HasExposureContext {
		return limits, nil
	}
	limits.CompanionEvidenceRows = authorizedVisibleRowLimit
	limits.CompanionPolicyRows = limits.CompanionEvidenceRows
	if state.UsesExpandedEvidence {
		limits.CompanionEvidenceRows = state.InfluenceFacts
		limits.CompanionPolicyRows = limits.CompanionEvidenceRows + 1
	}
	if limits.CompanionEvidenceRows < 1 {
		return Limits{}, ErrExposureBudgetExhausted
	}
	return limits, nil
}

// RequestedVisibleRowLimit is the limit the visible statement is authorized
// with, before the policy engine has its say.
//
// A non-expanded exposure operation is clamped to the influence budget; an
// expanded one is not, because its companion settles against that budget
// instead.
func RequestedVisibleRowLimit(state LedgerPreState) int64 {
	limit := state.RemainingRows
	if state.HasExposureContext && !state.UsesExpandedEvidence {
		limit = min64(limit, state.InfluenceFacts)
	}
	return limit
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

// StatementIdentity is the complete identity of one physical statement.
type StatementIdentity struct {
	// ExactSHA256 digests the executable bytes, constants included. It is what
	// pins a constant-only mutation that a normalized digest cannot see.
	ExactSHA256 string `json:"exact_sha256"`
	// StrictASTSHA256 is the structural identity the observer classifies on. It
	// is stable across row limits, because the limit normalizes to a
	// placeholder, so a budget difference cannot cause a misclassification.
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	// RowLimit is the limit rendered into these bytes.
	RowLimit int64 `json:"row_limit"`
	// Fingerprint is the policy engine's own audit fingerprint.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Derivation is the pair of statements one operation executes, plus the limits
// they were rendered with.
//
// Companion is absent for a plain query. Callers must not infer a companion from
// the presence of a visible statement.
type Derivation struct {
	Limits    Limits             `json:"limits"`
	Visible   StatementIdentity  `json:"visible"`
	Companion *StatementIdentity `json:"companion,omitempty"`
}

// StrictASTDigester computes the structural digest. It is injected because the
// implementation lives in the evaluation tree, and production must not depend on
// evaluation code; when it is nil only the exact digest is computed.
type StrictASTDigester func(sql string) (string, error)

// Authorizer is the subset of sqlpolicy this package needs.
type Authorizer interface {
	Authorize(sqlpolicy.Request) (sqlpolicy.Decision, error)
}

// Request is one operation's inputs.
type Request struct {
	// VisibleSQL is the compiled physical statement for the visible plan.
	VisibleSQL string
	// CompanionSQL is the compiled physical statement for the provenance plan.
	// Empty means the operation has no companion.
	CompanionSQL string
	Grant        sqlpolicy.Grant
	State        LedgerPreState
}

// Derive authorizes both statements exactly as production does and returns their
// identities.
//
// The order matters and is not cosmetic: the companion's limit depends on the
// visible statement's AUTHORIZED limit, so the visible statement must be
// authorized first.
func Derive(engine Authorizer, digester StrictASTDigester, request Request) (Derivation, error) {
	if engine == nil {
		return Derivation{}, errors.New("physical query derivation requires an authorizer")
	}
	if request.VisibleSQL == "" {
		return Derivation{}, errors.New("physical query derivation requires a visible statement")
	}
	if request.CompanionSQL != "" && !request.State.HasExposureContext {
		return Derivation{}, errors.New("a companion statement requires an exposure context")
	}

	visibleDecision, err := engine.Authorize(sqlpolicy.Request{
		SQL: request.VisibleSQL, Grant: request.Grant,
		RowLimit: RequestedVisibleRowLimit(request.State),
	})
	if err != nil {
		return Derivation{}, fmt.Errorf("authorize visible statement: %w", err)
	}

	limits, err := DeriveLimits(request.State, visibleDecision.RowLimit)
	if err != nil {
		return Derivation{}, err
	}
	// The authorized limit is what actually executed, so it is what the
	// derivation reports.
	limits.VisibleRowLimit = visibleDecision.RowLimit

	derivation := Derivation{Limits: limits}
	if derivation.Visible, err = identify(visibleDecision, digester); err != nil {
		return Derivation{}, fmt.Errorf("visible statement identity: %w", err)
	}
	if request.CompanionSQL == "" {
		return derivation, nil
	}

	companionDecision, err := engine.Authorize(sqlpolicy.Request{
		SQL: request.CompanionSQL, Grant: request.Grant, RowLimit: limits.CompanionPolicyRows,
	})
	if err != nil {
		return Derivation{}, fmt.Errorf("authorize companion statement: %w", err)
	}
	companion, err := identify(companionDecision, digester)
	if err != nil {
		return Derivation{}, fmt.Errorf("companion statement identity: %w", err)
	}
	derivation.Companion = &companion
	return derivation, nil
}

func identify(decision sqlpolicy.Decision, digester StrictASTDigester) (StatementIdentity, error) {
	identity := StatementIdentity{
		ExactSHA256: ExactDigest(decision.SQL), RowLimit: decision.RowLimit,
		Fingerprint: decision.Fingerprint,
	}
	if digester == nil {
		return identity, nil
	}
	strict, err := digester(decision.SQL)
	if err != nil {
		return StatementIdentity{}, err
	}
	identity.StrictASTSHA256 = strict
	return identity, nil
}

// ExactDigest digests executable statement bytes.
func ExactDigest(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}
