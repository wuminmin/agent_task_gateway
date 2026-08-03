package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

type finalV5PublicationRuntimeBinding struct {
	publication loadedPublication
	catalogSHA  string
	oracle      directOracle
	evidence    experiment.RQ5PublicationEvidence
}

type finalV5CycleEnvironment struct {
	request      finalV5RQ5DriverRequest
	options      finalV5CycleOptions
	store        *control.Store
	manager      *resultartifact.Manager
	backend      *resultartifact.S3Backend
	signer       *queryreceipt.Signer
	verifier     *queryreceipt.Verifier
	principal    mcp.Principal
	oa           *finalV5OAWorkflow
	publications map[string]finalV5PublicationRuntimeBinding
	slot         *singleGatewayServiceSlot
}

func runFinalV5Cycle(options finalV5CycleOptions) error {
	response := finalV5RQ5DriverResponse{SchemaVersion: 1, DriverVersion: finalV5RQ5DriverVersion, Status: "invalid"}
	if options.OutputPath == "" {
		return errors.New("final-v5-cycle -output is required")
	}
	request, err := loadFinalV5CycleRequest(options.RequestPath)
	if err != nil {
		response.ErrorCode = "rq5_cycle_request_invalid"
		return writeJSONAtomicExclusive(options.OutputPath, response)
	}
	response.Status = "fail"
	evidence, cycleErr := executeFinalV5Cycle(context.Background(), request, options)
	response.Evidence = evidence
	if cycleErr != nil {
		response.ErrorCode = "rq5_live_cycle_failed"
	} else {
		response.Status = "pass"
	}
	return writeJSONAtomicExclusive(options.OutputPath, response)
}

func loadFinalV5CycleRequest(path string) (finalV5RQ5DriverRequest, error) {
	var request finalV5RQ5DriverRequest
	if path == "" {
		return request, errors.New("request path is required")
	}
	if err := decodeJSONFileStrict(path, &request); err != nil {
		return request, err
	}
	cycle, err := rq5fixture.LookupCycle(request.Operation.Iteration)
	if err != nil || request.SchemaVersion != 1 || request.DriverVersion != finalV5RQ5DriverVersion ||
		request.FixtureSHA256 != rq5fixture.FixtureSHA256() || request.Operation.ExperimentID != "rq5" ||
		request.Operation.Mode != rq5fixture.BuildMode ||
		!rq5fixture.IsCell(request.Operation.WorkloadID, request.Operation.Scale, request.Operation.Mode, request.Operation.Iteration) ||
		request.CycleIndex != cycle.Index || request.FromDay != cycle.From || request.ToDay != cycle.To ||
		!sha256Regexp.MatchString(request.BuildManifestSHA256) ||
		!sha256Regexp.MatchString(request.GeneratorSHA256) || !sha256Regexp.MatchString(request.ConfigSHA256) ||
		!imageIDRegexp.MatchString(request.PhaseImageID) || !imageIDRegexp.MatchString(request.OnlineImageID) ||
		!imageIDRegexp.MatchString(request.OAImageID) || request.OnlineImageID != request.OAImageID ||
		!sha256Regexp.MatchString(request.PhaseBinarySHA256) ||
		!sha256Regexp.MatchString(request.OnlineBinarySHA256) || !sha256Regexp.MatchString(request.OABinarySHA256) ||
		request.PhaseBinaryMTime == nil || request.OnlineBinaryMTime == nil || request.OABinaryMTime == nil ||
		*request.PhaseBinaryMTime != 0 || *request.OnlineBinaryMTime != 0 || *request.OABinaryMTime != 0 {
		return request, errors.New("request differs from the frozen RQ5 cycle")
	}
	return request, nil
}

