package experiment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"taskbound.local/agent-data-gateway/evaluation/finalv5contracts"
	"taskbound.local/agent-data-gateway/evaluation/internal/finalv5profile"
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/domain"
	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
	"taskbound.local/agent-data-gateway/internal/sqllowering"
)

// This file is the deployment half of v3 acceptance: the five places a running
// Final-V5 deployment keeps the finalizer's trusted material, and the one
// constructor that wires them.
//
// runtime_finalizer_v3.go deliberately shipped no resolver -- "an artifact that
// ships a resolver nothing has ever executed is worse evidence than one that
// ships none" -- and left them to the first cutover, which is the first caller
// that needs real ones. This is that cutover.
//
// # Why the constructor names nothing
//
// OpenDeploymentFinalizerV3 takes a context and nothing else. Every input is
// read from the environment the operator started this deployment with. That is
// not brevity: a parameter naming a Catalog, a qualification document or a
// contract cell would let its caller -- and the only production caller is the
// Adapter -- choose which archive its own claim is checked against. Keeping
// trusted material out of FinalizationRequestV3 stops a caller supplying the
// answer; this is the same rule one level up, where a caller would otherwise
// choose the standard.
//
// # Why an opened finalizer owns no connection
//
// The Control Store reader dials per read, and the dataset probe is taken once
// while the constructor still holds a context. So there is no pool to close, no
// Close for a caller to forget, and no handle onto the running deployment living
// longer than the read that needed it. The cost is one connection per finalized
// sample, which is not a measured quantity: finalization happens after the
// observer window has closed.
//
// # What each resolver is, and is not
//
//	contracts  -- the frozen Contract Index this binary embeds, bound to the
//	              Catalog the Gateway is running and lowered through the same
//	              production lowering the Gateway used;
//	profiles   -- the source-controlled profile registry and the artifact
//	              directory the deployment mounted;
//	footprints -- one retained qualification document, self-checked against its
//	              own recorded digest;
//	runtime    -- the retained PostgreSQL runtime identity, bound to the running
//	              server by the observer rather than by this reader;
//	control    -- the Control Store's own account of one request.
//
// None of them reads the Adapter, the Sample, or the receipt.

// The environment this deployment describes itself with. They are named here
// once so that the set of things an operator has to supply is readable in one
// place rather than scattered across five resolvers.
const (
	deploymentGatewayURLEnv         = "TASKGATE_FINAL_V5_GATEWAY_URL"
	deploymentLiveCatalogEnv        = "TASKGATE_FINAL_V5_CATALOG"
	deploymentBusinessDSNEnv        = "TASKGATE_FINAL_V5_BUSINESS_DSN"
	deploymentControlDSNEnv         = "TASKGATE_FINAL_V5_CONTROL_DSN"
	deploymentProfileRegistryEnv    = "TASKGATE_FINAL_V5_PROFILE_REGISTRY"
	deploymentProfileArtifactDirEnv = "TASKGATE_PROFILE_ARTIFACT_DIR"
	deploymentQualificationEnv      = "TASKGATE_FINAL_V5_ATTESTATION_QUALIFICATION"
	deploymentPostgreSQLIdentityEnv = "TASKGATE_FINAL_V5_POSTGRESQL_IDENTITY"
	deploymentRepositoryRootEnv     = "TASKGATE_FINAL_V5_REPO_ROOT"
)

// artifactDeploymentProfileAlias is the activated deployment profile the frozen
// Artifact cells run under.
//
// It is the registry ALIAS rather than the profile ID, because the ID is a
// closure digest: it moves whenever the closure is recomputed, while the frozen
// contract that names the profile does not. Resolving by alias is what lets a
// frozen contract keep naming the same deployment profile across registry
// regenerations; what stops the alias from being loose is that the resolver then
// requires the profile's Catalog to be byte-identical to the one the Gateway is
// actually serving.
const artifactDeploymentProfileAlias = "result-heavy"

