package ordinal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"sync"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	DictionaryVersion      = "taskgate-ordinal-dictionary-v1"
	dictionaryDigestDomain = "TASKGATE-ORDINAL-DICTIONARY-V1\x00"
	manifestDigestDomain   = "TASKGATE-ORDINAL-MANIFEST-V1\x00"
	segmentHashesDomain    = "TASKGATE-ORDINAL-SEGMENT-HASHES-V1\x00"
	segmentPayloadsDomain  = "TASKGATE-ORDINAL-SEGMENT-PAYLOADS-V1\x00"
	maxOrdinalSegmentFacts = uint64(math.MaxUint32) + 1
)

type SegmentKind string

const (
	SegmentBaseRow  SegmentKind = "base-row"
	SegmentBaseCell SegmentKind = "base-cell"
	SegmentDerived  SegmentKind = "derived"
	SegmentOutcome  SegmentKind = "outcome"
)

// DictionaryManifest is the immutable publication metadata. DictionaryDigest
// commits all FactID hashes and canonical payloads; Digest additionally binds
// those facts to their snapshot, source and artifact roots.
type DictionaryManifest struct {
	Version           string            `json:"version"`
	SourceID          string            `json:"source_id"`
	SourceNamespace   string            `json:"source_namespace"`
	Snapshot          string            `json:"snapshot"`
	SchemaDigest      string            `json:"schema_digest"`
	DictionaryDigest  string            `json:"dictionary_digest"`
	SidecarDigest     string            `json:"sidecar_digest"`
	ColdPayloadDigest string            `json:"cold_payload_digest"`
	HotIndexDigest    string            `json:"hot_index_digest"`
	Segments          []SegmentManifest `json:"segments"`
}

type SegmentManifest struct {
	ID             string      `json:"id"`
	Kind           SegmentKind `json:"kind"`
	Field          string      `json:"field,omitempty"`
	Shard          uint16      `json:"shard"`
	FactCount      uint64      `json:"fact_count"`
	HashesDigest   string      `json:"hashes_digest"`
	PayloadsDigest string      `json:"payloads_digest"`
}

