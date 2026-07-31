package catalog

import (
	"bytes"
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
	if parsed.V4Enabled() {
		t.Fatal("legacy catalog fixture unexpectedly selected V4 deployment mode")
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

func TestCatalogTimeTZBoundaryMatchesExposureProfile(t *testing.T) {
	legacy := strings.Replace(validCatalogYAML(t), "        type: date\n", "        type: time with time zone\n", 1)
	if _, err := Parse([]byte(legacy)); err != nil {
		t.Fatalf("legacy Catalog rejected supported timetz result field: %v", err)
	}

	v4Data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("read V4 Catalog: %v", err)
	}
	v4 := strings.Replace(string(v4Data), "        type: date\n", "        type: time with time zone\n", 1)
	if _, err := Parse([]byte(v4)); !errors.Is(err, ErrInvalidCatalog) ||
		!strings.Contains(err.Error(), "time with time zone is outside taskgate-exposure-v2") {
		t.Fatalf("V4 Catalog timetz error = %v, want exposure-domain rejection", err)
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
	if len(parsed.SnapshotPublications) != 2 {
		t.Fatalf("repository snapshot publications = %d, want 2", len(parsed.SnapshotPublications))
	}
	if !parsed.V4Enabled() {
		t.Fatal("repository Catalog did not select V4 deployment mode")
	}
	publication, found := parsed.LookupSnapshotPublication("expense-detail-v1")
	if !found || publication.SourceNamespace != "travel.expense_receipt" || len(publication.ManifestDigest) != 64 {
		t.Fatalf("unexpected V4 publication: %#v, found=%v", publication, found)
	}
	policy, err := parsed.ResolveTaskPolicy([]string{"expense_detail"})
	if err != nil || policy.Budget.ExposureProfileVersion != "taskgate-exposure-v4" {
		t.Fatalf("repository V4 policy = %#v, err=%v", policy, err)
	}
}

func TestParseViewContractCandidatesRelaxesOnlyMissingGeneratedContract(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	candidate := bytes.Replace(data, []byte("    snapshot_publication: expense-summary-v1\n"), nil, 1)
	if bytes.Equal(candidate, data) {
		t.Fatal("fixture did not remove the candidate publication")
	}
	if _, err := Parse(candidate); !errors.Is(err, ErrInvalidSnapshotPublication) {
		t.Fatalf("strict Parse error = %v, want %v", err, ErrInvalidSnapshotPublication)
	}
	parsed, err := ParseViewContractCandidates(candidate, []string{"expense_summary"})
	if err != nil {
		t.Fatalf("candidate parse: %v", err)
	}
	if parsed.SHA256 == "" {
		t.Fatal("candidate parse omitted exact artifact digest")
	}
	if _, err := ParseViewContractCandidates(candidate, []string{"expense_detail"}); !errors.Is(err, ErrInvalidSnapshotPublication) {
		t.Fatalf("unselected missing contract error = %v, want %v", err, ErrInvalidSnapshotPublication)
	}
	if _, err := ParseViewContractCandidates(candidate, []string{"Expense Summary"}); !errors.Is(err, ErrInvalidViewContract) {
		t.Fatalf("invalid candidate name error = %v, want %v", err, ErrInvalidViewContract)
	}
}

func TestCatalogViewContractIsAllOrNothingAndCloned(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	contract := "    view_contract:\n" +
		"      profile_version: taskgate-view-contract-v1\n" +
		"      definition_digest: " + digest + "\n" +
		"      dependency_digest: " + digest + "\n" +
		"      canonical_plan_digest: " + digest + "\n" +
		"      interface_digest: " + digest + "\n"
	withoutOpaquePublication := strings.Replace(string(data), "    snapshot_publication: expense-summary-v1\n", "", 1)
	withContract := strings.Replace(withoutOpaquePublication, "    stable_relation_role: expense_summary\n",
		"    stable_relation_role: expense_summary\n"+contract, 1)
	parsed, err := Parse([]byte(withContract))
	if err != nil {
		t.Fatalf("parse View contract: %v", err)
	}
	product, found := parsed.LookupProduct("expense_summary")
	if !found || product.ViewContract == nil || product.ViewContract.ProfileVersion != ViewContractV1 {
		t.Fatalf("missing parsed View contract: %#v", product.ViewContract)
	}
	product.ViewContract.DependencyDigest = strings.Repeat("b", 64)
	again, _ := parsed.LookupProduct("expense_summary")
	if again.ViewContract.DependencyDigest != digest {
		t.Fatal("LookupProduct returned an aliased View contract")
	}

	invalid := strings.Replace(withContract, "      interface_digest: "+digest,
		"      interface_digest: not-a-digest", 1)
	if _, err := Parse([]byte(invalid)); !errors.Is(err, ErrInvalidViewContract) {
		t.Fatalf("invalid View contract error = %v, want ErrInvalidViewContract", err)
	}

	withBoth := strings.Replace(withContract, "    stable_relation_role: expense_summary\n",
		"    stable_relation_role: expense_summary\n    snapshot_publication: expense-summary-v1\n", 1)
	if _, err := Parse([]byte(withBoth)); !errors.Is(err, ErrInvalidViewContract) {
		t.Fatalf("View contract plus opaque publication error = %v, want ErrInvalidViewContract", err)
	}
}

func TestV4CatalogRejectsMixedLegacyApprovalRoute(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the profile internally well-formed (V3 also requires Outcome), but
	// make one reachable route legacy while the other routes remain V4.
	mixed := strings.Replace(string(data),
		"exposure_profile_version: taskgate-exposure-v4",
		"exposure_profile_version: taskgate-exposure-v3", 1)
	if _, err := Parse([]byte(mixed)); !errors.Is(err, ErrInvalidApprovalRoute) {
		t.Fatalf("mixed V4/legacy Catalog error = %v, want ErrInvalidApprovalRoute", err)
	}
}

func TestV4CatalogFailsClosedOnMissingOrMismatchedPublication(t *testing.T) {
	data, err := os.ReadFile("../../config/catalog.yaml")
	if err != nil {
		t.Fatalf("read repository catalog: %v", err)
	}
	valid := string(data)
	parsed, err := Parse(data)
	if err != nil {
		t.Fatalf("parse repository catalog fixture: %v", err)
	}
	detailPublication, found := parsed.LookupSnapshotPublication("expense-detail-v1")
	if !found {
		t.Fatal("repository catalog omits expense-detail-v1")
	}
	tests := []struct {
		name string
		yaml string
	}{
		{"missing product binding", strings.Replace(valid, "    snapshot_publication: expense-detail-v1\n", "", 1)},
		{"mismatched namespace", strings.Replace(valid, "    source_namespace: travel.expense_receipt", "    source_namespace: travel.wrong", 1)},
		{"invalid manifest digest", strings.Replace(valid, detailPublication.ManifestDigest, "not-a-digest", 1)},
		{"unknown sidecar schema", strings.Replace(valid, "taskgate_ordinal.expense_detail_v1", "reporting.expense_detail_v1", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, parseErr := Parse([]byte(test.yaml))
			if !errors.Is(parseErr, ErrInvalidSnapshotPublication) {
				t.Fatalf("Parse error = %v, want ErrInvalidSnapshotPublication", parseErr)
			}
		})
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
