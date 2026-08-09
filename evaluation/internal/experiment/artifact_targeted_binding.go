package experiment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/approval"
)

// ArtifactTargetedDeploymentBindingVersion identifies the credential-free
// binding used only by the non-publication Artifact targeted runner.
const ArtifactTargetedDeploymentBindingVersion = "taskgate-final-v5-artifact-targeted-deployment-binding-v1"

var frozenArtifactTargetedScales = [...]string{
	"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16",
}

// ArtifactTargetedBindingInput names the source and live deployment material
// the targeted binding independently acquires. None of these paths or the DSN
// is serialized into the binding.
type ArtifactTargetedBindingInput struct {
	SubmissionCommit       string
	ProfileRegistryPath    string
	ProfileAlias           string
	CatalogPath            string
	BusinessDSN            string
	SelectedScales         []string
	QualificationPath      string
	PostgreSQLIdentityPath string
}

// ArtifactTargetedProfileBinding records the Result-heavy profile identity and
// the clearance that existed before the targeted run began.
type ArtifactTargetedProfileBinding struct {
	ProfileID             string `json:"profile_id"`
	ProfileAlias          string `json:"profile_alias"`
	ClosureSHA256         string `json:"closure_sha256"`
	CatalogSHA256         string `json:"catalog_sha256"`
	PublicationIdentity   string `json:"publication_identity"`
	ActivationSupported   bool   `json:"activation_supported"`
	ActivationSmokePassed bool   `json:"activation_smoke_passed"`
	TargetedRunEligible   bool   `json:"targeted_run_eligible"`
}

// ArtifactTargetedCellBinding is the bounded source-controlled identity of one
// frozen Artifact cell. It intentionally carries hashes, shapes and names but
// never raw SQL or result rows.
type ArtifactTargetedCellBinding struct {
	Cell                   finalv5contracts.CellIdentity `json:"cell"`
	SpecID                 string                        `json:"spec_id"`
	BaselineIdentity       string                        `json:"baseline_identity"`
	ProductID              string                        `json:"product_id"`
	PublicationID          string                        `json:"publication_id"`
	Rows                   int64                         `json:"rows"`
	Columns                int                           `json:"columns"`
	BDGTemplateSHA256      string                        `json:"bdg_template_sha256"`
	BDGSQLSHA256           string                        `json:"bdg_sql_sha256"`
	DirectTemplateSHA256   string                        `json:"direct_template_sha256"`
	DirectSQLSHA256        string                        `json:"direct_sql_sha256"`
	NormalizationSHA256    string                        `json:"normalization_sha256"`
	OracleManifestPath     string                        `json:"oracle_manifest_path"`
	OracleManifestSHA256   string                        `json:"oracle_manifest_sha256"`
	NormalizedSchemaSHA256 string                        `json:"normalized_schema_sha256"`
	CanonicalResultSHA256  string                        `json:"canonical_result_sha256"`
}

// ArtifactTargetedDeploymentBinding binds the six immutable Artifact cells to
// one fresh targeted deployment. SelectedScales narrows execution only; the
// source-controlled cell set is always complete.
type ArtifactTargetedDeploymentBinding struct {
	SchemaVersion                  int                            `json:"schema_version"`
	Record                         string                         `json:"record"`
	SubmissionCommit               string                         `json:"submission_commit"`
	ContractRelease                string                         `json:"contract_release"`
	ContractIndexSHA256            string                         `json:"contract_index_sha256"`
	ProfileRegistrySHA256          string                         `json:"profile_registry_sha256"`
	Profile                        ArtifactTargetedProfileBinding `json:"profile"`
	ArtifactCells                  []ArtifactTargetedCellBinding  `json:"artifact_cells"`
	SelectedScales                 []string                       `json:"selected_scales"`
	DatasetProbeSQLSHA256          string                         `json:"dataset_probe_sql_sha256"`
	DatasetProbeSHA256             string                         `json:"dataset_probe_sha256"`
	AttestationQualificationSHA256 string                         `json:"attestation_qualification_sha256"`
	PostgreSQLIdentitySHA256       string                         `json:"postgresql_identity_sha256"`
}

