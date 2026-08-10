package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5linker"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/snapshotbundle"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

const (
	p4c2ProvSQLCatalog   = "config/profiles/provsql-nonce-join.catalog.yaml"
	p4c2ManifestRoot     = "evaluation/final-v5-wsl2/oracle-manifests"
	p4c2SetMemoryMembers = 64 * 1024
	p4c2SetSampleMembers = 8
	p4c2ExpectedCells    = 105
	p4c2ExpectedRows     = int64(3)
)

type p4c2ExpectedCell struct {
	cell finalv5oracle.ProvSQLNonceJoinCell
	link finalv5linker.ExpectedLink
}

type p4c2PreparedStatementState struct {
	backendPID int64
	count      int64
}

// TestP4C2ProvSQLSemanticOrdinalLinkAgainstPostgreSQL is the member-level
// closure deliberately left open by P4.0-D1. It first regenerates and verifies
// the complete 105-file independent manifest set. It then resolves every
// semantic Fact into the reviewed publication universe before the tested
// lowering/Prepare/Derive chain runs, and finally compares each real production
// Influence bitmap member-for-member and by its role-bound semantic digest.
func TestP4C2ProvSQLSemanticOrdinalLinkAgainstPostgreSQL(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Fatal("BUSINESS_TEST_POSTGRES_DSN is required; P4.0-C2 must not skip its live production comparison")
	}
	repositoryRoot := p4c2RepositoryRoot(t)
	workContext, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()

	setOptions := finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: p4c2SetMemoryMembers,
		CaptureMembers:     p4c2SetSampleMembers,
		TempDir:            t.TempDir(),
	}
	// This full closed-set verifier is intentionally the first semantic or
	// production-facing operation in the test. Individual Decode calls do not
	// establish that no manifest is missing, extra, duplicated, or changed.
	manifestValues := p4c2ReadManifestClosedSet(t, filepath.Join(repositoryRoot, p4c2ManifestRoot), "provsql")
	manifestArtifacts, err := finalv5oracle.VerifyProvSQLNonceJoinManifestSet(manifestValues, setOptions)
	if err != nil {
		t.Fatalf("verify complete ProvSQL manifest closed set: %v", err)
	}
	if len(manifestArtifacts) != p4c2ExpectedCells {
		t.Fatalf("verified ProvSQL manifest count = %d, want %d", len(manifestArtifacts), p4c2ExpectedCells)
	}

	logicalCatalog, err := catalog.Load(filepath.Join(repositoryRoot, p4c2ProvSQLCatalog))
	if err != nil {
		t.Fatalf("load ProvSQL production Catalog: %v", err)
	}
	simpleDSN := p4c2SimpleProtocolDSN(t, dsn)
	artifactRoot := p4c2CompileFreshArtifacts(t, workContext, repositoryRoot, logicalCatalog, simpleDSN)
	publications, registry := p4c2LoadReviewedPublications(t, artifactRoot, logicalCatalog)
	universe, err := finalv5linker.ReviewPublications(logicalCatalog.SHA256, publications...)
	if err != nil {
		t.Fatalf("review ProvSQL publication universe: %v", err)
	}

	// Freeze all 105 semantic expectations and their exact ordinal members
	// before creating the production Connector or lowering any tested SQL. The
	// later production bitmap is therefore only a comparison operand; it cannot
	// feed the oracle, the reviewed semantic digest, or the expected bitmap.
	expectedCells := p4c2ResolveExpectedCells(t, universe, manifestArtifacts, setOptions)
	if len(expectedCells) != p4c2ExpectedCells {
		t.Fatalf("pre-run semantic-to-ordinal links = %d, want %d", len(expectedCells), p4c2ExpectedCells)
	}

	products, grant, queryTimeout := p4c2ProductionInputs(t, logicalCatalog)
	connector, err := dataconnector.New(workContext, dataconnector.Config{
		DSN:              simpleDSN,
		StatementTimeout: queryTimeout,
		ConnectTimeout:   10 * time.Second,
		MaxRows:          grant.Exposure.Limits.InfluenceFacts + 1,
		MaxConnections:   1,
		ApplicationName:  "taskgate-p4c2-semantic-ordinal-link",
	})
	if err != nil {
		t.Fatalf("open required Business PostgreSQL connector: %v", err)
	}
	defer connector.Close()

	before := p4c2PreparedStatements(t, workContext, connector)
	if before.count != 0 {
		t.Fatalf("production connector session began with %d prepared statements, want 0", before.count)
	}
	service := &Service{catalog: logicalCatalog, snapshotRegistry: registry}
	seenActualDigests := make(map[string]string, p4c2ExpectedCells)
	matchedByScale := make(map[string]int)
	for _, expectedCell := range expectedCells {
		cell := expectedCell.cell
		lowered, lowerErr := sqllowering.Lower(p4c2ProvSQL(cell), products)
		if lowerErr != nil {
			t.Fatalf("lower fixed production cell %s: %v", cell.BindingKey, lowerErr)
		}
		p4c2RequireFrozenPlan(t, cell.BindingKey, lowered.Plan)
		prepared, prepareErr := service.preparePlan(grant, lowered.Plan)
		if prepareErr != nil {
			t.Fatalf("prepare production cell %s: %v", cell.BindingKey, prepareErr)
		}
		if prepared.Exposure == nil || prepared.Exposure.ordinal == nil {
			t.Fatalf("production cell %s prepared no ordinal exposure context", cell.BindingKey)
		}
		visibleSQL, companionSQL, statementsErr := prepared.Prepared.ExecutableStatements()
		if statementsErr != nil {
			t.Fatalf("read prepared statements for cell %s: %v", cell.BindingKey, statementsErr)
		}
		derived, deriveErr := service.derivePhysicalQuery(sqlpolicy.New(sqlpolicy.Config{}), visibleSQL, companionSQL,
			prepared.PolicyGrant, physicalquery.LedgerPreState{
				RemainingRows:        grant.Budget.Rows,
				InfluenceFacts:       grant.Exposure.Limits.InfluenceFacts,
				UsesExpandedEvidence: prepared.Prepared.Binding().UsesExpandedEvidence(),
				HasExposureContext:   true,
			}, true)
		if deriveErr != nil {
			t.Fatalf("derive production cell %s: %v", cell.BindingKey, deriveErr)
		}
		if derived.companion == nil {
			t.Fatalf("production cell %s derived no companion statement", cell.BindingKey)
		}
		sink := &ordinalDerivationSink{
			program:            prepared.Exposure.ordinal.Program,
			indexes:            prepared.Exposure.ordinal.Indexes,
			planDigest:         prepared.Exposure.planDigest,
			predicateFootprint: prepared.Exposure.predicateFootprint,
		}
		pair, pairErr := connector.QueryPairStream(workContext, dataconnector.QueryPairStreamRequest{
			Visible: dataconnector.QueryRequest{
				SQL: derived.visible.SQL, StatementTimeout: queryTimeout, MaxRows: grant.Budget.Rows,
			},
			Provenance: dataconnector.QueryRequest{
				SQL: derived.companion.SQL, StatementTimeout: queryTimeout, MaxRows: derived.companionEvidenceRows,
			},
			ProvenanceSink: sink,
		})
		if pairErr != nil {
			t.Fatalf("execute production cell %s: %v", cell.BindingKey, pairErr)
		}
		if pair.Visible.Truncated || pair.Provenance.Truncated || pair.Visible.RowCount != p4c2ExpectedRows ||
			pair.Provenance.RowCount != 5*cell.Limit {
			t.Fatalf("production cell %s row closure: visible=%d truncated=%t companion=%d truncated=%t; want %d and %d",
				cell.BindingKey, pair.Visible.RowCount, pair.Visible.Truncated, pair.Provenance.RowCount,
				pair.Provenance.Truncated, p4c2ExpectedRows, 5*cell.Limit)
		}
		effect, finishErr := sink.Finish()
		if finishErr != nil {
			t.Fatalf("finish production ordinal derivation for cell %s: %v", cell.BindingKey, finishErr)
		}
		report, compareErr := universe.Compare(expectedCell.link, finalv5linker.CompareRequest{
			Actual: effect.Influence, ActualSource: finalv5linker.ActualSetSourceProductionFactSet,
		})
		if compareErr != nil {
			t.Fatalf("compare production members for cell %s: %v", cell.BindingKey, compareErr)
		}
		p4c2RequireExactLink(t, cell, report, prepared.Exposure.ordinal.DictionarySetDigest)
		if previous, duplicate := seenActualDigests[report.ActualOrdinalSetSHA256]; duplicate {
			t.Fatalf("production cells %s and %s have the same ordinal-set digest", previous, cell.BindingKey)
		}
		seenActualDigests[report.ActualOrdinalSetSHA256] = cell.BindingKey
		matchedByScale[cell.Scale]++
	}

	after := p4c2PreparedStatements(t, workContext, connector)
	if after.backendPID != before.backendPID || after.count != 0 {
		t.Fatalf("production connector prepared-statement state changed: before pid=%d count=%d; after pid=%d count=%d",
			before.backendPID, before.count, after.backendPID, after.count)
	}
	if len(seenActualDigests) != p4c2ExpectedCells || matchedByScale["1k"] != 35 ||
		matchedByScale["10k"] != 35 || matchedByScale["45k"] != 35 {
		t.Fatalf("production comparison closure: digests=%d scales=%v", len(seenActualDigests), matchedByScale)
	}
	t.Logf("P4.0-C2 matched exact members and role-bound semantic digests for 105/105 production FactSets; "+
		"scale cells=1k:%d,10k:%d,45k:%d; connector backend_pid=%d, pg_prepared_statements before/after=0/0",
		matchedByScale["1k"], matchedByScale["10k"], matchedByScale["45k"], before.backendPID)
}

