// Package finalv5attack owns the immutable, source-controlled A--E corpus.
// It imports no Gateway, Control, exposure, SQL-policy, or query-plan code so
// the finalizer can bind observations to the exact preregistered bytes.
package finalv5attack

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	CorpusID       = "taskgate-final-v5-attack-corpus-v1"
	CorpusSHA256   = "483b667369a05b2d3f8b2e7729d7794c6e0f89f5c2e0ba6800b918fa783ffae9"
	DatasetID      = "travel-demo-2026-v1"
	SchemaVersion  = 1
	MaxCorpusSteps = 16

	TaskRouteRoot           = "root"
	TaskRouteDelegatedChild = "delegated_child"
)

//go:embed corpus-v1.json
var corpusBytes []byte

type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	CorpusID      string `json:"corpus_id"`
	DatasetID     string `json:"dataset_id"`
	Cases         []Case `json:"cases"`
}

type Case struct {
	WorkloadID       string  `json:"workload_id"`
	Scale            string  `json:"scale"`
	OutcomeCeiling   int64   `json:"outcome_ceiling,omitempty"`
	Thresholds       []int64 `json:"thresholds,omitempty"`
	ThresholdResults []int64 `json:"threshold_results,omitempty"`
	Steps            []Step  `json:"steps"`
}

type Step struct {
	ID                  string `json:"id"`
	LogicalSQL          string `json:"logical_sql"`
	DirectSQL           string `json:"direct_sql"`
	Classification      string `json:"classification"`
	Role                string `json:"role"`
	TaskRoute           string `json:"task_route,omitempty"`
	ExpectedErrorCode   string `json:"expected_error_code,omitempty"`
	ExpectedErrorReason string `json:"expected_error_reason,omitempty"`
	Threshold           int64  `json:"threshold,omitempty"`
	Primary             bool   `json:"primary,omitempty"`
}

