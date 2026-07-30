package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

type boundOrdinalExecution struct {
	Program             queryplan.OrdinalProgram
	ProvenanceSQL       string
	ProvenanceFields    []string
	Indexes             map[string]ordinal.SnapshotIndex
	DictionarySet       ordinal.DictionarySetManifest
	DictionarySetDigest string
	SidecarGrants       []sqlpolicy.ProductGrant
	// EstimatedBaseFacts is a conservative pre-execution working-set estimate
	// derived from immutable publication row counts and the exact evidence-field
	// contract. It selects the million-fact lane; it is not a budget charge.
	EstimatedBaseFacts uint64
}

func (s *Service) ordinalQueryProduct(product catalog.Product, approved map[string]struct{}) (queryplan.Product, error) {
	publication, found := s.catalog.LookupSnapshotPublication(product.SnapshotPublication)
	if !found || publication.Source != product.Source || publication.SourceNamespace != product.FactNamespace ||
		publication.Snapshot != product.Snapshot || publication.ManifestDigest == "" {
		return queryplan.Product{}, fmt.Errorf("product %q has no matching immutable snapshot publication", product.Name)
	}
	result := relationalQueryProduct(product, approved)
	result.RequiredEvidence = append([]string(nil), product.Scopes...)
	result.SnapshotPublication = publication.Name
	result.SidecarManifestDigest = publication.ManifestDigest
	return result, nil
}

func extendOrdinalPolicyGrant(grant sqlpolicy.Grant, sidecars []sqlpolicy.ProductGrant) (sqlpolicy.Grant, error) {
	result := sqlpolicy.Grant{Products: append([]sqlpolicy.ProductGrant(nil), grant.Products...)}
	seen := make(map[string]struct{}, len(result.Products)+len(sidecars))
	for _, product := range result.Products {
		if _, duplicate := seen[product.LogicalName]; duplicate {
			return sqlpolicy.Grant{}, fmt.Errorf("duplicate policy product %q", product.LogicalName)
		}
		seen[product.LogicalName] = struct{}{}
	}
	for _, sidecar := range sidecars {
		if _, collision := seen[sidecar.LogicalName]; collision {
			return sqlpolicy.Grant{}, fmt.Errorf("ordinal sidecar logical name %q collides with a task product", sidecar.LogicalName)
		}
		seen[sidecar.LogicalName] = struct{}{}
		result.Products = append(result.Products, sidecar)
	}
	return result, nil
}

