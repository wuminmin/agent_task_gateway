//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. They exercise the evaluation harness rather than the
// product, and the formal campaign exercises the same material at runtime, so
// they sit behind taskgate_hostonly instead of failing the acceptance run.

package main

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/evaluation/internal/formalbuild"
)

const (
	dbTestComposeProject      = "taskgate-dbtest"
	certifiedPostgreSQLImage  = "postgres@sha256:92620daddcd947f8d5ab5ba66e848702fe443d87fed30c4cea8e389fd78dfc55"
	liveCensusObserverTimeout = 30 * time.Second
	liveSystemIdentifierShell = `set -eu
test -n "${POSTGRES_DB:-}"
psql -X --no-psqlrc --set ON_ERROR_STOP=1 --username postgres --dbname "$POSTGRES_DB" \
  --tuples-only --no-align <<'TASKGATE_SQL'
SELECT system_identifier::text FROM pg_control_system();
TASKGATE_SQL`
)

// liveBusinessObserverEngine keeps the unrelated Gateway, Control and resource
// surfaces on the existing fixture while routing the Business census and its
// PostgreSQL image inspection to the real db-test container. That isolates this
// regression to the exact observer-before path that failed without replacing
// the production businessCensusShell with a test query.
type liveBusinessObserverEngine struct {
	*fakeEngine
	live                 *dockerEngine
	liveBusinessID       string
	businessExecCommands [][]string
}

func (engine *liveBusinessObserverEngine) ImageInspect(
	ctx context.Context, reference string,
) (formalbuild.ImageInspect, error) {
	if _, canned := engine.fakeEngine.images[reference]; canned {
		return engine.fakeEngine.ImageInspect(ctx, reference)
	}
	return engine.live.ImageInspect(ctx, reference)
}

func (engine *liveBusinessObserverEngine) ContainerInspect(
	ctx context.Context, containerID string,
) (formalbuild.ContainerInspect, error) {
	if _, canned := engine.fakeEngine.containers[containerID]; canned {
		return engine.fakeEngine.ContainerInspect(ctx, containerID)
	}
	return engine.live.ContainerInspect(ctx, containerID)
}

func (engine *liveBusinessObserverEngine) Exec(
	ctx context.Context, containerID string, command []string,
) ([]byte, error) {
	if containerID == engine.liveBusinessID {
		return engine.live.Exec(ctx, containerID, command)
	}
	return engine.fakeEngine.Exec(ctx, containerID, command)
}

func (engine *liveBusinessObserverEngine) ResolveService(
	ctx context.Context, project, service string,
) (string, error) {
	if project == testProject && service == businessService {
		return engine.liveBusinessID, nil
	}
	return engine.fakeEngine.ResolveService(ctx, project, service)
}

func (engine *liveBusinessObserverEngine) exec(
	ctx context.Context, containerID string, command []string,
) ([]byte, error) {
	if containerID != engine.liveBusinessID {
		return engine.fakeEngine.exec(ctx, containerID, command)
	}
	engine.businessExecCommands = append(engine.businessExecCommands, append([]string(nil), command...))
	return engine.live.exec(ctx, containerID, command)
}

