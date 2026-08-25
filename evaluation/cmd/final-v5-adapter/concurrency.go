package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	gatewayapp "taskbound.local/agent-data-gateway/internal/gateway"
)

const (
	concurrencyProbeTokenEnv   = "TASKGATE_FINAL_V5_CONCURRENCY_TOKEN"
	concurrencyEvidenceVersion = "taskgate-final-v5-concurrency-verification-v1"
)

type concurrencyBackend interface {
	Capacity(context.Context) (gatewayapp.ConcurrencyProbeCapacity, error)
	Run(context.Context, experiment.AdapterOperation, concurrencyfixture.Cell) (experiment.Sample, error)
	Close()
}

type concurrencyAdapter struct {
	backend  concurrencyBackend
	capacity gatewayapp.ConcurrencyProbeCapacity
}

type concurrencyRunError struct {
	code    string
	invalid bool
	sample  experiment.Sample
	cause   error
}

func (err *concurrencyRunError) Error() string {
	if err == nil {
		return "concurrency run error"
	}
	if err.cause != nil {
		return err.code + ": " + err.cause.Error()
	}
	return err.code
}

func (err *concurrencyRunError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

// newConcurrencyAdapter is capability-safe: it requires the ordinary real
// adapter dependencies plus the opt-in authenticated service probe and the
// capacity needed by the frozen width-500 cell. Missing runtime configuration
// therefore yields an invalid sample; it can never fall back to a local
// barrier or self-asserted measurement.
func newConcurrencyAdapter(ctx context.Context) (*concurrencyAdapter, error) {
	token := strings.TrimSpace(os.Getenv(concurrencyProbeTokenEnv))
	if len(token) < 32 {
		return nil, fmt.Errorf("%s must contain at least 32 characters", concurrencyProbeTokenEnv)
	}
	real, err := newRealAdapter(ctx)
	if err != nil {
		return nil, err
	}
	backend := &realConcurrencyBackend{
		real: real,
		probe: &httpConcurrencyProbeClient{
			baseURL: strings.TrimRight(real.gatewayBase, "/") + gatewayapp.ConcurrencyProbeAdminPath,
			token:   token, client: real.http,
		},
		probeToken: token,
	}
	adapter, err := newConcurrencyAdapterWithBackend(ctx, backend)
	if err != nil {
		backend.Close()
		return nil, err
	}
	return adapter, nil
}

func newConcurrencyAdapterWithBackend(ctx context.Context, backend concurrencyBackend) (*concurrencyAdapter, error) {
	if backend == nil {
		return nil, errors.New("real concurrency backend is required")
	}
	if err := concurrencyfixture.Validate(); err != nil {
		return nil, err
	}
	capacity, err := backend.Capacity(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateConcurrencyCapacity(capacity); err != nil {
		return nil, err
	}
	return &concurrencyAdapter{backend: backend, capacity: capacity}, nil
}

func validateConcurrencyCapacity(capacity gatewayapp.ConcurrencyProbeCapacity) error {
	if capacity.Version != gatewayapp.ConcurrencyProbeVersion || !validDigest(capacity.GatewayInstanceSHA256) ||
		capacity.HTTPActiveCapacity != int64(concurrencyfixture.ServiceActiveWindow) ||
		capacity.HTTPQueueCapacity < int64(concurrencyfixture.MinimumServiceQueue) ||
		capacity.ControlPoolCapacity < int64(concurrencyfixture.MinimumProductionPoolWidth) ||
		capacity.ConnectorPoolCapacity < int64(concurrencyfixture.MinimumProductionPoolWidth) {
		return errors.New("authenticated Gateway concurrency capacity cannot execute the frozen width-500 matrix")
	}
	return nil
}

func (adapter *concurrencyAdapter) Close() {
	if adapter != nil && adapter.backend != nil {
		adapter.backend.Close()
	}
}

// concurrencyResampleBound is how many times one natural-contention round may
// be drawn before its sample is failed loudly instead of drawn again.
//
// It is derived from the same conservative prior the frozen preregistration
// uses -- a single-round success probability of at least 0.25 -- and never from
// an observed miss rate, because fitting a stopping rule to the data it will be
// reported alongside is exactly the objection a reviewer should raise. A
// publication campaign draws 360 natural-contention samples (3 deployments x 4
// cells x 30), so holding the campaign-wide probability of exhausting the bound
// anywhere below 0.05 needs ceil(log(1-0.95^(1/360))/log(0.75)) = 31 draws.
const concurrencyResampleBound = 31

// Execute measures one concurrency cell, redrawing a natural-contention round
// whose contention never materialised.
//
// Such a round is not a failed measurement: the clients do assemble at the
// barrier, but their writes never collide, so the OutcomeRadix reports no
// natural CAS conflict and nothing was measured about contention. It is not a
// draw from the cell at all. Redrawing it is therefore replacing a trial that
// did not happen, not discarding an inconvenient outcome -- and because every
// discarded attempt is disclosed, a reader can check that claim rather than
// trust it. Exhausting the bound leaves the sample invalid and loud.
func (adapter *concurrencyAdapter) Execute(ctx context.Context, operation experiment.AdapterOperation) experiment.Sample {
	cell, found := concurrencyfixture.Lookup(operation.WorkloadID, operation.Scale, operation.Mode)
	if adapter == nil || adapter.backend == nil || operation.ExperimentID != "concurrency" || !found {
		return invalidSample(operation, "unsupported_source_controlled_concurrency_cell")
	}
	for attempt := 1; ; attempt++ {
		sample, unrealised := adapter.drawConcurrencyRound(ctx, operation, cell)
		if unrealised == nil {
			return sample
		}
		if attempt >= concurrencyResampleBound {
			writeConcurrencyResampleAttempt(sample, attempt, true, unrealised)
			if observedPass, err := experiment.ValidatePreregisteredConcurrencyRound(sample); err == nil && !observedPass {
				writePreregisteredConcurrencyMissDiagnostic(sample, unrealised)
			}
			return sample
		}
		writeConcurrencyResampleAttempt(sample, attempt, false, unrealised)
	}
}

// drawConcurrencyRound runs one round. It reports a non-nil second value only
// when the round is a natural-contention draw whose contention never happened,
// which is the single outcome Execute is allowed to redraw.
func (adapter *concurrencyAdapter) drawConcurrencyRound(ctx context.Context, operation experiment.AdapterOperation,
	cell concurrencyfixture.Cell) (experiment.Sample, error) {
	sample, err := adapter.backend.Run(ctx, operation, cell)
	if err != nil {
		var measured *concurrencyRunError
		if errors.As(err, &measured) {
			retained := measured.sample
			if retained.SchemaVersion == 0 {
				retained = sample
			}
			if retained.SchemaVersion == 0 {
				retained = invalidSample(operation, measured.code)
			}
			if measured.invalid {
				retained.Status = "invalid"
				retained.ErrorCode = measured.code
				retained.Reason = "the requested concurrency width was not observed at the authenticated Gateway service boundary"
				if unrealisedNaturalContention(retained) {
					return retained, err
				}
				if observedPass, validationErr := experiment.ValidatePreregisteredConcurrencyRound(retained); validationErr == nil && !observedPass {
					writePreregisteredConcurrencyMissDiagnostic(retained, err)
				} else {
					writeAdapterFailureDiagnostic("concurrency", operation, err)
				}
				return retained, nil
			}
			writeAdapterFailureDiagnostic("concurrency", operation, err)
			retained.Status = "fail"
			retained.ErrorCode = measured.code
			retained.Reason = "a real concurrency backend operation was attempted and failed; retained evidence must be audited"
			return retained, nil
		}
		writeAdapterFailureDiagnostic("concurrency", operation, err)
		if sample.SchemaVersion != 0 {
			sample.Status = "fail"
			sample.ErrorCode = "real_concurrency_measurement_failed"
			sample.Reason = "a real concurrency backend operation was attempted and failed; retained evidence must be audited"
			return sample, nil
		}
		return failedSample(operation, "real_concurrency_measurement_failed"), nil
	}
	if sample.Status != "pass" {
		return sample, nil
	}
	if err := experiment.ValidateConcurrencyEvidence(sample); err != nil {
		writeAdapterFailureDiagnostic("concurrency", operation, err)
		sample.Status = "fail"
		sample.ErrorCode = "concurrency_evidence_invariant_failed"
		sample.Reason = "the real concurrency run completed but violated a frozen evidence invariant"
	}
	return sample, nil
}

// unrealisedNaturalContention reports the one redrawable outcome: a
// natural-contention round that offered its width but produced no collision.
func unrealisedNaturalContention(sample experiment.Sample) bool {
	return sample.ExperimentID == "concurrency" && sample.Mode == "natural_contention" &&
		sample.Status == "invalid" && sample.ErrorCode == concurrencyfixture.PreregisteredMissCode
}

type concurrencyProbeAPI interface {
	Capacity(context.Context) (gatewayapp.ConcurrencyProbeCapacity, error)
	CreateRound(context.Context, string, string, int) (gatewayapp.ConcurrencyProbeSnapshot, error)
	Snapshot(context.Context, string) (gatewayapp.ConcurrencyProbeSnapshot, error)
	DeleteRound(context.Context, string) error
}

type httpConcurrencyProbeClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func (client *httpConcurrencyProbeClient) Capacity(ctx context.Context) (gatewayapp.ConcurrencyProbeCapacity, error) {
	var result gatewayapp.ConcurrencyProbeCapacity
	return result, client.do(ctx, http.MethodGet, "/capacity", nil, &result, http.StatusOK)
}

func (client *httpConcurrencyProbeClient) CreateRound(ctx context.Context, roundSHA, mode string, width int) (gatewayapp.ConcurrencyProbeSnapshot, error) {
	var result gatewayapp.ConcurrencyProbeSnapshot
	err := client.do(ctx, http.MethodPost, "/rounds", map[string]any{
		"round_sha256": roundSHA, "mode": mode, "expected_width": width,
	}, &result, http.StatusCreated)
	return result, err
}

func (client *httpConcurrencyProbeClient) Snapshot(ctx context.Context, roundSHA string) (gatewayapp.ConcurrencyProbeSnapshot, error) {
	var result gatewayapp.ConcurrencyProbeSnapshot
	err := client.do(ctx, http.MethodGet, "/rounds/"+url.PathEscape(roundSHA), nil, &result, http.StatusOK)
	return result, err
}

func (client *httpConcurrencyProbeClient) DeleteRound(ctx context.Context, roundSHA string) error {
	return client.do(ctx, http.MethodDelete, "/rounds/"+url.PathEscape(roundSHA), nil, nil, http.StatusNoContent)
}

func (client *httpConcurrencyProbeClient) do(ctx context.Context, method, path string, input, output any, wantStatus int) error {
	if client == nil || client.client == nil || strings.TrimSpace(client.baseURL) == "" || len(client.token) < 32 {
		return errors.New("concurrency probe client is incomplete")
	}
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.token)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	encoded, err := readExactlyBounded(response.Body, 1<<20)
	if err != nil {
		return err
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("concurrency probe returned HTTP %d", response.StatusCode)
	}
	if output == nil {
		if len(bytes.TrimSpace(encoded)) != 0 {
			return errors.New("concurrency probe returned an unexpected response body")
		}
		return nil
	}
	if err := experiment.StrictJSON(encoded, output); err != nil {
		return fmt.Errorf("decode strict concurrency probe response: %w", err)
	}
	return nil
}

func concurrencyRoundIdentity(operation experiment.AdapterOperation) concurrencyfixture.RoundIdentity {
	return concurrencyfixture.RoundIdentity{
		CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID, ExperimentID: operation.ExperimentID,
		CellID: operation.CellID, SampleID: operation.SampleID, Iteration: operation.Iteration,
		ProcessReplicate: operation.ProcessReplicate, PairID: operation.PairID, RootGroupID: operation.RootGroupID,
	}
}

func waitForProbeCompletion(ctx context.Context, probe concurrencyProbeAPI, roundSHA string, width int64) (gatewayapp.ConcurrencyProbeSnapshot, error) {
	return gatewayapp.WaitForConcurrencyProbeSnapshot(ctx, 10*time.Millisecond,
		func(loadCtx context.Context) (gatewayapp.ConcurrencyProbeSnapshot, error) {
			return probe.Snapshot(loadCtx, roundSHA)
		},
		func(snapshot gatewayapp.ConcurrencyProbeSnapshot) bool {
			return snapshot.Released && snapshot.Completed == width && snapshot.Active == 0 && snapshot.Queued == 0 && snapshot.BarrierWaiting == 0
		})
}
