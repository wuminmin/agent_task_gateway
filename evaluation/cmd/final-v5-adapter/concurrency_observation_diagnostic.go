package main

import (
	"encoding/json"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

// concurrencyObservationShortfallRecord is emitted to the stderr evidence
// boundary whenever a concurrency round is judged not to have shown its offered
// width. The Sample schema is frozen across three historical versions, so the
// detail lives here instead, next to the task-migration diagnostics.
//
// It exists because "offered_concurrency_not_observed" alone cannot tell a
// reader whether the concurrency never happened or whether the harness failed
// to witness it, and the retained Sample carries only the peak counters, not
// the quiescence values the verdict actually turns on. Every unmet condition is
// named, and the raw snapshot is included so the verdict can be recomputed.
const concurrencyObservationShortfallRecordV1 = "taskgate-final-v5-concurrency-observation-shortfall-v1"

type concurrencyObservationShortfallV1 struct {
	SchemaVersion int      `json:"schema_version"`
	Record        string   `json:"record"`
	CampaignID    string   `json:"campaign_id"`
	DeploymentID  string   `json:"deployment_id"`
	ExperimentID  string   `json:"experiment_id"`
	CellID        string   `json:"cell_id"`
	SampleID      string   `json:"sample_id"`
	OrderPosition int      `json:"order_position"`
	Warmup        bool     `json:"warmup"`
	RequestedMode string   `json:"requested_mode"`
	ExpectedWidth int64    `json:"expected_width"`
	RoundSHA256   string   `json:"round_sha256"`
	Unmet         []string `json:"unmet"`
	AwaitError    string   `json:"await_error"`

	ProbeVersion                 string `json:"probe_version"`
	GatewayInstanceSHA256        string `json:"gateway_instance_sha256"`
	SnapshotRoundSHA256          string `json:"snapshot_round_sha256"`
	SnapshotMode                 string `json:"snapshot_mode"`
	SnapshotExpectedWidth        int64  `json:"snapshot_expected_width"`
	Released                     bool   `json:"released"`
	Arrived                      int64  `json:"arrived"`
	UniqueParticipants           int64  `json:"unique_participants"`
	ParticipantSetSHA256         string `json:"participant_set_sha256"`
	ExpectedParticipantSetSHA256 string `json:"expected_participant_set_sha256"`
	BarrierWaiting               int64  `json:"barrier_waiting"`
	PeakBarrierWaiting           int64  `json:"peak_barrier_waiting"`
	Active                       int64  `json:"active"`
	PeakActive                   int64  `json:"peak_active"`
	Queued                       int64  `json:"queued"`
	PeakQueued                   int64  `json:"peak_queued"`
	Completed                    int64  `json:"completed"`
	Canceled                     int64  `json:"canceled"`
	Rejected                     int64  `json:"rejected"`
	HTTPActiveCapacity           int64  `json:"http_active_capacity"`
	HTTPQueueCapacity            int64  `json:"http_queue_capacity"`
	PeakControlPoolInUse         int64  `json:"peak_control_pool_in_use"`
	ControlPoolCapacity          int64  `json:"control_pool_capacity"`
	ControlPoolWaitCountDelta    int64  `json:"control_pool_wait_count_delta"`
	ControlPoolWaitNanoseconds   int64  `json:"control_pool_wait_nanoseconds"`

	ObservedAt time.Time `json:"observed_at"`
}

// concurrencyObservationShortfall names every condition the round failed to
// meet. exactConcurrencyServiceObservation is defined as "this list is empty",
// so the verdict and its explanation cannot drift apart.
func concurrencyObservationShortfall(snapshot gatewayapp.ConcurrencyProbeSnapshot,
	roundSHA, mode string, width int) []string {
	var unmet []string
	require := func(satisfied bool, condition string) {
		if !satisfied {
			unmet = append(unmet, condition)
		}
	}
	require(width >= 1, "width_positive")
	require(snapshot.Version == gatewayapp.ConcurrencyProbeVersion, "probe_version")
	require(validDigest(snapshot.GatewayInstanceSHA256), "gateway_instance_digest")
	require(snapshot.RoundSHA256 == roundSHA, "round_digest")
	require(snapshot.Mode == mode, "mode")
	require(snapshot.ExpectedWidth == int64(width), "expected_width")
	require(snapshot.Released, "released")
	require(snapshot.Arrived == int64(width), "arrived_equals_width")
	require(snapshot.UniqueParticipants == int64(width), "unique_participants_equal_width")
	require(snapshot.ParticipantSetSHA256 == concurrencyfixture.ParticipantSetSHA256(roundSHA, width),
		"participant_set_digest")
	require(snapshot.BarrierWaiting == 0, "barrier_drained")
	require(snapshot.PeakBarrierWaiting == int64(width), "peak_barrier_waiting_equals_width")
	require(snapshot.Active == 0, "active_drained")
	require(snapshot.PeakActive >= 1 && snapshot.PeakActive <= snapshot.HTTPActiveCapacity,
		"peak_active_within_capacity")
	require(snapshot.Queued == 0, "queue_drained")
	require(snapshot.PeakQueued >= 0 && snapshot.PeakQueued <= snapshot.HTTPQueueCapacity,
		"peak_queued_within_capacity")
	require(snapshot.Completed == int64(width), "completed_equals_width")
	require(snapshot.Canceled == 0, "no_cancellations")
	require(snapshot.Rejected == 0, "no_rejections")
	require(snapshot.PeakControlPoolInUse >= 1 && snapshot.PeakControlPoolInUse <= snapshot.ControlPoolCapacity,
		"peak_control_pool_within_capacity")
	require(snapshot.ControlPoolWaitCountDelta >= 0, "control_pool_wait_count_non_negative")
	require(snapshot.ControlPoolWaitNanoseconds >= 0, "control_pool_wait_nanoseconds_non_negative")
	return unmet
}

// recordConcurrencyObservationShortfall writes one record per missed round. It
// is deliberately not gated behind the diagnosis marker: a publication reviewer
// needs the same explanation, and a miss costs a handful of records per
// deployment.
func recordConcurrencyObservationShortfall(operation experiment.AdapterOperation, roundSHA string, width int,
	snapshot gatewayapp.ConcurrencyProbeSnapshot, unmet []string, awaitErr error) {
	if len(unmet) == 0 && awaitErr == nil {
		return
	}
	awaitError := "none"
	if awaitErr != nil {
		awaitError = awaitErr.Error()
	}
	if unmet == nil {
		unmet = []string{}
	}
	_ = json.NewEncoder(adapterDiagnosticOutput).Encode(concurrencyObservationShortfallV1{
		SchemaVersion: 1, Record: concurrencyObservationShortfallRecordV1,
		CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
		ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
		OrderPosition: operation.OrderPosition, Warmup: operation.Warmup,
		RequestedMode: operation.Mode, ExpectedWidth: int64(width), RoundSHA256: roundSHA,
		Unmet: unmet, AwaitError: awaitError,

		ProbeVersion: snapshot.Version, GatewayInstanceSHA256: snapshot.GatewayInstanceSHA256,
		SnapshotRoundSHA256: snapshot.RoundSHA256, SnapshotMode: snapshot.Mode,
		SnapshotExpectedWidth: snapshot.ExpectedWidth, Released: snapshot.Released,
		Arrived: snapshot.Arrived, UniqueParticipants: snapshot.UniqueParticipants,
		ParticipantSetSHA256:         snapshot.ParticipantSetSHA256,
		ExpectedParticipantSetSHA256: concurrencyfixture.ParticipantSetSHA256(roundSHA, width),
		BarrierWaiting:               snapshot.BarrierWaiting, PeakBarrierWaiting: snapshot.PeakBarrierWaiting,
		Active: snapshot.Active, PeakActive: snapshot.PeakActive,
		Queued: snapshot.Queued, PeakQueued: snapshot.PeakQueued,
		Completed: snapshot.Completed, Canceled: snapshot.Canceled, Rejected: snapshot.Rejected,
		HTTPActiveCapacity: snapshot.HTTPActiveCapacity, HTTPQueueCapacity: snapshot.HTTPQueueCapacity,
		PeakControlPoolInUse: snapshot.PeakControlPoolInUse, ControlPoolCapacity: snapshot.ControlPoolCapacity,
		ControlPoolWaitCountDelta:  snapshot.ControlPoolWaitCountDelta,
		ControlPoolWaitNanoseconds: snapshot.ControlPoolWaitNanoseconds,

		ObservedAt: time.Now().UTC(),
	})
}
