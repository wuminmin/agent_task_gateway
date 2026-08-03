package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"taskbound.local/agent-data-gateway/internal/catalog"
)

// ProfileDiagnosticVersion identifies the read-only activation diagnostic.
const ProfileDiagnosticVersion = "taskgate-final-v5-profile-diagnostic-v1"

// ProfileDiagnosticPath is served only on the authenticated admin interface.
const ProfileDiagnosticPath = "/admin/v1/evaluation/profile-activation"

// DrainObserverVersion identifies the pre-switch drain observation.
const DrainObserverVersion = "taskgate-final-v5-drain-observer-v1"

// drainCounts is read from Control at request time. A profile switch that
// stranded an in-flight query, an unresolved PENDING artifact, an unreleased
// reservation or a still-served ACTIVE root would lose work, so the orchestrator
// must be able to observe all four rather than assume them.
type drainCounts struct {
	InflightQueries   int64 `json:"inflight_queries"`
	PendingArtifacts  int64 `json:"pending_artifacts"`
	OpenReservations  int64 `json:"open_reservations"`
	ActiveServedRoots int64 `json:"active_served_roots"`
}

// drainObservation is deliberately explicit about whether it observed anything.
// An unavailable Control store yields status "unavailable" and never zeros:
// serializing "cannot observe" as "nothing outstanding" is exactly the failure
// this record exists to prevent.
type drainObservation struct {
	Version string       `json:"observer_version"`
	Status  string       `json:"status"`
	Counts  *drainCounts `json:"counts,omitempty"`
	SHA256  string       `json:"observation_sha256"`
	Error   string       `json:"error,omitempty"`
}

func observeDrain(ctx context.Context, store drainQuerier) drainObservation {
	observation := drainObservation{Version: DrainObserverVersion, Status: "unavailable"}
	if store == nil {
		observation.Error = "control store is not configured"
		return observation
	}
	var counts drainCounts
	const query = `SELECT
  (SELECT count(*) FROM query_records WHERE status = 'RESERVED')::bigint,
  (SELECT count(*) FROM result_artifacts WHERE status = 'PENDING')::bigint,
  (SELECT count(*) FROM query_exposure_reservations WHERE status = 'RESERVED')::bigint,
  (SELECT count(*) FROM tasks WHERE state = 'ACTIVE')::bigint`
	row := store.QueryRowContext(ctx, query)
	if err := row.Scan(&counts.InflightQueries, &counts.PendingArtifacts,
		&counts.OpenReservations, &counts.ActiveServedRoots); err != nil {
		observation.Error = "control drain query failed"
		return observation
	}
	observation.Status = "observed"
	observation.Counts = &counts
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\x00%d\x00%d\x00%d\x00%d\x00", DrainObserverVersion,
		counts.InflightQueries, counts.PendingArtifacts, counts.OpenReservations, counts.ActiveServedRoots)
	observation.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return observation
}

// drainQuerier is the minimal Control surface the diagnostic needs.
type drainQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// activatedHotArtifact is one HOT artifact this process actually parsed.
type activatedHotArtifact struct {
	Publication    string `json:"publication"`
	ManifestDigest string `json:"manifest_digest"`
	HotIndexDigest string `json:"hot_index_digest"`
	Bytes          int64  `json:"bytes"`
}

// activationState is captured from the snapshot loader while it verifies each
// publication, so the diagnostic reports what this process actually holds
// rather than echoing a Catalog or a Profile Registry back to its caller.
type activationState struct {
	CatalogSHA256      string                 `json:"catalog_sha256"`
	CatalogVersion     string                 `json:"catalog_version"`
	Products           []string               `json:"products"`
	Publications       []string               `json:"publications"`
	HotArtifacts       []activatedHotArtifact `json:"hot_artifacts"`
	ActualHotBytes     int64                  `json:"actual_hot_bytes"`
	HotLimitBytes      int64                  `json:"configured_hot_limit_bytes"`
	LoaderGeneration   int64                  `json:"snapshot_loader_generation"`
	CacheNamespace     string                 `json:"cache_namespace_digest"`
	ProcessNonce       string                 `json:"process_instance_nonce"`
	ProcessStartedUnix int64                  `json:"process_started_unix"`
}

