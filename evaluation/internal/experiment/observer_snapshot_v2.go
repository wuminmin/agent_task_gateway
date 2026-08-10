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

// observerWindowDomain domain-separates the retained before/after pair from
// either member's standalone identity.
const observerWindowDomain = "TASKGATE-FINAL-V5-OBSERVER-WINDOW-V2"

// ObserverStructuralRow is one classified row of the gateway_reader census.
type ObserverStructuralRow struct {
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	TopLevel        bool   `json:"toplevel"`
	Calls           int64  `json:"calls"`
}

// ObserverRuntimeIdentity binds what was actually running during the window.
//
// Gateway is the typed identity rather than a single gateway_source_sha256.
// That earlier field was filled by hashing the checkout the observer happened to
// run from, which asserts something the observer cannot know: that the running
// container was built from those bytes. Nothing in the evidence distinguished
// "the image came from this commit" from "someone ran the observer in a
// directory that happens to be at this commit".
type ObserverRuntimeIdentity struct {
	GatewayContainerID string `json:"gateway_container_id"`
	// Gateway is the immutable source/build/runtime identity the observer
	// inspected for itself through Docker Engine. Its members stay individually
	// inspectable; the healthcheck digest it carries is the running periodic
	// probe, which matters because the observer-v3 override replaces
	// /health/ready with /health/live and a probe that still reaches
	// /health/ready performs a full Business PostgreSQL Attestation on every
	// interval.
	Gateway    GatewayRuntimeIdentityV1  `json:"gateway"`
	PostgreSQL PostgreSQLRuntimeIdentity `json:"postgresql"`
	// ProjectTopologySHA256 binds the exact Compose service set, the container
	// identity of every service, the critical host PIDs and the periodic
	// healthcheck.
	//
	// The Gateway and PostgreSQL identities above say what those two services
	// are; they say nothing about the other ten. A sidecar restarted or replaced
	// between the before and after snapshots would leave both untouched while
	// changing what the measured interval contained, so the topology is bound
	// separately and compared across the window.
	ProjectTopologySHA256 string `json:"project_topology_sha256"`
}

// Validate rejects an identity that leaves the running system ambiguous.
func (identity ObserverRuntimeIdentity) Validate() error {
	if identity.GatewayContainerID == "" {
		return errors.New("gateway_container_id is empty")
	}
	if !validSHA256(identity.ProjectTopologySHA256) {
		return errors.New("project_topology_sha256 is not a lowercase SHA-256")
	}
	if err := identity.Gateway.Validate(); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	return identity.PostgreSQL.Validate()
}

