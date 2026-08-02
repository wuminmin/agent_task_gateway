package main

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

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
