package queryplan

import "testing"

// The compiler identity is signed into every Query Execution Binding. A binding
// says "this plan, compiled by this compiler, produced these bytes"; if the
// compiler changes what it emits and the identity does not move, every binding
// signed afterwards makes a claim about a compiler that no longer exists.
//
// This test recomputes the digest from the compiler's own behaviour. It fails
// when probe SQL, either normal form, the predicate footprint, or any frozen
// contract version changes -- which is the point. Do NOT update the expectation
// to make it pass: bump CompilerVersion, so that old and new bindings are
// distinguishable, and then record the new value here.
func TestCompilerIdentityIsPinnedToItsSource(t *testing.T) {
	const want = "13fd7f3bf8c21209354d04b82c5006c5f29a5b0dd568b820fc2ef43a81f641ed"
	got, err := CompilerSHA256()
	if err != nil {
		t.Fatalf("compiler identity: %v", err)
	}
	if got != want {
		t.Fatalf("THE COMPILER'S BEHAVIOUR CHANGED: identity is %s, was %s.\n"+
			"Every Query Execution Binding signed under the old value claims this compiler "+
			"produced its statements. Bump CompilerVersion and record the new identity; "+
			"do not simply update this expectation.", got, want)
	}
}

// The identity must be a function of the compiler alone, not of the process.
func TestCompilerIdentityIsStable(t *testing.T) {
	first, err := CompilerSHA256()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 16; attempt++ {
		again, err := CompilerSHA256()
		if err != nil || again != first {
			t.Fatalf("compiler identity is not stable: %s then %s (%v)", first, again, err)
		}
	}
	if len(first) != 64 {
		t.Fatalf("compiler identity is not a SHA-256: %q", first)
	}
}

// The probe has to keep exercising the compiler. A probe that silently stopped
// compiling would leave the identity pinned to an error path.
func TestCompilerIdentityProbeStillCompiles(t *testing.T) {
	plan, product := compilerIdentityProbe()
	compiled, err := Compile(plan, product)
	if err != nil {
		t.Fatalf("the compiler identity probe no longer compiles: %v", err)
	}
	if compiled == "" {
		t.Fatal("the compiler identity probe compiled to nothing")
	}
}
