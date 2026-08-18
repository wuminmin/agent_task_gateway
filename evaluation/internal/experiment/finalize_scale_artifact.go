package experiment

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const (
	scaleEvidenceVersion             = "taskgate-final-v5-scale-verification-v1"
	scaleDependencyEvidenceVersionV2 = "taskgate-final-v5-scale-verification-v2"
	scaleDependencyEvidenceVersionV3 = "taskgate-final-v5-scale-verification-v3"
	scaleDependencyEvidenceVersionV4 = "taskgate-final-v5-scale-verification-v4"
	scaleDependencyEvidenceVersionV5 = "taskgate-final-v5-scale-verification-v5"
	artifactEvidenceVersionV1        = "taskgate-final-v5-artifact-verification-v1"
	artifactEvidenceVersionV2        = "taskgate-final-v5-artifact-verification-v2"
	outcomeProductionPath            = "control.differenceAndUnionV5Tx+persistV5SetObjectsTx"
	kernelProductionPath             = "ordinal.BitmapSet.Difference+Union+PortableContainers"
)

func validateScaleVerification(sample Sample) error {
	evidence := sample.ScaleVerification
	if evidence == nil {
		return errors.New("scale verification evidence is absent")
	}
	switch sample.WorkloadID {
	case "dependency-e2e":
		switch {
		case sample.SchemaVersion == SampleSchemaVersion && evidence.Version == scaleEvidenceVersion:
			return validateDependencyScaleVerificationV1(sample, evidence)
		case sample.SchemaVersion == FinalizedSampleSchemaVersion && evidence.Version == scaleDependencyEvidenceVersionV2:
			return validateDependencyScaleVerificationV2(sample, evidence)
		case sample.SchemaVersion == FinalizedSampleSchemaVersion && evidence.Version == scaleDependencyEvidenceVersionV3:
			return validateDependencyScaleVerificationV3(sample, evidence)
		case sample.SchemaVersion == FinalizedSampleSchemaVersion && evidence.Version == scaleDependencyEvidenceVersionV4:
			return validateDependencyScaleVerificationV4(sample, evidence)
		case sample.SchemaVersion == FinalizedSampleSchemaVersion && evidence.Version == scaleDependencyEvidenceVersionV5:
			return validateDependencyScaleVerificationV5(sample, evidence)
		default:
			return errors.New("dependency scale sample/evidence versions are incompatible")
		}
	case "outcome-merkle":
		if sample.SchemaVersion != SampleSchemaVersion || evidence.Version != scaleEvidenceVersion {
			return errors.New("Outcome-Merkle sample/evidence versions are incompatible")
		}
		return validateOutcomeMerkleVerification(sample, evidence)
	case "taskgate_scale_extreme":
		if sample.SchemaVersion != SampleSchemaVersion || evidence.Version != scaleEvidenceVersion {
			return errors.New("kernel/storage sample/evidence versions are incompatible")
		}
		return validateKernelStorageVerification(sample, evidence)
	default:
		return errors.New("scale workload is not frozen")
	}
}

// validateDependencyScaleVerificationV3 retains every Decision-18 dependency
// check and additionally binds the accepted operation to the finalizer's
// member-level Outcome comparison. The two ordinary-set digests are deliberately
// separate from the production radix digest retained on Sample and the receipt.
func validateDependencyScaleVerificationV3(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence.HistoryDependencyLink != nil ||
		evidence.CandidateDependencyLink != nil || evidence.RootBeforeDependencyLink != nil ||
		evidence.RootAfterDependencyLink != nil {
		return errors.New("dependency Scale evidence-v3 cannot carry evidence-v4 semantic-to-ordinal links")
	}
	if err := validateDependencyScaleVerificationDecision18(sample, evidence); err != nil {
		return err
	}
	return validateOutcomeCandidateScaleEvidenceV3(sample, evidence)
}

// validateDependencyScaleVerificationV4 keeps the evidence-v3 Outcome member
// proof and replaces only the invalid semantic/native digest equality with the
// explicit semantic-to-ordinal links. Earlier evidence versions remain frozen.
func validateDependencyScaleVerificationV4(sample Sample, evidence *ScaleVerificationEvidence) error {
	if err := validateDependencyScaleVerificationDecision18V4(sample, evidence); err != nil {
		return err
	}
	spec, err := ParseDependencyScale(sample.Scale)
	if err != nil {
		return err
	}
	if err := validateDependencyScaleLinksV1(sample, evidence, spec); err != nil {
		return err
	}
	return validateOutcomeCandidateScaleEvidenceV3(sample, evidence)
}

// validateDependencyScaleVerificationV5 retains the P51 dependency links and
// requires the P54 complete-Catalog/profile-Catalog Outcome domain link.
func validateDependencyScaleVerificationV5(sample Sample, evidence *ScaleVerificationEvidence) error {
	if err := validateDependencyScaleVerificationDecision18V4(sample, evidence); err != nil {
		return err
	}
	spec, err := ParseDependencyScale(sample.Scale)
	if err != nil {
		return err
	}
	if err := validateDependencyScaleLinksV1(sample, evidence, spec); err != nil {
		return err
	}
	if err := validateOutcomeCandidateScaleEvidenceV3(sample, evidence); err != nil {
		return err
	}
	verification := sample.TaskGateAcceptanceV3.OutcomeCandidateVerification
	if verification.Version != OutcomeCandidateVerificationV2Version || verification.DomainLink == nil ||
		verification.DomainLink.LinkedExpected.OrdinarySetSHA256 != verification.Observed.OrdinarySetSHA256 {
		return errors.New("dependency Scale evidence-v5 lacks the P54 Outcome domain link")
	}
	return nil
}

func validateOutcomeCandidateScaleEvidenceV3(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence == nil || sample.TaskGateAcceptanceV3 == nil {
		return errors.New("dependency Scale retains no accepted Outcome candidate evidence")
	}
	verification := sample.TaskGateAcceptanceV3.OutcomeCandidateVerification
	if verification == nil {
		return errors.New("dependency Scale acceptance retains no Outcome candidate member verification")
	}
	if err := verification.Validate(); err != nil {
		return fmt.Errorf("dependency Scale Outcome candidate member verification: %w", err)
	}
	if sample.BaselineVerification == nil {
		return errors.New("dependency Scale sample retains no signed receipt for Outcome members")
	}
	receiptExposure := sample.BaselineVerification.Receipt.Exposure
	if receiptExposure == nil || !validSHA256(receiptExposure.CompositeOutcomeSHA256) {
		return errors.New("dependency Scale receipt carries no signed composite Outcome member")
	}
	memberIndex := sort.SearchStrings(verification.Observed.Members, receiptExposure.CompositeOutcomeSHA256)
	if memberIndex == len(verification.Observed.Members) ||
		verification.Observed.Members[memberIndex] != receiptExposure.CompositeOutcomeSHA256 {
		return errors.New("finalizer-observed Outcome members omit this sample's signed composite")
	}
	if evidence.ExpectedOutcomeMemberCardinality != verification.Expected.Cardinality ||
		evidence.ObservedOutcomeMemberCardinality != verification.Observed.Cardinality ||
		evidence.ExpectedOutcomeCandidateSetSHA256 != verification.Expected.OrdinarySetSHA256 ||
		evidence.ObservedOutcomeCandidateSetSHA256 != verification.Observed.OrdinarySetSHA256 {
		return errors.New("retained Outcome candidate evidence differs from the finalizer-authored member comparison")
	}
	if evidence.ExpectedOutcomeMemberCardinality != outcomeCandidateMemberCardinalityV1 ||
		evidence.ObservedOutcomeMemberCardinality != evidence.ExpectedOutcomeMemberCardinality ||
		sample.ActualOutcomeFacts != evidence.ObservedOutcomeMemberCardinality ||
		sample.PredicateAtomCount != outcomeCandidateMemberCardinalityV1-1 || sample.CompositeCount != 1 ||
		!validSHA256(evidence.ExpectedOutcomeCandidateSetSHA256) ||
		!validSHA256(evidence.ObservedOutcomeCandidateSetSHA256) {
		return errors.New("dependency Scale Outcome candidate cardinality or ordinary-set identity is inconsistent")
	}
	if evidence.Version != scaleDependencyEvidenceVersionV5 &&
		evidence.ObservedOutcomeCandidateSetSHA256 != evidence.ExpectedOutcomeCandidateSetSHA256 {
		return errors.New("historical dependency Scale Outcome candidate domains differ")
	}
	return nil
}

