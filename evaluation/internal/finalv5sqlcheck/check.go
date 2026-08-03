package finalv5sqlcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

// ManifestRecord names the executability manifest.
const ManifestRecord = "taskgate-final-v5-contract-sql-executability-v1"

// ProbeVersion is the probe_version the contract-indexed dataset probe must
// report. It is part of the probe's own output contract, not a new declaration.
const ProbeVersion = "taskgate-final-v5-benchmark-probe-v1"

// probeRequiredKeys are the members the dataset fingerprint is composed of. A
// probe that parses but silently drops a member would change the fingerprint's
// meaning without changing its shape, so the members are checked by name.
var probeRequiredKeys = []string{"probe_version", "database", "server_version_num", "collation",
	"provsql", "exposure_scale", "result_heavy", "dependency_overlap_checks", "depth4"}

// renderScales are the row parameters every Direct template is rendered at. The
// smallest legal value proves the statement parses and plans; the declared
// Artifact scales prove the exact bytes a measured run would issue.
var renderScales = []int64{1, 100, 10000, 100000}

// Manifest is the non-sensitive record of one executability run. It carries
// identities, digests and statuses only: no DSN, password, token, business row,
// object key or SQL parameter identity.
type Manifest struct {
	SchemaVersion      int             `json:"schema_version"`
	Record             string          `json:"record"`
	ContractRelease    string          `json:"contract_release"`
	ContractIndexSHA   string          `json:"contract_index_sha256"`
	PostgreSQLVersion  string          `json:"postgresql_version"`
	PostgreSQLVersionN string          `json:"postgresql_server_version_num"`
	Checked            []ArtifactCheck `json:"checked_artifacts"`
	CheckedCount       int             `json:"checked_artifact_count"`
	RenderedCellCount  int             `json:"rendered_cell_count"`
	FailedCount        int             `json:"failed_artifact_count"`
	Status             string          `json:"status"`
}

// ArtifactCheck is one contract-indexed artifact and what was proven about it.
type ArtifactCheck struct {
	Kind              string `json:"kind"`
	Path              string `json:"path"`
	SHA256            string `json:"sha256"`
	CheckType         string `json:"check_type"`
	RenderedCells     int    `json:"rendered_cells"`
	ParseStatus       string `json:"parse_status"`
	ExecuteStatus     string `json:"execute_compile_status"`
	ResultShapeStatus string `json:"result_shape_status"`
	Status            string `json:"status"`
	Detail            string `json:"detail,omitempty"`
}

const (
	statusPass    = "pass"
	statusFail    = "fail"
	statusSkipped = "not_applicable"
)

// Run executes every SQL and plan artifact the Contract Index names. The index
// is the only source of what to check, so a newly indexed artifact is covered
// without editing this code.
func Run(ctx context.Context, runtime *finalv5contracts.Runtime, adminDSN string) (Manifest, error) {
	artifacts, err := runtime.IndexedArtifacts()
	if err != nil {
		return Manifest{}, err
	}
	generator, err := runtime.DatasetGeneratorSQL()
	if err != nil {
		return Manifest{}, err
	}
	manifest := Manifest{SchemaVersion: 1, Record: ManifestRecord,
		ContractRelease: runtime.ContractRelease(), ContractIndexSHA: runtime.IndexSHA256()}

	// 6.1 The generator is checked by being the thing that builds the database
	// every other check runs against. A generator that half-applies fails here.
	database, err := Provision(ctx, adminDSN, generator)
	if err != nil {
		manifest.Checked = append(manifest.Checked, ArtifactCheck{Kind: "dataset_generator",
			Path: "sql/datasets/benchmark-v1-generate.sql", CheckType: "live_generate",
			ParseStatus: statusFail, ExecuteStatus: statusFail, ResultShapeStatus: statusSkipped,
			Status: statusFail, Detail: redact(err)})
		manifest.FailedCount, manifest.Status = 1, statusFail
		return manifest, nil
	}
	defer func() { _ = database.Drop(context.Background()) }()
	manifest.PostgreSQLVersion, manifest.PostgreSQLVersionN = database.Version, database.ServerNum

	products, catalogErr := contractProducts(runtime)

	for _, artifact := range artifacts {
		check := ArtifactCheck{Kind: artifact.Kind, Path: artifact.Path, SHA256: artifact.SHA256}
		switch artifact.Kind {
		case "dataset_generator":
			check.CheckType = "live_generate"
			check.ParseStatus, check.ExecuteStatus = statusPass, statusPass
			check.ResultShapeStatus, check.Status = statusSkipped, statusPass
		case "dataset_probe":
			check = checkDatasetProbe(ctx, runtime, database, check)
		case "query_template":
			switch {
			case strings.HasSuffix(artifact.Path, "-direct.sql"):
				check = checkDirectTemplate(ctx, runtime, database, check)
			case strings.HasSuffix(artifact.Path, ".json"):
				check = checkPlanTemplate(runtime, check)
			default:
				check = checkBDGTemplate(runtime, products, catalogErr, check)
			}
		default:
			// Non-SQL contract artifacts are digest-verified by the Runtime
			// itself; this gate deliberately claims nothing more about them.
			check.CheckType = "not_sql"
			check.ParseStatus, check.ExecuteStatus = statusSkipped, statusSkipped
			check.ResultShapeStatus, check.Status = statusSkipped, statusSkipped
		}
		manifest.Checked = append(manifest.Checked, check)
		manifest.RenderedCellCount += check.RenderedCells
		if check.Status == statusFail {
			manifest.FailedCount++
		}
	}
	sort.Slice(manifest.Checked, func(left, right int) bool {
		return manifest.Checked[left].Path < manifest.Checked[right].Path
	})
	manifest.CheckedCount = len(manifest.Checked)
	manifest.Status = statusPass
	if manifest.FailedCount != 0 {
		manifest.Status = statusFail
	}
	return manifest, nil
}

