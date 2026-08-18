package experiment

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const (
	// OutcomeCandidateVerificationV1Version identifies the finalizer-authored
	// comparison between the frozen ordinary-set oracle and the members the
	// finalizer recovered from this execution.
	OutcomeCandidateVerificationV1Version = "taskgate-outcome-candidate-verification-v1"
	OutcomeCandidateVerificationV2Version = "taskgate-outcome-candidate-verification-v2"
	OutcomeCandidateDomainLinkV1Version   = "taskgate-outcome-candidate-domain-link-v1"
	outcomeCandidateMemberCardinalityV1   = int64(5)
)

// OutcomeCandidateExpectationV1 is the frozen, ordinary-set identity of the
// exact Scale Outcome candidate: four predicate-atom Facts and one composite
// Outcome Fact. It is deliberately not the production radix-set identity.
//
// Members are retained because equality of two aggregate digests would not be
// a member-level check. OrdinarySetSHA256 is role-bound by the evaluation set
// algebra's "candidate" role.
type OutcomeCandidateExpectationV1 struct {
	Cardinality       int64    `json:"cardinality"`
	Members           []string `json:"members"`
	OrdinarySetSHA256 string   `json:"ordinary_set_sha256"`
}

// Validate requires the one closed shape Decision 21 authorizes. In
// particular it recomputes the ordinary-set identity from the retained members;
// a binding cannot make an arbitrary aggregate digest authoritative merely by
// placing it beside five hashes.
func (expectation OutcomeCandidateExpectationV1) Validate() error {
	if expectation.Cardinality != outcomeCandidateMemberCardinalityV1 {
		return fmt.Errorf("Outcome candidate cardinality is %d, want %d",
			expectation.Cardinality, outcomeCandidateMemberCardinalityV1)
	}
	if int64(len(expectation.Members)) != expectation.Cardinality {
		return fmt.Errorf("Outcome candidate retains %d members for cardinality %d",
			len(expectation.Members), expectation.Cardinality)
	}
	if !sort.StringsAreSorted(expectation.Members) {
		return errors.New("Outcome candidate members are not in canonical lexical order")
	}
	summary, err := summarizeOutcomeCandidateMembers(expectation.Members)
	if err != nil {
		return err
	}
	if summary.Cardinality != expectation.Cardinality {
		return fmt.Errorf("Outcome candidate members collapse to ordinary-set cardinality %d, want %d",
			summary.Cardinality, expectation.Cardinality)
	}
	if summary.OrdinarySetSHA256 != expectation.OrdinarySetSHA256 {
		return fmt.Errorf("Outcome candidate ordinary-set identity is %s, members derive %s",
			shortDigest(expectation.OrdinarySetSHA256), shortDigest(summary.OrdinarySetSHA256))
	}
	return nil
}

// OutcomeCandidateVerificationV1 is written by the finalizer only after exact
// cardinality, member and ordinary-set identity all agree. Keeping both sides
// makes the accepted sample auditable without treating the production radix
// OutcomeSetSHA256 as an ordinary semantic-set digest.
type OutcomeCandidateVerificationV1 struct {
	Version    string                        `json:"version"`
	Expected   OutcomeCandidateExpectationV1 `json:"expected"`
	Observed   OutcomeCandidateExpectationV1 `json:"observed"`
	DomainLink *OutcomeCandidateDomainLinkV1 `json:"domain_link,omitempty"`
}

// OutcomeCandidateDomainLinkV1 records the evaluation-only normalization from
// the complete Catalog identity used by the frozen oracle to the activated
// profile Catalog identity used by production preparation. LinkedExpected is
// regenerated from the fixed oracle model, never from production output.
type OutcomeCandidateDomainLinkV1 struct {
	Version                string                        `json:"version"`
	GeneratorVersion       string                        `json:"generator_version"`
	FrozenCatalogSHA256    string                        `json:"frozen_catalog_sha256"`
	ActivatedCatalogSHA256 string                        `json:"activated_catalog_sha256"`
	CandidateFacts         int64                         `json:"candidate_facts"`
	FrozenExpected         OutcomeCandidateExpectationV1 `json:"frozen_expected"`
	LinkedExpected         OutcomeCandidateExpectationV1 `json:"linked_expected"`
}

