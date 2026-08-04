package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
)

// businessCensusShell collects the whole authoritative Business reading in ONE
// statement.
//
// The census CTE is MATERIALIZED on purpose. The role-wide total and the
// individual statement rows are both derived from it, so they cannot describe
// two different instants -- which is the property ObserverSnapshotV2.Validate
// checks by requiring the total to equal the sum of the rows. Two separate
// queries would satisfy the arithmetic only by luck.
//
// Query text is base64-encoded so that newlines and the field separator cannot
// corrupt the row framing. It is decoded, digested and discarded in process; no
// caller ever receives it.
const businessCensusShell = `set -eu
test -n "${POSTGRES_DB:-}"
psql -X --no-psqlrc --set ON_ERROR_STOP=1 --username postgres --dbname "$POSTGRES_DB" \
  --tuples-only --no-align --field-separator '|' <<'TASKGATE_SQL'
WITH census AS MATERIALIZED (
  SELECT s.query AS query, s.toplevel AS toplevel, s.calls AS calls, s.queryid AS queryid
  FROM pg_stat_statements AS s
  WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
    AND s.userid = (SELECT oid FROM pg_roles WHERE rolname = 'gateway_reader')
)
SELECT 'E'
     , current_setting('server_version_num')
     , current_setting('pg_stat_statements.track')
     , current_setting('pg_stat_statements.track_utility')
     , current_setting('pg_stat_statements.track_planning')
UNION ALL
SELECT 'S'
     , (SELECT stats_reset::text FROM pg_stat_statements_info)
     , (SELECT dealloc::text FROM pg_stat_statements_info)
     , pg_postmaster_start_time()::text
     , pg_current_wal_lsn() - '0/0'::pg_lsn
UNION ALL
SELECT 'T', (SELECT COALESCE(sum(c.calls), 0)::text FROM census c), '', '', ''
UNION ALL
SELECT 'R'
     , replace(encode(convert_to(c.query, 'UTF8'), 'base64'), E'\n', '')
     , c.toplevel::text
     , c.calls::text
     , c.queryid::text
FROM census c
ORDER BY 1, 2;
TASKGATE_SQL`

// statementExecutor is the only capability the census needs. Narrowing it from
// the full engine interface keeps the census independent of Docker inspection
// and lets it be tested against a recorded response.
type statementExecutor interface {
	exec(ctx context.Context, containerID string, command []string) ([]byte, error)
}

// businessCensus is the parsed authoritative Business reading.
type businessCensus struct {
	environment         experiment.MeasurementEnvironment
	statsReset          string
	dealloc             int64
	postmasterStartTime string
	walBytes            int64
	total               int64
	rows                []experiment.ObserverStructuralRow
	// queryIDs are deployment-local decimal diagnostics, never identity.
	queryIDs map[string]string
}

// readBusinessCensus executes the single census statement and classifies it.
//
// Every parse failure is reported without the offending text. A statement the
// strict AST digester cannot parse is named by its deployment-local queryid
// only: echoing the fragment would put SQL into stderr, and from there into any
// log the harness retains.
func readBusinessCensus(ctx context.Context, docker statementExecutor, containerID string) (businessCensus, error) {
	var census businessCensus
	census.queryIDs = map[string]string{}
	raw, err := docker.exec(ctx, containerID, []string{"sh", "-c", businessCensusShell})
	if err != nil {
		return census, fmt.Errorf("read Business PostgreSQL census: %w", err)
	}
	var sawEnvironment, sawState, sawTotal bool
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 5 {
			return census, fmt.Errorf("Business census returned a malformed row with %d fields", len(fields))
		}
		switch fields[0] {
		case "E":
			versionNum, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return census, errors.New("Business census returned a malformed server_version_num")
			}
			census.environment = experiment.MeasurementEnvironment{
				PostgreSQLVersionNum: versionNum, Track: fields[2],
				TrackUtility: fields[3], TrackPlanning: fields[4],
			}
			sawEnvironment = true
		case "S":
			census.statsReset = fields[1]
			if census.dealloc, err = strconv.ParseInt(fields[2], 10, 64); err != nil {
				return census, errors.New("Business census returned a malformed dealloc counter")
			}
			census.postmasterStartTime = fields[3]
			if census.walBytes, err = strconv.ParseInt(fields[4], 10, 64); err != nil {
				return census, errors.New("Business census returned a malformed WAL position")
			}
			sawState = true
		case "T":
			if census.total, err = strconv.ParseInt(fields[1], 10, 64); err != nil {
				return census, errors.New("Business census returned a malformed role total")
			}
			sawTotal = true
		case "R":
			row, queryID, err := parseCensusRow(fields)
			if err != nil {
				return census, err
			}
			key := row.StrictASTSHA256 + "|" + strconv.FormatBool(row.TopLevel)
			// Two pg_stat_statements rows can normalize to one structural key.
			// Their calls are summed rather than one silently replacing the
			// other, which would make the total disagree with the census.
			if existing, found := census.queryIDs[key]; found {
				_ = existing
				for index := range census.rows {
					if census.rows[index].StrictASTSHA256 == row.StrictASTSHA256 &&
						census.rows[index].TopLevel == row.TopLevel {
						census.rows[index].Calls += row.Calls
						break
					}
				}
				continue
			}
			census.queryIDs[key] = queryID
			census.rows = append(census.rows, row)
		default:
			return census, fmt.Errorf("Business census returned an unknown row marker %q", fields[0])
		}
	}
	if !sawEnvironment || !sawState || !sawTotal {
		return census, errors.New("Business census is missing its environment, state or total row")
	}
	sort.Slice(census.rows, func(left, right int) bool {
		if census.rows[left].StrictASTSHA256 != census.rows[right].StrictASTSHA256 {
			return census.rows[left].StrictASTSHA256 < census.rows[right].StrictASTSHA256
		}
		return !census.rows[left].TopLevel && census.rows[right].TopLevel
	})
	var sum int64
	for _, row := range census.rows {
		sum += row.Calls
	}
	if sum != census.total {
		return census, fmt.Errorf("Business census total is %d but its rows sum to %d; "+
			"the reading was not atomic", census.total, sum)
	}
	return census, nil
}

