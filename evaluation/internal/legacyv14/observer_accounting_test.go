package legacyv14

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Result-heavy artifact cell is the worked example the derivation in
// docs/final_v5_observer_statement_accounting.md settles: one declared reporting
// view, two governed transactions, one visible and one companion statement.
// Every count below is derived from those four numbers, never copied from an
// observed run.
func resultHeavyPlan() GatewayControlPlan { return NewGatewayControlPlan(2, 1, 1, 1) }

// resultHeavyAccounting is a valid record: a before census, an after census
// whose per-class delta is exactly the derived expectation, and an independent
// observer total that agrees with the census.
func resultHeavyAccounting() ObserverAccounting {
	plan := resultHeavyPlan()
	before := NewGatewayStatementCensus()
	// A non-zero baseline: pg_stat_statements is cumulative, so the accounting
	// must work off the delta rather than off the after census.
	for _, class := range GatewayStatementClasses() {
		before.Counts[class] = 7
	}
	after := NewGatewayStatementCensus()
	expected := plan.Expected()
	for _, class := range GatewayStatementClasses() {
		after.Counts[class] = before.Counts[class] + expected[class]
	}
	return ObserverAccounting{Version: ObserverAccountingVersion, Plan: plan,
		Before: before, After: after, ObserverTotalDelta: plan.ExpectedTotal()}
}

func TestResultHeavyAccountingClosesTheBooks(t *testing.T) {
	plan := resultHeavyPlan()
	if got := plan.RequiredGatewayControls(); got != 14 {
		t.Fatalf("required controls = %d, want 14 from 2 * (5 + 2 * 1)", got)
	}
	if got := plan.ExpectedTotal(); got != 16 {
		t.Fatalf("expected total = %d, want 16 = 2 targeted + 14 controls", got)
	}
	if err := ValidateObserverAccounting(resultHeavyAccounting()); err != nil {
		t.Fatalf("the derived Result-heavy accounting was rejected: %v", err)
	}
}

func TestArchivedAccountingDecoderIsStrict(t *testing.T) {
	encoded, err := json.Marshal(resultHeavyAccounting())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeObserverAccounting(encoded); err != nil {
		t.Fatalf("the archived v1.4 accounting did not decode: %v", err)
	}
	var hybrid map[string]any
	if err := json.Unmarshal(encoded, &hybrid); err != nil {
		t.Fatal(err)
	}
	hybrid["taskgate_acceptance_v3"] = map[string]any{}
	encoded, _ = json.Marshal(hybrid)
	if _, err := DecodeObserverAccounting(encoded); err == nil {
		t.Fatal("the legacy decoder accepted a current v3 acceptance member")
	}
}

// The 14 is a function of the activated profile, not a constant. A profile
// declaring two reporting views derives 18 controls and a total of 20, so an
// implementation that hard-coded the Result-heavy number would fail here.
func TestControlMultiplicityFollowsTheProfileRatherThanAConstant(t *testing.T) {
	for _, testCase := range []struct {
		name                       string
		transactions, views        int64
		wantControls, wantTotal    int64
		visibleCalls, companionSet int64
	}{
		{"one view, two transactions", 2, 1, 14, 16, 1, 1},
		{"two views, two transactions", 2, 2, 18, 20, 1, 1},
		{"five views, two transactions", 2, 5, 30, 32, 1, 1},
		{"two views, four transactions", 4, 2, 36, 40, 2, 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := NewGatewayControlPlan(testCase.transactions, testCase.views,
				testCase.visibleCalls, testCase.companionSet)
			if err := plan.Validate(); err != nil {
				t.Fatalf("plan rejected: %v", err)
			}
			if got := plan.RequiredGatewayControls(); got != testCase.wantControls {
				t.Fatalf("required controls = %d, want %d", got, testCase.wantControls)
			}
			if got := plan.ExpectedTotal(); got != testCase.wantTotal {
				t.Fatalf("expected total = %d, want %d", got, testCase.wantTotal)
			}
		})
	}
}

