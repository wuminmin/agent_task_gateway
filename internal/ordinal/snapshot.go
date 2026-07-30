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
	dictionary, fields, rows, err := compileSnapshotDictionary(spec, segmentCapacity, true)
	if err != nil {
		return nil, err
	}
	if err := attachDictionarySnapshotRows(dictionary, spec, fields, rows); err != nil {
		return nil, err
	}
	if err := finalizeDictionarySnapshot(dictionary, spec.SidecarDigest); err != nil {
		return nil, err
	}
	return dictionary, nil
}

func compileSnapshotDictionary(spec SnapshotSpec, segmentCapacity uint64,
	buildLookup bool) (*Dictionary, []SnapshotField, []SnapshotRow, error) {
	if len(spec.Fields) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: snapshot fields are required", ErrInvalid)
	}
	fields := append([]SnapshotField(nil), spec.Fields...)
	inputFields := make(map[string]string, len(fields))
	seenCanonicalFields := make(map[string]struct{}, len(fields))
	for index := range fields {
		if !validID(fields[index].Name) {
			return nil, nil, nil, fmt.Errorf("%w: invalid snapshot field", ErrInvalid)
		}
		if fields[index].CanonicalFieldID == "" {
			fields[index].CanonicalFieldID = fields[index].Name
		}
		if !validID(fields[index].CanonicalFieldID) {
			return nil, nil, nil, fmt.Errorf("%w: invalid canonical snapshot field", ErrInvalid)
		}
		canonicalType, err := exposure.CanonicalSQLTypeV2(fields[index].SQLType)
		if err != nil {
			return nil, nil, nil, err
		}
		fields[index].SQLType = canonicalType
		if _, duplicate := seenCanonicalFields[fields[index].CanonicalFieldID]; duplicate {
			return nil, nil, nil, fmt.Errorf("%w: duplicate canonical snapshot field", ErrInvalid)
		}
		if existingType, present := inputFields[fields[index].Name]; present && existingType != canonicalType {
			return nil, nil, nil, fmt.Errorf("%w: one input field has conflicting SQL types", ErrInvalid)
		}
		inputFields[fields[index].Name] = canonicalType
		seenCanonicalFields[fields[index].CanonicalFieldID] = struct{}{}
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].CanonicalFieldID < fields[j].CanonicalFieldID })

	rows := append([]SnapshotRow(nil), spec.Rows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].EntityKey < rows[j].EntityKey })
	for index, row := range rows {
		if !validID(row.EntityKey) || (index > 0 && row.EntityKey == rows[index-1].EntityKey) {
			return nil, nil, nil, fmt.Errorf("%w: missing or duplicate canonical entity key", ErrInvalid)
		}
		if len(row.Values) != len(inputFields) {
			return nil, nil, nil, fmt.Errorf("%w: snapshot row field set differs from schema", ErrInvalid)
		}
		for _, field := range fields {
			_, present := row.Values[field.Name]
			if !present {
				return nil, nil, nil, fmt.Errorf("%w: snapshot row misses field", ErrInvalid)
			}
		}
	}

	dictionarySpec := DictionarySpec{SourceID: spec.SourceID, SourceNamespace: spec.SourceNamespace,
		Snapshot: spec.Snapshot, SchemaDigest: spec.SchemaDigest, ColdPayloadDigest: spec.ColdPayloadDigest}
	state, err := newDictionaryCompileState(dictionarySpec, segmentCapacity, len(fields)+1)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := state.addSegment(SegmentSpec{ID: "row", Kind: SegmentBaseRow}, len(rows),
		func(index int) (exposure.FactID, error) {
			return exposure.NewBaseRowFactV2(spec.SourceNamespace, spec.Snapshot, rows[index].EntityKey)
		}); err != nil {
		return nil, nil, nil, err
	}
	for _, field := range fields {
		currentField := field
		if err := state.addSegment(SegmentSpec{ID: "cell:" + currentField.CanonicalFieldID,
			Kind: SegmentBaseCell, Field: currentField.CanonicalFieldID}, len(rows),
			func(index int) (exposure.FactID, error) {
				row := rows[index]
				return exposure.NewBaseCellFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey,
					currentField.CanonicalFieldID, currentField.SQLType, row.Values[currentField.Name])
			}); err != nil {
			return nil, nil, nil, err
		}
	}
	dictionary, err := state.finishWithLookup(buildLookup)
	if err != nil {
		return nil, nil, nil, err
	}
	if spec.ColdPayloadDigest != "" && spec.ColdPayloadDigest != dictionary.manifest.ColdPayloadDigest {
		return nil, nil, nil, fmt.Errorf("%w: compiled cold payload digest", ErrDigestMismatch)
	}
	return dictionary, fields, rows, nil
}

