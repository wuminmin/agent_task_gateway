package main

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func TestExpectedSchemasBuildsSingleSourceContract(t *testing.T) {
	logical := &catalog.Catalog{
		Sources: []catalog.Source{{Name: "approved", User: "gateway_reader"}},
		Products: []catalog.Product{
			{Name: "expense", Source: "approved", ReportingView: "reporting.expense", Fields: []catalog.Field{{Name: "id", Type: "bigint"}}},
			{Name: "summary", Source: "approved", ReportingView: "reporting.summary", Fields: []catalog.Field{{Name: "month", Type: "date"}}},
		},
	}
	source, schemas, err := expectedSchemas(logical)
	if err != nil {
		t.Fatalf("build expected schemas: %v", err)
	}
	if source.Name != "approved" || source.User != "gateway_reader" {
		t.Fatalf("unexpected source: %+v", source)
	}
	if len(schemas) != 2 {
		t.Fatalf("schema count = %d, want 2", len(schemas))
	}
	if schemas[0].Schema != "reporting" || schemas[0].View != "expense" {
		t.Fatalf("unexpected first view: %+v", schemas[0])
	}
}

func TestExpectedSchemasRejectsMultipleSources(t *testing.T) {
	logical := &catalog.Catalog{
		Sources: []catalog.Source{{Name: "a"}, {Name: "b"}},
		Products: []catalog.Product{
			{Name: "one", Source: "a", ReportingView: "reporting.one"},
			{Name: "two", Source: "b", ReportingView: "reporting.two"},
		},
	}
	if _, _, err := expectedSchemas(logical); err == nil {
		t.Fatal("expected multiple-source catalog to be rejected")
	}
}
