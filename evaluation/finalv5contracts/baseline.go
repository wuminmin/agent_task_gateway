package finalv5contracts

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

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
	// SecondaryParameter is S5's overlap branch bound. Every other Baseline
	// workload freezes exactly one threshold and leaves this zero.
	SecondaryParameter int64
	ExpectedRows       int64
	ExpectedColumns    int
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
	case "S5":
		// S5 unions two branches of the same relation at different thresholds,
		// so it is the one Baseline workload with two frozen parameters.
		renderName, renderValue = "orderkey_max", query.Parameters.OrderkeyMax
		if query.Parameters.OverlapBranchMax <= 0 {
			return BaselineCell{}, fmt.Errorf("baseline cell %s carries no overlap branch bound", identity)
		}
	case "S3":
		// S3 walks a frozen family's members, so its threshold is a member rank
		// rather than an order key.
		renderName, renderValue = "member_max", query.Parameters.MemberMax
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
		SecondaryParameter: query.Parameters.OverlapBranchMax,
		ExpectedRows:       expected.RowCount, ExpectedColumns: expected.ColumnCount,
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
	var bdg RenderedQuery
	if target.SecondaryParameter > 0 {
		// S5's governed arm is a declarative plan, not SQL.
		bdg, err = runtime.BaselinePlanContract(target)
	} else {
		bdg, err = runtime.RenderIndexedTemplate(target.BDGTemplate, target.RenderParameter)
		bdg.Role, bdg.Entrypoint, bdg.PublicTool = "bdg", EntrypointBDGQuery, PublicBDGTool
	}
	if err != nil {
		return QueryContract{}, err
	}
	direct, err := runtime.renderBaselineDirect(target)
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

