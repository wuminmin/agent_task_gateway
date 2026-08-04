package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/internal/approval"
)

// ObserverSnapshotV2Version identifies the authoritative observer snapshot.
//
// Version 1 evidence must never be accepted on the v3 path. It carried the
// role-wide total and the structural census as separate readings, so the two
// could describe different instants and no check could tell; and it left the
// runtime and image identities to the Adapter, which is the party the finalizer
// is supposed to be independent of.
const ObserverSnapshotV2Version = "taskgate-final-v5-observer-snapshot-v2"

// observerSnapshotDomain domain-separates the snapshot digest.
const observerSnapshotDomain = "TASKGATE-FINAL-V5-OBSERVER-SNAPSHOT-V2"

// ObserverStructuralRow is one classified row of the gateway_reader census.
type ObserverStructuralRow struct {
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	TopLevel        bool   `json:"toplevel"`
	Calls           int64  `json:"calls"`
}

// ObserverRuntimeIdentity binds what was actually running during the window.
type ObserverRuntimeIdentity struct {
	GatewayImageID     string `json:"gateway_image_id"`
	GatewayContainerID string `json:"gateway_container_id"`
	// GatewaySourceSHA256 binds the running Gateway to the frozen submission
	// source, so a constant-only Connector mutation is detectable even when
	// pg_stat_statements has erased the constants.
	GatewaySourceSHA256 string `json:"gateway_source_sha256"`
	// HealthcheckCommandSHA256 digests the running periodic probe command. The
	// observer-v3 override replaces /health/ready with /health/live; without
	// binding it, the override could be silently omitted and the window would
	// absorb a wall-clock-dependent number of Attestations.
	HealthcheckCommandSHA256 string                    `json:"healthcheck_command_sha256"`
	PostgreSQL               PostgreSQLRuntimeIdentity `json:"postgresql"`
}

// Validate rejects an identity that leaves the running system ambiguous.
func (identity ObserverRuntimeIdentity) Validate() error {
	if err := requireImageID(identity.GatewayImageID); err != nil {
		return fmt.Errorf("gateway_image_id: %w", err)
	}
	if identity.GatewayContainerID == "" {
		return errors.New("gateway_container_id is empty")
	}
	if !validSHA256(identity.GatewaySourceSHA256) {
		return errors.New("gateway_source_sha256 is not a lowercase SHA-256")
	}
	if !validSHA256(identity.HealthcheckCommandSHA256) {
		return errors.New("healthcheck_command_sha256 is not a lowercase SHA-256")
	}
	return identity.PostgreSQL.Validate()
}

// ObserverResourceEvidence is the evidence that the window was not disturbed by
// something outside the statement accounting.
type ObserverResourceEvidence struct {
	// PostmasterStartTime detects a PostgreSQL restart in band.
	PostmasterStartTime string `json:"postmaster_start_time"`
	// WALLSN is the write-ahead log position, so WAL growth across the window is
	// reportable.
	WALLSN string `json:"wal_lsn"`
	// GatewayRestartCount and GatewayOOMKilled come from the container runtime.
	GatewayRestartCount int64 `json:"gateway_restart_count"`
	GatewayOOMKilled    bool  `json:"gateway_oom_killed"`
}

// ObserverSnapshotV2 is one atomic reading of everything the accounting depends
// on.
//
// The role-wide total and the structural rows come from ONE row set. Validate
// enforces that the total equals the sum of the rows, which is what makes "the
// total and the structural entries came from the same snapshot" checkable rather
// than merely intended. A snapshot that read them separately cannot satisfy it
// except by coincidence.
type ObserverSnapshotV2 struct {
	Version string `json:"version"`
	Label   string `json:"label"`
	// Role is the database role the census is restricted to.
	Role string `json:"role"`
	// Total is the role-wide call total.
	Total      int64                   `json:"total_calls"`
	Structural []ObserverStructuralRow `json:"structural_rows"`

	Environment MeasurementEnvironment `json:"measurement_environment"`
	StatsReset  string                 `json:"stats_reset"`
	Dealloc     int64                  `json:"dealloc"`

	Runtime  ObserverRuntimeIdentity  `json:"runtime_identity"`
	Resource ObserverResourceEvidence `json:"resource_evidence"`
}