// ArtifactTargetedBindingValidation is the non-sensitive report printed after
// the create-exclusive output has been re-opened and validated.
type ArtifactTargetedBindingValidation struct {
	SchemaVersion      int    `json:"schema_version"`
	Status             string `json:"status"`
	ArtifactCells      int    `json:"artifact_cells"`
	SelectedCells      int    `json:"selected_cells"`
	DatasetProbeSHA256 string `json:"dataset_probe_sha256"`
	BindingFileSHA256  string `json:"binding_file_sha256"`
}

type artifactTargetedBindingDependencies struct {
	loadRuntime func() (*finalv5contracts.Runtime, error)
	probe       func(context.Context, *finalv5contracts.Runtime, string) (string, error)
}

var productionArtifactTargetedBindingDependencies = artifactTargetedBindingDependencies{
	loadRuntime: finalv5contracts.LoadRuntime,
	// The targeted record and RuntimeFinalizerV3 must identify one deployment
	// through the same independently acquired probe derivation.
	probe: datasetProbeDigestV3,
}

// BuildArtifactTargetedDeploymentBinding loads and revalidates every source,
// then executes exactly one pre-measurement dataset probe.
func BuildArtifactTargetedDeploymentBinding(ctx context.Context,
	input ArtifactTargetedBindingInput) (ArtifactTargetedDeploymentBinding, error) {
	return buildArtifactTargetedDeploymentBinding(ctx, input, productionArtifactTargetedBindingDependencies)
}

