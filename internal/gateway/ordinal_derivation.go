package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"taskbound.local/agent-data-gateway/internal/control"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
)

// ordinalEffect is the representation-independent V4 result produced by the
// streaming provenance sink. Only base facts occupy the immutable bitmap;
// derived releases and the single proposition-bearing outcome remain sparse.
type ordinalEffect struct {
	Release        ordinal.BitmapSet
	Influence      ordinal.BitmapSet
	DerivedRelease []exposure.FactID
	Outcome        exposure.FactID
}

// ordinalDerivationSink delays construction until QueryPairStream has
// buffered the visible statement inside its repeatable-read transaction.
type ordinalDerivationSink struct {
	program    queryplan.OrdinalProgram
	indexes    map[string]ordinal.SnapshotIndex
	planDigest string
	deriver    *ordinalDeriver
}

func (s *ordinalDerivationSink) VisibleResult(_ context.Context, visible dataconnector.Result) error {
	if s == nil || s.deriver != nil {
		return errors.New("ordinal visible result arrived more than once")
	}
	deriver, err := newOrdinalDeriver(s.program, s.indexes, visible, s.planDigest)
	if err != nil {
		return err
	}
	s.deriver = deriver
	return nil
}

func (s *ordinalDerivationSink) Begin(ctx context.Context, columns []dataconnector.Column) error {
	if s == nil || s.deriver == nil {
		return errors.New("ordinal provenance began before the visible result")
	}
	return s.deriver.Begin(ctx, columns)
}

func (s *ordinalDerivationSink) Row(ctx context.Context, values []any) error {
	if s == nil || s.deriver == nil {
		return errors.New("ordinal provenance row arrived before initialization")
	}
	return s.deriver.Row(ctx, values)
}

func (s *ordinalDerivationSink) Finish() (ordinalEffect, error) {
	if s == nil || s.deriver == nil {
		return ordinalEffect{}, errors.New("ordinal derivation did not observe a visible result")
	}
	return s.deriver.Finish()
}

// ordinalDeriver consumes the provenance companion one row at a time. It is
// deliberately query-private: no prefix may escape if QueryPairStream rolls
// back or reports an error.
type ordinalDeriver struct {
	program            queryplan.OrdinalProgram
	indexes            map[string]ordinal.SnapshotIndex
	multi              *ordinal.MultiIndex
	visible            dataconnector.Result
	visiblePos         map[string]int
	provenance         map[string]int
	planDigest         string
	bundle             []exposure.SnapshotBinding
	grouped            bool
	leafFields         map[string][]string
	outerFields        []string
	visibleRows        map[string]ordinalVisibleRow
	groupKeys          map[string]string
	lastGroupSignature string
	lastGroupKey       string
	seenGroups         map[string]struct{}
	current            *ordinalGroupAccumulator

	currentUnion *ordinalUnionAccumulator
	closedGroup  map[string]struct{}
	visibleIndex int
	memberCount  int64

	release   *ordinal.Builder
	influence *ordinal.Builder
	facts     exposure.FactSet
	finished  bool
	effect    ordinalEffect
}

type ordinalVisibleRow struct {
	values []any
	key    string
}

type ordinalCellState struct {
	ref       ordinal.FactRef
	value     any
	binding   queryplan.OrdinalFieldBinding
	entityKey string
	namespace string
	snapshot  string
	witness   ordinalWitness
	baseRef   ordinal.FactRef
	hasBase   bool
}

type ordinalMember struct {
	key      string
	groupKey string
	row      ordinalWitness
	cells    map[string]ordinalCellState
}

// ordinalWitness keeps the common per-row witness as a tiny direct list. It
// materializes a Roaring-backed WeightedSet only at an alternative-proof or
// output-group boundary. This avoids constructing and cloning several tiny
// bitmap maps for every provenance row while retaining exact multiplicity.
type ordinalWitness struct {
	first        ordinal.WeightedItem
	rest         []ordinal.WeightedItem
	hasFirst     bool
	weighted     ordinal.WeightedSet
	materialized bool
}

func directOrdinalWitness(ref ordinal.FactRef) ordinalWitness {
	return ordinalWitness{first: ordinal.WeightedItem{Ref: ref, Multiplicity: 1}, hasFirst: true}
}

func (w *ordinalWitness) addRef(ref ordinal.FactRef, multiplicity uint64) error {
	if w == nil || w.materialized || multiplicity == 0 {
		return fmt.Errorf("invalid direct ordinal witness")
	}
	item := ordinal.WeightedItem{Ref: ref, Multiplicity: multiplicity}
	if !w.hasFirst {
		w.first = item
		w.hasFirst = true
		return nil
	}
	w.rest = append(w.rest, item)
	return nil
}

func (w *ordinalWitness) appendDirect(other ordinalWitness) error {
	if w == nil || w.materialized || other.materialized {
		return fmt.Errorf("invalid direct ordinal witness append")
	}
	if other.hasFirst {
		if err := w.addRef(other.first.Ref, other.first.Multiplicity); err != nil {
			return err
		}
	}
	for _, item := range other.rest {
		if err := w.addRef(item.Ref, item.Multiplicity); err != nil {
			return err
		}
	}
	return nil
}

func (w ordinalWitness) addTo(builder *ordinal.WeightedBuilder, scale uint64) error {
	if builder == nil || scale == 0 {
		return fmt.Errorf("invalid ordinal witness target")
	}
	if w.materialized {
		return builder.AddWeighted(w.weighted, scale)
	}
	if w.hasFirst {
		if w.first.Multiplicity > ^uint64(0)/scale {
			return ordinal.ErrMultiplicityOverflow
		}
		if err := builder.AddRef(w.first.Ref, w.first.Multiplicity*scale); err != nil {
			return err
		}
	}
	for _, item := range w.rest {
		if item.Multiplicity > ^uint64(0)/scale {
			return ordinal.ErrMultiplicityOverflow
		}
		if err := builder.AddRef(item.Ref, item.Multiplicity*scale); err != nil {
			return err
		}
	}
	return nil
}