// OpenDeploymentFinalizerV3 opens the finalizer this deployment's environment
// describes.
//
// It is the only exported constructor of a RuntimeFinalizerV3, and it selects no
// content: see the file comment for why that is the whole point rather than a
// simplification.
func OpenDeploymentFinalizerV3(ctx context.Context) (*RuntimeFinalizerV3, error) {
	gatewayURL, err := requiredDeploymentValue(deploymentGatewayURLEnv)
	if err != nil {
		return nil, err
	}
	// The keyring first. The keys decide which signatures the finalizer will
	// accept, so they are trusted material like any other and the finalizer
	// fetches them itself rather than being handed a verifier.
	verifier, err := gatewayKeyringVerifierV3(ctx, gatewayURL)
	if err != nil {
		return nil, err
	}
	contracts, err := openDeploymentContractsV3(ctx)
	if err != nil {
		return nil, err
	}
	profiles, err := openDeploymentProfilesV3(contracts.live.CatalogSHA256)
	if err != nil {
		return nil, err
	}
	qualification, err := requiredDeploymentValue(deploymentQualificationEnv)
	if err != nil {
		return nil, err
	}
	identity, err := requiredDeploymentValue(deploymentPostgreSQLIdentityEnv)
	if err != nil {
		return nil, err
	}
	controlDSN, err := requiredDeploymentValue(deploymentControlDSNEnv)
	if err != nil {
		return nil, err
	}
	return openRuntimeFinalizerV3(verifier, contracts, profiles,
		retainedQualificationV3{documentPath: qualification},
		retainedPostgreSQLIdentityV3{documentPath: identity},
		controlStoreEvidenceV3{dsn: controlDSN})
}

func requiredDeploymentValue(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s must name where this deployment keeps the finalizer's material; "+
			"without it that material could only come from the party being checked", name)
	}
	return value, nil
}

// ------------------------------------------------------------- the receipt keys

// gatewayKeyringVerifierV3 fetches the Gateway's published verification keys.
//
// The Gateway publishes the PUBLIC half, so fetching it from the Gateway is not
// asking the measured party whether its own signature is valid: the bundle
// cannot make a forged receipt verify, it can only fail to verify a genuine one.
// What it does decide is which key identities are admissible at all, which is
// why the finalizer fetches it rather than accepting one.
func gatewayKeyringVerifierV3(ctx context.Context, gatewayURL string) (ReceiptVerifierV3, error) {
	const keyringPath = "/.well-known/taskgate/query-receipt-keyring.json"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(gatewayURL, "/")+keyringPath, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch the Gateway receipt keyring: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the Gateway receipt keyring returned %d", response.StatusCode)
	}
	// Bounded, because the keyring is fetched over the network from a service
	// this process does not control. A megabyte is far more than a key bundle and
	// far less than a document worth buffering by accident.
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read the Gateway receipt keyring: %w", err)
	}
	var bundle queryreceipt.PublicKeyBundleV1
	if err := json.Unmarshal(payload, &bundle); err != nil {
		return nil, fmt.Errorf("decode the Gateway receipt keyring: %w", err)
	}
	verifier, err := bundle.Verifier()
	if err != nil {
		return nil, fmt.Errorf("the Gateway receipt keyring yields no verifier: %w", err)
	}
	return verifier, nil
}

// ---------------------------------------------------------- the frozen contracts

// deploymentContractsV3 resolves frozen Artifact operations against the Catalog
// the Gateway is actually running.
//
// The Contract Index is embedded in this binary and its digests are revalidated
// by LoadRuntime, so the contract half needs no deployment at all. What does is
// the binding: which Product, which Publication, which approved projection and
// which budget profile the live Catalog gives that contract. A contract that
// cannot be bound to this deployment is refused rather than resolved against the
// contract's own expectations, because preparing from those would reproduce a
// statement no Gateway here could have executed.
type deploymentContractsV3 struct {
	runtime *finalv5contracts.Runtime
	live    finalv5contracts.LiveDeployment
	// catalog is the live Catalog, loaded once. It is the same file
	// live.CatalogPath names, kept parsed because every candidate reads its
	// Product, its scopes and its task policy.
	catalog *catalog.Catalog
}