func p4c2RepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve P4.0-C2 test source path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", ".."))
	if err != nil {
		t.Fatal("resolve P4.0-C2 repository root")
	}
	return root
}

func p4c2ReadManifestClosedSet(t *testing.T, oracleRoot, subtree string) map[string][]byte {
	t.Helper()
	root := filepath.Join(oracleRoot, subtree)
	values := make(map[string][]byte, p4c2ExpectedCells)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("manifest closed set contains symlink %q", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("manifest closed set contains non-JSON regular file %q", path)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 1<<20 {
			return fmt.Errorf("manifest closed-set member %q is not a bounded regular file", path)
		}
		encoded, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(oracleRoot, path)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("manifest path %q escapes the oracle root", path)
		}
		canonical := filepath.ToSlash(relative)
		if _, duplicate := values[canonical]; duplicate {
			return fmt.Errorf("duplicate manifest path %q", canonical)
		}
		values[canonical] = encoded
		return nil
	})
	if err != nil {
		t.Fatalf("read ProvSQL manifest closed set: %v", err)
	}
	return values
}

func p4c2SimpleProtocolDSN(t *testing.T, dsn string) string {
	t.Helper()
	var result string
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("parse Business PostgreSQL URL DSN")
		}
		query := parsed.Query()
		query.Set("default_query_exec_mode", "simple_protocol")
		parsed.RawQuery = query.Encode()
		result = parsed.String()
	} else {
		result = dsn + " default_query_exec_mode=simple_protocol"
	}
	parsed, err := pgx.ParseConfig(result)
	if err != nil || parsed.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol {
		t.Fatal("Business PostgreSQL DSN cannot be pinned to pgx simple protocol")
	}
	return result
}

