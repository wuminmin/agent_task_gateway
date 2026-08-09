package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5binding"
	"taskbound.local/agent-data-gateway/evaluation/internal/provsqlfixture"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
	fixture "taskbound.local/agent-data-gateway/internal/testfixture/queryreceiptv10"
)

// These tests run the deployment resolvers against the repository's own
// retained material: the embedded Contract Index, the source-controlled profile
// registry and Catalog, the published snapshot artifacts, and one retained
// qualification run with the PostgreSQL identity it was measured against.
//
// That is deliberate rather than convenient. A resolver nothing has ever
// executed is worse evidence than none, and three of the five can be executed
// here for real -- only the Control Store reader and the keyring fetch need a
// running deployment. What cannot be established without one is whether the
// deployment is serving this material; what can be, and is, is that the material
// resolves and that the frozen contract prepares from it.

// retainedQualificationRun is the current qualification whose footprint and
// PostgreSQL identity the repository retains together. Both files come from one
// run, and a second independent run reproduces its portable footprint in the
// active i2b agreement.
const retainedQualificationRun = "diagnosis-attestation-footprint-qualification-i2b-01-" +
	"20260808T152236Z-017e73a3a749"

func retainedQualificationDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "final-v5-wsl2", "raw", retainedQualificationRun))
	if err != nil {
		t.Fatalf("resolve the retained qualification: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the retained qualification is not present: %v", err)
	}
	return path
}

func repositoryRootForDeployment(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	return root
}

func sourceControlledRegistryPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRootForDeployment(t), "config", "profiles", "registry.json")
}

// deploymentProfilesForTest points the profile resolver at a copy of the
// source-controlled registry with Result-heavy cleared, and at everything else
// real: the Catalog, retained publication artifacts and a served Catalog digest
// taken from the Catalog file itself. The copy keeps these resolver mechanics
// independent of whether this checkout has recorded its release-specific live
// smoke yet.
func deploymentProfilesForTest(t *testing.T) deploymentProfilesV3 {
	t.Helper()
	root := repositoryRootForDeployment(t)
	digest, err := FileSHA256(resultHeavyCatalogPath(t))
	if err != nil {
		t.Fatalf("digest the profile Catalog: %v", err)
	}
	return deploymentProfilesV3{
		registryPath:        registryPathWithResultHeavyClearance(t, true),
		repositoryRoot:      root,
		artifactDir:         retainedSnapshotArtifacts(t),
		servedCatalogSHA256: digest,
	}
}

// registryPathWithResultHeavyClearance writes a registry copy with an explicit
// Result-heavy clearance state. Positive and negative resolver tests therefore
// exercise the same real registry material without hand-editing committed
// readiness or depending on which activation evidence this release has recorded.
func registryPathWithResultHeavyClearance(t *testing.T, clearance bool) string {
	t.Helper()
	payload, err := os.ReadFile(sourceControlledRegistryPath(t))
	if err != nil {
		t.Fatalf("read the profile registry: %v", err)
	}
	var registry map[string]any
	if err := json.Unmarshal(payload, &registry); err != nil {
		t.Fatalf("decode the profile registry: %v", err)
	}
	profiles, _ := registry["profiles"].([]any)
	found := false
	for _, entry := range profiles {
		profile, _ := entry.(map[string]any)
		if profile == nil || profile["alias"] != artifactDeploymentProfileAlias {
			continue
		}
		profile["targeted_run_eligible"] = clearance
		status, _ := profile["status"].(map[string]any)
		if status == nil {
			t.Fatal("the registry profile carries no status")
		}
		status["activation_supported"] = clearance
		status["activation_smoke_passed"] = clearance
		found = true
	}
	if !found {
		t.Fatalf("the profile registry declares no %q profile", artifactDeploymentProfileAlias)
	}
	edited, err := json.Marshal(registry)
	if err != nil {
		t.Fatalf("re-encode the profile registry: %v", err)
	}
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("write the registry clearance fixture: %v", err)
	}
	return path
}

// The clearance gates are load bearing. Removing the current-release activation
// clearance from a registry copy must make resolution fail, so a measured run
// cannot bootstrap the readiness it depends on.
func TestTheProfileResolverRefusesAnUnclearedProfile(t *testing.T) {
	profiles := deploymentProfilesForTest(t)
	profiles.registryPath = registryPathWithResultHeavyClearance(t, false)
	_, err := profiles.Resolve(artifactDeploymentProfileAlias)
	if err == nil {
		t.Fatal("the finalizer resolved material for a profile with no recorded live activation smoke")
	}
	if !strings.Contains(err.Error(), artifactDeploymentProfileAlias) {
		t.Fatalf("the refusal does not name the profile: %v", err)
	}
}

