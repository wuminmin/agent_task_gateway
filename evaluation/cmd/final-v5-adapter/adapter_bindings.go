package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

const adapterBindingSectionName = "final_v5_adapter_v1"

type adapterDeploymentBinding struct {
	DatasetSHA256 string
	CatalogSHA256 string
	SectionSHA256 string
	Section       adapterBindingSection
}

type adapterBindingSection struct {
	SchemaVersion   int                        `json:"schema_version"`
	DatasetProbeSQL string                     `json:"dataset_probe_sql"`
	Observer        observerCommandBinding     `json:"observer"`
	Scale           *scaleDeploymentBinding    `json:"scale,omitempty"`
	Artifact        *artifactDeploymentBinding `json:"artifact,omitempty"`
	ProvSQL         *provSQLDeploymentBinding  `json:"provsql,omitempty"`
}

type observerCommandBinding struct {
	Argv             []string `json:"argv"`
	ExecutableSHA256 string   `json:"executable_sha256"`
}

type boundTaskRequest struct {
	Objective         string              `json:"objective"`
	DataProducts      []string            `json:"data_products"`
	Columns           map[string][]string `json:"columns"`
	Scopes            map[string][]string `json:"scopes"`
	VisibleRelation   string              `json:"visible_relation"`
	CompanionRelation string              `json:"companion_relation"`
}

type boundQueryExpectation struct {
	SQL                    string `json:"sql"`
	ExpectedRows           int64  `json:"expected_rows"`
	ExpectedColumns        int    `json:"expected_columns"`
	ExpectedResultSHA256   string `json:"expected_result_sha256"`
	DependencyFacts        int64  `json:"dependency_facts"`
	DependencySetSHA256    string `json:"dependency_set_sha256"`
	ExpectedVisibleCalls   int64  `json:"expected_visible_calls,omitempty"`
	ExpectedCompanionCalls int64  `json:"expected_companion_calls,omitempty"`
}

type dependencyCellBinding struct {
	Task      boundTaskRequest       `json:"task"`
	Candidate boundQueryExpectation  `json:"candidate"`
	History   *boundQueryExpectation `json:"history,omitempty"`
}

type scaleDeploymentBinding struct {
	DependencyE2E       map[string]dependencyCellBinding `json:"dependency_e2e,omitempty"`
	EnableOutcomeMerkle bool                             `json:"enable_outcome_merkle,omitempty"`
	EnableExtreme       bool                             `json:"enable_extreme,omitempty"`
}

type artifactCellBinding struct {
	Task  boundTaskRequest      `json:"task"`
	Query boundQueryExpectation `json:"query"`
}

type artifactDeploymentBinding struct {
	ResultHeavy map[string]artifactCellBinding `json:"result_heavy"`
}

// provSQLDeploymentBinding contains only deployment facts that cannot be
// inferred from the public fixture: the approved Task request and exact
// TaskGate Dependency FactSet oracle for every nonce. SQL, visible results,
// dataset contents, versions, and nonce allocation remain source-controlled.
type provSQLDeploymentBinding struct {
	FixtureVersion                string                           `json:"fixture_version"`
	FixtureSQLSHA256              string                           `json:"fixture_sql_sha256"`
	EnableSQLSHA256               string                           `json:"enable_sql_sha256"`
	DatasetSHA256                 string                           `json:"dataset_sha256"`
	DatasetProbeSQLSHA256         string                           `json:"dataset_probe_sql_sha256"`
	BusinessDatasetProbeSQLSHA256 string                           `json:"business_dataset_probe_sql_sha256"`
	Task                          boundTaskRequest                 `json:"task"`
	TaskGate                      map[string]boundQueryExpectation `json:"taskgate"`
}

func provSQLBindingKey(scale string, nonce int64) string {
	return scale + "/" + fmt.Sprintf("%d", nonce)
}

