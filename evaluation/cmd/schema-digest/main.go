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
	"taskbound.local/agent-data-gateway/internal/catalogschema"
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
	built, err := catalogschema.Build(logical)
	return built.Source, built.Entries, err
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
