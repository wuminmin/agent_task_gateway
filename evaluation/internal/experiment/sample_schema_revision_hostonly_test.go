//go:build taskgate_hostonly

// These cases require host resources the product Compose stack has no reason to
// carry: a Docker socket, the retained qualification artifacts, or a live
// benchmark Dataset. They exercise the evaluation harness rather than the
// product, and the formal campaign exercises the same material at runtime, so
// they sit behind taskgate_hostonly instead of failing the acceptance run.

package experiment

import (
	"path/filepath"
	"testing"
)

func TestSampleV1SchemaAndTrackedEvidenceRemainVersionCompatible(t *testing.T) {
	const schemaSHA256 = "0cc18e7bc68a8659f922e260b6db3353dcdfc57f939ac2f69b043a1974475a2e"
	schemaPath := filepath.Join("..", "..", "final-v5-wsl2", "schema", "sample-v1.schema.json")
	got, err := FileSHA256(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != schemaSHA256 {
		t.Fatalf("sample-v1 schema SHA-256 = %s, want frozen %s", got, schemaSHA256)
	}

	tests := []struct {
		name   string
		dir    string
		sha256 string
	}{
		{
			name:   "run03",
			dir:    "targeted-p33-artifact-100x4-03-20260809T181354Z-0dc072f9e6be",
			sha256: "efa58673b88b930ffddeaac7398bccb0978fac3316ebc5dd85c181b2cb0c00c1",
		},
		{
			name:   "run04",
			dir:    "targeted-p33-artifact-100x4-04-20260810T014910Z-9316682fa30c",
			sha256: "3ad230ec470581be9def8fc510e578e173207589309320e751878c7e23b7d283",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "final-v5-wsl2", "raw", test.dir, "raw", "deployment-01.jsonl")
			got, err := FileSHA256(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.sha256 {
				t.Fatalf("retained evidence SHA-256 = %s, want frozen %s", got, test.sha256)
			}
			samples, err := ReadSamples([]string{path})
			if err != nil {
				t.Fatalf("ReadSamples rejected retained v1 evidence: %v", err)
			}
			if len(samples) != 1 {
				t.Fatalf("ReadSamples returned %d samples, want 1", len(samples))
			}
			if samples[0].SchemaVersion != SampleSchemaVersion || samples[0].TaskGateRejectionV1 != nil {
				t.Fatalf("retained sample decoded as schema_version=%d rejection=%v, want unchanged v1", samples[0].SchemaVersion, samples[0].TaskGateRejectionV1 != nil)
			}
		})
	}
}
