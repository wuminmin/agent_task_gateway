package finalv5publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5dataset"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
)

const (
	liveObservationVersion = "taskgate-final-v5-publication-live-observation-v1"
	liveSessionProbeSQL    = `SELECT current_database()::text, current_user::text,
       current_setting('server_version_num')::text,
       pg_backend_pid()::bigint, txid_current_snapshot()::text`
	scaleCandidateDirectPath = "sql/contracts/scale-dependency-candidate-direct.sql"
	scaleHistoryDirectPath   = "sql/contracts/scale-dependency-history-direct.sql"
	datasetProbeContractPath = "sql/datasets/benchmark-v1-probe.sql"
)

// DigestReference names exact source-controlled bytes without retaining their
// contents in generated evidence.
type DigestReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// QueryObservation is a complete-drain comparison against an independent,
// pre-run oracle. Neither SQL nor result rows are retained.
type QueryObservation struct {
	Workload     string                      `json:"workload"`
	Cell         string                      `json:"cell"`
	Role         string                      `json:"role"`
	QuerySHA256  string                      `json:"query_sha256"`
	OracleInputs []DigestReference           `json:"oracle_inputs"`
	Expected     finalv5oracle.ResultSummary `json:"expected"`
	Actual       finalv5oracle.ResultSummary `json:"actual"`
	Matched      bool                        `json:"matched"`
}

// DatasetProbeObservation keeps the source-query and returned-scalar
// identities separate. The scalar itself is never retained.
type DatasetProbeObservation struct {
	SourcePath   string `json:"source_path"`
	SourceSHA256 string `json:"source_sha256"`
	ResultSHA256 string `json:"result_sha256"`
}

// LiveObservation is generated only after every fixed query has run and
// matched. SessionIdentitySHA256 commits a live backend PID and repeatable-read
// snapshot without exposing either value.
type LiveObservation struct {
	Version                    string                  `json:"version"`
	StartedAtUTC               string                  `json:"started_at_utc"`
	CompletedAtUTC             string                  `json:"completed_at_utc"`
	Database                   string                  `json:"database"`
	User                       string                  `json:"user"`
	PostgreSQLServerVersionNum string                  `json:"postgresql_server_version_num"`
	QueryExecMode              string                  `json:"query_exec_mode"`
	TransactionIsolation       string                  `json:"transaction_isolation"`
	SessionProbeSQLSHA256      string                  `json:"session_probe_sql_sha256"`
	SessionIdentitySHA256      string                  `json:"session_identity_sha256"`
	PreparedStatementsBefore   int64                   `json:"prepared_statements_before"`
	PreparedStatementsAfter    int64                   `json:"prepared_statements_after"`
	QueryCount                 int                     `json:"query_count"`
	Queries                    []QueryObservation      `json:"queries"`
	DatasetProbe               DatasetProbeObservation `json:"dataset_probe"`
	ObservationSHA256          string                  `json:"observation_sha256"`
}

type liveSessionIdentity struct {
	Database         string `json:"database"`
	User             string `json:"user"`
	ServerVersionNum string `json:"server_version_num"`
	BackendPID       int64  `json:"backend_pid"`
	Snapshot         string `json:"snapshot"`
}

