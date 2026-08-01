package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
)

const (
	outcomeLeafDomainV5  = "TASKGATE-V5-HASH-LEAF\x00"
	outcomeBlockDomainV5 = "TASKGATE-V5-HASH-BLOCK\x00"
	outcomeSetDomainV5   = "TASKGATE-V5-HASH-SET\x00"
	outcomeLeafChunkSize = 4096
)

type OutcomeSetV5 struct {
	SetSHA256    string
	Cardinality  int64
	BlockCount   int
	RootManifest []byte
}

type OutcomeHashLeafV5 struct {
	LeafSHA256  string
	Prefix16    uint16
	ChunkIndex  uint32
	Cardinality int
	Payload     []byte
}

type OutcomeHashBlockV5 struct {
	BlockSHA256 string
	Prefix8     byte
	Cardinality int64
	Manifest    []byte
}

// OutcomeHashSetObjectsV5 is the immutable content-addressed object graph for
// one exact set. Maps are keyed by their content digests and are suitable for
// bulk upsert. Members is an in-memory verification/index aid and is not part
// of the persisted root object.
type OutcomeHashSetObjectsV5 struct {
	Set     OutcomeSetV5
	Leaves  map[string]OutcomeHashLeafV5
	Blocks  map[string]OutcomeHashBlockV5
	Members [][sha256.Size]byte
}

type OutcomeCandidateV5 struct {
	Facts  []OrdinalDynamicFact
	Hashes [][sha256.Size]byte
}

// OutcomeRadixTelemetryV5 reports the physical work performed by one exact
// novelty merge. Loaded counters exclude the small root manifest itself.
type OutcomeRadixTelemetryV5 struct {
	RootCardinality         int64         `json:"root_cardinality"`
	CandidateCardinality    int64         `json:"candidate_cardinality"`
	BlocksLoaded            int64         `json:"blocks_loaded"`
	LeavesLoaded            int64         `json:"leaves_loaded"`
	HashesLoaded            int64         `json:"hashes_loaded"`
	BlocksReused            int64         `json:"blocks_reused"`
	LeavesChanged           int64         `json:"leaves_changed"`
	CASAttempts             int64         `json:"cas_attempts"`
	CASConflicts            int64         `json:"cas_conflicts"`
	CASRetries              int64         `json:"cas_retries"`
	LoadDuration            time.Duration `json:"-"`
	DifferenceUnionDuration time.Duration `json:"-"`
	PersistDuration         time.Duration `json:"-"`
}

type outcomeLeafReferenceV5 struct {
	prefix16    uint16
	chunk       uint32
	digest      string
	cardinality int
}

type outcomeBlockReferenceV5 struct {
	prefix      byte
	digest      string
	cardinality int64
}

// BuildCandidateSet converts V5 predicate/composite facts into a sorted exact
// candidate. Equal hashes must have byte-identical kind and payload.
func BuildCandidateSet(facts []exposure.FactID) (OutcomeCandidateV5, error) {
	byHash := make(map[[sha256.Size]byte]OrdinalDynamicFact, len(facts))
	for _, fact := range facts {
		if fact.Profile != exposure.ProfileV5 || (fact.Kind != exposure.FactPredicateAtom && fact.Kind != exposure.FactCompositeOutcome) {
			return OutcomeCandidateV5{}, errors.New("V5 outcome candidate contains a non-V5 outcome fact")
		}
		hashText, err := fact.Hash()
		if err != nil {
			return OutcomeCandidateV5{}, err
		}
		hash, err := decodeOutcomeHashV5(hashText)
		if err != nil {
			return OutcomeCandidateV5{}, err
		}
		payload, err := fact.CanonicalPayload()
		if err != nil {
			return OutcomeCandidateV5{}, err
		}
		kind := "PREDICATE_ATOM"
		if fact.Kind == exposure.FactCompositeOutcome {
			kind = "COMPOSITE_OUTCOME"
		}
		dynamic := OrdinalDynamicFact{SHA256: hashText, Kind: kind, CanonicalPayload: payload}
		if existing, present := byHash[hash]; present {
			if existing.Kind != dynamic.Kind || !bytes.Equal(existing.CanonicalPayload, dynamic.CanonicalPayload) {
				return OutcomeCandidateV5{}, errors.New("V5 outcome fact SHA-256 collision")
			}
			continue
		}
		byHash[hash] = dynamic
	}
	hashes := make([][sha256.Size]byte, 0, len(byHash))
	for hash := range byHash {
		hashes = append(hashes, hash)
	}
	sortOutcomeHashesV5(hashes)
	result := OutcomeCandidateV5{Hashes: hashes, Facts: make([]OrdinalDynamicFact, 0, len(hashes))}
	for _, hash := range hashes {
		result.Facts = append(result.Facts, byHash[hash])
	}
	return result, nil
}

