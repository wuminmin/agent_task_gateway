package experiment

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The three structural guards on the acceptance boundary.
//
// Each of them is a POSITIVE statement about where trusted material may be
// constructed, which is the half the older guards could not make. "The Adapter
// does not name TrustedInputsV3" leaves every other file in the tree free to;
// "only runtime_finalizer_v3.go constructs it" says where the one legitimate
// construction lives, and therefore fails when a second one appears anywhere.

// trustedMaterialTypes are the finalizer's own. A file that names one is
// deciding what the answer should be, not submitting evidence for judgement.
//
// FrozenOperationMaterialV3 and IdempotentReplayEvidenceV1 are here beside the
// two input structs because they are the same kind of thing: the plan and grant
// an operation is reproduced from, and the store's account of whether anything
// was settled. An Adapter that could build either would be back to supplying the
// standard against which it is measured.
var trustedMaterialTypes = []string{
	"TrustedInputsV3",
	"IndependentInputsV3",
	"FrozenOperationMaterialV3",
	"IdempotentReplayEvidenceV1",
}

// TestOnlyRuntimeFinalizerConstructsTrustedInputsV3 pins the single legitimate
// construction point.
//
// The expectation is an exact set rather than a maximum. A new file that
// constructs trusted inputs fails here even if it is not an Adapter, because the
// property being protected is not "Adapters are restricted" -- it is "there is
// one place where the finalizer's answer is assembled, and it is the finalizer".
func TestOnlyRuntimeFinalizerConstructsTrustedInputsV3(t *testing.T) {
	root := repositoryRoot(t)
	// The declarations live with the core and are not constructions of it.
	declarations := map[string]bool{
		"evaluation/internal/experiment/finalize_taskgate_v3.go":    true,
		"evaluation/internal/experiment/finalize_observation_v3.go": true,
		"evaluation/internal/experiment/reproduce_execution_v3.go":  true,
	}
	want := []string{"evaluation/internal/experiment/runtime_finalizer_v3.go"}

	var found []string
	for _, path := range activeGoFiles(t, root) {
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		if declarations[relative] {
			continue
		}
		if constructsTrustedInputs(t, path) {
			found = append(found, relative)
		}
	}
	sort.Strings(found)
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("trusted inputs are constructed in %v; exactly one place may construct them, %v.\n"+
			"A second construction point is a second opinion about what the finalizer's answer is.",
			found, want)
	}
}

// constructsTrustedInputs reports whether a file builds a TrustedInputsV3, by
// composite literal or by declaring a variable of the type.
//
// It looks for the type name in either spelling -- bare inside package
// experiment, qualified outside it -- because the property is about the file,
// not about which package it happens to live in.
func constructsTrustedInputs(t *testing.T, path string) bool {
	t.Helper()
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	found := false
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.Ident:
			if typed.Name == "TrustedInputsV3" {
				found = true
			}
		case *ast.SelectorExpr:
			if typed.Sel.Name == "TrustedInputsV3" {
				found = true
			}
		}
		return true
	})
	return found
}

// TestFinalizerRequestCarriesNoTrustedMaterial reads the public request type and
// requires every member to be evidence or a hint.
//
// This is the guard that survives a refactor nobody remembers the reason for. A
// later change that "just adds the catalog path to the request so the caller can
// point at the right profile" would restore exactly the defect the façade
// removed, and would otherwise pass every other test in this file.
func TestFinalizerRequestCarriesNoTrustedMaterial(t *testing.T) {
	root := repositoryRoot(t)
	path := filepath.Join(root, "evaluation", "internal", "experiment", "runtime_finalizer_v3.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse the runtime finalizer: %v", err)
	}
	// Everything an Adapter may submit, stated once here so that adding a member
	// is a decision recorded in this test rather than a field appearing.
	allowed := map[string]bool{
		"Receipt": true, "Carried": true, "ContractSelector": true, "ReturnedReceiptJSON": true,
	}
	var members []string
	ast.Inspect(parsed, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "FinalizationRequestV3" {
			return true
		}
		structure, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range structure.Fields.List {
			for _, name := range field.Names {
				members = append(members, name.Name)
			}
		}
		return false
	})
	if len(members) == 0 {
		t.Fatal("FinalizationRequestV3 was not found; the acceptance boundary is not where this guard looks")
	}
	for _, member := range members {
		if !allowed[member] {
			t.Errorf("FinalizationRequestV3 carries %q, which is not evidence or a lookup hint.\n"+
				"A caller that can supply it is deciding part of the finalizer's answer.", member)
		}
	}
	// And the type names of those members must not be trusted material either: a
	// member called Carried whose type is TrustedInputsV3 would pass the name
	// check above.
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the runtime finalizer: %v", err)
	}
	declaration := requestTypeDeclaration(t, string(source))
	for _, forbidden := range trustedMaterialTypes {
		if strings.Contains(declaration, forbidden) {
			t.Errorf("FinalizationRequestV3 names %s; the request carries evidence, not the answer", forbidden)
		}
	}
}

