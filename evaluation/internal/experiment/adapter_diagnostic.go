package experiment

import (
	"errors"
	"strings"
)

const PreregisteredConcurrencyMissDiagnosticV1Record = "taskgate-preregistered-concurrency-miss-diagnostic-v1"

// PreregisteredConcurrencyMissDiagnosticV1 is a private stderr control
// envelope. It keeps the exact Adapter cause in the credential-gated
// diagnostic channel while allowing the Runner to distinguish one validated
// preregistered miss from an ordinary process failure.
type PreregisteredConcurrencyMissDiagnosticV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Record        string `json:"record"`
	ExperimentID  string `json:"experiment_id"`
	CellID        string `json:"cell_id"`
	SampleID      string `json:"sample_id"`
	Cause         string `json:"cause"`
}

func NewPreregisteredConcurrencyMissDiagnosticV1(sample Sample, cause error) PreregisteredConcurrencyMissDiagnosticV1 {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	return PreregisteredConcurrencyMissDiagnosticV1{
		SchemaVersion: 1,
		Record:        PreregisteredConcurrencyMissDiagnosticV1Record,
		ExperimentID:  sample.ExperimentID,
		CellID:        sample.CellID,
		SampleID:      sample.SampleID,
		Cause:         message,
	}
}

func (diagnostic PreregisteredConcurrencyMissDiagnosticV1) Validate() error {
	if diagnostic.SchemaVersion != 1 || diagnostic.Record != PreregisteredConcurrencyMissDiagnosticV1Record ||
		diagnostic.ExperimentID != "concurrency" || diagnostic.CellID == "" ||
		diagnostic.SampleID == "" || strings.TrimSpace(diagnostic.Cause) == "" {
		return errors.New("invalid preregistered concurrency miss diagnostic")
	}
	return nil
}