// TestBusinessCensusShellProducesValidObserverBeforeSnapshotLive is the live
// regression for the four-arm census. Earlier tests only replayed canned rows,
// so PostgreSQL never type-checked the UNION ALL and the numeric/text mismatch
// survived until the Artifact canary's first observer-before invocation.
func TestBusinessCensusShellProducesValidObserverBeforeSnapshotLive(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("BUSINESS_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("BUSINESS_TEST_POSTGRES_DSN is required for the live observer census regression")
	}

	ctx, cancel := context.WithTimeout(context.Background(), liveCensusObserverTimeout)
	defer cancel()

	// This query both proves which server the DSN reaches and leaves at least one
	// real gateway_reader statement for the R arm to return.
	reader, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to Business PostgreSQL through the supplied DSN: %v", err)
	}
	var version int64
	var postmasterStart, database, role, systemIdentifier string
	if err := reader.QueryRow(ctx, `
SELECT current_setting('server_version_num')::bigint,
       pg_postmaster_start_time()::text,
       current_database(),
       current_user,
       (SELECT system_identifier::text FROM pg_control_system())`).Scan(
		&version, &postmasterStart, &database, &role, &systemIdentifier,
	); err != nil {
		_ = reader.Close(ctx)
		t.Fatalf("identify the Business PostgreSQL reached through the DSN: %v", err)
	}
	if err := reader.Close(ctx); err != nil {
		t.Fatalf("close the Business PostgreSQL identity connection: %v", err)
	}
	if version != experiment.RequiredMeasurementEnvironment().PostgreSQLVersionNum ||
		database != "travel_demo" || role != businessCensusRole {
		t.Fatalf("DSN reached PostgreSQL version=%d database=%q role=%q", version, database, role)
	}

	socket, err := dockerSocket(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	liveDocker, err := newDockerEngine(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := liveDocker.negotiate(ctx); err != nil {
		t.Fatalf("negotiate the Docker Engine API: %v", err)
	}
	liveBusiness, err := liveDocker.resolveService(ctx, dbTestComposeProject, businessService)
	if err != nil {
		t.Fatalf("resolve the db-test Business PostgreSQL container: %v", err)
	}
	livePostgreSQL, err := formalbuild.ResolvePostgreSQLIdentity(ctx, liveDocker, liveBusiness.id)
	if err != nil {
		t.Fatalf("resolve the live PostgreSQL identity: %v", err)
	}
	if livePostgreSQL.ImageReference != certifiedPostgreSQLImage ||
		livePostgreSQL.RepoDigest != certifiedPostgreSQLImage ||
		livePostgreSQL.Platform != "linux/amd64" {
		t.Fatalf("live regression is not using the certified PostgreSQL image: %+v", livePostgreSQL)
	}
	liveSystemIdentifier, err := liveDocker.exec(ctx, liveBusiness.id,
		[]string{"sh", "-c", liveSystemIdentifierShell})
	if err != nil {
		t.Fatalf("read the live PostgreSQL system identifier: %v", err)
	}
	if got := strings.TrimSpace(string(liveSystemIdentifier)); got == "" || got != systemIdentifier {
		t.Fatalf("DSN PostgreSQL system identifier %q differs from container identifier %q",
			systemIdentifier, got)
	}

	fixture := completeFakeEngine()
	fixture.services[testProject+"/"+businessService] = serviceIdentity{
		project: testProject, service: businessService, id: liveBusiness.id, pid: liveBusiness.pid,
	}
	for index := range fixture.restarts {
		count := fixture.restarts[index].counts["business-id"]
		delete(fixture.restarts[index].counts, "business-id")
		fixture.restarts[index].counts[liveBusiness.id] = count
		fixture.restarts[index].services[businessService] = liveBusiness.id
	}
	liveEngine := &liveBusinessObserverEngine{
		fakeEngine: fixture, live: liveDocker, liveBusinessID: liveBusiness.id,
	}
	observerSource, err := observerSourceSHA256()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := collectV2(ctx, liveEngine, testProject, fakeInvocation("before"),
		observerSource, fakeExpectedSource())
	if err != nil {
		t.Fatalf("collect live observer-before snapshot: %v", err)
	}

	if len(liveEngine.businessExecCommands) != 1 ||
		!reflect.DeepEqual(liveEngine.businessExecCommands[0], []string{"sh", "-c", businessCensusShell}) {
		t.Fatalf("Business census executions = %#v, want the complete shell exactly once",
			liveEngine.businessExecCommands)
	}
	if snapshot.Phase != "before" || snapshot.Environment != experiment.RequiredMeasurementEnvironment() ||
		snapshot.Resource.PostmasterStartTime != postmasterStart || snapshot.Runtime.PostgreSQL != livePostgreSQL {
		t.Fatalf("live observer-before snapshot is incomplete or bound to another server: %+v", snapshot)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("live observer-before snapshot is invalid: %v", err)
	}
}
