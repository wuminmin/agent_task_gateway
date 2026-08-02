// Package releasedartifact composes the independent receipt, audit, and
// artifact checks used by the final campaign adapter. It is evaluation-only:
// no production wire format or release protocol depends on this package.
package releasedartifact

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"

	"taskbound.local/agent-data-gateway/internal/auditchain"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/resultartifact"
)

var ErrInvalidRelease = errors.New("invalid released artifact evidence")

// SettlementEvidence supplies the signed settlement receipt and the audit
// paths needed to connect terminal settlement, PENDING registration, and the
// later AVAILABLE transition. AvailabilityInclusion is a pointer so an absent
// availability event cannot be confused with a zero-valued proof.
type SettlementEvidence struct {
	Receipt               queryreceipt.QueryReceiptV1
	ExpectedBinding       ExpectedBinding
	ReceiptInclusion      auditchain.InclusionProof
	TerminalInclusion     auditchain.InclusionProof
	RegistrationInclusion auditchain.InclusionProof
	AvailabilityInclusion *auditchain.InclusionProof
}

// ExpectedBinding is the final adapter's independent Control projection for
// the signed authorization and exposure fields. Comparing this projection to
// the receipt distinguishes "the gateway signed these values" from "these are
// the values persisted for the campaign query being audited."
type ExpectedBinding struct {
	TaskID                    string
	QueryID                   string
	ResultID                  string
	ManifestDigest            string
	GrantDigest               string
	CatalogDigest             string
	CatalogVersion            string
	DatasourceID              string
	SchemaDigest              string
	RootTaskID                string
	ProfileVersion            string
	PredicateProfileVersion   string
	ObservationSHA256         string
	DictionarySetSHA256       string
	ReleaseSetSHA256          string
	InfluenceSetSHA256        string
	OutcomeSetSHA256          string
	PredicateContextSHA256    string
	PredicateSetSHA256        string
	CompositeOutcomeSHA256    string
	ActualReleaseFacts        int64
	ActualInfluenceFacts      int64
	ActualOutcomeFacts        int64
	ChargedReleaseFacts       int64
	ChargedInfluenceFacts     int64
	ChargedOutcomeFacts       int64
	ActualPredicateAtomCount  int64
	ChargedPredicateAtomCount int64
	RootEpoch                 int64
}

// CanonicalObjectEvidence supplies both representations bound by a V8
// Artifact Intent. Ciphertext is the immutable canonical object-store stream;
// ReleasedParquet is the authenticated plaintext returned by the gateway.
// Keeping both inputs lets an external campaign verifier check object hash and
// size without receiving the deployment's encryption key, while also checking
// the Parquet hash, size, identity, row/column shape, and embedded schema.
type CanonicalObjectEvidence struct {
	ResultID           string
	QueryID            string
	TaskID             string
	KeyID              string
	Format             string
	Encryption         string
	StagingKey         string
	ObjectKey          string
	ParquetSHA256      string
	ObjectSHA256       string
	ParquetSize        int64
	ObjectSize         int64
	RowCount           int64
	ColumnCount        int64
	SchemaJSON         []byte
	ResultMetadataJSON []byte
	ACLJSON            []byte
	ExpiresAt          *time.Time
	Status             string
	Ciphertext         io.Reader
	ReleasedParquet    []byte
}

