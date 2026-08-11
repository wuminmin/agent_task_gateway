package finalv5binding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

const (
	exposureScaleProduct    = "final_v5_exposure_scale"
	resultHeavyProduct      = "final_v5_result_heavy"
	resultHeavyPublication  = "final-v5-result-heavy-v1"
	provSQLOrdersProduct    = "provsql_orders"
	provSQLLineitemProduct  = "provsql_lineitem"
	provSQLNonceProduct     = "provsql_nonce"
	generatedBindingVersion = 2
)

var (
	resultHeavyX4Columns  = []string{"row_id", "category", "amount", "event_date"}
	resultHeavyX16Columns = []string{
		"row_id", "category", "amount", "event_date", "sequence_no", "approved",
		"event_timestamp", "description", "quantity", "unit_price", "tax_amount",
		"settled_date", "processed_at", "region", "revision", "active",
	}
)

// CompleteBindingInput contains only exact identities and already materialized
// independent-oracle artifacts. BuildCompleteBinding re-verifies both closed
// manifest sets before using them. Task and SQL models are deliberately not
// caller inputs: this package owns the one fixed model used by schema v2.
type CompleteBindingInput struct {
	DatasetSHA256      string
	DatasetProbeSHA256 string
	Catalog            *catalog.Catalog
	ScaleOutcomes      ScaleOutcomeCandidateExpectations
	ScaleManifests     []finalv5oracle.ExposureScaleManifestArtifact
	ArtifactRuntime    *finalv5contracts.Runtime
	ProvSQLManifests   []finalv5oracle.ProvSQLManifestArtifact
	OracleOptions      finalv5oracle.StreamSetOptions
}

// ScaleOutcomeCandidateExpectations is an opaque bundle produced before any
// live publication observation. Its fields are deliberately private: callers
// can retain and pass the fixed-oracle result, but cannot assemble a bundle
// from production outputs after the run.
type ScaleOutcomeCandidateExpectations struct {
	catalogSHA256    string
	byCandidateFacts map[int64]BoundOutcomeCandidateExpectation
}

// GenerateScaleOutcomeCandidateExpectations fixes the three strict Scale
// Outcome candidate identities from the complete attested Catalog and the
// source-controlled oracle model. E1 calls this after Catalog attestation and
// before ObservePublicationClosure; BuildCompleteBinding later verifies and
// serializes this already established expectation without rerunning the oracle.
func GenerateScaleOutcomeCandidateExpectations(catalogSHA256 string,
	options finalv5oracle.StreamSetOptions) (ScaleOutcomeCandidateExpectations, error) {
	if !generatedDigest(catalogSHA256) {
		return ScaleOutcomeCandidateExpectations{}, errors.New("Scale Outcome expectations require a complete Catalog SHA-256")
	}
	result := ScaleOutcomeCandidateExpectations{
		catalogSHA256: catalogSHA256, byCandidateFacts: make(map[int64]BoundOutcomeCandidateExpectation, 3),
	}
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		if _, present := result.byCandidateFacts[spec.CandidateFacts]; present {
			continue
		}
		expected, err := buildScaleOutcomeCandidateExpectation(catalogSHA256, spec.CandidateFacts, options)
		if err != nil {
			return ScaleOutcomeCandidateExpectations{}, fmt.Errorf(
				"derive independent Scale Outcome candidate for %d Facts: %w", spec.CandidateFacts, err)
		}
		result.byCandidateFacts[spec.CandidateFacts] = expected
	}
	if err := result.validate(catalogSHA256); err != nil {
		return ScaleOutcomeCandidateExpectations{}, err
	}
	return result, nil
}