func buildArtifactTargetedDeploymentBinding(ctx context.Context, input ArtifactTargetedBindingInput,
	dependencies artifactTargetedBindingDependencies) (ArtifactTargetedDeploymentBinding, error) {
	var result ArtifactTargetedDeploymentBinding
	if ctx == nil {
		return result, errors.New("Artifact targeted binding requires a context")
	}
	if dependencies.loadRuntime == nil || dependencies.probe == nil {
		return result, errors.New("Artifact targeted binding dependencies are incomplete")
	}
	if !validArtifactTargetedCommit(strings.TrimSpace(input.SubmissionCommit)) {
		return result, errors.New("submission_commit is not a full lowercase git object name")
	}
	for name, value := range map[string]string{
		"profile registry":          input.ProfileRegistryPath,
		"profile alias":             input.ProfileAlias,
		"profile Catalog":           input.CatalogPath,
		"Business PostgreSQL DSN":   input.BusinessDSN,
		"attestation qualification": input.QualificationPath,
		"PostgreSQL identity":       input.PostgreSQLIdentityPath,
	} {
		if strings.TrimSpace(value) == "" {
			return result, fmt.Errorf("%s is required", name)
		}
	}
	selected, err := normalizeArtifactTargetedScales(input.SelectedScales)
	if err != nil {
		return result, err
	}

	// Refuse symlink/non-regular source inputs before any parser can follow one.
	registrySHA, _, err := readArtifactTargetedSource(input.ProfileRegistryPath, "profile registry")
	if err != nil {
		return result, err
	}
	catalogSHA, _, err := readArtifactTargetedSource(input.CatalogPath, "profile Catalog")
	if err != nil {
		return result, err
	}
	qualificationSHA, qualificationBytes, err := readArtifactTargetedSource(
		input.QualificationPath, "attestation qualification")
	if err != nil {
		return result, err
	}
	identitySHA, _, err := readArtifactTargetedSource(input.PostgreSQLIdentityPath, "PostgreSQL identity")
	if err != nil {
		return result, err
	}

	runtime, err := dependencies.loadRuntime()
	if err != nil {
		return result, fmt.Errorf("load the embedded Contract Index: %w", err)
	}
	profile, err := ResolveTargetedProfileIdentity(input.ProfileRegistryPath, input.ProfileAlias)
	if err != nil {
		return result, err
	}
	if profile.RegistrySHA256 != registrySHA {
		return result, errors.New("profile registry changed while its targeted identity was resolved")
	}
	if profile.ContractRelease != runtime.ContractRelease() {
		return result, fmt.Errorf("profile registry release %q differs from Contract Index release %q",
			profile.ContractRelease, runtime.ContractRelease())
	}
	if err := requireArtifactTargetedProfileCells(profile); err != nil {
		return result, err
	}
	if profile.CatalogSHA256 != catalogSHA {
		return result, errors.New("profile Catalog bytes differ from the Result-heavy registry entry")
	}
	if err := requireArtifactTargetedQualificationBinding(qualificationBytes, profile, runtime); err != nil {
		return result, err
	}
	postgres, err := (retainedPostgreSQLIdentityV3{documentPath: input.PostgreSQLIdentityPath}).
		ReadPostgreSQLIdentity(ctx)
	if err != nil {
		return result, err
	}
	if _, err := (retainedQualificationV3{documentPath: input.QualificationPath}).
		Resolve(input.CatalogPath, postgres); err != nil {
		return result, fmt.Errorf("validate the retained qualification and PostgreSQL identity pair: %w", err)
	}

	probeSQL, err := runtime.DatasetProbeSQL()
	if err != nil {
		return result, fmt.Errorf("load the Contract Index dataset probe: %w", err)
	}
	probeSQLSHA := sha256String(probeSQL)
	probeResultSHA, err := dependencies.probe(ctx, runtime, input.BusinessDSN)
	if err != nil {
		return result, err
	}
	if !validArtifactTargetedDigest(probeResultSHA) {
		return result, errors.New("live dataset probe produced no lowercase SHA-256 identity")
	}

	cells, err := artifactTargetedCells(runtime, input.CatalogPath, catalogSHA, probeResultSHA)
	if err != nil {
		return result, err
	}
	result = ArtifactTargetedDeploymentBinding{
		SchemaVersion:         1,
		Record:                ArtifactTargetedDeploymentBindingVersion,
		SubmissionCommit:      strings.TrimSpace(input.SubmissionCommit),
		ContractRelease:       runtime.ContractRelease(),
		ContractIndexSHA256:   runtime.IndexSHA256(),
		ProfileRegistrySHA256: registrySHA,
		Profile: ArtifactTargetedProfileBinding{
			ProfileID: profile.ProfileID, ProfileAlias: profile.ProfileAlias,
			ClosureSHA256: profile.ClosureSHA256, CatalogSHA256: profile.CatalogSHA256,
			PublicationIdentity:   profile.PublicationIdentity,
			ActivationSupported:   profile.ActivationSupported,
			ActivationSmokePassed: profile.ActivationSmokePassed,
			TargetedRunEligible:   profile.TargetedRunEligible,
		},
		ArtifactCells: cells, SelectedScales: selected,
		DatasetProbeSQLSHA256: probeSQLSHA, DatasetProbeSHA256: probeResultSHA,
		AttestationQualificationSHA256: qualificationSHA,
		PostgreSQLIdentitySHA256:       identitySHA,
	}
	if err := result.Validate(); err != nil {
		return ArtifactTargetedDeploymentBinding{}, err
	}

	// A byte changed during construction is not the source this record says was
	// checked. Re-read all four external inputs after every semantic validation.
	for _, check := range []struct {
		path, name, expected string
	}{
		{input.ProfileRegistryPath, "profile registry", registrySHA},
		{input.CatalogPath, "profile Catalog", catalogSHA},
		{input.QualificationPath, "attestation qualification", qualificationSHA},
		{input.PostgreSQLIdentityPath, "PostgreSQL identity", identitySHA},
	} {
		actual, _, readErr := readArtifactTargetedSource(check.path, check.name)
		if readErr != nil {
			return ArtifactTargetedDeploymentBinding{}, readErr
		}
		if actual != check.expected {
			return ArtifactTargetedDeploymentBinding{}, fmt.Errorf("%s changed while the targeted binding was built", check.name)
		}
	}
	return result, nil
}

func requireArtifactTargetedProfileCells(profile TargetedProfileIdentity) error {
	if profile.ProfileAlias != artifactDeploymentProfileAlias {
		return fmt.Errorf("Artifact targeted binding requires profile alias %q, got %q",
			artifactDeploymentProfileAlias, profile.ProfileAlias)
	}
	want := make(map[string]bool, len(frozenArtifactTargetedScales))
	for _, scale := range frozenArtifactTargetedScales {
		identity := finalv5contracts.CellIdentity{
			ExperimentID: finalv5contracts.ArtifactExperimentID,
			WorkloadID:   finalv5contracts.ArtifactWorkloadID,
			Scale:        scale,
			Mode:         "novel",
		}
		want[identity.String()] = true
	}
	seen := make(map[string]bool, len(want))
	for _, coordinate := range profile.WorkloadCells {
		if !strings.HasPrefix(coordinate, finalv5contracts.ArtifactExperimentID+"/"+
			finalv5contracts.ArtifactWorkloadID+"/") {
			continue
		}
		if !want[coordinate] || seen[coordinate] {
			return fmt.Errorf("Result-heavy profile carries unexpected or duplicate Artifact cell %q", coordinate)
		}
		seen[coordinate] = true
	}
	if len(seen) != len(want) {
		return fmt.Errorf("Result-heavy profile carries %d of the six frozen Artifact cells", len(seen))
	}
	return nil
}