func (w ordinalWitness) addSupport(builder *ordinal.Builder) error {
	if builder == nil {
		return fmt.Errorf("invalid ordinal support target")
	}
	if w.materialized {
		var result error
		w.weighted.Refs(func(ref ordinal.FactRef, _ uint64) bool {
			result = builder.Add(ref)
			return result == nil
		})
		return result
	}
	if w.hasFirst {
		if err := builder.Add(w.first.Ref); err != nil {
			return err
		}
	}
	for _, item := range w.rest {
		if err := builder.Add(item.Ref); err != nil {
			return err
		}
	}
	return nil
}

func (w ordinalWitness) freeze() (ordinal.WeightedSet, error) {
	if w.materialized {
		return w.weighted, nil
	}
	builder := ordinal.NewWeightedBuilder()
	if err := w.addTo(builder, 1); err != nil {
		return ordinal.WeightedSet{}, err
	}
	return builder.Freeze()
}

func addOrdinalWitness(left, right ordinalWitness) (ordinalWitness, error) {
	if !left.materialized && !right.materialized {
		var direct ordinalWitness
		if err := direct.appendDirect(left); err != nil {
			return ordinalWitness{}, err
		}
		if err := direct.appendDirect(right); err != nil {
			return ordinalWitness{}, err
		}
		return direct, nil
	}
	builder := ordinal.NewWeightedBuilder()
	if err := left.addTo(builder, 1); err != nil {
		return ordinalWitness{}, err
	}
	if err := right.addTo(builder, 1); err != nil {
		return ordinalWitness{}, err
	}
	weighted, err := builder.Freeze()
	return ordinalWitness{weighted: weighted, materialized: true}, err
}

type ordinalSourceMember struct {
	source queryplan.OrdinalSource
	row    ordinal.RowRefs
	state  ordinalMember
}

type ordinalGroupAccumulator struct {
	key     string
	visible ordinalVisibleRow
	targets map[string]*ordinal.WeightedBuilder
	members int64
}

type ordinalUnionAccumulator struct {
	key       string
	groupKey  string
	row       *ordinal.WeightedBuilder
	cells     map[string]*ordinal.WeightedBuilder
	cellState map[string]ordinalCellState
	members   int64
}

func newOrdinalDeriver(program queryplan.OrdinalProgram, indexes map[string]ordinal.SnapshotIndex,
	visible dataconnector.Result, planDigest string) (*ordinalDeriver, error) {
	if err := program.ValidateBoundSidecars(); err != nil {
		return nil, err
	}
	if len(indexes) == 0 || len(planDigest) != sha256.Size*2 {
		return nil, errors.New("ordinal derivation lacks indexes or normal-form digest")
	}
	visiblePos, err := columnPositions(visible.Columns)
	if err != nil {
		return nil, err
	}
	if visible.RowCount != int64(len(visible.Rows)) {
		return nil, errors.New("visible row count differs from buffered rows")
	}
	uniqueIndexes := make(map[string]ordinal.SnapshotIndex)
	for _, source := range program.Sources {
		index := indexes[source.SourceAlias]
		if index == nil || index.ManifestDigest() != source.SidecarBinding.ManifestDigest ||
			index.Manifest().SourceNamespace != source.SourceNamespace || index.Manifest().Snapshot != source.Snapshot {
			return nil, fmt.Errorf("ordinal source %q has no matching snapshot index", source.SourceAlias)
		}
		uniqueIndexes[index.DictionaryDigest()] = index
	}
	orderedIndexes := make([]ordinal.SnapshotIndex, 0, len(uniqueIndexes))
	for _, index := range uniqueIndexes {
		orderedIndexes = append(orderedIndexes, index)
	}
	sort.Slice(orderedIndexes, func(i, j int) bool {
		return orderedIndexes[i].DictionaryDigest() < orderedIndexes[j].DictionaryDigest()
	})
	multi, err := ordinal.NewMultiIndex(orderedIndexes...)
	if err != nil {
		return nil, err
	}
	bundle, err := ordinalSnapshotBundle(program.SnapshotBundle)
	if err != nil {
		return nil, err
	}
	result := &ordinalDeriver{
		program: program, indexes: indexes, multi: multi, visible: visible, visiblePos: visiblePos,
		planDigest: planDigest, bundle: bundle,
		grouped:     len(program.Groups) != 0 || len(program.Aggregates) != 0,
		leafFields:  make(map[string][]string, len(program.Sources)),
		outerFields: uniqueOrdinalPredicateFields(program.OuterPredicates),
		release:     ordinal.NewBuilder(), influence: ordinal.NewBuilder(), facts: make(exposure.FactSet),
		visibleRows: make(map[string]ordinalVisibleRow), groupKeys: make(map[string]string),
		seenGroups: make(map[string]struct{}), closedGroup: make(map[string]struct{}),
	}
	for _, source := range program.Sources {
		result.leafFields[source.SourceAlias] = uniqueOrdinalPredicateFields(source.LeafPredicates)
	}
	if err := result.prepareVisibleRows(); err != nil {
		return nil, err
	}
	return result, nil
}

func ordinalSnapshotBundle(input []queryplan.OrdinalSnapshotBinding) ([]exposure.SnapshotBinding, error) {
	byNamespace := make(map[string]string, len(input))
	for _, binding := range input {
		if binding.SourceNamespace == "" || binding.Snapshot == "" {
			return nil, errors.New("ordinal snapshot bundle is incomplete")
		}
		if previous, present := byNamespace[binding.SourceNamespace]; present && previous != binding.Snapshot {
			return nil, errors.New("ordinal snapshot bundle binds one namespace twice")
		}
		byNamespace[binding.SourceNamespace] = binding.Snapshot
	}
	result := make([]exposure.SnapshotBinding, 0, len(byNamespace))
	for namespace, snapshot := range byNamespace {
		result = append(result, exposure.SnapshotBinding{SourceNamespace: namespace, Snapshot: snapshot})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SourceNamespace < result[j].SourceNamespace })
	if len(result) == 0 {
		return nil, errors.New("ordinal snapshot bundle is empty")
	}
	return result, nil
}

