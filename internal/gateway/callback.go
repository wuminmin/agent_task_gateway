package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/domain"
)

type oaCallbackEvent struct {
	EventID         string                      `json:"event_id"`
	TaskID          string                      `json:"task_id"`
	DraftID         string                      `json:"draft_id"`
	Status          string                      `json:"status"`
	Actor           string                      `json:"actor"`
	OccurredAt      time.Time                   `json:"occurred_at"`
	CatalogVersion  string                      `json:"catalog_version"`
	CallbackContext string                      `json:"callback_context,omitempty"`
	ManifestDigest  string                      `json:"manifest_digest"`
	ApprovedGrant   *approval.TaskGrantCoreV1   `json:"approved_grant,omitempty"`
	ApprovalReceipt *approval.ApprovalReceiptV1 `json:"approval_receipt,omitempty"`
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
	if err := decoder.Decode(&event); err != nil || event.EventID != eventID || event.TaskID == "" || event.DraftID == "" || event.OccurredAt.IsZero() || !validSnapshotSHA256(event.ManifestDigest) {
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
	if !manifestMatchesTask(persistedPending, task, principal, s.catalog.SHA256) ||
		event.DraftID != task.ApprovalRef || event.CatalogVersion != task.CatalogVersion ||
		event.CatalogVersion != s.catalog.CatalogVersion || event.CallbackContext != pending.CallbackContext ||
		!sameSnapshotSHA256(event.ManifestDigest, persistedPending.ManifestDigest) {
		writeCallbackError(w, http.StatusConflict, "callback does not match task")
		return
	}
	if !validCallbackActor(event, pending, persistedPending.Manifest.HumanSubject) {
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
		if event.ApprovedGrant != nil || event.ApprovalReceipt != nil {
			writeCallbackError(w, http.StatusBadRequest, "submitted callback cannot contain approval evidence")
			return
		}
		callback.ExpectedState = control.TaskAwaitingSubmission
		callback.NewState = control.TaskAwaitingApproval
	case "approved", "narrowed":
		finalGrant, err := s.validateApprovedGrant(event, persistedPending.Manifest)
		if err != nil {
			writeCallbackError(w, http.StatusConflict, "approval grant or receipt is invalid")
			return
		}
		scope, err := json.Marshal(finalGrant.Core.MandatoryScope)
		if err != nil {
			writeCallbackError(w, http.StatusBadRequest, "approved scope is invalid")
			return
		}
		encodedGrant, err := approval.EncodeTaskGrantV1(finalGrant)
		if err != nil {
			writeCallbackError(w, http.StatusBadRequest, "approved grant is invalid")
			return
		}
		callback.ExpectedState = control.TaskAwaitingApproval
		callback.NewState = control.TaskActive
		callback.Grant = &control.TaskGrant{
			TaskID: task.ID, Subject: finalGrant.Core.HumanSubject, Purpose: finalGrant.Core.DeclaredObjective,
			ApprovedProducts: append([]string(nil), finalGrant.Core.ApprovedProducts...), ApprovedColumns: cloneColumns(finalGrant.Core.ApprovedColumns),
			MandatoryScope: scope, SensitivityCeiling: string(finalGrant.Core.SensitivityCeiling),
			Budget: control.BudgetLimits{Queries: finalGrant.Core.Budget.MaxQueries,
				Rows: finalGrant.Core.Budget.MaxResultRows, DBMS: finalGrant.Core.Budget.MaxDBMS},
			ExpiresAt: finalGrant.Core.ExpiresAt, CatalogVersion: finalGrant.Core.CatalogVersion,
			CatalogDigest: finalGrant.Core.CatalogSHA256, DatasourceID: finalGrant.Core.DatasourceID,
			SchemaDigest:    finalGrant.Core.SchemaDigest,
			ApprovalReceipt: encodedGrant, CreatedAt: finalGrant.ApprovalReceipt.IssuedAt,
		}
	case "rejected":
		if err := s.validateRejectedReceipt(event, persistedPending.Manifest); err != nil {
			writeCallbackError(w, http.StatusConflict, "rejection receipt is invalid")
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
	if value != strings.ToLower(value) {
		return false
	}
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

func manifestMatchesTask(persisted persistedPendingContext, task control.Task, principal control.Principal, catalogSHA256 string) bool {
	manifest := persisted.Manifest
	pending := persisted.pendingContext
	if manifest.TaskID != task.ID || manifest.HumanSubject != principal.Subject || manifest.AgentID != principal.ID ||
		manifest.DeclaredObjective != task.Objective || manifest.CatalogVersion != task.CatalogVersion ||
		manifest.CatalogSHA256 != catalogSHA256 || manifest.CallbackContext != pending.CallbackContext ||
		manifest.Sensitivity != pending.Sensitivity || manifest.DatasourceID != pending.DatasourceID ||
		manifest.SchemaDigest != pending.SchemaDigest {
		return false
	}
	if manifest.Budget != authorizationBudget(pending.Budget) || !sameCanonicalJSON(manifest.Products, pending.Products) ||
		!sameCanonicalJSON(manifest.ApprovedColumns, pending.Columns) || !sameCanonicalJSON(manifest.MandatoryScope, pending.MandatoryScope) {
		return false
	}
	digest, err := approval.ManifestDigest(manifest)
	return err == nil && sameSnapshotSHA256(digest, persisted.ManifestDigest)
}

func sameCanonicalJSON(left, right any) bool {
	leftJSON, leftErr := approval.CanonicalJSON(left)
	rightJSON, rightErr := approval.CanonicalJSON(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func (s *Service) validateApprovedGrant(event oaCallbackEvent, manifest approval.AuthorizationManifestV1) (approval.TaskGrantV1, error) {
	if event.ApprovedGrant == nil || event.ApprovalReceipt == nil {
		return approval.TaskGrantV1{}, errors.New("approved grant and receipt are required")
	}
	receipt := *event.ApprovalReceipt
	candidate := *event.ApprovedGrant
	expectedDecision := approval.ApprovalDecisionApprove
	if event.Status == "narrowed" {
		expectedDecision = approval.ApprovalDecisionNarrow
	}
	if receipt.Decision != expectedDecision || receipt.TaskID != event.TaskID ||
		!sameSnapshotSHA256(receipt.ManifestDigest, event.ManifestDigest) ||
		receipt.ApproverID != event.Actor || !receipt.IssuedAt.Equal(event.OccurredAt) {
		return approval.TaskGrantV1{}, errors.New("receipt does not match callback")
	}
	if candidate.TaskID != event.TaskID || !sameSnapshotSHA256(candidate.ManifestDigest, event.ManifestDigest) {
		return approval.TaskGrantV1{}, errors.New("grant does not match callback")
	}
	if !candidate.ExpiresAt.Equal(receipt.IssuedAt.UTC().Add(time.Duration(candidate.Budget.TaskTTLMS) * time.Millisecond)) {
		return approval.TaskGrantV1{}, errors.New("grant expiry is not derived from its signed TTL")
	}
	parent, err := domain.CoreFromManifest(manifest, event.ManifestDigest, receipt.IssuedAt)
	if err != nil {
		return approval.TaskGrantV1{}, err
	}
	if err := parent.CheckNarrowing(candidate); err != nil {
		return approval.TaskGrantV1{}, err
	}
	parentDigest, err := approval.GrantCoreDigest(parent)
	if err != nil {
		return approval.TaskGrantV1{}, err
	}
	candidateDigest, err := approval.GrantCoreDigest(candidate)
	if err != nil {
		return approval.TaskGrantV1{}, err
	}
	if event.Status == "approved" && candidateDigest != parentDigest {
		return approval.TaskGrantV1{}, errors.New("approve must preserve the complete manifest envelope")
	}
	if event.Status == "narrowed" && candidateDigest == parentDigest {
		return approval.TaskGrantV1{}, errors.New("narrow must reduce the authorization envelope")
	}
	if !sameSnapshotSHA256(receipt.ApprovedGrantDigest, candidateDigest) {
		return approval.TaskGrantV1{}, errors.New("receipt does not bind the approved grant")
	}
	finalGrant := approval.TaskGrantV1{
		Version: domain.TaskGrantV1Version, Core: candidate, ApprovalReceipt: receipt,
	}
	if err := approval.VerifyTaskGrantV1(s.receiptVerifier, finalGrant); err != nil {
		return approval.TaskGrantV1{}, fmt.Errorf("verify final grant: %w", err)
	}
	return finalGrant, nil
}

func (s *Service) validateRejectedReceipt(event oaCallbackEvent, manifest approval.AuthorizationManifestV1) error {
	if event.ApprovedGrant != nil || event.ApprovalReceipt == nil {
		return errors.New("rejection must contain only a receipt")
	}
	receipt := *event.ApprovalReceipt
	if receipt.Decision != approval.ApprovalDecisionReject || receipt.TaskID != event.TaskID ||
		!sameSnapshotSHA256(receipt.ManifestDigest, event.ManifestDigest) || receipt.ApproverID != event.Actor ||
		!receipt.IssuedAt.Equal(event.OccurredAt) || receipt.ApprovedGrantDigest != "" {
		return errors.New("rejection receipt does not match callback")
	}
	manifestDigest, err := approval.ManifestDigest(manifest)
	if err != nil || !sameSnapshotSHA256(manifestDigest, receipt.ManifestDigest) {
		return errors.New("rejection receipt does not match manifest")
	}
	return s.receiptVerifier.VerifyReceipt(receipt)
}

func validCallbackActor(event oaCallbackEvent, pending pendingContext, humanSubject string) bool {
	switch event.Status {
	case "submitted":
		return strings.EqualFold(event.Actor, humanSubject)
	case "approved":
		if pending.ApprovalMode == "auto" {
			return strings.EqualFold(event.Actor, humanSubject)
		}
		return pending.ApprovalMode == "manual" && strings.EqualFold(event.Actor, pending.Approver)
	case "narrowed", "rejected":
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
