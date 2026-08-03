package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/gateway"
	"taskbound.local/agent-data-gateway/internal/mcp"
	"taskbound.local/agent-data-gateway/internal/ordinal"
)

const callbackSecret = "rq5-online-experiment-callback-secret-v1"

var md5Regexp = regexp.MustCompile(`^[0-9a-f]{32}$`)

type datasetManifest struct {
	SchemaVersion         string                   `json:"schema_version"`
	Generator             string                   `json:"generator"`
	PostgresVersion       string                   `json:"postgres_version"`
	Rows                  map[string]int64         `json:"rows"`
	ChangesFromPrevious   map[string]datasetChange `json:"changes_from_previous"`
	OrderedRowFingerprint map[string]string        `json:"ordered_row_fingerprint_md5"`
}

type datasetChange struct {
	UpdatedRows  int64 `json:"updated_rows"`
	InsertedRows int64 `json:"inserted_rows"`
	DeletedRows  int64 `json:"deleted_rows"`
}

type directOracle struct {
	PublicationDigest string
	ResultSHA256      string
}

type routedTaskState struct {
	Task         control.Task
	ResultSHA256 string
	CacheKey     string
}

func runOnlineExperiment(options runOptions) error {
	for _, source := range []struct {
		path string
		name string
	}{
		{options.InputDirectory, "-input-dir"}, {options.ArtifactDirectory, "-artifact-dir"},
	} {
		if err := requireDirectory(source.path, source.name); err != nil {
			return err
		}
	}
	if err := createPrivateDirectory(options.CatalogDirectory, "-catalog-dir"); err != nil {
		return err
	}
	if options.OutputPath == "" || options.DatasetManifestPath == "" || options.GeneratorPath == "" || options.ConfigPath == "" {
		return errors.New("-output, -dataset-manifest, -generator, and -config are required")
	}
	for _, path := range []string{options.DatasetManifestPath, options.GeneratorPath, options.ConfigPath} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("bound source %s must be a regular file", path)
		}
	}
	dataset, rowsPerPublication, err := loadDatasetManifest(options.DatasetManifestPath)
	if err != nil {
		return err
	}
	_ = dataset
	controlDSN := os.Getenv("CONTROL_POSTGRES_DSN")
	if controlDSN == "" {
		return errors.New("CONTROL_POSTGRES_DSN is required")
	}
	businessDSNs := make(map[string]string, len(days))
	for _, day := range days {
		name := "SNAPSHOT_POSTGRES_DSN_" + strings.ToUpper(day)
		businessDSNs[day] = os.Getenv(name)
		if businessDSNs[day] == "" {
			return fmt.Errorf("%s is required for its retained deployment", name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cipher, err := control.NewAES256GCM(bytes.Repeat([]byte{0x52}, 32))
	if err != nil {
		return err
	}
	store, err := control.Open(ctx, controlDSN, cipher)
	if err != nil {
		return fmt.Errorf("open isolated Control PostgreSQL: %w", err)
	}
	defer store.Close()
	if err := requireCleanControl(ctx, store); err != nil {
		return err
	}
	principal := mcp.Principal{ID: "rq5-principal-alice", Subject: "alice", Role: "query"}
	if err := store.CreatePrincipal(ctx, control.Principal{
		ID: principal.ID, Subject: principal.Subject, Role: principal.Role, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}

	adapter := newCapturingApproval()
	deployments := make([]*retainedDeployment, 0, len(days))
	fixturePublications := make([]publicationFixtureEvidence, 0, len(days))
	oracles := make([]directOracle, 0, len(days))
	defer func() {
		for _, deployment := range deployments {
			deployment.Connector.Close()
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, day := range days {
		publication, err := loadVerifiedPublication(day, options.InputDirectory, options.ArtifactDirectory)
		if err != nil {
			return fmt.Errorf("load %s publication: %w", day, err)
		}
		if publication.Bundle.RowCount != uint64(rowsPerPublication) {
			return fmt.Errorf("%s Bundle rows=%d, dataset manifest rows=%d", day,
				publication.Bundle.RowCount, rowsPerPublication)
		}
		logicalCatalog, catalogBytes, err := catalogArtifact(publication)
		if err != nil {
			return err
		}
		catalogPath := filepath.Join(options.CatalogDirectory, day+".yaml")
		if err := writeBytesAtomicExclusive(catalogPath, catalogBytes, 0o600); err != nil {
			return err
		}
		catalogDigest, err := fileSHA256(catalogPath)
		if err != nil || catalogDigest != logicalCatalog.SHA256 {
			return fmt.Errorf("%s generated Catalog artifact digest mismatch", day)
		}
		connector, attestation, err := openPublicationConnector(ctx, businessDSNs[day], publication.Input,
			publication.Input.Snapshot.SchemaDigest, businessDatabase+"_"+day)
		if err != nil {
			return fmt.Errorf("open %s retained connector: %w", day, err)
		}
		if attestation.SchemaDigest != publication.Bundle.DictionaryManifest.SchemaDigest {
			connector.Close()
			return fmt.Errorf("%s connector, Bundle, and Catalog schema digests differ", day)
		}
		if err := verifyInstalledSidecar(ctx, connector, publication); err != nil {
			connector.Close()
			return fmt.Errorf("%s installed sidecar: %w", day, err)
		}
		registry, err := ordinal.NewRegistry(publication.Index)
		if err != nil {
			connector.Close()
			return err
		}
		if err := registry.RegisterPublication(ordinal.PublicationKey{
			CatalogDigest: logicalCatalog.SHA256, PublicationName: publication.Input.PublicationName,
		}, publication.Bundle.ManifestDigest, publication.Index); err != nil {
			connector.Close()
			return err
		}
		if err := store.EnforceExposureDeploymentMode(ctx, logicalCatalog.SHA256, true); err != nil {
			connector.Close()
			return err
		}
		if err := store.PutOrdinalSnapshotPublication(ctx, publication.Bundle.ManifestDigest,
			publication.Index, nil); err != nil {
			connector.Close()
			return err
		}
		service, err := gateway.New(gateway.Config{
			Catalog: logicalCatalog, Store: store, Approval: adapter, Connector: connector,
			SnapshotRegistry: registry, CallbackSecret: callbackSecret, Logger: logger,
			Clock: time.Now, Background: ctx,
		})
		if err != nil {
			connector.Close()
			return err
		}
		deployment := &retainedDeployment{Day: day, Catalog: logicalCatalog,
			Publication: publication, Connector: connector, Service: service}
		deployments = append(deployments, deployment)

		oracle, err := directSnapshotOracle(ctx, deployment)
		if err != nil {
			return fmt.Errorf("%s direct frozen snapshot oracle: %w", day, err)
		}
		oracles = append(oracles, oracle)
		binding, err := fixturePublicationBinding(options, deployment, oracle)
		if err != nil {
			return err
		}
		fixturePublications = append(fixturePublications, binding)
	}
	for index := 1; index < len(oracles); index++ {
		if oracles[index].ResultSHA256 == oracles[index-1].ResultSHA256 {
			return fmt.Errorf("direct frozen snapshots %s and %s return the same sentinel result", days[index-1], days[index])
		}
	}

	router, err := newExperimentRouter(store, adapter, principal, callbackSecret, deployments)
	if err != nil {
		return err
	}
	initialTask, err := router.approveRoot(ctx)
	if err != nil {
		return fmt.Errorf("approve day0 root: %w", err)
	}
	if err := requireMaterializationCount(ctx, store, initialTask.ID, 0); err != nil {
		return err
	}
	initialResult, err := router.executePlan(ctx, initialTask.ID, "rq5-day0-first")
	if err != nil {
		return err
	}
	initialReplay, err := semanticReplayValue(initialResult)
	if err != nil || initialReplay {
		return errors.New("day0 first query unexpectedly used semantic replay")
	}
	initialResultDigest, err := resultDigest(initialResult)
	if err != nil || initialResultDigest != oracles[0].ResultSHA256 {
		return errors.New("day0 root result differs from direct frozen snapshot")
	}
	initialCacheKey, err := cacheKeyForTask(ctx, store, initialTask.ID)
	if err != nil {
		return err
	}
	old := routedTaskState{Task: initialTask, ResultSHA256: initialResultDigest, CacheKey: initialCacheKey}

	evidence := onlineEvidence{
		SchemaVersion: onlineEvidenceSchema, RoutingModel: routingModel,
		RowsPerPublication: rowsPerPublication, MeasurementBoundary: measurementBoundary,
		Fixture: fixtureEvidence{
			FixtureClass: "correctness_fixture", RowsPerPublication: rowsPerPublication,
			Publications: fixturePublications,
		},
		Transitions: make([]transitionEvidence, 0, len(days)-1),
	}
	evidence.Fixture.GeneratorSHA256, err = fileSHA256(options.GeneratorPath)
	if err != nil {
		return err
	}
	evidence.Fixture.ConfigSHA256, err = fileSHA256(options.ConfigPath)
	if err != nil {
		return err
	}
	evidence.Fixture.DatasetManifestSHA256, err = fileSHA256(options.DatasetManifestPath)
	if err != nil {
		return err
	}

	for target := 1; target < len(days); target++ {
		transition, next, err := measureTransition(ctx, router, store, old, oracles[target-1], oracles[target], target)
		if err != nil {
			return err
		}
		if err := transition.validate(days[target-1], days[target]); err != nil {
			return err
		}
		evidence.Transitions = append(evidence.Transitions, transition)
		old = next
	}
	if err := evidence.validate(); err != nil {
		return err
	}
	if err := writeJSONAtomicExclusive(options.OutputPath, evidence); err != nil {
		return err
	}
	return nil
}

func measureTransition(ctx context.Context, router *experimentRouter, store *control.Store,
	old routedTaskState, oldOracle, newOracle directOracle, target int) (transitionEvidence, routedTaskState, error) {
	oldPublicationBefore, err := router.persistedPublicationDigest(ctx, old.Task.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	oldLedgerBefore, err := rootLedgerDigest(ctx, store, old.Task.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	switchStarted := time.Now()
	if err := router.switchTo(target); err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	switchWall, err := positiveMilliseconds(time.Since(switchStarted))
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}

	newTask, err := router.approveRoot(ctx)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	if err := requireMaterializationCount(ctx, store, newTask.ID, 0); err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	firstStarted := time.Now()
	firstResult, err := router.executePlan(ctx, newTask.ID, fmt.Sprintf("rq5-%s-first", days[target]))
	firstWall, timingErr := positiveMilliseconds(time.Since(firstStarted))
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	if timingErr != nil {
		return transitionEvidence{}, routedTaskState{}, timingErr
	}
	firstSemanticReplay, err := semanticReplayValue(firstResult)
	if err != nil || firstSemanticReplay {
		return transitionEvidence{}, routedTaskState{}, errors.New("first query on new publication was a semantic replay")
	}
	firstDigest, err := resultDigest(firstResult)
	if err != nil || firstDigest != newOracle.ResultSHA256 {
		return transitionEvidence{}, routedTaskState{}, errors.New("new root result differs from direct frozen snapshot")
	}
	firstCacheKey, err := cacheKeyForTask(ctx, store, newTask.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}

	replayStarted := time.Now()
	replayResult, err := router.executePlan(ctx, newTask.ID, fmt.Sprintf("rq5-%s-replay", days[target]))
	replayWall, timingErr := positiveMilliseconds(time.Since(replayStarted))
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	if timingErr != nil {
		return transitionEvidence{}, routedTaskState{}, timingErr
	}
	replaySemantic, err := semanticReplayValue(replayResult)
	if err != nil || !replaySemantic {
		return transitionEvidence{}, routedTaskState{}, errors.New("second query on new publication did not use semantic replay")
	}
	replayDigest, err := resultDigest(replayResult)
	if err != nil || replayDigest != firstDigest {
		return transitionEvidence{}, routedTaskState{}, errors.New("semantic replay result differs from first query")
	}
	replayCacheKey, err := cacheKeyForTask(ctx, store, newTask.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}

	oldAfterResult, err := router.executePlan(ctx, old.Task.ID, fmt.Sprintf("rq5-%s-after-switch", days[target-1]))
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	oldAfterSemantic, err := semanticReplayValue(oldAfterResult)
	if err != nil || !oldAfterSemantic {
		return transitionEvidence{}, routedTaskState{}, errors.New("old task did not reuse its retained semantic materialization")
	}
	oldAfterDigest, err := resultDigest(oldAfterResult)
	if err != nil || oldAfterDigest != old.ResultSHA256 || oldAfterDigest != oldOracle.ResultSHA256 {
		return transitionEvidence{}, routedTaskState{}, errors.New("old task result changed or differs from direct frozen snapshot")
	}
	oldPublicationAfter, err := router.persistedPublicationDigest(ctx, old.Task.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	oldLedgerAfter, err := rootLedgerDigest(ctx, store, old.Task.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}

	child, err := router.approveChild(ctx, newTask.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	rootPublication, err := router.persistedPublicationDigest(ctx, newTask.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	childPublication, err := router.persistedPublicationDigest(ctx, child.ID)
	if err != nil {
		return transitionEvidence{}, routedTaskState{}, err
	}
	value := transitionEvidence{
		From: days[target-1], To: days[target], SwitchWallMS: switchWall,
		FirstQueryWallMS: firstWall, ReplayWallMS: replayWall,
		OldTask: oldTaskEvidence{
			PublicationDigestBefore: oldPublicationBefore, PublicationDigestAfter: oldPublicationAfter,
			ExpectedPublicationDigest: oldOracle.PublicationDigest,
			ResultSHA256Before:        old.ResultSHA256, ResultSHA256After: oldAfterDigest,
			ExpectedResultSHA256: oldOracle.ResultSHA256,
		},
		NewTask: newTaskEvidence{
			PublicationDigest: rootPublication, ExpectedPublicationDigest: newOracle.PublicationDigest,
			ResultSHA256: firstDigest, ExpectedResultSHA256: newOracle.ResultSHA256,
		},
		OldLedger: oldLedgerEvidence{BeforeSwitchSHA256: oldLedgerBefore, AfterSwitchSHA256: oldLedgerAfter},
		Cache: cacheEvidence{
			OldCacheKeySHA256: old.CacheKey, FirstNewCacheKeySHA256: firstCacheKey,
			FirstNewSemanticReplay: firstSemanticReplay, ReplayNewCacheKeySHA256: replayCacheKey,
			ReplayNewSemanticReplay: replaySemantic,
		},
		Delegation: delegationEvidence{
			RootTaskID: newTask.ID, ChildRootTaskID: child.RootTaskID, ChildParentTaskID: child.ParentTaskID,
			RootPublicationDigest: rootPublication, ChildPublicationDigest: childPublication,
		},
	}
	return value, routedTaskState{Task: newTask, ResultSHA256: firstDigest, CacheKey: firstCacheKey}, nil
}

func requireCleanControl(ctx context.Context, store *control.Store) error {
	var principals, tasks, materializations, cutovers int64
	err := store.DB().QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM principals),
 (SELECT count(*) FROM tasks),
 (SELECT count(*) FROM v4_committed_materializations),
 (SELECT count(*) FROM v4_cutover_state)`).Scan(&principals, &tasks, &materializations, &cutovers)
	if err != nil {
		return err
	}
	if principals != 0 || tasks != 0 || materializations != 0 || cutovers != 0 {
		return errors.New("isolated Control PostgreSQL is not empty")
	}
	return nil
}

func loadDatasetManifest(path string) (datasetManifest, int64, error) {
	var manifest datasetManifest
	if err := decodeJSONFileStrict(path, &manifest); err != nil {
		return datasetManifest{}, 0, fmt.Errorf("decode dataset manifest: %w", err)
	}
	if manifest.SchemaVersion != "taskgate-daily-publication-dataset-v1" ||
		manifest.Generator != "deterministic TPC-H-shaped orders/lineitem fixture" ||
		manifest.PostgresVersion == "" {
		return datasetManifest{}, 0, errors.New("dataset manifest identity is invalid")
	}
	rows := manifest.Rows[days[0]]
	if rows < 500 || rows > 345000 || rows%500 != 0 {
		return datasetManifest{}, 0, errors.New("dataset row count is outside the declared fixture range")
	}
	for _, day := range days {
		if manifest.Rows[day] != rows || !md5Regexp.MatchString(manifest.OrderedRowFingerprint[day]) {
			return datasetManifest{}, 0, errors.New("dataset day row count or fingerprint is invalid")
		}
	}
	expected := map[string]datasetChange{
		"day1": {UpdatedRows: rows / 100},
		"day2": {UpdatedRows: rows * 5 / 100},
		"day3": {UpdatedRows: rows * 10 / 100, InsertedRows: rows / 100, DeletedRows: rows / 100},
	}
	for day, change := range expected {
		if manifest.ChangesFromPrevious[day] != change {
			return datasetManifest{}, 0, fmt.Errorf("dataset %s change schedule is invalid", day)
		}
	}
	return manifest, rows, nil
}

func verifyInstalledSidecar(ctx context.Context, connector *dataconnector.Connector,
	publication loadedPublication) error {
	query := fmt.Sprintf(`SELECT
 (SELECT count(*) FROM %s),
 (SELECT count(*) FROM %s),
 NOT EXISTS (
   (SELECT l_orderkey,l_linenumber FROM %s EXCEPT SELECT l_orderkey,l_linenumber FROM %s)
   UNION ALL
   (SELECT l_orderkey,l_linenumber FROM %s EXCEPT SELECT l_orderkey,l_linenumber FROM %s)
 )`, publication.Input.SourceRelation, publication.Input.OrdinalSidecar,
		publication.Input.SourceRelation, publication.Input.OrdinalSidecar,
		publication.Input.OrdinalSidecar, publication.Input.SourceRelation)
	result, err := connector.Query(ctx, dataconnector.QueryRequest{SQL: query, StatementTimeout: 30 * time.Second, MaxRows: 1})
	if err != nil {
		return err
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 3 {
		return errors.New("sidecar verification query returned an invalid shape")
	}
	sourceRows, ok := integerValue(result.Rows[0][0])
	if !ok {
		return errors.New("source row count is not an integer")
	}
	sidecarRows, ok := integerValue(result.Rows[0][1])
	equal, equalOK := result.Rows[0][2].(bool)
	if !ok || !equalOK || !equal || sourceRows != sidecarRows || sourceRows != int64(publication.Bundle.RowCount) {
		return errors.New("installed sidecar and frozen reporting entity sets differ")
	}
	return nil
}

func directSnapshotOracle(ctx context.Context, deployment *retainedDeployment) (directOracle, error) {
	sql := fmt.Sprintf(`SELECT l_orderkey,l_linenumber,l_extendedprice
FROM %s
WHERE dataset_partition = 1 AND l_orderkey = 1
ORDER BY l_linenumber ASC
LIMIT 10`, deployment.Publication.Input.SourceRelation)
	result, err := deployment.Connector.Query(ctx, dataconnector.QueryRequest{
		SQL: sql, StatementTimeout: 15 * time.Second, MaxRows: 10,
	})
	if err != nil {
		return directOracle{}, err
	}
	if result.RowCount != 5 || len(result.Rows) != 5 {
		return directOracle{}, fmt.Errorf("sentinel query rows=%d, want 5", result.RowCount)
	}
	digest, err := experiment.CanonicalResultHash(result.Rows)
	if err != nil {
		return directOracle{}, err
	}
	return directOracle{PublicationDigest: deployment.Publication.Bundle.ManifestDigest, ResultSHA256: digest}, nil
}

func fixturePublicationBinding(options runOptions, deployment *retainedDeployment,
	oracle directOracle) (publicationFixtureEvidence, error) {
	publication := deployment.Publication
	bundlePath := filepath.Join(publication.Directory, publication.Input.PublicationName+".bundle.json")
	bundleDigest, err := fileSHA256(bundlePath)
	if err != nil {
		return publicationFixtureEvidence{}, err
	}
	inputDigest, err := fileSHA256(filepath.Join(options.InputDirectory, publication.Day+".json"))
	if err != nil {
		return publicationFixtureEvidence{}, err
	}
	return publicationFixtureEvidence{
		Day: publication.Day, PublicationName: publication.Input.PublicationName, RowCount: publication.Bundle.RowCount,
		ApprovedInputSHA256: inputDigest, CatalogSHA256: deployment.Catalog.SHA256,
		BundleManifestSHA256: bundleDigest, PublicationManifestDigest: publication.Bundle.ManifestDigest,
		DictionaryDigest:  publication.Bundle.DictionaryManifest.DictionaryDigest,
		SidecarDigest:     publication.Bundle.DictionaryManifest.SidecarDigest,
		SchemaDigest:      publication.Bundle.DictionaryManifest.SchemaDigest,
		HotArtifactSHA256: publication.Bundle.Hot.SHA256, ColdArtifactSHA256: publication.Bundle.Cold.SHA256,
		SidecarArtifactSHA256: publication.Bundle.Sidecar.SHA256, DirectResultSHA256: oracle.ResultSHA256,
	}, nil
}

func resultDigest(result map[string]any) (string, error) {
	rows, ok := result["rows"].([][]any)
	if !ok {
		return "", fmt.Errorf("public query rows have type %T", result["rows"])
	}
	return normalizedJSONDigest(rows)
}

func semanticReplayValue(result map[string]any) (bool, error) {
	value, present := result["semantic_replay"]
	if !present {
		// The public Service currently omits this member on the novel path and
		// emits literal true on the semantic replay path. The caller separately
		// proves the novel path started with no Control materialization.
		return false, nil
	}
	boolean, ok := value.(bool)
	if !ok {
		return false, errors.New("semantic_replay response member is not boolean")
	}
	return boolean, nil
}

func requireMaterializationCount(ctx context.Context, store *control.Store, taskID string, expected int) error {
	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM v4_committed_materializations WHERE task_id=$1`, taskID).Scan(&count); err != nil {
		return err
	}
	if count != expected {
		return fmt.Errorf("task %s materializations=%d, want %d", taskID, count, expected)
	}
	return nil
}

func cacheKeyForTask(ctx context.Context, store *control.Store, taskID string) (string, error) {
	rows, err := store.DB().QueryContext(ctx,
		`SELECT cache_key_sha256 FROM v4_committed_materializations WHERE task_id=$1`, taskID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return "", err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(values) != 1 || !sha256Regexp.MatchString(values[0]) {
		return "", fmt.Errorf("task %s has %d valid semantic materializations, want 1", taskID, len(values))
	}
	return values[0], nil
}

func rootLedgerDigest(ctx context.Context, store *control.Store, taskID string) (string, error) {
	head, err := store.GetOrdinalRootHead(ctx, taskID)
	if err != nil {
		return "", err
	}
	return approval.CanonicalSHA256(struct {
		RootTaskID          string                 `json:"root_task_id"`
		ProfileVersion      string                 `json:"profile_version"`
		DictionarySetDigest string                 `json:"dictionary_set_digest"`
		Epoch               int64                  `json:"epoch"`
		Limits              control.ExposureLimits `json:"limits"`
		Used                control.ExposureLimits `json:"used"`
		ReleaseSetSHA256    string                 `json:"release_set_sha256"`
		InfluenceSetSHA256  string                 `json:"influence_set_sha256"`
		OutcomeSetSHA256    string                 `json:"outcome_set_sha256"`
	}{
		RootTaskID: head.RootTaskID, ProfileVersion: head.ProfileVersion,
		DictionarySetDigest: head.DictionarySetDigest, Epoch: head.Epoch, Limits: head.Limits, Used: head.Used,
		ReleaseSetSHA256: head.ReleaseSetSHA256, InfluenceSetSHA256: head.InfluenceSetSHA256,
		OutcomeSetSHA256: head.OutcomeSetSHA256,
	})
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int32:
		return int64(typed), true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
