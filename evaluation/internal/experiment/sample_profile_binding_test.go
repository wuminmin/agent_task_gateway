package experiment

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func executedReceipt(catalog string) queryreceipt.QueryReceiptV1 {
	return queryreceipt.QueryReceiptV1{
		CatalogDigest:      catalog,
		ExecutionBindingV2: &querybinding.QueryExecutionBindingV2{},
	}
}

func TestSampleProfileBinderFromAliasEnvironmentBindsNonArtifactSamples(t *testing.T) {
	registryPath := writeResolverRegistry(t, resolverTestRegistry())
	dataset := strings.Repeat("c", 64)
	t.Setenv(profileAliasEnv, resolverTestAlias)
	t.Setenv(profileRegistryEnv, registryPath)
	t.Setenv(datasetBindingSHA256Env, dataset)

	binder, err := ResolveSampleProfileBinderFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	sample := Sample{ExperimentID: "baseline", Status: "invalid"}
	if err := binder.BindSample(&sample); err != nil {
		t.Fatal(err)
	}
	if sample.ProfileBinding == nil || sample.ProfileBinding.CatalogSHA256 != resolverTestCatalog ||
		sample.ProfileBinding.DatasetBindingSHA256 != dataset {
		t.Fatalf("non-Artifact sample binding = %+v", sample.ProfileBinding)
	}
}

func TestSampleProfileBinderWithoutAliasPreservesUnboundSyntheticSmoke(t *testing.T) {
	t.Setenv(profileAliasEnv, "")
	t.Setenv(profileRegistryEnv, "/path/that/must/not/be-read")
	t.Setenv(datasetBindingSHA256Env, "not-a-digest")
	binder, err := ResolveSampleProfileBinderFromEnvironment()
	if err != nil {
		t.Fatalf("alias-free smoke resolved unrelated inputs: %v", err)
	}
	sample := Sample{ExperimentID: "baseline", Status: "pass"}
	if err := binder.BindSample(&sample); err != nil {
		t.Fatal(err)
	}
	if sample.ProfileBinding != nil {
		t.Fatalf("alias-free smoke gained binding %+v", sample.ProfileBinding)
	}
}

func TestSampleProfileBinderRejectsTopLevelReceiptCatalogMismatch(t *testing.T) {
	binding, err := ResolveProfileBinding(writeResolverRegistry(t, resolverTestRegistry()),
		resolverTestAlias, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewSampleProfileBinder(binding)
	if err != nil {
		t.Fatal(err)
	}
	sample := Sample{ExperimentID: "scale", Status: "pass", BaselineVerification: &BaselineVerificationEvidence{
		Receipt: executedReceipt(strings.Repeat("d", 64)),
	}}
	if err := binder.BindSample(&sample); err == nil || !strings.Contains(err.Error(), "top-level Receipt") {
		t.Fatalf("top-level Catalog mismatch error = %v", err)
	}
	if sample.ProfileBinding == nil {
		t.Fatal("mismatched sample did not retain independently resolved binding")
	}
}

func TestSampleProfileBinderChecksEveryNestedRLSAndAttackReceipt(t *testing.T) {
	binding, err := ResolveProfileBinding(writeResolverRegistry(t, resolverTestRegistry()),
		resolverTestAlias, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	binder, err := NewSampleProfileBinder(binding)
	if err != nil {
		t.Fatal(err)
	}
	matching := &BaselineVerificationEvidence{Receipt: executedReceipt(resolverTestCatalog)}
	mismatch := &BaselineVerificationEvidence{Receipt: executedReceipt(strings.Repeat("d", 64))}

	rls := Sample{ExperimentID: "rls", RLSVerification: &RLSVerificationEvidence{Steps: []RLSStepEvidence{
		{Verification: matching}, {Verification: mismatch},
	}}}
	if err := binder.BindSample(&rls); err == nil || !strings.Contains(err.Error(), "RLS step 2 Receipt") {
		t.Fatalf("nested RLS Catalog mismatch error = %v", err)
	}

	attack := Sample{ExperimentID: "attack", AttackVerification: &AttackVerificationEvidence{Steps: []AttackStepEvidence{
		{Verification: matching}, {Verification: mismatch},
	}}}
	if err := binder.BindSample(&attack); err == nil || !strings.Contains(err.Error(), "Attack step 2 Receipt") {
		t.Fatalf("nested Attack Catalog mismatch error = %v", err)
	}
}
