package experiment

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ObserverAccountingVersion identifies the closed-world statement accounting
// rule. Version 1 required the out-of-process observer's total gateway_reader
// delta to equal the targeted visible/companion counters. That equality can
// never hold on a governed deployment: the Connector re-establishes the
// controls that make a read attributable before it touches data, so the total
// always exceeds the targeted count by those controls. Version 2 replaces the
// equality with an accounting -- every statement the observer counted is
// assigned to exactly one class, and every class is compared with a
// multiplicity derived from the activated profile.
const ObserverAccountingVersion = "taskgate-final-v5-observer-accounting-v2"

// GatewayStatementClass names one class of gateway_reader statement. The set is
// closed: a statement that matches no control template and neither targeted
// relation is StatementUnexpected, which no plan ever expects.
type GatewayStatementClass string

const (
	// Targeted classes are the Product reads the experiment is measuring.
	StatementTargetedVisible   GatewayStatementClass = "targeted_visible"
	StatementTargetedCompanion GatewayStatementClass = "targeted_companion"

	// Control classes are what internal/dataconnector.Connector.Query issues
	// around each governed statement. docs/final_v5_observer_statement_accounting.md
	// derives these from the frozen execution path.
	StatementTransactionBegin          GatewayStatementClass = "transaction_begin"
	StatementStatementTimeoutPin       GatewayStatementClass = "statement_timeout_pin"
	StatementSessionPins               GatewayStatementClass = "session_pins"
	StatementDatasourceIdentity        GatewayStatementClass = "datasource_identity"
	StatementViewColumnAttestation     GatewayStatementClass = "view_column_attestation"
	StatementViewDefinitionAttestation GatewayStatementClass = "view_definition_attestation"
	StatementTransactionCommit         GatewayStatementClass = "transaction_commit"

	// StatementUnexpected is the fail-closed sink. A profile that reaches
	// Business PostgreSQL by a path this classifier does not model lands here
	// and fails the accounting rather than being absorbed into a control class.
	StatementUnexpected GatewayStatementClass = "unexpected"
)

// fixedControlsPerTransaction is the number of control statements every
// governed transaction issues regardless of how many reporting views the
// profile declares: begin, statement-timeout pin, session pins, datasource
// identity attestation, commit.
const fixedControlsPerTransaction = 5

// perViewControlsPerTransaction is the number of control statements each
// declared reporting view adds to every governed transaction: one column
// attestation and one view-definition attestation.
const perViewControlsPerTransaction = 2

// GatewayStatementClasses returns the closed world in canonical order. The
// order is fixed so that a per-class comparison always reports the same first
// offending class for the same evidence.
func GatewayStatementClasses() []GatewayStatementClass {
	return []GatewayStatementClass{
		StatementTargetedVisible,
		StatementTargetedCompanion,
		StatementTransactionBegin,
		StatementStatementTimeoutPin,
		StatementSessionPins,
		StatementDatasourceIdentity,
		StatementViewColumnAttestation,
		StatementViewDefinitionAttestation,
		StatementTransactionCommit,
		StatementUnexpected,
	}
}

// canonicalStatementText puts a pg_stat_statements query template into the one
// form the classifier matches against: lower case, no double quotes, and every
// whitespace run collapsed to a single space. pg_stat_statements has already
// replaced constants with $n placeholders, so no literal from the workload --
// and therefore no business value -- survives into the text this sees.
func canonicalStatementText(query string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(query, `"`, ""))), " ")
}