func (m DictionaryManifest) Validate() error {
	if m.Version != DictionaryVersion {
		return fmt.Errorf("%w: dictionary manifest version", ErrInvalid)
	}
	if !validID(m.SourceID) || !validID(m.SourceNamespace) || !validID(m.Snapshot) {
		return fmt.Errorf("%w: dictionary source, namespace, and snapshot are required", ErrInvalid)
	}
	for name, digest := range map[string]string{
		"schema": m.SchemaDigest, "dictionary": m.DictionaryDigest,
		"sidecar": m.SidecarDigest, "cold payload": m.ColdPayloadDigest,
		"hot index": m.HotIndexDigest,
	} {
		if !validDigest(digest) {
			return fmt.Errorf("%w: %s digest", ErrInvalid, name)
		}
	}
	if len(m.Segments) == 0 {
		return fmt.Errorf("%w: dictionary has no segments", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(m.Segments))
	previous := ""
	for index, segment := range m.Segments {
		if err := segment.Validate(); err != nil {
			return fmt.Errorf("%w: segment %d: %v", ErrInvalid, index, err)
		}
		if _, duplicate := seen[segment.ID]; duplicate {
			return fmt.Errorf("%w: duplicate dictionary segment", ErrInvalid)
		}
		if previous != "" && segment.ID <= previous {
			return fmt.Errorf("%w: dictionary segments are not canonically ordered", ErrNonCanonical)
		}
		seen[segment.ID] = struct{}{}
		previous = segment.ID
	}
	return nil
}

func (m SegmentManifest) Validate() error {
	if !validID(m.ID) || m.FactCount > uint64(math.MaxUint32)+1 {
		return fmt.Errorf("%w: invalid segment id or fact count", ErrInvalid)
	}
	switch m.Kind {
	case SegmentBaseRow:
		if m.Field != "" {
			return fmt.Errorf("%w: row segment cannot name a field", ErrInvalid)
		}
	case SegmentBaseCell:
		if !validID(m.Field) {
			return fmt.Errorf("%w: cell segment requires a field", ErrInvalid)
		}
	case SegmentDerived, SegmentOutcome:
		if m.Field != "" {
			return fmt.Errorf("%w: dynamic segment cannot name a field", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported segment kind", ErrInvalid)
	}
	if !validDigest(m.HashesDigest) || !validDigest(m.PayloadsDigest) {
		return fmt.Errorf("%w: invalid segment content digest", ErrInvalid)
	}
	return nil
}

// Digest returns a domain-separated digest of the complete manifest. The
// encoding is explicit rather than reflection/JSON based.
func (m DictionaryManifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write([]byte(manifestDigestDomain))
	writeString(hash, m.Version)
	writeString(hash, m.SourceID)
	writeString(hash, m.SourceNamespace)
	writeString(hash, m.Snapshot)
	writeString(hash, m.SchemaDigest)
	writeString(hash, m.DictionaryDigest)
	writeString(hash, m.SidecarDigest)
	writeString(hash, m.ColdPayloadDigest)
	writeString(hash, m.HotIndexDigest)
	writeUint64(hash, uint64(len(m.Segments)))
	for _, segment := range m.Segments {
		writeString(hash, segment.ID)
		writeString(hash, string(segment.Kind))
		writeString(hash, segment.Field)
		writeUint16(hash, segment.Shard)
		writeUint64(hash, segment.FactCount)
		writeString(hash, segment.HashesDigest)
		writeString(hash, segment.PayloadsDigest)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type DictionarySpec struct {
	SourceID          string
	SourceNamespace   string
	Snapshot          string
	SchemaDigest      string
	SidecarDigest     string
	ColdPayloadDigest string
	Segments          []SegmentSpec
}

type SegmentSpec struct {
	ID    string
	Kind  SegmentKind
	Field string
	Shard uint16
	Facts []exposure.FactID
}

type dictionaryEntry struct {
	hash    [sha256.Size]byte
	payload []byte
	fact    exposure.FactID
}

// Dictionary is immutable after Compile. Facts are retained here to provide a
// reference implementation and exact audit Decode; production registries may
// implement Resolver with mmap hash arrays and cold payload blocks instead.
type Dictionary struct {
	manifest       DictionaryManifest
	digest         string
	segments       map[string][]dictionaryEntry
	byHash         map[[sha256.Size]byte]FactRef
	entityToHandle map[string]RowHandle
	rows           map[RowHandle]RowRefs
}

// Resolver is the representation-independent audit boundary.
type Resolver interface {
	Expand(FactRef) (exposure.FactID, error)
}

// SnapshotIndex is the Gateway hot-path contract. Implementations must not
// require canonical payloads or full FactID objects to answer these methods.
type SnapshotIndex interface {
	Manifest() DictionaryManifest
	DictionaryDigest() string
	ManifestDigest() string
	Hash(FactRef) ([sha256.Size]byte, error)
	SegmentFactCount(string) (uint64, bool)
	ValidateSetBounds(BitmapSet) error
	RowCount() uint64
	LookupRowHandle(string) (RowHandle, bool)
	LookupRow(RowHandle) (RowRefs, bool)
	LookupEntity(string) (RowRefs, bool)
}

// Compile builds a deterministic hash-sorted ordinal dictionary while using
// exposure.FactID.Hash and CanonicalPayload as the sole semantic authority.
func Compile(spec DictionarySpec) (*Dictionary, error) {
	return compileWithSegmentCapacity(spec, maxOrdinalSegmentFacts)
}

type compiledSegment struct {
	spec    SegmentSpec
	entries []dictionaryEntry
}

type hashPrefixShard struct {
	prefixBits uint16
	prefixHash [sha256.Size]byte
	entries    []dictionaryEntry
}

// compileWithSegmentCapacity exposes a small-capacity production-equivalent
// path to package tests. Production always uses the complete uint32 ordinal
// space; the helper may only lower that limit, never enlarge it.
func compileWithSegmentCapacity(spec DictionarySpec, segmentCapacity uint64) (*Dictionary, error) {
	if !validID(spec.SourceID) || !validID(spec.SourceNamespace) || !validID(spec.Snapshot) {
		return nil, fmt.Errorf("%w: source, namespace, and snapshot are required", ErrInvalid)
	}
	if !validDigest(spec.SchemaDigest) {
		return nil, fmt.Errorf("%w: schema digest", ErrInvalid)
	}
	for name, digest := range map[string]string{"sidecar": spec.SidecarDigest, "cold payload": spec.ColdPayloadDigest} {
		if digest != "" && !validDigest(digest) {
			return nil, fmt.Errorf("%w: expected %s digest", ErrInvalid, name)
		}
	}
	if len(spec.Segments) == 0 {
		return nil, fmt.Errorf("%w: dictionary has no segments", ErrInvalid)
	}
	if err := validateSegmentCapacity(segmentCapacity); err != nil {
		return nil, err
	}

	segmentSpecs := append([]SegmentSpec(nil), spec.Segments...)
	sort.Slice(segmentSpecs, func(i, j int) bool { return segmentSpecs[i].ID < segmentSpecs[j].ID })
	compiledSegments := make([]compiledSegment, 0, len(segmentSpecs))
	allHashes := make(map[[sha256.Size]byte][]byte)
	seenLogicalSegments := make(map[string]struct{}, len(segmentSpecs))

	for _, segmentSpec := range segmentSpecs {
		if _, duplicate := seenLogicalSegments[segmentSpec.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate segment id", ErrInvalid)
		}
		if !validID(segmentSpec.ID) {
			return nil, fmt.Errorf("%w: invalid segment id", ErrInvalid)
		}
		seenLogicalSegments[segmentSpec.ID] = struct{}{}
		entries := make([]dictionaryEntry, 0, len(segmentSpec.Facts))
		for _, fact := range segmentSpec.Facts {
			if err := validateSegmentFact(spec, segmentSpec, fact); err != nil {
				return nil, err
			}
			hashText, err := fact.Hash()
			if err != nil {
				return nil, fmt.Errorf("%w: hash fact: %v", ErrInvalid, err)
			}
			hashBytes, _ := hex.DecodeString(hashText)
			var factHash [sha256.Size]byte
			copy(factHash[:], hashBytes)
			payload, err := fact.CanonicalPayload()
			if err != nil {
				return nil, fmt.Errorf("%w: canonical fact payload: %v", ErrInvalid, err)
			}
			if existing, present := allHashes[factHash]; present {
				if !bytes.Equal(existing, payload) {
					return nil, fmt.Errorf("%w: %s", ErrFactCollision, hashText)
				}
				return nil, fmt.Errorf("%w: fact appears in multiple dictionary positions", ErrInvalid)
			}
			// CanonicalPayload returns a newly owned byte slice. Share that
			// immutable slice between collision detection and the dictionary
			// entry instead of retaining two million-scale payload copies.
			allHashes[factHash] = payload
			entries = append(entries, dictionaryEntry{hash: factHash, payload: payload, fact: cloneFact(fact)})
		}
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].hash[:], entries[j].hash[:]) < 0 })
		shards, err := shardSortedSegment(segmentSpec, entries, segmentCapacity)
		if err != nil {
			return nil, err
		}
		compiledSegments = append(compiledSegments, shards...)
	}

	sort.Slice(compiledSegments, func(i, j int) bool { return compiledSegments[i].spec.ID < compiledSegments[j].spec.ID })
	segments := make(map[string][]dictionaryEntry, len(compiledSegments))
	manifests := make([]SegmentManifest, 0, len(compiledSegments))
	for _, compiled := range compiledSegments {
		segmentSpec, entries := compiled.spec, compiled.entries
		if _, duplicate := segments[segmentSpec.ID]; duplicate {
			return nil, fmt.Errorf("%w: generated shard segment id collision", ErrInvalid)
		}
		hashesDigest, payloadsDigest := digestSegmentEntries(entries)
		manifest := SegmentManifest{ID: segmentSpec.ID, Kind: segmentSpec.Kind, Field: segmentSpec.Field, Shard: segmentSpec.Shard,
			FactCount: uint64(len(entries)), HashesDigest: hashesDigest, PayloadsDigest: payloadsDigest}
		if err := manifest.Validate(); err != nil {
			return nil, err
		}
		segments[segmentSpec.ID] = entries
		manifests = append(manifests, manifest)
	}

	dictionaryDigest := digestDictionary(manifests, segments)
	coldPayloadDigest := digestColdEntries(manifests, segments)
	if spec.ColdPayloadDigest != "" && spec.ColdPayloadDigest != coldPayloadDigest {
		return nil, fmt.Errorf("%w: compiled cold payload digest", ErrDigestMismatch)
	}
	sidecarDigest := digestSidecarRows(nil)
	if spec.SidecarDigest != "" {
		sidecarDigest = spec.SidecarDigest
	}
	hotIndexDigest := digestHotDictionary(dictionaryDigest, manifests, segments, nil)
	manifest := DictionaryManifest{Version: DictionaryVersion, SourceID: spec.SourceID, SourceNamespace: spec.SourceNamespace,
		Snapshot: spec.Snapshot, SchemaDigest: spec.SchemaDigest, DictionaryDigest: dictionaryDigest,
		SidecarDigest: sidecarDigest, ColdPayloadDigest: coldPayloadDigest, HotIndexDigest: hotIndexDigest, Segments: manifests}
	manifestDigest, err := manifest.Digest()
	if err != nil {
		return nil, err
	}
	dictionary := &Dictionary{manifest: manifest, digest: manifestDigest, segments: segments,
		byHash: make(map[[sha256.Size]byte]FactRef, len(allHashes))}
	for _, segment := range manifests {
		for ordinal, entry := range segments[segment.ID] {
			if uint64(ordinal) > uint64(math.MaxUint32) {
				return nil, fmt.Errorf("%w: ordinal exceeds uint32", ErrInvalid)
			}
			dictionary.byHash[entry.hash] = FactRef{DictionaryDigest: dictionaryDigest, SegmentID: segment.ID, Ordinal: uint32(ordinal)}
		}
	}
	return dictionary, nil
}

func validateSegmentCapacity(capacity uint64) error {
	if capacity == 0 || capacity > maxOrdinalSegmentFacts {
		return fmt.Errorf("%w: segment capacity is outside the uint32 ordinal space", ErrInvalid)
	}
	return nil
}

func validateShardCount(count uint64) error {
	if count == 0 || count > uint64(math.MaxUint16)+1 {
		return fmt.Errorf("%w: hash-prefix shard count exceeds uint16", ErrInvalid)
	}
	return nil
}

func shardSortedSegment(spec SegmentSpec, entries []dictionaryEntry, capacity uint64) ([]compiledSegment, error) {
	if err := validateSegmentCapacity(capacity); err != nil {
		return nil, err
	}
	if uint64(len(entries)) <= capacity {
		spec.Facts = nil
		return []compiledSegment{{spec: spec, entries: entries}}, nil
	}
	if spec.Shard != 0 {
		return nil, fmt.Errorf("%w: cannot reshard a pre-numbered segment", ErrInvalid)
	}
	for index := 1; index < len(entries); index++ {
		comparison := bytes.Compare(entries[index-1].hash[:], entries[index].hash[:])
		if comparison >= 0 {
			return nil, fmt.Errorf("%w: segment hashes are duplicate or not sorted", ErrInvalid)
		}
	}
	leaves := make([]hashPrefixShard, 0)
	if err := splitHashPrefixShards(entries, capacity, 0, &leaves); err != nil {
		return nil, err
	}
	if err := validateShardCount(uint64(len(leaves))); err != nil {
		return nil, err
	}
	result := make([]compiledSegment, len(leaves))
	for index, leaf := range leaves {
		segmentID, err := prefixedSegmentID(spec.ID, leaf.prefixHash, leaf.prefixBits)
		if err != nil {
			return nil, err
		}
		shardSpec := spec
		shardSpec.ID = segmentID
		shardSpec.Shard = uint16(index)
		shardSpec.Facts = nil
		result[index] = compiledSegment{spec: shardSpec, entries: leaf.entries}
	}
	return result, nil
}

func splitHashPrefixShards(entries []dictionaryEntry, capacity uint64, prefixBits uint16, leaves *[]hashPrefixShard) error {
	if len(entries) == 0 {
		return fmt.Errorf("%w: empty hash-prefix shard", ErrInvalid)
	}
	if uint64(len(entries)) <= capacity {
		*leaves = append(*leaves, hashPrefixShard{prefixBits: prefixBits, prefixHash: entries[0].hash, entries: entries})
		return nil
	}
	if prefixBits >= sha256.Size*8 {
		return fmt.Errorf("%w: full FactHash prefix cannot satisfy segment capacity", ErrInvalid)
	}
	bit := int(prefixBits)
	boundary := sort.Search(len(entries), func(index int) bool { return factHashBit(entries[index].hash, bit) != 0 })
	nextBits := prefixBits + 1
	if boundary > 0 {
		if err := splitHashPrefixShards(entries[:boundary], capacity, nextBits, leaves); err != nil {
			return err
		}
	}
	if boundary < len(entries) {
		if err := splitHashPrefixShards(entries[boundary:], capacity, nextBits, leaves); err != nil {
			return err
		}
	}
	return nil
}

func factHashBit(hash [sha256.Size]byte, bit int) byte {
	return (hash[bit/8] >> uint(7-bit%8)) & 1
}

func prefixedSegmentID(base string, prefixHash [sha256.Size]byte, prefixBits uint16) (string, error) {
	if !validID(base) || prefixBits == 0 || prefixBits > sha256.Size*8 {
		return "", fmt.Errorf("%w: invalid hash-prefix shard identity", ErrInvalid)
	}
	prefixBytes := (int(prefixBits) + 7) / 8
	material := append([]byte(nil), prefixHash[:prefixBytes]...)
	if remainder := int(prefixBits) % 8; remainder != 0 {
		material[len(material)-1] &= byte(0xff << uint(8-remainder))
	}
	segmentID := fmt.Sprintf("%s:p%03d:%s", base, prefixBits, hex.EncodeToString(material))
	if !validID(segmentID) {
		return "", fmt.Errorf("%w: generated hash-prefix segment id is invalid", ErrInvalid)
	}
	return segmentID, nil
}

func hashMatchesPrefix(hash, prefix [sha256.Size]byte, prefixBits uint16) bool {
	if prefixBits > sha256.Size*8 {
		return false
	}
	for bit := 0; bit < int(prefixBits); bit++ {
		if factHashBit(hash, bit) != factHashBit(prefix, bit) {
			return false
		}
	}
	return true
}

func (d *Dictionary) Manifest() DictionaryManifest {
	if d == nil {
		return DictionaryManifest{}
	}
	result := d.manifest
	result.Segments = append([]SegmentManifest(nil), d.manifest.Segments...)
	return result
}

func (d *Dictionary) DictionaryDigest() string {
	if d == nil {
		return ""
	}
	return d.manifest.DictionaryDigest
}

func (d *Dictionary) ManifestDigest() string {
	if d == nil {
		return ""
	}
	return d.digest
}

func (d *Dictionary) Expand(ref FactRef) (exposure.FactID, error) {
	if d == nil || ref.Validate() != nil || ref.DictionaryDigest != d.manifest.DictionaryDigest {
		return exposure.FactID{}, ErrUnknownFact
	}
	entries := d.segments[ref.SegmentID]
	if uint64(ref.Ordinal) >= uint64(len(entries)) {
		return exposure.FactID{}, ErrUnknownFact
	}
	return cloneFact(entries[ref.Ordinal].fact), nil
}

func (d *Dictionary) Hash(ref FactRef) ([sha256.Size]byte, error) {
	if d == nil || ref.Validate() != nil || ref.DictionaryDigest != d.manifest.DictionaryDigest {
		return [sha256.Size]byte{}, ErrUnknownFact
	}
	entries := d.segments[ref.SegmentID]
	if uint64(ref.Ordinal) >= uint64(len(entries)) {
		return [sha256.Size]byte{}, ErrUnknownFact
	}
	return entries[ref.Ordinal].hash, nil
}

func (d *Dictionary) CanonicalPayload(ref FactRef) ([]byte, error) {
	if d == nil || ref.Validate() != nil || ref.DictionaryDigest != d.manifest.DictionaryDigest {
		return nil, ErrUnknownFact
	}
	entries := d.segments[ref.SegmentID]
	if uint64(ref.Ordinal) >= uint64(len(entries)) {
		return nil, ErrUnknownFact
	}
	return append([]byte(nil), entries[ref.Ordinal].payload...), nil
}

func (d *Dictionary) SegmentFactCount(segmentID string) (uint64, bool) {
	if d == nil {
		return 0, false
	}
	entries, found := d.segments[segmentID]
	return uint64(len(entries)), found
}

// ValidateSetBounds closes over an effect in O(number of segments), rejecting
// an unknown dictionary, unknown segment, or any ordinal outside the compiled
// dictionary without expanding the facts.
func (d *Dictionary) ValidateSetBounds(set BitmapSet) error {
	if d == nil {
		return fmt.Errorf("%w: nil dictionary", ErrInvalid)
	}
	for _, bound := range set.SegmentBounds() {
		if bound.Segment.DictionaryDigest != d.DictionaryDigest() {
			return ErrUnknownFact
		}
		count, found := d.SegmentFactCount(bound.Segment.SegmentID)
		if !found || uint64(bound.MaxOrdinal) >= count {
			return ErrUnknownFact
		}
	}
	return nil
}

type ExpandedContent struct {
	Ref              FactRef
	Hash             [sha256.Size]byte
	CanonicalPayload []byte
}

// StreamExpand visits hashes and cold canonical payloads in deterministic ref
// order. It is suitable for audit export and never builds a full fact slice.
func (d *Dictionary) StreamExpand(set BitmapSet, yield func(ExpandedContent) error) error {
	if yield == nil {
		return fmt.Errorf("%w: nil expansion callback", ErrInvalid)
	}
	if err := d.ValidateSetBounds(set); err != nil {
		return err
	}
	var expandErr error
	set.Refs(func(ref FactRef) bool {
		entries := d.segments[ref.SegmentID]
		entry := entries[ref.Ordinal]
		expandErr = yield(ExpandedContent{Ref: ref, Hash: entry.hash, CanonicalPayload: append([]byte(nil), entry.payload...)})
		return expandErr == nil
	})
	return expandErr
}

func (d *Dictionary) Lookup(fact exposure.FactID) (FactRef, bool, error) {
	if d == nil {
		return FactRef{}, false, ErrUnknownFact
	}
	hashText, err := fact.Hash()
	if err != nil {
		return FactRef{}, false, err
	}
	decoded, _ := hex.DecodeString(hashText)
	var hash [sha256.Size]byte
	copy(hash[:], decoded)
	ref, found := d.byHash[hash]
	if !found {
		return FactRef{}, false, nil
	}
	stored, _ := d.CanonicalPayload(ref)
	payload, err := fact.CanonicalPayload()
	if err != nil {
		return FactRef{}, false, err
	}
	if !bytes.Equal(stored, payload) {
		return FactRef{}, false, ErrFactCollision
	}
	return ref, true, nil
}

// Decode proves the representation refinement at the executable boundary. It
// rejects any resolver that maps two distinct refs to the same FactID, so the
// returned FactSet cardinality must equal the bitmap popcount.
func Decode(set BitmapSet, resolver Resolver) (exposure.FactSet, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: nil dictionary resolver", ErrInvalid)
	}
	result, _ := exposure.NewFactSet()
	var decodeErr error
	set.Refs(func(ref FactRef) bool {
		fact, err := resolver.Expand(ref)
		if err != nil {
			decodeErr = err
			return false
		}
		before := len(result)
		if err := result.Add(fact); err != nil {
			decodeErr = err
			return false
		}
		if len(result) == before {
			decodeErr = fmt.Errorf("%w: distinct ordinals decode to one FactID", ErrInvalid)
			return false
		}
		return true
	})
	if decodeErr != nil {
		return nil, decodeErr
	}
	if uint64(len(result)) != set.Cardinality() {
		return nil, fmt.Errorf("%w: decoded cardinality differs from bitmap", ErrInvalid)
	}
	return result, nil
}

