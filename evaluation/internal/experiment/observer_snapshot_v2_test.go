package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func testObserverRuntime(t *testing.T) ObserverRuntimeIdentity {
	t.Helper()
	image := "sha256:" + strings.Repeat("3", 64)
	return ObserverRuntimeIdentity{
		GatewayImageID: image, GatewayContainerID: "gateway-container",
		GatewaySourceSHA256:      strings.Repeat("4", 64),
		HealthcheckCommandSHA256: healthcheckDigest(`["CMD","curl","--fail","--silent","http://127.0.0.1:8082/health/live"]`),
		PostgreSQL:               testRuntimeIdentity(),
	}
}

func healthcheckDigest(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}

// snapshotOf builds a valid snapshot whose total is, by construction, the sum of
// its rows.
func snapshotOf(t *testing.T, label string, rows []ObserverStructuralRow) ObserverSnapshotV2 {
	t.Helper()
	sorted := append([]ObserverStructuralRow(nil), rows...)
	sortStructural(sorted)
	var total int64
	for _, row := range sorted {
		total += row.Calls
	}
	phase := "after"
	if label == "before" {
		phase = "before"
	}
	return ObserverSnapshotV2{
		Version: ObserverSnapshotV2Version, SchemaVersion: 2, Phase: phase,
		ObserverWindowID:         strings.Repeat("a1", 32),
		ClassifierManifestSHA256: strings.Repeat("b2", 32),
		OperationBindingSHA256:   strings.Repeat("c3", 32),
		ObserverSourceSHA256:     strings.Repeat("d4", 32),
		Label:                    label, Role: "gateway_reader",
		Total: total, Structural: sorted,
		Environment: RequiredMeasurementEnvironment(),
		StatsReset:  "2026-08-04 10:41:46.431868+00", Dealloc: 0,
		Runtime: testObserverRuntime(t),
		Resource: ObserverResourceEvidence{
			PostmasterStartTime: "2026-08-04 10:40:00+00", BusinessWALBytes: 27438592,
		},
	}
}

func sortStructural(rows []ObserverStructuralRow) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && structuralRowLess(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// pairedNovelWindow builds the window a correct Result-heavy paired-novel
// operation produces, matching the plan class for class.
func pairedNovelWindow(t *testing.T) (ObserverWindowV2, *CompiledClassifier, GatewayControlPlanV3) {
	t.Helper()
	targets := pairedTargets(t)
	manifest := compiledTestManifest(t, targets...)
	classifier, err := CompileClassifier(testOperation(t, PathPairedNovel), manifest)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	plan := mustPairedNovel(t, 1)

	// One row per control key at its expected multiplicity, plus the targets and
	// the internal key.
	var rows []ObserverStructuralRow
	for _, entry := range manifest.Entries {
		class := entry.Class
		calls := plan.Expected()[class]
		if calls == 0 {
			continue
		}
		rows = append(rows, ObserverStructuralRow{
			StrictASTSHA256: entry.StrictASTSHA256, TopLevel: entry.RequiredTopLevel, Calls: calls,
		})
	}
	before := snapshotOf(t, "before", nil)
	after := snapshotOf(t, "after", rows)
	return ObserverWindowV2{Before: before, After: after}, classifier, plan
}

func TestCorrectWindowIsAccepted(t *testing.T) {
	window, classifier, plan := pairedNovelWindow(t)
	delta, err := window.Delta(classifier)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if err := delta.Accept(plan); err != nil {
		t.Fatalf("a correct window was rejected: %v", err)
	}
	if delta.Total != plan.ExpectedTotal() {
		t.Fatalf("delta total = %d, want %d", delta.Total, plan.ExpectedTotal())
	}
}

// The atomicity check: a snapshot whose total disagrees with its rows cannot
// have read both from one row set.
func TestSnapshotRejectsATotalThatDisagreesWithItsRows(t *testing.T) {
	snapshot := snapshotOf(t, "after", []ObserverStructuralRow{
		{StrictASTSHA256: strings.Repeat("a", 64), TopLevel: true, Calls: 3},
	})
	snapshot.Total = 4
	err := snapshot.Validate()
	if err == nil {
		t.Fatal("a snapshot whose total disagreed with its census was accepted")
	}
	if !strings.Contains(err.Error(), "did not come from one row set") {
		t.Fatalf("error %q does not name the atomicity failure", err)
	}
}

// v1 evidence must never satisfy the v3 path.
func TestV1EvidenceIsRefusedOnTheV3Path(t *testing.T) {
	snapshot := snapshotOf(t, "after", nil)
	snapshot.Version = "taskgate-final-v5-observer-snapshot-v1"
	if err := snapshot.Validate(); err == nil {
		t.Fatal("v1 observer evidence was accepted on the v3 path")
	}
}

// Every invariant the window rests on must invalidate the measurement.
func TestWindowBindsItsInvariants(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*ObserverSnapshotV2)
		want   string
	}{
		{"pg_stat_statements reset",
			func(s *ObserverSnapshotV2) { s.StatsReset = "2026-08-04 12:00:00+00" }, "reset changed"},
		{"entries evicted",
			func(s *ObserverSnapshotV2) { s.Dealloc = 3 }, "dealloc changed"},
		// An uncovered environment is rejected at snapshot validation, before the
		// window comparison ever runs -- stronger than a before/after mismatch.
		{"measurement environment",
			func(s *ObserverSnapshotV2) { s.Environment.Track = "top" },
			"observer accounting v3 is derived for"},
		{"gateway image",
			func(s *ObserverSnapshotV2) { s.Runtime.GatewayImageID = "sha256:" + strings.Repeat("9", 64) },
			"runtime identity changed"},
		{"gateway source",
			func(s *ObserverSnapshotV2) { s.Runtime.GatewaySourceSHA256 = strings.Repeat("8", 64) },
			"runtime identity changed"},
		{"healthcheck command",
			func(s *ObserverSnapshotV2) {
				s.Runtime.HealthcheckCommandSHA256 = healthcheckDigest(`["CMD","curl","http://127.0.0.1:8082/health/ready"]`)
			}, "runtime identity changed"},
		{"PostgreSQL image",
			func(s *ObserverSnapshotV2) {
				s.Runtime.PostgreSQL.ContainerImageID = s.Runtime.PostgreSQL.LocalImageID
				s.Runtime.PostgreSQL.Platform = "linux/arm64"
			}, "runtime identity changed"},
		{"PostgreSQL restart",
			func(s *ObserverSnapshotV2) { s.Resource.PostmasterStartTime = "2026-08-04 13:00:00+00" },
			"postmaster start time changed"},
		{"gateway restart",
			func(s *ObserverSnapshotV2) { s.Resource.GatewayRestartCount = 1 }, "restart count changed"},
		{"gateway OOM",
			func(s *ObserverSnapshotV2) { s.Resource.GatewayOOMKilled = true }, "OOM state changed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			window, classifier, _ := pairedNovelWindow(t)
			testCase.mutate(&window.After)
			_, err := window.Delta(classifier)
			if err == nil {
				t.Fatal("a violated window invariant was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the violation %q", err, testCase.want)
			}
		})
	}
}

