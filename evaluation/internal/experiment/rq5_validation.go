package experiment

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

const rq5VerifierVersion = "taskgate-final-v5-composite-verifier-v1"

// RQ5PublicationSetSHA256 binds the ordered, complete four-publication set.
// It is exported only within evaluation/internal so the real adapter and the
// finalizer use one canonical implementation.
func RQ5PublicationSetSHA256(values []RQ5PublicationEvidence) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return sha256Hex(append([]byte("TASKGATE-FINAL-V5-RQ5-PUBLICATIONS-V1\x00"), encoded...))
}

// RQ5LifecycleSHA256 binds the complete single-slot start/stop transcript.
func RQ5LifecycleSHA256(values []RQ5LifecycleStep) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return sha256Hex(append([]byte("TASKGATE-FINAL-V5-RQ5-SERVICE-SLOT-LIFECYCLE-V1\x00"), encoded...))
}

// ValidateRQ5Evidence is the adapter-side pass gate. A measured invariant
// violation must be retained as status=fail; an implementation must never emit
// pass and defer discovery to the finalizer.
func ValidateRQ5Evidence(sample Sample) error {
	return validateRQ5VerificationStrict(sample)
}

func validateRQ5VerificationStrict(sample Sample) error {
	evidence := sample.RQ5Verification
	if evidence == nil {
		return errors.New("raw RQ5 publication-cycle evidence is absent")
	}
	if sample.ExperimentID != "rq5" || !rq5fixture.IsCell(sample.WorkloadID, sample.Scale, sample.Mode, sample.Iteration) {
		return errors.New("sample is outside the source-controlled RQ5 matrix")
	}
	cycle, err := rq5fixture.LookupCycle(sample.Iteration)
	if err != nil {
		return err
	}
	if evidence.Version != rq5fixture.Version || evidence.FixtureSHA256 != rq5fixture.FixtureSHA256() ||
		evidence.RowsPerPublication != rq5fixture.RowsPerPublication || evidence.CycleIndex != cycle.Index ||
		evidence.FromDay != cycle.From || evidence.ToDay != cycle.To {
		return errors.New("RQ5 fixture, scale, or cyclic transition identity differs from preregistration")
	}
	for _, digest := range []string{
		evidence.BuildManifestSHA256, evidence.DatasetManifestSHA256, evidence.GeneratorSHA256, evidence.ConfigSHA256,
		evidence.PhaseBinarySHA256, evidence.OnlineBinarySHA256, evidence.OABinarySHA256,
		evidence.PublicationSetSHA256, evidence.LifecycleSHA256,
	} {
		if !validSHA256(digest) {
			return errors.New("RQ5 fixture contains an invalid source or transcript digest")
		}
	}
	for _, imageID := range []string{evidence.PhaseImageID, evidence.OnlineImageID, evidence.OAImageID} {
		if !validRQ5DockerImageID(imageID) {
			return errors.New("RQ5 runtime image identity is not a content-addressed Docker image ID")
		}
	}
	if evidence.OnlineImageID != evidence.OAImageID {
		return errors.New("RQ5 online and OA processes did not execute the same source-built runtime image")
	}
	if evidence.PhaseBinaryMTimeUnix != 0 || evidence.OnlineBinaryMTimeUnix != 0 || evidence.OABinaryMTimeUnix != 0 {
		return errors.New("RQ5 runtime binary mtime differs from the frozen SOURCE_DATE_EPOCH")
	}
	publications, err := validateRQ5Publications(evidence)
	if err != nil {
		return err
	}
	oldPublication, oldOK := publications[cycle.From]
	newPublication, newOK := publications[cycle.To]
	if !oldOK || !newOK {
		return errors.New("RQ5 cycle publications are absent")
	}
	if err := validateRQ5Build(evidence.Build, newPublication); err != nil {
		return err
	}
	if err := validateRQ5Topology(evidence.Topology, evidence.Lifecycle, oldPublication, newPublication); err != nil {
		return err
	}
	if evidence.LifecycleSHA256 != RQ5LifecycleSHA256(evidence.Lifecycle) {
		return errors.New("RQ5 service-slot lifecycle digest does not recompute")
	}
	if err := validateRQ5Route(evidence.Route, oldPublication, newPublication); err != nil {
		return err
	}
	selected := evidence.Route.NewInitial
	if sample.Mode == rq5fixture.RetainedMode {
		selected = evidence.Route.NewRestored
	}
	if err := validateRQ5SelectedSample(sample, selected); err != nil {
		return err
	}
	return nil
}

