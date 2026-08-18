package experiment

import (
	"context"
	"errors"
	"fmt"
)

const ScaleDependencySetVerificationV1Version = "taskgate-scale-dependency-set-verification-v1"

// ScaleDependencySetExpectationV1 is the private, role-bound semantic oracle
// carried by the frozen Scale binding. These identities are deliberately not
// production ordinal/hybrid set identities.
type ScaleDependencySetExpectationV1 struct {
	Scale     string                       `json:"scale"`
	Candidate ScaleDependencySemanticSetV1 `json:"candidate"`
	Existing  ScaleDependencySemanticSetV1 `json:"existing"`
	Union     ScaleDependencySemanticSetV1 `json:"union"`
}

type ScaleDependencySemanticSetV1 struct {
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256"`
}

func (expectation ScaleDependencySetExpectationV1) Validate() error {
	spec, err := ParseDependencyScale(expectation.Scale)
	if err != nil {
		return err
	}
	for role, item := range map[DependencyScaleSummaryRole]ScaleDependencySemanticSetV1{
		DependencyScaleCandidateSummaryRole: expectation.Candidate,
		DependencyScaleExistingSummaryRole:  expectation.Existing,
		DependencyScaleUnionSummaryRole:     expectation.Union,
	} {
		if !validSHA256(item.SetSHA256) {
			return fmt.Errorf("Scale dependency %s semantic digest is invalid", role)
		}
	}
	if expectation.Candidate.Cardinality != spec.CandidateFacts ||
		expectation.Existing.Cardinality != spec.ExistingFacts ||
		expectation.Union.Cardinality != spec.UnionFacts {
		return errors.New("Scale dependency semantic cardinalities differ from the frozen scale")
	}
	return nil
}

func (expectation ScaleDependencySetExpectationV1) forRole(
	role DependencyScaleSummaryRole) (ScaleDependencySemanticSetV1, error) {
	switch role {
	case DependencyScaleCandidateSummaryRole:
		return expectation.Candidate, nil
	case DependencyScaleExistingSummaryRole:
		return expectation.Existing, nil
	case DependencyScaleUnionSummaryRole:
		return expectation.Union, nil
	default:
		return ScaleDependencySemanticSetV1{}, fmt.Errorf("Scale dependency role %q is not frozen", role)
	}
}

// ScaleDependencySetVerificationRequestV1 contains hints and a production set
// identity only. The finalizer resolves the frozen expectation, activated
// profile, publication closure and Control-store set independently.
type ScaleDependencySetVerificationRequestV1 struct {
	ContractSelector    FrozenContractSelectorV3
	Role                DependencyScaleSummaryRole
	ProductionSetSHA256 string
}

// ScaleDependencySetVerificationV1 keeps semantic and production identities in
// separate fields. Match is true only after the evaluation-only linker has
// reconstructed the role-bound semantic digest from the production BitmapSet.
type ScaleDependencySetVerificationV1 struct {
	Version                    string                     `json:"version"`
	Role                       DependencyScaleSummaryRole `json:"role"`
	Match                      bool                       `json:"match"`
	ExpectedCardinality        int64                      `json:"expected_cardinality"`
	ExpectedSemanticSetSHA256  string                     `json:"expected_semantic_set_sha256"`
	ObservedCardinality        int64                      `json:"observed_cardinality"`
	ObservedSemanticSetSHA256  string                     `json:"observed_semantic_set_sha256"`
	ProductionSetSHA256        string                     `json:"production_set_sha256"`
	ProductionDictionarySHA256 string                     `json:"production_dictionary_set_sha256"`
	ObservedOrdinalSetSHA256   string                     `json:"observed_ordinal_set_sha256"`
	ExpectedOrdinalsMissing    uint64                     `json:"expected_ordinals_missing"`
	UnexpectedActualOrdinals   uint64                     `json:"unexpected_actual_ordinals"`
}

func (verification ScaleDependencySetVerificationV1) Validate() error {
	if verification.Version != ScaleDependencySetVerificationV1Version {
		return fmt.Errorf("Scale dependency set verification version is %q", verification.Version)
	}
	switch verification.Role {
	case DependencyScaleCandidateSummaryRole, DependencyScaleExistingSummaryRole, DependencyScaleUnionSummaryRole:
	default:
		return fmt.Errorf("Scale dependency set verification role is %q", verification.Role)
	}
	for _, digest := range []string{verification.ExpectedSemanticSetSHA256,
		verification.ObservedSemanticSetSHA256, verification.ProductionSetSHA256,
		verification.ProductionDictionarySHA256, verification.ObservedOrdinalSetSHA256} {
		if !validSHA256(digest) {
			return errors.New("Scale dependency set verification contains an invalid digest")
		}
	}
	if !verification.Match || verification.ExpectedCardinality < 0 ||
		verification.ExpectedCardinality != verification.ObservedCardinality ||
		verification.ExpectedSemanticSetSHA256 != verification.ObservedSemanticSetSHA256 ||
		verification.ExpectedOrdinalsMissing != 0 || verification.UnexpectedActualOrdinals != 0 {
		return errors.New("Scale dependency semantic-to-ordinal verification did not match")
	}
	return nil
}

type scaleDependencySetVerifierV1 interface {
	Verify(context.Context, profileMaterialV3, ScaleDependencySetExpectationV1,
		DependencyScaleSummaryRole, string) (ScaleDependencySetVerificationV1, error)
}

type dependencySetVerifierV1 interface {
	scaleDependencySetVerifierV1
	provSQLDependencySetVerifierV1
}
