package finalv5publication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

const CatalogCandidateVersion = "2026-08-11.final-v5-publication-binding-review-v1"

const approvedC2ScaleCatalogSHA256 = "8bad5660a09b2de497dde9c038f05e115bfa20385f54590b8a87e418fa6ee4f3"

// CatalogCandidate is a complete, non-installed review companion. E1 uses it
// only to prove all 12/6/105 tasks are realizable against one exact Catalog;
// installation and activation remain E2 work.
type CatalogCandidate struct {
	value  []byte
	sha256 string
}

// Bytes returns a detached copy of the exact candidate artifact.
func (candidate CatalogCandidate) Bytes() []byte { return append([]byte(nil), candidate.value...) }

// SHA256 returns the digest of the exact candidate artifact bytes.
func (candidate CatalogCandidate) SHA256() string { return candidate.sha256 }

// Catalog reparses the retained bytes so callers cannot mutate a cached model
// while continuing to claim the original candidate SHA-256.
func (candidate CatalogCandidate) Catalog() (*catalog.Catalog, error) {
	if len(candidate.value) == 0 || sha256Hex(candidate.value) != candidate.sha256 {
		return nil, errors.New("complete Catalog candidate bytes no longer match their identity")
	}
	parsed, err := catalog.Parse(candidate.value)
	if err != nil || parsed.SHA256 != candidate.sha256 {
		return nil, errors.New("reparse complete Catalog candidate")
	}
	return parsed, nil
}

// CatalogAttestation is the credential-free result of deriving the exact
// Gateway ExpectedSchema through catalogschema.Build and checking it against
// one live datasource using pgx simple protocol.
type CatalogAttestation struct {
	Datasource               dataconnector.Attestation `json:"datasource"`
	ExpectedSchemaEntries    int64                     `json:"expected_schema_entries"`
	ExpectedSchemaListSHA256 string                    `json:"expected_schema_list_sha256"`
	QueryExecMode            string                    `json:"query_exec_mode"`
	PreparedStatementCount   int64                     `json:"prepared_statement_count"`
}

// BuildCatalogCandidate merges exactly the approved C2 Scale Product and
// publication into the current source-controlled complete Catalog. The live
// schema digest is caller-supplied only after independent datasource
// attestation; placeholders and all-zero digests are rejected.
func BuildCatalogCandidate(baseBytes, approvedScaleBytes []byte, liveSchemaSHA256 string) (CatalogCandidate, error) {
	var result CatalogCandidate
	if !generatedSHA256(liveSchemaSHA256) {
		return result, errors.New("complete Catalog candidate requires a live non-placeholder schema digest")
	}
	merged, scale, err := mergeCatalogModel(baseBytes, approvedScaleBytes, liveSchemaSHA256)
	if err != nil {
		return result, err
	}

	encoded, err := yaml.Marshal(merged)
	if err != nil {
		return result, errors.New("encode complete Catalog candidate")
	}
	header := []byte("# REVIEW_CANDIDATE; author_approved=false; runtime_installation=forbidden\n")
	encoded = append(header, encoded...)
	parsed, err := catalog.Parse(encoded)
	if err != nil {
		return result, fmt.Errorf("reparse complete Catalog candidate: %w", err)
	}
	if strings.Contains(string(encoded), "NOT_GENERATED") || bytes.Contains(encoded, []byte(strings.Repeat("0", 64))) {
		return result, errors.New("complete Catalog candidate contains a placeholder identity")
	}
	if !containsExactScaleMaterial(parsed, scale) {
		return result, errors.New("complete Catalog candidate changed the approved Scale Product or publication")
	}
	return CatalogCandidate{value: append([]byte(nil), encoded...), sha256: parsed.SHA256}, nil
}

// CatalogAttestationModel returns the exact merged Product/view closure used
// to discover a fresh schema digest. Its retained Source digest is deliberately
// ignored by catalogschema.Build's entry derivation and no bytes from this
// model are emitted.
func CatalogAttestationModel(baseBytes, approvedScaleBytes []byte) (*catalog.Catalog, error) {
	base, err := catalog.Parse(baseBytes)
	if err != nil {
		return nil, fmt.Errorf("parse base Catalog: %w", err)
	}
	merged, _, err := mergeCatalogModel(baseBytes, approvedScaleBytes, base.Sources[0].SchemaDigest)
	return merged, err
}

