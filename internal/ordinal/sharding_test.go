package ordinal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func shardingSnapshotSpec(reverse bool) SnapshotSpec {
	rows := make([]SnapshotRow, 12)
	for index := range rows {
		rows[index] = SnapshotRow{EntityKey: "row-" + strconv.Itoa(index+100), Values: map[string]any{
			"amount": strconv.Itoa(index + 1),
		}}
	}
	if reverse {
		for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
			rows[left], rows[right] = rows[right], rows[left]
		}
	}
	return SnapshotSpec{
		SourceID: "business", SourceNamespace: "travel.expense", Snapshot: "snapshot-sharded",
		SchemaDigest: testDigest("7"), Fields: []SnapshotField{{Name: "amount", SQLType: "numeric"}}, Rows: rows,
	}
}

func TestSnapshotHashPrefixShardingIsDeterministicAndRoundTrips(t *testing.T) {
	const capacity = uint64(2)
	firstDictionary, err := compileSnapshotWithSegmentCapacity(shardingSnapshotSpec(false), capacity)
	if err != nil {
		t.Fatalf("compile sharded snapshot: %v", err)
	}
	secondDictionary, err := compileSnapshotWithSegmentCapacity(shardingSnapshotSpec(true), capacity)
	if err != nil {
		t.Fatalf("compile reversed sharded snapshot: %v", err)
	}
	if firstDictionary.DictionaryDigest() != secondDictionary.DictionaryDigest() ||
		firstDictionary.ManifestDigest() != secondDictionary.ManifestDigest() ||
		!reflect.DeepEqual(firstDictionary.Manifest(), secondDictionary.Manifest()) {
		t.Fatal("input row order changed deterministic prefix shards")
	}

	manifest := firstDictionary.Manifest()
	groups := make(map[string][]testShardPrefix)
	shardNumbers := make(map[string]map[uint16]struct{})
	for _, segment := range manifest.Segments {
		if segment.FactCount == 0 || segment.FactCount > capacity {
			t.Fatalf("segment %q count = %d, capacity %d", segment.ID, segment.FactCount, capacity)
		}
		base := "row"
		if segment.Kind == SegmentBaseCell {
			base = "cell:" + segment.Field
		}
		prefix := parseTestShardPrefix(t, base, segment.ID)
		groups[base] = append(groups[base], prefix)
		if shardNumbers[base] == nil {
			shardNumbers[base] = make(map[uint16]struct{})
		}
		if _, duplicate := shardNumbers[base][segment.Shard]; duplicate {
			t.Fatalf("logical segment %q repeats shard number %d", base, segment.Shard)
		}
		shardNumbers[base][segment.Shard] = struct{}{}
		for _, entry := range firstDictionary.segments[segment.ID] {
			if !hashMatchesPrefix(entry.hash, prefix.hash, prefix.bits) {
				t.Fatalf("fact hash %x is outside segment prefix %q", entry.hash, segment.ID)
			}
		}
	}
	for base, prefixes := range groups {
		if len(prefixes) < 2 {
			t.Fatalf("logical segment %q was not split: %#v", base, prefixes)
		}
		if len(prefixes) != len(shardNumbers[base]) {
			t.Fatalf("logical segment %q shard numbering is not injective", base)
		}
		for shard := 0; shard < len(prefixes); shard++ {
			if _, found := shardNumbers[base][uint16(shard)]; !found {
				t.Fatalf("logical segment %q misses shard number %d", base, shard)
			}
		}
	}

	firstArtifact, err := firstDictionary.Split()
	if err != nil {
		t.Fatalf("split sharded dictionary: %v", err)
	}
	secondArtifact, err := secondDictionary.Split()
	if err != nil {
		t.Fatalf("split reversed sharded dictionary: %v", err)
	}
	firstHot, err := firstArtifact.Hot.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal sharded HOT: %v", err)
	}
	secondHot, _ := secondArtifact.Hot.MarshalBinary()
	firstCold, err := firstArtifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal sharded COLD: %v", err)
	}
	secondCold, _ := secondArtifact.Cold.MarshalBinary()
	if !bytes.Equal(firstHot, secondHot) || !bytes.Equal(firstCold, secondCold) {
		t.Fatal("input row order changed sharded artifact bytes")
	}
	loadedHot, err := ParseHotDictionary(firstHot, firstArtifact.Hot.ManifestDigest())
	if err != nil {
		t.Fatalf("parse sharded HOT: %v", err)
	}
	loadedCold, err := ParseColdDictionary(firstCold, firstArtifact.Hot.ManifestDigest())
	if err != nil {
		t.Fatalf("parse sharded COLD: %v", err)
	}

	builder := NewBuilder()
	rowShards := make(map[string]struct{})
	cellShards := make(map[string]struct{})
	for index := 0; index < 12; index++ {
		entityKey := "row-" + strconv.Itoa(index+100)
		row, found := loadedHot.LookupEntity(entityKey)
		if !found {
			t.Fatalf("loaded HOT misses %q", entityKey)
		}
		original, _ := firstDictionary.LookupEntity(entityKey)
		if row.Row != original.Row || !reflect.DeepEqual(row.Cells, original.Cells) {
			t.Fatalf("round-trip remapped %q: %#v != %#v", entityKey, row, original)
		}
		rowShards[row.Row.SegmentID] = struct{}{}
		cellShards[row.Cells["amount"].SegmentID] = struct{}{}
		for _, ref := range []FactRef{row.Row, row.Cells["amount"]} {
			if err := builder.Add(ref); err != nil {
				t.Fatal(err)
			}
			fact, expandErr := loadedCold.Expand(ref)
			hash, hashErr := loadedHot.Hash(ref)
			factHash, _ := fact.Hash()
			if expandErr != nil || hashErr != nil || hex.EncodeToString(hash[:]) != factHash {
				t.Fatalf("sharded bijection failed for %#v: fact=%#v expand=%v hash=%v", ref, fact, expandErr, hashErr)
			}
		}
	}
	if len(rowShards) < 2 || len(cellShards) < 2 {
		t.Fatalf("row/cell lookup did not cross shards: rows=%d cells=%d", len(rowShards), len(cellShards))
	}
	set, err := builder.Freeze()
	if err != nil || set.Cardinality() != 24 {
		t.Fatalf("cross-shard bitmap cardinality = %d, err=%v", set.Cardinality(), err)
	}
	if err := loadedHot.ValidateSetBounds(set); err != nil {
		t.Fatalf("cross-shard bounds: %v", err)
	}
	portable, err := set.PortableSegments()
	if err != nil {
		t.Fatal(err)
	}
	roundTripSet, err := ParsePortableSegments(portable)
	if err != nil || !set.Equal(roundTripSet) {
		t.Fatalf("cross-shard bitmap round-trip: equal=%t err=%v", set.Equal(roundTripSet), err)
	}
	decoded, err := Decode(set, loadedCold)
	if err != nil || decoded.Len() != 24 {
		t.Fatalf("cross-shard decode cardinality = %d, err=%v", decoded.Len(), err)
	}
	var previous [sha256.Size]byte
	seenHashes := 0
	if err := loadedHot.StreamHashesByFactHash(set, func(_ FactRef, hash [sha256.Size]byte) error {
		if seenHashes > 0 && bytes.Compare(previous[:], hash[:]) >= 0 {
			t.Fatalf("cross-shard hash stream is not strictly sorted")
		}
		previous = hash
		seenHashes++
		return nil
	}); err != nil || seenHashes != 24 {
		t.Fatalf("cross-shard hash stream count = %d, err=%v", seenHashes, err)
	}
}

