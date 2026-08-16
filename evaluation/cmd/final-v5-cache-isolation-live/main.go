// Command final-v5-cache-isolation-live executes one source-controlled query
// under both Catalogs of every overlapping deployment-profile pair. It records
// only redacted identities plus the production audit/cache/Business-SQL facts
// needed to distinguish a novel execution from semantic replay.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

const (
	recordName           = "taskgate-final-v5-same-query-cross-profile-live-evidence-v2"
	queryPath            = "evaluation/final-v5-wsl2/sql/contracts/S1-bdg.sql"
	selectedRows         = "5000"
	negativeProbeProduct = "expense_detail"
)

type options struct {
	root, registryPath, intersectionPath, outputPath               string
	composeProject, composeFiles, deploymentID, currentProfileID   string
	artifactRoot, artifactManifest, activationEvidenceDir          string
	gatewayURL, oaURL, controlDSNEnv, observerDSNEnv               string
	adminTokenEnv, aliceTokenEnv, alicePasswordEnv, bobPasswordEnv string
	businessDSNEnv                                                 string
	sequence                                                       int
	readyTimeout                                                   time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.root, "root", ".", "repository root")
	flag.StringVar(&opts.registryPath, "registry", "config/profiles/registry.json", "profile registry")
	flag.StringVar(&opts.intersectionPath, "product-intersection", "evaluation/final-v5-wsl2/profiles/product-intersection-v1.json", "product-intersection matrix")
	flag.StringVar(&opts.outputPath, "evidence-out", "", "live evidence output")
	flag.StringVar(&opts.composeProject, "compose-project", "", "live Compose project")
	flag.StringVar(&opts.composeFiles, "compose-files", "compose.yaml", "colon-separated Compose files")
	flag.StringVar(&opts.deploymentID, "deployment-id", "", "deployment identity")
	flag.StringVar(&opts.currentProfileID, "current-profile-id", "", "profile served when the runner starts")
	flag.StringVar(&opts.artifactRoot, "profile-artifact-root", "", "root holding one artifact directory per profile")
	flag.StringVar(&opts.artifactManifest, "profile-artifact-manifest", "", "combined profile artifact manifest set")
	flag.StringVar(&opts.activationEvidenceDir, "activation-evidence-dir", "", "separate directory for runner switch evidence")
	flag.StringVar(&opts.gatewayURL, "gateway-url", "http://127.0.0.1:8082", "Gateway base URL")
	flag.StringVar(&opts.oaURL, "oa-url", "http://127.0.0.1:8092", "OA base URL")
	flag.StringVar(&opts.controlDSNEnv, "control-dsn-env", "TASKGATE_FINAL_V5_CONTROL_DSN", "Control DSN environment variable")
	flag.StringVar(&opts.observerDSNEnv, "observer-dsn-env", "TASKGATE_FINAL_V5_BUSINESS_OBSERVER_DSN", "Business observer DSN environment variable")
	flag.StringVar(&opts.businessDSNEnv, "business-dsn-env", "TASKGATE_FINAL_V5_BUSINESS_DSN", "Business re-attestation DSN environment variable")
	flag.StringVar(&opts.adminTokenEnv, "admin-token-env", "GATEWAY_ADMIN_TOKEN", "Gateway admin token environment variable")
	flag.StringVar(&opts.aliceTokenEnv, "alice-token-env", "TASKBOUND_ALICE_TOKEN", "Alice token environment variable")
	flag.StringVar(&opts.alicePasswordEnv, "alice-password-env", "OA_ALICE_PASSWORD", "Alice OA password environment variable")
	flag.StringVar(&opts.bobPasswordEnv, "bob-password-env", "OA_BOB_PASSWORD", "Bob OA password environment variable")
	flag.IntVar(&opts.sequence, "activation-sequence", 1, "first activation sequence used by this runner")
	flag.DurationVar(&opts.readyTimeout, "ready-timeout", 10*time.Minute, "profile activation readiness timeout")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("positional arguments are not accepted"))
	}
	if err := run(context.Background(), opts); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "final-v5-cache-isolation-live:", err)
	os.Exit(1)
}

type registryProfile struct {
	ID            string `json:"profile_id"`
	Alias         string `json:"alias"`
	CatalogSHA256 string `json:"catalog_sha256"`
	CatalogPath   string `json:"catalog_path"`
	Closure       struct {
		Products []string `json:"products"`
	} `json:"closure"`
}

type registryDocument struct {
	ContractRelease string            `json:"contract_release"`
	Profiles        []registryProfile `json:"profiles"`
}

type intersectionWire struct {
	SchemaVersion int    `json:"schema_version"`
	Record        string `json:"record"`
	Pairs         []struct {
		LeftProfileID  string   `json:"left_profile_id"`
		RightProfileID string   `json:"right_profile_id"`
		LeftAlias      string   `json:"left_alias"`
		RightAlias     string   `json:"right_alias"`
		Intersection   []string `json:"intersection"`
		Applicable     bool     `json:"same_query_live_test_applicable"`
	} `json:"pairs"`
}