type artifactTargetedQualificationProfile struct {
	ProfileID              string `json:"profile_id"`
	ProfileAlias           string `json:"profile_alias"`
	ProfileRegistryRelease string `json:"profile_registry_release"`
	ProfileRegistrySHA256  string `json:"profile_registry_sha256"`
	CatalogSHA256          string `json:"catalog_sha256"`
}

func requireArtifactTargetedQualificationBinding(payload []byte, profile TargetedProfileIdentity,
	runtime *finalv5contracts.Runtime) error {
	var document struct {
		Profile artifactTargetedQualificationProfile `json:"profile_binding"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return fmt.Errorf("decode the retained qualification profile binding: %w", err)
	}
	want := artifactTargetedQualificationProfile{
		ProfileID: profile.ProfileID, ProfileAlias: profile.ProfileAlias,
		ProfileRegistryRelease: runtime.ContractRelease(), ProfileRegistrySHA256: profile.RegistrySHA256,
		CatalogSHA256: profile.CatalogSHA256,
	}
	if document.Profile != want {
		return errors.New("the retained qualification names a different profile registry, release or Catalog")
	}
	return nil
}

func artifactTargetedCells(runtime *finalv5contracts.Runtime, catalogPath, catalogSHA,
	probeSHA string) ([]ArtifactTargetedCellBinding, error) {
	all, err := runtime.ArtifactCells()
	if err != nil {
		return nil, fmt.Errorf("read the frozen Artifact matrix: %w", err)
	}
	if len(all) != len(frozenArtifactTargetedScales) {
		return nil, fmt.Errorf("Artifact contract has %d cells; want exactly %d", len(all), len(frozenArtifactTargetedScales))
	}
	for index, cell := range all {
		if cell.Identity.ExperimentID != finalv5contracts.ArtifactExperimentID ||
			cell.Identity.WorkloadID != finalv5contracts.ArtifactWorkloadID ||
			cell.Identity.Scale != frozenArtifactTargetedScales[index] || cell.Identity.Mode != "novel" {
			return nil, fmt.Errorf("Artifact contract cell %d is %s; want %s/%s/%s/novel", index,
				cell.Identity, finalv5contracts.ArtifactExperimentID, finalv5contracts.ArtifactWorkloadID,
				frozenArtifactTargetedScales[index])
		}
	}

	result := make([]ArtifactTargetedCellBinding, 0, len(frozenArtifactTargetedScales))
	live := finalv5contracts.LiveDeployment{
		CatalogPath: catalogPath, CatalogSHA256: catalogSHA, DatasetProbeSHA256: probeSHA,
	}
	for _, scale := range frozenArtifactTargetedScales {
		cell, err := runtime.ArtifactCell(scale, "novel")
		if err != nil {
			return nil, err
		}
		query, err := runtime.QueryContract(cell)
		if err != nil {
			return nil, fmt.Errorf("load Artifact query contract %s: %w", cell.Identity, err)
		}
		manifest, manifestSHA, err := runtime.OracleManifest(cell)
		if err != nil {
			return nil, fmt.Errorf("load Artifact oracle manifest %s: %w", cell.Identity, err)
		}
		bound, err := runtime.BindDeployment(cell, live)
		if err != nil {
			return nil, fmt.Errorf("bind Artifact cell %s to the Result-heavy deployment: %w", cell.Identity, err)
		}
		if query.Cell != cell.Identity || query.Rows != cell.Rows || query.Rows != cell.ExpectedRows ||
			query.Columns != cell.ExpectedColumns || manifest.Expected.RowCount == nil ||
			*manifest.Expected.RowCount != query.Rows || manifest.Expected.ColumnCount == nil ||
			*manifest.Expected.ColumnCount != query.Columns {
			return nil, fmt.Errorf("Artifact cell %s query/oracle NxC identity disagrees", cell.Identity)
		}
		if query.BDG.SQLSHA256 != sha256String(query.BDG.SQL) ||
			query.Direct.SQLSHA256 != sha256String(query.Direct.SQL) {
			return nil, fmt.Errorf("Artifact cell %s rendered SQL SHA-256 disagrees", cell.Identity)
		}
		if !validSHA256(manifestSHA) || !validSHA256(manifest.Expected.NormalizedSchemaSHA256) ||
			!validSHA256(manifest.Expected.CanonicalResultSHA256) {
			return nil, fmt.Errorf("Artifact cell %s oracle identity is incomplete", cell.Identity)
		}
		if len(cell.ProductIDs) != 1 || len(cell.PublicationIDs) != 1 ||
			bound.ProductID != cell.ProductIDs[0] || bound.PublicationID != cell.PublicationIDs[0] ||
			bound.IndexSHA256 != runtime.IndexSHA256() || bound.CatalogSHA256 != catalogSHA ||
			bound.DatasetProbeSHA256 != probeSHA {
			return nil, fmt.Errorf("Artifact cell %s live Catalog binding disagrees", cell.Identity)
		}
		result = append(result, ArtifactTargetedCellBinding{
			Cell: cell.Identity, SpecID: cell.SpecID, BaselineIdentity: cell.BaselineIdentity,
			ProductID: bound.ProductID, PublicationID: bound.PublicationID,
			Rows: query.Rows, Columns: query.Columns,
			BDGTemplateSHA256: query.BDG.TemplateSHA256, BDGSQLSHA256: query.BDG.SQLSHA256,
			DirectTemplateSHA256: query.Direct.TemplateSHA256, DirectSQLSHA256: query.Direct.SQLSHA256,
			NormalizationSHA256: query.NormalizationSHA256,
			OracleManifestPath:  cell.OracleManifestPath, OracleManifestSHA256: manifestSHA,
			NormalizedSchemaSHA256: manifest.Expected.NormalizedSchemaSHA256,
			CanonicalResultSHA256:  manifest.Expected.CanonicalResultSHA256,
		})
	}
	return result, nil
}

func normalizeArtifactTargetedScales(scales []string) ([]string, error) {
	if len(scales) == 0 {
		return nil, errors.New("selected_scales must be a non-empty frozen subset")
	}
	requested := make(map[string]bool, len(scales))
	for _, scale := range scales {
		if strings.TrimSpace(scale) == "" || scale != strings.TrimSpace(scale) {
			return nil, errors.New("selected_scales contains an empty or non-canonical member")
		}
		if requested[scale] {
			return nil, fmt.Errorf("selected_scales repeats %q", scale)
		}
		requested[scale] = true
	}
	selected := make([]string, 0, len(scales))
	for _, scale := range frozenArtifactTargetedScales {
		if requested[scale] {
			selected = append(selected, scale)
			delete(requested, scale)
		}
	}
	if len(requested) != 0 {
		for scale := range requested {
			return nil, fmt.Errorf("selected_scales contains non-frozen scale %q", scale)
		}
	}
	return selected, nil
}

// Validate rejects a partial, reordered, overclaiming or malformed record.
func (binding ArtifactTargetedDeploymentBinding) Validate() error {
	if binding.SchemaVersion != 1 || binding.Record != ArtifactTargetedDeploymentBindingVersion {
		return errors.New("Artifact targeted deployment binding header is invalid")
	}
	if !validArtifactTargetedCommit(binding.SubmissionCommit) || strings.TrimSpace(binding.ContractRelease) == "" {
		return errors.New("Artifact targeted deployment binding source identity is invalid")
	}
	for name, digest := range map[string]string{
		"contract_index_sha256":            binding.ContractIndexSHA256,
		"profile_registry_sha256":          binding.ProfileRegistrySHA256,
		"closure_sha256":                   binding.Profile.ClosureSHA256,
		"catalog_sha256":                   binding.Profile.CatalogSHA256,
		"publication_identity":             binding.Profile.PublicationIdentity,
		"dataset_probe_sql_sha256":         binding.DatasetProbeSQLSHA256,
		"dataset_probe_sha256":             binding.DatasetProbeSHA256,
		"attestation_qualification_sha256": binding.AttestationQualificationSHA256,
		"postgresql_identity_sha256":       binding.PostgreSQLIdentitySHA256,
	} {
		if !validArtifactTargetedDigest(digest) {
			return fmt.Errorf("Artifact targeted deployment binding %s is not a non-placeholder SHA-256", name)
		}
	}
	if binding.Profile.ProfileID != finalv5profile.ProfileID(binding.Profile.ClosureSHA256) ||
		binding.Profile.ProfileAlias != artifactDeploymentProfileAlias ||
		!binding.Profile.ActivationSupported || !binding.Profile.ActivationSmokePassed ||
		!binding.Profile.TargetedRunEligible {
		return errors.New("Artifact targeted deployment profile is not cleared")
	}
	if len(binding.ArtifactCells) != len(frozenArtifactTargetedScales) {
		return fmt.Errorf("Artifact targeted deployment binding has %d cells; want 6", len(binding.ArtifactCells))
	}
	for index, cell := range binding.ArtifactCells {
		if cell.Cell.ExperimentID != finalv5contracts.ArtifactExperimentID ||
			cell.Cell.WorkloadID != finalv5contracts.ArtifactWorkloadID ||
			cell.Cell.Scale != frozenArtifactTargetedScales[index] || cell.Cell.Mode != "novel" ||
			strings.TrimSpace(cell.SpecID) == "" ||
			strings.TrimSpace(cell.ProductID) == "" || strings.TrimSpace(cell.PublicationID) == "" ||
			cell.BaselineIdentity != "baseline/S6/"+cell.Cell.Scale+"/novel" {
			return fmt.Errorf("Artifact targeted cell %d identity or shape is invalid", index)
		}
		rows, columns := artifactTargetedExpectedShape(cell.Cell.Scale)
		if cell.Rows != rows || cell.Columns != columns ||
			cell.OracleManifestPath != path.Join("oracle-manifests", finalv5contracts.ArtifactExperimentID,
				finalv5contracts.ArtifactWorkloadID, cell.Cell.Scale, "novel.json") {
			return fmt.Errorf("Artifact targeted cell %s shape or oracle path is not frozen", cell.Cell)
		}
		for name, digest := range map[string]string{
			"bdg_template_sha256": cell.BDGTemplateSHA256, "bdg_sql_sha256": cell.BDGSQLSHA256,
			"direct_template_sha256": cell.DirectTemplateSHA256, "direct_sql_sha256": cell.DirectSQLSHA256,
			"normalization_sha256":     cell.NormalizationSHA256,
			"oracle_manifest_sha256":   cell.OracleManifestSHA256,
			"normalized_schema_sha256": cell.NormalizedSchemaSHA256,
			"canonical_result_sha256":  cell.CanonicalResultSHA256,
		} {
			if !validArtifactTargetedDigest(digest) {
				return fmt.Errorf("Artifact targeted cell %s %s is not a non-placeholder SHA-256", cell.Cell, name)
			}
		}
	}
	selected, err := normalizeArtifactTargetedScales(binding.SelectedScales)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(selected, binding.SelectedScales) {
		return errors.New("selected_scales is not in frozen order")
	}
	return nil
}

func artifactTargetedExpectedShape(scale string) (int64, int) {
	switch scale {
	case "100x4":
		return 100, 4
	case "10k-x4":
		return 10_000, 4
	case "100k-x4":
		return 100_000, 4
	case "100x16":
		return 100, 16
	case "10k-x16":
		return 10_000, 16
	case "100k-x16":
		return 100_000, 16
	default:
		return 0, 0
	}
}

func validArtifactTargetedDigest(value string) bool {
	return validSHA256(value) && value != strings.Repeat("0", sha256.Size*2)
}

func validArtifactTargetedCommit(value string) bool {
	return validCommitSHA1(value) && value != strings.Repeat("0", 40)
}

// CanonicalArtifactTargetedDeploymentBinding returns RFC 8785 JSON with no
// trailing newline. These exact bytes are the identity the ProfileBinding uses.
func CanonicalArtifactTargetedDeploymentBinding(binding ArtifactTargetedDeploymentBinding) ([]byte, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	return approval.CanonicalJSON(binding)
}

// DecodeArtifactTargetedDeploymentBinding accepts only the exact canonical
// representation. StrictJSON rejects unknown/trailing fields; reproducing the
// canonical bytes also rejects whitespace and duplicate member names.
func DecodeArtifactTargetedDeploymentBinding(payload []byte) (ArtifactTargetedDeploymentBinding, error) {
	var binding ArtifactTargetedDeploymentBinding
	if err := StrictJSON(payload, &binding); err != nil {
		return binding, fmt.Errorf("decode Artifact targeted deployment binding: %w", err)
	}
	canonical, err := CanonicalArtifactTargetedDeploymentBinding(binding)
	if err != nil {
		return binding, err
	}
	if !bytes.Equal(payload, canonical) {
		return binding, errors.New("Artifact targeted deployment binding is not canonical JSON")
	}
	return binding, nil
}

// WriteArtifactTargetedDeploymentBinding creates a regular, non-symlink mode
// 0600 file exclusively. A partial or unverifiable output is removed.
func WriteArtifactTargetedDeploymentBinding(path string, binding ArtifactTargetedDeploymentBinding) error {
	payload, err := CanonicalArtifactTargetedDeploymentBinding(binding)
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("Artifact targeted deployment binding output path is required")
	}
	output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create Artifact targeted deployment binding: %w", err)
	}
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := output.Chmod(0o600); err != nil {
		return fmt.Errorf("set Artifact targeted deployment binding mode: %w", err)
	}
	info, err := output.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("Artifact targeted deployment binding output is not a regular file")
	}
	if _, err := output.Write(payload); err != nil {
		return fmt.Errorf("write Artifact targeted deployment binding: %w", err)
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("sync Artifact targeted deployment binding: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close Artifact targeted deployment binding: %w", err)
	}
	info, err = os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("Artifact targeted deployment binding output is not a regular non-symlink mode 0600 file")
	}
	keep = true
	return nil
}

// ValidateArtifactTargetedDeploymentBindingFile reopens the exact output and
// produces the bounded validation report consumed by the targeted runner.
func ValidateArtifactTargetedDeploymentBindingFile(path string,
	expected ArtifactTargetedDeploymentBinding) (ArtifactTargetedBindingValidation, error) {
	var report ArtifactTargetedBindingValidation
	info, err := os.Lstat(path)
	if err != nil {
		return report, fmt.Errorf("stat Artifact targeted deployment binding: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return report, errors.New("Artifact targeted deployment binding is not a regular non-symlink file")
	}
	if info.Mode().Perm() != 0o600 {
		return report, fmt.Errorf("Artifact targeted deployment binding mode is %04o; want 0600", info.Mode().Perm())
	}
	if info.Size() <= 0 || info.Size() > 1<<20 {
		return report, errors.New("Artifact targeted deployment binding size is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return report, err
	}
	payload, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	closeErr := file.Close()
	if err != nil {
		return report, err
	}
	if closeErr != nil {
		return report, closeErr
	}
	decoded, err := DecodeArtifactTargetedDeploymentBinding(payload)
	if err != nil {
		return report, err
	}
	if !reflect.DeepEqual(decoded, expected) {
		return report, errors.New("Artifact targeted deployment binding bytes differ from the validated sources")
	}
	return ArtifactTargetedBindingValidation{
		SchemaVersion: 1, Status: "valid", ArtifactCells: len(decoded.ArtifactCells),
		SelectedCells: len(decoded.SelectedScales), DatasetProbeSHA256: decoded.DatasetProbeSHA256,
		BindingFileSHA256: sha256Hex(payload),
	}, nil
}

// ValidateArtifactTargetedDeploymentBindingSources rebuilds the binding from
// current inputs and rejects any contract, oracle, Catalog, registry,
// qualification, identity or live-probe drift.
func ValidateArtifactTargetedDeploymentBindingSources(ctx context.Context,
	binding ArtifactTargetedDeploymentBinding, input ArtifactTargetedBindingInput) error {
	current, err := BuildArtifactTargetedDeploymentBinding(ctx, input)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(binding, current) {
		return errors.New("Artifact targeted deployment binding source material changed")
	}
	return nil
}

func readArtifactTargetedSource(path, name string) (string, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("%s must be a regular non-symlink file", name)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(payload) == 0 {
		return "", nil, fmt.Errorf("%s is empty", name)
	}
	return sha256Hex(payload), payload, nil
}

func sha256String(value string) string { return sha256Hex([]byte(value)) }
