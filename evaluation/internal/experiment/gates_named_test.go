package experiment

import (
	"strings"
	"testing"
)

// The stable gate names from docs/final_v5_v3_runtime_integration_gates.md.
//
// Each test asserts its own requirement rather than delegating to a
// similarly-named existing test. Where an older test already covers the same
// ground it stays, because it was written against the implementation and reads
// as documentation of it; these are the contract, named so the gate document is
// checkable by symbol rather than by prose.

// Gate 2.
func TestGate02SnapshotV1Rejected(t *testing.T) {
	snapshot := snapshotOf(t, "after", nil)
	snapshot.Version = "taskgate-final-v5-observer-snapshot-v1"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("a schema-v1 observer snapshot validated on the v3 path")
	}
	// And it must not enter a window either -- there is no fallback anywhere.
	window, classifier, _ := pairedNovelWindow(t)
	window.After.Version = "taskgate-final-v5-observer-snapshot-v1"
	if _, err := window.Delta(classifier); err == nil {
		t.Fatal("a v1 snapshot was accepted as half of a v3 window")
	}
}

// Gate 3.
func TestGate03WindowIdentityMismatchRejected(t *testing.T) {
	window, classifier, _ := pairedNovelWindow(t)
	window.After.ObserverWindowID = strings.Repeat("9", 64)
	err := requireWindowRejected(t, window, classifier)
	if !strings.Contains(err.Error(), "observer window id") {
		t.Fatalf("the rejection %q does not name the window identity", err)
	}
}

// Gate 4.
func TestGate04ClassifierManifestMismatchRejected(t *testing.T) {
	// Before/after, as the observer reports them.
	window, classifier, _ := pairedNovelWindow(t)
	window.After.ClassifierManifestSHA256 = strings.Repeat("8", 64)
	err := requireWindowRejected(t, window, classifier)
	if !strings.Contains(err.Error(), "classifier manifest") {
		t.Fatalf("the rejection %q does not name the classifier manifest", err)
	}
	// Adapter against the finalizer's independent derivation.
	broken := pairedNovelCase(t)
	broken.carried.ClassifierManifestSHA256 = strings.Repeat("a", 64)
	if _, err := broken.finalize(); err == nil {
		t.Fatal("a carried classifier manifest differing from the derivation was accepted")
	}
}

// Gate 5.
func TestGate05OperationBindingMismatchRejected(t *testing.T) {
	window, classifier, _ := pairedNovelWindow(t)
	window.After.OperationBindingSHA256 = strings.Repeat("7", 64)
	err := requireWindowRejected(t, window, classifier)
	if !strings.Contains(err.Error(), "operation binding") {
		t.Fatalf("the rejection %q does not name the operation binding", err)
	}
	broken := pairedNovelCase(t)
	broken.carried.Operation.OperationID = "another-operation"
	if _, err := broken.finalize(); err == nil {
		t.Fatal("a carried operation identity differing from the derivation was accepted")
	}
}

// Gate 6. The role-wide total and the structural rows come from one snapshot and
// must describe one measurement.
func TestGate06RoleTotalMustEqualStructuralSum(t *testing.T) {
	snapshot := snapshotOf(t, "after", []ObserverStructuralRow{
		{StrictASTSHA256: strings.Repeat("a1", 32), TopLevel: true, Calls: 3},
	})
	snapshot.Total += 2
	if err := snapshot.Validate(); err == nil {
		t.Fatal("a role total disagreeing with its own structural rows was accepted")
	}
}

// Gate 7. Both halves: a counter that goes backwards, and a key that vanishes.
func TestGate07CounterRegressionOrDisappearanceRejected(t *testing.T) {
	t.Run("cumulative regression", func(t *testing.T) {
		window, classifier, _ := pairedNovelWindow(t)
		// The before snapshot already holds counts the after snapshot undercuts.
		window.Before = snapshotOf(t, "before", window.After.Structural)
		window.After = snapshotOf(t, "after", nil)
		requireWindowRejected(t, window, classifier)
	})
	t.Run("disappearing structural key", func(t *testing.T) {
		window, classifier, _ := pairedNovelWindow(t)
		rows := window.After.Structural
		if len(rows) < 2 {
			t.Fatalf("precondition: only %d structural rows", len(rows))
		}
		window.Before = snapshotOf(t, "before", rows)
		window.After = snapshotOf(t, "after", rows[1:])
		err := requireWindowRejected(t, window, classifier)
		if !strings.Contains(err.Error(), "disappeared") && !strings.Contains(err.Error(), "backwards") {
			t.Fatalf("the rejection %q names neither disappearance nor regression", err)
		}
	})
}

