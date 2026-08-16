package main

import (
	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/compilerfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/concurrencyfixture"
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/rq5fixture"
)

// publicationCell is the smallest unit advertised by --capabilities. A
// constructor alone is not a publication capability: every workload, scale,
// and mode preregistered by the protocol must have a real implementation.
type publicationCell struct {
	WorkloadID string
	Scale      string
	Mode       string
}

type publicationWorkload struct {
	ID     string
	Scales []string
	Modes  []string
}

type publicationProfileCoverage struct {
	required    []publicationCell
	implemented []publicationCell
}

func (coverage publicationProfileCoverage) complete() bool {
	if len(coverage.required) == 0 || len(coverage.implemented) != len(coverage.required) {
		return false
	}
	required := make(map[publicationCell]struct{}, len(coverage.required))
	for _, cell := range coverage.required {
		required[cell] = struct{}{}
	}
	// Duplicate declarations cannot make an incomplete profile appear complete.
	if len(required) != len(coverage.required) {
		return false
	}
	implemented := make(map[publicationCell]struct{}, len(coverage.implemented))
	for _, cell := range coverage.implemented {
		if _, preregistered := required[cell]; !preregistered {
			return false
		}
		implemented[cell] = struct{}{}
	}
	return len(implemented) == len(required)
}

func expandPublicationWorkloads(workloads []publicationWorkload) []publicationCell {
	var cells []publicationCell
	for _, workload := range workloads {
		for _, scale := range workload.Scales {
			for _, mode := range workload.Modes {
				cells = append(cells, publicationCell{WorkloadID: workload.ID, Scale: scale, Mode: mode})
			}
		}
	}
	return cells
}

var baselinePublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "S1", Scales: []string{"SF1", "SF10"}, Modes: []string{"direct", "novel", "semantic_replay", "idempotent_replay", "normalized_rewrite_replay"}},
	{ID: "S2", Scales: []string{"SF1", "SF10"}, Modes: []string{"direct", "novel", "semantic_replay", "idempotent_replay", "normalized_rewrite_replay"}},
	{ID: "S3", Scales: []string{"1k-5k", "10k-50k", "45k-225k"}, Modes: []string{"direct", "novel", "semantic_replay", "idempotent_replay", "normalized_rewrite_replay"}},
	{ID: "S4", Scales: []string{"depth-4"}, Modes: []string{"direct", "novel", "semantic_replay"}},
	{ID: "S5", Scales: []string{"SF1", "SF10"}, Modes: []string{"direct", "novel", "semantic_replay", "idempotent_replay"}},
	{ID: "S6", Scales: []string{"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16"}, Modes: []string{"direct", "novel"}},
})

// Baseline coverage is derived from the frozen contract and from the real
// resolver Execute uses, never from a hand-kept list: a cell counts as
// implemented only when its contract entry decodes, both arm templates render
// from the Contract Index, and its Products resolve to an approvable Task.
//
// S1 and S2 satisfy that as of 2026-08-16; S3--S6 do not, so Baseline stays
// 20 of 58 and its capability stays false. The non-publication S1/tiny Pilot
// contributes nothing here and never could: it is not a frozen cell.
var baselineImplementedPublicationCells = baselineImplementedPublicationCellsFromContract()