// BuildOutcomeHashSetV5 builds canonical leaves, blocks, and the root
// manifest from full SHA-256 members. Duplicate members are idempotent.
func BuildOutcomeHashSetV5(hashTexts []string) (OutcomeHashSetObjectsV5, error) {
	hashes := make([][sha256.Size]byte, 0, len(hashTexts))
	for _, text := range hashTexts {
		hash, err := decodeOutcomeHashV5(text)
		if err != nil {
			return OutcomeHashSetObjectsV5{}, err
		}
		hashes = append(hashes, hash)
	}
	return buildOutcomeHashSetFromBinaryV5(hashes)
}

func buildOutcomeHashSetFromBinaryV5(hashes [][sha256.Size]byte) (OutcomeHashSetObjectsV5, error) {
	hashes = uniqueOutcomeHashesV5(hashes)
	result := OutcomeHashSetObjectsV5{Leaves: make(map[string]OutcomeHashLeafV5),
		Blocks: make(map[string]OutcomeHashBlockV5), Members: hashes}
	type prefixMembers struct {
		prefix uint16
		hashes [][sha256.Size]byte
	}
	groups := make([]prefixMembers, 0)
	for index := 0; index < len(hashes); {
		prefix := binary.BigEndian.Uint16(hashes[index][:2])
		end := index + 1
		for end < len(hashes) && binary.BigEndian.Uint16(hashes[end][:2]) == prefix {
			end++
		}
		groups = append(groups, prefixMembers{prefix: prefix, hashes: hashes[index:end]})
		index = end
	}
	byBlock := make(map[byte][]outcomeLeafReferenceV5)
	for _, group := range groups {
		for start, chunk := 0, uint32(0); start < len(group.hashes); start, chunk = start+outcomeLeafChunkSize, chunk+1 {
			end := start + outcomeLeafChunkSize
			if end > len(group.hashes) {
				end = len(group.hashes)
			}
			payload := canonicalOutcomeLeafV5(group.prefix, chunk, group.hashes[start:end])
			digest := sha256HexV5(payload)
			leaf := OutcomeHashLeafV5{LeafSHA256: digest, Prefix16: group.prefix, ChunkIndex: chunk,
				Cardinality: end - start, Payload: payload}
			if existing, present := result.Leaves[digest]; present && !sameOutcomeLeafV5(existing, leaf) {
				return OutcomeHashSetObjectsV5{}, errors.New("V5 leaf SHA-256 collision")
			}
			result.Leaves[digest] = leaf
			prefix8 := byte(group.prefix >> 8)
			byBlock[prefix8] = append(byBlock[prefix8], outcomeLeafReferenceV5{prefix16: group.prefix, chunk: chunk,
				digest: digest, cardinality: leaf.Cardinality})
		}
	}
	blockRefs := make([]outcomeBlockReferenceV5, 0, len(byBlock))
	for prefix := 0; prefix <= 255; prefix++ {
		refs := byBlock[byte(prefix)]
		if len(refs) == 0 {
			continue
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].prefix16 != refs[j].prefix16 {
				return refs[i].prefix16 < refs[j].prefix16
			}
			return refs[i].chunk < refs[j].chunk
		})
		manifest, cardinality, err := canonicalOutcomeBlockV5(byte(prefix), refs)
		if err != nil {
			return OutcomeHashSetObjectsV5{}, err
		}
		digest := sha256HexV5(manifest)
		block := OutcomeHashBlockV5{BlockSHA256: digest, Prefix8: byte(prefix), Cardinality: cardinality, Manifest: manifest}
		if existing, present := result.Blocks[digest]; present && !sameOutcomeBlockV5(existing, block) {
			return OutcomeHashSetObjectsV5{}, errors.New("V5 block SHA-256 collision")
		}
		result.Blocks[digest] = block
		blockRefs = append(blockRefs, outcomeBlockReferenceV5{prefix: byte(prefix), digest: digest, cardinality: cardinality})
	}
	root, err := canonicalOutcomeRootV5(int64(len(hashes)), blockRefs)
	if err != nil {
		return OutcomeHashSetObjectsV5{}, err
	}
	result.Set = OutcomeSetV5{SetSHA256: sha256HexV5(root), Cardinality: int64(len(hashes)), BlockCount: len(blockRefs),
		RootManifest: root}
	return result, nil
}

