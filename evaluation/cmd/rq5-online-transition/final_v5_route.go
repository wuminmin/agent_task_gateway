package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/control"
)

type finalV5RootSnapshot struct {
	RootTaskID          string                 `json:"root_task_id"`
	ProfileVersion      string                 `json:"profile_version"`
	DictionarySetDigest string                 `json:"dictionary_set_digest"`
	Epoch               int64                  `json:"epoch"`
	Limits              control.ExposureLimits `json:"limits"`
	Used                control.ExposureLimits `json:"used"`
	ReleaseSetSHA256    string                 `json:"release_set_sha256"`
	InfluenceSetSHA256  string                 `json:"influence_set_sha256"`
	OutcomeSetSHA256    string                 `json:"outcome_set_sha256"`
}

func (snapshot finalV5RootSnapshot) ledgerSHA256() string {
	encoded, _ := approval.CanonicalJSON(snapshot)
	return finalV5Hash(encoded)
}

func (snapshot finalV5RootSnapshot) setSHA256() string {
	return finalV5Hash([]byte(snapshot.ReleaseSetSHA256 + "\x00" + snapshot.InfluenceSetSHA256 +
		"\x00" + snapshot.OutcomeSetSHA256))
}

type finalV5MeasuredQuery struct {
	evidence experiment.RQ5QueryEvidence
	task     control.Task
	before   finalV5RootSnapshot
	after    finalV5RootSnapshot
	cacheKey string
}