func p4c2CompileFreshArtifacts(t *testing.T, ctx context.Context, repositoryRoot string,
	logicalCatalog *catalog.Catalog, dsn string) string {
	t.Helper()
	generated := filepath.Join(t.TempDir(), "snapshot-index-artifacts")
	t.Log("compiling fresh ProvSQL publications from current reviewed inputs and the required live frozen database")
	for _, publication := range logicalCatalog.SnapshotPublications {
		inputPath := filepath.Join(repositoryRoot, "config", "snapshots", publication.Name+".json")
		inputFile, openErr := p4c2OpenRegularFile(inputPath, -1)
		if openErr != nil {
			t.Fatalf("open compiler input for %s: %v", publication.Name, openErr)
		}
		input, decodeErr := snapshotbundle.DecodeCompilerInput(inputFile)
		closeErr := inputFile.Close()
		if decodeErr != nil || closeErr != nil {
			t.Fatalf("decode compiler input for %s: %v", publication.Name, errors.Join(decodeErr, closeErr))
		}
		scanned, scanErr := snapshotbundle.ScanPostgresSnapshot(ctx, input, dsn)
		if scanErr != nil {
			t.Fatalf("scan live snapshot for %s: %v", publication.Name, scanErr)
		}
		written, compileErr := snapshotbundle.CompileOwnedToDirectory(&scanned, generated,
			snapshotbundle.DefaultPublicationLimits())
		if compileErr != nil || written.Manifest.ManifestDigest != publication.ManifestDigest {
			t.Fatalf("compile Catalog-bound publication %s: digest=%s err=%v",
				publication.Name, written.Manifest.ManifestDigest, compileErr)
		}
	}
	return generated
}