// ValidateScaleEvidence is the adapter-side fail-closed gate. FinalizeRun
// invokes the same strict implementation again over retained campaign data.
func ValidateScaleEvidence(sample Sample) error {
	if sample.ExperimentID != "scale" || sample.Status != "pass" {
		return errors.New("scale evidence validation requires a passing scale sample")
	}
	return validateScaleVerification(sample)
}

// validateDependencyScaleVerificationV1 permanently retains the historical
// sample-v1 interpretation. Its history represented only the overlap (and was
// absent at zero overlap), and it conflated Dataset identity with the SQL
// sanity-probe result. New runtime output must never enter this branch.
func validateDependencyScaleVerificationV1(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence.HistoryDependencyLink != nil ||
		evidence.CandidateDependencyLink != nil || evidence.RootBeforeDependencyLink != nil ||
		evidence.RootAfterDependencyLink != nil {
		return errors.New("dependency Scale evidence-v1 cannot carry semantic-to-ordinal link evidence")
	}
	spec, err := ParseDependencyScale(sample.Scale)
	if err != nil || evidence.Boundary != "dependency_e2e" || sample.KernelOnly || sample.System != "taskgate" ||
		(sample.Mode != "novel" && sample.Mode != "semantic_replay") {
		return errors.New("dependency E2E identity or boundary is invalid")
	}
	for _, digest := range []string{evidence.BindingFileSHA256, evidence.BindingSHA256,
		evidence.DatasetSHA256, evidence.CatalogSHA256,
		evidence.DatasetProbeSHA256, evidence.QuerySHA256, evidence.ExpectedResultSHA256,
		evidence.CandidateDependencySHA256} {
		if !validSHA256(digest) {
			return errors.New("dependency E2E binding contains an invalid digest")
		}
	}
	if evidence.DatasetProbeSHA256 != evidence.DatasetSHA256 || evidence.ExpectedCandidateFacts != spec.CandidateFacts ||
		evidence.ObservedCandidateFacts != sample.ActualDependencyFacts || evidence.ExpectedOverlapFacts != spec.OverlapFacts ||
		evidence.ObservedOverlapFacts != spec.OverlapFacts || evidence.ExpectedRows != sample.RowCount ||
		evidence.ExpectedColumns != sample.ColumnCount || evidence.ExpectedResultSHA256 != sample.ResultSHA256 ||
		evidence.CandidateDependencySHA256 != sample.DependencySetSHA256 {
		return errors.New("dependency E2E label/result/oracle binding differs from the observed sample")
	}
	if sample.ActualDependencyFacts != spec.CandidateFacts {
		return errors.New("dependency scale label differs from signed actual cardinality")
	}
	if sample.Mode == "novel" {
		if spec.OverlapFacts == 0 {
			if evidence.HistoryDependencySHA256 != "" || validateFreshRootLedgerSnapshot(evidence.RootBefore) != nil {
				return errors.New("zero-overlap dependency cell did not begin from an empty root")
			}
		} else {
			if !validSHA256(evidence.HistoryDependencySHA256) || evidence.RootBefore.DependencyCardinality != spec.OverlapFacts ||
				evidence.RootBefore.DependencySetSHA256 != evidence.HistoryDependencySHA256 {
				return errors.New("dependency history does not match the frozen overlap oracle")
			}
			if err := validateRootLedgerSnapshot(evidence.RootBefore); err != nil {
				return err
			}
		}
	} else {
		if evidence.RootBefore.DependencyCardinality != spec.CandidateFacts ||
			evidence.RootBefore.DependencySetSHA256 != evidence.CandidateDependencySHA256 {
			return errors.New("semantic replay did not begin from the novel candidate root")
		}
		if err := validateRootLedgerSnapshot(evidence.RootBefore); err != nil {
			return err
		}
	}
	if err := validateRootLedgerSnapshot(evidence.RootAfter); err != nil {
		return err
	}
	if err := validateRootMatchesSample(evidence.RootAfter, sample); err != nil {
		return err
	}
	if evidence.RootAfter.DependencyCardinality != spec.CandidateFacts ||
		evidence.RootAfter.DependencySetSHA256 != evidence.CandidateDependencySHA256 ||
		sample.RootEpochBefore != evidence.RootBefore.Epoch || sample.RootEpochAfter != evidence.RootAfter.Epoch ||
		sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.RootBefore) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.RootAfter) {
		return errors.New("dependency root transition differs from its independent snapshots")
	}
	if err := validateBaselineVerification(sample); err != nil {
		return err
	}
	if err := validateRedactedVerifierManifest(sample); err != nil {
		return err
	}
	if err := validateDependencyScaleReceiptCatalog(sample, evidence); err != nil {
		return err
	}
	if err := validateScaleObservationV3(sample, evidence); err != nil {
		return err
	}
	if sample.Mode == "novel" {
		if sample.SemanticReplay || sample.IdempotentReplay || sample.ChargedDependencyFacts != spec.CandidateFacts-spec.OverlapFacts ||
			sample.ActualDependencyFacts-sample.ChargedDependencyFacts != spec.OverlapFacts {
			return errors.New("novel dependency charge does not equal exact candidate minus history")
		}
		if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 1, 1); err != nil {
			return err
		}
	} else {
		if !sample.SemanticReplay || sample.IdempotentReplay || sample.ChargedReleaseFacts != 0 ||
			sample.ChargedDependencyFacts != 0 || sample.ChargedOutcomeFacts != 0 || evidence.RootBefore != evidence.RootAfter {
			return errors.New("semantic replay charged or changed the complete root")
		}
		if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 0, 0); err != nil {
			return err
		}
		receipt := sample.BaselineVerification.Receipt
		if receipt.Exposure == nil || !validSHA256(evidence.SourceObservationSHA256) ||
			evidence.SourceObservationSHA256 != evidence.ReplayObservationSHA256 ||
			evidence.ReplayObservationSHA256 != receipt.Exposure.ObservationSHA256 {
			return errors.New("semantic replay observation is not bound to its novel source")
		}
	}
	observedBusiness := evidence.BusinessAfter.VisibleCalls - evidence.BusinessBefore.VisibleCalls +
		evidence.BusinessAfter.CompanionCalls - evidence.BusinessBefore.CompanionCalls
	if observedBusiness != sample.BusinessSQLDelta {
		return errors.New("dependency Business SQL delta differs from independent counters")
	}
	return nil
}

