package main

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const (
	internalKey = "e5738df1650276a7f20e677172e067bc62bab12d48c18a378c9b6ed602433842"
	otherKey    = "3cfbbde6160f50e1d80a3302c6f6a95426c191405290b3d6c54980d3e71c9f34"
	targetKey   = "aa11bb22cc33dd44ee55ff6677889900aabbccddeeff00112233445566778899"
)

// baseSnapshot is a healthy cumulative reading. Intervals are built by advancing
// a copy of it, so every invariant is exercised against a realistic pair.
func baseSnapshot(label string, index int) snapshot {
	return snapshot{
		Index: index, Label: label,
		calls:               map[structuralKey]int64{},
		queryIDs:            map[structuralKey]string{},
		StatsReset:          "2026-08-04 10:41:46.431868+00",
		Dealloc:             0,
		PostmasterStartTime: "2026-08-04 10:40:00+00",
		Environment:         experiment.RequiredMeasurementEnvironment(),
	}
}

// advance returns the next cumulative snapshot after adding the given calls.
func advance(from snapshot, label string, added map[structuralKey]int64) snapshot {
	to := baseSnapshot(label, from.Index+1)
	to.StatsReset, to.Dealloc = from.StatsReset, from.Dealloc
	to.PostmasterStartTime, to.Environment = from.PostmasterStartTime, from.Environment
	for key, calls := range from.calls {
		to.calls[key] = calls
		to.queryIDs[key] = from.queryIDs[key]
	}
	to.Total = from.Total
	for key, calls := range added {
		to.calls[key] += calls
		to.queryIDs[key] = "diagnosis-local"
		to.Total += calls
	}
	return to
}

func internal(digest string) structuralKey { return structuralKey{StrictASTSHA256: digest} }
func topLevel(digest string) structuralKey {
	return structuralKey{StrictASTSHA256: digest, TopLevel: true}
}

// One Attestation is isolated between two adjacent snapshots. Nothing is
// divided by an assumed attestations-per-trial.
func TestIntervalIsolatesExactlyOneAttestation(t *testing.T) {
	from := baseSnapshot("baseline", 0)
	to := advance(from, "explicit-preflight-0", map[structuralKey]int64{
		internal(internalKey): 1,
		topLevel(targetKey):   3,
	})
	interval, err := deltaBetween(from, to, experiment.AttestationScopeExplicitPreflightPool, 0)
	if err != nil {
		t.Fatalf("deltaBetween: %v", err)
	}
	if interval.Attestations != 1 {
		t.Fatalf("interval isolates %d attestations, want 1", interval.Attestations)
	}
	if len(interval.Internal) != 1 || interval.Internal[0].Calls != 1 {
		t.Fatalf("internal delta = %+v", interval.Internal)
	}
	if interval.TotalDelta != 4 || interval.StructuralSum != 4 {
		t.Fatalf("total=%d structural=%d, want 4 and 4", interval.TotalDelta, interval.StructuralSum)
	}
}

// Every invariant an interval depends on must invalidate it rather than skew a
// count silently.
func TestIntervalBindsItsInvariants(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*snapshot)
		want   string
	}{
		{"pg_stat_statements reset inside the window",
			func(s *snapshot) { s.StatsReset = "2026-08-04 11:00:00+00" }, "was reset inside the window"},
		{"entries evicted",
			func(s *snapshot) { s.Dealloc = 2 }, "evicted entries"},
		{"PostgreSQL restarted",
			func(s *snapshot) { s.PostmasterStartTime = "2026-08-04 12:00:00+00" }, "restarted inside the window"},
		{"measurement environment changed",
			func(s *snapshot) { s.Environment.Track = "top" }, "environment changed inside the window"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			from := baseSnapshot("baseline", 0)
			to := advance(from, "explicit-preflight-0", map[structuralKey]int64{internal(internalKey): 1})
			testCase.mutate(&to)
			_, err := deltaBetween(from, to, experiment.AttestationScopeExplicitPreflightPool, 0)
			if err == nil {
				t.Fatal("a violated interval invariant was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not name the violation %q", err, testCase.want)
			}
		})
	}
}

// The total delta must equal the sum of the classified structural rows, or a
// call was counted outside what the probe accounted for.
func TestIntervalRejectsAnUnaccountedCall(t *testing.T) {
	from := baseSnapshot("baseline", 0)
	to := advance(from, "explicit-preflight-0", map[structuralKey]int64{internal(internalKey): 1})
	to.Total += 5 // a call with no structural row behind it
	_, err := deltaBetween(from, to, experiment.AttestationScopeExplicitPreflightPool, 0)
	if err == nil {
		t.Fatal("an unaccounted call was accepted")
	}
	if !strings.Contains(err.Error(), "outside the classified rows") {
		t.Fatalf("error %q does not name the discrepancy", err)
	}
}

