package experiment

import (
	"strings"
	"testing"
)

const testSchemaDigest = "1111111111111111111111111111111111111111111111111111111111111111"

// The Result-heavy novel paired path is the case Stage B measured live. The
// plan must reproduce the observed 16 exactly, class by class, from the phase
// dimensions alone.
func TestPairedNovelPlanReproducesTheMeasuredSixteen(t *testing.T) {
	plan := PairedNovelPlanV3(1, testSchemaDigest)
	if err := plan.Validate(); err != nil {
		t.Fatalf("paired novel plan rejected: %v", err)
	}
	expected := plan.Expected()
	for class, want := range map[GatewayStatementClassV3]int64{
		V3TransactionBegin: 1, V3TransactionCommit: 1,
		V3SafetySessionPin: 1, V3RepresentationPin: 1,
		V3StatementTimeoutPin: 2, V3DatasourceIdentity: 2,
		V3ViewColumnAttestation: 2, V3ViewDefinitionAttest: 2,
		V3NestedViewdefRewrite: 2,
		V3TargetedVisible:      1, V3TargetedCompanion: 1,
		V3Unexpected: 0,
	} {
		if expected[class] != want {
			t.Errorf("class %s = %d, want %d", class, expected[class], want)
		}
	}
	if got := plan.ExpectedTotal(); got != 16 {
		t.Fatalf("expected total = %d, want the 16 Stage B measured", got)
	}
}

// A semantic replay is served from cache, but the cache lookup happens after
// datasourceEvidence, so the preflight attestation still runs. v2 modelled this
// as all-zero and would have rejected a correct replay.
func TestSemanticReplayPlanExpectsFourNotZero(t *testing.T) {
	plan := SemanticReplayPlanV3(1, testSchemaDigest)
	if err := plan.Validate(); err != nil {
		t.Fatalf("semantic replay plan rejected: %v", err)
	}
	expected := plan.Expected()
	for class, want := range map[GatewayStatementClassV3]int64{
		V3DatasourceIdentity: 1, V3ViewColumnAttestation: 1,
		V3ViewDefinitionAttest: 1, V3NestedViewdefRewrite: 1,
	} {
		if expected[class] != want {
			t.Errorf("class %s = %d, want %d", class, expected[class], want)
		}
	}
	// It must open no transaction, pin nothing and execute no target statement.
	for _, class := range []GatewayStatementClassV3{
		V3TransactionBegin, V3TransactionCommit, V3SafetySessionPin,
		V3RepresentationPin, V3StatementTimeoutPin,
		V3TargetedVisible, V3TargetedCompanion,
	} {
		if expected[class] != 0 {
			t.Errorf("semantic replay expects %d of class %s, want 0", expected[class], class)
		}
	}
	if got := plan.ExpectedTotal(); got != 4 {
		t.Fatalf("semantic replay total = %d, want 4 for one ExpectedSchema entry", got)
	}
}

// An exact idempotent replay returns before datasourceEvidence and must touch
// Business PostgreSQL not at all.
func TestIdempotentReplayPlanExpectsNothing(t *testing.T) {
	plan := IdempotentReplayPlanV3()
	if err := plan.Validate(); err != nil {
		t.Fatalf("idempotent replay plan rejected: %v", err)
	}
	if got := plan.ExpectedTotal(); got != 0 {
		t.Fatalf("idempotent replay total = %d, want 0", got)
	}
}

// Connector.Query issues no representation pin; only QueryPairStream does.
func TestSingleQueryPlanHasNoRepresentationPin(t *testing.T) {
	plan := SingleQueryPlanV3(1, testSchemaDigest)
	if err := plan.Validate(); err != nil {
		t.Fatalf("single query plan rejected: %v", err)
	}
	expected := plan.Expected()
	if expected[V3RepresentationPin] != 0 {
		t.Fatalf("single query path expects %d representation pins, want 0", expected[V3RepresentationPin])
	}
	for class, want := range map[GatewayStatementClassV3]int64{
		V3TransactionBegin: 1, V3TransactionCommit: 1, V3SafetySessionPin: 1,
		V3StatementTimeoutPin: 1, V3DatasourceIdentity: 2,
		V3TargetedVisible: 1, V3TargetedCompanion: 0,
	} {
		if expected[class] != want {
			t.Errorf("class %s = %d, want %d", class, expected[class], want)
		}
	}
}

