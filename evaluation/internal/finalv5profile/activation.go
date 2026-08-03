package finalv5profile

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ActivationEvidenceVersion identifies the activation evidence record.
const ActivationEvidenceVersion = "taskgate-final-v5-profile-activation-evidence-v1"

// ObservedArtifact is one HOT artifact a running Gateway reports holding.
type ObservedArtifact struct {
	Identity string `json:"identity"`
	Digest   string `json:"digest"`
	Bytes    int64  `json:"bytes"`
}

// DrainCounts are the pre-switch gates. A profile switch must not strand work.
type DrainCounts struct {
	InflightQueries   int64 `json:"inflight_queries"`
	PendingArtifacts  int64 `json:"pending_artifacts"`
	OpenReservations  int64 `json:"open_reservations"`
	ActiveServedRoots int64 `json:"active_served_roots"`
}

// Clean reports whether the previous profile can be stopped safely.
func (counts DrainCounts) Clean() bool {
	return counts.InflightQueries == 0 && counts.PendingArtifacts == 0 &&
		counts.OpenReservations == 0 && counts.ActiveServedRoots == 0
}

// CacheIsolation records the two isolation layers separately.
type CacheIsolation struct {
	ProcessRestarted            bool   `json:"process_restarted"`
	PreviousProcessNonce        string `json:"previous_process_nonce,omitempty"`
	ProcessNonce                string `json:"process_instance_nonce"`
	PreviousCacheNamespace      string `json:"previous_cache_namespace,omitempty"`
	CacheNamespace              string `json:"cache_namespace"`
	PreviousCacheUnreachable    bool   `json:"previous_in_process_cache_unreachable"`
	SemanticCacheCatalogBound   bool   `json:"semantic_cache_catalog_bound"`
	PreviousHotArtifactsRetired bool   `json:"previous_hot_artifacts_retired"`
}

// OutsideProductProbe is the negative check that a closure is really closed.
type OutsideProductProbe struct {
	Product  string `json:"product"`
	Refused  bool   `json:"refused"`
	Observed string `json:"observed"`
}

// ActivationEvidence is the non-sensitive record of one profile activation. It
// deliberately carries no token, DSN, password, secret, SQL, task identity,
// object key, Parquet content or business row.
type ActivationEvidence struct {
	SchemaVersion       int    `json:"schema_version"`
	Record              string `json:"record"`
	ContractRelease     string `json:"contract_release"`
	CampaignClass       string `json:"campaign_class"`
	PublicationEligible bool   `json:"publication_eligible"`
	DeploymentID        string `json:"deployment_id"`
	ActivationSequence  int    `json:"activation_sequence"`
	PreviousProfileID   string `json:"previous_profile_id,omitempty"`
	ProfileID           string `json:"profile_id"`
	ProfileAlias        string `json:"profile_alias"`
	ClosureSHA256       string `json:"closure_sha256"`
	CatalogSHA256       string `json:"catalog_sha256"`
	CatalogFileSHA256   string `json:"catalog_file_sha256"`
	DatasetBindingSHA   string `json:"dataset_binding_sha256"`
	PublicationSetSHA   string `json:"publication_set_sha256"`

	GatewayImageID     string  `json:"gateway_image_id"`
	GatewayContainerID string  `json:"gateway_container_id"`
	ProcessNonce       string  `json:"process_instance_nonce"`
	ProcessStartedUnix int64   `json:"process_started_unix"`
	ActivationStarted  string  `json:"activation_started_at"`
	ActivationEnded    string  `json:"activation_ended_at"`
	ReadinessAt        string  `json:"readiness_completed_at"`
	ActivationMS       float64 `json:"activation_duration_ms"`

	ExpectedProducts     []string           `json:"expected_products"`
	ObservedProducts     []string           `json:"observed_products"`
	ExpectedPublications []string           `json:"expected_publications"`
	ObservedPublications []string           `json:"observed_publications"`
	ExpectedHotArtifacts []ObservedArtifact `json:"expected_hot_artifacts"`
	ObservedHotArtifacts []ObservedArtifact `json:"observed_hot_artifacts"`
	ExpectedHotBytes     int64              `json:"expected_hot_bytes"`
	ActualHotBytes       int64              `json:"actual_hot_bytes"`
	HotLimitBytes        int64              `json:"hot_limit_bytes"`

	DrainBefore    DrainCounts           `json:"drain_before"`
	CacheIsolation CacheIsolation        `json:"cache_isolation"`
	OutsideProduct []OutsideProductProbe `json:"outside_product_probes"`

	// ActivationSmokePassed and WorkloadTargetedValidationPassed are separate
	// on purpose: activating a Catalog is not the same as executing the
	// profile's workload cells, and an activation smoke never implies the other.
	ActivationSmokePassed            bool `json:"activation_smoke_passed"`
	WorkloadTargetedValidationPassed bool `json:"workload_targeted_validation_passed"`

	Status   string   `json:"status"`
	Failures []string `json:"failures"`
}

