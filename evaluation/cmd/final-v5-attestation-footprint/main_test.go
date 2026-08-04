package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

const (
	internalKey = "e5738df1650276a7f20e677172e067bc62bab12d48c18a378c9b6ed602433842"
	otherKey    = "3cfbbde6160f50e1d80a3302c6f6a95426c191405290b3d6c54980d3e71c9f34"
	testImageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func testConfiguration(entries int) schemaConfiguration {
	schema := make([]dataconnector.ViewSchema, 0, entries)
	kinds := make([]string, 0, entries)
	for index := 0; index < entries; index++ {
		schema = append(schema, dataconnector.ViewSchema{
			Schema: "reporting", View: string(rune('a'+index)) + "_view",
			Columns: []dataconnector.SchemaColumn{{Name: "id", PostgreSQLType: "bigint"}},
		})
		kinds = append(kinds, "plain_view")
	}
	// The same digest function buildConfigurations uses, so the test stays in
	// the production ExpectedSchema identity space.
	return schemaConfiguration{kinds: kinds, entries: schema, digest: catalogschema.Digest(schema)}
}

func trialsFor(configuration schemaConfiguration, callsPerAttestation int64, repetitions int) []trial {
	var trials []trial
	for repetition := 0; repetition < repetitions; repetition++ {
		for _, scope := range []string{scopePreflight, scopeTransaction} {
			trials = append(trials, trial{
				Scope: scope, ExpectedSchemaDigest: configuration.digest,
				ExpectedSchemaEntries: int64(len(configuration.entries)),
				RelationKinds:         configuration.kinds, Repetition: repetition,
				AttestationsPerTrial: 2,
				InternalPerAttestation: []structuralEntry{
					{StrictASTSHA256: internalKey, Calls: callsPerAttestation},
				},
			})
		}
	}
	return trials
}

func TestQualifyEmitsAFootprintBoundToItsExpectedSchema(t *testing.T) {
	configuration := testConfiguration(1)
	trials := trialsFor(configuration, 1, 3)
	stability, footprint, err := qualify(trials, configuration,
		experiment.RequiredMeasurementEnvironment(), testImageID, "qualification-test")
	if err != nil {
		t.Fatalf("qualify: %v", err)
	}
	if !stability.PreflightStable || !stability.TransactionStable {
		t.Fatalf("stable trials reported unstable: %+v", stability)
	}
	if footprint.ExpectedSchemaDigest != configuration.digest {
		t.Fatalf("footprint bound to %s, want the measured schema %s",
			footprint.ExpectedSchemaDigest, configuration.digest)
	}
	if footprint.ExpectedSchemaEntries != 1 {
		t.Fatalf("footprint entries = %d, want 1", footprint.ExpectedSchemaEntries)
	}
	if err := footprint.Require(configuration.digest, 1,
		experiment.RequiredMeasurementEnvironment(), testImageID); err != nil {
		t.Fatalf("the emitted footprint must bind to its own conditions: %v", err)
	}
}

// The two-entry configuration is what separates "per Attestation" from "per
// ExpectedSchema entry"; the emitted footprint must carry the measured 2.
func TestQualifyCarriesTheMeasuredPerEntryMultiplicity(t *testing.T) {
	configuration := testConfiguration(2)
	_, footprint, err := qualify(trialsFor(configuration, 2, 3), configuration,
		experiment.RequiredMeasurementEnvironment(), testImageID, "qualification-test")
	if err != nil {
		t.Fatalf("qualify: %v", err)
	}
	scope, err := footprint.Scope(experiment.AttestationScopePreflight)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if got := scope.TotalCallsPerAttestation(); got != 2 {
		t.Fatalf("per-attestation calls = %d, want the measured 2", got)
	}
}

// Disagreement between repetitions must stop the qualification, not be averaged,
// unioned or resolved by taking the last trial.
func TestQualifyRefusesWhenTrialsDisagree(t *testing.T) {
	configuration := testConfiguration(1)
	for name, mutate := range map[string]func([]trial){
		"a differing multiplicity": func(trials []trial) {
			trials[len(trials)-1].InternalPerAttestation[0].Calls = 2
		},
		"a differing structural key": func(trials []trial) {
			trials[len(trials)-1].InternalPerAttestation[0].StrictASTSHA256 = otherKey
		},
		"an extra internal statement": func(trials []trial) {
			trials[len(trials)-1].InternalPerAttestation = append(
				trials[len(trials)-1].InternalPerAttestation,
				structuralEntry{StrictASTSHA256: otherKey, Calls: 1})
		},
		"a missing internal statement": func(trials []trial) {
			trials[len(trials)-1].InternalPerAttestation = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			trials := trialsFor(configuration, 1, 3)
			mutate(trials)
			_, _, err := qualify(trials, configuration,
				experiment.RequiredMeasurementEnvironment(), testImageID, "qualification-test")
			if err == nil {
				t.Fatal("disagreeing trials produced a qualified footprint")
			}
			if !strings.Contains(err.Error(), "ATTESTATION INTERNAL FOOTPRINT NOT STABLE") {
				t.Fatalf("error %q does not raise the stop condition", err)
			}
		})
	}
}

// A scope with no trial at all must fail rather than yield a footprint silent
// about it.
func TestQualifyRefusesAnUnmeasuredScope(t *testing.T) {
	configuration := testConfiguration(1)
	var preflightOnly []trial
	for _, measured := range trialsFor(configuration, 1, 3) {
		if measured.Scope == scopePreflight {
			preflightOnly = append(preflightOnly, measured)
		}
	}
	if _, _, err := qualify(preflightOnly, configuration,
		experiment.RequiredMeasurementEnvironment(), testImageID, "qualification-test"); err == nil {
		t.Fatal("a footprint was qualified without measuring the transactional scope")
	}
}

// Trials belonging to another ExpectedSchema must not be absorbed into this
// one's footprint.
func TestQualifyIgnoresAnotherExpectedSchemasTrials(t *testing.T) {
	one := testConfiguration(1)
	two := testConfiguration(2)
	trials := append(trialsFor(one, 1, 3), trialsFor(two, 2, 3)...)
	_, footprint, err := qualify(trials, one,
		experiment.RequiredMeasurementEnvironment(), testImageID, "qualification-test")
	if err != nil {
		t.Fatalf("qualify: %v", err)
	}
	scope, err := footprint.Scope(experiment.AttestationScopeTransactional)
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if got := scope.TotalCallsPerAttestation(); got != 1 {
		t.Fatalf("per-attestation calls = %d, want the 1 measured for this ExpectedSchema", got)
	}
}

func TestImageIDMustBeImmutable(t *testing.T) {
	for _, imageID := range []string{
		"", "postgres:16.14", "sha256:short",
		"sha256:zzzz111111111111111111111111111111111111111111111111111111111111",
		strings.Repeat("1", 64),
	} {
		if err := requireImageID(imageID); err == nil {
			t.Fatalf("image identity %q was accepted", imageID)
		}
	}
	if err := requireImageID(testImageID); err != nil {
		t.Fatalf("an immutable image identity was rejected: %v", err)
	}
}