type evidenceDocument struct {
	SchemaVersion                   int            `json:"schema_version"`
	Record                          string         `json:"record"`
	ContractRelease                 string         `json:"contract_release"`
	ProfileRegistrySHA256           string         `json:"profile_registry_sha256"`
	ProductIntersectionMatrixSHA256 string         `json:"product_intersection_matrix_sha256"`
	DeploymentID                    string         `json:"deployment_id"`
	QueryTemplateSHA256             string         `json:"query_template_sha256"`
	PairCount                       int            `json:"pair_count"`
	PassedPairCount                 int            `json:"passed_pair_count"`
	FailedPairCount                 int            `json:"failed_pair_count"`
	Pairs                           []pairEvidence `json:"pairs"`
	Failures                        []string       `json:"failures"`
	Status                          string         `json:"status"`
}

type pairEvidence struct {
	LeftProfileID                     string                   `json:"left_profile_id"`
	RightProfileID                    string                   `json:"right_profile_id"`
	LeftAlias                         string                   `json:"left_alias"`
	RightAlias                        string                   `json:"right_alias"`
	SharedProducts                    []string                 `json:"shared_products"`
	SelectedProduct                   string                   `json:"selected_product"`
	QuerySHA256                       string                   `json:"query_sha256"`
	LeftCatalogSHA256                 string                   `json:"left_catalog_sha256"`
	RightCatalogSHA256                string                   `json:"right_catalog_sha256"`
	FirstCacheKeySHA256               string                   `json:"first_cache_key_sha256"`
	SecondCacheKeySHA256              string                   `json:"second_cache_key_sha256"`
	FirstSQLFingerprintSHA256         string                   `json:"first_sql_fingerprint_sha256"`
	SecondSQLFingerprintSHA256        string                   `json:"second_sql_fingerprint_sha256"`
	SecondSourceQueryIsSelf           bool                     `json:"second_source_query_is_self"`
	SecondSemanticReplayAudits        int                      `json:"second_semantic_replay_audits"`
	SecondSettlementAudits            int                      `json:"second_settlement_audits"`
	SecondBusinessVisibleCallsDelta   int64                    `json:"second_business_visible_calls_delta"`
	SecondBusinessCompanionCallsDelta int64                    `json:"second_business_companion_calls_delta"`
	SecondSemanticReplay              bool                     `json:"second_semantic_replay"`
	SecondIdempotentReplay            bool                     `json:"second_idempotent_replay"`
	SecondNovelExecution              bool                     `json:"second_novel_execution"`
	LeftTaskFinalization              taskFinalizationEvidence `json:"left_task_finalization"`
	RightTaskFinalization             taskFinalizationEvidence `json:"right_task_finalization"`
	Status                            string                   `json:"status"`
}

type executionSnapshot struct {
	QueryID, CatalogSHA256, CacheKeySHA256, SQLFingerprintSHA256, SourceQueryID string
	SemanticReplayAudits, SettlementAudits                                      int
	SemanticReplay, IdempotentReplay                                            bool
}

type businessSnapshot struct{ visible, companion, dealloc, reset int64 }

type taskAuthorization struct {
	Products         []string
	Columns          map[string][]string
	Scopes           map[string]any
	BudgetProfile    string
	MaxQueries       int64
	MaxQueriesSource string
}

type taskExecution struct {
	TaskID   string
	Snapshot executionSnapshot
	Delta    businessSnapshot
}

type taskFinalizationEvidence struct {
	BudgetProfile           string `json:"budget_profile"`
	PolicyMaxQueries        int64  `json:"policy_max_queries"`
	PolicySource            string `json:"policy_source"`
	ObservedTaskState       string `json:"observed_task_state"`
	ObservedTerminalReason  string `json:"observed_terminal_reason"`
	UsedQueries             int64  `json:"used_queries"`
	RemainingQueries        int64  `json:"remaining_queries"`
	SemanticVerdictCaptured bool   `json:"semantic_verdict_captured"`
	CompleteTaskCalled      bool   `json:"complete_task_called"`
	FinalTaskState          string `json:"final_task_state"`
	FinalTerminalReason     string `json:"final_terminal_reason"`
	Disposition             string `json:"disposition"`
	Status                  string `json:"status"`
}

type taskStatus struct {
	State          string `json:"state"`
	TerminalReason string `json:"terminal_reason"`
}

type taskBudget struct {
	Budget struct {
		Limits struct {
			Queries int64 `json:"queries"`
		} `json:"limits"`
		Used struct {
			Queries int64 `json:"queries"`
		} `json:"used"`
		Remaining struct {
			Queries int64 `json:"queries"`
		} `json:"remaining"`
	} `json:"budget"`
}