// ObserverResourceEvidence is the evidence that the window was not disturbed by
// something outside the statement accounting.
type ObserverResourceEvidence struct {
	// PostmasterStartTime detects a PostgreSQL restart in band.
	PostmasterStartTime string `json:"postmaster_start_time"`
	// BusinessWALBytes and ControlWALBytes are write-ahead log positions, so WAL
	// growth across the window is reportable.
	BusinessWALBytes int64 `json:"business_wal_bytes"`
	ControlWALBytes  int64 `json:"control_wal_bytes"`
	// The Gateway resource counters carried since v1. They do not participate in
	// statement accounting, but a sample whose Gateway was starved or restarted
	// is not a clean measurement of anything.
	GatewayCPUUsec         int64 `json:"gateway_cpu_usec"`
	GatewayNetworkRXBytes  int64 `json:"gateway_network_rx_bytes"`
	GatewayNetworkTXBytes  int64 `json:"gateway_network_tx_bytes"`
	GatewayMemoryPeakBytes int64 `json:"gateway_memory_peak_bytes"`
	// GatewayRestartCount and GatewayOOMKilled come from the container runtime.
	GatewayRestartCount int64 `json:"gateway_restart_count"`
	GatewayOOMKilled    bool  `json:"gateway_oom_killed"`
	// ContainerRestarts is the whole project's restart total.
	ContainerRestarts int64 `json:"container_restarts"`
	// OOMEvents is the Gateway cgroup OOM counter.
	OOMEvents int64 `json:"oom_events"`
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
	// SchemaVersion is 2. It is carried alongside Version so a v1 reader that
	// only understands schema_version still refuses this document.
	SchemaVersion int `json:"schema_version"`
	// Phase is before or after. A window made of two snapshots of the same phase
	// is not a window.
	Phase string `json:"phase"`
	// ObserverWindowID ties the two snapshots of one measurement together. The
	// Adapter freezes it before observerBefore; a before/after pair carrying
	// different IDs is two halves of two different measurements.
	ObserverWindowID string `json:"observer_window_id"`
	// ClassifierManifestSHA256 and OperationBindingSHA256 are the identities the
	// observer was invoked under. Carrying them means the classifier a sample was
	// finalized with is the one the observer was told about, rather than one
	// chosen afterwards.
	ClassifierManifestSHA256 string `json:"classifier_manifest_sha256"`
	OperationBindingSHA256   string `json:"operation_binding_sha256,omitempty"`
	// ObserverSourceSHA256 is the source-build identity of the observer binary
	// that produced this snapshot.
	ObserverSourceSHA256 string `json:"observer_source_sha256"`
	Label                string `json:"label"`
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
	if snapshot.SchemaVersion != 2 {
		return fmt.Errorf("observer snapshot schema_version is %d; v3 acceptance requires 2",
			snapshot.SchemaVersion)
	}
	if snapshot.Phase != "before" && snapshot.Phase != "after" {
		return fmt.Errorf("observer snapshot phase %q is neither before nor after", snapshot.Phase)
	}
	if !validSHA256(snapshot.ObserverWindowID) {
		return errors.New("observer snapshot carries no window identifier")
	}
	if !validSHA256(snapshot.ClassifierManifestSHA256) {
		return errors.New("observer snapshot names no classifier manifest")
	}
	if snapshot.OperationBindingSHA256 != "" && !validSHA256(snapshot.OperationBindingSHA256) {
		return errors.New("observer snapshot operation binding is not a lowercase SHA-256")
	}
	if !validSHA256(snapshot.ObserverSourceSHA256) {
		return errors.New("observer snapshot carries no observer source identity")
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

// ValidateInterval checks the invariants that make two snapshots one continuous
// measurement. It is shared by classification, resource accounting, and the
// retained-window audit gate so none of those paths can silently accept a pair
// the others would call a reset, restart, or cross-runtime splice.
func (window ObserverWindowV2) ValidateInterval() error {
	if err := window.Before.Validate(); err != nil {
		return fmt.Errorf("before snapshot: %w", err)
	}
	if err := window.After.Validate(); err != nil {
		return fmt.Errorf("after snapshot: %w", err)
	}
	if window.Before.Role != window.After.Role {
		return fmt.Errorf("the window changed role from %q to %q",
			window.Before.Role, window.After.Role)
	}
	if window.Before.Phase != "before" || window.After.Phase != "after" {
		return fmt.Errorf("a window requires a before and an after snapshot, got %q and %q",
			window.Before.Phase, window.After.Phase)
	}
	for _, identity := range []struct {
		name          string
		before, after string
	}{
		{"observer window id", window.Before.ObserverWindowID, window.After.ObserverWindowID},
		{"classifier manifest", window.Before.ClassifierManifestSHA256, window.After.ClassifierManifestSHA256},
		{"operation binding", window.Before.OperationBindingSHA256, window.After.OperationBindingSHA256},
		{"observer source identity", window.Before.ObserverSourceSHA256, window.After.ObserverSourceSHA256},
	} {
		if identity.before != identity.after {
			return fmt.Errorf("the %s differs between the before and after snapshots (%s vs %s); "+
				"these are two halves of two different measurements",
				identity.name, shortDigest(identity.before), shortDigest(identity.after))
		}
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
		{"project container restart count", window.Before.Resource.ContainerRestarts, window.After.Resource.ContainerRestarts},
		{"gateway cgroup OOM events", window.Before.Resource.OOMEvents, window.After.Resource.OOMEvents},
	} {
		if invariant.before != invariant.after {
			return fmt.Errorf("the %s changed inside the observer window", invariant.name)
		}
	}
	if window.After.Dealloc != 0 {
		return fmt.Errorf("pg_stat_statements evicted %d entries; the census is incomplete",
			window.After.Dealloc)
	}
	return nil
}

// SHA256 is the canonical identity of the complete retained interval.
func (window ObserverWindowV2) SHA256() (string, error) {
	if err := window.ValidateInterval(); err != nil {
		return "", err
	}
	canonical, err := approval.CanonicalJSON(window)
	if err != nil {
		return "", fmt.Errorf("canonicalize observer window: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(observerWindowDomain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
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
	if err := window.ValidateInterval(); err != nil {
		return ObservedDelta{}, rejectTaskGateAt(err, rejectionGateObserverSnapshotInterval,
			rejectionFailureInvalidValue, rejectionSourceObserverWindow, rejectionSourceObserverWindow)
	}
	if classifier == nil {
		return ObservedDelta{}, rejectTaskGateAt(
			errors.New("a window cannot be classified without a compiled classifier"),
			rejectionGateClassifierBinding, rejectionFailureMissing,
			rejectionSourceClassifierPlan, rejectionSourceClassifierPlan)
	}
	// The commitment the observer was invoked under must be the classification it
	// is now being judged by.
	//
	// This is what makes the sealed digest mean anything. The observer records
	// which manifest it was told about before it took the "before" reading, and
	// the checks above only require the two snapshots to agree with EACH OTHER --
	// so without this a window could be classified by a manifest built after the
	// fact, from a derivation that had already seen what ran, while both
	// snapshots agreed on a digest nothing ever compared. The classification
	// would then be chosen with knowledge of the observations, which is precisely
	// what pre-registering it exists to rule out.
	committed := classifier.ManifestSHA256()
	if committed != window.Before.ClassifierManifestSHA256 {
		return ObservedDelta{}, rejectTaskGateAt(fmt.Errorf("the observer window was opened under classifier manifest %s "+
			"but is being classified by %s; the classification was not the one committed before the "+
			"observations were taken", shortDigest(window.Before.ClassifierManifestSHA256),
			shortDigest(committed)), rejectionGateClassifierCommitment,
			rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
			rejectionSHA256Pair(committed, window.Before.ClassifierManifestSHA256)...)
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
			return ObservedDelta{}, rejectTaskGateAt(
				fmt.Errorf("key %s went backwards by %d across the window",
					shortDigest(row.StrictASTSHA256), -change),
				rejectionGateCensusMonotonicity, rejectionFailureMismatch,
				rejectionSourceObserverWindow, rejectionSourceObserverWindow)
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
			return ObservedDelta{}, rejectTaskGateAt(
				fmt.Errorf("key %s disappeared from the census inside the window", shortDigest(key.digest)),
				rejectionGateCensusMonotonicity, rejectionFailureMismatch,
				rejectionSourceObserverWindow, rejectionSourceObserverWindow)
		}
	}
	if classified != delta.Total {
		differences := []rejectionDifferenceV1(nil)
		if delta.Total >= 0 && classified >= 0 {
			differences = append(differences,
				rejectionCountDifference(rejectionDifferenceExpectedCount, delta.Total),
				rejectionCountDifference(rejectionDifferenceActualCount, classified))
		}
		return ObservedDelta{}, rejectTaskGateAt(
			fmt.Errorf("the role total moved by %d but the classified rows account for %d; "+
				"a call was counted outside the census", delta.Total, classified),
			rejectionGateCensusMonotonicity, rejectionFailureMismatch,
			rejectionSourceObserverWindow, rejectionSourceObserverWindow, differences...)
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

// ObserverResourceDeltaV2 is what a window cost outside the statement
// accounting.
//
// None of it participates in acceptance: no class count, no plan and no
// classifier reads any of these. They are here because a sample whose Gateway
// was starved, restarted or OOM-killed is not a clean measurement of anything,
// and because the paper reports memory and CPU per operation.
type ObserverResourceDeltaV2 struct {
	GatewayMemoryPeakBytes int64
	GatewayCPUUsecDelta    int64
	GatewayNetworkRXDelta  int64
	GatewayNetworkTXDelta  int64
	ControlWALBytesDelta   int64
	BusinessWALBytesDelta  int64
}

// ResourceDelta subtracts the two snapshots' resource evidence.
//
// The disturbance checks are refusals rather than reported numbers. A restart or
// an OOM inside the window means the interval does not describe one continuous
// execution, so there is no delta to report -- the counters on either side of it
// belong to two different processes. ObserverWindowV2.Delta refuses the same
// transitions for the statement census; this is the same rule for the resource
// half, stated where the resource half is computed rather than left to whichever
// caller remembered.
func (window ObserverWindowV2) ResourceDelta() (ObserverResourceDeltaV2, error) {
	var delta ObserverResourceDeltaV2
	if err := window.ValidateInterval(); err != nil {
		return delta, err
	}
	before, after := window.Before.Resource, window.After.Resource
	if before.PostmasterStartTime != after.PostmasterStartTime {
		return delta, errors.New("PostgreSQL restarted inside the observer window")
	}
	if after.GatewayRestartCount != before.GatewayRestartCount || after.ContainerRestarts != before.ContainerRestarts {
		return delta, errors.New("a container restarted inside the observer window")
	}
	if after.GatewayOOMKilled || after.OOMEvents != before.OOMEvents {
		return delta, errors.New("the Gateway was OOM-killed inside the observer window")
	}
	// The memory peak is a high-water mark rather than a counter, so it is taken
	// from the after snapshot rather than subtracted. Subtracting two peaks would
	// report zero for a window whose peak was set before it opened, which is the
	// common case for a warm process. A high-water mark that went DOWN is not a
	// smaller peak; it is a different process or a reset counter.
	if after.GatewayMemoryPeakBytes < before.GatewayMemoryPeakBytes {
		return delta, errors.New("the Gateway memory peak regressed across the observer window")
	}
	delta.GatewayMemoryPeakBytes = after.GatewayMemoryPeakBytes
	for _, counter := range []struct {
		name          string
		target        *int64
		before, after int64
	}{
		{"gateway cpu", &delta.GatewayCPUUsecDelta, before.GatewayCPUUsec, after.GatewayCPUUsec},
		{"gateway network rx", &delta.GatewayNetworkRXDelta, before.GatewayNetworkRXBytes, after.GatewayNetworkRXBytes},
		{"gateway network tx", &delta.GatewayNetworkTXDelta, before.GatewayNetworkTXBytes, after.GatewayNetworkTXBytes},
		{"control WAL", &delta.ControlWALBytesDelta, before.ControlWALBytes, after.ControlWALBytes},
		{"business WAL", &delta.BusinessWALBytesDelta, before.BusinessWALBytes, after.BusinessWALBytes},
	} {
		if counter.after < counter.before {
			return ObserverResourceDeltaV2{}, fmt.Errorf("the %s counter went backwards across the window",
				counter.name)
		}
		*counter.target = counter.after - counter.before
	}
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
		return rejectTaskGateAt(err, rejectionGateControlPlan, rejectionFailureInvalidValue,
			rejectionSourceClassifierPlan, rejectionSourceClassifierPlan)
	}
	if len(delta.Unexpected) > 0 {
		keys := make([]string, 0, len(delta.Unexpected))
		for _, row := range delta.Unexpected {
			keys = append(keys, fmt.Sprintf("%s(toplevel=%t)x%d",
				shortDigest(row.StrictASTSHA256), row.TopLevel, row.Calls))
		}
		return rejectTaskGateStatementClassAt(fmt.Errorf("the window contains %d unexpected structural statement(s): %v",
			len(delta.Unexpected), keys), rejectionGateUnexpectedStructuralStatements,
			rejectionFailureMismatch, rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
			V3Unexpected,
			rejectionUnexpectedDifferences(delta.Unexpected)...)
	}
	expected := plan.Expected()
	if len(delta.PerClass) != len(expected) {
		return rejectTaskGateAt(
			fmt.Errorf("the observed delta carries %d statement classes, the closed world defines %d",
				len(delta.PerClass), len(expected)),
			rejectionGateClosedWorldClasses, rejectionFailureMismatch,
			rejectionSourceClassifierPlan, rejectionSourceObserverWindow,
			rejectionCountDifference(rejectionDifferenceExpectedCount, int64(len(expected))),
			rejectionCountDifference(rejectionDifferenceActualCount, int64(len(delta.PerClass))))
	}
	for class := range delta.PerClass {
		if _, known := expected[class]; !known {
			return rejectTaskGateAt(
				fmt.Errorf("the observed delta carries unknown statement class %q", class),
				rejectionGateClosedWorldClasses, rejectionFailureInvalidValue,
				rejectionSourceClassifierPlan, rejectionSourceObserverWindow)
		}
	}
	for _, class := range GatewayStatementClassesV3() {
		if _, present := delta.PerClass[class]; !present {
			return rejectTaskGateStatementClassAt(
				fmt.Errorf("the observed delta omits closed-world statement class %s", class),
				rejectionGateClosedWorldClasses, rejectionFailureMissing,
				rejectionSourceClassifierPlan, rejectionSourceObserverWindow, class)
		}
		if delta.PerClass[class] != expected[class] {
			differences := []rejectionDifferenceV1(nil)
			if delta.PerClass[class] >= 0 && expected[class] >= 0 {
				differences = append(differences,
					rejectionCountDifference(rejectionDifferenceExpectedCount, expected[class]),
					rejectionCountDifference(rejectionDifferenceActualCount, delta.PerClass[class]))
			}
			return rejectTaskGateStatementClassAt(fmt.Errorf("class %s observed %d, the plan expects %d",
				class, delta.PerClass[class], expected[class]),
				rejectionGateClosedWorldClasses, rejectionFailureMismatch,
				rejectionSourceClassifierPlan, rejectionSourceObserverWindow, class, differences...)
		}
	}
	if err := validateInternalExpectation(delta.Internal); err != nil {
		return rejectTaskGateAt(fmt.Errorf("observed PostgreSQL-internal accounting: %w", err),
			rejectionGateInternalStatementMultiset, rejectionFailureInvalidValue,
			rejectionSourceClassifierPlan, rejectionSourceObserverWindow)
	}
	if err := requireSameInternalExpectation(delta.Internal, plan.InternalExpectation); err != nil {
		return rejectTaskGateAt(fmt.Errorf("PostgreSQL-internal accounting: %w", err),
			rejectionGateInternalStatementMultiset, rejectionFailureMismatch,
			rejectionSourceClassifierPlan, rejectionSourceObserverWindow)
	}
	if delta.Total != plan.ExpectedTotal() {
		differences := []rejectionDifferenceV1(nil)
		if delta.Total >= 0 && plan.ExpectedTotal() >= 0 {
			differences = append(differences,
				rejectionCountDifference(rejectionDifferenceExpectedCount, plan.ExpectedTotal()),
				rejectionCountDifference(rejectionDifferenceActualCount, delta.Total))
		}
		return rejectTaskGateAt(
			fmt.Errorf("the window total is %d, the plan expects %d", delta.Total, plan.ExpectedTotal()),
			rejectionGateObserverTotal, rejectionFailureMismatch,
			rejectionSourceClassifierPlan, rejectionSourceObserverWindow, differences...)
	}
	return nil
}