func p4c2LoadReviewedPublications(t *testing.T, artifactRoot string,
	logicalCatalog *catalog.Catalog) ([]finalv5linker.Publication, *ordinal.Registry) {
	t.Helper()
	registry, err := ordinal.NewRegistry()
	if err != nil {
		t.Fatalf("create production snapshot registry: %v", err)
	}
	result := make([]finalv5linker.Publication, 0, len(logicalCatalog.SnapshotPublications))
	for _, publication := range logicalCatalog.SnapshotPublications {
		directory := filepath.Join(artifactRoot, publication.Name)
		p4c2RequirePublicationDirectory(t, directory, publication.Name)
		bundlePath := filepath.Join(directory, publication.Name+".bundle.json")
		bundleFile, openErr := p4c2OpenRegularFile(bundlePath, -1)
		if openErr != nil {
			t.Fatalf("open bundle manifest for %s: %v", publication.Name, openErr)
		}
		bundle, decodeErr := snapshotbundle.DecodeBundleManifest(io.LimitReader(bundleFile, 1<<20))
		closeErr := bundleFile.Close()
		if decodeErr != nil || closeErr != nil {
			t.Fatalf("decode bundle manifest for %s: %v", publication.Name, errors.Join(decodeErr, closeErr))
		}
		p4c2RequireCatalogBundle(t, logicalCatalog, publication, bundle)

		hotPath := filepath.Join(directory, bundle.Hot.Name)
		hotFile, hotOpenErr := p4c2OpenRegularFile(hotPath, bundle.Hot.Bytes)
		if hotOpenErr != nil {
			t.Fatalf("open HOT artifact for %s: %v", publication.Name, hotOpenErr)
		}
		hotBytes, hotReadErr := io.ReadAll(hotFile)
		hotCloseErr := hotFile.Close()
		if hotReadErr != nil || hotCloseErr != nil {
			t.Fatalf("read HOT artifact for %s: %v", publication.Name, errors.Join(hotReadErr, hotCloseErr))
		}
		if p4c2SHA256(hotBytes) != bundle.Hot.SHA256 {
			t.Fatalf("HOT transport digest for %s differs from its bundle descriptor", publication.Name)
		}
		hot, parseErr := ordinal.ParseHotDictionary(hotBytes, publication.ManifestDigest)
		hotBytes = nil
		runtime.GC()
		if parseErr != nil {
			t.Fatalf("parse HOT artifact for %s: %v", publication.Name, parseErr)
		}
		if hot.ManifestDigest() != bundle.ManifestDigest || hot.DictionaryDigest() != publication.DictionaryDigest ||
			hot.RowCount() != bundle.RowCount {
			t.Fatalf("HOT identity for %s differs from Catalog/bundle: manifest=%s dictionary=%s rows=%d",
				publication.Name, hot.ManifestDigest(), hot.DictionaryDigest(), hot.RowCount())
		}

		coldPath := filepath.Join(directory, bundle.Cold.Name)
		coldFile, coldOpenErr := p4c2OpenRegularFile(coldPath, bundle.Cold.Bytes)
		if coldOpenErr != nil {
			t.Fatalf("open COLD artifact for %s: %v", publication.Name, coldOpenErr)
		}
		closure, closureErr := finalv5linker.VerifyColdClosure(coldFile, bundle.Cold.Bytes, hot)
		coldCloseErr := coldFile.Close()
		if closureErr != nil || coldCloseErr != nil {
			t.Fatalf("verify complete COLD closure for %s: %v", publication.Name, errors.Join(closureErr, coldCloseErr))
		}
		if closure.ArtifactSHA256 != bundle.Cold.SHA256 || closure.ArtifactBytes != bundle.Cold.Bytes {
			t.Fatalf("COLD transport identity for %s differs from its bundle descriptor", publication.Name)
		}

		sidecarPath := filepath.Join(directory, bundle.Sidecar.Name)
		sidecarFile, sidecarOpenErr := p4c2OpenRegularFile(sidecarPath, bundle.Sidecar.Bytes)
		if sidecarOpenErr != nil {
			t.Fatalf("open sidecar artifact for %s: %v", publication.Name, sidecarOpenErr)
		}
		sidecarHash := sha256.New()
		sidecarErr := snapshotbundle.VerifySidecarNDJSON(io.TeeReader(sidecarFile, sidecarHash), hot,
			snapshotbundle.SidecarExpectation{
				PublicationName: publication.Name,
				OrdinalSidecar:  publication.OrdinalSidecar,
				SourceNamespace: publication.SourceNamespace,
				ManifestDigest:  publication.ManifestDigest,
				SidecarDigest:   publication.SidecarDigest,
			})
		sidecarCloseErr := sidecarFile.Close()
		if sidecarErr != nil || sidecarCloseErr != nil {
			t.Fatalf("verify complete sidecar for %s: %v", publication.Name, errors.Join(sidecarErr, sidecarCloseErr))
		}
		if hex.EncodeToString(sidecarHash.Sum(nil)) != bundle.Sidecar.SHA256 {
			t.Fatalf("sidecar transport digest for %s differs from its bundle descriptor", publication.Name)
		}

		if registerErr := registry.RegisterPublication(ordinal.PublicationKey{
			CatalogDigest: logicalCatalog.SHA256, PublicationName: publication.Name,
		}, publication.ManifestDigest, hot); registerErr != nil {
			t.Fatalf("register reviewed publication %s: %v", publication.Name, registerErr)
		}
		closureCopy := closure
		result = append(result, finalv5linker.Publication{
			Name: publication.Name, Index: hot, ColdClosure: &closureCopy,
		})
	}
	return result, registry
}