// The transaction count must not be a function of the target-statement count.
// Doubling the targets without doubling the transactions has to be rejected,
// because that is exactly the inference v2 made.
func TestTransactionCountIsNotDerivedFromTargetCount(t *testing.T) {
	plan := PairedNovelPlanV3(1, testSchemaDigest)
	plan.ExpectedVisibleCalls = 2
	if err := plan.Validate(); err == nil {
		t.Fatal("a plan whose targets do not follow its transaction mix was accepted")
	}
	// And the converse: more transactions than targets can account for.
	plan = PairedNovelPlanV3(1, testSchemaDigest)
	plan.PairedQueryTransactions = 2
	if err := plan.Validate(); err == nil {
		t.Fatal("a plan with more paired transactions than companion statements was accepted")
	}
}

// The nested-lookup and utility-statement rules are properties of one
// PostgreSQL configuration. A plan carrying a different environment must be
// rejected rather than silently accounted with rules derived elsewhere.
func TestPlanRejectsAnUnsupportedMeasurementEnvironment(t *testing.T) {
	for name, mutate := range map[string]func(*MeasurementEnvironment){
		"track is top rather than all":   func(e *MeasurementEnvironment) { e.Track = "top" },
		"track_utility is off":           func(e *MeasurementEnvironment) { e.TrackUtility = "off" },
		"track_planning is on":           func(e *MeasurementEnvironment) { e.TrackPlanning = "on" },
		"a different PostgreSQL version": func(e *MeasurementEnvironment) { e.PostgreSQLVersionNum = 170004 },
	} {
		t.Run(name, func(t *testing.T) {
			plan := PairedNovelPlanV3(1, testSchemaDigest)
			mutate(&plan.Environment)
			if err := plan.Validate(); err == nil {
				t.Fatal("a plan derived for a different measurement environment was accepted")
			}
		})
	}
}

func TestPlanRejectsStructurallyImpossiblePaths(t *testing.T) {
	for name, mutate := range map[string]func(*GatewayControlPlanV3){
		"version cleared": func(p *GatewayControlPlanV3) { p.Version = "" },
		"v2 version": func(p *GatewayControlPlanV3) {
			p.Version = ObserverAccountingVersion
		},
		"a transaction without a preflight attestation": func(p *GatewayControlPlanV3) {
			p.PreflightAttestationPasses = 0
		},
		"attesting against no ExpectedSchema entry": func(p *GatewayControlPlanV3) {
			p.ExpectedSchemaEntries = 0
		},
		"an ExpectedSchema digest that is not SHA-256": func(p *GatewayControlPlanV3) {
			p.ExpectedSchemaDigest = "not-a-digest"
		},
		"a negative dimension": func(p *GatewayControlPlanV3) {
			p.PairedQueryTransactions = -1
		},
		"a companion statement with no paired transaction": func(p *GatewayControlPlanV3) {
			p.PairedQueryTransactions = 0
			p.ExpectedVisibleCalls = 1
			p.ExpectedCompanionCall = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := PairedNovelPlanV3(1, testSchemaDigest)
			mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("a structurally impossible plan was accepted")
			}
		})
	}
}

// Multiplicity must follow the exact ExpectedSchema entry count, and the three
// per-entry classes must move together: the nested lookup exists once per
// pg_get_viewdef call, so it tracks the view-definition attestation exactly.
func TestPerEntryClassesFollowTheExpectedSchemaCount(t *testing.T) {
	for _, entries := range []int64{1, 2, 5} {
		plan := PairedNovelPlanV3(entries, testSchemaDigest)
		if err := plan.Validate(); err != nil {
			t.Fatalf("entries=%d rejected: %v", entries, err)
		}
		expected := plan.Expected()
		want := 2 * entries // one preflight pass plus one paired transaction
		for _, class := range []GatewayStatementClassV3{
			V3ViewColumnAttestation, V3ViewDefinitionAttest, V3NestedViewdefRewrite,
		} {
			if expected[class] != want {
				t.Errorf("entries=%d class %s = %d, want %d", entries, class, expected[class], want)
			}
		}
		if expected[V3NestedViewdefRewrite] != expected[V3ViewDefinitionAttest] {
			t.Errorf("entries=%d nested lookups (%d) do not track view-definition calls (%d)",
				entries, expected[V3NestedViewdefRewrite], expected[V3ViewDefinitionAttest])
		}
	}
}

