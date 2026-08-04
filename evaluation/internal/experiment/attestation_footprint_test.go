package experiment

import (
	"strings"
	"testing"
)

// testSchemaDigest is declared alongside the v3 plan tests; these reuse it so a
// footprint and a plan under test are qualified against one ExpectedSchema.
const (
	testInternalKeyA = "e5738df1650276a7f20e677172e067bc62bab12d48c18a378c9b6ed602433842"
	testInternalKeyB = "3cfbbde6160f50e1d80a3302c6f6a95426c191405290b3d6c54980d3e71c9f34"
)

func testRuntimeIdentity() PostgreSQLRuntimeIdentity {
	image := "sha256:" + strings.Repeat("1", 64)
	return PostgreSQLRuntimeIdentity{
		ImageReference:   "postgres@sha256:" + strings.Repeat("2", 64),
		RepoDigest:       "postgres@sha256:" + strings.Repeat("2", 64),
		LocalImageID:     image,
		ContainerImageID: image,
		Platform:         "linux/amd64",
	}
}

// qualifiedFootprint is the shape Stage N4 measures: one internal key at one
// call per Attestation, in every scope, for a one-entry ExpectedSchema.
func qualifiedFootprint(t *testing.T) AttestationFootprintV2 {
	t.Helper()
	return footprintWithScopeCalls(t, 1, map[AttestationScope]int64{
		AttestationScopeConstructorOrColdPool:  1,
		AttestationScopeExplicitPreflightPool:  1,
		AttestationScopeSingleQueryTransaction: 1,
		AttestationScopePairedQueryTransaction: 1,
	})
}

func footprintWithScopeCalls(t *testing.T, entries int64, calls map[AttestationScope]int64) AttestationFootprintV2 {
	t.Helper()
	measured := map[AttestationScope][]AttestationInternalEntry{}
	for scope, perAttestation := range calls {
		if perAttestation == 0 {
			continue
		}
		measured[scope] = []AttestationInternalEntry{
			{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: perAttestation},
		}
	}
	footprint, err := NewAttestationFootprintV2(schemaDigestFor(entries), entries,
		RequiredMeasurementEnvironment(), testRuntimeIdentity(), "attestation-footprint-qualification-test", measured)
	if err != nil {
		t.Fatalf("build footprint: %v", err)
	}
	return footprint
}

func TestQualifiedFootprintValidates(t *testing.T) {
	footprint := qualifiedFootprint(t)
	if err := footprint.Validate(); err != nil {
		t.Fatalf("qualified footprint must validate: %v", err)
	}
	if got := len(footprint.Scopes); got != 4 {
		t.Fatalf("footprint must carry all four scopes, got %d", got)
	}
	if keys := footprint.InternalKeys(); len(keys) != 1 || keys[0] != testInternalKeyA {
		t.Fatalf("internal keys = %v", keys)
	}
	if !footprint.ConstructorMatchesExplicitPreflight() {
		t.Fatal("equal constructor and explicit preflight scopes reported as differing")
	}
}

// The four scopes must stay distinct. A footprint measured only through
// Connector.Query must not answer for the paired path.
func TestScopesAreNotInterchangeable(t *testing.T) {
	footprint := footprintWithScopeCalls(t, 1, map[AttestationScope]int64{
		AttestationScopeConstructorOrColdPool:  1,
		AttestationScopeExplicitPreflightPool:  1,
		AttestationScopeSingleQueryTransaction: 1,
		AttestationScopePairedQueryTransaction: 7,
	})
	single, err := footprint.Scope(AttestationScopeSingleQueryTransaction)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	paired, err := footprint.Scope(AttestationScopePairedQueryTransaction)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if single.TotalCallsPerAttestation() == paired.TotalCallsPerAttestation() {
		t.Fatal("the single-query and paired scopes were collapsed")
	}
	// One paired transaction must draw on the paired scope, not the single one.
	expectation, err := footprint.InternalExpectation(0, 0, 1)
	if err != nil {
		t.Fatalf("internal expectation: %v", err)
	}
	if len(expectation) != 1 || expectation[0].Calls != 7 {
		t.Fatalf("paired expectation = %+v, want the paired scope's 7", expectation)
	}
}