func openDeploymentContractsV3(ctx context.Context) (deploymentContractsV3, error) {
	var contracts deploymentContractsV3
	runtime, err := finalv5contracts.LoadRuntime()
	if err != nil {
		return contracts, fmt.Errorf("load the frozen Contract Index: %w", err)
	}
	catalogPath, err := requiredDeploymentValue(deploymentLiveCatalogEnv)
	if err != nil {
		return contracts, err
	}
	catalogDigest, err := FileSHA256(catalogPath)
	if err != nil {
		return contracts, fmt.Errorf("digest the live Profile Catalog: %w", err)
	}
	liveCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		return contracts, fmt.Errorf("load the live Profile Catalog: %w", err)
	}
	businessDSN, err := requiredDeploymentValue(deploymentBusinessDSNEnv)
	if err != nil {
		return contracts, err
	}
	// The dataset fingerprint is read once, here, because it is a property of the
	// deployment rather than of a cell: BindDeployment records it in the binding
	// and nothing downstream of it depends on when it was taken. Reading it in
	// the constructor is also what keeps ResolveCandidates -- which has no
	// context, by interface -- from having to open a database.
	probe, err := datasetProbeDigestV3(ctx, runtime, businessDSN)
	if err != nil {
		return contracts, err
	}
	return deploymentContractsV3{
		runtime: runtime,
		live: finalv5contracts.LiveDeployment{
			CatalogPath: catalogPath, CatalogSHA256: catalogDigest, DatasetProbeSHA256: probe,
		},
		catalog: liveCatalog,
	}, nil
}

// datasetProbeDigestV3 runs the contract-indexed dataset probe against Business
// PostgreSQL.
//
// It is a deployment observation, not a logical oracle: it says which dataset is
// installed, and BindDeployment refuses a binding that carries no such
// observation at all. The finalizer takes it for itself rather than from the
// Adapter for the ordinary reason -- a probe digest supplied by the measured
// party would make "this ran against the frozen dataset" its own claim.
func datasetProbeDigestV3(ctx context.Context, runtime *finalv5contracts.Runtime, dsn string) (string, error) {
	probe, err := runtime.DatasetProbeSQL()
	if err != nil {
		return "", err
	}
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect Business PostgreSQL for the dataset probe: %w", err)
	}
	defer connection.Close(context.Background())
	var fingerprint string
	if err := connection.QueryRow(ctx, probe).Scan(&fingerprint); err != nil {
		return "", fmt.Errorf("run the contract dataset probe: %w", err)
	}
	return sha256Hex([]byte(fingerprint)), nil
}

