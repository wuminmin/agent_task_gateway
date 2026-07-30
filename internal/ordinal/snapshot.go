package ordinal

import (
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

// RowHandle is a compact, publication-local stable handle. Zero is reserved
// for "not found"; handles are assigned by canonical EntityKey order.
type RowHandle uint64

type SnapshotField struct {
	// Name addresses the sidecar/input column. CanonicalFieldID is the exact
	// field string used by the existing FactID semantics (for example
	// "expense.amount" in relational V2). Empty means Name.
	Name             string
	CanonicalFieldID string
	SQLType          string
}

type SnapshotRow struct {
	// EntityKey is the output of exposure.ComposeCanonicalKeyV2 for the
	// Catalog-declared entity key columns.
	EntityKey string
	Values    map[string]any
}

type SnapshotSpec struct {
	SourceID          string
	SourceNamespace   string
	Snapshot          string
	SchemaDigest      string
	SidecarDigest     string
	ColdPayloadDigest string
	Fields            []SnapshotField
	Rows              []SnapshotRow
}

type RowRefs struct {
	Handle    RowHandle
	EntityKey string
	Row       FactRef
	Cells     map[string]FactRef
}

// CompileSnapshot deterministically creates canonical base FactIDs, assigns
// per-segment ordinals by full SHA-256 order, and builds both directions of the
// hot row sidecar index.
func CompileSnapshot(spec SnapshotSpec) (*Dictionary, error) {
	return compileSnapshotWithSegmentCapacity(spec, maxOrdinalSegmentFacts)
}

func compileSnapshotWithSegmentCapacity(spec SnapshotSpec, segmentCapacity uint64) (*Dictionary, error) {
	if len(spec.Fields) == 0 {
		return nil, fmt.Errorf("%w: snapshot fields are required", ErrInvalid)
	}
	fields := append([]SnapshotField(nil), spec.Fields...)
	inputFields := make(map[string]string, len(fields))
	seenCanonicalFields := make(map[string]struct{}, len(fields))
	for index := range fields {
		if !validID(fields[index].Name) {
			return nil, fmt.Errorf("%w: invalid snapshot field", ErrInvalid)
		}
		if fields[index].CanonicalFieldID == "" {
			fields[index].CanonicalFieldID = fields[index].Name
		}
		if !validID(fields[index].CanonicalFieldID) {
			return nil, fmt.Errorf("%w: invalid canonical snapshot field", ErrInvalid)
		}
		canonicalType, err := exposure.CanonicalSQLTypeV2(fields[index].SQLType)
		if err != nil {
			return nil, err
		}
		fields[index].SQLType = canonicalType
		if _, duplicate := seenCanonicalFields[fields[index].CanonicalFieldID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate canonical snapshot field", ErrInvalid)
		}
		if existingType, present := inputFields[fields[index].Name]; present && existingType != canonicalType {
			return nil, fmt.Errorf("%w: one input field has conflicting SQL types", ErrInvalid)
		}
		inputFields[fields[index].Name] = canonicalType
		seenCanonicalFields[fields[index].CanonicalFieldID] = struct{}{}
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].CanonicalFieldID < fields[j].CanonicalFieldID })

	rows := append([]SnapshotRow(nil), spec.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].EntityKey < rows[j].EntityKey })
	rowFacts := make([]exposure.FactID, 0, len(rows))
	cellFacts := make(map[string][]exposure.FactID, len(fields))
	for _, field := range fields {
		cellFacts[field.CanonicalFieldID] = make([]exposure.FactID, 0, len(rows))
	}
	for index, row := range rows {
		if !validID(row.EntityKey) || (index > 0 && row.EntityKey == rows[index-1].EntityKey) {
			return nil, fmt.Errorf("%w: missing or duplicate canonical entity key", ErrInvalid)
		}
		if len(row.Values) != len(inputFields) {
			return nil, fmt.Errorf("%w: snapshot row field set differs from schema", ErrInvalid)
		}
		rowFact, err := exposure.NewBaseRowFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey)
		if err != nil {
			return nil, err
		}
		rowFacts = append(rowFacts, rowFact)
		for _, field := range fields {
			value, present := row.Values[field.Name]
			if !present {
				return nil, fmt.Errorf("%w: snapshot row misses field", ErrInvalid)
			}
			fact, err := exposure.NewBaseCellFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey, field.CanonicalFieldID, field.SQLType, value)
			if err != nil {
				return nil, err
			}
			cellFacts[field.CanonicalFieldID] = append(cellFacts[field.CanonicalFieldID], fact)
		}
	}

	segments := make([]SegmentSpec, 0, len(fields)+1)
	segments = append(segments, SegmentSpec{ID: "row", Kind: SegmentBaseRow, Facts: rowFacts})
	for _, field := range fields {
		segments = append(segments, SegmentSpec{ID: "cell:" + field.CanonicalFieldID, Kind: SegmentBaseCell,
			Field: field.CanonicalFieldID, Facts: cellFacts[field.CanonicalFieldID]})
	}
	dictionary, err := compileWithSegmentCapacity(DictionarySpec{SourceID: spec.SourceID, SourceNamespace: spec.SourceNamespace,
		Snapshot: spec.Snapshot, SchemaDigest: spec.SchemaDigest, Segments: segments}, segmentCapacity)
	if err != nil {
		return nil, err
	}
	if spec.ColdPayloadDigest != "" && spec.ColdPayloadDigest != dictionary.manifest.ColdPayloadDigest {
		return nil, fmt.Errorf("%w: compiled cold payload digest", ErrDigestMismatch)
	}
	dictionary.entityToHandle = make(map[string]RowHandle, len(rows))
	dictionary.rows = make(map[RowHandle]RowRefs, len(rows))
	for index, row := range rows {
		handle := RowHandle(index + 1)
		rowFact, _ := exposure.NewBaseRowFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey)
		rowRef, found, lookupErr := dictionary.Lookup(rowFact)
		if lookupErr != nil || !found {
			return nil, fmt.Errorf("%w: compiled row fact is missing", ErrInvalid)
		}
		refs := RowRefs{Handle: handle, EntityKey: row.EntityKey, Row: rowRef, Cells: make(map[string]FactRef, len(fields))}
		for _, field := range fields {
			fact, factErr := exposure.NewBaseCellFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey,
				field.CanonicalFieldID, field.SQLType, row.Values[field.Name])
			if factErr != nil {
				return nil, factErr
			}
			ref, cellFound, cellErr := dictionary.Lookup(fact)
			if cellErr != nil || !cellFound {
				return nil, fmt.Errorf("%w: compiled cell fact is missing", ErrInvalid)
			}
			refs.Cells[field.CanonicalFieldID] = ref
		}
		dictionary.entityToHandle[row.EntityKey] = handle
		dictionary.rows[handle] = refs
	}
	sidecarDigest := digestSidecarRows(dictionary.rows)
	if spec.SidecarDigest != "" && spec.SidecarDigest != sidecarDigest {
		return nil, fmt.Errorf("%w: compiled sidecar digest", ErrDigestMismatch)
	}
	dictionary.manifest.SidecarDigest = sidecarDigest
	dictionary.manifest.HotIndexDigest = digestHotDictionary(dictionary.DictionaryDigest(), dictionary.manifest.Segments, dictionary.segments, dictionary.rows)
	manifestDigest, err := dictionary.manifest.Digest()
	if err != nil {
		return nil, err
	}
	dictionary.digest = manifestDigest
	return dictionary, nil
}

