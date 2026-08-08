package experiment

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/internal/approval"
)

const (
	// ObserverWindowTicketV3Version identifies the one-shot preregistration
	// capability issued for one TaskGate request attempt.
	ObserverWindowTicketV3Version = "taskgate-final-v5-observer-window-ticket-v3"
	observerWindowTicketV3Domain  = "TASKGATE-FINAL-V5-OBSERVER-WINDOW-TICKET-V3\x00"
)

// ObserverAttemptV3 names the request attempt whose observer window is about to
// open. Both identities exist before the request is sent: the task has already
// been provisioned, and the Adapter fixes its request id before taking the
// before snapshot.
type ObserverAttemptV3 struct {
	TaskID    string `json:"task_id"`
	RequestID string `json:"request_id"`
}

func (attempt ObserverAttemptV3) validate() error {
	if strings.TrimSpace(attempt.TaskID) == "" {
		return errors.New("opening an observer window requires the attempt's task id")
	}
	if strings.TrimSpace(attempt.RequestID) == "" {
		return errors.New("opening an observer window requires the attempt's request id")
	}
	return nil
}

// ObserverWindowTicketV3 is the finalizer-signed, one-use binding between a
// preregistered classifier, one observer window, and the exact TaskGate request
// whose signed receipt may consume that window.
//
// Every member is exported because the Adapter has to carry the ticket from
// OpenObserverWindowV3 to FinalizeTaskGateObservationV3. None is authoritative
// merely because it is present: the finalizer verifies the signature, compares
// the binding against the signed receipt and carried window, and atomically
// consumes its private outstanding record before deriving acceptance.
type ObserverWindowTicketV3 struct {
	Version                  string            `json:"version"`
	TicketID                 string            `json:"ticket_id"`
	ObserverWindowID         string            `json:"observer_window_id"`
	TaskID                   string            `json:"task_id"`
	RequestID                string            `json:"request_id"`
	Operation                OperationIdentity `json:"operation"`
	ClassifierManifestSHA256 string            `json:"classifier_manifest_sha256"`
	ClassifierBindingSHA256  string            `json:"classifier_binding_sha256"`
	Signature                string            `json:"signature"`
}

func (ticket ObserverWindowTicketV3) validate() error {
	if ticket.Version != ObserverWindowTicketV3Version {
		return fmt.Errorf("observer window ticket version %q is unsupported", ticket.Version)
	}
	if !validSHA256(ticket.TicketID) {
		return errors.New("observer window ticket carries no random ticket id")
	}
	if !validSHA256(ticket.ObserverWindowID) {
		return errors.New("observer window ticket carries no random window id")
	}
	if err := (ObserverAttemptV3{TaskID: ticket.TaskID, RequestID: ticket.RequestID}).validate(); err != nil {
		return err
	}
	if err := ticket.Operation.Validate(); err != nil {
		return fmt.Errorf("observer window ticket operation: %w", err)
	}
	if !validSHA256(ticket.ClassifierManifestSHA256) {
		return errors.New("observer window ticket names no classifier manifest")
	}
	if !validSHA256(ticket.ClassifierBindingSHA256) {
		return errors.New("observer window ticket names no classifier binding")
	}
	signature, err := base64.RawURLEncoding.DecodeString(ticket.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize ||
		base64.RawURLEncoding.EncodeToString(signature) != ticket.Signature {
		return errors.New("observer window ticket carries no canonical Ed25519 signature")
	}
	return nil
}

type observerWindowTicketPayloadV3 struct {
	Version                  string            `json:"version"`
	TicketID                 string            `json:"ticket_id"`
	ObserverWindowID         string            `json:"observer_window_id"`
	TaskID                   string            `json:"task_id"`
	RequestID                string            `json:"request_id"`
	Operation                OperationIdentity `json:"operation"`
	ClassifierManifestSHA256 string            `json:"classifier_manifest_sha256"`
	ClassifierBindingSHA256  string            `json:"classifier_binding_sha256"`
}

func (ticket ObserverWindowTicketV3) signingPayload() ([]byte, error) {
	canonical, err := approval.CanonicalJSON(observerWindowTicketPayloadV3{
		Version: ticket.Version, TicketID: ticket.TicketID,
		ObserverWindowID: ticket.ObserverWindowID,
		TaskID:           ticket.TaskID, RequestID: ticket.RequestID,
		Operation:                ticket.Operation,
		ClassifierManifestSHA256: ticket.ClassifierManifestSHA256,
		ClassifierBindingSHA256:  ticket.ClassifierBindingSHA256,
	})
	if err != nil {
		return nil, fmt.Errorf("canonicalize observer window ticket: %w", err)
	}
	payload := make([]byte, 0, len(observerWindowTicketV3Domain)+len(canonical))
	payload = append(payload, observerWindowTicketV3Domain...)
	payload = append(payload, canonical...)
	return payload, nil
}

type observerAttemptKeyV3 struct {
	taskID, requestID string
}

type observerWindowTicketRecordV3 struct {
	ticket   ObserverWindowTicketV3
	consumed bool
}

// observerWindowTicketsV3 is deliberately private finalizer state. The Adapter
// receives neither signing key nor registry access; it can only ask the opened
// finalizer to issue a ticket and later submit that ticket for consumption.
type observerWindowTicketsV3 struct {
	mu         sync.Mutex
	publicKey  ed25519.PublicKey
	privateKey ed25519.PrivateKey
	attempts   map[observerAttemptKeyV3]observerWindowTicketRecordV3
	identities map[string]struct{}
}

func newObserverWindowTicketsV3() (*observerWindowTicketsV3, error) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate the observer-window ticket key: %w", err)
	}
	return &observerWindowTicketsV3{
		publicKey:  append(ed25519.PublicKey(nil), publicKey...),
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
		attempts:   make(map[observerAttemptKeyV3]observerWindowTicketRecordV3),
		identities: make(map[string]struct{}),
	}, nil
}