// ResolveCandidates returns every frozen Artifact operation the selector admits.
//
// An empty selector member is a wildcard, because the selector is a hint whose
// only job is to narrow the search: finalization identifies the operation by
// preparing the candidates and keeping the one the Gateway's signature agrees
// with, so admitting too many costs work rather than correctness.
//
// A cell the selector admits but the deployment cannot bind is an ERROR rather
// than a skipped candidate. Silently dropping it would report "no frozen
// contract operation matches" for a deployment that is running the wrong
// Catalog, which is a different fault and a much harder one to find.
func (contracts deploymentContractsV3) ResolveCandidates(
	selector FrozenContractSelectorV3) ([]frozenOperationCandidateV3, error) {
	cells, err := contracts.runtime.ArtifactCells()
	if err != nil {
		return nil, fmt.Errorf("read the frozen Artifact cells: %w", err)
	}
	var candidates []frozenOperationCandidateV3
	for _, cell := range cells {
		if !selectorAdmitsCellV3(selector, cell.Identity) {
			continue
		}
		candidate, candidateErr := contracts.candidateFor(cell)
		if candidateErr != nil {
			return nil, fmt.Errorf("bind frozen Artifact cell %s to this deployment: %w",
				cell.Identity, candidateErr)
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func selectorAdmitsCellV3(selector FrozenContractSelectorV3, identity finalv5contracts.CellIdentity) bool {
	for _, coordinate := range [][2]string{
		{selector.ExperimentID, identity.ExperimentID},
		{selector.WorkloadID, identity.WorkloadID},
		{selector.Scale, identity.Scale},
		{selector.Mode, identity.Mode},
	} {
		if coordinate[0] != "" && coordinate[0] != coordinate[1] {
			return false
		}
	}
	return true
}

// candidateFor turns one frozen cell into the plan and approved surface the
// finalizer prepares from.
//
// The plan is LOWERED rather than transcribed. The contract holds the rendered
// SQL the Adapter hands to the public MCP tool, and the Gateway turns that text
// into a canonical QueryPlan through internal/sqllowering; a finalizer that
// wrote the plan out by hand would be asserting the result of a step it is
// supposed to be reproducing. Calling the same lowering against the same Catalog
// Product is what makes the plan an independent acquisition rather than a second
// opinion -- and QueryProductFromCatalog is the same construction the Gateway
// lowers with, so the two cannot differ in a member.
func (contracts deploymentContractsV3) candidateFor(
	cell finalv5contracts.ArtifactCell) (frozenOperationCandidateV3, error) {
	var candidate frozenOperationCandidateV3
	binding, err := contracts.runtime.BindDeployment(cell, contracts.live)
	if err != nil {
		return candidate, err
	}
	query, err := contracts.runtime.QueryContract(cell)
	if err != nil {
		return candidate, err
	}
	product, found := contracts.catalog.LookupProduct(binding.ProductID)
	if !found {
		return candidate, fmt.Errorf("the live Catalog declares no Product %q", binding.ProductID)
	}
	approved := make(map[string]struct{}, len(binding.Columns))
	for _, column := range binding.Columns {
		approved[column] = struct{}{}
	}
	lowered, err := sqllowering.Lower(query.BDG.SQL, map[string]queryplan.Product{
		binding.ProductID: physicalquery.QueryProductFromCatalog(product, approved),
	})
	if err != nil {
		return candidate, fmt.Errorf("lower the frozen BDG statement: %w", err)
	}
	grant, err := contracts.grantFor(binding)
	if err != nil {
		return candidate, err
	}
	operation, err := ArtifactOperationV3(contracts.runtime, cell.Identity)
	if err != nil {
		return candidate, err
	}
	return frozenOperationCandidateV3{
		OperationID: operation.OperationID, ContractIdentity: operation.ContractIdentity,
		ProfileID: artifactDeploymentProfileAlias,
		// Every Artifact cell is an Exposure V4/V5 novel execution: one preflight
		// attestation, one QueryPairStream transaction, a visible statement and
		// its provenance companion. It is a contract fact and is committed before
		// the operation runs; a cell that then took a different path fails
		// finalization, which is the correct outcome.
		PathKind: PathPairedNovel,
		Plan:     lowered.Plan, Grant: grant,
	}, nil
}

// grantFor maps the deployment binding onto the authorization preparation reads.
//
// It is the finalizer's counterpart of internal/gateway.preparationGrant, over
// different sources: the Gateway maps the Control Store's stored TaskGrant, this
// maps the frozen contract's binding to the live Catalog. Both hand the same six
// values to the same type, which is the only reason the two preparations
// agreeing means anything.
//
// The scope is the Catalog's complete enumerated domain for every scope the
// Product declares, sorted, exactly as the Gateway normalizes an incoming task's
// scopes before sealing them into the authorization manifest. Sorting matters
// because the grant digest is over canonical JSON and RFC 8785 preserves array
// order: two orderings of one domain would be two authorizations.
func (contracts deploymentContractsV3) grantFor(
	binding finalv5contracts.Binding) (physicalquery.Grant, error) {
	policy, err := contracts.catalog.ResolveTaskPolicy([]string{binding.ProductID})
	if err != nil {
		return physicalquery.Grant{}, fmt.Errorf("resolve the live task policy for %q: %w",
			binding.ProductID, err)
	}
	scope := make(map[string][]string, len(binding.Scopes))
	for name, values := range binding.Scopes {
		sorted := append([]string(nil), values...)
		sort.Strings(sorted)
		scope[name] = sorted
	}
	encoded, err := json.Marshal(scope)
	if err != nil {
		return physicalquery.Grant{}, fmt.Errorf("encode the mandatory scope: %w", err)
	}
	grant := physicalquery.Grant{
		ApprovedProducts: []string{binding.ProductID},
		ApprovedColumns:  map[string][]string{binding.ProductID: append([]string(nil), binding.Columns...)},
		MandatoryScope:   encoded,
		ExposureProfile:  policy.Budget.ExposureProfileVersion,
		PredicateLimits:  predicateLimitsFromBudgetV3(policy.Budget),
	}
	if err := grant.Validate(); err != nil {
		return physicalquery.Grant{}, fmt.Errorf("the frozen contract's approved surface does not "+
			"settle a preparation: %w", err)
	}
	return grant, nil
}

// predicateLimitsFromBudgetV3 is the V5 predicate ceiling the approved budget
// profile carries. A profile that accounts no predicate footprint carries none,
// and the zero value is what preparation reads as "this profile prepares none".
func predicateLimitsFromBudgetV3(budget domain.Budget) queryplan.PredicateLimits {
	if budget.PredicateFootprint == nil {
		return queryplan.PredicateLimits{}
	}
	return queryplan.PredicateLimits{
		MaxRawLiteralsPerQuery:   int(budget.PredicateFootprint.MaxRawLiteralsPerQuery),
		MaxUniqueAtomsPerQuery:   int(budget.PredicateFootprint.MaxUniqueAtomsPerQuery),
		MaxAtomPayloadBytes:      int(budget.PredicateFootprint.MaxAtomPayloadBytes),
		MaxTotalAtomPayloadBytes: int(budget.PredicateFootprint.MaxTotalAtomPayloadBytes),
	}
}

// FrozenArtifactOperationV3 names one frozen Artifact cell.
//
// The two identities answer different questions and are both needed. OperationID
// says which measured operation this is within a run; ContractIdentity says which
// frozen contract its statements were rendered from, and is what every target
// entry is derived from, so a target belonging to another contract release
// cannot classify for this one.
type FrozenArtifactOperationV3 struct {
	OperationID      string
	ContractIdentity string
}

// ArtifactOperationV3 is the single construction of those two identities.
//
// It is exported because both sides need it and they must not spell it
// differently: the finalizer derives it from the frozen contract, and the
// Adapter has to name the same operation in the evidence it submits. Two
// spellings would fail finalization on a naming difference and report it as an
// operation mismatch, which is the least informative way for a string to be
// wrong.
//
// The contract release and the Contract Index digest are both in the contract
// identity because the cell coordinate alone is stable across releases: the same
// artifact/result-heavy/100x4/novel cell can be re-frozen with different query
// bytes, and a target rendered from one release must not classify for the other.
func ArtifactOperationV3(runtime *finalv5contracts.Runtime,
	cell finalv5contracts.CellIdentity) (FrozenArtifactOperationV3, error) {
	if runtime == nil {
		return FrozenArtifactOperationV3{},
			errors.New("naming a frozen Artifact operation requires the loaded Contract Index")
	}
	release, index := runtime.ContractRelease(), runtime.IndexSHA256()
	if strings.TrimSpace(release) == "" || !validSHA256(index) {
		return FrozenArtifactOperationV3{},
			errors.New("the loaded Contract Index carries no release identity")
	}
	if strings.TrimSpace(cell.ExperimentID) == "" || strings.TrimSpace(cell.WorkloadID) == "" ||
		strings.TrimSpace(cell.Scale) == "" || strings.TrimSpace(cell.Mode) == "" {
		return FrozenArtifactOperationV3{},
			fmt.Errorf("cell %s is not a complete protocol coordinate", cell)
	}
	return FrozenArtifactOperationV3{
		OperationID:      cell.String(),
		ContractIdentity: release + ":" + index + ":" + cell.String(),
	}, nil
}

// --------------------------------------------------------- the activated profile

// deploymentProfilesV3 resolves an activated deployment profile to the immutable
// material it was activated with.
//
// It is separate from the contract resolver on purpose, and the separation is
// the paper's: the contract says what runs, the activated profile says which
// Catalog the result is attested against. Collapsing them would let a contract
// choose its own standard of proof.
type deploymentProfilesV3 struct {
	registryPath   string
	repositoryRoot string
	artifactDir    string
	// servedCatalogSHA256 is the digest of the Catalog file this deployment is
	// running. The registry's copy of a profile Catalog is only usable as
	// evidence about this run if the two are the same bytes.
	servedCatalogSHA256 string
}

func openDeploymentProfilesV3(servedCatalogSHA256 string) (deploymentProfilesV3, error) {
	var profiles deploymentProfilesV3
	registryPath, err := requiredDeploymentValue(deploymentProfileRegistryEnv)
	if err != nil {
		return profiles, err
	}
	artifactDir, err := requiredDeploymentValue(deploymentProfileArtifactDirEnv)
	if err != nil {
		return profiles, err
	}
	root, err := requiredDeploymentValue(deploymentRepositoryRootEnv)
	if err != nil {
		return profiles, err
	}
	return deploymentProfilesV3{registryPath: registryPath, repositoryRoot: root,
		artifactDir: artifactDir, servedCatalogSHA256: servedCatalogSHA256}, nil
}

// Resolve reads the registry, and refuses a profile this run may not be measured
// against.
//
// Three separate refusals, because they fail for three different reasons. A
// profile the registry has not cleared for a targeted run must not be used to
// bootstrap the readiness it depends on; a profile with no recorded live
// activation smoke has never been shown to activate at all; and a profile whose
// Catalog is not the one the Gateway is serving describes a different
// deployment.
//
// The Catalog handed back is the REGISTRY's copy, resolved under the repository
// root, and it is required to be byte-identical to the served one. That is
// deliberate rather than incidental: the finalizer then reads the same immutable
// material through its own acquisition, which is the property agreement between
// the two preparations rests on.
func (profiles deploymentProfilesV3) Resolve(profileID string) (profileMaterialV3, error) {
	var material profileMaterialV3
	payload, err := os.ReadFile(profiles.registryPath)
	if err != nil {
		return material, fmt.Errorf("read the profile registry: %w", err)
	}
	var registry finalv5profile.Registry
	if err := json.Unmarshal(payload, &registry); err != nil {
		return material, fmt.Errorf("decode the profile registry: %w", err)
	}
	var profile finalv5profile.Profile
	found := false
	for _, candidate := range registry.Profiles {
		if candidate.Alias == profileID || candidate.ID == profileID {
			profile, found = candidate, true
			break
		}
	}
	if !found {
		return material, fmt.Errorf("the profile registry declares no profile %q", profileID)
	}
	if !profile.TargetedRunEligible {
		return material, fmt.Errorf("profile %s is not eligible for a targeted run: %+v",
			profile.Alias, profile.Status)
	}
	if !profile.Status.ActivationSupported {
		return material, fmt.Errorf("profile %s has no recorded live activation smoke", profile.Alias)
	}
	catalogPath := profile.CatalogPath
	if !filepath.IsAbs(catalogPath) {
		catalogPath = filepath.Join(profiles.repositoryRoot, catalogPath)
	}
	digest, err := FileSHA256(catalogPath)
	if err != nil {
		return material, fmt.Errorf("digest the profile Catalog of %s: %w", profile.Alias, err)
	}
	if digest != profile.CatalogSHA256 {
		return material, fmt.Errorf("profile %s pins Catalog %s but its file digests to %s",
			profile.Alias, shortDigest(profile.CatalogSHA256), shortDigest(digest))
	}
	if digest != profiles.servedCatalogSHA256 {
		return material, fmt.Errorf("profile %s was activated with Catalog %s, this deployment serves %s",
			profile.Alias, shortDigest(digest), shortDigest(profiles.servedCatalogSHA256))
	}
	// The artifact directory is required rather than optional here because every
	// Final-V5 workload profile compiles an ordinal program. If one ever does not,
	// FrozenOperationMaterialV3.Validate rejects the pairing outright instead of
	// preparing against artifacts the profile never bound.
	info, err := os.Stat(profiles.artifactDir)
	if err != nil || !info.IsDir() {
		return material, fmt.Errorf("%s must name the mounted publication artifact directory",
			deploymentProfileArtifactDirEnv)
	}
	return profileMaterialV3{CatalogPath: catalogPath, SnapshotArtifactDir: profiles.artifactDir}, nil
}

// ------------------------------------------------------ the qualified footprint

// retainedQualificationV3 loads the qualified Attestation footprint from the
// retained qualification run that measured it.
//
// The footprint is the one class in the closed world that is not derivable from
// TaskGate source at all, so it can only come from a measurement -- and a
// measurement is evidence only while it is bound to what it was measured
// against. This reads the retained document, checks it against its own recorded
// digest, and then requires it to qualify THIS ExpectedSchema, environment and
// server before handing it over.
type retainedQualificationV3 struct{ documentPath string }

// qualificationDocumentV3 is the part of the retained report this reads.
//
// The report carries a great deal more -- provenance, per-scope stability, the
// measured intervals -- and none of it is decoded here. Decoding only the two
// members that matter keeps a report schema revision from making an otherwise
// valid qualification unreadable, and the footprint's own digest is what makes
// that safe: a document whose footprint was edited fails the self-check whatever
// else the file has grown.
type qualificationDocumentV3 struct {
	Footprint       AttestationFootprintV2 `json:"footprint"`
	FootprintSHA256 string                 `json:"footprint_sha256"`
}

func (qualification retainedQualificationV3) Resolve(catalogPath string,
	runtime PostgreSQLRuntimeIdentity) (AttestationFootprintV2, error) {
	payload, err := os.ReadFile(qualification.documentPath)
	if err != nil {
		return AttestationFootprintV2{}, fmt.Errorf("read the retained qualification: %w", err)
	}
	var document qualificationDocumentV3
	if err := json.Unmarshal(payload, &document); err != nil {
		return AttestationFootprintV2{}, fmt.Errorf("decode the retained qualification: %w", err)
	}
	digest, err := document.Footprint.SHA256()
	if err != nil {
		return AttestationFootprintV2{}, fmt.Errorf("the retained qualification's footprint is invalid: %w", err)
	}
	if digest != document.FootprintSHA256 {
		return AttestationFootprintV2{}, fmt.Errorf(
			"the retained qualification records footprint %s but its footprint digests to %s",
			shortDigest(document.FootprintSHA256), shortDigest(digest))
	}
	// The bindings, checked here rather than left to acceptance. A qualification
	// measured for another ExpectedSchema, another environment or another server
	// says nothing about this run, and finding that out at the point of loading
	// names the qualification rather than the sample.
	logicalCatalog, err := catalog.Load(catalogPath)
	if err != nil {
		return AttestationFootprintV2{}, fmt.Errorf("load the activated Profile Catalog: %w", err)
	}
	built, err := catalogschema.Build(logicalCatalog)
	if err != nil {
		return AttestationFootprintV2{}, fmt.Errorf("build the ExpectedSchema: %w", err)
	}
	if err := document.Footprint.Require(built.Digest, built.Count,
		RequiredMeasurementEnvironment(), runtime); err != nil {
		return AttestationFootprintV2{}, err
	}
	return document.Footprint, nil
}

// ------------------------------------------------------ the PostgreSQL identity

// retainedPostgreSQLIdentityV3 reads the complete immutable identity of the
// server this deployment runs.
//
// It is a retained document rather than a live query, and that is not a
// shortcut: PostgreSQLRuntimeIdentity is an image, repository and container
// identity, which no SQL can answer. It is produced by inspecting Docker Engine,
// the same way the qualification run obtained the identity it measured against.
//
// What binds the file to the server that actually served the window is not this
// reader. FinalizeObservationV3 requires the observer's OWN Docker Engine
// inspection, taken inside the measured window and sealed into the snapshot, to
// equal this identity -- so a stale or edited file fails there rather than
// quietly standing in for the running server.
type retainedPostgreSQLIdentityV3 struct{ documentPath string }

func (reader retainedPostgreSQLIdentityV3) ReadPostgreSQLIdentity(
	context.Context) (PostgreSQLRuntimeIdentity, error) {
	payload, err := os.ReadFile(reader.documentPath)
	if err != nil {
		return PostgreSQLRuntimeIdentity{}, fmt.Errorf("read the PostgreSQL runtime identity: %w", err)
	}
	var identity PostgreSQLRuntimeIdentity
	if err := StrictJSON(payload, &identity); err != nil {
		return PostgreSQLRuntimeIdentity{}, fmt.Errorf("decode the PostgreSQL runtime identity: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return PostgreSQLRuntimeIdentity{}, fmt.Errorf("PostgreSQL runtime identity: %w", err)
	}
	return identity, nil
}

// ------------------------------------------------------------ the Control Store

// controlStoreEvidenceV3 reads the Control Store's own account of one request.
//
// # What it answers, and what it deliberately does not
//
// It answers whether this request's query settled an execution binding row. It
// never asserts a replay. The store cannot distinguish "this call executed" from
// "this call replayed an earlier settlement": both resolve the same query row,
// and the evidence that makes a replay a replay -- the persisted document coming
// back byte for byte -- is transport evidence the store does not hold.
//
// Reporting no replay is therefore the honest answer, and it is fail-closed
// rather than permissive. A request that really was an exact request-ID replay
// resolves the original query's row, which HAS a binding row, so finalization
// proceeds down the executing path, derives a paired-novel plan and then finds a
// window in which nothing reached Business PostgreSQL. That is a rejection. The
// one thing this reader cannot do is make a replay pass as an execution.
type controlStoreEvidenceV3 struct{ dsn string }

// requestSettlementQueryV3 asks the two questions in one statement, so the query
// row and its binding row are read at one instant. Asking separately would admit
// a settlement landing between the two reads.
const requestSettlementQueryV3 = `SELECT q.id,
       EXISTS (SELECT 1 FROM query_execution_bindings b WHERE b.query_id = q.id)
FROM query_records q
WHERE q.task_id = $1 AND q.request_id = $2`

func (control controlStoreEvidenceV3) ReadRequestState(ctx context.Context,
	taskID, requestID string) (requestSettlementStateV3, error) {
	var state requestSettlementStateV3
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(requestID) == "" {
		return state, errors.New("the Control Store is asked about one task and one request id")
	}
	connection, err := pgx.Connect(ctx, control.dsn)
	if err != nil {
		return state, fmt.Errorf("connect the Control Store: %w", err)
	}
	defer connection.Close(context.Background())
	var queryID string
	if err := connection.QueryRow(ctx, requestSettlementQueryV3, taskID, requestID).Scan(
		&queryID, &state.WroteExecutionBindingRow); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Not a lookup miss. A receipt exists for this request, so the store
			// that issued it must hold the query it settled; a store that does not
			// is describing a different deployment.
			return state, errors.New("the Control Store holds no query for this task and request id, " +
				"but a signed receipt claims one was settled")
		}
		return state, fmt.Errorf("read the Control Store's account of this request: %w", err)
	}
	return state, nil
}