func validateProvSQLDeploymentBinding(binding *provSQLDeploymentBinding) error {
	if binding == nil || binding.FixtureVersion != provsqlfixture.Version ||
		binding.FixtureSQLSHA256 != provsqlfixture.FixtureSQLSHA256() ||
		binding.EnableSQLSHA256 != provsqlfixture.EnableSQLSHA256() ||
		binding.DatasetSHA256 != provsqlfixture.ExpectedDatasetSHA256() ||
		binding.DatasetProbeSQLSHA256 != provsqlfixture.DatasetProbeSQLSHA256() ||
		binding.BusinessDatasetProbeSQLSHA256 != provsqlfixture.BusinessDatasetProbeSQLSHA256() ||
		validateBoundTask(binding.Task) != nil || len(binding.TaskGate) != 105 {
		return errors.New("ProvSQL deployment binding differs from the frozen fixture")
	}
	for _, scale := range []string{"1k", "10k", "45k"} {
		for _, phase := range []struct {
			warmup bool
			count  int
		}{{warmup: true, count: 5}, {warmup: false, count: 30}} {
			for iteration := 1; iteration <= phase.count; iteration++ {
				nonce, err := provsqlfixture.Nonce(scale, 1, iteration, phase.warmup)
				if err != nil {
					return err
				}
				expected, present := binding.TaskGate[provSQLBindingKey(scale, nonce)]
				if !present {
					return errors.New("ProvSQL deployment binding omits a frozen nonce cell")
				}
				if err := validateProvSQLCellExpectation(expected, scale, nonce); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateProvSQLCellBinding(binding *provSQLDeploymentBinding, scale string, nonce int64) (boundQueryExpectation, error) {
	if err := validateProvSQLDeploymentBinding(binding); err != nil {
		return boundQueryExpectation{}, err
	}
	expected, present := binding.TaskGate[provSQLBindingKey(scale, nonce)]
	if !present {
		return boundQueryExpectation{}, errors.New("ProvSQL TaskGate cell lacks its exact frozen query/FactSet oracle")
	}
	if err := validateProvSQLCellExpectation(expected, scale, nonce); err != nil {
		return boundQueryExpectation{}, err
	}
	return expected, nil
}

func validateProvSQLCellExpectation(expected boundQueryExpectation, scale string, nonce int64) error {
	logical, err := provsqlfixture.LogicalSQL(scale, nonce)
	if err != nil || validateBoundQuery(expected) != nil || expected.SQL != logical ||
		expected.ExpectedRows != provsqlfixture.ExpectedRows || expected.ExpectedColumns != provsqlfixture.ExpectedColumns ||
		expected.DependencyFacts <= 0 || !validDigest(expected.DependencySetSHA256) ||
		expected.ExpectedVisibleCalls < 0 || expected.ExpectedCompanionCalls < 0 ||
		expected.ExpectedVisibleCalls+expected.ExpectedCompanionCalls == 0 {
		return errors.New("ProvSQL TaskGate cell lacks its exact frozen query/FactSet oracle")
	}
	rows, err := provsqlfixture.ExpectedResultRows(scale)
	if err != nil {
		return err
	}
	resultSHA256, err := experiment.CanonicalResultHash(rows)
	if err != nil || expected.ExpectedResultSHA256 != resultSHA256 {
		return errors.New("ProvSQL TaskGate result oracle differs from the source fixture")
	}
	return nil
}

func loadAdapterDeploymentBinding() (adapterDeploymentBinding, error) {
	var result adapterDeploymentBinding
	path := strings.TrimSpace(os.Getenv("TASKGATE_DATASET_BINDINGS"))
	if path == "" {
		return result, errors.New("TASKGATE_DATASET_BINDINGS is required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4<<20 {
		return result, errors.New("dataset binding must be a bounded regular file")
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	var top map[string]json.RawMessage
	if err := experiment.StrictJSON(value, &top); err != nil {
		return result, fmt.Errorf("decode dataset binding: %w", err)
	}
	if err := json.Unmarshal(top["dataset_sha256"], &result.DatasetSHA256); err != nil || !validDigest(result.DatasetSHA256) {
		return result, errors.New("dataset binding lacks dataset_sha256")
	}
	if err := json.Unmarshal(top["catalog_sha256"], &result.CatalogSHA256); err != nil || !validDigest(result.CatalogSHA256) {
		return result, errors.New("dataset binding lacks catalog_sha256")
	}
	sectionBytes := top[adapterBindingSectionName]
	if len(sectionBytes) == 0 {
		return result, errors.New("dataset binding lacks the strict final_v5_adapter_v1 section")
	}
	if err := experiment.StrictJSON(sectionBytes, &result.Section); err != nil {
		return result, fmt.Errorf("decode strict adapter binding section: %w", err)
	}
	result.SectionSHA256 = shaBytes(sectionBytes)
	if result.Section.SchemaVersion != 1 {
		return result, errors.New("adapter binding section schema is unsupported")
	}
	if err := validateReadOnlySQL(result.Section.DatasetProbeSQL); err != nil {
		return result, fmt.Errorf("dataset probe: %w", err)
	}
	if err := validateObserverCommand(result.Section.Observer); err != nil {
		return result, err
	}
	return result, nil
}

func validateObserverCommand(binding observerCommandBinding) error {
	if len(binding.Argv) == 0 || !filepath.IsAbs(binding.Argv[0]) || !validDigest(binding.ExecutableSHA256) {
		return errors.New("observer binding lacks an absolute executable and SHA-256")
	}
	for _, argument := range binding.Argv {
		if strings.TrimSpace(argument) == "" || strings.IndexByte(argument, 0) >= 0 {
			return errors.New("observer argv contains an invalid argument")
		}
	}
	info, err := os.Lstat(binding.Argv[0])
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&0o111 == 0 {
		return errors.New("observer executable is absent, non-executable, or a symlink")
	}
	digest, err := experiment.FileSHA256(binding.Argv[0])
	if err != nil || digest != binding.ExecutableSHA256 {
		return errors.New("observer executable differs from its deployment binding")
	}
	return nil
}

func validateBoundTask(task boundTaskRequest) error {
	canonicalRelation := regexp.MustCompile(`^[a-z_][a-z0-9_]*\.[a-z_][a-z0-9_]*$`)
	if strings.TrimSpace(task.Objective) == "" || len(task.DataProducts) == 0 || len(task.Columns) == 0 ||
		!canonicalRelation.MatchString(task.VisibleRelation) || !canonicalRelation.MatchString(task.CompanionRelation) ||
		task.VisibleRelation == task.CompanionRelation {
		return errors.New("bound task request is incomplete")
	}
	seen := map[string]bool{}
	for _, product := range task.DataProducts {
		if strings.TrimSpace(product) == "" || seen[product] || len(task.Columns[product]) == 0 {
			return errors.New("bound task products/columns are inconsistent")
		}
		seen[product] = true
		columnSeen := map[string]bool{}
		for _, column := range task.Columns[product] {
			if strings.TrimSpace(column) == "" || columnSeen[column] {
				return errors.New("bound task contains an empty or duplicate column")
			}
			columnSeen[column] = true
		}
	}
	if len(seen) != len(task.Columns) {
		return errors.New("bound task columns contain an undeclared product")
	}
	return nil
}

func validateBoundQuery(query boundQueryExpectation) error {
	if err := validateReadOnlySQL(query.SQL); err != nil {
		return err
	}
	if query.ExpectedRows < 0 || query.ExpectedColumns <= 0 || !validDigest(query.ExpectedResultSHA256) ||
		query.DependencyFacts < 0 || (query.DependencyFacts > 0 && !validDigest(query.DependencySetSHA256)) ||
		(query.DependencyFacts == 0 && query.DependencySetSHA256 != "") {
		return errors.New("bound query expectation is incomplete")
	}
	return nil
}

func validateReadOnlySQL(sqlText string) error {
	trimmed := strings.TrimSpace(sqlText)
	upper := strings.ToUpper(trimmed)
	if trimmed == "" || (!strings.HasPrefix(upper, "SELECT ") && !strings.HasPrefix(upper, "WITH ")) ||
		strings.Contains(trimmed, ";") || strings.Contains(trimmed, "\x00") {
		return errors.New("only one semicolon-free SELECT/CTE statement is allowed")
	}
	return nil
}

func (adapter *realAdapter) verifyDatasetProbe(ctx context.Context, binding adapterDeploymentBinding) (string, error) {
	tx, err := adapter.business.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, binding.Section.DatasetProbeSQL)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if len(rows.FieldDescriptions()) != 1 || !rows.Next() {
		return "", errors.New("dataset probe must return exactly one scalar row")
	}
	var fingerprint string
	if err := rows.Scan(&fingerprint); err != nil || rows.Next() {
		return "", errors.New("dataset probe must return exactly one scalar row")
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	digest := sha(fingerprint)
	if digest != binding.DatasetSHA256 {
		return digest, errors.New("live dataset probe differs from dataset_sha256")
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return digest, nil
}

func (adapter *realAdapter) businessSQLSnapshotFor(ctx context.Context, task boundTaskRequest) (experiment.BusinessSQLSnapshot, error) {
	const query = `WITH statements AS (
  SELECT s.calls::bigint AS calls,replace(lower(s.query),'"','') AS normalized_query
  FROM pg_stat_statements s
  WHERE s.dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
    AND s.userid=(SELECT oid FROM pg_roles WHERE rolname='gateway_reader')
)
SELECT COALESCE(sum(calls) FILTER (WHERE position($1 in normalized_query)>0
                                   AND position($2 in normalized_query)=0),0)::bigint,
       COALESCE(sum(calls) FILTER (WHERE position($2 in normalized_query)>0),0)::bigint,
       info.stats_reset,info.dealloc::bigint
FROM pg_stat_statements_info info
LEFT JOIN statements ON true
GROUP BY info.stats_reset,info.dealloc`
	var snapshot experiment.BusinessSQLSnapshot
	var reset time.Time
	if err := adapter.observer.QueryRow(ctx, query, task.VisibleRelation, task.CompanionRelation).Scan(
		&snapshot.VisibleCalls, &snapshot.CompanionCalls, &reset, &snapshot.Dealloc); err != nil {
		return snapshot, err
	}
	snapshot.StatsResetUnixMicro = reset.UTC().UnixMicro()
	return snapshot, nil
}

func (adapter *realAdapter) provisionBoundTask(ctx context.Context, operation experiment.AdapterOperation, task boundTaskRequest) (string, error) {
	if err := validateBoundTask(task); err != nil {
		return "", err
	}
	var created struct {
		TaskID string `json:"task_id"`
		OAURL  string `json:"oa_url"`
	}
	arguments := map[string]any{
		"objective":     task.Objective + " / " + operation.PairID,
		"data_products": task.DataProducts,
		"columns":       task.Columns,
		"scopes":        task.Scopes,
	}
	if err := adapter.alice.call(ctx, "request_data_task", arguments, &created); err != nil {
		return "", err
	}
	if created.TaskID == "" || created.OAURL == "" {
		return "", errors.New("bound task request omitted identity")
	}
	draftID := pathTail(created.OAURL)
	if err := oaAction(ctx, adapter.aliceOA, adapter.oaBase, draftID, "submit", ""); err != nil {
		return "", err
	}
	if err := adapter.waitTask(ctx, created.TaskID, "AWAITING_APPROVAL"); err != nil {
		return "", err
	}
	if err := oaAction(ctx, adapter.bobOA, adapter.oaBase, draftID, "decision", "approved"); err != nil {
		return "", err
	}
	if err := adapter.waitTask(ctx, created.TaskID, "ACTIVE"); err != nil {
		return "", err
	}
	return created.TaskID, nil
}

func captureBoundObserver(ctx context.Context, binding observerCommandBinding) (experiment.ObserverSnapshot, error) {
	if err := validateObserverCommand(binding); err != nil {
		return experiment.ObserverSnapshot{}, err
	}
	environment := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for _, name := range []string{"COMPOSE_PROJECT_NAME", "DOCKER_HOST", "XDG_RUNTIME_DIR"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return experiment.RunObserver(ctx, binding.Argv, environment)
}

func applyObserverDelta(sample *experiment.Sample, before, after experiment.ObserverSnapshot) error {
	delta, err := experiment.DifferenceObserver(before, after)
	if err != nil {
		return err
	}
	if delta.OOMDelta != 0 {
		return errors.New("observer recorded an OOM event")
	}
	if delta.ContainerRestartDelta != 0 {
		return errors.New("observer recorded a container restart")
	}
	sample.GatewayMemoryPeakBytes = delta.GatewayMemoryPeakBytes
	sample.GatewayCPUUsecDelta = delta.GatewayCPUUsecDelta
	sample.GatewayNetworkRXDelta = delta.GatewayNetworkRXDelta
	sample.GatewayNetworkTXDelta = delta.GatewayNetworkTXDelta
	sample.ControlWALBytesDelta = delta.ControlWALBytesDelta
	sample.BusinessWALBytesDelta = delta.BusinessWALBytesDelta
	return nil
}

func failedSample(operation experiment.AdapterOperation, code string) experiment.Sample {
	sample := invalidSample(operation, code)
	sample.Status = "fail"
	sample.Reason = "a real backend operation was attempted and failed; the original frozen cell was retained"
	return sample
}
