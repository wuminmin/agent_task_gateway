package experiment

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
)

const ProfileCampaignSampleV1Record = "taskgate-final-v5-profile-campaign-sample-v1"

// ProfileCampaignSampleV1 is the runner-owned envelope for a profile-split
// campaign. The nested Sample keeps its original v1, v2, or v3 wire;
// campaign metadata therefore does not silently revise any retained Sample
// schema.
type ProfileCampaignSampleV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Record        string `json:"record"`
	CampaignClass string `json:"campaign_class"`
	Sample        Sample `json:"sample"`
}

func NewProfileCampaignSampleV1(campaignClass string, sample Sample) ProfileCampaignSampleV1 {
	return ProfileCampaignSampleV1{
		SchemaVersion: 1,
		Record:        ProfileCampaignSampleV1Record,
		CampaignClass: campaignClass,
		Sample:        sample,
	}
}

func (record ProfileCampaignSampleV1) Validate() error {
	if record.SchemaVersion != 1 || record.Record != ProfileCampaignSampleV1Record {
		return errors.New("profile campaign sample has an unknown record version")
	}
	if record.CampaignClass != "pilot" && record.CampaignClass != "publication" {
		return errors.New("profile campaign sample has an invalid runner-owned class")
	}
	if err := record.Sample.Validate(); err != nil {
		return fmt.Errorf("nested sample: %w", err)
	}
	if record.Sample.PublicationEligible != (record.CampaignClass == "publication") {
		return errors.New("profile campaign sample eligibility differs from its runner-owned class")
	}
	return nil
}

