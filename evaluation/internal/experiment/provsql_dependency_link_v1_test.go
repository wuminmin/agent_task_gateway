package experiment

import (
	"strings"
	"testing"
)

func validProvSQLDependencyLinkForTest() ProvSQLDependencySetVerificationV1 {
	return ProvSQLDependencySetVerificationV1{
		Version: ProvSQLDependencySetVerificationV1Version, Match: true,
		ExpectedCardinality: 29_003, ExpectedSemanticSetSHA256: strings.Repeat("1", 64),
		ObservedCardinality: 29_003, ObservedSemanticSetSHA256: strings.Repeat("1", 64),
		ProductionSetSHA256: strings.Repeat("2", 64), ProductionDictionarySHA256: strings.Repeat("3", 64),
		ObservedOrdinalSetSHA256: strings.Repeat("4", 64),
	}
}

func TestProvSQLDependencyLinkRejectsRealMemberDifferences(t *testing.T) {
	valid := validProvSQLDependencyLinkForTest()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid ProvSQL semantic-to-ordinal link: %v", err)
	}
	for name, mutate := range map[string]func(*ProvSQLDependencySetVerificationV1){
		"missing oracle member": func(value *ProvSQLDependencySetVerificationV1) {
			value.ExpectedOrdinalsMissing = 1
		},
		"unexpected production member": func(value *ProvSQLDependencySetVerificationV1) {
			value.UnexpectedActualOrdinals = 1
		},
		"semantic member digest differs": func(value *ProvSQLDependencySetVerificationV1) {
			value.ObservedSemanticSetSHA256 = strings.Repeat("5", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := valid
			mutate(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatal("real ProvSQL dependency member difference passed the linker evidence gate")
			}
		})
	}
}
