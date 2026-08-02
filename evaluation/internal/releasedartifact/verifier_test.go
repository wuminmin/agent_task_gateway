package releasedartifact

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

func TestVerifyReleasedArtifactComposesReceiptAuditAndObjectEvidence(t *testing.T) {
	fixture := newReleaseFixture(t, "one")
	if err := VerifyReleasedArtifact(fixture.verifier, fixture.settlement, fixture.object()); err != nil {
		t.Fatalf("VerifyReleasedArtifact: %v", err)
	}
}

func TestVerifyReleasedArtifactRejectsCorrectReceiptWithWrongObject(t *testing.T) {
	fixture := newReleaseFixture(t, "one")
	wrong := fixture.object()
	wrong.Ciphertext = bytes.NewReader([]byte("wrong canonical ciphertext"))
	if err := VerifyReleasedArtifact(fixture.verifier, fixture.settlement, wrong); !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("wrong canonical object error = %v, want %v", err, ErrInvalidRelease)
	}
}

func TestVerifyReleasedArtifactRejectsCorrectObjectWithAnotherQueryReceipt(t *testing.T) {
	first := newReleaseFixture(t, "one")
	second := newReleaseFixture(t, "two")
	if err := VerifyReleasedArtifact(second.verifier, second.settlement, first.object()); !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("cross-query receipt/object error = %v, want %v", err, ErrInvalidRelease)
	}
}

func TestVerifyReleasedArtifactRejectsMissingAvailabilityEvent(t *testing.T) {
	fixture := newReleaseFixture(t, "one")
	fixture.settlement.AvailabilityInclusion = nil
	if err := VerifyReleasedArtifact(fixture.verifier, fixture.settlement, fixture.object()); !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("missing availability error = %v, want %v", err, ErrInvalidRelease)
	}
}

func TestVerifyReleasedArtifactRejectsIndependentCatalogBindingMismatch(t *testing.T) {
	fixture := newReleaseFixture(t, "one")
	fixture.settlement.ExpectedBinding.CatalogDigest = shaHex([]byte("another catalog"))
	if err := VerifyReleasedArtifact(fixture.verifier, fixture.settlement, fixture.object()); !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("Catalog binding error = %v, want %v", err, ErrInvalidRelease)
	}
}

func TestVerifyReleasedArtifactRejectsExpiryProjectionMismatch(t *testing.T) {
	fixture := newReleaseFixture(t, "one")
	object := fixture.object()
	wrongExpiry := object.ExpiresAt.Add(time.Second)
	object.ExpiresAt = &wrongExpiry
	if err := VerifyReleasedArtifact(fixture.verifier, fixture.settlement, object); !errors.Is(err, ErrInvalidRelease) {
		t.Fatalf("expiry projection error = %v, want %v", err, ErrInvalidRelease)
	}
}

type releaseFixture struct {
	verifier     *queryreceipt.Verifier
	settlement   SettlementEvidence
	objectKey    string
	stagingKey   string
	ciphertext   []byte
	parquet      []byte
	schemaJSON   []byte
	metadataJSON []byte
	aclJSON      []byte
}

func (fixture releaseFixture) object() CanonicalObjectEvidence {
	receipt := fixture.settlement.Receipt
	intent := receipt.ArtifactIntent
	return CanonicalObjectEvidence{
		ResultID: intent.ResultID, QueryID: receipt.QueryID, TaskID: receipt.TaskID,
		KeyID: intent.KeyID, Format: intent.Format, Encryption: intent.Encryption,
		StagingKey: fixture.stagingKey, ObjectKey: fixture.objectKey,
		ParquetSHA256: intent.ParquetSHA256, ObjectSHA256: intent.ObjectSHA256,
		ParquetSize: intent.ParquetSize, ObjectSize: intent.ObjectSize,
		RowCount: intent.RowCount, ColumnCount: intent.ColumnCount,
		SchemaJSON:         append([]byte(nil), fixture.schemaJSON...),
		ResultMetadataJSON: append([]byte(nil), fixture.metadataJSON...),
		ACLJSON:            append([]byte(nil), fixture.aclJSON...),
		ExpiresAt:          intent.ExpiresAt, Status: "AVAILABLE",
		Ciphertext:      bytes.NewReader(fixture.ciphertext),
		ReleasedParquet: append([]byte(nil), fixture.parquet...),
	}
}

