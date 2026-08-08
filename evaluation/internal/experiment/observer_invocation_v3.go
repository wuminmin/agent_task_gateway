package experiment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// This file is how an observer window is opened, and it is one place on purpose.
//
// The observer takes its window identity and its classifier commitment as
// command-line flags and seals them into the snapshot it returns. Assembling
// that argv by hand at each call site is how a flag comes to be spelled two ways
// or omitted: the adapter-side capture passes only --phase, so every invocation
// it makes is refused by an observer that requires three arguments -- a break
// nothing noticed, because the path it serves cannot run for other reasons
// either.
//
// So the invocation is a type, the argv is built once, and what comes back is
// required to be a snapshot of the invocation that was actually made.

// ObserverInvocationV3 is one observer run.
type ObserverInvocationV3 struct {
	// Phase is before or after.
	Phase string
	// ObserverWindowID ties the two snapshots of one measurement together.
	ObserverWindowID string
	// ClassifierManifestSHA256 is the classification this window is opened
	// under, computed by the finalizer before the operation runs. It is the
	// commitment ObserverWindowV2.Delta later enforces.
	ClassifierManifestSHA256 string
	// OperationBindingSHA256 is optional and names the prepared operation.
	OperationBindingSHA256 string
}

// Validate rejects an invocation the observer would refuse, so the failure names
// the wrong value rather than surfacing as a process exit status.
func (invocation ObserverInvocationV3) Validate() error {
	if invocation.Phase != "before" && invocation.Phase != "after" {
		return fmt.Errorf("an observer invocation is before or after, not %q", invocation.Phase)
	}
	for name, value := range map[string]string{
		"observer window id":  invocation.ObserverWindowID,
		"classifier manifest": invocation.ClassifierManifestSHA256,
	} {
		if !isLowercaseSHA256(value) {
			return fmt.Errorf("the %s must be a lowercase SHA-256, got %q", name, value)
		}
	}
	if invocation.OperationBindingSHA256 != "" && !isLowercaseSHA256(invocation.OperationBindingSHA256) {
		return fmt.Errorf("the operation binding must be a lowercase SHA-256 when supplied, got %q",
			invocation.OperationBindingSHA256)
	}
	return nil
}

// Argv is the exact command line, and the only place these flags are spelled.
func (invocation ObserverInvocationV3) Argv(executable string) ([]string, error) {
	if strings.TrimSpace(executable) == "" {
		return nil, errors.New("an observer invocation needs the observer executable")
	}
	if err := invocation.Validate(); err != nil {
		return nil, err
	}
	argv := []string{executable,
		"--phase", invocation.Phase,
		"--observer-window-id", invocation.ObserverWindowID,
		"--classifier-manifest-sha256", invocation.ClassifierManifestSHA256,
	}
	if invocation.OperationBindingSHA256 != "" {
		argv = append(argv, "--operation-binding-sha256", invocation.OperationBindingSHA256)
	}
	return argv, nil
}

// RunObserverV2 runs the observer and returns the snapshot it produced.
//
// The snapshot is required to describe the invocation that produced it. Without
// that a snapshot could carry any phase, any window and any commitment, and the
// window checks downstream would be comparing two documents that agree with each
// other about something neither was asked for.
func RunObserverV2(ctx context.Context, executable string, invocation ObserverInvocationV3,
	environment []string) (ObserverSnapshotV2, error) {
	argv, err := invocation.Argv(executable)
	if err != nil {
		return ObserverSnapshotV2{}, err
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = environment
	value, err := command.Output()
	if err != nil {
		return ObserverSnapshotV2{}, fmt.Errorf("run observer %s: %w", invocation.Phase, err)
	}
	var snapshot ObserverSnapshotV2
	if err := StrictJSON(value, &snapshot); err != nil {
		return ObserverSnapshotV2{}, fmt.Errorf("decode observer snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return ObserverSnapshotV2{}, err
	}
	if err := requireSnapshotMatchesInvocationV3(snapshot, invocation); err != nil {
		return ObserverSnapshotV2{}, err
	}
	return snapshot, nil
}

// requireSnapshotMatchesInvocationV3 holds the observer to what it was asked.
func requireSnapshotMatchesInvocationV3(snapshot ObserverSnapshotV2,
	invocation ObserverInvocationV3) error {
	for _, member := range []struct {
		name            string
		asked, reported string
	}{
		{"phase", invocation.Phase, snapshot.Phase},
		{"observer window id", invocation.ObserverWindowID, snapshot.ObserverWindowID},
		{"classifier manifest", invocation.ClassifierManifestSHA256, snapshot.ClassifierManifestSHA256},
		{"operation binding", invocation.OperationBindingSHA256, snapshot.OperationBindingSHA256},
	} {
		if member.asked != member.reported {
			return fmt.Errorf("the observer was invoked with %s %q but reported %q",
				member.name, member.asked, member.reported)
		}
	}
	return nil
}

// DeriveObserverWindowID names one measurement window.
//
// It is derived from the operation identity rather than issued by the finalizer,
// because it carries no scientific claim: it exists so that a before snapshot
// cannot be paired with an after snapshot from a different measurement. What the
// window is judged BY is the classifier manifest, and that one the finalizer
// computes -- see OpenObserverWindowV3.
//
// It is hashed because the observer requires a lowercase SHA-256, and domain
// separated so a window id cannot collide with another identifier derived from
// the same operation.
func DeriveObserverWindowID(operationID string) (string, error) {
	if strings.TrimSpace(operationID) == "" {
		return "", errors.New("an observer window is named after an operation, and none was given")
	}
	sum := sha256.Sum256([]byte("taskgate-final-v5-observer-window-v3\x00" + operationID))
	return hex.EncodeToString(sum[:]), nil
}

func isLowercaseSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