// The profile resolver returns the registry's own copy of the activated Catalog,
// and refuses one whose bytes are not the ones the deployment serves.
func TestTheProfileResolverBindsTheRegistryCatalogToTheServedOne(t *testing.T) {
	profiles := deploymentProfilesForTest(t)
	material, err := profiles.Resolve(artifactDeploymentProfileAlias)
	if err != nil {
		t.Fatalf("resolve the activated profile: %v", err)
	}
	if material.CatalogPath == "" || material.SnapshotArtifactDir == "" {
		t.Fatalf("the activated profile resolved to %+v", material)
	}
	digest, err := FileSHA256(material.CatalogPath)
	if err != nil {
		t.Fatalf("digest the resolved Catalog: %v", err)
	}
	if digest != profiles.servedCatalogSHA256 {
		t.Fatalf("the resolver returned Catalog %s, the deployment serves %s",
			shortDigest(digest), shortDigest(profiles.servedCatalogSHA256))
	}

	// A deployment serving different bytes is refused rather than resolved. This
	// is the check that stops a finalization attesting against a Catalog nobody
	// activated -- the profile would still be eligible, still have an activation
	// smoke, and still name a Catalog that exists.
	other := profiles
	other.servedCatalogSHA256 = strings.Repeat("a", 64)
	if _, err := other.Resolve(artifactDeploymentProfileAlias); err == nil {
		t.Fatal("a profile was resolved for a deployment serving a different Catalog")
	}
	if _, err := profiles.Resolve("a-profile-no-registry-declares"); err == nil {
		t.Fatal("an undeclared profile resolved to material")
	}
}

// The retained qualification is accepted only where its own recorded digest
// agrees with the footprint it carries, and where that footprint qualifies the
// ExpectedSchema and the server this run uses.
func TestTheQualificationResolverSelfChecksAndBinds(t *testing.T) {
	directory := retainedQualificationDirectory(t)
	qualification := retainedQualificationV3{
		documentPath: filepath.Join(directory, "attestation-footprint-v2.json"),
	}
	identity := retainedPostgreSQLIdentityV3{
		documentPath: filepath.Join(directory, "postgresql-identity.json"),
	}
	postgres, err := identity.ReadPostgreSQLIdentity(context.Background())
	if err != nil {
		t.Fatalf("read the retained PostgreSQL identity: %v", err)
	}
	footprint, err := qualification.Resolve(resultHeavyCatalogPath(t), postgres)
	if err != nil {
		t.Fatalf("resolve the qualified footprint: %v", err)
	}
	if len(footprint.InternalKeys()) == 0 {
		t.Fatal("the qualified footprint measured no PostgreSQL-internal statement")
	}

	// A different server is refused. The footprint scales with the ExpectedSchema
	// and is a property of one server build, so a qualification carried across
	// either binding would be asserting a measurement that was never made.
	if _, err := qualification.Resolve(resultHeavyCatalogPath(t), testRuntimeIdentity()); err == nil {
		t.Fatal("a footprint qualified elsewhere was accepted for this deployment")
	}

	// And a document whose footprint no longer digests to what it records is
	// refused, which is what makes the retained file self-checking rather than
	// merely present.
	payload, err := os.ReadFile(qualification.documentPath)
	if err != nil {
		t.Fatalf("read the retained qualification: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode the retained qualification: %v", err)
	}
	document["footprint_sha256"] = strings.Repeat("b", 64)
	edited, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-encode the retained qualification: %v", err)
	}
	path := filepath.Join(t.TempDir(), "attestation-footprint-v2.json")
	if err := os.WriteFile(path, edited, 0o600); err != nil {
		t.Fatalf("write the edited qualification: %v", err)
	}
	if _, err := (retainedQualificationV3{documentPath: path}).Resolve(
		resultHeavyCatalogPath(t), postgres); err == nil {
		t.Fatal("a qualification that disagrees with its own recorded digest was accepted")
	}
}

// deploymentContractsForTest binds the embedded Contract Index to the
// source-controlled Catalog.
//
// The dataset probe is a placeholder here and nowhere else: BindDeployment
// requires a SHA-256-shaped deployment observation and records it, and nothing
// downstream of the binding reads it. Every other input is the real one.
func deploymentContractsForTest(t *testing.T) deploymentContractsV3 {
	t.Helper()
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatalf("load the frozen Contract Index: %v", err)
	}
	catalogPath := resultHeavyCatalogPath(t)
	digest, err := FileSHA256(catalogPath)
	if err != nil {
		t.Fatalf("digest the Catalog: %v", err)
	}
	liveCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		t.Fatalf("load the Catalog: %v", err)
	}
	return deploymentContractsV3{
		runtime: runtime,
		live: finalv5contracts.LiveDeployment{
			CatalogPath: catalogPath, CatalogSHA256: digest,
			DatasetProbeSHA256: strings.Repeat("c", 64),
		},
		catalog: liveCatalog,
	}
}