func executeFinalV5Cycle(ctx context.Context, request finalV5RQ5DriverRequest,
	options finalV5CycleOptions) (*experiment.RQ5VerificationEvidence, error) {
	for _, source := range []struct{ path, name string }{
		{options.InputDirectory, "-input-dir"}, {options.FixtureArtifactDirectory, "-fixture-artifact-dir"},
		{options.TargetArtifactDirectory, "-target-artifact-dir"}, {options.PhaseDirectory, "-phase-dir"},
	} {
		if err := requireDirectory(source.path, source.name); err != nil {
			return nil, err
		}
	}
	for _, path := range []string{options.DatasetManifestPath, options.GeneratorPath, options.ConfigPath} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("bound RQ5 source %s must be a regular non-symlink file", path)
		}
	}
	_, rows, err := loadDatasetManifest(options.DatasetManifestPath)
	if err != nil || rows != rq5fixture.RowsPerPublication {
		return nil, errors.New("live RQ5 dataset is not the frozen 345000-row four-publication fixture")
	}
	evidence := &experiment.RQ5VerificationEvidence{
		Version: rq5fixture.Version, FixtureSHA256: rq5fixture.FixtureSHA256(),
		BuildManifestSHA256: request.BuildManifestSHA256, PhaseImageID: request.PhaseImageID,
		OnlineImageID: request.OnlineImageID, OAImageID: request.OAImageID,
		PhaseBinarySHA256: request.PhaseBinarySHA256, OnlineBinarySHA256: request.OnlineBinarySHA256,
		OABinarySHA256: request.OABinarySHA256, PhaseBinaryMTimeUnix: *request.PhaseBinaryMTime,
		OnlineBinaryMTimeUnix: *request.OnlineBinaryMTime, OABinaryMTimeUnix: *request.OABinaryMTime,
		RowsPerPublication: rows, CycleIndex: request.CycleIndex, FromDay: request.FromDay, ToDay: request.ToDay,
	}
	evidence.DatasetManifestSHA256, err = fileSHA256(options.DatasetManifestPath)
	if err != nil {
		return evidence, err
	}
	evidence.GeneratorSHA256, err = fileSHA256(options.GeneratorPath)
	if err != nil {
		return evidence, err
	}
	evidence.ConfigSHA256, err = fileSHA256(options.ConfigPath)
	if err != nil {
		return evidence, err
	}
	if evidence.GeneratorSHA256 != request.GeneratorSHA256 || evidence.ConfigSHA256 != request.ConfigSHA256 {
		return evidence, errors.New("cycle-mounted runtime sources differ from the build-manifest bindings")
	}
	evidence.Build, err = loadFinalV5BuildEvidence(options.PhaseDirectory, request)
	if err != nil {
		return evidence, err
	}

	dataKey, keyErr := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("RQ5_RESULT_DATA_KEY")))
	if keyErr != nil || len(dataKey) != 32 {
		return evidence, errors.New("deployment-generated RQ5 result data key is absent or invalid")
	}
	cipher, err := control.NewAES256GCMWithKeyID("rq5-sequential-artifact-aes-v1", dataKey)
	if err != nil {
		return evidence, err
	}
	store, err := control.Open(ctx, strings.TrimSpace(os.Getenv("CONTROL_POSTGRES_DSN")), cipher)
	if err != nil {
		return evidence, fmt.Errorf("open isolated RQ5 Control PostgreSQL: %w", err)
	}
	defer store.Close()
	backend, err := resultartifact.NewS3Backend(resultartifact.S3Config{
		Endpoint: strings.TrimSpace(os.Getenv("RQ5_RESULT_OBJECT_ENDPOINT")), Region: "us-east-1",
		Bucket:    strings.TrimSpace(os.Getenv("RQ5_RESULT_OBJECT_BUCKET")),
		AccessKey: strings.TrimSpace(os.Getenv("RQ5_RESULT_OBJECT_ACCESS_KEY")),
		SecretKey: strings.TrimSpace(os.Getenv("RQ5_RESULT_OBJECT_SECRET_KEY")), ForcePathStyle: true,
	})
	if err != nil {
		return evidence, err
	}
	manager, err := resultartifact.NewManager(backend, cipher, filepath.Join(os.TempDir(), "rq5-result-artifacts"))
	if err != nil {
		return evidence, err
	}
	if err := manager.Ready(ctx); err != nil {
		return evidence, fmt.Errorf("RQ5 MinIO result manager is not ready: %w", err)
	}
	signer, err := queryreceipt.NewSignerFromBase64(strings.TrimSpace(os.Getenv("RQ5_RECEIPT_KEY_ID")),
		strings.TrimSpace(os.Getenv("RQ5_RECEIPT_PRIVATE_KEY")))
	if err != nil {
		return evidence, fmt.Errorf("initialize dedicated RQ5 V8 signer: %w", err)
	}
	verifier, err := queryreceipt.NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		return evidence, err
	}
	principal := mcp.Principal{ID: "rq5-final-v5-principal-alice", Subject: "alice", Role: "query"}
	if existing, getErr := store.GetPrincipal(ctx, principal.ID); getErr != nil {
		if err := store.CreatePrincipal(ctx, control.Principal{ID: principal.ID, Subject: principal.Subject,
			Role: principal.Role, CreatedAt: time.Now().UTC()}); err != nil {
			return evidence, err
		}
	} else if existing.Subject != principal.Subject || existing.Role != principal.Role || existing.DisabledAt != nil {
		return evidence, errors.New("persisted RQ5 principal identity changed")
	}
	oaBaseURL, err := finalV5RequiredEnvironment("RQ5_OA_BASE_URL", 1)
	if err != nil {
		return evidence, err
	}
	oaServiceToken, err := finalV5RequiredEnvironment("RQ5_OA_SERVICE_TOKEN", 32)
	if err != nil {
		return evidence, err
	}
	callbackSecret, err := finalV5RequiredEnvironment("RQ5_OA_CALLBACK_SECRET", 32)
	if err != nil {
		return evidence, err
	}
	oaReceiptKeyID, err := finalV5RequiredEnvironment("RQ5_OA_RECEIPT_KEY_ID", 1)
	if err != nil {
		return evidence, err
	}
	oaReceiptPublicKey, err := finalV5RequiredEnvironment("RQ5_OA_RECEIPT_PUBLIC_KEY", 1)
	if err != nil {
		return evidence, err
	}
	alicePassword, err := finalV5RequiredEnvironment("RQ5_OA_ALICE_PASSWORD", 32)
	if err != nil {
		return evidence, err
	}
	bobPassword, err := finalV5RequiredEnvironment("RQ5_OA_BOB_PASSWORD", 32)
	if err != nil {
		return evidence, err
	}
	callbackListenAddress, err := finalV5RequiredEnvironment("RQ5_CALLBACK_LISTEN_ADDR", 1)
	if err != nil {
		return evidence, err
	}
	deliverySigningKey, err := finalV5RequiredEnvironment("RQ5_DELIVERY_SIGNING_KEY", 32)
	if err != nil {
		return evidence, err
	}
	approvalClient, err := approval.NewClient(oaBaseURL, oaServiceToken, nil)
	if err != nil {
		return evidence, fmt.Errorf("initialize production RQ5 approval.Client: %w", err)
	}
	oaReceiptVerifier, err := approval.NewReceiptVerifierFromBase64(oaReceiptKeyID, oaReceiptPublicKey)
	if err != nil {
		return evidence, fmt.Errorf("initialize deployment RQ5 OA receipt verifier: %w", err)
	}
	oaWorkflow, err := newFinalV5OAWorkflow(ctx, oaBaseURL, alicePassword, bobPassword)
	if err != nil {
		return evidence, err
	}

	businessDSNs := make(map[string]string, len(days))
	artifactRoots := make(map[string]string, len(days))
	for _, day := range days {
		businessDSNs[day] = strings.TrimSpace(os.Getenv("SNAPSHOT_POSTGRES_DSN_" + strings.ToUpper(day)))
		artifactRoots[day] = options.FixtureArtifactDirectory
		if businessDSNs[day] == "" {
			return evidence, fmt.Errorf("Business DSN for %s is absent", day)
		}
	}
	artifactRoots[request.ToDay] = options.TargetArtifactDirectory
	bindings, publicationEvidence, err := loadFinalV5PublicationSet(ctx, options.InputDirectory,
		artifactRoots, businessDSNs)
	if err != nil {
		return evidence, err
	}
	evidence.Publications = publicationEvidence
	evidence.PublicationSetSHA256 = experiment.RQ5PublicationSetSHA256(evidence.Publications)
	newPublication := bindings[request.ToDay].evidence
	if evidence.Build.Day != request.ToDay || evidence.Build.PublicationManifestSHA256 != newPublication.PublicationManifestSHA256 ||
		evidence.Build.DictionarySHA256 != newPublication.DictionarySHA256 || evidence.Build.ArtifactBytes != newPublication.ArtifactBytes ||
		evidence.Build.HOTArtifactBytes != newPublication.HOTArtifactBytes {
		return evidence, errors.New("current measured target phase reports differ from activated target artifacts")
	}

	factory := &finalV5RuntimeFactory{store: store, approval: approvalClient, principal: principal,
		inputDir: options.InputDirectory, artifactRoots: artifactRoots, businessDSNs: businessDSNs,
		manager: manager, signer: signer, logger: discardLogger(), callbackSecret: callbackSecret,
		receiptVerifier: oaReceiptVerifier, callbackListenAddress: callbackListenAddress,
		deliverySigningKey: []byte(deliverySigningKey)}
	slot, err := newSingleGatewayServiceSlot(factory)
	if err != nil {
		return evidence, err
	}
	environment := &finalV5CycleEnvironment{request: request, options: options, store: store,
		manager: manager, backend: backend, signer: signer, verifier: verifier, principal: principal,
		oa: oaWorkflow, publications: bindings, slot: slot}
	evidence.Route, err = environment.executeRoute(ctx)
	evidence.Topology, evidence.Lifecycle = slot.Evidence()
	evidence.LifecycleSHA256 = experiment.RQ5LifecycleSHA256(evidence.Lifecycle)
	if err != nil {
		return evidence, err
	}
	probeOperation := request.Operation
	probeOperation.Mode = rq5fixture.BuildMode
	sample := rq5EvidenceSample(probeOperation, evidence)
	if err := experiment.ValidateRQ5Evidence(sample); err != nil {
		return evidence, fmt.Errorf("strict live RQ5 evidence validation: %w", err)
	}
	return evidence, nil
}

