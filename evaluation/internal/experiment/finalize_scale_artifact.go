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
)

const (
	scaleEvidenceVersion    = "taskgate-final-v5-scale-verification-v1"
	artifactEvidenceVersion = "taskgate-final-v5-artifact-verification-v1"
	outcomeProductionPath   = "control.differenceAndUnionV5Tx+persistV5SetObjectsTx"
	kernelProductionPath    = "ordinal.BitmapSet.Difference+Union+PortableContainers"
	observerMemoryScope     = "cgroup_v2_memory_peak_including_mmap"
)

func validateScaleVerification(sample Sample) error {
	evidence := sample.ScaleVerification
	if evidence == nil || evidence.Version != scaleEvidenceVersion {
		return errors.New("scale verification evidence is absent or versioned incorrectly")
	}
	switch sample.WorkloadID {
	case "dependency-e2e":
		return validateDependencyScaleVerification(sample, evidence)
	case "outcome-merkle":
		return validateOutcomeMerkleVerification(sample, evidence)
	case "taskgate_scale_extreme":
		return validateKernelStorageVerification(sample, evidence)
	default:
		return errors.New("scale workload is not frozen")
	}
}

// ValidateScaleEvidence is the adapter-side fail-closed gate. FinalizeRun
// invokes the same strict implementation again over retained campaign data.
func ValidateScaleEvidence(sample Sample) error {
	if sample.ExperimentID != "scale" || sample.Status != "pass" {
		return errors.New("scale evidence validation requires a passing scale sample")
	}
	return validateScaleVerification(sample)
}

func validateDependencyScaleVerification(sample Sample, evidence *ScaleVerificationEvidence) error {
	spec, err := ParseDependencyScale(sample.Scale)
	if err != nil || evidence.Boundary != "dependency_e2e" || sample.KernelOnly || sample.System != "taskgate" ||
		(sample.Mode != "novel" && sample.Mode != "semantic_replay") {
		return errors.New("dependency E2E identity or boundary is invalid")
	}
	for _, digest := range []string{evidence.BindingSHA256, evidence.DatasetSHA256, evidence.CatalogSHA256,
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
	if sample.BaselineVerification.Receipt.CatalogDigest != evidence.CatalogSHA256 {
		return errors.New("signed Catalog digest differs from the deployment binding")
	}
	if err := validateObserverTransition(sample, evidence.ObserverBefore, evidence.ObserverAfter); err != nil {
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

func validateOutcomeMerkleVerification(sample Sample, evidence *ScaleVerificationEvidence) error {
	spec, err := ParseOutcomeMerkleScale(sample.Scale)
	merkle := evidence.OutcomeMerkle
	if err != nil || evidence.Boundary != "outcome_merkle_control" || merkle == nil || sample.Mode != "merkle_control" ||
		sample.System != "taskgate" || sample.KernelOnly || merkle.ProductionPath != outcomeProductionPath ||
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
		!sample.KernelOnly || sample.System != "taskgate" || kernel.ProductionPath != kernelProductionPath {
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
	if err != nil || evidence == nil || evidence.Version != artifactEvidenceVersion || sample.WorkloadID != "result-heavy" ||
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
	if evidence.DatasetProbeSHA256 != evidence.DatasetSHA256 || evidence.ExpectedRows != spec.Rows ||
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
	if err := validateObserverTransition(sample, &evidence.ObserverBefore, &evidence.ObserverAfter); err != nil {
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

// ValidateArtifactEvidence is the adapter-side fail-closed gate. Keeping the
// wrapper here prevents the production adapter and finalizer from drifting to
// two different definitions of a passing result-heavy sample.
func ValidateArtifactEvidence(sample Sample) error {
	if sample.ExperimentID != "artifact" || sample.Status != "pass" {
		return errors.New("artifact evidence validation requires a passing artifact sample")
	}
	return validateArtifactVerification(sample)
}

func validateObserverTransition(sample Sample, before, after *ObserverSnapshot) error {
	if before == nil || after == nil || before.SchemaVersion != 1 || after.SchemaVersion != 1 ||
		before.MemoryScope != observerMemoryScope || after.MemoryScope != observerMemoryScope {
		return errors.New("out-of-process observer evidence is absent or has the wrong memory scope")
	}
	delta, err := DifferenceObserver(*before, *after)
	if err != nil {
		return err
	}
	if delta.GatewayMemoryPeakBytes <= 0 || delta.OOMDelta != 0 || delta.ContainerRestartDelta != 0 ||
		delta.GatewayMemoryPeakBytes != sample.GatewayMemoryPeakBytes || delta.GatewayCPUUsecDelta != sample.GatewayCPUUsecDelta ||
		delta.GatewayNetworkRXDelta != sample.GatewayNetworkRXDelta || delta.GatewayNetworkTXDelta != sample.GatewayNetworkTXDelta ||
		delta.ControlWALBytesDelta != sample.ControlWALBytesDelta || delta.BusinessWALBytesDelta != sample.BusinessWALBytesDelta {
		return errors.New("observer delta differs from the sample or records OOM/restart")
	}
	return validateSampleObserverAccounting(sample, delta)
}

// validateSampleObserverAccounting is the finalizer-side twin of the Adapter
// gate. The finalizer deliberately re-derives rather than trusting the Adapter's
// verdict, so a sample whose accounting was never settled -- or was settled
// against a different observer transition -- cannot reach a published cell.
func validateSampleObserverAccounting(sample Sample, delta ObserverDelta) error {
	accounting := sample.ObserverAccounting
	if accounting == nil {
		return errors.New("sample carries no closed-world observer statement accounting")
	}
	if err := ValidateObserverAccounting(*accounting); err != nil {
		return err
	}
	if accounting.ObserverTotalDelta != delta.BusinessSQLDelta {
		return fmt.Errorf("statement accounting was settled against %d gateway_reader statements, this transition shows %d",
			accounting.ObserverTotalDelta, delta.BusinessSQLDelta)
	}
	// Tie the accounting back to the targeted counter the rest of the finalizer
	// reasons about, so the two cannot describe different executions.
	targeted := accounting.Plan.ExpectedVisibleCalls + accounting.Plan.ExpectedCompanionCalls
	if targeted != sample.BusinessSQLDelta {
		return fmt.Errorf("statement accounting expects %d targeted statements, the sample records %d",
			targeted, sample.BusinessSQLDelta)
	}
	return nil
}
