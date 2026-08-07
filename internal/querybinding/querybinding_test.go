package querybinding

import (
	"encoding/json"
	"strings"
	"testing"
)

func digestOf(seed string) string { return strings.Repeat(seed, 64/len(seed)) }

func testLedger(t *testing.T) ExposureLedgerBeforeV1 {
	t.Helper()
	sealed, err := ExposureLedgerBeforeV1{
		ProfileVersion: "taskgate-exposure-v5",
		RootTaskID:     "task-root-1",
		RootEpoch:      3,
		Limits: FactVector{ReleaseFacts: 500, InfluenceFacts: 40, OutcomeFacts: 10,
			PredicateAtoms: 25, CompositeOutcomes: 5},
		Used: FactVector{ReleaseFacts: 100, InfluenceFacts: 8, OutcomeFacts: 2,
			PredicateAtoms: 5, CompositeOutcomes: 1},
		Remaining: FactVector{ReleaseFacts: 400, InfluenceFacts: 32, OutcomeFacts: 8,
			PredicateAtoms: 20, CompositeOutcomes: 4},
		RemainingRows:        1000,
		UsesExpandedEvidence: true,
		HasExposureContext:   true,
	}.Seal()
	if err != nil {
		t.Fatalf("seal exposure ledger pre-state: %v", err)
	}
	return sealed
}

func testTarget(role TargetRole, rowLimit int64, executed bool) TargetRecordV1 {
	seed := "a"
	if role == RoleCompanion {
		seed = "b"
	}
	return TargetRecordV1{
		Role: role, Authorized: true, Executed: executed,
		ExactSQLSHA256:              digestOf(seed + "1"),
		StrictASTSHA256:             digestOf(seed + "2"),
		RowLimit:                    rowLimit,
		PolicyFingerprint:           "fp-" + string(role),
		PolicyRendererVersion:       "sqlpolicy-v3",
		PolicyRendererDigest:        digestOf(seed + "3"),
		PreparedTargetBindingSHA256: digestOf(seed + "4"),
	}
}

func TestExposureLedgerBeforeSealsAndValidates(t *testing.T) {
	ledger := testLedger(t)
	if err := ledger.Validate(); err != nil {
		t.Fatalf("a well-formed pre-state was rejected: %v", err)
	}
	if len(ledger.SHA256) != 64 {
		t.Fatalf("the pre-state digest is %q", ledger.SHA256)
	}
}

// The three vectors must agree. Remaining is what a caller would have to forge
// to widen a row limit, so it must not be independently assertable.
func TestExposureLedgerBeforeRequiresRemainingToEqualLimitsMinusUsed(t *testing.T) {
	ledger := testLedger(t)
	ledger.Remaining.InfluenceFacts = 999
	if err := ledger.Validate(); err == nil {
		t.Fatal("a pre-state whose remaining vector contradicted its limits was accepted")
	}
	if _, err := ledger.Seal(); err == nil {
		t.Fatal("an incoherent pre-state sealed cleanly")
	}
}

func TestExposureLedgerBeforeFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*ExposureLedgerBeforeV1){
		"no profile":     func(l *ExposureLedgerBeforeV1) { l.ProfileVersion = "" },
		"no root task":   func(l *ExposureLedgerBeforeV1) { l.RootTaskID = "" },
		"negative epoch": func(l *ExposureLedgerBeforeV1) { l.RootEpoch = -1 },
		"negative rows":  func(l *ExposureLedgerBeforeV1) { l.RemainingRows = -1 },
		"negative limit": func(l *ExposureLedgerBeforeV1) { l.Limits.ReleaseFacts = -1 },
		"wrong version":  func(l *ExposureLedgerBeforeV1) { l.Version = "taskgate-exposure-ledger-before-v0" },
		"expanded without context": func(l *ExposureLedgerBeforeV1) {
			l.HasExposureContext = false
		},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := testLedger(t)
			mutate(&ledger)
			if err := ledger.Validate(); err == nil {
				t.Fatal("an invalid exposure pre-state validated")
			}
		})
	}
}