// BuildCompleteBinding constructs canonical schema-v2 bytes for the exact
// 12/6/105 private publication closure. It performs no database access and
// never accepts SQL, a production Fact identity, or a production derivation as
// input. Returned Binding identities are recomputed by Parse from returned
// bytes, so callers cannot serialize an unvalidated intermediate value.
func BuildCompleteBinding(input CompleteBindingInput) (Binding, []byte, error) {
	if err := validateCompleteBindingInput(input); err != nil {
		return Binding{}, nil, err
	}
	scaleManifests, err := verifyScaleManifestArtifacts(input.ScaleManifests, input.OracleOptions)
	if err != nil {
		return Binding{}, nil, fmt.Errorf("verify the exact Scale manifest closure: %w", err)
	}
	provSQLManifests, err := verifyProvSQLManifestArtifacts(input.ProvSQLManifests, input.OracleOptions)
	if err != nil {
		return Binding{}, nil, fmt.Errorf("verify the exact ProvSQL manifest closure: %w", err)
	}

	scale, err := buildScaleBinding(scaleManifests, input.ScaleOutcomes)
	if err != nil {
		return Binding{}, nil, err
	}
	artifact, err := buildArtifactBinding(input.ArtifactRuntime)
	if err != nil {
		return Binding{}, nil, err
	}
	provSQL, err := buildProvSQLBinding(provSQLManifests)
	if err != nil {
		return Binding{}, nil, err
	}
	document := bindingDocument{
		DatasetSHA256:      input.DatasetSHA256,
		DatasetProbeSHA256: input.DatasetProbeSHA256,
		CatalogSHA256:      input.Catalog.SHA256,
		Adapter: Section{SchemaVersion: generatedBindingVersion, Scale: scale,
			Artifact: artifact, ProvSQL: provSQL},
	}
	value, err := canonicalBindingDocument(document)
	if err != nil {
		return Binding{}, nil, err
	}
	parsed, err := Parse(value)
	if err != nil {
		return Binding{}, nil, fmt.Errorf("validate generated complete binding: %w", err)
	}
	if len(parsed.Section.Scale.DependencyE2E) != 12 || len(parsed.Section.Artifact.ResultHeavy) != 6 ||
		len(parsed.Section.ProvSQL.TaskGate) != 105 {
		return Binding{}, nil, errors.New("generated binding is not the exact 12/6/105 closure")
	}
	if err := ValidateAgainstCatalog(parsed, input.Catalog); err != nil {
		return Binding{}, nil, fmt.Errorf("validate generated binding against the complete Catalog: %w", err)
	}
	return parsed, value, nil
}

type bindingDocument struct {
	DatasetSHA256      string  `json:"dataset_sha256"`
	DatasetProbeSHA256 string  `json:"dataset_probe_sha256"`
	CatalogSHA256      string  `json:"catalog_sha256"`
	Adapter            Section `json:"final_v5_adapter_v2"`
}

func canonicalBindingDocument(document bindingDocument) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode canonical complete binding: %w", err)
	}
	return output.Bytes(), nil
}

func validateCompleteBindingInput(input CompleteBindingInput) error {
	for name, digest := range map[string]string{
		"dataset_sha256": input.DatasetSHA256, "dataset_probe_sha256": input.DatasetProbeSHA256,
	} {
		if !generatedDigest(digest) {
			return fmt.Errorf("complete binding %s is missing, a placeholder, or not SHA-256", name)
		}
	}
	if input.DatasetSHA256 == input.DatasetProbeSHA256 {
		return errors.New("typed Dataset identity and live scalar probe digest must be independent")
	}
	if input.Catalog == nil || !generatedDigest(input.Catalog.SHA256) {
		return errors.New("complete binding requires a parsed, non-placeholder Catalog")
	}
	if err := input.Catalog.Validate(); err != nil {
		return fmt.Errorf("complete binding Catalog is invalid: %w", err)
	}
	if input.Catalog.SHA256 == input.DatasetSHA256 || input.Catalog.SHA256 == input.DatasetProbeSHA256 {
		return errors.New("Catalog, typed Dataset, and live scalar probe identities must be independent")
	}
	if err := validateFixedCatalogRoutes(input.Catalog); err != nil {
		return err
	}
	if input.ArtifactRuntime == nil {
		return errors.New("complete binding requires the verified Contract runtime")
	}
	if len(input.ScaleManifests) != 24 {
		return fmt.Errorf("complete binding received %d Scale manifests; expected exactly 24", len(input.ScaleManifests))
	}
	if len(input.ProvSQLManifests) != 105 {
		return fmt.Errorf("complete binding received %d ProvSQL manifests; expected exactly 105", len(input.ProvSQLManifests))
	}
	reviewedDataset, err := input.ArtifactRuntime.DatasetIdentitySHA256()
	if err != nil {
		return fmt.Errorf("resolve the reviewed typed Dataset identity: %w", err)
	}
	if input.DatasetSHA256 != reviewedDataset {
		return errors.New("complete binding Dataset identity differs from the reviewed five-Product formula")
	}
	if err := validateContractRuntimeClosure(input.ArtifactRuntime); err != nil {
		return err
	}
	if err := input.ScaleOutcomes.validate(input.Catalog.SHA256); err != nil {
		return fmt.Errorf("complete binding strict Scale Outcome expectations are invalid: %w", err)
	}
	return nil
}

