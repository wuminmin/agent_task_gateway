package experiment

import (
	"testing"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

func outcomeCandidateRetainedFixture(t *testing.T) (Sample, ScaleVerificationEvidence) {
	t.Helper()
	expectation := outcomeCandidateTestExpectation(t,
		outcomeCandidateTestDigest(1), outcomeCandidateTestDigest(2), outcomeCandidateTestDigest(3),
		outcomeCandidateTestDigest(4), outcomeCandidateTestDigest(5))
	verification := &OutcomeCandidateVerificationV1{
		Version:  OutcomeCandidateVerificationV1Version,
		Expected: expectation,
		Observed: expectation,
	}
	sample := Sample{
		ActualOutcomeFacts: 5,
		PredicateAtomCount: 4,
		CompositeCount:     1,
		// The production radix identity is intentionally unrelated to the
		// ordinary-set identity retained below.
		OutcomeSetSHA256: outcomeCandidateTestDigest(900),
		TaskGateAcceptanceV3: &FinalizationV3{
			OutcomeCandidateVerification: verification,
		},
		BaselineVerification: &BaselineVerificationEvidence{
			Receipt: queryreceipt.QueryReceiptV1{Exposure: &queryreceipt.ExposureEvidenceV1{
				CompositeOutcomeSHA256: expectation.Members[len(expectation.Members)-1],
			}},
		},
	}
	evidence := ScaleVerificationEvidence{
		Version:                           scaleDependencyEvidenceVersionV3,
		ExpectedOutcomeMemberCardinality:  expectation.Cardinality,
		ObservedOutcomeMemberCardinality:  expectation.Cardinality,
		ExpectedOutcomeCandidateSetSHA256: expectation.OrdinarySetSHA256,
		ObservedOutcomeCandidateSetSHA256: expectation.OrdinarySetSHA256,
	}
	return sample, evidence
}

func TestScaleEvidenceV3RetainsFinalizerAuthoredOutcomeMemberComparison(t *testing.T) {
	sample, evidence := outcomeCandidateRetainedFixture(t)
	if err := validateOutcomeCandidateScaleEvidenceV3(sample, &evidence); err != nil {
		t.Fatalf("honest retained Outcome member evidence was rejected: %v", err)
	}

	mutations := map[string]func(*Sample, *ScaleVerificationEvidence){
		"expected cardinality": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ExpectedOutcomeMemberCardinality--
		},
		"observed cardinality": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObservedOutcomeMemberCardinality--
		},
		"expected ordinary set": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ExpectedOutcomeCandidateSetSHA256 = outcomeCandidateTestDigest(901)
		},
		"observed ordinary set": func(_ *Sample, evidence *ScaleVerificationEvidence) {
			evidence.ObservedOutcomeCandidateSetSHA256 = outcomeCandidateTestDigest(902)
		},
		"signed actual cardinality": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.ActualOutcomeFacts--
		},
		"predicate atom cardinality": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.PredicateAtomCount--
		},
		"composite cardinality": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.CompositeCount = 0
		},
		"accepted member": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.OutcomeCandidateVerification.Observed.Members[0] =
				outcomeCandidateTestDigest(903)
		},
		"missing finalizer verification": func(sample *Sample, _ *ScaleVerificationEvidence) {
			sample.TaskGateAcceptanceV3.OutcomeCandidateVerification = nil
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutatedSample, mutatedEvidence := outcomeCandidateRetainedFixture(t)
			mutate(&mutatedSample, &mutatedEvidence)
			if err := validateOutcomeCandidateScaleEvidenceV3(mutatedSample, &mutatedEvidence); err == nil {
				t.Fatal("mutated retained Outcome candidate evidence was accepted")
			}
		})
	}
}

func TestScaleEvidenceV2CannotAcquireOutcomeEvidenceV3Meaning(t *testing.T) {
	sample, evidence := outcomeCandidateRetainedFixture(t)
	evidence.Version = scaleDependencyEvidenceVersionV2
	evidence.ExpectedOutcomeMemberCardinality = 0
	evidence.ObservedOutcomeMemberCardinality = 0
	evidence.ExpectedOutcomeCandidateSetSHA256 = ""
	evidence.ObservedOutcomeCandidateSetSHA256 = ""
	if err := validateDependencyScaleVerificationV2(sample, &evidence); err == nil {
		t.Fatal("historical Scale evidence-v2 accepted finalizer Outcome evidence-v3")
	}
	sample.TaskGateAcceptanceV3 = nil
	evidence.ExpectedOutcomeMemberCardinality = 5
	if err := validateDependencyScaleVerificationV2(sample, &evidence); err == nil {
		t.Fatal("historical Scale evidence-v2 accepted Outcome evidence-v3 members")
	}
}

func TestScaleEvidenceV3RejectsCoordinatedOutcomeVerificationSplice(t *testing.T) {
	sample, evidence := outcomeCandidateRetainedFixture(t)
	donor := outcomeCandidateTestExpectation(t,
		outcomeCandidateTestDigest(11), outcomeCandidateTestDigest(12), outcomeCandidateTestDigest(13),
		outcomeCandidateTestDigest(14), outcomeCandidateTestDigest(15))
	sample.TaskGateAcceptanceV3.OutcomeCandidateVerification = &OutcomeCandidateVerificationV1{
		Version: OutcomeCandidateVerificationV1Version, Expected: donor, Observed: donor,
	}
	evidence.ExpectedOutcomeMemberCardinality = donor.Cardinality
	evidence.ObservedOutcomeMemberCardinality = donor.Cardinality
	evidence.ExpectedOutcomeCandidateSetSHA256 = donor.OrdinarySetSHA256
	evidence.ObservedOutcomeCandidateSetSHA256 = donor.OrdinarySetSHA256
	if err := validateOutcomeCandidateScaleEvidenceV3(sample, &evidence); err == nil {
		t.Fatal("coordinated Outcome verification splice omitted this receipt's signed composite but was accepted")
	}
}