// Scale has real handler code for its microbenchmark arms, but the frozen
// Catalog cannot currently grant a task that carries the complete
// dependency-e2e pair at the 1,035,000-Fact scale.
//
// The route ceiling that used to block this is no longer the reason. 7e705a9
// added the final_v5_exposure_scale route and the final-v5-exposure-scale-v1
// budget profile to the master Catalog, where max_queries is 8 and
// max_influence_facts is 2,500,000 -- enough to carry novel plus semantic
// replay on one task at that scale. That commit did not update this comment,
// which kept describing a gate that had already been opened. Whether the pair
// actually executes is untested, so the cells stay unimplemented on evidence,
// not on a ceiling.
// Do not register a partial scale matrix as a publication capability.
var scalePublicationRequirements = append(
	expandPublicationWorkloads([]publicationWorkload{
		{ID: "dependency-e2e", Scales: []string{
			"10k-overlap-0", "10k-overlap-50", "10k-overlap-90", "10k-overlap-100",
			"100k-overlap-0", "100k-overlap-50", "100k-overlap-90", "100k-overlap-100",
			"1035000-overlap-0", "1035000-overlap-50", "1035000-overlap-90", "1035000-overlap-100",
		}, Modes: []string{"novel", "semantic_replay"}},
		{ID: "outcome-merkle", Scales: []string{
			"10k-x1-o0", "10k-x1-o50", "10k-x1-o90", "10k-x1-o100",
			"10k-x100-o0", "10k-x100-o50", "10k-x100-o90", "10k-x100-o100",
			"10k-x10k-o0", "10k-x10k-o50", "10k-x10k-o90", "10k-x10k-o100",
			"100k-x1-o0", "100k-x1-o50", "100k-x1-o90", "100k-x1-o100",
			"100k-x100-o0", "100k-x100-o50", "100k-x100-o90", "100k-x100-o100",
			"100k-x10k-o0", "100k-x10k-o50", "100k-x10k-o90", "100k-x10k-o100",
			"1m-x1-o0", "1m-x1-o50", "1m-x1-o90", "1m-x1-o100",
			"1m-x100-o0", "1m-x100-o50", "1m-x100-o90", "1m-x100-o100",
			"1m-x10k-o0", "1m-x10k-o50", "1m-x10k-o90", "1m-x10k-o100",
		}, Modes: []string{"merkle_control"}},
	}),
	expandPublicationWorkloads([]publicationWorkload{
		{ID: "taskgate_scale_extreme", Scales: []string{"10m", "100m"}, Modes: []string{"kernel_storage_only"}},
	})...,
)

// Scale coverage is derived from the same predicates Execute dispatches on,
// so a registered cell is one the Adapter can really run. The two kernel
// workloads qualify: Outcome-Merkle and the extreme storage arm resolve from
// frozen scale specifications alone, provision no Task and bind no Product, so
// nothing about the Catalog can make or break them.
//
// dependency-e2e is deliberately withheld even though its handler exists. It
// is the one Scale arm that needs an approved Task over final_v5_exposure_scale
// and it has never executed, so it stays unimplemented on evidence. Scale
// therefore reports 38 of 62 and its capability stays false.
var scaleImplementedPublicationCells = implementedPublicationCells("scale", scalePublicationRequirements)

// artifactRealSystemValidated records whether a targeted, non-publication
// real-system run has executed all six frozen result-heavy cells end to end --
// OA approval, the public BDG query, result exposure settlement, Parquet,
// AES-GCM staging, PENDING, canonical object promotion, AVAILABLE, the
// composite Receipt/Object/Audit verification, and the independent Artifact
// Oracle. Source-controlled cell resolution is necessary but never sufficient:
// flip this only together with retained evidence of that run.
//
// That run is retained. Campaign p8-artifact-observerfix-v110-six-cell-02
// executed all six frozen cells on 2026-08-16 from clean commit 59e035ecd67c
// against contract release final-v5-contracts-v1.10: six samples, every status
// pass, every Receipt verified, every v3 acceptance non-null with zero
// unexpected calls, and non-zero Parquet and encrypted object bytes per cell.
// The evidence directory is retained under evaluation/final-v5-wsl2/raw/ and
// its sixteen first-hand files are committed. The run remains a pilot: it is
// campaign_class=pilot, publication_eligible=false, and this constant does not
// make it publication evidence.
const artifactRealSystemValidated = true

// artifactContractRuntime is the verified Contract Index. The Artifact matrix
// is derived from it rather than from a second hand-maintained table, so the
// Adapter and the contract can never disagree about which cells exist.
var artifactContractRuntime, artifactContractRuntimeErr = finalv5contracts.LoadRuntime()

var artifactPublicationRequirements = contractArtifactRequirements()

