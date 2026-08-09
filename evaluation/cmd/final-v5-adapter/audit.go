package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

type publicAuditEvent struct {
	Sequence     int64           `json:"sequence"`
	EventID      string          `json:"event_id"`
	TaskID       string          `json:"task_id"`
	QueryID      string          `json:"query_id"`
	Actor        string          `json:"actor"`
	EventType    string          `json:"event_type"`
	Payload      json.RawMessage `json:"payload"`
	OccurredAt   time.Time       `json:"occurred_at"`
	PreviousHash string          `json:"previous_hash"`
	CurrentHash  string          `json:"current_hash"`
}

type publicAuditProof struct {
	TerminalEvent    publicAuditEvent   `json:"terminal_event"`
	PredecessorEvent *publicAuditEvent  `json:"predecessor_event"`
	SuccessorEvents  []publicAuditEvent `json:"successor_events"`
	Checkpoint       struct {
		Sequence int64  `json:"sequence"`
		Hash     string `json:"hash"`
	} `json:"checkpoint"`
}

type auditReceiptResponse struct {
	Receipt                 queryreceipt.QueryReceiptV1 `json:"receipt"`
	AuditInclusion          publicAuditProof            `json:"audit_inclusion"`
	ArtifactIntentInclusion struct {
		Terminal     publicAuditProof `json:"terminal"`
		Registration publicAuditProof `json:"registration"`
	} `json:"artifact_intent_inclusion"`
	AvailabilityEventInclusion *publicAuditProof `json:"availability_event_inclusion"`
}

type verifiedAuditEvidence struct {
	Audit        auditchain.InclusionProof
	Terminal     auditchain.InclusionProof
	Registration auditchain.InclusionProof
	Availability auditchain.InclusionProof
}

func (adapter *realAdapter) loadAuditEvidence(ctx context.Context, response queryResponse) (verifiedAuditEvidence, error) {
	var evidence auditReceiptResponse
	if err := adapter.carol.call(ctx, "get_audit_receipt", map[string]string{"receipt_id": response.QueryID}, &evidence); err != nil {
		return verifiedAuditEvidence{}, err
	}
	left, err := queryreceipt.DocumentSHA256(response.Receipt)
	if err != nil {
		return verifiedAuditEvidence{}, errors.New("query response receipt has no typed document identity")
	}
	right, err := queryreceipt.DocumentSHA256(evidence.Receipt)
	if err != nil {
		return verifiedAuditEvidence{}, errors.New("audit response receipt has no typed document identity")
	}
	if left != right {
		return verifiedAuditEvidence{}, errors.New("audit receipt differs from query receipt")
	}
	auditProof := evidence.AuditInclusion.proof()
	terminalProof := evidence.ArtifactIntentInclusion.Terminal.proof()
	registrationProof := evidence.ArtifactIntentInclusion.Registration.proof()
	if evidence.AvailabilityEventInclusion == nil {
		return verifiedAuditEvidence{}, errors.New("AVAILABLE result omitted availability inclusion")
	}
	availabilityProof := evidence.AvailabilityEventInclusion.proof()
	return verifiedAuditEvidence{Audit: auditProof, Terminal: terminalProof, Registration: registrationProof, Availability: availabilityProof}, nil
}

func (proof publicAuditProof) proof() auditchain.InclusionProof {
	result := auditchain.InclusionProof{TerminalEvent: proof.TerminalEvent.event(), Checkpoint: auditchain.Checkpoint{Sequence: proof.Checkpoint.Sequence, Hash: proof.Checkpoint.Hash}}
	if proof.PredecessorEvent != nil {
		value := proof.PredecessorEvent.event()
		result.PredecessorEvent = &value
	}
	for _, event := range proof.SuccessorEvents {
		result.SuccessorEvents = append(result.SuccessorEvents, event.event())
	}
	return result
}

func (event publicAuditEvent) event() auditchain.Event {
	return auditchain.Event{Sequence: event.Sequence, EventID: event.EventID, TaskID: event.TaskID, QueryID: event.QueryID, Actor: event.Actor, EventType: event.EventType, Payload: event.Payload, OccurredAt: event.OccurredAt, PreviousHash: event.PreviousHash, CurrentHash: event.CurrentHash}
}