func (d *ordinalDeriver) prepareVisibleRows() error {
	if !d.grouped {
		return nil
	}
	if len(d.program.Groups) == 0 && len(d.visible.Rows) != 1 {
		return errors.New("a global aggregate must return exactly one visible row")
	}
	for _, values := range d.visible.Rows {
		key, err := d.visibleGroupKey(values)
		if err != nil {
			return err
		}
		if _, duplicate := d.visibleRows[key]; duplicate {
			return errors.New("visible aggregate contains a duplicate canonical group")
		}
		d.visibleRows[key] = ordinalVisibleRow{values: append([]any(nil), values...), key: key}
	}
	return nil
}

func (d *ordinalDeriver) visibleGroupKey(row []any) (string, error) {
	components := make([]string, 0, len(d.program.Groups))
	for _, group := range d.program.Groups {
		position, present := d.visiblePos[group.ResultAlias]
		if !present || position >= len(row) {
			return "", fmt.Errorf("visible result omits group field %q", group.ResultAlias)
		}
		binding, bound := ordinalBinding(d.program, group.Field.FieldID)
		if !bound {
			return "", fmt.Errorf("group field %q has no canonical binding", group.Field.FieldID)
		}
		canonical, err := exposure.CanonicalSQLValue(binding.SQLType, row[position])
		if err != nil {
			return "", err
		}
		components = append(components, binding.CanonicalExpression+"\x00"+binding.SQLType+"\x00"+canonical)
	}
	if len(components) == 0 {
		components = []string{"global"}
	}
	return d.canonicalGroupKey(components, true)
}

// Begin implements dataconnector.ProvenanceSink.
func (d *ordinalDeriver) Begin(_ context.Context, columns []dataconnector.Column) error {
	if d == nil || d.provenance != nil || d.finished {
		return errors.New("ordinal provenance sink began more than once")
	}
	positions, err := columnPositions(columns)
	if err != nil {
		return err
	}
	fields := make([]string, 0, len(columns))
	for _, column := range columns {
		fields = append(fields, column.Name)
	}
	if err := d.program.ValidateProvenanceFields(fields); err != nil {
		return err
	}
	if d.program.Kind == "union_distinct" {
		if _, present := positions["tg_branch"]; !present {
			return errors.New("UNION DISTINCT provenance omits branch identity")
		}
	}
	d.provenance = positions
	return nil
}

// Row implements dataconnector.ProvenanceSink.
func (d *ordinalDeriver) Row(_ context.Context, values []any) error {
	if d == nil || d.provenance == nil || d.finished {
		return errors.New("ordinal provenance row arrived outside an active stream")
	}
	member, err := d.buildMember(values)
	if err != nil {
		return err
	}
	if d.program.Kind == "union_distinct" {
		return d.acceptUnionCandidate(member)
	}
	return d.acceptMember(member)
}

func (d *ordinalDeriver) buildMember(values []any) (ordinalMember, error) {
	active := d.program.Sources
	branch := -1
	if d.program.Kind == "union_distinct" {
		position, present := d.provenance["tg_branch"]
		if !present || position >= len(values) {
			return ordinalMember{}, errors.New("UNION DISTINCT provenance row omits branch identity")
		}
		value, err := ordinalBranchNumber(values[position])
		if err != nil {
			return ordinalMember{}, err
		}
		branch = value
		active = make([]queryplan.OrdinalSource, 0, len(d.program.Sources))
		for _, source := range d.program.Sources {
			if source.Branch < 0 || source.Branch == branch {
				active = append(active, source)
			}
		}
	}
	if len(active) == 0 || (d.program.Kind != "union_distinct" && len(active) != len(d.program.Sources)) {
		return ordinalMember{}, errors.New("provenance row has no exact source binding")
	}
	if d.program.Kind == "scan" && len(active) == 1 {
		source, err := d.buildSourceMember(active[0], values)
		if err != nil {
			return ordinalMember{}, err
		}
		member := source.state
		member.key = source.row.EntityKey
		member.groupKey, err = d.memberGroupKey(member)
		return member, err
	}
	sources := make([]ordinalSourceMember, 0, len(active))
	for _, source := range active {
		state, err := d.buildSourceMember(source, values)
		if err != nil {
			return ordinalMember{}, err
		}
		sources = append(sources, state)
	}

	cellCapacity := 0
	for _, source := range sources {
		cellCapacity += len(source.state.cells)
	}
	// Keep every source map isolated. Besides making ownership explicit, this
	// preserves fail-closed source-local witness lookup even if a malformed
	// program somehow crosses the normalization boundary.
	member := ordinalMember{cells: make(map[string]ordinalCellState, cellCapacity)}
	for _, source := range sources {
		if err := member.row.appendDirect(source.state.row); err != nil {
			return ordinalMember{}, err
		}
		for field, cell := range source.state.cells {
			if previous, duplicate := member.cells[field]; duplicate && previous.ref != cell.ref {
				return ordinalMember{}, fmt.Errorf("field %q has ambiguous source facts", field)
			}
			member.cells[field] = cell
		}
	}
	if d.program.Kind == "join" {
		for _, source := range sources {
			for _, use := range source.source.JoinKeyFields {
				cell, present := source.state.cells[use.FieldID]
				if !present || member.row.addRef(cell.ref, use.Multiplicity) != nil {
					return ordinalMember{}, fmt.Errorf("join dependency field %q is unavailable", use.FieldID)
				}
			}
		}
		// Ungrouped observations bind derived outputs to the canonical joined-row
		// identity. Grouped observations bind them to member.groupKey in
		// flushGroup, so materializing the joined-row identity here has no
		// semantic consumer and is avoidable work on every provenance row.
		if !d.grouped {
			key, err := ordinalJoinRowKey(sources)
			if err != nil {
				return ordinalMember{}, err
			}
			member.key = key
		}
	} else {
		member.key = sources[0].row.EntityKey
	}
	if d.program.Kind == "union_distinct" {
		for _, use := range sources[0].source.UnionKeyFields {
			cell, present := sources[0].state.cells[use.FieldID]
			if !present || member.row.addRef(cell.ref, use.Multiplicity) != nil {
				return ordinalMember{}, fmt.Errorf("union dependency field %q is unavailable", use.FieldID)
			}
		}
		key, err := ordinalUnionRowKey(sources[0].source, sources[0].state.cells)
		if err != nil {
			return ordinalMember{}, err
		}
		member.key = key
	}
	groupKey, err := d.memberGroupKey(member)
	member.groupKey = groupKey
	return member, err
}

