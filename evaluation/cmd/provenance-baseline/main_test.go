package main

import (
	"encoding/json"
	"math"
	"testing"
)

func validTestConfig() config {
	return config{
		SchemaVersion: 1, CampaignID: "test", DataCacheStrategy: "warm", CircuitStrategy: "novel_nonce", Warmups: 1, Runs: 3,
		OrderSeed: 7, StatementTimeoutMS: 1000, DirectDSNEnv: "DIRECT_DSN",
		ProvSQLDSNEnv: "PROVSQL_DSN", ExpectedProvSQLVersion: "1.11.0",
		ExpectedProvSQLCommit: "6388fd06b79b7d247b4ff4dad4959374d0e92358",
		DatasetFingerprintSQL: "SELECT 'x'", Workloads: []workload{{
			ID: "join_group", Scale: 10, ExpectedRows: 3,
			ProvenanceCarriers: 1, NonceStart: 1, CarrierGateType: "agg", RowGateType: "delta",
			SQL: "SELECT 'x', 1 WHERE $1 > 0 AND $2 > 0",
		}},
	}
}

func TestValidateConfig(t *testing.T) {
	cfg := validTestConfig()
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*config)
	}{
		{"cache", func(c *config) { c.DataCacheStrategy = "cold" }},
		{"circuit", func(c *config) { c.CircuitStrategy = "reuse" }},
		{"commit", func(c *config) { c.ExpectedProvSQLCommit = "master" }},
		{"same env", func(c *config) { c.ProvSQLDSNEnv = c.DirectDSNEnv }},
		{"warmups", func(c *config) { c.Warmups = 0 }},
		{"runs", func(c *config) { c.Runs = 0 }},
		{"workload", func(c *config) { c.Workloads[0].ExpectedRows = 0 }},
		{"carrier columns", func(c *config) { c.Workloads[0].ProvenanceCarriers = 101 }},
		{"nonce", func(c *config) { c.Workloads[0].NonceStart = 0 }},
		{"root type", func(c *config) { c.Workloads[0].CarrierGateType = "" }},
		{"overlapping nonces", func(c *config) {
			other := c.Workloads[0]
			other.ID = "other"
			other.NonceStart = 4
			c.Workloads = append(c.Workloads, other)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := validTestConfig()
			test.mutate(&copy)
			if err := validateConfig(copy); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestCanonicalHashIsOrderIndependentAndMultiplicitySensitive(t *testing.T) {
	left := canonicalHash([]string{"b", "a", "a"})
	right := canonicalHash([]string{"a", "b", "a"})
	if left != right {
		t.Fatal("hash depends on row order")
	}
	if left == canonicalHash([]string{"a", "b"}) {
		t.Fatal("hash ignores multiplicity")
	}
	if left == canonicalHash([]string{"ab", "a"}) {
		t.Fatal("hash lacks an unambiguous length framing")
	}
}

func TestOrderedHashIsOrderAndMultiplicitySensitive(t *testing.T) {
	if orderedHash([]string{"a", "b"}) == orderedHash([]string{"b", "a"}) {
		t.Fatal("ordered result hash ignores row order")
	}
	if orderedHash([]string{"a", "a"}) == orderedHash([]string{"a"}) {
		t.Fatal("ordered result hash ignores multiplicity")
	}
}

func TestDescribeUsesTypeSevenQuantiles(t *testing.T) {
	described := describe([]float64{4, 1, 3, 2})
	if described.Count != 4 || described.Min != 1 || described.P50 != 2.5 || math.Abs(described.P95-3.85) > 1e-12 || described.Max != 4 || described.Mean != 2.5 {
		t.Fatalf("unexpected distribution: %#v", described)
	}
}

func TestSummarizeSeparatesSystems(t *testing.T) {
	gates, bytes := int64(2), int64(4096)
	samples := []sample{
		{WorkloadID: "q", Scale: 1, System: "direct_postgresql", DurationMS: 2},
		{WorkloadID: "q", Scale: 1, System: "provsql", DurationMS: 5, GateDelta: &gates, ArtifactByteDelta: &bytes},
	}
	got := summarize(samples)
	if len(got) != 2 || got[0].System != "direct_postgresql" || got[1].System != "provsql" || got[1].GateDelta.P50 != 2 {
		t.Fatalf("unexpected summaries: %#v", got)
	}
	if got[0].GateDelta != nil {
		t.Fatalf("direct PostgreSQL has a fabricated gate distribution: %#v", got[0].GateDelta)
	}
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if _, exists := object["gate_delta"]; exists {
		t.Fatalf("direct PostgreSQL serializes an inapplicable gate distribution: %s", encoded)
	}
}

func TestReportBoundaryKeepsNonComparableFunctionsExplicit(t *testing.T) {
	cfg := validTestConfig()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report := newReport(cfg, raw)
	if report.Boundary.ID != boundaryID || len(report.Boundary.Excluded) < 2 || report.Boundary.MissingSemantics == "" {
		t.Fatalf("comparison boundary is incomplete: %#v", report.Boundary)
	}
}
