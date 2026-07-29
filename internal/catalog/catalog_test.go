package catalog

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

func validCatalogYAML(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("read test catalog: %v", err)
	}
	return string(data)
}

func TestParseValidCatalogAndResolvePolicy(t *testing.T) {
	parsed, err := Parse([]byte(validCatalogYAML(t)))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if parsed.CatalogVersion != "2026.07.21" || len(parsed.Products) != 2 {
		t.Fatalf("unexpected catalog: %#v", parsed)
	}
	if len(parsed.SHA256) != 64 {
		t.Fatalf("catalog SHA-256 = %q, want 64 lowercase hex characters", parsed.SHA256)
	}
	policy, err := parsed.ResolveTaskPolicy([]string{"expense_summary", "expense_detail"})
	if err != nil {
		t.Fatalf("ResolveTaskPolicy returned error: %v", err)
	}
	if policy.Sensitivity != domain.SensitivityHigh || policy.ApprovalRoute.Mode != domain.ApprovalModeManual {
		t.Fatalf("unexpected policy routing: %#v", policy)
	}
	if policy.Budget.MaxRows != 100 || policy.Budget.TaskTTL != 15*time.Minute {
		t.Fatalf("unexpected policy budget: %#v", policy.Budget)
	}
}

func TestRepositoryCatalog(t *testing.T) {
	parsed, err := Load("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("repository catalog is not startup-safe: %v", err)
	}
	if _, _, err := parsed.ResolveProducts([]string{"expense_summary", "expense_detail"}); err != nil {
		t.Fatalf("repository products cannot be resolved: %v", err)
	}
}

func TestCatalogRejectsRequiredInvalidInputs(t *testing.T) {
	valid := validCatalogYAML(t)
	tests := []struct {
		name   string
		yaml   string
		target error
	}{
		{
			name:   "missing field",
			yaml:   strings.Replace(valid, "catalog_version: \"2026.07.21\"\n", "", 1),
			target: ErrMissingField,
		},
		{
			name:   "duplicate product",
			yaml:   strings.Replace(valid, "name: expense_detail", "name: expense_summary", 1),
			target: ErrDuplicateProduct,
		},
		{
			name:   "illegal reporting view",
			yaml:   strings.Replace(valid, "reporting.expense_summary", "legacy.expense_summary", 1),
			target: ErrInvalidReportingView,
		},
		{
			name:   "plaintext password",
			yaml:   strings.Replace(valid, "    secretRef: env:EXPENSES_DB_PASSWORD", "    secretRef: env:EXPENSES_DB_PASSWORD\n    password: do-not-log-this", 1),
			target: ErrPlaintextPassword,
		},
		{
			name:   "missing secretRef",
			yaml:   strings.Replace(valid, "    secretRef: env:EXPENSES_DB_PASSWORD\n", "", 1),
			target: ErrMissingSecretRef,
		},
		{
			name:   "missing datasource id",
			yaml:   strings.Replace(valid, "    datasource_id: taskgate-test-expenses\n", "", 1),
			target: ErrMissingField,
		},
		{
			name:   "invalid schema digest",
			yaml:   strings.Replace(valid, "    schema_digest: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "    schema_digest: not-a-digest", 1),
			target: ErrInvalidCatalog,
		},
		{
			name: "duplicate datasource id",
			yaml: strings.Replace(valid, "    secretRef: env:EXPENSES_DB_PASSWORD\n", `    secretRef: env:EXPENSES_DB_PASSWORD
  - name: warehouse
    datasource_id: taskgate-test-expenses
    type: postgres
    address: warehouse
    port: 5432
    database: travel_demo
    user: gateway_reader
    postgres_major_version: 16
    secretRef: env:EXPENSES_DB_PASSWORD
`, 1),
			target: ErrDuplicateSource,
		},
		{
			name:   "invalid secretRef",
			yaml:   strings.Replace(valid, "env:EXPENSES_DB_PASSWORD", "actual-password", 1),
			target: ErrInvalidSecretRef,
		},
		{
			name:   "manual route without approver",
			yaml:   strings.Replace(valid, "    approver: bob\n", "", 1),
			target: ErrInvalidApprovalRoute,
		},
		{
			name:   "automatic approval disabled",
			yaml:   strings.Replace(valid, "    mode: manual", "    mode: auto", 1),
			target: ErrInvalidApprovalRoute,
		},
		{
			name:   "route with missing budget",
			yaml:   strings.Replace(valid, "budget_profile: detail_manual", "budget_profile: absent", 1),
			target: ErrInvalidApprovalRoute,
		},
		{
			name:   "invalid budget",
			yaml:   strings.Replace(valid, "    query_timeout: 5s", "    query_timeout: 45s", 1),
			target: ErrInvalidBudgetProfile,
		},
		{
			name: "V1 exposure profile removed",
			yaml: strings.Replace(valid, "    task_ttl: 30m", `    task_ttl: 30m
    max_release_facts: 10
    max_influence_facts: 20
    exposure_profile_version: taskgate-exposure-v1`, 1),
			target: ErrInvalidBudgetProfile,
		},
		{
			name:   "unattested type modifier",
			yaml:   strings.Replace(valid, "type: numeric", "type: numeric(10,2)", 1),
			target: ErrInvalidCatalog,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.yaml))
			if !errors.Is(err, test.target) {
				t.Fatalf("Parse error = %v, want %v", err, test.target)
			}
			if strings.Contains(err.Error(), "do-not-log-this") {
				t.Fatalf("error leaked plaintext password: %v", err)
			}
		})
	}
}

func TestCatalogRejectsUnknownFieldsAndNumericDurations(t *testing.T) {
	valid := validCatalogYAML(t)
	withUnknown := strings.Replace(valid, "catalog_version: \"2026.07.21\"", "catalog_version: \"2026.07.21\"\nunknown_key: value", 1)
	if _, err := Parse([]byte(withUnknown)); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("unknown field error = %v", err)
	}
	withNumericDuration := strings.Replace(valid, "max_db_time: 30s", "max_db_time: 30", 1)
	if _, err := Parse([]byte(withNumericDuration)); !errors.Is(err, ErrInvalidCatalog) {
		t.Fatalf("numeric duration error = %v", err)
	}
}

func TestCatalogRejectsEmbeddedPassword(t *testing.T) {
	valid := validCatalogYAML(t)
	withPassword := strings.Replace(valid, "address: postgres", "address: postgres://reader:super-secret@postgres/db", 1)
	_, err := Parse([]byte(withPassword))
	if !errors.Is(err, ErrPlaintextPassword) {
		t.Fatalf("embedded password error = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked embedded password: %v", err)
	}
}
