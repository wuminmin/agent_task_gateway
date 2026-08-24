package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

func observedRound(roundSHA, mode string, width int) gatewayapp.ConcurrencyProbeSnapshot {
	return gatewayapp.ConcurrencyProbeSnapshot{
		ConcurrencyProbeCapacity: gatewayapp.ConcurrencyProbeCapacity{
			Version:               gatewayapp.ConcurrencyProbeVersion,
			GatewayInstanceSHA256: strings.Repeat("a", 64),
			HTTPActiveCapacity:    10, HTTPQueueCapacity: 512,
			ControlPoolCapacity: 32, ConnectorPoolCapacity: 32,
		},
		RoundSHA256: roundSHA, Mode: mode, ExpectedWidth: int64(width),
		Arrived: int64(width), UniqueParticipants: int64(width),
		ParticipantSetSHA256: concurrencyfixture.ParticipantSetSHA256(roundSHA, width),
		BarrierWaiting:       0, PeakBarrierWaiting: int64(width),
		Active: 0, PeakActive: 3, Queued: 0, PeakQueued: 1,
		Completed: int64(width), Canceled: 0, Rejected: 0,
		PeakControlPoolInUse: 2, ControlPoolWaitCountDelta: 0, ControlPoolWaitNanoseconds: 0,
		Released: true,
	}
}

// TestConcurrencyObservationShortfallExplainsEveryVerdict pins P74. The verdict
// and its explanation are the same computation, so a round can never be judged
// a miss without the artifact naming which condition it failed.
func TestConcurrencyObservationShortfallExplainsEveryVerdict(t *testing.T) {
	const width = 4
	roundSHA := strings.Repeat("b", 64)
	mode := "natural_contention"

	good := observedRound(roundSHA, mode, width)
	if unmet := concurrencyObservationShortfall(good, roundSHA, mode, width); len(unmet) != 0 {
		t.Fatalf("a fully observed round must have no shortfall, got %v", unmet)
	}
	if !exactConcurrencyServiceObservation(good, roundSHA, mode, width) {
		t.Fatal("the predicate disagreed with an empty shortfall")
	}

	for _, testCase := range []struct {
		name      string
		mutate    func(*gatewayapp.ConcurrencyProbeSnapshot)
		condition string
	}{
		{"never released", func(s *gatewayapp.ConcurrencyProbeSnapshot) { s.Released = false }, "released"},
		{"barrier still occupied", func(s *gatewayapp.ConcurrencyProbeSnapshot) { s.BarrierWaiting = 1 }, "barrier_drained"},
		{"still active", func(s *gatewayapp.ConcurrencyProbeSnapshot) { s.Active = 1 }, "active_drained"},
		{"still queued", func(s *gatewayapp.ConcurrencyProbeSnapshot) { s.Queued = 1 }, "queue_drained"},
		{"one participant short", func(s *gatewayapp.ConcurrencyProbeSnapshot) { s.Arrived = width - 1 }, "arrived_equals_width"},
		{"a participant was rejected", func(s *gatewayapp.ConcurrencyProbeSnapshot) { s.Rejected = 1 }, "no_rejections"},
		{"participant set differs", func(s *gatewayapp.ConcurrencyProbeSnapshot) {
			s.ParticipantSetSHA256 = strings.Repeat("c", 64)
		}, "participant_set_digest"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := observedRound(roundSHA, mode, width)
			testCase.mutate(&snapshot)
			unmet := concurrencyObservationShortfall(snapshot, roundSHA, mode, width)
			found := false
			for _, condition := range unmet {
				if condition == testCase.condition {
					found = true
				}
			}
			if !found {
				t.Fatalf("shortfall %v did not name %q", unmet, testCase.condition)
			}
			if exactConcurrencyServiceObservation(snapshot, roundSHA, mode, width) {
				t.Fatal("the predicate passed a round its own shortfall rejected")
			}
		})
	}
}

// TestConcurrencyObservationShortfallRecordCarriesTheQuiescenceValues covers the
// gap that made -12's misses unexplainable: the retained Sample keeps only peak
// counters, so the record must carry the current ones the verdict turns on.
func TestConcurrencyObservationShortfallRecordCarriesTheQuiescenceValues(t *testing.T) {
	const width = 4
	roundSHA := strings.Repeat("b", 64)
	mode := "natural_contention"
	snapshot := observedRound(roundSHA, mode, width)
	snapshot.Released = false
	snapshot.Queued = 7

	previous := adapterDiagnosticOutput
	var buffer bytes.Buffer
	adapterDiagnosticOutput = &buffer
	defer func() { adapterDiagnosticOutput = previous }()

	operation := experiment.AdapterOperation{
		CampaignID: "p74", DeploymentID: "deployment-01", ExperimentID: "concurrency",
		CellID: "shared-root/4/natural_contention", SampleID: "deployment-01-sample-0001",
		OrderPosition: 12, Mode: mode,
	}
	unmet := concurrencyObservationShortfall(snapshot, roundSHA, mode, width)
	recordConcurrencyObservationShortfall(operation, roundSHA, width, snapshot, unmet, nil)

	var record concurrencyObservationShortfallV1
	if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
		t.Fatalf("the shortfall record is not valid JSON: %v", err)
	}
	if record.Record != concurrencyObservationShortfallRecordV1 || record.SampleID != operation.SampleID ||
		record.ExpectedWidth != width || record.OrderPosition != 12 {
		t.Fatalf("record identity is wrong: %+v", record)
	}
	if record.Released || record.Queued != 7 || record.Arrived != width || record.Completed != width {
		t.Fatalf("record did not carry the raw snapshot: released=%v queued=%d arrived=%d completed=%d",
			record.Released, record.Queued, record.Arrived, record.Completed)
	}
	if record.ExpectedParticipantSetSHA256 != concurrencyfixture.ParticipantSetSHA256(roundSHA, width) {
		t.Fatal("record omitted the expected participant-set digest a reviewer needs to recompute the verdict")
	}
	joined := strings.Join(record.Unmet, ",")
	if !strings.Contains(joined, "released") || !strings.Contains(joined, "queue_drained") {
		t.Fatalf("unmet conditions were not named: %v", record.Unmet)
	}
}

// TestConcurrencyObservationShortfallStaysSilentOnASatisfiedRound keeps the
// record out of passing samples so it cannot be mistaken for a fault signal.
func TestConcurrencyObservationShortfallStaysSilentOnASatisfiedRound(t *testing.T) {
	previous := adapterDiagnosticOutput
	var buffer bytes.Buffer
	adapterDiagnosticOutput = &buffer
	defer func() { adapterDiagnosticOutput = previous }()
	roundSHA := strings.Repeat("b", 64)
	recordConcurrencyObservationShortfall(experiment.AdapterOperation{}, roundSHA, 4,
		observedRound(roundSHA, "natural_contention", 4), nil, nil)
	if buffer.Len() != 0 {
		t.Fatalf("a satisfied round must emit nothing, got %s", buffer.String())
	}
}
