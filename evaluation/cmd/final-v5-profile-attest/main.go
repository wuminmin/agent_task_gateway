// Command final-v5-profile-attest computes each deployment profile's own
// datasource schema attestation against a live Business PostgreSQL.
//
// A profile Catalog declares only its own Product closure, so the live
// reporting-schema attestation covers only those views and cannot equal the
// digest computed for the full Catalog. This binds each profile Catalog to the
// attestation of exactly what it declares, reusing evaluation/cmd/schema-digest's
// canonicalization rather than introducing a second one.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

const (
	registryPath     = "config/profiles/registry.json"
	attestationPath  = "config/profiles/schema-attestations-v1.json"
	attestationLabel = "taskgate-final-v5-profile-schema-attestation-v1"
)

func main() {
	root := flag.String("root", ".", "repository root")
	dsn := flag.String("dsn", "", "PostgreSQL DSN for the catalog reader")
	out := flag.String("out", attestationPath, "attestation registry output path")
	flag.Parse()
	if flag.NArg() != 0 || strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(os.Stderr, "final-v5-profile-attest: -dsn is required")
		os.Exit(2)
	}
	if err := run(*root, *dsn, *out); err != nil {
		fmt.Fprintln(os.Stderr, "final-v5-profile-attest:", err)
		os.Exit(1)
	}
}

func run(root, dsn, out string) error {
	value, err := os.ReadFile(filepath.Join(root, registryPath))
	if err != nil {
		return err
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(value, &registry); err != nil {
		return err
	}
	document := finalv5profile.SchemaAttestationRegistry{SchemaVersion: 1,
		AttestationVersion: attestationLabel, ContractRelease: registry.ContractRelease,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	toolDigest, err := sourceDigest(filepath.Join(root, "evaluation/cmd/schema-digest/main.go"))
	if err != nil {
		return err
	}
	for _, profile := range registry.Profiles {
		if !profile.Status.CatalogMaterializable {
			// A profile without a live per-profile Catalog must not acquire a
			// fabricated digest merely because its abstract closure exists.
			continue
		}
		attestation, err := attest(root, dsn, profile, toolDigest)
		if err != nil {
			return fmt.Errorf("profile %q: %w", profile.Alias, err)
		}
		document.Profiles = append(document.Profiles, attestation)
		if document.Source == "" {
			document.Source = attestation.Source
			document.PostgreSQLMajorVersion = attestation.PostgreSQLMajorVersion
		}
		fmt.Printf("%-28s %s %s\n", profile.Alias, profile.ID, attestation.SchemaDigest)
	}
	if len(document.Profiles) == 0 {
		return errors.New("no materializable profile was attested")
	}
	sort.Slice(document.Profiles, func(a, b int) bool {
		return document.Profiles[a].ProfileID < document.Profiles[b].ProfileID
	})
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, out), append(encoded, '\n'), 0o644)
}

func attest(root, dsn string, profile finalv5profile.Profile,
	toolDigest string) (finalv5profile.SchemaAttestation, error) {
	loaded, err := catalog.Load(filepath.Join(root, profile.CatalogPath))
	if err != nil {
		return finalv5profile.SchemaAttestation{}, err
	}
	source, schemas, err := expectedSchemas(loaded)
	if err != nil {
		return finalv5profile.SchemaAttestation{}, err
	}
	views := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		views = append(views, schema.Schema+"."+schema.View)
	}
	sort.Strings(views)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: dsn, StatementTimeout: 30 * time.Second, ConnectTimeout: 15 * time.Second,
		MaxRows: 1, MaxConnections: 1, ExpectedSchema: schemas,
		ExpectedAttestation: dataconnector.ExpectedAttestation{DatasourceID: source.DatasourceID,
			Database: source.Database, User: source.User, PostgreSQLMajorVersion: source.PostgreSQLMajorVersion},
	})
	if err != nil {
		return finalv5profile.SchemaAttestation{}, err
	}
	defer connector.Close()
	attestation, err := connector.Attestation(ctx)
	if err != nil {
		return finalv5profile.SchemaAttestation{}, err
	}
	return finalv5profile.SchemaAttestation{ProfileID: profile.ID, ProfileAlias: profile.Alias,
		ClosureSHA256: profile.Closure.SHA256, Source: source.Name,
		PostgreSQLMajorVersion: source.PostgreSQLMajorVersion,
		ProductSetSHA256:       finalv5profile.CanonicalNameSetSHA256("product-set", profile.Closure.Products),
		ReportingViewSetSHA256: finalv5profile.CanonicalNameSetSHA256("reporting-view-set", views),
		ReportingViews:         views, SchemaDigest: attestation.SchemaDigest,
		SchemaDigestToolSHA256: toolDigest, GeneratedFromFreshDeployment: true}, nil
}

func sourceDigest(path string) (string, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:]), nil
}
