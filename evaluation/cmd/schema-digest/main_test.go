package main

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

func TestExpectedSchemasLoadsWorkflowStudyContract(t *testing.T) {
	logical, err := catalog.Load("../../workflow-study/catalog.yaml")
	if err != nil {
		t.Fatalf("load workflow-study catalog: %v", err)
	}
	source, schemas, err := expectedSchemas(logical)
	if err != nil {
		t.Fatalf("build expected schemas: %v", err)
	}
	if source.Name != "workflow_study" || source.User != "gateway_reader" {
		t.Fatalf("unexpected source: %+v", source)
	}
	if len(schemas) != 6 {
		t.Fatalf("schema count = %d, want 6", len(schemas))
	}
	if schemas[0].Schema != "reporting" || schemas[0].View != "wf_expense_claim" {
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