// The contract resolver produces an operation the finalizer can actually
// prepare, and the selector narrows it exactly.
//
// The plan is not written out anywhere in this test. It is lowered from the
// frozen BDG statement through the production lowering, which is the property
// that matters: a finalizer that transcribed the plan would agree with itself
// about a step it is supposed to be reproducing.
func TestTheContractResolverLowersTheFrozenStatement(t *testing.T) {
	contracts := deploymentContractsForTest(t)
	// An explicit Artifact selector must never acquire Scale's private binding.
	// Point its captured source at a missing file so this test proves that
	// separation instead of merely running with a convenient environment.
	contracts.bindingSource = deploymentBindingSourceV3{
		path:       "/a-scale-binding-artifact-must-not-read",
		fileSHA256: strings.Repeat("1", 64), sectionSHA256: strings.Repeat("2", 64),
	}
	all, err := contracts.ResolveCandidates(FrozenContractSelectorV3{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
	})
	if err != nil {
		t.Fatalf("resolve every frozen Artifact cell: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("the frozen Contract Index defines no Artifact cell")
	}
	for _, candidate := range all {
		if candidate.PathKind != PathPairedNovel {
			t.Errorf("%s resolved to path_kind %s", candidate.OperationID, candidate.PathKind)
		}
		if candidate.Plan.Product == "" || len(candidate.Plan.Columns) == 0 {
			t.Errorf("%s lowered to an empty plan", candidate.OperationID)
		}
		if err := candidate.Grant.Validate(); err != nil {
			t.Errorf("%s resolved to an unusable approved surface: %v", candidate.OperationID, err)
		}
		if candidate.Grant.ExposureProfile == "" || !candidate.Grant.UsesOrdinalProgram() {
			t.Errorf("%s resolved to exposure profile %q, which compiles no ordinal program",
				candidate.OperationID, candidate.Grant.ExposureProfile)
		}
	}

	// The selector narrows rather than decides: naming one cell must leave one
	// candidate, and naming a cell the contract does not define must leave none.
	one := all[0]
	coordinates := strings.Split(one.OperationID, "/")
	if len(coordinates) != 4 {
		t.Fatalf("operation id %q is not a protocol coordinate", one.OperationID)
	}
	narrowed, err := contracts.ResolveCandidates(FrozenContractSelectorV3{
		ExperimentID: coordinates[0], WorkloadID: coordinates[1],
		Scale: coordinates[2], Mode: coordinates[3],
	})
	if err != nil {
		t.Fatalf("resolve one frozen cell: %v", err)
	}
	if len(narrowed) != 1 || narrowed[0].OperationID != one.OperationID {
		t.Fatalf("a selector naming %s admitted %d candidate(s)", one.OperationID, len(narrowed))
	}
	if _, err := contracts.ResolveCandidates(FrozenContractSelectorV3{
		ExperimentID: "scale", WorkloadID: "dependency-e2e",
	}); err == nil || !strings.Contains(err.Error(), "private dataset binding") {
		t.Fatalf("a Scale selector did not acquire its independently frozen private binding: %v", err)
	}
}

const scaleResolverTestProduct = "final_v5_exposure_scale"

// scaleBindingForResolverTest is synthetic, non-evidence private material whose
// coordinates and Products are read from the real embedded Contract Index. It
// exists only to exercise the dormant resolver without making the committed
// exposure-scale profile routable.
func scaleBindingForResolverTest(t *testing.T) finalv5binding.Binding {
	t.Helper()
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatalf("load the frozen Contract Index: %v", err)
	}
	cells, err := runtime.ContractWorkloadCells()
	if err != nil {
		t.Fatalf("read the frozen workload cells: %v", err)
	}
	dependency := make(map[string]finalv5binding.DependencyCellBinding, 12)
	for _, cell := range cells {
		if cell.Identity.ExperimentID != "scale" || cell.Identity.WorkloadID != "dependency-e2e" {
			continue
		}
		if len(cell.Products) != 1 || cell.Products[0] != scaleResolverTestProduct {
			t.Fatalf("Scale cell %s requests unexpected Products %v", cell.Identity, cell.Products)
		}
		if _, present := dependency[cell.Identity.Scale]; present {
			continue
		}
		dependency[cell.Identity.Scale] = finalv5binding.DependencyCellBinding{
			Task: finalv5binding.BoundTaskRequest{
				Objective:    "synthetic resolver-only Scale task",
				DataProducts: []string{scaleResolverTestProduct},
				Columns: map[string][]string{
					scaleResolverTestProduct: {"row_id", "category"},
				},
				Scopes: map[string][]string{
					"category": {"alpha", "beta", "gamma", "delta"},
				},
				VisibleRelation:   "reporting.final_v5_result_heavy",
				CompanionRelation: "taskgate_ordinal.final_v5_result_heavy_v1",
			},
			Candidate: finalv5binding.BoundQueryExpectation{
				SQL:          "SELECT row_id FROM final_v5_exposure_scale ORDER BY row_id",
				ExpectedRows: 1, ExpectedColumns: 1,
				ExpectedResultSHA256: strings.Repeat("3", 64),
				DependencyFacts:      1, DependencySetSHA256: strings.Repeat("4", 64),
			},
		}
	}
	if len(dependency) != 12 {
		t.Fatalf("the embedded Scale contract resolved to %d dependency scales", len(dependency))
	}
	return finalv5binding.Binding{
		DatasetSHA256: strings.Repeat("5", 64), CatalogSHA256: strings.Repeat("6", 64),
		FileSHA256: strings.Repeat("7", 64), SectionSHA256: strings.Repeat("a", 64),
		Section: finalv5binding.Section{SchemaVersion: 1,
			Scale: &finalv5binding.ScaleBinding{DependencyE2E: dependency, EnableOutcomeMerkle: true}},
	}
}