// Constructor and explicit preflight are retained separately. Their equality is
// evidence a later revision would need before merging them, not an assumption.
func TestConstructorAndPreflightDisagreementIsVisible(t *testing.T) {
	footprint := footprintWithScopeCalls(t, 1, map[AttestationScope]int64{
		AttestationScopeConstructorOrColdPool:  2,
		AttestationScopeExplicitPreflightPool:  1,
		AttestationScopeSingleQueryTransaction: 1,
		AttestationScopePairedQueryTransaction: 1,
	})
	if footprint.ConstructorMatchesExplicitPreflight() {
		t.Fatal("differing pool-scope observations reported as equal")
	}
	// The constructor scope must not leak into a per-operation expectation: the
	// Gateway constructs its pool once, outside every measurement window.
	expectation, err := footprint.InternalExpectation(1, 0, 0)
	if err != nil {
		t.Fatalf("internal expectation: %v", err)
	}
	if len(expectation) != 1 || expectation[0].Calls != 1 {
		t.Fatalf("preflight expectation = %+v, want the explicit preflight scope's 1", expectation)
	}
}

// The expectation is a multiset. Summing scopes key by key is what makes a
// same-total substitution detectable.
func TestInternalExpectationSumsPerKey(t *testing.T) {
	measured := map[AttestationScope][]AttestationInternalEntry{
		AttestationScopeConstructorOrColdPool:  {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
		AttestationScopeExplicitPreflightPool:  {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
		AttestationScopeSingleQueryTransaction: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
		AttestationScopePairedQueryTransaction: {
			{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1},
			{StrictASTSHA256: testInternalKeyB, CallsPerAttestation: 3},
		},
	}
	footprint, err := NewAttestationFootprintV2(schemaDigestFor(1), 1,
		RequiredMeasurementEnvironment(), testRuntimeIdentity(), "multi-key-test", measured)
	if err != nil {
		t.Fatalf("build footprint: %v", err)
	}
	// 2 preflight + 1 paired: key A = 2*1 + 1*1 = 3, key B = 1*3 = 3.
	expectation, err := footprint.InternalExpectation(2, 0, 1)
	if err != nil {
		t.Fatalf("internal expectation: %v", err)
	}
	byKey := map[string]int64{}
	for _, entry := range expectation {
		if entry.RequiredTopLevel {
			t.Fatalf("internal key %s expects toplevel", entry.StrictASTSHA256[:12])
		}
		byKey[entry.StrictASTSHA256] = entry.Calls
	}
	if byKey[testInternalKeyA] != 3 || byKey[testInternalKeyB] != 3 {
		t.Fatalf("per-key expectation = %v", byKey)
	}
	if len(expectation) != 2 {
		t.Fatalf("expectation carries %d keys, want 2", len(expectation))
	}
}

// TestRequireBindsEveryQualificationCondition is the fail-closed core.
func TestRequireBindsEveryQualificationCondition(t *testing.T) {
	footprint := qualifiedFootprint(t)
	environment := RequiredMeasurementEnvironment()
	identity := testRuntimeIdentity()

	if err := footprint.Require(schemaDigestFor(1), 1, environment, identity); err != nil {
		t.Fatalf("matching conditions must bind: %v", err)
	}

	otherEnvironment := environment
	otherEnvironment.Track = "top"
	otherIdentity := identity
	otherIdentity.Platform = "linux/arm64"

	for _, testCase := range []struct {
		name    string
		digest  string
		entries int64
		env     MeasurementEnvironment
		image   PostgreSQLRuntimeIdentity
		want    string
	}{
		{"wrong ExpectedSchema", strings.Repeat("a", 64), 1, environment, identity, "was qualified for ExpectedSchema"},
		{"wrong entry count", schemaDigestFor(1), 2, environment, identity, "were not derived from one builder"},
		{"wrong environment", schemaDigestFor(1), 1, otherEnvironment, identity, "was qualified under PostgreSQL"},
		{"wrong platform", schemaDigestFor(1), 1, environment, otherIdentity, "was qualified against PostgreSQL image"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := footprint.Require(testCase.digest, testCase.entries, testCase.env, testCase.image)
			if err == nil {
				t.Fatal("differing qualification condition must fail closed")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not explain the binding failure", err)
			}
		})
	}
}

// A footprint measured at one ExpectedSchema must not answer for another by
// scaling, however tempting the arithmetic is.
func TestFootprintIsNotScaledToAnotherExpectedSchema(t *testing.T) {
	footprint := qualifiedFootprint(t)
	if err := footprint.Require(schemaDigestFor(2), 2, RequiredMeasurementEnvironment(),
		testRuntimeIdentity()); err == nil {
		t.Fatal("a footprint qualified at one ExpectedSchema must not be reused for another")
	}
}

func TestInternalExpectationRejectsNegativeAttestations(t *testing.T) {
	footprint := qualifiedFootprint(t)
	if _, err := footprint.InternalExpectation(-1, 0, 0); err == nil {
		t.Fatal("negative attestation count must be rejected")
	}
}

func TestRuntimeIdentityRejectsIncompleteBindings(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*PostgreSQLRuntimeIdentity)
		want   string
	}{
		{"mutable tag reference", func(i *PostgreSQLRuntimeIdentity) { i.ImageReference = "postgres:16.14" },
			"not digest-pinned"},
		{"mutable repo digest", func(i *PostgreSQLRuntimeIdentity) { i.RepoDigest = "postgres:16-bookworm" },
			"not digest-pinned"},
		{"absent local image", func(i *PostgreSQLRuntimeIdentity) { i.LocalImageID = "" },
			"local_image_id"},
		{"absent container image", func(i *PostgreSQLRuntimeIdentity) { i.ContainerImageID = "" },
			"container_image_id"},
		{"container running another image", func(i *PostgreSQLRuntimeIdentity) {
			i.ContainerImageID = "sha256:" + strings.Repeat("9", 64)
		}, "the running container was created from"},
		{"absent platform", func(i *PostgreSQLRuntimeIdentity) { i.Platform = "" }, "platform is empty"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			identity := testRuntimeIdentity()
			testCase.mutate(&identity)
			err := identity.Validate()
			if err == nil {
				t.Fatal("an incomplete PostgreSQL identity was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the defect %q", err, testCase.want)
			}
		})
	}
}