// bindOrdinalSidecars is the only place where trusted Catalog physical
// sidecars enter generated SQL. Queryplan emits logical evidence SQL plus a
// required handle contract; this binder wraps it, joins each pinned immutable
// sidecar by the Catalog entity key and projects the required handle aliases.
func (s *Service) bindOrdinalSidecars(baseSQL string, fields []string, program queryplan.OrdinalProgram) (boundOrdinalExecution, error) {
	if s == nil || s.catalog == nil || s.snapshotRegistry == nil {
		return boundOrdinalExecution{}, errors.New("V4 snapshot registry is unavailable")
	}
	if strings.TrimSpace(baseSQL) == "" || len(fields) == 0 || len(program.Sources) == 0 {
		return boundOrdinalExecution{}, errors.New("ordinal sidecar binding input is incomplete")
	}

	result := boundOrdinalExecution{Program: program, Indexes: make(map[string]ordinal.SnapshotIndex, len(program.Sources))}
	innerAlias := "tg_v4_provenance"
	selected := make([]string, 0, len(fields)+len(program.Sources))
	seenFields := make(map[string]struct{}, len(fields)+len(program.Sources))
	for _, field := range fields {
		if !safeGeneratedIdentifier(field) {
			return boundOrdinalExecution{}, fmt.Errorf("unsafe generated provenance field %q", field)
		}
		if _, duplicate := seenFields[field]; duplicate {
			return boundOrdinalExecution{}, fmt.Errorf("duplicate generated provenance field %q", field)
		}
		seenFields[field] = struct{}{}
		selected = append(selected, qualifiedGenerated(innerAlias, field)+" AS "+quoteGenerated(field))
		result.ProvenanceFields = append(result.ProvenanceFields, field)
	}

	type sidecarJoin struct {
		key         string
		logicalName string
		alias       string
		publication catalog.SnapshotPublication
		entity      []queryplan.OrdinalFieldBinding
	}
	joins := make([]sidecarJoin, 0, len(program.Sources))
	joinByKey := make(map[string]int)
	dictionaryManifests := make(map[string]string)
	publicationMembers := make(map[string]ordinal.DictionarySetMember)
	grantsByLogical := make(map[string]sqlpolicy.ProductGrant)
	registerPublication := func(publication catalog.SnapshotPublication) (ordinal.SnapshotIndex, error) {
		index, resolveErr := s.snapshotRegistry.Resolve(ordinal.PublicationKey{
			CatalogDigest: s.catalog.SHA256, PublicationName: publication.Name,
		})
		if resolveErr != nil || index.DictionaryDigest() != publication.DictionaryDigest ||
			index.ManifestDigest() != publication.ManifestDigest || index.Manifest().SidecarDigest != publication.SidecarDigest ||
			index.Manifest().SourceNamespace != publication.SourceNamespace || index.Manifest().Snapshot != publication.Snapshot {
			return nil, fmt.Errorf("snapshot publication %q does not match the Catalog manifest", publication.Name)
		}
		if previous, exists := dictionaryManifests[index.DictionaryDigest()]; exists && previous != index.ManifestDigest() {
			return nil, errors.New("one dictionary digest resolves to conflicting manifests")
		}
		dictionaryManifests[index.DictionaryDigest()] = index.ManifestDigest()
		member := ordinal.DictionarySetMember{PublicationName: publication.Name,
			DictionaryDigest: index.DictionaryDigest(), ManifestDigest: index.ManifestDigest()}
		if previous, duplicate := publicationMembers[publication.Name]; duplicate && previous != member {
			return nil, errors.New("one publication resolves to conflicting dictionary evidence")
		}
		publicationMembers[publication.Name] = member
		return index, nil
	}

	for sourceIndex, source := range program.Sources {
		publication, found := s.catalog.LookupSnapshotPublication(source.SidecarBinding.PublicationID)
		if !found || publication.Name == "" || source.SidecarBinding.ManifestDigest != publication.ManifestDigest ||
			publication.SourceNamespace != source.SourceNamespace || publication.Snapshot != source.Snapshot {
			return boundOrdinalExecution{}, fmt.Errorf("source %q is not bound to its Catalog snapshot publication", source.SourceAlias)
		}
		index, resolveErr := registerPublication(publication)
		if resolveErr != nil {
			return boundOrdinalExecution{}, fmt.Errorf("source %q snapshot index does not match the Catalog manifest", source.SourceAlias)
		}
		result.Indexes[source.SourceAlias] = index

		if !safeGeneratedIdentifier(source.HandleAlias) {
			return boundOrdinalExecution{}, fmt.Errorf("unsafe generated handle alias %q", source.HandleAlias)
		}
		if _, duplicate := seenFields[source.HandleAlias]; duplicate {
			return boundOrdinalExecution{}, fmt.Errorf("duplicate generated handle alias %q", source.HandleAlias)
		}
		seenFields[source.HandleAlias] = struct{}{}

		logical := ordinalSidecarLogicalName(publication.Name)
		schema, view, ok := strings.Cut(publication.OrdinalSidecar, ".")
		if !ok || !safeGeneratedIdentifier(schema) || !safeGeneratedIdentifier(view) {
			return boundOrdinalExecution{}, errors.New("Catalog ordinal sidecar identifier is invalid")
		}
		approvedColumns := []string{"row_handle"}
		entityParts := make([]string, 0, len(source.EntityKey))
		for _, keyField := range source.EntityKey {
			if !safeGeneratedIdentifier(keyField.Column) || !safeGeneratedIdentifier(keyField.ProvenanceAlias) {
				return boundOrdinalExecution{}, errors.New("ordinal entity-key binding is invalid")
			}
			approvedColumns = append(approvedColumns, keyField.Column)
			entityParts = append(entityParts, keyField.Column+"\x00"+keyField.ProvenanceAlias)
		}
		joinKey := publication.Name + "\x01" + strings.Join(entityParts, "\x01")
		joinIndex, present := joinByKey[joinKey]
		if !present {
			joinIndex = len(joins)
			joinByKey[joinKey] = joinIndex
			joins = append(joins, sidecarJoin{key: joinKey, logicalName: logical,
				alias: "tg_v4_sidecar_" + fmt.Sprintf("%d", len(joins)), publication: publication,
				entity: append([]queryplan.OrdinalFieldBinding(nil), source.EntityKey...)})
		}
		selected = append(selected, qualifiedGenerated(joins[joinIndex].alias, "row_handle")+" AS "+quoteGenerated(source.HandleAlias))
		result.ProvenanceFields = append(result.ProvenanceFields, source.HandleAlias)
		grantsByLogical[logical] = sqlpolicy.ProductGrant{LogicalName: logical, PhysicalSchema: schema,
			PhysicalView: view, ApprovedColumns: uniqueSortedStrings(approvedColumns), AllowedOperators: []string{"="}}
		_ = sourceIndex
	}
	// Root-family ledgers may execute different approved products over time.
	// Every query therefore binds the same immutable Catalog-wide dictionary
	// universe, while its bitmap still contains only facts actually observed by
	// that query. A query-local member set would pin the root to its first
	// product and make exact cross-product union impossible.
	for _, publication := range s.catalog.SnapshotPublications {
		if _, err := registerPublication(publication); err != nil {
			return boundOrdinalExecution{}, err
		}
	}

	var sql strings.Builder
	sql.WriteString("SELECT ")
	sql.WriteString(strings.Join(selected, ", "))
	sql.WriteString(" FROM (")
	sql.WriteString(baseSQL)
	sql.WriteString(") AS ")
	sql.WriteString(quoteGenerated(innerAlias))
	for _, join := range joins {
		sql.WriteString(" INNER JOIN ")
		sql.WriteString(quoteGenerated(join.logicalName))
		sql.WriteString(" AS ")
		sql.WriteString(quoteGenerated(join.alias))
		sql.WriteString(" ON ")
		predicates := make([]string, 0, len(join.entity))
		for _, keyField := range join.entity {
			predicates = append(predicates, qualifiedGenerated(innerAlias, keyField.ProvenanceAlias)+" = "+
				qualifiedGenerated(join.alias, keyField.Column))
		}
		if len(predicates) == 0 {
			return boundOrdinalExecution{}, errors.New("ordinal sidecar join has no entity key")
		}
		sql.WriteString(strings.Join(predicates, " AND "))
	}
	if len(program.ProvenanceOrder) > 0 {
		orders := make([]string, 0, len(program.ProvenanceOrder))
		for _, order := range program.ProvenanceOrder {
			if !safeGeneratedIdentifier(order.ProvenanceAlias) || (order.Direction != "ASC" && order.Direction != "DESC") {
				return boundOrdinalExecution{}, errors.New("ordinal provenance order is invalid")
			}
			orders = append(orders, qualifiedGenerated(innerAlias, order.ProvenanceAlias)+" "+order.Direction)
		}
		sql.WriteString(" ORDER BY ")
		sql.WriteString(strings.Join(orders, ", "))
	}
	result.ProvenanceSQL = sql.String()

	members := make([]ordinal.DictionarySetMember, 0, len(publicationMembers))
	for _, member := range publicationMembers {
		members = append(members, member)
	}
	dictionarySet, err := ordinal.NewDictionarySetManifest(s.catalog.SHA256, members...)
	if err != nil {
		return boundOrdinalExecution{}, err
	}
	result.DictionarySet = dictionarySet
	result.DictionarySetDigest, err = dictionarySet.Digest()
	if err != nil {
		return boundOrdinalExecution{}, err
	}
	for _, grant := range grantsByLogical {
		result.SidecarGrants = append(result.SidecarGrants, grant)
	}
	sort.Slice(result.SidecarGrants, func(i, j int) bool { return result.SidecarGrants[i].LogicalName < result.SidecarGrants[j].LogicalName })
	if err := result.Program.ValidateBoundSidecars(); err != nil {
		return boundOrdinalExecution{}, err
	}
	if err := result.Program.ValidateProvenanceFields(result.ProvenanceFields); err != nil {
		return boundOrdinalExecution{}, err
	}
	result.EstimatedBaseFacts = estimateOrdinalBaseFacts(result, 0)
	return result, nil
}

