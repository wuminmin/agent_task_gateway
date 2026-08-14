// Package finalv5contracts verifies the source-controlled Final-V5 author
// contracts without importing TaskGate execution or exposure implementations.
package finalv5contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

const (
	schemaVersion       = 1
	contractVersion     = "taskgate-final-v5-author-contract-v1"
	authorApproved      = "AUTHOR_APPROVED_FOR_IMPLEMENTATION"
	notApproved         = "NOT_APPROVED"
	notGenerated        = "NOT_GENERATED"
	workloadManifestSHA = "c5a921581dd8ab3e43d940504c5c0e537b913cc6107f78116ca91650fa1aaee7"

	// Reviewed contract releases. v1.1 corrects the int4 overflow in the
	// Result-heavy Dataset Generator; see contracts/AMENDMENT-v1.1.md. v1.3 is a
	// syntax-only erratum: the benchmark probe used the reserved keyword
	// COLLATION as a bare CTE identifier and could not parse at all; see
	// contracts/AMENDMENT-v1.3.md. v1.4 replaces an unsatisfiable observer gate
	// with closed-world statement accounting; see contracts/AMENDMENT-v1.4.md.
	// It changes gate code only: every indexed artifact is byte-identical to
	// v1.3.
	contractReleaseV1  = "final-v5-contracts-v1"
	contractReleaseV11 = "final-v5-contracts-v1.1"
	contractReleaseV12 = "final-v5-contracts-v1.2"
	contractReleaseV13 = "final-v5-contracts-v1.3"
	contractReleaseV14 = "final-v5-contracts-v1.4"
	contractReleaseV15 = "final-v5-contracts-v1.5"
	contractReleaseV16 = "final-v5-contracts-v1.6"
	contractReleaseV17 = "final-v5-contracts-v1.7"
	contractReleaseV18 = "final-v5-contracts-v1.8"
	contractReleaseV19 = "final-v5-contracts-v1.9"
)