// AttestCatalogModel obtains the live schema digest inserted into the emitted
// Catalog candidate. The DSN is used only in memory and never returned.
func AttestCatalogModel(ctx context.Context, dsn string, logical *catalog.Catalog) (CatalogAttestation, error) {
	var result CatalogAttestation
	if ctx == nil || strings.TrimSpace(dsn) == "" {
		return result, errors.New("complete Catalog attestation requires context and Business PostgreSQL DSN")
	}
	built, err := catalogschema.Build(logical)
	if err != nil {
		return result, fmt.Errorf("derive exact Gateway ExpectedSchema: %w", err)
	}
	simpleDSN, err := catalogSimpleProtocolDSN(dsn)
	if err != nil {
		return result, errors.New("parse Business PostgreSQL DSN for complete Catalog attestation")
	}
	connector, err := dataconnector.New(ctx, dataconnector.Config{
		DSN: simpleDSN, StatementTimeout: 2 * time.Minute, ConnectTimeout: 30 * time.Second,
		MaxRows: 1, MaxConnections: 1, ApplicationName: "taskgate-final-v5-publication-catalog-e1",
		ExpectedSchema: built.Entries,
		ExpectedAttestation: dataconnector.ExpectedAttestation{
			DatasourceID: built.Source.DatasourceID, Database: built.Source.Database, User: built.Source.User,
			PostgreSQLMajorVersion: built.Source.PostgreSQLMajorVersion,
		},
	})
	if err != nil {
		return result, errors.New("attest complete Catalog datasource")
	}
	defer connector.Close()
	attestation, err := connector.Attestation(ctx)
	if err != nil || !generatedSHA256(attestation.SchemaDigest) {
		return result, errors.New("complete Catalog live schema attestation is missing or invalid")
	}
	prepared, err := connector.Query(ctx, dataconnector.QueryRequest{
		SQL:              "SELECT count(*)::bigint AS prepared_statements FROM pg_prepared_statements",
		StatementTimeout: 30 * time.Second, MaxRows: 1,
	})
	if err != nil || prepared.Truncated || prepared.RowCount != 1 || len(prepared.Columns) != 1 ||
		prepared.Columns[0].Name != "prepared_statements" || len(prepared.Rows) != 1 || len(prepared.Rows[0]) != 1 {
		return result, errors.New("verify complete Catalog attestation prepared-statement state")
	}
	count, ok := prepared.Rows[0][0].(int64)
	if !ok || count != 0 {
		return result, errors.New("complete Catalog attestation created a prepared statement")
	}
	return CatalogAttestation{Datasource: attestation, ExpectedSchemaEntries: built.Count,
		ExpectedSchemaListSHA256: built.Digest, QueryExecMode: "simple_protocol",
		PreparedStatementCount: count}, nil
}

func catalogSimpleProtocolDSN(dsn string) (string, error) {
	trimmed := strings.TrimSpace(dsn)
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", err
		}
		query := parsed.Query()
		query.Set("default_query_exec_mode", "simple_protocol")
		query.Set("statement_cache_capacity", "0")
		query.Set("description_cache_capacity", "0")
		parsed.RawQuery = query.Encode()
		trimmed = parsed.String()
	} else {
		trimmed += " default_query_exec_mode=simple_protocol statement_cache_capacity=0 description_cache_capacity=0"
	}
	config, err := pgx.ParseConfig(trimmed)
	if err != nil || config.DefaultQueryExecMode != pgx.QueryExecModeSimpleProtocol ||
		config.StatementCacheCapacity != 0 || config.DescriptionCacheCapacity != 0 {
		return "", errors.New("DSN does not enforce pgx simple protocol")
	}
	return trimmed, nil
}

func generatedSHA256(value string) bool {
	return validSHA256(value) && value != strings.Repeat(value[:1], 64)
}

