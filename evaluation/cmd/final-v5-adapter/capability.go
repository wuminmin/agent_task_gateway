package main

import (
	"taskbound.local/agent-data-gateway/evaluation/finalv5attack"
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

// The source-controlled baseline currently implements only the non-publication
// S1/tiny Pilot. Pilot coverage is intentionally absent here: it cannot satisfy
// any SF1/SF10 or S1--S6 publication cell. Add a cell only together with its
// real workload SQL, dataset binding, task definition, oracle, and execution
// path.
var baselineImplementedPublicationCells = []publicationCell{}

// Scale has real handler code for its microbenchmark arms, but the frozen
// Catalog cannot currently grant a task that carries the complete
// dependency-e2e pair at the 1,035,000-Fact scale. In particular, the only
// route with a sufficient influence ceiling grants one query, while every
// preregistered pair contains novel plus semantic replay on the same task.
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

var scaleImplementedPublicationCells = []publicationCell{}

// The current Catalog likewise has no real result-heavy task route. Its
// largest row grant is below the 10k/100k cells, and no legal product set can
// approve the 16 distinct columns required by the x16 cells. Keep the whole
// capability false rather than advertising the one possibly routable corner.
var artifactPublicationRequirements = expandPublicationWorkloads([]publicationWorkload{
	{ID: "result-heavy", Scales: []string{"100x4", "10k-x4", "100k-x4", "100x16", "10k-x16", "100k-x16"}, Modes: []string{"novel"}},
})

var artifactImplementedPublicationCells = []publicationCell{}

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
