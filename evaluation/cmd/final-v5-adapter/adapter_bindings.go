package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

const adapterBindingSectionName = finalv5binding.SectionName

type adapterDeploymentBinding struct {
	DatasetSHA256 string
	CatalogSHA256 string
	FileSHA256    string
	SectionSHA256 string
	Section       adapterBindingSection
}

type adapterBindingValidation struct {
	SchemaVersion        int    `json:"schema_version"`
	Status               string `json:"status"`
	DatasetSHA256        string `json:"dataset_sha256"`
	CatalogSHA256        string `json:"catalog_sha256"`
	BindingFileSHA256    string `json:"dataset_binding_sha256"`
	AdapterSectionSHA256 string `json:"final_v5_adapter_sha256"`
	DatasetProbeSHA256   string `json:"dataset_probe_sql_sha256"`
	ScaleCells           int    `json:"scale_cells"`
	ArtifactCells        int    `json:"artifact_cells"`
	ProvSQLCells         int    `json:"provsql_cells"`
}

type adapterBindingSection = finalv5binding.Section
type boundTaskRequest = finalv5binding.BoundTaskRequest
type boundQueryExpectation = finalv5binding.BoundQueryExpectation
type dependencyCellBinding = finalv5binding.DependencyCellBinding
type scaleDeploymentBinding = finalv5binding.ScaleBinding
type artifactCellBinding = finalv5binding.ArtifactCellBinding
type artifactDeploymentBinding = finalv5binding.ArtifactBinding
type provSQLDeploymentBinding = finalv5binding.ProvSQLBinding

func provSQLBindingKey(scale string, nonce int64) string {
	return finalv5binding.ProvSQLBindingKey(scale, nonce)
}

func validateProvSQLDeploymentBinding(binding *provSQLDeploymentBinding) error {
	return finalv5binding.ValidateProvSQLBinding(binding)
}

func validateProvSQLCellBinding(binding *provSQLDeploymentBinding, scale string, nonce int64) (boundQueryExpectation, error) {
	return finalv5binding.ValidateProvSQLCellBinding(binding, scale, nonce)
}

func loadAdapterDeploymentBinding() (adapterDeploymentBinding, error) {
	var result adapterDeploymentBinding
	path := strings.TrimSpace(os.Getenv("TASKGATE_DATASET_BINDINGS"))
	if path == "" {
		return result, errors.New("TASKGATE_DATASET_BINDINGS is required")
	}
	binding, err := finalv5binding.LoadPublicationFile(path, finalv5binding.CatalogPath)
	if err != nil {
		return result, err
	}
	if err := validateFrozenAdapterBindingIdentity(binding); err != nil {
		return result, err
	}
	return adapterDeploymentBinding{DatasetSHA256: binding.DatasetSHA256, CatalogSHA256: binding.CatalogSHA256,
		FileSHA256: binding.FileSHA256, SectionSHA256: binding.SectionSHA256, Section: binding.Section}, nil
}

func validateFrozenAdapterBindingIdentity(binding finalv5binding.Binding) error {
	expectedFile := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BINDING_FILE_SHA256"))
	expectedSection := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_BINDING_SECTION_SHA256"))
	if !validDigest(expectedFile) || !validDigest(expectedSection) || binding.FileSHA256 != expectedFile ||
		binding.SectionSHA256 != expectedSection {
		return errors.New("dataset binding differs from the pre-start frozen file/section identity")
	}
	return nil
}

func validateAdapterBindingInput() (adapterBindingValidation, error) {
	path := strings.TrimSpace(os.Getenv("TASKGATE_DATASET_BINDINGS"))
	if path == "" {
		return adapterBindingValidation{}, errors.New("TASKGATE_DATASET_BINDINGS is required")
	}
	binding, err := finalv5binding.LoadPublicationFile(path, finalv5binding.CatalogPath)
	if err != nil {
		return adapterBindingValidation{}, err
	}
	return adapterBindingValidation{SchemaVersion: 1, Status: "valid",
		DatasetSHA256: binding.DatasetSHA256, CatalogSHA256: binding.CatalogSHA256,
		BindingFileSHA256: binding.FileSHA256, AdapterSectionSHA256: binding.SectionSHA256,
		DatasetProbeSHA256: finalv5binding.DatasetProbeSHA256(),
		ScaleCells:         len(binding.Section.Scale.DependencyE2E),
		ArtifactCells:      len(binding.Section.Artifact.ResultHeavy), ProvSQLCells: len(binding.Section.ProvSQL.TaskGate)}, nil
}

func validateBoundTask(task boundTaskRequest) error {
	return finalv5binding.ValidateBoundTask(task)
}

func validateBoundQuery(query boundQueryExpectation) error {
	return finalv5binding.ValidateBoundQuery(query)
}

func (adapter *realAdapter) verifyDatasetProbe(ctx context.Context, binding adapterDeploymentBinding) (string, error) {
	tx, err := adapter.business.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, finalv5binding.DatasetProbeSQL)
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

