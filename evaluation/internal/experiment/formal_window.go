package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// FormalHealthcheckDomain domain-separates the healthcheck identity.
const FormalHealthcheckDomain = "TASKGATE-FINAL-V5-FORMAL-GATEWAY-HEALTHCHECK-V1"

// The approved periodic Gateway healthcheck for a formal v1.5 measurement.
//
// Ordinary production compose.yaml probes /health/ready, which calls
// connector.Ping and therefore performs a full Business PostgreSQL Attestation:
// one datasource identity read plus, per ExpectedSchema entry, a column
// attestation, a view-definition attestation and the nested pg_rewrite lookup
// inside pg_get_viewdef. Under pg_stat_statements.track=all those are counted as
// gateway_reader statements, so at a three-second interval any sample
// outstanding longer than one interval absorbs a wall-clock-dependent number of
// them.
//
// evaluation/final-v5-wsl2/compose.observer-v3.yaml overrides the periodic probe
// to /health/live, which returns 204 without touching the control store or the
// data source. Readiness is still proven -- explicitly, by the harness, outside
// the observer interval.
//
// These values are the exact resolved definition a formal deployment must carry.
var formalGatewayHealthcheckTest = []string{
	"CMD", "curl", "--fail", "--silent", "http://127.0.0.1:8082/health/live",
}

const (
	formalGatewayHealthcheckInterval = "3s"
	formalGatewayHealthcheckTimeout  = "3s"
	formalGatewayHealthcheckRetries  = 30
)

// GatewayHealthcheck is the resolved periodic healthcheck of the running
// Gateway container. It is bound into the observer runtime identity so that a
// deployment which silently dropped the override cannot produce an accepted
// formal snapshot.
type GatewayHealthcheck struct {
	Test     []string `json:"test"`
	Interval string   `json:"interval"`
	Timeout  string   `json:"timeout"`
	Retries  int64    `json:"retries"`
}

// FormalGatewayHealthcheck is the approved definition.
func FormalGatewayHealthcheck() GatewayHealthcheck {
	return GatewayHealthcheck{
		Test:     append([]string(nil), formalGatewayHealthcheckTest...),
		Interval: formalGatewayHealthcheckInterval,
		Timeout:  formalGatewayHealthcheckTimeout,
		Retries:  formalGatewayHealthcheckRetries,
	}
}

// Validate rejects any periodic healthcheck other than the approved one.
//
// The comparison is exact and covers the interval, timeout and retry count as
// well as the command: a probe that still reaches /health/live but on a
// different schedule is a different deployment, and binding only the command
// would let the schedule drift without invalidating evidence.
func (healthcheck GatewayHealthcheck) Validate() error {
	approved := FormalGatewayHealthcheck()
	// Tested first so the specific failure this gate exists to catch is named
	// rather than reported as a generic command mismatch.
	for _, argument := range healthcheck.Test {
		if strings.Contains(argument, "/health/ready") {
			return errors.New("the running Gateway still probes /health/ready periodically; " +
				"every probe performs a full Business PostgreSQL Attestation and would " +
				"contaminate the observer window")
		}
	}
	if len(healthcheck.Test) != len(approved.Test) {
		return fmt.Errorf("formal Gateway healthcheck must be %v, this deployment runs %v",
			approved.Test, healthcheck.Test)
	}
	for index, argument := range approved.Test {
		if healthcheck.Test[index] != argument {
			return fmt.Errorf("formal Gateway healthcheck must be %v, this deployment runs %v",
				approved.Test, healthcheck.Test)
		}
	}
	if healthcheck.Interval != approved.Interval || healthcheck.Timeout != approved.Timeout ||
		healthcheck.Retries != approved.Retries {
		return fmt.Errorf("formal Gateway healthcheck schedule must be interval=%s timeout=%s retries=%d, "+
			"this deployment runs interval=%s timeout=%s retries=%d",
			approved.Interval, approved.Timeout, approved.Retries,
			healthcheck.Interval, healthcheck.Timeout, healthcheck.Retries)
	}
	return nil
}