func Load() (Manifest, error) {
	var manifest Manifest
	digest := sha256.Sum256(corpusBytes)
	if hex.EncodeToString(digest[:]) != CorpusSHA256 {
		return manifest, errors.New("attack corpus bytes differ from their compiled SHA-256")
	}
	decoder := json.NewDecoder(strings.NewReader(string(corpusBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	if decoder.More() {
		return manifest, errors.New("attack corpus contains multiple JSON values")
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return manifest, errors.New("attack corpus contains trailing JSON")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Bytes() []byte { return append([]byte(nil), corpusBytes...) }

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != SchemaVersion || manifest.CorpusID != CorpusID || manifest.DatasetID != DatasetID || len(manifest.Cases) != 7 {
		return errors.New("attack corpus identity or case count is invalid")
	}
	seen := map[string]bool{}
	for _, attackCase := range manifest.Cases {
		key := attackCase.WorkloadID + "\x00" + attackCase.Scale
		if attackCase.WorkloadID == "" || attackCase.Scale == "" || seen[key] || len(attackCase.Steps) == 0 || len(attackCase.Steps) > MaxCorpusSteps {
			return fmt.Errorf("invalid or duplicate attack case %q", key)
		}
		seen[key] = true
		stepIDs := map[string]bool{}
		primary := 0
		for stepIndex, step := range attackCase.Steps {
			if step.ID == "" || stepIDs[step.ID] || strings.TrimSpace(step.LogicalSQL) == "" || strings.TrimSpace(step.DirectSQL) == "" {
				return fmt.Errorf("invalid attack step in %q", key)
			}
			stepIDs[step.ID] = true
			switch step.Classification {
			case "accepted", "accepted_equivalent", "semantic_replay":
				if step.ExpectedErrorCode != "" || step.ExpectedErrorReason != "" {
					return fmt.Errorf("accepted attack step %q declares an error", step.ID)
				}
			case "expected_rejection":
				if step.ExpectedErrorCode == "" {
					return fmt.Errorf("rejected attack step %q lacks a stable code", step.ID)
				}
			default:
				return fmt.Errorf("attack step %q has unknown classification", step.ID)
			}
			if step.Primary {
				primary++
			}
			if attackCase.WorkloadID == "E-threshold" {
				expectedRoute := TaskRouteRoot
				if stepIndex == 0 || stepIndex == len(attackCase.Steps)-1 {
					expectedRoute = TaskRouteDelegatedChild
				}
				if step.TaskRoute != expectedRoute {
					return fmt.Errorf("attack E step %q has task route %q, want %q", step.ID, step.TaskRoute, expectedRoute)
				}
			} else if step.TaskRoute != "" {
				return fmt.Errorf("non-E attack step %q declares a delegated task route", step.ID)
			}
		}
		if primary == 0 {
			return fmt.Errorf("attack case %q lacks a primary result", key)
		}
		if attackCase.WorkloadID == "E-threshold" {
			if attackCase.OutcomeCeiling != 5 || len(attackCase.Thresholds) != 3 || len(attackCase.ThresholdResults) != 3 {
				return errors.New("attack E ceiling or threshold count differs from preregistration")
			}
		} else if attackCase.OutcomeCeiling != 0 || len(attackCase.Thresholds) != 0 || len(attackCase.ThresholdResults) != 0 {
			return fmt.Errorf("non-threshold case %q declares threshold metadata", key)
		}
	}
	wanted := []string{
		"A-pagination\x00complete-to-pages", "A-pagination\x00pages-to-complete",
		"B-equivalent-sql\x00variants-v1", "C-request-id\x00same-and-different",
		"D-split-union\x00complete-to-split", "D-split-union\x00split-to-complete",
		"E-threshold\x00preregistered-v1",
	}
	for _, key := range wanted {
		if !seen[key] {
			return fmt.Errorf("attack corpus lacks %q", key)
		}
	}
	return nil
}

func (manifest Manifest) Lookup(workloadID, scale string) (Case, bool) {
	for _, attackCase := range manifest.Cases {
		if attackCase.WorkloadID == workloadID && attackCase.Scale == scale {
			return attackCase, true
		}
	}
	return Case{}, false
}

// RowSHA256 commits one canonical typed row digest. The caller supplies the
// canonical result hash of a one-row relation, never raw result values.
func RowSHA256(canonicalOneRowSHA256 string) (string, error) {
	if !validSHA256(canonicalOneRowSHA256) {
		return "", errors.New("canonical one-row digest is invalid")
	}
	return domainDigest("TASKGATE-FINAL-V5-ATTACK-ROW-V1", []string{canonicalOneRowSHA256}), nil
}

// RowSetSHA256 is an order-independent exact set commitment. Duplicate rows
// are rejected because every frozen A/D query projects a unique receipt key.
func RowSetSHA256(rows []string) (string, error) {
	ordered := append([]string(nil), rows...)
	for _, row := range ordered {
		if !validSHA256(row) {
			return "", errors.New("attack row digest is invalid")
		}
	}
	sort.Strings(ordered)
	for index := 1; index < len(ordered); index++ {
		if ordered[index] == ordered[index-1] {
			return "", errors.New("attack row set contains a duplicate key")
		}
	}
	return domainDigest("TASKGATE-FINAL-V5-ATTACK-ROW-SET-V1", ordered), nil
}

// PrimaryResultSHA256 binds the ordered sequence selected by Primary=true in
// the corpus. It lets direct and TaskGate arms compare the same logical target
// even when the trace also contains a fail-closed negative control.
func PrimaryResultSHA256(results []string) (string, error) {
	if len(results) == 0 {
		return "", errors.New("attack primary result sequence is empty")
	}
	for _, result := range results {
		if !validSHA256(result) {
			return "", errors.New("attack primary result digest is invalid")
		}
	}
	return domainDigest("TASKGATE-FINAL-V5-ATTACK-PRIMARY-RESULT-V1", results), nil
}

func domainDigest(domain string, values []string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain + "\x00"))
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}