// BaselinePlanContract renders Baseline S5's frozen QueryPlan. S5 is the one
// Baseline workload whose governed arm is a declarative plan rather than SQL,
// and the one that carries two thresholds, so it needs a renderer of its own:
// the positional SQL renderer takes a single parameter and would silently drop
// the overlap branch bound.
//
// The render rule is the template's own: every complete {"$parameter": name}
// object becomes a JSON integer. Anything else in the template is copied
// verbatim, so the plan the Gateway receives differs from the frozen bytes only
// where the contract says a threshold goes.
func (runtime *Runtime) BaselinePlanContract(target BaselineCell) (RenderedQuery, error) {
	if target.Identity.WorkloadID != "S5" {
		return RenderedQuery{}, fmt.Errorf("baseline cell %s does not render a QueryPlan", target.Identity)
	}
	digest, err := runtime.ContractSHA256(target.BDGTemplate)
	if err != nil {
		return RenderedQuery{}, err
	}
	template, err := runtime.readContract(target.BDGTemplate)
	if err != nil {
		return RenderedQuery{}, err
	}
	var document struct {
		Entrypoint string          `json:"entrypoint"`
		Parameters map[string]any  `json:"parameters"`
		Plan       json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal(template, &document); err != nil {
		return RenderedQuery{}, fmt.Errorf("plan template %s: %w", target.BDGTemplate, err)
	}
	if document.Entrypoint != EntrypointExecutePlan {
		return RenderedQuery{}, fmt.Errorf("plan template %s declares entrypoint %q",
			target.BDGTemplate, document.Entrypoint)
	}
	if len(document.Plan) == 0 {
		return RenderedQuery{}, fmt.Errorf("plan template %s carries no plan", target.BDGTemplate)
	}
	values := map[string]int64{
		"orderkey_max":       target.RenderParameter,
		"overlap_branch_max": target.SecondaryParameter,
	}
	for name := range document.Parameters {
		if values[name] <= 0 {
			return RenderedQuery{}, fmt.Errorf("plan template %s parameter %q has no frozen value",
				target.BDGTemplate, name)
		}
	}
	var plan any
	if err := json.Unmarshal(document.Plan, &plan); err != nil {
		return RenderedQuery{}, fmt.Errorf("plan template %s plan section: %w", target.BDGTemplate, err)
	}
	rendered, substitutions, err := substitutePlanParameters(plan, values, document.Parameters)
	if err != nil {
		return RenderedQuery{}, fmt.Errorf("plan template %s: %w", target.BDGTemplate, err)
	}
	if substitutions != len(document.Parameters) {
		return RenderedQuery{}, fmt.Errorf("plan template %s substituted %d of %d declared parameters",
			target.BDGTemplate, substitutions, len(document.Parameters))
	}
	encoded, err := json.Marshal(rendered)
	if err != nil {
		return RenderedQuery{}, err
	}
	parameters := make([]RenderedParameter, 0, len(values))
	for _, name := range []string{"orderkey_max", "overlap_branch_max"} {
		if _, declared := document.Parameters[name]; declared {
			parameters = append(parameters, RenderedParameter{
				Ordinal: len(parameters) + 1, Name: name, SQLType: "bigint",
				Literal: strconv.FormatInt(values[name], 10)})
		}
	}
	return RenderedQuery{
		Role: "bdg", Entrypoint: EntrypointExecutePlan, PublicTool: PublicExecutePlanTool,
		TemplatePath: target.BDGTemplate, TemplateSHA256: digest,
		SQL: string(encoded), SQLSHA256: digestBytes(encoded), Parameters: parameters,
	}, nil
}

// substitutePlanParameters replaces every complete {"$parameter": name} object
// with its frozen integer. An object that carries the marker together with any
// other member is refused rather than partially rendered.
func substitutePlanParameters(node any, values map[string]int64, declared map[string]any) (any, int, error) {
	switch typed := node.(type) {
	case map[string]any:
		if marker, carries := typed["$parameter"]; carries {
			if len(typed) != 1 {
				return nil, 0, errors.New("a $parameter object carries additional members")
			}
			name, ok := marker.(string)
			if !ok {
				return nil, 0, errors.New("a $parameter name is not a string")
			}
			if _, isDeclared := declared[name]; !isDeclared {
				return nil, 0, fmt.Errorf("undeclared parameter %q", name)
			}
			return values[name], 1, nil
		}
		result := make(map[string]any, len(typed))
		total := 0
		for key, child := range typed {
			value, count, err := substitutePlanParameters(child, values, declared)
			if err != nil {
				return nil, 0, err
			}
			result[key], total = value, total+count
		}
		return result, total, nil
	case []any:
		result := make([]any, len(typed))
		total := 0
		for index, child := range typed {
			value, count, err := substitutePlanParameters(child, values, declared)
			if err != nil {
				return nil, 0, err
			}
			result[index], total = value, total+count
		}
		return result, total, nil
	default:
		return node, 0, nil
	}
}

// renderBaselineDirect renders the Direct arm. S5's Direct template unions two
// branches at two thresholds, so it is the one Baseline template with a second
// positional parameter; every other workload renders through the shared
// single-parameter renderer.
func (runtime *Runtime) renderBaselineDirect(target BaselineCell) (RenderedQuery, error) {
	if target.SecondaryParameter <= 0 {
		rendered, err := runtime.RenderIndexedTemplate(target.DirectTemplate, target.RenderParameter)
		if err != nil {
			return RenderedQuery{}, err
		}
		rendered.Role, rendered.Entrypoint = "direct", EntrypointDirectSQL
		return rendered, nil
	}
	digest, err := runtime.ContractSHA256(target.DirectTemplate)
	if err != nil {
		return RenderedQuery{}, err
	}
	template, err := runtime.readContract(target.DirectTemplate)
	if err != nil {
		return RenderedQuery{}, err
	}
	text := string(template)
	if strings.Count(text, "$1") != 1 || strings.Count(text, "$2") != 1 || strings.Contains(text, "$3") {
		return RenderedQuery{}, fmt.Errorf("template %s does not carry exactly the two frozen parameters",
			target.DirectTemplate)
	}
	rendered := strings.ReplaceAll(text, "$1", strconv.FormatInt(target.RenderParameter, 10))
	rendered = strings.ReplaceAll(rendered, "$2", strconv.FormatInt(target.SecondaryParameter, 10))
	return RenderedQuery{
		Role: "direct", Entrypoint: EntrypointDirectSQL,
		TemplatePath: target.DirectTemplate, TemplateSHA256: digest,
		SQL: rendered, SQLSHA256: digestBytes([]byte(rendered)),
		Parameters: []RenderedParameter{
			{Ordinal: 1, Name: "orderkey_max", SQLType: "bigint", Literal: strconv.FormatInt(target.RenderParameter, 10)},
			{Ordinal: 2, Name: "overlap_branch_max", SQLType: "bigint", Literal: strconv.FormatInt(target.SecondaryParameter, 10)},
		},
	}, nil
}