func validateObserverRuntimeBinding() (string, error) {
	executable := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_OBSERVER"))
	executableSHA256 := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_OBSERVER_SHA256"))
	manifest := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST"))
	manifestSHA256 := strings.TrimSpace(os.Getenv("TASKGATE_FINAL_V5_OBSERVER_BUILD_MANIFEST_SHA256"))
	if !filepath.IsAbs(executable) || !filepath.IsAbs(manifest) || !validDigest(executableSHA256) || !validDigest(manifestSHA256) {
		return "", errors.New("source-built observer runtime binding is incomplete")
	}
	for _, file := range []struct {
		path       string
		digest     string
		executable bool
	}{{executable, executableSHA256, true}, {manifest, manifestSHA256, false}} {
		info, err := os.Lstat(file.path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 ||
			(file.executable && info.Mode()&0o111 == 0) {
			return "", errors.New("source-built observer runtime file is absent, unsafe, or non-executable")
		}
		digest, err := experiment.FileSHA256(file.path)
		if err != nil || digest != file.digest {
			return "", errors.New("source-built observer runtime file differs from its SHA-256 binding")
		}
	}
	if _, err := experiment.VerifySourceBuildManifest(manifest, executable, executableSHA256,
		strings.TrimSpace(os.Getenv("TASKGATE_SUBMISSION_COMMIT")),
		"go build -buildvcs=false -trimpath -o final-v5-observer ./evaluation/cmd/final-v5-observer",
		experiment.ObserverRuntimeRequiredSources()); err != nil {
		return "", fmt.Errorf("source-built observer manifest contract: %w", err)
	}
	return executable, nil
}

// captureBoundObserverV2 runs the observer for one phase of one pre-registered
// window.
//
// The invocation is a value rather than a phase string because the v3 observer
// is told which window it is in and which classification that window is judged
// by, and seals both into the snapshot it returns. The argv is built in one
// place -- experiment.ObserverInvocationV3.Argv -- so an Adapter cannot spell a
// flag two ways or omit one, which is exactly how the v1 capture came to pass
// only --phase to an observer that requires three arguments.
func captureBoundObserverV2(ctx context.Context,
	invocation experiment.ObserverInvocationV3) (experiment.ObserverSnapshotV2, error) {
	executable, err := validateObserverRuntimeBinding()
	if err != nil {
		return experiment.ObserverSnapshotV2{}, err
	}
	return experiment.RunObserverV2(ctx, executable, invocation, observerEnvironment())
}

// observerEnvironment is the minimal environment the out-of-process observer is
// run with. It carries no DSN, no token and no credential: the observer opens
// its own connections from its own configuration, which is what keeps it
// independent of the Adapter that invokes it.
func observerEnvironment() []string {
	environment := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for _, name := range []string{"COMPOSE_PROJECT_NAME", "DOCKER_HOST", "XDG_RUNTIME_DIR"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func failedSample(operation experiment.AdapterOperation, code string) experiment.Sample {
	sample := invalidSample(operation, code)
	sample.Status = "fail"
	sample.Reason = "a real backend operation was attempted and failed; the original frozen cell was retained"
	return sample
}

// retainTaskGateRejection attaches only the finalizer's opaque typed marker.
// Ordinary errors -- including pre-registration, observation, execution and
// evidence-validation failures that never called FinalizeTaskGateObservationV3
// -- leave both acceptance and rejection absent.
func retainTaskGateRejection(sample experiment.Sample, err error) experiment.Sample {
	rejection, rejected := experiment.TaskGateRejectionFromError(err)
	if !rejected {
		return sample
	}
	sample.SchemaVersion = experiment.TaskGateRejectionSampleSchemaVersion
	sample.TaskGateAcceptanceV3 = nil
	sample.TaskGateRejectionV1 = rejection
	return sample
}

// requireVerifiedReceipt is the v3 path's receipt acceptance rule.
//
// The Adapter used to accept a receipt that described no execution and then
// RE-DERIVE the physical statements itself to decide what the Gateway had
// executed. That is a second opinion about an execution, not evidence about it,
// and the two can differ in exactly the case the evidence exists to detect. The
// Gateway now signs what it executed, so the Adapter reads it instead of
// reconstructing it.
//
// There is deliberately no fallback. A completed exposure-V5 artifact operation
// on this path always carries an execution binding; accepting a receipt without
// one would silently restore the re-derivation it replaces.
func requireVerifiedReceipt(verifier receiptVerifier, receipt queryreceipt.QueryReceiptV1) error {
	if err := queryreceipt.RequireCurrentVersion(receipt.Version); err != nil {
		return fmt.Errorf("the v3 path requires the current receipt: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return fmt.Errorf("receipt does not validate: %w", err)
	}
	if verifier != nil {
		if err := verifier.Verify(receipt); err != nil {
			return fmt.Errorf("verify receipt: %w", err)
		}
	}
	// Validate() already requires both structures. Restating it here makes the
	// Adapter's dependency explicit: everything below reads them.
	if receipt.ExecutionBindingV2 == nil || receipt.ExposureLedgerBefore == nil {
		return errors.New("receipt carries no execution binding or pre-state")
	}
	if receipt.ArtifactIntent == nil || receipt.Exposure == nil {
		return errors.New("receipt carries no artifact intent or exposure evidence")
	}
	return nil
}

// receiptVerifier is the subset of the keyring the acceptance rule needs.
type receiptVerifier interface {
	Verify(queryreceipt.QueryReceiptV1) error
}