// validateDependencyScaleReceiptCatalog keeps the two Catalog identities in
// their respective domains. Evidence-v5 is produced by a profile deployment:
// the Receipt therefore names the activated profile Catalog carried by the
// independently resolved ProfileBinding. ScaleVerificationEvidence.CatalogSHA256
// continues to name the master Catalog against which LoadPublicationFile
// validates the private binding's provenance; it is not a deployment identity.
//
// Earlier evidence versions permanently retain their historical equality rule
// so old samples are never silently reinterpreted under the split deployment
// identity model.
func validateDependencyScaleReceiptCatalog(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence.Version != scaleDependencyEvidenceVersionV5 {
		if sample.BaselineVerification.Receipt.CatalogDigest != evidence.CatalogSHA256 {
			return errors.New("signed Catalog digest differs from the deployment binding")
		}
		return nil
	}
	if err := RequireProfileBinding(sample); err != nil {
		return fmt.Errorf("dependency Scale deployment Catalog identity: %w", err)
	}
	if sample.BaselineVerification.Receipt.CatalogDigest != sample.ProfileBinding.CatalogSHA256 {
		return errors.New("signed Catalog digest differs from the activated profile Catalog")
	}
	return nil
}

// validateDependencyScaleVerificationV2 is the sample-v3 Decision-18 model:
// history is the complete existing set N in every cell, the measured candidate
// is N, and RootAfter is the independently bound union 2N-5K. The Dataset
// identity and live SQL sanity probe are separate domains.
func validateDependencyScaleVerificationV2(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence.hasOutcomeCandidateEvidenceV3() ||
		(sample.TaskGateAcceptanceV3 != nil && sample.TaskGateAcceptanceV3.OutcomeCandidateVerification != nil) ||
		evidence.HistoryDependencyLink != nil ||
		evidence.CandidateDependencyLink != nil || evidence.RootBeforeDependencyLink != nil ||
		evidence.RootAfterDependencyLink != nil {
		return errors.New("dependency Scale evidence-v2 cannot carry Outcome candidate evidence-v3 members")
	}
	return validateDependencyScaleVerificationDecision18(sample, evidence)
}

func validateDependencyScaleVerificationDecision18(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence.CandidateDependencySHA256 != sample.DependencySetSHA256 {
		return errors.New("historical dependency E2E semantic/native digest equality differs from the retained sample")
	}
	if sample.Mode == "novel" {
		if evidence.RootBefore.DependencySetSHA256 != evidence.ExistingDependencySHA256 ||
			evidence.RootAfter.DependencySetSHA256 != evidence.UnionDependencySHA256 {
			return errors.New("historical dependency root digest differs from its retained oracle")
		}
	} else if evidence.RootBefore.DependencySetSHA256 != evidence.UnionDependencySHA256 {
		return errors.New("historical semantic replay root digest differs from its retained oracle")
	}
	return validateDependencyScaleVerificationDecision18V4(sample, evidence)
}

func validateDependencyScaleVerificationDecision18V4(sample Sample, evidence *ScaleVerificationEvidence) error {
	spec, err := ParseDependencyScale(sample.Scale)
	if err != nil || evidence.Boundary != "dependency_e2e" || sample.KernelOnly || sample.System != "taskgate" ||
		(sample.Mode != "novel" && sample.Mode != "semantic_replay") {
		return errors.New("dependency E2E identity or boundary is invalid")
	}
	for _, digest := range []string{evidence.BindingFileSHA256, evidence.BindingSHA256,
		evidence.DatasetSHA256, evidence.CatalogSHA256, evidence.DatasetProbeSHA256,
		evidence.QuerySHA256, evidence.ExpectedResultSHA256, evidence.ExistingDependencySHA256,
		evidence.CandidateDependencySHA256, evidence.UnionDependencySHA256} {
		if !validSHA256(digest) {
			return errors.New("dependency E2E binding contains an invalid digest")
		}
	}
	if evidence.HistoryDependencySHA256 != "" ||
		evidence.ExpectedCandidateFacts != spec.CandidateFacts ||
		evidence.ExpectedExistingFacts != spec.ExistingFacts ||
		evidence.ExpectedOverlapFacts != spec.OverlapFacts ||
		evidence.ExpectedUnionFacts != spec.UnionFacts ||
		evidence.ObservedCandidateFacts != sample.ActualDependencyFacts ||
		evidence.ObservedOverlapFacts != spec.OverlapFacts ||
		evidence.ExpectedRows != sample.RowCount || evidence.ExpectedColumns != sample.ColumnCount ||
		evidence.ExpectedResultSHA256 != sample.ResultSHA256 {
		return errors.New("dependency E2E label/result/oracle binding differs from the observed sample")
	}
	if err := validateDependencyScaleAccountingV3(sample, evidence, spec); err != nil {
		return err
	}
	if err := validateBaselineVerification(sample); err != nil {
		return err
	}
	if err := validateRedactedVerifierManifest(sample); err != nil {
		return err
	}
	if err := validateDependencyScaleReceiptCatalog(sample, evidence); err != nil {
		return err
	}
	if err := validateScaleObservationV3(sample, evidence); err != nil {
		return err
	}
	if sample.Mode == "novel" {
		if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 1, 1); err != nil {
			return err
		}
	} else {
		if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 0, 0); err != nil {
			return err
		}
		receipt := sample.BaselineVerification.Receipt
		if receipt.Exposure == nil || !validSHA256(evidence.SourceObservationSHA256) ||
			evidence.SourceObservationSHA256 != evidence.ReplayObservationSHA256 ||
			evidence.ReplayObservationSHA256 != receipt.Exposure.ObservationSHA256 {
			return errors.New("semantic replay observation is not bound to its novel source")
		}
	}
	observedBusiness := evidence.BusinessAfter.VisibleCalls - evidence.BusinessBefore.VisibleCalls +
		evidence.BusinessAfter.CompanionCalls - evidence.BusinessBefore.CompanionCalls
	if observedBusiness != sample.BusinessSQLDelta {
		return errors.New("dependency Business SQL delta differs from independent counters")
	}
	return nil
}

// validateDependencyScaleAccountingV3 is the one production-used transition
// rule exercised by the permanent twelve-cell Decision-18 table test. It keeps
// current-query facts separate from the cumulative task root: the sample binds
// the candidate, while the root snapshots bind existing and union.
func validateDependencyScaleAccountingV3(sample Sample, evidence *ScaleVerificationEvidence,
	spec DependencyScaleSpec) error {
	if sample.ActualDependencyFacts != spec.CandidateFacts ||
		evidence.ExpectedCandidateFacts != spec.CandidateFacts ||
		evidence.ExpectedExistingFacts != spec.ExistingFacts ||
		evidence.ExpectedOverlapFacts != spec.OverlapFacts ||
		evidence.ExpectedUnionFacts != spec.UnionFacts {
		return errors.New("dependency scale cardinality labels differ from Decision 18")
	}
	if err := validateRootLedgerSnapshot(evidence.RootBefore); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(evidence.RootAfter); err != nil {
		return err
	}
	if sample.RootEpochBefore != evidence.RootBefore.Epoch || sample.RootEpochAfter != evidence.RootAfter.Epoch ||
		sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.RootBefore) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.RootAfter) {
		return errors.New("dependency root transition differs from its independent snapshots")
	}
	if sample.Mode == "novel" {
		if evidence.RootBefore.DependencyCardinality != spec.ExistingFacts {
			return errors.New("novel dependency root does not begin at the complete existing set")
		}
		if evidence.RootAfter.DependencyCardinality != spec.UnionFacts {
			return errors.New("novel dependency root does not end at the independent union")
		}
		if sample.SemanticReplay || sample.IdempotentReplay ||
			sample.ChargedDependencyFacts != spec.CandidateFacts-spec.OverlapFacts ||
			sample.ActualDependencyFacts-sample.ChargedDependencyFacts != spec.OverlapFacts {
			return errors.New("novel dependency charge does not equal candidate minus overlap")
		}
		return nil
	}
	if evidence.RootBefore.DependencyCardinality != spec.UnionFacts ||
		evidence.RootAfter != evidence.RootBefore {
		return errors.New("semantic replay did not preserve the complete union root")
	}
	if !sample.SemanticReplay || sample.IdempotentReplay || sample.ChargedReleaseFacts != 0 ||
		sample.ChargedDependencyFacts != 0 || sample.ChargedOutcomeFacts != 0 {
		return errors.New("semantic replay charged or changed the complete root")
	}
	return nil
}

