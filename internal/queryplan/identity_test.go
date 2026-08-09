package queryplan

import (
	"strings"
	"testing"
)

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
	const want = "f83c42413d9c38b4cddafb91c7fa3bf7f49f34e2deb9ecaac12f55f75b0a1cb6"
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

func TestCompilerIdentityGroupedRelationalProbeStillCompiles(t *testing.T) {
	plan, products := compilerIdentityGroupedRelationalProbe()
	compiled, err := CompileRelational(plan, products)
	if err != nil {
		t.Fatalf("the grouped relational identity probe no longer compiles: %v", err)
	}
	if !strings.Contains(compiled.VisibleSQL, "CAST(") ||
		!strings.Contains(compiled.VisibleSQL, " AS text)") ||
		!strings.Contains(compiled.VisibleSQL, " ORDER BY ") {
		t.Fatalf("the grouped relational identity probe lost its encoding or delivery order: %s", compiled.VisibleSQL)
	}
	for _, order := range plan.OrderBy {
		alias := compiled.OutputAliases[order.Column]
		if alias == "" || !strings.Contains(compiled.VisibleSQL, `"`+alias+`" `+strings.ToUpper(order.Direction)) {
			t.Fatalf("the grouped relational identity probe lost order %q: %s", order.Column, compiled.VisibleSQL)
		}
	}
	if strings.Contains(compiled.ProvenanceSQL, "CAST(") || !strings.Contains(compiled.ProvenanceSQL, " ORDER BY ") {
		t.Fatalf("the grouped relational identity probe lost its independent provenance order: %s", compiled.ProvenanceSQL)
	}
	semantic, err := SemanticNormalFormV4(plan, compiled, products)
	if err != nil || semantic.SHA256 == "" {
		t.Fatalf("the grouped relational identity probe lacks a semantic digest: %+v, %v", semantic, err)
	}
	programJSON, err := compiled.OrdinalProgram.CanonicalJSON()
	if err != nil || len(programJSON) == 0 {
		t.Fatalf("the grouped relational identity probe lacks a canonical ordinal program: %q, %v", programJSON, err)
	}
	programDigest, err := compiled.OrdinalProgram.Digest()
	if err != nil || len(programDigest) != 64 {
		t.Fatalf("the grouped relational identity probe lacks an ordinal digest: %q, %v", programDigest, err)
	}
}
