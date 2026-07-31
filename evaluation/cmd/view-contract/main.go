// Command view-contract discovers and compiles candidate TaskGate semantic
// Views. It never edits the Catalog: operators review the JSON and copy the
// four generated digests into a new, versioned Catalog artifact.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

type contractOutput struct {
	Version                string                          `json:"version"`
	RegistryRevisionDigest string                          `json:"registry_revision_digest"`
	Contracts              map[string]catalog.ViewContract `json:"contracts"`
	Dependencies           map[string][]string             `json:"dependencies"`
}

func main() {
	var catalogPath, dsn, productList string
	flag.StringVar(&catalogPath, "catalog", "", "validated candidate TaskGate catalog path")
	flag.StringVar(&dsn, "dsn", "", "PostgreSQL DSN for the read-only catalog reader")
	flag.StringVar(&productList, "products", "", "comma-separated semantic products; empty uses products already carrying view_contract")
	flag.Parse()
	if strings.TrimSpace(catalogPath) == "" || strings.TrimSpace(dsn) == "" {
		fatalf("-catalog and -dsn are required")
	}
	var logical *catalog.Catalog
	var err error
	if strings.TrimSpace(productList) == "" {
		logical, err = catalog.Load(catalogPath)
	} else {
		candidateNames, parseErr := parseSelectedProductNames(productList)
		if parseErr != nil {
			fatalf("select products: %v", parseErr)
		}
		logical, err = catalog.LoadViewContractCandidates(catalogPath, candidateNames)
	}
	if err != nil {
		fatalf("load catalog: %v", err)
	}
	selected, source, err := selectSemanticProducts(logical, productList)
	if err != nil {
		fatalf("select products: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 10 * time.Second,
		MaxRows: 1, MaxConnections: 1,
		ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: source.DatasourceID,
			Database: source.Database, User: source.User, PostgreSQLMajorVersion: source.PostgreSQLMajorVersion},
	})
	if err != nil {
		fatalf("connect datasource: %v", err)
	}
	defer connector.Close()
	output, err := generateContracts(ctx, logical, connector, selected)
	if err != nil {
		fatalf("generate contracts: %v", err)
	}
	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		fatalf("encode output: %v", err)
	}
	fmt.Println(string(encoded))
}

