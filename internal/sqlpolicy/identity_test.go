package sqlpolicy

import (
	"strings"
	"testing"
)

// The renderer identity is signed into every target record of a Query Execution
// Binding, beside the exact digest of the bytes it produced. If the renderer
// changes how it emits a statement and the identity does not move, the exact
// digest changes for a reason nothing recorded.
//
// Do NOT update the expectation to make this pass: bump RendererVersion, so old
// and new bindings are distinguishable, and then record the new value.
func TestRendererIdentityIsPinnedToItsSource(t *testing.T) {
	const want = "7792e8cdadf68730f9a912472f53b79eeeba482395178aef55822a01898ce3c5"
	got, err := RendererSHA256()
	if err != nil {
		t.Fatalf("renderer identity: %v", err)
	}
	if got != want {
		t.Fatalf("THE RENDERER'S OUTPUT CHANGED: identity is %s, was %s.\n"+
			"Every target record signed under the old value names it beside the exact digest "+
			"of bytes this renderer produced. Bump RendererVersion and record the new identity; "+
			"do not simply update this expectation.", got, want)
	}
}

func TestRendererIdentityIsStable(t *testing.T) {
	first, err := RendererSHA256()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 16; attempt++ {
		again, err := RendererSHA256()
		if err != nil || again != first {
			t.Fatalf("renderer identity is not stable: %s then %s (%v)", first, again, err)
		}
	}
	if len(first) != 64 {
		t.Fatalf("renderer identity is not a SHA-256: %q", first)
	}
}

// The probe must keep reaching every construct the renderer emits, or a change
// to one of them would not move the identity.
func TestRendererIdentityProbeCoversEveryRenderedConstruct(t *testing.T) {
	referenced, products := rendererIdentityProbe()
	rendered, err := renderExecutable(
		`SELECT "month", sum("amount") AS "total" FROM "expense" GROUP BY "month"`,
		referenced, products, 137)
	if err != nil {
		t.Fatalf("the renderer identity probe no longer renders: %v", err)
	}
	for name, fragment := range map[string]string{
		"CTE framing":          "WITH ",
		"CTE separator":        ",\n",
		"schema qualification": `"reporting".`,
		"escaped identifier":   `"head""count"`,
		"set-valued predicate": " IN (",
		"escaped literal":      "'o''brien'",
		"null predicate":       " IS NULL",
		"comparison predicate": " >= ",
		"inequality predicate": " <> ",
		"row limit":            "LIMIT 137",
	} {
		if !strings.Contains(rendered, fragment) {
			t.Errorf("the probe no longer exercises %s; a change to it would not move the renderer identity", name)
		}
	}
}
