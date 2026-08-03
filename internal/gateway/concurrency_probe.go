package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	ConcurrencyProbeVersion           = "taskgate-service-concurrency-probe-v1"
	ConcurrencyRoundHeader            = "X-TaskGate-Concurrency-Round"
	ConcurrencyParticipantHeader      = "X-TaskGate-Concurrency-Participant"
	ConcurrencyAuthorizationHeader    = "X-TaskGate-Concurrency-Authorization"
	ConcurrencyProbeAdminPath         = "/admin/v1/evaluation/concurrency"
	concurrencyParticipantSetDomain   = "TASKGATE-SERVICE-CONCURRENCY-PARTICIPANTS-V1\x00"
	maximumConcurrencyProbeRoundCount = 8
)

// ConcurrencyProbeConfig enables an explicit, authenticated service-side
// arrival barrier. It is disabled in ordinary deployments. MaxActive is the
// number of HTTP requests admitted past the barrier; MaxQueued is the bounded
// service queue behind it. PoolStats contains only database/sql counters and
// never a DSN or SQL text.
type ConcurrencyProbeConfig struct {
	Token                   string
	MaxActive               int
	MaxQueued               int
	PoolStats               func() sql.DBStats
	ConnectorMaxConnections int
}

// ConcurrencyProbeCapacity is returned by the authenticated preflight
// endpoint and cryptographically bound into raw experiment evidence.
type ConcurrencyProbeCapacity struct {
	Version               string `json:"version"`
	GatewayInstanceSHA256 string `json:"gateway_instance_sha256"`
	HTTPActiveCapacity    int64  `json:"http_active_capacity"`
	HTTPQueueCapacity     int64  `json:"http_queue_capacity"`
	ControlPoolCapacity   int64  `json:"control_pool_capacity"`
	ConnectorPoolCapacity int64  `json:"connector_pool_capacity"`
}

// ConcurrencyProbeSnapshot contains counts only. Round and participant
// identities are already SHA-256 values; no task, query, request, or bearer
// identity is retained.
type ConcurrencyProbeSnapshot struct {
	ConcurrencyProbeCapacity
	RoundSHA256                string `json:"round_sha256"`
	Mode                       string `json:"mode"`
	ExpectedWidth              int64  `json:"expected_width"`
	Arrived                    int64  `json:"arrived"`
	UniqueParticipants         int64  `json:"unique_participants"`
	ParticipantSetSHA256       string `json:"participant_set_sha256"`
	BarrierWaiting             int64  `json:"barrier_waiting"`
	PeakBarrierWaiting         int64  `json:"peak_barrier_waiting"`
	Active                     int64  `json:"active"`
	PeakActive                 int64  `json:"peak_active"`
	Queued                     int64  `json:"queued"`
	PeakQueued                 int64  `json:"peak_queued"`
	Completed                  int64  `json:"completed"`
	Canceled                   int64  `json:"canceled"`
	Rejected                   int64  `json:"rejected"`
	PeakControlPoolInUse       int64  `json:"peak_control_pool_in_use"`
	ControlPoolWaitCountDelta  int64  `json:"control_pool_wait_count_delta"`
	ControlPoolWaitNanoseconds int64  `json:"control_pool_wait_nanoseconds"`
	Released                   bool   `json:"released"`
}

type ConcurrencyProbe struct {
	token       string
	capacity    ConcurrencyProbeCapacity
	poolStats   func() sql.DBStats
	activeSlots chan struct{}

	mu     sync.Mutex
	rounds map[string]*concurrencyProbeRound
}

type concurrencyProbeRound struct {
	mode          string
	expected      int
	participants  map[string]struct{}
	release       chan struct{}
	released      bool
	arrived       int64
	barrierWait   int64
	peakBarrier   int64
	active        int64
	peakActive    int64
	queued        int64
	peakQueued    int64
	completed     int64
	canceled      int64
	rejected      int64
	peakPoolInUse int64
	poolWaitStart int64
	poolWaitNanos int64
}

type concurrencyRoundRequest struct {
	RoundSHA256   string `json:"round_sha256"`
	Mode          string `json:"mode"`
	ExpectedWidth int    `json:"expected_width"`
}

