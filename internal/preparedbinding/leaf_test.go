package preparedbinding_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
)

const modulePath = "taskbound.local/agent-data-gateway"

// # Why these guards exist
//
// The durable prepared binding was moved out of physicalquery so that the Query
// Execution Binding could carry the whole sealed document without dragging an
// authorizer into the Query Receipt's link closure. That is an architectural
// property, and an architectural property nothing checks is a comment.
//
// The failure it prevents is quiet. Nothing breaks when preparedbinding grows an
// import of sqlpolicy for one convenient helper; the build still passes, the
// tests still pass, and the only thing that changed is that a receipt -- a
// description handed to a finalizer, an auditor and the evaluation -- can now
// reach the code that decides what a statement is allowed to touch. "The receipt
// cannot authorize" would silently become a claim about what the code happens to
// call rather than about what it can reach.

// importsOf returns the non-test imports one package's source files declare.
func importsOf(t *testing.T, root, packagePath string) []string {
	t.Helper()
	directory := filepath.Join(root, strings.TrimPrefix(packagePath, modulePath+"/"))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read package %s: %v", packagePath, err)
	}
	fileSet := token.NewFileSet()
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, parseErr := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, declared := range parsed.Imports {
			value, unquoteErr := strconv.Unquote(declared.Path.Value)
			if unquoteErr != nil {
				t.Fatalf("unquote import in %s: %v", name, unquoteErr)
			}
			seen[value] = true
		}
	}
	imports := make([]string, 0, len(seen))
	for value := range seen {
		imports = append(imports, value)
	}
	sort.Strings(imports)
	return imports
}

