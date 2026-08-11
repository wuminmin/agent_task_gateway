package finalv5publication

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
)

// ValidatePublicationOutput performs an offline, source-backed validation of
// the private three-file output. sensitiveValues may be nil for later review;
// generation passes the live DSN/password so exact credential echoes are also
// rejected before the first write.
func ValidatePublicationOutput(repositoryRoot, outputDirectory string,
	sensitiveValues []string) (GenerationSummary, error) {
	var summary GenerationSummary
	root, err := cleanRepositoryRoot(repositoryRoot)
	if err != nil {
		return summary, err
	}
	files, err := readClosedOutputFiles(outputDirectory, publicationOutputNames)
	if err != nil {
		return summary, err
	}
	if err := ValidateCredentialFree(files, sensitiveValues); err != nil {
		return summary, err
	}
	for name, value := range files {
		if containsPlaceholder(value) {
			return summary, fmt.Errorf("publication output %q contains a forbidden placeholder", name)
		}
	}

	logical, err := catalog.Parse(files[CatalogOutputName])
	if err != nil {
		return summary, fmt.Errorf("parse complete publication Catalog: %w", err)
	}
	binding, err := finalv5binding.Parse(files[BindingOutputName])
	if err != nil {
		return summary, fmt.Errorf("parse complete publication binding: %w", err)
	}
	if err := finalv5binding.ValidateAgainstCatalog(binding, logical); err != nil {
		return summary, err
	}
	var report ProvenanceReport
	if err := strictJSON(files[ProvenanceOutputName], &report); err != nil {
		return summary, fmt.Errorf("decode strict publication provenance: %w", err)
	}
	if report.Version != ProvenanceVersion || report.GeneratorVersion != GeneratorVersion ||
		report.Status != ReviewStatus || report.AuthorApproved || report.E2ExactByteReview != "REQUIRED" {
		return summary, errors.New("publication provenance status/version does not identify an unapproved E1 review candidate")
	}
	wantOutputs := []OutputEvidence{
		{Name: CatalogOutputName, SHA256: sha256Hex(files[CatalogOutputName]), Bytes: int64(len(files[CatalogOutputName]))},
		{Name: BindingOutputName, SHA256: sha256Hex(files[BindingOutputName]), Bytes: int64(len(files[BindingOutputName]))},
	}
	if !reflect.DeepEqual(report.Outputs, wantOutputs) {
		return summary, errors.New("publication provenance output byte references are incomplete or drifted")
	}

	materials, err := loadGenerationMaterials(root)
	if err != nil {
		return summary, err
	}
	attestationModel, err := CatalogAttestationModel(materials.baseCatalogBytes, materials.approvedScaleBytes)
	if err != nil {
		return summary, err
	}
	wantCandidate, err := BuildCatalogCandidate(materials.baseCatalogBytes, materials.approvedScaleBytes,
		report.CatalogAttestation.Datasource.SchemaDigest)
	if err != nil || !bytes.Equal(wantCandidate.Bytes(), files[CatalogOutputName]) ||
		wantCandidate.SHA256() != logical.SHA256 || binding.CatalogSHA256 != logical.SHA256 {
		return summary, errors.New("publication Catalog bytes are not the exact live-attested base plus approved C2 merge")
	}
	if err := validateCatalogAttestation(report.CatalogAttestation, logical, attestationModel); err != nil {
		return summary, err
	}
	if err := finalv5dataset.ValidateBenchmarkAgreement(report.DatasetAgreement); err != nil {
		return summary, fmt.Errorf("validate retained full Dataset agreement: %w", err)
	}
	if report.DatasetAgreement.Observed.SHA256 != binding.DatasetSHA256 ||
		report.DatasetAgreement.Reference.SHA256 != binding.DatasetSHA256 {
		return summary, errors.New("retained Dataset agreement does not bind the generated binding")
	}
	if err := validateLiveObservation(report.LiveObservation, materials, binding); err != nil {
		return summary, err
	}
	if report.LiveObservation.Database != report.CatalogAttestation.Datasource.Database ||
		report.LiveObservation.User != report.CatalogAttestation.Datasource.User {
		return summary, errors.New("publication live observation and Catalog attestation identify different datasource sessions")
	}
	oracleOptions := finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: publicationOracleMaxInMemoryMembers,
	}
	wantScaleOutcomes, err := finalv5binding.GenerateScaleOutcomeCandidateExpectations(
		logical.SHA256, oracleOptions)
	if err != nil {
		return summary, fmt.Errorf("rederive Scale Outcome expectations from retained Catalog: %w", err)
	}
	wantBinding, wantBindingBytes, err := finalv5binding.BuildCompleteBinding(finalv5binding.CompleteBindingInput{
		DatasetSHA256:      report.DatasetAgreement.Reference.SHA256,
		DatasetProbeSHA256: report.LiveObservation.DatasetProbe.ResultSHA256,
		Catalog:            logical,
		ScaleOutcomes:      wantScaleOutcomes,
		ScaleManifests:     materials.scaleManifests,
		ArtifactRuntime:    materials.runtime,
		ProvSQLManifests:   materials.provSQLManifests,
		OracleOptions:      oracleOptions,
	})
	if err != nil || !bytes.Equal(wantBindingBytes, files[BindingOutputName]) || !reflect.DeepEqual(wantBinding, binding) {
		return summary, errors.New("publication binding bytes differ from the exact source-controlled 12/6/105 model")
	}
	wantInputs, err := buildInputInventory(materials, wantBinding, report.LiveObservation)
	if err != nil || !reflect.DeepEqual(report.Inputs, wantInputs) {
		return summary, errors.New("publication provenance input inventory is incomplete or drifted")
	}
	wantAlgebra, err := buildSetAlgebra(materials.scaleManifests, report.LiveObservation.ObservationSHA256)
	if err != nil || !reflect.DeepEqual(report.SetAlgebra, wantAlgebra) {
		return summary, errors.New("publication provenance set algebra is incomplete or drifted")
	}
	wantBindingInputIdentity, err := buildBindingInputIdentity(binding.FileSHA256, logical.SHA256, binding.DatasetSHA256,
		binding.DatasetProbeSHA256, report.LiveObservation.ObservationSHA256, report.SetAlgebra.SHA256,
		report.Inputs.CellInputSetSHA256)
	if err != nil || !reflect.DeepEqual(report.BindingInputIdentity, wantBindingInputIdentity) {
		return summary, errors.New("publication binding-input identity is incomplete or drifted")
	}
	outputs := append([]OutputEvidence(nil), wantOutputs...)
	outputs = append(outputs, OutputEvidence{Name: ProvenanceOutputName,
		SHA256: sha256Hex(files[ProvenanceOutputName]), Bytes: int64(len(files[ProvenanceOutputName]))})
	return GenerationSummary{Version: GeneratorVersion, Status: ReviewStatus,
		OutputDirectory: filepath.Clean(outputDirectory), Outputs: outputs,
		ScaleCells:    len(binding.Section.Scale.DependencyE2E),
		ArtifactCells: len(binding.Section.Artifact.ResultHeavy), ProvSQLCells: len(binding.Section.ProvSQL.TaskGate),
		BindingInputIdentitySHA256: report.BindingInputIdentity.SHA256,
		SetAlgebraSHA256:           report.SetAlgebra.SHA256}, nil
}