func run(ctx context.Context, opts options) error {
	if opts.outputPath == "" || opts.composeProject == "" || opts.deploymentID == "" ||
		opts.currentProfileID == "" || opts.artifactRoot == "" || opts.artifactManifest == "" ||
		opts.activationEvidenceDir == "" || opts.sequence <= 0 {
		return errors.New("evidence-out, compose-project, deployment-id, current-profile-id, artifact paths and activation sequence are required")
	}
	root, err := filepath.Abs(opts.root)
	if err != nil {
		return err
	}
	opts.root = filepath.Clean(root)
	registryBytes, err := os.ReadFile(filepath.Join(opts.root, opts.registryPath))
	if err != nil {
		return err
	}
	intersectionBytes, err := os.ReadFile(filepath.Join(opts.root, opts.intersectionPath))
	if err != nil {
		return err
	}
	var registry registryDocument
	if err := json.Unmarshal(registryBytes, &registry); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	var wire intersectionWire
	if err := json.Unmarshal(intersectionBytes, &wire); err != nil {
		return fmt.Errorf("product intersection: %w", err)
	}
	if wire.SchemaVersion != 1 || wire.Record != "taskgate-final-v5-product-intersection-v1" {
		return errors.New("product-intersection identity is not recognised")
	}
	profiles := map[string]registryProfile{}
	for _, profile := range registry.Profiles {
		profiles[profile.ID] = profile
	}
	var pairs []intersectionWirePair
	for _, pair := range wire.Pairs {
		if pair.Applicable {
			pairs = append(pairs, intersectionWirePair{pair.LeftProfileID, pair.RightProfileID,
				pair.LeftAlias, pair.RightAlias, pair.Intersection})
		}
	}
	if len(pairs) == 0 {
		return errors.New("no overlapping profile pairs require live evidence")
	}
	authorizations := map[string]taskAuthorization{}
	for _, pair := range pairs {
		for _, profileID := range []string{pair.leftID, pair.rightID} {
			if _, derived := authorizations[profileID]; derived {
				continue
			}
			profile, found := profiles[profileID]
			if !found {
				return fmt.Errorf("profile %s is absent from the registry", profileID)
			}
			authorization, err := deriveTaskAuthorization(opts.root, profile, "provsql_orders")
			if err != nil {
				return fmt.Errorf("derive task authorization for profile %s: %w", profileID, err)
			}
			authorizations[profileID] = authorization
		}
	}
	template, err := os.ReadFile(filepath.Join(opts.root, queryPath))
	if err != nil {
		return err
	}
	if strings.Count(string(template), "$1") != 1 {
		return errors.New("S1 live-evidence query template does not bind exactly one parameter")
	}
	query := strings.Replace(string(template), "$1", selectedRows, 1)
	document := evidenceDocument{SchemaVersion: 2, Record: recordName,
		ContractRelease: registry.ContractRelease, ProfileRegistrySHA256: digest(registryBytes),
		ProductIntersectionMatrixSHA256: digest(intersectionBytes), DeploymentID: opts.deploymentID,
		QueryTemplateSHA256: digest(template), PairCount: len(pairs), Pairs: []pairEvidence{},
		Failures: []string{}, Status: "fail"}
	writeFailure := func(runErr error) error {
		document.Failures = append(document.Failures, runErr.Error())
		document.PassedPairCount = countPassed(document.Pairs)
		document.FailedPairCount = document.PairCount - document.PassedPairCount
		if err := writeEvidence(opts.outputPath, document); err != nil {
			return fmt.Errorf("%v; also write live evidence: %w", runErr, err)
		}
		return runErr
	}
	controlDSN, observerDSN := strings.TrimSpace(os.Getenv(opts.controlDSNEnv)), strings.TrimSpace(os.Getenv(opts.observerDSNEnv))
	if controlDSN == "" || observerDSN == "" {
		return writeFailure(errors.New("Control and Business observer DSNs are required"))
	}
	control, err := pgxpool.New(ctx, controlDSN)
	if err != nil {
		return writeFailure(err)
	}
	defer control.Close()
	observer, err := pgxpool.New(ctx, observerDSN)
	if err != nil {
		return writeFailure(err)
	}
	defer observer.Close()
	if err := control.Ping(ctx); err != nil {
		return writeFailure(err)
	}
	if err := observer.Ping(ctx); err != nil {
		return writeFailure(err)
	}
	alice := &mcpClient{url: strings.TrimRight(opts.gatewayURL, "/") + "/mcp",
		token: strings.TrimSpace(os.Getenv(opts.aliceTokenEnv)), http: &http.Client{Timeout: opts.readyTimeout}}
	if alice.token == "" {
		return writeFailure(errors.New("Alice token is required"))
	}
	current, sequence := opts.currentProfileID, opts.sequence
	for index, pair := range pairs {
		left, leftOK := profiles[pair.leftID]
		right, rightOK := profiles[pair.rightID]
		if !leftOK || !rightOK || !contains(pair.shared, "provsql_orders") ||
			contains(left.Closure.Products, negativeProbeProduct) || contains(right.Closure.Products, negativeProbeProduct) {
			return writeFailure(fmt.Errorf("pair %s/%s is not a resolvable provsql_orders overlap", pair.leftID, pair.rightID))
		}
		if err := activate(ctx, opts, current, pair.leftID, sequence); err != nil {
			return writeFailure(fmt.Errorf("activate left profile for pair %d: %w", index, err))
		}
		current, sequence = pair.leftID, sequence+1
		firstRun, err := executeQuery(ctx, opts, alice, control, observer,
			pairKey(pair), "left", query, authorizations[pair.leftID])
		if err != nil {
			return writeFailure(fmt.Errorf("execute left profile for pair %d: %w", index, err))
		}
		if err := activate(ctx, opts, current, pair.rightID, sequence); err != nil {
			return writeFailure(fmt.Errorf("activate right profile for pair %d: %w", index, err))
		}
		current, sequence = pair.rightID, sequence+1
		secondRun, err := executeQuery(ctx, opts, alice, control, observer,
			pairKey(pair), "right", query, authorizations[pair.rightID])
		if err != nil {
			return writeFailure(fmt.Errorf("execute right profile for pair %d: %w", index, err))
		}
		first, second, delta := firstRun.Snapshot, secondRun.Snapshot, secondRun.Delta
		novel := first.CatalogSHA256 == left.CatalogSHA256 && second.CatalogSHA256 == right.CatalogSHA256 &&
			isSHA256(first.CacheKeySHA256) && isSHA256(second.CacheKeySHA256) &&
			isSHA256(first.SQLFingerprintSHA256) && isSHA256(second.SQLFingerprintSHA256) &&
			first.CacheKeySHA256 != second.CacheKeySHA256 && first.SQLFingerprintSHA256 == second.SQLFingerprintSHA256 &&
			second.SourceQueryID == second.QueryID && second.SemanticReplayAudits == 0 && second.SettlementAudits == 1 &&
			delta.visible == 1 && delta.companion == 1 && !second.SemanticReplay && !second.IdempotentReplay
		entry := pairEvidence{LeftProfileID: pair.leftID, RightProfileID: pair.rightID,
			LeftAlias: pair.leftAlias, RightAlias: pair.rightAlias, SharedProducts: pair.shared,
			SelectedProduct: "provsql_orders", QuerySHA256: digest([]byte(query)),
			LeftCatalogSHA256: first.CatalogSHA256, RightCatalogSHA256: second.CatalogSHA256,
			FirstCacheKeySHA256: first.CacheKeySHA256, SecondCacheKeySHA256: second.CacheKeySHA256,
			FirstSQLFingerprintSHA256: first.SQLFingerprintSHA256, SecondSQLFingerprintSHA256: second.SQLFingerprintSHA256,
			SecondSourceQueryIsSelf:    second.SourceQueryID == second.QueryID,
			SecondSemanticReplayAudits: second.SemanticReplayAudits, SecondSettlementAudits: second.SettlementAudits,
			SecondBusinessVisibleCallsDelta: delta.visible, SecondBusinessCompanionCallsDelta: delta.companion,
			SecondSemanticReplay: second.SemanticReplay, SecondIdempotentReplay: second.IdempotentReplay,
			SecondNovelExecution: novel, Status: "fail"}
		if novel {
			entry.LeftTaskFinalization, err = finalizeTask(ctx, alice, firstRun.TaskID,
				authorizations[pair.leftID], true)
			if err == nil {
				entry.RightTaskFinalization, err = finalizeTask(ctx, alice, secondRun.TaskID,
					authorizations[pair.rightID], true)
			}
			if err != nil {
				document.Pairs = append(document.Pairs, entry)
				return writeFailure(fmt.Errorf("finalize profile pair %s/%s after semantic verdict: %w",
					pair.leftAlias, pair.rightAlias, err))
			}
		}
		if novel {
			entry.Status = "pass"
		}
		document.Pairs = append(document.Pairs, entry)
		if err := writeEvidence(opts.outputPath, document); err != nil {
			return err
		}
		if !novel {
			if second.SemanticReplay || second.SemanticReplayAudits > 0 || second.SourceQueryID != second.QueryID {
				return writeFailure(fmt.Errorf("semantic cache crossed profile pair %s/%s", pair.leftAlias, pair.rightAlias))
			}
			return writeFailure(fmt.Errorf("profile pair %s/%s did not prove a novel second execution", pair.leftAlias, pair.rightAlias))
		}
	}
	document.PassedPairCount = countPassed(document.Pairs)
	document.FailedPairCount = document.PairCount - document.PassedPairCount
	document.Status = "pass"
	if err := writeEvidence(opts.outputPath, document); err != nil {
		return err
	}
	fmt.Printf("same-query cross-profile live evidence: pass (%d/%d pairs)\n", document.PassedPairCount, document.PairCount)
	return nil
}