// A replay is served from cache and must not reach Business PostgreSQL at all.
// Saying so as a plan keeps it checked: every class expects zero, so one stray
// statement fails rather than passing unnoticed.
func TestReplayPlanForbidsEveryBusinessStatement(t *testing.T) {
	plan := NewGatewayControlPlan(0, 1, 0, 0)
	if err := plan.Validate(); err != nil {
		t.Fatalf("replay plan rejected: %v", err)
	}
	if got := plan.ExpectedTotal(); got != 0 {
		t.Fatalf("replay expected total = %d, want 0", got)
	}
	census := NewGatewayStatementCensus()
	accounting := ObserverAccounting{Version: ObserverAccountingVersion, Plan: plan,
		Before: census, After: census, ObserverTotalDelta: 0}
	if err := ValidateObserverAccounting(accounting); err != nil {
		t.Fatalf("a replay that issued no statement was rejected: %v", err)
	}
	accounting.After = NewGatewayStatementCensus()
	accounting.After.Counts[StatementTransactionBegin] = 1
	accounting.ObserverTotalDelta = 1
	if err := ValidateObserverAccounting(accounting); err == nil {
		t.Fatal("a replay that opened a governed transaction was accepted")
	}
}

// TestObserverAccountingRejectsEveryMutation is the core of the v2 gate. Each
// case mutates one valid record in one way; every one must be rejected.
//
// The two substitution cases are the point of per-class accounting: they keep
// the total at 16, so a rule that only compared totals -- including the one this
// replaces -- would accept them.
func TestObserverAccountingRejectsEveryMutation(t *testing.T) {
	for _, mutation := range []struct {
		name  string
		apply func(*ObserverAccounting)
	}{
		{"record version cleared", func(a *ObserverAccounting) { a.Version = "" }},
		{"record version downgraded to v1", func(a *ObserverAccounting) {
			a.Version = "taskgate-final-v5-observer-accounting-v1"
		}},
		{"plan version cleared", func(a *ObserverAccounting) { a.Plan.Version = "" }},
		{"plan declares no reporting view", func(a *ObserverAccounting) { a.Plan.ReportingViews = 0 }},
		{"plan claims more transactions than targeted statements", func(a *ObserverAccounting) {
			a.Plan.GovernedTransactions = 3
		}},
		{"plan claims fewer transactions than targeted statements", func(a *ObserverAccounting) {
			a.Plan.GovernedTransactions = 1
		}},
		{"governed plan expects no companion statement", func(a *ObserverAccounting) {
			a.Plan.ExpectedCompanionCalls = 0
		}},
		{"plan carries a negative multiplicity", func(a *ObserverAccounting) { a.Plan.ReportingViews = -1 }},
		{"before census version cleared", func(a *ObserverAccounting) { a.Before.Version = "" }},
		{"after census omits a class", func(a *ObserverAccounting) {
			delete(a.After.Counts, StatementViewDefinitionAttestation)
		}},
		{"after census carries a negative count", func(a *ObserverAccounting) {
			a.After.Counts[StatementUnexpected] = -1
		}},
		{"census regressed between snapshots", func(a *ObserverAccounting) {
			a.After.Counts[StatementTransactionCommit] = a.Before.Counts[StatementTransactionCommit] - 1
		}},
		{"one control statement is missing", func(a *ObserverAccounting) {
			a.After.Counts[StatementTransactionBegin]--
			a.ObserverTotalDelta--
		}},
		{"one extra control statement appeared", func(a *ObserverAccounting) {
			a.After.Counts[StatementDatasourceIdentity]++
			a.ObserverTotalDelta++
		}},
		{"an unmodelled statement reached Business PostgreSQL", func(a *ObserverAccounting) {
			a.After.Counts[StatementUnexpected]++
			a.ObserverTotalDelta++
		}},
		// Same-total substitution: one attestation is replaced by one statement
		// the closed world does not model. The total is still 16.
		{"attestation substituted by an unmodelled statement at the same total", func(a *ObserverAccounting) {
			a.After.Counts[StatementViewDefinitionAttestation]--
			a.After.Counts[StatementUnexpected]++
		}},
		// Same-total substitution between two control classes: a transaction was
		// committed without being opened. The total is still 16.
		{"a begin substituted by a commit at the same total", func(a *ObserverAccounting) {
			a.After.Counts[StatementTransactionBegin]--
			a.After.Counts[StatementTransactionCommit]++
		}},
		{"the independent observer counted more than the census explains", func(a *ObserverAccounting) {
			a.ObserverTotalDelta++
		}},
		{"the independent observer counted fewer than the census explains", func(a *ObserverAccounting) {
			a.ObserverTotalDelta--
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			accounting := resultHeavyAccounting()
			mutation.apply(&accounting)
			if err := ValidateObserverAccounting(accounting); err == nil {
				t.Fatalf("mutation %q was accepted", mutation.name)
			}
		})
	}
}

