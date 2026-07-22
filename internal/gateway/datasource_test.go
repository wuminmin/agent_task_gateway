package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/apierr"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/mcp"
)

func TestCatalogSourceForProductsRejectsMultiSourceTasks(t *testing.T) {
	service := &Service{catalog: &catalog.Catalog{
		Sources: []catalog.Source{
			{Name: "source_a", DatasourceID: "source-a"},
			{Name: "source_b", DatasourceID: "source-b"},
		},
		Products: []catalog.Product{
			{Name: "product_a", Source: "source_a"},
			{Name: "product_b", Source: "source_b"},
		},
	}}
	_, err := service.catalogSourceForProducts([]string{"product_a", "product_b"})
	var toolErr *mcp.ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != apierr.CodePolicyDenied {
		t.Fatalf("catalogSourceForProducts() = %T %v, want policy denial", err, err)
	}
}

func TestDatasourceEvidenceRejectsSchemaDigestMismatch(t *testing.T) {
	service := &Service{
		catalog: &catalog.Catalog{
			Sources:  []catalog.Source{{Name: "source_a", DatasourceID: "source-a", Database: "db", User: "reader", PostgreSQLMajorVersion: 16, SchemaDigest: strings.Repeat("a", 64)}},
			Products: []catalog.Product{{Name: "product_a", Source: "source_a"}},
		},
		connector: &fakeConnector{attestation: testAttestation("source-a", "db", "reader", 16, strings.Repeat("b", 64))},
	}
	_, err := service.datasourceEvidence(context.Background(), []string{"product_a"})
	if err == nil {
		t.Fatal("schema digest mismatch was accepted")
	}
}

func testAttestation(source, database, user string, version int, schemaDigest string) dataconnector.Attestation {
	return dataconnector.Attestation{
		DatasourceID: source, Database: database, User: user,
		PostgreSQLMajorVersion: version, SchemaDigest: schemaDigest,
	}
}
