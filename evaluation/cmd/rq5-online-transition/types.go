package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
)

const (
	onlineEvidenceSchema = "taskgate-daily-publication-online-evidence-v1"
	routingModel         = "approval_time_version_routed_retained_instances"
	measurementBoundary  = "experiment_only_router_over_four_retained_catalog_bound_gateway_services; excludes offline build_verify_activate and production routing"
)

var (
	days          = [...]string{"day0", "day1", "day2", "day3"}
	sha256Regexp  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	imageIDRegexp = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type onlineEvidence struct {
	SchemaVersion       string               `json:"schema_version"`
	RoutingModel        string               `json:"routing_model"`
	RowsPerPublication  int64                `json:"rows_per_publication"`
	MeasurementBoundary string               `json:"measurement_boundary"`
	Fixture             fixtureEvidence      `json:"fixture"`
	Transitions         []transitionEvidence `json:"transitions"`
}

type fixtureEvidence struct {
	FixtureClass          string                       `json:"fixture_class"`
	RowsPerPublication    int64                        `json:"rows_per_publication"`
	GeneratorSHA256       string                       `json:"generator_sha256"`
	ConfigSHA256          string                       `json:"config_sha256"`
	DatasetManifestSHA256 string                       `json:"dataset_manifest_sha256"`
	Publications          []publicationFixtureEvidence `json:"publications"`
}

type publicationFixtureEvidence struct {
	Day                       string `json:"day"`
	PublicationName           string `json:"publication_name"`
	RowCount                  uint64 `json:"row_count"`
	ApprovedInputSHA256       string `json:"approved_input_sha256"`
	CatalogSHA256             string `json:"catalog_sha256"`
	BundleManifestSHA256      string `json:"bundle_manifest_sha256"`
	PublicationManifestDigest string `json:"publication_manifest_digest"`
	DictionaryDigest          string `json:"dictionary_digest"`
	SidecarDigest             string `json:"sidecar_digest"`
	SchemaDigest              string `json:"schema_digest"`
	HotArtifactSHA256         string `json:"hot_artifact_sha256"`
	ColdArtifactSHA256        string `json:"cold_artifact_sha256"`
	SidecarArtifactSHA256     string `json:"sidecar_artifact_sha256"`
	DirectResultSHA256        string `json:"direct_result_sha256"`
}

type transitionEvidence struct {
	From             string             `json:"from"`
	To               string             `json:"to"`
	SwitchWallMS     float64            `json:"switch_wall_ms"`
	FirstQueryWallMS float64            `json:"first_query_wall_ms"`
	ReplayWallMS     float64            `json:"replay_wall_ms"`
	OldTask          oldTaskEvidence    `json:"old_task"`
	NewTask          newTaskEvidence    `json:"new_task"`
	OldLedger        oldLedgerEvidence  `json:"old_ledger"`
	Cache            cacheEvidence      `json:"cache"`
	Delegation       delegationEvidence `json:"delegation"`
}

type oldTaskEvidence struct {
	PublicationDigestBefore   string `json:"publication_digest_before"`
	PublicationDigestAfter    string `json:"publication_digest_after"`
	ExpectedPublicationDigest string `json:"expected_publication_digest"`
	ResultSHA256Before        string `json:"result_sha256_before"`
	ResultSHA256After         string `json:"result_sha256_after"`
	ExpectedResultSHA256      string `json:"expected_result_sha256"`
}

type newTaskEvidence struct {
	PublicationDigest         string `json:"publication_digest"`
	ExpectedPublicationDigest string `json:"expected_publication_digest"`
	ResultSHA256              string `json:"result_sha256"`
	ExpectedResultSHA256      string `json:"expected_result_sha256"`
}

type oldLedgerEvidence struct {
	BeforeSwitchSHA256 string `json:"before_switch_sha256"`
	AfterSwitchSHA256  string `json:"after_switch_sha256"`
}

type cacheEvidence struct {
	OldCacheKeySHA256       string `json:"old_cache_key_sha256"`
	FirstNewCacheKeySHA256  string `json:"first_new_cache_key_sha256"`
	FirstNewSemanticReplay  bool   `json:"first_new_semantic_replay"`
	ReplayNewCacheKeySHA256 string `json:"replay_new_cache_key_sha256"`
	ReplayNewSemanticReplay bool   `json:"replay_new_semantic_replay"`
}

type delegationEvidence struct {
	RootTaskID             string `json:"root_task_id"`
	ChildRootTaskID        string `json:"child_root_task_id"`
	ChildParentTaskID      string `json:"child_parent_task_id"`
	RootPublicationDigest  string `json:"root_publication_digest"`
	ChildPublicationDigest string `json:"child_publication_digest"`
}

func (e onlineEvidence) validate() error {
	if e.SchemaVersion != onlineEvidenceSchema {
		return fmt.Errorf("schema_version must be %q", onlineEvidenceSchema)
	}
	if e.RoutingModel != routingModel {
		return fmt.Errorf("routing_model must be %q", routingModel)
	}
	if e.RowsPerPublication <= 0 {
		return errors.New("rows_per_publication must be positive")
	}
	if e.MeasurementBoundary != measurementBoundary {
		return errors.New("measurement_boundary does not identify the experiment-only router")
	}
	if err := e.Fixture.validate(e.RowsPerPublication); err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	if len(e.Transitions) != len(days)-1 {
		return fmt.Errorf("transitions = %d, want %d", len(e.Transitions), len(days)-1)
	}
	for index, value := range e.Transitions {
		if err := value.validate(days[index], days[index+1]); err != nil {
			return fmt.Errorf("transition %d: %w", index, err)
		}
		oldPublication := e.Fixture.Publications[index]
		newPublication := e.Fixture.Publications[index+1]
		if value.OldTask.ExpectedPublicationDigest != oldPublication.PublicationManifestDigest ||
			value.OldTask.ExpectedResultSHA256 != oldPublication.DirectResultSHA256 ||
			value.NewTask.ExpectedPublicationDigest != newPublication.PublicationManifestDigest ||
			value.NewTask.ExpectedResultSHA256 != newPublication.DirectResultSHA256 ||
			value.Delegation.RootPublicationDigest != newPublication.PublicationManifestDigest {
			return fmt.Errorf("transition %d is not bound to its fixture publications", index)
		}
	}
	return nil
}

func (e fixtureEvidence) validate(rows int64) error {
	if e.FixtureClass != "correctness_fixture" || e.RowsPerPublication != rows {
		return errors.New("fixture class or row scale is invalid")
	}
	for name, value := range map[string]string{
		"generator_sha256":        e.GeneratorSHA256,
		"config_sha256":           e.ConfigSHA256,
		"dataset_manifest_sha256": e.DatasetManifestSHA256,
	} {
		if !sha256Regexp.MatchString(value) {
			return fmt.Errorf("%s must be lowercase SHA-256", name)
		}
	}
	if len(e.Publications) != len(days) {
		return errors.New("fixture must bind all four publications")
	}
	uniqueCatalogs := make(map[string]struct{}, len(days))
	uniqueManifests := make(map[string]struct{}, len(days))
	uniqueResults := make(map[string]struct{}, len(days))
	for index, publication := range e.Publications {
		if publication.Day != days[index] ||
			publication.PublicationName != fmt.Sprintf("daily-lineitem-%s-r%d", days[index], rows) ||
			publication.RowCount != uint64(rows) {
			return fmt.Errorf("publication %d identity or row count is invalid", index)
		}
		for name, value := range map[string]string{
			"approved_input_sha256":       publication.ApprovedInputSHA256,
			"catalog_sha256":              publication.CatalogSHA256,
			"bundle_manifest_sha256":      publication.BundleManifestSHA256,
			"publication_manifest_digest": publication.PublicationManifestDigest,
			"dictionary_digest":           publication.DictionaryDigest,
			"sidecar_digest":              publication.SidecarDigest,
			"schema_digest":               publication.SchemaDigest,
			"hot_artifact_sha256":         publication.HotArtifactSHA256,
			"cold_artifact_sha256":        publication.ColdArtifactSHA256,
			"sidecar_artifact_sha256":     publication.SidecarArtifactSHA256,
			"direct_result_sha256":        publication.DirectResultSHA256,
		} {
			if !sha256Regexp.MatchString(value) {
				return fmt.Errorf("publication %s %s must be lowercase SHA-256", publication.Day, name)
			}
		}
		uniqueCatalogs[publication.CatalogSHA256] = struct{}{}
		uniqueManifests[publication.PublicationManifestDigest] = struct{}{}
		uniqueResults[publication.DirectResultSHA256] = struct{}{}
	}
	if len(uniqueCatalogs) != len(days) || len(uniqueManifests) != len(days) || len(uniqueResults) != len(days) {
		return errors.New("fixture Catalog, publication, and direct-result digests must distinguish all four days")
	}
	return nil
}

func (e transitionEvidence) validate(from, to string) error {
	if e.From != from || e.To != to {
		return fmt.Errorf("route = %s->%s, want %s->%s", e.From, e.To, from, to)
	}
	for name, value := range map[string]float64{
		"switch_wall_ms": e.SwitchWallMS, "first_query_wall_ms": e.FirstQueryWallMS,
		"replay_wall_ms": e.ReplayWallMS,
	} {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s must be positive and finite", name)
		}
	}
	digests := map[string]string{
		"old_task.publication_digest_before":   e.OldTask.PublicationDigestBefore,
		"old_task.publication_digest_after":    e.OldTask.PublicationDigestAfter,
		"old_task.expected_publication_digest": e.OldTask.ExpectedPublicationDigest,
		"old_task.result_sha256_before":        e.OldTask.ResultSHA256Before,
		"old_task.result_sha256_after":         e.OldTask.ResultSHA256After,
		"old_task.expected_result_sha256":      e.OldTask.ExpectedResultSHA256,
		"new_task.publication_digest":          e.NewTask.PublicationDigest,
		"new_task.expected_publication_digest": e.NewTask.ExpectedPublicationDigest,
		"new_task.result_sha256":               e.NewTask.ResultSHA256,
		"new_task.expected_result_sha256":      e.NewTask.ExpectedResultSHA256,
		"old_ledger.before_switch_sha256":      e.OldLedger.BeforeSwitchSHA256,
		"old_ledger.after_switch_sha256":       e.OldLedger.AfterSwitchSHA256,
		"cache.old_cache_key_sha256":           e.Cache.OldCacheKeySHA256,
		"cache.first_new_cache_key_sha256":     e.Cache.FirstNewCacheKeySHA256,
		"cache.replay_new_cache_key_sha256":    e.Cache.ReplayNewCacheKeySHA256,
		"delegation.root_publication_digest":   e.Delegation.RootPublicationDigest,
		"delegation.child_publication_digest":  e.Delegation.ChildPublicationDigest,
	}
	for name, value := range digests {
		if !sha256Regexp.MatchString(value) {
			return fmt.Errorf("%s must be lowercase SHA-256", name)
		}
	}
	if e.OldTask.PublicationDigestBefore != e.OldTask.PublicationDigestAfter ||
		e.OldTask.PublicationDigestBefore != e.OldTask.ExpectedPublicationDigest ||
		e.OldTask.ResultSHA256Before != e.OldTask.ResultSHA256After ||
		e.OldTask.ResultSHA256Before != e.OldTask.ExpectedResultSHA256 {
		return errors.New("old task changed or differs from its direct frozen-publication oracle")
	}
	if e.NewTask.PublicationDigest != e.NewTask.ExpectedPublicationDigest ||
		e.NewTask.ResultSHA256 != e.NewTask.ExpectedResultSHA256 {
		return errors.New("new task differs from its direct frozen-publication oracle")
	}
	if e.NewTask.ResultSHA256 == e.OldTask.ResultSHA256Before {
		return errors.New("new and old publications returned the same result")
	}
	if e.OldLedger.BeforeSwitchSHA256 != e.OldLedger.AfterSwitchSHA256 {
		return errors.New("old root ledger changed across the switch/replay check")
	}
	if e.Cache.FirstNewSemanticReplay || !e.Cache.ReplayNewSemanticReplay {
		return errors.New("new publication must miss first and replay second")
	}
	if e.Cache.OldCacheKeySHA256 == e.Cache.FirstNewCacheKeySHA256 ||
		e.Cache.FirstNewCacheKeySHA256 != e.Cache.ReplayNewCacheKeySHA256 {
		return errors.New("cache key did not partition and then replay as required")
	}
	if e.Delegation.RootTaskID == "" || e.Delegation.ChildParentTaskID == "" ||
		e.Delegation.ChildRootTaskID != e.Delegation.RootTaskID ||
		e.Delegation.RootPublicationDigest != e.Delegation.ChildPublicationDigest ||
		e.Delegation.RootPublicationDigest != e.NewTask.ExpectedPublicationDigest {
		return errors.New("delegated child is not bound to its root publication")
	}
	return nil
}
