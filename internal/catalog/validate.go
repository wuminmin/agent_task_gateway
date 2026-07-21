package catalog

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/domain"
)

var (
	identifierPattern    = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	configNamePattern    = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	versionPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	secretRefPattern     = regexp.MustCompile(`^(?:env:)?[A-Z][A-Z0-9_]{1,127}$`)
	databaseNamePattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	reportingViewPattern = regexp.MustCompile(`^reporting\.[a-z_][a-z0-9_]*$`)
	typePattern          = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]*(?:\([0-9]+(?:,[0-9]+)?\))?$`)
	functionPattern      = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
)

var safeOperators = map[string]struct{}{
	"=": {}, "<>": {}, "!=": {}, "<": {}, "<=": {}, ">": {}, ">=": {},
	"+": {}, "-": {}, "*": {}, "/": {}, "%": {}, "and": {}, "or": {},
	"not": {}, "like": {}, "ilike": {}, "in": {}, "between": {}, "is": {},
	"is not": {},
}

var forbiddenFunctions = map[string]struct{}{
	"dblink": {}, "lo_export": {}, "lo_import": {}, "pg_ls_dir": {},
	"pg_read_binary_file": {}, "pg_read_file": {}, "pg_sleep": {},
	"pg_terminate_backend": {}, "set_config": {},
}

func (c *Catalog) Validate() error {
	if c == nil {
		return fieldError("catalog", "catalog is nil", ErrMissingField)
	}

	var problems ValidationErrors
	if !versionPattern.MatchString(c.CatalogVersion) {
		problems = append(problems, fieldError("catalog_version", "a version using letters, digits, '.', '_' or '-' is required", ErrMissingField))
	}
	if len(c.Sources) == 0 {
		problems = append(problems, fieldError("sources", "at least one source is required", ErrMissingField))
	}
	if len(c.Products) == 0 {
		problems = append(problems, fieldError("products", "at least one product is required", ErrMissingField))
	}
	if len(c.Scopes) == 0 {
		problems = append(problems, fieldError("scopes", "at least one scope is required", ErrMissingField))
	}
	if len(c.ApprovalRoutes) == 0 {
		problems = append(problems, fieldError("approval_routes", "at least one route is required", ErrMissingField))
	}
	if len(c.BudgetProfiles) == 0 {
		problems = append(problems, fieldError("budget_profiles", "at least one profile is required", ErrMissingField))
	}

	sources := make(map[string]struct{}, len(c.Sources))
	for index, source := range c.Sources {
		path := fmt.Sprintf("sources[%d]", index)
		problems = append(problems, validateSource(path, source)...)
		if source.Name != "" {
			if _, exists := sources[source.Name]; exists {
				problems = append(problems, fieldError(path+".name", "source name is duplicated", ErrDuplicateSource))
			}
			sources[source.Name] = struct{}{}
		}
	}

	scopes := make(map[string]struct{}, len(c.Scopes))
	for index, scope := range c.Scopes {
		path := fmt.Sprintf("scopes[%d]", index)
		problems = append(problems, validateScope(path, scope)...)
		if scope.Name != "" {
			if _, exists := scopes[scope.Name]; exists {
				problems = append(problems, fieldError(path+".name", "scope name is duplicated", ErrInvalidCatalog))
			}
			scopes[scope.Name] = struct{}{}
		}
	}

	profiles := make(map[string]BudgetProfile, len(c.BudgetProfiles))
	for index, profile := range c.BudgetProfiles {
		path := fmt.Sprintf("budget_profiles[%d]", index)
		problems = append(problems, validateBudgetProfile(path, profile)...)
		if profile.Name != "" {
			if _, exists := profiles[profile.Name]; exists {
				problems = append(problems, fieldError(path+".name", "budget profile name is duplicated", ErrInvalidBudgetProfile))
			}
			profiles[profile.Name] = profile
		}
	}

	routes := make(map[domain.Sensitivity]ApprovalRoute, len(c.ApprovalRoutes))
	for index, route := range c.ApprovalRoutes {
		path := fmt.Sprintf("approval_routes[%d]", index)
		problems = append(problems, validateApprovalRoute(path, route, profiles)...)
		if route.Sensitivity != "" {
			if _, exists := routes[route.Sensitivity]; exists {
				problems = append(problems, fieldError(path+".sensitivity", "approval route is duplicated", ErrInvalidApprovalRoute))
			}
			routes[route.Sensitivity] = route
		}
	}

	products := make(map[string]struct{}, len(c.Products))
	views := make(map[string]struct{}, len(c.Products))
	for index, product := range c.Products {
		path := fmt.Sprintf("products[%d]", index)
		problems = append(problems, validateProduct(path, product, sources, scopes)...)
		if product.Name != "" {
			if _, exists := products[product.Name]; exists {
				problems = append(problems, fieldError(path+".name", "product name is duplicated", ErrDuplicateProduct))
			}
			products[product.Name] = struct{}{}
		}
		if product.ReportingView != "" {
			if _, exists := views[product.ReportingView]; exists {
				problems = append(problems, fieldError(path+".reporting_view", "reporting view is published more than once", ErrInvalidReportingView))
			}
			views[product.ReportingView] = struct{}{}
		}
		if sensitivity, err := product.EffectiveSensitivity(); err == nil {
			if _, exists := routes[sensitivity]; !exists {
				problems = append(problems, fieldError(path+".sensitivity", "no approval route exists for the effective sensitivity", ErrInvalidApprovalRoute))
			}
		}
	}

	if len(problems) > 0 {
		return problems
	}
	return nil
}

func validateSource(path string, source Source) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(source.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase logical name is required", ErrMissingField))
	}
	if source.Type != "postgres" && source.Type != "postgresql" {
		problems = append(problems, fieldError(path+".type", "type must be postgres", ErrInvalidCatalog))
	}
	if strings.TrimSpace(source.Address) == "" || strings.ContainsAny(source.Address, " /@\t\r\n") || containsUserPassword(source.Address) {
		problems = append(problems, fieldError(path+".address", "a host address without credentials is required", ErrInvalidCatalog))
	}
	if source.Port < 1 || source.Port > 65535 {
		problems = append(problems, fieldError(path+".port", "port must be between 1 and 65535", ErrInvalidCatalog))
	}
	if !databaseNamePattern.MatchString(source.Database) {
		problems = append(problems, fieldError(path+".database", "a database name is required", ErrMissingField))
	}
	if !databaseNamePattern.MatchString(source.User) {
		problems = append(problems, fieldError(path+".user", "a database user is required", ErrMissingField))
	}
	if source.Password != "" || source.DSN != "" {
		problems = append(problems, fieldError(path, "plaintext credentials and DSNs are forbidden; use secretRef", ErrPlaintextPassword))
	}
	if strings.TrimSpace(source.SecretRef) == "" {
		problems = append(problems, fieldError(path+".secretRef", "secretRef is required", ErrMissingSecretRef))
	} else if !secretRefPattern.MatchString(source.SecretRef) {
		problems = append(problems, fieldError(path+".secretRef", "secretRef must name an environment variable", ErrInvalidSecretRef))
	}
	return problems
}

func validateScope(path string, scope Scope) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(scope.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase scope name is required", ErrMissingField))
	}
	if strings.TrimSpace(scope.Description) == "" {
		problems = append(problems, fieldError(path+".description", "description is required", ErrMissingField))
	}
	switch scope.Type {
	case ScopeTypeEnum:
		if len(scope.AllowedValues) == 0 {
			problems = append(problems, fieldError(path+".allowed_values", "enum scope requires allowed values", ErrMissingField))
		}
		if duplicateOrEmpty(scope.AllowedValues) {
			problems = append(problems, fieldError(path+".allowed_values", "values must be non-empty and unique", ErrInvalidCatalog))
		}
		if scope.Min != "" || scope.Max != "" {
			problems = append(problems, fieldError(path, "enum scope cannot define min or max", ErrInvalidCatalog))
		}
	case ScopeTypeDateRange:
		minimum, minErr := time.Parse("2006-01-02", scope.Min)
		maximum, maxErr := time.Parse("2006-01-02", scope.Max)
		if minErr != nil || maxErr != nil {
			problems = append(problems, fieldError(path, "date_range scope requires min and max in YYYY-MM-DD form", ErrInvalidCatalog))
		} else if minimum.After(maximum) {
			problems = append(problems, fieldError(path, "date_range min is after max", ErrInvalidCatalog))
		}
		if len(scope.AllowedValues) != 0 {
			problems = append(problems, fieldError(path+".allowed_values", "date_range scope cannot define allowed values", ErrInvalidCatalog))
		}
	default:
		problems = append(problems, fieldError(path+".type", "type must be enum or date_range", ErrMissingField))
	}
	return problems
}

func validateBudgetProfile(path string, profile BudgetProfile) ValidationErrors {
	var problems ValidationErrors
	if !configNamePattern.MatchString(profile.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase profile name is required", ErrMissingField))
	}
	if err := profile.Budget().Validate(); err != nil {
		problems = append(problems, fieldError(path, err.Error(), ErrInvalidBudgetProfile))
	}
	return problems
}

func validateApprovalRoute(path string, route ApprovalRoute, profiles map[string]BudgetProfile) ValidationErrors {
	var problems ValidationErrors
	if err := route.Sensitivity.Validate(); err != nil {
		problems = append(problems, fieldError(path+".sensitivity", "a supported sensitivity is required", ErrInvalidApprovalRoute))
	}
	if err := route.Mode.Validate(); err != nil {
		problems = append(problems, fieldError(path+".mode", "mode must be auto or manual", ErrInvalidApprovalRoute))
	}
	switch route.Mode {
	case domain.ApprovalModeAuto:
		if strings.TrimSpace(route.Approver) != "" {
			problems = append(problems, fieldError(path+".approver", "auto route cannot name an approver", ErrInvalidApprovalRoute))
		}
	case domain.ApprovalModeManual:
		if strings.TrimSpace(route.Approver) == "" {
			problems = append(problems, fieldError(path+".approver", "manual route requires an approver", ErrInvalidApprovalRoute))
		}
	}
	if strings.TrimSpace(route.BudgetProfile) == "" {
		problems = append(problems, fieldError(path+".budget_profile", "budget profile is required", ErrInvalidApprovalRoute))
	} else if _, exists := profiles[route.BudgetProfile]; !exists {
		problems = append(problems, fieldError(path+".budget_profile", "referenced budget profile does not exist", ErrInvalidApprovalRoute))
	}
	return problems
}

func validateProduct(path string, product Product, sources, scopes map[string]struct{}) ValidationErrors {
	var problems ValidationErrors
	if !identifierPattern.MatchString(product.Name) {
		problems = append(problems, fieldError(path+".name", "a lowercase logical name is required", ErrMissingField))
	}
	if _, exists := sources[product.Source]; !exists {
		problems = append(problems, fieldError(path+".source", "referenced source does not exist", ErrInvalidCatalog))
	}
	if !reportingViewPattern.MatchString(product.ReportingView) {
		problems = append(problems, fieldError(path+".reporting_view", "view must be an unquoted reporting.<name> identifier", ErrInvalidReportingView))
	}
	if strings.TrimSpace(product.Description) == "" {
		problems = append(problems, fieldError(path+".description", "description is required", ErrMissingField))
	}
	if err := product.Sensitivity.Validate(); err != nil {
		problems = append(problems, fieldError(path+".sensitivity", "a supported sensitivity is required", ErrInvalidCatalog))
	}
	if len(product.Fields) == 0 {
		problems = append(problems, fieldError(path+".fields", "at least one field is required", ErrMissingField))
	}
	fieldNames := make(map[string]struct{}, len(product.Fields))
	for index, field := range product.Fields {
		fieldPath := fmt.Sprintf("%s.fields[%d]", path, index)
		if !identifierPattern.MatchString(field.Name) {
			problems = append(problems, fieldError(fieldPath+".name", "a lowercase field name is required", ErrMissingField))
		}
		if _, exists := fieldNames[field.Name]; exists {
			problems = append(problems, fieldError(fieldPath+".name", "field name is duplicated", ErrDuplicateField))
		}
		fieldNames[field.Name] = struct{}{}
		if !typePattern.MatchString(field.Type) {
			problems = append(problems, fieldError(fieldPath+".type", "a safe logical type is required", ErrMissingField))
		}
		if strings.TrimSpace(field.Description) == "" {
			problems = append(problems, fieldError(fieldPath+".description", "description is required", ErrMissingField))
		}
		if field.Sensitivity != "" {
			if err := field.Sensitivity.Validate(); err != nil {
				problems = append(problems, fieldError(fieldPath+".sensitivity", "unsupported sensitivity", ErrInvalidCatalog))
			}
		}
	}
	if len(product.Scopes) == 0 {
		problems = append(problems, fieldError(path+".scopes", "at least one mandatory scope is required", ErrMissingField))
	}
	if duplicateOrEmpty(product.Scopes) {
		problems = append(problems, fieldError(path+".scopes", "scope names must be non-empty and unique", ErrInvalidCatalog))
	}
	for _, scope := range product.Scopes {
		if _, exists := scopes[scope]; !exists {
			problems = append(problems, fieldError(path+".scopes", "referenced scope does not exist", ErrInvalidCatalog))
		}
		if _, published := fieldNames[scope]; !published {
			problems = append(problems, fieldError(path+".scopes", "mandatory scope must name a published product field", ErrInvalidCatalog))
		}
	}
	problems = append(problems, validateFunctions(path+".allowed_functions", product.AllowedFunctions)...)
	problems = append(problems, validateFunctions(path+".allowed_aggregates", product.AllowedAggregates)...)
	for _, operator := range product.AllowedOperators {
		if _, ok := safeOperators[strings.ToLower(strings.TrimSpace(operator))]; !ok {
			problems = append(problems, fieldError(path+".allowed_operators", "operator is not in the safe catalog vocabulary", ErrInvalidCatalog))
		}
	}
	if duplicateOrEmpty(product.AllowedOperators) {
		problems = append(problems, fieldError(path+".allowed_operators", "operators must be non-empty and unique", ErrInvalidCatalog))
	}
	return problems
}

func validateFunctions(path string, functions []string) ValidationErrors {
	var problems ValidationErrors
	for _, function := range functions {
		if !functionPattern.MatchString(function) {
			problems = append(problems, fieldError(path, "function must be an unqualified lowercase identifier", ErrInvalidCatalog))
			continue
		}
		if _, forbidden := forbiddenFunctions[function]; forbidden {
			problems = append(problems, fieldError(path, "dangerous function cannot be published", ErrInvalidCatalog))
		}
	}
	if duplicateOrEmpty(functions) {
		problems = append(problems, fieldError(path, "functions must be non-empty and unique", ErrInvalidCatalog))
	}
	return problems
}

func duplicateOrEmpty(values []string) bool {
	if len(values) == 0 {
		return false
	}
	copyValues := append([]string(nil), values...)
	for index := range copyValues {
		copyValues[index] = strings.TrimSpace(copyValues[index])
		if copyValues[index] == "" {
			return true
		}
	}
	sort.Strings(copyValues)
	for index := 1; index < len(copyValues); index++ {
		if copyValues[index] == copyValues[index-1] {
			return true
		}
	}
	return false
}