// Gate 8.
func TestGate08StatsResetMutationRejected(t *testing.T) {
	window, classifier, _ := pairedNovelWindow(t)
	window.After.StatsReset = "2026-08-04 12:00:00+00"
	err := requireWindowRejected(t, window, classifier)
	if !strings.Contains(err.Error(), "reset") {
		t.Fatalf("the rejection %q does not name the reset", err)
	}
}

// Gate 9. Both a change across the window and a non-zero value on its own: a
// non-zero dealloc means entries may have been evicted, so the census this
// window rests on may simply be missing rows.
func TestGate09DeallocMutationRejected(t *testing.T) {
	t.Run("dealloc changed", func(t *testing.T) {
		window, classifier, _ := pairedNovelWindow(t)
		window.After.Dealloc = 3
		requireWindowRejected(t, window, classifier)
	})
	t.Run("non-zero dealloc throughout", func(t *testing.T) {
		window, classifier, _ := pairedNovelWindow(t)
		window.Before.Dealloc, window.After.Dealloc = 5, 5
		window.Before = resealSnapshot(t, window.Before)
		window.After = resealSnapshot(t, window.After)
		err := requireWindowRejected(t, window, classifier)
		if !strings.Contains(err.Error(), "evicted") {
			t.Fatalf("the rejection %q does not name eviction", err)
		}
	})
}

// Gate 10.
func TestGate10MeasurementEnvironmentMutationRejected(t *testing.T) {
	for name, mutate := range map[string]func(*MeasurementEnvironment){
		"version":        func(e *MeasurementEnvironment) { e.PostgreSQLVersionNum = 150000 },
		"track":          func(e *MeasurementEnvironment) { e.Track = "top" },
		"track_utility":  func(e *MeasurementEnvironment) { e.TrackUtility = "off" },
		"track_planning": func(e *MeasurementEnvironment) { e.TrackPlanning = "on" },
	} {
		t.Run(name, func(t *testing.T) {
			window, classifier, _ := pairedNovelWindow(t)
			mutate(&window.After.Environment)
			requireWindowRejected(t, window, classifier)
		})
	}
}

// Gate 11.
func TestGate11PostgreSQLRuntimeMutationRejected(t *testing.T) {
	for name, mutate := range map[string]func(*ObserverSnapshotV2){
		"image reference": func(s *ObserverSnapshotV2) {
			s.Runtime.PostgreSQL.ImageReference = "postgres@sha256:" + strings.Repeat("a", 64)
		},
		"repo digest": func(s *ObserverSnapshotV2) {
			// Must differ from the fixture's own digest, or this is a no-op
			// dressed as a mutation and the gate proves nothing.
			s.Runtime.PostgreSQL.RepoDigest = "postgres@sha256:" + strings.Repeat("b", 64)
		},
		"container image id": func(s *ObserverSnapshotV2) {
			// The container and the named image must resolve together, so both
			// move: what the window rejects is a different but valid deployment.
			s.Runtime.PostgreSQL.LocalImageID = "sha256:" + strings.Repeat("c", 64)
			s.Runtime.PostgreSQL.ContainerImageID = s.Runtime.PostgreSQL.LocalImageID
		},
		"platform": func(s *ObserverSnapshotV2) { s.Runtime.PostgreSQL.Platform = "linux/arm64" },
		"server restart": func(s *ObserverSnapshotV2) {
			s.Resource.PostmasterStartTime = "2026-08-04 13:00:00+00"
		},
	} {
		t.Run(name, func(t *testing.T) {
			window, classifier, _ := pairedNovelWindow(t)
			mutate(&window.After)
			requireWindowRejected(t, window, classifier)
		})
	}
}