func TestFootprintValidationRejectsMalformedContracts(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*AttestationFootprintV2)
		want   string
	}{
		{"unsupported version", func(f *AttestationFootprintV2) { f.Version = "v0" }, "version"},
		{"absent ExpectedSchema digest", func(f *AttestationFootprintV2) { f.ExpectedSchemaDigest = "" },
			"no ExpectedSchema digest"},
		{"zero entries", func(f *AttestationFootprintV2) { f.ExpectedSchemaEntries = 0 }, "ExpectedSchema entries"},
		{"uncovered environment", func(f *AttestationFootprintV2) { f.Environment.PostgreSQLVersionNum = 160013 },
			"observer accounting v3 is derived for"},
		{"incomplete PostgreSQL identity", func(f *AttestationFootprintV2) { f.PostgreSQL.Platform = "" },
			"PostgreSQL identity"},
		{"no qualification run", func(f *AttestationFootprintV2) { f.QualificationID = "  " },
			"names no qualification run"},
		{"missing scope", func(f *AttestationFootprintV2) { f.Scopes = f.Scopes[:3] }, "qualifies 3 scopes"},
		{"scopes out of canonical order", func(f *AttestationFootprintV2) {
			f.Scopes[0], f.Scopes[1] = f.Scopes[1], f.Scopes[0]
		}, "canonical order requires"},
		{"duplicate internal key", func(f *AttestationFootprintV2) {
			f.Scopes[0].Internal = append(f.Scopes[0].Internal,
				AttestationInternalEntry{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1})
		}, "twice"},
		{"zero-call entry", func(f *AttestationFootprintV2) { f.Scopes[0].Internal[0].CallsPerAttestation = 0 },
			"calls per attestation"},
		{"internal entries out of order", func(f *AttestationFootprintV2) {
			f.Scopes[0].Internal = []AttestationInternalEntry{
				{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1},
				{StrictASTSHA256: testInternalKeyB, CallsPerAttestation: 1},
			}
		}, "canonical order"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			footprint := qualifiedFootprint(t)
			testCase.mutate(&footprint)
			err := footprint.Validate()
			if err == nil {
				t.Fatal("malformed footprint must be rejected")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the defect %q", err, testCase.want)
			}
		})
	}
}

