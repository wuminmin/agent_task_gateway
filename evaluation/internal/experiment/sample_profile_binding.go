package experiment

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const (
	profileAliasEnv         = "TASKGATE_FINAL_V5_PROFILE_ALIAS"
	profileRegistryEnv      = "TASKGATE_FINAL_V5_PROFILE_REGISTRY"
	datasetBindingSHA256Env = "TASKGATE_FINAL_V5_DATASET_BINDING_SHA256"
	rq5CatalogFamilyEnv     = "TASKGATE_FINAL_V5_RQ5_CATALOG_FAMILY"
	rq5BuildManifestEnv     = "TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST"
	rq5BuildManifestSHAEnv  = "TASKGATE_FINAL_V5_RQ5_BUILD_MANIFEST_SHA256"
	rq5GeneratorSHAEnv      = "TASKGATE_FINAL_V5_RQ5_GENERATOR_SHA256"
	rq5ConfigSHAEnv         = "TASKGATE_FINAL_V5_RQ5_CONFIG_SHA256"
	submissionCommitEnv     = "TASKGATE_SUBMISSION_COMMIT"
)

// SampleProfileBinder is the Adapter's process-scoped, independently resolved
// deployment identity. The launcher supplies only a source-controlled profile
// alias; resolution still passes through the registry clearance gate and the
// exact Dataset Binding file digest.
//
// An inactive binder preserves synthetic-smoke behavior: when no alias was
// supplied, samples remain unbound and no registry inputs are required.
type SampleProfileBinder struct {
	binding *ProfileBinding
	rq5     *finalv5RQ5CatalogExpectation
}

type finalv5RQ5CatalogExpectation struct {
	FamilySHA256        string
	BuildManifestSHA256 string
	GeneratorSHA256     string
	ConfigSHA256        string
}

// ResolveSampleProfileBinderFromEnvironment resolves the deployment profile
// once at Adapter process startup. Callers retain the returned binder and must
// not resolve again per operation.
func ResolveSampleProfileBinderFromEnvironment() (*SampleProfileBinder, error) {
	alias := strings.TrimSpace(os.Getenv(profileAliasEnv))
	if alias == "" {
		return &SampleProfileBinder{}, nil
	}
	binding, err := ResolveProfileBinding(
		strings.TrimSpace(os.Getenv(profileRegistryEnv)),
		alias,
		strings.TrimSpace(os.Getenv(datasetBindingSHA256Env)),
	)
	if err != nil {
		return nil, fmt.Errorf("resolve sample profile binding for %q: %w", alias, err)
	}
	return NewSampleProfileBinder(binding)
}

