package physicalquery

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// stubEngine renders the row limit into the SQL the way sqlpolicy does, so the
// tests can observe which limit each statement was authorized with without
// depending on the policy engine's parser. The real engine is exercised through
// the Gateway's own tests.
type stubEngine struct {
	// authorizedRowLimit, when non-zero, is returned instead of the requested
	// limit, modelling the engine lowering it.
	authorizedRowLimit int64
	requests           []sqlpolicy.Request
	err                error
}

func (engine *stubEngine) Authorize(request sqlpolicy.Request) (sqlpolicy.Decision, error) {
	engine.requests = append(engine.requests, request)
	if engine.err != nil {
		return sqlpolicy.Decision{}, engine.err
	}
	limit := request.RowLimit
	if engine.authorizedRowLimit != 0 {
		limit = engine.authorizedRowLimit
	}
	return sqlpolicy.Decision{
		SQL:      fmt.Sprintf("%s LIMIT %d", request.SQL, limit),
		RowLimit: limit, Fingerprint: "stub",
	}, nil
}

func digester(sql string) (string, error) {
	// A structural digest ignores the rendered limit, which is what makes the
	// visible identity stable across budgets.
	structural, _, _ := strings.Cut(sql, " LIMIT ")
	return ExactDigest("STRUCTURAL:" + structural), nil
}

// A non-expanded exposure operation clamps the visible limit to the influence
// budget; the companion then settles against the AUTHORIZED visible limit.
func TestNonExpandedEvidenceClampsVisibleAndFollowsAuthorizedLimit(t *testing.T) {
	state := LedgerPreState{
		RemainingRows: 500, InfluenceFacts: 120,
		HasExposureContext: true, UsesExpandedEvidence: false,
	}
	if got := RequestedVisibleRowLimit(state); got != 120 {
		t.Fatalf("requested visible limit = %d, want the influence budget 120", got)
	}
	// The engine lowers it further; the companion must follow what was
	// authorized, not what was requested.
	engine := &stubEngine{authorizedRowLimit: 90}
	derivation, err := Derive(engine, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: state,
	})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if derivation.Limits.VisibleRowLimit != 90 {
		t.Fatalf("visible limit = %d, want the authorized 90", derivation.Limits.VisibleRowLimit)
	}
	if derivation.Limits.CompanionEvidenceRows != 90 || derivation.Limits.CompanionPolicyRows != 90 {
		t.Fatalf("companion limits = %+v, want 90/90 from the authorized visible limit", derivation.Limits)
	}
	if len(engine.requests) != 2 || engine.requests[1].RowLimit != 90 {
		t.Fatalf("companion was authorized with %+v", engine.requests)
	}
}

// An expanded-evidence operation settles the companion against the influence
// budget and asks for one extra row so a truncation is detectable.
func TestExpandedEvidenceUsesInfluenceBudgetPlusOne(t *testing.T) {
	state := LedgerPreState{
		RemainingRows: 500, InfluenceFacts: 120,
		HasExposureContext: true, UsesExpandedEvidence: true,
	}
	// Expanded evidence does NOT clamp the visible limit.
	if got := RequestedVisibleRowLimit(state); got != 500 {
		t.Fatalf("requested visible limit = %d, want the unclamped 500", got)
	}
	engine := &stubEngine{}
	derivation, err := Derive(engine, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: state,
	})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if derivation.Limits.CompanionEvidenceRows != 120 {
		t.Fatalf("companion evidence rows = %d, want the influence budget 120",
			derivation.Limits.CompanionEvidenceRows)
	}
	if derivation.Limits.CompanionPolicyRows != 121 {
		t.Fatalf("companion policy rows = %d, want 121 so a truncation is detectable",
			derivation.Limits.CompanionPolicyRows)
	}
}

