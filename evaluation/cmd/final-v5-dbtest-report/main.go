// final-v5-dbtest-report turns a `go test -json` stream into the committed,
// machine-readable record of one DSN-enabled suite run, and fails when that run
// contains a skip nobody declared.
//
// It exists because "the suite passed" was previously established from an exit
// code. An exit code cannot distinguish a suite that ran from a suite that
// skipped: three DB-backed tests -- the pin domain-separation proof and both
// halves of the strict-AST C3 gate that the classifier rests on -- skipped for
// months against a harness that could run them, and the run still exited zero.
// Only reading the report showed it.
//
// So a skip is a failure here unless it is declared in allowedSkips with a
// reason and a category, and the reason has to match what the test actually
// printed. A test whose skip reason changes stops being covered by its
// allowance, which is what keeps the allowance from outliving its cause.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// SkipCategory says why a declared skip cannot run in this harness. It is
// carried into the report so a reviewer can tell "needs a deployment we do not
// build here" from "the evidence it checks does not exist yet".
type SkipCategory string

const (
	// SkipSeparateDatabase is a test that provisions its own dataset and needs a
	// database this harness does not offer.
	SkipSeparateDatabase SkipCategory = "separate_database_required"
	// SkipSeparateDeployment is a test that needs a Compose project this harness
	// does not start -- a different topology, a source-built image, or ports that
	// collide with the ones here.
	SkipSeparateDeployment SkipCategory = "separate_deployment_required"
	// SkipEvidenceNotYetProduced is a test whose subject does not exist at this
	// contract release. It is not DB-backed.
	SkipEvidenceNotYetProduced SkipCategory = "evidence_not_yet_produced"
)

// DeferredUntil names the milestone by which a declared skip must have been run
// somewhere else. Every allowance carries one.
//
// A skip with no deadline is a permanent waiver, and a permanent waiver is how a
// suite quietly stops proving something. These are stage-scoped debts: the test
// is not excused, it is scheduled.
type DeferredUntil string

const (
	// DeferredSatisfiedNow is a skip already covered by other evidence at this
	// same code HEAD. It is the only value that carries no future obligation.
	DeferredSatisfiedNow DeferredUntil = "satisfied_by_evidence_at_this_head"
	// DeferredV3RuntimeIntegrationPass is due before the boundary
	// "V3 RUNTIME INTEGRATION PASS -- CONTRACTS V1.5 FREEZE PENDING".
	DeferredV3RuntimeIntegrationPass DeferredUntil = "before_v3_runtime_integration_pass"
	// DeferredResultHeavyCanary is due before the Result-heavy 100x4
	// diagnosis-only v3 canary.
	DeferredResultHeavyCanary DeferredUntil = "before_result_heavy_100x4_canary"
	// DeferredContractsV15Freeze is due before contracts v1.5 freeze.
	DeferredContractsV15Freeze DeferredUntil = "before_contracts_v1_5_freeze"
	// DeferredTargetedRunEligible is due after v1.5 freeze and before
	// targeted_run_eligible becomes true.
	DeferredTargetedRunEligible DeferredUntil = "after_v1_5_freeze_before_targeted_run_eligible"
)

// AllowedSkip declares one skip, why it cannot run here, and when it must.
type AllowedSkip struct {
	Package  string       `json:"package"`
	Test     string       `json:"test"`
	Category SkipCategory `json:"category"`
	// ReasonSubstring must appear in what the test printed when it skipped. A
	// declared allowance that stops matching is reported as undeclared.
	ReasonSubstring string `json:"reason_substring"`
	// Why is the human-readable justification, and is the part a reviewer reads.
	Why string `json:"why"`
	// Scope names the harness this allowance is scoped to. An allowance is never
	// global: it says this suite cannot run this test, not that nothing need.
	Scope string `json:"scope"`
	// DeferredUntil is the milestone by which the test must have run elsewhere.
	DeferredUntil DeferredUntil `json:"deferred_until"`
	// SatisfiedByEvidence names what already covers it, when anything does.
	SatisfiedByEvidence string `json:"satisfied_by_evidence,omitempty"`
	// RequiredExternalGate names the harness that must eventually run it.
	RequiredExternalGate string `json:"required_external_gate,omitempty"`
}

// phase0Scope is the harness every current allowance is scoped to: the two
// PostgreSQL services scripts/db-test-env.sh starts.
const phase0Scope = "scripts/db-test-env.sh two-server DB harness"