// checkDatasetProbe runs the exact contract-indexed probe bytes. It is never
// rewritten, string-substituted or repaired before being sent to PostgreSQL.
func checkDatasetProbe(ctx context.Context, runtime *finalv5contracts.Runtime,
	database *BenchmarkDatabase, check ArtifactCheck) ArtifactCheck {
	check.CheckType = "live_execute_and_shape"
	check.RenderedCells = 1
	probe, err := runtime.DatasetProbeSQL()
	if err != nil {
		check.ParseStatus, check.ExecuteStatus = statusFail, statusFail
		check.ResultShapeStatus, check.Status = statusSkipped, statusFail
		check.Detail = redact(err)
		return check
	}
	value, column, err := database.ScalarQuery(ctx, probe)
	if err != nil {
		// A syntax error is a parse failure; anything else failed later.
		check.ParseStatus = statusPass
		if strings.Contains(err.Error(), "SQLSTATE 42601") || strings.Contains(err.Error(), "syntax error") {
			check.ParseStatus = statusFail
		}
		check.ExecuteStatus, check.ResultShapeStatus, check.Status = statusFail, statusSkipped, statusFail
		check.Detail = redact(err)
		return check
	}
	check.ParseStatus, check.ExecuteStatus = statusPass, statusPass
	if err := ValidateProbeOutput(value, column); err != nil {
		check.ResultShapeStatus, check.Status = statusFail, statusFail
		check.Detail = redact(err)
		return check
	}
	check.ResultShapeStatus, check.Status = statusPass, statusPass
	return check
}

// ValidateProbeOutput enforces the probe's own output contract: one named
// column, valid JSON, the declared probe version, and every fingerprint member
// present. The values themselves are deployment facts and are not recorded.
func ValidateProbeOutput(value, column string) error {
	if column != "benchmark_probe_v1" {
		return fmt.Errorf("probe returned column %q", column)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &document); err != nil {
		return fmt.Errorf("probe output is not a JSON object: %w", err)
	}
	var version string
	if raw, present := document["probe_version"]; !present {
		return errors.New("probe output has no probe_version")
	} else if err := json.Unmarshal(raw, &version); err != nil || version != ProbeVersion {
		return fmt.Errorf("probe_version is %q, expected %q", version, ProbeVersion)
	}
	for _, key := range probeRequiredKeys {
		raw, present := document[key]
		if !present {
			return fmt.Errorf("probe output has no %q member", key)
		}
		if len(raw) == 0 || string(raw) == "null" {
			return fmt.Errorf("probe output member %q is null", key)
		}
	}
	return nil
}

