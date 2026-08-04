package experiment

import (
	"fmt"
	"strings"
	"testing"
)

const testSchemaDigest = "1111111111111111111111111111111111111111111111111111111111111111"

// schemaDigestFor gives each ExpectedSchema entry count its own digest, because
// a footprint is qualified against one ExpectedSchema identity and reusing a
// single digest across entry counts would hide exactly that binding.
func schemaDigestFor(entries int64) string {
	if entries == 1 {
		return testSchemaDigest
	}
	return fmt.Sprintf("%064x", entries)
}

// footprintFor is the footprint Stage N1's requalification measures for an
// E-entry ExpectedSchema: one internal key, E calls per Attestation, in both
// scopes. Tests that need a footprint disagreeing with that build their own.
func footprintFor(t *testing.T, entries int64) AttestationFootprintV1 {
	t.Helper()
	return footprintWithCalls(t, entries, entries, entries)
}

func footprintWithCalls(t *testing.T, entries, preflightCalls, transactionalCalls int64) AttestationFootprintV1 {
	t.Helper()
	footprint, err := NewAttestationFootprintV1(schemaDigestFor(entries), entries,
		RequiredMeasurementEnvironment(), testImageID, "v3-plan-test",
		map[AttestationScope][]AttestationInternalEntry{
			AttestationScopePreflight:     {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: preflightCalls}},
			AttestationScopeTransactional: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: transactionalCalls}},
		})
	if err != nil {
		t.Fatalf("build footprint for %d entries: %v", entries, err)
	}
	return footprint
}

func mustPairedNovel(t *testing.T, entries int64) GatewayControlPlanV3 {
	t.Helper()
	plan, err := PairedNovelPlanV3(entries, schemaDigestFor(entries), footprintFor(t, entries))
	if err != nil {
		t.Fatalf("paired novel plan for %d entries: %v", entries, err)
	}
	return plan
}

func mustSemanticReplay(t *testing.T, entries int64) GatewayControlPlanV3 {
	t.Helper()
	plan, err := SemanticReplayPlanV3(entries, schemaDigestFor(entries), footprintFor(t, entries))
	if err != nil {
		t.Fatalf("semantic replay plan for %d entries: %v", entries, err)
	}
	return plan
}

func mustSingleQuery(t *testing.T, entries int64) GatewayControlPlanV3 {
	t.Helper()
	plan, err := SingleQueryPlanV3(entries, schemaDigestFor(entries), footprintFor(t, entries))
	if err != nil {
		t.Fatalf("single query plan for %d entries: %v", entries, err)
	}
	return plan
}

func mustIdempotentReplay(t *testing.T) GatewayControlPlanV3 {
	t.Helper()
	plan, err := IdempotentReplayPlanV3()
	if err != nil {
		t.Fatalf("idempotent replay plan: %v", err)
	}
	return plan
}

// The Result-heavy novel paired path is the case Stage B measured live. The
// plan must reproduce the observed 16 exactly, class by class, from the phase
// dimensions and the qualified footprint.
func TestPairedNovelPlanReproducesTheMeasuredSixteen(t *testing.T) {
	plan := mustPairedNovel(t, 1)
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
	plan := mustSemanticReplay(t, 1)
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
	plan := mustIdempotentReplay(t)
	if got := plan.ExpectedTotal(); got != 0 {
		t.Fatalf("idempotent replay total = %d, want 0", got)
	}
}

// Connector.Query issues no representation pin; only QueryPairStream does.
func TestSingleQueryPlanHasNoRepresentationPin(t *testing.T) {
	plan := mustSingleQuery(t, 1)
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
	plan := mustPairedNovel(t, 1)
	plan.ExpectedVisibleCalls = 2
	if err := plan.Validate(); err == nil {
		t.Fatal("a plan whose targets do not follow its transaction mix was accepted")
	}
	// And the converse: more transactions than targets can account for.
	plan = mustPairedNovel(t, 1)
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
			plan := mustPairedNovel(t, 1)
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
			plan := mustPairedNovel(t, 1)
			mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("a structurally impossible plan was accepted")
			}
		})
	}
}