func p4c2RequirePublicationDirectory(t *testing.T, directory, publication string) {
	t.Helper()
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		t.Fatalf("publication %s has no owner-controlled regular directory: %v", publication, err)
	}
	want := map[string]bool{
		publication + ".bundle.json":    true,
		publication + ".hot.tgord":      true,
		publication + ".cold.tgord":     true,
		publication + ".sidecar.ndjson": true,
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read publication directory %s: %v", publication, err)
	}
	if len(entries) != len(want) {
		t.Fatalf("publication directory %s contains %d entries, want exactly %d", publication, len(entries), len(want))
	}
	for _, entry := range entries {
		if !want[entry.Name()] || entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			t.Fatalf("publication directory %s contains unexpected or non-regular entry %q", publication, entry.Name())
		}
	}
}

func p4c2RequireCatalogBundle(t *testing.T, logicalCatalog *catalog.Catalog,
	publication catalog.SnapshotPublication, bundle snapshotbundle.BundleManifest) {
	t.Helper()
	manifest := bundle.DictionaryManifest
	wantFiles := []string{
		publication.Name + ".cold.tgord",
		publication.Name + ".hot.tgord",
		publication.Name + ".sidecar.ndjson",
	}
	gotFiles := []string{bundle.Cold.Name, bundle.Hot.Name, bundle.Sidecar.Name}
	sort.Strings(wantFiles)
	sort.Strings(gotFiles)
	var sourceID string
	for _, source := range logicalCatalog.Sources {
		if source.Name == publication.Source {
			sourceID = source.DatasourceID
			break
		}
	}
	if bundle.PublicationName != publication.Name || bundle.CatalogSource != publication.Source ||
		bundle.OrdinalSidecar != publication.OrdinalSidecar || bundle.ManifestDigest != publication.ManifestDigest ||
		manifest.SourceID != sourceID || manifest.SourceNamespace != publication.SourceNamespace ||
		manifest.Snapshot != publication.Snapshot || manifest.DictionaryDigest != publication.DictionaryDigest ||
		manifest.SidecarDigest != publication.SidecarDigest || strings.Join(gotFiles, "\x00") != strings.Join(wantFiles, "\x00") {
		t.Fatalf("bundle for %s does not match its Catalog publication identity", publication.Name)
	}
}