func TestHashPrefixShardLimitsFailClosed(t *testing.T) {
	if err := validateSegmentCapacity(0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero segment capacity = %v", err)
	}
	if err := validateSegmentCapacity(maxOrdinalSegmentFacts + 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized segment capacity = %v", err)
	}
	if err := validateShardCount(uint64(math.MaxUint16) + 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("uint16 shard overflow = %v", err)
	}

	identical := make([]dictionaryEntry, 2)
	identical[0].hash = [sha256.Size]byte{0xff}
	identical[1].hash = identical[0].hash
	leaves := make([]hashPrefixShard, 0)
	if err := splitHashPrefixShards(identical, 1, 0, &leaves); !errors.Is(err, ErrInvalid) {
		t.Fatalf("identical full hashes did not fail at depth 256: %v", err)
	}

	extreme := make([]dictionaryEntry, 3)
	for index := range extreme {
		for offset := 0; offset < sha256.Size-1; offset++ {
			extreme[index].hash[offset] = 0xff
		}
		extreme[index].hash[sha256.Size-1] = byte(index)
	}
	leaves = leaves[:0]
	if err := splitHashPrefixShards(extreme, 1, 0, &leaves); err != nil {
		t.Fatalf("extreme valid prefix split: %v", err)
	}
	for _, leaf := range leaves {
		if leaf.prefixBits < 255 || len(leaf.entries) != 1 {
			t.Fatalf("extreme prefix leaf = bits:%d entries:%d", leaf.prefixBits, len(leaf.entries))
		}
	}

	longBase := strings.Repeat("x", 250)
	if _, err := shardSortedSegment(SegmentSpec{ID: longBase, Kind: SegmentBaseRow}, extreme, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("overflowing shard segment ID = %v", err)
	}
}

