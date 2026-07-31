package gateway

import (
	"context"
	"encoding/json"
	"strings"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

const catalogReportingSQLProfile = sqllowering.Profile

func (s *Service) describeDataProduct(_ context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	args.Name = strings.TrimSpace(args.Name)
	if args.Name == "" {
		return nil, &mcp.ToolError{Code: apierr.CodeInvalidRequest, Message: "name 必须是非空逻辑数据产品名"}
	}
	product, found := s.catalog.LookupProduct(args.Name)
	if !found {
		return nil, &mcp.ToolError{Code: apierr.CodeNotFound, Message: "请求的数据产品不存在"}
	}
	result, err := publicDataProduct(product, true)
	if err != nil {
		return nil, err
	}
	result["catalog_version"] = s.catalog.CatalogVersion
	return result, nil
}

func (s *Service) getSQLCapabilities(_ context.Context, _ mcp.Principal, raw json.RawMessage) (any, error) {
	var args struct{}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	return map[string]any{
		"catalog_version":            s.catalog.CatalogVersion,
		"sql_profile":                catalogReportingSQLProfile,
		"statement_types":            []string{"SELECT"},
		"single_statement_only":      true,
		"logical_products_only":      true,
		"lossless_lowering_required": true,
		"join": map[string]any{
			"types":              []string{"INNER"},
			"predicate":          "equality",
			"graph":              "connected",
			"min_sources":        2,
			"max_sources":        queryplan.MaxJoinSources,
			"limit_kind":         "operational_complexity_guard",
			"arbitrary_topology": true,
			"self_join":          false,
		},
		"pagination": map[string]any{
			"single_product": true,
			"joined_query":   false,
		},
		"catalog_controls": map[string]any{
			"columns":    "per_product",
			"aggregates": "per_product",
			"operators":  "per_product",
			"functions":  "per_product",
		},
		"repairable_error_codes": []string{
			apierr.CodeSQLSyntaxError,
			apierr.CodeProductNotApproved,
			apierr.CodeColumnNotApproved,
			apierr.CodeSQLNotLowerable,
			apierr.CodeJoinTypeUnsupported,
			apierr.CodeJoinGraphDisconnected,
			apierr.CodeJoinKeyTypeMismatch,
			apierr.CodeCollationMismatch,
			apierr.CodeSubqueryUnsupported,
		},
		"features": map[string]any{
			"projection":                            true,
			"filters":                               true,
			"group_by":                              true,
			"aggregates":                            true,
			"order_by":                              true,
			"limit":                                 true,
			"offset":                                true,
			"inner_equijoins":                       true,
			"multi_relation_join_graphs":            true,
			"multiple_equality_predicates_per_edge": true,
			"multiple_statements":                   false,
			"data_modification":                     false,
			"outer_joins":                           false,
			"cross_joins":                           false,
			"non_equality_joins":                    false,
			"disconnected_join_graphs":              false,
			"subqueries":                            false,
			"common_table_expressions":              false,
			"set_operations":                        false,
			"window_functions":                      false,
			"having":                                false,
			"select_distinct":                       false,
			"implicit_star_projection":              false,
			"positional_order_group_refs":           false,
		},
	}, nil
}

func publicDataProduct(product catalog.Product, detailed bool) (map[string]any, error) {
	sensitivity, err := product.EffectiveSensitivity()
	if err != nil {
		return nil, err
	}
	fields := make([]map[string]any, 0, len(product.Fields))
	for _, field := range product.Fields {
		fieldSensitivity := field.Sensitivity
		if fieldSensitivity == "" {
			fieldSensitivity = product.Sensitivity
		}
		publicField := map[string]any{
			"name": field.Name, "type": field.Type, "description": field.Description,
			"sensitivity": fieldSensitivity,
		}
		if field.Collation != "" {
			publicField["collation"] = field.Collation
			publicField["collation_version"] = field.CollationVersion
		}
		fields = append(fields, publicField)
	}
	result := map[string]any{
		"name":                 product.Name,
		"description":          product.Description,
		"sensitivity":          sensitivity,
		"fields":               fields,
		"scopes":               append([]string{}, product.Scopes...),
		"snapshot":             product.Snapshot,
		"entity_key":           append([]string{}, product.EntityKey...),
		"stable_relation_role": product.StableRelationRole,
		"allowed_aggregates":   append([]string{}, product.AllowedAggregates...),
		"sql_profile":          catalogReportingSQLProfile,
	}
	if detailed {
		result["allowed_operators"] = append([]string{}, product.AllowedOperators...)
		result["allowed_functions"] = append([]string{}, product.AllowedFunctions...)
	}
	return result, nil
}