func NewConcurrencyProbe(config ConcurrencyProbeConfig) (*ConcurrencyProbe, error) {
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("concurrency probe token is required")
	}
	if len(config.Token) < 32 || config.MaxActive < 1 || config.MaxActive > 4096 ||
		config.MaxQueued < 1 || config.MaxQueued > 16384 ||
		config.ConnectorMaxConnections < 1 || config.ConnectorMaxConnections > 4096 {
		return nil, errors.New("invalid concurrency probe capacity")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(append([]byte(ConcurrencyProbeVersion+"\x00"), random...))
	controlCapacity := int64(0)
	if config.PoolStats != nil {
		controlCapacity = int64(config.PoolStats().MaxOpenConnections)
	}
	if controlCapacity < 1 {
		return nil, errors.New("concurrency probe requires Control pool telemetry")
	}
	return &ConcurrencyProbe{
		token: config.Token, poolStats: config.PoolStats,
		capacity: ConcurrencyProbeCapacity{
			Version: ConcurrencyProbeVersion, GatewayInstanceSHA256: hex.EncodeToString(digest[:]),
			HTTPActiveCapacity: int64(config.MaxActive), HTTPQueueCapacity: int64(config.MaxQueued),
			ControlPoolCapacity: controlCapacity, ConnectorPoolCapacity: int64(config.ConnectorMaxConnections),
		},
		activeSlots: make(chan struct{}, config.MaxActive),
		rounds:      make(map[string]*concurrencyProbeRound),
	}, nil
}

func (probe *ConcurrencyProbe) Capacity() ConcurrencyProbeCapacity {
	if probe == nil {
		return ConcurrencyProbeCapacity{}
	}
	return probe.capacity
}

// Middleware records arrivals before any client can enter the real MCP
// handler. The last expected arrival releases the barrier. All subsequent
// execution, settlement, receipt, and artifact work remains the production
// handler; the probe never fabricates a response.
func (probe *ConcurrencyProbe) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		roundSHA := strings.TrimSpace(request.Header.Get(ConcurrencyRoundHeader))
		if roundSHA == "" {
			next.ServeHTTP(w, request)
			return
		}
		participant := strings.TrimSpace(request.Header.Get(ConcurrencyParticipantHeader))
		token := request.Header.Get(ConcurrencyAuthorizationHeader)
		if !probe.authorized(token) || !validConcurrencyProbeSHA256(roundSHA) ||
			!validConcurrencyProbeSHA256(participant) {
			http.Error(w, "invalid concurrency probe binding", http.StatusForbidden)
			return
		}
		round, release, err := probe.arrive(roundSHA, participant)
		if err != nil {
			http.Error(w, "concurrency probe rejected participant", http.StatusConflict)
			return
		}
		select {
		case <-release:
			probe.leaveBarrier(round)
		case <-request.Context().Done():
			probe.cancelBarrier(round)
			return
		}
		queued := false
		select {
		case probe.activeSlots <- struct{}{}:
		default:
			if !probe.enterQueue(round) {
				http.Error(w, "concurrency probe service queue is full", http.StatusServiceUnavailable)
				return
			}
			queued = true
			select {
			case probe.activeSlots <- struct{}{}:
			case <-request.Context().Done():
				probe.cancelQueue(round)
				return
			}
		}
		probe.enterActive(round, queued)
		defer func() {
			<-probe.activeSlots
			probe.complete(round)
		}()
		// The probe binding is consumed at the service boundary. Never pass the
		// private authorization token (or round metadata) into MCP logging,
		// tracing, tool dispatch, or application error paths.
		request.Header.Del(ConcurrencyAuthorizationHeader)
		request.Header.Del(ConcurrencyRoundHeader)
		request.Header.Del(ConcurrencyParticipantHeader)
		next.ServeHTTP(w, request)
	})
}

func (probe *ConcurrencyProbe) AdminHandler() http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			const prefix = "Bearer "
			authorization := request.Header.Get("Authorization")
			if !strings.HasPrefix(authorization, prefix) || !probe.authorized(strings.TrimPrefix(authorization, prefix)) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, request)
		})
	})
	router.Get("/capacity", func(w http.ResponseWriter, _ *http.Request) {
		writeConcurrencyProbeJSON(w, http.StatusOK, probe.capacity)
	})
	router.Post("/rounds", probe.createRoundHandler)
	router.Get("/rounds/{round_sha256}", probe.roundSnapshotHandler)
	router.Delete("/rounds/{round_sha256}", probe.deleteRoundHandler)
	return router
}

