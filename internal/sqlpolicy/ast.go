package sqlpolicy

import (
	"sort"
	"strings"
)

type analyzer struct {
	products           map[string]ProductGrant
	approvedColumns    map[string]struct{}
	derivedColumns     map[string]struct{}
	relations          map[string]struct{}
	ctes               map[string]struct{}
	allowedFunctions   map[string]struct{}
	allowedOperators   map[string]struct{}
	referencedProducts map[string]struct{}
	referencedColumns  map[string]struct{}
}

func newAnalyzer(products map[string]ProductGrant, columns, functions, operators map[string]struct{}) *analyzer {
	relations := make(map[string]struct{}, len(products))
	for product := range products {
		relations[product] = struct{}{}
	}
	return &analyzer{
		products:           products,
		approvedColumns:    columns,
		derivedColumns:     make(map[string]struct{}),
		relations:          relations,
		ctes:               make(map[string]struct{}),
		allowedFunctions:   functions,
		allowedOperators:   operators,
		referencedProducts: make(map[string]struct{}),
		referencedColumns:  make(map[string]struct{}),
	}
}

// discover makes later validation independent of Go map iteration order. It
// records query-local relation aliases and derived output names, never grants.
func (a *analyzer) discover(value any) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			a.discover(item)
		}
	case map[string]any:
		if raw, ok := typed["CommonTableExpr"].(map[string]any); ok {
			if name, ok := raw["ctename"].(string); ok && name != "" {
				a.ctes[name] = struct{}{}
				a.relations[name] = struct{}{}
			}
			for _, name := range nodeStringList(raw["aliascolnames"]) {
				a.derivedColumns[name] = struct{}{}
			}
		}
		if raw, ok := typed["ResTarget"].(map[string]any); ok {
			if name, ok := raw["name"].(string); ok && name != "" {
				a.derivedColumns[name] = struct{}{}
			}
		}
		if alias, ok := typed["alias"].(map[string]any); ok {
			if name, ok := alias["aliasname"].(string); ok && name != "" {
				a.relations[name] = struct{}{}
			}
			for _, name := range nodeStringList(alias["colnames"]) {
				a.derivedColumns[name] = struct{}{}
			}
		}
		for _, child := range typed {
			a.discover(child)
		}
	}
}

