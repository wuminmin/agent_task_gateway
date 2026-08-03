package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

// liveSchemaAttestation recomputes one profile's reporting-schema attestation
// against the running Business PostgreSQL, using the same derivation the
// reviewed registry was produced with rather than a second canonicalization.
func liveSchemaAttestation(ctx context.Context, root string, profile finalv5profile.Profile,
	dsn string) (string, []string, error) {
	loaded, err := catalog.Load(filepath.Join(root, profile.CatalogPath))
	if err != nil {
		return "", nil, err
	}
	sources := map[string]catalog.Source{}
	for _, source := range loaded.Sources {
		sources[source.Name] = source
	}
	var selected catalog.Source
	schemas := make([]dataconnector.ViewSchema, 0, len(loaded.Products))
	views := make([]string, 0, len(loaded.Products))
	for _, product := range loaded.Products {
		source, ok := sources[product.Source]
		if !ok {
			return "", nil, fmt.Errorf("product %s has no source", product.Name)
		}
		if selected.Name == "" {
			selected = source
		} else if selected.Name != source.Name {
			return "", nil, fmt.Errorf("multiple sources are not supported")
		}
		if product.ViewContract != nil {
			continue
		}
		schema, view, ok := strings.Cut(product.ReportingView, ".")
		if !ok || schema == "" || view == "" {
			return "", nil, fmt.Errorf("invalid reporting view %q", product.ReportingView)
		}
		columns := make([]dataconnector.SchemaColumn, 0, len(product.Fields))
		for _, field := range product.Fields {
			columns = append(columns, dataconnector.SchemaColumn{Name: field.Name,
				PostgreSQLType: field.Type, Collation: field.Collation,
				CollationVersion: field.CollationVersion, CollationDeterministic: field.Collation != ""})
		}
		schemas = append(schemas, dataconnector.ViewSchema{Schema: schema, View: view, Columns: columns})
		views = append(views, product.ReportingView)
	}
	if len(schemas) == 0 {
		return "", nil, fmt.Errorf("profile Catalog declares no attestable reporting view")
	}
	sort.Strings(views)
	attestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	connector, err := dataconnector.New(attestCtx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 15 * time.Second,
		MaxRows: 1, MaxConnections: 1, ExpectedSchema: schemas,
		ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: selected.DatasourceID,
			Database: selected.Database, User: selected.User,
			PostgreSQLMajorVersion: selected.PostgreSQLMajorVersion},
	})
	if err != nil {
		return "", nil, err
	}
	defer connector.Close()
	attestation, err := connector.Attestation(attestCtx)
	if err != nil {
		return "", nil, err
	}
	return attestation.SchemaDigest, views, nil
}