// DifferenceAndUnion computes exact novelty and a new immutable object graph.
// The root is verified from its canonical objects before it is trusted.
func DifferenceAndUnion(root OutcomeHashSetObjectsV5, candidate OutcomeCandidateV5) (OutcomeHashSetObjectsV5, [][sha256.Size]byte, error) {
	if err := VerifySetDigest(root); err != nil {
		return OutcomeHashSetObjectsV5{}, nil, err
	}
	candidates := uniqueOutcomeHashesV5(candidate.Hashes)
	rootMembers := root.Members
	if rootMembers == nil {
		var err error
		rootMembers, err = decodeOutcomeMembersV5(root)
		if err != nil {
			return OutcomeHashSetObjectsV5{}, nil, err
		}
	}
	rootMembers = uniqueOutcomeHashesV5(rootMembers)
	union := make([][sha256.Size]byte, 0, len(rootMembers)+len(candidates))
	novel := make([][sha256.Size]byte, 0, len(candidates))
	left, right := 0, 0
	for left < len(rootMembers) || right < len(candidates) {
		switch {
		case left >= len(rootMembers):
			union = append(union, candidates[right])
			novel = append(novel, candidates[right])
			right++
		case right >= len(candidates):
			union = append(union, rootMembers[left])
			left++
		default:
			comparison := bytes.Compare(rootMembers[left][:], candidates[right][:])
			switch {
			case comparison < 0:
				union = append(union, rootMembers[left])
				left++
			case comparison > 0:
				union = append(union, candidates[right])
				novel = append(novel, candidates[right])
				right++
			default:
				union = append(union, rootMembers[left])
				left++
				right++
			}
		}
	}
	built, err := buildOutcomeHashSetFromBinaryV5(union)
	return built, novel, err
}

func VerifySetDigest(objects OutcomeHashSetObjectsV5) error {
	if err := verifyOutcomeRootV5(objects.Set); err != nil {
		return err
	}
	if objects.Set.BlockCount != len(objects.Blocks) {
		return errors.New("V5 outcome set verification requires the complete block graph")
	}
	if objects.Set.Cardinality < 0 || len(objects.Set.RootManifest) == 0 || sha256HexV5(objects.Set.RootManifest) != objects.Set.SetSHA256 {
		return errors.New("invalid V5 outcome set digest or cardinality")
	}
	members, err := decodeOutcomeMembersV5(objects)
	if err != nil {
		return err
	}
	rebuilt, err := buildOutcomeHashSetFromBinaryV5(members)
	if err != nil {
		return err
	}
	if rebuilt.Set.SetSHA256 != objects.Set.SetSHA256 || rebuilt.Set.Cardinality != objects.Set.Cardinality ||
		!bytes.Equal(rebuilt.Set.RootManifest, objects.Set.RootManifest) {
		return errors.New("V5 outcome set object graph disagrees with its root")
	}
	for digest, want := range rebuilt.Blocks {
		got, present := objects.Blocks[digest]
		if !present || !sameOutcomeBlockV5(got, want) {
			return errors.New("V5 outcome set is missing a canonical block")
		}
	}
	return nil
}