// ObservePublicationClosure executes only the closed E1 queries constructed
// inside this package. It accepts no SQL and exposes no generic query API.
func ObservePublicationClosure(ctx context.Context, dsn string, runtime *finalv5contracts.Runtime,
	scaleManifests []finalv5oracle.ExposureScaleManifestArtifact,
	provSQLManifests []finalv5oracle.ProvSQLManifestArtifact) (LiveObservation, error) {
	var result LiveObservation
	if ctx == nil || strings.TrimSpace(dsn) == "" || runtime == nil {
		return result, errors.New("live publication observation requires context, DSN, and verified Contract runtime")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return result, errors.New("parse Business PostgreSQL DSN")
	}
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.StatementCacheCapacity = 0
	config.DescriptionCacheCapacity = 0
	if config.RuntimeParams == nil {
		config.RuntimeParams = make(map[string]string)
	}
	config.RuntimeParams["default_transaction_read_only"] = "on"
	config.RuntimeParams["search_path"] = "pg_catalog"
	config.RuntimeParams["statement_timeout"] = "1800000"
	config.RuntimeParams["application_name"] = "taskgate-final-v5-publication-binding-e1"
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return result, errors.New("connect Business PostgreSQL for publication observation")
	}
	defer connection.Close(context.Background())
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, errors.New("begin read-only repeatable-read publication observation")
	}
	defer tx.Rollback(context.Background())

	result.Version = liveObservationVersion
	result.StartedAtUTC = time.Now().UTC().Format(time.RFC3339Nano)
	result.QueryExecMode = "simple_protocol"
	result.TransactionIsolation = "repeatable_read_read_only"
	result.SessionProbeSQLSHA256 = sha256Hex([]byte(liveSessionProbeSQL))
	beforeSession, beforeDigest, err := readLiveSessionIdentity(ctx, tx)
	if err != nil {
		return LiveObservation{}, err
	}
	result.Database, result.User = beforeSession.Database, beforeSession.User
	result.PostgreSQLServerVersionNum = beforeSession.ServerVersionNum
	result.SessionIdentitySHA256 = beforeDigest
	result.PreparedStatementsBefore, err = finalv5dataset.PreparedStatementCount(ctx, tx)
	if err != nil || result.PreparedStatementsBefore != 0 {
		return LiveObservation{}, errors.New("publication observation session is not free of prepared statements before queries")
	}

	observations, err := observeFixedQueries(ctx, tx, runtime, scaleManifests, provSQLManifests)
	if err != nil {
		return LiveObservation{}, err
	}
	result.Queries = observations
	result.QueryCount = len(observations)
	if result.QueryCount != 135 {
		return LiveObservation{}, fmt.Errorf("publication observation ran %d fixed queries; expected exactly 135", result.QueryCount)
	}
	result.DatasetProbe, err = observeDatasetProbe(ctx, tx, runtime)
	if err != nil {
		return LiveObservation{}, err
	}
	result.PreparedStatementsAfter, err = finalv5dataset.PreparedStatementCount(ctx, tx)
	if err != nil || result.PreparedStatementsAfter != 0 {
		return LiveObservation{}, errors.New("publication observation session is not free of prepared statements after queries")
	}
	afterSession, afterDigest, err := readLiveSessionIdentity(ctx, tx)
	if err != nil || beforeSession != afterSession || beforeDigest != afterDigest {
		return LiveObservation{}, errors.New("publication observation session or repeatable-read snapshot changed")
	}
	result.CompletedAtUTC = time.Now().UTC().Format(time.RFC3339Nano)
	result.ObservationSHA256, err = liveObservationDigest(result)
	if err != nil {
		return LiveObservation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return LiveObservation{}, errors.New("commit read-only publication observation")
	}
	return result, nil
}