func (link OutcomeCandidateDomainLinkV1) Validate() error {
	if link.Version != OutcomeCandidateDomainLinkV1Version ||
		link.GeneratorVersion != finalv5oracle.ExposureScaleOutcomeGeneratorVersion ||
		!validSHA256(link.FrozenCatalogSHA256) || !validSHA256(link.ActivatedCatalogSHA256) {
		return errors.New("Outcome candidate domain link identity is invalid")
	}
	switch link.CandidateFacts {
	case finalv5oracle.DependencyScale10K, finalv5oracle.DependencyScale100K, finalv5oracle.DependencyScale1035000:
	default:
		return errors.New("Outcome candidate domain link scale is not frozen")
	}
	if err := link.FrozenExpected.Validate(); err != nil {
		return fmt.Errorf("Outcome candidate domain link frozen endpoint: %w", err)
	}
	return link.LinkedExpected.Validate()
}

// verifyOutcomeCandidateV1 permanently retains the same-domain historical
// comparison for existing evidence and its compatibility tests. New runtime
// output uses verifyOutcomeCandidateV2.
func verifyOutcomeCandidateV1(expected OutcomeCandidateExpectationV1,
	reproduced ReproducedExecutionV3, exposure *queryreceipt.ExposureEvidenceV1) (OutcomeCandidateVerificationV1, error) {
	verification, observed, err := reconstructOutcomeCandidateV1(expected, reproduced, exposure)
	if err != nil {
		return verification, err
	}
	if !equalOutcomeCandidateExpectations(expected, observed) {
		return verification, rejectTaskGateAt(errors.New("observed Outcome candidate members differ from the frozen ordinary-set oracle"),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation)
	}
	verification = OutcomeCandidateVerificationV1{Version: OutcomeCandidateVerificationV1Version,
		Expected: expected, Observed: observed}
	verification.Expected.Members = append([]string(nil), expected.Members...)
	return verification, nil
}

func reconstructOutcomeCandidateV1(expected OutcomeCandidateExpectationV1,
	reproduced ReproducedExecutionV3, exposure *queryreceipt.ExposureEvidenceV1) (OutcomeCandidateVerificationV1,
	OutcomeCandidateExpectationV1, error) {
	var verification OutcomeCandidateVerificationV1
	if err := expected.Validate(); err != nil {
		return verification, OutcomeCandidateExpectationV1{}, rejectTaskGateAt(fmt.Errorf("frozen Outcome candidate is invalid: %w", err),
			rejectionGateFrozenMaterial, rejectionFailureInvalidValue,
			rejectionSourceFrozenContract, rejectionSourceFrozenContract)
	}
	if exposure == nil {
		return verification, OutcomeCandidateExpectationV1{}, rejectTaskGateAt(errors.New("verified receipt carries no exposure evidence"),
			rejectionGateFrozenMaterial, rejectionFailureMissing,
			rejectionSourceFrozenContract, rejectionSourceGatewayReceipt)
	}
	atoms := reproduced.PreparedPredicateAtomSHA256
	if int64(len(atoms)) != exposure.ActualPredicateAtomCount ||
		reproduced.PreparedPredicateContextSHA256 != exposure.PredicateContextSHA256 ||
		reproduced.PreparedPredicateSetSHA256 != exposure.PredicateSetSHA256 {
		return verification, OutcomeCandidateExpectationV1{}, rejectTaskGateAt(errors.New("the finalizer's prepared predicate footprint differs from the verified receipt"),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation)
	}
	members := append(append([]string(nil), atoms...), exposure.CompositeOutcomeSHA256)
	observed, err := summarizeOutcomeCandidateMembers(members)
	if err != nil {
		return verification, OutcomeCandidateExpectationV1{}, rejectTaskGateAt(fmt.Errorf("reconstruct observed ordinary Outcome candidate: %w", err),
			rejectionGateFrozenMaterial, rejectionFailureInvalidValue,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation)
	}
	if exposure.ActualOutcomeFacts != observed.Cardinality {
		return verification, OutcomeCandidateExpectationV1{}, rejectTaskGateAt(errors.New("the reconstructed ordinary Outcome candidate cardinality differs from the verified receipt"),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation)
	}
	return verification, observed, nil
}