// allowedSkips is the closed set. Anything else is a failure.
//
// Every entry names a concrete, checkable reason the test cannot run against
// scripts/db-test-env.sh, not a preference. Adding one is a decision about what
// the suite is allowed not to prove.
var allowedSkips = []AllowedSkip{
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/internal/finalv5sqlcheck",
		Test:    "TestBenchmarkProbeRenameIsSemanticsPreserving", Category: SkipSeparateDatabase,
		ReasonSubstring: "TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is required",
		Why: "provisions its own benchmark dataset and requires a database that does not already " +
			"carry the frozen final_v5_benchmark schema, which db/init installs on this harness's " +
			"business server; covered instead by the SQL-executability gate against a disposable empty database",
		Scope: phase0Scope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: "evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh, which runs the contract SQL against a disposable empty PostgreSQL 16.14 at this same code HEAD",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/internal/finalv5sqlcheck",
		Test:    "TestReservedKeywordCTEIsRejectedByPostgreSQL", Category: SkipSeparateDatabase,
		ReasonSubstring: "TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is required",
		Why: "same provisioning requirement as the probe-rename check; covered instead by the " +
			"SQL-executability gate against a disposable empty database",
		Scope: phase0Scope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: "evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh at this same code HEAD",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-adapter",
		Test:    "TestProvSQLLiveExternalPair", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_DIRECT_DSN",
		Why: "requires evaluation/final-v5-wsl2/compose.provsql.yaml, whose final-v5-direct-postgres " +
			"binds 127.0.0.1:25534 -- the port this harness's business server uses -- and whose " +
			"ProvSQL server is a source-built image; the two projects cannot run side by side",
		Scope: phase0Scope, DeferredUntil: DeferredV3RuntimeIntegrationPass,
		RequiredExternalGate: "evaluation/final-v5-wsl2/compose.provsql.yaml, in its own Compose project",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-adapter",
		Test:    "TestAttackAdapterLivePreflight", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_ATTACK_LIVE=1 is required",
		Why: "runs every frozen Attack arm through a full formal topology -- OA, Gateway, Control " +
			"store, MinIO, the V8 verifier and Parquet -- none of which this two-server harness starts",
		Scope: phase0Scope, DeferredUntil: DeferredContractsV15Freeze,
		RequiredExternalGate: "the final formal Compose topology with TASKGATE_FINAL_V5_ATTACK_LIVE=1",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-adapter",
		Test:    "TestRLSAdapterLivePreflight", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_RLS_LIVE=1 is required",
		Why:             "same full formal topology as the Attack preflight, plus a digest-consistent activated Catalog",
		Scope:           phase0Scope, DeferredUntil: DeferredContractsV15Freeze,
		RequiredExternalGate: "the final formal Compose topology with TASKGATE_FINAL_V5_RLS_LIVE=1",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/internal/experiment",
		Test:    "TestFormalDeploymentRunsTheApprovedHealthcheckLive", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT is not set",
		Why:             "inspects a running formal Compose project through Docker; there is none in this harness",
		Scope:           phase0Scope, DeferredUntil: DeferredResultHeavyCanary,
		RequiredExternalGate: "a running formal deployment named by TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/internal/experiment",
		Test:    "TestPeriodicLivenessProbesAddNoBusinessStatements", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT is not set",
		Why:             "measures the periodic healthcheck's Business statements on a running formal deployment",
		Scope:           phase0Scope, DeferredUntil: DeferredResultHeavyCanary,
		RequiredExternalGate: "a running formal deployment named by TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/internal/experiment",
		Test:    "TestExplicitReadinessOutsideTheWindowStillAttests", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT is not set",
		Why:             "requires a running formal Gateway to attest against",
		Scope:           phase0Scope, DeferredUntil: DeferredResultHeavyCanary,
		RequiredExternalGate: "a running formal deployment named by TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-activation-support",
		Test:    "TestCommittedManifestSupportsExactlyTheCurrentReleaseProvenProfiles", Category: SkipEvidenceNotYetProduced,
		ReasonSubstring: "no activation support manifest for this contract release yet",
		Why:             "not DB-backed; the activation-support manifest is produced at the contract release this HEAD precedes",
		Scope:           phase0Scope, DeferredUntil: DeferredTargetedRunEligible,
		RequiredExternalGate: "the activation-support manifest produced after contracts v1.5 freeze",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-activation-support",
		Test:    "TestCommittedRegistryMatchesTheManifest", Category: SkipEvidenceNotYetProduced,
		ReasonSubstring: "no activation support manifest for this contract release yet",
		Why:             "not DB-backed; same absent manifest",
		Scope:           phase0Scope, DeferredUntil: DeferredTargetedRunEligible,
		RequiredExternalGate: "the activation-support manifest produced after contracts v1.5 freeze",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-activation-support",
		Test:    "TestResultHeavyCarriesTheCurrentReleaseActivationEvidence", Category: SkipEvidenceNotYetProduced,
		ReasonSubstring: "no activation support manifest for this contract release yet",
		Why:             "not DB-backed; same absent manifest",
		Scope:           phase0Scope, DeferredUntil: DeferredTargetedRunEligible,
		RequiredExternalGate: "the activation-support manifest produced after contracts v1.5 freeze",
	},
	{
		Package: "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-activation-support",
		Test:    "TestCommittedManifestCarriesNoSecretsOrBusinessData", Category: SkipEvidenceNotYetProduced,
		ReasonSubstring: "no activation support manifest for this contract release yet",
		Why:             "not DB-backed; same absent manifest",
		Scope:           phase0Scope, DeferredUntil: DeferredTargetedRunEligible,
		RequiredExternalGate: "the activation-support manifest produced after contracts v1.5 freeze",
	},
}

