package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/parquet-go/parquet-go"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/releasedartifact"
	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

func (environment *finalV5CycleEnvironment) verifyQuery(ctx context.Context,
	response finalV5QueryResponse, task control.Task, binding finalV5PublicationRuntimeBinding,
	before, after finalV5RootSnapshot, businessDelta int64, availableMS float64,
	started time.Time) (experiment.RQ5QueryEvidence, error) {
	var evidence experiment.RQ5QueryEvidence
	var receipt queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(response.Receipt, &receipt); err != nil {
		return evidence, fmt.Errorf("decode RQ5 V8 receipt: %w", err)
	}
	if !queryreceipt.VersionAtLeast(receipt.Version, queryreceipt.VersionV8) || receipt.TaskID != task.ID || receipt.QueryID != response.QueryID ||
		receipt.ArtifactIntent == nil || receipt.Exposure == nil || receipt.CatalogDigest != binding.evidence.CatalogSHA256 ||
		receipt.CatalogVersion != task.CatalogVersion || receipt.Exposure.RootTaskID != task.RootTaskID {
		return evidence, errors.New("RQ5 query receipt is not a V8 attestation for the active Catalog-bound task")
	}
	if response.SemanticReplay {
		if businessDelta != 0 || before.ledgerSHA256() != after.ledgerSHA256() || before.Epoch != after.Epoch {
			return evidence, errors.New("RQ5 semantic replay executed Business SQL or changed its root ledger")
		}
	} else if businessDelta <= 0 || after.Epoch <= before.Epoch {
		return evidence, errors.New("RQ5 novel Catalog query did not cross the real Business connector and advance its root")
	}

	// deliver_result records the AVAILABLE consumption event. The evaluation
	// verifier reads the authenticated plaintext from the same Manager rather
	// than dereferencing the public URL, so no HTTP server or synthetic object
	// transport is introduced into this in-process production Service cycle.
	activeRuntime, err := activeFinalV5Runtime(environment.slot)
	if err != nil || activeRuntime.service == nil {
		return evidence, errors.New("RQ5 delivery has no active single-slot Service")
	}
	delivery, err := callTool(ctx, activeRuntime.service, environment.principal,
		"deliver_result", map[string]any{"result_id": response.ResultID, "format": "parquet"})
	if err != nil {
		return evidence, err
	}
	if artifactSHA, ok := delivery["artifact_sha256"].(string); !ok ||
		artifactSHA != receipt.ArtifactIntent.ParquetSHA256 {
		return evidence, errors.New("RQ5 delivery metadata differs from the signed artifact intent")
	}

	storedReceipt, err := environment.store.GetQueryReceipt(ctx, response.QueryID)
	if err != nil || storedReceipt.Receipt == nil || storedReceipt.Artifact == nil ||
		storedReceipt.ArtifactRegistrationAudit == nil {
		return evidence, errors.New("RQ5 Control projection omitted receipt, artifact, or registration audit")
	}
	if finalV5Hash(storedReceipt.Receipt.ReceiptJSON) != storedReceipt.Receipt.ReceiptSHA256 {
		return evidence, errors.New("RQ5 persisted receipt bytes differ from their Control digest")
	}
	var persisted queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(storedReceipt.Receipt.ReceiptJSON, &persisted); err != nil ||
		finalV5ReceiptSHA256(persisted) != finalV5ReceiptSHA256(receipt) {
		return evidence, errors.New("RQ5 response receipt differs from the co-committed Control receipt")
	}
	events, err := environment.store.ListAuditEventsForQuery(ctx, response.QueryID)
	if err != nil {
		return evidence, err
	}
	var availability *control.AuditEvent
	for index := range events {
		if events[index].EventType == "QUERY_RESULT_CONSUMED" {
			if availability != nil {
				return evidence, errors.New("RQ5 query has more than one availability audit")
			}
			value := events[index]
			availability = &value
		}
	}
	if availability == nil {
		return evidence, errors.New("RQ5 AVAILABLE artifact lacks a consumption audit")
	}
	receiptProof, err := finalV5AuditProof(ctx, environment.store, storedReceipt.Audit.Sequence)
	if err != nil {
		return evidence, err
	}
	registrationProof, err := finalV5AuditProof(ctx, environment.store,
		storedReceipt.ArtifactRegistrationAudit.Sequence)
	if err != nil {
		return evidence, err
	}
	availabilityProof, err := finalV5AuditProof(ctx, environment.store, availability.Sequence)
	if err != nil {
		return evidence, err
	}
	bindingProjection, err := loadFinalV5ExpectedBinding(ctx, environment.store,
		response.QueryID, task.ID, response.ResultID)
	if err != nil {
		return evidence, err
	}
	artifact := *storedReceipt.Artifact
	reference := resultartifact.ArtifactRef{ResultID: artifact.ResultID, TaskID: artifact.TaskID,
		ObjectKey: artifact.ObjectKey, KeyID: artifact.KeyID, ParquetSHA256: artifact.ParquetSHA256,
		ObjectSHA256: artifact.ObjectSHA256, ParquetSize: artifact.ParquetSize, ObjectSize: artifact.ObjectSize}
	parquetBytes, err := environment.manager.ReadParquet(ctx, reference, 64<<20)
	if err != nil {
		return evidence, fmt.Errorf("read authenticated RQ5 Parquet: %w", err)
	}
	ciphertext, err := environment.backend.Get(ctx, artifact.ObjectKey)
	if err != nil {
		return evidence, err
	}
	defer ciphertext.Close()
	object := releasedartifact.CanonicalObjectEvidence{ResultID: artifact.ResultID, QueryID: artifact.QueryID,
		TaskID: artifact.TaskID, KeyID: artifact.KeyID, Format: artifact.Format, Encryption: artifact.Encryption,
		StagingKey: artifact.StagingKey, ObjectKey: artifact.ObjectKey, ParquetSHA256: artifact.ParquetSHA256,
		ObjectSHA256: artifact.ObjectSHA256, ParquetSize: artifact.ParquetSize, ObjectSize: artifact.ObjectSize,
		RowCount: artifact.RowCount, ColumnCount: int64(artifact.ColumnCount), SchemaJSON: artifact.SchemaJSON,
		ResultMetadataJSON: artifact.ResultMetadataJSON, ACLJSON: artifact.ACLJSON, ExpiresAt: artifact.ExpiresAt,
		Status: string(artifact.Status), Ciphertext: ciphertext, ReleasedParquet: parquetBytes}
	transcript, err := releasedartifact.VerifyReleasedArtifactWithTranscript(environment.verifier,
		releasedartifact.SettlementEvidence{Receipt: receipt, ExpectedBinding: bindingProjection,
			ReceiptInclusion: receiptProof, TerminalInclusion: receiptProof,
			RegistrationInclusion: registrationProof, AvailabilityInclusion: &availabilityProof}, object)
	if err != nil || !transcript.Passed {
		return evidence, fmt.Errorf("composite RQ5 V8/Control/MinIO/Parquet verification failed: %w", err)
	}
	rows, err := finalV5ParseParquet(parquetBytes, artifact.ResultID, artifact.RowCount)
	if err != nil {
		return evidence, err
	}
	resultSHA, err := experiment.CanonicalResultHash(rows)
	if err != nil || resultSHA != binding.evidence.DirectResultSHA256 {
		return evidence, errors.New("released RQ5 Parquet differs from the direct frozen-publication oracle")
	}
	if response.RowCount != artifact.RowCount || response.ColumnCount != artifact.ColumnCount ||
		artifact.Status != control.ResultArtifactAvailable || receipt.ArtifactIntent.RowCount != artifact.RowCount ||
		receipt.ArtifactIntent.ColumnCount != int64(artifact.ColumnCount) {
		return evidence, errors.New("RQ5 response, receipt, and AVAILABLE Control artifact dimensions differ")
	}
	if response.PipelineMS == nil || response.DiagnosticMS == nil || !sha256Regexp.MatchString(response.PlanDigest) ||
		!sha256Regexp.MatchString(receipt.SQLFingerprint) {
		return evidence, errors.New("RQ5 production response omitted pipeline, diagnostic, SQL, or plan evidence")
	}
	availabilityBytes, _ := json.Marshal(*availability)
	receiptSHA := finalV5ReceiptSHA256(receipt)
	exposure := receipt.Exposure
	manifest := &experiment.RedactedVerifierManifest{VerifierVersion: "taskgate-final-v5-composite-verifier-v1",
		QueryIDHash:    finalV5IdentityHash(environment.request, "query", response.QueryID),
		ResultIDHash:   finalV5IdentityHash(environment.request, "result", response.ResultID),
		RootTaskIDHash: finalV5IdentityHash(environment.request, "task", exposure.RootTaskID),
		ReceiptSHA256:  receiptSHA, ObservationSHA256: bindingProjection.ObservationSHA256,
		ReleaseSetSHA256:          bindingProjection.ReleaseSetSHA256,
		DependencySetSHA256:       bindingProjection.InfluenceSetSHA256,
		OutcomeSetSHA256:          bindingProjection.OutcomeSetSHA256,
		ArtifactIntentSHA256:      receipt.ArtifactIntent.IntentSHA256,
		ObjectKeySHA256:           receipt.ArtifactIntent.ObjectKeySHA256,
		CanonicalCiphertextSHA256: transcript.CiphertextSHA256,
		CanonicalCiphertextSize:   transcript.CiphertextSize,
		ReleasedParquetSHA256:     transcript.ReleasedParquetSHA256,
		ReleasedParquetSize:       transcript.ReleasedParquetSize, SchemaSHA256: transcript.ReleasedSchemaSHA256,
		TerminalAuditSequence:     transcript.TerminalAuditSequence,
		RegistrationAuditSequence: transcript.RegistrationAuditSequence,
		AvailabilityAuditSequence: transcript.AvailabilityAuditSequence, VerificationResult: "pass"}
	evidence = experiment.RQ5QueryEvidence{Day: binding.evidence.Day,
		CatalogSHA256:     binding.evidence.CatalogSHA256,
		PublicationSHA256: binding.evidence.PublicationManifestSHA256,
		TaskIDHash:        finalV5IdentityHash(environment.request, "task", task.ID),
		RootTaskIDHash:    finalV5IdentityHash(environment.request, "task", exposure.RootTaskID),
		RequestIDHash:     finalV5IdentityHash(environment.request, "request", receipt.RequestID),
		QueryIDHash:       manifest.QueryIDHash, ResultIDHash: manifest.ResultIDHash, ResultSHA256: resultSHA,
		RowCount: artifact.RowCount, ColumnCount: artifact.ColumnCount, ClientAvailableMS: availableMS,
		ClientFullDrainMS: finalV5DurationMS(time.Since(started)), PipelineMS: cloneFinalV5FloatMap(response.PipelineMS),
		DiagnosticMS: cloneFinalV5FloatMap(response.DiagnosticMS), PhysicalSQLSHA256: receipt.SQLFingerprint,
		LogicalSQLSHA256: finalV5Hash([]byte("daily_lineitem(l_orderkey,l_linenumber,l_extendedprice);l_orderkey=1;order=l_linenumber;limit=10")),
		QueryPlanSHA256:  response.PlanDigest, ActualReleaseFacts: exposure.ActualReleaseFacts,
		ChargedReleaseFacts: exposure.ChargedReleaseFacts, ActualDependencyFacts: exposure.ActualInfluenceFacts,
		ChargedDependencyFacts: exposure.ChargedInfluenceFacts, ActualOutcomeFacts: exposure.ActualOutcomeFacts,
		ChargedOutcomeFacts: exposure.ChargedOutcomeFacts, PredicateAtomCount: exposure.ActualPredicateAtomCount,
		CompositeCount: exposure.ActualCompositeCount, SemanticReplay: response.SemanticReplay,
		BusinessSQLDelta: businessDelta, RootEpochBefore: before.Epoch, RootEpochAfter: after.Epoch,
		RootSetSHA256Before: before.setSHA256(), RootSetSHA256After: after.setSHA256(),
		ParquetBytes: artifact.ParquetSize, EncryptedObjectBytes: artifact.ObjectSize,
		ReceiptVersion: receipt.Version, ReceiptSHA256: receiptSHA,
		ArtifactIntentSHA256:    receipt.ArtifactIntent.IntentSHA256,
		AvailabilityAuditSHA256: finalV5Hash(availabilityBytes), ReceiptVerified: true,
		ArtifactAvailable: true, VerifierManifest: manifest}
	return evidence, nil
}