// requestTypeDeclaration returns the source text of the request struct, so the
// member TYPES can be inspected as written.
func requestTypeDeclaration(t *testing.T, source string) string {
	t.Helper()
	const marker = "type FinalizationRequestV3 struct {"
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatal("FinalizationRequestV3 is not declared where this guard looks")
	}
	rest := source[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatal("FinalizationRequestV3's declaration is unterminated")
	}
	return rest[:end]
}

// TestTheAcceptanceCoreIsNotExported states the other half of the boundary.
//
// The façade would be pointless if the core it protects were still reachable:
// an Adapter that could call the exported wrapper with its own TrustedInputsV3
// would have gone round the entire arrangement, and no guard on the request type
// would notice.
func TestTheAcceptanceCoreIsNotExported(t *testing.T) {
	root := repositoryRoot(t)
	core := filepath.Join(root, "evaluation", "internal", "experiment", "finalize_taskgate_v3.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, core, nil, 0)
	if err != nil {
		t.Fatalf("parse the acceptance core: %v", err)
	}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil {
			continue
		}
		if !ast.IsExported(function.Name.Name) {
			continue
		}
		for _, parameter := range function.Type.Params.List {
			if identifier, ok := parameter.Type.(*ast.Ident); ok && identifier.Name == "TrustedInputsV3" {
				t.Errorf("%s is exported and takes TrustedInputsV3; a caller could then supply the "+
					"finalizer's own answer, which is what RuntimeFinalizerV3 exists to prevent",
					function.Name.Name)
			}
		}
	}
}

// finalizerSourceSelectors are the identifiers through which the finalizer's
// trusted material is chosen: the constructor and the five collaborator types.
//
// They are listed rather than discovered so that adding a sixth source is a
// decision recorded here. A source that nothing guards is a source a caller can
// eventually be given.
var finalizerSourceSelectors = []string{
	"openRuntimeFinalizerV3",
	"contractResolverV3",
	"profileMaterialResolverV3",
	"footprintResolverV3",
	"runtimeIdentityReaderV3",
	"controlEvidenceReaderV3",
}

// TestNoExportedWayToChooseTheFinalizersSources closes the half a guard on the
// request type cannot see.
//
// Keeping trusted material out of FinalizationRequestV3 stops a caller supplying
// the answer. It does not stop a caller ASSEMBLING THE FINALIZER -- picking
// which contract registry, which Catalog, which qualification evidence it reads
// -- which is choosing the standard rather than the answer, and is the same
// defect one level up.
//
// The property asserted is structural rather than behavioural: every identifier
// through which a source could be chosen is unexported, so no code outside this
// package can name one, implement one that would be accepted, or call the
// constructor.
func TestNoExportedWayToChooseTheFinalizersSources(t *testing.T) {
	for _, name := range finalizerSourceSelectors {
		if ast.IsExported(name) {
			t.Errorf("%s is exported; a caller that can name it can decide where the finalizer "+
				"reads its trusted material, which is choosing the standard its own evidence "+
				"is judged by", name)
		}
	}

	root := repositoryRoot(t)
	selectors := map[string]bool{}
	for _, name := range finalizerSourceSelectors {
		selectors[name] = true
	}
	// Both files: the one that declares the collaborators and the one that
	// implements them for a real deployment. The second is where the pressure
	// actually is -- it is the file a later change would be tempted to give an
	// "override the Catalog for this run" parameter, and that parameter is a
	// caller choosing the standard.
	for _, name := range []string{"runtime_finalizer_v3.go", "deployment_finalizer_v3.go"} {
		path := filepath.Join(root, "evaluation", "internal", "experiment", name)
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// No exported function or method may take a source, which is the way an
		// unexported type still reaches a caller: through a parameter it can
		// satisfy with a value obtained elsewhere.
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !ast.IsExported(function.Name.Name) {
				continue
			}
			for _, parameter := range function.Type.Params.List {
				ast.Inspect(parameter.Type, func(node ast.Node) bool {
					identifier, ok := node.(*ast.Ident)
					if ok && selectors[identifier.Name] {
						t.Errorf("exported %s in %s takes %s; the finalizer's sources are chosen "+
							"inside this package, never by a caller", function.Name.Name, name, identifier.Name)
					}
					return true
				})
			}
		}
	}
}