// Proof obligation: an operation does not necessarily begin with an unused
// ledger. A partly-consumed pre-state must derive different statements, and the
// derivation must follow the pre-state rather than assume a fresh one.
func TestDerivationFollowsAPartlyConsumedLedger(t *testing.T) {
	fresh := LedgerPreState{RemainingRows: 500, InfluenceFacts: 400, HasExposureContext: true}
	consumed := LedgerPreState{RemainingRows: 37, InfluenceFacts: 400, HasExposureContext: true}

	freshDerivation, err := Derive(&stubEngine{}, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: fresh})
	if err != nil {
		t.Fatalf("derive fresh: %v", err)
	}
	consumedDerivation, err := Derive(&stubEngine{}, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: consumed})
	if err != nil {
		t.Fatalf("derive consumed: %v", err)
	}
	if consumedDerivation.Limits.VisibleRowLimit != 37 {
		t.Fatalf("consumed visible limit = %d, want the remaining 37",
			consumedDerivation.Limits.VisibleRowLimit)
	}
	if freshDerivation.Visible.ExactSHA256 == consumedDerivation.Visible.ExactSHA256 {
		t.Fatal("a partly-consumed ledger produced the same executable bytes as a fresh one")
	}
	// The structural identity must NOT move with the budget, or the observer
	// would misclassify a legitimate statement whenever the ledger differed.
	if freshDerivation.Visible.StrictASTSHA256 != consumedDerivation.Visible.StrictASTSHA256 {
		t.Fatal("the structural identity moved with the row limit")
	}
}

// An exhausted budget must refuse rather than derive a zero-row companion.
func TestExhaustedBudgetRefuses(t *testing.T) {
	state := LedgerPreState{RemainingRows: 0, InfluenceFacts: 0, HasExposureContext: true}
	_, err := Derive(&stubEngine{}, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: state})
	if !errors.Is(err, ErrExposureBudgetExhausted) {
		t.Fatalf("err = %v, want ErrExposureBudgetExhausted", err)
	}
}

// A plain query has no companion and no clamp.
func TestPlainQueryHasNoCompanion(t *testing.T) {
	state := LedgerPreState{RemainingRows: 250, InfluenceFacts: 10, HasExposureContext: false}
	derivation, err := Derive(&stubEngine{}, digester, Request{VisibleSQL: "SELECT visible", State: state})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if derivation.Companion != nil {
		t.Fatal("a plain query derived a companion statement")
	}
	if derivation.Limits.VisibleRowLimit != 250 {
		t.Fatalf("visible limit = %d, want the unclamped 250", derivation.Limits.VisibleRowLimit)
	}
}

func TestCompanionRequiresAnExposureContext(t *testing.T) {
	state := LedgerPreState{RemainingRows: 250, HasExposureContext: false}
	if _, err := Derive(&stubEngine{}, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: state}); err == nil {
		t.Fatal("a companion outside an exposure context was accepted")
	}
}

// The visible statement must be authorized before the companion, because the
// companion's limit depends on the authorized visible limit.
func TestVisibleIsAuthorizedBeforeCompanion(t *testing.T) {
	engine := &stubEngine{}
	state := LedgerPreState{RemainingRows: 500, InfluenceFacts: 400, HasExposureContext: true}
	if _, err := Derive(engine, digester, Request{
		VisibleSQL: "SELECT visible", CompanionSQL: "SELECT companion", State: state}); err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(engine.requests) != 2 {
		t.Fatalf("%d statements authorized, want 2", len(engine.requests))
	}
	if engine.requests[0].SQL != "SELECT visible" || engine.requests[1].SQL != "SELECT companion" {
		t.Fatalf("authorization order = %q then %q", engine.requests[0].SQL, engine.requests[1].SQL)
	}
}

// The exact digest must move with a constant-only change that the structural
// digest deliberately ignores.
func TestExactDigestSeparatesConstantOnlyChanges(t *testing.T) {
	if ExactDigest("SELECT 1") == ExactDigest("SELECT 2") {
		t.Fatal("the exact digest ignored a constant change")
	}
	left, _ := digester("SELECT x LIMIT 10")
	right, _ := digester("SELECT x LIMIT 99")
	if left != right {
		t.Fatal("the structural digest moved with the rendered limit")
	}
}

func TestDeriveRejectsAMissingAuthorizer(t *testing.T) {
	if _, err := Derive(nil, digester, Request{VisibleSQL: "SELECT visible"}); err == nil {
		t.Fatal("a nil authorizer was accepted")
	}
}

func TestAuthorizationFailurePropagates(t *testing.T) {
	engine := &stubEngine{err: errors.New("policy refusal")}
	if _, err := Derive(engine, digester, Request{
		VisibleSQL: "SELECT visible",
		State:      LedgerPreState{RemainingRows: 10}}); err == nil {
		t.Fatal("a policy refusal was swallowed")
	}
}
