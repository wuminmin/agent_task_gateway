package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/viewbinding"
	"taskbound.local/agent-data-gateway/internal/viewcompiler"
)

const viewSemanticObservationDomain = "TASKGATE-VIEW-SEMANTIC-OBSERVATION-V1\x00"

// pendingViewBinding retains the canonical evidence required to activate a
// grant. CanonicalJSON itself contains only logical product names and digests;
// Dependencies is private reverse-index metadata and may contain physical
// schema-qualified relation names.
type pendingViewBinding struct {
	Digest         string                       `json:"digest"`
	ProfileVersion string                       `json:"profile_version"`
	CanonicalJSON  json.RawMessage              `json:"canonical_json"`
	Dependencies   []control.TaskViewDependency `json:"dependencies"`
}

type resolvedViewBinding struct {
	pendingViewBinding
	Expectation dataconnector.ViewRegistryExpectation
	// Artifacts are the executable, canonical expansions observed in the same
	// registry snapshot as Expectation. They remain private to the gateway: the
	// signed task surface carries only their digests, never terminal products or
	// physical dependency names.
	Artifacts map[string]viewcompiler.Artifact
}

type viewRegistryConnector interface {
	DiscoverViewRegistry(context.Context, []viewcompiler.RelationName, map[string]string) (viewcompiler.RegistrySnapshot, error)
}