func (environment *finalV5CycleEnvironment) executeRoute(ctx context.Context) (experiment.RQ5RouteEvidence, error) {
	var route experiment.RQ5RouteEvidence
	oldBinding, oldOK := environment.publications[environment.request.FromDay]
	newBinding, newOK := environment.publications[environment.request.ToDay]
	if !oldOK || !newOK {
		return route, errors.New("old/new RQ5 publication binding is absent")
	}
	oldTarget := finalV5SlotTarget(oldBinding)
	newTarget := finalV5SlotTarget(newBinding)
	fullStarted := time.Now()
	if err := environment.slot.Start(ctx, oldTarget, "start_retained_old"); err != nil {
		return route, err
	}
	oldRuntime, err := activeFinalV5Runtime(environment.slot)
	if err != nil {
		return route, err
	}
	oldTask, err := requestAndApproveLive(ctx, oldRuntime.service, environment.store, environment.oa,
		environment.principal, "")
	if err != nil {
		return route, fmt.Errorf("approve retained old root: %w", err)
	}
	oldInitial, err := environment.measureQuery(ctx, oldRuntime, oldTask,
		"rq5-"+environment.request.Operation.SampleID+"-old-initial", false, oldBinding)
	if err != nil {
		return route, err
	}

	switchStarted := time.Now()
	if err := environment.slot.Stop("stop_old_for_new_activation"); err != nil {
		return route, err
	}
	if err := environment.slot.Start(ctx, newTarget, "start_new_after_activation"); err != nil {
		return route, err
	}
	newRuntime, err := activeFinalV5Runtime(environment.slot)
	if err != nil {
		return route, err
	}
	newTask, err := requestAndApproveLive(ctx, newRuntime.service, environment.store, environment.oa,
		environment.principal, "")
	if err != nil {
		return route, fmt.Errorf("approve activated new root: %w", err)
	}
	newInitial, err := environment.measureQuery(ctx, newRuntime, newTask,
		"rq5-"+environment.request.Operation.SampleID+"-new-initial", false, newBinding)
	if err != nil {
		return route, err
	}
	route.SwitchToNewWallMS = finalV5DurationMS(time.Since(switchStarted))

	retainedStarted := time.Now()
	if err := environment.slot.Stop("stop_new_for_retained_check"); err != nil {
		return route, err
	}
	if err := environment.slot.Start(ctx, oldTarget, "start_old_for_retained_check"); err != nil {
		return route, err
	}
	oldRuntime, err = activeFinalV5Runtime(environment.slot)
	if err != nil {
		return route, err
	}
	oldRetained, err := environment.measureQuery(ctx, oldRuntime, oldTask,
		"rq5-"+environment.request.Operation.SampleID+"-old-retained", true, oldBinding)
	if err != nil {
		return route, err
	}
	if err := environment.slot.Stop("stop_old_for_new_restore"); err != nil {
		return route, err
	}
	route.RetainedCheckWallMS = finalV5DurationMS(time.Since(retainedStarted))

	restoreStarted := time.Now()
	if err := environment.slot.Start(ctx, newTarget, "restore_new_after_retained_check"); err != nil {
		return route, err
	}
	newRuntime, err = activeFinalV5Runtime(environment.slot)
	if err != nil {
		return route, err
	}
	newRestored, err := environment.measureQuery(ctx, newRuntime, newTask,
		"rq5-"+environment.request.Operation.SampleID+"-new-restored", true, newBinding)
	if err != nil {
		return route, err
	}
	child, err := requestAndApproveLive(ctx, newRuntime.service, environment.store, environment.oa,
		environment.principal, newTask.ID)
	if err != nil {
		return route, fmt.Errorf("approve restored new delegated child: %w", err)
	}
	if child.ParentTaskID != newTask.ID || child.RootTaskID != newTask.RootTaskID || child.ID == newTask.ID ||
		child.CatalogVersion != newTask.CatalogVersion {
		return route, errors.New("delegated RQ5 child is not bound to its restored root Catalog")
	}
	rootPublication, err := loadFinalV5PersistedPublication(ctx, environment.store, newTask.ID)
	if err != nil {
		return route, fmt.Errorf("read restored root publication from Control: %w", err)
	}
	childPublication, err := loadFinalV5PersistedPublication(ctx, environment.store, child.ID)
	if err != nil {
		return route, fmt.Errorf("read delegated child publication from Control: %w", err)
	}
	for _, observed := range []finalV5PersistedPublication{rootPublication, childPublication} {
		if observed.CatalogSHA256 != newBinding.evidence.CatalogSHA256 ||
			observed.PublicationName != newBinding.evidence.PublicationName ||
			observed.ManifestSHA256 != newBinding.evidence.PublicationManifestSHA256 {
			return route, errors.New("Control root/child dictionary set differs from the active new publication")
		}
	}
	if rootPublication.DictionarySetSHA256 != childPublication.DictionarySetSHA256 {
		return route, errors.New("delegated child and restored root do not share one persisted dictionary set")
	}
	if err := environment.slot.Stop("stop_new_cycle_complete"); err != nil {
		return route, err
	}
	route.RestoreNewWallMS = finalV5DurationMS(time.Since(restoreStarted))
	route.FullRouteWallMS = finalV5DurationMS(time.Since(fullStarted))

	route.OldPublicationSHA256 = oldBinding.evidence.PublicationManifestSHA256
	route.NewPublicationSHA256 = newBinding.evidence.PublicationManifestSHA256
	route.OldCatalogSHA256 = oldBinding.evidence.CatalogSHA256
	route.NewCatalogSHA256 = newBinding.evidence.CatalogSHA256
	route.OldTaskIDHash = finalV5IdentityHash(environment.request, "task", oldTask.ID)
	route.NewTaskIDHash = finalV5IdentityHash(environment.request, "task", newTask.ID)
	route.NewRootTaskIDHash = finalV5IdentityHash(environment.request, "task", newTask.RootTaskID)
	route.ChildTaskIDHash = finalV5IdentityHash(environment.request, "task", child.ID)
	route.ChildParentTaskIDHash = finalV5IdentityHash(environment.request, "task", child.ParentTaskID)
	route.ChildRootTaskIDHash = finalV5IdentityHash(environment.request, "task", child.RootTaskID)
	route.OldLedgerBeforeSHA256 = oldInitial.after.ledgerSHA256()
	route.OldLedgerAfterSHA256 = oldRetained.after.ledgerSHA256()
	route.NewLedgerBeforeRestore = newInitial.after.ledgerSHA256()
	route.NewLedgerAfterRestore = newRestored.after.ledgerSHA256()
	route.OldCacheKeySHA256 = oldInitial.cacheKey
	route.NewCacheKeySHA256 = newInitial.cacheKey
	route.CrossReplaySourceSHA256 = oldBinding.evidence.PublicationManifestSHA256
	route.CrossReplayTargetSHA256 = newBinding.evidence.PublicationManifestSHA256
	route.CrossPublicationReplayHit = newInitial.evidence.SemanticReplay
	route.ChildPublicationSHA256 = childPublication.ManifestSHA256
	route.RootPublicationSHA256 = rootPublication.ManifestSHA256
	route.OldInitial = oldInitial.evidence
	route.NewInitial = newInitial.evidence
	route.OldRetained = oldRetained.evidence
	route.NewRestored = newRestored.evidence
	if oldInitial.after.ledgerSHA256() != oldRetained.before.ledgerSHA256() ||
		newInitial.after.ledgerSHA256() != newRestored.before.ledgerSHA256() ||
		oldRetained.cacheKey != oldInitial.cacheKey || newRestored.cacheKey != newInitial.cacheKey ||
		oldInitial.cacheKey == newInitial.cacheKey {
		return route, errors.New("RQ5 retained ledgers or Catalog-partitioned semantic cache keys changed")
	}
	return route, nil
}