func p4c2OpenRegularFile(path string, expectedBytes int64) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("path is not an owner-controlled regular file")
	}
	if expectedBytes >= 0 && before.Size() != expectedBytes {
		return nil, fmt.Errorf("file size = %d, want %d", before.Size(), expectedBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("opened file identity differs from checked path")
	}
	return file, nil
}

func p4c2SHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func p4c2ResolveExpectedCells(t *testing.T, universe *finalv5linker.ReviewedUniverse,
	artifacts []finalv5oracle.ProvSQLManifestArtifact,
	options finalv5oracle.StreamSetOptions) []p4c2ExpectedCell {
	t.Helper()
	byBinding := make(map[string]finalv5oracle.OracleManifest, len(artifacts))
	for _, artifact := range artifacts {
		binding := artifact.Manifest.BindingKey
		if _, duplicate := byBinding[binding]; duplicate {
			t.Fatalf("verified manifest set repeats binding %s", binding)
		}
		byBinding[binding] = artifact.Manifest
	}
	result := make([]p4c2ExpectedCell, 0, p4c2ExpectedCells)
	for _, scheduled := range finalv5oracle.ProvSQLNonceJoinCells() {
		cell := scheduled
		manifest, present := byBinding[cell.BindingKey]
		if !present || manifest.Expected.DependencyCandidateCardinality == nil {
			t.Fatalf("verified manifest set omits complete dependency expectation for %s", cell.BindingKey)
		}
		wantCardinality := finalv5oracle.ProvSQLFactsPerOrder*cell.Limit + finalv5oracle.ProvSQLNonceFacts
		if *manifest.Expected.DependencyCandidateCardinality != wantCardinality {
			t.Fatalf("verified manifest cardinality for %s = %d, want 29L+3 = %d", cell.BindingKey,
				*manifest.Expected.DependencyCandidateCardinality, wantCardinality)
		}
		link, err := universe.Expected(finalv5linker.ExpectedRequest{
			Role: "candidate",
			OracleFacts: func(yield func(finalv5oracle.CanonicalFact) error) error {
				return finalv5oracle.StreamProvSQLNonceJoinFacts(cell.Limit, cell.Nonce, yield)
			},
			Expected: finalv5linker.SemanticExpectation{
				Cardinality: *manifest.Expected.DependencyCandidateCardinality,
				SetSHA256:   manifest.Expected.DependencyCandidateSetSHA256,
			},
			Options: finalv5linker.Options{Set: options},
		})
		if err != nil {
			t.Fatalf("resolve pre-run semantic expectation %s: %v", cell.BindingKey, err)
		}
		if link.Report.PayloadVerification.VerifiedColdClosureMembers != wantCardinality ||
			link.Report.PayloadVerification.ExactCanonicalPayloadMembers != 0 ||
			link.Report.PayloadVerification.CachedExactPayloadMembers != 0 ||
			link.Report.OracleSemantic.SetSHA256 != manifest.Expected.DependencyCandidateSetSHA256 ||
			link.Report.ExpectedOrdinalCardinality != uint64(wantCardinality) {
			t.Fatalf("pre-run linker closure for %s is incomplete: payload=%+v semantic=%+v ordinals=%d",
				cell.BindingKey, link.Report.PayloadVerification, link.Report.OracleSemantic,
				link.Report.ExpectedOrdinalCardinality)
		}
		result = append(result, p4c2ExpectedCell{cell: cell, link: link})
	}
	return result
}

