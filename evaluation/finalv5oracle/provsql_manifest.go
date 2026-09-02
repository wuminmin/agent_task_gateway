package finalv5oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"
)

const (
	ProvSQLManifestGeneratorVersion = "taskgate-final-v5-provsql-semantic-manifest-v1"

	ProvSQLDatasetSpecSHA256       = "52532741e9b303219656f0f3d3949ffbfc3cc77d16c4aa6dd54f41cea558c095"
	ProvSQLCatalogSpecSHA256       = "d691a58ef73ac7746ad2eb926c6d5999aae9d2646a34c9f0513616e380f29a8d"
	ProvSQLNormalizationSpecSHA256 = "9e78abdc2130b19cb414d520edeb6da4dfac0f481609efd244f22f6ba2b1e816"
	ProvSQLCombinedQuerySpecSHA256 = "017c9a9e8b51ac41c9ee4a26e1e4f47c63934c170a37a4e7d066ba30bb4ac1fb"
	ProvSQLOrdersSnapshotSHA256    = "830f9584a2a919af819131420ee4fc33c795095302387312c92349164b094ef1"
	ProvSQLLineitemSnapshotSHA256  = "97b86d4f7b51be2ca3cbbb90214043c00ca5e2526ca8a886ed3150d98f48a658"
	ProvSQLNonceSnapshotSHA256     = "965c9baa7f1fa219704017282a514505a4c470d1b52ffcf61720ef4656fb5cc8"

	provSQLQuerySpecDomain = "TASKGATE-FINAL-V5-PROVSQL-QUERY-SPEC-V1\x00"

	provSQLWarmupIterations    = 5
	provSQLMeasuredIterations  = 30
	provSQLMeasuredNonceOffset = int64(100)
)

type provSQLScaleScheduleEntry struct {
	name      string
	limit     int64
	nonceBase int64
}

// provSQLScaleSchedule is the sole regular-source definition of the reviewed
// three-scale nonce grid. Both the Query Spec identity and the 105 cells consume
// detached values from this function.
func provSQLScaleSchedule() [3]provSQLScaleScheduleEntry {
	return [3]provSQLScaleScheduleEntry{
		{name: "1k", limit: 1_000, nonceBase: 0},
		{name: "10k", limit: 10_000, nonceBase: 300},
		{name: "45k", limit: 45_000, nonceBase: 600},
	}
}

type ProvSQLNonceJoinCell struct {
	Scale      string `json:"scale"`
	Limit      int64  `json:"limit"`
	Nonce      int64  `json:"nonce"`
	Warmup     bool   `json:"warmup"`
	Iteration  int    `json:"iteration"`
	BindingKey string `json:"binding_key"`
}

type ProvSQLManifestSpecHashes struct {
	Dataset       string `json:"dataset_spec_sha256"`
	Catalog       string `json:"catalog_spec_sha256"`
	Query         string `json:"query_spec_sha256"`
	Normalization string `json:"normalization_spec_sha256"`
}

type ProvSQLManifestArtifact struct {
	RelativePath string         `json:"path"`
	SHA256       string         `json:"sha256"`
	Manifest     OracleManifest `json:"-"`
}

func FrozenProvSQLManifestSpecHashes() ProvSQLManifestSpecHashes {
	return ProvSQLManifestSpecHashes{
		Dataset: ProvSQLDatasetSpecSHA256, Catalog: ProvSQLCatalogSpecSHA256,
		Query: ProvSQLCombinedQuerySpecSHA256, Normalization: ProvSQLNormalizationSpecSHA256,
	}
}

func (specs ProvSQLManifestSpecHashes) Validate() error {
	want := FrozenProvSQLManifestSpecHashes()
	if specs != want {
		return fmt.Errorf("ProvSQL manifest Spec hashes are %+v; expected the reviewed fixed binding %+v", specs, want)
	}
	return nil
}

// ProvSQLQuerySpecSHA256 commits the fixed typed relational model without
// parsing SQL. Every string below is a closed, pre-run contract member.
func ProvSQLQuerySpecSHA256() string {
	target := sha256.New()
	_, _ = target.Write([]byte(provSQLQuerySpecDomain))
	schedule := provSQLScaleSchedule()
	scaleBindings, nonceBases := make([]string, 0, len(schedule)), make([]string, 0, len(schedule))
	for _, scale := range schedule {
		scaleBindings = append(scaleBindings, scale.name+"="+strconv.FormatInt(scale.limit, 10))
		nonceBases = append(nonceBases, scale.name+"="+strconv.FormatInt(scale.nonceBase, 10))
	}
	values := []string{
		"products:provsql_orders,provsql_lineitem,provsql_nonce",
		"join:eq(provsql_lineitem.orderkey,provsql_orders.orderkey)",
		"join:eq(provsql_lineitem.partition_key,provsql_orders.partition_key)",
		"join:eq(provsql_nonce.partition_key,provsql_orders.partition_key)",
		"predicate:provsql_orders.orderkey<=scale_limit",
		"predicate:provsql_nonce.nonce_id=nonce",
		"scope:partition_key=1",
		"group:provsql_orders.status",
		"outputs:status:bigint,price:numeric_text,lines:bigint,members:bigint",
		"aggregates:sum(provsql_lineitem.extendedprice),sum(provsql_lineitem.linenumber),count(*)",
		"order:provsql_orders.status ASC",
		"scales:" + strings.Join(scaleBindings, ","),
		"nonce-bases:" + strings.Join(nonceBases, ","),
		fmt.Sprintf("nonce-schedule:one-process-replicate;warmup=1..%d;base+iteration;measured=1..%d;base+%d+iteration",
			provSQLWarmupIterations, provSQLMeasuredIterations, provSQLMeasuredNonceOffset),
		"snapshot:provsql_orders=" + ProvSQLOrdersSnapshotSHA256,
		"snapshot:provsql_lineitem=" + ProvSQLLineitemSnapshotSHA256,
		"snapshot:provsql_nonce=" + ProvSQLNonceSnapshotSHA256,
	}
	writeUint64(target, uint64(len(values)))
	for _, value := range values {
		writeFramed(target, []byte(value))
	}
	return hex.EncodeToString(target.Sum(nil))
}