func verifyOutcomeRootV5(set OutcomeSetV5) error {
	if set.Cardinality < 0 || set.BlockCount < 0 || set.BlockCount > 256 || len(set.RootManifest) == 0 ||
		sha256HexV5(set.RootManifest) != set.SetSHA256 {
		return errors.New("invalid V5 outcome set digest or cardinality")
	}
	refs, err := parseV5RootManifestReferences(set.RootManifest, set.Cardinality, set.BlockCount)
	if err != nil {
		return err
	}
	var total int64
	for _, ref := range refs {
		total += ref.cardinality
	}
	if total != set.Cardinality {
		return errors.New("V5 root block cardinality disagrees with set")
	}
	return nil
}

func canonicalOutcomeRootV5(cardinality int64, refs []outcomeBlockReferenceV5) ([]byte, error) {
	if cardinality < 0 || len(refs) > 256 {
		return nil, errors.New("invalid V5 root cardinality")
	}
	root := new(bytes.Buffer)
	root.WriteString(outcomeSetDomainV5)
	writeOutcomeStringV5(root, exposure.ProfileV5)
	writeOutcomeUint64V5(root, uint64(cardinality))
	writeOutcomeUint32V5(root, uint32(len(refs)))
	var total int64
	lastPrefix := -1
	for _, ref := range refs {
		if int(ref.prefix) <= lastPrefix || ref.cardinality < 1 {
			return nil, errors.New("invalid V5 root block reference")
		}
		lastPrefix = int(ref.prefix)
		digest, err := hex.DecodeString(ref.digest)
		if err != nil || len(digest) != sha256.Size {
			return nil, errors.New("invalid V5 root block digest")
		}
		root.WriteByte(ref.prefix)
		root.Write(digest)
		writeOutcomeUint64V5(root, uint64(ref.cardinality))
		if ref.cardinality > int64(^uint64(0)>>1)-total {
			return nil, errors.New("V5 root cardinality overflow")
		}
		total += ref.cardinality
	}
	if total != cardinality {
		return nil, errors.New("V5 root block cardinality disagrees with set")
	}
	return root.Bytes(), nil
}

func canonicalOutcomeLeafV5(prefix uint16, chunk uint32, hashes [][sha256.Size]byte) []byte {
	payload := new(bytes.Buffer)
	payload.WriteString(outcomeLeafDomainV5)
	_ = binary.Write(payload, binary.BigEndian, prefix)
	writeOutcomeUint32V5(payload, chunk)
	writeOutcomeUint32V5(payload, uint32(len(hashes)))
	for _, hash := range hashes {
		payload.Write(hash[:])
	}
	return payload.Bytes()
}

func canonicalOutcomeBlockV5(prefix byte, refs []outcomeLeafReferenceV5) ([]byte, int64, error) {
	manifest := new(bytes.Buffer)
	manifest.WriteString(outcomeBlockDomainV5)
	manifest.WriteByte(prefix)
	writeOutcomeUint32V5(manifest, uint32(len(refs)))
	var cardinality int64
	for _, ref := range refs {
		if byte(ref.prefix16>>8) != prefix || ref.cardinality < 1 {
			return nil, 0, errors.New("invalid V5 leaf reference")
		}
		manifest.WriteByte(byte(ref.prefix16))
		writeOutcomeUint32V5(manifest, ref.chunk)
		digest, err := hex.DecodeString(ref.digest)
		if err != nil || len(digest) != sha256.Size {
			return nil, 0, errors.New("invalid V5 leaf digest")
		}
		manifest.Write(digest)
		writeOutcomeUint32V5(manifest, uint32(ref.cardinality))
		cardinality += int64(ref.cardinality)
	}
	return manifest.Bytes(), cardinality, nil
}

func decodeOutcomeMembersV5(objects OutcomeHashSetObjectsV5) ([][sha256.Size]byte, error) {
	members := make([][sha256.Size]byte, 0, objects.Set.Cardinality)
	for _, leaf := range objects.Leaves {
		if sha256HexV5(leaf.Payload) != leaf.LeafSHA256 {
			return nil, errors.New("invalid V5 outcome leaf digest")
		}
		decoded, prefix, chunk, err := decodeOutcomeLeafV5(leaf.Payload)
		if err != nil || prefix != leaf.Prefix16 || chunk != leaf.ChunkIndex || len(decoded) != leaf.Cardinality {
			return nil, errors.New("invalid V5 outcome leaf metadata")
		}
		members = append(members, decoded...)
	}
	members = uniqueOutcomeHashesV5(members)
	if int64(len(members)) != objects.Set.Cardinality {
		return nil, errors.New("V5 outcome leaf cardinality disagrees with root")
	}
	return members, nil
}