// Validate rejects a partial or merely aggregate agreement record.
func (verification OutcomeCandidateVerificationV1) Validate() error {
	if verification.Version != OutcomeCandidateVerificationV1Version &&
		verification.Version != OutcomeCandidateVerificationV2Version {
		return fmt.Errorf("Outcome candidate verification version is %q", verification.Version)
	}
	if err := verification.Expected.Validate(); err != nil {
		return fmt.Errorf("expected Outcome candidate: %w", err)
	}
	if err := verification.Observed.Validate(); err != nil {
		return fmt.Errorf("observed Outcome candidate: %w", err)
	}
	if verification.Version == OutcomeCandidateVerificationV1Version {
		if verification.DomainLink != nil {
			return errors.New("Outcome candidate verification-v1 cannot carry a domain link")
		}
		if !equalOutcomeCandidateExpectations(verification.Expected, verification.Observed) {
			return errors.New("expected and observed Outcome candidate members differ")
		}
		return nil
	}
	if verification.DomainLink == nil {
		return errors.New("Outcome candidate verification-v2 omits its domain link")
	}
	if err := verification.DomainLink.Validate(); err != nil {
		return err
	}
	if !equalOutcomeCandidateExpectations(verification.Expected, verification.DomainLink.FrozenExpected) ||
		verification.Expected.Cardinality != verification.Observed.Cardinality ||
		!equalOutcomeCandidateExpectations(verification.DomainLink.LinkedExpected, verification.Observed) {
		return errors.New("linked and observed Outcome candidate members differ")
	}
	return nil
}

func equalOutcomeCandidateExpectations(left, right OutcomeCandidateExpectationV1) bool {
	return left.Cardinality == right.Cardinality &&
		left.OrdinarySetSHA256 == right.OrdinarySetSHA256 && equalStringsV3(left.Members, right.Members)
}

type outcomeCandidateDomainLinkerV1 struct {
	mu      sync.Mutex
	byInput map[string]OutcomeCandidateExpectationV1
}

func newOutcomeCandidateDomainLinkerV1() *outcomeCandidateDomainLinkerV1 {
	return &outcomeCandidateDomainLinkerV1{byInput: make(map[string]OutcomeCandidateExpectationV1)}
}

func (linker *outcomeCandidateDomainLinkerV1) linkedExpectation(catalogPath string,
	candidateFacts int64) (OutcomeCandidateExpectationV1, string, error) {
	logical, err := catalog.Load(catalogPath)
	if err != nil {
		return OutcomeCandidateExpectationV1{}, "", fmt.Errorf("load activated Scale profile Catalog: %w", err)
	}
	linked, err := linker.generatedExpectation(logical.SHA256, candidateFacts)
	return linked, logical.SHA256, err
}

func (linker *outcomeCandidateDomainLinkerV1) generatedExpectation(catalogSHA256 string,
	candidateFacts int64) (OutcomeCandidateExpectationV1, error) {
	key := catalogSHA256 + "\x00" + fmt.Sprint(candidateFacts)
	linker.mu.Lock()
	defer linker.mu.Unlock()
	if cached, ok := linker.byInput[key]; ok {
		return cached, nil
	}
	generated, err := finalv5oracle.GenerateExposureScaleOutcomeCandidate(finalv5oracle.ExposureScaleOutcomeRequest{
		CatalogSHA256: catalogSHA256, CandidateFacts: candidateFacts,
		SetOptions: finalv5oracle.StreamSetOptions{MaxInMemoryMembers: 65_536},
	})
	if err != nil {
		return OutcomeCandidateExpectationV1{}, fmt.Errorf("regenerate Catalog-domain Outcome candidate: %w", err)
	}
	linked := OutcomeCandidateExpectationV1{Cardinality: generated.CandidateCardinality,
		Members: append([]string(nil), generated.Members...), OrdinarySetSHA256: generated.CandidateSetSHA256}
	if err := linked.Validate(); err != nil {
		return OutcomeCandidateExpectationV1{}, err
	}
	linker.byInput[key] = linked
	return linked, nil
}

