package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func resampleOperation(scale string) experiment.AdapterOperation {
	return experiment.AdapterOperation{
		CampaignID: "p75", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		WorkloadID: "shared-root", Scale: scale, Mode: "natural_contention",
		CellID:   "shared-root/" + scale + "/natural_contention",
		SampleID: "deployment-01-p01-sample-0001", OrderPosition: 1,
	}
}

func captureDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := adapterDiagnosticOutput
	buffer := &bytes.Buffer{}
	adapterDiagnosticOutput = buffer
	t.Cleanup(func() { adapterDiagnosticOutput = previous })
	return buffer
}

func resampleRecords(t *testing.T, buffer *bytes.Buffer) []experiment.ConcurrencyResampleAttemptV1 {
	t.Helper()
	var records []experiment.ConcurrencyResampleAttemptV1
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if !strings.Contains(line, experiment.ConcurrencyResampleAttemptV1Record) {
			continue
		}
		var record experiment.ConcurrencyResampleAttemptV1
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("resample disclosure is not valid JSON: %v", err)
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("resample disclosure failed its own contract: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func missError() error {
	return &concurrencyRunError{code: concurrencyfixture.PreregisteredMissCode, invalid: true}
}

// TestContentionRoundIsRedrawnAndEveryDiscardDisclosed pins P75. A round whose
// contention never happened measured nothing, so it is drawn again -- and each
// discarded draw is disclosed so the redrawing is auditable rather than silent.
func TestContentionRoundIsRedrawnAndEveryDiscardDisclosed(t *testing.T) {
	draws := 0
	backend := &fakeConcurrencyBackend{
		capacity: validConcurrencyTestCapacity(),
		run: func(operation experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
			draws++
			if draws <= 2 {
				return experiment.Sample{}, missError()
			}
			settled := invalidSample(operation, "")
			settled.Status = "fail"
			settled.ErrorCode = "some_other_outcome"
			return settled, nil
		},
	}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	buffer := captureDiagnostics(t)
	sample := adapter.Execute(context.Background(), resampleOperation("10"))

	if draws != 3 {
		t.Fatalf("two unrealised rounds should cost three draws, got %d", draws)
	}
	if sample.ErrorCode == concurrencyfixture.PreregisteredMissCode {
		t.Fatal("the redrawn sample still carries the miss code")
	}
	records := resampleRecords(t, buffer)
	if len(records) != 2 {
		t.Fatalf("each discarded draw must be disclosed, got %d records", len(records))
	}
	for index, record := range records {
		if record.Attempt != index+1 || record.Terminal {
			t.Fatalf("discarded draw %d disclosed as attempt=%d terminal=%v", index+1, record.Attempt, record.Terminal)
		}
		if record.CellID != "shared-root/10/natural_contention" || record.SampleID != "deployment-01-p01-sample-0001" {
			t.Fatalf("disclosure lost its identity: %+v", record)
		}
	}
}

// TestContentionResampleBoundFailsLoudly keeps the bound honest: exhausting it
// leaves the sample invalid and says so, rather than accepting a round whose
// contention never happened.
func TestContentionResampleBoundFailsLoudly(t *testing.T) {
	draws := 0
	backend := &fakeConcurrencyBackend{
		capacity: validConcurrencyTestCapacity(),
		run: func(_ experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
			draws++
			return experiment.Sample{}, missError()
		},
	}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	buffer := captureDiagnostics(t)
	sample := adapter.Execute(context.Background(), resampleOperation("10"))

	if draws != concurrencyResampleBound {
		t.Fatalf("the bound must cap the draws at %d, got %d", concurrencyResampleBound, draws)
	}
	if sample.Status != "invalid" || sample.ErrorCode != concurrencyfixture.PreregisteredMissCode {
		t.Fatalf("an exhausted bound must stay invalid, got %s/%s", sample.Status, sample.ErrorCode)
	}
	records := resampleRecords(t, buffer)
	if len(records) != concurrencyResampleBound {
		t.Fatalf("every draw must be disclosed, got %d", len(records))
	}
	terminal := 0
	for _, record := range records {
		if record.Terminal {
			terminal++
		}
	}
	if terminal != 1 || !records[len(records)-1].Terminal {
		t.Fatalf("exactly the last draw must be terminal, got %d terminal records", terminal)
	}
}

// TestOnlyNaturalContentionIsRedrawn keeps the exemption narrow: a forced-queue
// cell induces its contention deterministically, so a miss there is a real
// failure and must not be redrawn away.
func TestOnlyNaturalContentionIsRedrawn(t *testing.T) {
	draws := 0
	backend := &fakeConcurrencyBackend{
		capacity: validConcurrencyTestCapacity(),
		run: func(_ experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
			draws++
			return experiment.Sample{}, missError()
		},
	}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	buffer := captureDiagnostics(t)
	operation := resampleOperation("10")
	operation.Mode = "forced_queue_safety"
	operation.CellID = "shared-root/10/forced_queue_safety"
	adapter.Execute(context.Background(), operation)

	if draws != 1 {
		t.Fatalf("a forced-queue miss must not be redrawn, got %d draws", draws)
	}
	if records := resampleRecords(t, buffer); len(records) != 0 {
		t.Fatalf("a forced-queue miss must disclose no redraw, got %d", len(records))
	}
}

// TestSettledRoundDisclosesNoRedraw stops the record from appearing on rounds
// that never needed one, where it would read as a fault signal.
func TestSettledRoundDisclosesNoRedraw(t *testing.T) {
	backend := &fakeConcurrencyBackend{
		capacity: validConcurrencyTestCapacity(),
		run: func(operation experiment.AdapterOperation, _ concurrencyfixture.Cell) (experiment.Sample, error) {
			settled := invalidSample(operation, "")
			settled.Status = "fail"
			settled.ErrorCode = "some_other_outcome"
			return settled, nil
		},
	}
	adapter, err := newConcurrencyAdapterWithBackend(context.Background(), backend)
	if err != nil {
		t.Fatal(err)
	}
	buffer := captureDiagnostics(t)
	adapter.Execute(context.Background(), resampleOperation("10"))
	if records := resampleRecords(t, buffer); len(records) != 0 {
		t.Fatalf("a round that needed no redraw must disclose none, got %d", len(records))
	}
}