// Registry resolves a bundle of immutable dictionaries and rejects digest
// ambiguity at construction.
type Registry struct {
	mu           sync.RWMutex
	dictionaries map[string]SnapshotIndex
	publications map[PublicationKey]string
}

type PublicationKey struct {
	CatalogDigest   string `json:"catalog_digest"`
	PublicationName string `json:"publication_name"`
}

func NewRegistry(indexes ...SnapshotIndex) (*Registry, error) {
	result := &Registry{dictionaries: make(map[string]SnapshotIndex, len(indexes)), publications: make(map[PublicationKey]string)}
	for _, index := range indexes {
		if index == nil || !validDigest(index.DictionaryDigest()) {
			return nil, fmt.Errorf("%w: nil or invalid snapshot index", ErrInvalid)
		}
		if _, duplicate := result.dictionaries[index.DictionaryDigest()]; duplicate {
			return nil, fmt.Errorf("%w: duplicate dictionary digest", ErrInvalid)
		}
		result.dictionaries[index.DictionaryDigest()] = index
	}
	return result, nil
}

// RegisterPublication binds a Catalog artifact and publication name to one
// already verified dictionary. A conflicting re-registration fails closed.
func (r *Registry) RegisterPublication(key PublicationKey, expectedManifestDigest string, index SnapshotIndex) error {
	if r == nil || index == nil || !validDigest(key.CatalogDigest) || !validID(key.PublicationName) ||
		!validDigest(expectedManifestDigest) || expectedManifestDigest != index.ManifestDigest() {
		return fmt.Errorf("%w: invalid publication binding", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.dictionaries[index.DictionaryDigest()]; existing != nil && existing.ManifestDigest() != index.ManifestDigest() {
		return fmt.Errorf("%w: dictionary digest ambiguity", ErrFactCollision)
	}
	if digest, present := r.publications[key]; present && digest != index.DictionaryDigest() {
		return fmt.Errorf("%w: publication is already bound", ErrInvalid)
	}
	r.dictionaries[index.DictionaryDigest()] = index
	r.publications[key] = index.DictionaryDigest()
	return nil
}

// Resolve is the SnapshotRegistry boundary used before ordinal query
// execution. Returned dictionaries are immutable.
func (r *Registry) Resolve(key PublicationKey) (SnapshotIndex, error) {
	if r == nil || !validDigest(key.CatalogDigest) || !validID(key.PublicationName) {
		return nil, ErrUnknownFact
	}
	r.mu.RLock()
	digest := r.publications[key]
	dictionary := r.dictionaries[digest]
	r.mu.RUnlock()
	if dictionary == nil {
		return nil, ErrUnknownFact
	}
	return dictionary, nil
}

func validateSegmentFact(dictionary DictionarySpec, segment SegmentSpec, fact exposure.FactID) error {
	if err := fact.Validate(); err != nil {
		return fmt.Errorf("%w: invalid segment fact: %v", ErrInvalid, err)
	}
	expectedKind := map[SegmentKind]exposure.FactKind{
		SegmentBaseRow: exposure.FactBaseRow, SegmentBaseCell: exposure.FactBaseCell,
		SegmentDerived: exposure.FactDerived, SegmentOutcome: exposure.FactOutcome,
	}[segment.Kind]
	if expectedKind == "" || fact.Kind != expectedKind {
		return fmt.Errorf("%w: fact kind does not match segment", ErrInvalid)
	}
	if segment.Kind == SegmentBaseRow || segment.Kind == SegmentBaseCell {
		if fact.SourceNamespace != dictionary.SourceNamespace || fact.Snapshot != dictionary.Snapshot {
			return fmt.Errorf("%w: base fact does not match dictionary snapshot", ErrInvalid)
		}
	}
	if segment.Kind == SegmentBaseCell {
		if !validID(segment.Field) || fact.Field != segment.Field {
			return fmt.Errorf("%w: cell fact does not match segment field", ErrInvalid)
		}
	} else if segment.Field != "" {
		return fmt.Errorf("%w: non-cell segment cannot name a field", ErrInvalid)
	}
	return nil
}

func digestSegmentEntries(entries []dictionaryEntry) (string, string) {
	hashes := sha256.New()
	hashes.Write([]byte(segmentHashesDomain))
	payloads := sha256.New()
	payloads.Write([]byte(segmentPayloadsDomain))
	writeUint64(hashes, uint64(len(entries)))
	writeUint64(payloads, uint64(len(entries)))
	for _, entry := range entries {
		_, _ = hashes.Write(entry.hash[:])
		writeBytes(payloads, entry.payload)
	}
	return hex.EncodeToString(hashes.Sum(nil)), hex.EncodeToString(payloads.Sum(nil))
}

func digestDictionary(manifests []SegmentManifest, entries map[string][]dictionaryEntry) string {
	hash := sha256.New()
	hash.Write([]byte(dictionaryDigestDomain))
	writeUint64(hash, uint64(len(manifests)))
	for _, segment := range manifests {
		writeString(hash, segment.ID)
		writeString(hash, string(segment.Kind))
		writeString(hash, segment.Field)
		writeUint16(hash, segment.Shard)
		writeUint64(hash, uint64(len(entries[segment.ID])))
		for _, entry := range entries[segment.ID] {
			_, _ = hash.Write(entry.hash[:])
			writeBytes(hash, entry.payload)
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneFact(fact exposure.FactID) exposure.FactID {
	fact.SnapshotBundle = append([]exposure.SnapshotBinding(nil), fact.SnapshotBundle...)
	sort.Slice(fact.SnapshotBundle, func(i, j int) bool {
		if fact.SnapshotBundle[i].SourceNamespace != fact.SnapshotBundle[j].SourceNamespace {
			return fact.SnapshotBundle[i].SourceNamespace < fact.SnapshotBundle[j].SourceNamespace
		}
		return fact.SnapshotBundle[i].Snapshot < fact.SnapshotBundle[j].Snapshot
	})
	return fact
}