func deriveTaskAuthorization(root string, profile registryProfile, selectedProduct string) (taskAuthorization, error) {
	if profile.CatalogPath == "" || profile.CatalogSHA256 == "" || len(profile.Closure.Products) == 0 {
		return taskAuthorization{}, errors.New("profile registry entry omits Catalog or Product closure identity")
	}
	logical, err := catalog.Load(filepath.Join(root, profile.CatalogPath))
	if err != nil {
		return taskAuthorization{}, err
	}
	if logical.SHA256 != profile.CatalogSHA256 {
		return taskAuthorization{}, errors.New("profile Catalog bytes do not match the registry digest")
	}
	products := append([]string(nil), profile.Closure.Products...)
	policy, err := logical.ResolveTaskPolicy(products)
	if err != nil {
		return taskAuthorization{}, fmt.Errorf("profile Product closure has no compatible approval route: %w", err)
	}
	columns := make(map[string][]string, len(policy.Products))
	requiredScopes := map[string]bool{}
	selected := false
	for _, product := range policy.Products {
		selected = selected || product.Name == selectedProduct
		if len(product.Fields) == 0 {
			return taskAuthorization{}, fmt.Errorf("profile Product %s declares no fields", product.Name)
		}
		columns[product.Name] = product.FieldNames()
		for _, scope := range product.Scopes {
			requiredScopes[scope] = true
		}
	}
	if !selected {
		return taskAuthorization{}, fmt.Errorf("profile Product closure omits selected Product %s", selectedProduct)
	}
	scopes := make(map[string]any, len(requiredScopes))
	for name := range requiredScopes {
		definition, found := catalogScope(logical, name)
		if !found {
			return taskAuthorization{}, fmt.Errorf("profile Product closure requires missing scope %s", name)
		}
		switch definition.Type {
		case catalog.ScopeTypeEnum:
			if len(definition.AllowedValues) == 0 {
				return taskAuthorization{}, fmt.Errorf("profile scope %s has no allowed values", name)
			}
			scopes[name] = append([]string(nil), definition.AllowedValues...)
		case catalog.ScopeTypeDateRange:
			if definition.Min == "" || definition.Max == "" {
				return taskAuthorization{}, fmt.Errorf("profile scope %s has no closed date range", name)
			}
			scopes[name] = map[string]any{"from": definition.Min, "to": definition.Max}
		default:
			return taskAuthorization{}, fmt.Errorf("profile scope %s has unsupported type", name)
		}
	}
	policySource, err := budgetPolicySource(filepath.Join(root, profile.CatalogPath),
		profile.CatalogPath, policy.BudgetProfile)
	if err != nil {
		return taskAuthorization{}, err
	}
	return taskAuthorization{Products: products, Columns: columns, Scopes: scopes,
		BudgetProfile: policy.BudgetProfile, MaxQueries: policy.Budget.MaxQueries,
		MaxQueriesSource: policySource}, nil
}