// Gate 12. Each mutation reseals the Gateway identity, so what the window
// rejects is a genuinely different but internally valid Gateway rather than a
// corrupted struct.
func TestGate12GatewayRuntimeMutationRejected(t *testing.T) {
	for name, mutate := range map[string]func(*GatewayRuntimeIdentityV1){
		"submission commit": func(i *GatewayRuntimeIdentityV1) {
			// The image label and the build must name one commit, so both move.
			i.SubmissionCommit = strings.Repeat("cd", 20)
			i.OCIRevisionLabel = i.SubmissionCommit
		},
		"build context":   func(i *GatewayRuntimeIdentityV1) { i.BuildContextSHA256 = strings.Repeat("8", 64) },
		"source manifest": func(i *GatewayRuntimeIdentityV1) { i.SourceManifestSHA256 = strings.Repeat("9", 64) },
		"image id": func(i *GatewayRuntimeIdentityV1) {
			i.LocalImageID = "sha256:" + strings.Repeat("d", 64)
			i.ContainerImageID = i.LocalImageID
		},
		"binary": func(i *GatewayRuntimeIdentityV1) { i.BinarySHA256 = strings.Repeat("e", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			window, classifier, _ := pairedNovelWindow(t)
			window.After.Runtime.Gateway = reseal(t, window.After.Runtime.Gateway, mutate)
			requireWindowRejected(t, window, classifier)
		})
	}
	t.Run("container restart", func(t *testing.T) {
		window, classifier, _ := pairedNovelWindow(t)
		window.After.Resource.GatewayRestartCount = 1
		requireWindowRejected(t, window, classifier)
	})
	t.Run("OOM", func(t *testing.T) {
		window, classifier, _ := pairedNovelWindow(t)
		window.After.Resource.GatewayOOMKilled = true
		requireWindowRejected(t, window, classifier)
	})
}

// Gate 13.
func TestGate13FormalHealthcheckMutationRejected(t *testing.T) {
	approved := FormalGatewayHealthcheck()
	for name, healthcheck := range map[string]GatewayHealthcheck{
		"command":  {Test: []string{"CMD", "curl", "http://127.0.0.1:8082/health/ready"}, Interval: approved.Interval, Timeout: approved.Timeout, Retries: approved.Retries},
		"interval": {Test: approved.Test, Interval: "30s", Timeout: approved.Timeout, Retries: approved.Retries},
		"timeout":  {Test: approved.Test, Interval: approved.Interval, Timeout: "9s", Retries: approved.Retries},
		"retries":  {Test: approved.Test, Interval: approved.Interval, Timeout: approved.Timeout, Retries: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := healthcheck.Validate(); err == nil {
				t.Fatalf("a healthcheck with a drifted %s was approved", name)
			}
		})
	}
	// And the identity carrying it must move the window's runtime identity.
	window, classifier, _ := pairedNovelWindow(t)
	window.After.Runtime.Gateway = reseal(t, window.After.Runtime.Gateway,
		func(i *GatewayRuntimeIdentityV1) {
			i.HealthcheckSHA256 = healthcheckDigest(`["CMD","curl","http://127.0.0.1:8082/health/ready"]`)
		})
	requireWindowRejected(t, window, classifier)
}

// Gate 14.
func TestGate14SameTotalControlSubstitutionRejected(t *testing.T) {
	window, classifier, plan := pairedNovelWindow(t)
	manifest := compiledTestManifest(t, pairedTargets(t)...)
	var safety, representation string
	for _, entry := range manifest.Entries {
		switch entry.Class {
		case V3SafetySessionPin:
			safety = entry.StrictASTSHA256
		case V3RepresentationPin:
			representation = entry.StrictASTSHA256
		}
	}
	if safety == "" || representation == "" {
		t.Fatal("precondition: the manifest lacks a safety or representation pin")
	}
	// Drop the safety pin and double the representation pin: the total is
	// unchanged and every aggregate stays put.
	for index, row := range window.After.Structural {
		if row.StrictASTSHA256 == safety {
			window.After.Structural[index].Calls = 0
		}
		if row.StrictASTSHA256 == representation {
			window.After.Structural[index].Calls += 1
		}
	}
	delta, err := window.Delta(classifier)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.Total != plan.ExpectedTotal() {
		t.Fatalf("precondition: the substitution changed the total (%d vs %d)",
			delta.Total, plan.ExpectedTotal())
	}
	if err := delta.Accept(plan); err == nil {
		t.Fatal("a same-total control substitution was accepted")
	}
}