func TestIntervalRejectsCountsGoingBackwards(t *testing.T) {
	from := baseSnapshot("baseline", 0)
	from.calls[internal(internalKey)] = 5
	from.Total = 5
	to := advance(from, "explicit-preflight-0", nil)
	to.calls[internal(internalKey)] = 2
	to.Total = 2
	if _, err := deltaBetween(from, to, experiment.AttestationScopeExplicitPreflightPool, 0); err == nil {
		t.Fatal("a backwards cumulative count was accepted")
	}
}

func TestIntervalRejectsADisappearingKey(t *testing.T) {
	from := baseSnapshot("baseline", 0)
	from.calls[internal(internalKey)] = 5
	from.Total = 5
	to := baseSnapshot("explicit-preflight-0", 1)
	to.Total = 5
	if _, err := deltaBetween(from, to, experiment.AttestationScopeExplicitPreflightPool, 0); err == nil {
		t.Fatal("a key vanishing from pg_stat_statements was accepted")
	}
}

// intervalsFor builds agreeing intervals for one scope.
func intervalsFor(scope experiment.AttestationScope, calls int64, repetitions int) []measuredInterval {
	var intervals []measuredInterval
	for repetition := 0; repetition < repetitions; repetition++ {
		intervals = append(intervals, measuredInterval{
			Scope: scope, Repetition: repetition, Attestations: 1,
			Internal: []structuralEntry{{structuralKey: internal(internalKey), Calls: calls}},
		})
	}
	return intervals
}

func TestAgreedFootprintAcceptsStableIntervals(t *testing.T) {
	entries, stable, count, err := agreedFootprint(
		intervalsFor(experiment.AttestationScopeExplicitPreflightPool, 1, 3),
		experiment.AttestationScopeExplicitPreflightPool)
	if err != nil {
		t.Fatalf("agreedFootprint: %v", err)
	}
	if !stable || count != 3 {
		t.Fatalf("stable=%t count=%d, want true and 3", stable, count)
	}
	if len(entries) != 1 || entries[0].CallsPerAttestation != 1 {
		t.Fatalf("entries = %+v", entries)
	}
}

// Disagreement must stop the qualification, not be averaged, unioned or
// resolved by taking the last interval.
func TestAgreedFootprintRefusesWhenIntervalsDisagree(t *testing.T) {
	scope := experiment.AttestationScopeExplicitPreflightPool
	for name, mutate := range map[string]func([]measuredInterval){
		"a differing multiplicity": func(intervals []measuredInterval) {
			intervals[2].Internal[0].Calls = 2
		},
		"a differing structural key": func(intervals []measuredInterval) {
			intervals[2].Internal[0].StrictASTSHA256 = otherKey
		},
		"an extra internal statement": func(intervals []measuredInterval) {
			intervals[2].Internal = append(intervals[2].Internal,
				structuralEntry{structuralKey: internal(otherKey), Calls: 1})
		},
		"a missing internal statement": func(intervals []measuredInterval) {
			intervals[2].Internal = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			intervals := intervalsFor(scope, 1, 3)
			mutate(intervals)
			_, stable, _, err := agreedFootprint(intervals, scope)
			if err != nil {
				t.Fatalf("agreedFootprint: %v", err)
			}
			if stable {
				t.Fatal("disagreeing intervals were reported stable")
			}
		})
	}
}

// A scope with no interval at all must fail rather than yield a footprint silent
// about it.
func TestAgreedFootprintRefusesAnUnmeasuredScope(t *testing.T) {
	intervals := intervalsFor(experiment.AttestationScopeExplicitPreflightPool, 1, 3)
	if _, _, _, err := agreedFootprint(intervals,
		experiment.AttestationScopePairedQueryTransaction); err == nil {
		t.Fatal("a scope with no interval was accepted")
	}
}

// Intervals belonging to another scope must not be absorbed.
func TestAgreedFootprintIgnoresOtherScopes(t *testing.T) {
	intervals := append(
		intervalsFor(experiment.AttestationScopeExplicitPreflightPool, 1, 3),
		intervalsFor(experiment.AttestationScopePairedQueryTransaction, 9, 3)...)
	entries, stable, count, err := agreedFootprint(intervals,
		experiment.AttestationScopeExplicitPreflightPool)
	if err != nil {
		t.Fatalf("agreedFootprint: %v", err)
	}
	if !stable || count != 3 {
		t.Fatalf("stable=%t count=%d", stable, count)
	}
	if entries[0].CallsPerAttestation != 1 {
		t.Fatalf("the paired scope's 9 leaked into the preflight footprint: %+v", entries)
	}
}
