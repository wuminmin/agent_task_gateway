package finalv5contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	contractfs "taskbound.local/agent-data-gateway/evaluation/final-v5-wsl2"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/domain"
)

// BridgeVersion identifies the Contract-to-Runtime Bridge that binds the
// frozen Contract Index to a running deployment. The Contract Index and the
// machine contracts are the single runtime source of truth: an Adapter derives
// its workload matrix, query text, bindings and expectations from this Bridge
// and never from a second hand-maintained table.
const BridgeVersion = "taskgate-final-v5-contract-runtime-bridge-v1"

// Contract-level entrypoint roles. These are contract vocabulary, not MCP tool
// names: the contract states which architectural entrypoint an arm must use,
// and the Bridge is the only place that maps a role onto a concrete runtime
// interface.
const (
	EntrypointBDGQuery  = "query"
	EntrypointBDGPlan   = "execute_plan"
	EntrypointDirectSQL = "postgresql_sql"
)

// PublicBDGTool is the only public MCP tool implementing the "query" role.
// execute_plan is intentionally absent from the advertised tool list, so an
// Artifact cell that resolves to it is not an OA-approved public BDG query.
const PublicBDGTool = "query_sql"

const (
	ArtifactExperimentID = "artifact"
	ArtifactWorkloadID   = "result-heavy"
)

const (
	indexContractPath         = "contracts/index-v1.json"
	baselineContractPath      = "contracts/baseline-v1.json"
	artifactContractPath      = "contracts/artifact-v1.json"
	scaleContractPath         = "contracts/scale-v1.json"
	normalizationContractPath = "contracts/result-normalization-v1.json"
	catalogCandidatePath      = "catalog/benchmark-contract-v1.yaml"
	datasetGeneratorPath      = "sql/datasets/benchmark-v1-generate.sql"
	datasetProbePath          = "sql/datasets/benchmark-v1-probe.sql"
	profileActivationPath     = "contracts/profile-activation-v1.json"
	protocolDocumentPath      = "protocol/protocol-v1.yaml"
	workloadManifestPath      = "protocol/workloads-v1.yaml"
)

// Runtime is a verified Contract Index bound for runtime use. Constructing one
// re-derives every index digest from the contract bytes the binary actually
// carries, so a contract edited without a matching index update fails closed
// here instead of silently changing what an Adapter executes.
type Runtime struct {
	files           fs.FS
	contractRelease string
	indexSHA256     string
	digests         map[string]string
	contents        map[string][]byte
	baseline        baselineDocument
	artifact        artifactDocument
}

// LoadRuntime verifies and binds the contracts embedded in this binary.
func LoadRuntime() (*Runtime, error) { return LoadRuntimeFS(contractfs.FS) }

// LoadRuntimeFS binds an arbitrary contract tree. Tests use it to prove that a
// tampered contract, index, or oracle manifest is rejected.
func LoadRuntimeFS(files fs.FS) (*Runtime, error) {
	if files == nil {
		return nil, errors.New("contract bridge requires a contract filesystem")
	}
	runtime := &Runtime{files: files, digests: map[string]string{}, contents: map[string][]byte{}}
	indexBytes, err := runtime.readContract(indexContractPath)
	if err != nil {
		return nil, fmt.Errorf("contract index: %w", err)
	}
	var index indexDocument
	if err := decodeStrictJSON(indexBytes, &index); err != nil {
		return nil, fmt.Errorf("contract index: %w", err)
	}
	if index.SchemaVersion != schemaVersion || index.IndexVersion != "taskgate-final-v5-contract-index-v1" ||
		index.Status != authorApproved || index.DigestStatus != "REVIEW_CANDIDATE" ||
		index.ExactGeneratedBytesFreezeStatus != notApproved {
		return nil, errors.New("contract index status/version header is invalid")
	}
	if !index.HashLockedReferences.ProtocolAndMatrixBytesUnchangedByContract {
		return nil, errors.New("contract index no longer hash-locks the protocol and workload matrix")
	}
	// An amended contract must identify its own release, so evidence can never
	// attribute corrected bytes to the release they superseded.
	if index.ContractRelease != contractReleaseV1 && index.ContractRelease != contractReleaseV11 &&
		index.ContractRelease != contractReleaseV12 && index.ContractRelease != contractReleaseV13 &&
		index.ContractRelease != contractReleaseV14 && index.ContractRelease != contractReleaseV15 &&
		index.ContractRelease != contractReleaseV16 && index.ContractRelease != contractReleaseV17 &&
		index.ContractRelease != contractReleaseV18 && index.ContractRelease != contractReleaseV19 &&
		index.ContractRelease != contractReleaseV110 {
		return nil, fmt.Errorf("contract index release %q is not reviewed", index.ContractRelease)
	}
	runtime.contractRelease = index.ContractRelease
	runtime.indexSHA256 = digestBytes(indexBytes)
	if err := runtime.revalidateDigests(index); err != nil {
		return nil, err
	}
	if err := runtime.revalidateHashLockedReferences(index); err != nil {
		return nil, err
	}
	if err := decodeStrictJSON(runtime.contents[baselineContractPath], &runtime.baseline); err != nil {
		return nil, fmt.Errorf("baseline contract: %w", err)
	}
	if err := decodeStrictJSON(runtime.contents[artifactContractPath], &runtime.artifact); err != nil {
		return nil, fmt.Errorf("artifact contract: %w", err)
	}
	if runtime.artifact.ExperimentID != ArtifactExperimentID || runtime.artifact.ProtocolProfile != ArtifactExperimentID ||
		runtime.artifact.ContractVersion != contractVersion || runtime.artifact.Status != authorApproved ||
		len(runtime.artifact.Cells) != runtime.artifact.ProtocolMatrix.ExpectedExpandedCellCount {
		return nil, errors.New("artifact contract identity or expanded cell count is invalid")
	}
	return runtime, nil
}

// revalidateDigests re-hashes every indexed contract. A missing entry, an
// unreadable path, a duplicate path, or any digest drift is fatal: the index
// is the runtime source of truth only while it still describes these bytes.
func (runtime *Runtime) revalidateDigests(index indexDocument) error {
	if len(index.Artifacts) == 0 {
		return errors.New("contract index declares no artifacts")
	}
	for _, artifact := range index.Artifacts {
		if !safeRelativePath(artifact.Path) || !sha256Pattern.MatchString(artifact.SHA256) || artifact.Kind == "" {
			return fmt.Errorf("contract index entry is invalid: kind=%q path=%q", artifact.Kind, artifact.Path)
		}
		if _, duplicate := runtime.digests[artifact.Path]; duplicate {
			return fmt.Errorf("contract index lists %s twice", artifact.Path)
		}
		value, err := runtime.readContract(artifact.Path)
		if err != nil {
			return fmt.Errorf("contract index path %s: %w", artifact.Path, err)
		}
		if actual := digestBytes(value); actual != artifact.SHA256 {
			return fmt.Errorf("contract index SHA-256 mismatch for %s: index=%s actual=%s",
				artifact.Path, artifact.SHA256, actual)
		}
		runtime.digests[artifact.Path] = artifact.SHA256
	}
	for _, required := range []string{baselineContractPath, artifactContractPath, normalizationContractPath,
		catalogCandidatePath, datasetGeneratorPath, datasetProbePath} {
		if _, indexed := runtime.digests[required]; !indexed {
			return fmt.Errorf("contract index omits the required runtime contract %s", required)
		}
	}
	return nil
}

func (runtime *Runtime) revalidateHashLockedReferences(index indexDocument) error {
	for path, expected := range map[string]string{
		protocolDocumentPath: index.HashLockedReferences.ProtocolSHA256,
		workloadManifestPath: index.HashLockedReferences.WorkloadManifestSHA256,
	} {
		value, err := runtime.readContract(path)
		if err != nil {
			return fmt.Errorf("hash-locked reference %s: %w", path, err)
		}
		if actual := digestBytes(value); actual != expected {
			return fmt.Errorf("hash-locked reference %s drifted: index=%s actual=%s", path, expected, actual)
		}
	}
	if index.HashLockedReferences.WorkloadManifestSHA256 != workloadManifestSHA {
		return errors.New("contract index hash-locked workload reference drifted")
	}
	return nil
}