func readClosedOutputFiles(directory string, names []string) (map[string][]byte, error) {
	if err := ValidateClosedOutputDirectory(directory, names); err != nil {
		return nil, err
	}
	result := make(map[string][]byte, len(names))
	for _, name := range names {
		path := filepath.Join(filepath.Clean(directory), name)
		before, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("reopen publication output %q", name)
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("reopen publication output %q", name)
		}
		value, readErr := io.ReadAll(io.LimitReader(file, outputFileMaxBytes+1))
		opened, statErr := file.Stat()
		closeErr := file.Close()
		after, lstatErr := os.Lstat(path)
		if readErr != nil || statErr != nil || closeErr != nil || lstatErr != nil ||
			!os.SameFile(before, opened) || !os.SameFile(opened, after) ||
			!after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 || after.Mode().Perm() != outputFileMode ||
			len(value) == 0 || len(value) > outputFileMaxBytes || int64(len(value)) != after.Size() {
			return nil, fmt.Errorf("publication output %q changed or became unsafe while validating", name)
		}
		result[name] = value
	}
	return result, nil
}

func validateCatalogAttestation(value CatalogAttestation, logical, fixedModel *catalog.Catalog) error {
	if logical == nil || fixedModel == nil || len(logical.Sources) != 1 || len(fixedModel.Sources) != 1 ||
		value.QueryExecMode != "simple_protocol" || value.PreparedStatementCount != 0 ||
		!generatedSHA256(value.Datasource.SchemaDigest) {
		return errors.New("publication Catalog attestation is incomplete")
	}
	built, err := catalogschema.Build(logical)
	if err != nil {
		return err
	}
	fixed, err := catalogschema.Build(fixedModel)
	if err != nil {
		return err
	}
	source, fixedSource := logical.Sources[0], fixedModel.Sources[0]
	sourceWithoutDigest, fixedSourceWithoutDigest := source, fixedSource
	sourceWithoutDigest.SchemaDigest, fixedSourceWithoutDigest.SchemaDigest = "", ""
	if !reflect.DeepEqual(sourceWithoutDigest, fixedSourceWithoutDigest) ||
		!reflect.DeepEqual(built.Entries, fixed.Entries) || built.Count != fixed.Count || built.Digest != fixed.Digest ||
		value.Datasource.DatasourceID != fixedSource.DatasourceID || value.Datasource.Database != fixedSource.Database ||
		value.Datasource.User != fixedSource.User || value.Datasource.PostgreSQLMajorVersion != 16 ||
		value.Datasource.PostgreSQLMajorVersion != fixedSource.PostgreSQLMajorVersion ||
		value.Datasource.SchemaDigest != source.SchemaDigest || value.ExpectedSchemaEntries != fixed.Count ||
		value.ExpectedSchemaListSHA256 != fixed.Digest {
		return errors.New("publication Catalog attestation differs from the exact emitted Catalog surface")
	}
	return nil
}

