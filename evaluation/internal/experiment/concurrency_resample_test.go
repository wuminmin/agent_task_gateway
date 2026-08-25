package experiment

import (
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
)

func resampleOperations() []AdapterOperation {
	return []AdapterOperation{{
		CampaignID: "p75", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		CellID: "shared-root/10/natural_contention", SampleID: "sample-0001", OrderPosition: 1,
	}}
}

func resampleSample(status, code string) []*Sample {
	return []*Sample{{
		ExperimentID: "concurrency", CellID: "shared-root/10/natural_contention",
		SampleID: "sample-0001", Status: status, ErrorCode: code,
	}}
}

func resampleLine(attempt int, terminal bool) string {
	record := NewConcurrencyResampleAttemptV1(Sample{
		ExperimentID: "concurrency", CellID: "shared-root/10/natural_contention", SampleID: "sample-0001",
	}, attempt, terminal, errString("no natural CAS conflict"))
	return mustJSONLine(record)
}

type errString string

func (e errString) Error() string { return string(e) }

// TestResampleDisclosureMustMatchTheOutcome pins the half of P75 that keeps the
// redrawing honest: the disclosure and the retained sample have to agree, so a
// redraw can neither bury a round that never realised its contention nor be
// invented for one that did.
func TestResampleDisclosureMustMatchTheOutcome(t *testing.T) {
	operations := resampleOperations()

	t.Run("redrawn then settled is accepted", func(t *testing.T) {
		payload := resampleLine(1, false) + resampleLine(2, false)
		if err := validateConcurrencyResampleDiagnostics("concurrency", operations,
			resampleSample("pass", ""), []byte(payload)); err != nil {
			t.Fatalf("a disclosed redraw that later settled must be accepted: %v", err)
		}
	})

	t.Run("exhausted bound is accepted only with a terminal marker", func(t *testing.T) {
		terminalPayload := resampleLine(1, false) + resampleLine(2, true)
		if err := validateConcurrencyResampleDiagnostics("concurrency", operations,
			resampleSample("invalid", concurrencyfixture.PreregisteredMissCode), []byte(terminalPayload)); err != nil {
			t.Fatalf("a terminal series over an invalid sample must be accepted: %v", err)
		}
		openPayload := resampleLine(1, false) + resampleLine(2, false)
		if err := validateConcurrencyResampleDiagnostics("concurrency", operations,
			resampleSample("invalid", concurrencyfixture.PreregisteredMissCode), []byte(openPayload)); err == nil {
			t.Fatal("an invalid sample with no terminal marker must be rejected")
		}
	})

	t.Run("terminal marker over a settled sample is rejected", func(t *testing.T) {
		payload := resampleLine(1, true)
		if err := validateConcurrencyResampleDiagnostics("concurrency", operations,
			resampleSample("pass", ""), []byte(payload)); err == nil {
			t.Fatal("a terminal marker must not stand over a sample that settled")
		}
	})

	t.Run("a repeated attempt number is rejected", func(t *testing.T) {
		payload := resampleLine(1, false) + resampleLine(1, false)
		if err := validateConcurrencyResampleDiagnostics("concurrency", operations,
			resampleSample("pass", ""), []byte(payload)); err == nil {
			t.Fatal("the same attempt must not be disclosed twice")
		}
	})

	t.Run("an unrequested sample is rejected", func(t *testing.T) {
		record := NewConcurrencyResampleAttemptV1(Sample{
			ExperimentID: "concurrency", CellID: "shared-root/10/natural_contention", SampleID: "sample-9999",
		}, 1, false, errString("no natural CAS conflict"))
		if err := validateConcurrencyResampleDiagnostics("concurrency", operations,
			resampleSample("pass", ""), []byte(mustJSONLine(record))); err == nil {
			t.Fatal("a redraw naming an operation nobody requested must be rejected")
		}
	})

	t.Run("a forced-queue cell may not be redrawn", func(t *testing.T) {
		record := NewConcurrencyResampleAttemptV1(Sample{
			ExperimentID: "concurrency", CellID: "shared-root/10/forced_queue_safety", SampleID: "sample-0001",
		}, 1, false, errString("no natural CAS conflict"))
		if err := record.Validate(); err == nil {
			t.Fatal("only a natural-contention round may be redrawn")
		}
	})
}

func mustJSONLine(record ConcurrencyResampleAttemptV1) string {
	encoded, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(encoded)) + "\n"
}