// TestTheDeploymentConstructorSelectsNoContent is the guard the deployment
// resolvers make necessary.
//
// The five collaborators are unexported, so no caller can implement one. What a
// caller could still do is ask the ONE exported constructor for a finalizer
// pointed at material of its choosing -- "open one against this Catalog", "against
// this qualification document" -- and every existing guard would pass, because
// nothing about the request type or the collaborator types would have changed.
//
// So the constructor's parameter list is pinned. It may take a context, because a
// context selects nothing; everything else comes from the environment the
// operator started the deployment with, which the measured party does not choose
// at the moment it submits its evidence.
func TestTheDeploymentConstructorSelectsNoContent(t *testing.T) {
	root := repositoryRoot(t)
	path := filepath.Join(root, "evaluation", "internal", "experiment", "deployment_finalizer_v3.go")
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse the deployment finalizer: %v", err)
	}
	found := false
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Name.Name != "OpenDeploymentFinalizerV3" {
			continue
		}
		found = true
		var parameters []string
		for _, parameter := range function.Type.Params.List {
			selector, ok := parameter.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Context" {
				parameters = append(parameters, "a non-context parameter")
				continue
			}
			parameters = append(parameters, "context.Context")
		}
		if len(parameters) != 1 || parameters[0] != "context.Context" {
			t.Errorf("OpenDeploymentFinalizerV3 takes %v; a constructor that takes anything a caller "+
				"chooses lets the measured party pick which archive its own claim is checked against",
				parameters)
		}
	}
	if !found {
		t.Fatal("OpenDeploymentFinalizerV3 is not declared where this guard looks")
	}
}

// TestAdapterCannotAssembleAFinalizer states the same property from the other
// end, in the vocabulary of the packages it protects.
//
// The identifiers are unexported, so this cannot currently fail -- which is the
// point. It fails the moment somebody exports one "for convenience", and it says
// why that is not a convenience.
func TestAdapterCannotAssembleAFinalizer(t *testing.T) {
	root := repositoryRoot(t)
	forbidden := map[string]bool{}
	for _, name := range finalizerSourceSelectors {
		forbidden[name] = true
		// The exported spelling too, so re-exporting one is caught here rather
		// than only by the structural test above.
		forbidden[strings.ToUpper(name[:1])+name[1:]] = true
	}
	for _, pkg := range adapterPackages {
		err := filepath.Walk(filepath.Join(root, pkg), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fileSet := token.NewFileSet()
			parsed, parseErr := parser.ParseFile(fileSet, path, nil, 0)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			relative, _ := filepath.Rel(root, path)
			ast.Inspect(parsed, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if !ok || identifier.Name != "experiment" {
					return true
				}
				if forbidden[selector.Sel.Name] {
					t.Errorf("%s names experiment.%s; an Adapter receives an opened finalizer and "+
						"submits evidence to it. Assembling one would let the measured party choose "+
						"which archive its own claim is checked against.",
						filepath.ToSlash(relative), selector.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("walk %s: %v", pkg, err)
		}
	}
}
