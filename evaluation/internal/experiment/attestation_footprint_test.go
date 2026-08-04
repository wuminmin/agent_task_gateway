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
	testImageID      = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// qualifiedFootprint is the shape Stage N1 measured, expressed as the contract
// N1's own record could not carry: one internal key, one call per ExpectedSchema
// entry per Attestation, in both scopes, at E=1.
func qualifiedFootprint(t *testing.T) AttestationFootprintV1 {
	t.Helper()
	footprint, err := NewAttestationFootprintV1(testSchemaDigest, 1, RequiredMeasurementEnvironment(),
		testImageID, "attestation-footprint-qualification-test",
		map[AttestationScope][]AttestationInternalEntry{
			AttestationScopePreflight:     {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeTransactional: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
		})
	if err != nil {
		t.Fatalf("build qualified footprint: %v", err)
	}
	return footprint
}

func TestQualifiedFootprintValidates(t *testing.T) {
	footprint := qualifiedFootprint(t)
	if err := footprint.Validate(); err != nil {
		t.Fatalf("qualified footprint must validate: %v", err)
	}
	if got := len(footprint.Scopes); got != 2 {
		t.Fatalf("footprint must carry both scopes, got %d", got)
	}
	if keys := footprint.InternalKeys(); len(keys) != 1 || keys[0] != testInternalKeyA {
		t.Fatalf("internal keys = %v", keys)
	}
}

// TestRequireBindsEveryQualificationCondition is the fail-closed core: each of the
// four bindings must independently reject a measurement that differs.
func TestRequireBindsEveryQualificationCondition(t *testing.T) {
	footprint := qualifiedFootprint(t)
	environment := RequiredMeasurementEnvironment()

	if err := footprint.Require(testSchemaDigest, 1, environment, testImageID); err != nil {
		t.Fatalf("matching conditions must bind: %v", err)
	}

	otherEnvironment := environment
	otherEnvironment.Track = "top"
	otherSchema := strings.Repeat("a", 64)

	for _, testCase := range []struct {
		name    string
		digest  string
		entries int64
		env     MeasurementEnvironment
		image   string
		want    string
	}{
		{"wrong ExpectedSchema", otherSchema, 1, environment, testImageID, "was qualified for ExpectedSchema"},
		{"wrong entry count", testSchemaDigest, 2, environment, testImageID, "were not derived from one builder"},
		{"wrong environment", testSchemaDigest, 1, otherEnvironment, testImageID, "was qualified under PostgreSQL"},
		{"wrong image", testSchemaDigest, 1, environment,
			"sha256:2222222222222222222222222222222222222222222222222222222222222222",
			"was qualified against PostgreSQL image"},
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

// TestFootprintIsNotScaledToAnotherExpectedSchema is the assumption Stage N1
// retired. A footprint measured at E=1 must not answer for E=2 by multiplication,
// however tempting the arithmetic is.
func TestFootprintIsNotScaledToAnotherExpectedSchema(t *testing.T) {
	footprint := qualifiedFootprint(t)
	twoEntrySchema := strings.Repeat("b", 64)
	if err := footprint.Require(twoEntrySchema, 2, RequiredMeasurementEnvironment(), testImageID); err == nil {
		t.Fatal("a footprint qualified at one ExpectedSchema must not be reused for another")
	}
}

// TestInternalCallsMultipliesScopesSeparately proves the two scopes are never
// collapsed into one attestation count, by giving them different footprints.
func TestInternalCallsMultipliesScopesSeparately(t *testing.T) {
	footprint, err := NewAttestationFootprintV1(testSchemaDigest, 1, RequiredMeasurementEnvironment(),
		testImageID, "asymmetric-scope-test",
		map[AttestationScope][]AttestationInternalEntry{
			AttestationScopePreflight:     {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeTransactional: {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 3}},
		})
	if err != nil {
		t.Fatalf("build asymmetric footprint: %v", err)
	}
	// 2 preflight * 1 + 5 transactional * 3 = 17. A lumped model would give 21.
	calls, err := footprint.InternalCalls(2, 5)
	if err != nil {
		t.Fatalf("internal calls: %v", err)
	}
	if calls != 17 {
		t.Fatalf("scope-wise internal calls = %d, want 17", calls)
	}
}

func TestInternalCallsRejectsNegativeAttestations(t *testing.T) {
	footprint := qualifiedFootprint(t)
	if _, err := footprint.InternalCalls(-1, 0); err == nil {
		t.Fatal("negative attestation count must be rejected")
	}
}

func TestFootprintValidationRejectsMalformedContracts(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*AttestationFootprintV1)
		want   string
	}{
		{"unsupported version", func(f *AttestationFootprintV1) { f.Version = "v0" }, "version"},
		{"absent ExpectedSchema digest", func(f *AttestationFootprintV1) { f.ExpectedSchemaDigest = "" },
			"no ExpectedSchema digest"},
		{"zero entries", func(f *AttestationFootprintV1) { f.ExpectedSchemaEntries = 0 }, "ExpectedSchema entries"},
		{"uncovered environment", func(f *AttestationFootprintV1) { f.Environment.PostgreSQLVersionNum = 160013 },
			"observer accounting v3 is derived for"},
		{"mutable image tag", func(f *AttestationFootprintV1) { f.PostgreSQLImageID = "postgres:16.14" },
			"not an immutable sha256"},
		{"no qualification run", func(f *AttestationFootprintV1) { f.QualificationID = "  " },
			"names no qualification run"},
		{"missing scope", func(f *AttestationFootprintV1) { f.Scopes = f.Scopes[:1] }, "qualifies 1 scopes"},
		{"scopes out of canonical order", func(f *AttestationFootprintV1) {
			f.Scopes[0], f.Scopes[1] = f.Scopes[1], f.Scopes[0]
		}, "canonical order requires"},
		{"duplicate internal key", func(f *AttestationFootprintV1) {
			f.Scopes[0].Internal = append(f.Scopes[0].Internal,
				AttestationInternalEntry{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1})
		}, "twice"},
		{"zero-call entry", func(f *AttestationFootprintV1) { f.Scopes[0].Internal[0].CallsPerAttestation = 0 },
			"calls per attestation"},
		{"internal entries out of order", func(f *AttestationFootprintV1) {
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

// TestEmptyScopeFootprintIsLegal keeps "this scope emits nothing" expressible.
// Silence and zero must stay distinguishable from an unqualified scope.
func TestEmptyScopeFootprintIsLegal(t *testing.T) {
	footprint, err := NewAttestationFootprintV1(testSchemaDigest, 1, RequiredMeasurementEnvironment(),
		testImageID, "empty-scope-test",
		map[AttestationScope][]AttestationInternalEntry{
			AttestationScopePreflight:     {{StrictASTSHA256: testInternalKeyA, CallsPerAttestation: 1}},
			AttestationScopeTransactional: nil,
		})
	if err != nil {
		t.Fatalf("an empty scope footprint must be expressible: %v", err)
	}
	calls, err := footprint.InternalCalls(3, 9)
	if err != nil {
		t.Fatalf("internal calls: %v", err)
	}
	if calls != 3 {
		t.Fatalf("internal calls = %d, want 3", calls)
	}
}

func TestFootprintDigestChangesWithEveryBinding(t *testing.T) {
	base := qualifiedFootprint(t)
	baseDigest, err := base.SHA256()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if !validSHA256(baseDigest) {
		t.Fatalf("digest %q is not a SHA-256", baseDigest)
	}
	for _, testCase := range []struct {
		name   string
		mutate func(*AttestationFootprintV1)
	}{
		{"ExpectedSchema digest", func(f *AttestationFootprintV1) { f.ExpectedSchemaDigest = strings.Repeat("c", 64) }},
		{"image", func(f *AttestationFootprintV1) { f.PostgreSQLImageID = strings.Repeat("d", 64) }},
		{"call count", func(f *AttestationFootprintV1) { f.Scopes[1].Internal[0].CallsPerAttestation = 2 }},
		{"qualification run", func(f *AttestationFootprintV1) { f.QualificationID = "another-run" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mutated := qualifiedFootprint(t)
			testCase.mutate(&mutated)
			// Malformed mutations legitimately fail to digest; what must never
			// happen is a different footprint sharing the base digest.
			if digest, err := mutated.SHA256(); err == nil && digest == baseDigest {
				t.Fatalf("changing the %s did not change the footprint digest", testCase.name)
			}
		})
	}
}
