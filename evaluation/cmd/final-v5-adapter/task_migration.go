package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

const p68CliffDiagnosisMarker = "DIAGNOSIS-NOT-FOR-PUBLICATION"
const p68CliffDiagnosisEnv = "TASKGATE_P68_CLIFF_DIAGNOSIS"

type taskMigrationOperationContextKey struct{}

type taskMigrationOperationRecorder struct {
	operation experiment.AdapterOperation
	mu        sync.Mutex
	ordinals  map[string]int
	next      int
}

func withTaskMigrationOperation(ctx context.Context, operation experiment.AdapterOperation) context.Context {
	if os.Getenv(p68CliffDiagnosisEnv) != p68CliffDiagnosisMarker {
		return ctx
	}
	recorder := &taskMigrationOperationRecorder{operation: operation, ordinals: make(map[string]int)}
	return context.WithValue(ctx, taskMigrationOperationContextKey{}, recorder)
}

func recordTaskMigrationWait(ctx context.Context, taskID, taskRole, expectedState, lastState, status string,
	elapsed time.Duration, pollCount int, lastErr error) {
	recorder, _ := ctx.Value(taskMigrationOperationContextKey{}).(*taskMigrationOperationRecorder)
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	ordinal := recorder.ordinals[taskID]
	if ordinal == 0 {
		recorder.next++
		ordinal = recorder.next
		recorder.ordinals[taskID] = ordinal
	}
	recorder.mu.Unlock()
	lastError := "none"
	if lastErr != nil {
		lastError = lastErr.Error()
	}
	diagnostic := experiment.NewTaskMigrationWaitDiagnosticV1(recorder.operation, ordinal, taskRole, taskID,
		expectedState, lastState, status, elapsed, pollCount, lastError, time.Now())
	_ = json.NewEncoder(adapterDiagnosticOutput).Encode(diagnostic)
}

type taskMigrationWaitError struct {
	expected  string
	lastState string
	elapsed   time.Duration
	polls     int
	lastErr   error
	status    string
}

func (err *taskMigrationWaitError) Error() string {
	if err == nil {
		return "task state transition failed"
	}
	lastError := "none"
	if err.lastErr != nil {
		lastError = err.lastErr.Error()
	}
	return fmt.Sprintf("task state transition %s: expected=%s last_state=%s elapsed=%s polls=%d last_error=%s",
		err.status, err.expected, err.lastState, err.elapsed, err.polls, lastError)
}

func (err *taskMigrationWaitError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.lastErr
}

func isTaskMigrationWaitError(err error) bool {
	var target *taskMigrationWaitError
	return errors.As(err, &target)
}
