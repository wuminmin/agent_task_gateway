package main

import (
	"fmt"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

// expectedSchemas mirrors evaluation/cmd/schema-digest so both derive the
// reporting surface from a Catalog the same way.
func expectedSchemas(logical *catalog.Catalog) (catalog.Source, []dataconnector.ViewSchema, error) {
	sources := make(map[string]catalog.Source, len(logical.Sources))
	for _, source := range logical.Sources {
		sources[source.Name] = source
	}
	var selected catalog.Source
	result := make([]dataconnector.ViewSchema, 0, len(logical.Products))
	for _, product := range logical.Products {
		source, ok := sources[product.Source]
		if !ok {
			return catalog.Source{}, nil, fmt.Errorf("product %s has no source", product.Name)
		}
		if selected.Name == "" {
			selected = source
		} else if selected.Name != source.Name {
			return catalog.Source{}, nil, fmt.Errorf("multiple sources are not supported")
		}
		if product.ViewContract != nil {
			continue
		}
		schema, view, ok := strings.Cut(product.ReportingView, ".")
		if !ok || schema == "" || view == "" {
			return catalog.Source{}, nil, fmt.Errorf("invalid reporting view %q", product.ReportingView)
		}
		columns := make([]dataconnector.SchemaColumn, 0, len(product.Fields))
		for _, field := range product.Fields {
			columns = append(columns, dataconnector.SchemaColumn{
				Name: field.Name, PostgreSQLType: field.Type, Collation: field.Collation,
				CollationVersion: field.CollationVersion, CollationDeterministic: field.Collation != "",
			})
		}
		result = append(result, dataconnector.ViewSchema{Schema: schema, View: view, Columns: columns})
	}
	if selected.Name == "" || len(result) == 0 {
		return catalog.Source{}, nil, fmt.Errorf("catalog has no products")
	}
	return selected, result, nil
}