// event is the subset of the `go test -json` record this reads.
type event struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// SkipRecord is one observed skip as it enters the report.
type SkipRecord struct {
	Package  string       `json:"package"`
	Test     string       `json:"test"`
	Reason   string       `json:"reason"`
	Declared bool         `json:"declared"`
	Category SkipCategory `json:"category,omitempty"`
	Why      string       `json:"why,omitempty"`
	// Scope, DeferredUntil, SatisfiedByEvidence and RequiredExternalGate turn a
	// declared skip into a debt with a due date rather than a waiver. They are
	// copied from the allowance so the retained record states the obligation,
	// not merely the excuse.
	Scope                string        `json:"scope,omitempty"`
	DeferredUntil        DeferredUntil `json:"deferred_until,omitempty"`
	SatisfiedByEvidence  string        `json:"satisfied_by_evidence,omitempty"`
	RequiredExternalGate string        `json:"required_external_gate,omitempty"`
}

// Report is the committed record.
type Report struct {
	Version string `json:"version"`
	// Commit, PostgreSQLImage, PostgreSQLVersionNum, GoVersion and Timeout are
	// the run's identity. They are supplied rather than inferred: this tool reads
	// a report, and a tool that guessed the deployment it came from would be
	// asserting exactly what the record is supposed to state.
	Commit               string `json:"commit"`
	PostgreSQLImage      string `json:"postgresql_image"`
	PostgreSQLVersionNum string `json:"postgresql_version_num"`
	GoVersion            string `json:"go_version"`
	Timeout              string `json:"package_timeout"`

	Packages       int `json:"packages"`
	PackagesFailed int `json:"packages_failed"`
	Tests          int `json:"tests"`
	TestsPassed    int `json:"tests_passed"`
	TestsFailed    int `json:"tests_failed"`
	TestsSkipped   int `json:"tests_skipped"`

	FailedPackages []string     `json:"failed_packages"`
	FailedTests    []string     `json:"failed_tests"`
	Skips          []SkipRecord `json:"skips"`
	// OutstandingObligations is every declared skip that is NOT already covered
	// by evidence, grouped by the milestone it is due before. It is the part a
	// later session reads: acceptance here is acceptance in this harness, and
	// these are what still has to happen elsewhere.
	OutstandingObligations map[DeferredUntil][]string `json:"outstanding_obligations,omitempty"`
	// UndeclaredSkips is the acceptance condition. A non-empty list is a failure
	// however green the exit code was.
	UndeclaredSkips []SkipRecord `json:"undeclared_skips"`

	// ReportSHA256 digests the go test -json stream this was derived from, so the
	// summary points at its own source.
	ReportSHA256 string `json:"report_sha256"`
	Accepted     bool   `json:"accepted"`
}

const reportVersion = "taskgate-final-v5-dbtest-report-v1"

func main() {
	var (
		commit  = flag.String("commit", "", "the commit the suite ran at")
		image   = flag.String("postgresql-image", "", "the digest-pinned PostgreSQL image")
		version = flag.String("postgresql-version-num", "", "server_version_num both servers reported")
		goVer   = flag.String("go-version", "", "the Go toolchain version")
		timeout = flag.String("timeout", "", "the per-package timeout the suite ran with")
		out     = flag.String("out", "", "write the summary here instead of stdout")
	)
	flag.Parse()

	report, err := summarize(os.Stdin, Report{
		Version: reportVersion, Commit: *commit, PostgreSQLImage: *image,
		PostgreSQLVersionNum: *version, GoVersion: *goVer, Timeout: *timeout,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "final-v5 dbtest report:", err)
		os.Exit(2)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "final-v5 dbtest report:", err)
		os.Exit(2)
	}
	encoded = append(encoded, '\n')
	if *out == "" {
		os.Stdout.Write(encoded)
	} else if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5 dbtest report:", err)
		os.Exit(2)
	}
	if !report.Accepted {
		fmt.Fprintf(os.Stderr,
			"final-v5 dbtest report: NOT accepted -- %d failed package(s), %d failed test(s), %d undeclared skip(s)\n",
			report.PackagesFailed, report.TestsFailed, len(report.UndeclaredSkips))
		for _, skip := range report.UndeclaredSkips {
			fmt.Fprintf(os.Stderr, "  undeclared skip %s %s: %s\n", skip.Package, skip.Test, skip.Reason)
		}
		os.Exit(1)
	}
}