// checkDirectTemplate proves PostgreSQL can parse, analyse and plan the
// template. The contract froze templates with different parameter arities --
// zero, one and two positional parameters all occur -- so the template is first
// prepared with its placeholders intact, which resolves every relation, column,
// type, operator and function and infers each parameter type without the gate
// guessing what to substitute. Templates that bind exactly one row parameter
// are additionally rendered at every declared scale and planned, so the exact
// bytes a measured run would issue are proven too.
func checkDirectTemplate(ctx context.Context, runtime *finalv5contracts.Runtime,
	database *BenchmarkDatabase, check ArtifactCheck) ArtifactCheck {
	check.CheckType = "prepare_and_explain"
	check.ResultShapeStatus = statusSkipped
	template, err := runtime.ContractBytes(check.Path)
	if err != nil {
		check.ParseStatus, check.ExecuteStatus, check.Status = statusFail, statusFail, statusFail
		check.Detail = redact(err)
		return check
	}
	parameters, err := database.Prepare(ctx, string(template))
	if err != nil {
		check.ParseStatus = statusFail
		check.ExecuteStatus, check.Status = statusFail, statusFail
		check.Detail = redact(err)
		return check
	}
	check.ParseStatus = statusPass
	check.RenderedCells++
	if parameters == 1 {
		for _, rows := range renderScales {
			rendered, err := runtime.RenderIndexedTemplate(check.Path, rows)
			if err != nil {
				check.ExecuteStatus, check.Status = statusFail, statusFail
				check.Detail = redact(err)
				return check
			}
			if err := database.Explain(ctx, rendered.SQL); err != nil {
				check.ExecuteStatus, check.Status = statusFail, statusFail
				check.Detail = redact(err)
				return check
			}
			check.RenderedCells++
		}
	}
	check.ExecuteStatus, check.Status = statusPass, statusPass
	return check
}

// checkBDGTemplate lowers and compiles each rendering through the production
// SQL path, so a template that names a Product, field or operation the runtime
// would reject fails the contract rather than a measured run.
func checkBDGTemplate(runtime *finalv5contracts.Runtime, products map[string]queryplan.Product,
	catalogErr error, check ArtifactCheck) ArtifactCheck {
	check.CheckType = "render_and_compile"
	check.ResultShapeStatus = statusSkipped
	if catalogErr != nil {
		check.ParseStatus, check.ExecuteStatus, check.Status = statusSkipped, statusFail, statusFail
		check.Detail = redact(catalogErr)
		return check
	}
	template, err := runtime.ContractBytes(check.Path)
	if err != nil {
		check.ParseStatus, check.ExecuteStatus, check.Status = statusFail, statusFail, statusFail
		check.Detail = redact(err)
		return check
	}
	// The lowerer accepts agent SQL, which carries literals rather than
	// placeholders, so a template is only compilable once rendered. Templates
	// that bind exactly the one frozen row parameter are rendered by the
	// contract's own renderer at every declared scale, so the exact bytes a
	// measured run would issue are what gets compiled. The contract renderer
	// defines only $1; templates that bind more are rendered here with the
	// minimal legal parameters, purely to prove they compile.
	var renderings []string
	if bindsSingleRowParameter(string(template)) {
		for _, rows := range renderScales {
			rendered, err := runtime.RenderIndexedTemplate(check.Path, rows)
			if err != nil {
				check.ParseStatus, check.ExecuteStatus, check.Status = statusFail, statusFail, statusFail
				check.Detail = redact(err)
				return check
			}
			renderings = append(renderings, rendered.SQL)
		}
	} else {
		for _, rows := range renderScales {
			renderings = append(renderings, renderMinimalPositional(string(template), rows))
		}
	}
	for _, sql := range renderings {
		lowered, err := sqllowering.Lower(sql, products)
		if err != nil {
			check.ParseStatus, check.ExecuteStatus, check.Status = statusFail, statusFail, statusFail
			check.Detail = redact(err)
			return check
		}
		if err := compileLoweredPlan(lowered.Plan, products); err != nil {
			check.ParseStatus = statusPass
			check.ExecuteStatus, check.Status = statusFail, statusFail
			check.Detail = redact(err)
			return check
		}
		check.RenderedCells++
	}
	check.ParseStatus, check.ExecuteStatus, check.Status = statusPass, statusPass, statusPass
	return check
}

// compileLoweredPlan compiles a lowered plan through whichever production
// compiler its shape selects. Lowering emits the relational form for multi
// source queries and the single-product form otherwise; picking the wrong one
// would report a contract defect where there is none.
func compileLoweredPlan(plan queryplan.QueryPlan, products map[string]queryplan.Product) error {
	if plan.From != nil && plan.Product == "" {
		_, _, err := queryplan.CompileSemantic(plan, products)
		return err
	}
	product, present := products[plan.Product]
	if !present {
		return fmt.Errorf("lowered plan names unknown product %q", plan.Product)
	}
	_, err := queryplan.Compile(plan, product)
	return err
}

