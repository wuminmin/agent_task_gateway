package main

import (
	"context"
	"errors"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

type fakeSequentialRuntime struct {
	closed   bool
	closeErr error
}

func (runtime *fakeSequentialRuntime) Close() error {
	if runtime.closeErr != nil {
		return runtime.closeErr
	}
	runtime.closed = true
	return nil
}

type fakeSequentialFactory struct {
	values []*fakeSequentialRuntime
}

func (factory *fakeSequentialFactory) Start(context.Context, sequentialSlotTarget) (sequentialSlotRuntime, error) {
	runtime := &fakeSequentialRuntime{}
	factory.values = append(factory.values, runtime)
	return runtime, nil
}

func sequentialTarget(day string, seed int) sequentialSlotTarget {
	return sequentialSlotTarget{Day: day, CatalogSHA256: contractDigest(seed),
		PublicationSHA256: contractDigest(seed + 1), HOTArtifactBytes: 139 << 20}
}

func TestSingleGatewayServiceSlotExactSwitchCheckRestore(t *testing.T) {
	factory := &fakeSequentialFactory{}
	slot, err := newSingleGatewayServiceSlot(factory)
	if err != nil {
		t.Fatal(err)
	}
	oldTarget, newTarget := sequentialTarget("day0", 1), sequentialTarget("day1", 10)
	steps := []struct {
		start  bool
		target sequentialSlotTarget
		reason string
	}{
		{true, oldTarget, "start_retained_old"},
		{false, sequentialSlotTarget{}, "stop_old_for_new_activation"},
		{true, newTarget, "start_new_after_activation"},
		{false, sequentialSlotTarget{}, "stop_new_for_retained_check"},
		{true, oldTarget, "start_old_for_retained_check"},
		{false, sequentialSlotTarget{}, "stop_old_for_new_restore"},
		{true, newTarget, "restore_new_after_retained_check"},
		{false, sequentialSlotTarget{}, "stop_new_cycle_complete"},
	}
	for index, step := range steps {
		if step.start {
			err = slot.Start(t.Context(), step.target, step.reason)
		} else {
			err = slot.Stop(step.reason)
		}
		if err != nil {
			t.Fatalf("step %d: %v", index+1, err)
		}
	}
	topology, lifecycle := slot.Evidence()
	if topology.Model != rq5fixture.TopologyModel || topology.RequestRouterPresent ||
		topology.MaxConcurrentServices != 1 || topology.ServiceStarts != 4 || topology.ServiceStops != 4 ||
		topology.FinalActiveServices != 0 || len(lifecycle) != 8 || len(factory.values) != 4 {
		t.Fatalf("topology=%#v lifecycle=%#v starts=%d", topology, lifecycle, len(factory.values))
	}
	seen := map[string]bool{}
	for index := 0; index < len(lifecycle); index += 2 {
		start, stop := lifecycle[index], lifecycle[index+1]
		if start.Action != "start" || stop.Action != "stop" || start.ServiceInstanceSHA256 != stop.ServiceInstanceSHA256 ||
			seen[start.ServiceInstanceSHA256] || !factory.values[index/2].closed {
			t.Fatalf("invalid boot pair %d: %#v %#v", index/2, start, stop)
		}
		seen[start.ServiceInstanceSHA256] = true
	}
}

func TestSingleGatewayServiceSlotRefusesOverlapAndEmptyStop(t *testing.T) {
	slot, _ := newSingleGatewayServiceSlot(&fakeSequentialFactory{})
	target := sequentialTarget("day0", 1)
	if err := slot.Stop("empty"); err == nil {
		t.Fatal("empty stop succeeded")
	}
	if err := slot.Start(t.Context(), target, "first"); err != nil {
		t.Fatal(err)
	}
	if err := slot.Start(t.Context(), target, "overlap"); err == nil {
		t.Fatal("overlapping start succeeded")
	}
}

func TestSingleGatewayServiceSlotFailedCloseRemainsOccupied(t *testing.T) {
	runtime := &fakeSequentialRuntime{closeErr: errors.New("still live")}
	factory := sequentialFactoryFunc(func(context.Context, sequentialSlotTarget) (sequentialSlotRuntime, error) {
		return runtime, nil
	})
	slot, _ := newSingleGatewayServiceSlot(factory)
	if err := slot.Start(t.Context(), sequentialTarget("day0", 1), "first"); err != nil {
		t.Fatal(err)
	}
	if err := slot.Stop("failed"); err == nil {
		t.Fatal("failed close was reported as stopped")
	}
	topology, lifecycle := slot.Evidence()
	if topology.FinalActiveServices != 1 || topology.ServiceStops != 0 || len(lifecycle) != 1 {
		t.Fatalf("failed close changed slot evidence: %#v %#v", topology, lifecycle)
	}
}

type sequentialFactoryFunc func(context.Context, sequentialSlotTarget) (sequentialSlotRuntime, error)

func (function sequentialFactoryFunc) Start(ctx context.Context, target sequentialSlotTarget) (sequentialSlotRuntime, error) {
	return function(ctx, target)
}
