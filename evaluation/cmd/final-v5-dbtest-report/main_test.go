package main

import (
	"strings"
	"testing"
)

// stream builds a go test -json stream from compact records.
func stream(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

func summarizeStream(t *testing.T, text string) Report {
	t.Helper()
	report, err := summarize(strings.NewReader(text), Report{Version: reportVersion})
	if err != nil {
		t.Fatalf("summarize: %v", err)
	}
	return report
}

const passingRun = `{"Action":"run","Package":"p","Test":"TestOne"}
{"Action":"pass","Package":"p","Test":"TestOne"}
{"Action":"pass","Package":"p"}`

func TestACleanRunIsAccepted(t *testing.T) {
	report := summarizeStream(t, stream(passingRun))
	if !report.Accepted {
		t.Fatalf("a clean run was not accepted: %+v", report)
	}
	if report.Packages != 1 || report.Tests != 1 || report.TestsPassed != 1 {
		t.Fatalf("counts are wrong: %+v", report)
	}
	if len(report.ReportSHA256) != 64 {
		t.Fatalf("the summary does not digest its own source: %q", report.ReportSHA256)
	}
}

// The defect this tool exists for: a suite that skips a DB-backed test still
// exits zero, so acceptance cannot rest on the exit code.
func TestAnUndeclaredSkipIsNotAccepted(t *testing.T) {
	report := summarizeStream(t, stream(
		`{"Action":"output","Package":"internal/sqlidentity","Test":"TestSourceDerivedDigestsMatchLivePostgreSQL","Output":"    live_test.go:1: pg_stat_statements is not installed on this deployment\n"}`,
		`{"Action":"skip","Package":"internal/sqlidentity","Test":"TestSourceDerivedDigestsMatchLivePostgreSQL"}`,
		`{"Action":"pass","Package":"internal/sqlidentity"}`))
	if report.Accepted {
		t.Fatal("a run that silently skipped a DB-backed gate was accepted")
	}
	if len(report.UndeclaredSkips) != 1 {
		t.Fatalf("the undeclared skip was not reported: %+v", report.UndeclaredSkips)
	}
	if !strings.Contains(report.UndeclaredSkips[0].Reason, "pg_stat_statements is not installed") {
		t.Fatalf("the skip reason was lost: %q", report.UndeclaredSkips[0].Reason)
	}
}

// A declared skip is accepted, and it carries its category and justification
// into the report rather than merely disappearing from it.
func TestADeclaredSkipIsAcceptedAndExplained(t *testing.T) {
	allowance := allowedSkips[0]
	report := summarizeStream(t, stream(
		`{"Action":"output","Package":"`+allowance.Package+`","Test":"`+allowance.Test+
			`","Output":"    probe_equivalence_test.go:26: `+allowance.ReasonSubstring+` for the probe check\n"}`,
		`{"Action":"skip","Package":"`+allowance.Package+`","Test":"`+allowance.Test+`"}`,
		`{"Action":"pass","Package":"`+allowance.Package+`"}`))
	if !report.Accepted {
		t.Fatalf("a declared skip was not accepted: %+v", report.UndeclaredSkips)
	}
	if len(report.Skips) != 1 || !report.Skips[0].Declared {
		t.Fatalf("the skip was not recorded as declared: %+v", report.Skips)
	}
	if report.Skips[0].Category == "" || report.Skips[0].Why == "" {
		t.Fatal("a declared skip must carry its category and justification into the report")
	}
}

// An allowance is tied to the reason the test gives. If the test starts skipping
// for a different reason it is no longer the skip that was declared, and the
// allowance must not cover it -- otherwise an allowance outlives its cause.
func TestAnAllowanceDoesNotCoverADifferentReason(t *testing.T) {
	allowance := allowedSkips[0]
	report := summarizeStream(t, stream(
		`{"Action":"output","Package":"`+allowance.Package+`","Test":"`+allowance.Test+
			`","Output":"    probe_equivalence_test.go:26: the fixture table is missing\n"}`,
		`{"Action":"skip","Package":"`+allowance.Package+`","Test":"`+allowance.Test+`"}`,
		`{"Action":"pass","Package":"`+allowance.Package+`"}`))
	if report.Accepted {
		t.Fatal("an allowance covered a skip that happened for another reason")
	}
}

func TestFailuresAreNotAccepted(t *testing.T) {
	report := summarizeStream(t, stream(
		`{"Action":"fail","Package":"p","Test":"TestOne"}`,
		`{"Action":"fail","Package":"p"}`))
	if report.Accepted {
		t.Fatal("a failing run was accepted")
	}
	if report.TestsFailed != 1 || report.PackagesFailed != 1 {
		t.Fatalf("failures were miscounted: %+v", report)
	}
}

// A build failure is printed raw rather than as a JSON record. Skipping such a
// line would turn a package that never compiled into an empty, accepted summary.
func TestARawBuildFailureLineIsFatal(t *testing.T) {
	_, err := summarize(strings.NewReader("# taskbound.local/pkg\nsome.go:1: undefined: X\n"), Report{})
	if err == nil {
		t.Fatal("a non-JSON line in the report was ignored")
	}
}

// Every allowance must be complete. An entry with no reason substring would
// match any skip of that test, and one with no justification is an allowance
// nobody can review.
func TestEveryAllowanceIsCompleteAndUnique(t *testing.T) {
	seen := map[[2]string]bool{}
	for _, allowance := range allowedSkips {
		key := [2]string{allowance.Package, allowance.Test}
		if seen[key] {
			t.Errorf("%s %s is declared twice", allowance.Package, allowance.Test)
		}
		seen[key] = true
		if allowance.Package == "" || allowance.Test == "" {
			t.Errorf("an allowance names no test: %+v", allowance)
		}
		if strings.TrimSpace(allowance.ReasonSubstring) == "" {
			t.Errorf("%s has no reason substring, so it would match any skip", allowance.Test)
		}
		if strings.TrimSpace(allowance.Why) == "" {
			t.Errorf("%s carries no justification", allowance.Test)
		}
		switch allowance.Category {
		case SkipSeparateDatabase, SkipSeparateDeployment, SkipEvidenceNotYetProduced:
		default:
			t.Errorf("%s carries unknown category %q", allowance.Test, allowance.Category)
		}
	}
}
