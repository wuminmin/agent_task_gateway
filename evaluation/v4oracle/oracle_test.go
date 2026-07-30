package v4oracle

import (
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

func TestMaximumPointNormalFormGolden(t *testing.T) {
	digest, err := maximumPointNormalForm()
	if err != nil {
		t.Fatal(err)
	}
	const want = "0969c8eec58e2d7d68db33a55f21600ad6ac2ad80cdd91587b77ce79d27a6333"
	if digest != want {
		t.Fatalf("maximum-point normal form = %s, want %s", digest, want)
	}
}

func TestValidateUniqueJSONRejectsAmbiguityAndTrailingValues(t *testing.T) {
	for _, raw := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":2}}`,
		`{"a":1} {"b":2}`,
		`{"a":[1,2]`,
	} {
		if err := validateUniqueJSON([]byte(raw)); err == nil {
			t.Fatalf("ambiguous JSON accepted: %s", raw)
		}
	}
	if err := validateUniqueJSON([]byte(`{"a":[1,{"b":2}]}`)); err != nil {
		t.Fatalf("canonical JSON rejected: %v", err)
	}
}

func TestMaximumPointIdentityRequiresOneExactCommittedIdentity(t *testing.T) {
	digest := strings.Repeat("a", 64)
	exposureValue := resultExposure{ProfileVersion: exposure.ProfileV4,
		ActualRelease: int64(maximumPointRelease), ActualInfluence: int64(maximumPointInfluence),
		ActualOutcome: int64(maximumPointOutcome), ObservationSHA256: digest, DictionarySetSHA256: digest,
		ReleaseSetSHA256: digest, InfluenceSetSHA256: digest, OutcomeSetSHA256: digest}
	envelope := resultEnvelope{Samples: []resultSample{{Phase: "novel", Status: "measured", Exposure: &exposureValue}}}
	if _, err := maximumPointIdentity(envelope); err != nil {
		t.Fatalf("exact maximum-point identity rejected: %v", err)
	}
	conflict := exposureValue
	conflict.OutcomeSetSHA256 = strings.Repeat("b", 64)
	envelope.Samples = append(envelope.Samples, resultSample{Phase: "novel", Status: "measured", Exposure: &conflict})
	if _, err := maximumPointIdentity(envelope); err == nil {
		t.Fatal("conflicting maximum-point effect identities were accepted")
	}
}