func validateFixedCatalogRoutes(source *catalog.Catalog) error {
	artifactX4, _ := FixedArtifactTask(4)
	artifactX16, _ := FixedArtifactTask(16)
	for _, target := range []struct {
		name     string
		products []string
	}{
		{"Scale", FixedScaleTask().DataProducts}, {"Artifact-x4", artifactX4.DataProducts},
		{"Artifact-x16", artifactX16.DataProducts}, {"ProvSQL", FixedProvSQLTask().DataProducts},
	} {
		if _, err := source.ResolveTaskPolicy(target.products); err != nil {
			return fmt.Errorf("complete Catalog cannot route the fixed %s task: %w", target.name, err)
		}
	}
	return nil
}

func validateContractRuntimeClosure(runtime *finalv5contracts.Runtime) error {
	contractCells, err := runtime.ContractWorkloadCells()
	if err != nil {
		return fmt.Errorf("load fixed Scale Contract cells: %w", err)
	}
	wantedScale := make(map[string]bool, 24)
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		for _, mode := range []string{finalv5oracle.ExposureScaleModeNovel,
			finalv5oracle.ExposureScaleModeSemanticReplay} {
			wantedScale[cell.Scale+"/"+mode] = true
		}
	}
	seenScale := make(map[string]bool, len(wantedScale))
	for _, cell := range contractCells {
		if cell.Identity.ExperimentID != "scale" || cell.Identity.WorkloadID != "dependency-e2e" {
			continue
		}
		key := cell.Identity.Scale + "/" + cell.Identity.Mode
		if !wantedScale[key] || seenScale[key] || len(cell.Products) != 1 || cell.Products[0] != exposureScaleProduct {
			return fmt.Errorf("Scale Contract cell %s is outside the exact 24-cell Product closure", cell.Identity)
		}
		seenScale[key] = true
	}
	if len(seenScale) != len(wantedScale) {
		return fmt.Errorf("Scale Contract runtime has %d dependency cells; expected exactly 24", len(seenScale))
	}

	provSQLCells, err := runtime.ProtocolProfileCells("provsql")
	if err != nil {
		return fmt.Errorf("load fixed ProvSQL protocol cells: %w", err)
	}
	wantedProvSQL := make(map[string]bool, 9)
	for _, scale := range []string{"1k", "10k", "45k"} {
		for _, mode := range []string{"direct", "provsql", "taskgate"} {
			wantedProvSQL[scale+"/"+mode] = true
		}
	}
	seenProvSQL := make(map[string]bool, len(wantedProvSQL))
	for _, cell := range provSQLCells {
		key := cell.Scale + "/" + cell.Mode
		if cell.ExperimentID != "provsql" || cell.WorkloadID != "nonce-join-group" ||
			!wantedProvSQL[key] || seenProvSQL[key] {
			return fmt.Errorf("ProvSQL protocol cell %s is outside the exact 3-by-3 closure", cell)
		}
		seenProvSQL[key] = true
	}
	if len(seenProvSQL) != len(wantedProvSQL) {
		return fmt.Errorf("ProvSQL Contract runtime has %d protocol cells; expected exactly 9", len(seenProvSQL))
	}
	return nil
}

