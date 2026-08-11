package finalv5publication

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5oracle"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
)

const testLiveDigest = "bf4c3fe0897000f7673250e5fe0131a03019b49e9c3839e9fc4911c3716290b4"

func TestLoadGenerationMaterialsEnumeratesExactSourceInputs(t *testing.T) {
	materials, err := loadGenerationMaterials(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if materials.approval.ApprovalID != "APPROVE-C2" ||
		materials.decision.Path != "docs/final_v5_author_decisions.md" ||
		materials.baseCatalog.Path != baseCatalogRelativePath ||
		materials.approvedScaleCatalog.SHA256 != approvedC2ScaleCatalogSHA256 ||
		materials.contract.Release == "" || materials.contract.Index.SHA256 != materials.runtime.IndexSHA256() ||
		len(materials.contract.Artifacts) != 28 || len(materials.scaleManifests) != 24 ||
		len(materials.provSQLManifests) != 105 {
		t.Fatalf("generation inputs are incomplete: decision=%+v base=%+v Scale=%+v contracts=%d/%d manifests=%d/%d",
			materials.decision, materials.baseCatalog, materials.approvedScaleCatalog,
			len(materials.contract.Artifacts), len(materials.contract.Index.SHA256),
			len(materials.scaleManifests), len(materials.provSQLManifests))
	}
}

func TestBuildSetAlgebraIsExactTwelveAndLiveBound(t *testing.T) {
	materials, err := loadGenerationMaterials(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	one, err := buildSetAlgebra(materials.scaleManifests, testLiveDigest)
	if err != nil {
		t.Fatal(err)
	}
	if one.ScaleCells != 12 || len(one.Cells) != 12 || !generatedSHA256(one.SHA256) {
		t.Fatalf("set algebra is not exact/live-bound: %+v", one)
	}
	other, err := buildSetAlgebra(materials.scaleManifests,
		"a67b472a3b495f59f14b09e539edeaa7a6f7cdae7853e04cc92cd1eff1eb872d")
	if err != nil {
		t.Fatal(err)
	}
	if one.SHA256 == other.SHA256 {
		t.Fatal("set algebra identity did not bind the live observation")
	}
}

func TestBuildBindingInputIdentityRejectsPlaceholders(t *testing.T) {
	digests := []string{
		"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881",
		"a1fce4363854ff888cff4b8e7875d600c2682390412ef426f58ee5c6c35a93b4",
		"594e519ae499312b29433b7dd8a97ff068defcba9755b6d5d00e84c526d8a5c6",
		"f4fa59c07fdd3e582ed8b0d1f2f87d521b00dfac1374d6de1dc3886c53aa69af",
		"fa20140766ffde1f0d003bad99e13367f740067e895986d34e657de6d871402a",
		"f5c5a68f10b5f83ce241435e51bfef7f28c2320fb755223e04da1e3d1e8c3b18",
		"01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b",
	}
	identity, err := buildBindingInputIdentity(digests[0], digests[1], digests[2], digests[3], digests[4], digests[5], digests[6])
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != "taskgate-final-v5-publication-binding-input-identity-v1" ||
		identity.ClaimScope != "publication_binding_inputs_and_live_postgresql_result_agreement_only" ||
		!generatedSHA256(identity.SHA256) {
		t.Fatalf("publication binding-input identity has the wrong scope: %+v", identity)
	}
	for _, placeholder := range []string{strings.Repeat("0", 64), strings.Repeat("b", 64)} {
		if _, err := buildBindingInputIdentity(placeholder, digests[1], digests[2], digests[3],
			digests[4], digests[5], digests[6]); err == nil {
			t.Fatal("placeholder publication binding-input identity component was accepted")
		}
	}
}

func TestAppendOutcomeCandidateIdentitiesRetainsExactSortedFiveMembers(t *testing.T) {
	members := []string{
		sha256Hex([]byte("member-a")),
		sha256Hex([]byte("member-b")),
		sha256Hex([]byte("member-c")),
		sha256Hex([]byte("member-d")),
		sha256Hex([]byte("member-e")),
	}
	sort.Strings(members)
	summary, err := finalv5oracle.SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, finalv5oracle.StreamSetOptions{MaxInMemoryMembers: len(members)})
	if err != nil {
		t.Fatal(err)
	}
	expected := finalv5binding.BoundOutcomeCandidateExpectation{
		Cardinality: summary.Cardinality, Members: members, OrdinarySetSHA256: summary.SetSHA256,
	}
	identities, err := appendOutcomeCandidateIdentities(nil, expected)
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 6 || identities[0].Name != "outcome_candidate_ordinary_set" ||
		identities[0].SHA256 != expected.OrdinarySetSHA256 {
		t.Fatalf("Outcome candidate provenance closure is incomplete: %+v", identities)
	}
	for index, member := range members {
		if identities[index+1].Name != "outcome_candidate_member_0"+string(rune('1'+index)) ||
			identities[index+1].SHA256 != member {
			t.Fatalf("Outcome member %d is not retained exactly: %+v", index+1, identities[index+1])
		}
	}
}

func TestGenerationFixesScaleOutcomeExpectationsBeforeLiveObservation(t *testing.T) {
	value, err := os.ReadFile("generate.go")
	if err != nil {
		t.Fatal(err)
	}
	oracleIndex := bytes.Index(value, []byte("finalv5binding.GenerateScaleOutcomeCandidateExpectations("))
	datasetIndex := bytes.Index(value, []byte("finalv5dataset.VerifyBenchmarkPostgreSQL("))
	observationIndex := bytes.Index(value, []byte("ObservePublicationClosure("))
	bindingIndex := bytes.Index(value, []byte("finalv5binding.BuildCompleteBinding("))
	if oracleIndex < 0 || datasetIndex < 0 || observationIndex < 0 || bindingIndex < 0 ||
		oracleIndex >= datasetIndex || datasetIndex >= observationIndex || observationIndex >= bindingIndex {
		t.Fatalf("Scale Outcome expectations are not fixed before live Dataset/closure observation: oracle=%d Dataset=%d observation=%d binding=%d",
			oracleIndex, datasetIndex, observationIndex, bindingIndex)
	}
}

func TestStrictProvenanceRejectsUnknownField(t *testing.T) {
	value, err := canonicalJSON(ProvenanceReport{})
	if err != nil {
		t.Fatal(err)
	}
	value = insertBeforeFinalObjectEnd(t, value, `,"unknown_field":true`)
	var report ProvenanceReport
	if err := strictJSON(value, &report); err == nil {
		t.Fatal("unknown provenance field was accepted")
	}
}

func TestRequireAbsentOutputEnforcesCreateExclusivePrecondition(t *testing.T) {
	root := t.TempDir()
	absent := filepath.Join(root, "candidate")
	if err := requireAbsentOutput(absent); err != nil {
		t.Fatalf("absent output was rejected: %v", err)
	}
	if err := os.Mkdir(absent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := requireAbsentOutput(absent); err == nil {
		t.Fatal("existing output was accepted")
	}
}

func TestLiveObservationDigestDetectsByteDrift(t *testing.T) {
	one := LiveObservation{Version: liveObservationVersion, StartedAtUTC: "2026-08-11T00:00:00Z",
		CompletedAtUTC: "2026-08-11T00:00:01Z", SessionIdentitySHA256: testLiveDigest}
	digest, err := liveObservationDigest(one)
	if err != nil {
		t.Fatal(err)
	}
	two := one
	two.CompletedAtUTC = "2026-08-11T00:00:02Z"
	drifted, err := liveObservationDigest(two)
	if err != nil {
		t.Fatal(err)
	}
	if digest == drifted || reflect.DeepEqual(one, two) {
		t.Fatal("live observation byte drift did not change its identity")
	}
}