// A same-total substitution must be rejected for naming the substituted class,
// not merely rejected: a reviewer has to be able to tell a missing control from
// an unmodelled statement.
func TestSameTotalSubstitutionIsReportedByClass(t *testing.T) {
	accounting := resultHeavyAccounting()
	accounting.After.Counts[StatementViewColumnAttestation]--
	accounting.After.Counts[StatementUnexpected]++
	delta, err := accounting.Delta()
	if err != nil {
		t.Fatal(err)
	}
	if delta.Total() != accounting.ObserverTotalDelta {
		t.Fatalf("the substitution changed the total to %d; it must stay at %d to be a real test",
			delta.Total(), accounting.ObserverTotalDelta)
	}
	err = ValidateObserverAccounting(accounting)
	if err == nil {
		t.Fatal("a same-total substitution was accepted")
	}
	if !strings.Contains(err.Error(), string(StatementUnexpected)) {
		t.Fatalf("rejection did not name the unmodelled statements: %v", err)
	}
}

// The classifier is the fail-closed half of the accounting: a template it does
// not recognise must land in StatementUnexpected rather than in whichever
// control class it superficially resembles.
func TestClassifyGatewayStatementAssignsEachControlTemplate(t *testing.T) {
	const visible = "reporting.expense_detail"
	const companion = "taskgate_ordinal.expense_detail_v1"
	for _, testCase := range []struct {
		name  string
		query string
		want  GatewayStatementClass
	}{
		{"transaction begin", "begin", StatementTransactionBegin},
		{"transaction begin with isolation options",
			"begin isolation level repeatable read read only", StatementTransactionBegin},
		{"transaction commit", "commit", StatementTransactionCommit},
		{"statement timeout pin",
			"SELECT pg_catalog.set_config($1, $2, $3)", StatementStatementTimeoutPin},
		{"session pins set two settings in one statement",
			"SELECT pg_catalog.set_config($1, $2, $3), pg_catalog.set_config($4, $5, $6)", StatementSessionPins},
		{"datasource identity attestation",
			`SELECT COALESCE((SELECT datasource_id FROM reporting.datasource_attestation WHERE singleton = $1), $2),
			 current_database(), current_user, current_setting($3)`, StatementDatasourceIdentity},
		{"reporting-view column attestation",
			`SELECT attr.attname FROM pg_namespace AS ns JOIN pg_class AS cls ON cls.relnamespace = ns.oid
			 JOIN pg_attribute AS attr ON attr.attrelid = cls.oid ORDER BY attr.attnum`,
			StatementViewColumnAttestation},
		// The view-definition attestation also pins search_path through
		// set_config, so a classifier that tested set_config first would
		// misfile it as a session pin.
		{"view-definition attestation despite its embedded set_config",
			`WITH taskgate_schema_digest_path AS (SELECT set_config($1, $2, $3))
			 SELECT pg_get_viewdef(format($4, $5::text, $6::text)::regclass, $7) FROM taskgate_schema_digest_path`,
			StatementViewDefinitionAttestation},
		{"targeted visible statement",
			`SELECT receipt_no, department FROM reporting.expense_detail WHERE department = ANY ($1)`,
			StatementTargetedVisible},
		{"targeted companion statement",
			`SELECT ordinal FROM taskgate_ordinal.expense_detail_v1 WHERE id = ANY ($1)`,
			StatementTargetedCompanion},
		// The provenance companion joins the visible relation too. The companion
		// wins, mirroring the SQL that produces the independent counters.
		{"companion joined to the visible relation counts as companion",
			`SELECT o.ordinal FROM taskgate_ordinal.expense_detail_v1 AS o
			 JOIN reporting.expense_detail AS e ON e.id = o.id`,
			StatementTargetedCompanion},
		{"an unmodelled read of another relation", "SELECT * FROM reporting.salary", StatementUnexpected},
		{"an unmodelled catalog probe", "SELECT current_setting($1)", StatementUnexpected},
		{"quoted and re-cased text is canonicalized",
			`BEGIN`, StatementTransactionBegin},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			class, err := ClassifyGatewayStatement(testCase.query, visible, companion)
			if err != nil {
				t.Fatalf("classification failed: %v", err)
			}
			if class != testCase.want {
				t.Fatalf("classified as %s, want %s", class, testCase.want)
			}
		})
	}
}