func budgetPolicySource(absolutePath, relativePath, budgetProfile string) (string, error) {
	payload, err := os.ReadFile(absolutePath)
	if err != nil {
		return "", err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(payload, &document); err != nil {
		return "", fmt.Errorf("decode Catalog policy source: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return "", errors.New("Catalog policy source has no root mapping")
	}
	budgets := mappingValue(document.Content[0], "budget_profiles")
	if budgets == nil || budgets.Kind != yaml.SequenceNode {
		return "", errors.New("Catalog policy source omits budget_profiles")
	}
	for _, item := range budgets.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		name, maxQueries := mappingValue(item, "name"), mappingValue(item, "max_queries")
		if name != nil && name.Value == budgetProfile && maxQueries != nil && maxQueries.Line > 0 {
			return filepath.ToSlash(relativePath) + ":" + fmt.Sprint(maxQueries.Line), nil
		}
	}
	return "", fmt.Errorf("Catalog policy source omits max_queries for budget profile %s", budgetProfile)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func catalogScope(logical *catalog.Catalog, name string) (catalog.Scope, bool) {
	for _, scope := range logical.Scopes {
		if scope.Name == name {
			return scope, true
		}
	}
	return catalog.Scope{}, false
}

type intersectionWirePair struct {
	leftID, rightID, leftAlias, rightAlias string
	shared                                 []string
}

func pairKey(pair intersectionWirePair) string { return pair.leftID + "/" + pair.rightID }

func activate(ctx context.Context, opts options, previous, profile string, sequence int) error {
	output := filepath.Join(opts.activationEvidenceDir, fmt.Sprintf("%03d-%s.json", sequence, profile))
	args := []string{"run", "./evaluation/cmd/final-v5-profile-activate", "-root", opts.root,
		"-compose-project", opts.composeProject, "-compose-files", opts.composeFiles,
		"-deployment-id", opts.deploymentID, "-profile-id", profile, "-registry", opts.registryPath,
		"-gateway-url", opts.gatewayURL, "-previous-profile-id", previous,
		"-activation-sequence", fmt.Sprint(sequence), "-evidence-out", output,
		"-profile-artifact-dir", filepath.Join(opts.artifactRoot, profile),
		"-profile-artifact-manifest", opts.artifactManifest, "-business-dsn-env", opts.businessDSNEnv,
		"-admin-token-env", opts.adminTokenEnv,
		"-outside-products", negativeProbeProduct,
		"-ready-timeout", opts.readyTimeout.String()}
	command := exec.CommandContext(ctx, "go", args...)
	command.Dir = opts.root
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return err
	}
	payload, err := os.ReadFile(output)
	if err != nil {
		return fmt.Errorf("read activation evidence: %w", err)
	}
	var evidence struct {
		ProfileID             string `json:"profile_id"`
		PreviousProfileID     string `json:"previous_profile_id"`
		ActivationSequence    int    `json:"activation_sequence"`
		CatalogSHA256         string `json:"catalog_sha256"`
		ActivationSmokePassed bool   `json:"activation_smoke_passed"`
		Status                string `json:"status"`
	}
	if err := json.Unmarshal(payload, &evidence); err != nil {
		return fmt.Errorf("decode activation evidence: %w", err)
	}
	if evidence.ProfileID != profile || evidence.PreviousProfileID != previous ||
		evidence.ActivationSequence != sequence || !isSHA256(evidence.CatalogSHA256) ||
		!evidence.ActivationSmokePassed || evidence.Status != "pass" {
		return errors.New("profile activation evidence did not pass")
	}
	return nil
}

func executeQuery(ctx context.Context, opts options, alice *mcpClient,
	control, observer *pgxpool.Pool, pair, side, query string,
	authorization taskAuthorization) (taskExecution, error) {
	aliceOA, err := oaClient(opts.oaURL, "alice", os.Getenv(opts.alicePasswordEnv), opts.readyTimeout)
	if err != nil {
		return taskExecution{}, err
	}
	bobOA, err := oaClient(opts.oaURL, "bob", os.Getenv(opts.bobPasswordEnv), opts.readyTimeout)
	if err != nil {
		return taskExecution{}, err
	}
	var created struct {
		TaskID string `json:"task_id"`
		OAURL  string `json:"oa_url"`
	}
	if err := alice.call(ctx, "request_data_task", map[string]any{
		"objective":     "P26 same-query cross-profile live " + pair + " " + side,
		"data_products": authorization.Products,
		"columns":       authorization.Columns,
		"scopes":        authorization.Scopes,
	}, &created); err != nil {
		return taskExecution{}, err
	}
	if created.TaskID == "" || created.OAURL == "" {
		return taskExecution{}, errors.New("task request omitted identity")
	}
	draft := filepath.Base(created.OAURL)
	if err := oaAction(ctx, aliceOA, opts.oaURL, draft, "submit", ""); err != nil {
		return taskExecution{}, err
	}
	if err := waitTask(ctx, alice, created.TaskID, "AWAITING_APPROVAL", opts.readyTimeout); err != nil {
		return taskExecution{}, err
	}
	if err := oaAction(ctx, bobOA, opts.oaURL, draft, "decision", "approved"); err != nil {
		return taskExecution{}, err
	}
	if err := waitTask(ctx, alice, created.TaskID, "ACTIVE", opts.readyTimeout); err != nil {
		return taskExecution{}, err
	}
	before, err := businessSQLSnapshot(ctx, observer)
	if err != nil {
		return taskExecution{}, err
	}
	requestID := "p26-" + digest([]byte(pair + "/" + side))[:20]
	var response struct {
		QueryID, TaskID                  string
		SemanticReplay, IdempotentReplay bool
	}
	var decoded struct {
		QueryID          string `json:"query_id"`
		TaskID           string `json:"task_id"`
		SemanticReplay   bool   `json:"semantic_replay"`
		IdempotentReplay bool   `json:"idempotent_replay"`
	}
	if err := alice.call(ctx, "query_sql", map[string]any{"task_id": created.TaskID,
		"request_id": requestID, "sql": query}, &decoded); err != nil {
		return taskExecution{}, err
	}
	response.QueryID, response.TaskID = decoded.QueryID, decoded.TaskID
	response.SemanticReplay, response.IdempotentReplay = decoded.SemanticReplay, decoded.IdempotentReplay
	after, err := businessSQLSnapshot(ctx, observer)
	if err != nil {
		return taskExecution{}, err
	}
	if before.reset != after.reset || before.dealloc != after.dealloc {
		return taskExecution{}, errors.New("pg_stat_statements changed identity during query")
	}
	var snapshot executionSnapshot
	var cacheKey, sourceQuery string
	err = control.QueryRow(ctx, `
SELECT q.id,q.catalog_digest,q.sql_fingerprint,
       COALESCE(m.cache_key_sha256,''),COALESCE(m.source_query_id,''),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_SEMANTIC_REPLAY'),
       (SELECT count(*) FROM audit_events e WHERE e.query_id=q.id AND e.event_type='QUERY_V5_EXPOSURE_SETTLED')
FROM query_records q
LEFT JOIN v5_committed_materializations m ON m.task_id=q.task_id AND m.source_query_id=q.id
WHERE q.task_id=$1 AND q.request_id=$2`, created.TaskID, requestID).Scan(&snapshot.QueryID,
		&snapshot.CatalogSHA256, &snapshot.SQLFingerprintSHA256, &cacheKey, &sourceQuery,
		&snapshot.SemanticReplayAudits, &snapshot.SettlementAudits)
	if err != nil {
		return taskExecution{}, err
	}
	if snapshot.QueryID == "" || snapshot.QueryID != response.QueryID || response.TaskID != created.TaskID ||
		snapshot.CatalogSHA256 == "" || snapshot.SQLFingerprintSHA256 == "" {
		return taskExecution{}, errors.New("query response and persisted execution identity differ")
	}
	snapshot.CacheKeySHA256, snapshot.SourceQueryID = cacheKey, sourceQuery
	snapshot.SemanticReplay, snapshot.IdempotentReplay = response.SemanticReplay, response.IdempotentReplay
	return taskExecution{TaskID: created.TaskID, Snapshot: snapshot,
		Delta: businessSnapshot{visible: after.visible - before.visible,
			companion: after.companion - before.companion, reset: after.reset, dealloc: after.dealloc}}, nil
}

func finalizeTask(ctx context.Context, alice *mcpClient, taskID string,
	authorization taskAuthorization, semanticVerdictCaptured bool) (taskFinalizationEvidence, error) {
	evidence := taskFinalizationEvidence{BudgetProfile: authorization.BudgetProfile,
		PolicyMaxQueries: authorization.MaxQueries, PolicySource: authorization.MaxQueriesSource,
		SemanticVerdictCaptured: semanticVerdictCaptured, Status: "fail"}
	var status taskStatus
	if err := alice.call(ctx, "get_task_status", map[string]string{"task_id": taskID}, &status); err != nil {
		return evidence, fmt.Errorf("read task state: %w", err)
	}
	var budget taskBudget
	if err := alice.call(ctx, "get_budget", map[string]string{"task_id": taskID}, &budget); err != nil {
		return evidence, fmt.Errorf("read task budget: %w", err)
	}
	evidence.ObservedTaskState, evidence.ObservedTerminalReason = status.State, status.TerminalReason
	evidence.UsedQueries, evidence.RemainingQueries = budget.Budget.Used.Queries, budget.Budget.Remaining.Queries
	disposition, err := taskFinalizationDisposition(authorization, status, budget, semanticVerdictCaptured)
	evidence.Disposition = disposition
	if err != nil {
		return evidence, err
	}
	if disposition == "complete_active_task" {
		evidence.CompleteTaskCalled = true
		var completed taskStatus
		if err := alice.call(ctx, "complete_task", map[string]any{"task_id": taskID,
			"summary": "P28 same-query live probe complete after semantic verdict"}, &completed); err != nil {
			return evidence, fmt.Errorf("complete ACTIVE task: %w", err)
		}
		evidence.FinalTaskState, evidence.FinalTerminalReason = completed.State, completed.TerminalReason
		if completed.State != "ARCHIVED" || completed.TerminalReason != "completed" {
			return evidence, errors.New("complete_task did not return the expected completed ARCHIVED state")
		}
	} else {
		evidence.FinalTaskState, evidence.FinalTerminalReason = status.State, status.TerminalReason
	}
	evidence.Status = "pass"
	return evidence, nil
}

func taskFinalizationDisposition(authorization taskAuthorization, status taskStatus,
	budget taskBudget, semanticVerdictCaptured bool) (string, error) {
	if authorization.BudgetProfile == "" || authorization.MaxQueries <= 0 || authorization.MaxQueriesSource == "" {
		return "", errors.New("source-controlled query policy identity is incomplete")
	}
	limits, used, remaining := budget.Budget.Limits.Queries, budget.Budget.Used.Queries, budget.Budget.Remaining.Queries
	if limits != authorization.MaxQueries || used < 0 || remaining < 0 || used+remaining != limits {
		return "", errors.New("live task budget does not match the source-controlled max_queries policy")
	}
	if !semanticVerdictCaptured {
		return "", errors.New("task finalization was attempted before the semantic verdict was captured")
	}
	switch status.State {
	case "ACTIVE":
		if remaining == 0 {
			return "", errors.New("query budget is exhausted but the task is unexpectedly ACTIVE")
		}
		if status.TerminalReason != "" {
			return "", errors.New("ACTIVE task unexpectedly carries a terminal reason")
		}
		return "complete_active_task", nil
	case "ARCHIVED":
		if used != limits || remaining != 0 || status.TerminalReason != "budget_exhausted" {
			return "", errors.New("ARCHIVED task is not the source-policy-derived query-budget exhaustion case")
		}
		return "accept_automatic_budget_archive", nil
	default:
		return "", fmt.Errorf("task is neither ACTIVE nor ARCHIVED after query: %s", status.State)
	}
}

func businessSQLSnapshot(ctx context.Context, observer *pgxpool.Pool) (businessSnapshot, error) {
	const query = `WITH statements AS (
  SELECT s.calls::bigint AS calls,replace(lower(s.query),'"','') AS normalized_query
  FROM pg_stat_statements s
  WHERE s.dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
    AND s.userid=(SELECT oid FROM pg_roles WHERE rolname='gateway_reader')
)
SELECT COALESCE(sum(calls) FILTER (WHERE position('reporting.provsql_orders' in normalized_query)>0
                                   AND position('taskgate_ordinal.provsql_orders_v1' in normalized_query)=0),0)::bigint,
       COALESCE(sum(calls) FILTER (WHERE position('taskgate_ordinal.provsql_orders_v1' in normalized_query)>0),0)::bigint,
       extract(epoch from info.stats_reset)*1000000,info.dealloc::bigint
FROM pg_stat_statements_info info LEFT JOIN statements ON true GROUP BY info.stats_reset,info.dealloc`
	var value businessSnapshot
	var reset float64
	if err := observer.QueryRow(ctx, query).Scan(&value.visible, &value.companion, &reset, &value.dealloc); err != nil {
		return value, err
	}
	value.reset = int64(reset)
	return value, nil
}

type mcpClient struct {
	url, token string
	http       *http.Client
	next       atomic.Int64
}

func (client *mcpClient) call(ctx context.Context, tool string, arguments, output any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": client.next.Add(1),
		"method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("MCP HTTP status %d", response.StatusCode)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &rpc); err != nil {
		return err
	}
	if rpc.Error != nil {
		return fmt.Errorf("MCP RPC error %d", rpc.Error.Code)
	}
	var result struct {
		IsError    bool            `json:"isError"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return err
	}
	if result.IsError {
		return errors.New("MCP tool returned an error")
	}
	if len(result.Structured) == 0 {
		return errors.New("MCP tool omitted structured content")
	}
	return json.Unmarshal(result.Structured, output)
}

func waitTask(ctx context.Context, client *mcpClient, taskID, state string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var current struct {
			State string `json:"state"`
		}
		if client.call(ctx, "get_task_status", map[string]string{"task_id": taskID}, &current) == nil && current.State == state {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("task did not reach %s", state)
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func oaClient(base, user, password string, timeout time.Duration) (*http.Client, error) {
	if strings.TrimSpace(password) == "" {
		return nil, errors.New("OA password is required")
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: timeout}
	page, err := httpGet(context.Background(), client, strings.TrimRight(base, "/")+"/login")
	if err != nil {
		return nil, err
	}
	match := csrfPattern.FindSubmatch(page)
	if len(match) != 2 {
		return nil, errors.New("OA login omitted CSRF token")
	}
	_, err = httpPost(context.Background(), client, strings.TrimRight(base, "/")+"/login",
		url.Values{"csrf": {string(match[1])}, "username": {user}, "password": {password}})
	return client, err
}

func oaAction(ctx context.Context, client *http.Client, base, draft, action, decision string) error {
	target := strings.TrimRight(base, "/") + "/tasks/" + url.PathEscape(draft)
	page, err := httpGet(ctx, client, target)
	if err != nil {
		return err
	}
	match := csrfPattern.FindSubmatch(page)
	if len(match) != 2 {
		return errors.New("OA task page omitted CSRF token")
	}
	values := url.Values{"csrf": {string(match[1])}}
	if decision != "" {
		values.Set("decision", decision)
	}
	_, err = httpPost(ctx, client, target+"/"+action, values)
	return err
}

func httpGet(ctx context.Context, client *http.Client, target string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET returned %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 2<<20))
}

func httpPost(ctx context.Context, client *http.Client, target string, values url.Values) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return nil, fmt.Errorf("POST returned %d", response.StatusCode)
	}
	return body, nil
}

func writeEvidence(path string, document evidenceDocument) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o600)
}

func countPassed(pairs []pairEvidence) int {
	count := 0
	for _, pair := range pairs {
		if pair.Status == "pass" {
			count++
		}
	}
	return count
}
func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}
func digest(payload []byte) string {
	value := sha256.Sum256(payload)
	return hex.EncodeToString(value[:])
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