func generatedDigest(value string) bool {
	if !ValidDigest(value) {
		return false
	}
	// Repeated-nibble values cover the source-controlled all-zero
	// NOT_GENERATED sentinel and the obvious aaaa/bbbb test placeholders. None
	// is evidence of a generated identity.
	return strings.Trim(value, value[:1]) != ""
}

func verifyScaleManifestArtifacts(input []finalv5oracle.ExposureScaleManifestArtifact,
	options finalv5oracle.StreamSetOptions) ([]finalv5oracle.ExposureScaleManifestArtifact, error) {
	values := make(map[string][]byte, len(input))
	for _, artifact := range input {
		if artifact.RelativePath == "" || !generatedDigest(artifact.SHA256) {
			return nil, errors.New("Scale manifest artifact has no canonical path/SHA-256")
		}
		if _, duplicate := values[artifact.RelativePath]; duplicate {
			return nil, fmt.Errorf("Scale manifest artifact path %q is duplicated", artifact.RelativePath)
		}
		value, err := finalv5oracle.CanonicalManifest(artifact.Manifest)
		if err != nil {
			return nil, fmt.Errorf("canonicalize Scale manifest %s: %w", artifact.RelativePath, err)
		}
		if shaBytes(value) != artifact.SHA256 {
			return nil, fmt.Errorf("Scale manifest %s bytes differ from its supplied SHA-256", artifact.RelativePath)
		}
		values[artifact.RelativePath] = value
	}
	return finalv5oracle.VerifyExposureScaleDependencyManifestSet(values, options)
}

func verifyProvSQLManifestArtifacts(input []finalv5oracle.ProvSQLManifestArtifact,
	options finalv5oracle.StreamSetOptions) ([]finalv5oracle.ProvSQLManifestArtifact, error) {
	values := make(map[string][]byte, len(input))
	for _, artifact := range input {
		if artifact.RelativePath == "" || !generatedDigest(artifact.SHA256) {
			return nil, errors.New("ProvSQL manifest artifact has no canonical path/SHA-256")
		}
		if _, duplicate := values[artifact.RelativePath]; duplicate {
			return nil, fmt.Errorf("ProvSQL manifest artifact path %q is duplicated", artifact.RelativePath)
		}
		value, err := finalv5oracle.CanonicalManifest(artifact.Manifest)
		if err != nil {
			return nil, fmt.Errorf("canonicalize ProvSQL manifest %s: %w", artifact.RelativePath, err)
		}
		if shaBytes(value) != artifact.SHA256 {
			return nil, fmt.Errorf("ProvSQL manifest %s bytes differ from its supplied SHA-256", artifact.RelativePath)
		}
		values[artifact.RelativePath] = value
	}
	return finalv5oracle.VerifyProvSQLNonceJoinManifestSet(values, options)
}

