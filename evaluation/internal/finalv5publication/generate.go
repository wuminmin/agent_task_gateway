package finalv5publication

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

const (
	ProvenanceVersion = "taskgate-final-v5-publication-binding-provenance-v1"
	GeneratorVersion  = "taskgate-final-v5-publication-binding-generator-v1"
	ReviewStatus      = "REVIEW_CANDIDATE"

	CatalogOutputName    = "catalog.yaml"
	BindingOutputName    = "publication-binding.json"
	ProvenanceOutputName = "publication-binding.provenance.json"

	publicationOracleMaxInMemoryMembers = 2_100_000
)

var publicationOutputNames = []string{CatalogOutputName, BindingOutputName, ProvenanceOutputName}

type GenerateOptions struct {
	RepositoryRoot  string
	OutputDirectory string
	ArtifactRoot    string
	BusinessDSN     string
}

type NamedIdentity struct {
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	SHA256 string `json:"sha256"`
}

type CellInput struct {
	Workload     string            `json:"workload"`
	Cell         string            `json:"cell"`
	TaskSHA256   string            `json:"task_sha256"`
	Identities   []NamedIdentity   `json:"identities"`
	OracleInputs []DigestReference `json:"oracle_inputs"`
}

type InputInventory struct {
	Approval             ApprovalEvidence      `json:"approval"`
	AuthorDecision       FileEvidence          `json:"author_decision"`
	BaseCatalog          FileEvidence          `json:"base_catalog"`
	ApprovedScaleCatalog FileEvidence          `json:"approved_scale_catalog"`
	Contract             contractInputEvidence `json:"contract"`
	Cells                []CellInput           `json:"cells"`
	CellCount            int                   `json:"cell_count"`
	ScaleCells           int                   `json:"scale_cells"`
	ArtifactCells        int                   `json:"artifact_cells"`
	ProvSQLCells         int                   `json:"provsql_cells"`
	CellInputSetSHA256   string                `json:"cell_input_set_sha256"`
}

type ScaleSetAlgebraCell struct {
	Scale                string            `json:"scale"`
	CandidateCardinality int64             `json:"candidate_cardinality"`
	CandidateSetSHA256   string            `json:"candidate_set_sha256"`
	ExistingCardinality  int64             `json:"existing_cardinality"`
	ExistingSetSHA256    string            `json:"existing_set_sha256"`
	OverlapCardinality   int64             `json:"overlap_cardinality"`
	OverlapSetSHA256     string            `json:"overlap_set_sha256"`
	NovelCardinality     int64             `json:"novel_cardinality"`
	NovelSetSHA256       string            `json:"novel_set_sha256"`
	UnionCardinality     int64             `json:"union_cardinality"`
	UnionSetSHA256       string            `json:"union_set_sha256"`
	OracleInputs         []DigestReference `json:"oracle_inputs"`
}

type SetAlgebraIdentity struct {
	Version               string                `json:"version"`
	LiveObservationSHA256 string                `json:"live_observation_sha256"`
	Cells                 []ScaleSetAlgebraCell `json:"cells"`
	ScaleCells            int                   `json:"scale_cells"`
	SHA256                string                `json:"sha256"`
}

// BindingInputIdentity commits to the complete binding inputs and the live
// PostgreSQL result agreement. It is an inventory identity, not a production
// Finalizer OutcomeSet or a schema-v2 extension.
type BindingInputIdentity struct {
	Version               string `json:"version"`
	ClaimScope            string `json:"claim_scope"`
	BindingSHA256         string `json:"binding_sha256"`
	CatalogSHA256         string `json:"catalog_sha256"`
	DatasetSHA256         string `json:"dataset_sha256"`
	DatasetProbeSHA256    string `json:"dataset_probe_sha256"`
	LiveObservationSHA256 string `json:"live_observation_sha256"`
	SetAlgebraSHA256      string `json:"set_algebra_sha256"`
	CellInputSetSHA256    string `json:"cell_input_set_sha256"`
	SHA256                string `json:"sha256"`
}