func TestProductionCapacityPreservesUnshardedV1Layout(t *testing.T) {
	production, err := CompileSnapshotArtifact(snapshotTestSpec("", false))
	if err != nil {
		t.Fatal(err)
	}
	referenceDictionary, err := compileSnapshotWithSegmentCapacity(snapshotTestSpec("", false), maxOrdinalSegmentFacts)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := referenceDictionary.Split()
	if err != nil {
		t.Fatal(err)
	}
	productionHot, _ := production.Hot.MarshalBinary()
	referenceHot, _ := reference.Hot.MarshalBinary()
	productionCold, _ := production.Cold.MarshalBinary()
	referenceCold, _ := reference.Cold.MarshalBinary()
	if !reflect.DeepEqual(production.Hot.Manifest(), reference.Hot.Manifest()) ||
		production.Hot.ManifestDigest() != reference.Hot.ManifestDigest() ||
		!bytes.Equal(productionHot, referenceHot) || !bytes.Equal(productionCold, referenceCold) {
		t.Fatal("production sharding support changed the unsharded V1 artifact layout")
	}
	for _, segment := range production.Hot.Manifest().Segments {
		if segment.ID != "row" && segment.ID != "cell:amount" {
			t.Fatalf("small snapshot segment ID changed to %q", segment.ID)
		}
	}
}

type testShardPrefix struct {
	bits uint16
	hash [sha256.Size]byte
}

func parseTestShardPrefix(t *testing.T, base, segmentID string) testShardPrefix {
	t.Helper()
	prefix := base + ":p"
	if !strings.HasPrefix(segmentID, prefix) {
		t.Fatalf("segment %q does not use the %q hash prefix", segmentID, base)
	}
	parts := strings.SplitN(strings.TrimPrefix(segmentID, prefix), ":", 2)
	if len(parts) != 2 {
		t.Fatalf("segment %q has malformed prefix metadata", segmentID)
	}
	bits, err := strconv.ParseUint(parts[0], 10, 16)
	material, hexErr := hex.DecodeString(parts[1])
	if err != nil || hexErr != nil || bits == 0 || bits > sha256.Size*8 || len(material) != (int(bits)+7)/8 {
		t.Fatalf("segment %q has invalid prefix: bits=%d bytes=%d errors=%v/%v", segmentID, bits, len(material), err, hexErr)
	}
	var prefixHash [sha256.Size]byte
	copy(prefixHash[:], material)
	return testShardPrefix{bits: uint16(bits), hash: prefixHash}
}