func ProvSQLNonceJoinCells() []ProvSQLNonceJoinCell {
	result := make([]ProvSQLNonceJoinCell, 0, 105)
	for _, scale := range provSQLScaleSchedule() {
		for iteration := 1; iteration <= provSQLWarmupIterations; iteration++ {
			nonce := scale.nonceBase + int64(iteration)
			result = append(result, ProvSQLNonceJoinCell{Scale: scale.name, Limit: scale.limit, Nonce: nonce,
				Warmup: true, Iteration: iteration, BindingKey: ProvSQLBindingKey(scale.name, nonce)})
		}
		for iteration := 1; iteration <= provSQLMeasuredIterations; iteration++ {
			nonce := scale.nonceBase + provSQLMeasuredNonceOffset + int64(iteration)
			result = append(result, ProvSQLNonceJoinCell{Scale: scale.name, Limit: scale.limit, Nonce: nonce,
				Iteration: iteration, BindingKey: ProvSQLBindingKey(scale.name, nonce)})
		}
	}
	return result
}

func ProvSQLBindingKey(scale string, nonce int64) string {
	return scale + "/" + strconv.FormatInt(nonce, 10)
}

func ParseProvSQLBindingKey(value string) (ProvSQLNonceJoinCell, error) {
	if value == "" || value != strings.TrimSpace(value) || strings.Count(value, "/") != 1 {
		return ProvSQLNonceJoinCell{}, errors.New("ProvSQL binding key is not canonical")
	}
	for _, cell := range ProvSQLNonceJoinCells() {
		if cell.BindingKey == value {
			return cell, nil
		}
	}
	return ProvSQLNonceJoinCell{}, fmt.Errorf("ProvSQL binding key %q is not one of the fixed 105 nonce cells", value)
}

func (cell ProvSQLNonceJoinCell) validate() error {
	want, err := ParseProvSQLBindingKey(cell.BindingKey)
	if err != nil || cell != want {
		return errors.New("ProvSQL nonce-join cell is not one of the fixed 105 cells")
	}
	return nil
}

func ProvSQLNonceJoinManifestPath(scale string, nonce int64) (string, error) {
	cell, err := ParseProvSQLBindingKey(ProvSQLBindingKey(scale, nonce))
	if err != nil {
		return "", err
	}
	return path.Join("provsql", "nonce-join-group", cell.Scale, "taskgate", strconv.FormatInt(cell.Nonce, 10)+".json"), nil
}

// GenerateProvSQLNonceJoinManifests independently regenerates the exact 105
// scale/nonce cells. Its inputs are reviewed public Spec hashes plus a bounded
// sorter configuration; there is no runtime output or DSN input.
func GenerateProvSQLNonceJoinManifests(specs ProvSQLManifestSpecHashes,
	options StreamSetOptions) ([]ProvSQLManifestArtifact, error) {
	if err := specs.Validate(); err != nil {
		return nil, err
	}
	cells := ProvSQLNonceJoinCells()
	result := make([]ProvSQLManifestArtifact, 0, len(cells))
	for _, cell := range cells {
		report, err := GenerateProvSQLNonceJoinDependency(cell, options)
		if err != nil {
			return nil, fmt.Errorf("generate ProvSQL %s: %w", cell.BindingKey, err)
		}
		manifest := provSQLManifest(cell, specs, report)
		relative, _ := ProvSQLNonceJoinManifestPath(cell.Scale, cell.Nonce)
		digest, err := ManifestSHA256(manifest)
		if err != nil {
			return nil, fmt.Errorf("canonicalize %s: %w", relative, err)
		}
		result = append(result, ProvSQLManifestArtifact{RelativePath: relative, SHA256: digest, Manifest: manifest})
	}
	if len(result) != 105 {
		return nil, fmt.Errorf("generated %d ProvSQL manifests; expected 105", len(result))
	}
	paths, digests := make(map[string]bool, len(result)), make(map[string]bool, len(result))
	dependencyDigests, releaseDigests := make(map[string]bool, len(result)), make(map[string]bool, len(result))
	for _, artifact := range result {
		dependency := artifact.Manifest.Expected.DependencyCandidateSetSHA256
		release := artifact.Manifest.Expected.ReleaseCandidateSetSHA256
		if paths[artifact.RelativePath] || digests[artifact.SHA256] || dependencyDigests[dependency] || releaseDigests[release] {
			return nil, errors.New("ProvSQL paths, manifests, dependency sets, and release sets must all be unique")
		}
		paths[artifact.RelativePath], digests[artifact.SHA256] = true, true
		dependencyDigests[dependency], releaseDigests[release] = true, true
	}
	return result, nil
}