func p4c2ProductionInputs(t *testing.T, logicalCatalog *catalog.Catalog) (
	map[string]queryplan.Product, control.TaskGrant, time.Duration) {
	t.Helper()
	approvedProducts := []string{"provsql_lineitem", "provsql_nonce", "provsql_orders"}
	approvedColumns := map[string][]string{
		"provsql_orders":   {"orderkey", "status", "partition_key"},
		"provsql_lineitem": {"orderkey", "linenumber", "extendedprice", "partition_key"},
		"provsql_nonce":    {"nonce_id", "partition_key"},
	}
	policy, err := logicalCatalog.ResolveTaskPolicy(approvedProducts)
	if err != nil {
		t.Fatalf("resolve production ProvSQL task policy: %v", err)
	}
	if policy.Budget.MaxRows != 3 || policy.Budget.MaxInfluenceFacts != 2_000_000 ||
		policy.Budget.MaxReleaseFacts != 32 || policy.Budget.MaxOutcomeFacts != 64 {
		t.Fatalf("production ProvSQL budget changed: %+v", policy.Budget)
	}
	products := make(map[string]queryplan.Product, len(approvedColumns))
	for name, columns := range approvedColumns {
		product, present := logicalCatalog.LookupProduct(name)
		if !present {
			t.Fatalf("production Catalog omits product %s", name)
		}
		products[name] = physicalquery.QueryProductFromCatalog(product, stringSetFromSlice(columns))
	}
	grant := control.TaskGrant{
		TaskID:           "p4c2-semantic-ordinal-link",
		Subject:          "p4c2-review",
		Purpose:          "P4.0-C2 production FactSet comparison",
		ApprovedProducts: append([]string(nil), approvedProducts...),
		ApprovedColumns:  approvedColumns,
		MandatoryScope:   json.RawMessage(`{"partition_key":["1"]}`),
		Budget: control.BudgetLimits{
			Queries: policy.Budget.MaxQueries,
			Rows:    policy.Budget.MaxRows,
			DBMS:    policy.Budget.MaxDBTime.Milliseconds(),
		},
		Exposure: control.ExposureGrant{
			ProfileVersion: policy.Budget.ExposureProfileVersion,
			Limits: control.ExposureLimits{
				ReleaseFacts: policy.Budget.MaxReleaseFacts, InfluenceFacts: policy.Budget.MaxInfluenceFacts,
				OutcomeFacts: policy.Budget.MaxOutcomeFacts,
			},
			PredicateFootprint: controlPredicateFootprint(policy.Budget.PredicateFootprint),
		},
		CatalogVersion: logicalCatalog.CatalogVersion,
		CatalogDigest:  logicalCatalog.SHA256,
	}
	return products, grant, policy.Budget.PerQueryTimeout
}

func p4c2ProvSQL(cell finalv5oracle.ProvSQLNonceJoinCell) string {
	return fmt.Sprintf(`SELECT o.status::bigint AS status,
       sum(l.extendedprice)::text AS price,
       sum(l.linenumber)::bigint AS lines,
       count(*)::bigint AS members
FROM provsql_orders AS o
JOIN provsql_lineitem AS l
  ON l.orderkey = o.orderkey AND l.partition_key = o.partition_key
JOIN provsql_nonce AS nonce
  ON nonce.partition_key = o.partition_key
WHERE o.orderkey <= %d AND nonce.nonce_id = %d
GROUP BY o.status
ORDER BY o.status`, cell.Limit, cell.Nonce)
}

func p4c2RequireFrozenPlan(t *testing.T, bindingKey string, plan queryplan.QueryPlan) {
	t.Helper()
	if len(plan.Columns) != 1 || plan.Columns[0] != "provsql_orders.status" ||
		len(plan.GroupBy) != 1 || plan.GroupBy[0] != plan.Columns[0] ||
		len(plan.OrderBy) != 1 || plan.OrderBy[0].Column != plan.GroupBy[0] || plan.OrderBy[0].Direction != "ASC" ||
		len(plan.Aggregates) != 3 {
		t.Fatalf("lowered production plan for %s lost the frozen grouped shape: %+v", bindingKey, plan)
	}
	wantAggregates := map[string]struct {
		function string
		column   string
		encoding string
	}{
		"price":   {function: "sum", column: "provsql_lineitem.extendedprice", encoding: queryplan.NumericTextResultEncoding},
		"lines":   {function: "sum", column: "provsql_lineitem.linenumber"},
		"members": {function: "count", column: "*"},
	}
	for _, aggregate := range plan.Aggregates {
		want, present := wantAggregates[aggregate.Alias]
		if !present || aggregate.Function != want.function || aggregate.Column != want.column ||
			aggregate.ResultEncoding != want.encoding {
			t.Fatalf("lowered production aggregate for %s differs from frozen shape: %+v", bindingKey, aggregate)
		}
	}
}