func observeFixedQueries(ctx context.Context, tx pgx.Tx, runtime *finalv5contracts.Runtime,
	scaleManifests []finalv5oracle.ExposureScaleManifestArtifact,
	provSQLManifests []finalv5oracle.ProvSQLManifestArtifact) ([]QueryObservation, error) {
	result := make([]QueryObservation, 0, 135)
	candidateSQL, candidateDigest, err := indexedSQL(runtime, scaleCandidateDirectPath,
		finalv5oracle.ExposureScaleCandidateDirectQuerySHA256)
	if err != nil {
		return nil, err
	}
	historySQL, historyDigest, err := indexedSQL(runtime, scaleHistoryDirectPath,
		finalv5oracle.ExposureScaleHistoryDirectQuerySHA256)
	if err != nil {
		return nil, err
	}
	scaleByCell := make(map[string][]finalv5oracle.ExposureScaleManifestArtifact, 12)
	for _, artifact := range scaleManifests {
		scaleByCell[artifact.Manifest.Scale] = append(scaleByCell[artifact.Manifest.Scale], artifact)
	}
	for _, cell := range finalv5oracle.ExposureScaleDependencyCells() {
		manifests := scaleByCell[cell.Scale]
		if len(manifests) != 2 {
			return nil, fmt.Errorf("Scale cell %s does not have its exact novel/replay oracle pair", cell.Scale)
		}
		if !reflect.DeepEqual(manifests[0].Manifest.Expected, manifests[1].Manifest.Expected) {
			return nil, fmt.Errorf("Scale cell %s novel/replay oracle results differ", cell.Scale)
		}
		inputs := []DigestReference{{Path: pathJoinOracle(manifests[0].RelativePath), SHA256: manifests[0].SHA256},
			{Path: pathJoinOracle(manifests[1].RelativePath), SHA256: manifests[1].SHA256}}
		expected, err := manifestResultSummary(manifests[0].Manifest)
		if err != nil {
			return nil, fmt.Errorf("Scale candidate oracle %s: %w", cell.Scale, err)
		}
		rows := cell.CandidateFacts / finalv5oracle.ExposureScaleFactsPerRow
		actual, err := runFixedResultQuery(ctx, tx, candidateSQL,
			[]finalv5oracle.ResultColumn{{Name: "member_count", Type: finalv5oracle.SQLBigInt}}, []any{rows})
		if err != nil {
			return nil, fmt.Errorf("observe Scale candidate %s: %w", cell.Scale, err)
		}
		observation, err := matchedObservation("scale/dependency-e2e", cell.Scale, "candidate",
			candidateDigest, inputs, expected, actual)
		if err != nil {
			return nil, err
		}
		result = append(result, observation)
		historyExpected, err := finalv5oracle.ExposureScaleHistoryResultSummary(cell.Scale)
		if err != nil {
			return nil, err
		}
		overlapRows := cell.OverlapFacts / finalv5oracle.ExposureScaleFactsPerRow
		historyActual, err := runFixedResultQuery(ctx, tx, historySQL,
			[]finalv5oracle.ResultColumn{{Name: "history_total", Type: finalv5oracle.SQLNumeric}},
			[]any{rows - overlapRows, 2*rows - overlapRows})
		if err != nil {
			return nil, fmt.Errorf("observe Scale history %s: %w", cell.Scale, err)
		}
		observation, err = matchedObservation("scale/dependency-e2e", cell.Scale, "history",
			historyDigest, inputs, historyExpected, historyActual)
		if err != nil {
			return nil, err
		}
		result = append(result, observation)
	}

	artifactCells, err := runtime.ArtifactCells()
	if err != nil || len(artifactCells) != 6 {
		return nil, errors.New("Contract runtime does not contain the exact six Artifact cells")
	}
	for _, cell := range artifactCells {
		query, err := runtime.QueryContract(cell)
		if err != nil {
			return nil, err
		}
		manifest, manifestSHA, err := runtime.OracleManifest(cell)
		if err != nil {
			return nil, err
		}
		expected, err := manifestResultSummary(manifest)
		if err != nil {
			return nil, err
		}
		actual, err := runFixedResultQuery(ctx, tx, query.Direct.SQL, query.Schema, nil)
		if err != nil {
			return nil, fmt.Errorf("observe Artifact %s: %w", cell.Identity.Scale, err)
		}
		observation, err := matchedObservation("artifact/result-heavy", cell.Identity.Scale, "direct",
			query.Direct.SQLSHA256,
			[]DigestReference{{Path: pathJoinContract(cell.OracleManifestPath), SHA256: manifestSHA}},
			expected, actual)
		if err != nil {
			return nil, err
		}
		result = append(result, observation)
	}

	provByKey := make(map[string]finalv5oracle.ProvSQLManifestArtifact, len(provSQLManifests))
	for _, artifact := range provSQLManifests {
		provByKey[artifact.Manifest.BindingKey] = artifact
	}
	for _, cell := range finalv5oracle.ProvSQLNonceJoinCells() {
		artifact, present := provByKey[cell.BindingKey]
		if !present {
			return nil, fmt.Errorf("ProvSQL cell %s lacks its verified oracle manifest", cell.BindingKey)
		}
		query, err := provsqlfixture.BusinessSQL(cell.Scale, cell.Nonce)
		if err != nil {
			return nil, err
		}
		expected, err := manifestResultSummary(artifact.Manifest)
		if err != nil {
			return nil, err
		}
		actual, err := runFixedResultQuery(ctx, tx, query, finalv5oracle.ProvSQLResultSchema(), nil)
		if err != nil {
			return nil, fmt.Errorf("observe ProvSQL %s: %w", cell.BindingKey, err)
		}
		observation, err := matchedObservation("provsql/nonce-join-group", cell.BindingKey, "direct",
			provsqlfixture.SHA256String(query),
			[]DigestReference{{Path: pathJoinOracle(artifact.RelativePath), SHA256: artifact.SHA256}}, expected, actual)
		if err != nil {
			return nil, err
		}
		result = append(result, observation)
	}
	return result, nil
}

func indexedSQL(runtime *finalv5contracts.Runtime, path, expected string) (string, string, error) {
	digest, err := runtime.ContractSHA256(path)
	if err != nil || digest != expected {
		return "", "", fmt.Errorf("indexed fixed query %s differs from its reviewed SHA-256", path)
	}
	value, err := runtime.ContractBytes(path)
	if err != nil || sha256Hex(value) != digest {
		return "", "", fmt.Errorf("indexed fixed query %s bytes drifted", path)
	}
	return string(value), digest, nil
}