type finalV5PersistedPublication struct {
	DictionarySetSHA256 string
	CatalogSHA256       string
	PublicationName     string
	ManifestSHA256      string
}

func loadFinalV5PersistedPublication(ctx context.Context, store *control.Store,
	taskID string) (finalV5PersistedPublication, error) {
	var value finalV5PersistedPublication
	var members int64
	err := store.DB().QueryRowContext(ctx, `
SELECT h.dictionary_set_digest,s.catalog_digest,min(m.publication_name),min(m.manifest_digest),count(*)
FROM tasks t
JOIN v5_exposure_root_heads h ON h.root_task_id=t.root_task_id
JOIN v4_dictionary_sets s ON s.dictionary_set_digest=h.dictionary_set_digest
JOIN v4_dictionary_set_members m ON m.dictionary_set_digest=s.dictionary_set_digest
WHERE t.id=$1
GROUP BY h.dictionary_set_digest,s.catalog_digest`, taskID).Scan(
		&value.DictionarySetSHA256, &value.CatalogSHA256, &value.PublicationName, &value.ManifestSHA256, &members)
	if err != nil {
		return value, err
	}
	if members != 1 || !sha256Regexp.MatchString(value.DictionarySetSHA256) ||
		!sha256Regexp.MatchString(value.CatalogSHA256) || value.PublicationName == "" ||
		!sha256Regexp.MatchString(value.ManifestSHA256) {
		return value, errors.New("task does not resolve to exactly one valid persisted publication member")
	}
	return value, nil
}

func finalV5SlotTarget(binding finalV5PublicationRuntimeBinding) sequentialSlotTarget {
	return sequentialSlotTarget{Day: binding.evidence.Day, CatalogSHA256: binding.evidence.CatalogSHA256,
		PublicationSHA256: binding.evidence.PublicationManifestSHA256,
		HOTArtifactBytes:  binding.evidence.HOTArtifactBytes}
}