var (
	requiredCellFields = []string{"workload", "scale", "mode", "product", "publication", "query", "direct", "bdg", "setup", "measured", "expected", "oracle", "negative", "claims", "status"}
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type cell struct {
	Workload    string          `json:"workload"`
	Scale       string          `json:"scale"`
	Mode        string          `json:"mode"`
	Product     json.RawMessage `json:"product"`
	Publication json.RawMessage `json:"publication"`
	Query       json.RawMessage `json:"query"`
	Direct      json.RawMessage `json:"direct"`
	BDG         json.RawMessage `json:"bdg"`
	Setup       json.RawMessage `json:"setup"`
	Measured    json.RawMessage `json:"measured"`
	Expected    json.RawMessage `json:"expected"`
	Oracle      json.RawMessage `json:"oracle"`
	Negative    json.RawMessage `json:"negative"`
	Claims      json.RawMessage `json:"claims"`
	Status      json.RawMessage `json:"status"`
}

type protocolMatrix struct {
	Source                       string `json:"source"`
	WorkloadManifestSHA256       string `json:"workload_manifest_sha256"`
	BytesAndLabelsUnchanged      bool   `json:"bytes_and_labels_unchanged"`
	ExpectedExpandedCellCount    int    `json:"expected_expanded_cell_count"`
	ScaleExtremeDefaultCellCount *int   `json:"scale_extreme_default_cell_count,omitempty"`
}

type baselineDocument struct {
	SchemaVersion                   int             `json:"schema_version"`
	ContractVersion                 string          `json:"contract_version"`
	Status                          string          `json:"status"`
	ResearchDesignStatus            string          `json:"research_design_status"`
	ExactGeneratedBytesFreezeStatus string          `json:"exact_generated_bytes_freeze_status"`
	ExperimentID                    string          `json:"experiment_id"`
	ProtocolProfile                 string          `json:"protocol_profile"`
	ProtocolMatrix                  protocolMatrix  `json:"protocol_matrix"`
	References                      json.RawMessage `json:"references"`
	RequiredCellFields              []string        `json:"required_cell_fields"`
	WorkloadSpecs                   json.RawMessage `json:"workload_specs"`
	Cells                           []cell          `json:"cells"`
}

type scaleDocument struct {
	SchemaVersion                   int             `json:"schema_version"`
	ContractVersion                 string          `json:"contract_version"`
	Status                          string          `json:"status"`
	ResearchDesignStatus            string          `json:"research_design_status"`
	ExactGeneratedBytesFreezeStatus string          `json:"exact_generated_bytes_freeze_status"`
	ExperimentID                    string          `json:"experiment_id"`
	ProtocolProfile                 string          `json:"protocol_profile"`
	ProtocolMatrix                  protocolMatrix  `json:"protocol_matrix"`
	References                      json.RawMessage `json:"references"`
	RequiredCellFields              []string        `json:"required_cell_fields"`
	DependencyE2EDesign             json.RawMessage `json:"dependency_e2e_design"`
	OutcomeMerkleDesign             json.RawMessage `json:"outcome_merkle_design"`
	Cells                           []cell          `json:"cells"`
	ScaleExtremeConflictAppendix    json.RawMessage `json:"scale_extreme_conflict_appendix"`
}

type artifactDocument struct {
	SchemaVersion                   int             `json:"schema_version"`
	ContractVersion                 string          `json:"contract_version"`
	Status                          string          `json:"status"`
	ResearchDesignStatus            string          `json:"research_design_status"`
	ExactGeneratedBytesFreezeStatus string          `json:"exact_generated_bytes_freeze_status"`
	ExperimentID                    string          `json:"experiment_id"`
	ProtocolProfile                 string          `json:"protocol_profile"`
	ProtocolMatrix                  protocolMatrix  `json:"protocol_matrix"`
	References                      json.RawMessage `json:"references"`
	RequiredCellFields              []string        `json:"required_cell_fields"`
	IdentityRule                    string          `json:"identity_rule"`
	ArtifactBoundary                json.RawMessage `json:"artifact_boundary"`
	Cells                           []cell          `json:"cells"`
}

type protocolWorkload struct {
	ID     string   `yaml:"id"`
	Scales []string `yaml:"scales"`
	Modes  []string `yaml:"modes"`
}

type protocolProfile struct {
	Workloads []protocolWorkload `yaml:"workloads"`
}

type workloadProtocol struct {
	SchemaVersion int                        `yaml:"schema_version"`
	Profiles      map[string]protocolProfile `yaml:"profiles"`
}

type orderedCell struct {
	Workload string
	Scale    string
	Mode     string
}

type indexArtifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type indexReferences struct {
	ProtocolSHA256                            string `json:"protocol_sha256"`
	WorkloadManifestSHA256                    string `json:"workload_manifest_sha256"`
	ProtocolAndMatrixBytesUnchangedByContract bool   `json:"protocol_and_matrix_bytes_unchanged_by_this_contract"`
}

type indexDocument struct {
	SchemaVersion int    `json:"schema_version"`
	IndexVersion  string `json:"index_version"`
	// ContractRelease names the reviewed release these exact bytes belong to.
	// Amended bytes must never continue to call themselves the release they
	// superseded, so the index identifies itself rather than relying on a tag.
	ContractRelease                 string          `json:"contract_release"`
	SupersedesContractRelease       string          `json:"supersedes_contract_release"`
	Amendment                       string          `json:"amendment"`
	Status                          string          `json:"status"`
	DigestStatus                    string          `json:"digest_status"`
	ExactGeneratedBytesFreezeStatus string          `json:"exact_generated_bytes_freeze_status"`
	HashLockedReferences            indexReferences `json:"hash_locked_references"`
	Artifacts                       []indexArtifact `json:"artifacts"`
	Rule                            string          `json:"rule"`
}

// VerifyRepository validates the complete source contract rooted at root.
// root is the repository root, not evaluation/final-v5-wsl2.
func VerifyRepository(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	evaluationRoot := filepath.Join(root, "evaluation", "final-v5-wsl2")
	contractsRoot := filepath.Join(evaluationRoot, "contracts")

	baseline, baselineBytes, err := loadBaseline(filepath.Join(contractsRoot, "baseline-v1.json"))
	if err != nil {
		return fmt.Errorf("baseline contract: %w", err)
	}
	scale, scaleBytes, err := loadScale(filepath.Join(contractsRoot, "scale-v1.json"))
	if err != nil {
		return fmt.Errorf("scale contract: %w", err)
	}
	artifact, artifactBytes, err := loadArtifact(filepath.Join(contractsRoot, "artifact-v1.json"))
	if err != nil {
		return fmt.Errorf("artifact contract: %w", err)
	}
	if err := validateExperimentHeader("baseline", baseline.SchemaVersion, baseline.ContractVersion, baseline.Status,
		baseline.ResearchDesignStatus, baseline.ExactGeneratedBytesFreezeStatus, baseline.ExperimentID,
		baseline.ProtocolProfile, baseline.ProtocolMatrix, baseline.RequiredCellFields, len(baseline.Cells), 58); err != nil {
		return err
	}
	if err := validateExperimentHeader("scale", scale.SchemaVersion, scale.ContractVersion, scale.Status,
		scale.ResearchDesignStatus, scale.ExactGeneratedBytesFreezeStatus, scale.ExperimentID,
		scale.ProtocolProfile, scale.ProtocolMatrix, scale.RequiredCellFields, len(scale.Cells), 60); err != nil {
		return err
	}
	if scale.ProtocolMatrix.ScaleExtremeDefaultCellCount == nil || *scale.ProtocolMatrix.ScaleExtremeDefaultCellCount != 0 {
		return errors.New("scale protocol matrix must exclude scale-extreme from the default 60 cells")
	}
	if err := validateExperimentHeader("artifact", artifact.SchemaVersion, artifact.ContractVersion, artifact.Status,
		artifact.ResearchDesignStatus, artifact.ExactGeneratedBytesFreezeStatus, artifact.ExperimentID,
		artifact.ProtocolProfile, artifact.ProtocolMatrix, artifact.RequiredCellFields, len(artifact.Cells), 6); err != nil {
		return err
	}

	for name, value := range map[string][]byte{"baseline": baselineBytes, "scale": scaleBytes, "artifact": artifactBytes} {
		if err := rejectJSONReference(value, "$ref"); err != nil {
			return fmt.Errorf("%s contract: %w", name, err)
		}
	}
	for name, cells := range map[string][]cell{"baseline": baseline.Cells, "scale": scale.Cells, "artifact": artifact.Cells} {
		if err := validateCells(name, cells); err != nil {
			return err
		}
	}
	if err := validateBaselineOrdering(baseline); err != nil {
		return err
	}

	protocol, err := loadWorkloadProtocol(filepath.Join(evaluationRoot, "protocol", "workloads-v1.yaml"))
	if err != nil {
		return fmt.Errorf("workload protocol: %w", err)
	}
	if err := compareProfile("baseline", baseline.Cells, protocol); err != nil {
		return err
	}
	if err := compareProfile("scale", scale.Cells, protocol); err != nil {
		return err
	}
	if err := compareProfile("artifact", artifact.Cells, protocol); err != nil {
		return err
	}

	if err := validateScale(scale); err != nil {
		return err
	}
	if err := validateArtifactIdentity(baseline, artifact); err != nil {
		return err
	}
	if err := validateQueryPaths(evaluationRoot, baseline.Cells, scale.Cells, artifact.Cells); err != nil {
		return err
	}
	if err := validateSupplementalContracts(contractsRoot); err != nil {
		return err
	}
	if err := validateCatalogCandidate(evaluationRoot); err != nil {
		return err
	}
	if err := validateIndex(root, evaluationRoot); err != nil {
		return err
	}
	return nil
}

func loadBaseline(path string) (baselineDocument, []byte, error) {
	var result baselineDocument
	value, err := decodeStrictJSONFile(path, &result)
	return result, value, err
}

func loadScale(path string) (scaleDocument, []byte, error) {
	var result scaleDocument
	value, err := decodeStrictJSONFile(path, &result)
	return result, value, err
}

func loadArtifact(path string) (artifactDocument, []byte, error) {
	var result artifactDocument
	value, err := decodeStrictJSONFile(path, &result)
	return result, value, err
}

func validateExperimentHeader(name string, schema int, version, status, research, freeze, experiment, profile string,
	matrix protocolMatrix, fields []string, actualCells, expectedCells int) error {
	if schema != schemaVersion || version != contractVersion {
		return fmt.Errorf("%s contract version is not frozen v1", name)
	}
	if status != authorApproved || research != authorApproved || freeze != notApproved {
		return fmt.Errorf("%s approval/freeze status is invalid", name)
	}
	if experiment != name || profile != name {
		return fmt.Errorf("%s experiment/profile identity is invalid", name)
	}
	if matrix.Source != "protocol/workloads-v1.yaml#profiles."+name || matrix.WorkloadManifestSHA256 != workloadManifestSHA ||
		!matrix.BytesAndLabelsUnchanged || matrix.ExpectedExpandedCellCount != expectedCells || actualCells != expectedCells {
		return fmt.Errorf("%s protocol matrix or cell count drifted", name)
	}
	if !reflect.DeepEqual(fields, requiredCellFields) {
		return fmt.Errorf("%s required_cell_fields drifted", name)
	}
	return nil
}

func validateCells(experiment string, cells []cell) error {
	seen := map[orderedCell]bool{}
	for index, current := range cells {
		identity := orderedCell{current.Workload, current.Scale, current.Mode}
		if identity.Workload == "" || identity.Scale == "" || identity.Mode == "" || seen[identity] {
			return fmt.Errorf("%s cell %d has empty or duplicate identity", experiment, index)
		}
		seen[identity] = true
		for field, value := range map[string]json.RawMessage{
			"product": current.Product, "publication": current.Publication, "query": current.Query,
			"direct": current.Direct, "bdg": current.BDG, "setup": current.Setup, "measured": current.Measured,
			"expected": current.Expected, "oracle": current.Oracle, "negative": current.Negative,
			"claims": current.Claims, "status": current.Status,
		} {
			if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return fmt.Errorf("%s cell %s/%s/%s omits %s", experiment, current.Workload, current.Scale, current.Mode, field)
			}
		}
		for field, raw := range map[string]json.RawMessage{"product": current.Product, "publication": current.Publication,
			"query": current.Query, "direct": current.Direct, "bdg": current.BDG, "setup": current.Setup,
			"measured": current.Measured, "expected": current.Expected, "oracle": current.Oracle,
			"claims": current.Claims, "status": current.Status} {
			if _, err := decodeRawObject(raw); err != nil {
				return fmt.Errorf("%s cell %s/%s/%s %s: %w", experiment, current.Workload, current.Scale, current.Mode, field, err)
			}
		}
		var negative []string
		if err := decodeStrictJSON(current.Negative, &negative); err != nil || len(negative) == 0 {
			return fmt.Errorf("%s cell %s/%s/%s negative checks are invalid", experiment, current.Workload, current.Scale, current.Mode)
		}
		if err := validateCellDigestState(current); err != nil {
			return fmt.Errorf("%s cell %s/%s/%s: %w", experiment, current.Workload, current.Scale, current.Mode, err)
		}
	}
	return nil
}