func (d *ordinalDeriver) buildSourceMember(source queryplan.OrdinalSource, values []any) (ordinalSourceMember, error) {
	position, present := d.provenance[source.HandleAlias]
	if !present || position >= len(values) {
		return ordinalSourceMember{}, fmt.Errorf("source %q omits row handle", source.SourceAlias)
	}
	handle, err := ordinalRowHandle(values[position])
	if err != nil || handle == 0 {
		return ordinalSourceMember{}, fmt.Errorf("source %q has an invalid row handle", source.SourceAlias)
	}
	index := d.indexes[source.SourceAlias]
	row := ordinal.RowRefs{Handle: handle}
	// Only the two audited concrete dictionary implementations may bypass the
	// defensive full-row lookup. A decorator embedding *HotDictionary inherits
	// its projection methods through Go method promotion even when it overrides
	// LookupRow for validation/fault injection; accepting the interface by
	// assertion would silently bypass that override.
	var projected ordinal.ProjectedSnapshotIndex
	switch trusted := index.(type) {
	case *ordinal.HotDictionary:
		projected = trusted
	case *ordinal.Dictionary:
		projected = trusted
	}
	projectedLookup := projected != nil
	if projectedLookup {
		var found bool
		row.EntityKey, row.Row, found = projected.LookupRowIdentity(handle)
		if !found || row.EntityKey == "" {
			return ordinalSourceMember{}, fmt.Errorf("source %q references an unknown row handle", source.SourceAlias)
		}
	} else {
		var found bool
		row, found = index.LookupRow(handle)
		if !found || row.Handle != handle || row.EntityKey == "" {
			return ordinalSourceMember{}, fmt.Errorf("source %q references an unknown row handle", source.SourceAlias)
		}
	}
	entityKey, err := d.sourceEntityKey(source, values)
	if err != nil {
		return ordinalSourceMember{}, err
	}
	if entityKey != row.EntityKey {
		return ordinalSourceMember{}, fmt.Errorf("source %q row handle does not match its provenance entity key", source.SourceAlias)
	}
	state := ordinalMember{key: row.EntityKey, row: directOrdinalWitness(row.Row),
		cells: make(map[string]ordinalCellState, len(source.EvidenceFields))}
	for _, binding := range source.EvidenceFields {
		var ref ordinal.FactRef
		var found bool
		if projectedLookup {
			ref, found = projected.LookupCellRef(handle, binding.FieldID)
		} else {
			ref, found = row.Cells[binding.FieldID]
		}
		fieldPosition, positioned := d.provenance[binding.ProvenanceAlias]
		if !found || !positioned || fieldPosition >= len(values) {
			return ordinalSourceMember{}, fmt.Errorf("source %q has no ordinal/value for %q", source.SourceAlias, binding.FieldID)
		}
		state.cells[binding.FieldID] = ordinalCellState{ref: ref, value: values[fieldPosition], binding: binding,
			entityKey: row.EntityKey, namespace: source.SourceNamespace, snapshot: source.Snapshot,
			witness: directOrdinalWitness(ref), baseRef: ref, hasBase: true}
	}
	for _, fieldID := range d.leafFields[source.SourceAlias] {
		cell, found := state.cells[fieldID]
		if !found || state.row.addRef(cell.ref, 1) != nil {
			return ordinalSourceMember{}, fmt.Errorf("leaf dependency field %q is unavailable", fieldID)
		}
	}
	return ordinalSourceMember{source: source, row: row, state: state}, nil
}

// sourceEntityKey independently reconstructs the sidecar lookup identity from
// the typed provenance values. A handle is only an efficient reference; it is
// not authority to substitute another valid row in the same publication.
func (d *ordinalDeriver) sourceEntityKey(source queryplan.OrdinalSource, values []any) (string, error) {
	components := make([]string, 0, 1+3*len(source.EntityKey))
	components = append(components, source.SourceNamespace)
	for _, binding := range source.EntityKey {
		position, present := d.provenance[binding.ProvenanceAlias]
		if !present || position >= len(values) {
			return "", fmt.Errorf("source %q omits entity-key field %q", source.SourceAlias, binding.FieldID)
		}
		canonical, err := exposure.CanonicalSQLValue(binding.SQLType, values[position])
		if err != nil {
			return "", fmt.Errorf("source %q has a non-canonical entity-key field %q: %w", source.SourceAlias, binding.FieldID, err)
		}
		components = append(components, binding.Column, binding.SQLType, canonical)
	}
	key, err := exposure.ComposeCanonicalKeyV2("base-entity", components...)
	if err != nil {
		return "", fmt.Errorf("source %q entity key: %w", source.SourceAlias, err)
	}
	return key, nil
}