// ReadProfileCampaignSamples decodes only the runner-owned P30 envelope. It
// deliberately does not accept legacy flat Sample JSONL; retained pre-fix
// evidence must be wrapped explicitly in memory by the offline audit path.
func ReadProfileCampaignSamples(path string) ([]ProfileCampaignSampleV1, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var records []ProfileCampaignSampleV1
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var record ProfileCampaignSampleV1
		if err := StrictJSON(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// WrapRetainedSamplesForProfileCampaignAudit models the runner stamp in memory
// for immutable pre-fix evidence. It never writes or changes the source bytes.
func WrapRetainedSamplesForProfileCampaignAudit(samples []Sample, campaignClass string) []ProfileCampaignSampleV1 {
	records := make([]ProfileCampaignSampleV1, len(samples))
	for index := range samples {
		records[index] = NewProfileCampaignSampleV1(campaignClass, samples[index])
	}
	return records
}

// ValidateProfileCampaignExperimentGate is the launcher-level terminal gate.
// Deep evidence validation remains owned by each adapter/runner validator; this
// gate checks the cross-experiment terminal envelope that the former one-line
// jq predicate incorrectly treated as uniform.
func ValidateProfileCampaignExperimentGate(experimentID string, selectedCells []string,
	records []ProfileCampaignSampleV1, samplesPerCell int) error {
	return validateProfileCampaignExperimentGate("pilot", experimentID, selectedCells, records, samplesPerCell, nil, "")
}

// ValidateProfileCampaignExperimentGateForClass is the publication launcher
// entry point. Pilot callers keep the historical wrapper above.
func ValidateProfileCampaignExperimentGateForClass(campaignClass, experimentID string, selectedCells []string,
	records []ProfileCampaignSampleV1, samplesPerCell int) error {
	return validateProfileCampaignExperimentGate(campaignClass, experimentID, selectedCells, records, samplesPerCell, nil, "")
}

func ValidateProfileCampaignExperimentGateWithPreregistration(experimentID string, selectedCells []string,
	records []ProfileCampaignSampleV1, samplesPerCell int, preregistration *concurrencyfixture.Preregistration,
	preregistrationSHA256 string) error {
	return validateProfileCampaignExperimentGate("pilot", experimentID, selectedCells, records, samplesPerCell,
		preregistration, preregistrationSHA256)
}

func validateProfileCampaignExperimentGate(campaignClass, experimentID string, selectedCells []string,
	records []ProfileCampaignSampleV1, samplesPerCell int, preregistration *concurrencyfixture.Preregistration,
	preregistrationSHA256 string) error {
	if campaignClass != "pilot" && campaignClass != "publication" {
		return errors.New("profile campaign gate has an invalid campaign class")
	}
	if campaignClass == "publication" && preregistration != nil {
		return errors.New("publication profile campaign cannot use the pilot preregistration exception")
	}
	if !profileCampaignExperiment(experimentID) {
		return fmt.Errorf("unsupported profile campaign experiment %q", experimentID)
	}
	if samplesPerCell < 1 {
		return errors.New("samples per selected cell must be positive")
	}
	expected := make(map[string]int, len(selectedCells))
	for _, identity := range selectedCells {
		if identity == "" || identity != strings.TrimSpace(identity) ||
			!strings.HasPrefix(identity, experimentID+"/") {
			return fmt.Errorf("selected cell %q is outside experiment %s", identity, experimentID)
		}
		if _, exists := expected[identity]; exists {
			return fmt.Errorf("selected cell %q is duplicated", identity)
		}
		expected[identity] = samplesPerCell
	}
	if len(expected) == 0 {
		return errors.New("selected cell set must not be empty")
	}
	if preregistration != nil {
		if experimentID != "concurrency" || preregistrationSHA256 != concurrencyfixture.PreregistrationSHA256 {
			return errors.New("preregistration is attached to the wrong experiment or digest")
		}
		if err := preregistration.Validate(); err != nil {
			return err
		}
		if expected[preregistration.Cell] != samplesPerCell {
			return errors.New("preregistered cell is absent from the exact launcher selection")
		}
	}
	if len(records) != len(expected)*samplesPerCell {
		return fmt.Errorf("retained sample count is %d, want %d", len(records), len(expected)*samplesPerCell)
	}
	observed := make(map[string]int, len(expected))
	for index := range records {
		record := records[index]
		if err := record.Validate(); err != nil {
			return fmt.Errorf("record %d: %w", index+1, err)
		}
		if record.CampaignClass != campaignClass {
			return fmt.Errorf("record %d campaign class differs from the launcher", index+1)
		}
		sample := record.Sample
		identity := sample.ExperimentID + "/" + sample.CellID
		if sample.ExperimentID != experimentID || expected[identity] == 0 {
			return fmt.Errorf("record %d retained unassigned cell %q", index+1, identity)
		}
		observed[identity]++
		if observed[identity] > expected[identity] {
			return fmt.Errorf("cell %q retained too many samples", identity)
		}
		if sample.TaskGateRejectionV1 != nil {
			return fmt.Errorf("cell %q retained an unexpected finalizer rejection", identity)
		}
		if preregistration != nil && identity == preregistration.Cell {
			if sample.Iteration != 1 || sample.ProcessReplicate != 1 {
				return fmt.Errorf("cell %q is not one round in one fresh deployment", identity)
			}
			observedPass, err := ValidatePreregisteredConcurrencyRound(sample)
			if err != nil {
				return fmt.Errorf("cell %q: %w", identity, err)
			}
			if observedPass {
				if err := validateProfileCampaignTerminalShape(sample); err != nil {
					return fmt.Errorf("cell %q: %w", identity, err)
				}
			} else if sample.TaskGateAcceptanceV3 != nil {
				return fmt.Errorf("cell %q preregistered miss carries a fabricated v3 acceptance", identity)
			}
			continue
		}
		if sample.Status != "pass" {
			return fmt.Errorf("cell %q retained status %q", identity, sample.Status)
		}
		if err := validateProfileCampaignTerminalShape(sample); err != nil {
			return fmt.Errorf("cell %q: %w", identity, err)
		}
	}
	for identity, count := range expected {
		if observed[identity] != count {
			return fmt.Errorf("cell %q retained %d samples, want %d", identity, observed[identity], count)
		}
	}
	return nil
}

func profileCampaignExperiment(value string) bool {
	switch value {
	case "baseline", "artifact", "scale", "provsql", "rls", "attack", "concurrency", "rq5", "footprint", "benign", "counter", "adversary":
		return true
	default:
		return false
	}
}

func validateProfileCampaignTerminalShape(sample Sample) error {
	switch sample.ExperimentID {
	case "baseline":
		if sample.TaskGateAcceptanceV3 != nil {
			return errors.New("Baseline does not produce a top-level v3 finalizer acceptance")
		}
		if !validSHA256(sample.ResultSHA256) || !validSHA256(sample.PhysicalSQLSHA256) ||
			!validSHA256(sample.LogicalSQLSHA256) || !validSHA256(sample.QueryPlanSHA256) {
			return errors.New("Baseline lacks its canonical result/SQL/plan identity")
		}
		if sample.Mode == "direct" {
			if sample.System != "postgresql" || sample.BaselineVerification != nil {
				return errors.New("Baseline direct arm has a non-PostgreSQL or fabricated TaskGate shape")
			}
		} else if sample.System != "taskgate" || sample.BaselineVerification == nil {
			return errors.New("Baseline TaskGate arm lacks its receipt verification shape")
		}
	case "artifact":
		if sample.System != "taskgate" || sample.Mode != "novel" || sample.TaskGateAcceptanceV3 == nil ||
			sample.ArtifactVerification == nil {
			return errors.New("Artifact pass lacks v3 acceptance or Artifact verification")
		}
	case "scale":
		if sample.System != "taskgate" || (sample.Mode != "novel" && sample.Mode != "semantic_replay") ||
			sample.TaskGateAcceptanceV3 == nil || sample.ScaleVerification == nil {
			return errors.New("Scale dependency-e2e pass lacks v3 acceptance or Scale verification")
		}
	case "provsql":
		if sample.ProvSQLVerification == nil {
			return errors.New("ProvSQL pass lacks its verification envelope")
		}
		switch sample.Mode {
		case "direct":
			if sample.System != "postgresql" || sample.TaskGateAcceptanceV3 != nil {
				return errors.New("ProvSQL direct arm has the wrong system/finalizer shape")
			}
		case "provsql":
			if sample.System != "provsql" || sample.TaskGateAcceptanceV3 != nil {
				return errors.New("ProvSQL circuit arm has the wrong system/finalizer shape")
			}
		case "taskgate":
			if sample.System != "taskgate" || sample.TaskGateAcceptanceV3 == nil {
				return errors.New("ProvSQL TaskGate arm lacks v3 acceptance")
			}
		default:
			return errors.New("ProvSQL pass has an unknown mode")
		}
	case "rls":
		if sample.TaskGateAcceptanceV3 != nil || sample.RLSVerification == nil {
			return errors.New("RLS pass must use RLS verification without top-level v3 acceptance")
		}
		if sample.Mode != "rls" && sample.Mode != "bounded" && sample.Mode != "unlimited" {
			return errors.New("RLS pass has an unknown mode")
		}
		if (sample.Mode == "rls" && sample.System != "postgresql") ||
			(sample.Mode != "rls" && sample.System != "taskgate") {
			return errors.New("RLS pass has the wrong mode/system shape")
		}
	case "attack":
		if sample.TaskGateAcceptanceV3 != nil || sample.AttackVerification == nil {
			return errors.New("Attack pass must use Attack verification without top-level v3 acceptance")
		}
		if err := validateAttackModeAndSystem(sample); err != nil {
			return err
		}
		if err := validateProfileCampaignAttackShape(sample); err != nil {
			return err
		}
	case "concurrency":
		if sample.TaskGateAcceptanceV3 != nil || sample.ConcurrencyVerification == nil {
			return errors.New("Concurrency pass must use Concurrency verification without top-level v3 acceptance")
		}
	case "rq5":
		if sample.TaskGateAcceptanceV3 != nil || sample.RQ5Verification == nil {
			return errors.New("RQ5 pass must use RQ5 verification without top-level v3 acceptance")
		}
	case "footprint":
		if sample.TaskGateAcceptanceV3 != nil || sample.FootprintVerification == nil {
			return errors.New("footprint pass must use footprint verification without top-level v3 acceptance")
		}
		if sample.System != "taskgate" || (sample.Mode != "bounded" && sample.Mode != "unlimited") {
			return errors.New("footprint pass has the wrong mode/system shape")
		}
	case "benign":
		if sample.TaskGateAcceptanceV3 != nil || sample.BenignVerification == nil {
			return errors.New("benign pass must use benign verification without top-level v3 acceptance")
		}
		if sample.System != "taskgate" ||
			(sample.Mode != "recipe" && sample.Mode != "x2" && sample.Mode != "x4") {
			return errors.New("benign pass has the wrong mode/system shape")
		}
	case "counter":
		if sample.TaskGateAcceptanceV3 != nil || sample.CounterVerification == nil {
			return errors.New("counter pass must use counter verification without top-level v3 acceptance")
		}
		if sample.System != "taskgate" {
			return errors.New("counter pass has the wrong system shape")
		}
	case "adversary":
		if sample.TaskGateAcceptanceV3 != nil || sample.AdversaryVerification == nil {
			return errors.New("adversary pass must use adversary verification without top-level v3 acceptance")
		}
		if sample.System != "taskgate" {
			return errors.New("adversary pass has the wrong system shape")
		}
	default:
		return errors.New("unknown experiment terminal shape")
	}
	return nil
}

func validateProfileCampaignAttackShape(sample Sample) error {
	manifest, err := finalv5attack.Load()
	if err != nil {
		return err
	}
	attackCase, found := manifest.Lookup(sample.WorkloadID, sample.Scale)
	if !found || len(sample.AttackVerification.Steps) != len(attackCase.Steps) {
		return errors.New("Attack pass lacks the frozen case step set")
	}
	for index, expected := range attackCase.Steps {
		step := sample.AttackVerification.Steps[index]
		expectRejected := sample.System == "taskgate" && expected.Classification == "expected_rejection"
		if step.Rejected != expectRejected || step.Accepted == step.Rejected {
			return fmt.Errorf("Attack step %d accepted/rejected shape differs from the frozen case", index+1)
		}
	}
	return nil
}