func TestClassifyGatewayStatementRejectsUnusableInputs(t *testing.T) {
	const visible = "reporting.expense_detail"
	const companion = "taskgate_ordinal.expense_detail_v1"
	for _, testCase := range []struct{ name, query, visible, companion string }{
		{"empty template", "   ", visible, companion},
		{"absent visible relation", "begin", "", companion},
		{"absent companion relation", "begin", visible, ""},
		{"visible and companion are the same relation", "begin", visible, visible},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ClassifyGatewayStatement(testCase.query, testCase.visible, testCase.companion); err == nil {
				t.Fatal("an unusable classification input was accepted")
			}
		})
	}
}

// CensusFromTemplates is where a whole pg_stat_statements reading becomes
// counts. A reading of the Result-heavy cell must reproduce the derived
// expectation exactly, including the zero in the unexpected class.
func TestCensusFromTemplatesReproducesTheDerivedResultHeavyCell(t *testing.T) {
	const visible = "reporting.expense_detail"
	const companion = "taskgate_ordinal.expense_detail_v1"
	// Two governed transactions over a one-view profile: every control template
	// was executed twice, and each targeted statement once.
	templates := map[string]int64{
		"begin":  2,
		"commit": 2,
		"select pg_catalog.set_config($1, $2, $3)":                                                                                     2,
		"select pg_catalog.set_config($1, $2, $3), pg_catalog.set_config($4,$5,$6)":                                                    2,
		"select coalesce((select datasource_id from reporting.datasource_attestation where singleton = $1), $2), current_database()":   2,
		"select attr.attname from pg_namespace as ns join pg_attribute as attr on attr.attrelid = cls.oid":                             2,
		"with taskgate_schema_digest_path as (select set_config($1,$2,$3)) select pg_get_viewdef($4) from taskgate_schema_digest_path": 2,
		"select receipt_no from reporting.expense_detail where department = any ($1)":                                                  1,
		"select ordinal from taskgate_ordinal.expense_detail_v1":                                                                       1,
	}
	census, err := CensusFromTemplates(templates, visible, companion)
	if err != nil {
		t.Fatalf("census failed: %v", err)
	}
	if census.Total() != 16 {
		t.Fatalf("census total = %d, want 16", census.Total())
	}
	for class, want := range resultHeavyPlan().Expected() {
		if census.Counts[class] != want {
			t.Fatalf("class %s counted %d, derived expectation %d", class, census.Counts[class], want)
		}
	}
}

// A reading containing a statement the closed world does not model must produce
// a non-zero unexpected count, which then fails the accounting. This is the
// property that makes the classifier fail closed rather than silently absorbing
// an unknown read into a control class.
func TestCensusFromTemplatesSendsUnknownStatementsToUnexpected(t *testing.T) {
	census, err := CensusFromTemplates(map[string]int64{
		"begin":                          1,
		"select * from reporting.salary": 1,
	}, "reporting.expense_detail", "taskgate_ordinal.expense_detail_v1")
	if err != nil {
		t.Fatalf("census failed: %v", err)
	}
	if census.Counts[StatementUnexpected] != 1 {
		t.Fatalf("unexpected class counted %d, want 1", census.Counts[StatementUnexpected])
	}
}

func TestCensusFromTemplatesRejectsANegativeCallCount(t *testing.T) {
	if _, err := CensusFromTemplates(map[string]int64{"begin": -1},
		"reporting.expense_detail", "taskgate_ordinal.expense_detail_v1"); err == nil {
		t.Fatal("a negative pg_stat_statements call count was accepted")
	}
}
