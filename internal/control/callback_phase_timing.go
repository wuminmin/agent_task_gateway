package control

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

const CallbackPhaseTimingV1Record = "taskgate-control-submitted-callback-phase-timing-v1"

// CallbackPoolSnapshotV1Record is the periodic Control pool/in-flight snapshot
// emitted on the same diagnostic stream while callback phase timing is
// enabled. It exists so a stall that never reaches a phase trace (for example
// a handler blocked on pool acquisition before ApplyApprovalCallback) is still
// observable as pool exhaustion in the retained log.
const CallbackPoolSnapshotV1Record = "taskgate-control-pool-snapshot-v1"

const (
	callbackPhaseClaim          = "callback_claim"
	callbackPhaseTaskRowLock    = "task_row_lock"
	callbackPhaseAuditChainHead = "audit_chain_head"
	callbackPhaseCommit         = "commit"

	// callbackPhaseStallThreshold is the in-flight age after which a callback
	// trace is snapshotted while still running; it matches the analyzer's
	// stalled-operation criterion. callbackPhaseStallRepeat re-emits a still
	// running trace at this cadence so a long wedge leaves a timeline.
	callbackPhaseStallThreshold = 10 * time.Second
	callbackPhaseStallRepeat    = 60 * time.Second
	callbackPhaseWatchTick      = 5 * time.Second
	callbackPoolSnapshotEvery   = 30 * time.Second

	callbackPhaseResultInProgress = "in_progress"
	callbackPhaseFinalInFlight    = "in_flight"
	callbackPhaseCurrentBefore    = "before_first_phase"
	callbackPhaseCurrentBetween   = "between_phases"
)

var callbackPhaseOrder = [...]string{
	callbackPhaseClaim,
	callbackPhaseTaskRowLock,
	callbackPhaseAuditChainHead,
	callbackPhaseCommit,
}

// callbackPhaseTimingRecorder owns the diagnostic writer, the registry of
// in-flight traces, and the watchdog that snapshots stalled traces and pool
// statistics. It is only ever constructed behind WithCallbackPhaseTiming.
type callbackPhaseTimingRecorder struct {
	mu       sync.Mutex
	writer   io.Writer
	inflight map[*callbackPhaseTrace]struct{}
	stats    func() sql.DBStats
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	started  bool
	now      func() time.Time
}

type callbackPhaseTimingContextKey struct{}

type callbackPhaseTrace struct {
	recorder     *callbackPhaseTimingRecorder
	started      time.Time
	taskID       string
	eventID      string
	mu           sync.Mutex
	phases       map[string]CallbackPhaseTimingPhaseV1
	current      string
	lastSnapshot time.Time
	snapshots    int
}

// CallbackPhaseTimingV1 is diagnostic-only telemetry for one submitted OA
// callback transaction. Offsets and durations are computed from time.Time's
// monotonic component; wall time is present only to align this record with the
// retained evaluation logs.
//
// A record with FinalResult "in_flight" is a snapshot of a callback that has
// not finished: it is emitted once its age crosses the stall threshold, again
// at a fixed cadence while it keeps running, and at shutdown. Its in-progress
// phase carries the elapsed time so far and CurrentPhase names it.
type CallbackPhaseTimingV1 struct {
	SchemaVersion  int                          `json:"schema_version"`
	Record         string                       `json:"record"`
	TaskID         string                       `json:"task_id"`
	TaskIDSHA256   string                       `json:"task_id_sha256"`
	EventID        string                       `json:"event_id"`
	ObservedAt     string                       `json:"observed_at"`
	Phases         []CallbackPhaseTimingPhaseV1 `json:"phases"`
	FinalResult    string                       `json:"final_result"`
	ErrorClass     string                       `json:"error_class"`
	SnapshotReason string                       `json:"snapshot_reason,omitempty"`
	SnapshotAt     string                       `json:"snapshot_at,omitempty"`
	InFlightAgeMS  float64                      `json:"in_flight_age_ms,omitempty"`
	CurrentPhase   string                       `json:"current_phase,omitempty"`
	SnapshotIndex  int                          `json:"snapshot_index,omitempty"`
}

type CallbackPhaseTimingPhaseV1 struct {
	Name             string  `json:"name"`
	Attempted        bool    `json:"attempted"`
	StartedOffsetMS  float64 `json:"started_offset_ms"`
	FinishedOffsetMS float64 `json:"finished_offset_ms"`
	DurationMS       float64 `json:"duration_ms"`
	Result           string  `json:"result"`
}