// resolveViewBinding discovers and compiles only the requested semantic
// products. Ordinary Catalog products remain on the legacy schema-attestation
// path, so an unrelated View replacement cannot invalidate this task.
func (s *Service) resolveViewBinding(ctx context.Context, productNames []string) (*resolvedViewBinding, error) {
	selectedProducts := make([]catalog.Product, 0, len(productNames))
	semanticProducts := make([]catalog.Product, 0, len(productNames))
	seen := make(map[string]struct{}, len(productNames))
	for _, name := range productNames {
		if _, duplicate := seen[name]; duplicate {
			return nil, viewSemanticChangedError()
		}
		seen[name] = struct{}{}
		product, present := s.catalog.LookupProduct(name)
		if !present {
			return nil, viewSemanticChangedError()
		}
		selectedProducts = append(selectedProducts, product)
		if product.ViewContract != nil {
			semanticProducts = append(semanticProducts, product)
		}
	}
	if len(semanticProducts) == 0 {
		return nil, nil
	}
	discoverer, ok := s.connector.(viewRegistryConnector)
	if !ok {
		return nil, viewSemanticChangedError()
	}
	sort.Slice(semanticProducts, func(i, j int) bool { return semanticProducts[i].Name < semanticProducts[j].Name })

	roots := make([]viewcompiler.RelationName, 0, len(semanticProducts))
	rootProducts := make(map[viewcompiler.RelationName]catalog.Product, len(semanticProducts))
	for _, product := range semanticProducts {
		root, err := reportingRelationName(product.ReportingView)
		if err != nil {
			return nil, viewSemanticChangedError()
		}
		if _, duplicate := rootProducts[root]; duplicate {
			return nil, viewSemanticChangedError()
		}
		roots = append(roots, root)
		rootProducts[root] = product
	}

	baseMap := make(map[string]string)
	compilerProducts := make(map[string]queryplan.Product)
	selectedSource := semanticProducts[0].Source
	for _, product := range s.catalog.ListProducts() {
		if product.Source != selectedSource || product.ViewContract != nil {
			continue
		}
		baseMap[product.ReportingView] = product.Name
		compilerProducts[product.Name] = relationalQueryProduct(product, stringSetFromSlice(product.FieldNames()))
	}
	if len(baseMap) == 0 {
		return nil, viewSemanticChangedError()
	}
	snapshot, err := discoverer.DiscoverViewRegistry(ctx, roots, baseMap)
	if err != nil {
		return nil, err
	}
	reachableBases, err := reachableRegistryBaseProducts(snapshot, baseMap)
	if err != nil {
		return nil, viewSemanticChangedError()
	}
	compiler, err := viewcompiler.New(snapshot, compilerProducts)
	if err != nil {
		return nil, viewSemanticChangedError()
	}

	contractsByProduct := make(map[string]viewbinding.ProductContract, len(selectedProducts))
	artifactsByProduct := make(map[string]viewcompiler.Artifact, len(semanticProducts))
	dependencies := make([]control.TaskViewDependency, 0)
	for _, root := range roots {
		product := rootProducts[root]
		artifact, compileErr := compiler.Compile(root)
		if compileErr != nil || !viewArtifactMatchesCatalog(artifact, product) {
			return nil, viewSemanticChangedError()
		}
		if _, governanceErr := s.semanticViewGovernanceFor(product, artifact); governanceErr != nil {
			return nil, viewSemanticChangedError()
		}
		contractsByProduct[product.Name] = viewbinding.ProductContract{
			Product: product.Name, CanonicalPlanDigest: artifact.CanonicalPlanDigest,
			DependencyDigest: artifact.DependencyDigest, InterfaceDigest: artifact.InterfaceDigest,
		}
		artifactsByProduct[product.Name] = artifact
		for _, dependency := range artifact.DependencyClosure {
			dependencies = append(dependencies, control.TaskViewDependency{
				Product: product.Name, DependencyKey: dependency.Name.String(),
			})
		}
	}
	for _, product := range selectedProducts {
		if _, semantic := contractsByProduct[product.Name]; semantic {
			continue
		}
		contract, contractErr := opaqueProductViewContract(product)
		if contractErr != nil {
			return nil, viewSemanticChangedError()
		}
		contractsByProduct[product.Name] = contract
		dependencies = append(dependencies, control.TaskViewDependency{
			Product: product.Name, DependencyKey: product.ReportingView,
		})
	}
	contracts := make([]viewbinding.ProductContract, 0, len(contractsByProduct))
	for _, contract := range contractsByProduct {
		contracts = append(contracts, contract)
	}
	set, err := viewbinding.New(contracts)
	if err != nil {
		return nil, viewSemanticChangedError()
	}
	canonical, err := json.Marshal(set)
	if err != nil {
		return nil, err
	}
	digest, err := set.Digest()
	if err != nil {
		return nil, err
	}
	sort.Slice(dependencies, func(i, j int) bool {
		if dependencies[i].Product != dependencies[j].Product {
			return dependencies[i].Product < dependencies[j].Product
		}
		return dependencies[i].DependencyKey < dependencies[j].DependencyKey
	})
	return &resolvedViewBinding{pendingViewBinding: pendingViewBinding{
		Digest: digest, ProfileVersion: viewbinding.Version,
		CanonicalJSON: append(json.RawMessage(nil), canonical...), Dependencies: dependencies,
	}, Expectation: dataconnector.ViewRegistryExpectation{
		Roots: append([]viewcompiler.RelationName(nil), roots...), BaseProducts: reachableBases,
		ExpectedRevisionDigest: snapshot.RevisionDigest,
	}, Artifacts: artifactsByProduct}, nil
}

func reachableRegistryBaseProducts(snapshot viewcompiler.RegistrySnapshot, candidates map[string]string) (map[string]string, error) {
	result := make(map[string]string)
	for name, relation := range snapshot.Relations {
		if relation.Kind != viewcompiler.RelationBase {
			continue
		}
		qualified := name.String()
		product, present := candidates[qualified]
		if !present || product == "" || product != relation.ProductName {
			return nil, errors.New("reachable registry leaf is not mapped to its governed product")
		}
		result[qualified] = product
	}
	if len(result) == 0 {
		return nil, errors.New("registry has no governed terminal products")
	}
	return result, nil
}