func (registry *observerWindowTicketsV3) issue(attempt ObserverAttemptV3,
	registered PreRegisteredObservationV3) (ObserverWindowTicketV3, error) {
	var ticket ObserverWindowTicketV3
	if registry == nil || len(registry.privateKey) != ed25519.PrivateKeySize {
		return ticket, errors.New("observer window ticket issuer is not open")
	}
	if err := attempt.validate(); err != nil {
		return ticket, err
	}
	if err := registered.Operation.Validate(); err != nil {
		return ticket, fmt.Errorf("pre-registered operation: %w", err)
	}
	if !validSHA256(registered.ClassifierManifestSHA256) ||
		!validSHA256(registered.ClassifierBindingSHA256) {
		return ticket, errors.New("pre-registration carries no complete classifier commitment")
	}

	key := observerAttemptKeyV3{taskID: attempt.TaskID, requestID: attempt.RequestID}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.attempts[key]; exists {
		return ticket, errors.New("this task and request attempt already has an observer window ticket")
	}

	ticketID, err := registry.uniqueRandomIdentityLocked("")
	if err != nil {
		return ticket, err
	}
	windowID, err := registry.uniqueRandomIdentityLocked(ticketID)
	if err != nil {
		return ticket, err
	}
	ticket = ObserverWindowTicketV3{
		Version:  ObserverWindowTicketV3Version,
		TicketID: ticketID, ObserverWindowID: windowID,
		TaskID: attempt.TaskID, RequestID: attempt.RequestID,
		Operation:                registered.Operation,
		ClassifierManifestSHA256: registered.ClassifierManifestSHA256,
		ClassifierBindingSHA256:  registered.ClassifierBindingSHA256,
	}
	payload, err := ticket.signingPayload()
	if err != nil {
		return ObserverWindowTicketV3{}, err
	}
	ticket.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(registry.privateKey, payload))
	registry.identities[ticketID] = struct{}{}
	registry.identities[windowID] = struct{}{}
	registry.attempts[key] = observerWindowTicketRecordV3{ticket: ticket}
	return ticket, nil
}

// uniqueRandomIdentityLocked returns 256 random bits encoded in the lowercase
// digest-shaped form the observer protocol accepts. identities is retained for
// the finalizer lifetime, so uniqueness is enforced rather than only assumed
// from the random source.
func (registry *observerWindowTicketsV3) uniqueRandomIdentityLocked(disallowed string) (string, error) {
	for {
		var value [32]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", fmt.Errorf("generate an observer-window identity: %w", err)
		}
		encoded := hex.EncodeToString(value[:])
		_, exists := registry.identities[encoded]
		if !exists && encoded != disallowed {
			return encoded, nil
		}
	}
}