// CallbackPoolSnapshotV1 is the periodic Control pool and in-flight summary.
type CallbackPoolSnapshotV1 struct {
	SchemaVersion        int            `json:"schema_version"`
	Record               string         `json:"record"`
	ObservedAt           string         `json:"observed_at"`
	Reason               string         `json:"reason"`
	PoolOpen             int            `json:"pool_open_connections"`
	PoolInUse            int            `json:"pool_in_use"`
	PoolIdle             int            `json:"pool_idle"`
	PoolMaxOpen          int            `json:"pool_max_open"`
	PoolWaitCount        int64          `json:"pool_wait_count"`
	PoolWaitDurationMS   float64        `json:"pool_wait_duration_ms"`
	InFlightCallbacks    int            `json:"in_flight_callbacks"`
	OldestInFlightAgeMS  float64        `json:"oldest_in_flight_age_ms"`
	StalledInFlight      int            `json:"stalled_in_flight"`
	StalledThresholdMS   float64        `json:"stalled_threshold_ms"`
	InFlightCurrentPhase map[string]int `json:"in_flight_current_phase,omitempty"`
}

// CallbackPhaseTaskIDSHA256 is shared with the evaluation-only diagnostic
// adapter so a private task ID can be joined to its operation/order record
// without making production behavior depend on evaluation metadata.
func CallbackPhaseTaskIDSHA256(taskID string) string {
	digest := sha256.Sum256([]byte("TASKGATE-CALLBACK-PHASE-TIMING-V1\x00" + taskID))
	return hex.EncodeToString(digest[:])
}

func newCallbackPhaseTimingRecorder(writer io.Writer, stats func() sql.DBStats) *callbackPhaseTimingRecorder {
	return &callbackPhaseTimingRecorder{writer: writer, inflight: make(map[*callbackPhaseTrace]struct{}),
		stats: stats, stop: make(chan struct{}), done: make(chan struct{}), now: time.Now}
}

// startWatchdog begins the stall/pool snapshot loop. It is only called for a
// recorder installed behind the diagnosis marker; ordinary stores have no
// recorder and therefore no goroutine.
func (r *callbackPhaseTimingRecorder) startWatchdog() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.started = true
	r.mu.Unlock()
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(callbackPhaseWatchTick)
		defer ticker.Stop()
		lastPool := r.now()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				now := r.now()
				r.snapshotStalled(now)
				if now.Sub(lastPool) >= callbackPoolSnapshotEvery {
					r.snapshotPool(now, "periodic")
					lastPool = now
				}
			}
		}
	}()
}

// stopWatchdog ends the loop and emits a final dump of every trace still in
// flight, so a Gateway stopped mid-wedge still records where each hung callback
// was. Safe to call more than once and on a nil recorder.
func (r *callbackPhaseTimingRecorder) stopWatchdog(reason string) {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.mu.Lock()
		started := r.started
		r.mu.Unlock()
		close(r.stop)
		if started {
			<-r.done
		}
		now := r.now()
		r.snapshotAll(now, reason)
		r.snapshotPool(now, reason)
	})
}

func (r *callbackPhaseTimingRecorder) register(trace *callbackPhaseTrace) {
	r.mu.Lock()
	r.inflight[trace] = struct{}{}
	r.mu.Unlock()
}

func (r *callbackPhaseTimingRecorder) unregister(trace *callbackPhaseTrace) {
	r.mu.Lock()
	delete(r.inflight, trace)
	r.mu.Unlock()
}

func (r *callbackPhaseTimingRecorder) inflightTraces() []*callbackPhaseTrace {
	r.mu.Lock()
	defer r.mu.Unlock()
	traces := make([]*callbackPhaseTrace, 0, len(r.inflight))
	for trace := range r.inflight {
		traces = append(traces, trace)
	}
	return traces
}

// snapshotStalled emits an in-flight record for every trace older than the
// stall threshold that has not been snapshotted within the repeat cadence.
func (r *callbackPhaseTimingRecorder) snapshotStalled(now time.Time) {
	for _, trace := range r.inflightTraces() {
		age := now.Sub(trace.started)
		if age < callbackPhaseStallThreshold {
			continue
		}
		trace.mu.Lock()
		due := trace.snapshots == 0 || now.Sub(trace.lastSnapshot) >= callbackPhaseStallRepeat
		trace.mu.Unlock()
		if due {
			trace.emitInFlight(now, "stall_threshold")
		}
	}
}