func p4c2RequireExactLink(t *testing.T, cell finalv5oracle.ProvSQLNonceJoinCell,
	report finalv5linker.Report, preparedDictionarySetSHA256 string) {
	t.Helper()
	wantCardinality := finalv5oracle.ProvSQLFactsPerOrder*cell.Limit + finalv5oracle.ProvSQLNonceFacts
	if !report.Match || !report.OrdinalSetEqual || report.Role != "candidate" ||
		report.ActualOrdinalSource != finalv5linker.ActualSetSourceProductionFactSet ||
		report.OracleSemantic.Cardinality != wantCardinality || report.ActualSemantic.Cardinality != wantCardinality ||
		report.OracleSemantic.SetSHA256 != report.Reviewed.SetSHA256 ||
		report.ActualSemantic.SetSHA256 != report.Reviewed.SetSHA256 ||
		report.ExpectedOrdinalCardinality != uint64(wantCardinality) ||
		report.ActualOrdinalCardinality != uint64(wantCardinality) ||
		report.ExpectedOrdinalSetSHA256 != report.ActualOrdinalSetSHA256 ||
		report.DictionarySetSHA256 != preparedDictionarySetSHA256 {
		t.Fatalf("production semantic/ordinal comparison for %s is not exact: match=%t ordinal_equal=%t "+
			"semantic=%d/%s actual=%d/%s reviewed=%d/%s ordinals=%d/%s actual=%d/%s dictionaries=%s/%s",
			cell.BindingKey, report.Match, report.OrdinalSetEqual,
			report.OracleSemantic.Cardinality, report.OracleSemantic.SetSHA256,
			report.ActualSemantic.Cardinality, report.ActualSemantic.SetSHA256,
			report.Reviewed.Cardinality, report.Reviewed.SetSHA256,
			report.ExpectedOrdinalCardinality, report.ExpectedOrdinalSetSHA256,
			report.ActualOrdinalCardinality, report.ActualOrdinalSetSHA256,
			report.DictionarySetSHA256, preparedDictionarySetSHA256)
	}
	if len(report.Dictionaries) != 3 {
		t.Fatalf("production comparison for %s reports %d dictionaries, want 3", cell.BindingKey, len(report.Dictionaries))
	}
	for _, dictionary := range report.Dictionaries {
		if dictionary.PayloadVerificationMode != finalv5linker.PayloadVerificationColdClosure ||
			dictionary.ColdArtifactSHA256 == "" || dictionary.ColdArtifactBytes <= 0 {
			t.Fatalf("production comparison for %s has incomplete COLD closure identity: %+v", cell.BindingKey, dictionary)
		}
	}
}

func p4c2PreparedStatements(t *testing.T, ctx context.Context,
	connector *dataconnector.Connector) p4c2PreparedStatementState {
	t.Helper()
	result, err := connector.Query(ctx, dataconnector.QueryRequest{
		SQL: `SELECT pg_backend_pid()::bigint,
                    (SELECT count(*)::bigint FROM pg_catalog.pg_prepared_statements)`,
		StatementTimeout: 5 * time.Second,
		MaxRows:          1,
	})
	if err != nil {
		t.Fatalf("read production connector prepared-statement state: %v", err)
	}
	if result.Truncated || result.RowCount != 1 || len(result.Rows) != 1 || len(result.Rows[0]) != 2 {
		t.Fatalf("prepared-statement state query returned invalid shape: rows=%d truncated=%t", result.RowCount, result.Truncated)
	}
	pid, pidOK := result.Rows[0][0].(int64)
	count, countOK := result.Rows[0][1].(int64)
	if !pidOK || !countOK || pid <= 0 || count < 0 {
		t.Fatalf("prepared-statement state query returned invalid typed values: %T/%T",
			result.Rows[0][0], result.Rows[0][1])
	}
	return p4c2PreparedStatementState{backendPID: pid, count: count}
}