func (a *analyzer) validateNode(nodeName string, body map[string]any) error {
	if code, denied := deniedNodes[nodeName]; denied {
		return reject(code)
	}
	if _, allowed := allowedNodes[nodeName]; !allowed {
		return reject(CodeFeatureNotAllowed)
	}

	switch nodeName {
	case "SelectStmt":
		if _, present := body["intoClause"]; present {
			return reject(CodeSelectInto)
		}
		if clauses, present := body["lockingClause"].([]any); present && len(clauses) != 0 {
			return reject(CodeLocking)
		}
		if values, present := body["valuesLists"].([]any); present && len(values) != 0 {
			return reject(CodeFeatureNotAllowed)
		}
		if with, present := body["withClause"].(map[string]any); present {
			if recursive, _ := with["recursive"].(bool); recursive {
				return reject(CodeRecursiveCTE)
			}
		}
	case "CommonTableExpr":
		if recursive, _ := body["cterecursive"].(bool); recursive {
			return reject(CodeRecursiveCTE)
		}
	case "RangeVar":
		if err := a.validateRangeVar(body); err != nil {
			return err
		}
	case "ColumnRef":
		if err := a.validateColumnRef(body); err != nil {
			return err
		}
	case "FuncCall":
		if err := a.validateFunction(body); err != nil {
			return err
		}
	case "A_Expr":
		if err := a.validateOperator(body); err != nil {
			return err
		}
	case "TypeCast":
		if err := validateTypeCast(body); err != nil {
			return err
		}
	}

	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := a.validateValue(body[key]); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) validateValue(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			if err := a.validateValue(child); err != nil {
				return err
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := typed[key]
			if isNodeWrapper(key, child) {
				body, ok := child.(map[string]any)
				if !ok {
					return reject(CodeInvalidSQL)
				}
				if err := a.validateNode(key, body); err != nil {
					return err
				}
				continue
			}
			if key == "recursive" {
				if recursive, _ := child.(bool); recursive {
					return reject(CodeRecursiveCTE)
				}
			}
			if err := a.validateValue(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *analyzer) validateRangeVar(body map[string]any) error {
	if schema, _ := body["schemaname"].(string); schema != "" {
		return reject(CodeSystemObject)
	}
	if catalog, _ := body["catalogname"].(string); catalog != "" {
		return reject(CodeSystemObject)
	}
	relation, _ := body["relname"].(string)
	if relation == "" {
		return reject(CodeObjectNotAllowed)
	}
	if systemRelation(relation) {
		return reject(CodeSystemObject)
	}
	if _, allowed := a.products[relation]; allowed {
		a.referencedProducts[relation] = struct{}{}
		return nil
	}
	if _, allowed := a.ctes[relation]; allowed {
		return nil
	}
	return reject(CodeObjectNotAllowed)
}

func (a *analyzer) validateColumnRef(body map[string]any) error {
	parts, wildcard, valid := nodeIdentifierPath(body["fields"])
	if wildcard {
		return reject(CodeWildcard)
	}
	if !valid || len(parts) == 0 || len(parts) > 2 {
		return reject(CodeColumnNotAllowed)
	}
	column := parts[len(parts)-1]
	if len(parts) == 2 {
		if _, allowed := a.relations[parts[0]]; !allowed {
			return reject(CodeObjectNotAllowed)
		}
	}
	if _, allowed := a.approvedColumns[column]; !allowed {
		if _, derived := a.derivedColumns[column]; !derived {
			return reject(CodeColumnNotAllowed)
		}
	}
	a.referencedColumns[column] = struct{}{}
	return nil
}

func (a *analyzer) validateFunction(body map[string]any) error {
	nameParts := nodeStringList(body["funcname"])
	if len(nameParts) != 1 {
		return reject(CodeFunctionNotAllowed)
	}
	name := nameParts[0]
	if dangerousFunction(name) {
		return reject(CodeFunctionNotAllowed)
	}
	if _, allowed := a.allowedFunctions[name]; !allowed {
		return reject(CodeFunctionNotAllowed)
	}
	return nil
}

func (a *analyzer) validateOperator(body map[string]any) error {
	operators := nodeStringList(body["name"])
	if len(operators) > 1 {
		return reject(CodeOperatorNotAllowed)
	}
	if len(operators) == 1 {
		if _, allowed := a.allowedOperators[operators[0]]; !allowed {
			return reject(CodeOperatorNotAllowed)
		}
	}
	return nil
}

func validateTypeCast(body map[string]any) error {
	typeName, ok := body["typeName"].(map[string]any)
	if !ok {
		return reject(CodeFeatureNotAllowed)
	}
	if bounds, ok := typeName["arrayBounds"].([]any); ok && len(bounds) != 0 {
		return reject(CodeFeatureNotAllowed)
	}
	names := nodeStringList(typeName["names"])
	if len(names) == 2 && names[0] == "pg_catalog" {
		names = names[1:]
	}
	if len(names) != 1 {
		return reject(CodeFeatureNotAllowed)
	}
	if _, safe := safeTypes[names[0]]; !safe {
		return reject(CodeFeatureNotAllowed)
	}
	return nil
}

func nodeIdentifierPath(value any) (parts []string, wildcard bool, valid bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false, false
	}
	parts = make([]string, 0, len(items))
	for _, item := range items {
		node, ok := item.(map[string]any)
		if !ok || len(node) != 1 {
			return nil, false, false
		}
		if raw, found := node["A_Star"]; found {
			if _, ok := raw.(map[string]any); !ok {
				return nil, false, false
			}
			wildcard = true
			continue
		}
		stringNode, found := node["String"]
		if !found {
			return nil, false, false
		}
		body, ok := stringNode.(map[string]any)
		if !ok {
			return nil, false, false
		}
		value, ok := body["sval"].(string)
		if !ok || value == "" {
			return nil, false, false
		}
		parts = append(parts, value)
	}
	return parts, wildcard, true
}

func nodeStringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		node, ok := item.(map[string]any)
		if !ok {
			return nil
		}
		stringNode, ok := node["String"].(map[string]any)
		if !ok {
			return nil
		}
		value, ok := stringNode["sval"].(string)
		if !ok || value == "" {
			return nil
		}
		result = append(result, value)
	}
	return result
}

func isNodeWrapper(key string, value any) bool {
	if key == "" || key[0] < 'A' || key[0] > 'Z' {
		return false
	}
	_, ok := value.(map[string]any)
	return ok
}

func systemRelation(name string) bool {
	return name == "information_schema" || name == "pg_catalog" || strings.HasPrefix(name, "pg_")
}