func (r *callbackPhaseTimingRecorder) snapshotAll(now time.Time, reason string) {
	for _, trace := range r.inflightTraces() {
		trace.emitInFlight(now, reason)
	}
}

func (r *callbackPhaseTimingRecorder) snapshotPool(now time.Time, reason string) {
	if r == nil {
		return
	}
	record := CallbackPoolSnapshotV1{SchemaVersion: 1, Record: CallbackPoolSnapshotV1Record,
		ObservedAt: now.UTC().Format(time.RFC3339Nano), Reason: reason,
		StalledThresholdMS:   callbackDurationMS(callbackPhaseStallThreshold),
		InFlightCurrentPhase: make(map[string]int)}
	if r.stats != nil {
		stats := r.stats()
		record.PoolOpen, record.PoolInUse, record.PoolIdle, record.PoolMaxOpen = stats.OpenConnections, stats.InUse, stats.Idle, stats.MaxOpenConnections
		record.PoolWaitCount, record.PoolWaitDurationMS = stats.WaitCount, callbackDurationMS(stats.WaitDuration)
	}
	for _, trace := range r.inflightTraces() {
		record.InFlightCallbacks++
		age := now.Sub(trace.started)
		if ageMS := callbackDurationMS(age); ageMS > record.OldestInFlightAgeMS {
			record.OldestInFlightAgeMS = ageMS
		}
		if age >= callbackPhaseStallThreshold {
			record.StalledInFlight++
		}
		trace.mu.Lock()
		record.InFlightCurrentPhase[trace.currentPhaseLocked()]++
		trace.mu.Unlock()
	}
	if len(record.InFlightCurrentPhase) == 0 {
		record.InFlightCurrentPhase = nil
	}
	r.write(record)
}

func (r *callbackPhaseTimingRecorder) write(record any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Diagnostic output must never become part of callback success semantics.
	_ = json.NewEncoder(r.writer).Encode(record)
}

func (s *Store) newCallbackPhaseTrace(taskID, eventID string) *callbackPhaseTrace {
	if s == nil || s.callbackPhaseTiming == nil {
		return nil
	}
	trace := &callbackPhaseTrace{
		recorder: s.callbackPhaseTiming, started: s.callbackPhaseTiming.now(), taskID: taskID, eventID: eventID,
		phases:  make(map[string]CallbackPhaseTimingPhaseV1, len(callbackPhaseOrder)),
		current: callbackPhaseCurrentBefore,
	}
	s.callbackPhaseTiming.register(trace)
	return trace
}

// SnapshotInflightCallbackPhases writes an in-flight record for every submitted
// callback transaction still running plus a pool snapshot, tagged with reason.
// The Gateway calls it at the start of shutdown so a stop mid-wedge cannot lose
// the hung traces to the stop grace period. It is a no-op unless callback phase
// timing is enabled and never affects callback processing.
func (s *Store) SnapshotInflightCallbackPhases(reason string) int {
	if s == nil || s.callbackPhaseTiming == nil {
		return 0
	}
	now := s.callbackPhaseTiming.now()
	traces := s.callbackPhaseTiming.inflightTraces()
	for _, trace := range traces {
		trace.emitInFlight(now, reason)
	}
	s.callbackPhaseTiming.snapshotPool(now, reason)
	return len(traces)
}

func withCallbackPhaseTrace(ctx context.Context, trace *callbackPhaseTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, callbackPhaseTimingContextKey{}, trace)
}

func callbackPhaseTraceFromContext(ctx context.Context) *callbackPhaseTrace {
	trace, _ := ctx.Value(callbackPhaseTimingContextKey{}).(*callbackPhaseTrace)
	return trace
}

// begin marks the phase as in progress immediately, so a snapshot taken while
// it runs names it as the current phase, and returns the closure that closes
// it with the observed result.
func (trace *callbackPhaseTrace) begin(name string) func(error) {
	if trace == nil {
		return func(error) {}
	}
	started := trace.recorder.now()
	trace.mu.Lock()
	trace.phases[name] = CallbackPhaseTimingPhaseV1{
		Name: name, Attempted: true,
		StartedOffsetMS: callbackDurationMS(started.Sub(trace.started)), Result: callbackPhaseResultInProgress,
	}
	trace.current = name
	trace.mu.Unlock()
	return func(err error) {
		finished := trace.recorder.now()
		result := "ok"
		if err != nil {
			result = "error"
		}
		trace.mu.Lock()
		trace.phases[name] = CallbackPhaseTimingPhaseV1{
			Name: name, Attempted: true,
			StartedOffsetMS:  callbackDurationMS(started.Sub(trace.started)),
			FinishedOffsetMS: callbackDurationMS(finished.Sub(trace.started)),
			DurationMS:       callbackDurationMS(finished.Sub(started)), Result: result,
		}
		trace.current = callbackPhaseCurrentBetween
		trace.mu.Unlock()
	}
}

