// Package auditchain defines the exported hash material and inclusion verifier
// for TaskGate's append-only audit chain.
package auditchain

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

var ErrInvalidProof = errors.New("invalid audit inclusion proof")

type Event struct {
	Sequence     int64
	EventID      string
	TaskID       string
	QueryID      string
	Actor        string
	EventType    string
	Payload      json.RawMessage
	OccurredAt   time.Time
	PreviousHash string
	CurrentHash  string
}

type Checkpoint struct {
	Sequence int64
	Hash     string
}

type InclusionProof struct {
	TerminalEvent    Event
	PredecessorEvent *Event
	SuccessorEvents  []Event
	Checkpoint       Checkpoint
}

type hashMaterial struct {
	PreviousHash string          `json:"previous_hash"`
	EventID      string          `json:"event_id"`
	TaskID       string          `json:"task_id"`
	QueryID      string          `json:"query_id"`
	Actor        string          `json:"actor"`
	EventType    string          `json:"event_type"`
	Payload      json.RawMessage `json:"payload"`
	OccurredAt   string          `json:"occurred_at"`
}

func Hash(previous string, event Event) (string, error) {
	payload, err := NormalizePayload(event.Payload)
	if err != nil {
		return "", err
	}
	material, err := json.Marshal(hashMaterial{
		PreviousHash: previous,
		EventID:      event.EventID,
		TaskID:       event.TaskID,
		QueryID:      event.QueryID,
		Actor:        event.Actor,
		EventType:    event.EventType,
		Payload:      payload,
		OccurredAt:   FormatTime(event.OccurredAt),
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

func VerifyInclusion(proof InclusionProof) error {
	terminal := proof.TerminalEvent
	if terminal.Sequence <= 0 {
		return fmt.Errorf("%w: terminal sequence must be positive", ErrInvalidProof)
	}
	if err := validateHash(terminal.PreviousHash, "terminal previous hash"); err != nil {
		return err
	}
	if err := validateHash(terminal.CurrentHash, "terminal current hash"); err != nil {
		return err
	}
	if err := verifyLink(terminal.PreviousHash, terminal); err != nil {
		return err
	}
	if proof.PredecessorEvent == nil {
		if terminal.Sequence != 1 || terminal.PreviousHash != GenesisHash {
			return fmt.Errorf("%w: missing predecessor event", ErrInvalidProof)
		}
	} else {
		predecessor := *proof.PredecessorEvent
		if predecessor.Sequence != terminal.Sequence-1 || predecessor.CurrentHash != terminal.PreviousHash {
			return fmt.Errorf("%w: predecessor does not link to terminal event", ErrInvalidProof)
		}
		if err := verifyLink(predecessor.PreviousHash, predecessor); err != nil {
			return err
		}
	}
	if proof.Checkpoint.Sequence < terminal.Sequence {
		return fmt.Errorf("%w: checkpoint precedes terminal event", ErrInvalidProof)
	}
	if err := validateHash(proof.Checkpoint.Hash, "checkpoint hash"); err != nil {
		return err
	}
	previous := terminal
	for _, successor := range proof.SuccessorEvents {
		if successor.Sequence != previous.Sequence+1 {
			return fmt.Errorf("%w: successor sequence gap after %d", ErrInvalidProof, previous.Sequence)
		}
		if successor.PreviousHash != previous.CurrentHash {
			return fmt.Errorf("%w: successor does not link to previous event", ErrInvalidProof)
		}
		if err := verifyLink(successor.PreviousHash, successor); err != nil {
			return err
		}
		previous = successor
	}
	if previous.Sequence != proof.Checkpoint.Sequence || previous.CurrentHash != proof.Checkpoint.Hash {
		return fmt.Errorf("%w: path does not reach checkpoint", ErrInvalidProof)
	}
	return nil
}

func VerifyLinear(events []Event, checkpoint Checkpoint) error {
	previousHash := GenesisHash
	var previousSequence int64
	for _, event := range events {
		if event.Sequence != previousSequence+1 {
			return fmt.Errorf("%w: sequence gap at %d", ErrInvalidProof, event.Sequence)
		}
		if err := verifyLink(previousHash, event); err != nil {
			return err
		}
		previousHash = event.CurrentHash
		previousSequence = event.Sequence
	}
	if checkpoint.Sequence != previousSequence || checkpoint.Hash != previousHash {
		return fmt.Errorf("%w: checkpoint does not match event path", ErrInvalidProof)
	}
	return nil
}

func NormalizePayload(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func FormatTime(value time.Time) string {
	return value.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

func verifyLink(previousHash string, event Event) error {
	if event.Sequence <= 0 {
		return fmt.Errorf("%w: event sequence must be positive", ErrInvalidProof)
	}
	if strings.TrimSpace(event.EventID) == "" || strings.TrimSpace(event.Actor) == "" ||
		strings.TrimSpace(event.EventType) == "" || event.OccurredAt.IsZero() {
		return fmt.Errorf("%w: event identity fields are missing", ErrInvalidProof)
	}
	if event.PreviousHash != previousHash {
		return fmt.Errorf("%w: event previous hash mismatch", ErrInvalidProof)
	}
	if err := validateHash(previousHash, "previous hash"); err != nil {
		return err
	}
	if err := validateHash(event.CurrentHash, "current hash"); err != nil {
		return err
	}
	expected, err := Hash(previousHash, event)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProof, err)
	}
	if expected != event.CurrentHash {
		return fmt.Errorf("%w: event current hash mismatch", ErrInvalidProof)
	}
	return nil
}

func validateHash(value, name string) error {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return fmt.Errorf("%w: %s is not lowercase SHA-256", ErrInvalidProof, name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size || subtle.ConstantTimeEq(int32(len(decoded)), sha256.Size) != 1 {
		return fmt.Errorf("%w: %s is not valid SHA-256", ErrInvalidProof, name)
	}
	return nil
}