func validateLiveObservation(value LiveObservation, materials generationMaterials,
	binding finalv5binding.Binding) error {
	if value.Version != liveObservationVersion || value.QueryExecMode != "simple_protocol" ||
		value.TransactionIsolation != "repeatable_read_read_only" || value.PostgreSQLServerVersionNum != "160014" ||
		value.PreparedStatementsBefore != 0 || value.PreparedStatementsAfter != 0 || value.QueryCount != 135 ||
		len(value.Queries) != 135 || !generatedSHA256(value.SessionIdentitySHA256) ||
		value.SessionProbeSQLSHA256 != sha256Hex([]byte(liveSessionProbeSQL)) ||
		value.DatasetProbe.SourcePath != datasetProbeContractPath ||
		!generatedSHA256(value.DatasetProbe.ResultSHA256) ||
		value.DatasetProbe.ResultSHA256 != binding.DatasetProbeSHA256 {
		return errors.New("publication live observation header is incomplete or drifted")
	}
	started, startErr := time.Parse(time.RFC3339Nano, value.StartedAtUTC)
	completed, completeErr := time.Parse(time.RFC3339Nano, value.CompletedAtUTC)
	if startErr != nil || completeErr != nil || completed.Before(started) {
		return errors.New("publication live observation has an invalid run interval")
	}
	probeSHA, err := materials.runtime.DatasetProbeSourceSHA256()
	if err != nil || probeSHA != value.DatasetProbe.SourceSHA256 {
		return errors.New("publication live Dataset probe source identity drifted")
	}
	wantQueries, err := expectedQueryObservations(materials, binding)
	if err != nil || !reflect.DeepEqual(value.Queries, wantQueries) {
		return errors.New("publication live query observations differ from the fixed 135-query closure")
	}
	wantDigest, err := liveObservationDigest(value)
	if err != nil || !generatedSHA256(value.ObservationSHA256) || wantDigest != value.ObservationSHA256 {
		return errors.New("publication live observation aggregate identity drifted")
	}
	return nil
}

