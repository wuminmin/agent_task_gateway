package experiment

import (
	"strings"
	"testing"
)

func formalComposeFiles() []string {
	return []string{
		"compose.yaml",
		"compose.debug.yaml",
		"evaluation/final-v5-wsl2/compose.real-pilot.yaml",
		"evaluation/final-v5-wsl2/compose.provsql.yaml",
		FormalObserverOverridePath,
	}
}

func TestFormalComposeFilesRequireTheOverrideLast(t *testing.T) {
	if err := ValidateFormalComposeFiles(formalComposeFiles()); err != nil {
		t.Fatalf("the formal Compose file set was rejected: %v", err)
	}
}

// Presence is not enough. Compose merges overrides in order, so a file after the
// measurement override could restore the readiness probe; requiring the position
// is what makes this a gate.
func TestFormalComposeFilesRejectAnOverrideThatIsNotLast(t *testing.T) {
	files := formalComposeFiles()
	shuffled := []string{files[0], files[1], FormalObserverOverridePath, files[2], files[3]}
	err := ValidateFormalComposeFiles(shuffled)
	if err == nil {
		t.Fatal("an override that was not last was accepted")
	}
	if !strings.Contains(err.Error(), "last") {
		t.Fatalf("the rejection does not explain the ordering requirement: %v", err)
	}
}

func TestFormalComposeFilesRejectAMissingOverride(t *testing.T) {
	files := formalComposeFiles()
	if err := ValidateFormalComposeFiles(files[:len(files)-1]); err == nil {
		t.Fatal("a formal deployment without the measurement override was accepted")
	}
	if err := ValidateFormalComposeFiles(nil); err == nil {
		t.Fatal("a deployment declaring no Compose files was accepted")
	}
}

func TestFormalHealthcheckAcceptsOnlyTheApprovedDefinition(t *testing.T) {
	if err := FormalGatewayHealthcheck().Validate(); err != nil {
		t.Fatalf("the approved healthcheck was rejected: %v", err)
	}
	approved := FormalGatewayHealthcheck()
	for name, probe := range map[string]GatewayHealthcheck{
		"the readiness probe": {
			Test:     []string{"CMD", "curl", "--fail", "--silent", "http://127.0.0.1:8082/health/ready"},
			Interval: "3s", Timeout: "3s", Retries: 30,
		},
		"a shorter command": {
			Test: approved.Test[:len(approved.Test)-1], Interval: "3s", Timeout: "3s", Retries: 30,
		},
		"a different port": {
			Test:     []string{"CMD", "curl", "--fail", "--silent", "http://127.0.0.1:9999/health/live"},
			Interval: "3s", Timeout: "3s", Retries: 30,
		},
		"a drifted interval": {Test: approved.Test, Interval: "30s", Timeout: "3s", Retries: 30},
		"a drifted timeout":  {Test: approved.Test, Interval: "3s", Timeout: "9s", Retries: 30},
		"a drifted retries":  {Test: approved.Test, Interval: "3s", Timeout: "3s", Retries: 1},
		"no probe":           {},
	} {
		t.Run(name, func(t *testing.T) {
			if err := probe.Validate(); err == nil {
				t.Fatal("an unapproved periodic healthcheck was accepted")
			}
		})
	}
}

// The readiness probe is the specific failure this gate exists for, so the
// rejection must name it rather than report a generic mismatch.
func TestReadinessProbeRejectionIsDiagnostic(t *testing.T) {
	probe := GatewayHealthcheck{
		Test:     []string{"CMD", "curl", "--fail", "--silent", "http://127.0.0.1:8082/health/ready"},
		Interval: "3s", Timeout: "3s", Retries: 30,
	}
	err := probe.Validate()
	if err == nil {
		t.Fatal("the readiness probe was accepted")
	}
	if !strings.Contains(err.Error(), "Attestation") {
		t.Fatalf("the rejection does not explain why a readiness probe contaminates the window: %v", err)
	}
}

func TestHealthcheckIdentitySeparatesEveryDimension(t *testing.T) {
	approved := FormalGatewayHealthcheck()
	baseline := approved.SHA256()
	for name, mutate := range map[string]func(*GatewayHealthcheck){
		"command":  func(h *GatewayHealthcheck) { h.Test[len(h.Test)-1] = "http://127.0.0.1:8082/health/ready" },
		"interval": func(h *GatewayHealthcheck) { h.Interval = "5s" },
		"timeout":  func(h *GatewayHealthcheck) { h.Timeout = "5s" },
		"retries":  func(h *GatewayHealthcheck) { h.Retries = 29 },
	} {
		t.Run(name, func(t *testing.T) {
			probe := FormalGatewayHealthcheck()
			mutate(&probe)
			if probe.SHA256() == baseline {
				t.Fatalf("the healthcheck identity ignores %s", name)
			}
		})
	}
}

func validReadiness() ReadinessQualificationV1 {
	return ReadinessQualificationV1{
		Version: ReadinessQualificationVersion, Phase: "before",
		ObservedAtUnixMicro: 1, HTTPStatus: 204,
		RuntimeIdentitySHA256: testSchemaDigest, HealthcheckSHA256: testSchemaDigest,
		ExpectedSchemaDigest: testSchemaDigest,
	}
}

func TestReadinessQualificationRequiresAPassingProof(t *testing.T) {
	if err := validReadiness().Validate(); err != nil {
		t.Fatalf("a valid readiness record was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ReadinessQualificationV1){
		"version cleared":    func(r *ReadinessQualificationV1) { r.Version = "" },
		"an unknown phase":   func(r *ReadinessQualificationV1) { r.Phase = "during" },
		"a 503 status":       func(r *ReadinessQualificationV1) { r.HTTPStatus = 503 },
		"a 200 status":       func(r *ReadinessQualificationV1) { r.HTTPStatus = 200 },
		"no observation":     func(r *ReadinessQualificationV1) { r.ObservedAtUnixMicro = 0 },
		"no runtime bind":    func(r *ReadinessQualificationV1) { r.RuntimeIdentitySHA256 = "" },
		"no healthcheck":     func(r *ReadinessQualificationV1) { r.HealthcheckSHA256 = "" },
		"no expected schema": func(r *ReadinessQualificationV1) { r.ExpectedSchemaDigest = "" },
	} {
		t.Run(name, func(t *testing.T) {
			record := validReadiness()
			mutate(&record)
			if err := record.Validate(); err == nil {
				t.Fatal("an unusable readiness qualification was accepted")
			}
		})
	}
}

// The record proves readiness without carrying deployment or task state.
func TestReadinessQualificationCarriesNoSecretOrPayload(t *testing.T) {
	record := validReadiness()
	for _, field := range []string{
		record.Version, record.Phase, record.RuntimeIdentitySHA256,
		record.HealthcheckSHA256, record.ExpectedSchemaDigest,
	} {
		for _, forbidden := range []string{"password", "token", "SELECT", "dsn", "postgres://"} {
			if strings.Contains(strings.ToLower(field), strings.ToLower(forbidden)) {
				t.Fatalf("the readiness record carries %q", forbidden)
			}
		}
	}
}