func validRQ5DockerImageID(value string) bool {
	digest, found := strings.CutPrefix(value, "sha256:")
	return found && validSHA256(digest)
}

func validateRQ5Publications(evidence *RQ5VerificationEvidence) (map[string]RQ5PublicationEvidence, error) {
	if len(evidence.Publications) != len(rq5fixture.Days) ||
		evidence.PublicationSetSHA256 != RQ5PublicationSetSHA256(evidence.Publications) {
		return nil, errors.New("RQ5 evidence does not bind the exact ordered four-publication set")
	}
	result := make(map[string]RQ5PublicationEvidence, len(evidence.Publications))
	uniqueCatalogs := map[string]bool{}
	uniqueManifests := map[string]bool{}
	uniqueResults := map[string]bool{}
	for index, value := range evidence.Publications {
		day := rq5fixture.Days[index]
		if value.Index != index || value.Day != day ||
			value.PublicationName != fmt.Sprintf("daily-lineitem-%s-r%d", day, rq5fixture.RowsPerPublication) ||
			value.RowCount != rq5fixture.RowsPerPublication || value.HOTArtifactBytes <= 0 ||
			value.HOTArtifactBytes > rq5fixture.MaximumHOTBytes || value.ArtifactBytes < value.HOTArtifactBytes {
			return nil, fmt.Errorf("RQ5 publication %d identity, scale, or artifact size is invalid", index)
		}
		for _, digest := range []string{
			value.ApprovedInputSHA256, value.CatalogSHA256, value.BundleManifestSHA256,
			value.PublicationManifestSHA256, value.DictionarySHA256, value.SidecarSHA256,
			value.SchemaSHA256, value.HOTArtifactSHA256, value.ColdArtifactSHA256,
			value.SidecarArtifactSHA256, value.DirectResultSHA256,
		} {
			if !validSHA256(digest) {
				return nil, fmt.Errorf("RQ5 publication %s contains an invalid digest", day)
			}
		}
		uniqueCatalogs[value.CatalogSHA256] = true
		uniqueManifests[value.PublicationManifestSHA256] = true
		uniqueResults[value.DirectResultSHA256] = true
		result[day] = value
	}
	if len(uniqueCatalogs) != len(rq5fixture.Days) || len(uniqueManifests) != len(rq5fixture.Days) ||
		len(uniqueResults) != len(rq5fixture.Days) {
		return nil, errors.New("RQ5 Catalog, publication, and direct-result identities must distinguish all four days")
	}
	return result, nil
}

func validateRQ5Build(build RQ5BuildEvidence, publication RQ5PublicationEvidence) error {
	if build.Day != publication.Day || build.RowCount != publication.RowCount ||
		build.ArtifactBytes != publication.ArtifactBytes || build.HOTArtifactBytes != publication.HOTArtifactBytes ||
		build.PublicationManifestSHA256 != publication.PublicationManifestSHA256 ||
		build.DictionarySHA256 != publication.DictionarySHA256 || !validSHA256(build.VerificationReceiptSHA256) {
		return errors.New("RQ5 measured build is not bound to the activated target publication")
	}
	phases := []struct {
		name  string
		value RQ5PhaseEvidence
	}{
		{"build", build.Build}, {"strict_verify", build.StrictVerify}, {"activation", build.Activation},
	}
	var sum float64
	for _, phase := range phases {
		value := phase.value
		if value.Phase != phase.name || value.Status != "pass" || !positiveFinite(value.WallMS) || value.PeakRSSBytes <= 0 {
			return fmt.Errorf("RQ5 %s phase did not complete as a measured real process", phase.name)
		}
		for _, digest := range []string{value.ExecutableSHA256, value.ArgvSHA256, value.StdoutSHA256, value.CommandReportSHA256} {
			if !validSHA256(digest) {
				return fmt.Errorf("RQ5 %s phase lacks a source/process binding", phase.name)
			}
		}
		sum += value.WallMS
	}
	if !positiveFinite(build.CycleWallMS) || math.Abs(build.CycleWallMS-sum) > 0.001 ||
		build.CycleWallMS > rq5fixture.DailyCycleGateMS {
		return errors.New("RQ5 build+strict-verify+activation wall time is inconsistent or exceeds five minutes")
	}
	return nil
}