func (d *ordinalDeriver) memberGroupKey(member ordinalMember) (string, error) {
	components := make([]string, 0, len(d.program.Groups))
	for _, group := range d.program.Groups {
		cell, present := member.cells[group.Field.FieldID]
		if !present {
			return "", fmt.Errorf("group field %q is unavailable", group.Field.FieldID)
		}
		canonical, err := exposure.CanonicalSQLValue(cell.binding.SQLType, cell.value)
		if err != nil {
			return "", err
		}
		components = append(components, cell.binding.CanonicalExpression+"\x00"+cell.binding.SQLType+"\x00"+canonical)
	}
	if len(components) == 0 {
		components = []string{"global"}
	}
	return d.canonicalGroupKey(components, false)
}

// canonicalGroupKey memoizes visible output groups plus one most-recent
// provenance group. Large aggregate companions commonly contain hundreds of
// thousands of members but only a bounded visible result page. Retaining only
// visible signatures prevents a high-cardinality page from rebuilding a
// million-entry heap map, while the one-entry cache still makes a canonically
// ordered group's repeated members O(1). The signature is length-prefixed, so
// lookup cannot introduce delimiter ambiguity; the returned identity remains
// the exact ComposeCanonicalKeyV2 value used by the reference algorithm.
func (d *ordinalDeriver) canonicalGroupKey(components []string, retainVisible bool) (string, error) {
	sort.Strings(components)
	var signature strings.Builder
	for _, component := range components {
		signature.WriteString(strconv.Itoa(len(component)))
		signature.WriteByte(':')
		signature.WriteString(component)
	}
	cacheKey := signature.String()
	if key, present := d.groupKeys[cacheKey]; present {
		return key, nil
	}
	if cacheKey == d.lastGroupSignature {
		return d.lastGroupKey, nil
	}
	key, err := exposure.ComposeCanonicalKeyV2("group-row", components...)
	if err == nil {
		if retainVisible {
			d.groupKeys[cacheKey] = key
		} else {
			d.lastGroupSignature = cacheKey
			d.lastGroupKey = key
		}
	}
	return key, err
}

func (d *ordinalDeriver) acceptUnionCandidate(candidate ordinalMember) error {
	if d.currentUnion != nil && (d.currentUnion.key != candidate.key || d.currentUnion.groupKey != candidate.groupKey) {
		if err := d.flushUnion(); err != nil {
			return err
		}
	}
	if d.currentUnion == nil {
		d.currentUnion = &ordinalUnionAccumulator{key: candidate.key, groupKey: candidate.groupKey,
			row: ordinal.NewWeightedBuilder(), cells: make(map[string]*ordinal.WeightedBuilder),
			cellState: make(map[string]ordinalCellState)}
	}
	row, err := candidate.row.freeze()
	if err != nil {
		return err
	}
	if err := d.currentUnion.row.MaxWeighted(row); err != nil {
		return err
	}
	for field, cell := range candidate.cells {
		builder := d.currentUnion.cells[field]
		if builder == nil {
			builder = ordinal.NewWeightedBuilder()
			d.currentUnion.cells[field] = builder
			d.currentUnion.cellState[field] = cell
		}
		witness, err := cell.witness.freeze()
		if err != nil {
			return err
		}
		if err := builder.MaxWeighted(witness); err != nil {
			return err
		}
		previous := d.currentUnion.cellState[field]
		if previous.hasBase && previous.baseRef != cell.ref {
			previous.hasBase = false
			d.currentUnion.cellState[field] = previous
		}
	}
	d.currentUnion.members++
	return nil
}

func (d *ordinalDeriver) flushUnion() error {
	if d.currentUnion == nil {
		return nil
	}
	row, err := d.currentUnion.row.Freeze()
	if err != nil {
		return err
	}
	member := ordinalMember{key: d.currentUnion.key, groupKey: d.currentUnion.groupKey,
		row:   ordinalWitness{weighted: row, materialized: true},
		cells: make(map[string]ordinalCellState, len(d.currentUnion.cells))}
	for field, builder := range d.currentUnion.cells {
		witness, err := builder.Freeze()
		if err != nil {
			return err
		}
		cell := d.currentUnion.cellState[field]
		cell.witness = ordinalWitness{weighted: witness, materialized: true}
		member.cells[field] = cell
	}
	d.currentUnion = nil
	return d.acceptMember(member)
}

func (d *ordinalDeriver) acceptMember(member ordinalMember) error {
	row := member.row
	for _, fieldID := range d.outerFields {
		cell, present := member.cells[fieldID]
		if !present {
			return fmt.Errorf("outer dependency field %q is unavailable", fieldID)
		}
		combined, err := addOrdinalWitness(row, cell.witness)
		if err != nil {
			return err
		}
		row = combined
	}
	member.row = row
	d.memberCount++
	if !d.grouped {
		return d.acceptUngrouped(member)
	}
	return d.acceptGrouped(member)
}

func uniqueOrdinalPredicateFields(predicates []queryplan.OrdinalPredicateSpec) []string {
	seen := make(map[string]struct{}, len(predicates))
	result := make([]string, 0, len(predicates))
	for _, predicate := range predicates {
		fieldID := predicate.Field.FieldID
		if _, present := seen[fieldID]; present {
			continue
		}
		seen[fieldID] = struct{}{}
		result = append(result, fieldID)
	}
	return result
}

func (d *ordinalDeriver) acceptUngrouped(member ordinalMember) error {
	if d.program.Kind == "union_distinct" {
		visible, present := d.visibleUnionRow(member)
		if !present {
			return nil
		}
		if _, duplicate := d.seenGroups[member.key]; duplicate {
			return errors.New("UNION DISTINCT provenance emitted a non-contiguous duplicate tuple")
		}
		d.seenGroups[member.key] = struct{}{}
		return d.observeUngrouped(member, visible)
	}
	if d.visibleIndex >= len(d.visible.Rows) {
		return errors.New("provenance contains more rows than the visible result")
	}
	visible := ordinalVisibleRow{values: d.visible.Rows[d.visibleIndex]}
	d.visibleIndex++
	return d.observeUngrouped(member, visible)
}