func selectSemanticProducts(logical *catalog.Catalog, supplied string) ([]catalog.Product, catalog.Source, error) {
	if logical == nil {
		return nil, catalog.Source{}, fmt.Errorf("catalog is nil")
	}
	wanted := make(map[string]struct{})
	if strings.TrimSpace(supplied) != "" {
		names, err := parseSelectedProductNames(supplied)
		if err != nil {
			return nil, catalog.Source{}, err
		}
		for _, value := range names {
			wanted[value] = struct{}{}
		}
	}
	sources := make(map[string]catalog.Source, len(logical.Sources))
	for _, source := range logical.Sources {
		sources[source.Name] = source
	}
	var selected []catalog.Product
	var source catalog.Source
	for _, product := range logical.ListProducts() {
		_, explicitlySelected := wanted[product.Name]
		if len(wanted) != 0 && !explicitlySelected {
			continue
		}
		if len(wanted) == 0 && product.ViewContract == nil {
			continue
		}
		candidate, present := sources[product.Source]
		if !present {
			return nil, catalog.Source{}, fmt.Errorf("product %q has no source", product.Name)
		}
		if source.Name == "" {
			source = candidate
		} else if source.Name != candidate.Name {
			return nil, catalog.Source{}, fmt.Errorf("semantic products span multiple sources")
		}
		selected = append(selected, product)
		delete(wanted, product.Name)
	}
	if len(wanted) != 0 {
		return nil, catalog.Source{}, fmt.Errorf("one or more selected products are absent")
	}
	if len(selected) == 0 {
		return nil, catalog.Source{}, fmt.Errorf("no semantic products selected")
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return selected, source, nil
}

func parseSelectedProductNames(supplied string) ([]string, error) {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, value := range strings.Split(supplied, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("product list contains an empty name")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("product %q is repeated", value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

type viewDiscoverer interface {
	DiscoverViewRegistry(context.Context, []viewcompiler.RelationName, map[string]string) (viewcompiler.RegistrySnapshot, error)
}

func generateContracts(ctx context.Context, logical *catalog.Catalog, discoverer viewDiscoverer,
	selected []catalog.Product) (contractOutput, error) {
	if logical == nil || discoverer == nil || len(selected) == 0 {
		return contractOutput{}, fmt.Errorf("catalog, discoverer, and selected products are required")
	}
	selectedNames := make(map[string]struct{}, len(selected))
	roots := make([]viewcompiler.RelationName, 0, len(selected))
	byRoot := make(map[viewcompiler.RelationName]catalog.Product, len(selected))
	for _, product := range selected {
		root, err := relationName(product.ReportingView)
		if err != nil {
			return contractOutput{}, err
		}
		if _, duplicate := byRoot[root]; duplicate {
			return contractOutput{}, fmt.Errorf("semantic roots are duplicated")
		}
		selectedNames[product.Name] = struct{}{}
		roots = append(roots, root)
		byRoot[root] = product
	}
	baseMap := make(map[string]string)
	products := make(map[string]queryplan.Product)
	for _, product := range logical.ListProducts() {
		if product.Source != selected[0].Source || product.ViewContract != nil {
			continue
		}
		if _, selected := selectedNames[product.Name]; selected {
			continue
		}
		baseMap[product.ReportingView] = product.Name
		products[product.Name] = compilerProduct(product)
	}
	if len(products) == 0 {
		return contractOutput{}, fmt.Errorf("semantic Views require governed terminal products")
	}
	snapshot, err := discoverer.DiscoverViewRegistry(ctx, roots, baseMap)
	if err != nil {
		return contractOutput{}, err
	}
	compiler, err := viewcompiler.New(snapshot, products)
	if err != nil {
		return contractOutput{}, err
	}
	output := contractOutput{Version: catalog.ViewContractV1, RegistryRevisionDigest: snapshot.RevisionDigest,
		Contracts: make(map[string]catalog.ViewContract, len(selected)), Dependencies: make(map[string][]string, len(selected))}
	for _, root := range roots {
		product := byRoot[root]
		artifact, err := compiler.Compile(root)
		if err != nil {
			return contractOutput{}, fmt.Errorf("compile %s: %w", product.Name, err)
		}
		if err := verifyInterface(product, artifact.Outputs); err != nil {
			return contractOutput{}, err
		}
		output.Contracts[product.Name] = catalog.ViewContract{ProfileVersion: catalog.ViewContractV1,
			DefinitionDigest: artifact.DefinitionDigest, DependencyDigest: artifact.DependencyDigest,
			CanonicalPlanDigest: artifact.CanonicalPlanDigest, InterfaceDigest: artifact.InterfaceDigest}
		for _, dependency := range artifact.DependencyClosure {
			output.Dependencies[product.Name] = append(output.Dependencies[product.Name], dependency.Name.String())
		}
	}
	return output, nil
}

func compilerProduct(product catalog.Product) queryplan.Product {
	columns := make(map[string]struct{}, len(product.Fields))
	types := make(map[string]string, len(product.Fields))
	collations := make(map[string]string, len(product.Fields))
	versions := make(map[string]string, len(product.Fields))
	for _, field := range product.Fields {
		columns[field.Name] = struct{}{}
		types[field.Name] = field.Type
		collations[field.Name] = field.Collation
		versions[field.Name] = field.CollationVersion
	}
	aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
	for _, aggregate := range product.AllowedAggregates {
		aggregates[strings.ToLower(strings.TrimSpace(aggregate))] = struct{}{}
	}
	return queryplan.Product{Name: product.Name, Columns: columns, AllowedAggregates: aggregates,
		ColumnTypes: types, ColumnCollations: collations, CollationVersions: versions,
		SourceNamespace: product.FactNamespace, Snapshot: product.Snapshot, StableRole: product.StableRelationRole,
		StableEntityKey: append([]string(nil), product.EntityKey...), LineageDigest: product.LineageManifestDigest,
		RequiredEvidence: append([]string(nil), product.Scopes...)}
}

func verifyInterface(product catalog.Product, outputs []viewcompiler.Output) error {
	if len(product.Fields) != len(outputs) {
		return fmt.Errorf("product %q field count disagrees with its View", product.Name)
	}
	for index, field := range product.Fields {
		typeName, err := exposure.CanonicalSQLTypeV2(field.Type)
		if err != nil || outputs[index].Name != field.Name || outputs[index].SQLType != typeName ||
			outputs[index].Collation != field.Collation || outputs[index].CollationVersion != field.CollationVersion {
			return fmt.Errorf("product %q field %d disagrees with its View", product.Name, index)
		}
	}
	return nil
}

func relationName(value string) (viewcompiler.RelationName, error) {
	schema, name, ok := strings.Cut(value, ".")
	if !ok || schema == "" || name == "" || strings.Contains(name, ".") {
		return viewcompiler.RelationName{}, fmt.Errorf("invalid reporting view %q", value)
	}
	return viewcompiler.RelationName{Schema: schema, Name: name}, nil
}

func fatalf(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", arguments...)
	os.Exit(1)
}