func validateRQ5Topology(topology RQ5TopologyEvidence, lifecycle []RQ5LifecycleStep,
	oldPublication, newPublication RQ5PublicationEvidence) error {
	if topology.Model != rq5fixture.TopologyModel || topology.Disclosure != rq5fixture.TopologyDisclosure ||
		!topology.SingleServiceSlot || topology.RequestRouterPresent || topology.MaxConcurrentServices != 1 ||
		topology.ServiceStarts != 4 || topology.ServiceStops != 4 || topology.FinalActiveServices != 0 ||
		topology.HOTArtifactLimitBytes != rq5fixture.MaximumHOTBytes {
		return errors.New("RQ5 topology is not the disclosed single service slot with sequential restarts and no router")
	}
	wantMaxHOT := oldPublication.HOTArtifactBytes
	if newPublication.HOTArtifactBytes > wantMaxHOT {
		wantMaxHOT = newPublication.HOTArtifactBytes
	}
	if topology.MaxActiveHOTArtifactBytes != wantMaxHOT || topology.MaxActiveHOTArtifactBytes > topology.HOTArtifactLimitBytes {
		return errors.New("RQ5 topology retained multiple HOT publications or exceeded the one-Catalog limit")
	}
	if len(lifecycle) != 8 {
		return errors.New("RQ5 service-slot lifecycle must contain four exact start/stop pairs")
	}
	wantDays := []string{oldPublication.Day, oldPublication.Day, newPublication.Day, newPublication.Day,
		oldPublication.Day, oldPublication.Day, newPublication.Day, newPublication.Day}
	wantActions := []string{"start", "stop", "start", "stop", "start", "stop", "start", "stop"}
	wantReasons := []string{
		"start_retained_old", "stop_old_for_new_activation", "start_new_after_activation",
		"stop_new_for_retained_check", "start_old_for_retained_check", "stop_old_for_new_restore",
		"restore_new_after_retained_check", "stop_new_cycle_complete",
	}
	active := int64(0)
	currentInstance := ""
	seenStarts := map[string]bool{}
	for index, step := range lifecycle {
		publication := oldPublication
		if step.Day == newPublication.Day {
			publication = newPublication
		}
		if step.Sequence != index+1 || step.Action != wantActions[index] || step.Reason != wantReasons[index] ||
			step.Day != wantDays[index] || step.CatalogSHA256 != publication.CatalogSHA256 ||
			step.PublicationSHA256 != publication.PublicationManifestSHA256 || !validSHA256(step.ServiceInstanceSHA256) ||
			step.ActiveBefore != active {
			return fmt.Errorf("RQ5 service-slot lifecycle step %d is not the exact switch/check/restore sequence", index+1)
		}
		if step.Action == "start" {
			if active != 0 || step.ActiveAfter != 1 || seenStarts[step.ServiceInstanceSHA256] {
				return errors.New("RQ5 service slot overlapped services or reused a boot identity")
			}
			active = 1
			currentInstance = step.ServiceInstanceSHA256
			seenStarts[currentInstance] = true
		} else {
			if active != 1 || step.ActiveAfter != 0 || step.ServiceInstanceSHA256 != currentInstance {
				return errors.New("RQ5 service stop does not close the one active boot")
			}
			active = 0
			currentInstance = ""
		}
	}
	if active != 0 || int64(len(seenStarts)) != topology.ServiceStarts {
		return errors.New("RQ5 service slot did not terminate every sequential boot")
	}
	return nil
}