func opaqueProductViewContract(product catalog.Product) (viewbinding.ProductContract, error) {
	type publicField struct {
		Name             string `json:"name"`
		SQLType          string `json:"sql_type"`
		Collation        string `json:"collation,omitempty"`
		CollationVersion string `json:"collation_version,omitempty"`
	}
	fields := make([]publicField, 0, len(product.Fields))
	for _, field := range product.Fields {
		typeName, err := exposure.CanonicalSQLTypeV2(field.Type)
		if err != nil {
			return viewbinding.ProductContract{}, err
		}
		fields = append(fields, publicField{Name: field.Name, SQLType: typeName,
			Collation: field.Collation, CollationVersion: field.CollationVersion})
	}
	interfaceDigest, err := digestViewEvidence("TASKGATE-VIEW-INTERFACE-V1\x00", fields)
	if err != nil {
		return viewbinding.ProductContract{}, err
	}
	planDigest, err := digestViewEvidence("TASKGATE-OPAQUE-PRODUCT-PLAN-V1\x00", struct {
		Product         string        `json:"product"`
		SourceNamespace string        `json:"source_namespace,omitempty"`
		Snapshot        string        `json:"snapshot"`
		StableRole      string        `json:"stable_role,omitempty"`
		LineageDigest   string        `json:"lineage_digest,omitempty"`
		Fields          []publicField `json:"fields"`
	}{product.Name, product.FactNamespace, product.Snapshot, product.StableRelationRole,
		product.LineageManifestDigest, fields})
	if err != nil {
		return viewbinding.ProductContract{}, err
	}
	dependencyDigest, err := digestViewEvidence("TASKGATE-OPAQUE-PRODUCT-DEPENDENCY-V1\x00", struct {
		ReportingView string `json:"reporting_view"`
		Snapshot      string `json:"snapshot"`
	}{product.ReportingView, product.Snapshot})
	if err != nil {
		return viewbinding.ProductContract{}, err
	}
	return viewbinding.ProductContract{Product: product.Name, CanonicalPlanDigest: planDigest,
		DependencyDigest: dependencyDigest, InterfaceDigest: interfaceDigest}, nil
}