// Validate rejects a snapshot that cannot serve as authoritative evidence.
func (snapshot ObserverSnapshotV2) Validate() error {
	if snapshot.Version != ObserverSnapshotV2Version {
		return fmt.Errorf("observer snapshot version %q is unsupported; v3 acceptance requires %s",
			snapshot.Version, ObserverSnapshotV2Version)
	}
	if snapshot.Role == "" {
		return errors.New("observer snapshot names no role")
	}
	if err := snapshot.Environment.Validate(); err != nil {
		return err
	}
	if snapshot.StatsReset == "" {
		return errors.New("observer snapshot carries no pg_stat_statements reset marker")
	}
	if err := snapshot.Runtime.Validate(); err != nil {
		return fmt.Errorf("observer snapshot runtime identity: %w", err)
	}
	if snapshot.Resource.PostmasterStartTime == "" {
		return errors.New("observer snapshot carries no postmaster start time")
	}
	seen := map[classifierKey]bool{}
	var sum int64
	for _, row := range snapshot.Structural {
		if !validSHA256(row.StrictASTSHA256) {
			return errors.New("observer snapshot has a structural row with no strict AST SHA-256")
		}
		key := classifierKey{digest: row.StrictASTSHA256, topLevel: row.TopLevel}
		if seen[key] {
			return fmt.Errorf("observer snapshot lists key %s twice at the same toplevel",
				shortDigest(row.StrictASTSHA256))
		}
		seen[key] = true
		if row.Calls < 0 {
			return fmt.Errorf("observer snapshot key %s reports %d calls",
				shortDigest(row.StrictASTSHA256), row.Calls)
		}
		sum += row.Calls
	}
	// The one check that makes atomicity verifiable rather than asserted.
	if sum != snapshot.Total {
		return fmt.Errorf("observer snapshot role total is %d but its structural rows sum to %d; "+
			"the total and the census did not come from one row set", snapshot.Total, sum)
	}
	if !sort.SliceIsSorted(snapshot.Structural, func(left, right int) bool {
		return structuralRowLess(snapshot.Structural[left], snapshot.Structural[right])
	}) {
		return errors.New("observer snapshot structural rows are not in canonical order")
	}
	return nil
}

func structuralRowLess(left, right ObserverStructuralRow) bool {
	if left.StrictASTSHA256 != right.StrictASTSHA256 {
		return left.StrictASTSHA256 < right.StrictASTSHA256
	}
	return !left.TopLevel && right.TopLevel
}