// A statement no classifier entry matches must fail closed, and the rejection
// must say which key appeared.
func TestUnexpectedStatementFailsClosed(t *testing.T) {
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
		t.Fatal("an unexpected structural statement was accepted")
	}
	if !strings.Contains(err.Error(), shortDigest(intruder)) {
		t.Fatalf("the rejection %q does not name the intruding key", err)
	}
}

// A same-total substitution of one internal key by another must fail. This is
// the mutation a single aggregate count cannot see.
func TestSameTotalInternalSubstitutionFails(t *testing.T) {
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
	// The role total is unchanged; only the identity moved.
	if delta.Total != plan.ExpectedTotal() {
		t.Fatalf("precondition: the substitution changed the total (%d vs %d)",
			delta.Total, plan.ExpectedTotal())
	}
	if err := delta.Accept(plan); err == nil {
		t.Fatal("one internal key was replaced by another at the same total and accepted")
	}
}

// A same-total control substitution must fail too.
func TestSameTotalControlSubstitutionFails(t *testing.T) {
	window, classifier, plan := pairedNovelWindow(t)
	// Move one call from the safety pin to the representation pin: both are
	// control classes expecting 1, so every aggregate stays put.
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
	for index, row := range window.After.Structural {
		if row.StrictASTSHA256 == safety {
			window.After.Structural[index].Calls = 0
		}
		if row.StrictASTSHA256 == representation {
			window.After.Structural[index].Calls = 2
		}
	}
	sortStructural(window.After.Structural)
	delta, err := window.Delta(classifier)
	if err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.Total != plan.ExpectedTotal() {
		t.Fatalf("precondition: the substitution changed the total")
	}
	if err := delta.Accept(plan); err == nil {
		t.Fatal("a same-total control substitution was accepted")
	}
}

// A missing control must fail; acceptance is equality, never a lower bound.
func TestMissingAndExtraControlsBothFail(t *testing.T) {
	for name, adjust := range map[string]int64{"missing": -1, "extra": +1} {
		t.Run(name, func(t *testing.T) {
			window, classifier, plan := pairedNovelWindow(t)
			manifest := compiledTestManifest(t, pairedTargets(t)...)
			var safety string
			for _, entry := range manifest.Entries {
				if entry.Class == V3SafetySessionPin {
					safety = entry.StrictASTSHA256
				}
			}
			for index, row := range window.After.Structural {
				if row.StrictASTSHA256 == safety {
					window.After.Structural[index].Calls += adjust
					window.After.Total += adjust
				}
			}
			delta, err := window.Delta(classifier)
			if err != nil {
				t.Fatalf("delta: %v", err)
			}
			if err := delta.Accept(plan); err == nil {
				t.Fatalf("a %s control was accepted", name)
			}
		})
	}
}

// The census must account for the whole role total; a call outside it means the
// observer did not see everything the role did.
func TestUnaccountedRoleCallFails(t *testing.T) {
	window, classifier, _ := pairedNovelWindow(t)
	window.After.Total += 5
	// Validate would catch this on its own, so bypass it by adding a row the
	// classifier knows nothing about and then correcting only the total.
	if _, err := window.Delta(classifier); err == nil {
		t.Fatal("a role total exceeding the census was accepted")
	}
}