type OutputEvidence struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type ProvenanceReport struct {
	Version              string                            `json:"version"`
	GeneratorVersion     string                            `json:"generator_version"`
	Status               string                            `json:"status"`
	AuthorApproved       bool                              `json:"author_approved"`
	E2ExactByteReview    string                            `json:"e2_exact_byte_review"`
	Inputs               InputInventory                    `json:"inputs"`
	CatalogAttestation   CatalogAttestation                `json:"catalog_attestation"`
	DatasetAgreement     finalv5dataset.BenchmarkAgreement `json:"dataset_agreement"`
	LiveObservation      LiveObservation                   `json:"live_observation"`
	SetAlgebra           SetAlgebraIdentity                `json:"set_algebra"`
	BindingInputIdentity BindingInputIdentity              `json:"binding_input_identity"`
	Outputs              []OutputEvidence                  `json:"outputs"`
}

type GenerationSummary struct {
	Version                    string           `json:"version"`
	Status                     string           `json:"status"`
	OutputDirectory            string           `json:"output_directory"`
	Outputs                    []OutputEvidence `json:"outputs"`
	ScaleCells                 int              `json:"scale_cells"`
	ArtifactCells              int              `json:"artifact_cells"`
	ProvSQLCells               int              `json:"provsql_cells"`
	BindingInputIdentitySHA256 string           `json:"binding_input_identity_sha256"`
	SetAlgebraSHA256           string           `json:"set_algebra_sha256"`
}