func validateRQ5Route(route RQ5RouteEvidence, oldPublication, newPublication RQ5PublicationEvidence) error {
	if !positiveFinite(route.SwitchToNewWallMS) || !positiveFinite(route.RetainedCheckWallMS) ||
		!positiveFinite(route.RestoreNewWallMS) || !positiveFinite(route.FullRouteWallMS) ||
		route.FullRouteWallMS+0.001 < route.SwitchToNewWallMS+route.RetainedCheckWallMS+route.RestoreNewWallMS {
		return errors.New("RQ5 switch/check/restore timing boundary is absent or overlapping")
	}
	for _, digest := range []string{
		route.OldPublicationSHA256, route.NewPublicationSHA256, route.OldCatalogSHA256, route.NewCatalogSHA256,
		route.OldTaskIDHash, route.NewTaskIDHash, route.NewRootTaskIDHash, route.ChildTaskIDHash,
		route.ChildParentTaskIDHash, route.ChildRootTaskIDHash, route.OldLedgerBeforeSHA256,
		route.OldLedgerAfterSHA256, route.NewLedgerBeforeRestore, route.NewLedgerAfterRestore,
		route.OldCacheKeySHA256, route.NewCacheKeySHA256, route.CrossReplaySourceSHA256,
		route.CrossReplayTargetSHA256, route.ChildPublicationSHA256, route.RootPublicationSHA256,
	} {
		if !validSHA256(digest) {
			return errors.New("RQ5 route contains an invalid identity or state digest")
		}
	}
	if route.OldPublicationSHA256 != oldPublication.PublicationManifestSHA256 ||
		route.NewPublicationSHA256 != newPublication.PublicationManifestSHA256 ||
		route.OldCatalogSHA256 != oldPublication.CatalogSHA256 || route.NewCatalogSHA256 != newPublication.CatalogSHA256 ||
		route.OldPublicationSHA256 == route.NewPublicationSHA256 || route.OldCatalogSHA256 == route.NewCatalogSHA256 {
		return errors.New("RQ5 old/new route is not bound to the measured Catalog publications")
	}
	if route.OldLedgerBeforeSHA256 != route.OldLedgerAfterSHA256 ||
		route.NewLedgerBeforeRestore != route.NewLedgerAfterRestore {
		return errors.New("RQ5 retained check or restore changed a root ledger")
	}
	if route.CrossReplaySourceSHA256 != route.OldPublicationSHA256 ||
		route.CrossReplayTargetSHA256 != route.NewPublicationSHA256 || route.CrossPublicationReplayHit ||
		route.OldCacheKeySHA256 == route.NewCacheKeySHA256 {
		return errors.New("RQ5 new publication reused an old-publication semantic observation")
	}
	if route.NewTaskIDHash != route.NewRootTaskIDHash || route.ChildParentTaskIDHash != route.NewTaskIDHash ||
		route.ChildRootTaskIDHash != route.NewRootTaskIDHash || route.ChildTaskIDHash == route.NewTaskIDHash ||
		route.ChildPublicationSHA256 != route.NewPublicationSHA256 || route.RootPublicationSHA256 != route.NewPublicationSHA256 {
		return errors.New("RQ5 delegated child is not bound to the restored new root publication")
	}
	checks := []struct {
		name        string
		value       RQ5QueryEvidence
		publication RQ5PublicationEvidence
		replay      bool
	}{
		{"old initial", route.OldInitial, oldPublication, false},
		{"new initial", route.NewInitial, newPublication, false},
		{"old retained", route.OldRetained, oldPublication, true},
		{"new restored", route.NewRestored, newPublication, true},
	}
	for _, check := range checks {
		if err := validateRQ5Query(check.value, check.publication, check.replay); err != nil {
			return fmt.Errorf("RQ5 %s query: %w", check.name, err)
		}
	}
	if route.OldInitial.TaskIDHash != route.OldTaskIDHash || route.OldRetained.TaskIDHash != route.OldTaskIDHash ||
		route.OldInitial.RootTaskIDHash != route.OldTaskIDHash || route.OldRetained.RootTaskIDHash != route.OldTaskIDHash ||
		route.NewInitial.TaskIDHash != route.NewTaskIDHash || route.NewRestored.TaskIDHash != route.NewTaskIDHash ||
		route.NewInitial.RootTaskIDHash != route.NewRootTaskIDHash || route.NewRestored.RootTaskIDHash != route.NewRootTaskIDHash {
		return errors.New("RQ5 queries did not preserve their old/new task and root bindings")
	}
	if route.OldInitial.ResultSHA256 != oldPublication.DirectResultSHA256 ||
		route.OldRetained.ResultSHA256 != oldPublication.DirectResultSHA256 ||
		route.NewInitial.ResultSHA256 != newPublication.DirectResultSHA256 ||
		route.NewRestored.ResultSHA256 != newPublication.DirectResultSHA256 ||
		route.OldInitial.ResultSHA256 == route.NewInitial.ResultSHA256 {
		return errors.New("RQ5 old/new task result differs from its direct frozen-publication oracle")
	}
	if route.OldInitial.RootSetSHA256After != route.OldRetained.RootSetSHA256Before ||
		route.OldRetained.RootSetSHA256Before != route.OldRetained.RootSetSHA256After ||
		route.NewInitial.RootSetSHA256After != route.NewRestored.RootSetSHA256Before ||
		route.NewRestored.RootSetSHA256Before != route.NewRestored.RootSetSHA256After {
		return errors.New("RQ5 replay root snapshots are not continuous and immutable")
	}
	uniqueRequests := map[string]bool{}
	uniqueQueries := map[string]bool{}
	for _, value := range []RQ5QueryEvidence{route.OldInitial, route.NewInitial, route.OldRetained, route.NewRestored} {
		if uniqueRequests[value.RequestIDHash] || uniqueQueries[value.QueryIDHash] {
			return errors.New("RQ5 query/replay operations reused a request or query identity")
		}
		uniqueRequests[value.RequestIDHash] = true
		uniqueQueries[value.QueryIDHash] = true
	}
	return nil
}