// ResolveRQ5SampleProfileBinderFromEnvironment independently rebuilds the RQ5
// Catalog-family binding. It never reads the operation binding emitted by the
// Runner, so operation==sample remains an independent equality check.
func ResolveRQ5SampleProfileBinderFromEnvironment() (*SampleProfileBinder, error) {
	alias := strings.TrimSpace(os.Getenv(profileAliasEnv))
	if alias == "" {
		return &SampleProfileBinder{}, nil
	}
	resolved, err := ResolveRQ5ProfileBinding(RQ5ProfileBindingRequest{
		RegistryPath: strings.TrimSpace(os.Getenv(profileRegistryEnv)), ProfileAlias: alias,
		DatasetBindingSHA256: strings.TrimSpace(os.Getenv(datasetBindingSHA256Env)),
		CatalogFamilyPath:    strings.TrimSpace(os.Getenv(rq5CatalogFamilyEnv)),
		BuildManifestPath:    strings.TrimSpace(os.Getenv(rq5BuildManifestEnv)),
		BuildManifestSHA256:  strings.TrimSpace(os.Getenv(rq5BuildManifestSHAEnv)),
		SubmissionCommit:     strings.TrimSpace(os.Getenv(submissionCommitEnv)),
		GeneratorSHA256:      strings.TrimSpace(os.Getenv(rq5GeneratorSHAEnv)),
		ConfigSHA256:         strings.TrimSpace(os.Getenv(rq5ConfigSHAEnv)),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve RQ5 sample profile binding for %q: %w", alias, err)
	}
	binder, err := NewSampleProfileBinder(resolved.Binding)
	if err != nil {
		return nil, err
	}
	binder.rq5 = &finalv5RQ5CatalogExpectation{FamilySHA256: resolved.Family.FamilySHA256,
		BuildManifestSHA256: resolved.Family.BuildManifestSHA256,
		GeneratorSHA256:     resolved.Family.GeneratorSHA256, ConfigSHA256: resolved.Family.ConfigSHA256}
	return binder, nil
}

// NewSampleProfileBinder constructs a binder around an already independently
// resolved profile. It is also the narrow seam used by focused tests.
func NewSampleProfileBinder(binding *ProfileBinding) (*SampleProfileBinder, error) {
	if binding == nil {
		return &SampleProfileBinder{}, nil
	}
	if err := binding.Validate(); err != nil {
		return nil, fmt.Errorf("sample profile binding: %w", err)
	}
	copy := *binding
	return &SampleProfileBinder{binding: &copy}, nil
}

// Active reports whether the launcher supplied a deployment profile.
func (binder *SampleProfileBinder) Active() bool {
	return binder != nil && binder.binding != nil
}

// Binding returns a per-sample copy so one caller cannot mutate the cached
// process identity used for later samples.
func (binder *SampleProfileBinder) Binding() *ProfileBinding {
	if !binder.Active() {
		return nil
	}
	copy := *binder.binding
	return &copy
}

// RequireReceiptCatalog compares a successfully executed Gateway Receipt with
// the independently resolved profile Catalog. Non-execution receipts carry no
// execution Catalog and are outside this observation gate.
func (binder *SampleProfileBinder) RequireReceiptCatalog(receipt queryreceipt.QueryReceiptV1) error {
	if !binder.Active() || !receipt.DescribesAnExecution() {
		return nil
	}
	if receipt.CatalogDigest != binder.binding.CatalogSHA256 {
		return fmt.Errorf("Gateway Receipt Catalog digest %q differs from deployment profile %q",
			receipt.CatalogDigest, binder.binding.CatalogSHA256)
	}
	return nil
}

// BindSample attaches the independently resolved identity to every sample,
// including invalid and retained-failure samples, then cross-checks every
// successful Receipt that survives in the sample. RLS and Attack keep their
// authoritative Receipts on nested steps, so all of them are checked.
func (binder *SampleProfileBinder) BindSample(sample *Sample) error {
	if sample == nil {
		return errors.New("cannot bind a nil sample")
	}
	if !binder.Active() {
		return nil
	}
	sample.ProfileBinding = binder.Binding()
	if sample.BaselineVerification != nil {
		if err := binder.RequireReceiptCatalog(sample.BaselineVerification.Receipt); err != nil {
			return fmt.Errorf("top-level Receipt: %w", err)
		}
	}
	if sample.RLSVerification != nil {
		for index := range sample.RLSVerification.Steps {
			verification := sample.RLSVerification.Steps[index].Verification
			if verification == nil {
				continue
			}
			if err := binder.RequireReceiptCatalog(verification.Receipt); err != nil {
				return fmt.Errorf("RLS step %d Receipt: %w", index+1, err)
			}
		}
	}
	if sample.AttackVerification != nil {
		for index := range sample.AttackVerification.Steps {
			verification := sample.AttackVerification.Steps[index].Verification
			if verification == nil {
				continue
			}
			if err := binder.RequireReceiptCatalog(verification.Receipt); err != nil {
				return fmt.Errorf("Attack step %d Receipt: %w", index+1, err)
			}
		}
	}
	if sample.RQ5Verification != nil && sample.Status == "pass" {
		if binder.rq5 == nil || binder.binding.CatalogSHA256 != binder.rq5.FamilySHA256 ||
			sample.RQ5Verification.BuildManifestSHA256 != binder.rq5.BuildManifestSHA256 ||
			sample.RQ5Verification.GeneratorSHA256 != binder.rq5.GeneratorSHA256 ||
			sample.RQ5Verification.ConfigSHA256 != binder.rq5.ConfigSHA256 {
			return errors.New("RQ5 evidence differs from the independently resolved Catalog family source identity")
		}
	}
	return nil
}