func (d *ordinalDeriver) observeUngrouped(member ordinalMember, visible ordinalVisibleRow) error {
	if err := member.row.addSupport(d.influence); err != nil {
		return err
	}
	for _, output := range d.program.Visible {
		if output.Kind != "field" {
			return errors.New("ungrouped ordinal output unexpectedly contains an aggregate")
		}
		position, present := d.visiblePos[output.ResultAlias]
		cell, cellPresent := member.cells[output.FieldID]
		if !present || position >= len(visible.values) || !cellPresent {
			return fmt.Errorf("visible field %q cannot be matched to provenance", output.OutputID)
		}
		if err := sameCanonicalValue(output.SQLType, visible.values[position], cell.value); err != nil {
			return fmt.Errorf("visible/provenance value mismatch for %q: %w", output.OutputID, err)
		}
		if err := cell.witness.addSupport(d.influence); err != nil {
			return err
		}
		if cell.hasBase {
			fact, err := cell.baseFact()
			if err != nil {
				return err
			}
			if err := d.verifyBaseFact(cell.baseRef, fact); err != nil {
				return err
			}
			if err := d.release.Add(cell.baseRef); err != nil {
				return err
			}
			if err := d.facts.Add(fact); err != nil {
				return err
			}
			continue
		}
		witness, err := cell.witness.freeze()
		if err != nil {
			return err
		}
		commitment, err := ordinalWitnessCommitment(witness, d.multi)
		if err != nil {
			return err
		}
		fact, err := exposure.NewDerivedFactV2(d.bundle, member.key, output.CanonicalExpression,
			output.SQLType, visible.values[position], commitment)
		if err != nil {
			return err
		}
		if err := d.facts.Add(fact); err != nil {
			return err
		}
	}
	return nil
}

func (d *ordinalDeriver) visibleUnionRow(member ordinalMember) (ordinalVisibleRow, bool) {
	for _, row := range d.visible.Rows {
		key, err := d.unionKeyFromVisible(row)
		if err == nil && key == member.key {
			return ordinalVisibleRow{values: row, key: key}, true
		}
	}
	return ordinalVisibleRow{}, false
}

func (d *ordinalDeriver) unionKeyFromVisible(row []any) (string, error) {
	if len(d.program.Sources) == 0 {
		return "", errors.New("UNION DISTINCT program has no source")
	}
	values := make(map[string]ordinalCellState)
	for _, use := range d.program.Sources[0].UnionKeyFields {
		binding, present := ordinalBinding(d.program, use.FieldID)
		if !present {
			return "", fmt.Errorf("union field %q has no binding", use.FieldID)
		}
		alias := ordinalResultAlias(use.FieldID)
		position, present := d.visiblePos[alias]
		if !present || position >= len(row) {
			return "", fmt.Errorf("visible union tuple omits %q", alias)
		}
		values[use.FieldID] = ordinalCellState{value: row[position], binding: binding}
	}
	return ordinalUnionRowKey(d.program.Sources[0], values)
}

func (d *ordinalDeriver) acceptGrouped(member ordinalMember) error {
	if d.current != nil && d.current.key != member.groupKey {
		if err := d.flushGroup(); err != nil {
			return err
		}
	}
	visible, wanted := d.visibleRows[member.groupKey]
	if !wanted {
		return nil
	}
	if d.current == nil {
		if _, closed := d.closedGroup[member.groupKey]; closed {
			return errors.New("provenance group ordering is non-canonical")
		}
		d.current = &ordinalGroupAccumulator{key: member.groupKey, visible: visible, targets: make(map[string]*ordinal.WeightedBuilder)}
	}
	if err := member.row.addSupport(d.influence); err != nil {
		return err
	}
	for _, group := range d.program.Groups {
		cell, present := member.cells[group.Field.FieldID]
		if !present {
			return fmt.Errorf("group field %q has no member cell", group.Field.FieldID)
		}
		if err := cell.witness.addSupport(d.influence); err != nil {
			return err
		}
		if ordinalVisibleField(d.program.Visible, group.Field.FieldID) {
			builder := d.groupTarget(group.Field.FieldID)
			if err := cell.witness.addTo(builder, group.WitnessMultiplicity); err != nil {
				return err
			}
		}
	}
	for _, aggregate := range d.program.Aggregates {
		builder := d.groupTarget(aggregate.OutputID)
		input := member.row
		if aggregate.InputKind == "field" {
			cell, present := member.cells[aggregate.Input.FieldID]
			if !present {
				return fmt.Errorf("aggregate field %q has no member cell", aggregate.Input.FieldID)
			}
			input = cell.witness
		}
		if err := input.addTo(builder, aggregate.WitnessMultiplicity); err != nil {
			return err
		}
		if err := input.addSupport(d.influence); err != nil {
			return err
		}
	}
	d.current.members++
	return nil
}

func (d *ordinalDeriver) groupTarget(id string) *ordinal.WeightedBuilder {
	if d.current.targets[id] == nil {
		d.current.targets[id] = ordinal.NewWeightedBuilder()
	}
	return d.current.targets[id]
}