func buildScaleBinding(manifests []finalv5oracle.ExposureScaleManifestArtifact,
	outcomes ScaleOutcomeCandidateExpectations) (*ScaleBinding, error) {
	byScale := make(map[string]map[string]finalv5oracle.OracleManifest, 12)
	for _, artifact := range manifests {
		manifest := artifact.Manifest
		if byScale[manifest.Scale] == nil {
			byScale[manifest.Scale] = make(map[string]finalv5oracle.OracleManifest, 2)
		}
		if _, duplicate := byScale[manifest.Scale][manifest.Mode]; duplicate {
			return nil, fmt.Errorf("verified Scale manifests duplicate %s/%s", manifest.Scale, manifest.Mode)
		}
		byScale[manifest.Scale][manifest.Mode] = manifest
	}
	binding := &ScaleBinding{DependencyE2E: make(map[string]DependencyCellBinding, 12), EnableOutcomeMerkle: true}
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		modes := byScale[spec.Scale]
		novel, novelPresent := modes[finalv5oracle.ExposureScaleModeNovel]
		replay, replayPresent := modes[finalv5oracle.ExposureScaleModeSemanticReplay]
		if len(modes) != 2 || !novelPresent || !replayPresent {
			return nil, fmt.Errorf("verified Scale closure lacks the novel/replay pair for %s", spec.Scale)
		}
		if err := requireSameScaleExpectation(novel, replay); err != nil {
			return nil, fmt.Errorf("verified Scale pair %s: %w", spec.Scale, err)
		}
		expected := novel.Expected
		if expected.RowCount == nil || expected.ColumnCount == nil ||
			expected.DependencyCandidateCardinality == nil || expected.ExistingCardinality == nil ||
			expected.UnionCardinality == nil {
			return nil, fmt.Errorf("verified Scale manifest %s omits a binding expectation", spec.Scale)
		}
		history, err := finalv5oracle.ExposureScaleHistoryResultSummary(spec.Scale)
		if err != nil {
			return nil, fmt.Errorf("derive independent Scale history result %s: %w", spec.Scale, err)
		}
		outcome, err := outcomes.forCandidateFacts(spec.CandidateFacts)
		if err != nil {
			return nil, fmt.Errorf("load pre-run Scale Outcome candidate %s: %w", spec.Scale, err)
		}
		memberWindow := spec.CandidateFacts / 5
		overlapWindow := spec.OverlapFacts / 5
		binding.DependencyE2E[spec.Scale] = DependencyCellBinding{
			Task: FixedScaleTask(),
			Candidate: BoundQueryExpectation{
				SQL: dependencyCandidateSQL(memberWindow), ExpectedRows: *expected.RowCount,
				ExpectedColumns: *expected.ColumnCount, ExpectedResultSHA256: expected.CanonicalResultSHA256,
				DependencyFacts:      *expected.DependencyCandidateCardinality,
				DependencySetSHA256:  expected.DependencyCandidateSetSHA256,
				ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1,
			},
			History: BoundQueryExpectation{
				SQL:          dependencyHistorySQL(memberWindow-overlapWindow, 2*memberWindow-overlapWindow),
				ExpectedRows: history.RowCount, ExpectedColumns: history.ColumnCount,
				ExpectedResultSHA256: history.CanonicalResultSHA256,
				DependencyFacts:      *expected.ExistingCardinality, DependencySetSHA256: expected.ExistingSetSHA256,
				ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1,
			},
			Union: BoundDependencySetExpectation{DependencyFacts: *expected.UnionCardinality,
				DependencySetSHA256: expected.UnionSetSHA256},
			OutcomeCandidate: BoundOutcomeCandidateExpectation{
				Cardinality:       outcome.Cardinality,
				Members:           append([]string(nil), outcome.Members...),
				OrdinarySetSHA256: outcome.OrdinarySetSHA256,
			},
		}
	}
	if len(byScale) != 12 || len(binding.DependencyE2E) != 12 {
		return nil, errors.New("verified Scale manifests do not reduce to the exact 12-cell binding closure")
	}
	if err := validateScaleBinding(binding); err != nil {
		return nil, fmt.Errorf("validate constructed Scale binding: %w", err)
	}
	return binding, nil
}

func (expected ScaleOutcomeCandidateExpectations) validate(catalogSHA256 string) error {
	if expected.catalogSHA256 != catalogSHA256 || !generatedDigest(expected.catalogSHA256) {
		return errors.New("pre-run Scale Outcome expectations differ from the complete Catalog")
	}
	wanted := make(map[int64]bool, 3)
	for _, spec := range finalv5oracle.ExposureScaleDependencyCells() {
		wanted[spec.CandidateFacts] = true
	}
	if len(expected.byCandidateFacts) != len(wanted) {
		return fmt.Errorf("pre-run Scale Outcome expectations contain %d candidate sizes; expected %d",
			len(expected.byCandidateFacts), len(wanted))
	}
	for candidateFacts := range wanted {
		outcome, present := expected.byCandidateFacts[candidateFacts]
		if !present {
			return fmt.Errorf("pre-run Scale Outcome expectations omit %d candidate Facts", candidateFacts)
		}
		if err := ValidateBoundOutcomeCandidate(outcome); err != nil {
			return fmt.Errorf("pre-run Scale Outcome expectation for %d Facts: %w", candidateFacts, err)
		}
	}
	for candidateFacts := range expected.byCandidateFacts {
		if !wanted[candidateFacts] {
			return fmt.Errorf("pre-run Scale Outcome expectations contain unfrozen size %d", candidateFacts)
		}
	}
	return nil
}