func scaleCapableContractsForTest(t *testing.T) deploymentContractsV3 {
	t.Helper()
	contracts := deploymentContractsForTest(t)
	product, found := contracts.catalog.LookupProduct("final_v5_result_heavy")
	if !found {
		t.Fatal("the test Catalog declares no result-heavy Product to clone")
	}
	product.Name = scaleResolverTestProduct
	contracts.catalog.Products = []catalog.Product{product}
	return contracts
}

// The Scale resolver obtains 24 exact public coordinates from the Contract
// Index, crosses them with 12 private candidate cells, and lowers both modes
// through the same production-derived material construction Artifact uses.
func TestTheScaleResolverCrossesEveryFrozenDependencyCell(t *testing.T) {
	contracts := scaleCapableContractsForTest(t)
	binding := scaleBindingForResolverTest(t)
	candidates, err := contracts.resolveScaleCandidatesV3(FrozenContractSelectorV3{
		ExperimentID: "scale", WorkloadID: "dependency-e2e",
	}, binding)
	if err != nil {
		t.Fatalf("resolve every frozen Scale dependency cell: %v", err)
	}
	if len(candidates) != 24 {
		t.Fatalf("the resolver produced %d Scale candidates, want 24", len(candidates))
	}
	modesByScale := make(map[string]map[GatewayPathKind]bool, 12)
	previous := ""
	for _, candidate := range candidates {
		if previous != "" && candidate.OperationID <= previous {
			t.Fatalf("Scale candidates are not stably ordered: %q then %q", previous, candidate.OperationID)
		}
		previous = candidate.OperationID
		coordinates := strings.Split(candidate.OperationID, "/")
		if len(coordinates) != 4 || coordinates[0] != "scale" || coordinates[1] != "dependency-e2e" {
			t.Fatalf("Scale operation %q is not an exact protocol coordinate", candidate.OperationID)
		}
		if modesByScale[coordinates[2]] == nil {
			modesByScale[coordinates[2]] = map[GatewayPathKind]bool{}
		}
		modesByScale[coordinates[2]][candidate.PathKind] = true
		if candidate.ProfileID != scaleDeploymentProfileAlias || candidate.Plan.Product != scaleResolverTestProduct {
			t.Errorf("candidate %s resolved to profile/plan %q/%q",
				candidate.OperationID, candidate.ProfileID, candidate.Plan.Product)
		}
		if err := candidate.Grant.Validate(); err != nil || !candidate.Grant.UsesOrdinalProgram() {
			t.Errorf("candidate %s resolved to an unusable V4/V5 grant: %v", candidate.OperationID, err)
		}
		for _, member := range []string{contracts.runtime.ContractRelease(), contracts.runtime.IndexSHA256(),
			binding.FileSHA256, binding.SectionSHA256, candidate.OperationID} {
			if !strings.Contains(candidate.ContractIdentity, member) {
				t.Errorf("candidate %s contract identity omits %q", candidate.OperationID, member)
			}
		}
	}
	if len(modesByScale) != 12 {
		t.Fatalf("the 24 candidates cover %d scales, want 12", len(modesByScale))
	}
	for scale, modes := range modesByScale {
		if !modes[PathPairedNovel] || !modes[PathSemanticReplay] || len(modes) != 2 {
			t.Errorf("Scale %s resolved to paths %v", scale, modes)
		}
	}

	one, err := contracts.resolveScaleCandidatesV3(FrozenContractSelectorV3{
		ExperimentID: "scale", WorkloadID: "dependency-e2e",
		Scale: "10k-overlap-0", Mode: "semantic_replay",
	}, binding)
	if err != nil {
		t.Fatalf("resolve one exact Scale cell: %v", err)
	}
	if len(one) != 1 || one[0].OperationID != "scale/dependency-e2e/10k-overlap-0/semantic_replay" ||
		one[0].PathKind != PathSemanticReplay {
		t.Fatalf("the exact selector resolved to %+v", one)
	}
}

