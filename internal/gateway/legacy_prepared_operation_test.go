package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/physicalquery"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// This file is the Query Execution Binding V1 prepared-operation construction,
// retained as evidence rather than as code.
//
// It was production until T1d, when the Gateway began signing a
// QueryExecutionBindingV2 that carries the sealed PreparedOperationBindingV1
// whole. Deleting it outright would have deleted the coverage proof the version
// decision rests on: TestQueryReceiptV9DoesNotCoverThePreparedOperationBinding
// shows that two preparations differing in a load-bearing member of the new
// binding reach the SAME digest under the construction below, which is why
// route A -- adopting the new preparation under the existing receipt version --
// was not available. A proof about V9 needs V9 to still be computable.
//
// Nothing in production calls any of it. It is here so the argument stays
// checkable, and it must not acquire a production caller: a Gateway that built
// one of these again would be writing the preparation identity down a second
// time, which is what V2 exists to stop.

// The domains below separate three digests that would otherwise be computed
// over overlapping material. A prepared target and the operation that contains
// it must never collide, or a target could be presented as its own operation.
const (
	preparedOperationDomain = "TASKGATE-PREPARED-OPERATION-BINDING-V1"
	preparedTargetDomain    = "TASKGATE-PREPARED-TARGET-BINDING-V1"
	sidecarGrantsDomain     = "TASKGATE-ORDINAL-SIDECAR-GRANTS-V1"
)

// preparedOperation is everything fixed before any row limit was known.
//
// It is the compiler's and the Catalog's half of the execution identity: which
// plan, compiled by which compiler, against which schema, dictionary and
// sidecar grants. The runtime half -- the limits, the rendered bytes, what
// actually ran -- lives in the target records.
//
// Every field here is read from a real source. None of them is a constant
// chosen to make a SHA-256 check pass; a binding whose required digests were
// filled with arbitrary values would validate and mean nothing.
type preparedOperation struct {
	PlanSHA256             string
	ExposureProfileVersion string
	GrantDigest            string
	ManifestDigest         string
	CatalogSHA256          string
	DatasourceID           string
	SchemaDigest           string
	ViewBindingDigest      string
	// OrdinalDictionarySetSHA256 and SidecarGrantsSHA256 are empty for an
	// exposure operation with no ordinal sidecar.
	OrdinalDictionarySetSHA256 string
	SidecarGrantsSHA256        string
	// VisibleSQL and CompanionSQL are the COMPILED statements, before sqlpolicy
	// rendered a row limit into them. They are digested, never stored.
	VisibleSQL   string
	CompanionSQL string

	compilerVersion    string
	compilerSHA256     string
	rendererVersion    string
	rendererSHA256     string
	visibleTargetSHA   string
	companionTargetSHA string
}

// newPreparedOperation resolves the compiler and renderer identities and
// computes the prepared-target digests.
func newPreparedOperation(base preparedOperation) (preparedOperation, error) {
	compilerSHA, err := queryplan.CompilerSHA256()
	if err != nil {
		return preparedOperation{}, fmt.Errorf("compiler identity: %w", err)
	}
	rendererSHA, err := sqlpolicy.RendererSHA256()
	if err != nil {
		return preparedOperation{}, fmt.Errorf("renderer identity: %w", err)
	}
	base.compilerVersion = queryplan.CompilerVersion
	base.compilerSHA256 = compilerSHA
	base.rendererVersion = sqlpolicy.RendererVersion
	base.rendererSHA256 = rendererSHA
	if strings.TrimSpace(base.VisibleSQL) == "" {
		return preparedOperation{}, errors.New("a prepared operation has no visible statement")
	}
	base.visibleTargetSHA = base.preparedTargetDigest(querybinding.RoleVisible, base.VisibleSQL)
	if base.CompanionSQL != "" {
		base.companionTargetSHA = base.preparedTargetDigest(querybinding.RoleCompanion, base.CompanionSQL)
	}
	return base, nil
}

