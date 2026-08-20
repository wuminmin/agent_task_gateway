package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

func TestPearsonRanksSynchronousAndConstantMetrics(t *testing.T) {
	value, ok := pearson([]float64{1, 2, 3, 4}, []float64{10, 20, 30, 40})
	if !ok || math.Abs(value-1) > 1e-12 {
		t.Fatalf("pearson=%v ok=%v, want 1,true", value, ok)
	}
	if _, ok := pearson([]float64{1, 1, 1}, []float64{1, 2, 3}); ok {
		t.Fatal("constant diagnostic metric produced a correlation")
	}
}

func TestSelectMeasuredSamplesReportsAndExcludesWarmups(t *testing.T) {
	records := []experiment.ProfileCampaignSampleV1{
		{CampaignClass: "pilot", Sample: experiment.Sample{SampleID: "warmup-1", ExperimentID: "concurrency", Warmup: true, Status: "invalid"}},
		{CampaignClass: "pilot", Sample: experiment.Sample{SampleID: "measured-1", ExperimentID: "concurrency", Status: "pass"}},
	}
	samples, statuses, excluded, err := selectMeasuredSamples(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples["measured-1"].Warmup || statuses["pass"] != 1 || excluded != 1 {
		t.Fatalf("samples=%+v statuses=%+v excluded=%d", samples, statuses, excluded)
	}
}

func TestSelectMeasuredSamplesKeepsNonWarmupContractFailClosed(t *testing.T) {
	tests := []experiment.ProfileCampaignSampleV1{
		{CampaignClass: "publication", Sample: experiment.Sample{SampleID: "wrong-class", ExperimentID: "concurrency", PublicationEligible: true}},
		{CampaignClass: "pilot", Sample: experiment.Sample{SampleID: "wrong-experiment", ExperimentID: "baseline"}},
	}
	for _, record := range tests {
		if _, _, _, err := selectMeasuredSamples([]experiment.ProfileCampaignSampleV1{record}); err == nil {
			t.Fatalf("accepted invalid record: %+v", record)
		}
	}
}

func TestReadMigrationsRetainsFrozenDensityAndFindsTailTimeouts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	if _, err := fmt.Fprintln(file, "concurrency diagnostic progress"); err != nil {
		t.Fatal(err)
	}
	samples := make(map[string]experiment.Sample, 270)
	for position := 1; position <= 315; position++ {
		sampleID := fmt.Sprintf("sample-%04d", position)
		warmup := position <= 45
		operation := experiment.AdapterOperation{CampaignID: "p68", DeploymentID: "deployment-01",
			ExperimentID: "concurrency", CellID: "shared-root/500/natural_contention",
			SampleID: sampleID, OrderPosition: position, Warmup: warmup}
		if !warmup {
			status := "pass"
			if position >= 275 {
				status = "fail"
			}
			samples[sampleID] = experiment.Sample{SampleID: sampleID, Status: status, ErrorCode: ""}
		}
		first := experiment.NewTaskMigrationWaitDiagnosticV1(operation, 1, "root", "task-"+sampleID,
			"AWAITING_APPROVAL", "AWAITING_APPROVAL", "reached", time.Millisecond, 1, "none", time.Now())
		secondStatus, secondLast := "reached", "ACTIVE"
		if position >= 275 {
			secondStatus, secondLast = "timeout", "AWAITING_APPROVAL"
		}
		second := experiment.NewTaskMigrationWaitDiagnosticV1(operation, 1, "root", "task-"+sampleID,
			"ACTIVE", secondLast, secondStatus, 30*time.Second, 300, "none", time.Now())
		if err := encoder.Encode(first); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(second); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	migrations, timeouts, err := readMigrations(path, samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 315 || timeouts != 41 || migrations[274].OrderPosition != 275 || migrations[274].Activation.Timeouts != 1 {
		t.Fatalf("migration curve operations=%d timeouts=%d first_tail=%+v", len(migrations), timeouts, migrations[274])
	}
}
