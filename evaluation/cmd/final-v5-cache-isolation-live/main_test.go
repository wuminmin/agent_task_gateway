package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func TestTaskAuthorizationClosureComesFromSourceControlledProfileRegistry(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	registryBytes, err := os.ReadFile(filepath.Join(root, "config/profiles/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry registryDocument
	if err := json.Unmarshal(registryBytes, &registry); err != nil {
		t.Fatal(err)
	}
	intersectionBytes, err := os.ReadFile(filepath.Join(root,
		"evaluation/final-v5-wsl2/profiles/product-intersection-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var intersection intersectionWire
	if err := json.Unmarshal(intersectionBytes, &intersection); err != nil {
		t.Fatal(err)
	}
	requiredProfiles := map[string]bool{}
	for _, pair := range intersection.Pairs {
		if pair.Applicable {
			requiredProfiles[pair.LeftProfileID] = true
			requiredProfiles[pair.RightProfileID] = true
		}
	}
	if len(requiredProfiles) == 0 {
		t.Fatal("source-controlled intersection matrix has no applicable profiles")
	}
	var nonceProfile registryProfile
	for _, profile := range registry.Profiles {
		if requiredProfiles[profile.ID] {
			authorization, err := deriveTaskAuthorization(root, profile, "provsql_orders")
			if err != nil {
				t.Fatalf("derive %s authorization: %v", profile.Alias, err)
			}
			if !reflect.DeepEqual(authorization.Products, profile.Closure.Products) {
				t.Fatalf("%s authorization Products = %v, want registry closure %v",
					profile.Alias, authorization.Products, profile.Closure.Products)
			}
			logical, err := catalog.Load(filepath.Join(root, profile.CatalogPath))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := logical.ResolveTaskPolicy(authorization.Products)
			if err != nil {
				t.Fatalf("%s derived closure does not resolve through the source-controlled approval route: %v",
					profile.Alias, err)
			}
			for _, product := range policy.Products {
				if !reflect.DeepEqual(authorization.Columns[product.Name], product.FieldNames()) {
					t.Fatalf("authorization columns for %s = %v, want Catalog fields %v",
						product.Name, authorization.Columns[product.Name], product.FieldNames())
				}
			}
			delete(requiredProfiles, profile.ID)
		}
		if profile.Alias == "provsql-nonce-join" {
			nonceProfile = profile
		}
	}
	if len(requiredProfiles) != 0 {
		t.Fatalf("applicable profiles absent from registry: %v", requiredProfiles)
	}
	if nonceProfile.ID == "" {
		t.Fatal("source-controlled registry omits provsql-nonce-join")
	}
	authorization, err := deriveTaskAuthorization(root, nonceProfile, "provsql_orders")
	if err != nil {
		t.Fatal(err)
	}
	logical, err := catalog.Load(filepath.Join(root, nonceProfile.CatalogPath))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := logical.ResolveTaskPolicy(authorization.Products)
	if err != nil {
		t.Fatalf("derived closure does not resolve through the source-controlled approval route: %v", err)
	}
	if policy.BudgetProfile != "final-v5-provsql-low-v1" {
		t.Fatalf("derived closure selected budget profile %q", policy.BudgetProfile)
	}
	if authorization.BudgetProfile != policy.BudgetProfile || authorization.MaxQueries != 1 ||
		authorization.MaxQueriesSource != "config/profiles/provsql-nonce-join.catalog.yaml:172" {
		t.Fatalf("derived query policy = %#v", authorization)
	}
	if values, ok := authorization.Scopes["partition_key"].([]string); !ok || !reflect.DeepEqual(values, []string{"1"}) {
		t.Fatalf("authorization partition scope = %#v, want Catalog value [1]", authorization.Scopes["partition_key"])
	}

	// Removing one registry closure member must not be repaired by a hidden
	// runner-side list: the product-scoped Catalog route rejects the subset.
	mutated := nonceProfile
	mutated.Closure.Products = []string{"provsql_lineitem", "provsql_orders"}
	if _, err := deriveTaskAuthorization(root, mutated, "provsql_orders"); err == nil {
		t.Fatal("registry closure mutation unexpectedly retained a compatible approval route")
	}
}

func TestEveryRegistryProfileRouteMatchesRunnerAssumptions(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "config/profiles/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry registryDocument
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, profile := range registry.Profiles {
		if profile.Alias == "" || len(profile.Closure.Products) == 0 {
			t.Fatalf("registry profile omits alias or closure: %#v", profile)
		}
		seen[profile.Alias] = true
		logical, err := catalog.Load(filepath.Join(root, profile.CatalogPath))
		if err != nil {
			t.Fatalf("load %s: %v", profile.Alias, err)
		}
		policy, err := logical.ResolveTaskPolicy(profile.Closure.Products)
		if err != nil {
			t.Fatalf("resolve %s closure route: %v", profile.Alias, err)
		}
		if policy.ApprovalRoute.Mode != "manual" || policy.ApprovalRoute.Approver != "bob" {
			t.Fatalf("%s route is not compatible with the runner OA path: %#v",
				profile.Alias, policy.ApprovalRoute)
		}
		authorization, err := deriveTaskAuthorization(root, profile, profile.Closure.Products[0])
		if err != nil {
			t.Fatalf("derive %s route-bound authorization: %v", profile.Alias, err)
		}
		if authorization.BudgetProfile != policy.BudgetProfile ||
			authorization.MaxQueries != policy.Budget.MaxQueries || authorization.MaxQueries < 1 ||
			!strings.HasPrefix(authorization.MaxQueriesSource, profile.CatalogPath+":") {
			t.Fatalf("%s query policy was not derived from its Catalog: %#v", profile.Alias, authorization)
		}
		for _, product := range policy.Products {
			if !reflect.DeepEqual(authorization.Columns[product.Name], product.FieldNames()) {
				t.Fatalf("%s columns for %s are not Catalog-derived", profile.Alias, product.Name)
			}
			for _, scope := range product.Scopes {
				if _, found := authorization.Scopes[scope]; !found {
					t.Fatalf("%s authorization omits Catalog scope %s", profile.Alias, scope)
				}
			}
		}
	}
	for _, required := range []string{"rls-unlimited", "expense-detail", "attack-expense-detail", "rls-bounded",
		"concurrency-expense-detail", "depth4-semantic-view", "analytics-orders-lineitem", "exposure-scale",
		"provsql-nonce-join", "result-heavy", "analytics-orders"} {
		if !seen[required] {
			t.Errorf("source-controlled registry omits audited profile %s", required)
		}
	}
}

func TestTaskFinalizationUsesSourceControlledQueryPolicy(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "config/profiles/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry registryDocument
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatal(err)
	}
	var nonce, analytics registryProfile
	for _, profile := range registry.Profiles {
		switch profile.Alias {
		case "provsql-nonce-join":
			nonce = profile
		case "analytics-orders":
			analytics = profile
		}
	}
	nonceAuthorization, err := deriveTaskAuthorization(root, nonce, "provsql_orders")
	if err != nil {
		t.Fatal(err)
	}
	analyticsAuthorization, err := deriveTaskAuthorization(root, analytics, "provsql_orders")
	if err != nil {
		t.Fatal(err)
	}
	archivedBudget := queryBudget(1, 1, 0)
	disposition, err := taskFinalizationDisposition(nonceAuthorization,
		taskStatus{State: "ARCHIVED", TerminalReason: "budget_exhausted"}, archivedBudget, true)
	if err != nil || disposition != "accept_automatic_budget_archive" {
		t.Fatalf("source-controlled one-query archive disposition = %q, %v", disposition, err)
	}
	activeBudget := queryBudget(128, 1, 127)
	disposition, err = taskFinalizationDisposition(analyticsAuthorization,
		taskStatus{State: "ACTIVE"}, activeBudget, true)
	if err != nil || disposition != "complete_active_task" {
		t.Fatalf("source-controlled 128-query active disposition = %q, %v", disposition, err)
	}

	for name, testCase := range map[string]struct {
		authorization taskAuthorization
		status        taskStatus
		budget        taskBudget
		verdict       bool
	}{
		"TASK_NOT_ACTIVE for manual completion is not swallowed": {
			analyticsAuthorization, taskStatus{State: "ARCHIVED", TerminalReason: "completed"}, activeBudget, true},
		"archive before policy exhaustion": {
			analyticsAuthorization, taskStatus{State: "ARCHIVED", TerminalReason: "budget_exhausted"}, activeBudget, true},
		"live budget differs from Catalog": {
			nonceAuthorization, taskStatus{State: "ARCHIVED", TerminalReason: "budget_exhausted"}, queryBudget(2, 2, 0), true},
		"semantic verdict not captured": {
			nonceAuthorization, taskStatus{State: "ARCHIVED", TerminalReason: "budget_exhausted"}, archivedBudget, false},
		"exhausted task unexpectedly active": {
			nonceAuthorization, taskStatus{State: "ACTIVE"}, archivedBudget, true},
	} {
		t.Run(name, func(t *testing.T) {
			if disposition, err := taskFinalizationDisposition(testCase.authorization, testCase.status,
				testCase.budget, testCase.verdict); err == nil {
				t.Fatalf("unsafe task finalization was accepted as %q", disposition)
			}
		})
	}
}

func queryBudget(limit, used, remaining int64) taskBudget {
	var budget taskBudget
	budget.Budget.Limits.Queries = limit
	budget.Budget.Used.Queries = used
	budget.Budget.Remaining.Queries = remaining
	return budget
}