// ValidateActivationEvidence is the fail-closed acceptance rule for one record.
func ValidateActivationEvidence(evidence ActivationEvidence, profile Profile) error {
	if evidence.SchemaVersion != 1 || evidence.Record != ActivationEvidenceVersion {
		return errors.New("activation evidence header is invalid")
	}
	if evidence.PublicationEligible {
		return errors.New("a profile activation record is never publication evidence")
	}
	if evidence.WorkloadTargetedValidationPassed {
		return errors.New("an activation record must not claim workload targeted validation")
	}
	if evidence.ProfileID != profile.ID || evidence.ClosureSHA256 != profile.Closure.SHA256 ||
		evidence.CatalogSHA256 != profile.CatalogSHA256 {
		return errors.New("activation evidence identifies a different profile")
	}
	if !equalStringSets(evidence.ExpectedProducts, evidence.ObservedProducts) {
		return fmt.Errorf("profile %q activated Products %v, expected %v",
			profile.Alias, evidence.ObservedProducts, evidence.ExpectedProducts)
	}
	if !equalStringSets(evidence.ExpectedPublications, evidence.ObservedPublications) {
		return fmt.Errorf("profile %q activated Publications %v, expected %v",
			profile.Alias, evidence.ObservedPublications, evidence.ExpectedPublications)
	}
	if !equalArtifacts(evidence.ExpectedHotArtifacts, evidence.ObservedHotArtifacts) {
		return fmt.Errorf("profile %q activated a different HOT artifact set", profile.Alias)
	}
	if evidence.ActualHotBytes != evidence.ExpectedHotBytes {
		return fmt.Errorf("profile %q activated %d HOT bytes, the registry records %d",
			profile.Alias, evidence.ActualHotBytes, evidence.ExpectedHotBytes)
	}
	if evidence.HotLimitBytes != MaxHotBytesPerInstance || evidence.ActualHotBytes > MaxHotBytesPerInstance {
		return fmt.Errorf("profile %q activated %d HOT bytes against the %d byte %s",
			profile.Alias, evidence.ActualHotBytes, MaxHotBytesPerInstance, HotLimitScope)
	}
	if !evidence.DrainBefore.Clean() {
		return fmt.Errorf("profile %q was activated while %+v remained", profile.Alias, evidence.DrainBefore)
	}
	if !evidence.CacheIsolation.ProcessRestarted || !evidence.CacheIsolation.PreviousCacheUnreachable ||
		!evidence.CacheIsolation.SemanticCacheCatalogBound {
		return fmt.Errorf("profile %q did not prove cache isolation", profile.Alias)
	}
	if evidence.PreviousProfileID != "" {
		if evidence.CacheIsolation.PreviousProcessNonce == evidence.CacheIsolation.ProcessNonce {
			return fmt.Errorf("profile %q reused the previous Gateway process", profile.Alias)
		}
		if evidence.CacheIsolation.PreviousCacheNamespace == evidence.CacheIsolation.CacheNamespace {
			return fmt.Errorf("profile %q reused the previous cache namespace", profile.Alias)
		}
	}
	if len(evidence.OutsideProduct) == 0 {
		return fmt.Errorf("profile %q ran no outside-Product negative probe", profile.Alias)
	}
	for _, probe := range evidence.OutsideProduct {
		if !probe.Refused {
			return fmt.Errorf("profile %q served outside-closure Product %q", profile.Alias, probe.Product)
		}
		if contains(profile.Closure.Products, probe.Product) {
			return fmt.Errorf("profile %q probed %q, which is inside its own closure", profile.Alias, probe.Product)
		}
	}
	if evidence.Status != "pass" || len(evidence.Failures) != 0 {
		return fmt.Errorf("profile %q activation status %q: %s",
			profile.Alias, evidence.Status, strings.Join(evidence.Failures, "; "))
	}
	return nil
}

// ExpectedArtifacts converts registry HOT artifacts into the evidence shape.
func ExpectedArtifacts(profile Profile) []ObservedArtifact {
	artifacts := make([]ObservedArtifact, 0, len(profile.HotArtifacts))
	for _, artifact := range profile.HotArtifacts {
		artifacts = append(artifacts, ObservedArtifact{Identity: artifact.Publication,
			Digest: artifact.SHA256, Bytes: artifact.Bytes})
	}
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].Identity < artifacts[right].Identity })
	return artifacts
}

func equalStringSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	first := append([]string(nil), left...)
	second := append([]string(nil), right...)
	sort.Strings(first)
	sort.Strings(second)
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func equalArtifacts(left, right []ObservedArtifact) bool {
	if len(left) != len(right) {
		return false
	}
	first := append([]ObservedArtifact(nil), left...)
	second := append([]ObservedArtifact(nil), right...)
	sort.Slice(first, func(a, b int) bool { return first[a].Identity < first[b].Identity })
	sort.Slice(second, func(a, b int) bool { return second[a].Identity < second[b].Identity })
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}
