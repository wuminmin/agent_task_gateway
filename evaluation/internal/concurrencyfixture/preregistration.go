package concurrencyfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	PreregistrationSourcePath = "config/profiles/concurrency-preregistration-v1.json"
	PreregistrationSHA256     = "fc51165e2203d6df913894698bc6acb1b6b239dafa0f53602fabc804c4f08c8a"
	PreregisteredProfileAlias = "concurrency-expense-detail"
	PreregisteredCell         = "concurrency/shared-root/50/natural_contention"
	PreregisteredRounds       = 11
	PreregisteredSuccesses    = 1
	PreregisteredMissCode     = "offered_concurrency_not_observed"
)

type PreregistrationProbabilityModel struct {
	Model                                   string `json:"model"`
	SingleRoundSuccessProbabilityLowerBound string `json:"single_round_success_probability_lower_bound"`
	AllMissProbabilityThreshold             string `json:"all_miss_probability_threshold"`
	AllMissProbabilityUpperBound            string `json:"all_miss_probability_upper_bound"`
	Derivation                              string `json:"derivation"`
}

// Preregistration freezes the only multi-round observation rule. It is kept
// outside Sample so the three historical Sample schemas remain immutable; the
// campaign plan and merger bind these bytes before any fresh deployment runs.
type Preregistration struct {
	SchemaVersion      int                             `json:"schema_version"`
	Record             string                          `json:"record"`
	ProfileAlias       string                          `json:"profile_alias"`
	Cell               string                          `json:"cell"`
	Rounds             int                             `json:"rounds"`
	SuccessesRequired  int                             `json:"successes_required"`
	SuccessStatus      string                          `json:"success_status"`
	MissStatus         string                          `json:"miss_status"`
	MissErrorCode      string                          `json:"miss_error_code"`
	RoundIdentityScope string                          `json:"round_identity_scope"`
	RetainAllRounds    bool                            `json:"retain_all_rounds"`
	EarlyStop          bool                            `json:"early_stop"`
	ProbabilityModel   PreregistrationProbabilityModel `json:"probability_model"`
}

func LoadPreregistration(path string) (Preregistration, string, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return Preregistration{}, "", err
	}
	digest := sha256.Sum256(payload)
	digestText := hex.EncodeToString(digest[:])
	if digestText != PreregistrationSHA256 {
		return Preregistration{}, digestText, errors.New("concurrency preregistration bytes differ from the source-controlled digest")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var contract Preregistration
	if err := decoder.Decode(&contract); err != nil {
		return Preregistration{}, digestText, fmt.Errorf("decode concurrency preregistration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Preregistration{}, digestText, errors.New("concurrency preregistration has trailing JSON")
	}
	if err := contract.Validate(); err != nil {
		return Preregistration{}, digestText, err
	}
	return contract, digestText, nil
}

func (contract Preregistration) Validate() error {
	if contract.SchemaVersion != 1 || contract.Record != "taskgate-final-v5-concurrency-preregistration-v1" ||
		contract.ProfileAlias != PreregisteredProfileAlias || contract.Cell != PreregisteredCell ||
		contract.Rounds != PreregisteredRounds || contract.SuccessesRequired != PreregisteredSuccesses ||
		contract.SuccessStatus != "pass" || contract.MissStatus != "invalid" ||
		contract.MissErrorCode != PreregisteredMissCode || contract.RoundIdentityScope != "fresh_profile_deployment" ||
		!contract.RetainAllRounds || contract.EarlyStop {
		return errors.New("concurrency preregistration differs from the closed fixed-N aggregate")
	}
	model := contract.ProbabilityModel
	if model.Model != "conservative-first-cohort-v1" ||
		model.SingleRoundSuccessProbabilityLowerBound != "0.25" ||
		model.AllMissProbabilityThreshold != "0.05" ||
		model.AllMissProbabilityUpperBound != "0.0422351360" ||
		model.Derivation != "ceil(log(0.05)/log(0.75))=11" {
		return errors.New("concurrency preregistration probability model differs from the frozen derivation")
	}
	if got := int(math.Ceil(math.Log(0.05) / math.Log(0.75))); got != contract.Rounds ||
		math.Pow(0.75, float64(contract.Rounds)) > 0.05 || math.Pow(0.75, float64(contract.Rounds-1)) <= 0.05 {
		return errors.New("concurrency preregistration N is not the minimum satisfying the all-miss threshold")
	}
	return nil
}
