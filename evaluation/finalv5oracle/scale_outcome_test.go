package finalv5oracle

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/catalog"
	productionexposure "taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

const scaleOutcomeTestCatalogSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestExposureScaleOutcomeCandidateExactFiveMemberOrdinarySet(t *testing.T) {
	candidate, err := GenerateExposureScaleOutcomeCandidate(ExposureScaleOutcomeRequest{
		CatalogSHA256:  scaleOutcomeTestCatalogSHA256,
		CandidateFacts: DependencyScale10K,
		SetOptions:     StreamSetOptions{MaxInMemoryMembers: 2048, CaptureMembers: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.GeneratorVersion != ExposureScaleOutcomeGeneratorVersion ||
		candidate.ProductID != ExposureScaleProductID || candidate.Publication != exposureScalePublication ||
		candidate.CandidateFacts != DependencyScale10K || candidate.CandidateRows != 2_000 ||
		candidate.QueryNormalFormVersion != exposureScaleQueryNormalFormVersion {
		t.Fatalf("fixed Scale Outcome identity = %+v", candidate)
	}
	if len(candidate.Atoms) != 4 || candidate.CandidateCardinality != 5 || len(candidate.Members) != 5 ||
		!slices.IsSorted(candidate.Members) || !slices.Contains(candidate.Members, candidate.Composite.SHA256) {
		t.Fatalf("Scale Outcome candidate is not four atoms plus one sorted composite: %+v", candidate)
	}
	for index, atom := range candidate.Atoms {
		if atom.Profile != OracleExposureProfileV5 || atom.Kind != OracleFactKindPredicateAtom {
			t.Fatalf("atom %d has wrong profile/kind: %+v", index+1, atom)
		}
		if err := ValidateCanonicalFact(atom); err != nil {
			t.Fatalf("atom %d: %v", index+1, err)
		}
		if !slices.Contains(candidate.Members, atom.SHA256) {
			t.Fatalf("atom %d is absent from retained candidate members", index+1)
		}
	}
	if err := ValidateCanonicalFact(candidate.Composite); err != nil {
		t.Fatalf("composite: %v", err)
	}
	summary, err := SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range candidate.Members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, StreamSetOptions{MaxInMemoryMembers: 5, CaptureMembers: 5})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Cardinality != candidate.CandidateCardinality ||
		summary.SetSHA256 != candidate.CandidateSetSHA256 || !slices.Equal(summary.Members, candidate.Members) {
		t.Fatalf("ordinary-set replay = %+v, candidate = %+v", summary, candidate)
	}

	// Filled fixed vectors make changes to the reviewed model explicit rather
	// than allowing a self-consistent encoder change to bless itself.
	wantAtoms := []string{
		"8e061c69bef59b7b1d5ae73990801781feb130d7e86843b8302d160a76b59bb6",
		"b94eeb15c937218f0b25582775b95bf954db3495b4c0ebfc2b17035ed75965b9",
		"cbe4970f4352711daedc0f308b5651ffd1589ec0532944e5801d40e3b00b8cf0",
		"e757f0a17153ef5bf855f371e83681cef47bb430c5185c2abb452d275000d582",
	}
	wantMembers := []string{
		wantAtoms[0],
		"afd451610d9d78c28e921f772a3d5cd3db9a182205c5e9d5b52ab9108e941c38",
		wantAtoms[1], wantAtoms[2], wantAtoms[3],
	}
	if candidate.PredicateContextSHA256 != "a63ecd28ae801247929b59447817455ead801ca46028a6360846baeb1d618551" ||
		candidate.QueryNormalFormSHA256 != "a6425a21e02a3ae5705d1c53f11101475950ff0baa1867893fa3d6bd5d641a40" ||
		candidate.ResultObservationSHA256 != "05deb68a12f20dc221dc4ac0a41c10ee13c0b8dbd323ca9bc813bb4365e46541" ||
		candidate.PredicateSetSHA256 != "c3f65a6e3c3c6cee436affc025c350db2b70ff2907398c32085762ae0bae35cc" ||
		candidate.Composite.SHA256 != "afd451610d9d78c28e921f772a3d5cd3db9a182205c5e9d5b52ab9108e941c38" ||
		candidate.CandidateSetSHA256 != "08a2a71cc856a646ea797abc4cfebf7e3f95bfc8d660ae5a5365403fc1687ef5" ||
		!slices.Equal(scaleOutcomeAtomHashes(candidate), wantAtoms) || !slices.Equal(candidate.Members, wantMembers) {
		t.Fatalf("Scale Outcome fixed vector: context=%s normal=%s result=%s predicate_set=%s composite=%s candidate_set=%s members=%v atoms=%v",
			candidate.PredicateContextSHA256, candidate.QueryNormalFormSHA256, candidate.ResultObservationSHA256,
			candidate.PredicateSetSHA256, candidate.Composite.SHA256, candidate.CandidateSetSHA256,
			candidate.Members, scaleOutcomeAtomHashes(candidate))
	}
}

func TestExposureScaleOutcomeCandidateAtomMutations(t *testing.T) {
	model := scaleOutcomeTestModel(t)
	base := mustBuildScaleOutcomeCandidate(t, model)
	tests := []struct {
		name   string
		mutate func(*exposureScaleOutcomeModel)
	}{
		{"field", func(value *exposureScaleOutcomeModel) { value.Atoms[0].PublicFieldID = "changed_partition_key" }},
		{"type", func(value *exposureScaleOutcomeModel) { value.Atoms[2].SQLType = "integer" }},
		{"operator", func(value *exposureScaleOutcomeModel) { value.Atoms[2].Operator = "LT" }},
		{"literal", func(value *exposureScaleOutcomeModel) { value.Atoms[2].CanonicalLiteral = "i:1999" }},
		{"stable role", func(value *exposureScaleOutcomeModel) { value.Atoms[0].StableRole = "changed_scale_role" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutatedModel := cloneExposureScaleOutcomeModel(model)
			test.mutate(&mutatedModel)
			changed := mustBuildScaleOutcomeCandidate(t, mutatedModel)
			if base.PredicateContextSHA256 != changed.PredicateContextSHA256 ||
				base.QueryNormalFormSHA256 != changed.QueryNormalFormSHA256 ||
				base.ResultObservationSHA256 != changed.ResultObservationSHA256 {
				t.Fatal("atom mutation changed a surrounding context commitment")
			}
			if scaleOutcomeAtomIntersection(base, changed) != 3 ||
				base.PredicateSetSHA256 == changed.PredicateSetSHA256 ||
				base.Composite.SHA256 == changed.Composite.SHA256 ||
				base.CandidateSetSHA256 == changed.CandidateSetSHA256 {
				t.Fatalf("%s mutation was not bound through atom, predicate set, composite, and ordinary candidate set", test.name)
			}
		})
	}
}

func TestExposureScaleOutcomeCandidateContextMutations(t *testing.T) {
	model := scaleOutcomeTestModel(t)
	base := mustBuildScaleOutcomeCandidate(t, model)
	tests := []struct {
		name   string
		mutate func(*exposureScaleOutcomeModel)
	}{
		{"Catalog", func(value *exposureScaleOutcomeModel) {
			value.CatalogSHA256 = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
		}},
		{"publication", func(value *exposureScaleOutcomeModel) {
			value.Publication.PublicationSHA256 = strings.Repeat("ab", 32)
		}},
		{"scope", func(value *exposureScaleOutcomeModel) {
			value.MandatoryScope = []byte(`{"partition_key":["2"]}`)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutatedModel := cloneExposureScaleOutcomeModel(model)
			test.mutate(&mutatedModel)
			changed := mustBuildScaleOutcomeCandidate(t, mutatedModel)
			if base.PredicateContextSHA256 == changed.PredicateContextSHA256 ||
				base.QueryNormalFormSHA256 != changed.QueryNormalFormSHA256 ||
				base.ResultObservationSHA256 != changed.ResultObservationSHA256 ||
				scaleOutcomeAtomIntersection(base, changed) != 0 ||
				base.Composite.SHA256 == changed.Composite.SHA256 ||
				base.CandidateSetSHA256 == changed.CandidateSetSHA256 {
				t.Fatalf("%s mutation was not bound through context, all atoms, composite, and ordinary candidate set", test.name)
			}
		})
	}
}

func TestExposureScaleOutcomeCandidateNormalFormAndResultMutations(t *testing.T) {
	model := scaleOutcomeTestModel(t)
	base := mustBuildScaleOutcomeCandidate(t, model)

	normalModel := cloneExposureScaleOutcomeModel(model)
	normalModel.NormalForm.NumericMode = "changed-exact-mode"
	changedNormal := mustBuildScaleOutcomeCandidate(t, normalModel)
	if base.QueryNormalFormSHA256 == changedNormal.QueryNormalFormSHA256 ||
		base.PredicateContextSHA256 != changedNormal.PredicateContextSHA256 ||
		base.ResultObservationSHA256 != changedNormal.ResultObservationSHA256 ||
		!slices.Equal(scaleOutcomeAtomHashes(base), scaleOutcomeAtomHashes(changedNormal)) ||
		base.PredicateSetSHA256 != changedNormal.PredicateSetSHA256 ||
		base.Composite.SHA256 == changedNormal.Composite.SHA256 ||
		base.CandidateSetSHA256 == changedNormal.CandidateSetSHA256 {
		t.Fatal("normal-form mutation was not isolated to the composite and ordinary candidate set")
	}

	resultModel := cloneExposureScaleOutcomeModel(model)
	outputRowKey, err := ComposeOracleCanonicalKeyV2("group-row", "global")
	if err != nil {
		t.Fatal(err)
	}
	resultModel.ReleaseFacts[0], err = BuildV2DerivedFact(V2DerivedInput{
		SnapshotBundle: []V2SnapshotBinding{{
			SourceNamespace: ExposureScaleSourceNamespace,
			Snapshot:        ExposureScaleSnapshot,
		}},
		OutputRowKey:         outputRowKey,
		NormalizedExpression: "count(*)",
		SQLType:              "bigint",
		CanonicalValue:       "i:2000",
		WitnessCommitment:    strings.Repeat("7", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	changedResult := mustBuildScaleOutcomeCandidate(t, resultModel)
	if base.ResultObservationSHA256 == changedResult.ResultObservationSHA256 ||
		base.PredicateContextSHA256 != changedResult.PredicateContextSHA256 ||
		base.QueryNormalFormSHA256 != changedResult.QueryNormalFormSHA256 ||
		!slices.Equal(scaleOutcomeAtomHashes(base), scaleOutcomeAtomHashes(changedResult)) ||
		base.PredicateSetSHA256 != changedResult.PredicateSetSHA256 ||
		base.Composite.SHA256 == changedResult.Composite.SHA256 ||
		base.CandidateSetSHA256 == changedResult.CandidateSetSHA256 {
		t.Fatal("result-observation mutation was not isolated to the composite and ordinary candidate set")
	}
}

// TestExposureScaleOutcomeCandidateAgreesWithProductionPreparation is a
// comparator test, not part of expected-value generation. The left side is
// built first from the fixed evaluation model. Production preparation and Fact
// encoders then supply only the observed/right-side values. Covering the three
// distinct M values settles all twelve dependency cells because overlap changes
// history, never the candidate query.
func TestExposureScaleOutcomeCandidateAgreesWithProductionPreparation(t *testing.T) {
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate Scale Outcome comparator test")
	}
	catalogPath := filepath.Join(filepath.Dir(sourcePath), "..", "final-v5-wsl2", "publication-review",
		"exposure-scale-v1", "catalog.yaml")
	loaded, err := catalog.Load(catalogPath)
	if err != nil {
		t.Fatalf("load reviewed Scale Catalog: %v", err)
	}
	product, found := loaded.LookupProduct(ExposureScaleProductID)
	if !found {
		t.Fatal("reviewed Scale Catalog omits the fixed Product")
	}
	approvedColumns := []string{"member_rank", "metric", "family_id", "partition_key"}
	approvedSet := make(map[string]struct{}, len(approvedColumns))
	for _, column := range approvedColumns {
		approvedSet[column] = struct{}{}
	}
	queryProduct := physicalquery.QueryProductFromCatalog(product, approvedSet)
	view, err := physicalquery.CatalogViewFromCatalog(*loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SnapshotPublications) != 1 {
		t.Fatalf("reviewed Scale Catalog has %d snapshot publications", len(loaded.SnapshotPublications))
	}
	publication := loaded.SnapshotPublications[0]
	snapshotBindings := map[string]physicalquery.SnapshotBinding{
		publication.Name: {
			PublicationName:  publication.Name,
			DictionaryDigest: publication.DictionaryDigest,
			ManifestDigest:   publication.ManifestDigest,
			SidecarDigest:    publication.SidecarDigest,
			SourceNamespace:  publication.SourceNamespace,
			Snapshot:         publication.Snapshot,
			OrdinalSidecar:   publication.OrdinalSidecar,
			RowCount:         414_000,
		},
	}
	grant := physicalquery.Grant{
		ApprovedProducts: []string{ExposureScaleProductID},
		ApprovedColumns:  map[string][]string{ExposureScaleProductID: approvedColumns},
		MandatoryScope:   []byte(exposureScaleMandatoryScope),
		ExposureProfile:  OracleExposureProfileV5,
		PredicateLimits: queryplan.PredicateLimits{
			MaxRawLiteralsPerQuery: 64, MaxUniqueAtomsPerQuery: 16,
			MaxAtomPayloadBytes: 4096, MaxTotalAtomPayloadBytes: 65536,
		},
	}

	for _, candidateFacts := range []int64{DependencyScale10K, DependencyScale100K, DependencyScale1035000} {
		candidateFacts := candidateFacts
		t.Run(strconv.FormatInt(candidateFacts, 10), func(t *testing.T) {
			_, witness, err := SummarizeUnitWitnessSemanticSetRoles([]string{"candidate"},
				func(yield func(string) error) error {
					return StreamExposureScaleFacts(0, candidateFacts, func(fact CanonicalFact) error {
						return yield(fact.SHA256)
					})
				}, StreamSetOptions{MaxInMemoryMembers: 16_384})
			if err != nil {
				t.Fatal(err)
			}
			expectedModel, err := fixedExposureScaleOutcomeModel(loaded.SHA256, candidateFacts, witness)
			if err != nil {
				t.Fatal(err)
			}
			expected := mustBuildScaleOutcomeCandidate(t, expectedModel)

			rows := candidateFacts / ExposureScaleFactsPerRow
			logicalSQL := fmt.Sprintf(`SELECT count(*) AS member_count
FROM final_v5_exposure_scale
WHERE partition_key = 1
  AND family_id = 1
  AND member_rank <= %d
  AND metric <= 1001.00`, rows)
			lowered, err := sqllowering.Lower(logicalSQL,
				map[string]queryplan.Product{ExposureScaleProductID: queryProduct})
			if err != nil {
				t.Fatalf("lower fixed production comparator: %v", err)
			}
			if lowered.Plan.From != nil || lowered.Plan.Product != ExposureScaleProductID {
				t.Fatalf("single-source Scale comparator lowered to unexpected plan shape: %+v", lowered.Plan)
			}
			prepared, err := physicalquery.Prepare(physicalquery.PreparationInputs{
				Plan: lowered.Plan, Grant: grant, Catalog: view, SnapshotBindings: snapshotBindings,
			})
			if err != nil {
				t.Fatalf("prepare fixed production comparator: %v", err)
			}
			if prepared.Binding().NormalFormSHA256 != expected.QueryNormalFormSHA256 {
				t.Fatalf("normal-form digest: expected oracle %s, production actual %s",
					expected.QueryNormalFormSHA256, prepared.Binding().NormalFormSHA256)
			}
			footprint, err := prepared.PredicateFootprint()
			if err != nil {
				t.Fatal(err)
			}
			if footprint == nil || footprint.ContextSHA256 != expected.PredicateContextSHA256 ||
				footprint.AtomSetSHA256 != expected.PredicateSetSHA256 || len(footprint.Atoms) != 4 {
				t.Fatalf("predicate footprint: expected context/set/count %s/%s/4, production actual %+v",
					expected.PredicateContextSHA256, expected.PredicateSetSHA256, footprint)
			}
			actualAtomHashes := make([]string, len(footprint.Atoms))
			for index, atom := range footprint.Atoms {
				actualAtomHashes[index], err = atom.Hash()
				if err != nil {
					t.Fatal(err)
				}
			}
			sort.Strings(actualAtomHashes)
			if !slices.Equal(actualAtomHashes, scaleOutcomeAtomHashes(expected)) {
				t.Fatalf("atom members: expected oracle %v, production actual %v",
					scaleOutcomeAtomHashes(expected), actualAtomHashes)
			}

			outputRowKey, err := productionexposure.ComposeCanonicalKeyV2("group-row", "global")
			if err != nil {
				t.Fatal(err)
			}
			releaseFact, err := productionexposure.NewDerivedFactV2(
				[]productionexposure.SnapshotBinding{{
					SourceNamespace: ExposureScaleSourceNamespace, Snapshot: ExposureScaleSnapshot,
				}}, outputRowKey, "count(*)", "bigint", rows, witness)
			if err != nil {
				t.Fatal(err)
			}
			resultObservation, err := productionexposure.ReleaseOutcomeDigest(
				[]productionexposure.FactID{releaseFact}, 1)
			if err != nil {
				t.Fatal(err)
			}
			if resultObservation != expected.ResultObservationSHA256 {
				t.Fatalf("result observation: expected oracle %s, production actual %s",
					expected.ResultObservationSHA256, resultObservation)
			}
			composite, err := productionexposure.NewCompositeOutcomeFactV5(productionexposure.CompositeOutcomeFactV5{
				QueryNormalFormVersion:  exposureScaleQueryNormalFormVersion,
				QueryNormalFormSHA256:   expected.QueryNormalFormSHA256,
				ResultObservationSHA256: resultObservation,
				VisibleRows:             1,
				PredicateContextSHA256:  footprint.ContextSHA256,
				PredicateSetSHA256:      footprint.AtomSetSHA256,
				PredicateAtomCount:      int64(len(footprint.Atoms)),
			})
			if err != nil {
				t.Fatal(err)
			}
			actualComposite, err := composite.Hash()
			if err != nil {
				t.Fatal(err)
			}
			if actualComposite != expected.Composite.SHA256 {
				t.Fatalf("composite: expected oracle %s, production actual %s",
					expected.Composite.SHA256, actualComposite)
			}
			actualMembers := append(actualAtomHashes, actualComposite)
			sort.Strings(actualMembers)
			actualSummary, err := SummarizeSemanticSet("candidate", func(yield func(string) error) error {
				for _, member := range actualMembers {
					if err := yield(member); err != nil {
						return err
					}
				}
				return nil
			}, StreamSetOptions{MaxInMemoryMembers: 5})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(actualMembers, expected.Members) ||
				actualSummary.Cardinality != expected.CandidateCardinality ||
				actualSummary.SetSHA256 != expected.CandidateSetSHA256 {
				t.Fatalf("ordinary candidate: expected oracle %v/%d/%s, production operands %v/%d/%s",
					expected.Members, expected.CandidateCardinality, expected.CandidateSetSHA256,
					actualMembers, actualSummary.Cardinality, actualSummary.SetSHA256)
			}
		})
	}
}

func TestExposureScaleOutcomeOracleHasNoSQLOrProductionDerivationInput(t *testing.T) {
	request := reflect.TypeOf(ExposureScaleOutcomeRequest{})
	wantFields := []string{"CatalogSHA256", "CandidateFacts", "SetOptions"}
	if request.NumField() != len(wantFields) {
		t.Fatalf("Scale Outcome request exposes %d inputs, want %d", request.NumField(), len(wantFields))
	}
	for index, want := range wantFields {
		if request.Field(index).Name != want {
			t.Fatalf("Scale Outcome request field %d is %q, want %q", index+1, request.Field(index).Name, want)
		}
	}
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate Scale Outcome boundary test")
	}
	source, err := os.ReadFile(strings.TrimSuffix(sourcePath, "_test.go") + ".go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"github.com/pganalyze", "/internal/physicalquery", "/internal/queryplan",
		"SELECT ", "Prepare(", ".Prepare(", "Derive(", ".Derive(",
	} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("Scale Outcome fixed oracle contains forbidden parser/production operation %q", forbidden)
		}
	}
}

func TestExposureScaleOutcomeCandidateRejectsUnfrozenInputs(t *testing.T) {
	for _, request := range []ExposureScaleOutcomeRequest{
		{CatalogSHA256: strings.Repeat("0", 64), CandidateFacts: DependencyScale10K},
		{CatalogSHA256: scaleOutcomeTestCatalogSHA256, CandidateFacts: 15_000},
	} {
		if _, err := GenerateExposureScaleOutcomeCandidate(request); err == nil {
			t.Fatalf("unfrozen Scale Outcome input was accepted: %+v", request)
		}
	}
}

func scaleOutcomeTestModel(t *testing.T) exposureScaleOutcomeModel {
	t.Helper()
	summaries, witness, err := SummarizeUnitWitnessSemanticSetRoles([]string{"candidate"},
		func(yield func(string) error) error {
			return StreamExposureScaleFacts(0, DependencyScale10K, func(fact CanonicalFact) error {
				return yield(fact.SHA256)
			})
		}, StreamSetOptions{MaxInMemoryMembers: 2048})
	if err != nil {
		t.Fatal(err)
	}
	if summaries["candidate"].Cardinality != DependencyScale10K {
		t.Fatalf("test witness cardinality = %d", summaries["candidate"].Cardinality)
	}
	model, err := fixedExposureScaleOutcomeModel(scaleOutcomeTestCatalogSHA256, DependencyScale10K, witness)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func mustBuildScaleOutcomeCandidate(t *testing.T, model exposureScaleOutcomeModel) ExposureScaleOutcomeCandidate {
	t.Helper()
	candidate, err := buildExposureScaleOutcomeCandidate(model)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

func cloneExposureScaleOutcomeModel(source exposureScaleOutcomeModel) exposureScaleOutcomeModel {
	cloned := source
	cloned.MandatoryScope = append([]byte(nil), source.MandatoryScope...)
	cloned.NormalForm.Columns = append([]string(nil), source.NormalForm.Columns...)
	cloned.NormalForm.Aggregates = append([]exposureScaleOutcomeAggregate(nil), source.NormalForm.Aggregates...)
	cloned.NormalForm.Filters = append([]exposureScaleOutcomeFilter(nil), source.NormalForm.Filters...)
	for index := range cloned.NormalForm.Filters {
		cloned.NormalForm.Filters[index].Value = append([]byte(nil), source.NormalForm.Filters[index].Value...)
	}
	cloned.NormalForm.GroupBy = append([]string(nil), source.NormalForm.GroupBy...)
	cloned.NormalForm.OrderBy = append([]exposureScaleOutcomeOrder(nil), source.NormalForm.OrderBy...)
	cloned.NormalForm.Collations = append([]exposureScaleOutcomeCollation(nil), source.NormalForm.Collations...)
	cloned.ReleaseFacts = append([]CanonicalFact(nil), source.ReleaseFacts...)
	for index := range cloned.ReleaseFacts {
		cloned.ReleaseFacts[index].Payload = append([]byte(nil), source.ReleaseFacts[index].Payload...)
	}
	cloned.Atoms = append([]V5PredicateAtomInput(nil), source.Atoms...)
	return cloned
}

func scaleOutcomeAtomHashes(candidate ExposureScaleOutcomeCandidate) []string {
	result := make([]string, len(candidate.Atoms))
	for index, atom := range candidate.Atoms {
		result[index] = atom.SHA256
	}
	return result
}

func scaleOutcomeAtomIntersection(left, right ExposureScaleOutcomeCandidate) int {
	rightMembers := make(map[string]bool, len(right.Atoms))
	for _, atom := range right.Atoms {
		rightMembers[atom.SHA256] = true
	}
	intersection := 0
	for _, atom := range left.Atoms {
		if rightMembers[atom.SHA256] {
			intersection++
		}
	}
	return intersection
}