func validateRQ5Query(value RQ5QueryEvidence, publication RQ5PublicationEvidence, replay bool) error {
	if value.Day != publication.Day || value.CatalogSHA256 != publication.CatalogSHA256 ||
		value.PublicationSHA256 != publication.PublicationManifestSHA256 || value.SemanticReplay != replay ||
		value.ReceiptVersion != "8" || !value.ReceiptVerified || !value.ArtifactAvailable ||
		value.RowCount != 5 || value.ColumnCount != 3 || !positiveFinite(value.ClientAvailableMS) ||
		!positiveFinite(value.ClientFullDrainMS) || value.ClientFullDrainMS < value.ClientAvailableMS ||
		value.ParquetBytes <= 0 || value.EncryptedObjectBytes <= 0 || value.RootEpochBefore < 0 || value.RootEpochAfter < 0 {
		return errors.New("query lacks its real Catalog route, V8 receipt, AVAILABLE artifact, or replay marker")
	}
	for _, digest := range []string{
		value.TaskIDHash, value.RootTaskIDHash, value.RequestIDHash, value.QueryIDHash, value.ResultIDHash,
		value.ResultSHA256, value.RootSetSHA256Before, value.RootSetSHA256After, value.ReceiptSHA256,
		value.ArtifactIntentSHA256, value.AvailabilityAuditSHA256, value.PhysicalSQLSHA256,
		value.LogicalSQLSHA256, value.QueryPlanSHA256,
	} {
		if !validSHA256(digest) {
			return errors.New("query contains an invalid redacted identity or evidence digest")
		}
	}
	var pipelineSum float64
	for _, name := range requiredPipeline {
		measurement, present := value.PipelineMS[name]
		if !present || measurement < 0 || math.IsNaN(measurement) || math.IsInf(measurement, 0) {
			return errors.New("query lacks a complete finite production pipeline")
		}
		if name != "server_total" {
			pipelineSum += measurement
		}
	}
	if value.PipelineMS["server_total"]+0.001 < pipelineSum || value.DiagnosticMS == nil ||
		value.ActualReleaseFacts < 0 || value.ChargedReleaseFacts < 0 || value.ChargedReleaseFacts > value.ActualReleaseFacts ||
		value.ActualDependencyFacts < 0 || value.ChargedDependencyFacts < 0 || value.ChargedDependencyFacts > value.ActualDependencyFacts ||
		value.ActualOutcomeFacts < 0 || value.ChargedOutcomeFacts < 0 || value.ChargedOutcomeFacts > value.ActualOutcomeFacts ||
		value.PredicateAtomCount < 0 || value.CompositeCount < 0 {
		return errors.New("query pipeline or signed FactSet cardinalities are invalid")
	}
	if replay {
		if value.BusinessSQLDelta != 0 || value.RootEpochBefore != value.RootEpochAfter ||
			value.RootSetSHA256Before != value.RootSetSHA256After {
			return errors.New("retained semantic replay executed Business SQL or changed its root")
		}
	} else if value.BusinessSQLDelta <= 0 || value.RootEpochAfter <= value.RootEpochBefore {
		return errors.New("novel publication query was not observed on Business PostgreSQL and its root")
	}
	manifest := value.VerifierManifest
	if err := validateRedactedManifestStructure(manifest); err != nil {
		return err
	}
	if manifest.VerifierVersion != rq5VerifierVersion || manifest.QueryIDHash != value.QueryIDHash ||
		manifest.ResultIDHash != value.ResultIDHash || manifest.RootTaskIDHash != value.RootTaskIDHash ||
		manifest.ReceiptSHA256 != value.ReceiptSHA256 || manifest.ArtifactIntentSHA256 != value.ArtifactIntentSHA256 ||
		manifest.CanonicalCiphertextSize != value.EncryptedObjectBytes || manifest.ReleasedParquetSize != value.ParquetBytes {
		return errors.New("query verifier manifest differs from its signed terminal evidence")
	}
	return nil
}