func VerifyProvSQLNonceJoinManifest(manifest OracleManifest, options StreamSetOptions) error {
	if manifest.ExperimentID != "provsql" || manifest.WorkloadID != "nonce-join-group" || manifest.Mode != "taskgate" {
		return errors.New("manifest is not a supported ProvSQL nonce-join manifest")
	}
	cell, err := ParseProvSQLBindingKey(manifest.BindingKey)
	if err != nil || cell.Scale != manifest.Scale {
		return errors.New("ProvSQL manifest has no exact fixed cell")
	}
	specs := ProvSQLManifestSpecHashes{Dataset: manifest.DatasetSpecSHA256, Catalog: manifest.CatalogSpecSHA256,
		Query: manifest.QuerySpecSHA256, Normalization: manifest.NormalizationSpecSHA256}
	if err := specs.Validate(); err != nil {
		return err
	}
	report, err := GenerateProvSQLNonceJoinDependency(cell, options)
	if err != nil {
		return err
	}
	want := provSQLManifest(cell, specs, report)
	if !reflect.DeepEqual(manifest, want) {
		return errors.New("ProvSQL nonce-join manifest differs from independent regeneration")
	}
	return nil
}

// VerifyProvSQLNonceJoinManifestSet regenerates the exact closed set once, then
// rejects every missing, extra, duplicate-path, non-canonical, or changed file.
func VerifyProvSQLNonceJoinManifestSet(values map[string][]byte,
	options StreamSetOptions) ([]ProvSQLManifestArtifact, error) {
	if len(values) != 105 {
		return nil, fmt.Errorf("ProvSQL manifest set has %d files; expected exactly 105", len(values))
	}
	want, err := GenerateProvSQLNonceJoinManifests(FrozenProvSQLManifestSpecHashes(), options)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(want))
	for _, artifact := range want {
		value, present := values[artifact.RelativePath]
		if !present {
			return nil, fmt.Errorf("ProvSQL manifest set omits %q", artifact.RelativePath)
		}
		manifest, err := DecodeManifest(value)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", artifact.RelativePath, err)
		}
		if !reflect.DeepEqual(manifest, artifact.Manifest) {
			return nil, fmt.Errorf("%s differs from independent regeneration", artifact.RelativePath)
		}
		seen[artifact.RelativePath] = true
	}
	for relative := range values {
		if !seen[relative] {
			return nil, fmt.Errorf("ProvSQL manifest set contains unexpected path %q", relative)
		}
	}
	return want, nil
}

func ProvSQLManifestGenerationCommand(specs ProvSQLManifestSpecHashes) string {
	return fmt.Sprintf("final-v5-oracle provsql-manifests --dataset-spec-sha256 %s --catalog-spec-sha256 %s --query-spec-sha256 %s --normalization-spec-sha256 %s",
		specs.Dataset, specs.Catalog, specs.Query, specs.Normalization)
}

func provSQLManifest(cell ProvSQLNonceJoinCell, specs ProvSQLManifestSpecHashes,
	report ProvSQLDependencyOracleReport) OracleManifest {
	return OracleManifest{
		SchemaVersion: ManifestSchemaVersion, OracleVersion: ManifestOracleVersion,
		ContractVersion: ManifestContractVersion, ExperimentID: "provsql", WorkloadID: "nonce-join-group",
		Scale: cell.Scale, Mode: "taskgate", BindingKey: cell.BindingKey,
		DatasetSpecSHA256: specs.Dataset, CatalogSpecSHA256: specs.Catalog,
		QuerySpecSHA256: specs.Query, NormalizationSpecSHA256: specs.Normalization,
		Expected: ManifestExpected{
			RowCount: Int64(report.Result.RowCount), ColumnCount: Int(report.Result.ColumnCount),
			NormalizedSchemaSHA256:         report.Result.NormalizedSchemaSHA256,
			CanonicalResultSHA256:          report.Result.CanonicalResultSHA256,
			ReleaseCandidateCardinality:    Int64(report.Release.Cardinality),
			ReleaseCandidateSetSHA256:      report.Release.SetSHA256,
			DependencyCandidateCardinality: Int64(report.Candidate.Cardinality),
			DependencyCandidateSetSHA256:   report.Candidate.SetSHA256,
		},
		Generation: ManifestGeneration{Seed: BenchmarkDatasetGeneratorSeed,
			GeneratorVersion: ProvSQLManifestGeneratorVersion,
			Command:          ProvSQLManifestGenerationCommand(specs)},
	}
}