func contractArtifactRequirements() []publicationCell {
	if artifactContractRuntimeErr != nil {
		return nil
	}
	required, err := artifactContractRuntime.ArtifactRequirements()
	if err != nil {
		return nil
	}
	cells := make([]publicationCell, 0, len(required))
	for _, identity := range required {
		cells = append(cells, publicationCell{WorkloadID: identity.WorkloadID, Scale: identity.Scale, Mode: identity.Mode})
	}
	return cells
}

var artifactImplementedPublicationCells = implementedPublicationCells("artifact", artifactPublicationRequirements)

var rlsPublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "adaptive-100-v1", Scales: []string{"100-queries"}, Modes: []string{"rls", "unlimited", "bounded"}},
	{ID: "policy-denied-control", Scales: []string{"single"}, Modes: []string{"rls", "unlimited", "bounded"}},
})

var attackPublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "A-pagination", Scales: []string{"complete-to-pages", "pages-to-complete"}, Modes: []string{"direct", "novel"}},
	{ID: "B-equivalent-sql", Scales: []string{"variants-v1"}, Modes: []string{"direct", "novel"}},
	{ID: "C-request-id", Scales: []string{"same-and-different"}, Modes: []string{"novel", "semantic_replay", "idempotent_replay"}},
	{ID: "D-split-union", Scales: []string{"complete-to-split", "split-to-complete"}, Modes: []string{"direct", "novel"}},
	{ID: "E-threshold", Scales: []string{"preregistered-v1"}, Modes: []string{"direct", "novel"}},
})

var provSQLPublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "nonce-join-group", Scales: []string{"1k", "10k", "45k"}, Modes: []string{"direct", "provsql", "taskgate"}},
})

var compilerPublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "view-depth", Scales: []string{"1", "2", "4", "8", "16"}, Modes: []string{"compile"}},
	{ID: "join-sources", Scales: []string{"2", "4", "8", "16"}, Modes: []string{"compile"}},
	{ID: "limit-controls", Scales: []string{"depth-17", "sources-17"}, Modes: []string{"structured_rejection"}},
})

var concurrencyPublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "shared-root", Scales: []string{"10", "50", "100", "500"}, Modes: []string{"forced_queue_safety", "natural_contention"}},
	{ID: "serial-control", Scales: []string{"1"}, Modes: []string{"serial"}},
})

var rq5PublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: rq5fixture.WorkloadID, Scales: []string{rq5fixture.Scale}, Modes: []string{rq5fixture.BuildMode, rq5fixture.RetainedMode}},
})

var attackCapabilityManifest, attackCapabilityManifestErr = finalv5attack.Load()

// implementedPublicationCells derives advertised coverage from the same
// source-controlled predicates used by Execute. A duplicated declaration is
// therefore insufficient to turn a capability on: every protocol cell must
// also be accepted by its real handler or frozen fixture.
func implementedPublicationCells(experimentID string, required []publicationCell) []publicationCell {
	implemented := make([]publicationCell, 0, len(required))
	for _, cell := range required {
		if realPublicationCellImplemented(experimentID, cell) {
			implemented = append(implemented, cell)
		}
	}
	return implemented
}