// SHA256 is the healthcheck identity bound into the observer runtime identity.
func (healthcheck GatewayHealthcheck) SHA256() string {
	hash := sha256.New()
	hash.Write([]byte(FormalHealthcheckDomain + "\x00"))
	fmt.Fprintf(hash, "%d\x00", len(healthcheck.Test))
	for _, argument := range healthcheck.Test {
		fmt.Fprintf(hash, "%d\x00%s\x00", len(argument), argument)
	}
	fmt.Fprintf(hash, "%d\x00%s\x00%d\x00%s\x00%d\x00",
		len(healthcheck.Interval), healthcheck.Interval,
		len(healthcheck.Timeout), healthcheck.Timeout, healthcheck.Retries)
	return hex.EncodeToString(hash.Sum(nil))
}

// FormalObserverOverridePath is the Compose override that installs the approved
// periodic healthcheck.
const FormalObserverOverridePath = "evaluation/final-v5-wsl2/compose.observer-v3.yaml"

// ValidateFormalComposeFiles requires the measurement override to be present and
// last.
//
// Last matters: Compose merges overrides in order, so a file after this one
// could restore the readiness probe. Requiring the position rather than mere
// presence is what makes the check a gate instead of a hint.
func ValidateFormalComposeFiles(files []string) error {
	if len(files) == 0 {
		return errors.New("a formal v1.5 deployment declares no Compose files")
	}
	position := -1
	for index, file := range files {
		if strings.TrimSpace(file) == FormalObserverOverridePath {
			position = index
		}
	}
	if position < 0 {
		return fmt.Errorf("a formal v1.5 deployment must include %s; without it the Gateway probes "+
			"/health/ready every interval and contaminates every observer window", FormalObserverOverridePath)
	}
	if position != len(files)-1 {
		return fmt.Errorf("%s must be the last Compose override so nothing can restore the readiness probe; "+
			"it is at position %d of %d", FormalObserverOverridePath, position+1, len(files))
	}
	return nil
}

// ReadinessQualificationV1 records one explicit readiness proof taken outside
// the observer interval.
//
// It deliberately carries no credential, no SQL and no response body: readiness
// is proven by the status code plus the deployment identity it was taken
// against, and anything more would put deployment or task state into evidence.
type ReadinessQualificationV1 struct {
	Version               string `json:"version"`
	Phase                 string `json:"phase"`
	ObservedAtUnixMicro   int64  `json:"observed_at_unix_micro"`
	HTTPStatus            int    `json:"http_status"`
	RuntimeIdentitySHA256 string `json:"runtime_identity_sha256"`
	HealthcheckSHA256     string `json:"healthcheck_sha256"`
	ExpectedSchemaDigest  string `json:"expected_schema_digest"`
}

// ReadinessQualificationVersion identifies the record.
const ReadinessQualificationVersion = "taskgate-final-v5-readiness-qualification-v1"

// Validate rejects an unusable or non-passing readiness record.
func (record ReadinessQualificationV1) Validate() error {
	if record.Version != ReadinessQualificationVersion {
		return fmt.Errorf("readiness qualification version %q is unsupported", record.Version)
	}
	switch record.Phase {
	case "before", "after", "startup":
	default:
		return fmt.Errorf("readiness qualification phase %q is not one of startup, before, after", record.Phase)
	}
	// readiness() returns 204 on success; any other status means the Gateway
	// was not ready and no sample taken around it is admissible.
	if record.HTTPStatus != 204 {
		return fmt.Errorf("readiness qualification records HTTP %d, want 204", record.HTTPStatus)
	}
	if record.ObservedAtUnixMicro <= 0 {
		return errors.New("readiness qualification carries no observation time")
	}
	if !validSHA256(record.RuntimeIdentitySHA256) || !validSHA256(record.HealthcheckSHA256) ||
		!validSHA256(record.ExpectedSchemaDigest) {
		return errors.New("readiness qualification is not bound to a complete deployment identity")
	}
	return nil
}