func newReleaseFixture(t *testing.T, suffix string) releaseFixture {
	t.Helper()
	queryID := "query-" + suffix
	taskID := "task-" + suffix
	resultID := "result-" + suffix
	objectKey := "results/" + taskID + "/" + resultID + ".parquet.enc"
	stagingKey := "staging/" + taskID + "/" + resultID + ".parquet.enc"

	var parquetBytes bytes.Buffer
	schema, err := resultartifact.WriteParquet(&parquetBytes, resultID,
		[]resultartifact.Column{{Name: "value", DataTypeOID: 23}}, [][]any{{int32(7)}})
	if err != nil {
		t.Fatal(err)
	}
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	normalizedSchemaJSON, err := normalizeJSON(schemaJSON)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := []byte("canonical-ciphertext-" + suffix)
	metadataJSON := []byte(`{"columns":["value"]}`)
	aclJSON := []byte(`{"principal":"alice"}`)
	created := time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC)
	expiresAt := created.Add(time.Hour)
	digest := shaHex([]byte("binding-" + suffix))

	terminal := auditchain.Event{
		Sequence: 1, EventID: "terminal-" + suffix, TaskID: taskID, QueryID: queryID,
		Actor: "gateway", EventType: "QUERY_COMPLETED", Payload: json.RawMessage(`{"status":"COMPLETED"}`),
		OccurredAt: created.Add(time.Millisecond), PreviousHash: auditchain.GenesisHash,
	}
	terminal.CurrentHash = auditHash(t, terminal.PreviousHash, terminal)

	intent := queryreceipt.ArtifactIntentEvidenceV1{
		Version: queryreceipt.ArtifactIntentVersionV1, ResultID: resultID,
		Format: resultartifact.FormatParquet, Encryption: resultartifact.EncryptionChunkedAESGCMV1,
		KeyID:         "artifact-key-" + suffix,
		ParquetSHA256: shaHex(parquetBytes.Bytes()), ObjectSHA256: shaHex(ciphertext),
		ParquetSize: int64(parquetBytes.Len()), ObjectSize: int64(len(ciphertext)),
		RowCount: 1, ColumnCount: int64(len(schema)), SchemaSHA256: shaHex(normalizedSchemaJSON),
		ResultMetadataSHA256: shaHex(metadataJSON),
		ACLSHA256:            shaHex(aclJSON),
		ObjectKeySHA256:      shaHex([]byte(objectKey)), StagingKeySHA256: shaHex([]byte(stagingKey)),
		ExpiresAt: &expiresAt,
		Status:    queryreceipt.ArtifactStatusPending,
	}
	registration := auditchain.Event{
		Sequence: 2, EventID: "registration-" + suffix, TaskID: taskID, QueryID: queryID,
		Actor: "gateway", EventType: "QUERY_RESULT_OBJECT_REGISTERED",
		Payload: registrationPayload(t, intent), OccurredAt: created.Add(2 * time.Millisecond),
		PreviousHash: terminal.CurrentHash,
	}
	registration.CurrentHash = auditHash(t, registration.PreviousHash, registration)
	intent.RegistrationAuditSequence = registration.Sequence
	intent.RegistrationPreviousAuditHash = registration.PreviousHash
	intent.RegistrationAuditHash = registration.CurrentHash
	intent, err = queryreceipt.BuildArtifactIntent(intent)
	if err != nil {
		t.Fatal(err)
	}

	signedAt := created.Add(3 * time.Millisecond)
	receipt := queryreceipt.QueryReceiptV1{
		Version: queryreceipt.VersionV8, ReceiptID: queryID, TaskID: taskID, QueryID: queryID,
		RequestID: "request-" + suffix, ManifestDigest: digest, GrantDigest: digest,
		CatalogDigest: digest, CatalogVersion: "catalog-v1", DatasourceID: "datasource-v1",
		SchemaDigest: digest, RequestDigest: digest, SQLFingerprint: "select-value-" + suffix,
		PolicyDecision: "ALLOW",
		BudgetBefore:   queryreceipt.BudgetStateV1{Limits: queryreceipt.BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100}},
		BudgetReserved: queryreceipt.BudgetVectorV1{Queries: 1, Rows: 5, DBMS: 50},
		BudgetCharged:  queryreceipt.BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		BudgetAfter: queryreceipt.BudgetStateV1{
			Limits: queryreceipt.BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100},
			Used:   queryreceipt.BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		},
		RowCount: 1, DatabaseMS: 2, ResultHash: intent.ParquetSHA256,
		Status: queryreceipt.StatusCompleted, CreatedAt: created, CompletedAt: terminal.OccurredAt,
		AuditSequence: terminal.Sequence, PreviousAuditHash: terminal.PreviousHash,
		AuditHash: terminal.CurrentHash, SignedAt: &signedAt,
		Exposure: &queryreceipt.ExposureEvidenceV1{
			RootTaskID: taskID, ProfileVersion: "taskgate-exposure-v5",
			ActualReleaseFacts: 1, ActualInfluenceFacts: 1, ActualOutcomeFacts: 2,
			ChargedReleaseFacts: 1, ChargedInfluenceFacts: 1, ChargedOutcomeFacts: 2,
			ObservationSHA256: digest, DictionarySetSHA256: digest,
			ReleaseSetSHA256: digest, InfluenceSetSHA256: digest, OutcomeSetSHA256: digest,
			RootEpoch: 1, PredicateProfileVersion: "taskgate-predicate-footprint-v1",
			PredicateContextSHA256: digest, PredicateSetSHA256: digest,
			ActualPredicateAtomCount: 1, ChargedPredicateAtomCount: 1,
			CompositeOutcomeSHA256: digest, ActualCompositeCount: 1, ChargedCompositeCount: 1,
		},
		ArtifactIntent: &intent,
	}
	signer, err := queryreceipt.NewSigner("test-key-"+suffix,
		ed25519.NewKeyFromSeed(bytes.Repeat([]byte{suffix[0]}, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err = signer.Sign(receipt)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := queryreceipt.NewVerifier(map[string]ed25519.PublicKey{signer.KeyID(): signer.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}

	availability := auditchain.Event{
		Sequence: 3, EventID: "availability-" + suffix, TaskID: taskID, QueryID: queryID,
		Actor: "gateway", EventType: "QUERY_RESULT_CONSUMED",
		Payload: mustJSON(t, map[string]any{
			"result_id": resultID, "result_sha256": intent.ParquetSHA256,
			"object_sha256": intent.ObjectSHA256, "format": intent.Format,
			"status": "AVAILABLE", "consumed_at": created.Add(4 * time.Millisecond).Format(time.RFC3339Nano),
		}),
		OccurredAt: created.Add(4 * time.Millisecond), PreviousHash: registration.CurrentHash,
	}
	availability.CurrentHash = auditHash(t, availability.PreviousHash, availability)
	terminalProof := auditchain.InclusionProof{
		TerminalEvent: terminal, SuccessorEvents: []auditchain.Event{registration, availability},
		Checkpoint: auditchain.Checkpoint{Sequence: availability.Sequence, Hash: availability.CurrentHash},
	}
	registrationProof := auditchain.InclusionProof{
		TerminalEvent: registration, PredecessorEvent: &terminal, SuccessorEvents: []auditchain.Event{availability},
		Checkpoint: auditchain.Checkpoint{Sequence: availability.Sequence, Hash: availability.CurrentHash},
	}
	availabilityProof := auditchain.InclusionProof{
		TerminalEvent: availability, PredecessorEvent: &registration,
		Checkpoint: auditchain.Checkpoint{Sequence: availability.Sequence, Hash: availability.CurrentHash},
	}
	return releaseFixture{
		verifier: verifier,
		settlement: SettlementEvidence{
			Receipt: receipt, ExpectedBinding: expectedBinding(receipt), ReceiptInclusion: terminalProof,
			TerminalInclusion: terminalProof, RegistrationInclusion: registrationProof,
			AvailabilityInclusion: &availabilityProof,
		},
		objectKey: objectKey, stagingKey: stagingKey, ciphertext: ciphertext,
		parquet: parquetBytes.Bytes(), schemaJSON: schemaJSON,
		metadataJSON: metadataJSON, aclJSON: aclJSON,
	}
}

func expectedBinding(receipt queryreceipt.QueryReceiptV1) ExpectedBinding {
	exposure := receipt.Exposure
	return ExpectedBinding{
		TaskID: receipt.TaskID, QueryID: receipt.QueryID, ResultID: receipt.ArtifactIntent.ResultID,
		ManifestDigest: receipt.ManifestDigest, GrantDigest: receipt.GrantDigest,
		CatalogDigest: receipt.CatalogDigest, CatalogVersion: receipt.CatalogVersion,
		DatasourceID: receipt.DatasourceID, SchemaDigest: receipt.SchemaDigest,
		RootTaskID: exposure.RootTaskID, ProfileVersion: exposure.ProfileVersion,
		PredicateProfileVersion: exposure.PredicateProfileVersion,
		ObservationSHA256:       exposure.ObservationSHA256, DictionarySetSHA256: exposure.DictionarySetSHA256,
		ReleaseSetSHA256: exposure.ReleaseSetSHA256, InfluenceSetSHA256: exposure.InfluenceSetSHA256,
		OutcomeSetSHA256: exposure.OutcomeSetSHA256, PredicateContextSHA256: exposure.PredicateContextSHA256,
		PredicateSetSHA256: exposure.PredicateSetSHA256, CompositeOutcomeSHA256: exposure.CompositeOutcomeSHA256,
		ActualReleaseFacts: exposure.ActualReleaseFacts, ActualInfluenceFacts: exposure.ActualInfluenceFacts,
		ActualOutcomeFacts: exposure.ActualOutcomeFacts, ChargedReleaseFacts: exposure.ChargedReleaseFacts,
		ChargedInfluenceFacts: exposure.ChargedInfluenceFacts, ChargedOutcomeFacts: exposure.ChargedOutcomeFacts,
		ActualPredicateAtomCount:  exposure.ActualPredicateAtomCount,
		ChargedPredicateAtomCount: exposure.ChargedPredicateAtomCount, RootEpoch: exposure.RootEpoch,
	}
}

func registrationPayload(t *testing.T, intent queryreceipt.ArtifactIntentEvidenceV1) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"version": intent.Version, "result_id": intent.ResultID, "format": intent.Format,
		"encryption": intent.Encryption, "key_id": intent.KeyID,
		"parquet_sha256": intent.ParquetSHA256, "object_sha256": intent.ObjectSHA256,
		"parquet_size": intent.ParquetSize, "object_size": intent.ObjectSize,
		"row_count": intent.RowCount, "column_count": intent.ColumnCount,
		"schema_sha256": intent.SchemaSHA256, "result_metadata_sha256": intent.ResultMetadataSHA256,
		"acl_sha256": intent.ACLSHA256, "object_key_sha256": intent.ObjectKeySHA256,
		"staging_key_sha256": intent.StagingKeySHA256, "status": intent.Status,
	}
	if intent.ExpiresAt != nil {
		payload["expires_at"] = intent.ExpiresAt
	}
	return mustJSON(t, payload)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func auditHash(t *testing.T, previous string, event auditchain.Event) string {
	t.Helper()
	digest, err := auditchain.Hash(previous, event)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func shaHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