// GeneratePublicationBinding validates Decision 20 first, then performs the
// live reads, constructs exact bytes in memory, and finally creates the closed
// output directory exclusively. No output exists before the live run succeeds.
func GeneratePublicationBinding(ctx context.Context, options GenerateOptions) (GenerationSummary, error) {
	var summary GenerationSummary
	if ctx == nil {
		return summary, errors.New("publication binding generation requires a context")
	}
	root, err := cleanRepositoryRoot(options.RepositoryRoot)
	if err != nil {
		return summary, err
	}
	if strings.TrimSpace(options.BusinessDSN) == "" {
		return summary, errors.New("publication binding generation requires Business PostgreSQL DSN")
	}
	if err := requireAbsentOutput(options.OutputDirectory); err != nil {
		return summary, err
	}
	artifactRoot, err := requirePrivateArtifactDirectory(options.ArtifactRoot)
	if err != nil {
		return summary, err
	}
	materials, err := loadGenerationMaterials(root)
	if err != nil {
		return summary, err
	}

	attestationModel, err := CatalogAttestationModel(materials.baseCatalogBytes, materials.approvedScaleBytes)
	if err != nil {
		return summary, err
	}
	catalogAttestation, err := AttestCatalogModel(ctx, options.BusinessDSN, attestationModel)
	if err != nil {
		return summary, err
	}
	candidate, err := BuildCatalogCandidate(materials.baseCatalogBytes, materials.approvedScaleBytes,
		catalogAttestation.Datasource.SchemaDigest)
	if err != nil {
		return summary, err
	}
	completeCatalog, err := candidate.Catalog()
	if err != nil {
		return summary, err
	}
	oracleOptions := finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: publicationOracleMaxInMemoryMembers,
		TempDir:            artifactRoot,
	}
	scaleOutcomes, err := finalv5binding.GenerateScaleOutcomeCandidateExpectations(
		completeCatalog.SHA256, oracleOptions)
	if err != nil {
		return summary, fmt.Errorf("generate pre-observation Scale Outcome expectations: %w", err)
	}
	datasetAgreement, err := finalv5dataset.VerifyBenchmarkPostgreSQL(ctx, options.BusinessDSN)
	if err != nil {
		return summary, fmt.Errorf("verify complete live benchmark Dataset: %w", err)
	}
	live, err := ObservePublicationClosure(ctx, options.BusinessDSN, materials.runtime,
		materials.scaleManifests, materials.provSQLManifests)
	if err != nil {
		return summary, err
	}
	if live.Database != catalogAttestation.Datasource.Database || live.User != catalogAttestation.Datasource.User ||
		catalogAttestation.Datasource.PostgreSQLMajorVersion != 16 ||
		live.PostgreSQLServerVersionNum != "160014" {
		// The exact 160014 check is repeated by the DB harness; this comparison
		// keeps generator evidence bound to PostgreSQL 16.14 itself.
		return summary, errors.New("live query observation and Catalog attestation do not identify PostgreSQL 16.14")
	}
	datasetIdentity, err := materials.runtime.DatasetIdentitySHA256()
	if err != nil || datasetAgreement.Observed.SHA256 != datasetIdentity ||
		datasetAgreement.Reference.SHA256 != datasetIdentity {
		return summary, errors.New("live Dataset agreement differs from the Contract-reviewed Dataset identity")
	}
	binding, bindingBytes, err := finalv5binding.BuildCompleteBinding(finalv5binding.CompleteBindingInput{
		DatasetSHA256: datasetIdentity, DatasetProbeSHA256: live.DatasetProbe.ResultSHA256,
		Catalog: completeCatalog, ScaleOutcomes: scaleOutcomes,
		ScaleManifests: materials.scaleManifests, ArtifactRuntime: materials.runtime,
		ProvSQLManifests: materials.provSQLManifests,
		OracleOptions:    oracleOptions,
	})
	if err != nil {
		return summary, err
	}
	inputs, err := buildInputInventory(materials, binding, live)
	if err != nil {
		return summary, err
	}
	setAlgebra, err := buildSetAlgebra(materials.scaleManifests, live.ObservationSHA256)
	if err != nil {
		return summary, err
	}
	bindingInputIdentity, err := buildBindingInputIdentity(binding.FileSHA256, candidate.SHA256(), datasetIdentity,
		live.DatasetProbe.ResultSHA256, live.ObservationSHA256, setAlgebra.SHA256, inputs.CellInputSetSHA256)
	if err != nil {
		return summary, err
	}
	catalogBytes := candidate.Bytes()
	outputs := []OutputEvidence{
		{Name: CatalogOutputName, SHA256: candidate.SHA256(), Bytes: int64(len(catalogBytes))},
		{Name: BindingOutputName, SHA256: binding.FileSHA256, Bytes: int64(len(bindingBytes))},
	}
	report := ProvenanceReport{Version: ProvenanceVersion, GeneratorVersion: GeneratorVersion,
		Status: ReviewStatus, AuthorApproved: false, E2ExactByteReview: "REQUIRED", Inputs: inputs,
		CatalogAttestation: catalogAttestation, DatasetAgreement: datasetAgreement, LiveObservation: live,
		SetAlgebra: setAlgebra, BindingInputIdentity: bindingInputIdentity, Outputs: outputs}
	reportBytes, err := canonicalJSON(report)
	if err != nil {
		return summary, errors.New("encode publication binding provenance")
	}
	files := map[string][]byte{CatalogOutputName: catalogBytes, BindingOutputName: bindingBytes,
		ProvenanceOutputName: reportBytes}
	sensitive, err := sensitiveDSNValues(options.BusinessDSN)
	if err != nil {
		return summary, err
	}
	if err := ValidateCredentialFree(files, sensitive); err != nil {
		return summary, err
	}
	if err := WriteClosedOutputDirectory(options.OutputDirectory, files, publicationOutputNames); err != nil {
		return summary, err
	}
	validated, err := ValidatePublicationOutput(root, options.OutputDirectory, sensitive)
	if err != nil {
		return summary, fmt.Errorf("validate freshly generated publication output: %w", err)
	}
	return validated, nil
}

func requireAbsentOutput(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("publication output directory is required")
	}
	clean := filepath.Clean(value)
	parent, err := os.Lstat(filepath.Dir(clean))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o022 != 0 {
		return errors.New("publication output parent must be an owner-controlled non-symlink directory")
	}
	if _, err := os.Lstat(clean); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("publication output path must be absent for create-exclusive generation")
	}
	return nil
}

func requirePrivateArtifactDirectory(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("private oracle artifact directory is required")
	}
	clean, err := filepath.Abs(value)
	if err != nil {
		return "", errors.New("resolve private oracle artifact directory")
	}
	info, err := os.Lstat(clean)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return "", errors.New("private oracle artifact directory must be a non-symlink mode-0700 directory")
	}
	return clean, nil
}

func sensitiveDSNValues(dsn string) ([]string, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("parse Business PostgreSQL DSN for credential scan")
	}
	values := []string{dsn}
	if config.Password != "" {
		values = append(values, config.Password)
	}
	return values, nil
}

