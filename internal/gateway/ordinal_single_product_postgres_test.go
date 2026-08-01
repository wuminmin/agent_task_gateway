//go:build taskgate_scale

package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// TestOrdinalSingleProductOnlinePathAgainstPostgreSQL covers the V4 scan and
// paged-scan companion path against a published sidecar and its exact HOT
// dictionary. The full acceptance Compose stack supplies these opt-in inputs;
// ordinary unit runs do not require the scale fixture.
func TestOrdinalSingleProductOnlinePathAgainstPostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("V4_SCALE_BUSINESS_TEST_POSTGRES_DSN"))
	artifactDirectory := strings.TrimSpace(os.Getenv("V4_SCALE_SNAPSHOT_ARTIFACT_DIR"))
	catalogPath := strings.TrimSpace(os.Getenv("V4_SCALE_CATALOG_PATH"))
	if dsn == "" || artifactDirectory == "" || catalogPath == "" {
		t.Fatal("V4 scale database, snapshot artifact directory, and Catalog are required")
	}

	logicalCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		t.Fatalf("load V4 scale Catalog: %v", err)
	}
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("new snapshot registry: %v", err)
	}
	for _, publication := range logicalCatalog.SnapshotPublications {
		hotPath := filepath.Join(artifactDirectory, publication.Name, publication.Name+".hot.tgord")
		encoded, readErr := os.ReadFile(hotPath)
		if readErr != nil {
			t.Fatalf("read %s: %v", publication.Name, readErr)
		}
		hot, parseErr := ordinal.ParseHotDictionary(encoded, publication.ManifestDigest)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", publication.Name, parseErr)
		}
		if registerErr := registry.RegisterPublication(ordinal.PublicationKey{
			CatalogDigest: logicalCatalog.SHA256, PublicationName: publication.Name,
		}, publication.ManifestDigest, hot); registerErr != nil {
			t.Fatalf("register %s: %v", publication.Name, registerErr)
		}
	}

	service := &Service{catalog: logicalCatalog, snapshotRegistry: registry}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second,
		MaxRows: 100, MaxConnections: 1,
	})
	if err != nil {
		t.Fatalf("connect to V4 scale publication: %v", err)
	}
	defer connector.Close()

	tests := []struct {
		name string
		plan queryplan.QueryPlan
		rows int64
	}{
		{name: "scan", plan: queryplan.QueryPlan{
			Product: "scale_orders", Columns: []string{"o_orderkey", "o_orderstatus"},
			Filters: []queryplan.Filter{{Column: "o_orderkey", Op: "=", Value: float64(1)}},
			OrderBy: []queryplan.Order{{Column: "o_orderkey", Direction: "asc"}},
		}, rows: 1},
		{name: "page", plan: queryplan.QueryPlan{
			Product: "scale_lineitem", Columns: []string{"l_orderkey", "l_linenumber", "l_extendedprice"},
			Filters: []queryplan.Filter{{Column: "l_orderkey", Op: "<=", Value: float64(20)}},
			OrderBy: []queryplan.Order{{Column: "l_orderkey", Direction: "asc"}, {Column: "l_linenumber", Direction: "asc"}},
			Limit:   20, Offset: 20,
		}, rows: 20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			product, found := logicalCatalog.LookupProduct(test.plan.Product)
			if !found {
				t.Fatalf("Catalog omits %s", test.plan.Product)
			}
			approved := stringSetFromSlice(product.FieldNames())
			ordinalProduct, productErr := service.ordinalQueryProduct(product, approved)
			if productErr != nil {
				t.Fatalf("ordinal product: %v", productErr)
			}
			compiled, compileErr := queryplan.CompileOrdinal(test.plan, ordinalProduct)
			if compileErr != nil {
				t.Fatalf("compile ordinal query: %v", compileErr)
			}
			bound, bindErr := service.bindOrdinalSidecars(compiled.ProvenanceSQL, compiled.ProvenanceFields, compiled.OrdinalProgram)
			if bindErr != nil {
				t.Fatalf("bind ordinal sidecar: %v", bindErr)
			}

			visibleSQL := ordinalScalePhysicalSQL(compiled.VisibleSQL, product, nil)
			provenanceSQL := ordinalScalePhysicalSQL(bound.ProvenanceSQL, product, bound.SidecarGrants)
			sink := &ordinalDerivationSink{program: bound.Program, indexes: bound.Indexes, planDigest: strings.Repeat("a", 64)}
			pair, pairErr := connector.QueryPairStream(ctx, dataconnector.QueryPairStreamRequest{
				Visible:        dataconnector.QueryRequest{SQL: visibleSQL, StatementTimeout: 5 * time.Second, MaxRows: 100},
				Provenance:     dataconnector.QueryRequest{SQL: provenanceSQL, StatementTimeout: 5 * time.Second, MaxRows: 100},
				ProvenanceSink: sink,
			})
			if pairErr != nil {
				t.Fatalf("stream V4 pair: %v (internal cause: %v)", pairErr, errors.Unwrap(pairErr))
			}
			if pair.Visible.RowCount != test.rows || pair.Provenance.RowCount != test.rows {
				t.Fatalf("row counts: visible=%d provenance=%d, want %d", pair.Visible.RowCount, pair.Provenance.RowCount, test.rows)
			}
			if _, finishErr := sink.Finish(); finishErr != nil {
				t.Fatalf("finish ordinal derivation: %v", finishErr)
			}
		})
	}
}

func ordinalScalePhysicalSQL(sql string, product catalog.Product, sidecars []sqlpolicy.ProductGrant) string {
	schema, view, ok := strings.Cut(product.ReportingView, ".")
	if !ok {
		return sql
	}
	result := strings.ReplaceAll(sql, `"`+product.Name+`"`, `"`+schema+`"."`+view+`"`)
	for _, sidecar := range sidecars {
		result = strings.ReplaceAll(result, `"`+sidecar.LogicalName+`"`,
			`"`+sidecar.PhysicalSchema+`"."`+sidecar.PhysicalView+`"`)
	}
	return result
}