// Gate 15.
func TestGate15SameTotalInternalKeySubstitutionRejected(t *testing.T) {
	window, classifier, plan := pairedNovelWindow(t)
	qualified := qualifiedFootprint(t).InternalKeys()[0]
	substitute := strings.Repeat("b", 64)
	for index, row := range window.After.Structural {
		if row.StrictASTSHA256 == qualified && !row.TopLevel {
			window.After.Structural[index].StrictASTSHA256 = substitute
			break
		}
	}
	sortStructural(window.After.Structural)
	delta, err := window.Delta(classifier)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.Total != plan.ExpectedTotal() {
		t.Fatalf("precondition: the substitution changed the total (%d vs %d)",
			delta.Total, plan.ExpectedTotal())
	}
	if err := delta.Accept(plan); err == nil {
		t.Fatal("one qualified internal key was replaced by another at the same total and accepted")
	}
}

// Gate 16.
func TestGate16MissingRequiredControlRejected(t *testing.T) {
	window, classifier, plan := pairedNovelWindow(t)
	window.After.Structural[0].Calls = 0
	// Recompute rather than decrement: the row may have expected more than one
	// call, and a total that disagrees with its rows would be rejected by the
	// atomicity invariant instead of by the missing control.
	window.After = resealSnapshot(t, window.After)
	delta, err := window.Delta(classifier)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if err := delta.Accept(plan); err == nil {
		t.Fatal("a class below its exact expected multiplicity was accepted")
	}
}

// Gate 17. Both above-multiplicity and absent-from-the-classifier.
func TestGate17UnexpectedStatementRejected(t *testing.T) {
	t.Run("above expected multiplicity", func(t *testing.T) {
		window, classifier, plan := pairedNovelWindow(t)
		window.After.Structural[0].Calls++
		window.After = resealSnapshot(t, window.After)
		delta, err := window.Delta(classifier)
		if err != nil {
			t.Fatalf("delta: %v", err)
		}
		if err := delta.Accept(plan); err == nil {
			t.Fatal("a class above its exact expected multiplicity was accepted")
		}
	})
	t.Run("absent from the classifier", func(t *testing.T) {
		window, classifier, plan := pairedNovelWindow(t)
		intruder := strings.Repeat("e", 64)
		window.After.Structural = append(window.After.Structural,
			ObserverStructuralRow{StrictASTSHA256: intruder, TopLevel: true, Calls: 1})
		sortStructural(window.After.Structural)
		window.After.Total++
		delta, err := window.Delta(classifier)
		if err != nil {
			t.Fatalf("delta: %v", err)
		}
		err = delta.Accept(plan)
		if err == nil {
			t.Fatal("a structural key no classifier entry matches was accepted")
		}
		if !strings.Contains(err.Error(), shortDigest(intruder)) {
			t.Fatalf("the rejection %q does not name the intruding key", err)
		}
	})
}

// Gate 20. A target that is perfectly valid for another operation must not
// classify here, even though its SQL structure is otherwise legitimate.
func TestGate20AnotherWorkloadTargetRejected(t *testing.T) {
	foreign, err := TargetEntry(V3TargetedVisible, "SELECT 1 FROM reporting.other_workload",
		"artifact/another-cell/100x4")
	if err != nil {
		t.Fatalf("build a foreign target: %v", err)
	}
	manifest := compiledTestManifest(t, foreign)
	if _, err := CompileClassifier(testOperation(t, PathPairedNovel), manifest); err == nil {
		t.Fatal("a target belonging to another workload compiled for this operation")
	}
}

