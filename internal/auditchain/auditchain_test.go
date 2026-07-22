package auditchain

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestVerifyInclusionTraversesBeyondFiveHundredEvents(t *testing.T) {
	events := testAuditEvents(t, 505)
	terminal := events[10]
	predecessor := events[9]
	proof := InclusionProof{
		TerminalEvent:    terminal,
		PredecessorEvent: &predecessor,
		SuccessorEvents:  append([]Event(nil), events[11:]...),
		Checkpoint:       Checkpoint{Sequence: events[len(events)-1].Sequence, Hash: events[len(events)-1].CurrentHash},
	}
	if err := VerifyInclusion(proof); err != nil {
		t.Fatalf("VerifyInclusion: %v", err)
	}
	proof.SuccessorEvents = proof.SuccessorEvents[:len(proof.SuccessorEvents)-1]
	if err := VerifyInclusion(proof); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("truncated successor path error = %v, want %v", err, ErrInvalidProof)
	}
}

func TestVerifyInclusionDetectsTampering(t *testing.T) {
	events := testAuditEvents(t, 4)
	terminal := events[1]
	predecessor := events[0]
	proof := InclusionProof{
		TerminalEvent:    terminal,
		PredecessorEvent: &predecessor,
		SuccessorEvents:  append([]Event(nil), events[2:]...),
		Checkpoint:       Checkpoint{Sequence: events[3].Sequence, Hash: events[3].CurrentHash},
	}
	proof.TerminalEvent.Payload = json.RawMessage(`{"tampered":true}`)
	if err := VerifyInclusion(proof); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("tampered terminal error = %v, want %v", err, ErrInvalidProof)
	}

	proof = InclusionProof{
		TerminalEvent:    terminal,
		PredecessorEvent: &predecessor,
		SuccessorEvents:  append([]Event(nil), events[2:]...),
		Checkpoint:       Checkpoint{Sequence: events[3].Sequence, Hash: events[3].CurrentHash},
	}
	proof.SuccessorEvents[0].PreviousHash = GenesisHash
	if err := VerifyInclusion(proof); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("broken successor link error = %v, want %v", err, ErrInvalidProof)
	}
}

func TestSignedCheckpointAnchorBindsCheckpointAndKeyWindow(t *testing.T) {
	signer, err := NewAnchorSigner("audit-anchor-2026-q3", ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x71}, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("NewAnchorSigner: %v", err)
	}
	checkpoint := Checkpoint{Sequence: 12, Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	signedAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	anchor, err := signer.SignCheckpoint(checkpoint, signedAt)
	if err != nil {
		t.Fatalf("SignCheckpoint: %v", err)
	}
	verifier, err := NewAnchorVerifier([]AnchorVerifyingKey{{
		KeyID: signer.KeyID(), PublicKey: signer.PublicKey(),
		ValidFrom: signedAt.Add(-time.Hour), RetiredAt: signedAt.Add(time.Hour),
	}})
	if err != nil {
		t.Fatalf("NewAnchorVerifier: %v", err)
	}
	if err := verifier.Verify(anchor); err != nil {
		t.Fatalf("Verify anchor: %v", err)
	}

	tampered := anchor
	tampered.Hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidAnchor) {
		t.Fatalf("hash tamper error = %v, want %v", err, ErrInvalidAnchor)
	}
	tampered = anchor
	tampered.SignedAt = tampered.SignedAt.Add(time.Millisecond)
	if err := verifier.Verify(tampered); !errors.Is(err, ErrInvalidAnchorSignature) {
		t.Fatalf("signed_at tamper error = %v, want %v", err, ErrInvalidAnchorSignature)
	}

	retiredVerifier, err := NewAnchorVerifier([]AnchorVerifyingKey{{
		KeyID: signer.KeyID(), PublicKey: signer.PublicKey(),
		ValidFrom: signedAt.Add(-2 * time.Hour), RetiredAt: signedAt.Add(-time.Nanosecond),
	}})
	if err != nil {
		t.Fatalf("NewAnchorVerifier retired: %v", err)
	}
	if err := retiredVerifier.Verify(anchor); !errors.Is(err, ErrAnchorKeyNotValid) {
		t.Fatalf("retired anchor key error = %v, want %v", err, ErrAnchorKeyNotValid)
	}
}

func testAuditEvents(t *testing.T, count int) []Event {
	t.Helper()
	events := make([]Event, 0, count)
	previous := GenesisHash
	start := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	for index := 0; index < count; index++ {
		event := Event{
			Sequence:   int64(index + 1),
			EventID:    fmt.Sprintf("audit-%03d", index+1),
			TaskID:     "task-audit",
			QueryID:    "query-audit",
			Actor:      "alice",
			EventType:  "QUERY_COMPLETED",
			Payload:    json.RawMessage(fmt.Sprintf(`{"index":%d}`, index)),
			OccurredAt: start.Add(time.Duration(index) * time.Millisecond),
		}
		event.PreviousHash = previous
		current, err := Hash(previous, event)
		if err != nil {
			t.Fatalf("Hash event %d: %v", index, err)
		}
		event.CurrentHash = current
		events = append(events, event)
		previous = current
	}
	return events
}
