package finalv5contracts

import "fmt"

// BaselineExperimentID is the protocol identifier of the Baseline family.
const BaselineExperimentID = "baseline"

// BaselineCell is one frozen Baseline cell resolved from the Contract Index.
// It carries only what a measured run needs in order to execute the cell: the
// protocol coordinate, the Product and Publication the cell binds, the frozen
// query parameter, and the two arm templates. Expected digests deliberately do
// not appear -- contract v1 holds none, and a generated digest must reach a
// measured run through the generation round, never through this decoder.
type BaselineCell struct {
	Identity       CellIdentity
	SpecID         string
	ProductIDs     []string
	PublicationIDs []string
	// RenderParameterName is the frozen threshold this cell renders its two
	// arm templates with, and is empty for a workload whose parameterisation
	// this decoder does not yet render. RenderParameter is its value.
	RenderParameterName string
	RenderParameter     int64
	ExpectedRows        int64
	ExpectedColumns     int
	// QueryTemplate is the arm this cell measures: the Direct template for a
	// direct cell and the BDG template for every governed mode. BDGTemplate and
	// DirectTemplate are both always present, because the two arms are rendered
	// as a pair so their bytes can be compared.
	QueryTemplate  string
	BDGTemplate    string
	DirectTemplate string
	DirectActive   bool
	BDGActive      bool
	BDGEntrypoint  string
	ModeSemantics  string
	Warmups        int
	Samples        int
}

// BaselineCells returns every frozen Baseline cell in contract order.
func (runtime *Runtime) BaselineCells() ([]BaselineCell, error) {
	cells := make([]BaselineCell, 0, len(runtime.baseline.Cells))
	for index := range runtime.baseline.Cells {
		decoded, err := runtime.decodeBaselineCell(runtime.baseline.Cells[index])
		if err != nil {
			return nil, err
		}
		cells = append(cells, decoded)
	}
	return cells, nil
}

// BaselineCell looks one frozen cell up by its full protocol coordinate.
// Baseline, unlike Artifact, has six workloads, so the workload is part of the
// key rather than something the caller may leave implicit.
func (runtime *Runtime) BaselineCell(workloadID, scale, mode string) (BaselineCell, error) {
	cells, err := runtime.BaselineCells()
	if err != nil {
		return BaselineCell{}, err
	}
	for _, candidate := range cells {
		if candidate.Identity.WorkloadID == workloadID &&
			candidate.Identity.Scale == scale && candidate.Identity.Mode == mode {
			return candidate, nil
		}
	}
	return BaselineCell{}, fmt.Errorf("baseline contract has no cell for workload=%q scale=%q mode=%q",
		workloadID, scale, mode)
}