func (expected ScaleOutcomeCandidateExpectations) forCandidateFacts(candidateFacts int64) (
	BoundOutcomeCandidateExpectation, error) {
	outcome, present := expected.byCandidateFacts[candidateFacts]
	if !present {
		return BoundOutcomeCandidateExpectation{}, fmt.Errorf(
			"pre-run Scale Outcome expectations omit %d candidate Facts", candidateFacts)
	}
	outcome.Members = append([]string(nil), outcome.Members...)
	return outcome, nil
}

func buildScaleOutcomeCandidateExpectation(catalogSHA256 string, candidateFacts int64,
	options finalv5oracle.StreamSetOptions) (BoundOutcomeCandidateExpectation, error) {
	generated, err := finalv5oracle.GenerateExposureScaleOutcomeCandidate(finalv5oracle.ExposureScaleOutcomeRequest{
		CatalogSHA256: catalogSHA256, CandidateFacts: candidateFacts, SetOptions: options,
	})
	if err != nil {
		return BoundOutcomeCandidateExpectation{}, err
	}
	if generated.CandidateFacts != candidateFacts || generated.CandidateCardinality != 5 ||
		len(generated.Members) != 5 {
		return BoundOutcomeCandidateExpectation{}, errors.New("independent Scale Outcome candidate is not the exact five-member closure")
	}
	expected := BoundOutcomeCandidateExpectation{
		Cardinality:       generated.CandidateCardinality,
		Members:           append([]string(nil), generated.Members...),
		OrdinarySetSHA256: generated.CandidateSetSHA256,
	}
	if err := ValidateBoundOutcomeCandidate(expected); err != nil {
		return BoundOutcomeCandidateExpectation{}, fmt.Errorf("validate independent Scale Outcome candidate: %w", err)
	}
	return expected, nil
}

func requireSameScaleExpectation(left, right finalv5oracle.OracleManifest) error {
	left.Mode, right.Mode = "", ""
	if !reflect.DeepEqual(left, right) {
		return errors.New("novel and semantic replay manifests disagree outside mode")
	}
	return nil
}

func buildArtifactBinding(runtime *finalv5contracts.Runtime) (*ArtifactBinding, error) {
	cells, err := runtime.ArtifactCells()
	if err != nil {
		return nil, fmt.Errorf("load the exact Artifact runtime cells: %w", err)
	}
	wanted := map[string]struct {
		rows    int64
		columns int
	}{
		"100x4": {100, 4}, "10k-x4": {10_000, 4}, "100k-x4": {100_000, 4},
		"100x16": {100, 16}, "10k-x16": {10_000, 16}, "100k-x16": {100_000, 16},
	}
	if len(cells) != len(wanted) {
		return nil, fmt.Errorf("Artifact Contract runtime has %d cells; expected exactly 6", len(cells))
	}
	binding := &ArtifactBinding{ResultHeavy: make(map[string]ArtifactCellBinding, len(cells))}
	for _, cell := range cells {
		spec, present := wanted[cell.Identity.Scale]
		if !present || cell.Identity.ExperimentID != finalv5contracts.ArtifactExperimentID ||
			cell.Identity.WorkloadID != finalv5contracts.ArtifactWorkloadID || cell.Identity.Mode != "novel" ||
			cell.ExpectedRows != spec.rows || cell.ExpectedColumns != spec.columns ||
			len(cell.ProductIDs) != 1 || cell.ProductIDs[0] != resultHeavyProduct ||
			len(cell.PublicationIDs) != 1 || cell.PublicationIDs[0] != resultHeavyPublication {
			return nil, fmt.Errorf("Artifact Contract runtime cell %s is outside the exact six-cell closure", cell.Identity)
		}
		if _, duplicate := binding.ResultHeavy[cell.Identity.Scale]; duplicate {
			return nil, fmt.Errorf("Artifact Contract runtime duplicates scale %s", cell.Identity.Scale)
		}
		query, err := runtime.QueryContract(cell)
		if err != nil {
			return nil, fmt.Errorf("load Artifact query contract %s: %w", cell.Identity, err)
		}
		manifest, _, err := runtime.OracleManifest(cell)
		if err != nil {
			return nil, fmt.Errorf("verify Artifact oracle manifest %s: %w", cell.Identity, err)
		}
		if manifest.Expected.RowCount == nil || manifest.Expected.ColumnCount == nil ||
			*manifest.Expected.RowCount != query.Rows || *manifest.Expected.ColumnCount != query.Columns {
			return nil, fmt.Errorf("Artifact runtime/oracle result shape differs for %s", cell.Identity)
		}
		task, err := FixedArtifactTask(query.Columns)
		if err != nil {
			return nil, err
		}
		wantColumns := task.Columns[resultHeavyProduct]
		if len(query.Schema) != len(wantColumns) {
			return nil, fmt.Errorf("Artifact query schema differs from the fixed task for %s", cell.Identity)
		}
		for index := range query.Schema {
			if query.Schema[index].Name != wantColumns[index] {
				return nil, fmt.Errorf("Artifact query schema differs from the fixed task for %s", cell.Identity)
			}
		}
		binding.ResultHeavy[cell.Identity.Scale] = ArtifactCellBinding{Task: task,
			Query: BoundResultExpectation{SQL: query.BDG.SQL, ExpectedRows: *manifest.Expected.RowCount,
				ExpectedColumns:      *manifest.Expected.ColumnCount,
				ExpectedResultSHA256: manifest.Expected.CanonicalResultSHA256,
				ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1}}
	}
	if err := validateArtifactBinding(binding); err != nil {
		return nil, fmt.Errorf("validate constructed Artifact binding: %w", err)
	}
	return binding, nil
}