// The two source-derived per-entry classes follow the ExpectedSchema count,
// because the Connector executes one constant per entry.
//
// The nested lookup tracking them is a MEASURED coincidence at this deployment,
// not a rule the plan enforces -- see
// TestNestedCountComesFromTheFootprintNotFromE.
func TestPerEntryClassesFollowTheExpectedSchemaCount(t *testing.T) {
	for _, entries := range []int64{1, 2, 5} {
		plan := mustPairedNovel(t, entries)
		expected := plan.Expected()
		want := 2 * entries // one preflight pass plus one paired transaction
		for _, class := range []GatewayStatementClassV3{
			V3ViewColumnAttestation, V3ViewDefinitionAttest, V3NestedViewdefRewrite,
		} {
			if expected[class] != want {
				t.Errorf("entries=%d class %s = %d, want %d", entries, class, expected[class], want)
			}
		}
	}
}

// The nested count must come from the qualified footprint, never from E. If the
// server were measured emitting two internal statements per entry, the plan must
// expect that -- and must no longer equal the view-definition count.
func TestNestedCountComesFromTheFootprintNotFromE(t *testing.T) {
	doubled := footprintWithCalls(t, 1, 2, 2)
	plan, err := PairedNovelPlanV3(1, schemaDigestFor(1), doubled)
	if err != nil {
		t.Fatalf("plan under a doubled footprint: %v", err)
	}
	expected := plan.Expected()
	if expected[V3NestedViewdefRewrite] != 4 {
		t.Fatalf("nested lookups = %d, want the 4 the footprint measures", expected[V3NestedViewdefRewrite])
	}
	if expected[V3ViewDefinitionAttest] != 2 {
		t.Fatalf("view-definition attestations = %d, want the 2 the source derives", expected[V3ViewDefinitionAttest])
	}
	if expected[V3NestedViewdefRewrite] == expected[V3ViewDefinitionAttest] {
		t.Fatal("the nested count is still being derived from the per-entry rule")
	}
}

// The two scopes must be multiplied out separately. A footprint whose
// transactional scope differs from its preflight scope has to show up in the
// plan, which a single combined attestation count could not represent.
func TestPlanMultipliesAttestationScopesSeparately(t *testing.T) {
	asymmetric := footprintWithCalls(t, 1, 1, 4)
	plan, err := PairedNovelPlanV3(1, schemaDigestFor(1), asymmetric)
	if err != nil {
		t.Fatalf("plan under an asymmetric footprint: %v", err)
	}
	// 1 preflight * 1 + 1 paired transaction * 4.
	if got := plan.Expected()[V3NestedViewdefRewrite]; got != 5 {
		t.Fatalf("nested lookups = %d, want 5 from scope-wise multiplication", got)
	}
}

// A plan may only be built from a footprint qualified for the very ExpectedSchema
// it plans against. Scaling a footprint to another entry count is the assumption
// Stage N1 retired.
func TestConstructorRejectsAFootprintQualifiedElsewhere(t *testing.T) {
	if _, err := PairedNovelPlanV3(2, schemaDigestFor(2), footprintFor(t, 1)); err == nil {
		t.Fatal("a footprint qualified at one ExpectedSchema built a plan for another")
	}
	if _, err := PairedNovelPlanV3(1, "NOT-LOWERCASE-SHA256", footprintFor(t, 1)); err == nil {
		t.Fatal("an ExpectedSchema digest that is not a lowercase SHA-256 was accepted")
	}
}

// RequireFootprint is what lets the finalizer refuse to trust the Adapter: the
// carried nested count is recomputed rather than believed.
func TestRequireFootprintRecomputesTheNestedCount(t *testing.T) {
	footprint := footprintFor(t, 1)
	plan := mustPairedNovel(t, 1)
	if err := plan.RequireFootprint(footprint, testImageID); err != nil {
		t.Fatalf("the qualifying footprint must satisfy its own plan: %v", err)
	}

	tampered := plan
	tampered.NestedInternalCalls = 3
	if err := tampered.RequireFootprint(footprint, testImageID); err == nil {
		t.Fatal("an edited nested count was accepted against its own footprint")
	}

	// A footprint qualified against a different deployment must not satisfy it,
	// even though the plan's own digest field is internally consistent.
	elsewhere := footprintWithCalls(t, 1, 1, 1)
	elsewhere.PostgreSQLImageID = "sha256:" + strings.Repeat("9", 64)
	if err := plan.RequireFootprint(elsewhere, testImageID); err == nil {
		t.Fatal("a footprint qualified on another image satisfied the plan")
	}
	if err := plan.RequireFootprint(footprint,
		"sha256:"+strings.Repeat("8", 64)); err == nil {
		t.Fatal("a run on another PostgreSQL image was accepted")
	}
}