func attachDictionarySnapshotRows(dictionary *Dictionary, spec SnapshotSpec, fields []SnapshotField, rows []SnapshotRow) error {
	dictionary.entityToHandle = make(map[string]RowHandle, len(rows))
	dictionary.rows = make(map[RowHandle]RowRefs, len(rows))
	for index, row := range rows {
		handle := RowHandle(index + 1)
		rowFact, _ := exposure.NewBaseRowFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey)
		rowRef, found, lookupErr := dictionary.lookupSegmentFact(rowFact, SegmentBaseRow, "")
		if lookupErr != nil || !found {
			return fmt.Errorf("%w: compiled row fact is missing", ErrInvalid)
		}
		refs := RowRefs{Handle: handle, EntityKey: row.EntityKey, Row: rowRef, Cells: make(map[string]FactRef, len(fields))}
		for _, field := range fields {
			fact, factErr := exposure.NewBaseCellFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey,
				field.CanonicalFieldID, field.SQLType, row.Values[field.Name])
			if factErr != nil {
				return factErr
			}
			ref, cellFound, cellErr := dictionary.lookupSegmentFact(fact, SegmentBaseCell, field.CanonicalFieldID)
			if cellErr != nil || !cellFound {
				return fmt.Errorf("%w: compiled cell fact is missing", ErrInvalid)
			}
			refs.Cells[field.CanonicalFieldID] = ref
		}
		dictionary.entityToHandle[row.EntityKey] = handle
		dictionary.rows[handle] = refs
	}
	return nil
}

func finalizeDictionarySnapshot(dictionary *Dictionary, expectedSidecarDigest string) error {
	sidecarDigest := digestSidecarRows(dictionary.rows)
	if expectedSidecarDigest != "" && expectedSidecarDigest != sidecarDigest {
		return fmt.Errorf("%w: compiled sidecar digest", ErrDigestMismatch)
	}
	dictionary.manifest.SidecarDigest = sidecarDigest
	dictionary.manifest.HotIndexDigest = digestHotDictionary(dictionary.DictionaryDigest(), dictionary.manifest.Segments, dictionary.segments, dictionary.rows)
	manifestDigest, err := dictionary.manifest.Digest()
	if err != nil {
		return err
	}
	dictionary.digest = manifestDigest
	return nil
}

// CompileSnapshotArtifact is the production-facing compiler entrypoint. The
// combined oracle dictionary is released after the hot/cold split. Unlike the
// reference CompileSnapshot path, production projects rows directly into the
// compact HOT layout and never materializes one map[string]FactRef per row.
func CompileSnapshotArtifact(spec SnapshotSpec) (CompiledArtifact, error) {
	dictionary, fields, rows, err := compileSnapshotDictionary(spec, maxOrdinalSegmentFacts, false)
	if err != nil {
		return CompiledArtifact{}, err
	}
	layout, err := buildCompactSnapshotLayout(dictionary, spec, fields, rows)
	if err != nil {
		return CompiledArtifact{}, err
	}
	// Lookup is only a compiler aid. Drop its million-scale reverse map before
	// copying entry headers into the independently owned HOT/COLD artifacts.
	dictionary.byHash = nil
	artifact, err := dictionary.splitOwned()
	if err != nil {
		return CompiledArtifact{}, err
	}
	artifact.Hot.entityToHandle = layout.entityToHandle
	artifact.Hot.rowSegment = layout.rowSegment
	artifact.Hot.fields = layout.fields
	artifact.Hot.rows = layout.rows
	if err := finalizeSnapshotArtifact(artifact, spec.SidecarDigest); err != nil {
		return CompiledArtifact{}, err
	}
	return artifact, nil
}

type compactSnapshotLayout struct {
	entityToHandle map[string]RowHandle
	rowSegment     string
	fields         []hotField
	rows           []hotRow
}