func (registry *observerWindowTicketsV3) verify(ticket ObserverWindowTicketV3) error {
	if registry == nil || len(registry.publicKey) != ed25519.PublicKeySize {
		return errors.New("observer window ticket verifier is not open")
	}
	if err := ticket.validate(); err != nil {
		return err
	}
	signature, _ := base64.RawURLEncoding.DecodeString(ticket.Signature)
	payload, err := ticket.signingPayload()
	if err != nil {
		return err
	}
	if !ed25519.Verify(registry.publicKey, payload, signature) {
		return errors.New("observer window ticket signature does not verify")
	}
	return nil
}

// consume atomically changes exactly one outstanding ticket into a retained
// tombstone. The record is not deleted: otherwise the same task/request pair
// could be opened again and the already-settled receipt replayed under a new
// ticket.
func (registry *observerWindowTicketsV3) consume(ticket ObserverWindowTicketV3) error {
	if registry == nil {
		return errors.New("observer window ticket registry is not open")
	}
	key := observerAttemptKeyV3{taskID: ticket.TaskID, requestID: ticket.RequestID}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	record, exists := registry.attempts[key]
	if !exists || record.ticket.TicketID != ticket.TicketID ||
		record.ticket.ObserverWindowID != ticket.ObserverWindowID ||
		record.ticket.Signature != ticket.Signature {
		return errors.New("observer window ticket is not outstanding in this finalizer")
	}
	if record.consumed {
		return errors.New("observer window ticket has already been consumed")
	}
	record.consumed = true
	registry.attempts[key] = record
	return nil
}

// verifyAndConsumeObserverWindowTicketV3 is called only after the Gateway
// receipt has validated and its signature has verified. It first authenticates
// the finalizer's ticket, then requires every attempt/window/classifier binding
// to agree, and only then performs the atomic one-way state transition.
func (finalizer *RuntimeFinalizerV3) verifyAndConsumeObserverWindowTicketV3(
	request FinalizationRequestV3) error {
	ticket := request.ObserverWindowTicket
	if err := finalizer.observerWindows.verify(ticket); err != nil {
		return fmt.Errorf("verify observer window ticket: %w", err)
	}
	if ticket.TaskID != request.Receipt.TaskID || ticket.RequestID != request.Receipt.RequestID {
		return errors.New("observer window ticket is bound to a different task or request than the signed receipt")
	}
	if !ticket.Operation.equalTo(request.Carried.Operation) {
		return errors.New("observer window ticket is bound to a different operation than the carried evidence")
	}
	if ticket.ClassifierManifestSHA256 != request.Carried.ClassifierManifestSHA256 {
		return errors.New("observer window ticket is bound to a different classifier manifest than the carried evidence")
	}
	if ticket.ClassifierBindingSHA256 != request.Carried.ClassifierBindingSHA256 {
		return errors.New("observer window ticket is bound to a different classifier binding than the carried evidence")
	}
	for _, snapshot := range []struct {
		name       string
		windowID   string
		manifestID string
	}{
		{name: "before", windowID: request.Carried.Window.Before.ObserverWindowID,
			manifestID: request.Carried.Window.Before.ClassifierManifestSHA256},
		{name: "after", windowID: request.Carried.Window.After.ObserverWindowID,
			manifestID: request.Carried.Window.After.ClassifierManifestSHA256},
	} {
		if snapshot.windowID != ticket.ObserverWindowID {
			return fmt.Errorf("observer window ticket is bound to a different window than the %s snapshot", snapshot.name)
		}
		if snapshot.manifestID != ticket.ClassifierManifestSHA256 {
			return fmt.Errorf("observer window ticket is bound to a different classifier than the %s snapshot", snapshot.name)
		}
	}
	if err := finalizer.observerWindows.consume(ticket); err != nil {
		return fmt.Errorf("consume observer window ticket: %w", err)
	}
	return nil
}