func estimateOrdinalBaseFacts(bound boundOrdinalExecution, scanRowLimit uint64) uint64 {
	var total uint64
	limitedScan := bound.Program.Kind == "scan" && len(bound.Program.Sources) == 1 &&
		len(bound.Program.Groups) == 0 && len(bound.Program.Aggregates) == 0 && scanRowLimit > 0
	for _, source := range bound.Program.Sources {
		index := bound.Indexes[source.SourceAlias]
		if index == nil {
			return ^uint64(0)
		}
		rows := index.RowCount()
		if limitedScan && rows > scanRowLimit {
			rows = scanRowLimit
		}
		perRow := uint64(len(source.EvidenceFields)) + 1
		if rows != 0 && perRow > ^uint64(0)/rows {
			return ^uint64(0)
		}
		facts := rows * perRow
		if facts > ^uint64(0)-total {
			return ^uint64(0)
		}
		total += facts
	}
	return total
}

func ordinalSidecarLogicalName(publication string) string {
	sum := sha256.Sum256([]byte("taskgate-sidecar-logical\x00" + publication))
	return "ordinal_sidecar_" + hex.EncodeToString(sum[:8])
}

func quoteGenerated(value string) string { return `"` + value + `"` }
func qualifiedGenerated(alias, field string) string {
	return quoteGenerated(alias) + "." + quoteGenerated(field)
}

func safeGeneratedIdentifier(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || character == '_' ||
			(index > 0 && character >= '0' && character <= '9') {
			continue
		}
		return false
	}
	return true
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, present := seen[value]; present {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