// CompileSnapshotArtifact is the production-facing compiler entrypoint. The
// combined oracle dictionary is released after the hot/cold split.
func CompileSnapshotArtifact(spec SnapshotSpec) (CompiledArtifact, error) {
	dictionary, err := CompileSnapshot(spec)
	if err != nil {
		return CompiledArtifact{}, err
	}
	return dictionary.splitOwned()
}

func (d *Dictionary) RowCount() uint64 {
	if d == nil {
		return 0
	}
	return uint64(len(d.rows))
}

func (d *Dictionary) LookupRowHandle(entityKey string) (RowHandle, bool) {
	if d == nil {
		return 0, false
	}
	handle, found := d.entityToHandle[entityKey]
	return handle, found
}

func (d *Dictionary) LookupRow(handle RowHandle) (RowRefs, bool) {
	if d == nil || handle == 0 {
		return RowRefs{}, false
	}
	row, found := d.rows[handle]
	if !found {
		return RowRefs{}, false
	}
	row.Cells = cloneCellRefs(row.Cells)
	return row, true
}

func (d *Dictionary) LookupEntity(entityKey string) (RowRefs, bool) {
	handle, found := d.LookupRowHandle(entityKey)
	if !found {
		return RowRefs{}, false
	}
	return d.LookupRow(handle)
}

func cloneCellRefs(source map[string]FactRef) map[string]FactRef {
	result := make(map[string]FactRef, len(source))
	for field, ref := range source {
		result[field] = ref
	}
	return result
}
