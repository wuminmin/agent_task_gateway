package experiment

import (
	"context"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
)

func pairedNovelReceiptForObserverTicketV3(t *testing.T, taskID, requestID string) queryreceipt.QueryReceiptV1 {
	t.Helper()
	operation := prepareOperationV3(t)
	options := operation.fixtureOptions()
	options.Companion = operation.companionTarget()
	options.TaskID = taskID
	options.RequestID = requestID
	options.QueryID = "query-for-" + requestID
	receipt, err := fixture.PairedNovel(options)
	if err != nil {
		t.Fatalf("build the signed receipt: %v", err)
	}
	return receipt
}

func openedObserverTicketRequestV3(t *testing.T, finalizer *RuntimeFinalizerV3,
	receipt queryreceipt.QueryReceiptV1) (FinalizationRequestV3, FrozenContractSelectorV3) {
	t.Helper()
	selector := FrozenContractSelectorV3{
		ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: "100x4", Mode: "novel",
	}
	opened, err := finalizer.OpenObserverWindowV3(context.Background(), selector, ObserverAttemptV3{
		TaskID: receipt.TaskID, RequestID: receipt.RequestID,
	})
	if err != nil {
		t.Fatalf("open the observer window: %v", err)
	}
	carried := carriedFor(t, finalizerInputs(t))
	if !opened.Operation.equalTo(carried.Operation) ||
		opened.ClassifierManifestSHA256 != carried.ClassifierManifestSHA256 ||
		opened.ClassifierBindingSHA256 != carried.ClassifierBindingSHA256 ||
		!opened.Plan.Equal(carried.Plan) {
		t.Fatal("the public preregistration does not match the honest carried classification")
	}
	carried.Window.Before.ObserverWindowID = opened.ObserverWindowID
	carried.Window.After.ObserverWindowID = opened.ObserverWindowID
	carried.Window.Before.ClassifierManifestSHA256 = opened.ClassifierManifestSHA256
	carried.Window.After.ClassifierManifestSHA256 = opened.ClassifierManifestSHA256
	return FinalizationRequestV3{
		Receipt: receipt, Carried: carried, ContractSelector: selector,
		ObserverWindowTicket: opened.ObserverWindowTicket,
	}, selector
}

// A stable frozen operation may run repeatedly. Every request attempt gets a
// fresh ticket and window, while the same task/request pair can never select a
// second window.
func TestObserverWindowTicketsAreUniquePerAttemptAndDuplicateOpenIsRefused(t *testing.T) {
	finalizer := runtimeFinalizerFor(t, honestCandidate(t))
	selector := FrozenContractSelectorV3{
		ExperimentID: "artifact", WorkloadID: "result-heavy", Scale: "100x4", Mode: "novel",
	}
	firstAttempt := ObserverAttemptV3{TaskID: "task-ticket-one", RequestID: "request-ticket-one"}
	first, err := finalizer.OpenObserverWindowV3(context.Background(), selector, firstAttempt)
	if err != nil {
		t.Fatalf("open the first attempt: %v", err)
	}
	second, err := finalizer.OpenObserverWindowV3(context.Background(), selector,
		ObserverAttemptV3{TaskID: "task-ticket-two", RequestID: "request-ticket-two"})
	if err != nil {
		t.Fatalf("open the second attempt: %v", err)
	}
	if first.Operation.OperationID != second.Operation.OperationID {
		t.Fatal("the uniqueness test did not open two attempts of the same stable operation")
	}
	if first.ObserverWindowID == second.ObserverWindowID ||
		first.ObserverWindowTicket.TicketID == second.ObserverWindowTicket.TicketID {
		t.Fatal("two attempts of one stable operation share a ticket or observer window id")
	}
	for name, value := range map[string]string{
		"first ticket":  first.ObserverWindowTicket.TicketID,
		"first window":  first.ObserverWindowID,
		"second ticket": second.ObserverWindowTicket.TicketID,
		"second window": second.ObserverWindowID,
	} {
		if !validSHA256(value) {
			t.Errorf("%s is not a random 256-bit lowercase identity: %q", name, value)
		}
	}
	if err := finalizer.observerWindows.verify(first.ObserverWindowTicket); err != nil {
		t.Fatalf("the first issued ticket does not verify: %v", err)
	}
	if _, err := finalizer.OpenObserverWindowV3(context.Background(), selector, firstAttempt); err == nil {
		t.Fatal("the same task/request attempt opened a second observer window")
	}
}