// bindsSingleRowParameter reports whether a template carries exactly the one
// frozen $1 row parameter the contract renderer knows how to substitute.
func bindsSingleRowParameter(template string) bool {
	return strings.Count(template, "$1") == 1 && !strings.Contains(template, "$2")
}

// renderMinimalPositional binds every positional parameter to the same minimal
// legal row literal. It exists only so a multi-parameter template can be
// compiled; it is a check-time rendering and never a contract artifact.
func renderMinimalPositional(template string, rows int64) string {
	literal := strconv.FormatInt(rows, 10)
	rendered := template
	for ordinal := 9; ordinal >= 1; ordinal-- {
		rendered = strings.ReplaceAll(rendered, "$"+strconv.Itoa(ordinal), literal)
	}
	return rendered
}

// checkPlanTemplate strictly decodes a JSON plan artifact and requires it to
// carry a canonical digest.
func checkPlanTemplate(runtime *finalv5contracts.Runtime, check ArtifactCheck) ArtifactCheck {
	check.CheckType = "strict_decode_and_digest"
	check.RenderedCells = 1
	value, err := runtime.ContractBytes(check.Path)
	if err != nil {
		check.ParseStatus, check.ExecuteStatus, check.Status = statusFail, statusFail, statusFail
		check.ResultShapeStatus, check.Detail = statusSkipped, redact(err)
		return check
	}
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	var plan map[string]any
	if err := decoder.Decode(&plan); err != nil {
		check.ParseStatus, check.ExecuteStatus, check.Status = statusFail, statusFail, statusFail
		check.ResultShapeStatus, check.Detail = statusSkipped, redact(err)
		return check
	}
	if Digest(value) != check.SHA256 {
		check.ParseStatus, check.ExecuteStatus, check.Status = statusPass, statusFail, statusFail
		check.ResultShapeStatus = statusFail
		check.Detail = "plan bytes differ from the contract-indexed digest"
		return check
	}
	check.ParseStatus, check.ExecuteStatus = statusPass, statusPass
	check.ResultShapeStatus, check.Status = statusPass, statusPass
	return check
}

// contractProducts builds the production Product map from the contract-indexed
// Catalog candidate, so compilation is checked against the same Product
// definitions the contract ships.
func contractProducts(runtime *finalv5contracts.Runtime) (map[string]queryplan.Product, error) {
	value, err := runtime.ContractBytes("catalog/benchmark-contract-v1.yaml")
	if err != nil {
		return nil, err
	}
	parsed, err := catalog.Parse(value)
	if err != nil {
		return nil, err
	}
	products := make(map[string]queryplan.Product, len(parsed.Products))
	for _, product := range parsed.Products {
		columns := make(map[string]struct{}, len(product.Fields))
		types := make(map[string]string, len(product.Fields))
		collations := make(map[string]string, len(product.Fields))
		versions := make(map[string]string, len(product.Fields))
		for _, field := range product.Fields {
			columns[field.Name] = struct{}{}
			types[field.Name] = field.Type
			collations[field.Name] = field.Collation
			versions[field.Name] = field.CollationVersion
		}
		aggregates := make(map[string]struct{}, len(product.AllowedAggregates))
		for _, aggregate := range product.AllowedAggregates {
			aggregates[strings.ToLower(strings.TrimSpace(aggregate))] = struct{}{}
		}
		products[product.Name] = queryplan.Product{Name: product.Name, Columns: columns,
			AllowedAggregates: aggregates, ColumnTypes: types, ColumnCollations: collations,
			CollationVersions: versions, SourceNamespace: product.FactNamespace,
			Snapshot: product.Snapshot, StableRole: product.StableRelationRole,
			StableEntityKey:  append([]string(nil), product.EntityKey...),
			LineageDigest:    product.LineageManifestDigest,
			RequiredEvidence: append([]string(nil), product.Scopes...)}
	}
	return products, nil
}

// redact keeps a failure attributable without copying a DSN, a credential or a
// business value into a source-controlled manifest. PostgreSQL error text names
// the offending identifier, which is exactly what a reviewer needs.
func redact(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if index := strings.Index(text, "postgres://"); index >= 0 {
		text = text[:index] + "[redacted-dsn]"
	}
	if len(text) > 400 {
		text = text[:400] + "..."
	}
	return text
}