func digestViewEvidence(domain string, value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(domain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func viewArtifactMatchesCatalog(artifact viewcompiler.Artifact, product catalog.Product) bool {
	contract := product.ViewContract
	if contract == nil || contract.ProfileVersion != catalog.ViewContractV1 ||
		artifact.DefinitionDigest != contract.DefinitionDigest ||
		artifact.DependencyDigest != contract.DependencyDigest ||
		artifact.CanonicalPlanDigest != contract.CanonicalPlanDigest ||
		artifact.InterfaceDigest != contract.InterfaceDigest ||
		len(artifact.Outputs) != len(product.Fields) {
		return false
	}
	for index, output := range artifact.Outputs {
		field := product.Fields[index]
		typeName, err := exposure.CanonicalSQLTypeV2(field.Type)
		if err != nil || output.Name != field.Name || output.SQLType != typeName ||
			output.Collation != field.Collation || output.CollationVersion != field.CollationVersion {
			return false
		}
	}
	return true
}

func reportingRelationName(value string) (viewcompiler.RelationName, error) {
	schema, name, ok := strings.Cut(value, ".")
	if !ok || schema == "" || name == "" || strings.Contains(name, ".") {
		return viewcompiler.RelationName{}, errors.New("invalid reporting relation")
	}
	return viewcompiler.RelationName{Schema: schema, Name: name}, nil
}

func (binding *pendingViewBinding) validate() (viewbinding.Set, error) {
	if binding == nil || binding.ProfileVersion != viewbinding.Version ||
		!validSnapshotSHA256(binding.Digest) || len(binding.CanonicalJSON) == 0 || len(binding.Dependencies) == 0 {
		return viewbinding.Set{}, errors.New("invalid pending View binding")
	}
	var set viewbinding.Set
	decoder := json.NewDecoder(bytes.NewReader(binding.CanonicalJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&set); err != nil {
		return viewbinding.Set{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return viewbinding.Set{}, errors.New("pending View binding has trailing JSON")
	}
	canonical, err := viewbinding.New(set.Products)
	if err != nil || set.Version != viewbinding.Version {
		return viewbinding.Set{}, errors.New("pending View binding is not canonical")
	}
	if !reflect.DeepEqual(set, canonical) {
		return viewbinding.Set{}, errors.New("pending View binding encoding is not canonical")
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return viewbinding.Set{}, errors.New("pending View binding cannot be canonically encoded")
	}
	// request_context_json is PostgreSQL JSONB, which normalizes whitespace and
	// object-key order inside RawMessage values. Restore the protocol encoding
	// after semantic validation before persisting the immutable control set.
	binding.CanonicalJSON = encoded
	digest, err := canonical.Digest()
	if err != nil || !sameSnapshotSHA256(digest, binding.Digest) {
		return viewbinding.Set{}, errors.New("pending View binding digest mismatch")
	}
	products := make(map[string]struct{}, len(canonical.Products))
	for _, contract := range canonical.Products {
		products[contract.Product] = struct{}{}
	}
	previous := ""
	covered := make(map[string]bool, len(products))
	for _, dependency := range binding.Dependencies {
		key := dependency.Product + "\x00" + dependency.DependencyKey
		if _, present := products[dependency.Product]; !present ||
			strings.TrimSpace(dependency.DependencyKey) != dependency.DependencyKey || dependency.DependencyKey == "" ||
			(previous != "" && key <= previous) {
			return viewbinding.Set{}, errors.New("pending View dependency index is invalid")
		}
		covered[dependency.Product] = true
		previous = key
	}
	for product := range products {
		if !covered[product] {
			return viewbinding.Set{}, errors.New("pending View dependency index is incomplete")
		}
	}
	return canonical, nil
}

func (binding *pendingViewBinding) controlSet(createdAt time.Time) *control.ViewBindingSet {
	if binding == nil {
		return nil
	}
	return &control.ViewBindingSet{
		Digest: binding.Digest, ProfileVersion: binding.ProfileVersion,
		CanonicalJSON: append(json.RawMessage(nil), binding.CanonicalJSON...),
		Dependencies:  append([]control.TaskViewDependency(nil), binding.Dependencies...), CreatedAt: createdAt,
	}
}

func boundViewProducts(binding *pendingViewBinding) ([]string, error) {
	set, err := binding.validate()
	if err != nil {
		return nil, err
	}
	result := make([]string, 0, len(set.Products))
	for _, contract := range set.Products {
		result = append(result, contract.Product)
	}
	return result, nil
}

func viewBindingMatchesCurrent(pending *pendingViewBinding, current *resolvedViewBinding) bool {
	if pending == nil || current == nil {
		return pending == nil && current == nil
	}
	if _, err := pending.validate(); err != nil {
		return false
	}
	return sameSnapshotSHA256(pending.Digest, current.Digest)
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func clonePendingViewBinding(input *pendingViewBinding) *pendingViewBinding {
	if input == nil {
		return nil
	}
	return &pendingViewBinding{
		Digest: input.Digest, ProfileVersion: input.ProfileVersion,
		CanonicalJSON: append(json.RawMessage(nil), input.CanonicalJSON...),
		Dependencies:  append([]control.TaskViewDependency(nil), input.Dependencies...),
	}
}

func pendingViewBindingDigest(binding *pendingViewBinding) string {
	if binding == nil {
		return ""
	}
	return binding.Digest
}

func viewSemanticChangedError() error {
	return &dataconnector.Error{Code: dataconnector.CodeViewSemanticChanged}
}

// viewSemanticObservationDigest supplies durable evidence even when a changed
// definition no longer compiles and therefore has no valid binding-set digest.
// It is deliberately distinct from every valid task View binding domain.
func viewSemanticObservationDigest(bound string, cause error) string {
	code := "VIEW_SEMANTIC_CHANGED"
	var connectorErr *dataconnector.Error
	if errors.As(cause, &connectorErr) {
		code = string(connectorErr.Code)
	}
	payload := fmt.Sprintf("%s\x00%s", bound, code)
	digest := sha256.Sum256(append([]byte(viewSemanticObservationDomain), []byte(payload)...))
	return hex.EncodeToString(digest[:])
}