func cloneOutcomeCandidateExpectationV1(expectation *OutcomeCandidateExpectationV1) *OutcomeCandidateExpectationV1 {
	if expectation == nil {
		return nil
	}
	cloned := *expectation
	cloned.Members = append([]string(nil), expectation.Members...)
	return &cloned
}

// verifyOutcomeCandidateV1 reconstructs the observed ORDINARY candidate set.
// Its atom operands are the finalizer's prepared predicate footprint and its
// composite operand is the already-verified Gateway-signed receipt member. The
// production OutcomeSetSHA256 is a radix-set identity and is intentionally not
// read here.
func verifyOutcomeCandidateV2(linker *outcomeCandidateDomainLinkerV1, expected OutcomeCandidateExpectationV1,
	frozenCatalogSHA256 string, candidateFacts int64, activatedCatalogPath string,
	reproduced ReproducedExecutionV3, exposure *queryreceipt.ExposureEvidenceV1) (OutcomeCandidateVerificationV1, error) {
	var verification OutcomeCandidateVerificationV1
	if err := expected.Validate(); err != nil {
		return verification, rejectTaskGateAt(fmt.Errorf("frozen Outcome candidate is invalid: %w", err),
			rejectionGateFrozenMaterial, rejectionFailureInvalidValue,
			rejectionSourceFrozenContract, rejectionSourceFrozenContract)
	}
	if linker == nil || !validSHA256(frozenCatalogSHA256) || activatedCatalogPath == "" {
		return verification, rejectTaskGateAt(errors.New("Outcome candidate domain linker input is incomplete"),
			rejectionGateFrozenMaterial, rejectionFailureMissing,
			rejectionSourceFrozenContract, rejectionSourceActivatedProfile)
	}
	frozenGenerated, err := linker.generatedExpectation(frozenCatalogSHA256, candidateFacts)
	if err != nil || !equalOutcomeCandidateExpectations(expected, frozenGenerated) {
		if err == nil {
			err = errors.New("frozen Outcome candidate differs from its complete-Catalog oracle regeneration")
		}
		return verification, rejectTaskGateAt(err, rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFrozenContract)
	}
	if exposure == nil {
		return verification, rejectTaskGateAt(errors.New("verified receipt carries no exposure evidence"),
			rejectionGateFrozenMaterial, rejectionFailureMissing,
			rejectionSourceFrozenContract, rejectionSourceGatewayReceipt)
	}
	preparedAtomSHA256 := reproduced.PreparedPredicateAtomSHA256
	if int64(len(preparedAtomSHA256)) != exposure.ActualPredicateAtomCount {
		return verification, rejectTaskGateAt(fmt.Errorf("the finalizer prepared %d predicate atoms but the verified receipt signs %d",
			len(preparedAtomSHA256), exposure.ActualPredicateAtomCount),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation,
			rejectionCountDifference(rejectionDifferenceExpectedCount, exposure.ActualPredicateAtomCount),
			rejectionCountDifference(rejectionDifferenceActualCount, int64(len(preparedAtomSHA256))))
	}
	if reproduced.PreparedPredicateContextSHA256 != exposure.PredicateContextSHA256 ||
		reproduced.PreparedPredicateSetSHA256 != exposure.PredicateSetSHA256 {
		return verification, rejectTaskGateAt(errors.New("the finalizer's prepared predicate footprint differs from the verified receipt"),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation)
	}
	members := append([]string(nil), preparedAtomSHA256...)
	members = append(members, exposure.CompositeOutcomeSHA256)
	observed, err := summarizeOutcomeCandidateMembers(members)
	if err != nil {
		return verification, rejectTaskGateAt(fmt.Errorf("reconstruct observed ordinary Outcome candidate: %w", err),
			rejectionGateFrozenMaterial, rejectionFailureInvalidValue,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation)
	}
	if exposure.ActualOutcomeFacts != observed.Cardinality {
		return verification, rejectTaskGateAt(fmt.Errorf("the reconstructed ordinary Outcome candidate has %d members but the verified receipt signs %d",
			observed.Cardinality, exposure.ActualOutcomeFacts),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation,
			rejectionCountDifference(rejectionDifferenceExpectedCount, exposure.ActualOutcomeFacts),
			rejectionCountDifference(rejectionDifferenceActualCount, observed.Cardinality))
	}
	linked, activatedCatalogSHA256, err := linker.linkedExpectation(activatedCatalogPath, candidateFacts)
	if err != nil {
		return verification, rejectTaskGateAt(err, rejectionGateFrozenMaterial, rejectionFailureInvalidValue,
			rejectionSourceFrozenContract, rejectionSourceActivatedProfile)
	}
	if linked.Cardinality != observed.Cardinality ||
		linked.OrdinarySetSHA256 != observed.OrdinarySetSHA256 ||
		!equalStringsV3(linked.Members, observed.Members) {
		differences := []rejectionDifferenceV1{
			rejectionCountDifference(rejectionDifferenceExpectedCount, linked.Cardinality),
			rejectionCountDifference(rejectionDifferenceActualCount, observed.Cardinality),
		}
		differences = append(differences,
			rejectionSHA256Pair(linked.OrdinarySetSHA256, observed.OrdinarySetSHA256)...)
		return verification, rejectTaskGateAt(fmt.Errorf("observed Outcome candidate members differ from the linked profile-domain ordinary-set oracle"),
			rejectionGateFrozenMaterial, rejectionFailureMismatch,
			rejectionSourceFrozenContract, rejectionSourceFinalizerDerivation, differences...)
	}
	verification = OutcomeCandidateVerificationV1{
		Version: OutcomeCandidateVerificationV2Version,
		Expected: OutcomeCandidateExpectationV1{
			Cardinality: expected.Cardinality, Members: append([]string(nil), expected.Members...),
			OrdinarySetSHA256: expected.OrdinarySetSHA256,
		},
		Observed: observed,
		DomainLink: &OutcomeCandidateDomainLinkV1{
			Version:             OutcomeCandidateDomainLinkV1Version,
			GeneratorVersion:    finalv5oracle.ExposureScaleOutcomeGeneratorVersion,
			FrozenCatalogSHA256: frozenCatalogSHA256, ActivatedCatalogSHA256: activatedCatalogSHA256,
			CandidateFacts: candidateFacts,
			FrozenExpected: OutcomeCandidateExpectationV1{
				Cardinality: expected.Cardinality, Members: append([]string(nil), expected.Members...),
				OrdinarySetSHA256: expected.OrdinarySetSHA256,
			},
			LinkedExpected: linked,
		},
	}
	return verification, nil
}

func summarizeOutcomeCandidateMembers(members []string) (OutcomeCandidateExpectationV1, error) {
	for index, member := range members {
		if !validSHA256(member) {
			return OutcomeCandidateExpectationV1{},
				fmt.Errorf("Outcome candidate member %d is not lowercase SHA-256", index+1)
		}
	}
	summary, err := finalv5oracle.SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, finalv5oracle.StreamSetOptions{
		MaxInMemoryMembers: int(outcomeCandidateMemberCardinalityV1),
		CaptureMembers:     int(outcomeCandidateMemberCardinalityV1),
	})
	if err != nil {
		return OutcomeCandidateExpectationV1{}, err
	}
	if !summary.MembersComplete {
		return OutcomeCandidateExpectationV1{}, errors.New("Outcome candidate ordinary-set summary did not retain every member")
	}
	return OutcomeCandidateExpectationV1{
		Cardinality: summary.Cardinality, Members: append([]string(nil), summary.Members...),
		OrdinarySetSHA256: summary.SetSHA256,
	}, nil
}

func equalStringsV3(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
