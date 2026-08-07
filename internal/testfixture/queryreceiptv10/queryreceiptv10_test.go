package queryreceiptv10

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func pairedOptions() Options {
	companion := Target{
		ExactSQLSHA256: digest("companion-exact"), StrictASTSHA256: digest("companion-strict"),
	}
	return Options{
		Visible: Target{
			ExactSQLSHA256: digest("visible-exact"), StrictASTSHA256: digest("visible-strict"),
		},
		Companion: &companion,
	}
}

// Every fixture must be a receipt the production validator and verifier accept.
// A fixture that only looks right would make every gate built on it vacuous.
func TestFixturesValidateAndVerify(t *testing.T) {
	verifier, err := Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	for name, build := range map[string]func(Options) (queryreceipt.QueryReceiptV1, error){
		"paired novel":    PairedNovel,
		"semantic replay": SemanticReplay,
		"single query":    SingleQuery,
	} {
		t.Run(name, func(t *testing.T) {
			receipt, err := build(pairedOptions())
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if receipt.Version != queryreceipt.Version {
				t.Fatalf("fixture is V%s, want V10", receipt.Version)
			}
			if err := receipt.Validate(); err != nil {
				t.Fatalf("receipt does not validate: %v", err)
			}
			if err := verifier.Verify(receipt); err != nil {
				t.Fatalf("receipt does not verify: %v", err)
			}
			if receipt.ExecutionBindingV2 == nil || receipt.ExposureLedgerBefore == nil {
				t.Fatal("fixture carries no execution binding or pre-state")
			}
			if err := receipt.ExecutionBindingV2.Validate(); err != nil {
				t.Fatalf("execution binding does not validate: %v", err)
			}
		})
	}
}

// A semantic replay authorizes its targets and executes neither. This is the
// property gate 21 rests on, so the fixture must actually have it.
func TestSemanticReplayAuthorizesWithoutExecuting(t *testing.T) {
	receipt, err := SemanticReplay(pairedOptions())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	binding := receipt.ExecutionBindingV2
	if binding.PathKind != querybinding.PathSemanticReplay {
		t.Fatalf("path kind is %q", binding.PathKind)
	}
	if !binding.Visible.Authorized || binding.Visible.Executed {
		t.Fatalf("visible target is authorized=%t executed=%t; want true/false",
			binding.Visible.Authorized, binding.Visible.Executed)
	}
	if binding.Companion == nil || !binding.Companion.Authorized || binding.Companion.Executed {
		t.Fatal("companion target is not authorized-but-unexecuted")
	}
}

// Gate 22 compares stored receipt bytes. That comparison means nothing unless
// the same inputs produce the same bytes and the same signature every time.
func TestFixturesAreDeterministic(t *testing.T) {
	first, err := PairedNovel(pairedOptions())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	second, err := PairedNovel(pairedOptions())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if first.Signature == "" {
		t.Fatal("fixture is unsigned")
	}
	if first.Signature != second.Signature {
		t.Fatal("two builds of the same fixture produced different signatures")
	}
	firstJSON, err := PersistedJSON(first)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	secondJSON, err := PersistedJSON(second)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("two builds of the same fixture produced different persisted bytes")
	}
}

// A mutation must produce a receipt that is internally consistent and genuinely
// describes a different execution. If Mutate left a stale digest, every gate
// built on it would prove only that a digest check exists.
func TestMutationResealsAndResigns(t *testing.T) {
	receipt, err := PairedNovel(pairedOptions())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	mutated, err := Mutate(receipt, func(b *querybinding.QueryExecutionBindingV2) {
		b.Visible.ExactSQLSHA256 = digest("another-visible-exact")
	})
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if err := mutated.ExecutionBindingV2.Validate(); err != nil {
		t.Fatalf("a mutated binding must still be internally valid: %v", err)
	}
	verifier, err := Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if err := verifier.Verify(mutated); err != nil {
		t.Fatalf("a mutated receipt must still verify: %v", err)
	}
	if mutated.ExecutionBindingV2.SHA256 == receipt.ExecutionBindingV2.SHA256 {
		t.Fatal("the mutation did not change the binding digest")
	}
	// The original must be untouched: a mutation helper that edited in place
	// would silently corrupt every later case in a table-driven test.
	if receipt.ExecutionBindingV2.Visible.ExactSQLSHA256 != digest("visible-exact") {
		t.Fatal("Mutate edited the original receipt")
	}
}

// sqlLike catches statement text in any of the forms it could arrive as. The
// receipt is retained, replayed and handed to a finalizer that must not learn
// what was queried; a fixture is not exempt from that.
var sqlLike = regexp.MustCompile(`(?i)\b(select|insert|update|delete|from|where|join|union|create|drop)\b`)

func TestFixtureCarriesNoSQL(t *testing.T) {
	receipt, err := PairedNovel(pairedOptions())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	encoded, err := PersistedJSON(receipt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch typed := value.(type) {
		case map[string]any:
			for key, member := range typed {
				walk(member, path+"."+key)
			}
		case []any:
			for index, member := range typed {
				walk(member, path)
				_ = index
			}
		case string:
			if sqlLike.MatchString(typed) {
				t.Errorf("%s carries SQL-like text %q", path, typed)
			}
		}
	}
	var document any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode: %v", err)
	}
	walk(document, "receipt")

	// And the source itself, so a future edit cannot introduce a statement in a
	// field this receipt shape does not currently have.
	source, err := os.ReadFile("queryreceiptv10.go")
	if err != nil {
		t.Fatalf("read fixture source: %v", err)
	}
	for _, line := range strings.Split(string(source), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.Contains(trimmed, `"`) {
			continue
		}
		if sqlLike.MatchString(trimmed) {
			t.Errorf("fixture source line carries SQL-like text: %s", trimmed)
		}
	}
}

// This package is test scaffolding. A production file importing it would put a
// deterministic signing key and a receipt builder into the shipped binary.
func TestFixtureIsNotImportedByProduction(t *testing.T) {
	root := moduleRoot(t)
	const importPath = "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return nil
		}
		for _, imported := range parsed.Imports {
			if strings.Trim(imported.Path.Value, `"`) == importPath {
				relative, _ := filepath.Rel(root, path)
				t.Errorf("non-test production file %s imports the receipt test fixture; "+
					"it would ship a deterministic signing key", filepath.ToSlash(relative))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test's working directory")
		}
		directory = parent
	}
}