// ClassifyGatewayStatement assigns one normalized pg_stat_statements template to
// exactly one class.
//
// The control templates are tested before the targeted relations because the
// control set is closed and known, whereas the targeted relations are matched by
// substring: testing controls first means a control statement can never be
// mistaken for a Product read. Within the targeted relations the companion is
// tested before the visible relation, mirroring the SQL that produces the
// independent VisibleCalls/CompanionCalls counters, so the two derivations of
// the targeted counts cannot disagree.
func ClassifyGatewayStatement(query, visibleRelation, companionRelation string) (GatewayStatementClass, error) {
	visible := canonicalStatementText(visibleRelation)
	companion := canonicalStatementText(companionRelation)
	if visible == "" || companion == "" || visible == companion {
		return "", errors.New("statement classification requires distinct visible and companion relations")
	}
	text := canonicalStatementText(query)
	if text == "" {
		return "", errors.New("statement classification requires a non-empty query template")
	}
	switch {
	case text == "begin" || strings.HasPrefix(text, "begin "):
		return StatementTransactionBegin, nil
	case text == "commit" || strings.HasPrefix(text, "commit "):
		return StatementTransactionCommit, nil
	// The view-definition attestation also pins search_path through set_config,
	// so it must be recognised before the set_config templates below.
	case strings.Contains(text, "pg_get_viewdef"):
		return StatementViewDefinitionAttestation, nil
	case strings.Contains(text, "pg_attribute") && strings.Contains(text, "pg_namespace"):
		return StatementViewColumnAttestation, nil
	case strings.Contains(text, "datasource_attestation"):
		return StatementDatasourceIdentity, nil
	case strings.Contains(text, "set_config"):
		// pg_stat_statements normalizes the setting names to placeholders, so
		// the two pins are told apart by arity rather than by name: the timeout
		// pin sets one GUC, the session pin sets two in one statement.
		switch strings.Count(text, "set_config") {
		case 1:
			return StatementStatementTimeoutPin, nil
		case 2:
			return StatementSessionPins, nil
		default:
			return StatementUnexpected, nil
		}
	case strings.Contains(text, companion):
		return StatementTargetedCompanion, nil
	case strings.Contains(text, visible):
		return StatementTargetedVisible, nil
	default:
		return StatementUnexpected, nil
	}
}

// GatewayStatementCensus is a complete per-class decomposition of the
// gateway_reader statements pg_stat_statements had recorded at one instant.
// Only counts are retained: the templates themselves are classified in process
// and never enter the evidence.
type GatewayStatementCensus struct {
	Version string                          `json:"version"`
	Counts  map[GatewayStatementClass]int64 `json:"counts"`
}

// NewGatewayStatementCensus returns a census with every class present at zero,
// so that an absent class is always an explicit zero rather than a missing key.
func NewGatewayStatementCensus() GatewayStatementCensus {
	counts := make(map[GatewayStatementClass]int64, len(GatewayStatementClasses()))
	for _, class := range GatewayStatementClasses() {
		counts[class] = 0
	}
	return GatewayStatementCensus{Version: ObserverAccountingVersion, Counts: counts}
}

// Validate rejects a census whose key set is not exactly the closed world. A
// census missing a class would let that class's statements go uncounted, which
// is the failure this accounting exists to prevent.
func (census GatewayStatementCensus) Validate() error {
	if census.Version != ObserverAccountingVersion {
		return fmt.Errorf("statement census version %q is unsupported", census.Version)
	}
	if len(census.Counts) != len(GatewayStatementClasses()) {
		return errors.New("statement census does not cover exactly the closed class set")
	}
	for _, class := range GatewayStatementClasses() {
		count, present := census.Counts[class]
		if !present {
			return fmt.Errorf("statement census omits class %s", class)
		}
		if count < 0 {
			return fmt.Errorf("statement census class %s is negative", class)
		}
	}
	return nil
}

// Total is the number of gateway_reader statements the census accounts for.
func (census GatewayStatementCensus) Total() int64 {
	var total int64
	for _, class := range GatewayStatementClasses() {
		total += census.Counts[class]
	}
	return total
}

// GatewayControlPlan is the derived expectation for one governed operation. It
// is computed from the activated profile, never copied from an observation:
// GovernedTransactions and ReportingViews are the two profile-dependent
// quantities the control multiplicities are a function of.
type GatewayControlPlan struct {
	Version string `json:"version"`
	// GovernedTransactions is how many read-only repeatable-read transactions
	// the operation settles. A governed artifact query settles two: the visible
	// statement and its provenance companion, each in its own transaction.
	GovernedTransactions int64 `json:"governed_transactions"`
	// ReportingViews is how many reporting views the activated profile's Catalog
	// declares; each one adds a column attestation and a view-definition
	// attestation to every governed transaction.
	ReportingViews         int64 `json:"reporting_views"`
	ExpectedVisibleCalls   int64 `json:"expected_visible_calls"`
	ExpectedCompanionCalls int64 `json:"expected_companion_calls"`
}