func (probe *ConcurrencyProbe) createRoundHandler(w http.ResponseWriter, request *http.Request) {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	var input concurrencyRoundRequest
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!validConcurrencyProbeSHA256(input.RoundSHA256) ||
		(input.Mode != "forced_queue_safety" && input.Mode != "natural_contention" && input.Mode != "serial") ||
		input.ExpectedWidth < 1 || int64(input.ExpectedWidth) > probe.capacity.HTTPActiveCapacity+probe.capacity.HTTPQueueCapacity {
		http.Error(w, "invalid concurrency round", http.StatusBadRequest)
		return
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.rounds) >= maximumConcurrencyProbeRoundCount {
		http.Error(w, "too many concurrency rounds", http.StatusConflict)
		return
	}
	if _, exists := probe.rounds[input.RoundSHA256]; exists {
		http.Error(w, "concurrency round exists", http.StatusConflict)
		return
	}
	waitCount, waitNanos := int64(0), int64(0)
	if probe.poolStats != nil {
		stats := probe.poolStats()
		waitCount, waitNanos = stats.WaitCount, stats.WaitDuration.Nanoseconds()
	}
	probe.rounds[input.RoundSHA256] = &concurrencyProbeRound{
		mode: input.Mode, expected: input.ExpectedWidth, participants: make(map[string]struct{}, input.ExpectedWidth),
		release: make(chan struct{}), poolWaitStart: waitCount, poolWaitNanos: waitNanos,
	}
	writeConcurrencyProbeJSON(w, http.StatusCreated, probe.snapshotLocked(input.RoundSHA256, probe.rounds[input.RoundSHA256]))
}

func (probe *ConcurrencyProbe) roundSnapshotHandler(w http.ResponseWriter, request *http.Request) {
	roundSHA := chi.URLParam(request, "round_sha256")
	probe.mu.Lock()
	defer probe.mu.Unlock()
	round := probe.rounds[roundSHA]
	if round == nil {
		http.Error(w, "concurrency round not found", http.StatusNotFound)
		return
	}
	probe.observePoolLocked(round)
	writeConcurrencyProbeJSON(w, http.StatusOK, probe.snapshotLocked(roundSHA, round))
}

func (probe *ConcurrencyProbe) deleteRoundHandler(w http.ResponseWriter, request *http.Request) {
	roundSHA := chi.URLParam(request, "round_sha256")
	probe.mu.Lock()
	defer probe.mu.Unlock()
	round := probe.rounds[roundSHA]
	if round == nil {
		http.Error(w, "concurrency round not found", http.StatusNotFound)
		return
	}
	if round.barrierWait != 0 || round.active != 0 || round.queued != 0 ||
		round.completed+round.canceled+round.rejected != round.arrived {
		http.Error(w, "concurrency round is active", http.StatusConflict)
		return
	}
	delete(probe.rounds, roundSHA)
	w.WriteHeader(http.StatusNoContent)
}

func (probe *ConcurrencyProbe) arrive(roundSHA, participant string) (*concurrencyProbeRound, <-chan struct{}, error) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	round := probe.rounds[roundSHA]
	if round == nil || round.released {
		return nil, nil, errors.New("round is unavailable")
	}
	if _, duplicate := round.participants[participant]; duplicate || len(round.participants) >= round.expected {
		round.rejected++
		return nil, nil, errors.New("participant is duplicate or excessive")
	}
	round.participants[participant] = struct{}{}
	round.arrived++
	round.barrierWait++
	if round.barrierWait > round.peakBarrier {
		round.peakBarrier = round.barrierWait
	}
	probe.observePoolLocked(round)
	if len(round.participants) == round.expected {
		round.released = true
		close(round.release)
	}
	return round, round.release, nil
}

func (probe *ConcurrencyProbe) leaveBarrier(round *concurrencyProbeRound) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if round.barrierWait > 0 {
		round.barrierWait--
	}
}

func (probe *ConcurrencyProbe) cancelBarrier(round *concurrencyProbeRound) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if round.barrierWait > 0 {
		round.barrierWait--
	}
	round.canceled++
}