func (runtime *Runtime) readContract(name string) ([]byte, error) {
	if cached, ok := runtime.contents[name]; ok {
		return cached, nil
	}
	if !safeRelativePath(name) {
		return nil, errors.New("contract path is not a safe relative path")
	}
	value, err := fs.ReadFile(runtime.files, name)
	if err != nil {
		return nil, err
	}
	runtime.contents[name] = value
	return value, nil
}

// ContractRelease is the reviewed release these exact contract bytes belong
// to. Evidence records it so an amended contract is never reported as the
// release it superseded.
func (runtime *Runtime) ContractRelease() string { return runtime.contractRelease }

// IndexSHA256 is the digest of the exact Contract Index bytes this runtime
// bound. Evidence records it so a sample can be tied back to one contract set.
func (runtime *Runtime) IndexSHA256() string { return runtime.indexSHA256 }

// ContractSHA256 returns the revalidated index digest for one contract path.
func (runtime *Runtime) ContractSHA256(contractPath string) (string, error) {
	digest, indexed := runtime.digests[contractPath]
	if !indexed {
		return "", fmt.Errorf("contract index does not cover %s", contractPath)
	}
	return digest, nil
}

// ContractBytes returns the verified bytes of one indexed contract.
func (runtime *Runtime) ContractBytes(contractPath string) ([]byte, error) {
	if _, indexed := runtime.digests[contractPath]; !indexed {
		return nil, fmt.Errorf("contract index does not cover %s", contractPath)
	}
	value, err := runtime.readContract(contractPath)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), value...), nil
}

// DatasetProbeSQL returns the contract-indexed deployment probe. Its output is
// a live deployment fingerprint recorded as an observation; it is never an
// independent logical oracle for a measured result.
func (runtime *Runtime) DatasetProbeSQL() (string, error) {
	if _, indexed := runtime.digests[datasetProbePath]; !indexed {
		return "", errors.New("contract index does not cover the dataset probe")
	}
	value, err := runtime.readContract(datasetProbePath)
	if err != nil {
		return "", err
	}
	// The probe is authored for psql; the leading meta-command is not SQL.
	return strings.TrimPrefix(string(value), "\\set ON_ERROR_STOP on\n"), nil
}

// DatasetProbeSourceSHA256 identifies the exact contract-indexed psql source,
// including its ON_ERROR_STOP preamble. It is the one SQL identity recorded by
// fresh-deployment, targeted, and publication-wide bindings; DatasetProbeSQL
// returns only the executable portion for pgx.
func (runtime *Runtime) DatasetProbeSourceSHA256() (string, error) {
	return runtime.ContractSHA256(datasetProbePath)
}

var benchmarkDatasetIdentityCache struct {
	sync.Once
	sha256 string
	err    error
}

// DatasetIdentitySHA256 returns the reviewed reference identity of the complete
// benchmark Dataset formula: all five Products, their
// namespaces and snapshots, ordered schemas, SQL types, collations and 815,000
// logical rows. It does not inspect a deployment; runtime callers must compare a
// full live typed stream with this reference. It is deliberately independent of
// DatasetProbeSQL, whose scalar result is only a deployment sanity observation.
func (runtime *Runtime) DatasetIdentitySHA256() (string, error) {
	if runtime == nil {
		return "", errors.New("contract runtime is nil")
	}
	if _, indexed := runtime.digests[datasetGeneratorPath]; !indexed {
		return "", errors.New("contract index does not cover the benchmark dataset generator")
	}
	benchmarkDatasetIdentityCache.Do(func() {
		summary, err := finalv5oracle.BenchmarkDatasetFingerprint()
		if err != nil {
			benchmarkDatasetIdentityCache.err = err
			return
		}
		if summary.DatasetSpecID != finalv5oracle.BenchmarkDatasetSpecID ||
			summary.GeneratorVersion != finalv5oracle.BenchmarkDatasetGeneratorVersion ||
			summary.ProductCount != 5 || summary.RowCount != 815_000 ||
			!sha256Pattern.MatchString(summary.SHA256) {
			benchmarkDatasetIdentityCache.err = errors.New("typed benchmark dataset identity is incomplete")
			return
		}
		benchmarkDatasetIdentityCache.sha256 = summary.SHA256
	})
	if benchmarkDatasetIdentityCache.err != nil {
		return "", fmt.Errorf("derive the typed benchmark dataset identity: %w", benchmarkDatasetIdentityCache.err)
	}
	return benchmarkDatasetIdentityCache.sha256, nil
}

// CellIdentity is the protocol coordinate of one preregistered cell.
type CellIdentity struct {
	ExperimentID string `json:"experiment_id"`
	WorkloadID   string `json:"workload_id"`
	Scale        string `json:"scale"`
	Mode         string `json:"mode"`
}

func (identity CellIdentity) String() string {
	return identity.ExperimentID + "/" + identity.WorkloadID + "/" + identity.Scale + "/" + identity.Mode
}

// ProtocolProfileCells expands one hash-locked public workload profile in
// manifest order. Each returned identity is complete and unique; malformed,
// missing, or drifted manifest contents fail closed.
func (runtime *Runtime) ProtocolProfileCells(profileID string) ([]CellIdentity, error) {
	value, err := runtime.readContract(workloadManifestPath)
	if err != nil {
		return nil, fmt.Errorf("workload protocol: %w", err)
	}
	if actual := digestBytes(value); actual != workloadManifestSHA {
		return nil, fmt.Errorf("hash-locked workload reference drifted: expected=%s actual=%s",
			workloadManifestSHA, actual)
	}
	protocol, err := decodeWorkloadProtocol(bytes.NewReader(value))
	if err != nil {
		return nil, fmt.Errorf("workload protocol: %w", err)
	}
	ordered, err := expandProtocolProfile(profileID, protocol)
	if err != nil {
		return nil, err
	}
	identities := make([]CellIdentity, 0, len(ordered))
	for _, cell := range ordered {
		identities = append(identities, CellIdentity{ExperimentID: profileID,
			WorkloadID: cell.Workload, Scale: cell.Scale, Mode: cell.Mode})
	}
	return identities, nil
}

type contractParameters struct {
	Rows       int64  `json:"rows"`
	Projection string `json:"projection"`
	// The remaining members are Baseline's frozen workload thresholds. Every
	// Baseline workload parameterises on exactly one of them except S5, which
	// pairs its key threshold with the overlap branch bound; Artifact and S6
	// parameterise on rows and projection instead and leave all four zero.
	OrderkeyMax          int64 `json:"orderkey_max"`
	MemberMax            int64 `json:"member_max"`
	FixedViewOrderkeyMax int64 `json:"fixed_view_orderkey_max"`
	OverlapBranchMax     int64 `json:"overlap_branch_max"`
}

type contractQuery struct {
	SpecID             string             `json:"spec_id"`
	BaselineIdentity   string             `json:"baseline_identity"`
	Parameters         contractParameters `json:"parameters"`
	Template           string             `json:"template"`
	TotalOrderRequired bool               `json:"total_order_required"`
	ResultOrdering     string             `json:"result_ordering"`
}

// contractArm covers both the Direct and BDG sections of a Baseline or
// Artifact cell; a section simply omits the members that do not apply to it.
type contractArm struct {
	Active           bool   `json:"active"`
	Role             string `json:"role"`
	Entrypoint       string `json:"entrypoint"`
	Template         string `json:"template"`
	CompleteDrain    bool   `json:"complete_drain"`
	ArtifactPipeline bool   `json:"artifact_pipeline"`
	ModeSemantics    string `json:"mode_semantics"`
}