// Ticket authentication and all external equality checks happen before the
// one-way consume. A rejected forgery or splice must not destroy the honest
// outstanding ticket for that attempt.
func TestObserverWindowTicketTamperAndBindingMismatchesAreRefusedBeforeConsume(t *testing.T) {
	tests := map[string]func(*testing.T, *RuntimeFinalizerV3, *FinalizationRequestV3){
		"missing ticket": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.ObserverWindowTicket = ObserverWindowTicketV3{}
		},
		"signed ticket field tamper": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.ObserverWindowTicket.TicketID = strings.Repeat("f", 64)
		},
		"different signed receipt attempt": func(t *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Receipt = pairedNovelReceiptForObserverTicketV3(t, "task-ticket-other", "request-ticket-other")
		},
		"different carried operation": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Carried.Operation.OperationID = "another-operation"
		},
		"different carried manifest": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Carried.ClassifierManifestSHA256 = strings.Repeat("e", 64)
		},
		"different carried binding": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Carried.ClassifierBindingSHA256 = strings.Repeat("e", 64)
		},
		"different before window": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Carried.Window.Before.ObserverWindowID = strings.Repeat("e", 64)
		},
		"different after window": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Carried.Window.After.ObserverWindowID = strings.Repeat("e", 64)
		},
		"different snapshot classifier": func(_ *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			request.Carried.Window.Before.ClassifierManifestSHA256 = strings.Repeat("e", 64)
		},
		"ticket from another finalizer": func(t *testing.T, _ *RuntimeFinalizerV3, request *FinalizationRequestV3) {
			other := runtimeFinalizerFor(t, honestCandidate(t))
			foreign, _ := openedObserverTicketRequestV3(t, other, request.Receipt)
			request.ObserverWindowTicket = foreign.ObserverWindowTicket
		},
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			finalizer := runtimeFinalizerFor(t, honestCandidate(t))
			receipt := pairedNovelReceiptForObserverTicketV3(t, "task-ticket-mismatch", "request-ticket-mismatch")
			honest, _ := openedObserverTicketRequestV3(t, finalizer, receipt)
			changed := honest
			mutate(t, finalizer, &changed)
			if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), changed); err == nil {
				t.Fatal("a tampered or mismatched observer window ticket binding was accepted")
			}
			if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), honest); err != nil {
				t.Fatalf("the rejected mismatch consumed the honest ticket: %v", err)
			}
		})
	}
}

// Once ticket authentication and equality have passed, consumption precedes
// the expensive acceptance derivation. A later evidence failure burns the
// ticket fail-closed rather than permitting the caller to edit and resubmit it.
func TestObserverWindowTicketIsConsumedBeforeLaterFinalizationFailure(t *testing.T) {
	finalizer := runtimeFinalizerFor(t, honestCandidate(t))
	receipt := pairedNovelReceiptForObserverTicketV3(t, "task-ticket-burn", "request-ticket-burn")
	honest, selector := openedObserverTicketRequestV3(t, finalizer, receipt)
	broken := honest
	broken.Carried.VisiblePreparedTargetBindingSHA256 = strings.Repeat("e", 64)
	if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), broken); err == nil {
		t.Fatal("the deliberately broken post-ticket evidence was accepted")
	}
	if _, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), honest); err == nil ||
		!strings.Contains(err.Error(), "already been consumed") {
		t.Fatalf("a ticket whose later finalization failed was reusable: %v", err)
	}
	if _, err := finalizer.OpenObserverWindowV3(context.Background(), selector, ObserverAttemptV3{
		TaskID: receipt.TaskID, RequestID: receipt.RequestID,
	}); err == nil {
		t.Fatal("a consumed task/request attempt reopened under a new ticket; the tombstone was not retained")
	}
}

// The mutex-protected transition is the authority, not call timing. Under a
// simultaneous replay storm exactly one caller may consume and finalize.
func TestObserverWindowTicketHasExactlyOneConcurrentConsumer(t *testing.T) {
	finalizer := runtimeFinalizerFor(t, honestCandidate(t))
	receipt := pairedNovelReceiptForObserverTicketV3(t, "task-ticket-concurrent", "request-ticket-concurrent")
	request, _ := openedObserverTicketRequestV3(t, finalizer, receipt)

	const contenders = 16
	start := make(chan struct{})
	results := make(chan error, contenders)
	for index := 0; index < contenders; index++ {
		go func() {
			<-start
			_, err := finalizer.FinalizeTaskGateObservationV3(context.Background(), request)
			results <- err
		}()
	}
	close(start)
	var succeeded, consumed int
	for index := 0; index < contenders; index++ {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "already been consumed"):
			consumed++
		default:
			t.Errorf("concurrent finalization failed for a reason other than consumption: %v", err)
		}
	}
	if succeeded != 1 || consumed != contenders-1 {
		t.Fatalf("concurrent consumers: %d succeeded and %d saw consumption; want 1 and %d",
			succeeded, consumed, contenders-1)
	}
}