func buildInputInventory(materials generationMaterials, binding finalv5binding.Binding,
	live LiveObservation) (InputInventory, error) {
	result := InputInventory{Approval: materials.approval, AuthorDecision: materials.decision,
		BaseCatalog: materials.baseCatalog, ApprovedScaleCatalog: materials.approvedScaleCatalog,
		Contract: materials.contract, Cells: make([]CellInput, 0, 123),
		ScaleCells: 12, ArtifactCells: 6, ProvSQLCells: 105, CellCount: 123}
	observations := make(map[string][]QueryObservation, len(live.Queries))
	for _, observation := range live.Queries {
		key := observation.Workload + "\x00" + observation.Cell
		observations[key] = append(observations[key], observation)
	}
	scaleManifests := make(map[string][]DigestReference, 12)
	for _, artifact := range materials.scaleManifests {
		scaleManifests[artifact.Manifest.Scale] = append(scaleManifests[artifact.Manifest.Scale],
			DigestReference{Path: pathJoinOracle(artifact.RelativePath), SHA256: artifact.SHA256})
	}
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		cell, present := binding.Section.Scale.DependencyE2E[spec.Scale]
		cellObservations := observations["scale/dependency-e2e\x00"+spec.Scale]
		if !present || len(scaleManifests[spec.Scale]) != 2 || len(cellObservations) != 2 {
			return InputInventory{}, fmt.Errorf("Scale input %s is incomplete", spec.Scale)
		}
		identities := []NamedIdentity{
			{Name: "candidate_binding_query", SHA256: sha256Hex([]byte(cell.Candidate.SQL))},
			{Name: "history_binding_query", SHA256: sha256Hex([]byte(cell.History.SQL))},
		}
		identities, err := appendOutcomeCandidateIdentities(identities, cell.OutcomeCandidate)
		if err != nil {
			return InputInventory{}, fmt.Errorf("Scale input %s Outcome candidate: %w", spec.Scale, err)
		}
		identities = appendLiveQueryIdentities(identities, cellObservations)
		result.Cells = append(result.Cells, CellInput{Workload: "scale/dependency-e2e", Cell: spec.Scale,
			TaskSHA256: digestStructured("TASKGATE-FINAL-V5-PUBLICATION-TASK-V1\x00", cell.Task),
			Identities: identities, OracleInputs: scaleManifests[spec.Scale]})
	}
	artifactCells, err := materials.runtime.ArtifactCells()
	if err != nil || len(artifactCells) != 6 {
		return InputInventory{}, errors.New("cannot enumerate exact six Artifact inputs")
	}
	for _, source := range artifactCells {
		cell, present := binding.Section.Artifact.ResultHeavy[source.Identity.Scale]
		cellObservations := observations["artifact/result-heavy\x00"+source.Identity.Scale]
		if !present || len(cellObservations) != 1 {
			return InputInventory{}, fmt.Errorf("Artifact input %s is incomplete", source.Identity.Scale)
		}
		query, err := materials.runtime.QueryContract(source)
		if err != nil {
			return InputInventory{}, err
		}
		_, manifestSHA, err := materials.runtime.OracleManifest(source)
		if err != nil {
			return InputInventory{}, err
		}
		identities := []NamedIdentity{
			{Name: "binding_query", SHA256: sha256Hex([]byte(cell.Query.SQL))},
			{Name: "bdg_template", Path: query.BDG.TemplatePath, SHA256: query.BDG.TemplateSHA256},
			{Name: "bdg_rendered_query", SHA256: query.BDG.SQLSHA256},
			{Name: "direct_template", Path: query.Direct.TemplatePath, SHA256: query.Direct.TemplateSHA256},
			{Name: "direct_rendered_query", SHA256: query.Direct.SQLSHA256},
			{Name: "normalization_contract", SHA256: query.NormalizationSHA256},
		}
		identities = appendLiveQueryIdentities(identities, cellObservations)
		result.Cells = append(result.Cells, CellInput{Workload: "artifact/result-heavy", Cell: source.Identity.Scale,
			TaskSHA256: digestStructured("TASKGATE-FINAL-V5-PUBLICATION-TASK-V1\x00", cell.Task),
			Identities: identities, OracleInputs: []DigestReference{{Path: pathJoinContract(source.OracleManifestPath),
				SHA256: manifestSHA}}})
	}
	provManifests := make(map[string]finalv5oracle.ProvSQLManifestArtifact, 105)
	for _, artifact := range materials.provSQLManifests {
		provManifests[artifact.Manifest.BindingKey] = artifact
	}
	for _, spec := range finalv5oracle.ProvSQLNonceJoinCells() {
		cell, present := binding.Section.ProvSQL.TaskGate[spec.BindingKey]
		cellObservations := observations["provsql/nonce-join-group\x00"+spec.BindingKey]
		if !present || len(cellObservations) != 1 {
			return InputInventory{}, fmt.Errorf("ProvSQL input %s is incomplete", spec.BindingKey)
		}
		businessSQL, err := provsqlfixture.BusinessSQL(spec.Scale, spec.Nonce)
		if err != nil {
			return InputInventory{}, err
		}
		artifact := provManifests[spec.BindingKey]
		identities := []NamedIdentity{
			{Name: "binding_query", SHA256: sha256Hex([]byte(cell.SQL))},
			{Name: "business_live_query", SHA256: provsqlfixture.SHA256String(businessSQL)},
		}
		identities = appendLiveQueryIdentities(identities, cellObservations)
		result.Cells = append(result.Cells, CellInput{Workload: "provsql/nonce-join-group", Cell: spec.BindingKey,
			TaskSHA256: digestStructured("TASKGATE-FINAL-V5-PUBLICATION-TASK-V1\x00", binding.Section.ProvSQL.Task),
			Identities: identities, OracleInputs: []DigestReference{{Path: pathJoinOracle(artifact.RelativePath),
				SHA256: artifact.SHA256}}})
	}
	if len(result.Cells) != result.CellCount {
		return InputInventory{}, errors.New("input inventory is not the exact 123-cell closure")
	}
	if len(observations) != 123 {
		return InputInventory{}, fmt.Errorf("live input observation map has %d cells; expected exactly 123", len(observations))
	}
	result.CellInputSetSHA256 = digestStructured("TASKGATE-FINAL-V5-PUBLICATION-CELL-INPUT-SET-V1\x00", result.Cells)
	return result, nil
}