type contractProduct struct {
	IDs                  []string `json:"ids"`
	CatalogBindingStatus string   `json:"catalog_binding_status"`
	// Scale control cells carry a deterministic member vector instead of a
	// Catalog Product, so they declare no Product closure at all.
	ControlFixture string `json:"control_fixture"`
}

type contractPublication struct {
	IDs           []string `json:"ids"`
	BindingStatus string   `json:"binding_status"`
}

type contractSetup struct {
	FreshDeployment         bool   `json:"fresh_deployment"`
	OAAndTaskProvisioning   string `json:"oa_and_task_provisioning"`
	PublicationProbeRequire bool   `json:"publication_probe_required"`
	Warmups                 int    `json:"warmups"`
	WarmupSeed              int64  `json:"warmup_seed"`
}

type contractMeasured struct {
	Samples             int    `json:"samples"`
	Start               string `json:"start"`
	End                 string `json:"end"`
	IncludeProvisioning bool   `json:"include_provisioning"`
	IncludeOracle       bool   `json:"include_oracle"`
	RetainFailedSamples bool   `json:"retain_failed_samples"`
}

type contractExpected struct {
	RowCount                       int64   `json:"row_count"`
	ColumnCount                    int     `json:"column_count"`
	NormalizedSchemaSHA256         *string `json:"normalized_schema_sha256"`
	CanonicalResultSHA256          *string `json:"canonical_result_sha256"`
	ReleaseCandidateCardinality    *int64  `json:"release_candidate_cardinality"`
	DependencyCandidateCardinality *int64  `json:"dependency_candidate_cardinality"`
	OutcomeCandidateCardinality    *int64  `json:"outcome_candidate_cardinality"`
	ReleaseCandidateSetSHA256      *string `json:"release_candidate_set_sha256"`
	DependencyCandidateSetSHA256   *string `json:"dependency_candidate_set_sha256"`
	OutcomeCandidateSetSHA256      *string `json:"outcome_candidate_set_sha256"`
	ParquetLogicalContentSHA256    *string `json:"parquet_logical_content_sha256"`
	RuntimeParquetSHA256           *string `json:"runtime_parquet_sha256"`
	RuntimeParquetBytes            *int64  `json:"runtime_parquet_bytes"`
	RuntimeObjectSHA256            *string `json:"runtime_object_sha256"`
	RuntimeObjectBytes             *int64  `json:"runtime_object_bytes"`
	DigestGenerationStatus         string  `json:"digest_generation_status"`
	DigestReviewStatus             string  `json:"digest_review_status"`
}

type contractOracle struct {
	Policy                          string  `json:"policy"`
	OracleManifestPath              string  `json:"oracle_manifest_path"`
	ManifestSHA256                  *string `json:"manifest_sha256"`
	GenerationStatus                string  `json:"generation_status"`
	ReviewStatus                    string  `json:"review_status"`
	RuntimeBinaryDigestsAreObserved bool    `json:"runtime_binary_digests_are_observations"`
}

// ArtifactCell is one decoded, frozen Artifact cell.
type ArtifactCell struct {
	Identity           CellIdentity
	SpecID             string
	BaselineIdentity   string
	ProductIDs         []string
	PublicationIDs     []string
	Rows               int64
	Projection         string
	ExpectedRows       int64
	ExpectedColumns    int
	TotalOrderRequired bool
	ResultOrdering     string
	QueryTemplate      string
	BDG                contractArm
	Direct             contractArm
	Setup              contractSetup
	Measured           contractMeasured
	OracleManifestPath string
	Negative           []string
}

// ArtifactCells returns every frozen Artifact cell in contract order.
func (runtime *Runtime) ArtifactCells() ([]ArtifactCell, error) {
	cells := make([]ArtifactCell, 0, len(runtime.artifact.Cells))
	for index := range runtime.artifact.Cells {
		decoded, err := runtime.decodeArtifactCell(runtime.artifact.Cells[index])
		if err != nil {
			return nil, err
		}
		cells = append(cells, decoded)
	}
	return cells, nil
}

// ArtifactCell looks one frozen cell up by its protocol coordinate.
func (runtime *Runtime) ArtifactCell(scale, mode string) (ArtifactCell, error) {
	cells, err := runtime.ArtifactCells()
	if err != nil {
		return ArtifactCell{}, err
	}
	for _, candidate := range cells {
		if candidate.Identity.Scale == scale && candidate.Identity.Mode == mode {
			return candidate, nil
		}
	}
	return ArtifactCell{}, fmt.Errorf("artifact contract has no cell for scale=%q mode=%q", scale, mode)
}

func (runtime *Runtime) decodeArtifactCell(source cell) (ArtifactCell, error) {
	var (
		query       contractQuery
		bdg, direct contractArm
		product     contractProduct
		publication contractPublication
		setup       contractSetup
		measured    contractMeasured
		expected    contractExpected
		oracle      contractOracle
		negative    []string
	)
	for _, section := range []struct {
		name        string
		raw         json.RawMessage
		destination any
	}{
		{"query", source.Query, &query}, {"bdg", source.BDG, &bdg}, {"direct", source.Direct, &direct},
		{"product", source.Product, &product}, {"publication", source.Publication, &publication},
		{"setup", source.Setup, &setup}, {"measured", source.Measured, &measured},
		{"expected", source.Expected, &expected}, {"oracle", source.Oracle, &oracle},
		{"negative", source.Negative, &negative},
	} {
		if err := decodeStrictJSON(section.raw, section.destination); err != nil {
			return ArtifactCell{}, fmt.Errorf("artifact cell %s/%s %s section: %w",
				source.Scale, source.Mode, section.name, err)
		}
	}
	identity := CellIdentity{ExperimentID: ArtifactExperimentID, WorkloadID: source.Workload,
		Scale: source.Scale, Mode: source.Mode}
	if source.Workload != ArtifactWorkloadID {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s declares workload %q", identity, source.Workload)
	}
	// A frozen Artifact cell must still be unbound and ungenerated: this Bridge
	// binds Product/Publication/oracle at runtime and never treats a contract
	// placeholder as a generated, reviewed value.
	if expected.NormalizedSchemaSHA256 != nil || expected.CanonicalResultSHA256 != nil ||
		expected.RuntimeParquetSHA256 != nil || expected.RuntimeObjectSHA256 != nil ||
		expected.DigestGenerationStatus != notGenerated || expected.DigestReviewStatus != notApproved {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s carries generated digests that contract v1 must not hold", identity)
	}
	if !oracle.RuntimeBinaryDigestsAreObserved {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s no longer treats runtime binary digests as observations", identity)
	}
	if len(product.IDs) != 1 || len(publication.IDs) != 1 {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s must bind exactly one Product and Publication", identity)
	}
	if !bdg.Active || !bdg.CompleteDrain || !bdg.ArtifactPipeline || bdg.Entrypoint != EntrypointBDGQuery {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s BDG arm is not a complete-drain artifact query", identity)
	}
	if !direct.CompleteDrain || direct.Template == "" {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s omits a complete-drain Direct equivalence template", identity)
	}
	if query.Template != bdg.Template {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s query template differs from its BDG template", identity)
	}
	if !query.TotalOrderRequired || query.ResultOrdering != "query_order_v1" {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s does not require a total query order", identity)
	}
	if expected.RowCount != query.Parameters.Rows {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s row parameter differs from its expected row count", identity)
	}
	if expected.ColumnCount != projectionColumns(query.Parameters.Projection) {
		return ArtifactCell{}, fmt.Errorf("artifact cell %s projection differs from its expected column count", identity)
	}
	if err := runtime.assertBaselineS6Identity(identity, query, direct); err != nil {
		return ArtifactCell{}, err
	}
	return ArtifactCell{
		Identity: identity, SpecID: query.SpecID, BaselineIdentity: query.BaselineIdentity,
		ProductIDs: product.IDs, PublicationIDs: publication.IDs,
		Rows: query.Parameters.Rows, Projection: query.Parameters.Projection,
		ExpectedRows: expected.RowCount, ExpectedColumns: expected.ColumnCount,
		TotalOrderRequired: query.TotalOrderRequired, ResultOrdering: query.ResultOrdering,
		QueryTemplate: query.Template, BDG: bdg, Direct: direct, Setup: setup, Measured: measured,
		OracleManifestPath: oracle.OracleManifestPath, Negative: negative,
	}, nil
}

