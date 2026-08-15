package experiment

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testInvocationV3(t *testing.T) ObserverInvocationV3 {
	t.Helper()
	return ObserverInvocationV3{
		Phase: "before", ObserverWindowID: strings.Repeat("a1", 32),
		ClassifierManifestSHA256: strings.Repeat("b2", 32),
	}
}

// The argv is the contract with the observer binary, and it is asserted
// literally rather than reconstructed, so a renamed or dropped flag fails here
// instead of at the observer's own argument parser during a measured run.
func TestObserverArgvIsTheFlagsTheObserverRequires(t *testing.T) {
	invocation := testInvocationV3(t)
	argv, err := invocation.Argv("/usr/local/bin/final-v5-observer")
	if err != nil {
		t.Fatalf("build argv: %v", err)
	}
	want := []string{
		"/usr/local/bin/final-v5-observer",
		"--phase", "before",
		"--observer-window-id", invocation.ObserverWindowID,
		"--classifier-manifest-sha256", invocation.ClassifierManifestSHA256,
	}
	if len(argv) != len(want) {
		t.Fatalf("argv is %v, want %v", argv, want)
	}
	for index := range want {
		if argv[index] != want[index] {
			t.Fatalf("argv[%d] is %q, want %q", index, argv[index], want[index])
		}
	}
	// The optional flag appears only when it has a value, because the observer
	// rejects an empty one rather than ignoring it.
	invocation.OperationBindingSHA256 = strings.Repeat("c3", 32)
	argv, err = invocation.Argv("/usr/local/bin/final-v5-observer")
	if err != nil {
		t.Fatalf("build argv with an operation binding: %v", err)
	}
	if len(argv) != len(want)+2 || argv[len(argv)-2] != "--operation-binding-sha256" {
		t.Fatalf("argv does not carry the operation binding: %v", argv)
	}
}

// An invocation the observer would refuse is refused here, where the failure can
// say which value is wrong.
//
// The empty-commitment case is the one that matters: an observer window opened
// without a classifier digest is a window committed to nothing, and Delta will
// later refuse to classify it. Catching it at the invocation means the run fails
// before it takes a measurement it cannot use.
func TestAnObserverInvocationTheObserverWouldRefuseIsRefusedHere(t *testing.T) {
	for name, mutate := range map[string]func(*ObserverInvocationV3){
		"no phase":                 func(i *ObserverInvocationV3) { i.Phase = "" },
		"a phase that is not one":  func(i *ObserverInvocationV3) { i.Phase = "during" },
		"no window id":             func(i *ObserverInvocationV3) { i.ObserverWindowID = "" },
		"a truncated window id":    func(i *ObserverInvocationV3) { i.ObserverWindowID = "abc123" },
		"an uppercase window id":   func(i *ObserverInvocationV3) { i.ObserverWindowID = strings.ToUpper(strings.Repeat("a1", 32)) },
		"no classifier commitment": func(i *ObserverInvocationV3) { i.ClassifierManifestSHA256 = "" },
		"a malformed binding":      func(i *ObserverInvocationV3) { i.OperationBindingSHA256 = "not-a-digest" },
	} {
		t.Run(name, func(t *testing.T) {
			invocation := testInvocationV3(t)
			mutate(&invocation)
			if err := invocation.Validate(); err == nil {
				t.Fatalf("an invocation with %s was accepted", name)
			}
			if _, err := invocation.Argv("/usr/local/bin/final-v5-observer"); err == nil {
				t.Fatalf("an argv was built for an invocation with %s", name)
			}
		})
	}
}

// A snapshot must describe the invocation that produced it.
//
// Without this the observer could return a reading of some other window, or of
// some other phase, and the downstream window checks would compare two documents
// that agree with each other about something neither was asked for.
func TestASnapshotMustDescribeTheInvocationThatProducedIt(t *testing.T) {
	invocation := testInvocationV3(t)
	honest := snapshotOf(t, "before", nil)
	honest.ObserverWindowID = invocation.ObserverWindowID
	honest.ClassifierManifestSHA256 = invocation.ClassifierManifestSHA256
	honest.OperationBindingSHA256 = invocation.OperationBindingSHA256
	if err := requireSnapshotMatchesInvocationV3(honest, invocation); err != nil {
		t.Fatalf("an honest snapshot was rejected, so the cases below prove nothing: %v", err)
	}
	for name, mutate := range map[string]func(*ObserverSnapshotV2){
		"another phase":    func(s *ObserverSnapshotV2) { s.Phase = "after" },
		"another window":   func(s *ObserverSnapshotV2) { s.ObserverWindowID = strings.Repeat("9", 64) },
		"another manifest": func(s *ObserverSnapshotV2) { s.ClassifierManifestSHA256 = strings.Repeat("8", 64) },
		"an added binding": func(s *ObserverSnapshotV2) { s.OperationBindingSHA256 = strings.Repeat("7", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := honest
			mutate(&snapshot)
			if err := requireSnapshotMatchesInvocationV3(snapshot, invocation); err == nil {
				t.Fatalf("a snapshot reporting %s was accepted", name)
			}
		})
	}
}

// Observer failures are written only to the Adapter's stderr. The runner
// suppresses that stream from samples and ordinary output, while the explicitly
// enabled targeted-Artifact diagnostic channel can retain it in its private
// create-exclusive file. Preserve the source-built observer's exact stderr here
// so that channel can expose the real failure rather than only exit status 1.
func TestObserverExitPreservesItsStderrForTheControlledDiagnosticChannel(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "observer")
	const stderr = "final-v5 observer: exact diagnostic from the observer\nsecond line\n"
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '"+stderr+"' >&2\nexit 23\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := RunObserverV2(context.Background(), executable, testInvocationV3(t),
		[]string{"PATH=/usr/bin:/bin"})
	if err == nil {
		t.Fatal("an observer that exited unsuccessfully was accepted")
	}
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 23 {
		t.Fatalf("observer error does not retain exit status 23: %v", err)
	}
	want := "run observer before: exit status 23\nobserver stderr:\n" + stderr
	if err.Error() != want {
		t.Fatalf("observer error = %q, want exact stderr-bearing error %q", err, want)
	}
}