// Gate 23.
func TestGate23AdapterWrongPlanRejected(t *testing.T) {
	for name, mutate := range map[string]func(*CarriedEvidenceV3){
		"another path": func(c *CarriedEvidenceV3) { c.Plan.PathKind = PathSingleQuery },
		"edited internal expectation": func(c *CarriedEvidenceV3) {
			c.Plan.InternalExpectation = []InternalExpectation{
				{StrictASTSHA256: testInternalKeyB, Calls: 2},
			}
		},
		"another schema entry count": func(c *CarriedEvidenceV3) { c.Plan.ExpectedSchemaEntries += 1 },
	} {
		t.Run(name, func(t *testing.T) {
			broken := pairedNovelCase(t)
			mutate(&broken.carried)
			if _, err := broken.finalize(); err == nil {
				t.Fatalf("a carried plan with %s was accepted", name)
			}
		})
	}
}

// Gate 24.
func TestGate24AdapterWrongManifestRejected(t *testing.T) {
	for name, mutate := range map[string]func(*CarriedEvidenceV3){
		"substituted manifest": func(c *CarriedEvidenceV3) {
			c.ClassifierManifestSHA256 = strings.Repeat("a", 64)
		},
		"substituted binding": func(c *CarriedEvidenceV3) {
			c.ClassifierBindingSHA256 = strings.Repeat("b", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			broken := pairedNovelCase(t)
			mutate(&broken.carried)
			if _, err := broken.finalize(); err == nil {
				t.Fatalf("a carried classifier with a %s was accepted", name)
			}
		})
	}
}

// Gate 26. A baseline arm issues its own SQL by its own means; a window around
// it would describe statements no TaskGate plan models, so letting it carry
// observer evidence would let a baseline manufacture the accounting the TaskGate
// arm is judged by.
func TestGate26BaselineArmObserverEvidenceRejected(t *testing.T) {
	for _, arm := range []MeasurementArm{ArmDirectPostgres, ArmNativeProvSQL, MeasurementArm("")} {
		t.Run(string(arm), func(t *testing.T) {
			if arm.CarriesObserverEvidence() {
				t.Fatalf("arm %q claims it may carry observer evidence", arm)
			}
			broken := pairedNovelCase(t)
			broken.carried.Arm = arm
			if _, err := broken.finalize(); err == nil {
				t.Fatalf("arm %q carried TaskGate observer evidence and was accepted", arm)
			}
		})
	}
}

// Gate 29. Legacy accounting cannot satisfy v3 acceptance even when its own
// counters are internally consistent.
func TestGate29LegacyV14EvidenceRejected(t *testing.T) {
	if ObserverAccountingVersion == ObserverAccountingV3Version {
		t.Fatal("the v1.4/v2 and v3 accounting versions are the same string")
	}
	t.Run("a plan wearing the v1.4/v2 version", func(t *testing.T) {
		broken := pairedNovelCase(t)
		broken.carried.Plan.Version = ObserverAccountingVersion
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a carried plan wearing the legacy accounting version was accepted")
		}
	})
	t.Run("an internally consistent v1.4 accounting", func(t *testing.T) {
		// The legacy document validates under its own rules...
		legacy := ObserverAccounting{
			Version: ObserverAccountingVersion,
			Plan:    NewGatewayControlPlan(2, 1, 1, 1),
			Before:  NewGatewayStatementCensus(), After: NewGatewayStatementCensus(),
		}
		if legacy.Plan.Validate() != nil {
			t.Fatal("precondition: the legacy plan is not internally consistent")
		}
		// ...and still cannot satisfy v3, whose plan validator requires its own
		// version before any number is compared.
		v3 := GatewayControlPlanV3{Version: legacy.Version, PathKind: PathPairedNovel}
		if err := v3.Validate(); err == nil {
			t.Fatal("a v3 plan wearing the legacy version validated")
		}
	})
	t.Run("a v1 observer snapshot", func(t *testing.T) {
		broken := pairedNovelCase(t)
		broken.carried.Window.After.Version = "taskgate-final-v5-observer-snapshot-v1"
		if _, err := broken.finalize(); err == nil {
			t.Fatal("legacy observer evidence was accepted on the v3 path")
		}
	})
}