func buildProvSQLBinding(manifests []finalv5oracle.ProvSQLManifestArtifact) (*ProvSQLBinding, error) {
	type resultIdentities struct {
		typedSHA256   string
		adapterSHA256 string
	}
	resultsByScale := make(map[string]resultIdentities, 3)
	binding := &ProvSQLBinding{
		FixtureVersion: provsqlfixture.Version, FixtureSQLSHA256: provsqlfixture.FixtureSQLSHA256(),
		EnableSQLSHA256: provsqlfixture.EnableSQLSHA256(), DatasetSHA256: provsqlfixture.ExpectedDatasetSHA256(),
		DatasetProbeSQLSHA256:         provsqlfixture.DatasetProbeSQLSHA256(),
		BusinessDatasetProbeSQLSHA256: provsqlfixture.BusinessDatasetProbeSQLSHA256(),
		Task:                          FixedProvSQLTask(), TaskGate: make(map[string]BoundQueryExpectation, 105),
	}
	for _, artifact := range manifests {
		manifest := artifact.Manifest
		cell, err := finalv5oracle.ParseProvSQLBindingKey(manifest.BindingKey)
		if err != nil || cell.Scale != manifest.Scale {
			return nil, fmt.Errorf("verified ProvSQL manifest %s has no exact fixed cell", artifact.RelativePath)
		}
		if _, duplicate := binding.TaskGate[cell.BindingKey]; duplicate {
			return nil, fmt.Errorf("verified ProvSQL manifests duplicate binding key %s", cell.BindingKey)
		}
		expected := manifest.Expected
		if expected.RowCount == nil || expected.ColumnCount == nil || expected.DependencyCandidateCardinality == nil {
			return nil, fmt.Errorf("verified ProvSQL manifest %s omits a binding expectation", artifact.RelativePath)
		}
		result, present := resultsByScale[cell.Scale]
		if !present {
			rows, err := provsqlfixture.ExpectedResultRows(cell.Scale)
			if err != nil {
				return nil, fmt.Errorf("load fixed ProvSQL result %s: %w", cell.Scale, err)
			}
			typed, err := finalv5oracle.CanonicalResult(finalv5oracle.ProvSQLResultSchema(), rows)
			if err != nil {
				return nil, fmt.Errorf("derive typed publication result identity %s: %w", cell.Scale, err)
			}
			adapterSHA256, err := canonicalAdapterResultHash(rows)
			if err != nil {
				return nil, fmt.Errorf("derive Adapter result identity %s: %w", cell.Scale, err)
			}
			result = resultIdentities{typedSHA256: typed.CanonicalResultSHA256, adapterSHA256: adapterSHA256}
			resultsByScale[cell.Scale] = result
		}
		if expected.CanonicalResultSHA256 != result.typedSHA256 {
			return nil, fmt.Errorf("verified ProvSQL manifest %s differs from the typed publication result oracle",
				artifact.RelativePath)
		}
		logical, err := provsqlfixture.LogicalSQL(cell.Scale, cell.Nonce)
		if err != nil {
			return nil, fmt.Errorf("render fixed ProvSQL query %s: %w", cell.BindingKey, err)
		}
		binding.TaskGate[cell.BindingKey] = BoundQueryExpectation{
			SQL: logical, ExpectedRows: *expected.RowCount, ExpectedColumns: *expected.ColumnCount,
			ExpectedResultSHA256: result.adapterSHA256,
			DependencyFacts:      *expected.DependencyCandidateCardinality,
			DependencySetSHA256:  expected.DependencyCandidateSetSHA256,
			ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1,
		}
	}
	if len(binding.TaskGate) != 105 {
		return nil, fmt.Errorf("verified ProvSQL manifests reduce to %d cells; expected exactly 105", len(binding.TaskGate))
	}
	if err := ValidateProvSQLBinding(binding); err != nil {
		return nil, fmt.Errorf("validate constructed ProvSQL binding: %w", err)
	}
	return binding, nil
}