func buildCompactSnapshotLayout(dictionary *Dictionary, spec SnapshotSpec, fields []SnapshotField,
	rows []SnapshotRow) (compactSnapshotLayout, error) {
	layout := compactSnapshotLayout{entityToHandle: make(map[string]RowHandle, len(rows)), rows: make([]hotRow, len(rows))}
	if len(rows) == 0 {
		return layout, nil
	}
	layout.fields = make([]hotField, len(fields))
	for index, field := range fields {
		layout.fields[index].name = field.CanonicalFieldID
	}
	for index, row := range rows {
		handle := RowHandle(index + 1)
		rowFact, _ := exposure.NewBaseRowFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey)
		rowRef, found, lookupErr := dictionary.lookupSegmentFact(rowFact, SegmentBaseRow, "")
		if lookupErr != nil || !found {
			return compactSnapshotLayout{}, fmt.Errorf("%w: compiled row fact is missing", ErrInvalid)
		}
		compact := hotRow{entityKey: row.EntityKey, rowSegmentID: rowRef.SegmentID, rowOrdinal: rowRef.Ordinal,
			cellSegmentIDs: make([]string, len(fields)), cellOrdinals: make([]uint32, len(fields))}
		for fieldIndex, field := range fields {
			fact, factErr := exposure.NewBaseCellFactV2(spec.SourceNamespace, spec.Snapshot, row.EntityKey,
				field.CanonicalFieldID, field.SQLType, row.Values[field.Name])
			if factErr != nil {
				return compactSnapshotLayout{}, factErr
			}
			ref, cellFound, cellErr := dictionary.lookupSegmentFact(fact, SegmentBaseCell, field.CanonicalFieldID)
			if cellErr != nil || !cellFound {
				return compactSnapshotLayout{}, fmt.Errorf("%w: compiled cell fact is missing", ErrInvalid)
			}
			compact.cellSegmentIDs[fieldIndex] = ref.SegmentID
			compact.cellOrdinals[fieldIndex] = ref.Ordinal
			if index == 0 {
				layout.fields[fieldIndex].segmentID = ref.SegmentID
			} else if layout.fields[fieldIndex].segmentID != ref.SegmentID {
				layout.fields[fieldIndex].segmentID = ""
			}
		}
		if index == 0 {
			layout.rowSegment = rowRef.SegmentID
		} else if layout.rowSegment != rowRef.SegmentID {
			layout.rowSegment = ""
		}
		layout.entityToHandle[row.EntityKey] = handle
		layout.rows[index] = compact
	}
	if layout.rowSegment != "" {
		for index := range layout.rows {
			layout.rows[index].rowSegmentID = ""
		}
	}
	for fieldIndex, field := range layout.fields {
		if field.segmentID == "" {
			continue
		}
		for rowIndex := range layout.rows {
			layout.rows[rowIndex].cellSegmentIDs[fieldIndex] = ""
		}
	}
	return layout, nil
}

func finalizeSnapshotArtifact(artifact CompiledArtifact, expectedSidecarDigest string) error {
	sidecarDigest := artifact.Hot.sidecarDigest()
	if expectedSidecarDigest != "" && expectedSidecarDigest != sidecarDigest {
		return fmt.Errorf("%w: compiled sidecar digest", ErrDigestMismatch)
	}
	manifest := artifact.Hot.manifest
	manifest.SidecarDigest = sidecarDigest
	artifact.Hot.manifest = manifest
	manifest.HotIndexDigest = artifact.Hot.hotIndexDigest()
	manifestDigest, err := manifest.Digest()
	if err != nil {
		return err
	}
	artifact.Hot.manifest = manifest
	artifact.Hot.manifestDigest = manifestDigest
	artifact.Cold.manifest = manifest
	artifact.Cold.manifestDigest = manifestDigest
	return nil
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

func (d *Dictionary) LookupRowIdentity(handle RowHandle) (string, FactRef, bool) {
	if d == nil || handle == 0 {
		return "", FactRef{}, false
	}
	row, found := d.rows[handle]
	if !found {
		return "", FactRef{}, false
	}
	return row.EntityKey, row.Row, true
}

func (d *Dictionary) LookupCellRef(handle RowHandle, fieldID string) (FactRef, bool) {
	if d == nil || handle == 0 {
		return FactRef{}, false
	}
	row, found := d.rows[handle]
	if !found {
		return FactRef{}, false
	}
	ref, found := row.Cells[fieldID]
	return ref, found
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
