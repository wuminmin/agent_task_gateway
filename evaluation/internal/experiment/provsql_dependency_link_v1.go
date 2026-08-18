package experiment

import (
	"context"
	"errors"
	"fmt"

	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

const ProvSQLDependencySetVerificationV1Version = "taskgate-provsql-dependency-set-verification-v1"

// ProvSQLDependencySetExpectationV1 is the independent ordinary semantic-set
// oracle for one frozen scale/nonce candidate. It is never a production set
// digest.
type ProvSQLDependencySetExpectationV1 struct {
	Scale       string `json:"scale"`
	Limit       int64  `json:"limit"`
	Nonce       int64  `json:"nonce"`
	Cardinality int64  `json:"cardinality"`
	SetSHA256   string `json:"set_sha256"`
}

func (expectation ProvSQLDependencySetExpectationV1) Validate() error {
	spec, err := provsqlfixture.ParseScale(expectation.Scale)
	if err != nil {
		return err
	}
	if expectation.Limit != spec.Limit || expectation.Nonce < 1 || expectation.Nonce > 1_000 ||
		expectation.Cardinality != 29*expectation.Limit+3 || !validSHA256(expectation.SetSHA256) {
		return errors.New("ProvSQL dependency semantic expectation is invalid")
	}
	return nil
}

type ProvSQLDependencySetVerificationRequestV1 struct {
	ContractSelector    FrozenContractSelectorV3
	ProductionSetSHA256 string
}

// ProvSQLDependencySetVerificationV1 retains both domains and proves their
// member equality through the reviewed HOT/COLD publication dictionaries.
type ProvSQLDependencySetVerificationV1 struct {
	Version                    string `json:"version"`
	Match                      bool   `json:"match"`
	ExpectedCardinality        int64  `json:"expected_cardinality"`
	ExpectedSemanticSetSHA256  string `json:"expected_semantic_set_sha256"`
	ObservedCardinality        int64  `json:"observed_cardinality"`
	ObservedSemanticSetSHA256  string `json:"observed_semantic_set_sha256"`
	ProductionSetSHA256        string `json:"production_set_sha256"`
	ProductionDictionarySHA256 string `json:"production_dictionary_set_sha256"`
	ObservedOrdinalSetSHA256   string `json:"observed_ordinal_set_sha256"`
	ExpectedOrdinalsMissing    uint64 `json:"expected_ordinals_missing"`
	UnexpectedActualOrdinals   uint64 `json:"unexpected_actual_ordinals"`
}

func (verification ProvSQLDependencySetVerificationV1) Validate() error {
	if verification.Version != ProvSQLDependencySetVerificationV1Version {
		return fmt.Errorf("ProvSQL dependency set verification version is %q", verification.Version)
	}
	for _, digest := range []string{verification.ExpectedSemanticSetSHA256,
		verification.ObservedSemanticSetSHA256, verification.ProductionSetSHA256,
		verification.ProductionDictionarySHA256, verification.ObservedOrdinalSetSHA256} {
		if !validSHA256(digest) {
			return errors.New("ProvSQL dependency set verification contains an invalid digest")
		}
	}
	if !verification.Match || verification.ExpectedCardinality <= 0 ||
		verification.ExpectedCardinality != verification.ObservedCardinality ||
		verification.ExpectedSemanticSetSHA256 != verification.ObservedSemanticSetSHA256 ||
		verification.ExpectedOrdinalsMissing != 0 || verification.UnexpectedActualOrdinals != 0 {
		return errors.New("ProvSQL dependency semantic-to-ordinal verification did not match")
	}
	return nil
}

type provSQLDependencySetVerifierV1 interface {
	VerifyProvSQL(context.Context, profileMaterialV3, ProvSQLDependencySetExpectationV1,
		string) (ProvSQLDependencySetVerificationV1, error)
}