func validateCellDigestState(current cell) error {
	expected, err := decodeRawObject(current.Expected)
	if err != nil {
		return err
	}
	for key, value := range expected {
		if strings.HasSuffix(key, "_sha256") && value != nil {
			return fmt.Errorf("expected %s must be null before generation/review", key)
		}
	}
	if stringValue(expected, "digest_generation_status") != notGenerated || stringValue(expected, "digest_review_status") != notApproved {
		return errors.New("expected digest generation/review status is invalid")
	}
	oracle, err := decodeRawObject(current.Oracle)
	if err != nil {
		return err
	}
	if oracle["manifest_sha256"] != nil || stringValue(oracle, "generation_status") != notGenerated || stringValue(oracle, "review_status") != notApproved {
		return errors.New("oracle manifest digest/status is invalid")
	}
	if stringValue(oracle, "policy") != "contracts/oracle-policy-v1.json" {
		return errors.New("cell does not bind the registered oracle policy")
	}
	manifestPath := stringValue(oracle, "oracle_manifest_path")
	if !strings.HasPrefix(manifestPath, "oracle-manifests/") || !safeRelativePath(manifestPath) {
		return errors.New("oracle_manifest_path is not a safe future manifest path")
	}
	status, err := decodeRawObject(current.Status)
	if err != nil {
		return err
	}
	if stringValue(status, "research_design") != authorApproved || stringValue(status, "generated_digest_review") != notGenerated ||
		stringValue(status, "exact_generated_bytes_freeze") != notApproved || stringValue(status, "implementation") == "" {
		return errors.New("cell approval/implementation/freeze status is invalid")
	}
	return nil
}

func validateBaselineOrdering(document baselineDocument) error {
	canonicalWorkloads := map[string]bool{"S2": true, "S4": true, "S5": true}
	queryOrderWorkloads := map[string]bool{"S1": true, "S3": true, "S6": true}
	for _, current := range document.Cells {
		query, err := decodeRawObject(current.Query)
		if err != nil {
			return fmt.Errorf("baseline cell %s/%s/%s query: %w", current.Workload, current.Scale, current.Mode, err)
		}
		ordering, orderingPresent := query["result_ordering"]
		totalOrder, totalOrderPresent := query["total_order_required"].(bool)
		switch {
		case canonicalWorkloads[current.Workload]:
			if !totalOrderPresent || totalOrder || !orderingPresent || ordering != "canonical_typed_row_lexicographic_v1" {
				return fmt.Errorf("Baseline %s must disable query total order and use canonical_typed_row_lexicographic_v1", current.Workload)
			}
		case queryOrderWorkloads[current.Workload]:
			if !totalOrderPresent || !totalOrder || !orderingPresent || ordering != "query_order_v1" {
				return fmt.Errorf("Baseline %s must retain explicit query order", current.Workload)
			}
		default:
			return fmt.Errorf("Baseline has unexpected workload %q", current.Workload)
		}
	}
	workloadSpecs, err := decodeRawObject(document.WorkloadSpecs)
	if err != nil {
		return fmt.Errorf("Baseline workload specs: %w", err)
	}
	s4, ok := objectValue(workloadSpecs, "S4")
	if !ok {
		return errors.New("Baseline S4 workload spec is missing")
	}
	viewContract, ok := objectValue(s4, "root_view_contract")
	if !ok || !validPendingRootViewContract(viewContract) {
		return errors.New("Baseline S4 root View-contract component digest state is invalid")
	}
	return nil
}

func validPendingRootViewContract(viewContract map[string]any) bool {
	return stringValue(viewContract, "profile_version") == catalog.ViewContractV1 &&
		viewContract["definition_digest"] == nil && viewContract["dependency_digest"] == nil &&
		viewContract["canonical_plan_digest"] == nil && viewContract["interface_digest"] == nil &&
		stringValue(viewContract, "digest_generation_status") == "PENDING_LIVE_POSTGRESQL_GENERATION" &&
		stringValue(viewContract, "digest_review_status") == notApproved
}

func loadWorkloadProtocol(path string) (workloadProtocol, error) {
	file, err := os.Open(path)
	if err != nil {
		return workloadProtocol{}, err
	}
	defer file.Close()
	return decodeWorkloadProtocol(file)
}

func decodeWorkloadProtocol(reader io.Reader) (workloadProtocol, error) {
	value, err := io.ReadAll(io.LimitReader(reader, maxContractBytes+1))
	if err != nil {
		return workloadProtocol{}, err
	}
	if len(value) > maxContractBytes {
		return workloadProtocol{}, fmt.Errorf("workload protocol exceeds %d bytes", maxContractBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(value))
	decoder.KnownFields(true)
	var result workloadProtocol
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return result, errors.New("workload protocol contains trailing YAML")
	}
	if result.SchemaVersion != 2 {
		return result, errors.New("workload protocol schema_version is not 2")
	}
	return result, nil
}