func dangerousFunction(name string) bool {
	if _, found := dangerousFunctions[name]; found {
		return true
	}
	for _, prefix := range dangerousFunctionPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var writeNodes = map[string]struct{}{
	"AlterTableStmt": {}, "CallStmt": {}, "CopyStmt": {}, "CreateStmt": {}, "DeleteStmt": {},
	"DoStmt": {}, "DropStmt": {}, "InsertStmt": {}, "MergeStmt": {}, "PlassignStmt": {},
	"RefreshMatViewStmt": {}, "TruncateStmt": {}, "UpdateStmt": {}, "VariableSetStmt": {},
}

var deniedNodes = map[string]Code{
	"A_Star": CodeWildcard, "Param": CodeParameter, "ParamRef": CodeParameter,
	"IntoClause": CodeSelectInto, "LockingClause": CodeLocking,
	"RangeFunction": CodeFeatureNotAllowed, "RangeTableFunc": CodeFeatureNotAllowed,
	"RangeTableSample": CodeFeatureNotAllowed, "SQLValueFunction": CodeParameter,
	"TableFunc": CodeFeatureNotAllowed, "JsonTable": CodeFeatureNotAllowed,
	"AlterTableStmt": CodeWriteForbidden, "CallStmt": CodeWriteForbidden,
	"CopyStmt": CodeWriteForbidden, "CreateStmt": CodeWriteForbidden,
	"DeleteStmt": CodeWriteForbidden, "DoStmt": CodeWriteForbidden,
	"DropStmt": CodeWriteForbidden, "InsertStmt": CodeWriteForbidden,
	"MergeStmt": CodeWriteForbidden, "PlassignStmt": CodeWriteForbidden,
	"RefreshMatViewStmt": CodeWriteForbidden, "TruncateStmt": CodeWriteForbidden,
	"UpdateStmt": CodeWriteForbidden, "VariableSetStmt": CodeWriteForbidden,
}

var allowedNodes = map[string]struct{}{
	"A_ArrayExpr": {}, "A_Const": {}, "A_Expr": {}, "A_Indices": {}, "A_Indirection": {},
	"BitString": {}, "Boolean": {}, "BooleanTest": {}, "BoolExpr": {},
	"CaseExpr": {}, "CaseWhen": {}, "CoalesceExpr": {}, "ColumnRef": {},
	"CommonTableExpr": {}, "Float": {}, "FuncCall": {}, "GroupingFunc": {},
	"GroupingSet": {}, "Integer": {}, "JoinExpr": {}, "List": {}, "MinMaxExpr": {},
	"NamedArgExpr": {}, "NullTest": {}, "RangeSubselect": {}, "RangeVar": {},
	"ResTarget": {}, "RowExpr": {}, "SelectStmt": {}, "SortBy": {}, "String": {},
	"SubLink": {}, "TypeCast": {}, "WindowDef": {},
}

var dangerousFunctions = map[string]struct{}{
	"current_setting": {}, "set_config": {}, "nextval": {}, "setval": {}, "currval": {},
	"pg_advisory_lock": {}, "pg_advisory_lock_shared": {}, "pg_advisory_unlock": {},
	"pg_advisory_unlock_all": {}, "pg_advisory_unlock_shared": {}, "pg_cancel_backend": {},
	"pg_notify": {}, "pg_reload_conf": {}, "pg_rotate_logfile": {}, "pg_sleep": {},
	"pg_sleep_for": {}, "pg_sleep_until": {}, "pg_terminate_backend": {},
	"lo_export": {}, "lo_import": {}, "dblink": {}, "dblink_exec": {},
	// ts_stat executes the SQL text supplied by its caller. Letting a catalog
	// enable it would create a second SQL parser/execution path that bypasses
	// logical-product, column, and mandatory-scope validation.
	"ts_stat": {},
}

var dangerousFunctionPrefixes = []string{
	// PostgreSQL's XML mapping helpers either execute caller-supplied SQL or
	// export a table, schema, database, or cursor. They must remain denied even
	// when a trusted catalog accidentally includes them in an allowlist.
	"cursor_to_xml", "database_to_xml", "query_to_xml", "schema_to_xml", "table_to_xml",
	"dblink_", "lo_", "pg_backup_", "pg_create_restore_point", "pg_file_", "pg_log_",
	"pg_ls_", "pg_promote", "pg_read_", "pg_replication_", "pg_stat_file", "pg_switch_wal",
	"pg_wal_replay_",
}

var safeTypes = map[string]struct{}{
	"bool": {}, "boolean": {}, "int2": {}, "smallint": {}, "int4": {}, "int": {},
	"integer": {}, "int8": {}, "bigint": {}, "float4": {}, "real": {}, "float8": {},
	"numeric": {}, "decimal": {}, "text": {}, "varchar": {}, "bpchar": {}, "date": {},
	"time": {}, "timetz": {}, "timestamp": {}, "timestamptz": {}, "interval": {}, "uuid": {},
	"json": {}, "jsonb": {},
}
