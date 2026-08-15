package experiment

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	jsonschema "github.com/google/jsonschema-go/jsonschema"

	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

func TestRQ5CompleteSampleMatchesStrictJSONSchema(t *testing.T) {
	schemaBytes, err := os.ReadFile(filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatal(err)
	}
	// The retained artifact intentionally uses a content identity rather than
	// a fetchable URI. Give the in-process resolver an absolute base while
	// preserving every validation keyword from the checked-in schema.
	schema.ID = "https://taskgate.local/schema/sample-v1"
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	validate := func(value Sample) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var instance any
		if err := json.Unmarshal(encoded, &instance); err != nil {
			return err
		}
		return resolved.Validate(instance)
	}
	for iteration := 1; iteration <= rq5fixture.CyclesPerDeployment; iteration++ {
		for _, mode := range []string{rq5fixture.BuildMode, rq5fixture.RetainedMode} {
			if err := validate(validRQ5SampleForTest(iteration, mode)); err != nil {
				t.Fatalf("cycle %d mode %s: %v", iteration, mode, err)
			}
		}
	}
	const historicalFixtureSHA256 = "193a0d70cc6bbd3a253a7513ec8cd0f7b6fb4eac6741703f9841523abb3f24c2"
	historicalLimit := rq5fixture.MaximumHOTBytes / 32 * 5
	historical := validRQ5SampleForTest(1, rq5fixture.BuildMode)
	historical.RQ5Verification.FixtureSHA256 = historicalFixtureSHA256
	historical.RQ5Verification.Topology.HOTArtifactLimitBytes = historicalLimit
	if err := validate(historical); err != nil {
		t.Fatalf("historical RQ5 fixture was reinterpreted or rejected: %v", err)
	}
	historical.RQ5Verification.Topology.HOTArtifactLimitBytes = rq5fixture.MaximumHOTBytes
	if err := validate(historical); err == nil {
		t.Fatal("historical fixture identity was accepted with the current HOT limit")
	}
	currentWithHistoricalLimit := validRQ5SampleForTest(1, rq5fixture.BuildMode)
	currentWithHistoricalLimit.RQ5Verification.Topology.HOTArtifactLimitBytes = historicalLimit
	if err := validate(currentWithHistoricalLimit); err == nil {
		t.Fatal("current fixture identity was accepted with the historical HOT limit")
	}
	mutated := validRQ5SampleForTest(1, rq5fixture.BuildMode)
	mutated.RQ5Verification.Lifecycle[0].Reason = "start old Catalog for retained baseline"
	if err := validate(mutated); err == nil {
		t.Fatal("non-wire lifecycle reason was accepted by schema")
	}
}

func rq5TestDigest(seed int) string { return fmt.Sprintf("%064x", seed) }