// assertBaselineS6Identity enforces the contract identity rule at runtime: an
// Artifact cell has its own spec identity but must execute byte-identical
// Baseline S6 query templates against the same Product and Publication.
func (runtime *Runtime) assertBaselineS6Identity(identity CellIdentity, query contractQuery, direct contractArm) error {
	wantBaseline := "baseline/S6/" + identity.Scale + "/" + identity.Mode
	if query.BaselineIdentity != wantBaseline {
		return fmt.Errorf("artifact cell %s claims baseline identity %q", identity, query.BaselineIdentity)
	}
	for index := range runtime.baseline.Cells {
		source := runtime.baseline.Cells[index]
		if source.Workload != "S6" || source.Scale != identity.Scale {
			continue
		}
		var baselineQuery contractQuery
		var baselineArm contractArm
		if err := decodeStrictJSON(source.Query, &baselineQuery); err != nil {
			return fmt.Errorf("baseline S6 %s query section: %w", source.Scale, err)
		}
		section := source.BDG
		template := query.Template
		if source.Mode == "direct" {
			section, template = source.Direct, direct.Template
		}
		if err := decodeStrictJSON(section, &baselineArm); err != nil {
			return fmt.Errorf("baseline S6 %s arm section: %w", source.Scale, err)
		}
		if baselineArm.Template != template || baselineQuery.Parameters != query.Parameters {
			return fmt.Errorf("artifact cell %s is not identical to baseline S6 %s/%s",
				identity, source.Scale, source.Mode)
		}
	}
	return nil
}

func projectionColumns(projection string) int {
	switch projection {
	case "x4":
		return 4
	case "x16":
		return 16
	default:
		return 0
	}
}

// RenderedQuery is one contract query template bound to its frozen parameters.
type RenderedQuery struct {
	Role           string              `json:"role"`
	Entrypoint     string              `json:"entrypoint"`
	PublicTool     string              `json:"public_tool,omitempty"`
	TemplatePath   string              `json:"template_path"`
	TemplateSHA256 string              `json:"template_sha256"`
	SQL            string              `json:"-"`
	SQLSHA256      string              `json:"sql_sha256"`
	Parameters     []RenderedParameter `json:"parameters"`
}

// RenderedParameter records how one positional placeholder became a literal.
type RenderedParameter struct {
	Ordinal int    `json:"ordinal"`
	Name    string `json:"name"`
	SQLType string `json:"sql_type"`
	Literal string `json:"literal"`
}

// QueryContract is the complete, contract-derived query pair for one cell.
type QueryContract struct {
	Cell                CellIdentity                 `json:"cell"`
	Rows                int64                        `json:"rows"`
	Columns             int                          `json:"columns"`
	BDG                 RenderedQuery                `json:"bdg"`
	Direct              RenderedQuery                `json:"direct"`
	NormalizationSHA256 string                       `json:"normalization_sha256"`
	Schema              []finalv5oracle.ResultColumn `json:"schema"`
}

// QueryContract loads and renders both arms of one Artifact cell. The rendered
// BDG text is the exact string handed to the public MCP tool, and the Direct
// text is the exact string executed against PostgreSQL, so their digests are
// evidence rather than description.
func (runtime *Runtime) QueryContract(target ArtifactCell) (QueryContract, error) {
	normalization, err := runtime.ContractSHA256(normalizationContractPath)
	if err != nil {
		return QueryContract{}, err
	}
	schema, err := finalv5oracle.ArtifactSchema(target.ExpectedColumns)
	if err != nil {
		return QueryContract{}, err
	}
	bdg, err := runtime.renderQuery("bdg", target.BDG.Entrypoint, target.QueryTemplate, target.Rows)
	if err != nil {
		return QueryContract{}, err
	}
	bdg.PublicTool = PublicBDGTool
	direct, err := runtime.renderQuery("direct", EntrypointDirectSQL, target.Direct.Template, target.Rows)
	if err != nil {
		return QueryContract{}, err
	}
	return QueryContract{Cell: target.Identity, Rows: target.Rows, Columns: target.ExpectedColumns,
		BDG: bdg, Direct: direct, NormalizationSHA256: normalization, Schema: schema}, nil
}

func (runtime *Runtime) renderQuery(role, entrypoint, templatePath string, rows int64) (RenderedQuery, error) {
	switch {
	case role == "bdg" && entrypoint != EntrypointBDGQuery:
		// execute_plan is not advertised to ordinary Agents, so it cannot be
		// the OA-approved public path an Artifact cell must exercise.
		return RenderedQuery{}, fmt.Errorf("artifact BDG entrypoint %q is not the public query entrypoint", entrypoint)
	case role == "direct" && entrypoint != EntrypointDirectSQL:
		return RenderedQuery{}, fmt.Errorf("artifact Direct entrypoint %q is not PostgreSQL SQL", entrypoint)
	}
	digest, err := runtime.ContractSHA256(templatePath)
	if err != nil {
		return RenderedQuery{}, err
	}
	template, err := runtime.readContract(templatePath)
	if err != nil {
		return RenderedQuery{}, err
	}
	rendered, parameters, err := renderPositionalInt64(string(template), rows)
	if err != nil {
		return RenderedQuery{}, fmt.Errorf("%s template %s: %w", role, templatePath, err)
	}
	return RenderedQuery{Role: role, Entrypoint: entrypoint, TemplatePath: templatePath,
		TemplateSHA256: digest, SQL: rendered, SQLSHA256: digestBytes([]byte(rendered)),
		Parameters: parameters}, nil
}

// renderPositionalInt64 substitutes the single frozen $1 row parameter. Only a
// canonical, non-negative base-10 int64 is emitted, and any other placeholder
// ordinal is rejected so a template can never smuggle an unbound literal into
// a measured query.
func renderPositionalInt64(template string, rows int64) (string, []RenderedParameter, error) {
	if rows < 0 {
		return "", nil, errors.New("row parameter must be non-negative")
	}
	literal := strconv.FormatInt(rows, 10)
	var output strings.Builder
	occurrences := 0
	for index := 0; index < len(template); {
		character := template[index]
		if character != '$' {
			output.WriteByte(character)
			index++
			continue
		}
		end := index + 1
		for end < len(template) && template[end] >= '0' && template[end] <= '9' {
			end++
		}
		if end == index+1 {
			return "", nil, errors.New("template contains a bare $ that is not a positional parameter")
		}
		ordinal, err := strconv.Atoi(template[index+1 : end])
		if err != nil || ordinal != 1 {
			return "", nil, fmt.Errorf("template uses unsupported positional parameter %s", template[index:end])
		}
		output.WriteString(literal)
		occurrences++
		index = end
	}
	if occurrences != 1 {
		return "", nil, fmt.Errorf("template must bind $1 exactly once, found %d", occurrences)
	}
	return output.String(), []RenderedParameter{{Ordinal: 1, Name: "rows",
		SQLType: "nonnegative_int64", Literal: literal}}, nil
}

// LiveDeployment is what a running deployment actually exposes. The Bridge
// never assumes these values: a caller reads them from the live Gateway,
// Catalog and Business PostgreSQL and hands them in for binding.
type LiveDeployment struct {
	CatalogPath        string
	CatalogSHA256      string
	DatasetSHA256      string
	DatasetProbeSHA256 string
}