// Product ownership is split deliberately: the public contract says which
// Product a coordinate requests, while the private binding says which approved
// task was actually frozen. A disagreement is an error, not a dropped cell.
func TestTheScaleResolverRefusesContractBindingProductDrift(t *testing.T) {
	contracts := scaleCapableContractsForTest(t)
	binding := scaleBindingForResolverTest(t)
	scale := *binding.Section.Scale
	scale.DependencyE2E = make(map[string]finalv5binding.DependencyCellBinding, len(binding.Section.Scale.DependencyE2E))
	for name, cell := range binding.Section.Scale.DependencyE2E {
		scale.DependencyE2E[name] = cell
	}
	drifted := scale.DependencyE2E["10k-overlap-0"]
	drifted.Task.DataProducts = []string{"final_v5_result_heavy"}
	scale.DependencyE2E["10k-overlap-0"] = drifted
	binding.Section.Scale = &scale
	if _, err := contracts.scaleDependencyCellsV3(binding); err == nil ||
		!strings.Contains(err.Error(), "Contract Index") {
		t.Fatalf("contract/private Product drift was not refused: %v", err)
	}
}

// The committed tree deliberately has neither a live exposure-scale Product nor
// targeted-run clearance. Resolver wiring must therefore remain dormant even
// though its synthetic positive test above proves all 24 candidates are real.
func TestTheCommittedExposureScaleProfileRemainsFailClosed(t *testing.T) {
	contracts := deploymentContractsForTest(t)
	binding := scaleBindingForResolverTest(t)
	if _, err := contracts.resolveScaleCandidatesV3(FrozenContractSelectorV3{
		ExperimentID: "scale", WorkloadID: "dependency-e2e",
		Scale: "10k-overlap-0", Mode: "novel",
	}, binding); err == nil || !strings.Contains(err.Error(), "declares no Product") {
		t.Fatalf("the committed Catalog unexpectedly materializes exposure-scale: %v", err)
	}
	profiles := deploymentProfilesV3{registryPath: sourceControlledRegistryPath(t)}
	if _, err := profiles.Resolve(scaleDeploymentProfileAlias); err == nil ||
		!strings.Contains(err.Error(), "not eligible for a targeted run") {
		t.Fatalf("the committed exposure-scale profile unexpectedly resolved: %v", err)
	}
}

func TestTheScaleBindingIdentityIsFrozenBeforeResolution(t *testing.T) {
	binding := scaleBindingForResolverTest(t)
	if err := requireDeploymentBindingIdentityV3(
		binding, binding.FileSHA256, binding.SectionSHA256); err != nil {
		t.Fatalf("the exact frozen binding identity was refused: %v", err)
	}
	if err := requireDeploymentBindingIdentityV3(
		binding, strings.Repeat("9", 64), binding.SectionSHA256); err == nil {
		t.Fatal("a changed private binding file was accepted")
	}
	if err := requireDeploymentBindingIdentityV3(
		binding, binding.FileSHA256, strings.ToUpper(binding.SectionSHA256)); err == nil {
		t.Fatal("a non-canonical private binding identity was accepted")
	}
}