// SHA256 is the snapshot's canonical domain-separated digest.
func (snapshot ObserverSnapshotV2) SHA256() (string, error) {
	if err := snapshot.Validate(); err != nil {
		return "", err
	}
	canonical, err := approval.CanonicalJSON(snapshot)
	if err != nil {
		return "", fmt.Errorf("canonicalize observer snapshot: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(observerSnapshotDomain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (snapshot ObserverSnapshotV2) callsByKey() map[classifierKey]int64 {
	byKey := make(map[classifierKey]int64, len(snapshot.Structural))
	for _, row := range snapshot.Structural {
		byKey[classifierKey{digest: row.StrictASTSHA256, topLevel: row.TopLevel}] = row.Calls
	}
	return byKey
}

// ObserverWindowV2 is one before/after interval.
type ObserverWindowV2 struct {
	Before ObserverSnapshotV2 `json:"before"`
	After  ObserverSnapshotV2 `json:"after"`
}

// ObservedDelta is the classified result of one window.
type ObservedDelta struct {
	// Total is the role-wide call delta.
	Total int64 `json:"total_calls"`
	// PerClass is every class in the closed world, including zeros.
	PerClass map[GatewayStatementClassV3]int64 `json:"per_class"`
	// Internal is the exact per-key multiset of PostgreSQL-internal statements.
	Internal []InternalExpectation `json:"internal"`
	// Unexpected names the keys no classifier entry matched. They are carried so
	// a rejection can say what appeared, never merely that something did.
	Unexpected []ObserverStructuralRow `json:"unexpected,omitempty"`
}

// Delta computes the classified per-key and per-class deltas across the window.
//
// Every invariant the window depends on is checked first. A reset, an eviction,
// a restart, an environment change or a runtime/image change inside the window
// invalidates the measurement outright rather than skewing a count.
func (window ObserverWindowV2) Delta(classifier *CompiledClassifier) (ObservedDelta, error) {
	if err := window.Before.Validate(); err != nil {
		return ObservedDelta{}, fmt.Errorf("before snapshot: %w", err)
	}
	if err := window.After.Validate(); err != nil {
		return ObservedDelta{}, fmt.Errorf("after snapshot: %w", err)
	}
	if classifier == nil {
		return ObservedDelta{}, errors.New("a window cannot be classified without a compiled classifier")
	}
	if window.Before.Role != window.After.Role {
		return ObservedDelta{}, fmt.Errorf("the window changed role from %q to %q",
			window.Before.Role, window.After.Role)
	}
	for _, invariant := range []struct {
		name          string
		before, after any
	}{
		{"pg_stat_statements reset", window.Before.StatsReset, window.After.StatsReset},
		{"pg_stat_statements dealloc", window.Before.Dealloc, window.After.Dealloc},
		{"measurement environment", window.Before.Environment, window.After.Environment},
		{"runtime identity", window.Before.Runtime, window.After.Runtime},
		{"postmaster start time", window.Before.Resource.PostmasterStartTime, window.After.Resource.PostmasterStartTime},
		{"gateway restart count", window.Before.Resource.GatewayRestartCount, window.After.Resource.GatewayRestartCount},
		{"gateway OOM state", window.Before.Resource.GatewayOOMKilled, window.After.Resource.GatewayOOMKilled},
	} {
		if invariant.before != invariant.after {
			return ObservedDelta{}, fmt.Errorf("the %s changed inside the observer window", invariant.name)
		}
	}
	// A non-zero dealloc means pg_stat_statements evicted entries, so a count
	// this window depends on may simply be missing.
	if window.After.Dealloc != 0 {
		return ObservedDelta{}, fmt.Errorf("pg_stat_statements evicted %d entries; the census is incomplete",
			window.After.Dealloc)
	}

	delta := ObservedDelta{
		Total:    window.After.Total - window.Before.Total,
		PerClass: map[GatewayStatementClassV3]int64{},
	}
	for _, class := range GatewayStatementClassesV3() {
		delta.PerClass[class] = 0
	}
	before := window.Before.callsByKey()
	internal := map[string]int64{}
	var classified int64
	for _, row := range window.After.Structural {
		key := classifierKey{digest: row.StrictASTSHA256, topLevel: row.TopLevel}
		change := row.Calls - before[key]
		if change < 0 {
			return ObservedDelta{}, fmt.Errorf("key %s went backwards by %d across the window",
				shortDigest(row.StrictASTSHA256), -change)
		}
		if change == 0 {
			continue
		}
		classified += change
		class := classifier.Classify(row.StrictASTSHA256, row.TopLevel)
		delta.PerClass[class] += change
		switch class {
		case V3PostgreSQLInternalAttestation:
			internal[row.StrictASTSHA256] += change
		case V3Unexpected:
			delta.Unexpected = append(delta.Unexpected,
				ObserverStructuralRow{StrictASTSHA256: row.StrictASTSHA256, TopLevel: row.TopLevel, Calls: change})
		}
	}
	// A key present before and absent after means the census lost rows.
	for key, calls := range before {
		if calls == 0 {
			continue
		}
		if _, present := window.After.callsByKey()[key]; !present {
			return ObservedDelta{}, fmt.Errorf("key %s disappeared from the census inside the window",
				shortDigest(key.digest))
		}
	}
	if classified != delta.Total {
		return ObservedDelta{}, fmt.Errorf("the role total moved by %d but the classified rows account for %d; "+
			"a call was counted outside the census", delta.Total, classified)
	}
	for key, calls := range internal {
		delta.Internal = append(delta.Internal,
			InternalExpectation{StrictASTSHA256: key, RequiredTopLevel: false, Calls: calls})
	}
	sort.Slice(delta.Internal, func(left, right int) bool {
		return delta.Internal[left].StrictASTSHA256 < delta.Internal[right].StrictASTSHA256
	})
	sort.Slice(delta.Unexpected, func(left, right int) bool {
		return structuralRowLess(delta.Unexpected[left], delta.Unexpected[right])
	})
	return delta, nil
}

// Accept compares an observed delta against a plan, class by class and key by
// key.
//
// Equality throughout, never a lower bound. Weakening any class to "at least the
// expected count" would let an extra legitimate-looking statement pass, and
// weakening the internal comparison to a total would let one internal key be
// replaced by another.
func (delta ObservedDelta) Accept(plan GatewayControlPlanV3) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	if len(delta.Unexpected) > 0 {
		keys := make([]string, 0, len(delta.Unexpected))
		for _, row := range delta.Unexpected {
			keys = append(keys, fmt.Sprintf("%s(toplevel=%t)x%d",
				shortDigest(row.StrictASTSHA256), row.TopLevel, row.Calls))
		}
		return fmt.Errorf("the window contains %d unexpected structural statement(s): %v",
			len(delta.Unexpected), keys)
	}
	expected := plan.Expected()
	for _, class := range GatewayStatementClassesV3() {
		if delta.PerClass[class] != expected[class] {
			return fmt.Errorf("class %s observed %d, the plan expects %d",
				class, delta.PerClass[class], expected[class])
		}
	}
	if err := requireSameInternalExpectation(delta.Internal, plan.InternalExpectation); err != nil {
		return fmt.Errorf("PostgreSQL-internal accounting: %w", err)
	}
	if delta.Total != plan.ExpectedTotal() {
		return fmt.Errorf("the window total is %d, the plan expects %d", delta.Total, plan.ExpectedTotal())
	}
	return nil
}