func validateRQ5SelectedSample(sample Sample, selected RQ5QueryEvidence) error {
	manifest := selected.VerifierManifest
	if manifest == nil || sample.RootTaskIDHash != selected.RootTaskIDHash || sample.ResultSHA256 != selected.ResultSHA256 ||
		sample.BusinessSQLDelta != selected.BusinessSQLDelta || sample.RootEpochBefore != selected.RootEpochBefore ||
		sample.RootEpochAfter != selected.RootEpochAfter || sample.RootSetSHA256Before != selected.RootSetSHA256Before ||
		sample.RootSetSHA256After != selected.RootSetSHA256After || sample.ParquetBytes != selected.ParquetBytes ||
		sample.EncryptedObjectBytes != selected.EncryptedObjectBytes || sample.ReceiptVersion != selected.ReceiptVersion ||
		sample.ReceiptSHA256 != selected.ReceiptSHA256 || sample.ArtifactIntentSHA256 != selected.ArtifactIntentSHA256 ||
		sample.AvailabilityAuditSHA256 != selected.AvailabilityAuditSHA256 || sample.ReceiptVerified != selected.ReceiptVerified ||
		sample.ArtifactAvailable != selected.ArtifactAvailable || sample.ArtifactSHA256 != manifest.ReleasedParquetSHA256 ||
		sample.ObjectSHA256 != manifest.CanonicalCiphertextSHA256 || sample.RowCount != selected.RowCount ||
		sample.ColumnCount != selected.ColumnCount || sample.PhysicalSQLSHA256 != selected.PhysicalSQLSHA256 ||
		sample.LogicalSQLSHA256 != selected.LogicalSQLSHA256 || sample.QueryPlanSHA256 != selected.QueryPlanSHA256 ||
		sample.ReleaseSetSHA256 != manifest.ReleaseSetSHA256 || sample.DependencySetSHA256 != manifest.DependencySetSHA256 ||
		sample.OutcomeSetSHA256 != manifest.OutcomeSetSHA256 || sample.ActualReleaseFacts != selected.ActualReleaseFacts ||
		sample.ChargedReleaseFacts != selected.ChargedReleaseFacts || sample.ActualDependencyFacts != selected.ActualDependencyFacts ||
		sample.ChargedDependencyFacts != selected.ChargedDependencyFacts || sample.ActualOutcomeFacts != selected.ActualOutcomeFacts ||
		sample.ChargedOutcomeFacts != selected.ChargedOutcomeFacts || sample.PredicateAtomCount != selected.PredicateAtomCount ||
		sample.CompositeCount != selected.CompositeCount {
		return errors.New("RQ5 sample fields do not select the matching verified cycle query")
	}
	if len(sample.PipelineMS) != len(selected.PipelineMS) {
		return errors.New("RQ5 sample pipeline differs from its selected query")
	}
	for name, measurement := range selected.PipelineMS {
		if sample.PipelineMS[name] != measurement {
			return errors.New("RQ5 sample pipeline differs from its selected query")
		}
	}
	if sample.Mode == rq5fixture.RetainedMode {
		if !sample.SemanticReplay {
			return errors.New("RQ5 retained_route sample omitted its restored semantic replay marker")
		}
	} else if sample.SemanticReplay {
		return errors.New("RQ5 build_verify_activate sample mislabeled its novel query as replay")
	}
	return nil
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}