// NewGatewayControlPlan builds a plan from the profile-dependent quantities.
func NewGatewayControlPlan(governedTransactions, reportingViews, visibleCalls, companionCalls int64) GatewayControlPlan {
	return GatewayControlPlan{
		Version: ObserverAccountingVersion, GovernedTransactions: governedTransactions,
		ReportingViews: reportingViews, ExpectedVisibleCalls: visibleCalls,
		ExpectedCompanionCalls: companionCalls,
	}
}

// Validate rejects a plan that is not a usable derivation.
//
// A plan is coherent in exactly two shapes. A governed plan settles at least one
// transaction and expects at least one visible and one companion statement. A
// replay plan settles none and expects none: a served-from-cache result must not
// reach Business PostgreSQL at all, and saying so as a plan means the accounting
// still checks it -- every class expects zero, so a single statement fails.
func (plan GatewayControlPlan) Validate() error {
	if plan.Version != ObserverAccountingVersion {
		return fmt.Errorf("gateway control plan version %q is unsupported", plan.Version)
	}
	// The reporting-view count is a property of the deployment's Catalog, so it
	// is required even by a replay plan that multiplies it by zero transactions.
	if plan.ReportingViews <= 0 {
		return errors.New("gateway control plan must declare at least one reporting view")
	}
	if plan.GovernedTransactions < 0 || plan.ExpectedVisibleCalls < 0 || plan.ExpectedCompanionCalls < 0 {
		return errors.New("gateway control plan multiplicities must not be negative")
	}
	switch {
	case plan.GovernedTransactions == 0:
		if plan.ExpectedVisibleCalls != 0 || plan.ExpectedCompanionCalls != 0 {
			return errors.New("gateway control plan settles no transaction but expects targeted statements")
		}
	case plan.ExpectedVisibleCalls <= 0 || plan.ExpectedCompanionCalls <= 0:
		return errors.New("a governed gateway control plan must expect at least one visible and one companion statement")
	}
	// Each targeted statement is settled by its own Connector.Query call, and
	// each of those opens exactly one transaction, so a plan that claims more
	// transactions than targeted statements is not derivable from the path.
	if plan.GovernedTransactions != plan.ExpectedVisibleCalls+plan.ExpectedCompanionCalls {
		return fmt.Errorf("gateway control plan settles %d transactions for %d targeted statements",
			plan.GovernedTransactions, plan.ExpectedVisibleCalls+plan.ExpectedCompanionCalls)
	}
	return nil
}

// RequiredGatewayControls is the derived control total:
//
//	T * (5 + 2 * N)
//
// for T governed transactions and N declared reporting views. It is deliberately
// a function of the activated profile: a one-view profile settling two
// transactions derives 14, a two-view profile derives 18, and hard-coding either
// would be wrong for every other profile.
func (plan GatewayControlPlan) RequiredGatewayControls() int64 {
	return plan.GovernedTransactions * (fixedControlsPerTransaction + perViewControlsPerTransaction*plan.ReportingViews)
}

// Expected is the per-class multiplicity the plan requires. Checking each class
// separately is what makes this accounting strictly stronger than the total it
// replaces: a substitution that preserves the total -- one fewer attestation and
// one more unknown statement -- still fails.
func (plan GatewayControlPlan) Expected() map[GatewayStatementClass]int64 {
	transactions, views := plan.GovernedTransactions, plan.ReportingViews
	return map[GatewayStatementClass]int64{
		StatementTargetedVisible:           plan.ExpectedVisibleCalls,
		StatementTargetedCompanion:         plan.ExpectedCompanionCalls,
		StatementTransactionBegin:          transactions,
		StatementStatementTimeoutPin:       transactions,
		StatementSessionPins:               transactions,
		StatementDatasourceIdentity:        transactions,
		StatementViewColumnAttestation:     transactions * views,
		StatementViewDefinitionAttestation: transactions * views,
		StatementTransactionCommit:         transactions,
		StatementUnexpected:                0,
	}
}

// ExpectedTotal is the whole closed-world expectation: the targeted statements
// the experiment is measuring plus the controls that make them attributable.
func (plan GatewayControlPlan) ExpectedTotal() int64 {
	return plan.ExpectedVisibleCalls + plan.ExpectedCompanionCalls + plan.RequiredGatewayControls()
}