func expectedQueryObservations(materials generationMaterials,
	binding finalv5binding.Binding) ([]QueryObservation, error) {
	result := make([]QueryObservation, 0, 135)
	candidateSHA, err := materials.runtime.ContractSHA256(scaleCandidateDirectPath)
	if err != nil || candidateSHA != finalv5oracle.ExposureScaleCandidateDirectQuerySHA256 {
		return nil, errors.New("fixed Scale candidate Direct query drifted")
	}
	historySHA, err := materials.runtime.ContractSHA256(scaleHistoryDirectPath)
	if err != nil || historySHA != finalv5oracle.ExposureScaleHistoryDirectQuerySHA256 {
		return nil, errors.New("fixed Scale history Direct query drifted")
	}
	scaleByCell := make(map[string][]finalv5oracle.ExposureScaleManifestArtifact, 12)
	for _, artifact := range materials.scaleManifests {
		scaleByCell[artifact.Manifest.Scale] = append(scaleByCell[artifact.Manifest.Scale], artifact)
	}
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		pair := scaleByCell[spec.Scale]
		if len(pair) != 2 {
			return nil, errors.New("fixed Scale oracle pair is incomplete")
		}
		inputs := []DigestReference{{Path: pathJoinOracle(pair[0].RelativePath), SHA256: pair[0].SHA256},
			{Path: pathJoinOracle(pair[1].RelativePath), SHA256: pair[1].SHA256}}
		candidate, err := manifestResultSummary(pair[0].Manifest)
		if err != nil {
			return nil, err
		}
		result = append(result, QueryObservation{Workload: "scale/dependency-e2e", Cell: spec.Scale,
			Role: "candidate", QuerySHA256: candidateSHA, OracleInputs: inputs,
			Expected: candidate, Actual: candidate, Matched: true})
		history, err := finalv5oracle.ExposureScaleHistoryResultSummary(spec.Scale)
		if err != nil {
			return nil, err
		}
		result = append(result, QueryObservation{Workload: "scale/dependency-e2e", Cell: spec.Scale,
			Role: "history", QuerySHA256: historySHA, OracleInputs: inputs,
			Expected: history, Actual: history, Matched: true})
	}
	artifactCells, err := materials.runtime.ArtifactCells()
	if err != nil || len(artifactCells) != 6 {
		return nil, errors.New("fixed Artifact closure is incomplete")
	}
	for _, cell := range artifactCells {
		query, err := materials.runtime.QueryContract(cell)
		if err != nil {
			return nil, err
		}
		manifest, manifestSHA, err := materials.runtime.OracleManifest(cell)
		if err != nil {
			return nil, err
		}
		expected, err := manifestResultSummary(manifest)
		if err != nil {
			return nil, err
		}
		result = append(result, QueryObservation{Workload: "artifact/result-heavy", Cell: cell.Identity.Scale,
			Role: "direct", QuerySHA256: query.Direct.SQLSHA256,
			OracleInputs: []DigestReference{{Path: pathJoinContract(cell.OracleManifestPath), SHA256: manifestSHA}},
			Expected:     expected, Actual: expected, Matched: true})
	}
	provByKey := make(map[string]finalv5oracle.ProvSQLManifestArtifact, 105)
	for _, artifact := range materials.provSQLManifests {
		provByKey[artifact.Manifest.BindingKey] = artifact
	}
	for _, cell := range finalv5oracle.ProvSQLNonceJoinCells() {
		artifact := provByKey[cell.BindingKey]
		business, err := provsqlfixture.BusinessSQL(cell.Scale, cell.Nonce)
		if err != nil {
			return nil, err
		}
		expected, err := manifestResultSummary(artifact.Manifest)
		if err != nil {
			return nil, err
		}
		result = append(result, QueryObservation{Workload: "provsql/nonce-join-group", Cell: cell.BindingKey,
			Role: "direct", QuerySHA256: provsqlfixture.SHA256String(business),
			OracleInputs: []DigestReference{{Path: pathJoinOracle(artifact.RelativePath), SHA256: artifact.SHA256}},
			Expected:     expected, Actual: expected, Matched: true})
	}
	return result, nil
}

func containsPlaceholder(value []byte) bool {
	upper := bytes.ToUpper(value)
	// NOT_GENERATED is allowed only as the exact, source-backed pre-generation
	// C2 state inside InputInventory; report equality checks enforce that narrow
	// context. Emitted identities themselves are typed digests and rederived.
	return bytes.Contains(upper, []byte("PLACEHOLDER")) ||
		bytes.Contains(value, []byte(strings.Repeat("0", 64)))
}
