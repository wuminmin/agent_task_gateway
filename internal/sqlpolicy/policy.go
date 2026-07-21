package sqlpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// Engine is immutable and safe for concurrent use.
type Engine struct {
	allowedFunctions map[string]struct{}
	allowedOperators map[string]struct{}
}

// New constructs a PostgreSQL AST policy engine.
func New(config Config) *Engine {
	functions := config.AllowedFunctions
	if functions == nil {
		functions = defaultFunctions
	}
	operators := config.AllowedOperators
	if operators == nil {
		operators = defaultOperators
	}
	return &Engine{
		allowedFunctions: stringSet(functions),
		allowedOperators: stringSet(operators),
	}
}

// Authorize validates SQL exclusively through pg_query_go's PostgreSQL AST.
// It never uses textual pattern matching to decide whether agent SQL is safe.
func (e *Engine) Authorize(request Request) (Decision, error) {
	if request.RowLimit <= 0 {
		return Decision{}, reject(CodeBudgetExhausted)
	}
	products, approvedColumns, err := validateGrant(request.Grant)
	if err != nil {
		return Decision{}, err
	}

	astJSON, parseErr := pg_query.ParseToJSON(request.SQL)
	if parseErr != nil {
		return Decision{}, reject(CodeInvalidSQL)
	}
	var document any
	decoder := json.NewDecoder(strings.NewReader(astJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return Decision{}, reject(CodeInvalidSQL)
	}

	root, ok := document.(map[string]any)
	if !ok {
		return Decision{}, reject(CodeInvalidSQL)
	}
	statements, ok := root["stmts"].([]any)
	if !ok || len(statements) == 0 {
		return Decision{}, reject(CodeInvalidSQL)
	}
	if len(statements) != 1 {
		return Decision{}, reject(CodeMultipleStatements)
	}
	statement, ok := statements[0].(map[string]any)
	if !ok {
		return Decision{}, reject(CodeInvalidSQL)
	}
	stmt, ok := statement["stmt"].(map[string]any)
	if !ok || len(stmt) != 1 {
		return Decision{}, reject(CodeInvalidSQL)
	}
	selectBody, ok := stmt["SelectStmt"].(map[string]any)
	if !ok {
		return Decision{}, reject(topLevelCode(stmt))
	}

	analyzer := newAnalyzer(products, approvedColumns, e.allowedFunctions, e.allowedOperators)
	analyzer.discover(selectBody)
	if err := analyzer.validateNode("SelectStmt", selectBody); err != nil {
		return Decision{}, err
	}

	parsed, err := pg_query.Parse(request.SQL)
	if err != nil || len(parsed.GetStmts()) != 1 {
		return Decision{}, reject(CodeInvalidSQL)
	}
	canonical, err := pg_query.Deparse(parsed)
	if err != nil {
		return Decision{}, reject(CodeInvalidSQL)
	}
	canonical = strings.TrimSpace(canonical)
	canonical = strings.TrimSuffix(canonical, ";")
	if canonical == "" {
		return Decision{}, reject(CodeInvalidSQL)
	}

	referencedProducts := sortedKeys(analyzer.referencedProducts)
	referencedColumns := sortedKeys(analyzer.referencedColumns)
	executable, err := renderExecutable(canonical, referencedProducts, products, request.RowLimit)
	if err != nil {
		return Decision{}, err
	}
	digest := sha256.Sum256([]byte(canonical))
	return Decision{
		SQL:                executable,
		CanonicalAgentSQL:  canonical,
		Fingerprint:        hex.EncodeToString(digest[:]),
		ReferencedProducts: referencedProducts,
		ReferencedColumns:  referencedColumns,
		RowLimit:           request.RowLimit,
	}, nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func topLevelCode(stmt map[string]any) Code {
	for node := range stmt {
		if _, found := writeNodes[node]; found {
			return CodeWriteForbidden
		}
	}
	return CodeNotSelect
}

func validateGrant(grant Grant) (map[string]ProductGrant, map[string]struct{}, error) {
	products := make(map[string]ProductGrant, len(grant.Products))
	columns := make(map[string]struct{})
	for _, product := range grant.Products {
		if !safeCatalogIdentifier(product.LogicalName) || product.PhysicalSchema == "" || product.PhysicalView == "" || len(product.ApprovedColumns) == 0 {
			return nil, nil, reject(CodeInvalidGrant)
		}
		if _, duplicate := products[product.LogicalName]; duplicate {
			return nil, nil, reject(CodeInvalidGrant)
		}
		seenColumns := make(map[string]struct{}, len(product.ApprovedColumns))
		for _, column := range product.ApprovedColumns {
			if !safeCatalogIdentifier(column) {
				return nil, nil, reject(CodeInvalidGrant)
			}
			if _, duplicate := seenColumns[column]; duplicate {
				return nil, nil, reject(CodeInvalidGrant)
			}
			seenColumns[column] = struct{}{}
			columns[column] = struct{}{}
		}
		for _, predicate := range product.MandatoryScope {
			if !safeCatalogIdentifier(predicate.Column) || !validPredicateShape(predicate) {
				return nil, nil, reject(CodeInvalidGrant)
			}
			for _, value := range predicate.Values {
				if strings.IndexByte(value, 0) >= 0 {
					return nil, nil, reject(CodeInvalidGrant)
				}
			}
		}
		if strings.IndexByte(product.PhysicalSchema, 0) >= 0 || strings.IndexByte(product.PhysicalView, 0) >= 0 {
			return nil, nil, reject(CodeInvalidGrant)
		}
		products[product.LogicalName] = product
	}
	return products, columns, nil
}

func safeCatalogIdentifier(value string) bool {
	if value == "" || strings.HasPrefix(value, "__taskbound_") {
		return false
	}
	for index, r := range value {
		if r == '_' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

var defaultFunctions = []string{
	"abs", "avg", "btrim", "ceil", "ceiling", "char_length", "concat",
	"concat_ws", "count", "date_part", "date_trunc", "floor", "jsonb_array_length",
	"jsonb_extract_path", "jsonb_extract_path_text", "length", "lower", "ltrim",
	"max", "min", "replace", "round", "rtrim", "substring", "sum", "to_char",
	"trim", "upper",
}

var defaultOperators = []string{
	"=", "<>", "!=", "<", "<=", ">", ">=", "+", "-", "*", "/", "%",
	"||", "~~", "!~~", "~~*", "!~~*", "->", "->>", "#>", "#>>", "@>", "<@",
}