// closureOf is every module package reachable from one package through non-test
// imports, transitively.
//
// Transitively is the point. A direct-import check would pass the moment the
// forbidden dependency arrives one hop away, which is exactly how queryreceipt
// would have acquired sqlpolicy: not by importing it, but by importing
// querybinding, which would have imported physicalquery.
func closureOf(t *testing.T, root, packagePath string) map[string]bool {
	t.Helper()
	closure := map[string]bool{}
	var walk func(string)
	walk = func(current string) {
		if closure[current] {
			return
		}
		closure[current] = true
		for _, imported := range importsOf(t, root, current) {
			if strings.HasPrefix(imported, modulePath+"/") {
				walk(imported)
			}
		}
	}
	walk(packagePath)
	delete(closure, packagePath)
	return closure
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// preparedbinding is a leaf. It may reach a canonical JSON encoder and nothing
// else in this module: it holds a version, flags, counts and digests, and there
// is no question it could answer that needs a compiler, an authorizer or a
// Gateway.
func TestPreparedBindingPackageIsLeaf(t *testing.T) {
	root := repositoryRoot(t)
	closure := closureOf(t, root, modulePath+"/internal/preparedbinding")

	for _, forbidden := range []string{
		"internal/physicalquery", "internal/querybinding", "internal/queryreceipt",
		"internal/gateway", "internal/sqlpolicy", "internal/queryplan", "internal/control",
	} {
		if closure[modulePath+"/"+forbidden] {
			t.Errorf("preparedbinding reaches %s; it must hold the durable binding and nothing that acts on one",
				forbidden)
		}
	}
	for reached := range closure {
		if strings.HasPrefix(reached, modulePath+"/evaluation/") {
			t.Errorf("preparedbinding reaches the evaluation package %s; production must not depend on evaluation code",
				reached)
		}
	}
	// Stated positively as well, so the guard says what the package IS rather
	// than only what it is not. A new module dependency here is a decision, and
	// it should have to be made deliberately.
	var reached []string
	for value := range closure {
		reached = append(reached, strings.TrimPrefix(value, modulePath+"/"))
	}
	sort.Strings(reached)
	want := []string{"internal/apierr", "internal/approval", "internal/domain"}
	if !reflect.DeepEqual(reached, want) {
		t.Errorf("preparedbinding's module closure is %v, want exactly %v", reached, want)
	}
}

// The receipt describes what happened. It is retained, replayed and handed to
// holders that must not be able to authorize anything, so the ability must not
// be in its closure at all -- not merely unused.
func TestQueryReceiptDependencyClosureExcludesSQLPolicy(t *testing.T) {
	root := repositoryRoot(t)
	closure := closureOf(t, root, modulePath+"/internal/queryreceipt")

	for _, forbidden := range []string{
		"internal/sqlpolicy", "internal/queryplan", "internal/gateway",
		"internal/physicalquery", "internal/control", "internal/dataconnector",
	} {
		if closure[modulePath+"/"+forbidden] {
			t.Errorf("queryreceipt reaches %s; the receipt must remain a description rather than acquire "+
				"the ability to authorize, compile or execute", forbidden)
		}
	}
	for reached := range closure {
		if strings.HasPrefix(reached, modulePath+"/evaluation/") {
			t.Errorf("queryreceipt reaches the evaluation package %s", reached)
		}
	}
	// The receipt must still reach the binding it carries, or this guard would
	// pass just as well on a receipt that carries no execution evidence at all.
	for _, required := range []string{"internal/querybinding", "internal/preparedbinding"} {
		if !closure[modulePath+"/"+required] {
			t.Errorf("queryreceipt does not reach %s; it cannot be carrying the execution binding", required)
		}
	}
}

// The alias must be an alias. A defined type would compile, and would then need
// a conversion at every boundary -- a conversion that can be applied in one
// direction and forgotten in the other, which is how two names for one binding
// become two bindings.
func TestPhysicalQueryAliasesTheCanonicalPreparedBindingType(t *testing.T) {
	for name, pair := range map[string][2]reflect.Type{
		"PreparedOperationBindingV1": {
			reflect.TypeOf(physicalquery.PreparedOperationBindingV1{}),
			reflect.TypeOf(preparedbinding.PreparedOperationBindingV1{}),
		},
		"CompilerIdentityV1": {
			reflect.TypeOf(physicalquery.CompilerIdentityV1{}),
			reflect.TypeOf(preparedbinding.CompilerIdentityV1{}),
		},
	} {
		if pair[0] != pair[1] {
			t.Errorf("physicalquery.%s is a distinct type from preparedbinding's, not an alias", name)
		}
		if got := pair[0].PkgPath(); got != modulePath+"/internal/preparedbinding" {
			t.Errorf("physicalquery.%s is defined in %s, not in preparedbinding", name, got)
		}
	}
	if physicalquery.PreparedOperationBindingV1Version != preparedbinding.PreparedOperationBindingV1Version {
		t.Error("physicalquery and preparedbinding disagree about the durable binding's version string")
	}
	if physicalquery.RoleVisible != preparedbinding.RoleVisible ||
		physicalquery.RoleCompanion != preparedbinding.RoleCompanion {
		t.Error("physicalquery and preparedbinding disagree about the target roles")
	}
}

// Persistence and the execution binding take the canonical type directly. Going
// through the alias would make them depend on physicalquery -- and so on
// sqlpolicy -- for a type that needs neither.
func TestPersistenceAndQueryBindingTakeTheCanonicalTypeDirectly(t *testing.T) {
	root := repositoryRoot(t)
	for _, packagePath := range []string{
		modulePath + "/internal/querybinding",
		modulePath + "/internal/control",
	} {
		direct := importsOf(t, root, packagePath)
		found := false
		for _, imported := range direct {
			if imported == modulePath+"/internal/preparedbinding" {
				found = true
			}
			if imported == modulePath+"/internal/physicalquery" {
				t.Errorf("%s imports physicalquery; the durable binding is available without it", packagePath)
			}
		}
		if !found {
			t.Errorf("%s does not import preparedbinding directly", packagePath)
		}
	}
	// And the types they expose really are the canonical one, not a local mirror
	// that happens to have the same members.
	binding := querybinding.QueryExecutionBindingV2{}
	if got := reflect.TypeOf(binding.PreparedOperation).PkgPath(); got != modulePath+"/internal/preparedbinding" {
		t.Errorf("QueryExecutionBindingV2 carries a preparation defined in %s", got)
	}
	prepared, ok := control.QueryExecutionBinding{BindingV2: &binding}.PreparedOperation()
	if !ok {
		t.Fatal("a stored V2 row reports it describes no preparation")
	}
	if got := reflect.TypeOf(prepared).PkgPath(); got != modulePath+"/internal/preparedbinding" {
		t.Errorf("persistence exposes a preparation defined in %s", got)
	}
	// A row with no document must say it cannot answer, rather than answering
	// with a zero binding that would read as a mismatch against everything.
	if _, answered := (control.QueryExecutionBinding{}).PreparedOperation(); answered {
		t.Error("a stored row with no document claims to carry a preparation")
	}
}