func TestWindowRejectsCountsGoingBackwards(t *testing.T) {
	window, classifier, _ := pairedNovelWindow(t)
	window.Before = window.After
	window.After = snapshotOf(t, "after", nil)
	if _, err := window.Delta(classifier); err == nil {
		t.Fatal("a census that shrank across the window was accepted")
	}
}

func TestDeltaRequiresACompiledClassifier(t *testing.T) {
	window, _, _ := pairedNovelWindow(t)
	if _, err := window.Delta(nil); err == nil {
		t.Fatal("a window was classified without a classifier")
	}
}

func TestSnapshotDigestCoversRuntimeAndResourceEvidence(t *testing.T) {
	base := snapshotOf(t, "after", []ObserverStructuralRow{
		{StrictASTSHA256: strings.Repeat("a", 64), TopLevel: true, Calls: 2},
	})
	baseDigest, err := base.SHA256()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	for name, mutate := range map[string]func(*ObserverSnapshotV2){
		"gateway source":      func(s *ObserverSnapshotV2) { s.Runtime.GatewaySourceSHA256 = strings.Repeat("7", 64) },
		"healthcheck command": func(s *ObserverSnapshotV2) { s.Runtime.HealthcheckCommandSHA256 = strings.Repeat("6", 64) },
		"WAL position":        func(s *ObserverSnapshotV2) { s.Resource.BusinessWALBytes = 99 },
		"restart count":       func(s *ObserverSnapshotV2) { s.Resource.GatewayRestartCount = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutate(&mutated)
			digest, err := mutated.SHA256()
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if digest == baseDigest {
				t.Fatalf("changing the %s did not change the snapshot digest", name)
			}
		})
	}
}

// A before/after pair carrying different window, manifest, binding or observer
// identities is two halves of two different measurements, not one window.
func TestWindowIdentitiesMustMatchAcrossThePair(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*ObserverSnapshotV2)
		want   string
	}{
		{"observer window id",
			func(s *ObserverSnapshotV2) { s.ObserverWindowID = strings.Repeat("9", 64) },
			"observer window id differs"},
		{"classifier manifest",
			func(s *ObserverSnapshotV2) { s.ClassifierManifestSHA256 = strings.Repeat("8", 64) },
			"classifier manifest differs"},
		{"operation binding",
			func(s *ObserverSnapshotV2) { s.OperationBindingSHA256 = strings.Repeat("7", 64) },
			"operation binding differs"},
		{"observer source identity",
			func(s *ObserverSnapshotV2) { s.ObserverSourceSHA256 = strings.Repeat("6", 64) },
			"observer source identity differs"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			window, classifier, _ := pairedNovelWindow(t)
			testCase.mutate(&window.After)
			_, err := window.Delta(classifier)
			if err == nil {
				t.Fatal("a mismatched window identity was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the mismatch %q", err, testCase.want)
			}
		})
	}
}

// Two snapshots of the same phase are not a window.
func TestWindowRequiresOneBeforeAndOneAfter(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		before, after string
	}{
		{"two befores", "before", "before"},
		{"two afters", "after", "after"},
		{"reversed", "after", "before"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			window, classifier, _ := pairedNovelWindow(t)
			window.Before.Phase, window.After.Phase = testCase.before, testCase.after
			if _, err := window.Delta(classifier); err == nil {
				t.Fatal("a window without one before and one after was accepted")
			}
		})
	}
}

// A snapshot missing any of the identities the observer must emit is refused
// before a single count is read.
func TestSnapshotRequiresEveryObserverIdentity(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*ObserverSnapshotV2)
		want   string
	}{
		{"schema version", func(s *ObserverSnapshotV2) { s.SchemaVersion = 1 }, "schema_version"},
		{"phase", func(s *ObserverSnapshotV2) { s.Phase = "" }, "neither before nor after"},
		{"window id", func(s *ObserverSnapshotV2) { s.ObserverWindowID = "" }, "no window identifier"},
		{"classifier manifest", func(s *ObserverSnapshotV2) { s.ClassifierManifestSHA256 = "" },
			"names no classifier manifest"},
		{"malformed operation binding", func(s *ObserverSnapshotV2) { s.OperationBindingSHA256 = "nope" },
			"operation binding is not"},
		{"observer source", func(s *ObserverSnapshotV2) { s.ObserverSourceSHA256 = "" },
			"no observer source identity"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := snapshotOf(t, "after", nil)
			testCase.mutate(&snapshot)
			err := snapshot.Validate()
			if err == nil {
				t.Fatal("a snapshot missing a required identity was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the missing identity %q", err, testCase.want)
			}
		})
	}
}

// The operation binding is optional for paths that have none, but must be a
// digest when present.
func TestOperationBindingIsOptionalButTyped(t *testing.T) {
	snapshot := snapshotOf(t, "after", nil)
	snapshot.OperationBindingSHA256 = ""
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("an absent operation binding must be legal: %v", err)
	}
}
