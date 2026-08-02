package main

import (
	"context"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

type nilAdapterFixture struct{}

func (*nilAdapterFixture) Execute(context.Context, experiment.AdapterOperation) experiment.Sample {
	return experiment.Sample{}
}
func (*nilAdapterFixture) Close() {}

type nilMapAdapterFixture map[string]string

func (nilMapAdapterFixture) Execute(context.Context, experiment.AdapterOperation) experiment.Sample {
	return experiment.Sample{}
}
func (nilMapAdapterFixture) Close() {}

func TestCapabilitiesAreDerivedOnlyFromRealFactories(t *testing.T) {
	capabilities := implementedCapabilities()
	if len(capabilities) != len(experimentIDs) {
		t.Fatalf("capability count = %d, want %d", len(capabilities), len(experimentIDs))
	}
	for _, experimentID := range experimentIDs {
		factory, registered := adapterFactories[experimentID]
		implemented := registered && factory != nil
		if capabilities[experimentID] != implemented {
			t.Fatalf("capability %q = %v, real factory available = %v", experimentID, capabilities[experimentID], implemented)
		}
	}
	for _, experimentID := range experimentIDs {
		if !capabilities[experimentID] {
			t.Fatalf("completed adapter %q was not reported", experimentID)
		}
	}
}

func TestNilFactoryCannotEnableCapability(t *testing.T) {
	const experimentID = "rq5"
	previous, existed := adapterFactories[experimentID]
	adapterFactories[experimentID] = nil
	t.Cleanup(func() {
		if existed {
			adapterFactories[experimentID] = previous
		} else {
			delete(adapterFactories, experimentID)
		}
	})

	if implementedCapabilities()[experimentID] {
		t.Fatal("nil factory was reported as an implemented capability")
	}
	if adapter, code := initializeAdapter(context.Background(), experimentID); adapter != nil || code != "source_controlled_experiment_not_implemented" {
		t.Fatalf("nil factory initialization = (%T, %q), want fail-closed not-implemented", adapter, code)
	}
}

func TestFactoryReturningNilAdapterFailsClosed(t *testing.T) {
	const experimentID = "rq5"
	previous, existed := adapterFactories[experimentID]
	adapterFactories[experimentID] = func(context.Context) (sourceControlledAdapter, error) { return nil, nil }
	t.Cleanup(func() {
		if existed {
			adapterFactories[experimentID] = previous
		} else {
			delete(adapterFactories, experimentID)
		}
	})

	if !implementedCapabilities()[experimentID] {
		t.Fatal("non-nil source-controlled factory was not reported as implemented")
	}
	if adapter, code := initializeAdapter(context.Background(), experimentID); adapter != nil || code != "adapter_environment_invalid" {
		t.Fatalf("nil adapter initialization = (%T, %q), want fail-closed environment error", adapter, code)
	}
}

func TestFactoryReturningTypedNilAdapterFailsClosed(t *testing.T) {
	const experimentID = "rq5"
	previous, existed := adapterFactories[experimentID]
	adapterFactories[experimentID] = func(context.Context) (sourceControlledAdapter, error) {
		var adapter *nilAdapterFixture
		return adapter, nil
	}
	t.Cleanup(func() {
		if existed {
			adapterFactories[experimentID] = previous
		} else {
			delete(adapterFactories, experimentID)
		}
	})

	if adapter, code := initializeAdapter(context.Background(), experimentID); adapter != nil || code != "adapter_environment_invalid" {
		t.Fatalf("typed nil adapter initialization = (%T, %q), want fail-closed environment error", adapter, code)
	}
}

func TestFactoryReturningNamedNilMapAdapterFailsClosed(t *testing.T) {
	const experimentID = "rq5"
	previous, existed := adapterFactories[experimentID]
	adapterFactories[experimentID] = func(context.Context) (sourceControlledAdapter, error) {
		var adapter nilMapAdapterFixture
		return adapter, nil
	}
	t.Cleanup(func() {
		if existed {
			adapterFactories[experimentID] = previous
		} else {
			delete(adapterFactories, experimentID)
		}
	})

	if adapter, code := initializeAdapter(context.Background(), experimentID); adapter != nil || code != "adapter_environment_invalid" {
		t.Fatalf("named nil map adapter initialization = (%T, %q), want fail-closed environment error", adapter, code)
	}
}

func TestInvalidSamplePreservesSystemArm(t *testing.T) {
	for mode, want := range map[string]string{"direct": "postgresql", "rls": "postgresql", "provsql": "provsql", "novel": "taskgate"} {
		sample := invalidSample(experiment.AdapterOperation{Mode: mode}, "test")
		if sample.System != want {
			t.Fatalf("mode %q invalid system = %q, want %q", mode, sample.System, want)
		}
	}
}