// VerifyReleasedArtifact is the final-campaign composition point for a
// released result. Verification proceeds from the receipt signature and its
// signed Grant/Catalog/profile/effect fields, through terminal and artifact
// audit inclusion, to the exact canonical ciphertext and released Parquet
// bytes named by the Artifact Intent.
func VerifyReleasedArtifact(verifier *queryreceipt.Verifier, settlement SettlementEvidence, object CanonicalObjectEvidence) error {
	if verifier == nil {
		return invalid("receipt verifier is unavailable")
	}
	if err := verifier.Verify(settlement.Receipt); err != nil {
		return fmt.Errorf("%w: receipt signature or signed binding: %v", ErrInvalidRelease, err)
	}
	if err := verifyExpectedBinding(settlement.Receipt, settlement.ExpectedBinding); err != nil {
		return fmt.Errorf("%w: Control binding projection: %v", ErrInvalidRelease, err)
	}
	if err := queryreceipt.VerifyAuditInclusion(settlement.Receipt, settlement.ReceiptInclusion); err != nil {
		return fmt.Errorf("%w: settlement audit inclusion: %v", ErrInvalidRelease, err)
	}
	if err := queryreceipt.VerifyArtifactIntentInclusion(settlement.Receipt, settlement.TerminalInclusion, settlement.RegistrationInclusion); err != nil {
		return fmt.Errorf("%w: artifact intent inclusion: %v", ErrInvalidRelease, err)
	}
	if settlement.AvailabilityInclusion == nil {
		return invalid("availability inclusion is absent")
	}
	registration := settlement.RegistrationInclusion.TerminalEvent
	availability := settlement.AvailabilityInclusion.TerminalEvent
	if !proofContains(settlement.ReceiptInclusion, registration) ||
		!proofContains(settlement.TerminalInclusion, registration) ||
		!proofContains(settlement.ReceiptInclusion, availability) ||
		!proofContains(settlement.RegistrationInclusion, availability) {
		return invalid("receipt, registration, and availability events do not share one audit path")
	}
	if err := queryreceipt.VerifyArtifactAvailabilityInclusion(settlement.Receipt, *settlement.AvailabilityInclusion); err != nil {
		return fmt.Errorf("%w: availability inclusion: %v", ErrInvalidRelease, err)
	}

	intent := settlement.Receipt.ArtifactIntent
	if intent == nil {
		// Verifier.Verify already rejects this for V8. Keep the guard local so
		// future receipt versions cannot accidentally bypass object checks.
		return invalid("artifact intent is absent")
	}
	if err := verifyArtifactProjection(settlement.Receipt, object); err != nil {
		return fmt.Errorf("%w: Control artifact projection: %v", ErrInvalidRelease, err)
	}
	if strings.TrimSpace(object.ObjectKey) == "" || object.Ciphertext == nil {
		return invalid("canonical object key and ciphertext stream are required")
	}
	if !digestMatches([]byte(object.ObjectKey), intent.ObjectKeySHA256) {
		return invalid("canonical object key does not match artifact intent")
	}
	if err := verifyStream(object.Ciphertext, intent.ObjectSize, intent.ObjectSHA256); err != nil {
		return fmt.Errorf("%w: canonical object: %v", ErrInvalidRelease, err)
	}
	if int64(len(object.ReleasedParquet)) != intent.ParquetSize ||
		!digestMatches(object.ReleasedParquet, intent.ParquetSHA256) {
		return invalid("released Parquet hash or size does not match artifact intent")
	}
	if err := verifyParquet(intent, object.ReleasedParquet); err != nil {
		return fmt.Errorf("%w: released Parquet: %v", ErrInvalidRelease, err)
	}
	return nil
}

func verifyStream(reader io.Reader, expectedSize int64, expectedSHA256 string) error {
	if expectedSize <= 0 {
		return errors.New("committed object size is invalid")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(reader, expectedSize+1))
	if err != nil {
		return err
	}
	if written != expectedSize {
		return errors.New("ciphertext size does not match artifact intent")
	}
	expected, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expected) != sha256.Size || subtle.ConstantTimeCompare(hash.Sum(nil), expected) != 1 {
		return errors.New("ciphertext hash does not match artifact intent")
	}
	return nil
}

func verifyParquet(intent *queryreceipt.ArtifactIntentEvidenceV1, value []byte) error {
	file, err := parquet.OpenFile(bytes.NewReader(value), int64(len(value)))
	if err != nil {
		return err
	}
	format, formatOK := file.Lookup("taskgate.format")
	resultID, resultIDOK := file.Lookup("taskgate.result_id")
	schemaJSON, schemaOK := file.Lookup("taskgate.schema")
	if !formatOK || format != "taskgate-result-parquet-v1" ||
		!resultIDOK || resultID != intent.ResultID || !schemaOK {
		return errors.New("identity metadata does not match artifact intent")
	}
	normalizedSchema, err := normalizeJSON([]byte(schemaJSON))
	if err != nil || !digestMatches(normalizedSchema, intent.SchemaSHA256) {
		return errors.New("schema digest does not match artifact intent")
	}
	var schema []resultartifact.ColumnSchema
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil || int64(len(schema)) != intent.ColumnCount {
		return errors.New("schema column count does not match artifact intent")
	}
	if file.NumRows() != intent.RowCount {
		return errors.New("row count does not match artifact intent")
	}
	return nil
}