func appendLiveQueryIdentities(values []NamedIdentity, observations []QueryObservation) []NamedIdentity {
	for _, observation := range observations {
		values = append(values, NamedIdentity{Name: "live_" + observation.Role + "_query",
			SHA256: observation.QuerySHA256})
	}
	return values
}

func appendOutcomeCandidateIdentities(values []NamedIdentity,
	expected finalv5binding.BoundOutcomeCandidateExpectation) ([]NamedIdentity, error) {
	if err := finalv5binding.ValidateBoundOutcomeCandidate(expected); err != nil {
		return nil, err
	}
	values = append(values, NamedIdentity{
		Name: "outcome_candidate_ordinary_set", SHA256: expected.OrdinarySetSHA256,
	})
	for index, member := range expected.Members {
		values = append(values, NamedIdentity{
			Name: fmt.Sprintf("outcome_candidate_member_%02d", index+1), SHA256: member,
		})
	}
	return values, nil
}

func pathJoinOracle(relative string) string {
	return filepath.ToSlash(filepath.Join(oracleRootRelativePath, relative))
}
func pathJoinContract(relative string) string {
	return filepath.ToSlash(filepath.Join(contractRootRelativePath, relative))
}

func buildSetAlgebra(manifests []finalv5oracle.ExposureScaleManifestArtifact,
	liveObservationSHA string) (SetAlgebraIdentity, error) {
	if !generatedSHA256(liveObservationSHA) {
		return SetAlgebraIdentity{}, errors.New("publication set algebra requires a generated live observation identity")
	}
	result := SetAlgebraIdentity{Version: "taskgate-final-v5-publication-set-algebra-v1",
		LiveObservationSHA256: liveObservationSHA, Cells: make([]ScaleSetAlgebraCell, 0, 12), ScaleCells: 12}
	byScale := make(map[string][]finalv5oracle.ExposureScaleManifestArtifact, 12)
	for _, artifact := range manifests {
		byScale[artifact.Manifest.Scale] = append(byScale[artifact.Manifest.Scale], artifact)
	}
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		pair := byScale[spec.Scale]
		if len(pair) != 2 || !reflect.DeepEqual(pair[0].Manifest.Expected, pair[1].Manifest.Expected) {
			return SetAlgebraIdentity{}, fmt.Errorf("Scale %s lacks an exact novel/replay algebra pair", spec.Scale)
		}
		expected := pair[0].Manifest.Expected
		if expected.DependencyCandidateCardinality == nil || expected.ExistingCardinality == nil ||
			expected.OverlapCardinality == nil || expected.NovelCardinality == nil || expected.UnionCardinality == nil {
			return SetAlgebraIdentity{}, fmt.Errorf("Scale %s oracle omits set algebra cardinalities", spec.Scale)
		}
		for _, digest := range []string{expected.DependencyCandidateSetSHA256, expected.ExistingSetSHA256,
			expected.OverlapSetSHA256, expected.NovelSetSHA256, expected.UnionSetSHA256} {
			if !validSHA256(digest) {
				return SetAlgebraIdentity{}, fmt.Errorf("Scale %s oracle omits a set algebra digest", spec.Scale)
			}
		}
		result.Cells = append(result.Cells, ScaleSetAlgebraCell{Scale: spec.Scale,
			CandidateCardinality: *expected.DependencyCandidateCardinality,
			CandidateSetSHA256:   expected.DependencyCandidateSetSHA256,
			ExistingCardinality:  *expected.ExistingCardinality, ExistingSetSHA256: expected.ExistingSetSHA256,
			OverlapCardinality: *expected.OverlapCardinality, OverlapSetSHA256: expected.OverlapSetSHA256,
			NovelCardinality: *expected.NovelCardinality, NovelSetSHA256: expected.NovelSetSHA256,
			UnionCardinality: *expected.UnionCardinality, UnionSetSHA256: expected.UnionSetSHA256,
			OracleInputs: []DigestReference{
				{Path: pathJoinOracle(pair[0].RelativePath), SHA256: pair[0].SHA256},
				{Path: pathJoinOracle(pair[1].RelativePath), SHA256: pair[1].SHA256},
			}})
	}
	if len(manifests) != 24 || len(byScale) != 12 || len(result.Cells) != 12 {
		return SetAlgebraIdentity{}, errors.New("publication set algebra is not the exact 12-cell/24-manifest closure")
	}
	result.SHA256 = digestStructured("TASKGATE-FINAL-V5-PUBLICATION-SET-ALGEBRA-V1\x00",
		struct {
			Version               string
			LiveObservationSHA256 string
			Cells                 []ScaleSetAlgebraCell
		}{result.Version, result.LiveObservationSHA256, result.Cells})
	return result, nil
}