func validateDependencyScaleLinksV1(sample Sample, evidence *ScaleVerificationEvidence,
	spec DependencyScaleSpec) error {
	if evidence.CandidateDependencyLink == nil ||
		evidence.RootBeforeDependencyLink == nil || evidence.RootAfterDependencyLink == nil {
		return errors.New("passing dependency Scale evidence omits a semantic-to-ordinal link")
	}
	require := func(name string, link *ScaleDependencySetVerificationV1,
		role DependencyScaleSummaryRole, expectedFacts int64, expectedSemantic, production string) error {
		if link == nil {
			return fmt.Errorf("%s dependency link is absent", name)
		}
		if err := link.Validate(); err != nil {
			return fmt.Errorf("%s dependency link: %w", name, err)
		}
		if link.Role != role || link.ExpectedCardinality != expectedFacts ||
			link.ExpectedSemanticSetSHA256 != expectedSemantic ||
			link.ProductionSetSHA256 != production {
			return fmt.Errorf("%s dependency link differs from its semantic/native identities", name)
		}
		return nil
	}
	if err := require("candidate", evidence.CandidateDependencyLink,
		DependencyScaleCandidateSummaryRole, spec.CandidateFacts,
		evidence.CandidateDependencySHA256, sample.DependencySetSHA256); err != nil {
		return err
	}
	if sample.Mode == "novel" {
		if err := require("history", evidence.HistoryDependencyLink,
			DependencyScaleExistingSummaryRole, spec.ExistingFacts,
			evidence.ExistingDependencySHA256, evidence.RootBefore.DependencySetSHA256); err != nil {
			return err
		}
		if err := require("root-before", evidence.RootBeforeDependencyLink,
			DependencyScaleExistingSummaryRole, spec.ExistingFacts,
			evidence.ExistingDependencySHA256, evidence.RootBefore.DependencySetSHA256); err != nil {
			return err
		}
	} else {
		if evidence.HistoryDependencyLink != nil {
			return errors.New("semantic replay unexpectedly carries a history-query dependency link")
		}
		if err := require("root-before", evidence.RootBeforeDependencyLink,
			DependencyScaleUnionSummaryRole, spec.UnionFacts,
			evidence.UnionDependencySHA256, evidence.RootBefore.DependencySetSHA256); err != nil {
			return err
		}
	}
	return require("root-after", evidence.RootAfterDependencyLink,
		DependencyScaleUnionSummaryRole, spec.UnionFacts,
		evidence.UnionDependencySHA256, evidence.RootAfter.DependencySetSHA256)
}

func validateOutcomeMerkleVerification(sample Sample, evidence *ScaleVerificationEvidence) error {
	spec, err := ParseOutcomeMerkleScale(sample.Scale)
	merkle := evidence.OutcomeMerkle
	if err != nil || evidence.Boundary != "outcome_merkle_control" || merkle == nil || sample.Mode != "merkle_control" ||
		sample.System != "taskgate" || sample.KernelOnly || merkle.ProductionPath != outcomeProductionPath ||
		evidence.KernelStorage != nil || evidence.ObserverWindow != nil || sample.TaskGateAcceptanceV3 != nil ||
		merkle.ContentCachePolicy != "warm_immutable_content_after_fixture_prefill" ||
		merkle.OverlapRounding != "nearest_integer_half_up" {
		return errors.New("Outcome-Merkle identity or production boundary is invalid")
	}
	oracle, err := reconstructOutcomeMerkleOracle(sample.RandomSeed, sample.Scale, spec)
	if err != nil {
		return fmt.Errorf("reconstruct Outcome-Merkle ordinary-set oracle: %w", err)
	}
	for _, digest := range []string{merkle.FixtureSHA256, merkle.BackendRunSHA256, merkle.RootMemberOracleSHA256,
		merkle.CandidateMemberOracleSHA256, merkle.UnionMemberOracleSHA256, merkle.ObservedUnionMemberSHA256,
		merkle.ProductionRootSHA256,
		merkle.ProductionUnionSHA256, merkle.ReplayUnionSHA256, sample.ResultSHA256} {
		if !validSHA256(digest) {
			return errors.New("Outcome-Merkle evidence contains an invalid digest")
		}
	}
	if merkle.FixtureSHA256 != oracle.fixtureSHA256 || merkle.RootMemberOracleSHA256 != oracle.rootSHA256 ||
		merkle.CandidateMemberOracleSHA256 != oracle.candidateSHA256 ||
		merkle.UnionMemberOracleSHA256 != oracle.unionSHA256 {
		return errors.New("Outcome-Merkle ordinary-set oracle differs from independent reconstruction")
	}
	wantNovel := spec.CandidateFacts - spec.OverlapFacts
	wantUnion := spec.RootFacts + wantNovel
	if merkle.RootCardinality != spec.RootFacts || merkle.CandidateCardinality != spec.CandidateFacts ||
		merkle.OverlapCardinality != spec.OverlapFacts || merkle.NovelCardinality != wantNovel ||
		merkle.UnionCardinality != wantUnion || merkle.ProductionUnionSHA256 != merkle.ReplayUnionSHA256 ||
		merkle.UnionMemberOracleSHA256 != merkle.ObservedUnionMemberSHA256 ||
		merkle.ProductionUnionSHA256 != sample.ResultSHA256 || sample.RootTaskIDHash != "" {
		return errors.New("Outcome-Merkle scale/cardinality/result binding is inconsistent")
	}
	if merkle.BlocksLoaded <= 0 || merkle.LeavesLoaded < 0 || merkle.HashesLoaded < 0 ||
		merkle.BlocksReused <= 0 || merkle.StorageObjectsBefore < 0 ||
		merkle.StorageObjectsAfter < merkle.StorageObjectsBefore || merkle.StorageBytesBefore <= 0 ||
		merkle.StorageBytesAfter < merkle.StorageBytesBefore || merkle.LoadMS <= 0 ||
		merkle.HeapAllocBytesAfter <= 0 || merkle.DifferenceUnionMS <= 0 || merkle.PersistMS <= 0 || merkle.ReplayChangedObjects != 0 {
		return errors.New("Outcome-Merkle production telemetry is absent or incoherent")
	}
	if spec.OverlapFacts > 0 && (merkle.LeavesLoaded <= 0 || merkle.HashesLoaded <= 0) {
		return errors.New("overlapping Outcome-Merkle candidate did not load its committed leaf")
	}
	if (wantNovel == 0 && (merkle.NovelCardinality != 0 || merkle.ChangedObjects != 0 || merkle.LeavesChanged != 0)) ||
		(wantNovel > 0 && (merkle.ChangedObjects <= 0 || merkle.LeavesChanged <= 0)) {
		return errors.New("Outcome-Merkle changed-object telemetry differs from novelty")
	}
	for key, want := range map[string]int64{
		"blocks_loaded": merkle.BlocksLoaded, "leaves_loaded": merkle.LeavesLoaded,
		"hashes_loaded": merkle.HashesLoaded, "blocks_reused": merkle.BlocksReused,
		"leaves_changed": merkle.LeavesChanged, "novelty": merkle.NovelCardinality,
		"storage_bytes": merkle.StorageBytesAfter, "heap_alloc_bytes_after": merkle.HeapAllocBytesAfter,
		"replay_changed_objects": merkle.ReplayChangedObjects,
	} {
		if sample.Counters[key] != want {
			return fmt.Errorf("Outcome-Merkle counter %s differs from raw production evidence", key)
		}
	}
	for key, want := range map[string]float64{
		"outcome_radix_load": merkle.LoadMS, "outcome_radix_difference_union": merkle.DifferenceUnionMS,
		"outcome_radix_persist": merkle.PersistMS,
	} {
		if sample.DiagnosticMS[key] != want {
			return fmt.Errorf("Outcome-Merkle diagnostic %s differs from raw production evidence", key)
		}
	}
	return nil
}