// Binding is one Artifact cell bound to a live deployment.
type Binding struct {
	Cell                   CellIdentity                 `json:"cell"`
	ProductID              string                       `json:"product_id"`
	PublicationID          string                       `json:"publication_id"`
	ReportingView          string                       `json:"reporting_view"`
	SourceNamespace        string                       `json:"source_namespace"`
	Snapshot               string                       `json:"snapshot"`
	OrdinalSidecar         string                       `json:"ordinal_sidecar"`
	SidecarDigest          string                       `json:"sidecar_digest"`
	DictionaryDigest       string                       `json:"dictionary_digest"`
	ManifestDigest         string                       `json:"manifest_digest"`
	Scopes                 map[string][]string          `json:"scopes"`
	Columns                []string                     `json:"columns"`
	Schema                 []finalv5oracle.ResultColumn `json:"schema"`
	BudgetProfile          string                       `json:"budget_profile"`
	MaxRows                int64                        `json:"max_rows"`
	MaxReleaseFacts        int64                        `json:"max_release_facts"`
	CatalogSHA256          string                       `json:"catalog_sha256"`
	CatalogCandidateSHA256 string                       `json:"catalog_candidate_sha256"`
	DatasetGeneratorSHA256 string                       `json:"dataset_generator_sha256"`
	DatasetSHA256          string                       `json:"dataset_sha256"`
	DatasetProbeSHA256     string                       `json:"dataset_probe_sha256"`
	IndexSHA256            string                       `json:"contract_index_sha256"`
}