func buildBindingInputIdentity(bindingSHA, catalogSHA, datasetSHA, probeSHA, liveSHA, setSHA,
	cellInputSHA string) (BindingInputIdentity, error) {
	for _, digest := range []string{bindingSHA, catalogSHA, datasetSHA, probeSHA, liveSHA, setSHA, cellInputSHA} {
		if !generatedSHA256(digest) {
			return BindingInputIdentity{}, errors.New("publication binding-input identity component is missing or a placeholder")
		}
	}
	result := BindingInputIdentity{Version: "taskgate-final-v5-publication-binding-input-identity-v1",
		ClaimScope:    "publication_binding_inputs_and_live_postgresql_result_agreement_only",
		BindingSHA256: bindingSHA, CatalogSHA256: catalogSHA,
		DatasetSHA256: datasetSHA, DatasetProbeSHA256: probeSHA, LiveObservationSHA256: liveSHA,
		SetAlgebraSHA256: setSHA, CellInputSetSHA256: cellInputSHA}
	copy := result
	copy.SHA256 = ""
	result.SHA256 = digestStructured("TASKGATE-FINAL-V5-PUBLICATION-BINDING-INPUT-IDENTITY-V1\x00", copy)
	return result, nil
}

func digestStructured(domain string, value any) string {
	encoded, err := canonicalJSON(value)
	if err != nil {
		panic("canonical JSON over a fixed in-memory publication structure failed: " + err.Error())
	}
	return domainSHA256(domain, encoded)
}

func sortedOutputEvidence(values []OutputEvidence) []OutputEvidence {
	result := append([]OutputEvidence(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func contractArtifacts(runtime *finalv5contracts.Runtime) ([]finalv5contracts.IndexedArtifact, error) {
	return runtime.IndexedArtifacts()
}