type reconstructedOutcomeMerkleOracle struct {
	fixtureSHA256   string
	rootSHA256      string
	candidateSHA256 string
	unionSHA256     string
}

type reconstructedOutcomeRoot struct {
	members [][sha256.Size]byte
	digest  string
	err     error
}

var reconstructedOutcomeRoots sync.Map
var reconstructedOutcomeOracles sync.Map

func reconstructOutcomeMerkleOracle(seed int64, scale string,
	spec OutcomeMerkleScaleSpec) (reconstructedOutcomeMerkleOracle, error) {
	key := strconv.FormatInt(seed, 10) + "\x00" + scale
	if cached, ok := reconstructedOutcomeOracles.Load(key); ok {
		return cached.(reconstructedOutcomeMerkleOracle), nil
	}
	root := reconstructOutcomeRoot(seed, spec.RootFacts)
	if root.err != nil {
		return reconstructedOutcomeMerkleOracle{}, root.err
	}
	candidate := make([][sha256.Size]byte, 0, spec.CandidateFacts)
	seenCandidate := make(map[[sha256.Size]byte]bool, spec.CandidateFacts)
	for index := int64(0); index < spec.OverlapFacts; index++ {
		member := deterministicOutcomeOracleMember("root", seed, index)
		if seenCandidate[member] {
			return reconstructedOutcomeMerkleOracle{}, errors.New("deterministic overlap fixture contains a duplicate")
		}
		seenCandidate[member] = true
		candidate = append(candidate, member)
	}
	for index := int64(0); int64(len(candidate)) < spec.CandidateFacts; index++ {
		member := deterministicOutcomeOracleMember("candidate", seed, index)
		rootIndex := sort.Search(len(root.members), func(position int) bool {
			return bytes.Compare(root.members[position][:], member[:]) >= 0
		})
		if (rootIndex < len(root.members) && root.members[rootIndex] == member) || seenCandidate[member] {
			continue
		}
		seenCandidate[member] = true
		candidate = append(candidate, member)
	}
	sort.Slice(candidate, func(left, right int) bool {
		return bytes.Compare(candidate[left][:], candidate[right][:]) < 0
	})
	candidateDigest := ordinaryOutcomeOracleDigest(candidate)
	unionDigest := ordinaryOutcomeOracleUnionDigest(root.members, candidate)
	fixtureDigest := sha256Hex([]byte(strings.Join([]string{"TASKGATE-FINAL-V5-OUTCOME-FIXTURE-V1",
		strconv.FormatInt(seed, 10), scale, root.digest, candidateDigest, unionDigest}, "\x00")))
	result := reconstructedOutcomeMerkleOracle{fixtureSHA256: fixtureDigest, rootSHA256: root.digest,
		candidateSHA256: candidateDigest, unionSHA256: unionDigest}
	reconstructedOutcomeOracles.Store(key, result)
	return result, nil
}

func reconstructOutcomeRoot(seed, facts int64) reconstructedOutcomeRoot {
	key := strconv.FormatInt(seed, 10) + "\x00" + strconv.FormatInt(facts, 10)
	if cached, ok := reconstructedOutcomeRoots.Load(key); ok {
		return cached.(reconstructedOutcomeRoot)
	}
	members := make([][sha256.Size]byte, facts)
	for index := int64(0); index < facts; index++ {
		members[index] = deterministicOutcomeOracleMember("root", seed, index)
	}
	sort.Slice(members, func(left, right int) bool {
		return bytes.Compare(members[left][:], members[right][:]) < 0
	})
	var result reconstructedOutcomeRoot
	for index := 1; index < len(members); index++ {
		if members[index-1] == members[index] {
			result.err = errors.New("deterministic root fixture contains a collision")
			reconstructedOutcomeRoots.Store(key, result)
			return result
		}
	}
	result.members, result.digest = members, ordinaryOutcomeOracleDigest(members)
	reconstructedOutcomeRoots.Store(key, result)
	return result
}

