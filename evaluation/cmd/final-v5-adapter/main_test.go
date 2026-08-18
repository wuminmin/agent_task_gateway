package main

import (
	"context"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
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

func TestCapabilitiesRequireRealFactoriesAndCompletePublicationProfiles(t *testing.T) {
	capabilities := implementedCapabilities()
	if len(capabilities) != len(experimentIDs) {
		t.Fatalf("capability count = %d, want %d", len(capabilities), len(experimentIDs))
	}
	// Artifact and Baseline turned true on 2026-08-16 when their required
	// retained real-system runs completed. Scale turned true after the author
	// approved its two retained 24-cell dependency-e2e runs on 2026-08-19.
	want := map[string]bool{
		"baseline": true,
		"scale":    true, "artifact": true, "rls": true, "attack": true,
		"provsql": true, "compiler": true, "concurrency": true, "rq5": true,
	}
	for _, experimentID := range experimentIDs {
		factory, registered := adapterFactories[experimentID]
		if !registered || factory == nil {
			t.Fatalf("source-controlled factory %q is absent", experimentID)
		}
		if capabilities[experimentID] != want[experimentID] {
			t.Fatalf("capability %q = %v, want %v", experimentID, capabilities[experimentID], want[experimentID])
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

func TestReceiptCatalogMismatchMarksAdapterSampleFailed(t *testing.T) {
	publication, err := experiment.CanonicalPublicationSetSHA256([]string{"publication-a"})
	if err != nil {
		t.Fatal(err)
	}
	binding := &experiment.ProfileBinding{
		Version: experiment.ProfileBindingVersion, ProfileID: "profile-a86cd4df5cad6e26",
		ClosureSHA256: strings.Repeat("a", 64), CatalogSHA256: strings.Repeat("b", 64),
		DatasetBindingSHA256: strings.Repeat("c", 64), PublicationIdentity: publication,
	}
	adapterSampleProfileBinder, err = experiment.NewSampleProfileBinder(binding)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adapterSampleProfileBinder = nil })
	operation := experiment.AdapterOperation{SampleID: "sample-1"}
	sample := experiment.Sample{Status: "pass", BaselineVerification: &experiment.BaselineVerificationEvidence{
		Receipt: queryreceipt.QueryReceiptV1{CatalogDigest: strings.Repeat("d", 64),
			ExecutionBindingV2: &querybinding.QueryExecutionBindingV2{}},
	}}
	got := bindAdapterOutputSample(operation, sample)
	if got.Status != "fail" || got.ErrorCode != "profile_binding_catalog_mismatch" || got.ProfileBinding == nil {
		t.Fatalf("Catalog mismatch sample = %+v", got)
	}
}
