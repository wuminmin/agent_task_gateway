package experiment

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The source wiring is complete, but the derivation boundary it depends on
// remains a permanent structural condition. The finalizer must require
// reproduced target statements, internal/physicalquery must retain the
// extracted API, and the deleted Gateway-local derivation surface must not
// return. Losing any one would either make acceptance unreachable or restore
// two derivation surfaces whose drift could be mistaken for a measurement
// result.

// sharedTargetDerivation names what internal/physicalquery must keep exported
// for a finalizer to reproduce a preparation at all.
//
// Each is written out rather than discovered, so that hiding or renaming one
// fails here instead of silently emptying the check. Re-hiding any of them would
// put the cutover back behind the barrier T1d removed.
var sharedTargetDerivation = []string{
	"Prepare",
	"PrepareWith",
	"PrepareSemanticView",
	"PrepareSemanticViewWith",
	"PreparedOperation",
	"PreparationInputs",
	"SemanticViewPreparationInputsV1",
}

// gatewayDeletedDerivation names the Gateway-local derivation the extraction
// removed. None of them may come back: a second implementation in the running
// system is the outcome the whole arc exists to avoid, and it would be invisible
// to a differential that compares the surviving one with itself.
var gatewayDeletedDerivation = []string{
	"buildPlanExposureContext",
	"buildRelationalExposureContext",
	"bindOrdinalSidecars",
	"ordinalQueryProduct",
	"configureV2",
	"configurePredicateFootprintV5",
	"extendGrant",
	"extendOrdinalPolicyGrant",
}

func TestFinalizerRetainsTheSingleSharedTargetDerivation(t *testing.T) {
	// The finalizer genuinely requires the reproduced statements.
	inputs := finalizerInputs(t)
	inputs.VisibleSQL, inputs.CompanionSQL = "", ""
	if _, err := deriveTargets(inputs, StrictASTDigest); err == nil {
		t.Fatal("the finalizer derived paired-novel targets without reproduced statements")
	}

	// The extracted shared-derivation API remains exported.
	root := repositoryRoot(t)
	shared := exportedIdentifiers(t, filepath.Join(root, "internal", "physicalquery"))
	for _, name := range sharedTargetDerivation {
		if !shared[name] {
			t.Fatalf("internal/physicalquery no longer exports extracted reproduction API %q", name)
		}
	}

	// The deleted Gateway-local derivation surface has not returned.
	gateway := exportedIdentifiers(t, filepath.Join(root, "internal", "gateway"))
	for _, name := range gatewayDeletedDerivation {
		if gateway["!declared:"+name] {
			t.Fatalf("internal/gateway declares %q again; production would hold two derivations "+
				"of one statement, which is what the extraction removed", name)
		}
	}
}

// exportedIdentifiers reports the exported top-level names a package declares,
// plus a "!declared:<name>" marker for every top-level name including unexported
// ones, so a caller can tell "unexported" from "gone".
func exportedIdentifiers(t *testing.T, directory string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read %s: %v", directory, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, filepath.Join(directory, entry.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		for _, declaration := range parsed.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				record(names, typed.Name.Name)
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						record(names, typeSpec.Name.Name)
					}
				}
			}
		}
	}
	return names
}

func record(names map[string]bool, name string) {
	names["!declared:"+name] = true
	if ast.IsExported(name) {
		names[name] = true
	}
}
