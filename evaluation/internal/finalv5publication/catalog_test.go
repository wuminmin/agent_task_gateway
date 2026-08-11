package finalv5publication

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalogschema"
)

func TestBuildCatalogCandidateMergesExactApprovedScaleMaterial(t *testing.T) {
	const liveSchemaSHA256 = "bf4c3fe0897000f7673250e5fe0131a03019b49e9c3839e9fc4911c3716290b4"
	root := repositoryRoot(t)
	base, err := os.ReadFile(filepath.Join(root, "config", "catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scale, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(filepath.Join(
		filepath.Dir(C2CandidateRelativePath), "catalog.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := BuildCatalogCandidate(base, scale, liveSchemaSHA256)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := candidate.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if candidate.SHA256() == "" || parsed.SHA256 != candidate.SHA256() ||
		parsed.CatalogVersion != CatalogCandidateVersion {
		t.Fatalf("incomplete Catalog candidate: %+v", candidate)
	}
	for _, products := range [][]string{{"final_v5_exposure_scale"}, {"final_v5_result_heavy"},
		{"provsql_lineitem", "provsql_nonce", "provsql_orders"}} {
		if _, err := parsed.ResolveTaskPolicy(products); err != nil {
			t.Fatalf("complete Catalog cannot route %v: %v", products, err)
		}
	}
	built, err := catalogschema.Build(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if built.Source.SchemaDigest != liveSchemaSHA256 || len(built.Entries) != 11 || built.Count != 11 {
		t.Fatalf("Catalog attestation closure = source %s, schemas %d; want live digest/11",
			built.Source.SchemaDigest, len(built.Entries))
	}
}

func TestBuildCatalogCandidateRejectsPlaceholderAndByteDrift(t *testing.T) {
	root := repositoryRoot(t)
	base := readFile(t, filepath.Join(root, "config", "catalog.yaml"))
	scalePath := filepath.Join(root, filepath.FromSlash(filepath.Join(filepath.Dir(C2CandidateRelativePath), "catalog.yaml")))
	scale := readFile(t, scalePath)
	if _, err := BuildCatalogCandidate(base, scale, strings.Repeat("0", 64)); err == nil {
		t.Fatal("placeholder live schema digest was accepted")
	}
	drifted := strings.Replace(string(scale), "  - name: final_v5_exposure_scale\n", "  - name: changed_scale\n", 1)
	if _, err := BuildCatalogCandidate(base, []byte(drifted), strings.Repeat("b", 64)); err == nil {
		t.Fatal("drifted approved Scale Catalog was accepted")
	}
	shapePreservingDrift := strings.Replace(string(scale),
		"description: Final-V5 controlled exposure and dependency scale relation",
		"description: silently drifted while retaining every fixed name", 1)
	if _, err := BuildCatalogCandidate(base, []byte(shapePreservingDrift), strings.Repeat("b", 64)); err == nil {
		t.Fatal("shape-preserving drift of the exact C2-approved Catalog bytes was accepted")
	}
}

func TestCatalogSimpleProtocolDSNDisablesStatementCaches(t *testing.T) {
	for _, dsn := range []string{
		"postgres://reader:password@example.test/travel_demo?sslmode=disable",
		"host=example.test user=reader password=password dbname=travel_demo sslmode=disable",
	} {
		resolved, err := catalogSimpleProtocolDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if resolved == dsn {
			t.Fatal("Catalog attestation DSN did not acquire simple-protocol cache guards")
		}
	}
}