func deterministicOutcomeOracleMember(kind string, seed, index int64) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-OUTCOME-MEMBER-V1\x00" + kind + "\x00"))
	var encoded [16]byte
	binary.BigEndian.PutUint64(encoded[:8], uint64(seed))
	binary.BigEndian.PutUint64(encoded[8:], uint64(index))
	_, _ = hash.Write(encoded[:])
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func ordinaryOutcomeOracleDigest(members [][sha256.Size]byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ORDINARY-HASH-SET-ORACLE-V1\x00"))
	for _, member := range members {
		writeOutcomeOracleMember(hash, member)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func ordinaryOutcomeOracleUnionDigest(root, candidate [][sha256.Size]byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("TASKGATE-FINAL-V5-ORDINARY-HASH-SET-ORACLE-V1\x00"))
	left, right := 0, 0
	for left < len(root) || right < len(candidate) {
		switch {
		case left == len(root):
			writeOutcomeOracleMember(hash, candidate[right])
			right++
		case right == len(candidate):
			writeOutcomeOracleMember(hash, root[left])
			left++
		default:
			comparison := bytes.Compare(root[left][:], candidate[right][:])
			if comparison < 0 {
				writeOutcomeOracleMember(hash, root[left])
				left++
			} else if comparison > 0 {
				writeOutcomeOracleMember(hash, candidate[right])
				right++
			} else {
				writeOutcomeOracleMember(hash, root[left])
				left++
				right++
			}
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeOutcomeOracleMember(hash interface{ Write([]byte) (int, error) }, member [sha256.Size]byte) {
	var text [sha256.Size * 2]byte
	hex.Encode(text[:], member[:])
	_, _ = hash.Write(text[:])
}

func validateKernelStorageVerification(sample Sample, evidence *ScaleVerificationEvidence) error {
	facts, err := ParseExtremeScale(sample.Scale)
	kernel := evidence.KernelStorage
	if err != nil || evidence.Boundary != "kernel_storage_only" || kernel == nil || sample.Mode != "kernel_storage_only" ||
		!sample.KernelOnly || sample.System != "taskgate" || kernel.ProductionPath != kernelProductionPath ||
		evidence.OutcomeMerkle != nil || evidence.ObserverWindow != nil || sample.TaskGateAcceptanceV3 != nil {
		return errors.New("kernel/storage identity or boundary is invalid")
	}
	for _, digest := range []string{kernel.FixtureSHA256, kernel.RunIdentitySHA256, kernel.CandidateSHA256,
		kernel.DifferenceSHA256, kernel.UnionSHA256, kernel.RoundTripSHA256, sample.ResultSHA256} {
		if !validSHA256(digest) {
			return errors.New("kernel/storage evidence contains an invalid digest")
		}
	}
	if kernel.ExpectedCardinality != facts || kernel.CandidateCardinality != facts || kernel.DifferenceCardinality != facts ||
		kernel.UnionCardinality != facts || kernel.CandidateSHA256 != kernel.DifferenceSHA256 ||
		kernel.CandidateSHA256 != kernel.UnionSHA256 || kernel.UnionSHA256 != kernel.RoundTripSHA256 ||
		kernel.UnionSHA256 != sample.ResultSHA256 || sample.RootTaskIDHash != "" ||
		kernel.SegmentCount <= 0 || kernel.ContainerCount <= 0 || kernel.StorageBytes <= 0 ||
		kernel.AllocatedBytes <= 0 || kernel.Allocations <= 0 || kernel.HeapAllocBytesAfter <= 0 ||
		kernel.DifferenceMS <= 0 || kernel.UnionMS <= 0 || kernel.CardinalityMS <= 0 || kernel.StorageRoundTripMS <= 0 {
		return errors.New("kernel/storage cardinality, digest, allocation, or timing evidence is incoherent")
	}
	for key, want := range map[string]int64{
		"candidate_facts": facts, "difference_facts": facts, "union_facts": facts,
		"segments": kernel.SegmentCount, "containers": kernel.ContainerCount,
		"storage_bytes": kernel.StorageBytes, "alloc_bytes": kernel.AllocatedBytes,
		"alloc_objects": kernel.Allocations, "heap_alloc_bytes_after": kernel.HeapAllocBytesAfter,
	} {
		if sample.Counters[key] != want {
			return fmt.Errorf("kernel/storage counter %s differs from raw production evidence", key)
		}
	}
	return nil
}

func validateArtifactVerification(sample Sample) error {
	evidence := sample.ArtifactVerification
	spec, err := ParseArtifactScale(sample.Scale)
	if err != nil || validateArtifactDatasetIdentity(sample, evidence) != nil || sample.WorkloadID != "result-heavy" ||
		sample.Mode != "novel" || sample.System != "taskgate" || sample.KernelOnly {
		return errors.New("artifact identity or verification version is invalid")
	}
	for _, digest := range []string{evidence.BindingSHA256, evidence.DatasetSHA256, evidence.CatalogSHA256,
		evidence.DatasetProbeSHA256, evidence.QuerySHA256, evidence.ExpectedResultSHA256,
		evidence.ObservedResultSHA256} {
		if !validSHA256(digest) {
			return errors.New("artifact binding contains an invalid digest")
		}
	}
	if evidence.ExpectedRows != spec.Rows ||
		evidence.ExpectedColumns != spec.Columns || evidence.ObservedRows != sample.RowCount ||
		evidence.ObservedColumns != sample.ColumnCount || evidence.ObservedResultSHA256 != sample.ResultSHA256 ||
		evidence.ExpectedRows != sample.RowCount || evidence.ExpectedColumns != sample.ColumnCount ||
		evidence.ExpectedResultSHA256 != sample.ResultSHA256 {
		return errors.New("artifact NxC/result binding differs from the completely drained result")
	}
	if err := validateBaselineVerification(sample); err != nil {
		return err
	}
	if err := validateRedactedVerifierManifest(sample); err != nil {
		return err
	}
	if sample.BaselineVerification.Receipt.CatalogDigest != evidence.CatalogSHA256 {
		return errors.New("artifact signed Catalog digest differs from the deployment binding")
	}
	if err := validateFreshRootLedgerSnapshot(evidence.RootBefore); err != nil {
		return err
	}
	if err := validateRootLedgerSnapshot(evidence.RootAfter); err != nil {
		return err
	}
	if err := validateRootMatchesSample(evidence.RootAfter, sample); err != nil {
		return err
	}
	if sample.RootEpochBefore != evidence.RootBefore.Epoch || sample.RootEpochAfter != evidence.RootAfter.Epoch ||
		sample.RootSetSHA256Before != rootLedgerSetSHA256(evidence.RootBefore) ||
		sample.RootSetSHA256After != rootLedgerSetSHA256(evidence.RootAfter) {
		return errors.New("artifact root transition differs from independent snapshots")
	}
	if err := validateBusinessSQLTransition(evidence.BusinessBefore, evidence.BusinessAfter, 1, 1); err != nil {
		return err
	}
	if sample.BusinessSQLDelta != 2 || sample.SemanticReplay || sample.IdempotentReplay {
		return errors.New("artifact execution markers or Business SQL delta are inconsistent")
	}
	if err := validateArtifactObservationV3(sample, evidence); err != nil {
		return err
	}
	for _, name := range []string{"parquet_encode_encrypt", "local_staging_sync", "staging_object_put", "receipt_signing",
		"canonical_object_stat", "canonical_object_copy", "canonical_object_hash_verify", "mark_available"} {
		if sample.DiagnosticMS[name] <= 0 {
			return fmt.Errorf("artifact production diagnostic %s is absent or zero", name)
		}
	}
	if sample.ParquetBytes <= 0 || sample.EncryptedObjectBytes <= 0 || sample.GatewayMemoryPeakBytes <= 0 {
		return errors.New("artifact byte or memory measurement is absent")
	}
	return nil
}

// validateArtifactDatasetIdentity is the wire-version seam for the Dataset
// correction. sample-v1/evidence-v1 permanently retains the historical
// dataset==probe assertion. sample-v3/evidence-v2 records the typed full-Dataset
// identity and deployment SQL sanity probe independently; equality is neither
// required nor evidence of agreement between those different domains.
func validateArtifactDatasetIdentity(sample Sample, evidence *ArtifactVerificationEvidence) error {
	if evidence == nil || !validSHA256(evidence.DatasetSHA256) || !validSHA256(evidence.DatasetProbeSHA256) {
		return errors.New("artifact Dataset identity or deployment probe is invalid")
	}
	switch sample.SchemaVersion {
	case SampleSchemaVersion:
		if evidence.Version != artifactEvidenceVersionV1 || evidence.DatasetSHA256 != evidence.DatasetProbeSHA256 {
			return errors.New("sample-v1 requires evidence-v1 with its historical dataset/probe identity")
		}
	case FinalizedSampleSchemaVersion:
		if evidence.Version != artifactEvidenceVersionV2 {
			return errors.New("sample-v3 requires artifact evidence-v2")
		}
	default:
		return errors.New("artifact Dataset identity has no semantics for this sample version")
	}
	return nil
}

// ValidateArtifactEvidence is the adapter-side fail-closed gate. Keeping the
// wrapper here prevents the production adapter and finalizer from drifting to
// two different definitions of a passing result-heavy sample.
func ValidateArtifactEvidence(sample Sample) error {
	if sample.ExperimentID != "artifact" || sample.Status != "pass" {
		return errors.New("artifact evidence validation requires a passing artifact sample")
	}
	return validateArtifactVerification(sample)
}

// validateArtifactObservationV3 is the artifact arm's observer gate after the v3
// cutover.
//
// # What it does and does not establish
//
// It does NOT re-derive acceptance. Acceptance for a v3 path is
// FinalizeTaskGateObservationV3, which needs the verified receipt, the frozen
// contracts, the activated Catalog, the retained qualification and the Control
// Store -- none of which a Sample carries, and deliberately so: a sample that
// carried them would be carrying the material its own claim was checked against.
//
// What it establishes is that this sample IS the one that was accepted. The
// finalizer's own record is present; the window it was settled over is the
// window the sample retains; and the numbers the sample reports elsewhere -- the
// Business SQL delta, the Gateway resource counters -- are the ones that window
// and that record produce. A sample assembled from one run's receipt and another
// run's window fails here.
func validateArtifactObservationV3(sample Sample, evidence *ArtifactVerificationEvidence) error {
	if sample.SchemaVersion == FinalizedSampleSchemaVersion && sample.TaskGateAcceptanceV3 != nil {
		if err := requireArtifactContractIdentityV3(sample, evidence,
			sample.TaskGateAcceptanceV3.Operation.ContractIdentity); err != nil {
			return err
		}
	}
	return validateAcceptedObservationV3(sample, evidence.ObserverWindow, PathPairedNovel, "artifact")
}

// requireArtifactContractIdentityV3 closes the public Bridge binding acquired
// independently by the finalizer against the Adapter-retained evidence. This is
// the Decision-19 replacement for the deleted private Artifact section: it
// binds Dataset, probe, Catalog and result contract without reintroducing a
// private dependency oracle.
func requireArtifactContractIdentityV3(sample Sample, evidence *ArtifactVerificationEvidence,
	contractIdentity string) error {
	if evidence == nil || !validSHA256(evidence.BindingSHA256) {
		return errors.New("Artifact evidence carries no public deployment binding identity")
	}
	parts := strings.Split(contractIdentity, ":")
	if len(parts) != 7 || strings.TrimSpace(parts[0]) == "" || parts[0] != strings.TrimSpace(parts[0]) ||
		!validSHA256(parts[1]) {
		return errors.New("Artifact acceptance has no exact contract release/index identity")
	}
	bindingSHA256, bindingOK := strings.CutPrefix(parts[2], "binding=")
	datasetSHA256, datasetOK := strings.CutPrefix(parts[3], "dataset=")
	probeSHA256, probeOK := strings.CutPrefix(parts[4], "probe=")
	catalogSHA256, catalogOK := strings.CutPrefix(parts[5], "catalog=")
	expectedOperationID := sampleOperationIDV3(sample)
	if !bindingOK || !validSHA256(bindingSHA256) || bindingSHA256 != evidence.BindingSHA256 ||
		!datasetOK || !validSHA256(datasetSHA256) || datasetSHA256 != evidence.DatasetSHA256 ||
		!probeOK || !validSHA256(probeSHA256) || probeSHA256 != evidence.DatasetProbeSHA256 ||
		!catalogOK || !validSHA256(catalogSHA256) || catalogSHA256 != evidence.CatalogSHA256 ||
		parts[6] != expectedOperationID {
		return fmt.Errorf("Artifact acceptance does not bind sample %s and public deployment binding %s",
			expectedOperationID, shortDigest(evidence.BindingSHA256))
	}
	return nil
}

// validateScaleObservationV3 closes the Scale dependency path over the same v3
// retained-window contract as Artifact. The current Sample wire schema has no
// legacy snapshot or accounting members; strict decoding rejects those before
// an active validator can see the sample.
func validateScaleObservationV3(sample Sample, evidence *ScaleVerificationEvidence) error {
	if evidence.ObserverWindow == nil {
		return errors.New("the dependency Scale sample retains no v3 observer window")
	}
	if evidence.OutcomeMerkle != nil || evidence.KernelStorage != nil {
		return errors.New("the dependency Scale sample mixes a governed operation with control-only evidence")
	}
	if sample.TaskGateAcceptanceV3 != nil {
		if err := requireScaleContractIdentityV3(sample, evidence,
			sample.TaskGateAcceptanceV3.Operation.ContractIdentity); err != nil {
			return err
		}
	}
	expectedPath := PathPairedNovel
	if sample.Mode == "semantic_replay" {
		expectedPath = PathSemanticReplay
	}
	return validateAcceptedObservationV3(sample, *evidence.ObserverWindow, expectedPath, "dependency Scale")
}

type scaleContractIdentityV3 struct {
	release, indexSHA256, bindingFileSHA256, bindingSectionSHA256, operationID string
}

// requireScaleContractIdentityV3 closes the public/private resolver identity
// against the two independently retained binding identities. The resolver emits
// one strict five-part form; accepting an arbitrary prefix plus the right cell
// suffix would discard the file and section identities after live acceptance.
func requireScaleContractIdentityV3(sample Sample, evidence *ScaleVerificationEvidence,
	contractIdentity string) error {
	parts := strings.Split(contractIdentity, ":")
	if len(parts) != 5 || strings.TrimSpace(parts[0]) == "" || parts[0] != strings.TrimSpace(parts[0]) ||
		!validSHA256(parts[1]) {
		return errors.New("the dependency Scale acceptance has no exact contract release/index identity")
	}
	fileSHA256, fileOK := strings.CutPrefix(parts[2], "binding-file=")
	sectionSHA256, sectionOK := strings.CutPrefix(parts[3], "binding-section=")
	identity := scaleContractIdentityV3{
		release: parts[0], indexSHA256: parts[1], bindingFileSHA256: fileSHA256,
		bindingSectionSHA256: sectionSHA256, operationID: parts[4],
	}
	if !fileOK || !sectionOK || !validSHA256(identity.bindingFileSHA256) ||
		!validSHA256(identity.bindingSectionSHA256) {
		return errors.New("the dependency Scale acceptance has no exact private binding file/section identity")
	}
	expectedOperationID := sampleOperationIDV3(sample)
	if identity.operationID != expectedOperationID ||
		identity.bindingFileSHA256 != evidence.BindingFileSHA256 ||
		identity.bindingSectionSHA256 != evidence.BindingSHA256 {
		return fmt.Errorf("the dependency Scale acceptance contract identity does not bind sample %s and private file/section %s/%s",
			expectedOperationID, shortDigest(evidence.BindingFileSHA256), shortDigest(evidence.BindingSHA256))
	}
	return nil
}

// validateAcceptedObservationV3 binds a retained observer window to the
// finalizer output carried by the same sample. Acceptance itself is not
// re-derived here: that requires trusted deployment material which the sample
// deliberately does not contain. This gate instead rejects any post-acceptance
// splice between a different path, classifier, window, delta, or resource
// reading.
func validateAcceptedObservationV3(sample Sample, window ObserverWindowV2,
	expectedPath GatewayPathKind, subject string) error {
	accepted := sample.TaskGateAcceptanceV3
	if accepted == nil {
		return fmt.Errorf("the %s sample carries no v3 acceptance record; it is accepted by the finalizer, "+
			"not by the Adapter that produced it", subject)
	}
	if sample.BaselineVerification == nil {
		return fmt.Errorf("the %s sample retains no receipt for its v3 acceptance", subject)
	}
	receiptSHA256, err := queryreceipt.DocumentSHA256(sample.BaselineVerification.Receipt)
	if err != nil {
		return fmt.Errorf("identify the retained %s receipt: %w", subject, err)
	}
	if !validSHA256(accepted.ReceiptSHA256) || accepted.ReceiptSHA256 != receiptSHA256 ||
		sample.ReceiptSHA256 != receiptSHA256 {
		return fmt.Errorf("the %s acceptance receipt %s, retained receipt %s, and sample receipt %s are not one identity",
			subject, shortDigest(accepted.ReceiptSHA256), shortDigest(receiptSHA256),
			shortDigest(sample.ReceiptSHA256))
	}
	if err := accepted.Operation.Validate(); err != nil {
		return fmt.Errorf("the %s acceptance operation is invalid: %w", subject, err)
	}
	if err := accepted.Plan.Validate(); err != nil {
		return fmt.Errorf("the %s acceptance plan is invalid: %w", subject, err)
	}
	planSHA256, err := accepted.Plan.SHA256()
	if err != nil {
		return fmt.Errorf("digest the %s acceptance plan: %w", subject, err)
	}
	if accepted.PlanSHA256 != planSHA256 {
		return fmt.Errorf("the %s acceptance plan digest is %s, the retained plan hashes to %s", subject,
			shortDigest(accepted.PlanSHA256), shortDigest(planSHA256))
	}
	if accepted.Operation.PathKind != expectedPath || accepted.Plan.PathKind != expectedPath {
		return fmt.Errorf("the %s operation requires path_kind %s, the acceptance operation/plan carry %s/%s",
			subject, expectedPath, accepted.Operation.PathKind, accepted.Plan.PathKind)
	}
	expectedOperationID := sampleOperationIDV3(sample)
	if accepted.Operation.OperationID != expectedOperationID ||
		!strings.HasSuffix(accepted.Operation.ContractIdentity, ":"+expectedOperationID) {
		return fmt.Errorf("the %s acceptance operation %q does not bind sample coordinate %q",
			subject, accepted.Operation.OperationID, expectedOperationID)
	}
	if accepted.Operation.ExpectedSchemaDigest != accepted.Plan.ExpectedSchemaDigest ||
		accepted.Operation.AttestationFootprintSHA256 != accepted.Plan.AttestationFootprintSHA256 {
		return fmt.Errorf("the %s acceptance operation and plan disagree on their schema qualification", subject)
	}
	if accepted.ExpectedSchemaDigest != accepted.Plan.ExpectedSchemaDigest ||
		accepted.ExpectedSchemaEntries != accepted.Plan.ExpectedSchemaEntries {
		return fmt.Errorf("the %s acceptance record and plan disagree on ExpectedSchema", subject)
	}
	classifierBinding, err := classifierBindingSHA256(accepted.Operation, planSHA256,
		accepted.ClassifierManifestSHA256)
	if err != nil {
		return fmt.Errorf("bind the %s acceptance classifier: %w", subject, err)
	}
	if accepted.ClassifierBindingSHA256 != classifierBinding {
		return fmt.Errorf("the %s acceptance classifier binding is %s, its operation/plan/manifest bind to %s",
			subject, shortDigest(accepted.ClassifierBindingSHA256), shortDigest(classifierBinding))
	}
	if err := validateInternalExpectation(accepted.InternalExpectation); err != nil {
		return fmt.Errorf("the %s acceptance record has an invalid internal expectation: %w", subject, err)
	}
	if err := requireSameInternalExpectation(accepted.InternalExpectation,
		accepted.Plan.InternalExpectation); err != nil {
		return fmt.Errorf("the %s acceptance record and plan disagree on internal expectations: %w", subject, err)
	}
	if err := accepted.Delta.Accept(accepted.Plan); err != nil {
		return fmt.Errorf("the %s acceptance delta does not settle its plan: %w", subject, err)
	}
	if err := window.ValidateInterval(); err != nil {
		return fmt.Errorf("the retained %s observer window is not one continuous interval: %w", subject, err)
	}
	windowSHA256, err := window.SHA256()
	if err != nil {
		return fmt.Errorf("digest the retained %s observer window: %w", subject, err)
	}
	if accepted.ObserverWindowID != window.Before.ObserverWindowID ||
		accepted.ObserverWindowSHA256 != windowSHA256 {
		return fmt.Errorf("the retained %s observer window %s/%s is not the window the finalizer accepted %s/%s",
			subject, shortDigest(window.Before.ObserverWindowID), shortDigest(windowSHA256),
			shortDigest(accepted.ObserverWindowID), shortDigest(accepted.ObserverWindowSHA256))
	}
	// The classification the window was opened under has to be the one the
	// finalizer accepted it by. ObserverWindowV2.Delta enforces this during
	// acceptance; restating it against the RETAINED pair is what stops a sample
	// pairing an accepted record with a different window afterwards.
	if window.Before.ClassifierManifestSHA256 != accepted.ClassifierManifestSHA256 {
		return fmt.Errorf("the retained observer window was opened under classifier manifest %s, "+
			"the finalizer accepted %s", shortDigest(window.Before.ClassifierManifestSHA256),
			shortDigest(accepted.ClassifierManifestSHA256))
	}
	// The plan's targets and the sample's targeted counter are two independent
	// readings of the same two statements: one derived from the path, one counted
	// against the bound relations. Requiring them equal is what ties the closed
	// world back to the counter the rest of the finalizer reasons about.
	if targeted := accepted.Plan.ExpectedVisibleCalls + accepted.Plan.ExpectedCompanionCall; targeted !=
		sample.BusinessSQLDelta {
		return fmt.Errorf("the accepted %s plan settles %d targeted statements, the sample records %d",
			subject, targeted, sample.BusinessSQLDelta)
	}
	if total := window.After.Total - window.Before.Total; total != accepted.Delta.Total {
		return fmt.Errorf("the retained %s window moved the role total by %d, the acceptance record "+
			"settled %d", subject, total, accepted.Delta.Total)
	}
	resource, err := window.ResourceDelta()
	if err != nil {
		return err
	}
	for _, counter := range []struct {
		name             string
		observed, stated int64
	}{
		{"gateway memory peak", resource.GatewayMemoryPeakBytes, sample.GatewayMemoryPeakBytes},
		{"gateway cpu", resource.GatewayCPUUsecDelta, sample.GatewayCPUUsecDelta},
		{"gateway network rx", resource.GatewayNetworkRXDelta, sample.GatewayNetworkRXDelta},
		{"gateway network tx", resource.GatewayNetworkTXDelta, sample.GatewayNetworkTXDelta},
		{"control WAL", resource.ControlWALBytesDelta, sample.ControlWALBytesDelta},
		{"business WAL", resource.BusinessWALBytesDelta, sample.BusinessWALBytesDelta},
	} {
		if counter.observed != counter.stated {
			return fmt.Errorf("the retained %s window reports %s %d, the sample states %d",
				subject, counter.name, counter.observed, counter.stated)
		}
	}
	return nil
}

func sampleOperationIDV3(sample Sample) string {
	return strings.Join([]string{sample.ExperimentID, sample.WorkloadID, sample.Scale, sample.Mode}, "/")
}