func realPublicationCellImplemented(experimentID string, cell publicationCell) bool {
	switch experimentID {
	case "rls":
		return validRLSCell(experiment.AdapterOperation{
			ExperimentID: experimentID,
			WorkloadID:   cell.WorkloadID,
			Scale:        cell.Scale,
			Mode:         cell.Mode,
		})
	case "attack":
		if attackCapabilityManifestErr != nil {
			return false
		}
		_, found := attackCapabilityManifest.Lookup(cell.WorkloadID, cell.Scale)
		return found && validAttackMode(cell.WorkloadID, cell.Mode)
	case "provsql":
		return validProvSQLCell(experiment.AdapterOperation{
			ExperimentID: experimentID,
			WorkloadID:   cell.WorkloadID,
			Scale:        cell.Scale,
			Mode:         cell.Mode,
		})
	case "compiler":
		return compilerfixture.IsFrozenCell(cell.WorkloadID, cell.Scale, cell.Mode)
	case "concurrency":
		_, found := concurrencyfixture.Lookup(cell.WorkloadID, cell.Scale, cell.Mode)
		return found
	case "artifact":
		// A cell counts as implemented only when the frozen contract resolves
		// it, its query renders from the indexed template, its independent
		// Oracle Manifest verifies against the Contract Index, and a real
		// end-to-end run has already validated the whole six-cell profile.
		if artifactContractRuntimeErr != nil || !artifactRealSystemValidated {
			return false
		}
		resolved, err := artifactContractRuntime.ArtifactCell(cell.Scale, cell.Mode)
		if err != nil || resolved.Identity.WorkloadID != cell.WorkloadID {
			return false
		}
		if _, err := artifactContractRuntime.QueryContract(resolved); err != nil {
			return false
		}
		if _, _, err := artifactContractRuntime.OracleManifest(resolved); err != nil {
			return false
		}
		return artifactContractRuntime.VerifyProjectionPrefix() == nil
	case "scale":
		// Mirror scale.go's dispatch exactly: same parser, same mode. A cell the
		// Adapter would refuse must never be advertised as implemented.
		switch cell.WorkloadID {
		case "outcome-merkle":
			_, err := experiment.ParseOutcomeMerkleScale(cell.Scale)
			return err == nil && cell.Mode == "merkle_control"
		case "taskgate_scale_extreme":
			_, err := experiment.ParseExtremeScale(cell.Scale)
			return err == nil && cell.Mode == "kernel_storage_only"
		default:
			return false
		}
	case "rq5":
		for iteration := 1; iteration <= rq5fixture.CyclesPerDeployment; iteration++ {
			if !rq5fixture.IsCell(cell.WorkloadID, cell.Scale, cell.Mode, iteration) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

var (
	rlsImplementedPublicationCells         = implementedPublicationCells("rls", rlsPublicationRequirements)
	attackImplementedPublicationCells      = implementedPublicationCells("attack", attackPublicationRequirements)
	provSQLImplementedPublicationCells     = implementedPublicationCells("provsql", provSQLPublicationRequirements)
	compilerImplementedPublicationCells    = implementedPublicationCells("compiler", compilerPublicationRequirements)
	concurrencyImplementedPublicationCells = implementedPublicationCells("concurrency", concurrencyPublicationRequirements)
	rq5ImplementedPublicationCells         = implementedPublicationCells("rq5", rq5PublicationRequirements)
)

// publicationCoverageGates is exhaustive. A source-controlled constructor is
// necessary but no experiment can advertise true until its exact frozen
// protocol profile is also covered by the real accepted-cell definitions.
var publicationCoverageGates = map[string]publicationProfileCoverage{
	"baseline": {
		required:    baselinePublicationRequirements,
		implemented: baselineImplementedPublicationCells,
	},
	"scale": {
		required:    scalePublicationRequirements,
		implemented: scaleImplementedPublicationCells,
	},
	"artifact": {
		required:    artifactPublicationRequirements,
		implemented: artifactImplementedPublicationCells,
	},
	"rls": {
		required:    rlsPublicationRequirements,
		implemented: rlsImplementedPublicationCells,
	},
	"attack": {
		required:    attackPublicationRequirements,
		implemented: attackImplementedPublicationCells,
	},
	"provsql": {
		required:    provSQLPublicationRequirements,
		implemented: provSQLImplementedPublicationCells,
	},
	"compiler": {
		required:    compilerPublicationRequirements,
		implemented: compilerImplementedPublicationCells,
	},
	"concurrency": {
		required:    concurrencyPublicationRequirements,
		implemented: concurrencyImplementedPublicationCells,
	},
	"rq5": {
		required:    rq5PublicationRequirements,
		implemented: rq5ImplementedPublicationCells,
	},
}

func publicationCoverageGateSatisfied(experimentID string) bool {
	coverage, gated := publicationCoverageGates[experimentID]
	return gated && coverage.complete()
}
