package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

type oaCallbackEvent struct {
	EventID                     string    `json:"event_id"`
	TaskID                      string    `json:"task_id"`
	DraftID                     string    `json:"draft_id"`
	Status                      string    `json:"status"`
	Actor                       string    `json:"actor"`
	OccurredAt                  time.Time `json:"occurred_at"`
	CatalogVersion              string    `json:"catalog_version"`
	CallbackContext             string    `json:"callback_context,omitempty"`
	AuthorizationSnapshotSHA256 string    `json:"authorization_snapshot_sha256"`
	ApprovalReceipt             string    `json:"approval_receipt,omitempty"`
}

func (s *Service) OACallbackHandler() http.Handler {
	return http.HandlerFunc(s.handleOACallback)
}

func (s *Service) handleOACallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil || len(rawBody) == 0 {
		writeCallbackError(w, http.StatusBadRequest, "invalid callback")
		return
	}
	eventID := r.Header.Get("X-OA-Event-ID")
	timestamp := r.Header.Get("X-OA-Timestamp")
	if eventID == "" || approval.Verify(s.callbackSecret, timestamp, r.Header.Get("X-OA-Signature"), rawBody, s.clock(), 5*time.Minute) != nil {
		writeCallbackError(w, http.StatusUnauthorized, "invalid callback signature")
		return
	}
	var event oaCallbackEvent
	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil || event.EventID != eventID || event.TaskID == "" || event.DraftID == "" || event.OccurredAt.IsZero() || !validSnapshotSHA256(event.AuthorizationSnapshotSHA256) {
		writeCallbackError(w, http.StatusBadRequest, "invalid callback")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeCallbackError(w, http.StatusBadRequest, "invalid callback")
		return
	}
	// Signature verification must always happen first, but a byte-for-byte
	// replay of an already completed event must not depend on mutable task or
	// catalog state (nor on the event body's original occurrence time).  OA
	// refreshes the signed request timestamp on retries while preserving the
	// event body, so return the transactionally stored response here.
	claim, err := s.store.LookupCallback(r.Context(), event.EventID, rawBody)
	if err == nil && claim.Replay {
		writeCallbackResponse(w, claim.Response)
		return
	}
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		writeControlCallbackError(w, err)
		return
	}
	if err == nil && claim.Status == control.CallbackProcessing {
		writeControlCallbackError(w, control.ErrCallbackInProgress)
		return
	}
	if delta := s.clock().Sub(event.OccurredAt); delta > 5*time.Minute || delta < -5*time.Minute {
		writeCallbackError(w, http.StatusUnauthorized, "stale callback")
		return
	}

	task, err := s.store.GetTask(r.Context(), event.TaskID)
	if err != nil {
		writeControlCallbackError(w, err)
		return
	}
	persistedPending, err := decodePersistedPending(task)
	if err != nil {
		writeCallbackError(w, http.StatusConflict, "callback does not match task")
		return
	}
	pending := persistedPending.pendingContext
	principal, err := s.store.GetPrincipal(r.Context(), task.PrincipalID)
	if err != nil {
		writeControlCallbackError(w, err)
		return
	}
	computedSnapshotSHA256, err := approval.AuthorizationSnapshotSHA256(authorizationDraftForPending(task, principal.Subject, pending))
	if err != nil || event.DraftID != task.ApprovalRef || event.CatalogVersion != task.CatalogVersion || event.CatalogVersion != s.catalog.CatalogVersion || event.CallbackContext != pending.CallbackContext || !sameSnapshotSHA256(computedSnapshotSHA256, persistedPending.AuthorizationSnapshotSHA256) || !sameSnapshotSHA256(event.AuthorizationSnapshotSHA256, persistedPending.AuthorizationSnapshotSHA256) {
		writeCallbackError(w, http.StatusConflict, "callback does not match task")
		return
	}
	if !validCallbackActor(event, pending) {
		writeCallbackError(w, http.StatusForbidden, "callback actor is not allowed")
		return
	}

	response := []byte(`{"ok":true}`)
	callback := control.ApprovalCallback{
		EventID: event.EventID, RawPayload: rawBody,
		Event: control.ApprovalEvent{EventID: event.EventID, TaskID: event.TaskID, Actor: event.Actor,
			Decision: event.Status, Payload: append(json.RawMessage(nil), rawBody...), CreatedAt: event.OccurredAt.UTC()},
		Response: response,
	}
	switch event.Status {
	case "submitted":
		callback.ExpectedState = control.TaskAwaitingSubmission
		callback.NewState = control.TaskAwaitingApproval
	case "approved":
		if event.ApprovalReceipt == "" {
			writeCallbackError(w, http.StatusBadRequest, "approval receipt is required")
			return
		}
		scope, _ := json.Marshal(pending.MandatoryScope)
		now := s.clock().UTC()
		callback.ExpectedState = control.TaskAwaitingApproval
		callback.NewState = control.TaskActive
		callback.Grant = &control.TaskGrant{
			TaskID: task.ID, Subject: principal.Subject, Purpose: task.Objective,
			ApprovedProducts: append([]string(nil), pending.Products...), ApprovedColumns: cloneColumns(pending.Columns),
			MandatoryScope: scope, SensitivityCeiling: string(pending.Sensitivity),
			Budget:    control.BudgetLimits{Queries: pending.Budget.MaxQueries, Rows: pending.Budget.MaxRows, DBMS: pending.Budget.MaxDBTime.Milliseconds()},
			ExpiresAt: now.Add(pending.Budget.TaskTTL), CatalogVersion: task.CatalogVersion,
			ApprovalReceipt: event.ApprovalReceipt, CreatedAt: now,
		}
	case "rejected":
		if event.ApprovalReceipt == "" {
			writeCallbackError(w, http.StatusBadRequest, "approval receipt is required")
			return
		}
		callback.ExpectedState = control.TaskAwaitingApproval
		callback.NewState = control.TaskArchived
		callback.Reason = control.TerminalRejected
	default:
		writeCallbackError(w, http.StatusBadRequest, "unknown callback status")
		return
	}
	claim, err = s.store.ApplyApprovalCallback(r.Context(), callback)
	if err != nil {
		writeControlCallbackError(w, err)
		return
	}
	if claim.Replay && len(claim.Response) != 0 {
		response = claim.Response
	}
	writeCallbackResponse(w, response)
}

func validSnapshotSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func sameSnapshotSHA256(actual, expected string) bool {
	if len(actual) != len(expected) || !validSnapshotSHA256(actual) || !validSnapshotSHA256(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) == 1
}

func writeCallbackResponse(w http.ResponseWriter, response []byte) {
	if len(response) == 0 {
		response = []byte(`{"ok":true}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

func validCallbackActor(event oaCallbackEvent, pending pendingContext) bool {
	switch event.Status {
	case "submitted":
		return strings.EqualFold(event.Actor, "alice")
	case "approved":
		if pending.ApprovalMode == "auto" {
			return event.Actor == "oa-auto"
		}
		return pending.ApprovalMode == "manual" && strings.EqualFold(event.Actor, pending.Approver)
	case "rejected":
		return pending.ApprovalMode == "manual" && strings.EqualFold(event.Actor, pending.Approver)
	default:
		return false
	}
}

func cloneColumns(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for product, columns := range source {
		result[product] = append([]string(nil), columns...)
	}
	return result
}

func writeControlCallbackError(w http.ResponseWriter, err error) {
	switch control.CodeOf(err) {
	case control.CodeNotFound:
		writeCallbackError(w, http.StatusNotFound, "task not found")
	case control.CodeIdempotencyConflict, control.CodeInvalidStateChange, control.CodeConflict, control.CodeCallbackInProgress:
		writeCallbackError(w, http.StatusConflict, "callback conflicts with current state")
	case control.CodeInvalid:
		writeCallbackError(w, http.StatusBadRequest, "invalid callback")
	default:
		writeCallbackError(w, http.StatusInternalServerError, "callback processing failed")
	}
}

func writeCallbackError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}