func (d *ordinalDeriver) flushGroup() error {
	if d.current == nil {
		return nil
	}
	if len(d.program.Groups) != 0 && d.current.members == 0 {
		return errors.New("visible group has no positive provenance member")
	}
	for _, output := range d.program.Visible {
		position, present := d.visiblePos[output.ResultAlias]
		if !present || position >= len(d.current.visible.values) {
			return fmt.Errorf("visible group output %q is unavailable", output.ResultAlias)
		}
		builder := d.current.targets[output.OutputID]
		if builder == nil && output.Kind == "field" {
			builder = d.current.targets[output.FieldID]
		}
		if builder == nil {
			builder = ordinal.NewWeightedBuilder()
		}
		witness, err := builder.Freeze()
		if err != nil {
			return err
		}
		commitment, err := ordinalWitnessCommitment(witness, d.multi)
		if err != nil {
			return err
		}
		expression := output.CanonicalExpression
		if output.Kind == "field" {
			for _, group := range d.program.Groups {
				if group.Field.FieldID == output.FieldID {
					expression = group.CanonicalExpression
					break
				}
			}
		}
		fact, err := exposure.NewDerivedFactV2(d.bundle, d.current.key, expression,
			output.SQLType, d.current.visible.values[position], commitment)
		if err != nil {
			return err
		}
		if err := d.facts.Add(fact); err != nil {
			return err
		}
	}
	d.seenGroups[d.current.key] = struct{}{}
	d.closedGroup[d.current.key] = struct{}{}
	d.current = nil
	return nil
}

func (d *ordinalDeriver) Finish() (ordinalEffect, error) {
	if d == nil || d.provenance == nil || d.finished {
		return ordinalEffect{}, errors.New("ordinal derivation is not finishable")
	}
	if err := d.flushUnion(); err != nil {
		return ordinalEffect{}, err
	}
	if err := d.flushGroup(); err != nil {
		return ordinalEffect{}, err
	}
	if !d.grouped && d.program.Kind != "union_distinct" && d.visibleIndex != len(d.visible.Rows) {
		return ordinalEffect{}, errors.New("visible result contains rows absent from provenance")
	}
	if !d.grouped && d.program.Kind == "union_distinct" {
		if len(d.seenGroups) != len(d.visible.Rows) {
			return ordinalEffect{}, errors.New("visible UNION DISTINCT tuples differ from provenance")
		}
		for _, row := range d.visible.Rows {
			key, err := d.unionKeyFromVisible(row)
			if err != nil {
				return ordinalEffect{}, err
			}
			if _, present := d.seenGroups[key]; !present {
				return ordinalEffect{}, errors.New("visible UNION DISTINCT tuple is absent from provenance")
			}
		}
	}
	if d.grouped {
		for key := range d.visibleRows {
			if _, present := d.seenGroups[key]; !present {
				if len(d.program.Groups) != 0 {
					return ordinalEffect{}, errors.New("visible group is absent from provenance")
				}
				// Empty global aggregates have a valid empty witness.
				d.current = &ordinalGroupAccumulator{key: key, visible: d.visibleRows[key], targets: make(map[string]*ordinal.WeightedBuilder)}
				if err := d.flushGroup(); err != nil {
					return ordinalEffect{}, err
				}
			}
		}
	}
	release, err := d.release.Freeze()
	if err != nil {
		return ordinalEffect{}, err
	}
	influence, err := d.influence.Freeze()
	if err != nil {
		return ordinalEffect{}, err
	}
	if err := d.multi.ValidateSetBounds(release.Union(influence)); err != nil {
		return ordinalEffect{}, err
	}
	releaseFacts := d.facts.Values()
	outcomeDigest, err := exposure.ReleaseOutcomeDigest(releaseFacts, d.visible.RowCount)
	if err != nil {
		return ordinalEffect{}, err
	}
	outcome, err := exposure.NewOutcomeFactV3(queryplan.NormalFormVersion, d.planDigest, outcomeDigest, d.visible.RowCount)
	if err != nil {
		return ordinalEffect{}, err
	}
	dynamic := make([]exposure.FactID, 0)
	for _, fact := range releaseFacts {
		if fact.Kind == exposure.FactDerived {
			dynamic = append(dynamic, fact)
		}
	}
	d.effect = ordinalEffect{Release: release, Influence: influence, DerivedRelease: dynamic, Outcome: outcome}
	d.finished = true
	return d.effect, nil
}

func (c ordinalCellState) baseFact() (exposure.FactID, error) {
	return exposure.NewBaseCellFactV2(c.namespace, c.snapshot, c.entityKey, c.binding.FieldID, c.binding.SQLType, c.value)
}

func (d *ordinalDeriver) verifyBaseFact(ref ordinal.FactRef, fact exposure.FactID) error {
	hashText, err := fact.Hash()
	if err != nil {
		return err
	}
	want, err := hex.DecodeString(hashText)
	if err != nil {
		return err
	}
	got, err := d.multi.Hash(ref)
	if err != nil || !bytes.Equal(want, got[:]) {
		return errors.New("snapshot ordinal does not match the canonical base fact")
	}
	return nil
}