func decodeOutcomeLeafV5(payload []byte) ([][sha256.Size]byte, uint16, uint32, error) {
	reader := bytes.NewReader(payload)
	domain := make([]byte, len(outcomeLeafDomainV5))
	if _, err := reader.Read(domain); err != nil || string(domain) != outcomeLeafDomainV5 {
		return nil, 0, 0, errors.New("invalid V5 leaf domain")
	}
	var prefix uint16
	if err := binary.Read(reader, binary.BigEndian, &prefix); err != nil {
		return nil, 0, 0, err
	}
	chunk, err := readOutcomeUint32V5(reader)
	if err != nil {
		return nil, 0, 0, err
	}
	count, err := readOutcomeUint32V5(reader)
	if err != nil || count == 0 || count > outcomeLeafChunkSize || reader.Len() != int(count)*sha256.Size {
		return nil, 0, 0, errors.New("invalid V5 leaf count")
	}
	result := make([][sha256.Size]byte, count)
	for index := range result {
		if _, err := reader.Read(result[index][:]); err != nil || binary.BigEndian.Uint16(result[index][:2]) != prefix {
			return nil, 0, 0, errors.New("invalid V5 leaf member")
		}
		if index > 0 && bytes.Compare(result[index-1][:], result[index][:]) >= 0 {
			return nil, 0, 0, errors.New("V5 leaf members are not strictly ordered")
		}
	}
	return result, prefix, chunk, nil
}

func decodeOutcomeHashV5(text string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if len(text) != sha256.Size*2 || text != string(bytes.ToLower([]byte(text))) {
		return result, errors.New("V5 outcome member must be lowercase SHA-256")
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return result, errors.New("V5 outcome member must be lowercase SHA-256")
	}
	copy(result[:], decoded)
	return result, nil
}

func uniqueOutcomeHashesV5(input [][sha256.Size]byte) [][sha256.Size]byte {
	result := append([][sha256.Size]byte(nil), input...)
	sortOutcomeHashesV5(result)
	write := 0
	for _, hash := range result {
		if write > 0 && bytes.Equal(result[write-1][:], hash[:]) {
			continue
		}
		result[write] = hash
		write++
	}
	return result[:write]
}

func sortOutcomeHashesV5(hashes [][sha256.Size]byte) {
	sort.Slice(hashes, func(i, j int) bool { return bytes.Compare(hashes[i][:], hashes[j][:]) < 0 })
}

func sameOutcomeLeafV5(left, right OutcomeHashLeafV5) bool {
	return left.Prefix16 == right.Prefix16 && left.ChunkIndex == right.ChunkIndex &&
		left.Cardinality == right.Cardinality && bytes.Equal(left.Payload, right.Payload)
}

func sameOutcomeBlockV5(left, right OutcomeHashBlockV5) bool {
	return left.Prefix8 == right.Prefix8 && left.Cardinality == right.Cardinality && bytes.Equal(left.Manifest, right.Manifest)
}

func sha256HexV5(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func writeOutcomeStringV5(buffer *bytes.Buffer, value string) {
	writeOutcomeUint32V5(buffer, uint32(len(value)))
	buffer.WriteString(value)
}

func writeOutcomeUint32V5(buffer *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	buffer.Write(encoded[:])
}

func writeOutcomeUint64V5(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	buffer.Write(encoded[:])
}

func readOutcomeUint32V5(reader *bytes.Reader) (uint32, error) {
	var encoded [4]byte
	if _, err := reader.Read(encoded[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(encoded[:]), nil
}

func OutcomeHashTextV5(hash [sha256.Size]byte) string { return hex.EncodeToString(hash[:]) }

func (set OutcomeSetV5) String() string {
	return fmt.Sprintf("%s/%d", set.SetSHA256, set.Cardinality)
}