func mergeCatalogModel(baseBytes, approvedScaleBytes []byte, schemaSHA256 string) (*catalog.Catalog, *catalog.Catalog, error) {
	if sha256Hex(approvedScaleBytes) != approvedC2ScaleCatalogSHA256 {
		return nil, nil, errors.New("Scale Catalog bytes differ from the exact C2-approved companion")
	}
	base, err := catalog.Parse(baseBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse base Catalog: %w", err)
	}
	scale, err := catalog.Parse(approvedScaleBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse approved Scale Catalog: %w", err)
	}
	if err := validateCatalogMergeInputs(base, scale); err != nil {
		return nil, nil, err
	}

	merged := *base
	merged.SHA256 = ""
	merged.CatalogVersion = CatalogCandidateVersion
	merged.Sources = append([]catalog.Source(nil), base.Sources...)
	merged.Sources[0].SchemaDigest = schemaSHA256
	merged.SnapshotPublications = append(append([]catalog.SnapshotPublication(nil), base.SnapshotPublications...),
		scale.SnapshotPublications[0])
	merged.Products = append(append([]catalog.Product(nil), base.Products...), scale.Products[0])
	merged.Scopes = append([]catalog.Scope(nil), base.Scopes...)
	merged.ApprovalRoutes = append([]catalog.ApprovalRoute(nil), base.ApprovalRoutes...)
	scaleRoute := scale.ApprovalRoutes[0]
	scaleRoute.Products = []string{scale.Products[0].Name}
	merged.ApprovalRoutes = append(merged.ApprovalRoutes, scaleRoute)
	merged.BudgetProfiles = append(append([]catalog.BudgetProfile(nil), base.BudgetProfiles...), scale.BudgetProfiles[0])
	if err := merged.Validate(); err != nil {
		return nil, nil, fmt.Errorf("validate complete Catalog candidate: %w", err)
	}
	return &merged, scale, nil
}

func validateCatalogMergeInputs(base, scale *catalog.Catalog) error {
	if base == nil || scale == nil || len(base.Sources) != 1 || len(scale.Sources) != 1 ||
		len(scale.SnapshotPublications) != 1 || len(scale.Products) != 1 || len(scale.Scopes) != 1 ||
		len(scale.ApprovalRoutes) != 1 || len(scale.BudgetProfiles) != 1 {
		return errors.New("approved Scale Catalog is not the exact single-Product merge input")
	}
	left, right := base.Sources[0], scale.Sources[0]
	left.SchemaDigest, right.SchemaDigest = "", ""
	if !reflect.DeepEqual(left, right) {
		return errors.New("base and approved Scale Catalogs identify different datasources")
	}
	if scale.Products[0].Name != "final_v5_exposure_scale" ||
		scale.SnapshotPublications[0].Name != "final-v5-exposure-scale-v1" ||
		scale.Scopes[0].Name != "partition_key" || scale.BudgetProfiles[0].Name != "final-v5-exposure-scale-review-v1" {
		return errors.New("approved Scale Catalog material is outside the fixed E1 closure")
	}
	for _, product := range base.Products {
		if product.Name == scale.Products[0].Name {
			return errors.New("base Catalog already contains the approved Scale Product")
		}
	}
	for _, publication := range base.SnapshotPublications {
		if publication.Name == scale.SnapshotPublications[0].Name {
			return errors.New("base Catalog already contains the approved Scale publication")
		}
	}
	foundScope := false
	for _, scope := range base.Scopes {
		if scope.Name == scale.Scopes[0].Name {
			foundScope = scope.Type == scale.Scopes[0].Type && reflect.DeepEqual(scope.AllowedValues, scale.Scopes[0].AllowedValues)
		}
	}
	if !foundScope {
		return errors.New("base Catalog partition scope differs from approved Scale material")
	}
	for _, profile := range base.BudgetProfiles {
		if profile.Name == scale.BudgetProfiles[0].Name {
			return errors.New("base Catalog already contains the Scale review budget")
		}
	}
	return nil
}

func containsExactScaleMaterial(merged, scale *catalog.Catalog) bool {
	if merged == nil || scale == nil {
		return false
	}
	// YAML omitempty normalizes an explicitly authored empty sequence to nil.
	// Compare the parsed candidate with the approved Catalog after that same
	// schema-preserving round trip; the approval validator separately anchors
	// the exact source bytes.
	normalizedBytes, err := yaml.Marshal(scale)
	if err != nil {
		return false
	}
	normalizedScale, err := catalog.Parse(normalizedBytes)
	if err != nil {
		return false
	}
	var product *catalog.Product
	for index := range merged.Products {
		if merged.Products[index].Name == scale.Products[0].Name {
			product = &merged.Products[index]
			break
		}
	}
	var publication *catalog.SnapshotPublication
	for index := range merged.SnapshotPublications {
		if merged.SnapshotPublications[index].Name == scale.SnapshotPublications[0].Name {
			publication = &merged.SnapshotPublications[index]
			break
		}
	}
	return product != nil && publication != nil && reflect.DeepEqual(*product, normalizedScale.Products[0]) &&
		reflect.DeepEqual(*publication, normalizedScale.SnapshotPublications[0])
}