// currentPhaseLocked reports the phase in progress, or where the trace is
// between phases. Callers hold trace.mu.
func (trace *callbackPhaseTrace) currentPhaseLocked() string {
	return trace.current
}

// phasesLocked returns the ordered phase list; a phase still in progress is
// reported with the elapsed time up to now. Callers hold trace.mu.
func (trace *callbackPhaseTrace) phasesLocked(now time.Time) []CallbackPhaseTimingPhaseV1 {
	phases := make([]CallbackPhaseTimingPhaseV1, 0, len(callbackPhaseOrder))
	for _, name := range callbackPhaseOrder {
		phase, present := trace.phases[name]
		if !present {
			phase = CallbackPhaseTimingPhaseV1{Name: name, Result: "not_attempted"}
		} else if phase.Result == callbackPhaseResultInProgress {
			phase.FinishedOffsetMS = callbackDurationMS(now.Sub(trace.started))
			phase.DurationMS = phase.FinishedOffsetMS - phase.StartedOffsetMS
		}
		phases = append(phases, phase)
	}
	return phases
}

// emitInFlight writes a snapshot of a trace that has not finished.
func (trace *callbackPhaseTrace) emitInFlight(now time.Time, reason string) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	trace.snapshots++
	trace.lastSnapshot = now
	record := CallbackPhaseTimingV1{
		SchemaVersion: 1, Record: CallbackPhaseTimingV1Record,
		TaskID: trace.taskID, TaskIDSHA256: CallbackPhaseTaskIDSHA256(trace.taskID), EventID: trace.eventID,
		ObservedAt: trace.started.UTC().Format(time.RFC3339Nano), Phases: trace.phasesLocked(now),
		FinalResult: callbackPhaseFinalInFlight, ErrorClass: "none",
		SnapshotReason: reason, SnapshotAt: now.UTC().Format(time.RFC3339Nano),
		InFlightAgeMS: callbackDurationMS(now.Sub(trace.started)), CurrentPhase: trace.currentPhaseLocked(),
		SnapshotIndex: trace.snapshots,
	}
	trace.mu.Unlock()
	trace.recorder.write(record)
}

func (trace *callbackPhaseTrace) finish(err error, replay bool) {
	if trace == nil {
		return
	}
	trace.recorder.unregister(trace)
	now := trace.recorder.now()
	trace.mu.Lock()
	phases := make([]CallbackPhaseTimingPhaseV1, 0, len(callbackPhaseOrder))
	for _, name := range callbackPhaseOrder {
		phase, present := trace.phases[name]
		if !present {
			phase = CallbackPhaseTimingPhaseV1{Name: name, Result: "not_attempted"}
		} else if phase.Result == callbackPhaseResultInProgress {
			// A phase whose closure never ran before the callback returned is
			// closed here as an error so the record stays well formed.
			phase.FinishedOffsetMS = callbackDurationMS(now.Sub(trace.started))
			phase.DurationMS = phase.FinishedOffsetMS - phase.StartedOffsetMS
			phase.Result = "error"
		}
		phases = append(phases, phase)
	}
	trace.mu.Unlock()
	finalResult := "committed"
	if replay {
		finalResult = "replay_committed"
	}
	errorClass := "none"
	if err != nil {
		finalResult = "error"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			errorClass = "context_deadline_exceeded"
		case errors.Is(err, context.Canceled):
			errorClass = "context_cancelled"
		default:
			errorClass = "other"
		}
	}
	record := CallbackPhaseTimingV1{
		SchemaVersion: 1, Record: CallbackPhaseTimingV1Record,
		TaskID: trace.taskID, TaskIDSHA256: CallbackPhaseTaskIDSHA256(trace.taskID), EventID: trace.eventID,
		ObservedAt: trace.started.UTC().Format(time.RFC3339Nano), Phases: phases,
		FinalResult: finalResult, ErrorClass: errorClass,
	}
	trace.recorder.write(record)
}

func callbackDurationMS(duration time.Duration) float64 {
	return float64(duration.Nanoseconds()) / float64(time.Millisecond)
}