func (environment *finalV5CycleEnvironment) measureQuery(ctx context.Context, runtime *finalV5Runtime,
	task control.Task, requestID string, wantReplay bool,
	binding finalV5PublicationRuntimeBinding) (finalV5MeasuredQuery, error) {
	var measured finalV5MeasuredQuery
	if runtime == nil || runtime.service == nil || runtime.connector == nil || task.ID == "" {
		return measured, errors.New("live RQ5 query has no active production runtime or task")
	}
	if runtime.catalog.SHA256 != binding.evidence.CatalogSHA256 || task.CatalogVersion != runtime.catalog.CatalogVersion {
		return measured, errors.New("live RQ5 task is not routed to the active slot Catalog")
	}
	before, err := loadFinalV5RootSnapshot(ctx, environment.store, task.ID)
	if err != nil {
		return measured, err
	}
	beforeCalls := runtime.connector.Count()
	started := time.Now()
	responseMap, err := callTool(ctx, runtime.service, environment.principal, "execute_plan", map[string]any{
		"task_id": task.ID, "request_id": requestID,
		"plan": map[string]any{
			"product":  "daily_lineitem",
			"columns":  []string{"l_orderkey", "l_linenumber", "l_extendedprice"},
			"filters":  []map[string]any{{"column": "l_orderkey", "op": "=", "value": 1}},
			"order_by": []map[string]any{{"column": "l_linenumber", "direction": "asc"}},
			"limit":    10,
		},
	})
	available := finalV5DurationMS(time.Since(started))
	if err != nil {
		return measured, err
	}
	encoded, err := json.Marshal(responseMap)
	if err != nil {
		return measured, err
	}
	var response finalV5QueryResponse
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil {
		return measured, err
	}
	if response.TaskID != task.ID || response.ArtifactStatus != "AVAILABLE" || response.QueryID == "" ||
		response.ResultID == "" || response.SemanticReplay != wantReplay || response.IdempotentReplay {
		return measured, errors.New("live RQ5 query response lacks its expected novel/replay AVAILABLE identity")
	}
	after, err := loadFinalV5RootSnapshot(ctx, environment.store, task.ID)
	if err != nil {
		return measured, err
	}
	businessDelta := runtime.connector.Count() - beforeCalls
	verified, err := environment.verifyQuery(ctx, response, task, binding, before, after,
		businessDelta, available, started)
	if err != nil {
		return measured, err
	}
	cacheKey, err := finalV5CacheKey(ctx, environment.store, task.ID)
	if err != nil {
		return measured, err
	}
	return finalV5MeasuredQuery{evidence: verified, task: task, before: before, after: after,
		cacheKey: cacheKey}, nil
}

func loadFinalV5RootSnapshot(ctx context.Context, store *control.Store,
	taskID string) (finalV5RootSnapshot, error) {
	var value finalV5RootSnapshot
	err := store.DB().QueryRowContext(ctx, `
SELECT h.root_task_id,h.profile_version,COALESCE(h.dictionary_set_digest,''),h.epoch,
 h.max_release_facts,h.max_influence_facts,h.max_outcome_facts,
 h.used_release_facts,h.used_influence_facts,h.used_outcome_facts,
 COALESCE(h.release_set_sha256,''),COALESCE(h.influence_set_sha256,''),COALESCE(h.outcome_set_sha256,'')
FROM tasks t JOIN v5_exposure_root_heads h ON h.root_task_id=t.root_task_id WHERE t.id=$1`, taskID).
		Scan(&value.RootTaskID, &value.ProfileVersion, &value.DictionarySetDigest, &value.Epoch,
			&value.Limits.ReleaseFacts, &value.Limits.InfluenceFacts, &value.Limits.OutcomeFacts,
			&value.Used.ReleaseFacts, &value.Used.InfluenceFacts, &value.Used.OutcomeFacts,
			&value.ReleaseSetSHA256, &value.InfluenceSetSHA256, &value.OutcomeSetSHA256)
	if err != nil {
		return value, err
	}
	if value.RootTaskID == "" || value.ProfileVersion != "taskgate-exposure-v5" || value.Epoch < 0 {
		return value, errors.New("RQ5 V5 root snapshot identity is invalid")
	}
	return value, nil
}

func finalV5CacheKey(ctx context.Context, store *control.Store, taskID string) (string, error) {
	rows, err := store.DB().QueryContext(ctx,
		`SELECT cache_key_sha256 FROM v5_committed_materializations WHERE task_id=$1 ORDER BY cache_key_sha256`, taskID)
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
		return "", fmt.Errorf("RQ5 task has %d valid V5 semantic cache keys, want one", len(values))
	}
	return values[0], nil
}

func finalV5DurationMS(value time.Duration) float64 {
	result := float64(value.Nanoseconds()) / float64(time.Millisecond)
	if result <= 0 {
		return 0.000001
	}
	return result
}