// SHA256 identifies the complete public Contract-Bridge deployment record for
// one Artifact cell. The record binds the cell/result contract to the live
// Catalog, full typed Dataset and independent scalar probe without importing
// the private Scale/ProvSQL adapter section deleted from Artifact by Decision 19.
func (binding Binding) SHA256() (string, error) {
	payload, err := json.Marshal(binding)
	if err != nil {
		return "", fmt.Errorf("encode public Artifact deployment binding: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

// BindDeployment binds one frozen cell to the Catalog the Gateway is actually
// running. It fails closed when the live Catalog is missing the contract
// Product or Publication, when the live Product drifted from the reviewed
// candidate, when a placeholder digest is still installed, or when the
// approved budget profile cannot carry the cell's rows and release Facts.
func (runtime *Runtime) BindDeployment(target ArtifactCell, live LiveDeployment) (Binding, error) {
	if !sha256Pattern.MatchString(live.CatalogSHA256) || !sha256Pattern.MatchString(live.DatasetSHA256) ||
		!sha256Pattern.MatchString(live.DatasetProbeSHA256) {
		return Binding{}, errors.New("deployment binding digests are not SHA-256")
	}
	datasetIdentity, err := runtime.DatasetIdentitySHA256()
	if err != nil {
		return Binding{}, err
	}
	if live.DatasetSHA256 != datasetIdentity {
		return Binding{}, errors.New("bound typed Dataset identity differs from the reviewed benchmark Dataset formula")
	}
	liveCatalog, err := catalog.Load(live.CatalogPath)
	if err != nil {
		return Binding{}, fmt.Errorf("load live catalog: %w", err)
	}
	candidateBytes, err := runtime.ContractBytes(catalogCandidatePath)
	if err != nil {
		return Binding{}, err
	}
	candidate, err := catalog.Parse(candidateBytes)
	if err != nil {
		return Binding{}, fmt.Errorf("parse reviewed catalog candidate: %w", err)
	}
	productID, publicationID := target.ProductIDs[0], target.PublicationIDs[0]
	liveProduct, found := lookupProduct(liveCatalog, productID)
	if !found {
		return Binding{}, fmt.Errorf("live catalog does not publish Product %q", productID)
	}
	candidateProduct, found := lookupProduct(candidate, productID)
	if !found {
		return Binding{}, fmt.Errorf("reviewed catalog candidate does not define Product %q", productID)
	}
	if err := compareProductIdentity(candidateProduct, liveProduct); err != nil {
		return Binding{}, err
	}
	if liveProduct.SnapshotPublication != publicationID {
		return Binding{}, fmt.Errorf("live Product %q binds Publication %q, contract requires %q",
			productID, liveProduct.SnapshotPublication, publicationID)
	}
	publication, found := lookupPublication(liveCatalog, publicationID)
	if !found {
		return Binding{}, fmt.Errorf("live catalog does not declare Publication %q", publicationID)
	}
	if err := validateLivePublication(publication, liveProduct); err != nil {
		return Binding{}, err
	}
	schema, err := finalv5oracle.ArtifactSchema(target.ExpectedColumns)
	if err != nil {
		return Binding{}, err
	}
	columns, err := bindProjection(liveProduct, schema)
	if err != nil {
		return Binding{}, err
	}
	policy, err := liveCatalog.ResolveTaskPolicy([]string{productID})
	if err != nil {
		return Binding{}, fmt.Errorf("resolve live task policy for %q: %w", productID, err)
	}
	if err := validateBudgetCeiling(target, policy); err != nil {
		return Binding{}, err
	}
	scopes, err := bindScopes(liveCatalog, liveProduct)
	if err != nil {
		return Binding{}, err
	}
	candidateDigest, err := runtime.ContractSHA256(catalogCandidatePath)
	if err != nil {
		return Binding{}, err
	}
	generatorDigest, err := runtime.ContractSHA256(datasetGeneratorPath)
	if err != nil {
		return Binding{}, err
	}
	return Binding{
		Cell: target.Identity, ProductID: productID, PublicationID: publicationID,
		ReportingView: liveProduct.ReportingView, SourceNamespace: publication.SourceNamespace,
		Snapshot: publication.Snapshot, OrdinalSidecar: publication.OrdinalSidecar,
		SidecarDigest: publication.SidecarDigest, DictionaryDigest: publication.DictionaryDigest,
		ManifestDigest: publication.ManifestDigest, Scopes: scopes, Columns: columns, Schema: schema,
		BudgetProfile: policy.BudgetProfile, MaxRows: policy.Budget.MaxRows,
		MaxReleaseFacts: policy.Budget.MaxReleaseFacts, CatalogSHA256: live.CatalogSHA256,
		CatalogCandidateSHA256: candidateDigest, DatasetGeneratorSHA256: generatorDigest,
		DatasetSHA256: live.DatasetSHA256, DatasetProbeSHA256: live.DatasetProbeSHA256,
		IndexSHA256: runtime.indexSHA256,
	}, nil
}

func lookupProduct(source *catalog.Catalog, name string) (catalog.Product, bool) {
	for _, product := range source.Products {
		if product.Name == name {
			return product, true
		}
	}
	return catalog.Product{}, false
}

func lookupPublication(source *catalog.Catalog, name string) (catalog.SnapshotPublication, bool) {
	for _, publication := range source.SnapshotPublications {
		if publication.Name == name {
			return publication, true
		}
	}
	return catalog.SnapshotPublication{}, false
}

// compareProductIdentity rejects a live Product that drifted from the reviewed
// candidate. Digests are deliberately excluded: the candidate carries
// fail-closed zero sentinels and only a live deployment can generate the real
// sidecar, dictionary, and manifest digests.
func compareProductIdentity(candidate, live catalog.Product) error {
	if candidate.ReportingView != live.ReportingView || candidate.Source != live.Source ||
		candidate.Sensitivity != live.Sensitivity || candidate.Snapshot != live.Snapshot ||
		candidate.SnapshotPublication != live.SnapshotPublication ||
		candidate.FactNamespace != live.FactNamespace || candidate.StableRelationRole != live.StableRelationRole ||
		!equalStrings(candidate.EntityKey, live.EntityKey) || !equalStrings(candidate.Scopes, live.Scopes) ||
		!equalStrings(candidate.AllowedFunctions, live.AllowedFunctions) ||
		!equalStrings(candidate.AllowedOperators, live.AllowedOperators) ||
		!equalStrings(candidate.AllowedAggregates, live.AllowedAggregates) ||
		len(candidate.Fields) != len(live.Fields) {
		return fmt.Errorf("live Product %q differs from the reviewed catalog candidate", candidate.Name)
	}
	for index := range candidate.Fields {
		want, actual := candidate.Fields[index], live.Fields[index]
		if want.Name != actual.Name || want.Type != actual.Type || want.Collation != actual.Collation ||
			want.CollationVersion != actual.CollationVersion || want.Sensitivity != actual.Sensitivity {
			return fmt.Errorf("live Product %q field %q differs from the reviewed catalog candidate",
				candidate.Name, want.Name)
		}
	}
	return nil
}

// validateLivePublication rejects the deliberate all-zero candidate sentinels.
// A running deployment must install generated digests before a measured cell
// may execute against it.
func validateLivePublication(publication catalog.SnapshotPublication, product catalog.Product) error {
	if publication.Snapshot != product.Snapshot {
		return fmt.Errorf("Publication %q snapshot %q differs from Product snapshot %q",
			publication.Name, publication.Snapshot, product.Snapshot)
	}
	if publication.OrdinalSidecar == "" {
		return fmt.Errorf("Publication %q has no ordinal sidecar", publication.Name)
	}
	for name, digest := range map[string]string{
		"sidecar_digest": publication.SidecarDigest, "dictionary_digest": publication.DictionaryDigest,
		"manifest_digest": publication.ManifestDigest,
	} {
		if !sha256Pattern.MatchString(digest) {
			return fmt.Errorf("Publication %q %s is not SHA-256", publication.Name, name)
		}
		if digest == strings.Repeat("0", 64) {
			return fmt.Errorf("Publication %q %s is still the fail-closed candidate sentinel",
				publication.Name, name)
		}
	}
	return nil
}

// bindProjection maps the contract's ordered result schema onto live Catalog
// fields. Approving a field that the Catalog does not carry, or carrying it
// with a different type or collation, is fail-closed.
func bindProjection(product catalog.Product, schema []finalv5oracle.ResultColumn) ([]string, error) {
	fields := make(map[string]catalog.Field, len(product.Fields))
	for _, field := range product.Fields {
		fields[field.Name] = field
	}
	columns := make([]string, 0, len(schema))
	for _, column := range schema {
		field, present := fields[column.Name]
		if !present {
			return nil, fmt.Errorf("live Product %q does not publish contract field %q", product.Name, column.Name)
		}
		declared, err := finalv5oracle.NormalizeSQLType(field.Type)
		if err != nil {
			return nil, fmt.Errorf("live Product %q field %q: %w", product.Name, column.Name, err)
		}
		if declared != column.Type {
			return nil, fmt.Errorf("live Product %q field %q is %q, contract requires %q",
				product.Name, column.Name, declared, column.Type)
		}
		if declared == finalv5oracle.SQLText &&
			(field.Collation != "en_US.utf8" || field.CollationVersion != "2.36") {
			return nil, fmt.Errorf("live Product %q field %q collation %q/%q is not the reviewed collation",
				product.Name, column.Name, field.Collation, field.CollationVersion)
		}
		columns = append(columns, column.Name)
	}
	return columns, nil
}

// validateBudgetCeiling proves the formal approval route can actually carry
// this cell. There is no test back door: a cell whose rows or release Facts
// exceed the approved production profile must fail rather than be waived.
func validateBudgetCeiling(target ArtifactCell, policy catalog.TaskPolicy) error {
	if policy.ApprovalRoute.Mode != domain.ApprovalModeManual || strings.TrimSpace(policy.ApprovalRoute.Approver) == "" {
		return fmt.Errorf("artifact cell %s route is not a manually approved OA route", target.Identity)
	}
	releaseFacts := target.ExpectedRows * int64(target.ExpectedColumns)
	switch {
	case policy.Budget.MaxRows < target.ExpectedRows:
		return fmt.Errorf("artifact cell %s needs %d rows; profile %q grants %d",
			target.Identity, target.ExpectedRows, policy.BudgetProfile, policy.Budget.MaxRows)
	case policy.Budget.MaxReleaseFacts < releaseFacts:
		return fmt.Errorf("artifact cell %s needs %d release Facts; profile %q grants %d",
			target.Identity, releaseFacts, policy.BudgetProfile, policy.Budget.MaxReleaseFacts)
	case policy.Budget.MaxQueries < 1:
		return fmt.Errorf("artifact cell %s profile %q grants no query", target.Identity, policy.BudgetProfile)
	}
	return nil
}

// bindScopes materialises the complete scope domain the contract query relies
// on. The frozen predicate enumerates every category value, so an incomplete
// live scope domain would silently change the result.
func bindScopes(source *catalog.Catalog, product catalog.Product) (map[string][]string, error) {
	scopes := make(map[string][]string, len(product.Scopes))
	for _, name := range product.Scopes {
		var declared *catalog.Scope
		for index := range source.Scopes {
			if source.Scopes[index].Name == name {
				declared = &source.Scopes[index]
				break
			}
		}
		if declared == nil {
			return nil, fmt.Errorf("live catalog omits scope %q required by Product %q", name, product.Name)
		}
		if declared.Type != "enum" || len(declared.AllowedValues) == 0 {
			return nil, fmt.Errorf("scope %q is not a complete enumerated domain", name)
		}
		scopes[name] = append([]string(nil), declared.AllowedValues...)
	}
	return scopes, nil
}

// OracleManifest loads and re-verifies one cell's independent Oracle Manifest.
// The manifest is bound to the Contract Index: its Spec digests must be the
// revalidated digests of the dataset generator, catalog candidate, query
// template and normalization contract this Bridge already verified.
func (runtime *Runtime) OracleManifest(target ArtifactCell) (finalv5oracle.OracleManifest, string, error) {
	if !safeRelativePath(target.OracleManifestPath) ||
		path.Dir(target.OracleManifestPath) != path.Join("oracle-manifests", ArtifactExperimentID,
			ArtifactWorkloadID, target.Identity.Scale) {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf("artifact cell %s oracle manifest path is not canonical",
			target.Identity)
	}
	value, err := runtime.readContract(target.OracleManifestPath)
	if err != nil {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf("oracle manifest %s: %w", target.OracleManifestPath, err)
	}
	manifest, err := finalv5oracle.DecodeManifest(value)
	if err != nil {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf("oracle manifest %s: %w", target.OracleManifestPath, err)
	}
	if manifest.ExperimentID != ArtifactExperimentID || manifest.WorkloadID != ArtifactWorkloadID ||
		manifest.Scale != target.Identity.Scale || manifest.Mode != target.Identity.Mode {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf("oracle manifest %s identifies a different cell",
			target.OracleManifestPath)
	}
	for name, pair := range map[string][2]string{
		"dataset_spec_sha256":       {manifest.DatasetSpecSHA256, runtime.digests[datasetGeneratorPath]},
		"catalog_spec_sha256":       {manifest.CatalogSpecSHA256, runtime.digests[catalogCandidatePath]},
		"query_spec_sha256":         {manifest.QuerySpecSHA256, runtime.digests[target.QueryTemplate]},
		"normalization_spec_sha256": {manifest.NormalizationSpecSHA256, runtime.digests[normalizationContractPath]},
	} {
		if pair[1] == "" || pair[0] != pair[1] {
			return finalv5oracle.OracleManifest{}, "", fmt.Errorf(
				"oracle manifest %s %s is not the indexed contract digest", target.OracleManifestPath, name)
		}
	}
	if manifest.Expected.RowCount == nil || *manifest.Expected.RowCount != target.ExpectedRows ||
		manifest.Expected.ColumnCount == nil || *manifest.Expected.ColumnCount != target.ExpectedColumns {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf("oracle manifest %s expects a different result shape",
			target.OracleManifestPath)
	}
	// A manifest stays canonical JSON even when one of its digests is edited,
	// so recompute the expectation from the independent oracle instead of
	// trusting the recorded bytes.
	expected, err := finalv5oracle.ArtifactResultSummary(target.ExpectedRows, target.ExpectedColumns)
	if err != nil {
		return finalv5oracle.OracleManifest{}, "", err
	}
	if manifest.Expected.NormalizedSchemaSHA256 != expected.NormalizedSchemaSHA256 ||
		manifest.Expected.CanonicalResultSHA256 != expected.CanonicalResultSHA256 {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf(
			"oracle manifest %s does not match the independently regenerated expectation", target.OracleManifestPath)
	}
	if manifest.Generation.Seed != finalv5oracle.ArtifactGeneratorSeed ||
		manifest.Generation.GeneratorVersion != finalv5oracle.ArtifactGeneratorVersion {
		return finalv5oracle.OracleManifest{}, "", fmt.Errorf("oracle manifest %s generator binding was mutated",
			target.OracleManifestPath)
	}
	digest, err := finalv5oracle.ManifestSHA256(manifest)
	if err != nil {
		return finalv5oracle.OracleManifest{}, "", err
	}
	return manifest, digest, nil
}

// ObservedResult is one completely drained logical result, normalized through
// the single shared normalizer. Direct and BDG results must be reduced by the
// same code path or their comparison would prove nothing.
type ObservedResult struct {
	Source  string                      `json:"source"`
	Summary finalv5oracle.ResultSummary `json:"summary"`
}

// NormalizeDirect reduces a completely drained PostgreSQL result. yield must
// emit every row exactly once, in query order.
func NormalizeDirect(schema []finalv5oracle.ResultColumn,
	drain func(func([]any) error) error) (ObservedResult, error) {
	return normalizeStream("direct", schema, drain)
}

// NormalizeBDG reduces a released Parquet artifact through the same normalizer.
// The Parquet binary digest is deliberately not part of this reduction: it is
// a runtime observation recorded elsewhere, never an independent logical oracle.
func NormalizeBDG(schema []finalv5oracle.ResultColumn, parquet parquetInput) (ObservedResult, error) {
	if parquet.Reader == nil || parquet.Size <= 0 {
		return ObservedResult{}, errors.New("released Parquet artifact is empty")
	}
	summary, err := finalv5oracle.CanonicalResultFromParquet(parquet.Reader, parquet.Size, schema)
	if err != nil {
		return ObservedResult{}, fmt.Errorf("normalize released Parquet: %w", err)
	}
	return ObservedResult{Source: "bdg", Summary: summary}, nil
}

// parquetInput is the released artifact under normalization.
type parquetInput struct {
	Reader interface {
		ReadAt([]byte, int64) (int, error)
	}
	Size int64
}

// ParquetInput builds the released-artifact input for NormalizeBDG.
func ParquetInput(reader interface {
	ReadAt([]byte, int64) (int, error)
}, size int64) parquetInput {
	return parquetInput{Reader: reader, Size: size}
}

func normalizeStream(source string, schema []finalv5oracle.ResultColumn,
	drain func(func([]any) error) error) (ObservedResult, error) {
	if drain == nil {
		return ObservedResult{}, errors.New("result drain is nil")
	}
	hasher, err := finalv5oracle.NewResultHasher(schema)
	if err != nil {
		return ObservedResult{}, err
	}
	if err := drain(hasher.WriteRow); err != nil {
		return ObservedResult{}, fmt.Errorf("%s complete drain: %w", source, err)
	}
	summary, err := hasher.Finalize()
	if err != nil {
		return ObservedResult{}, err
	}
	return ObservedResult{Source: source, Summary: summary}, nil
}

// ResultComparison is the full expected/actual verdict for one cell.
type ResultComparison struct {
	Cell                 CellIdentity `json:"cell"`
	ManifestSHA256       string       `json:"oracle_manifest_sha256"`
	ExpectedRows         int64        `json:"expected_rows"`
	ExpectedColumns      int          `json:"expected_columns"`
	ExpectedSchemaSHA256 string       `json:"expected_normalized_schema_sha256"`
	ExpectedResultSHA256 string       `json:"expected_canonical_result_sha256"`
	DirectRows           int64        `json:"direct_rows"`
	DirectColumns        int          `json:"direct_columns"`
	DirectSchemaSHA256   string       `json:"direct_normalized_schema_sha256"`
	DirectResultSHA256   string       `json:"direct_canonical_result_sha256"`
	BDGRows              int64        `json:"bdg_rows"`
	BDGColumns           int          `json:"bdg_columns"`
	BDGSchemaSHA256      string       `json:"bdg_normalized_schema_sha256"`
	BDGResultSHA256      string       `json:"bdg_canonical_result_sha256"`
	DirectMatchesBDG     bool         `json:"direct_matches_bdg"`
	DirectMatchesOracle  bool         `json:"direct_matches_oracle"`
	BDGMatchesOracle     bool         `json:"bdg_matches_oracle"`
}

// CompareResults is the fail-closed expected/actual comparison. It requires an
// exact row count, an exact column count, the normalized schema digest, the
// canonical logical-result digest, and Direct/BDG logical equality.
func (runtime *Runtime) CompareResults(target ArtifactCell, direct, bdg ObservedResult) (ResultComparison, error) {
	manifest, manifestDigest, err := runtime.OracleManifest(target)
	if err != nil {
		return ResultComparison{}, err
	}
	comparison := ResultComparison{
		Cell: target.Identity, ManifestSHA256: manifestDigest,
		ExpectedRows: *manifest.Expected.RowCount, ExpectedColumns: *manifest.Expected.ColumnCount,
		ExpectedSchemaSHA256: manifest.Expected.NormalizedSchemaSHA256,
		ExpectedResultSHA256: manifest.Expected.CanonicalResultSHA256,
		DirectRows:           direct.Summary.RowCount, DirectColumns: direct.Summary.ColumnCount,
		DirectSchemaSHA256: direct.Summary.NormalizedSchemaSHA256,
		DirectResultSHA256: direct.Summary.CanonicalResultSHA256,
		BDGRows:            bdg.Summary.RowCount, BDGColumns: bdg.Summary.ColumnCount,
		BDGSchemaSHA256: bdg.Summary.NormalizedSchemaSHA256,
		BDGResultSHA256: bdg.Summary.CanonicalResultSHA256,
	}
	if direct.Source != "direct" || bdg.Source != "bdg" {
		return comparison, errors.New("result comparison requires one Direct and one BDG observation")
	}
	for _, observed := range []ObservedResult{direct, bdg} {
		if err := compareToExpected(target.Identity, observed, manifest); err != nil {
			return comparison, err
		}
	}
	comparison.DirectMatchesOracle, comparison.BDGMatchesOracle = true, true
	if direct.Summary != bdg.Summary {
		return comparison, fmt.Errorf("artifact cell %s Direct and BDG logical results differ", target.Identity)
	}
	comparison.DirectMatchesBDG = true
	return comparison, nil
}

func compareToExpected(identity CellIdentity, observed ObservedResult,
	manifest finalv5oracle.OracleManifest) error {
	switch {
	case observed.Summary.RowCount != *manifest.Expected.RowCount:
		return fmt.Errorf("artifact cell %s %s row count is %d, oracle expects %d",
			identity, observed.Source, observed.Summary.RowCount, *manifest.Expected.RowCount)
	case observed.Summary.ColumnCount != *manifest.Expected.ColumnCount:
		return fmt.Errorf("artifact cell %s %s column count is %d, oracle expects %d",
			identity, observed.Source, observed.Summary.ColumnCount, *manifest.Expected.ColumnCount)
	case observed.Summary.NormalizedSchemaSHA256 != manifest.Expected.NormalizedSchemaSHA256:
		return fmt.Errorf("artifact cell %s %s normalized schema digest differs from the oracle",
			identity, observed.Source)
	case observed.Summary.CanonicalResultSHA256 != manifest.Expected.CanonicalResultSHA256:
		return fmt.Errorf("artifact cell %s %s canonical logical-result digest differs from the oracle",
			identity, observed.Source)
	}
	return nil
}

// VerifyProjectionPrefix proves the reviewed x4 schema is the exact leading
// projection of the reviewed x16 schema, so the narrow cells are a stable
// prefix of the wide cells rather than an unrelated result shape.
func (runtime *Runtime) VerifyProjectionPrefix() error {
	narrow, err := finalv5oracle.ArtifactSchema(4)
	if err != nil {
		return err
	}
	wide, err := finalv5oracle.ArtifactSchema(16)
	if err != nil {
		return err
	}
	if len(narrow) != 4 || len(wide) != 16 {
		return errors.New("reviewed artifact schema widths are not 4 and 16")
	}
	for index := range narrow {
		if narrow[index] != wide[index] {
			return fmt.Errorf("artifact column %d is not a stable x4/x16 prefix", index+1)
		}
	}
	cells, err := runtime.ArtifactCells()
	if err != nil {
		return err
	}
	rowsByProjection := map[string]map[int64]bool{}
	for _, target := range cells {
		if rowsByProjection[target.Projection] == nil {
			rowsByProjection[target.Projection] = map[int64]bool{}
		}
		rowsByProjection[target.Projection][target.ExpectedRows] = true
	}
	if len(rowsByProjection) != 2 || len(rowsByProjection["x4"]) != 3 ||
		!equalRowSets(rowsByProjection["x4"], rowsByProjection["x16"]) {
		return errors.New("artifact contract does not pair every row count across x4 and x16")
	}
	return nil
}

func equalRowSets(left, right map[int64]bool) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if !right[value] {
			return false
		}
	}
	return true
}

// CompletenessReport is the fail-closed profile coverage verdict.
type CompletenessReport struct {
	ExperimentID string         `json:"experiment_id"`
	Required     []CellIdentity `json:"required"`
	Implemented  []CellIdentity `json:"implemented"`
	Missing      []CellIdentity `json:"missing"`
	Unexpected   []CellIdentity `json:"unexpected"`
	Complete     bool           `json:"complete"`
}

// ArtifactRequirements is the preregistered Artifact matrix, derived from the
// contract rather than from a second hand-maintained table in the Adapter.
func (runtime *Runtime) ArtifactRequirements() ([]CellIdentity, error) {
	cells, err := runtime.ArtifactCells()
	if err != nil {
		return nil, err
	}
	required := make([]CellIdentity, 0, len(cells))
	for _, target := range cells {
		required = append(required, target.Identity)
	}
	return required, nil
}

// ArtifactCompleteness compares implemented cells against the frozen contract.
// Duplicates, unknown cells, and any missing cell all keep the profile
// incomplete: partial coverage never becomes a publication capability.
func (runtime *Runtime) ArtifactCompleteness(implemented []CellIdentity) (CompletenessReport, error) {
	required, err := runtime.ArtifactRequirements()
	if err != nil {
		return CompletenessReport{}, err
	}
	report := CompletenessReport{ExperimentID: ArtifactExperimentID, Required: required,
		Implemented: append([]CellIdentity(nil), implemented...)}
	requiredSet := make(map[CellIdentity]bool, len(required))
	for _, identity := range required {
		if requiredSet[identity] {
			return report, fmt.Errorf("artifact contract lists %s twice", identity)
		}
		requiredSet[identity] = true
	}
	seen := make(map[CellIdentity]bool, len(implemented))
	for _, identity := range implemented {
		if seen[identity] {
			report.Unexpected = append(report.Unexpected, identity)
			continue
		}
		seen[identity] = true
		if !requiredSet[identity] {
			report.Unexpected = append(report.Unexpected, identity)
		}
	}
	for _, identity := range required {
		if !seen[identity] {
			report.Missing = append(report.Missing, identity)
		}
	}
	sortIdentities(report.Missing)
	sortIdentities(report.Unexpected)
	report.Complete = len(report.Missing) == 0 && len(report.Unexpected) == 0
	return report, nil
}

func sortIdentities(values []CellIdentity) {
	sort.Slice(values, func(left, right int) bool {
		return values[left].String() < values[right].String()
	})
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// ContractCell is one preregistered cell of any contract experiment together
// with the Products its Query Contract requests.
type ContractCell struct {
	Identity CellIdentity `json:"identity"`
	Products []string     `json:"products"`
}

// ContractWorkloadCells returns every Baseline, Scale and Artifact cell with
// its requested Products. Deployment profiles are derived from this, so the
// contract remains the only place that says which Products a cell needs.
func (runtime *Runtime) ContractWorkloadCells() ([]ContractCell, error) {
	var scale scaleDocument
	if err := decodeStrictJSON(runtime.contents[scaleContractPath], &scale); err != nil {
		return nil, fmt.Errorf("scale contract: %w", err)
	}
	var cells []ContractCell
	for _, group := range []struct {
		experiment string
		source     []cell
	}{
		{"baseline", runtime.baseline.Cells},
		{"scale", scale.Cells},
		{ArtifactExperimentID, runtime.artifact.Cells},
	} {
		for index := range group.source {
			source := group.source[index]
			var product contractProduct
			if err := decodeStrictJSON(source.Product, &product); err != nil {
				return nil, fmt.Errorf("%s cell %s/%s product section: %w",
					group.experiment, source.Scale, source.Mode, err)
			}
			cells = append(cells, ContractCell{
				Identity: CellIdentity{ExperimentID: group.experiment, WorkloadID: source.Workload,
					Scale: source.Scale, Mode: source.Mode},
				Products: append([]string(nil), product.IDs...)})
		}
	}
	return cells, nil
}

// IndexedArtifact is one artifact the Contract Index names, exposed so a
// validator can iterate the contract rather than carry its own file list. A
// newly indexed artifact is then covered automatically.
type IndexedArtifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// IndexedArtifacts returns every artifact the Contract Index names, in index
// order. The digests are the reviewed ones the runtime already revalidated
// against the embedded bytes.
func (runtime *Runtime) IndexedArtifacts() ([]IndexedArtifact, error) {
	indexBytes, err := runtime.readContract(indexContractPath)
	if err != nil {
		return nil, err
	}
	var index indexDocument
	if err := decodeStrictJSON(indexBytes, &index); err != nil {
		return nil, fmt.Errorf("contract index: %w", err)
	}
	artifacts := make([]IndexedArtifact, 0, len(index.Artifacts))
	for _, artifact := range index.Artifacts {
		artifacts = append(artifacts, IndexedArtifact{Kind: artifact.Kind,
			Path: artifact.Path, SHA256: artifact.SHA256})
	}
	return artifacts, nil
}

// RenderIndexedTemplate renders one contract SQL template with the single
// frozen positional row parameter. It is the same renderer the measured path
// uses, so an executability check proves the exact bytes that would run.
func (runtime *Runtime) RenderIndexedTemplate(templatePath string, rows int64) (RenderedQuery, error) {
	digest, err := runtime.ContractSHA256(templatePath)
	if err != nil {
		return RenderedQuery{}, err
	}
	template, err := runtime.readContract(templatePath)
	if err != nil {
		return RenderedQuery{}, err
	}
	rendered, parameters, err := renderPositionalInt64(string(template), rows)
	if err != nil {
		return RenderedQuery{}, fmt.Errorf("template %s: %w", templatePath, err)
	}
	return RenderedQuery{TemplatePath: templatePath, TemplateSHA256: digest, SQL: rendered,
		SQLSHA256: digestBytes([]byte(rendered)), Parameters: parameters}, nil
}

// DatasetGeneratorSQL returns the contract-indexed benchmark generator bytes.
func (runtime *Runtime) DatasetGeneratorSQL() (string, error) {
	value, err := runtime.readContract(datasetGeneratorPath)
	if err != nil {
		return "", err
	}
	return string(value), nil
}
