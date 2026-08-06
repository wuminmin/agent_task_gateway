package queryreceipt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// orderMarker is the opt-in that permits an ordering comparison.
//
// It is a comment rather than a linter directive because the thing being
// asserted is a claim, not a suppression: whoever writes it is saying this
// comparison is about which version is older and not about what a version
// carries. A reviewer can check that claim; they cannot check a bare //nolint.
const orderMarker = "receipt-version-order:"

// TestNoReceiptVersionComparisonIsUsedAsAFeaturePredicate refuses new
// comparisons of the kind this package has already been broken by twice.
//
// The first was an equality against V8 deciding whether a receipt carried
// artifact intent. It stopped matching when V9 arrived, so V9 receipts skipped
// the artifact inclusion proofs and the registration projection they in fact
// had. The fix was a range, which was correct only while every later version
// stayed strictly additive -- and V10 is the version where that stops: it
// requires an execution binding V9 forbids, forbids the one V9 requires, and
// makes the artifact intent conditional on the delivery mode.
//
// So neither spelling is allowed to decide a feature. CapabilitiesFor and the
// predicates beside it are the only answer, and a comparison that genuinely
// means "which of these is older" has to say so.
func TestNoReceiptVersionComparisonIsUsedAsAFeaturePredicate(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", "formal-build", "generated":
				return filepath.SkipDir
			}
			return nil
		}
		// Test files are exempt. A test asserting a per-version rule is stating
		// the contract rather than reading a feature out of an ordering, and the
		// version tables in this package's own tests are how the contract is
		// pinned at all.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		findings = append(findings, scanForVersionPredicates(t, root, path)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Fatalf("receipt version comparisons used as feature predicates:\n  %s\n\n"+
			"Ask CapabilitiesFor, SupportsArtifactIntent, RequiresExposureEvidence, "+
			"RequiresExecutionBindingV1 or RequiresExecutionBindingV2 instead. If the comparison really is "+
			"about which version is older, say so with a %q comment on the line or the line above.",
			strings.Join(findings, "\n  "), orderMarker)
	}
}

func scanForVersionPredicates(t *testing.T, root, path string) []string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, source, parser.ParseComments)
	if err != nil {
		// A file this package cannot parse is not a file it can vouch for.
		t.Fatalf("parse %s: %v", path, err)
	}
	lines := strings.Split(string(source), "\n")
	relative, err := filepath.Rel(root, path)
	if err != nil {
		relative = path
	}
	// capabilities.go is the table itself. Its version constants are map keys and
	// struct members, not comparisons, but exempting the file makes the intent
	// explicit rather than dependent on how the table happens to be written.
	if relative == filepath.Join("internal", "queryreceipt", "capabilities.go") {
		return nil
	}

	var findings []string
	report := func(pos token.Pos, what string) {
		position := fileSet.Position(pos)
		if markedAsOrdering(lines, position.Line) {
			return
		}
		findings = append(findings, relative+":"+itoa(position.Line)+": "+what)
	}
	ast.Inspect(parsed, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BinaryExpr:
			switch typed.Op {
			case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
				if name, ok := receiptVersionConstant(typed.X); ok {
					report(typed.Pos(), "comparison against "+name)
				} else if name, ok := receiptVersionConstant(typed.Y); ok {
					report(typed.Pos(), "comparison against "+name)
				}
			}
		case *ast.CallExpr:
			if name, ok := calleeName(typed.Fun); ok && name == "VersionAtLeast" {
				report(typed.Pos(), "call to VersionAtLeast")
			}
		}
		return true
	})
	return findings
}

// receiptVersionConstant recognises VersionV1..VersionV10, whether written bare
// inside this package or qualified from outside it.
func receiptVersionConstant(expr ast.Expr) (string, bool) {
	switch typed := expr.(type) {
	case *ast.Ident:
		if isReceiptVersionName(typed.Name) {
			return typed.Name, true
		}
	case *ast.SelectorExpr:
		pkg, ok := typed.X.(*ast.Ident)
		if ok && pkg.Name == "queryreceipt" && isReceiptVersionName(typed.Sel.Name) {
			return pkg.Name + "." + typed.Sel.Name, true
		}
	}
	return "", false
}

// isReceiptVersionName matches the receipt's own version constants and nothing
// else. Other packages have constants beginning with "Version" -- the audit
// anchor's and the query plan's normal form among them -- and flagging those
// would train readers to ignore this guard.
func isReceiptVersionName(name string) bool {
	if !strings.HasPrefix(name, "VersionV") {
		return false
	}
	suffix := name[len("VersionV"):]
	if suffix == "" {
		return false
	}
	for _, character := range suffix {
		if character < '0' || character > '9' {
			return false
		}
	}
	for _, known := range receiptVersions {
		if suffix == known {
			return true
		}
	}
	return false
}

func calleeName(expr ast.Expr) (string, bool) {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name, true
	case *ast.SelectorExpr:
		return typed.Sel.Name, true
	}
	return "", false
}

func markedAsOrdering(lines []string, line int) bool {
	for _, candidate := range []int{line, line - 1, line - 2} {
		if candidate >= 1 && candidate <= len(lines) &&
			strings.Contains(lines[candidate-1], orderMarker) {
			return true
		}
	}
	return false
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// The guard has to be able to fail, or a passing run means nothing. This proves
// it recognises both spellings and honours the marker.
func TestTheVersionPredicateGuardActuallyDetects(t *testing.T) {
	directory := t.TempDir()
	offending := filepath.Join(directory, "offending.go")
	if err := os.WriteFile(offending, []byte(`package sample

import "taskbound.local/agent-data-gateway/internal/queryreceipt"

func carriesArtifact(version string) bool {
	if version == queryreceipt.VersionV8 {
		return true
	}
	return queryreceipt.VersionAtLeast(version, queryreceipt.VersionV9)
}

func isOlder(version string) bool {
	// receipt-version-order: strictly a chronological question
	return queryreceipt.VersionAtLeast(version, queryreceipt.VersionV3)
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	findings := scanForVersionPredicates(t, directory, offending)
	// Two findings: the equality and the unmarked VersionAtLeast call. The version
	// constants passed as arguments are not findings of their own -- the call is
	// the predicate -- and the marked ordering call on line 14 is not a finding at
	// all, which is what proves the marker is honoured rather than ignored.
	if len(findings) != 2 {
		t.Fatalf("the guard found %d predicates in a file with two unmarked ones: %v",
			len(findings), findings)
	}
	for _, finding := range findings {
		if strings.Contains(finding, ":13:") || strings.Contains(finding, ":14:") {
			t.Fatalf("the guard flagged a comparison marked as ordering: %s", finding)
		}
	}
}