func runFixedResultQuery(ctx context.Context, tx pgx.Tx, query string,
	columns []finalv5oracle.ResultColumn, arguments []any) (finalv5oracle.ResultSummary, error) {
	queryArguments := make([]any, 0, len(arguments)+1)
	queryArguments = append(queryArguments, pgx.QueryExecModeSimpleProtocol)
	queryArguments = append(queryArguments, arguments...)
	rows, err := tx.Query(ctx, query, queryArguments...)
	if err != nil {
		return finalv5oracle.ResultSummary{}, errors.New("execute fixed query")
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	if len(fields) != len(columns) {
		return finalv5oracle.ResultSummary{}, errors.New("fixed query returned an unexpected column count")
	}
	for index, field := range fields {
		resolved, typeErr := finalv5oracle.SQLTypeFromPostgresOID(field.DataTypeOID)
		if typeErr != nil || field.Name != columns[index].Name || resolved != columns[index].Type {
			return finalv5oracle.ResultSummary{}, errors.New("fixed query returned an unexpected typed schema")
		}
	}
	hasher, err := finalv5oracle.NewResultHasher(columns)
	if err != nil {
		return finalv5oracle.ResultSummary{}, err
	}
	for rows.Next() {
		values, valuesErr := rows.Values()
		if valuesErr != nil || hasher.WriteRow(values) != nil {
			return finalv5oracle.ResultSummary{}, errors.New("normalize fixed query row")
		}
	}
	if rows.Err() != nil {
		return finalv5oracle.ResultSummary{}, errors.New("drain fixed query")
	}
	return hasher.Finalize()
}

func matchedObservation(workload, cell, role, querySHA string, inputs []DigestReference,
	expected, actual finalv5oracle.ResultSummary) (QueryObservation, error) {
	if !validSHA256(querySHA) || expected != actual {
		return QueryObservation{}, fmt.Errorf("live %s %s/%s result differs from its independent pre-run oracle",
			workload, cell, role)
	}
	return QueryObservation{Workload: workload, Cell: cell, Role: role, QuerySHA256: querySHA,
		OracleInputs: append([]DigestReference(nil), inputs...), Expected: expected, Actual: actual, Matched: true}, nil
}

func manifestResultSummary(manifest finalv5oracle.OracleManifest) (finalv5oracle.ResultSummary, error) {
	expected := manifest.Expected
	if expected.RowCount == nil || expected.ColumnCount == nil ||
		!validSHA256(expected.NormalizedSchemaSHA256) || !validSHA256(expected.CanonicalResultSHA256) {
		return finalv5oracle.ResultSummary{}, errors.New("oracle manifest omits its complete result identity")
	}
	return finalv5oracle.ResultSummary{RowCount: *expected.RowCount, ColumnCount: *expected.ColumnCount,
		NormalizedSchemaSHA256: expected.NormalizedSchemaSHA256,
		CanonicalResultSHA256:  expected.CanonicalResultSHA256}, nil
}

func observeDatasetProbe(ctx context.Context, tx pgx.Tx,
	runtime *finalv5contracts.Runtime) (DatasetProbeObservation, error) {
	probe, err := runtime.DatasetProbeSQL()
	if err != nil {
		return DatasetProbeObservation{}, err
	}
	sourceSHA, err := runtime.DatasetProbeSourceSHA256()
	if err != nil {
		return DatasetProbeObservation{}, err
	}
	rows, err := tx.Query(ctx, probe, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return DatasetProbeObservation{}, errors.New("execute fixed Dataset scalar probe")
	}
	defer rows.Close()
	if len(rows.FieldDescriptions()) != 1 || !rows.Next() {
		return DatasetProbeObservation{}, errors.New("fixed Dataset probe did not return exactly one scalar row")
	}
	var scalar string
	if err := rows.Scan(&scalar); err != nil || strings.TrimSpace(scalar) == "" || rows.Next() {
		return DatasetProbeObservation{}, errors.New("fixed Dataset probe did not return exactly one non-empty scalar row")
	}
	if rows.Err() != nil {
		return DatasetProbeObservation{}, errors.New("drain fixed Dataset scalar probe")
	}
	return DatasetProbeObservation{SourcePath: datasetProbeContractPath, SourceSHA256: sourceSHA,
		ResultSHA256: sha256Hex([]byte(scalar))}, nil
}

func readLiveSessionIdentity(ctx context.Context, tx pgx.Tx) (liveSessionIdentity, string, error) {
	var result liveSessionIdentity
	err := tx.QueryRow(ctx, liveSessionProbeSQL, pgx.QueryExecModeSimpleProtocol).Scan(
		&result.Database, &result.User, &result.ServerVersionNum, &result.BackendPID, &result.Snapshot)
	if err != nil || result.Database == "" || result.User == "" || result.ServerVersionNum == "" || result.Snapshot == "" {
		return liveSessionIdentity{}, "", errors.New("read live publication session identity")
	}
	value, err := canonicalJSON(result)
	if err != nil {
		return liveSessionIdentity{}, "", err
	}
	return result, domainSHA256("TASKGATE-FINAL-V5-PUBLICATION-LIVE-SESSION-V1\x00", value), nil
}

func liveObservationDigest(observation LiveObservation) (string, error) {
	copy := observation
	copy.ObservationSHA256 = ""
	value, err := canonicalJSON(copy)
	if err != nil {
		return "", err
	}
	return domainSHA256("TASKGATE-FINAL-V5-PUBLICATION-LIVE-OBSERVATION-V1\x00", value), nil
}

func canonicalJSON(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func domainSHA256(domain string, value []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write(value)
	return hex.EncodeToString(hash.Sum(nil))
}