// FixedScaleTask returns a detached copy of the one Scale task model. The
// fixed publication relations and complete mandatory scope prevent a caller
// from widening the approved surface while retaining the same query text.
func FixedScaleTask() BoundTaskRequest {
	return BoundTaskRequest{
		Objective:    "IEEE TKDE Final-V5 Scale dependency-e2e",
		DataProducts: []string{exposureScaleProduct},
		Columns: map[string][]string{exposureScaleProduct: {
			"member_rank", "metric", "family_id", "partition_key",
		}},
		Scopes:            map[string][]string{"partition_key": {"1"}},
		VisibleRelation:   "reporting.final_v5_exposure_scale",
		CompanionRelation: "taskgate_ordinal.final_v5_exposure_scale_v1",
	}
}

// FixedArtifactTask returns the exact x4 or x16 result-heavy task surface.
func FixedArtifactTask(columnCount int) (BoundTaskRequest, error) {
	var columns []string
	switch columnCount {
	case 4:
		columns = resultHeavyX4Columns
	case 16:
		columns = resultHeavyX16Columns
	default:
		return BoundTaskRequest{}, fmt.Errorf("Artifact column count %d is not one of the fixed x4/x16 projections", columnCount)
	}
	return BoundTaskRequest{
		Objective:         "IEEE TKDE Final-V5 Artifact result-heavy",
		DataProducts:      []string{resultHeavyProduct},
		Columns:           map[string][]string{resultHeavyProduct: append([]string(nil), columns...)},
		Scopes:            map[string][]string{"category": {"alpha", "beta", "gamma", "delta"}},
		VisibleRelation:   "reporting.final_v5_result_heavy",
		CompanionRelation: "taskgate_ordinal.final_v5_result_heavy_v1",
	}, nil
}

// FixedProvSQLTask returns the one three-Product nonce-join task surface.
func FixedProvSQLTask() BoundTaskRequest {
	return BoundTaskRequest{
		Objective:    "IEEE TKDE Final-V5 ProvSQL nonce-join-group",
		DataProducts: []string{provSQLOrdersProduct, provSQLLineitemProduct, provSQLNonceProduct},
		Columns: map[string][]string{
			provSQLOrdersProduct:   {"orderkey", "status", "partition_key"},
			provSQLLineitemProduct: {"orderkey", "linenumber", "extendedprice", "partition_key"},
			provSQLNonceProduct:    {"nonce_id", "partition_key"},
		},
		Scopes:            map[string][]string{"partition_key": {"1"}},
		VisibleRelation:   "reporting.provsql_orders",
		CompanionRelation: "taskgate_ordinal.provsql_orders_v1",
	}
}