func ordinalWitnessCommitment(witness ordinal.WeightedSet, index ordinal.FactHashStreamer) (string, error) {
	if index == nil {
		return "", errors.New("witness commitment has no hash index")
	}
	hash := sha256.New()
	writeOrdinalString(hash, "witness-multiset")
	count := witness.Len()
	if count > ^uint64(0)/2 {
		return "", ordinal.ErrMultiplicityOverflow
	}
	values := count * 2
	if count == 0 {
		values = 1
	}
	writeOrdinalUint64(hash, values)
	if count == 0 {
		writeOrdinalString(hash, "empty")
	} else if err := witness.StreamByFactHash(index, func(_ ordinal.FactRef, factHash [sha256.Size]byte, multiplicity uint64) error {
		writeOrdinalString(hash, hex.EncodeToString(factHash[:]))
		writeOrdinalString(hash, fmt.Sprintf("%020d", multiplicity))
		return nil
	}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeOrdinalString(target interface{ Write([]byte) (int, error) }, value string) {
	writeOrdinalUint64(target, uint64(len(value)))
	_, _ = target.Write([]byte(value))
}

func writeOrdinalUint64(target interface{ Write([]byte) (int, error) }, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func ordinalRowHandle(value any) (ordinal.RowHandle, error) {
	switch typed := value.(type) {
	case uint64:
		return ordinal.RowHandle(typed), nil
	case uint32:
		return ordinal.RowHandle(typed), nil
	case int64:
		if typed > 0 {
			return ordinal.RowHandle(typed), nil
		}
	case int32:
		if typed > 0 {
			return ordinal.RowHandle(typed), nil
		}
	case int:
		if typed > 0 {
			return ordinal.RowHandle(typed), nil
		}
	case string:
		parsed, err := strconv.ParseUint(typed, 10, 64)
		return ordinal.RowHandle(parsed), err
	}
	return 0, fmt.Errorf("invalid row handle type %T", value)
}

func ordinalBranchNumber(value any) (int, error) {
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int16:
		return int(typed), nil
	case int32:
		return int(typed), nil
	case int64:
		return int(typed), nil
	case string:
		return strconv.Atoi(typed)
	default:
		return 0, fmt.Errorf("invalid union branch type %T", value)
	}
}

func ordinalJoinRowKey(sources []ordinalSourceMember) (string, error) {
	identities := make([]string, 0, len(sources))
	for _, source := range sources {
		fields := make([]queryplan.OrdinalFieldBinding, len(source.source.EvidenceFields))
		copy(fields, source.source.EvidenceFields)
		normalized := ordinalNormalizedSchema(fields)
		identity, err := exposure.ComposeCanonicalKeyV2("relation-row", normalized, source.row.EntityKey,
			source.source.SourceNamespace, source.source.Snapshot)
		if err != nil {
			return "", err
		}
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	return exposure.ComposeCanonicalKeyV2("join-row", identities...)
}

func ordinalUnionRowKey(source queryplan.OrdinalSource, cells map[string]ordinalCellState) (string, error) {
	bindings := make([]queryplan.OrdinalFieldBinding, 0, len(source.UnionKeyFields))
	for _, use := range source.UnionKeyFields {
		cell, present := cells[use.FieldID]
		if !present {
			return "", fmt.Errorf("union tuple lacks %q", use.FieldID)
		}
		bindings = append(bindings, cell.binding)
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].FieldID < bindings[j].FieldID })
	components := []string{ordinalNormalizedSchema(bindings)}
	for _, binding := range bindings {
		canonical, err := exposure.CanonicalSQLValue(binding.SQLType, cells[binding.FieldID].value)
		if err != nil {
			return "", err
		}
		components = append(components, binding.SQLType, canonical)
	}
	return exposure.ComposeCanonicalKeyV2("union-distinct-row", components...)
}

func ordinalNormalizedSchema(fields []queryplan.OrdinalFieldBinding) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field.FieldID+":"+field.SQLType+":"+field.Collation+":"+field.CollationVersion+":"+strconv.FormatBool(field.Collation != ""))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

func ordinalBinding(program queryplan.OrdinalProgram, fieldID string) (queryplan.OrdinalFieldBinding, bool) {
	for _, source := range program.Sources {
		for _, binding := range source.EvidenceFields {
			if binding.FieldID == fieldID {
				return binding, true
			}
		}
	}
	return queryplan.OrdinalFieldBinding{}, false
}

func ordinalFieldType(program queryplan.OrdinalProgram, fieldID string) string {
	binding, _ := ordinalBinding(program, fieldID)
	return binding.SQLType
}

func ordinalVisibleField(outputs []queryplan.OrdinalVisibleSpec, fieldID string) bool {
	for _, output := range outputs {
		if output.Kind == "field" && output.FieldID == fieldID {
			return true
		}
	}
	return false
}

func ordinalResultAlias(fieldID string) string {
	if !strings.Contains(fieldID, ".") {
		return fieldID
	}
	digest := sha256.Sum256([]byte("taskgate-relational-output\x00" + fieldID))
	return "tg_o_" + hex.EncodeToString(digest[:8])
}

func sameCanonicalValue(sqlType string, left, right any) error {
	leftCanonical, err := exposure.CanonicalSQLValue(sqlType, left)
	if err != nil {
		return err
	}
	rightCanonical, err := exposure.CanonicalSQLValue(sqlType, right)
	if err != nil {
		return err
	}
	if leftCanonical != rightCanonical {
		return errors.New("canonical values differ")
	}
	return nil
}

func ordinalControlObservation(effect ordinalEffect, dictionarySetDigest string) (control.OrdinalExposureObservation, error) {
	derived := make([]control.OrdinalDynamicFact, 0, len(effect.DerivedRelease))
	for _, fact := range effect.DerivedRelease {
		dynamic, err := ordinalControlDynamicFact(fact, control.OrdinalDynamicDerivedRelease)
		if err != nil {
			return control.OrdinalExposureObservation{}, err
		}
		derived = append(derived, dynamic)
	}
	outcome, err := ordinalControlDynamicFact(effect.Outcome, control.OrdinalDynamicOutcome)
	if err != nil {
		return control.OrdinalExposureObservation{}, err
	}
	return control.OrdinalExposureObservation{
		ProfileVersion: exposure.ProfileV4, DictionarySetDigest: dictionarySetDigest,
		Release:   control.OrdinalHybridSet{Static: effect.Release, DynamicFacts: derived},
		Influence: control.OrdinalHybridSet{Static: effect.Influence},
		Outcome:   control.OrdinalHybridSet{DynamicFacts: []control.OrdinalDynamicFact{outcome}},
	}, nil
}

func ordinalControlDynamicFact(fact exposure.FactID, kind string) (control.OrdinalDynamicFact, error) {
	hash, err := fact.Hash()
	if err != nil {
		return control.OrdinalDynamicFact{}, err
	}
	payload, err := fact.CanonicalPayload()
	if err != nil {
		return control.OrdinalDynamicFact{}, err
	}
	return control.OrdinalDynamicFact{SHA256: hash, Kind: kind, CanonicalPayload: payload}, nil
}