// The digest must cover every member, or a mutated pre-state would still verify.
func TestExposureLedgerBeforeDigestCoversEveryMember(t *testing.T) {
	base := testLedger(t)
	for name, mutate := range map[string]func(*ExposureLedgerBeforeV1){
		"profile":   func(l *ExposureLedgerBeforeV1) { l.ProfileVersion = "taskgate-exposure-v4" },
		"root task": func(l *ExposureLedgerBeforeV1) { l.RootTaskID = "task-root-2" },
		"epoch":     func(l *ExposureLedgerBeforeV1) { l.RootEpoch = 4 },
		"limits": func(l *ExposureLedgerBeforeV1) {
			l.Limits.InfluenceFacts, l.Remaining.InfluenceFacts = 41, 33
		},
		"used": func(l *ExposureLedgerBeforeV1) {
			l.Used.ReleaseFacts, l.Remaining.ReleaseFacts = 101, 399
		},
		"remaining rows": func(l *ExposureLedgerBeforeV1) { l.RemainingRows = 999 },
		"expanded flag":  func(l *ExposureLedgerBeforeV1) { l.UsesExpandedEvidence = false },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			// The digest is left at its original value on purpose: a member that
			// can change without changing the digest is a member the signature
			// does not protect.
			if err := mutated.Validate(); err == nil {
				t.Fatalf("mutating %s left the pre-state valid; the digest does not cover it", name)
			}
			resealed, err := mutated.Seal()
			if err != nil {
				t.Fatalf("reseal: %v", err)
			}
			if resealed.SHA256 == base.SHA256 {
				t.Fatalf("mutating %s did not change the pre-state digest", name)
			}
		})
	}
}

// A target executed without authorization is a contradiction, whichever binding
// carries it. The rule lives on the shared target record, so it is checked here
// rather than beside any one binding's own semantics.
func TestTargetCannotExecuteWithoutAuthorization(t *testing.T) {
	binding := pairedNovelV2(t).withCopiedCompanion()
	binding.Visible.Authorized = false
	if _, err := binding.Seal(); err == nil {
		t.Fatal("a target executed without authorization was accepted")
	}
}

// Nothing in either structure may carry SQL. The receipt is retained, replayed
// and handed to a finalizer that must not learn what was queried.
func TestNoStructureCarriesSQL(t *testing.T) {
	for name, value := range map[string]any{"binding": pairedNovelV2(t), "ledger": testLedger(t)} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		lowered := strings.ToLower(string(encoded))
		for _, fragment := range []string{"select ", " from ", " where ", "insert ", "update ", "--", "/*"} {
			if strings.Contains(lowered, fragment) {
				t.Fatalf("the %s carries the SQL fragment %q: %s", name, fragment, encoded)
			}
		}
		// The structures have no field that could hold a FactID, a bitmap member
		// or a task payload; this asserts the JSON shape stays that way.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		for field := range fields {
			for _, forbidden := range []string{"fact_id", "fact_ids", "bitmap", "members", "payload", "sql_text"} {
				if strings.Contains(field, forbidden) {
					t.Errorf("the %s exposes a %q field", name, field)
				}
			}
		}
	}
}

// The framing must be unambiguous: two different member sets must not be able to
// produce one digest by concatenation.
func TestCanonicalFramingCannotBeCollided(t *testing.T) {
	seal := func(profile, fingerprint string) QueryExecutionBindingV2 {
		t.Helper()
		binding := pairedNovelV2(t).withCopiedCompanion()
		binding.ExposureProfileVersion = profile
		binding.Visible.PolicyFingerprint = fingerprint
		sealed, err := binding.Seal()
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	if seal("ab", "cd").Equal(seal("a", "bcd")) {
		t.Fatal("two distinct bindings collided; the framing is ambiguous")
	}
}