func (runtime *Runtime) decodeBaselineCell(source cell) (BaselineCell, error) {
	var (
		query       contractQuery
		bdg, direct contractArm
		product     contractProduct
		publication contractPublication
		setup       contractSetup
		measured    contractMeasured
		expected    contractExpected
	)
	for _, section := range []struct {
		name        string
		raw         []byte
		destination any
	}{
		{"query", source.Query, &query}, {"bdg", source.BDG, &bdg}, {"direct", source.Direct, &direct},
		{"product", source.Product, &product}, {"publication", source.Publication, &publication},
		{"setup", source.Setup, &setup}, {"measured", source.Measured, &measured},
		{"expected", source.Expected, &expected},
	} {
		if err := decodeStrictJSON(section.raw, section.destination); err != nil {
			return BaselineCell{}, fmt.Errorf("baseline cell %s/%s/%s %s section: %w",
				source.Workload, source.Scale, source.Mode, section.name, err)
		}
	}
	identity := CellIdentity{ExperimentID: BaselineExperimentID, WorkloadID: source.Workload,
		Scale: source.Scale, Mode: source.Mode}
	// A frozen Baseline cell must still be ungenerated for the same reason an
	// Artifact cell must: the generation round produces expected digests under
	// author review, and a decoder that accepted them here would let an
	// unreviewed value reach a measured comparison.
	if expected.NormalizedSchemaSHA256 != nil || expected.CanonicalResultSHA256 != nil ||
		expected.DigestGenerationStatus != notGenerated || expected.DigestReviewStatus != notApproved {
		return BaselineCell{}, fmt.Errorf("baseline cell %s carries generated digests that contract v1 must not hold", identity)
	}
	// A cell must bind at least one Product and one Publication, but not one
	// each: S4's single derived analytics Product is published from both frozen
	// ProvSQL relations, so a strict pairing rule would reject the contract.
	if len(product.IDs) == 0 || len(publication.IDs) == 0 {
		return BaselineCell{}, fmt.Errorf("baseline cell %s does not bind a Product and a Publication", identity)
	}
	if query.Template == "" || direct.Template == "" || !direct.CompleteDrain || !bdg.CompleteDrain {
		return BaselineCell{}, fmt.Errorf("baseline cell %s omits a complete-drain arm template", identity)
	}
	// The Direct arm is plain PostgreSQL and the BDG arm is the public query
	// entrypoint; exactly one of them is active, and which one is decided by the
	// mode rather than by the cell repeating itself.
	directMode := identity.Mode == "direct"
	if direct.Active != directMode || bdg.Active == directMode {
		return BaselineCell{}, fmt.Errorf("baseline cell %s activates the wrong arm for its mode", identity)
	}
	if bdg.ModeSemantics != identity.Mode {
		return BaselineCell{}, fmt.Errorf("baseline cell %s BDG arm declares mode semantics %q", identity, bdg.ModeSemantics)
	}
	// The measured arm is what the cell's query section names: a direct cell
	// measures plain PostgreSQL, every governed mode measures the public BDG
	// entrypoint. Both arm templates stay declared either way so the pair can be
	// rendered and compared.
	if bdg.Template == "" {
		return BaselineCell{}, fmt.Errorf("baseline cell %s omits its BDG arm template", identity)
	}
	wantMeasured := bdg.Template
	if directMode {
		wantMeasured = direct.Template
	}
	if query.Template != wantMeasured {
		return BaselineCell{}, fmt.Errorf("baseline cell %s measures %q but its active arm is %q",
			identity, query.Template, wantMeasured)
	}
	if !query.TotalOrderRequired && query.ResultOrdering == "" {
		return BaselineCell{}, fmt.Errorf("baseline cell %s declares no result ordering discipline", identity)
	}
	// Each Baseline workload freezes its own threshold, and the positional
	// renderer takes exactly one. Naming the parameter per workload here keeps a
	// cell whose parameterisation this decoder does not yet render from being
	// silently rendered with a zero threshold.
	renderName, renderValue := "", int64(0)
	switch identity.WorkloadID {
	case "S1", "S2":
		renderName, renderValue = "orderkey_max", query.Parameters.OrderkeyMax
	case "S6":
		// S6 parameterises on rows, like the Artifact cells that execute its
		// templates byte-identically, and its projection is carried by which of
		// the two templates the cell names rather than by the parameter.
		renderName, renderValue = "rows", query.Parameters.Rows
		if query.Parameters.Projection == "" {
			return BaselineCell{}, fmt.Errorf("baseline cell %s names no projection", identity)
		}
	}
	if renderName != "" && renderValue <= 0 {
		return BaselineCell{}, fmt.Errorf("baseline cell %s carries no frozen %s parameter", identity, renderName)
	}
	if expected.RowCount <= 0 || expected.ColumnCount <= 0 {
		return BaselineCell{}, fmt.Errorf("baseline cell %s declares an empty expected result", identity)
	}
	// S1 projects one row per admitted order key, so its expected row count and
	// its frozen parameter are the same number stated twice. Checking them
	// against each other catches a parameter edited without its result.
	if identity.WorkloadID == "S1" && expected.RowCount != query.Parameters.OrderkeyMax {
		return BaselineCell{}, fmt.Errorf("baseline cell %s expects %d rows from a %d key threshold",
			identity, expected.RowCount, query.Parameters.OrderkeyMax)
	}
	if measured.Samples <= 0 || setup.Warmups < 0 {
		return BaselineCell{}, fmt.Errorf("baseline cell %s declares no measurable sample plan", identity)
	}
	return BaselineCell{
		Identity: identity, SpecID: query.SpecID,
		ProductIDs:          append([]string(nil), product.IDs...),
		PublicationIDs:      append([]string(nil), publication.IDs...),
		RenderParameterName: renderName, RenderParameter: renderValue,
		ExpectedRows: expected.RowCount, ExpectedColumns: expected.ColumnCount,
		QueryTemplate: query.Template, BDGTemplate: bdg.Template, DirectTemplate: direct.Template,
		DirectActive: direct.Active, BDGActive: bdg.Active,
		BDGEntrypoint: bdg.Entrypoint, ModeSemantics: bdg.ModeSemantics,
		Warmups: setup.Warmups, Samples: measured.Samples,
	}, nil
}

// BaselineQueryContract renders both arms of one Baseline cell with its frozen
// orderkey threshold. The rendered strings are the exact bytes the measured
// path executes, so their digests are evidence rather than description.
func (runtime *Runtime) BaselineQueryContract(target BaselineCell) (QueryContract, error) {
	normalization, err := runtime.ContractSHA256(normalizationContractPath)
	if err != nil {
		return QueryContract{}, err
	}
	if target.RenderParameterName == "" {
		return QueryContract{}, fmt.Errorf("baseline cell %s has no renderable frozen parameter", target.Identity)
	}
	bdg, err := runtime.RenderIndexedTemplate(target.BDGTemplate, target.RenderParameter)
	if err != nil {
		return QueryContract{}, err
	}
	bdg.Role, bdg.Entrypoint, bdg.PublicTool = "bdg", EntrypointBDGQuery, PublicBDGTool
	direct, err := runtime.RenderIndexedTemplate(target.DirectTemplate, target.RenderParameter)
	if err != nil {
		return QueryContract{}, err
	}
	direct.Role, direct.Entrypoint = "direct", EntrypointDirectSQL
	return QueryContract{Cell: target.Identity, Rows: target.ExpectedRows, Columns: target.ExpectedColumns,
		BDG: bdg, Direct: direct, NormalizationSHA256: normalization}, nil
}

// BaselineRequirements returns the coordinates of every frozen Baseline cell,
// so a capability gate iterates the contract instead of a second hand-kept
// table that could silently disagree with it.
func (runtime *Runtime) BaselineRequirements() ([]CellIdentity, error) {
	cells, err := runtime.BaselineCells()
	if err != nil {
		return nil, err
	}
	identities := make([]CellIdentity, 0, len(cells))
	for _, decoded := range cells {
		identities = append(identities, decoded.Identity)
	}
	return identities, nil
}