// "This scope emits nothing" must stay expressible and distinguishable from an
// unqualified scope.
func TestEmptyScopeFootprintIsLegal(t *testing.T) {
	footprint := footprintWithScopeCalls(t, 1, map[AttestationScope]int64{
		AttestationScopeConstructorOrColdPool:  1,
		AttestationScopeExplicitPreflightPool:  1,
		AttestationScopeSingleQueryTransaction: 1,
		// The paired scope emits nothing at all.
	})
	expectation, err := footprint.InternalExpectation(0, 0, 9)
	if err != nil {
		t.Fatalf("internal expectation: %v", err)
	}
	if len(expectation) != 0 {
		t.Fatalf("expectation = %+v, want none", expectation)
	}
}

// The portable digest is what two independent qualification runs must agree on;
// it must ignore what legitimately differs between them and nothing else.
func TestPortableDigestIgnoresOnlyRunLocalIdentity(t *testing.T) {
	base := qualifiedFootprint(t)
	basePortable, err := base.PortableSHA256()
	if err != nil {
		t.Fatalf("portable digest: %v", err)
	}

	secondRun := qualifiedFootprint(t)
	secondRun.QualificationID = "a-second-independent-run"
	local := "sha256:" + strings.Repeat("7", 64)
	secondRun.PostgreSQL.LocalImageID, secondRun.PostgreSQL.ContainerImageID = local, local
	secondPortable, err := secondRun.PortableSHA256()
	if err != nil {
		t.Fatalf("portable digest: %v", err)
	}
	if basePortable != secondPortable {
		t.Fatal("two independent runs of the same qualification disagreed on the portable digest")
	}
	if first, _ := base.SHA256(); first == mustSHA256(t, secondRun) {
		t.Fatal("the full digest ignored the run-local identity it must bind")
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*AttestationFootprintV2)
	}{
		{"ExpectedSchema digest", func(f *AttestationFootprintV2) { f.ExpectedSchemaDigest = strings.Repeat("c", 64) }},
		{"source image reference", func(f *AttestationFootprintV2) {
			f.PostgreSQL.ImageReference = "postgres@sha256:" + strings.Repeat("5", 64)
		}},
		{"platform", func(f *AttestationFootprintV2) { f.PostgreSQL.Platform = "linux/arm64" }},
		{"a scope's call count", func(f *AttestationFootprintV2) { f.Scopes[3].Internal[0].CallsPerAttestation = 2 }},
		{"a scope's internal key", func(f *AttestationFootprintV2) { f.Scopes[3].Internal[0].StrictASTSHA256 = testInternalKeyB }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := qualifiedFootprint(t)
			testCase.mutate(&mutated)
			if digest, err := mutated.PortableSHA256(); err == nil && digest == basePortable {
				t.Fatalf("changing the %s did not change the portable digest", testCase.name)
			}
		})
	}
}

func mustSHA256(t *testing.T, footprint AttestationFootprintV2) string {
	t.Helper()
	digest, err := footprint.SHA256()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}