func verifyExpectedBinding(receipt queryreceipt.QueryReceiptV1, expected ExpectedBinding) error {
	if receipt.Exposure == nil || receipt.ArtifactIntent == nil {
		return errors.New("V8 exposure or artifact intent is absent")
	}
	exposure := receipt.Exposure
	stringsToCompare := []struct {
		name, actual, expected string
	}{
		{"task ID", receipt.TaskID, expected.TaskID}, {"query ID", receipt.QueryID, expected.QueryID},
		{"result ID", receipt.ArtifactIntent.ResultID, expected.ResultID},
		{"manifest digest", receipt.ManifestDigest, expected.ManifestDigest},
		{"Grant digest", receipt.GrantDigest, expected.GrantDigest},
		{"Catalog digest", receipt.CatalogDigest, expected.CatalogDigest},
		{"Catalog version", receipt.CatalogVersion, expected.CatalogVersion},
		{"datasource ID", receipt.DatasourceID, expected.DatasourceID},
		{"schema digest", receipt.SchemaDigest, expected.SchemaDigest},
		{"root task ID", exposure.RootTaskID, expected.RootTaskID},
		{"exposure profile", exposure.ProfileVersion, expected.ProfileVersion},
		{"predicate profile", exposure.PredicateProfileVersion, expected.PredicateProfileVersion},
		{"observation digest", exposure.ObservationSHA256, expected.ObservationSHA256},
		{"dictionary-set digest", exposure.DictionarySetSHA256, expected.DictionarySetSHA256},
		{"Result-set digest", exposure.ReleaseSetSHA256, expected.ReleaseSetSHA256},
		{"Dependency-set digest", exposure.InfluenceSetSHA256, expected.InfluenceSetSHA256},
		{"Outcome-set digest", exposure.OutcomeSetSHA256, expected.OutcomeSetSHA256},
		{"predicate-context digest", exposure.PredicateContextSHA256, expected.PredicateContextSHA256},
		{"predicate-set digest", exposure.PredicateSetSHA256, expected.PredicateSetSHA256},
		{"composite-outcome digest", exposure.CompositeOutcomeSHA256, expected.CompositeOutcomeSHA256},
	}
	for _, comparison := range stringsToCompare {
		if comparison.expected == "" || comparison.actual != comparison.expected {
			return fmt.Errorf("%s differs from expected campaign evidence", comparison.name)
		}
	}
	countsToCompare := []struct {
		name             string
		actual, expected int64
	}{
		{"actual Result count", exposure.ActualReleaseFacts, expected.ActualReleaseFacts},
		{"actual Dependency count", exposure.ActualInfluenceFacts, expected.ActualInfluenceFacts},
		{"actual Outcome count", exposure.ActualOutcomeFacts, expected.ActualOutcomeFacts},
		{"charged Result count", exposure.ChargedReleaseFacts, expected.ChargedReleaseFacts},
		{"charged Dependency count", exposure.ChargedInfluenceFacts, expected.ChargedInfluenceFacts},
		{"charged Outcome count", exposure.ChargedOutcomeFacts, expected.ChargedOutcomeFacts},
		{"actual predicate count", exposure.ActualPredicateAtomCount, expected.ActualPredicateAtomCount},
		{"charged predicate count", exposure.ChargedPredicateAtomCount, expected.ChargedPredicateAtomCount},
		{"root epoch", exposure.RootEpoch, expected.RootEpoch},
	}
	for _, comparison := range countsToCompare {
		if comparison.actual != comparison.expected {
			return fmt.Errorf("%s differs from expected campaign evidence", comparison.name)
		}
	}
	return nil
}

func verifyArtifactProjection(receipt queryreceipt.QueryReceiptV1, object CanonicalObjectEvidence) error {
	intent := receipt.ArtifactIntent
	if intent == nil {
		return errors.New("artifact intent is absent")
	}
	stringsToCompare := []struct {
		name, actual, expected string
	}{
		{"result ID", object.ResultID, intent.ResultID}, {"query ID", object.QueryID, receipt.QueryID},
		{"task ID", object.TaskID, receipt.TaskID}, {"key ID", object.KeyID, intent.KeyID},
		{"format", object.Format, intent.Format}, {"encryption", object.Encryption, intent.Encryption},
		{"Parquet digest", object.ParquetSHA256, intent.ParquetSHA256},
		{"object digest", object.ObjectSHA256, intent.ObjectSHA256},
	}
	for _, comparison := range stringsToCompare {
		if comparison.actual == "" || comparison.actual != comparison.expected {
			return fmt.Errorf("%s does not match signed artifact intent", comparison.name)
		}
	}
	if object.Status != "AVAILABLE" || object.ParquetSize != intent.ParquetSize ||
		object.ObjectSize != intent.ObjectSize || object.RowCount != intent.RowCount ||
		object.ColumnCount != intent.ColumnCount || !sameOptionalTime(object.ExpiresAt, intent.ExpiresAt) {
		return errors.New("status or artifact dimensions do not match signed intent")
	}
	if !digestMatches([]byte(object.ObjectKey), intent.ObjectKeySHA256) ||
		!digestMatches([]byte(object.StagingKey), intent.StagingKeySHA256) {
		return errors.New("object key projection does not match signed intent")
	}
	for _, value := range []struct {
		name     string
		raw      []byte
		expected string
	}{
		{"schema", object.SchemaJSON, intent.SchemaSHA256},
		{"result metadata", object.ResultMetadataJSON, intent.ResultMetadataSHA256},
		{"ACL", object.ACLJSON, intent.ACLSHA256},
	} {
		normalized, err := normalizeJSON(value.raw)
		if err != nil || !digestMatches(normalized, value.expected) {
			return fmt.Errorf("%s does not match signed artifact intent", value.name)
		}
	}
	return nil
}

func normalizeJSON(raw []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func proofContains(proof auditchain.InclusionProof, event auditchain.Event) bool {
	if proof.TerminalEvent.Sequence == event.Sequence && proof.TerminalEvent.CurrentHash == event.CurrentHash {
		return true
	}
	for _, successor := range proof.SuccessorEvents {
		if successor.Sequence == event.Sequence && successor.CurrentHash == event.CurrentHash {
			return true
		}
	}
	return false
}

func sameOptionalTime(actual, expected *time.Time) bool {
	if actual == nil || expected == nil {
		return actual == nil && expected == nil
	}
	return actual.Equal(*expected)
}

func digestMatches(value []byte, expectedHex string) bool {
	expected, err := hex.DecodeString(expectedHex)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	actual := sha256.Sum256(value)
	return subtle.ConstantTimeCompare(actual[:], expected) == 1
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidRelease, reason)
}