func expandProtocolProfile(name string, protocol workloadProtocol) ([]orderedCell, error) {
	profile, present := protocol.Profiles[name]
	if !present {
		return nil, fmt.Errorf("%s protocol profile is missing", name)
	}
	if name == "" || len(profile.Workloads) == 0 {
		return nil, fmt.Errorf("%s protocol profile is incomplete", name)
	}
	expected := make([]orderedCell, 0)
	seen := map[orderedCell]bool{}
	for _, workload := range profile.Workloads {
		if workload.ID == "" || len(workload.Scales) == 0 || len(workload.Modes) == 0 {
			return nil, fmt.Errorf("%s protocol profile is incomplete", name)
		}
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				entry := orderedCell{workload.ID, scale, mode}
				if entry.Scale == "" || entry.Mode == "" {
					return nil, fmt.Errorf("%s protocol profile is incomplete", name)
				}
				if seen[entry] {
					return nil, fmt.Errorf("%s protocol profile duplicates a cell", name)
				}
				seen[entry] = true
				expected = append(expected, entry)
			}
		}
	}
	return expected, nil
}

func compareProfile(name string, cells []cell, protocol workloadProtocol) error {
	expected, err := expandProtocolProfile(name, protocol)
	if err != nil {
		return err
	}
	actual := make([]orderedCell, len(cells))
	for index, current := range cells {
		actual[index] = orderedCell{current.Workload, current.Scale, current.Mode}
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("%s ordered cells differ from workloads-v1.yaml", name)
	}
	return nil
}

