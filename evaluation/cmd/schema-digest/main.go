// Command schema-digest computes the live TaskGate reporting-view attestation
// for a validated single-source catalog without trusting its recorded digest.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

func main() {
	var catalogPath, dsn string
	flag.StringVar(&catalogPath, "catalog", "", "validated TaskGate catalog path")
	flag.StringVar(&dsn, "dsn", "", "PostgreSQL DSN for the catalog reader")
	flag.Parse()
	if strings.TrimSpace(catalogPath) == "" || strings.TrimSpace(dsn) == "" {
		fatalf("-catalog and -dsn are required")
	}
	logical, err := catalog.Load(catalogPath)
	if err != nil {
		fatalf("load catalog: %v", err)
	}
	source, schemas, err := expectedSchemas(logical)
	if err != nil {
		fatalf("catalog shape: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 10 * time.Second, ConnectTimeout: 10 * time.Second,
		MaxRows: 1, MaxConnections: 1, ExpectedSchema: schemas,
		ExpectedAttestation: dataconnector.ExpectedAttestation{
			DatasourceID: source.DatasourceID, Database: source.Database, User: source.User,
			PostgreSQLMajorVersion: source.PostgreSQLMajorVersion,
		},
	})
	if err != nil {
		fatalf("attest datasource: %v", err)
	}
	defer connector.Close()
	attestation, err := connector.Attestation(ctx)
	if err != nil {
		fatalf("read attestation: %v", err)
	}
	fmt.Println(attestation.SchemaDigest)
}

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

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