// summarize reads a go test -json stream and settles acceptance.
func summarize(input io.Reader, base Report) (Report, error) {
	report := base
	digest := sha256.New()
	scanner := bufio.NewScanner(io.TeeReader(input, digest))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<24)

	packages := map[string]string{}
	tests := map[[2]string]string{}
	output := map[[2]string][]string{}

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var record event
		if err := json.Unmarshal(line, &record); err != nil {
			// A non-JSON line is a build failure go test printed raw. Losing it
			// silently is how a broken package becomes an empty summary.
			return Report{}, fmt.Errorf("the report contains a line that is not a go test -json record: %.120q", line)
		}
		key := [2]string{record.Package, record.Test}
		if record.Action == "output" && record.Test != "" {
			output[key] = append(output[key], record.Output)
		}
		switch record.Action {
		case "pass", "fail", "skip":
			if record.Test == "" {
				packages[record.Package] = record.Action
			} else {
				tests[key] = record.Action
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, fmt.Errorf("read the report: %w", err)
	}
	report.ReportSHA256 = hex.EncodeToString(digest.Sum(nil))

	declared := map[[2]string]AllowedSkip{}
	for _, skip := range allowedSkips {
		declared[[2]string{skip.Package, skip.Test}] = skip
	}

	report.Packages = len(packages)
	for name, result := range packages {
		if result == "fail" {
			report.PackagesFailed++
			report.FailedPackages = append(report.FailedPackages, name)
		}
	}
	report.Tests = len(tests)
	for key, result := range tests {
		switch result {
		case "pass":
			report.TestsPassed++
		case "fail":
			report.TestsFailed++
			report.FailedTests = append(report.FailedTests, key[0]+" "+key[1])
		case "skip":
			report.TestsSkipped++
			record := SkipRecord{Package: key[0], Test: key[1], Reason: skipReason(output[key])}
			if allowance, ok := declared[key]; ok &&
				strings.Contains(record.Reason, allowance.ReasonSubstring) {
				record.Declared, record.Category, record.Why = true, allowance.Category, allowance.Why
				record.Scope, record.DeferredUntil = allowance.Scope, allowance.DeferredUntil
				record.SatisfiedByEvidence = allowance.SatisfiedByEvidence
				record.RequiredExternalGate = allowance.RequiredExternalGate
				if allowance.DeferredUntil != DeferredSatisfiedNow {
					if report.OutstandingObligations == nil {
						report.OutstandingObligations = map[DeferredUntil][]string{}
					}
					report.OutstandingObligations[allowance.DeferredUntil] = append(
						report.OutstandingObligations[allowance.DeferredUntil], key[0]+" "+key[1])
				}
			}
			report.Skips = append(report.Skips, record)
			if !record.Declared {
				report.UndeclaredSkips = append(report.UndeclaredSkips, record)
			}
		}
	}
	sort.Strings(report.FailedPackages)
	sort.Strings(report.FailedTests)
	for milestone := range report.OutstandingObligations {
		sort.Strings(report.OutstandingObligations[milestone])
	}
	sortSkips(report.Skips)
	sortSkips(report.UndeclaredSkips)

	report.Accepted = report.PackagesFailed == 0 && report.TestsFailed == 0 && len(report.UndeclaredSkips) == 0
	return report, nil
}

// skipReason is the message the test printed when it skipped, with go test's own
// framing removed so the allowance matches the test's words rather than the
// runner's.
func skipReason(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "=== ") || strings.HasPrefix(trimmed, "--- ") {
			continue
		}
		return trimmed
	}
	return ""
}

func sortSkips(skips []SkipRecord) {
	sort.Slice(skips, func(left, right int) bool {
		if skips[left].Package != skips[right].Package {
			return skips[left].Package < skips[right].Package
		}
		return skips[left].Test < skips[right].Test
	})
}