func finalV5RequiredEnvironment(name string, minimumLength int) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if len(value) < minimumLength {
		return "", fmt.Errorf("deployment-bound %s is absent or invalid", name)
	}
	return value, nil
}

func rq5EvidenceSample(operation experiment.AdapterOperation, evidence *experiment.RQ5VerificationEvidence) experiment.Sample {
	selected := evidence.Route.NewInitial
	return experiment.Sample{SchemaVersion: 1, CampaignID: operation.CampaignID, DeploymentID: operation.DeploymentID,
		ExperimentID: operation.ExperimentID, CellID: operation.CellID, SampleID: operation.SampleID,
		Iteration: operation.Iteration, ProcessReplicate: operation.ProcessReplicate, Warmup: operation.Warmup,
		OrderPosition: operation.OrderPosition,
		RandomSeed:    operation.RandomSeed, PairID: operation.PairID, PairedSystemOrder: operation.PairedSystemOrder,
		RootGroupID: operation.RootGroupID, System: "taskgate", Mode: operation.Mode, WorkloadID: operation.WorkloadID,
		Scale: operation.Scale, Status: "pass", ClientAvailableMS: evidence.Build.CycleWallMS,
		ClientFullDrainMS: evidence.Build.CycleWallMS, PipelineMS: selected.PipelineMS,
		DiagnosticMS: selected.DiagnosticMS, RowCount: selected.RowCount, ColumnCount: selected.ColumnCount,
		ResultSHA256: selected.ResultSHA256, PhysicalSQLSHA256: selected.PhysicalSQLSHA256,
		LogicalSQLSHA256: selected.LogicalSQLSHA256, QueryPlanSHA256: selected.QueryPlanSHA256,
		ActualReleaseFacts: selected.ActualReleaseFacts, ChargedReleaseFacts: selected.ChargedReleaseFacts,
		ActualDependencyFacts: selected.ActualDependencyFacts, ChargedDependencyFacts: selected.ChargedDependencyFacts,
		ActualOutcomeFacts: selected.ActualOutcomeFacts, ChargedOutcomeFacts: selected.ChargedOutcomeFacts,
		PredicateAtomCount: selected.PredicateAtomCount, CompositeCount: selected.CompositeCount,
		BusinessSQLDelta: selected.BusinessSQLDelta, RootEpochBefore: selected.RootEpochBefore,
		RootEpochAfter: selected.RootEpochAfter, RootSetSHA256Before: selected.RootSetSHA256Before,
		RootSetSHA256After: selected.RootSetSHA256After, RootTaskIDHash: selected.RootTaskIDHash,
		ParquetBytes: selected.ParquetBytes, EncryptedObjectBytes: selected.EncryptedObjectBytes,
		ReceiptVersion: selected.ReceiptVersion, ReceiptSHA256: selected.ReceiptSHA256,
		ArtifactIntentSHA256: selected.ArtifactIntentSHA256, AvailabilityAuditSHA256: selected.AvailabilityAuditSHA256,
		ReceiptVerified: selected.ReceiptVerified, ArtifactAvailable: selected.ArtifactAvailable,
		ReleaseSetSHA256:    selected.VerifierManifest.ReleaseSetSHA256,
		DependencySetSHA256: selected.VerifierManifest.DependencySetSHA256,
		OutcomeSetSHA256:    selected.VerifierManifest.OutcomeSetSHA256,
		ArtifactSHA256:      selected.VerifierManifest.ReleasedParquetSHA256,
		ObjectSHA256:        selected.VerifierManifest.CanonicalCiphertextSHA256,
		RQ5Verification:     evidence}
}

func finalV5Hash(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func finalV5IdentityHash(request finalV5RQ5DriverRequest, kind, value string) string {
	return finalV5Hash([]byte("TASKGATE-FINAL-V5-RQ5-IDENTITY-V1\x00" + request.Operation.CampaignID + "\x00" +
		request.Operation.DeploymentID + "\x00" + request.Operation.SampleID + "\x00" + kind + "\x00" + value))
}