func finalV5ReceiptSHA256(receipt queryreceipt.QueryReceiptV1) string {
	encoded, _ := json.Marshal(receipt)
	return finalV5Hash(encoded)
}

func finalV5AuditProof(ctx context.Context, store *control.Store,
	sequence int64) (auditchain.InclusionProof, error) {
	event, err := store.GetAuditEvent(ctx, sequence)
	if err != nil {
		return auditchain.InclusionProof{}, err
	}
	checkpoint, err := store.AuditCheckpoint(ctx)
	if err != nil {
		return auditchain.InclusionProof{}, err
	}
	proof := auditchain.InclusionProof{TerminalEvent: event, Checkpoint: checkpoint}
	if sequence > 1 {
		predecessor, err := store.GetAuditEvent(ctx, sequence-1)
		if err != nil {
			return proof, err
		}
		proof.PredecessorEvent = &predecessor
	}
	proof.SuccessorEvents, err = store.ListAuditEventsRange(ctx, sequence, checkpoint.Sequence)
	if err != nil {
		return proof, err
	}
	if err := auditchain.VerifyInclusion(proof); err != nil {
		return proof, err
	}
	return proof, nil
}

func loadFinalV5ExpectedBinding(ctx context.Context, store *control.Store,
	queryID, taskID, resultID string) (releasedartifact.ExpectedBinding, error) {
	var binding releasedartifact.ExpectedBinding
	err := store.DB().QueryRowContext(ctx, `
SELECT q.task_id,q.id,a.result_id,
 q.manifest_digest,q.grant_digest,q.catalog_digest,q.catalog_version,q.datasource_id,q.schema_digest,
 reservation.root_task_id,reservation.profile_version,head.predicate_profile_version,
 reservation.observation_sha256,observation.dictionary_set_digest,
 observation.release_set_sha256,observation.influence_set_sha256,observation.outcome_set_sha256,
 reservation.predicate_context_sha256,reservation.predicate_set_sha256,reservation.composite_outcome_sha256,
 reservation.actual_release_facts,reservation.actual_influence_facts,reservation.actual_outcome_facts,
 reservation.charged_release_facts,reservation.charged_influence_facts,reservation.charged_outcome_facts,
 reservation.actual_predicate_atom_count,reservation.charged_predicate_atom_count,reservation.root_epoch
FROM query_records q
JOIN v5_query_exposure_reservations reservation ON reservation.query_id=q.id AND reservation.status='SETTLED'
JOIN v5_observations observation ON observation.observation_sha256=reservation.observation_sha256
JOIN v5_exposure_root_heads head ON head.root_task_id=reservation.root_task_id
JOIN result_artifacts a ON a.query_id=q.id AND a.status='AVAILABLE'
WHERE q.id=$1 AND q.task_id=$2 AND a.result_id=$3`, queryID, taskID, resultID).Scan(
		&binding.TaskID, &binding.QueryID, &binding.ResultID, &binding.ManifestDigest, &binding.GrantDigest,
		&binding.CatalogDigest, &binding.CatalogVersion, &binding.DatasourceID, &binding.SchemaDigest,
		&binding.RootTaskID, &binding.ProfileVersion, &binding.PredicateProfileVersion,
		&binding.ObservationSHA256, &binding.DictionarySetSHA256, &binding.ReleaseSetSHA256,
		&binding.InfluenceSetSHA256, &binding.OutcomeSetSHA256, &binding.PredicateContextSHA256,
		&binding.PredicateSetSHA256, &binding.CompositeOutcomeSHA256, &binding.ActualReleaseFacts,
		&binding.ActualInfluenceFacts, &binding.ActualOutcomeFacts, &binding.ChargedReleaseFacts,
		&binding.ChargedInfluenceFacts, &binding.ChargedOutcomeFacts, &binding.ActualPredicateAtomCount,
		&binding.ChargedPredicateAtomCount, &binding.RootEpoch)
	return binding, err
}

func finalV5ParseParquet(value []byte, resultID string, expectedRows int64) ([][]any, error) {
	reader := parquet.NewReader(bytes.NewReader(value))
	defer reader.Close()
	storedSchema, ok := reader.File().Lookup("taskgate.schema")
	if !ok || reader.NumRows() != expectedRows {
		return nil, errors.New("RQ5 released Parquet schema or row count is invalid")
	}
	var schema []resultartifact.ColumnSchema
	if err := json.Unmarshal([]byte(storedSchema), &schema); err != nil {
		return nil, err
	}
	return resultartifact.ReadParquet(value, resultID, schema, 0, expectedRows)
}

func cloneFinalV5FloatMap(values map[string]float64) map[string]float64 {
	result := make(map[string]float64, len(values))
	for name, value := range values {
		result[name] = value
	}
	return result
}