func provSQLBindingForResolverTest(t *testing.T) finalv5binding.Binding {
	t.Helper()
	task := finalv5binding.BoundTaskRequest{
		Objective:    "synthetic resolver-only ProvSQL task",
		DataProducts: []string{"provsql_orders", "provsql_lineitem", "provsql_nonce"},
		Columns: map[string][]string{
			"provsql_orders":   {"orderkey", "status", "partition_key"},
			"provsql_lineitem": {"orderkey", "linenumber", "extendedprice", "partition_key"},
			"provsql_nonce":    {"nonce_id", "partition_key"},
		},
		Scopes:            map[string][]string{"partition_key": {"1"}},
		VisibleRelation:   "reporting.provsql_orders",
		CompanionRelation: "taskgate_ordinal.provsql_orders_v1",
	}
	provSQL := &finalv5binding.ProvSQLBinding{
		FixtureVersion: provsqlfixture.Version, FixtureSQLSHA256: provsqlfixture.FixtureSQLSHA256(),
		EnableSQLSHA256: provsqlfixture.EnableSQLSHA256(), DatasetSHA256: provsqlfixture.ExpectedDatasetSHA256(),
		DatasetProbeSQLSHA256:         provsqlfixture.DatasetProbeSQLSHA256(),
		BusinessDatasetProbeSQLSHA256: provsqlfixture.BusinessDatasetProbeSQLSHA256(),
		Task:                          task, TaskGate: map[string]finalv5binding.BoundQueryExpectation{},
	}
	for _, scale := range []string{"1k", "10k", "45k"} {
		rows, err := provsqlfixture.ExpectedResultRows(scale)
		if err != nil {
			t.Fatal(err)
		}
		resultSHA256, err := CanonicalResultHash(rows)
		if err != nil {
			t.Fatal(err)
		}
		for _, phase := range []struct {
			warmup bool
			count  int
		}{{warmup: true, count: 5}, {warmup: false, count: 30}} {
			for iteration := 1; iteration <= phase.count; iteration++ {
				nonce, err := provsqlfixture.Nonce(scale, 1, iteration, phase.warmup)
				if err != nil {
					t.Fatal(err)
				}
				logical, err := provsqlfixture.LogicalSQL(scale, nonce)
				if err != nil {
					t.Fatal(err)
				}
				key := finalv5binding.ProvSQLBindingKey(scale, nonce)
				provSQL.TaskGate[key] = finalv5binding.BoundQueryExpectation{
					SQL: logical, ExpectedRows: provsqlfixture.ExpectedRows,
					ExpectedColumns: provsqlfixture.ExpectedColumns, ExpectedResultSHA256: resultSHA256,
					DependencyFacts: int64(len(provSQL.TaskGate) + 1), DependencySetSHA256: sha256Hex([]byte(key)),
					ExpectedVisibleCalls: 1, ExpectedCompanionCalls: 1,
				}
			}
		}
	}
	return finalv5binding.Binding{
		DatasetSHA256: strings.Repeat("5", 64), CatalogSHA256: strings.Repeat("6", 64),
		FileSHA256: strings.Repeat("7", 64), SectionSHA256: strings.Repeat("a", 64),
		Section: finalv5binding.Section{SchemaVersion: 1, ProvSQL: provSQL},
	}
}

func provSQLCapableContractsForTest(t *testing.T) deploymentContractsV3 {
	t.Helper()
	contracts := deploymentContractsForTest(t)
	catalogPath := filepath.Join(repositoryRootForDeployment(t), "config", "profiles", "provsql-nonce-join.catalog.yaml")
	liveCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		t.Fatalf("load the ProvSQL profile Catalog: %v", err)
	}
	digest, err := FileSHA256(catalogPath)
	if err != nil {
		t.Fatalf("digest the ProvSQL profile Catalog: %v", err)
	}
	contracts.catalog = liveCatalog
	contracts.live.CatalogPath = catalogPath
	contracts.live.CatalogSHA256 = digest
	return contracts
}

