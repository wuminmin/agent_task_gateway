package experiment

import (
	"strings"
	"testing"
)

func testInvocationV3(t *testing.T) ObserverInvocationV3 {
	t.Helper()
	windowID, err := DeriveObserverWindowID("artifact-result-heavy-100x4-op-0001")
	if err != nil {
		t.Fatalf("derive observer window id: %v", err)
	}
	return ObserverInvocationV3{
		Phase: "before", ObserverWindowID: windowID,
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

// The window id is a name, and names have to be stable and distinct.
func TestTheObserverWindowIDIsDerivedFromTheOperation(t *testing.T) {
	first, err := DeriveObserverWindowID("operation-a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := DeriveObserverWindowID("operation-a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := DeriveObserverWindowID("operation-b")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("one operation produced two window ids; a before/after pair could not be tied together")
	}
	if first == other {
		t.Fatal("two operations share a window id; two measurements could be paired across each other")
	}
	if !isLowercaseSHA256(first) {
		t.Fatalf("the window id %q is not the lowercase SHA-256 the observer requires", first)
	}
	if _, err := DeriveObserverWindowID("  "); err == nil {
		t.Fatal("a window was named after no operation")
	}
}