func validRQ5SampleForTest(iteration int, mode string) Sample {
	cycle, err := rq5fixture.LookupCycle(iteration)
	if err != nil {
		panic(err)
	}
	evidence := &RQ5VerificationEvidence{
		Version: rq5fixture.Version, FixtureSHA256: rq5fixture.FixtureSHA256(),
		BuildManifestSHA256: rq5TestDigest(4), PhaseImageID: "sha256:" + rq5TestDigest(5),
		OnlineImageID: "sha256:" + rq5TestDigest(6), OAImageID: "sha256:" + rq5TestDigest(6),
		PhaseBinarySHA256: rq5TestDigest(7), OnlineBinarySHA256: rq5TestDigest(8), OABinarySHA256: rq5TestDigest(9),
		DatasetManifestSHA256: rq5TestDigest(1), GeneratorSHA256: rq5TestDigest(2), ConfigSHA256: rq5TestDigest(3),
		RowsPerPublication: rq5fixture.RowsPerPublication, CycleIndex: cycle.Index, FromDay: cycle.From, ToDay: cycle.To,
	}
	byDay := map[string]RQ5PublicationEvidence{}
	for index, day := range rq5fixture.Days {
		base := 100 + index*20
		publication := RQ5PublicationEvidence{
			Index: index, Day: day,
			PublicationName: fmt.Sprintf("daily-lineitem-%s-r%d", day, rq5fixture.RowsPerPublication),
			RowCount:        rq5fixture.RowsPerPublication, ApprovedInputSHA256: rq5TestDigest(base),
			CatalogSHA256: rq5TestDigest(base + 1), BundleManifestSHA256: rq5TestDigest(base + 2),
			PublicationManifestSHA256: rq5TestDigest(base + 3), DictionarySHA256: rq5TestDigest(base + 4),
			SidecarSHA256: rq5TestDigest(base + 5), SchemaSHA256: rq5TestDigest(base + 6),
			HOTArtifactSHA256: rq5TestDigest(base + 7), ColdArtifactSHA256: rq5TestDigest(base + 8),
			SidecarArtifactSHA256: rq5TestDigest(base + 9), DirectResultSHA256: rq5TestDigest(base + 10),
			ArtifactBytes: 700_000_000 + int64(index), HOTArtifactBytes: 139_000_000 + int64(index),
		}
		evidence.Publications = append(evidence.Publications, publication)
		byDay[day] = publication
	}
	evidence.PublicationSetSHA256 = RQ5PublicationSetSHA256(evidence.Publications)
	oldPublication, newPublication := byDay[cycle.From], byDay[cycle.To]
	phase := func(name string, base int) RQ5PhaseEvidence {
		return RQ5PhaseEvidence{Phase: name, Status: "pass", WallMS: 10, PeakRSSBytes: 200_000_000,
			ExecutableSHA256: rq5TestDigest(base), ArgvSHA256: rq5TestDigest(base + 1),
			StdoutSHA256: rq5TestDigest(base + 2), CommandReportSHA256: rq5TestDigest(base + 3)}
	}
	evidence.Build = RQ5BuildEvidence{
		Day: newPublication.Day, RowCount: newPublication.RowCount, CycleWallMS: 30,
		ArtifactBytes: newPublication.ArtifactBytes, HOTArtifactBytes: newPublication.HOTArtifactBytes,
		PublicationManifestSHA256: newPublication.PublicationManifestSHA256,
		DictionarySHA256:          newPublication.DictionarySHA256, VerificationReceiptSHA256: rq5TestDigest(300),
		Build: phase("build", 310), StrictVerify: phase("strict_verify", 320), Activation: phase("activation", 330),
	}
	evidence.Topology = RQ5TopologyEvidence{
		Model: rq5fixture.TopologyModel, Disclosure: rq5fixture.TopologyDisclosure, SingleServiceSlot: true,
		MaxConcurrentServices: 1, ServiceStarts: 4, ServiceStops: 4, HOTArtifactLimitBytes: rq5fixture.MaximumHOTBytes,
		MaxActiveHOTArtifactBytes: maxInt64(oldPublication.HOTArtifactBytes, newPublication.HOTArtifactBytes),
	}
	days := []string{cycle.From, cycle.From, cycle.To, cycle.To, cycle.From, cycle.From, cycle.To, cycle.To}
	actions := []string{"start", "stop", "start", "stop", "start", "stop", "start", "stop"}
	reasons := []string{"start_retained_old", "stop_old_for_new_activation", "start_new_after_activation",
		"stop_new_for_retained_check", "start_old_for_retained_check", "stop_old_for_new_restore",
		"restore_new_after_retained_check", "stop_new_cycle_complete"}
	instances := []string{rq5TestDigest(400), rq5TestDigest(400), rq5TestDigest(401), rq5TestDigest(401),
		rq5TestDigest(402), rq5TestDigest(402), rq5TestDigest(403), rq5TestDigest(403)}
	active := int64(0)
	for index := range actions {
		publication := byDay[days[index]]
		after := int64(1)
		if actions[index] == "stop" {
			after = 0
		}
		evidence.Lifecycle = append(evidence.Lifecycle, RQ5LifecycleStep{
			Sequence: index + 1, Action: actions[index], Reason: reasons[index], Day: days[index],
			CatalogSHA256: publication.CatalogSHA256, PublicationSHA256: publication.PublicationManifestSHA256,
			ServiceInstanceSHA256: instances[index], ActiveBefore: active, ActiveAfter: after,
		})
		active = after
	}
	evidence.LifecycleSHA256 = RQ5LifecycleSHA256(evidence.Lifecycle)
	oldTask, newTask := rq5TestDigest(500), rq5TestDigest(501)
	query := func(publication RQ5PublicationEvidence, task string, base int, replay bool, before, after string) RQ5QueryEvidence {
		business := int64(1)
		beforeEpoch, afterEpoch := int64(0), int64(1)
		if replay {
			business = 0
			beforeEpoch, afterEpoch = 1, 1
		}
		manifest := &RedactedVerifierManifest{
			VerifierVersion: rq5VerifierVersion, QueryIDHash: rq5TestDigest(base + 1), ResultIDHash: rq5TestDigest(base + 2),
			RootTaskIDHash: task, ReceiptSHA256: rq5TestDigest(base + 3), ObservationSHA256: rq5TestDigest(base + 4),
			ReleaseSetSHA256: rq5TestDigest(base + 5), DependencySetSHA256: rq5TestDigest(base + 6),
			OutcomeSetSHA256: rq5TestDigest(base + 7), ArtifactIntentSHA256: rq5TestDigest(base + 8),
			ObjectKeySHA256: rq5TestDigest(base + 9), CanonicalCiphertextSHA256: rq5TestDigest(base + 10),
			CanonicalCiphertextSize: 900, ReleasedParquetSHA256: rq5TestDigest(base + 11), ReleasedParquetSize: 800,
			SchemaSHA256: rq5TestDigest(base + 12), TerminalAuditSequence: int64(base),
			RegistrationAuditSequence: int64(base + 1), AvailabilityAuditSequence: int64(base + 2), VerificationResult: "pass",
		}
		return RQ5QueryEvidence{
			Day: publication.Day, CatalogSHA256: publication.CatalogSHA256,
			PublicationSHA256: publication.PublicationManifestSHA256, TaskIDHash: task, RootTaskIDHash: task,
			RequestIDHash: rq5TestDigest(base), QueryIDHash: manifest.QueryIDHash, ResultIDHash: manifest.ResultIDHash,
			ResultSHA256: publication.DirectResultSHA256, RowCount: 5, ColumnCount: 3,
			ClientAvailableMS: 5, ClientFullDrainMS: 6,
			PipelineMS: map[string]float64{"prepare": 1, "execute_and_derive": 1, "artifact_stage": 1,
				"control_settlement": 1, "artifact_publication": 1, "response_finalize": 1, "server_total": 6},
			DiagnosticMS: map[string]float64{}, PhysicalSQLSHA256: rq5TestDigest(base + 14),
			LogicalSQLSHA256: rq5TestDigest(base + 15), QueryPlanSHA256: rq5TestDigest(base + 16),
			ActualReleaseFacts: 5, ChargedReleaseFacts: 5, ActualDependencyFacts: 5, ChargedDependencyFacts: 5,
			ActualOutcomeFacts: 1, ChargedOutcomeFacts: 1, PredicateAtomCount: 1, CompositeCount: 1,
			SemanticReplay: replay, BusinessSQLDelta: business,
			RootEpochBefore: beforeEpoch, RootEpochAfter: afterEpoch, RootSetSHA256Before: before,
			RootSetSHA256After: after, ParquetBytes: 800, EncryptedObjectBytes: 900, ReceiptVersion: "8",
			ReceiptSHA256: manifest.ReceiptSHA256, ArtifactIntentSHA256: manifest.ArtifactIntentSHA256,
			AvailabilityAuditSHA256: rq5TestDigest(base + 13), ReceiptVerified: true, ArtifactAvailable: true,
			VerifierManifest: manifest,
		}
	}
	oldFinal, newFinal := rq5TestDigest(700), rq5TestDigest(701)
	evidence.Route = RQ5RouteEvidence{
		SwitchToNewWallMS: 5, RetainedCheckWallMS: 5, RestoreNewWallMS: 5, FullRouteWallMS: 20,
		OldPublicationSHA256: oldPublication.PublicationManifestSHA256,
		NewPublicationSHA256: newPublication.PublicationManifestSHA256,
		OldCatalogSHA256:     oldPublication.CatalogSHA256, NewCatalogSHA256: newPublication.CatalogSHA256,
		OldTaskIDHash: oldTask, NewTaskIDHash: newTask, NewRootTaskIDHash: newTask,
		ChildTaskIDHash: rq5TestDigest(502), ChildParentTaskIDHash: newTask, ChildRootTaskIDHash: newTask,
		OldLedgerBeforeSHA256: rq5TestDigest(510), OldLedgerAfterSHA256: rq5TestDigest(510),
		NewLedgerBeforeRestore: rq5TestDigest(511), NewLedgerAfterRestore: rq5TestDigest(511),
		OldCacheKeySHA256: rq5TestDigest(512), NewCacheKeySHA256: rq5TestDigest(513),
		CrossReplaySourceSHA256: oldPublication.PublicationManifestSHA256,
		CrossReplayTargetSHA256: newPublication.PublicationManifestSHA256,
		ChildPublicationSHA256:  newPublication.PublicationManifestSHA256,
		RootPublicationSHA256:   newPublication.PublicationManifestSHA256,
		OldInitial:              query(oldPublication, oldTask, 600, false, rq5TestDigest(699), oldFinal),
		NewInitial:              query(newPublication, newTask, 620, false, rq5TestDigest(698), newFinal),
		OldRetained:             query(oldPublication, oldTask, 640, true, oldFinal, oldFinal),
		NewRestored:             query(newPublication, newTask, 660, true, newFinal, newFinal),
	}
	selected := evidence.Route.NewInitial
	semanticReplay := false
	if mode == rq5fixture.RetainedMode {
		selected = evidence.Route.NewRestored
		semanticReplay = true
	}
	manifest := selected.VerifierManifest
	return Sample{
		SchemaVersion: 1, CampaignID: "campaign", DeploymentID: "deployment-01", ExperimentID: "rq5",
		CellID:   rq5fixture.WorkloadID + "/" + rq5fixture.Scale + "/" + mode,
		SampleID: "sample", Iteration: iteration, ProcessReplicate: 1, OrderPosition: 1, RandomSeed: 1, PairID: "pair",
		PairedSystemOrder: rq5fixture.BuildMode + "," + rq5fixture.RetainedMode,
		RootGroupID:       rq5fixture.BuildMode + "," + rq5fixture.RetainedMode,
		System:            "taskgate", Mode: mode, WorkloadID: rq5fixture.WorkloadID, Scale: rq5fixture.Scale,
		PipelineMS: selected.PipelineMS, DiagnosticMS: selected.DiagnosticMS,
		RowCount: selected.RowCount, ColumnCount: selected.ColumnCount, ResultSHA256: selected.ResultSHA256,
		PhysicalSQLSHA256: selected.PhysicalSQLSHA256, LogicalSQLSHA256: selected.LogicalSQLSHA256,
		QueryPlanSHA256: selected.QueryPlanSHA256, ReleaseSetSHA256: manifest.ReleaseSetSHA256,
		DependencySetSHA256: manifest.DependencySetSHA256, OutcomeSetSHA256: manifest.OutcomeSetSHA256,
		ActualReleaseFacts: selected.ActualReleaseFacts, ChargedReleaseFacts: selected.ChargedReleaseFacts,
		ActualDependencyFacts: selected.ActualDependencyFacts, ChargedDependencyFacts: selected.ChargedDependencyFacts,
		ActualOutcomeFacts: selected.ActualOutcomeFacts, ChargedOutcomeFacts: selected.ChargedOutcomeFacts,
		PredicateAtomCount: selected.PredicateAtomCount, CompositeCount: selected.CompositeCount,
		ArtifactSHA256: manifest.ReleasedParquetSHA256, ObjectSHA256: manifest.CanonicalCiphertextSHA256,
		SemanticReplay: semanticReplay, BusinessSQLDelta: selected.BusinessSQLDelta,
		RootEpochBefore: selected.RootEpochBefore, RootEpochAfter: selected.RootEpochAfter,
		RootTaskIDHash: selected.RootTaskIDHash, RootSetSHA256Before: selected.RootSetSHA256Before,
		RootSetSHA256After: selected.RootSetSHA256After, ParquetBytes: selected.ParquetBytes,
		EncryptedObjectBytes: selected.EncryptedObjectBytes, ReceiptVersion: selected.ReceiptVersion,
		ReceiptSHA256: selected.ReceiptSHA256, ArtifactIntentSHA256: selected.ArtifactIntentSHA256,
		AvailabilityAuditSHA256: selected.AvailabilityAuditSHA256, ReceiptVerified: true, ArtifactAvailable: true,
		Status: "pass", PublicationEligible: true, RQ5Verification: evidence,
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func cloneRQ5Sample(t *testing.T, value Sample) Sample {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result Sample
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRQ5StrictValidatorAcceptsFourCyclesAndPairedModes(t *testing.T) {
	for iteration := 1; iteration <= rq5fixture.CyclesPerDeployment; iteration++ {
		for _, mode := range []string{rq5fixture.BuildMode, rq5fixture.RetainedMode} {
			sample := validRQ5SampleForTest(iteration, mode)
			if err := ValidateRQ5Evidence(sample); err != nil {
				t.Fatalf("cycle %d mode %s: %v", iteration, mode, err)
			}
		}
	}
}

func TestRQ5StrictValidatorRejectsTopologyRouteAndVerifierMutations(t *testing.T) {
	base := validRQ5SampleForTest(2, rq5fixture.RetainedMode)
	tests := map[string]func(*Sample){
		"smoke scale":            func(value *Sample) { value.RQ5Verification.RowsPerPublication = 2_000 },
		"build manifest omitted": func(value *Sample) { value.RQ5Verification.BuildManifestSHA256 = "" },
		"phase image is a tag":   func(value *Sample) { value.RQ5Verification.PhaseImageID = "rq5-phase:latest" },
		"OA image diverged": func(value *Sample) {
			value.RQ5Verification.OAImageID = "sha256:" + rq5TestDigest(999)
		},
		"online binary digest omitted": func(value *Sample) { value.RQ5Verification.OnlineBinarySHA256 = "" },
		"OA binary mtime drifted":      func(value *Sample) { value.RQ5Verification.OABinaryMTimeUnix = 1 },
		"publication omitted": func(value *Sample) {
			value.RQ5Verification.Publications = value.RQ5Verification.Publications[:3]
			value.RQ5Verification.PublicationSetSHA256 = RQ5PublicationSetSHA256(value.RQ5Verification.Publications)
		},
		"hot aggregate": func(value *Sample) {
			value.RQ5Verification.Publications[0].HOTArtifactBytes = rq5fixture.MaximumHOTBytes + 1
			value.RQ5Verification.PublicationSetSHA256 = RQ5PublicationSetSHA256(value.RQ5Verification.Publications)
		},
		"placeholder phase": func(value *Sample) { value.RQ5Verification.Build.Build.ExecutableSHA256 = "placeholder" },
		"five minute gate": func(value *Sample) {
			value.RQ5Verification.Build.Build.WallMS = 300_001
			value.RQ5Verification.Build.CycleWallMS = 300_021
		},
		"four services":  func(value *Sample) { value.RQ5Verification.Topology.MaxConcurrentServices = 4 },
		"request router": func(value *Sample) { value.RQ5Verification.Topology.RequestRouterPresent = true },
		"client pointer only": func(value *Sample) {
			value.RQ5Verification.Lifecycle = value.RQ5Verification.Lifecycle[:2]
			value.RQ5Verification.LifecycleSHA256 = RQ5LifecycleSHA256(value.RQ5Verification.Lifecycle)
		},
		"missing restore": func(value *Sample) {
			value.RQ5Verification.Lifecycle[6].Reason = "not_restored"
			value.RQ5Verification.LifecycleSHA256 = RQ5LifecycleSHA256(value.RQ5Verification.Lifecycle)
		},
		"boot reused": func(value *Sample) {
			value.RQ5Verification.Lifecycle[4].ServiceInstanceSHA256 = value.RQ5Verification.Lifecycle[0].ServiceInstanceSHA256
			value.RQ5Verification.Lifecycle[5].ServiceInstanceSHA256 = value.RQ5Verification.Lifecycle[0].ServiceInstanceSHA256
			value.RQ5Verification.LifecycleSHA256 = RQ5LifecycleSHA256(value.RQ5Verification.Lifecycle)
		},
		"old ledger changed":            func(value *Sample) { value.RQ5Verification.Route.OldLedgerAfterSHA256 = rq5TestDigest(999) },
		"new ledger changed on restore": func(value *Sample) { value.RQ5Verification.Route.NewLedgerAfterRestore = rq5TestDigest(999) },
		"cross publication hit":         func(value *Sample) { value.RQ5Verification.Route.CrossPublicationReplayHit = true },
		"cache key reused": func(value *Sample) {
			value.RQ5Verification.Route.NewCacheKeySHA256 = value.RQ5Verification.Route.OldCacheKeySHA256
		},
		"child switched root": func(value *Sample) { value.RQ5Verification.Route.ChildRootTaskIDHash = rq5TestDigest(999) },
		"old routed new": func(value *Sample) {
			value.RQ5Verification.Route.OldRetained.PublicationSHA256 = value.RQ5Verification.Route.NewPublicationSHA256
		},
		"new result wrong":      func(value *Sample) { value.RQ5Verification.Route.NewRestored.ResultSHA256 = rq5TestDigest(999) },
		"replay executed SQL":   func(value *Sample) { value.RQ5Verification.Route.NewRestored.BusinessSQLDelta = 1 },
		"missing manifest":      func(value *Sample) { value.RQ5Verification.Route.NewRestored.VerifierManifest = nil },
		"self asserted receipt": func(value *Sample) { value.RQ5Verification.Route.NewRestored.ReceiptVerified = false },
		"sample not transcript": func(value *Sample) { value.ReceiptSHA256 = rq5TestDigest(999) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := cloneRQ5Sample(t, base)
			mutate(&value)
			if err := ValidateRQ5Evidence(value); err == nil {
				t.Fatal("mutation accepted")
			}
		})
	}
}

func TestRQ5FinalizerRejectsRuntimeIdentityChangeAcrossDeployments(t *testing.T) {
	first := validRQ5SampleForTest(1, rq5fixture.BuildMode)
	first.DeploymentID = "deployment-01"
	second := validRQ5SampleForTest(2, rq5fixture.BuildMode)
	second.DeploymentID = "deployment-02"
	expectedManifest := first.RQ5Verification.BuildManifestSHA256
	if reasons := validateRQ5RuntimeIdentityConsistency([]Sample{first, second}, expectedManifest); len(reasons) != 0 {
		t.Fatalf("stable runtime identity was rejected: %v", reasons)
	}
	second.RQ5Verification.PhaseImageID = "sha256:" + rq5TestDigest(999)
	if reasons := validateRQ5RuntimeIdentityConsistency([]Sample{first, second}, expectedManifest); len(reasons) != 1 ||
		reasons[0] != rq5RuntimeIdentityChangedReason {
		t.Fatalf("changed runtime identity was not rejected: %v", reasons)
	}
	second.Status = "fail"
	if reasons := validateRQ5RuntimeIdentityConsistency([]Sample{first, second}, expectedManifest); len(reasons) != 0 {
		t.Fatalf("retained failed sample incorrectly set the passing identity lock: %v", reasons)
	}
}

func TestRQ5FinalizerBindsSamplesToSealedDriverBuildManifest(t *testing.T) {
	runDir := t.TempDir()
	manifestPath := filepath.Join(runDir, "rq5-driver-build.json")
	if err := os.WriteFile(manifestPath, []byte("sealed RQ5 driver build manifest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := loadRQ5DriverBuildManifestSHA256(runDir)
	if err != nil {
		t.Fatal(err)
	}
	sample := validRQ5SampleForTest(1, rq5fixture.BuildMode)
	sample.RQ5Verification.BuildManifestSHA256 = expected
	if reasons := validateRQ5RuntimeIdentityConsistency([]Sample{sample}, expected); len(reasons) != 0 {
		t.Fatalf("sample bound to sealed manifest was rejected: %v", reasons)
	}
	sample.RQ5Verification.BuildManifestSHA256 = rq5TestDigest(999)
	if reasons := validateRQ5RuntimeIdentityConsistency([]Sample{sample}, expected); len(reasons) != 1 ||
		reasons[0] != rq5BuildManifestMismatchReason {
		t.Fatalf("sample/sidecar manifest mismatch was not rejected: %v", reasons)
	}
}
