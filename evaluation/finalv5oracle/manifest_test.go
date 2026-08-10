package finalv5oracle

import (
	"bytes"
	"strings"
	"testing"
)

func testManifest() OracleManifest {
	digest := strings.Repeat("a", 64)
	return OracleManifest{
		SchemaVersion: ManifestSchemaVersion, OracleVersion: ManifestOracleVersion,
		ContractVersion: ManifestContractVersion, ExperimentID: "artifact", WorkloadID: "result-heavy",
		Scale: "100x4", Mode: "novel", DatasetSpecSHA256: digest, CatalogSpecSHA256: digest,
		QuerySpecSHA256: digest, NormalizationSpecSHA256: digest,
		Expected: ManifestExpected{RowCount: Int64(100), ColumnCount: Int(4), NormalizedSchemaSHA256: digest,
			CanonicalResultSHA256: digest},
		Generation: ManifestGeneration{Seed: 20260801, GeneratorVersion: "generator-v1", Command: "final-v5-oracle generate"},
	}
}

func TestManifestCanonicalRoundTripAndDeterminism(t *testing.T) {
	one, err := CanonicalManifest(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	two, _ := CanonicalManifest(testManifest())
	if !bytes.Equal(one, two) {
		t.Fatal("identical manifest regeneration changed bytes")
	}
	decoded, err := DecodeManifest(one)
	if err != nil || decoded.Scale != "100x4" {
		t.Fatalf("decoded manifest = %+v, err=%v", decoded, err)
	}
	firstSHA, _ := ManifestSHA256(testManifest())
	changed := testManifest()
	changed.Generation.Seed++
	secondSHA, _ := ManifestSHA256(changed)
	if firstSHA == secondSHA {
		t.Fatal("seed mutation did not change manifest digest")
	}
}

func TestManifestRejectsNonCanonicalUnknownAndDuplicateJSON(t *testing.T) {
	canonical, _ := CanonicalManifest(testManifest())
	if _, err := DecodeManifest(append([]byte(" "), canonical...)); err == nil {
		t.Fatal("leading whitespace was accepted")
	}
	unknown := bytes.Replace(canonical, []byte(`"schema_version":1`), []byte(`"unknown":1,"schema_version":1`), 1)
	if _, err := DecodeManifest(unknown); err == nil {
		t.Fatal("unknown field was accepted")
	}
	duplicate := bytes.Replace(canonical, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1)
	if _, err := DecodeManifest(duplicate); err == nil {
		t.Fatal("duplicate field was accepted")
	}
	wrongIdentity := testManifest()
	wrongIdentity.BindingKey = "1k/101"
	if _, err := CanonicalManifest(wrongIdentity); err == nil {
		t.Fatal("a non-ProvSQL manifest carried a private binding key")
	}
}

func TestManifestWorkloadSpecificRequirements(t *testing.T) {
	digest := strings.Repeat("b", 64)
	manifest := testManifest()
	manifest.ExperimentID = "scale"
	manifest.WorkloadID = "outcome-merkle"
	manifest.Scale = "10k-x1-o50"
	manifest.Mode = "merkle_control"
	manifest.Expected = ManifestExpected{
		OutcomeCandidateCardinality: Int64(1), OutcomeCandidateSetSHA256: digest,
		ExistingCardinality: Int64(10_000), ExistingSetSHA256: digest,
		OverlapCardinality: Int64(1), OverlapSetSHA256: digest,
		NovelCardinality: Int64(0), NovelSetSHA256: digest,
		UnionCardinality: Int64(10_000), UnionSetSHA256: digest,
		ScheduleSHA256: digest,
	}
	if _, err := CanonicalManifest(manifest); err != nil {
		t.Fatalf("complete outcome manifest rejected: %v", err)
	}
	mutated := manifest
	mutated.Expected.UnionCardinality = Int64(10_001)
	if _, err := CanonicalManifest(mutated); err == nil {
		t.Fatal("inconsistent outcome set algebra accepted")
	}
	mutated = manifest
	mutated.Expected.ScheduleSHA256 = ""
	if _, err := CanonicalManifest(mutated); err == nil {
		t.Fatal("outcome manifest without x1 schedule accepted")
	}

	baseline := testManifest()
	baseline.ExperimentID = "baseline"
	baseline.WorkloadID = "S3"
	if _, err := CanonicalManifest(baseline); err == nil {
		t.Fatal("S3 manifest without component-set expectations accepted")
	}
}