// A path that performs no Attestation has nothing to re-derive, and must not be
// made to carry a footprint it never used.
func TestRequireFootprintIsVacuousForIdempotentReplay(t *testing.T) {
	plan := mustIdempotentReplay(t)
	if err := plan.RequireFootprint(AttestationFootprintV1{}, testImageID); err != nil {
		t.Fatalf("idempotent replay must need no footprint: %v", err)
	}
}

// A finalizer rejection has to say which dimension the Adapter got wrong.
func TestMismatchedFieldsNamesEveryDisagreement(t *testing.T) {
	derived := mustPairedNovel(t, 1)
	carried := mustSemanticReplay(t, 1)
	if derived.Equal(carried) {
		t.Fatal("a paired novel plan compared equal to a semantic replay plan")
	}
	fields := strings.Join(derived.MismatchedFields(carried), ",")
	for _, want := range []string{
		"paired_query_transactions", "expected_visible_calls", "expected_companion_calls",
		"nested_internal_calls",
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
			plan := mustPairedNovel(t, 1)
			mutate(&plan)
			if err := plan.Validate(); err == nil {
				t.Fatal("a plan that corresponds to no execution path was accepted")
			}
		})
	}
}

// E, its digest and the footprint binding are presence-coupled in both
// directions.
func TestExpectedSchemaEntriesAndDigestArePresenceCoupled(t *testing.T) {
	attesting := mustPairedNovel(t, 1)
	attesting.ExpectedSchemaDigest = ""
	if err := attesting.Validate(); err == nil {
		t.Fatal("an attesting plan with no ExpectedSchema digest was accepted")
	}
	attesting = mustPairedNovel(t, 1)
	attesting.AttestationFootprintSHA256 = ""
	if err := attesting.Validate(); err == nil {
		t.Fatal("an attesting plan naming no qualified footprint was accepted")
	}

	idempotent := mustIdempotentReplay(t)
	idempotent.ExpectedSchemaDigest = testSchemaDigest
	if err := idempotent.Validate(); err == nil {
		t.Fatal("a plan attesting against nothing was allowed to carry a digest")
	}
	idempotent = mustIdempotentReplay(t)
	idempotent.ExpectedSchemaEntries = 1
	if err := idempotent.Validate(); err == nil {
		t.Fatal("a plan reaching no ExpectedSchema entry was allowed to claim one")
	}
	idempotent = mustIdempotentReplay(t)
	idempotent.AttestationFootprintSHA256 = testSchemaDigest
	if err := idempotent.Validate(); err == nil {
		t.Fatal("a plan performing no Attestation was allowed to carry a footprint digest")
	}
	idempotent = mustIdempotentReplay(t)
	idempotent.NestedInternalCalls = 1
	if err := idempotent.Validate(); err == nil {
		t.Fatal("a plan performing no Attestation was allowed to expect nested statements")
	}
}

// Every named constructor must produce a plan its own Validate accepts, and
// each must carry its own path kind.
func TestEveryNamedPathConstructorValidates(t *testing.T) {
	for kind, plan := range map[GatewayPathKind]GatewayControlPlanV3{
		PathPairedNovel:      mustPairedNovel(t, 1),
		PathSemanticReplay:   mustSemanticReplay(t, 1),
		PathIdempotentReplay: mustIdempotentReplay(t),
		PathSingleQuery:      mustSingleQuery(t, 1),
	} {
		if plan.PathKind != kind {
			t.Errorf("constructor for %s produced path_kind %s", kind, plan.PathKind)
		}
		if err := plan.Validate(); err != nil {
			t.Errorf("constructor for %s produced an invalid plan: %v", kind, err)
		}
	}
}
