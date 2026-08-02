package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

// sequentialSlotTarget contains only one Catalog's activation identity. The
// slot factory loads exactly this target's HOT artifact when Start succeeds.
type sequentialSlotTarget struct {
	Day               string
	CatalogSHA256     string
	PublicationSHA256 string
	HOTArtifactBytes  int64
}

// sequentialSlotRuntime is one production Gateway boot and all resources
// owned by that boot (connector, background context, and HTTP/service handle).
// Close must synchronously make the boot unavailable before returning.
type sequentialSlotRuntime interface {
	Close() error
}

type sequentialSlotFactory interface {
	Start(context.Context, sequentialSlotTarget) (sequentialSlotRuntime, error)
}

// singleGatewayServiceSlot deliberately has no lookup-by-task or routing API.
// An experiment must explicitly Stop the current boot and Start the required
// Catalog before it can obtain Active. This makes a four-Service request router
// unrepresentable in the formal RQ5 execution path.
type singleGatewayServiceSlot struct {
	mu sync.Mutex

	factory   sequentialSlotFactory
	active    sequentialSlotRuntime
	target    sequentialSlotTarget
	instance  string
	activeN   int64
	maxActive int64
	maxHOT    int64
	starts    int64
	stops     int64
	lifecycle []experiment.RQ5LifecycleStep
}

func newSingleGatewayServiceSlot(factory sequentialSlotFactory) (*singleGatewayServiceSlot, error) {
	if factory == nil {
		return nil, errors.New("sequential Gateway service-slot factory is required")
	}
	return &singleGatewayServiceSlot{factory: factory}, nil
}

func (slot *singleGatewayServiceSlot) Start(ctx context.Context, target sequentialSlotTarget, reason string) error {
	if slot == nil || slot.factory == nil || target.Day == "" || !sha256Regexp.MatchString(target.CatalogSHA256) ||
		!sha256Regexp.MatchString(target.PublicationSHA256) || target.HOTArtifactBytes <= 0 ||
		target.HOTArtifactBytes > rq5fixture.MaximumHOTBytes || reason == "" {
		return errors.New("sequential Gateway start target is invalid")
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.active != nil || slot.activeN != 0 {
		return errors.New("single Gateway service slot is already occupied; stop must complete before start")
	}
	runtime, err := slot.factory.Start(ctx, target)
	if err != nil {
		return err
	}
	if runtime == nil {
		return errors.New("Gateway slot factory returned a nil production runtime")
	}
	instance, err := newRQ5ServiceInstanceSHA256(target, slot.starts+1)
	if err != nil {
		_ = runtime.Close()
		return err
	}
	slot.active, slot.target, slot.instance = runtime, target, instance
	slot.activeN = 1
	slot.starts++
	if slot.activeN > slot.maxActive {
		slot.maxActive = slot.activeN
	}
	if target.HOTArtifactBytes > slot.maxHOT {
		slot.maxHOT = target.HOTArtifactBytes
	}
	slot.lifecycle = append(slot.lifecycle, experiment.RQ5LifecycleStep{
		Sequence: len(slot.lifecycle) + 1, Action: "start", Reason: reason, Day: target.Day,
		CatalogSHA256: target.CatalogSHA256, PublicationSHA256: target.PublicationSHA256,
		ServiceInstanceSHA256: instance, ActiveBefore: 0, ActiveAfter: 1,
	})
	return nil
}

func (slot *singleGatewayServiceSlot) Stop(reason string) error {
	if slot == nil || reason == "" {
		return errors.New("sequential Gateway stop reason is required")
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.active == nil || slot.activeN != 1 {
		return errors.New("single Gateway service slot is empty")
	}
	if err := slot.active.Close(); err != nil {
		// A failed synchronous close leaves the slot occupied and prevents any
		// replacement from starting. This is safer than claiming a restart.
		return fmt.Errorf("stop active Gateway boot: %w", err)
	}
	slot.lifecycle = append(slot.lifecycle, experiment.RQ5LifecycleStep{
		Sequence: len(slot.lifecycle) + 1, Action: "stop", Reason: reason, Day: slot.target.Day,
		CatalogSHA256: slot.target.CatalogSHA256, PublicationSHA256: slot.target.PublicationSHA256,
		ServiceInstanceSHA256: slot.instance, ActiveBefore: 1, ActiveAfter: 0,
	})
	slot.active, slot.target, slot.instance = nil, sequentialSlotTarget{}, ""
	slot.activeN = 0
	slot.stops++
	return nil
}

func (slot *singleGatewayServiceSlot) Active() (sequentialSlotRuntime, sequentialSlotTarget, error) {
	if slot == nil {
		return nil, sequentialSlotTarget{}, errors.New("Gateway service slot is nil")
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.active == nil || slot.activeN != 1 {
		return nil, sequentialSlotTarget{}, errors.New("Gateway service slot has no active production boot")
	}
	return slot.active, slot.target, nil
}

func (slot *singleGatewayServiceSlot) Evidence() (experiment.RQ5TopologyEvidence, []experiment.RQ5LifecycleStep) {
	if slot == nil {
		return experiment.RQ5TopologyEvidence{}, nil
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	topology := experiment.RQ5TopologyEvidence{
		Model: rq5fixture.TopologyModel, Disclosure: rq5fixture.TopologyDisclosure,
		SingleServiceSlot: true, RequestRouterPresent: false, MaxConcurrentServices: slot.maxActive,
		ServiceStarts: slot.starts, ServiceStops: slot.stops, FinalActiveServices: slot.activeN,
		HOTArtifactLimitBytes: rq5fixture.MaximumHOTBytes, MaxActiveHOTArtifactBytes: slot.maxHOT,
	}
	return topology, append([]experiment.RQ5LifecycleStep(nil), slot.lifecycle...)
}

func newRQ5ServiceInstanceSHA256(target sequentialSlotTarget, sequence int64) (string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-RQ5-SERVICE-BOOT-V1\x00"))
	_, _ = hash.Write([]byte(target.Day))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(target.CatalogSHA256))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(target.PublicationSHA256))
	_, _ = hash.Write([]byte{0})
	_, _ = fmt.Fprintf(hash, "%d", sequence)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(nonce)
	return hex.EncodeToString(hash.Sum(nil)), nil
}