// Gate 30. Every binding digest, end to end.
func TestGate30BindingDigestMutationRejected(t *testing.T) {
	t.Run("operation, manifest and classifier", func(t *testing.T) {
		operation := testOperation(t, PathPairedNovel)
		manifest := compiledTestManifest(t, pairedTargets(t)...)
		classifier, err := CompileClassifier(operation, manifest)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		manifestDigest, err := manifest.SHA256()
		if err != nil {
			t.Fatalf("manifest digest: %v", err)
		}
		for name, mutate := range map[string]func(*OperationIdentity){
			"operation id":     func(o *OperationIdentity) { o.OperationID = "operation-0002" },
			"path kind":        func(o *OperationIdentity) { o.PathKind = PathSingleQuery },
			"contract":         func(o *OperationIdentity) { o.ContractIdentity = "artifact/other/100x4" },
			"ExpectedSchema":   func(o *OperationIdentity) { o.ExpectedSchemaDigest = strings.Repeat("a", 64) },
			"footprint digest": func(o *OperationIdentity) { o.AttestationFootprintSHA256 = strings.Repeat("b", 64) },
		} {
			t.Run(name, func(t *testing.T) {
				mutated := operation
				mutate(&mutated)
				if err := classifier.RequireBinding(mutated, manifestDigest); err == nil {
					t.Fatalf("the binding ignored a changed %s", name)
				}
			})
		}
		if err := classifier.RequireBinding(operation, strings.Repeat("c", 64)); err == nil {
			t.Fatal("the binding ignored a substituted manifest digest")
		}
	})

	t.Run("execution binding and pre-state", func(t *testing.T) {
		// QueryExecutionBindingV1.Validate recomputes the binding digest over
		// every member, so a stale digest is fatal wherever it came from.
		base := pairedNovelCase(t)
		binding := *base.receipt.ExecutionBinding
		binding.SHA256 = strings.Repeat("d", 64)
		if err := binding.Validate(); err == nil {
			t.Fatal("an execution binding with a substituted digest validated")
		}
		ledger := *base.receipt.ExposureLedgerBefore
		ledger.SHA256 = strings.Repeat("e", 64)
		if err := ledger.Validate(); err == nil {
			t.Fatal("an exposure pre-state with a substituted digest validated")
		}
	})

	t.Run("window, runtime and footprint", func(t *testing.T) {
		for name, mutate := range map[string]func(*ObserverSnapshotV2){
			"window id":          func(s *ObserverSnapshotV2) { s.ObserverWindowID = strings.Repeat("1", 64) },
			"classifier digest":  func(s *ObserverSnapshotV2) { s.ClassifierManifestSHA256 = strings.Repeat("2", 64) },
			"operation binding":  func(s *ObserverSnapshotV2) { s.OperationBindingSHA256 = strings.Repeat("3", 64) },
			"observer source":    func(s *ObserverSnapshotV2) { s.ObserverSourceSHA256 = strings.Repeat("4", 64) },
			"PostgreSQL runtime": func(s *ObserverSnapshotV2) { s.Runtime.PostgreSQL.Platform = "linux/arm64" },
		} {
			t.Run(name, func(t *testing.T) {
				window, classifier, _ := pairedNovelWindow(t)
				mutate(&window.After)
				requireWindowRejected(t, window, classifier)
			})
		}
		broken := pairedNovelCase(t)
		broken.trusted.Footprint.ExpectedSchemaDigest = strings.Repeat("f", 64)
		if _, err := broken.finalize(); err == nil {
			t.Fatal("a footprint bound to another ExpectedSchema was accepted")
		}
	})
}

// requireWindowRejected fails the test unless the window is refused, and returns
// the refusal so a caller can assert what it named.
func requireWindowRejected(t *testing.T, window ObserverWindowV2, classifier *CompiledClassifier) error {
	t.Helper()
	_, err := window.Delta(classifier)
	if err == nil {
		t.Fatal("a window that violates an invariant was accepted")
	}
	return err
}

// resealSnapshot recomputes a snapshot's total so a mutation under test is not
// masked by the role-total invariant firing first.
func resealSnapshot(t *testing.T, snapshot ObserverSnapshotV2) ObserverSnapshotV2 {
	t.Helper()
	var total int64
	for _, row := range snapshot.Structural {
		total += row.Calls
	}
	snapshot.Total = total
	return snapshot
}