func rejectJSONReference(value []byte, forbidden string) error {
	decoded, err := decodeAny(value)
	if err != nil {
		return err
	}
	var walk func(any) bool
	walk = func(current any) bool {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == forbidden || walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	if walk(decoded) {
		return fmt.Errorf("forbidden JSON reference key %q is present", forbidden)
	}
	return nil
}

func validateScale(document scaleDocument) error {
	dependencyDesign, err := decodeRawObject(document.DependencyE2EDesign)
	if err != nil {
		return fmt.Errorf("scale dependency design: %w", err)
	}
	if stringValue(dependencyDesign, "source_product") != "final_v5_exposure_scale" ||
		mustInt(dependencyDesign, "rows_available") != 414000 ||
		mustInt(dependencyDesign, "facts_per_retained_row") != 5 {
		return errors.New("scale dependency design constants drifted")
	}
	outcomeDesign, err := decodeRawObject(document.OutcomeMerkleDesign)
	if err != nil {
		return fmt.Errorf("scale outcome design: %w", err)
	}
	if err := validateOutcomeDesign(outcomeDesign); err != nil {
		return err
	}

	dependencyCount, outcomeCount := 0, 0
	for index := range document.Cells {
		current := document.Cells[index]
		switch current.Workload {
		case "dependency-e2e":
			dependencyCount++
			if err := validateDependencyCell(current); err != nil {
				return fmt.Errorf("scale cell %s/%s/%s: %w", current.Workload, current.Scale, current.Mode, err)
			}
		case "outcome-merkle":
			outcomeCount++
			if err := validateOutcomeCell(current); err != nil {
				return fmt.Errorf("scale cell %s/%s/%s: %w", current.Workload, current.Scale, current.Mode, err)
			}
		default:
			return fmt.Errorf("scale cell %d has unexpected workload %q", index, current.Workload)
		}
	}
	if dependencyCount != 24 || outcomeCount != 36 {
		return fmt.Errorf("scale workload counts drifted: dependency=%d outcome=%d", dependencyCount, outcomeCount)
	}
	return nil
}

func validateOutcomeDesign(design map[string]any) error {
	if mustInt(design, "measured_samples") != int64(finalv5oracle.OutcomeMeasuredSamples) ||
		mustInt(design, "warmups") != int64(finalv5oracle.OutcomeWarmupSamples) ||
		stringValue(design, "schedule_version") != finalv5oracle.OutcomeOverlapScheduleVersion ||
		mustInt(design, "seed") != 20260801 {
		return fmt.Errorf("scale outcome design schedule constants drifted: samples=%d warmups=%d version=%q seed=%d",
			mustInt(design, "measured_samples"), mustInt(design, "warmups"), stringValue(design, "schedule_version"), mustInt(design, "seed"))
	}
	tables := []struct {
		key    string
		values map[string]int64
	}{
		{"x1_samples_with_overlap", map[string]int64{"o0": 0, "o50": 15, "o90": 27, "o100": 30}},
		{"x100_overlap_per_sample", map[string]int64{"o0": 0, "o50": 50, "o90": 90, "o100": 100}},
		{"x10k_overlap_per_sample", map[string]int64{"o0": 0, "o50": 5000, "o90": 9000, "o100": 10000}},
	}
	for _, table := range tables {
		actual, ok := objectValue(design, table.key)
		if !ok || len(actual) != len(table.values) {
			return fmt.Errorf("outcome design %s is invalid", table.key)
		}
		for key, expected := range table.values {
			if mustInt(actual, key) != expected {
				return fmt.Errorf("outcome design %s.%s drifted", table.key, key)
			}
		}
	}
	return nil
}

func validateDependencyCell(current cell) error {
	if current.Mode != "novel" && current.Mode != "semantic_replay" {
		return fmt.Errorf("invalid dependency mode %q", current.Mode)
	}
	query, err := decodeRawObject(current.Query)
	if err != nil {
		return err
	}
	parameters, ok := objectValue(query, "parameters")
	if !ok {
		return errors.New("query parameters are missing")
	}
	m := mustInt(parameters, "candidate_member_max")
	lower := mustInt(parameters, "history_member_lower_exclusive")
	upper := mustInt(parameters, "history_member_upper_inclusive")
	percent := mustInt(parameters, "overlap_percent")
	if !memberOf(percent, 0, 50, 90, 100) {
		return fmt.Errorf("invalid overlap percentage %d", percent)
	}
	expected, err := decodeRawObject(current.Expected)
	if err != nil {
		return err
	}
	n := mustInt(expected, "candidate_dependency_cardinality")
	if !memberOf(n, 10000, 100000, 1035000) || n%5 != 0 || m != n/5 {
		return fmt.Errorf("candidate N/M constants drifted: N=%d M=%d", n, m)
	}
	kFacts := n * percent / 100
	kRows := m * percent / 100
	checks := map[string]int64{
		"row_count":                        1,
		"column_count":                     1,
		"retained_candidate_rows":          m,
		"retained_existing_rows":           m,
		"candidate_dependency_cardinality": n,
		"existing_dependency_cardinality":  n,
		"overlap_dependency_cardinality":   kFacts,
		"novel_dependency_cardinality":     n - kFacts,
		"union_dependency_cardinality":     2*n - kFacts,
		"release_candidate_cardinality":    1,
		"outcome_candidate_cardinality":    5,
	}
	for key, value := range checks {
		if mustInt(expected, key) != value {
			return fmt.Errorf("Dependency N/K/2N-K invariant failed for %s", key)
		}
	}
	if lower != m-kRows || upper != 2*m-kRows ||
		stringValue(query, "candidate_interval") != fmt.Sprintf("(0,%d]", m) ||
		stringValue(query, "history_interval") != fmt.Sprintf("(%d,%d]", lower, upper) ||
		!mustBool(query, "distinct_query_identities") || !mustBool(query, "total_order_required") ||
		stringValue(query, "result_ordering") != "query_order_v1" {
		return errors.New("dependency interval, query order, or distinct-query identity drifted")
	}
	setup, err := decodeRawObject(current.Setup)
	if err != nil {
		return err
	}
	measured, err := decodeRawObject(current.Measured)
	if err != nil {
		return err
	}
	if mustInt(setup, "warmups") != 5 || mustInt(measured, "samples") != 30 {
		return errors.New("dependency measurement schedule drifted")
	}
	return nil
}

func validateOutcomeCell(current cell) error {
	if current.Mode != "merkle_control" {
		return fmt.Errorf("invalid outcome mode %q", current.Mode)
	}
	query, err := decodeRawObject(current.Query)
	if err != nil {
		return err
	}
	parameters, ok := objectValue(query, "parameters")
	if !ok {
		return errors.New("query parameters are missing")
	}
	root := mustInt(parameters, "root_cardinality")
	candidate := mustInt(parameters, "candidate_cardinality")
	percent := mustInt(parameters, "overlap_percent")
	if !memberOf(root, 10000, 100000, 1000000) || !memberOf(candidate, 1, 100, 10000) ||
		!memberOf(percent, 0, 50, 90, 100) {
		return errors.New("outcome root/candidate/overlap dimension drifted")
	}
	if mustInt(parameters, "measured_samples") != int64(finalv5oracle.OutcomeMeasuredSamples) ||
		stringValue(parameters, "schedule_version") != finalv5oracle.OutcomeOverlapScheduleVersion ||
		mustInt(parameters, "seed") != 20260801 {
		return errors.New("outcome registered measured schedule drifted")
	}
	setup, err := decodeRawObject(current.Setup)
	if err != nil {
		return err
	}
	measured, err := decodeRawObject(current.Measured)
	if err != nil {
		return err
	}
	if mustInt(setup, "warmups") != int64(finalv5oracle.OutcomeWarmupSamples) ||
		stringValue(setup, "warmup_schedule") != "DETERMINISTIC_DOMAIN_SEPARATED_AND_EXCLUDED" ||
		mustInt(measured, "samples") != int64(finalv5oracle.OutcomeMeasuredSamples) || mustBool(measured, "include_warmups") {
		return errors.New("outcome warmups are not explicitly excluded from the measured schedule")
	}
	expected, err := decodeRawObject(current.Expected)
	if err != nil {
		return err
	}
	totalMemberships := candidate * int64(finalv5oracle.OutcomeMeasuredSamples)
	targetOverlap := totalMemberships * percent / 100
	checks := map[string]int64{
		"root_cardinality":                     root,
		"candidate_cardinality":                candidate,
		"target_overlap_memberships_across_30": targetOverlap,
		"target_novel_memberships_across_30":   totalMemberships - targetOverlap,
	}
	for key, value := range checks {
		if mustInt(expected, key) != value {
			return fmt.Errorf("outcome schedule invariant failed for %s", key)
		}
	}
	if candidate == 1 {
		x1 := map[int64]int64{0: 0, 50: 15, 90: 27, 100: 30}[percent]
		if mustInt(expected, "x1_samples_with_overlap") != x1 || expected["overlap_per_measured_sample"] != nil {
			return errors.New("Outcome x1 schedule must be exactly 0/15/27/30")
		}
	} else {
		if expected["x1_samples_with_overlap"] != nil || mustInt(expected, "overlap_per_measured_sample") != candidate*percent/100 {
			return errors.New("Outcome x100/x10k per-sample overlap drifted")
		}
	}
	return nil
}

func validateArtifactIdentity(baseline baselineDocument, artifact artifactDocument) error {
	type shape struct {
		rows       int64
		columns    int64
		projection string
	}
	shapes := map[string]shape{
		"100x4": {100, 4, "x4"}, "10k-x4": {10000, 4, "x4"}, "100k-x4": {100000, 4, "x4"},
		"100x16": {100, 16, "x16"}, "10k-x16": {10000, 16, "x16"}, "100k-x16": {100000, 16, "x16"},
	}
	baselineS6 := map[string]cell{}
	for _, current := range baseline.Cells {
		if current.Workload == "S6" && current.Mode == "novel" {
			baselineS6[current.Scale] = current
		}
	}
	if len(baselineS6) != len(shapes) || len(artifact.Cells) != len(shapes) {
		return errors.New("Artifact/Baseline S6 shape count drifted")
	}
	seen := map[string]bool{}
	for _, current := range artifact.Cells {
		shape, ok := shapes[current.Scale]
		if current.Workload != "result-heavy" || current.Mode != "novel" || !ok || seen[current.Scale] {
			return fmt.Errorf("invalid Artifact shape identity %s/%s/%s", current.Workload, current.Scale, current.Mode)
		}
		seen[current.Scale] = true
		baselineCell, ok := baselineS6[current.Scale]
		if !ok {
			return fmt.Errorf("Artifact shape %s has no Baseline S6 novel identity", current.Scale)
		}
		if !rawJSONEqual(current.Product, baselineCell.Product) || !rawJSONEqual(current.Publication, baselineCell.Publication) {
			return fmt.Errorf("Artifact shape %s Product/Publication differs from Baseline S6", current.Scale)
		}
		artifactQuery, _ := decodeRawObject(current.Query)
		baselineQuery, _ := decodeRawObject(baselineCell.Query)
		artifactTotalOrder, artifactTotalOrderPresent := artifactQuery["total_order_required"].(bool)
		artifactOrdering, artifactOrderingPresent := artifactQuery["result_ordering"]
		artifactParameters, artifactParametersOK := objectValue(artifactQuery, "parameters")
		baselineParameters, baselineParametersOK := objectValue(baselineQuery, "parameters")
		if !artifactParametersOK || !baselineParametersOK || !reflect.DeepEqual(artifactParameters, baselineParameters) ||
			mustInt(artifactParameters, "rows") != shape.rows || stringValue(artifactParameters, "projection") != shape.projection ||
			stringValue(artifactQuery, "template") != stringValue(baselineQuery, "template") ||
			stringValue(artifactQuery, "baseline_identity") != "baseline/S6/"+current.Scale+"/novel" ||
			!artifactTotalOrderPresent || !artifactTotalOrder || !artifactOrderingPresent || artifactOrdering != "query_order_v1" {
			return fmt.Errorf("Artifact shape %s query parameters/template differ from Baseline S6", current.Scale)
		}
		artifactDirect, _ := decodeRawObject(current.Direct)
		baselineDirect, _ := decodeRawObject(baselineCell.Direct)
		artifactBDG, _ := decodeRawObject(current.BDG)
		baselineBDG, _ := decodeRawObject(baselineCell.BDG)
		if stringValue(artifactDirect, "template") != stringValue(baselineDirect, "template") ||
			stringValue(artifactBDG, "template") != stringValue(baselineBDG, "template") {
			return fmt.Errorf("Artifact shape %s execution templates differ from Baseline S6", current.Scale)
		}
		artifactExpected, _ := decodeRawObject(current.Expected)
		baselineExpected, _ := decodeRawObject(baselineCell.Expected)
		if mustInt(artifactExpected, "row_count") != shape.rows || mustInt(artifactExpected, "column_count") != shape.columns {
			return fmt.Errorf("Artifact shape %s does not have the registered row/column shape", current.Scale)
		}
		for _, key := range []string{"row_count", "column_count", "normalized_schema_sha256", "canonical_result_sha256"} {
			if !reflect.DeepEqual(artifactExpected[key], baselineExpected[key]) {
				return fmt.Errorf("Artifact shape %s expected %s differs from Baseline S6", current.Scale, key)
			}
		}
	}
	return nil
}

func validateQueryPaths(evaluationRoot string, groups ...[]cell) error {
	referenced := map[string]bool{}
	for _, cells := range groups {
		for _, current := range cells {
			for _, raw := range []json.RawMessage{current.Query, current.Direct, current.BDG} {
				value, err := decodeAny(raw)
				if err != nil {
					return err
				}
				if err := collectTemplatePaths(value, referenced); err != nil {
					return fmt.Errorf("cell %s/%s/%s: %w", current.Workload, current.Scale, current.Mode, err)
				}
			}
		}
	}
	for path := range referenced {
		full, err := regularRelativeFile(evaluationRoot, path)
		if err != nil {
			return fmt.Errorf("query template %q: %w", path, err)
		}
		if filepath.Ext(full) == ".json" {
			value, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			if _, err := decodeAny(value); err != nil {
				return fmt.Errorf("structured query template %q: %w", path, err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(evaluationRoot, "sql", "contracts"))
	if err != nil {
		return err
	}
	actual := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("unexpected directory in sql/contracts: %s", entry.Name())
		}
		actual[filepath.ToSlash(filepath.Join("sql", "contracts", entry.Name()))] = true
	}
	if !reflect.DeepEqual(referenced, actual) {
		return fmt.Errorf("query contract paths do not cover sql/contracts exactly: referenced=%v files=%v", sortedKeys(referenced), sortedKeys(actual))
	}
	return nil
}

func collectTemplatePaths(value any, result map[string]bool) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if strings.Contains(strings.ToLower(key), "template") {
				path, ok := child.(string)
				if !ok || !strings.HasPrefix(path, "sql/") {
					return fmt.Errorf("template field %q is not an evaluation SQL path", key)
				}
				result[path] = true
			}
			if err := collectTemplatePaths(child, result); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := collectTemplatePaths(child, result); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSupplementalContracts(contractsRoot string) error {
	type supplemental struct {
		name string
		path string
	}
	files := []supplemental{
		{"benchmark products", filepath.Join(contractsRoot, "benchmark-products-v1.json")},
		{"oracle policy", filepath.Join(contractsRoot, "oracle-policy-v1.json")},
		{"result normalization", filepath.Join(contractsRoot, "result-normalization-v1.json")},
	}
	objects := map[string]map[string]any{}
	for _, file := range files {
		value, err := os.ReadFile(file.path)
		if err != nil {
			return err
		}
		decoded, err := decodeAny(value)
		if err != nil {
			return fmt.Errorf("%s contract: %w", file.name, err)
		}
		if err := rejectJSONReference(value, "$ref"); err != nil {
			return fmt.Errorf("%s contract: %w", file.name, err)
		}
		object, ok := decoded.(map[string]any)
		if !ok {
			return fmt.Errorf("%s contract is not an object", file.name)
		}
		if mustInt(object, "schema_version") != schemaVersion || stringValue(object, "contract_version") != contractVersion ||
			stringValue(object, "status") != authorApproved || stringValue(object, "exact_generated_bytes_freeze_status") != notApproved {
			return fmt.Errorf("%s approval/freeze header is invalid", file.name)
		}
		if research, present := object["research_design_status"]; present && research != authorApproved {
			return fmt.Errorf("%s research design is not author-approved", file.name)
		}
		objects[file.name] = object
	}
	if err := validateBenchmarkProducts(objects["benchmark products"], filepath.Dir(contractsRoot)); err != nil {
		return err
	}
	manifest, ok := objectValue(objects["oracle policy"], "manifest")
	if !ok || manifest["generated_manifest_digests"] != nil ||
		stringValue(manifest, "generated_manifest_digest_status") != notGenerated ||
		stringValue(manifest, "generated_manifest_digest_review_status") != notApproved {
		return errors.New("oracle policy generated manifest digest state is invalid")
	}
	normalization := objects["result normalization"]
	if normalization["generated_contract_digest"] != nil || stringValue(normalization, "generated_contract_digest_status") != notGenerated ||
		stringValue(normalization, "generated_contract_digest_review_status") != notApproved {
		return errors.New("normalization generated digest state is invalid")
	}
	rowStream, ok := objectValue(normalization, "row_stream_encoding")
	if !ok {
		return errors.New("normalization row_stream_encoding is missing")
	}
	orderingModes, ok := objectValue(rowStream, "ordering_modes")
	if !ok || len(orderingModes) != 2 || stringValue(orderingModes, "query_order_v1") == "" ||
		stringValue(orderingModes, "canonical_typed_row_lexicographic_v1") == "" {
		return errors.New("normalization must register query_order_v1 and canonical_typed_row_lexicographic_v1")
	}
	return nil
}

func validateBenchmarkProducts(document map[string]any, evaluationRoot string) error {
	if stringValue(document, "contract_status") != authorApproved {
		return errors.New("benchmark product contract_status is not author-approved")
	}
	generator, ok := objectValue(document, "generator")
	if !ok || generator["generated_bytes_sha256"] != nil || generator["probe_output_sha256"] != nil ||
		stringValue(generator, "digest_generation_status") != notGenerated || stringValue(generator, "digest_review_status") != notApproved {
		return errors.New("benchmark generator digest state is invalid")
	}
	for _, key := range []string{"sql", "probe_sql"} {
		path := stringValue(generator, key)
		if _, err := regularRelativeFile(evaluationRoot, path); err != nil {
			return fmt.Errorf("benchmark generator %s: %w", key, err)
		}
	}
	probe, ok := objectValue(document, "probe_hash_semantics")
	if !ok || probe["binding_dataset_fingerprint_sha256"] != nil || stringValue(probe, "generation_status") != notGenerated ||
		stringValue(probe, "review_status") != notApproved {
		return errors.New("benchmark probe digest state is invalid")
	}
	binding, ok := objectValue(document, "catalog_binding")
	if !ok || binding["formal_campaign_catalog_sha256"] != nil ||
		stringValue(binding, "formal_campaign_catalog_digest_review_status") != notApproved {
		return errors.New("benchmark Catalog binding digest state is invalid")
	}
	products, ok := arrayValue(document, "products")
	if !ok || len(products) != 6 {
		return errors.New("benchmark products must contain the six registered products")
	}
	for _, rawProduct := range products {
		product, ok := rawProduct.(map[string]any)
		if !ok {
			return errors.New("benchmark product is not an object")
		}
		if artifacts, present := objectValue(product, "publication_artifacts"); present {
			for key, value := range artifacts {
				if strings.HasSuffix(key, "_sha256") && value != nil {
					return fmt.Errorf("benchmark product %s digest %s must be null", stringValue(product, "product_id"), key)
				}
			}
			if stringValue(artifacts, "digest_generation_status") != notGenerated || stringValue(artifacts, "digest_review_status") != notApproved {
				return fmt.Errorf("benchmark product %s publication digest state is invalid", stringValue(product, "product_id"))
			}
		}
		if layers, present := arrayValue(product, "view_layers"); present {
			if len(layers) != 4 {
				return errors.New("analytics depth-4 product must declare four view layers")
			}
			for index, rawLayer := range layers {
				layer, ok := rawLayer.(map[string]any)
				if !ok || mustInt(layer, "depth") != int64(index+1) || stringValue(layer, "relation") == "" || stringValue(layer, "operation") == "" {
					return errors.New("analytics view layer identity is invalid")
				}
			}
			viewContract, ok := objectValue(product, "root_view_contract")
			if !ok || !validPendingRootViewContract(viewContract) {
				return errors.New("analytics root View-contract component digest state is invalid")
			}
		}
	}
	return nil
}

func validateIndex(root, evaluationRoot string) error {
	var index indexDocument
	_, err := decodeStrictJSONFile(filepath.Join(evaluationRoot, "contracts", "index-v1.json"), &index)
	if err != nil {
		return fmt.Errorf("contract index: %w", err)
	}
	if index.SchemaVersion != schemaVersion || index.IndexVersion != "taskgate-final-v5-contract-index-v1" ||
		index.Status != authorApproved || index.DigestStatus != "REVIEW_CANDIDATE" || index.ExactGeneratedBytesFreezeStatus != notApproved {
		return errors.New("contract index status/version header is invalid")
	}
	if err := validateContractRelease(index, evaluationRoot); err != nil {
		return err
	}
	if !index.HashLockedReferences.ProtocolAndMatrixBytesUnchangedByContract ||
		index.HashLockedReferences.WorkloadManifestSHA256 != workloadManifestSHA {
		return errors.New("contract index hash-locked workload reference drifted")
	}
	protocolHash, err := fileSHA256(filepath.Join(root, "evaluation", "final-v5-wsl2", "protocol", "protocol-v1.yaml"))
	if err != nil {
		return err
	}
	workloadHash, err := fileSHA256(filepath.Join(root, "evaluation", "final-v5-wsl2", "protocol", "workloads-v1.yaml"))
	if err != nil {
		return err
	}
	if index.HashLockedReferences.ProtocolSHA256 != protocolHash || index.HashLockedReferences.WorkloadManifestSHA256 != workloadHash {
		return errors.New("contract index protocol/workload SHA-256 does not match source bytes")
	}
	expectedKinds := map[string]string{
		"contracts/baseline-v1.json":             "experiment_contract",
		"contracts/scale-v1.json":                "experiment_contract",
		"contracts/artifact-v1.json":             "experiment_contract",
		"contracts/benchmark-products-v1.json":   "product_contract",
		"contracts/oracle-policy-v1.json":        "oracle_policy",
		"contracts/profile-activation-v1.json":   "profile_activation_policy",
		"contracts/result-normalization-v1.json": "normalization_contract",
		"catalog/benchmark-contract-v1.yaml":     "catalog_candidate",
		"sql/datasets/benchmark-v1-generate.sql": "dataset_generator",
		"sql/datasets/benchmark-v1-probe.sql":    "dataset_probe",
	}
	queryEntries, err := os.ReadDir(filepath.Join(evaluationRoot, "sql", "contracts"))
	if err != nil {
		return err
	}
	for _, entry := range queryEntries {
		if !entry.IsDir() {
			expectedKinds[filepath.ToSlash(filepath.Join("sql", "contracts", entry.Name()))] = "query_template"
		}
	}
	if len(index.Artifacts) != len(expectedKinds) {
		return fmt.Errorf("contract index artifact count=%d, expected=%d", len(index.Artifacts), len(expectedKinds))
	}
	seen := map[string]bool{}
	for _, artifact := range index.Artifacts {
		expectedKind, ok := expectedKinds[artifact.Path]
		if !ok || seen[artifact.Path] || artifact.Kind != expectedKind || !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("contract index entry is invalid: kind=%q path=%q sha256=%q", artifact.Kind, artifact.Path, artifact.SHA256)
		}
		seen[artifact.Path] = true
		full, err := regularRelativeFile(evaluationRoot, artifact.Path)
		if err != nil {
			return fmt.Errorf("contract index path %q: %w", artifact.Path, err)
		}
		actual, err := fileSHA256(full)
		if err != nil {
			return err
		}
		if actual != artifact.SHA256 {
			return fmt.Errorf("contract index SHA-256 mismatch for %s: index=%s actual=%s", artifact.Path, artifact.SHA256, actual)
		}
	}
	return nil
}

// validateContractRelease keeps an amended contract from continuing to call
// itself the release it superseded. Amended bytes must name their own release,
// name what they replaced, and ship a source-controlled amendment record.
func validateContractRelease(index indexDocument, evaluationRoot string) error {
	if index.ContractRelease == "" {
		return errors.New("contract index does not name its contract release")
	}
	if index.ContractRelease == contractReleaseV1 {
		if index.SupersedesContractRelease != "" || index.Amendment != "" {
			return errors.New("the original contract release cannot supersede or amend another release")
		}
		return nil
	}
	// Amendments form a chain: each release names exactly the one it replaced
	// and ships its own record, so evidence can never attribute corrected bytes
	// to an earlier release.
	chain := map[string][2]string{
		contractReleaseV11: {contractReleaseV1, "contracts/AMENDMENT-v1.1.md"},
		contractReleaseV12: {contractReleaseV11, "contracts/AMENDMENT-v1.2.md"},
		contractReleaseV13: {contractReleaseV12, "contracts/AMENDMENT-v1.3.md"},
		contractReleaseV14: {contractReleaseV13, "contracts/AMENDMENT-v1.4.md"},
		contractReleaseV15: {contractReleaseV14, "contracts/AMENDMENT-v1.5.md"},
		contractReleaseV16: {contractReleaseV15, "contracts/AMENDMENT-v1.6.md"},
		contractReleaseV17: {contractReleaseV16, "contracts/AMENDMENT-v1.7.md"},
		contractReleaseV18: {contractReleaseV17, "contracts/AMENDMENT-v1.8.md"},
		contractReleaseV19: {contractReleaseV18, "contracts/AMENDMENT-v1.9.md"},
	}
	expected, reviewed := chain[index.ContractRelease]
	if !reviewed || index.SupersedesContractRelease != expected[0] {
		return fmt.Errorf("contract release %q is not a reviewed release", index.ContractRelease)
	}
	if index.Amendment != expected[1] {
		return errors.New("amended contract index does not point at its amendment record")
	}
	if _, err := regularRelativeFile(evaluationRoot, index.Amendment); err != nil {
		return fmt.Errorf("amendment record %s: %w", index.Amendment, err)
	}
	return nil
}

func validateCatalogCandidate(evaluationRoot string) error {
	path := filepath.Join(evaluationRoot, "catalog", "benchmark-contract-v1.yaml")
	loaded, err := catalog.Load(path)
	if err != nil {
		return fmt.Errorf("catalog.Load candidate: %w", err)
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	parsed, err := catalog.Parse(value)
	if err != nil {
		return fmt.Errorf("catalog.Parse candidate: %w", err)
	}
	if loaded.SHA256 == "" || loaded.SHA256 != parsed.SHA256 || !reflect.DeepEqual(loaded, parsed) {
		return errors.New("catalog.Load and catalog.Parse do not produce the same validated candidate")
	}
	requiredProducts := map[string]bool{
		"provsql_orders": false, "provsql_lineitem": false, "provsql_nonce": false,
		"final_v5_exposure_scale": false, "final_v5_analytics_depth4": false, "final_v5_result_heavy": false,
	}
	for _, product := range loaded.Products {
		if _, ok := requiredProducts[product.Name]; ok {
			requiredProducts[product.Name] = true
		}
	}
	for name, found := range requiredProducts {
		if !found {
			return fmt.Errorf("Catalog candidate omits benchmark Product %q", name)
		}
	}
	publications := map[string]bool{}
	for _, publication := range loaded.SnapshotPublications {
		publications[publication.Name] = true
	}
	for _, name := range []string{"final-v5-exposure-scale-v1", "final-v5-result-heavy-v1"} {
		if !publications[name] {
			return fmt.Errorf("Catalog candidate omits Publication %q", name)
		}
	}
	if err := validateCatalogTaskPolicies(loaded); err != nil {
		return err
	}
	budgetFound := false
	for _, budget := range loaded.BudgetProfiles {
		if budget.Name == "final-v5-benchmark-low-v1" {
			budgetFound = budget.MaxRows >= 100000 && budget.MaxInfluenceFacts >= 2070000
		}
	}
	if !budgetFound {
		return errors.New("Catalog candidate benchmark budget cannot cover registered result/dependency scales")
	}
	return nil
}

func validateCatalogTaskPolicies(candidate *catalog.Catalog) error {
	tests := []struct {
		name     string
		products []string
		budget   string
	}{
		{"orders", []string{"provsql_orders"}, "final-v5-benchmark-low-v1"},
		{"orders-lineitem", []string{"provsql_orders", "provsql_lineitem"}, "final-v5-benchmark-low-v1"},
		{"orders-lineitem-nonce", []string{"provsql_orders", "provsql_lineitem", "provsql_nonce"}, "final-v5-benchmark-low-v1"},
		{"exposure", []string{"final_v5_exposure_scale"}, "final-v5-benchmark-low-v1"},
		{"result-heavy", []string{"final_v5_result_heavy"}, "final-v5-benchmark-low-v1"},
		{"depth-4", []string{"final_v5_analytics_depth4"}, "final-v5-benchmark-low-v1"},
		{"expense-summary-control", []string{"expense_summary"}, "summary-manual-v5"},
	}
	for _, test := range tests {
		policy, err := candidate.ResolveTaskPolicy(test.products)
		if err != nil {
			return fmt.Errorf("Catalog task policy %s: %w", test.name, err)
		}
		if policy.BudgetProfile != test.budget || policy.ApprovalRoute.BudgetProfile != test.budget {
			return fmt.Errorf("Catalog task policy %s resolved budget %q, expected %q", test.name, policy.BudgetProfile, test.budget)
		}
	}
	return nil
}

func stringValue(object map[string]any, key string) string {
	value, _ := object[key].(string)
	return value
}

func mustInt(object map[string]any, key string) int64 {
	value, ok := object[key]
	if !ok {
		return -1 << 63
	}
	switch typed := value.(type) {
	case json.Number:
		result, err := strconv.ParseInt(string(typed), 10, 64)
		if err == nil {
			return result
		}
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed)
		}
	case int:
		return int64(typed)
	case int64:
		return typed
	}
	return -1 << 63
}

func mustBool(object map[string]any, key string) bool {
	value, _ := object[key].(bool)
	return value
}

func objectValue(object map[string]any, key string) (map[string]any, bool) {
	value, ok := object[key].(map[string]any)
	return value, ok
}

func arrayValue(object map[string]any, key string) ([]any, bool) {
	value, ok := object[key].([]any)
	return value, ok
}

func memberOf(value int64, allowed ...int64) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func rawJSONEqual(left, right json.RawMessage) bool {
	leftValue, leftErr := decodeAny(left)
	rightValue, rightErr := decodeAny(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftValue, rightValue)
}

func safeRelativePath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || filepath.IsAbs(path) || filepath.Clean(path) != filepath.FromSlash(path) {
		return false
	}
	return path != "." && path != ".." && !strings.HasPrefix(path, "../")
}

func regularRelativeFile(root, path string) (string, error) {
	if !safeRelativePath(path) {
		return "", errors.New("path is not a clean relative path")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(path))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("path resolves outside the evaluation root")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	return resolved, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sortedKeys[V any](values map[string]V) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
