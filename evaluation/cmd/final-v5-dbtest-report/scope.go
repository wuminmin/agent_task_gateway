package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// composeGateScope is the Compose acceptance gate: scripts/integration-test.sh
// runs `go test -race ./...` inside the test-runner container of its own
// Compose project. That container is built from a copy of the tree, so it has
// no .git, none of the evidence-host paths and none of the live-deployment
// environment. What it may skip is therefore a closed set of its own, declared
// here separately from the two-server DB harness, and every entry still names
// the milestone by which the test must run somewhere that can.
const composeGateScope = "scripts/integration-test.sh Compose acceptance gate (test-runner container)"

const (
	// SkipHarnessHasNoGitWorktree is a test that inspects the git worktree it
	// runs in; the test-runner container carries no repository.
	SkipHarnessHasNoGitWorktree SkipCategory = "harness_has_no_git_worktree"
	// SkipHarnessSuppliesNoInput is a test that needs an input the harness
	// deliberately does not hand to the test process -- a live PostgreSQL DSN
	// or a generated fixture. The frozen Compose gate passes no such
	// environment into the container; the two-server DB harness does.
	SkipHarnessSuppliesNoInput SkipCategory = "harness_supplies_no_input"
)

const (
	// DeferredV111PublicationCampaign is due before the v1.11 publication
	// campaign is launched: its formal deployments are the only place the live
	// Attack/RLS/ProvSQL preflights and the formal-window observers can run.
	DeferredV111PublicationCampaign DeferredUntil = "before_v1_11_publication_campaign_launch"
	// DeferredNextReleaseFreeze is due at the next release-freeze bootstrap,
	// the one moment the activation-support manifest is absent by design.
	DeferredNextReleaseFreeze DeferredUntil = "at_next_release_freeze_bootstrap"
)

// dbtestSuiteEvidencePrefix marks a SatisfiedByEvidence that names the
// committed DSN-enabled suite record. Such a claim is checked, not merely read:
// the record for the gate's own commit must exist under docs/evidence, be
// accepted, and not itself have skipped the test.
const dbtestSuiteEvidencePrefix = "docs/evidence/dbtest-suite-"

const dbtestSuiteRecordAtCommit = dbtestSuiteEvidencePrefix +
	"<commit>.json, the accepted DSN-enabled suite record at this same commit (checked)"

const (
	adapterPackage           = "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-adapter"
	experimentPackage        = "taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	activationSupportPackage = "taskbound.local/agent-data-gateway/evaluation/cmd/final-v5-activation-support"
	sqlcheckPackage          = "taskbound.local/agent-data-gateway/evaluation/internal/finalv5sqlcheck"
	publicationPackage       = "taskbound.local/agent-data-gateway/evaluation/internal/finalv5publication"
	dataconnectorPackage     = "taskbound.local/agent-data-gateway/internal/dataconnector"
	sqlidentityPackage       = "taskbound.local/agent-data-gateway/internal/sqlidentity"
	repohygienePackage       = "taskbound.local/agent-data-gateway/internal/repohygiene"

	formalWindowReason = "TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT is not set"
	formalWindowGate   = "a running formal deployment named by TASKGATE_FINAL_V5_FORMAL_WINDOW_PROJECT, " +
		"as the v1.11 publication campaign's observer-window deployments are"
	campaignGate = "the v1.11 publication campaign's formal deployments " +
		"(evaluation/final-v5-wsl2/scripts/run-profile-campaign.sh)"
	businessAdminReason = "BUSINESS_ADMIN_TEST_POSTGRES_DSN is required for pg_stat_statements tests"
	businessAdminWhy    = "needs the Business admin DSN exported to the test process; the frozen Compose " +
		"gate passes none into the test-runner container, and the two-server DB harness runs it"
)

