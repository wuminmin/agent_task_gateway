package gateway

import (
	"errors"
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/sqlidentity"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// derivedQuery is one physicalquery derivation together with the values the
// Gateway reads off it.
//
// It exists so that the preparation derivation and the authoritative one are
// produced by the same call and compared as whole objects. When the two were
// one derivation, "the state the limits came from" and "the state the
// reservation observed" were the same only when nothing else touched the task.
type derivedQuery struct {
	derivation physicalquery.Derivation
	// visible and companion are the decisions actually handed to the Connector.
	visible   sqlpolicy.Decision
	companion *sqlpolicy.Decision
	// companionEvidenceRows is the row count the companion is expected to yield,
	// which is not the limit it is authorized with under expanded evidence.
	companionEvidenceRows int64
}

// derivePhysicalQuery authorizes both statements exactly as production executes
// them and computes their full identities.
//
// The strict-AST digester is sqlidentity.StrictASTDigest and is never nil. It
// used to be nil here, because the implementation lived in the evaluation tree
// where production could not reach it, so every statement identity the Gateway
// produced carried an empty structural digest -- the one field the observer
// classifies on.
func (s *Service) derivePhysicalQuery(engine physicalquery.Authorizer, visibleSQL, companionSQL string,
	grant sqlpolicy.Grant, state physicalquery.LedgerPreState, hasExposure bool) (derivedQuery, error) {
	derivation, err := physicalquery.Derive(engine, sqlidentity.StrictASTDigest, physicalquery.Request{
		VisibleSQL: visibleSQL, CompanionSQL: companionSQL, Grant: grant, State: state,
	})
	if err != nil {
		if errors.Is(err, physicalquery.ErrExposureBudgetExhausted) {
			return derivedQuery{}, toolError(control.ErrExposureBudgetExhausted)
		}
		if errors.Is(err, sqlidentity.ErrStrictAST) {
			// A statement the Gateway is about to execute but cannot give a
			// structural identity would produce a binding the observer can never
			// match. That is not a degraded receipt; it is an unclassifiable
			// execution, so it does not run.
			return derivedQuery{}, toolError(control.ErrExposureEvidenceRequired)
		}
		if hasExposure && strings.Contains(err.Error(), "companion") {
			return derivedQuery{}, toolError(control.ErrExposureEvidenceRequired)
		}
		return derivedQuery{}, err
	}
	result := derivedQuery{derivation: derivation, visible: derivation.VisibleDecision}
	if hasExposure {
		if derivation.CompanionDecision == nil {
			return derivedQuery{}, toolError(control.ErrExposureEvidenceRequired)
		}
		result.companion = derivation.CompanionDecision
		result.companionEvidenceRows = derivation.Limits.CompanionEvidenceRows
	}
	return result, nil
}

// requireNarrowedDerivation refuses an authoritative derivation that asks for
// more than preparation reserved.
//
// Preparation reads the budget and the exposure ledger outside any lock; the
// reservation reads them under the task lock. If they moved in the narrowing
// direction -- another query settled, the budget shrank -- the authoritative
// derivation renders a smaller limit and everything remains covered. If they
// moved the other way, the reservation is too small for the statement about to
// run, and executing it would charge against a reservation that never covered
// it. There is no safe silent repair: re-reserving would need the task lock the
// reservation already released, and clamping would sign a pre-state that did
// not produce the executed bytes.
func requireNarrowedDerivation(prepared, executed derivedQuery, allowedRows int64) error {
	if executed.visible.RowLimit > allowedRows {
		return fmt.Errorf("the authoritative derivation authorizes %d visible rows but only %d were reserved",
			executed.visible.RowLimit, allowedRows)
	}
	if executed.visible.RowLimit > prepared.visible.RowLimit {
		return fmt.Errorf("the authoritative derivation authorizes %d visible rows but preparation reserved for %d",
			executed.visible.RowLimit, prepared.visible.RowLimit)
	}
	if executed.companionEvidenceRows > prepared.companionEvidenceRows {
		return fmt.Errorf("the authoritative derivation expects %d companion evidence rows but preparation reserved for %d",
			executed.companionEvidenceRows, prepared.companionEvidenceRows)
	}
	if (executed.companion == nil) != (prepared.companion == nil) {
		return errors.New("the authoritative derivation and preparation disagree about whether a companion statement runs")
	}
	if executed.companion != nil && executed.companion.RowLimit > prepared.companion.RowLimit {
		return fmt.Errorf("the authoritative derivation authorizes %d companion rows but preparation reserved for %d",
			executed.companion.RowLimit, prepared.companion.RowLimit)
	}
	return nil
}
