package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmallQueryGateComparesDigestBoundLatencyAndThroughput(t *testing.T) {
	baseline := smallQueryTestEvidence(t, "baseline.json", []byte(`{"kind":"baseline"}`), 10, 100)
	candidate := smallQueryTestEvidence(t, "candidate.json", []byte(`{"kind":"candidate"}`), 11, 90)

	got := smallQueryGate(&baseline, &candidate)
	if got.Status != "pass" {
		t.Fatalf("small-query gate status = %q, reason=%q, want pass", got.Status, got.Reason)
	}
	evidence, ok := got.Evidence.(map[string]any)
	if !ok {
		t.Fatalf("small-query evidence type = %T", got.Evidence)
	}
	if evidence["baseline_artifact_sha256"] != baseline.ArtifactSHA256 ||
		evidence["candidate_artifact_sha256"] != candidate.ArtifactSHA256 {
		t.Fatalf("small-query evidence omitted artifact digests: %#v", evidence)
	}
	if value, ok := evidence["latency_degradation_percent"].(float64); !ok || math.Abs(value-10) > 1e-9 {
		t.Fatalf("latency degradation = %#v, want 10", evidence["latency_degradation_percent"])
	}
	if value, ok := evidence["throughput_degradation_percent"].(float64); !ok || math.Abs(value-10) > 1e-9 {
		t.Fatalf("throughput degradation = %#v, want 10", evidence["throughput_degradation_percent"])
	}
}

func TestSmallQueryGateFailsEitherRegressionBeyondThreshold(t *testing.T) {
	baseline := smallQueryTestEvidence(t, "baseline.json", []byte("baseline"), 10, 100)
	tests := []struct {
		name       string
		p50MS      float64
		throughput float64
	}{
		{name: "latency", p50MS: 11.01, throughput: 100},
		{name: "throughput", p50MS: 10, throughput: 89.9},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := smallQueryTestEvidence(t, "candidate.json", []byte(test.name), test.p50MS, test.throughput)
			if got := smallQueryGate(&baseline, &candidate); got.Status != "fail" {
				t.Fatalf("small-query gate status = %q, want fail: %#v", got.Status, got.Evidence)
			}
		})
	}
}

func TestSmallQueryGateRejectsInvalidEvidence(t *testing.T) {
	validBaseline := smallQueryTestEvidence(t, "baseline.json", []byte("baseline"), 10, 100)
	validCandidate := smallQueryTestEvidence(t, "candidate.json", []byte("candidate"), 9, 110)

	tests := []struct {
		name      string
		baseline  smallQueryBaseline
		candidate smallQueryBaseline
		reason    string
	}{
		{
			name: "baseline digest mismatch", baseline: func() smallQueryBaseline {
				value := validBaseline
				value.ArtifactSHA256 = strings.Repeat("0", sha256.Size*2)
				return value
			}(), candidate: validCandidate, reason: "baseline artifact SHA-256 mismatch",
		},
		{
			name: "candidate digest mismatch", baseline: validBaseline, candidate: func() smallQueryBaseline {
				value := validCandidate
				value.ArtifactSHA256 = strings.Repeat("0", sha256.Size*2)
				return value
			}(), reason: "candidate artifact SHA-256 mismatch",
		},
		{
			name: "uppercase digest", baseline: func() smallQueryBaseline {
				value := validBaseline
				value.ArtifactSHA256 = strings.ToUpper(value.ArtifactSHA256)
				return value
			}(), candidate: validCandidate, reason: "baseline artifact SHA-256 is invalid",
		},
		{
			name: "nonpositive baseline throughput", baseline: func() smallQueryBaseline {
				value := validBaseline
				value.ThroughputQPS = 0
				return value
			}(), candidate: validCandidate, reason: "baseline throughput_qps must be a positive finite number",
		},
		{
			name: "nonfinite candidate latency", baseline: validBaseline, candidate: func() smallQueryBaseline {
				value := validCandidate
				value.P50MS = math.Inf(1)
				return value
			}(), reason: "candidate p50_ms must be a positive finite number",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := smallQueryGate(&test.baseline, &test.candidate)
			if got.Status != "fail" || got.Reason != test.reason {
				t.Fatalf("small-query gate = status %q reason %q, want fail/%q", got.Status, got.Reason, test.reason)
			}
		})
	}
}

func TestSmallQueryGateIsUnmeasuredUntilBothArtifactsAreConfigured(t *testing.T) {
	baseline := smallQueryTestEvidence(t, "baseline.json", []byte("baseline"), 10, 100)
	if got := smallQueryGate(&baseline, nil); got.Status != "unmeasured" || !strings.Contains(got.Reason, "candidate") {
		t.Fatalf("missing candidate gate = %#v", got)
	}
	if got := smallQueryGate(nil, nil); got.Status != "unmeasured" || !strings.Contains(got.Reason, "baseline and candidate") {
		t.Fatalf("missing evidence gate = %#v", got)
	}
}

func smallQueryTestEvidence(t *testing.T, name string, contents []byte, p50MS, throughputQPS float64) smallQueryBaseline {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return smallQueryBaseline{ArtifactPath: path, ArtifactSHA256: hex.EncodeToString(digest[:]),
		P50MS: p50MS, ThroughputQPS: throughputQPS}
}