// composeGateAllowedSkips is the closed set for the Compose acceptance gate.
// Anything else the gate's go test stream skips is a failure, exactly as in the
// DB harness. The retained snapshot artifacts are deliberately NOT declared:
// they are discovered by Catalog digest on the evidence host, so on that host
// those tests run, and an allowance for them would be unmatched -- a fresh
// clone without retained evidence is expected to fail this gate, not to pass
// it by skipping.
var composeGateAllowedSkips = []AllowedSkip{
	{
		Package: activationSupportPackage,
		Test:    "TestRegistryClaimsNoSupportWithoutAManifest", Category: SkipPremiseExcludedByState,
		ReasonSubstring: "this contract release has an activation support manifest",
		Why: "between release freezes the tree carries its live activation-support manifest, so the " +
			"no-manifest premise is false; a state-independent fixture covers the invariant, and the " +
			"repository-state gate runs at each release-freeze bootstrap, where the manifest is absent " +
			"by design (last at 900fc96, the v1.11 freeze)",
		Scope: composeGateScope, DeferredUntil: DeferredNextReleaseFreeze,
		RequiredExternalGate: "the release-freeze bootstrap, which removes activation-support-v1.json " +
			"before regenerating; run ./evaluation/cmd/final-v5-activation-support then",
	},
	{
		Package: adapterPackage,
		Test:    "TestAttackAdapterLivePreflight", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_ATTACK_LIVE=1 is required",
		Why: "runs every frozen Attack arm through a full formal topology; the gate's test-runner is " +
			"a unit-test container beside a demo stack, not that topology",
		Scope: composeGateScope, DeferredUntil: DeferredV111PublicationCampaign,
		RequiredExternalGate: campaignGate + ", whose Attack cells run these arms live",
	},
	{
		Package: adapterPackage,
		Test:    "TestRLSAdapterLivePreflight", Category: SkipSeparateDeployment,
		ReasonSubstring: "TASKGATE_FINAL_V5_RLS_LIVE=1 is required",
		Why:             "same full formal topology as the Attack preflight, plus a digest-consistent activated Catalog",
		Scope:           composeGateScope, DeferredUntil: DeferredV111PublicationCampaign,
		RequiredExternalGate: campaignGate + ", whose RLS cells run these arms live",
	},
	{
		Package: adapterPackage,
		Test:    "TestProvSQLLiveExternalPair", Category: SkipSeparateDeployment,
		ReasonSubstring: "set TASKGATE_FINAL_V5_DIRECT_DSN and TASKGATE_FINAL_V5_PROVSQL_DSN",
		Why: "requires the direct and ProvSQL PostgreSQL servers of " +
			"evaluation/final-v5-wsl2/compose.provsql.yaml, which the gate's Compose project does not start",
		Scope: composeGateScope, DeferredUntil: DeferredV111PublicationCampaign,
		RequiredExternalGate: campaignGate + ", whose provsql-nonce-join deployment starts compose.provsql.yaml in its own project",
	},
	{
		Package: adapterPackage,
		Test:    "TestLiveCompilerPostgreSQLFixture", Category: SkipHarnessSuppliesNoInput,
		ReasonSubstring: "live compiler PostgreSQL DSN is not configured",
		Why: "needs a live PostgreSQL DSN exported to the test process; the frozen Compose gate passes " +
			"none into the test-runner container, and the two-server DB harness runs it",
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: experimentPackage,
		Test:    "TestFormalDeploymentRunsTheApprovedHealthcheckLive", Category: SkipSeparateDeployment,
		ReasonSubstring: formalWindowReason,
		Why:             "inspects a running formal Compose project through Docker; the gate's own project is the demo stack, not a formal deployment",
		Scope:           composeGateScope, DeferredUntil: DeferredV111PublicationCampaign,
		RequiredExternalGate: formalWindowGate,
	},
	{
		Package: experimentPackage,
		Test:    "TestPeriodicLivenessProbesAddNoBusinessStatements", Category: SkipSeparateDeployment,
		ReasonSubstring: formalWindowReason,
		Why:             "measures the periodic healthcheck's Business statements on a running formal deployment",
		Scope:           composeGateScope, DeferredUntil: DeferredV111PublicationCampaign,
		RequiredExternalGate: formalWindowGate,
	},
	{
		Package: experimentPackage,
		Test:    "TestExplicitReadinessOutsideTheWindowStillAttests", Category: SkipSeparateDeployment,
		ReasonSubstring: formalWindowReason,
		Why:             "requires a running formal Gateway to attest against",
		Scope:           composeGateScope, DeferredUntil: DeferredV111PublicationCampaign,
		RequiredExternalGate: formalWindowGate,
	},
	{
		Package: publicationPackage,
		Test:    "TestValidatePublicationOutputMutationMatrix", Category: SkipHarnessSuppliesNoInput,
		ReasonSubstring: "TASKGATE_E1_VALIDATION_FIXTURE is required",
		Why: "mutates a freshly generated E1 output handed in through TASKGATE_E1_VALIDATION_FIXTURE; " +
			"the frozen Compose gate hands the test-runner no fixture, the two-server DB harness runs it",
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: sqlcheckPackage,
		Test:    "TestBenchmarkProbeRenameIsSemanticsPreserving", Category: SkipSeparateDatabase,
		ReasonSubstring: "TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is required",
		Why: "provisions its own benchmark dataset and requires a database that does not already carry " +
			"the frozen final_v5_benchmark schema, which db/init installs on the gate's business server; " +
			"covered instead by the SQL-executability gate against a disposable empty database",
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: "evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh, whose export " +
			"evaluation/final-v5-wsl2/contracts/sql-executability-v1.json names this contract release",
	},
	{
		Package: sqlcheckPackage,
		Test:    "TestReservedKeywordCTEIsRejectedByPostgreSQL", Category: SkipSeparateDatabase,
		ReasonSubstring: "TASKGATE_FINAL_V5_SQLCHECK_ADMIN_DSN is required",
		Why:             "same provisioning requirement as the probe-rename check; covered by the same SQL-executability gate",
		Scope:           composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: "evaluation/final-v5-wsl2/scripts/run-sql-executability-gate.sh at this contract release",
	},
	{
		Package: dataconnectorPackage,
		Test:    "TestSessionPinsProduceDistinctQueryIDsLive", Category: SkipHarnessSuppliesNoInput,
		ReasonSubstring: businessAdminReason, Why: businessAdminWhy,
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: dataconnectorPackage,
		Test:    "TestSessionPinsApplyAndStayTransactionLocalLive", Category: SkipHarnessSuppliesNoInput,
		ReasonSubstring: businessAdminReason, Why: businessAdminWhy,
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: sqlidentityPackage,
		Test:    "TestSourceDerivedDigestsMatchLivePostgreSQL", Category: SkipHarnessSuppliesNoInput,
		ReasonSubstring: businessAdminReason, Why: businessAdminWhy,
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: sqlidentityPackage,
		Test:    "TestRuntimeTemplateDigestsAreStableOnLivePostgreSQL", Category: SkipHarnessSuppliesNoInput,
		ReasonSubstring: businessAdminReason, Why: businessAdminWhy,
		Scope: composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: repohygienePackage,
		Test:    "TestRepositoryRootCarriesNoTrackedBuildOutput", Category: SkipHarnessHasNoGitWorktree,
		ReasonSubstring: "not a git worktree",
		Why:             "asks git which files are tracked; the test-runner container is a copy of the tree with no repository, and the two-server DB harness runs on the host worktree",
		Scope:           composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
	{
		Package: repohygienePackage,
		Test:    "TestRepositoryRootCarriesNoUnignoredBuildOutput", Category: SkipHarnessHasNoGitWorktree,
		ReasonSubstring: "not a git worktree",
		Why:             "asks git which files are ignored; same container limitation, same host-worktree coverage",
		Scope:           composeGateScope, DeferredUntil: DeferredSatisfiedNow,
		SatisfiedByEvidence: dbtestSuiteRecordAtCommit,
	},
}

// scopes maps the -scope flag to the harness whose allowance set applies.
var scopes = map[string]string{
	"db-test-env":  phase0Scope,
	"compose-gate": composeGateScope,
}

// allowancesForScope selects the allowances declared for one harness. An
// allowance is never global, so a harness never inherits another's excuses.
func allowancesForScope(name string) ([]AllowedSkip, string, error) {
	scope, ok := scopes[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown scope %q (want db-test-env or compose-gate)", name)
	}
	var selected []AllowedSkip
	for _, allowance := range allowedSkips {
		if allowance.Scope == scope {
			selected = append(selected, allowance)
		}
	}
	for _, allowance := range composeGateAllowedSkips {
		if allowance.Scope == scope {
			selected = append(selected, allowance)
		}
	}
	return selected, scope, nil
}

// checkSatisfiedEvidence verifies every declared skip whose only excuse is the
// DSN-enabled suite record at this commit. The record must exist for exactly
// this commit, be accepted, and must not have skipped the same test; otherwise
// "satisfied by evidence" would be prose, and the report is not accepted.
func checkSatisfiedEvidence(root, commit string, skips []SkipRecord) []string {
	var problems []string
	for _, skip := range skips {
		if !skip.Declared || skip.DeferredUntil != DeferredSatisfiedNow ||
			!strings.HasPrefix(skip.SatisfiedByEvidence, dbtestSuiteEvidencePrefix) {
			continue
		}
		name := skip.Package + " " + skip.Test
		if root == "" || len(commit) < 7 {
			problems = append(problems, name+": a satisfied-by-evidence claim needs -evidence-root and a full -commit")
			continue
		}
		path := filepath.Join(root, "docs", "evidence", "dbtest-suite-"+commit[:7]+".json")
		payload, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		var record struct {
			Commit         string `json:"commit"`
			Accepted       bool   `json:"accepted"`
			PackagesFailed int    `json:"packages_failed"`
			Skips          []struct {
				Package string `json:"package"`
				Test    string `json:"test"`
			} `json:"skips"`
		}
		if err := json.Unmarshal(payload, &record); err != nil {
			problems = append(problems, fmt.Sprintf("%s: decode %s: %v", name, path, err))
			continue
		}
		if record.Commit != commit {
			problems = append(problems, fmt.Sprintf("%s: %s records commit %s, this gate ran at %s",
				name, path, record.Commit, commit))
			continue
		}
		if !record.Accepted || record.PackagesFailed != 0 {
			problems = append(problems, fmt.Sprintf("%s: %s is not an accepted record", name, path))
			continue
		}
		for _, recorded := range record.Skips {
			if recorded.Package == skip.Package && recorded.Test == skip.Test {
				problems = append(problems, fmt.Sprintf("%s: %s skipped that test as well", name, path))
				break
			}
		}
	}
	return problems
}