// One public ProvSQL TaskGate cell intentionally covers 35 nonce-specific
// statements. This synthetic, non-evidence binding proves the crossing
// algorithm over a complete validated matrix; only bindingSource.load proves
// the deployment's pinned private bytes. The opaque binding-key hint narrows
// the descriptor set to exactly one private variant without supplying plan or
// SQL authority; executable candidate construction remains on the production
// lowering path tested separately below.
func TestTheProvSQLResolverCrossesACompleteValidatedMatrix(t *testing.T) {
	contracts := provSQLCapableContractsForTest(t)
	binding := provSQLBindingForResolverTest(t)
	variants, err := contracts.provSQLVariantsV3(FrozenContractSelectorV3{
		ExperimentID: "provsql", WorkloadID: "nonce-join-group", Mode: "taskgate",
	}, binding)
	if err != nil {
		t.Fatalf("cross every frozen ProvSQL TaskGate variant: %v", err)
	}
	if len(variants) != 105 {
		t.Fatalf("the resolver crossed %d ProvSQL variants, want 105", len(variants))
	}
	perScale := map[string]int{}
	previous := ""
	for _, variant := range variants {
		perScale[variant.Cell.Scale]++
		operationID, contractIdentity, err := provSQLOperationIdentityV3(
			contracts.runtime, binding, variant.Cell, variant.BindingKey, variant.Expected.SQL)
		if err != nil {
			t.Fatalf("name ProvSQL variant %s/%s: %v", variant.Cell, variant.BindingKey, err)
		}
		stableIdentity := operationID + "#" + variant.BindingKey
		if previous != "" && stableIdentity <= previous {
			t.Fatalf("ProvSQL candidates are not stably ordered: %q then %q", previous, stableIdentity)
		}
		previous = stableIdentity
		for _, member := range []string{contracts.runtime.ContractRelease(), contracts.runtime.IndexSHA256(),
			binding.FileSHA256, binding.SectionSHA256, "binding-key=" + variant.BindingKey,
			"binding-query=" + sha256Hex([]byte(variant.Expected.SQL)), operationID} {
			if !strings.Contains(contractIdentity, member) {
				t.Errorf("candidate %s/%s contract identity omits %q",
					operationID, variant.BindingKey, member)
			}
		}
	}
	for _, scale := range []string{"1k", "10k", "45k"} {
		if perScale[scale] != 35 {
			t.Fatalf("the resolver crossed %d ProvSQL variants at %s, want 35", perScale[scale], scale)
		}
	}

	selector := FrozenContractSelectorV3{ExperimentID: "provsql", WorkloadID: "nonce-join-group",
		Scale: "1k", Mode: "taskgate", BindingKey: finalv5binding.ProvSQLBindingKey("1k", 101)}
	one, err := contracts.provSQLVariantsV3(selector, binding)
	if err != nil {
		t.Fatalf("select one exact ProvSQL nonce variant: %v", err)
	}
	if len(one) != 1 || one[0].Cell.String() != "provsql/nonce-join-group/1k/taskgate" ||
		one[0].BindingKey != selector.BindingKey {
		t.Fatalf("the exact selector resolved to %+v", one)
	}

	withoutKey, err := contracts.provSQLVariantsV3(FrozenContractSelectorV3{
		ExperimentID: "provsql", WorkloadID: "nonce-join-group", Scale: "1k", Mode: "taskgate",
	}, binding)
	if err != nil || len(withoutKey) != 35 {
		t.Fatalf("the public cell resolved to %d variants, want 35: %v", len(withoutKey), err)
	}

	for name, selector := range map[string]FrozenContractSelectorV3{
		"wrong key":  {ExperimentID: "provsql", WorkloadID: "nonce-join-group", Scale: "1k", Mode: "taskgate", BindingKey: "1k/999"},
		"direct arm": {ExperimentID: "provsql", WorkloadID: "nonce-join-group", Scale: "1k", Mode: "direct", BindingKey: finalv5binding.ProvSQLBindingKey("1k", 101)},
		"native arm": {ExperimentID: "provsql", WorkloadID: "nonce-join-group", Scale: "1k", Mode: "provsql", BindingKey: finalv5binding.ProvSQLBindingKey("1k", 101)},
	} {
		resolved, err := contracts.provSQLVariantsV3(selector, binding)
		if err != nil {
			t.Fatalf("%s selector errored: %v", name, err)
		}
		if len(resolved) != 0 {
			t.Fatalf("%s selector resolved %d candidates", name, len(resolved))
		}
	}
}

// The current SQL profile cannot materialize the exact frozen ProvSQL
// operation because multi-product ORDER BY is unsupported. The resolver must
// fail at the shared production lowering boundary, never strip the clause,
// transcribe a plan, or substitute a synthetic query to make dormant wiring
// look runnable.
func TestTheCommittedProvSQLResolverFailsClosedOnUnsupportedMultiProductOrder(t *testing.T) {
	contracts := provSQLCapableContractsForTest(t)
	binding := provSQLBindingForResolverTest(t)
	selector := FrozenContractSelectorV3{ExperimentID: "provsql", WorkloadID: "nonce-join-group",
		Scale: "1k", Mode: "taskgate", BindingKey: finalv5binding.ProvSQLBindingKey("1k", 101)}
	_, err := contracts.resolveProvSQLCandidatesV3(selector, binding)
	var loweringError *sqllowering.Error
	if !errors.As(err, &loweringError) || loweringError.Code != sqllowering.CodeNotLowerable ||
		loweringError.Reason != "PAGINATION_UNSUPPORTED" {
		t.Fatalf("the dormant ProvSQL resolver bypassed the production multi-product lowering boundary: %v", err)
	}
}

