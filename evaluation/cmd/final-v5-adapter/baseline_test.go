package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func TestReceiptDigestUsesTypedIdentityAcrossWireEncodings(t *testing.T) {
	receipt := matchingVerifiedResponse().Receipt
	canonical, err := approval.CanonicalJSON(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var decoded queryreceipt.QueryReceiptV1
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		t.Fatal(err)
	}
	if shaBytes(canonical) == receiptDigest(decoded) {
		t.Fatal("fixture did not exercise distinct canonical and typed JSON encodings")
	}
	if receiptDigest(receipt) != receiptDigest(decoded) {
		t.Fatal("semantically identical receipt changed identity across wire encodings")
	}
}

func TestValidateResponseAgainstVerifiedReceiptRejectsUnsignedResponseDrift(t *testing.T) {
	response := matchingVerifiedResponse()
	if err := validateResponseAgainstVerifiedReceipt(response); err != nil {
		t.Fatalf("matching response: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*queryResponse)
	}{
		{name: "set digest", mutate: func(value *queryResponse) { value.Exposure.ReleaseSetSHA256 = "different" }},
		{name: "column count", mutate: func(value *queryResponse) { value.ColumnCount++ }},
		{name: "predicate count", mutate: func(value *queryResponse) { value.Exposure.ActualPredicateAtomCount++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := response
			test.mutate(&mutated)
			if err := validateResponseAgainstVerifiedReceipt(mutated); err == nil {
				t.Fatal("response drift was accepted")
			}
		})
	}
}

func TestPilotIdentityHashesAreAuthorityAndKindSeparated(t *testing.T) {
	operation := experiment.AdapterOperation{CampaignID: "campaign-one", DeploymentID: "deployment-one"}
	query := saltedIdentityHash(operation, "query", "shared-id")
	result := saltedIdentityHash(operation, "result", "shared-id")
	otherDeployment := operation
	otherDeployment.DeploymentID = "deployment-two"

	for name, digest := range map[string]string{
		"query":            query,
		"result":           result,
		"other deployment": saltedIdentityHash(otherDeployment, "query", "shared-id"),
	} {
		if !validDigest(digest) {
			t.Fatalf("%s identity is not a SHA-256 digest: %q", name, digest)
		}
	}
	if query == result || query == saltedIdentityHash(otherDeployment, "query", "shared-id") {
		t.Fatal("redacted identity did not bind both identifier kind and deployment authority")
	}
}

func TestObservationBindingDigestSeparatesRootAuthority(t *testing.T) {
	observation := sha("shared-observation")
	first := observationBindingDigest("root-one", observation)
	second := observationBindingDigest("root-two", observation)
	if !validDigest(first) || !validDigest(second) || first == second {
		t.Fatal("root-bound observation identity did not separate independent roots")
	}
}

func TestResponseTerminalIdentityEvidenceRetainsOnlyRedactedIDs(t *testing.T) {
	operation := experiment.AdapterOperation{CampaignID: "campaign-one", DeploymentID: "deployment-one"}
	response := matchingVerifiedResponse()
	response.Receipt.ArtifactIntent.ObjectKeySHA256 = sha("results/private/object")
	response.Receipt.ArtifactIntent.ObjectSHA256 = sha("ciphertext")
	response.Receipt.ArtifactIntent.IntentSHA256 = sha("intent")
	response.Receipt.Exposure.ObservationSHA256 = sha("observation")
	response.Exposure.ObservationSHA256 = response.Receipt.Exposure.ObservationSHA256
	manifest := &experiment.RedactedVerifierManifest{
		CanonicalCiphertextSHA256: response.Receipt.ArtifactIntent.ObjectSHA256,
		CanonicalCiphertextSize:   123,
	}

	evidence := responseTerminalIdentityEvidence(operation, response, manifest)
	if !evidence.Found || evidence.QueryIDHash != saltedIdentityHash(operation, "query", response.QueryID) ||
		evidence.ResultIDHash != saltedIdentityHash(operation, "result", response.ResultID) ||
		evidence.ObjectKeySHA256 != response.Receipt.ArtifactIntent.ObjectKeySHA256 ||
		evidence.CanonicalCiphertextSHA256 != manifest.CanonicalCiphertextSHA256 ||
		evidence.CanonicalCiphertextSize != manifest.CanonicalCiphertextSize {
		t.Fatal("returned terminal identity is not bound to the verified response and transcript")
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{[]byte(response.QueryID), []byte(response.ResultID), []byte("results/private/object")} {
		if bytes.Contains(encoded, raw) {
			t.Fatalf("redacted terminal evidence leaked raw identity %q", raw)
		}
	}
}

func matchingVerifiedResponse() queryResponse {
	const (
		taskID   = "task-one"
		queryID  = "query-one"
		resultID = "result-one"
	)
	exposure := &queryreceipt.ExposureEvidenceV1{
		RootTaskID: taskID, ProfileVersion: "taskgate-exposure-v5",
		ActualReleaseFacts: 2, ActualInfluenceFacts: 3, ActualOutcomeFacts: 5,
		ChargedReleaseFacts: 1, ChargedInfluenceFacts: 2, ChargedOutcomeFacts: 4,
		ObservationSHA256: "observation", DictionarySetSHA256: "dictionary",
		ReleaseSetSHA256: "release", InfluenceSetSHA256: "influence", OutcomeSetSHA256: "outcome",
		RootEpoch: 7, PredicateContextSHA256: "context", PredicateSetSHA256: "predicate",
		ActualPredicateAtomCount: 3, ChargedPredicateAtomCount: 2,
		CompositeOutcomeSHA256: "composite", ActualCompositeCount: 1, ChargedCompositeCount: 1,
	}
	response := queryResponse{
		TaskID: taskID, QueryID: queryID, ResultID: resultID,
		ArtifactStatus: "AVAILABLE", RowCount: 11, ColumnCount: 4,
		Receipt: queryreceipt.QueryReceiptV1{
			TaskID: taskID, QueryID: queryID, Exposure: exposure,
			ArtifactIntent: &queryreceipt.ArtifactIntentEvidenceV1{ResultID: resultID, RowCount: 11, ColumnCount: 4},
		},
	}
	response.Exposure.QueryID = queryID
	response.Exposure.RootTaskID = exposure.RootTaskID
	response.Exposure.ProfileVersion = exposure.ProfileVersion
	response.Exposure.ActualReleaseFacts = exposure.ActualReleaseFacts
	response.Exposure.ActualInfluenceFacts = exposure.ActualInfluenceFacts
	response.Exposure.ActualOutcomeFacts = exposure.ActualOutcomeFacts
	response.Exposure.ChargedReleaseFacts = exposure.ChargedReleaseFacts
	response.Exposure.ChargedInfluenceFacts = exposure.ChargedInfluenceFacts
	response.Exposure.ChargedOutcomeFacts = exposure.ChargedOutcomeFacts
	response.Exposure.ActualPredicateAtomCount = exposure.ActualPredicateAtomCount
	response.Exposure.ChargedPredicateAtomCount = exposure.ChargedPredicateAtomCount
	response.Exposure.CompositeOutcomeSHA256 = exposure.CompositeOutcomeSHA256
	response.Exposure.PredicateContextSHA256 = exposure.PredicateContextSHA256
	response.Exposure.PredicateSetSHA256 = exposure.PredicateSetSHA256
	response.Exposure.ObservationSHA256 = exposure.ObservationSHA256
	response.Exposure.DictionarySetDigest = exposure.DictionarySetSHA256
	response.Exposure.ReleaseSetSHA256 = exposure.ReleaseSetSHA256
	response.Exposure.InfluenceSetSHA256 = exposure.InfluenceSetSHA256
	response.Exposure.OutcomeSetSHA256 = exposure.OutcomeSetSHA256
	response.Exposure.RootEpoch = exposure.RootEpoch
	return response
}