// ObserverAccounting is the typed evidence record. It carries the derivation
// (Plan), the two in-process censuses that decompose the statements, and the
// total the independent out-of-process observer measured over the same window.
//
// The two sources are deliberately different mechanisms: the census is taken by
// the Adapter over pg_stat_statements, while ObserverTotalDelta comes from the
// source-built, digest-bound observer binary. Requiring the census to sum to the
// observer's total means a census that misclassifies or silently drops a
// statement cannot pass -- the decomposition has to explain the number an
// independent process measured.
type ObserverAccounting struct {
	Version            string                 `json:"version"`
	Plan               GatewayControlPlan     `json:"plan"`
	Before             GatewayStatementCensus `json:"before"`
	After              GatewayStatementCensus `json:"after"`
	ObserverTotalDelta int64                  `json:"observer_total_delta"`
}

// Delta is the per-class census of the window between the two snapshots.
func (accounting ObserverAccounting) Delta() (GatewayStatementCensus, error) {
	if err := accounting.Before.Validate(); err != nil {
		return GatewayStatementCensus{}, err
	}
	if err := accounting.After.Validate(); err != nil {
		return GatewayStatementCensus{}, err
	}
	delta := NewGatewayStatementCensus()
	for _, class := range GatewayStatementClasses() {
		difference := accounting.After.Counts[class] - accounting.Before.Counts[class]
		if difference < 0 {
			return GatewayStatementCensus{}, fmt.Errorf("statement census class %s regressed between snapshots", class)
		}
		delta.Counts[class] = difference
	}
	return delta, nil
}

// ValidateObserverAccounting is the fail-closed gate. Every check names what it
// rejected, because a bare "accounting failed" would leave a reviewer unable to
// tell a missing control from an unmodelled statement.
func ValidateObserverAccounting(accounting ObserverAccounting) error {
	if accounting.Version != ObserverAccountingVersion {
		return fmt.Errorf("observer accounting version %q is unsupported", accounting.Version)
	}
	if err := accounting.Plan.Validate(); err != nil {
		return err
	}
	delta, err := accounting.Delta()
	if err != nil {
		return err
	}
	if count := delta.Counts[StatementUnexpected]; count != 0 {
		return fmt.Errorf("gateway statement class %s observed %d: that many gateway_reader statements matched "+
			"no modelled control and neither targeted relation", StatementUnexpected, count)
	}
	expected := accounting.Plan.Expected()
	for _, class := range GatewayStatementClasses() {
		if delta.Counts[class] != expected[class] {
			return fmt.Errorf("gateway statement class %s observed %d, derived expectation %d",
				class, delta.Counts[class], expected[class])
		}
	}
	// The closing identity. The per-class check above already pins every class,
	// so this can only fail if the census and the independent observer disagree
	// about how many statements the window contained at all.
	if delta.Total() != accounting.ObserverTotalDelta {
		return fmt.Errorf("classified %d gateway_reader statements but the independent observer counted %d",
			delta.Total(), accounting.ObserverTotalDelta)
	}
	if accounting.Plan.ExpectedTotal() != accounting.ObserverTotalDelta {
		return fmt.Errorf("derived total %d differs from the independent observer total %d",
			accounting.Plan.ExpectedTotal(), accounting.ObserverTotalDelta)
	}
	return nil
}

// CensusFromTemplates classifies a whole pg_stat_statements reading into a
// census. It is the single place a raw template set becomes counts, so the
// Adapter call sites cannot each grow their own classification rule.
func CensusFromTemplates(templates map[string]int64, visibleRelation, companionRelation string) (GatewayStatementCensus, error) {
	census := NewGatewayStatementCensus()
	// Sorted so that a classification error is reported for the same template
	// every time the same reading is classified.
	names := make([]string, 0, len(templates))
	for template := range templates {
		names = append(names, template)
	}
	sort.Strings(names)
	for _, template := range names {
		calls := templates[template]
		if calls < 0 {
			return GatewayStatementCensus{}, errors.New("pg_stat_statements returned a negative call count")
		}
		class, err := ClassifyGatewayStatement(template, visibleRelation, companionRelation)
		if err != nil {
			return GatewayStatementCensus{}, err
		}
		census.Counts[class] += calls
	}
	return census, nil
}