func (probe *ConcurrencyProbe) enterQueue(round *concurrencyProbeRound) bool {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if round.queued >= probe.capacity.HTTPQueueCapacity {
		round.rejected++
		return false
	}
	round.queued++
	if round.queued > round.peakQueued {
		round.peakQueued = round.queued
	}
	return true
}

func (probe *ConcurrencyProbe) cancelQueue(round *concurrencyProbeRound) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if round.queued > 0 {
		round.queued--
	}
	round.canceled++
}

func (probe *ConcurrencyProbe) enterActive(round *concurrencyProbeRound, queued bool) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if queued && round.queued > 0 {
		round.queued--
	}
	round.active++
	if round.active > round.peakActive {
		round.peakActive = round.active
	}
	probe.observePoolLocked(round)
}

func (probe *ConcurrencyProbe) complete(round *concurrencyProbeRound) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if round.active > 0 {
		round.active--
	}
	round.completed++
	probe.observePoolLocked(round)
}

func (probe *ConcurrencyProbe) observePoolLocked(round *concurrencyProbeRound) {
	if probe.poolStats == nil {
		return
	}
	stats := probe.poolStats()
	if int64(stats.InUse) > round.peakPoolInUse {
		round.peakPoolInUse = int64(stats.InUse)
	}
}

func (probe *ConcurrencyProbe) snapshotLocked(roundSHA string, round *concurrencyProbeRound) ConcurrencyProbeSnapshot {
	participants := make([]string, 0, len(round.participants))
	for participant := range round.participants {
		participants = append(participants, participant)
	}
	poolWaitCount, poolWaitNanos := int64(0), int64(0)
	if probe.poolStats != nil {
		stats := probe.poolStats()
		poolWaitCount = stats.WaitCount - round.poolWaitStart
		poolWaitNanos = stats.WaitDuration.Nanoseconds() - round.poolWaitNanos
	}
	return ConcurrencyProbeSnapshot{
		ConcurrencyProbeCapacity: probe.capacity,
		RoundSHA256:              roundSHA, Mode: round.mode, ExpectedWidth: int64(round.expected),
		Arrived: round.arrived, UniqueParticipants: int64(len(round.participants)),
		ParticipantSetSHA256: ConcurrencyParticipantSetSHA256(participants),
		BarrierWaiting:       round.barrierWait, PeakBarrierWaiting: round.peakBarrier,
		Active: round.active, PeakActive: round.peakActive, Queued: round.queued, PeakQueued: round.peakQueued,
		Completed: round.completed, Canceled: round.canceled, Rejected: round.rejected,
		PeakControlPoolInUse: round.peakPoolInUse, ControlPoolWaitCountDelta: poolWaitCount,
		ControlPoolWaitNanoseconds: poolWaitNanos, Released: round.released,
	}
}

func (probe *ConcurrencyProbe) authorized(candidate string) bool {
	want, got := []byte(probe.token), []byte(candidate)
	return len(want) == len(got) && subtle.ConstantTimeCompare(want, got) == 1
}

func validConcurrencyProbeSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

// ConcurrencyParticipantSetSHA256 is exported so the source-controlled
// adapter and finalizer can independently recompute the server snapshot.
func ConcurrencyParticipantSetSHA256(values []string) string {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	hash := sha256.New()
	_, _ = hash.Write([]byte(concurrencyParticipantSetDomain))
	for _, value := range ordered {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeConcurrencyProbeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// WaitForConcurrencyProbeSnapshot polls a snapshot source without sleeping
// past context cancellation. It is kept here for focused probe tests and
// adapter-side orchestration.
func WaitForConcurrencyProbeSnapshot(ctx context.Context, interval time.Duration, load func(context.Context) (ConcurrencyProbeSnapshot, error), ready func(ConcurrencyProbeSnapshot) bool) (ConcurrencyProbeSnapshot, error) {
	if interval <= 0 || load == nil || ready == nil {
		return ConcurrencyProbeSnapshot{}, errors.New("invalid concurrency probe poll")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		snapshot, err := load(ctx)
		if err != nil {
			return ConcurrencyProbeSnapshot{}, err
		}
		if ready(snapshot) {
			return snapshot, nil
		}
		select {
		case <-ctx.Done():
			return ConcurrencyProbeSnapshot{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