// parseCensusRow decodes one statement row, digests it structurally, and
// discards the text.
func parseCensusRow(fields []string) (experiment.ObserverStructuralRow, string, error) {
	queryID := strings.TrimSpace(fields[4])
	if queryID == "" {
		return experiment.ObserverStructuralRow{}, "", errors.New("Business census returned a statement row with no queryid")
	}
	// The queryid is recorded as a decimal deployment-local diagnostic. It is
	// never an identity: it is PostgreSQL-version and installation specific.
	if _, err := strconv.ParseInt(queryID, 10, 64); err != nil {
		return experiment.ObserverStructuralRow{}, "", errors.New("Business census returned a non-decimal queryid")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		// Deliberately no fragment: the payload is SQL.
		return experiment.ObserverStructuralRow{}, "",
			fmt.Errorf("Business census statement for queryid %s is not valid base64 (statement withheld)", queryID)
	}
	topLevel, err := strconv.ParseBool(fields[2])
	if err != nil {
		return experiment.ObserverStructuralRow{}, "",
			fmt.Errorf("Business census statement for queryid %s has a malformed toplevel flag", queryID)
	}
	calls, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || calls < 0 {
		return experiment.ObserverStructuralRow{}, "",
			fmt.Errorf("Business census statement for queryid %s has a malformed call count", queryID)
	}
	digest, err := experiment.StrictASTDigest(string(decoded))
	if err != nil {
		// Sanitized: the operator can look the statement up locally by queryid.
		return experiment.ObserverStructuralRow{}, "",
			fmt.Errorf("strict AST digest failed for queryid %s (statement withheld)", queryID)
	}
	// decoded goes out of scope here; nothing textual leaves this function.
	return experiment.ObserverStructuralRow{
		StrictASTSHA256: digest, TopLevel: topLevel, Calls: calls,
	}, queryID, nil
}

// observerInvocation is the exact argv identity a v1.5 observer runs under.
//
// These are flags, never inherited environment variables. An identity taken from
// the environment can be set by anything in the process tree, so a snapshot
// could silently be attributed to a different window or classifier.
type observerInvocation struct {
	phase                    string
	observerWindowID         string
	classifierManifestSHA256 string
	operationBindingSHA256   string
}

func parseObserverInvocation(args []string) (observerInvocation, error) {
	var invocation observerInvocation
	rest := args[1:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return invocation, fmt.Errorf("observer flag %q has no value", rest[0])
		}
		name, value := rest[0], rest[1]
		rest = rest[2:]
		switch name {
		case "--phase":
			invocation.phase = value
		case "--observer-window-id":
			invocation.observerWindowID = value
		case "--classifier-manifest-sha256":
			invocation.classifierManifestSHA256 = value
		case "--operation-binding-sha256":
			invocation.operationBindingSHA256 = value
		default:
			return invocation, fmt.Errorf("observer does not accept %q", name)
		}
	}
	if invocation.phase != "before" && invocation.phase != "after" {
		return invocation, errors.New("observer requires exact --phase before|after")
	}
	if !isSHA256(invocation.observerWindowID) {
		return invocation, errors.New("observer requires --observer-window-id as a lowercase SHA-256")
	}
	if !isSHA256(invocation.classifierManifestSHA256) {
		return invocation, errors.New("observer requires --classifier-manifest-sha256 as a lowercase SHA-256")
	}
	if invocation.operationBindingSHA256 != "" && !isSHA256(invocation.operationBindingSHA256) {
		return invocation, errors.New("--operation-binding-sha256 must be a lowercase SHA-256 when supplied")
	}
	return invocation, nil
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= '0' && character <= '9', character >= 'a' && character <= 'f':
		default:
			return false
		}
	}
	return true
}
