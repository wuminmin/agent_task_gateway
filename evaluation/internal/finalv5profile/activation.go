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

// DrainObservationObserved is the only status that admits a DrainCounts value.
const DrainObservationObserved = "observed"

// DrainCounts are the pre-switch gates. A profile switch must not strand work.
// The pointer fields are load bearing: a missing field must be distinguishable
// from an observed zero, so "cannot observe" can never be serialized as
// "nothing outstanding".
type DrainCounts struct {
	InflightQueries   *int64 `json:"inflight_queries"`
	PendingArtifacts  *int64 `json:"pending_artifacts"`
	OpenReservations  *int64 `json:"open_reservations"`
	ActiveServedRoots *int64 `json:"active_served_roots"`
}

// Complete reports whether every gate was actually reported.
func (counts DrainCounts) Complete() bool {
	return counts.InflightQueries != nil && counts.PendingArtifacts != nil &&
		counts.OpenReservations != nil && counts.ActiveServedRoots != nil
}

// Clean reports whether the previous profile can be stopped safely. An
// incomplete observation is never clean.
func (counts DrainCounts) Clean() bool {
	return counts.Complete() && *counts.InflightQueries == 0 && *counts.PendingArtifacts == 0 &&
		*counts.OpenReservations == 0 && *counts.ActiveServedRoots == 0
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
// A Catalog list check alone is not refusal evidence: it says what the file
// declares, not what the deployment does. Both layers must hold.
type OutsideProductProbe struct {
	Product                string `json:"product"`
	RequestedProductSHA256 string `json:"requested_product_sha256"`
	CatalogListAbsent      bool   `json:"catalog_list_absent"`
	LiveRequestRefused     bool   `json:"live_request_refused"`
	Refused                bool   `json:"refused"`
	Observed               string `json:"observed"`
	Classification         string `json:"classification"`
	ResponseSHA256         string `json:"response_sha256,omitempty"`
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

	DrainBefore            DrainCounts           `json:"drain_before"`
	DrainObservationStatus string                `json:"drain_observation_status"`
	DrainObserverVersion   string                `json:"drain_observer_version"`
	DrainObservationSHA256 string                `json:"drain_observation_sha256"`
	CacheIsolation         CacheIsolation        `json:"cache_isolation"`
	OutsideProduct         []OutsideProductProbe `json:"outside_product_probes"`

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
	// An unobserved drain is a failure, not an empty one.
	if evidence.DrainObservationStatus != DrainObservationObserved {
		return fmt.Errorf("profile %q was activated without an observed drain (status %q)",
			profile.Alias, evidence.DrainObservationStatus)
	}
	if strings.TrimSpace(evidence.DrainObserverVersion) == "" || !validDigest(evidence.DrainObservationSHA256) {
		return fmt.Errorf("profile %q drain observation is not attributable", profile.Alias)
	}
	if !evidence.DrainBefore.Complete() {
		return fmt.Errorf("profile %q drain observation omits a gate", profile.Alias)
	}
	if !evidence.DrainBefore.Clean() {
		return fmt.Errorf("profile %q was activated while work remained: inflight=%d pending=%d reservations=%d roots=%d",
			profile.Alias, *evidence.DrainBefore.InflightQueries, *evidence.DrainBefore.PendingArtifacts,
			*evidence.DrainBefore.OpenReservations, *evidence.DrainBefore.ActiveServedRoots)
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
		if !probe.CatalogListAbsent || !probe.LiveRequestRefused || !probe.Refused {
			return fmt.Errorf("profile %q outside-Product probe for %q is incomplete: catalog_absent=%t live_refused=%t",
				profile.Alias, probe.Product, probe.CatalogListAbsent, probe.LiveRequestRefused)
		}
		if strings.TrimSpace(probe.Classification) == "" {
			return fmt.Errorf("profile %q outside-Product refusal for %q has no stable classification",
				profile.Alias, probe.Product)
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

func validDigest(value string) bool {
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