// preparedTargetDigest identifies one compiled statement independently of the
// row limit it is later rendered with.
//
// The limit is deliberately excluded: the same prepared target is rendered with
// different limits as the budget moves, and a target binding that changed with
// the limit could not tie two executions to one compiled statement. The limit is
// covered by the target record's exact digest instead.
func (operation preparedOperation) preparedTargetDigest(role querybinding.TargetRole, preparedSQL string) string {
	hash := sha256.New()
	writeBindingField(hash, "domain", preparedTargetDomain)
	writeBindingField(hash, "role", string(role))
	writeBindingField(hash, "plan_sha256", operation.PlanSHA256)
	writeBindingField(hash, "compiler_version", operation.compilerVersion)
	writeBindingField(hash, "compiler_sha256", operation.compilerSHA256)
	writeBindingField(hash, "renderer_version", operation.rendererVersion)
	writeBindingField(hash, "renderer_sha256", operation.rendererSHA256)
	writeBindingField(hash, "prepared_sql_sha256", physicalquery.ExactDigest(preparedSQL))
	return hex.EncodeToString(hash.Sum(nil))
}

// digest is the operation-level identity the whole binding names.
func (operation preparedOperation) digest() string {
	hash := sha256.New()
	writeBindingField(hash, "domain", preparedOperationDomain)
	writeBindingField(hash, "plan_sha256", operation.PlanSHA256)
	writeBindingField(hash, "compiler_version", operation.compilerVersion)
	writeBindingField(hash, "compiler_sha256", operation.compilerSHA256)
	writeBindingField(hash, "renderer_version", operation.rendererVersion)
	writeBindingField(hash, "renderer_sha256", operation.rendererSHA256)
	writeBindingField(hash, "exposure_profile_version", operation.ExposureProfileVersion)
	writeBindingField(hash, "grant_digest", operation.GrantDigest)
	writeBindingField(hash, "manifest_digest", operation.ManifestDigest)
	writeBindingField(hash, "catalog_sha256", operation.CatalogSHA256)
	writeBindingField(hash, "datasource_id", operation.DatasourceID)
	writeBindingField(hash, "schema_digest", operation.SchemaDigest)
	writeBindingField(hash, "view_binding_digest", operation.ViewBindingDigest)
	writeBindingField(hash, "ordinal_dictionary_set_sha256", operation.OrdinalDictionarySetSHA256)
	writeBindingField(hash, "sidecar_grants_sha256", operation.SidecarGrantsSHA256)
	writeBindingField(hash, "visible_prepared_target_sha256", operation.visibleTargetSHA)
	writeBindingField(hash, "companion_prepared_target_sha256", operation.companionTargetSHA)
	return hex.EncodeToString(hash.Sum(nil))
}

// sidecarGrantsDigest identifies the least-privilege sidecar grants an ordinal
// execution was extended with.
//
// They widen what the policy engine admits, so an execution binding that did not
// name them could not distinguish a statement authorized against the task's own
// products from one authorized against a wider set.
func sidecarGrantsDigest(grants []sqlpolicy.ProductGrant) string {
	ordered := append([]sqlpolicy.ProductGrant(nil), grants...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].LogicalName < ordered[right].LogicalName
	})
	hash := sha256.New()
	writeBindingField(hash, "domain", sidecarGrantsDomain)
	writeBindingField(hash, "count", fmt.Sprintf("%d", len(ordered)))
	for _, grant := range ordered {
		writeBindingField(hash, "logical_name", grant.LogicalName)
		writeBindingField(hash, "physical_schema", grant.PhysicalSchema)
		writeBindingField(hash, "physical_view", grant.PhysicalView)
		writeBindingField(hash, "approved_columns", strings.Join(sortedCopy(grant.ApprovedColumns), "\x1f"))
		writeBindingField(hash, "allowed_functions", strings.Join(sortedCopy(grant.AllowedFunctions), "\x1f"))
		writeBindingField(hash, "allowed_aggregates", strings.Join(sortedCopy(grant.AllowedAggregates), "\x1f"))
		writeBindingField(hash, "allowed_operators", strings.Join(sortedCopy(grant.AllowedOperators), "\x1f"))
		scope := append([]sqlpolicy.ScopePredicate(nil), grant.MandatoryScope...)
		sort.SliceStable(scope, func(left, right int) bool {
			if scope[left].Column == scope[right].Column {
				return scope[left].Operator < scope[right].Operator
			}
			return scope[left].Column < scope[right].Column
		})
		writeBindingField(hash, "scope_count", fmt.Sprintf("%d", len(scope)))
		for _, predicate := range scope {
			writeBindingField(hash, "scope_column", predicate.Column)
			writeBindingField(hash, "scope_operator", string(predicate.Operator))
			writeBindingField(hash, "scope_values", strings.Join(predicate.Values, "\x1f"))
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func writeBindingField(hash interface{ Write([]byte) (int, error) }, name, value string) {
	fmt.Fprintf(hash, "%s\x00%d\x00%s\x00", name, len(value), value)
}