// processInstanceNonce is fresh per process. A profile switch that reuses a
// process would keep this value, which is exactly what the isolation check
// must be able to detect.
var processInstanceNonce, processStartedAt = newProcessIdentity()

func newProcessIdentity() (string, time.Time) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		// A process without a usable nonce must not be able to claim isolation.
		return "", time.Now().UTC()
	}
	return hex.EncodeToString(buffer), time.Now().UTC()
}

// newActivationState builds the diagnostic view from verified loader output.
func newActivationState(logicalCatalog *catalog.Catalog, artifacts []activatedHotArtifact,
	totalHotBytes, hotLimitBytes int64) *activationState {
	state := &activationState{CatalogSHA256: logicalCatalog.SHA256, CatalogVersion: logicalCatalog.CatalogVersion,
		HotArtifacts: append([]activatedHotArtifact(nil), artifacts...), ActualHotBytes: totalHotBytes,
		HotLimitBytes: hotLimitBytes, LoaderGeneration: 1,
		ProcessNonce: processInstanceNonce, ProcessStartedUnix: processStartedAt.Unix()}
	for _, product := range logicalCatalog.Products {
		state.Products = append(state.Products, product.Name)
	}
	for _, publication := range logicalCatalog.SnapshotPublications {
		state.Publications = append(state.Publications, publication.Name)
	}
	sort.Strings(state.Products)
	sort.Strings(state.Publications)
	sort.Slice(state.HotArtifacts, func(left, right int) bool {
		return state.HotArtifacts[left].Publication < state.HotArtifacts[right].Publication
	})
	state.CacheNamespace = cacheNamespaceDigest(state)
	return state
}

// cacheNamespaceDigest binds every in-process cache to this exact activation.
// A different Catalog, publication set or process nonce yields a different
// namespace, so a later profile can never address an earlier profile's entries.
func cacheNamespaceDigest(state *activationState) string {
	hash := sha256.New()
	hash.Write([]byte(ProfileDiagnosticVersion + "\x00"))
	fmt.Fprintf(hash, "%d\x00%s\x00", len(state.CatalogSHA256), state.CatalogSHA256)
	fmt.Fprintf(hash, "%d\x00", len(state.HotArtifacts))
	for _, artifact := range state.HotArtifacts {
		fmt.Fprintf(hash, "%d\x00%s\x00%d\x00%s\x00", len(artifact.Publication), artifact.Publication,
			len(artifact.ManifestDigest), artifact.ManifestDigest)
	}
	fmt.Fprintf(hash, "%d\x00%s\x00", len(state.ProcessNonce), state.ProcessNonce)
	return hex.EncodeToString(hash.Sum(nil))
}

// profileDiagnosticEnabled keeps the endpoint closed by default. It opens only
// for a declared evaluation class and only with an admin token, and it is
// mounted on the same authenticated admin group as retention administration.
func profileDiagnosticEnabled() bool {
	switch strings.TrimSpace(os.Getenv("TASKGATE_EXPERIMENT_CLASS")) {
	case "pilot", "publication":
		return strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_TOKEN")) != ""
	default:
		return false
	}
}

// mountProfileDiagnostic exposes the read-only activation view. The response
// carries no credential, DSN, SQL, task identity, object key or data row: it is
// deployment identity and byte accounting only.
func mountProfileDiagnostic(router chi.Router, state *activationState, adminToken string,
	ready func() error, drains drainQuerier) {
	if state == nil || !profileDiagnosticEnabled() {
		return
	}
	router.Group(func(r chi.Router) {
		r.Use(adminTokenAuth(adminToken))
		r.Get(ProfileDiagnosticPath, func(w http.ResponseWriter, r *http.Request) {
			readiness := "ready"
			if ready != nil {
				if err := ready(); err != nil {
					readiness = "not_ready"
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":   1,
				"diagnostic":       ProfileDiagnosticVersion,
				"readiness_status": readiness,
				"activation":       state,
				"drain":            observeDrain(r.Context(), drains),
			})
		})
	})
}

// constantTimeEqual is used by the admin auth middleware shared with retention.
func constantTimeEqual(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