// A finalizer rejection has to say which dimension the Adapter got wrong.
func TestMismatchedFieldsNamesEveryDisagreement(t *testing.T) {
	derived := PairedNovelPlanV3(1, testSchemaDigest)
	carried := SemanticReplayPlanV3(1, testSchemaDigest)
	if derived.Equal(carried) {
		t.Fatal("a paired novel plan compared equal to a semantic replay plan")
	}
	fields := strings.Join(derived.MismatchedFields(carried), ",")
	for _, want := range []string{
		"paired_query_transactions", "expected_visible_calls", "expected_companion_calls",
	} {
		if !strings.Contains(fields, want) {
			t.Errorf("mismatch report omits %s: %s", want, fields)
		}
	}
}

// path_kind is authoritative. Every dimension is pinned to the tuple the path
// actually produces, so a hybrid that is arithmetically self-consistent but
// corresponds to no code path is still rejected.
func TestPathKindRejectsEveryHybridCombination(t *testing.T) {
	for name, mutate := range map[string]func(*GatewayControlPlanV3){
		"paired novel claiming a single-query transaction": func(p *GatewayControlPlanV3) {
			p.SingleQueryTransactions = 1
			p.ExpectedVisibleCalls = 2 // keeps the algebra self-consistent
		},
		"paired novel with a second preflight pass": func(p *GatewayControlPlanV3) {
			p.PreflightAttestationPasses = 2
		},
		"paired novel demoted to no preflight": func(p *GatewayControlPlanV3) {
			p.PreflightAttestationPasses = 0
		},
		"semantic replay that opens a paired transaction": func(p *GatewayControlPlanV3) {
			p.PathKind = PathSemanticReplay
		},
		"single query that keeps a companion": func(p *GatewayControlPlanV3) {
			p.PathKind = PathSingleQuery
		},
		"idempotent replay that still attests": func(p *GatewayControlPlanV3) {
			p.PathKind = PathIdempotentReplay
		},
		"an unknown path kind": func(p *GatewayControlPlanV3) {
			p.PathKind = "paired_novel_v2"
		},
		"an empty path kind": func(p *GatewayControlPlanV3) {
			p.PathKind = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := PairedNovelPlanV3(1, testSchemaDigest)
			mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("a plan that corresponds to no execution path was accepted")
			}
		})
	}
}

// E and its digest are presence-coupled in both directions.
func TestExpectedSchemaEntriesAndDigestArePresenceCoupled(t *testing.T) {
	attesting := PairedNovelPlanV3(1, testSchemaDigest)
	attesting.ExpectedSchemaDigest = ""
	if err := attesting.Validate(); err == nil {
		t.Fatal("an attesting plan with no ExpectedSchema digest was accepted")
	}
	attesting = PairedNovelPlanV3(1, "NOT-LOWERCASE-SHA256")
	if err := attesting.Validate(); err == nil {
		t.Fatal("an ExpectedSchema digest that is not a lowercase SHA-256 was accepted")
	}

	idempotent := IdempotentReplayPlanV3()
	idempotent.ExpectedSchemaDigest = testSchemaDigest
	if err := idempotent.Validate(); err == nil {
		t.Fatal("a plan attesting against nothing was allowed to carry a digest")
	}
	idempotent = IdempotentReplayPlanV3()
	idempotent.ExpectedSchemaEntries = 1
	if err := idempotent.Validate(); err == nil {
		t.Fatal("a plan reaching no ExpectedSchema entry was allowed to claim one")
	}
}

// Every named constructor must produce a plan its own Validate accepts, and
// each must carry its own path kind.
func TestEveryNamedPathConstructorValidates(t *testing.T) {
	for kind, plan := range map[GatewayPathKind]GatewayControlPlanV3{
		PathPairedNovel:      PairedNovelPlanV3(1, testSchemaDigest),
		PathSemanticReplay:   SemanticReplayPlanV3(1, testSchemaDigest),
		PathIdempotentReplay: IdempotentReplayPlanV3(),
		PathSingleQuery:      SingleQueryPlanV3(1, testSchemaDigest),
	} {
		if plan.PathKind != kind {
			t.Errorf("constructor for %s produced path_kind %s", kind, plan.PathKind)
		}
		if err := plan.Validate(); err != nil {
			t.Errorf("constructor for %s produced an invalid plan: %v", kind, err)
		}
	}
}