func TestTheProvSQLResolverRefusesPrivateMatrixDrift(t *testing.T) {
	contracts := provSQLCapableContractsForTest(t)
	binding := provSQLBindingForResolverTest(t)
	delete(binding.Section.ProvSQL.TaskGate, finalv5binding.ProvSQLBindingKey("1k", 101))
	if _, err := contracts.resolveProvSQLCandidatesV3(FrozenContractSelectorV3{
		ExperimentID: "provsql", WorkloadID: "nonce-join-group", Mode: "taskgate",
	}, binding); err == nil || !strings.Contains(err.Error(), "private ProvSQL binding") {
		t.Fatalf("incomplete private ProvSQL matrix was not refused: %v", err)
	}
}

// The whole deployment side, assembled: the contract resolver, the profile
// resolver and the retained qualification together produce a pre-registered
// classification.
//
// This is the test that says the five resolvers are wired to material that
// works. OpenObserverWindowV3 prepares the operation from the resolved plan and
// grant, against the resolved Catalog and the published artifacts, derives the
// control plan from the resolved footprint, and builds the classifier manifest
// -- so a digest coming back means every one of those steps ran on real
// material rather than on a value a test wrote down.
func TestTheDeploymentResolversPreRegisterAClassification(t *testing.T) {
	verifier, err := fixture.Verifier()
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	directory := retainedQualificationDirectory(t)
	finalizer, err := openRuntimeFinalizerV3(verifier,
		deploymentContractsForTest(t),
		deploymentProfilesForTest(t),
		retainedQualificationV3{documentPath: filepath.Join(directory, "attestation-footprint-v2.json")},
		retainedPostgreSQLIdentityV3{documentPath: filepath.Join(directory, "postgresql-identity.json")},
		// The Control Store is the one collaborator this test cannot run: it needs
		// a settled request in a running deployment. Pre-registration reaches none
		// of it -- it happens before the request exists.
		stubControl{state: requestSettlementStateV3{WroteExecutionBindingRow: true}})
	if err != nil {
		t.Fatalf("open the finalizer: %v", err)
	}
	committed, err := finalizer.OpenObserverWindowV3(context.Background(), FrozenContractSelectorV3{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
		WorkloadID:   finalv5contracts.ArtifactWorkloadID,
		Scale:        "100x4", Mode: "novel",
	}, ObserverAttemptV3{TaskID: "task-deployment-preregistration", RequestID: "request-deployment-preregistration"})
	if err != nil {
		t.Fatalf("pre-register the classification from deployment material: %v", err)
	}
	if !validSHA256(committed.ClassifierManifestSHA256) || !validSHA256(committed.ClassifierBindingSHA256) {
		t.Fatalf("the pre-registered classification is %+v", committed)
	}
	if err := committed.Operation.Validate(); err != nil {
		t.Fatalf("the pre-registered operation identity: %v", err)
	}
	if err := committed.Plan.Validate(); err != nil {
		t.Fatalf("the pre-registered control plan: %v", err)
	}

	// The finalizer also issues the random window identity needed to make the
	// deployment material a runnable observer invocation for this one attempt.
	if err := (ObserverInvocationV3{Phase: "before", ObserverWindowID: committed.ObserverWindowID,
		ClassifierManifestSHA256: committed.ClassifierManifestSHA256}).Validate(); err != nil {
		t.Fatalf("the deployment material does not produce a runnable observer invocation: %v", err)
	}
}

// A frozen operation is named from the contract release and the Index digest, so
// the same cell coordinate re-frozen under a different release is a different
// contract. Without that a target rendered from one release would classify for
// the other, and no class count would notice.
func TestAFrozenOperationIsNamedByItsContractRelease(t *testing.T) {
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		t.Fatalf("load the frozen Contract Index: %v", err)
	}
	cell := finalv5contracts.CellIdentity{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
		WorkloadID:   finalv5contracts.ArtifactWorkloadID, Scale: "100x4", Mode: "novel",
	}
	operation, err := ArtifactOperationV3(runtime, cell)
	if err != nil {
		t.Fatalf("name the frozen operation: %v", err)
	}
	if operation.OperationID != cell.String() {
		t.Fatalf("the operation is named %q, the protocol coordinate is %q",
			operation.OperationID, cell.String())
	}
	for _, member := range []string{runtime.ContractRelease(), runtime.IndexSHA256(), cell.String()} {
		if !strings.Contains(operation.ContractIdentity, member) {
			t.Errorf("the contract identity %q does not carry %q", operation.ContractIdentity, member)
		}
	}
	if _, err := ArtifactOperationV3(runtime, finalv5contracts.CellIdentity{
		ExperimentID: finalv5contracts.ArtifactExperimentID,
	}); err == nil {
		t.Fatal("an incomplete protocol coordinate was named as a frozen operation")
	}
	if _, err := ArtifactOperationV3(nil, cell); err == nil {
		t.Fatal("a frozen operation was named without the Contract Index")
	}
}
