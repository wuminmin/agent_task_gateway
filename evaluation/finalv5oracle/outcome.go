package finalv5oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

const (
	OpaqueOutcomeGeneratorVersion = "taskgate-final-v5-outcome-merkle-control-v1"
	opaqueOutcomeMemberDomain     = "TASKGATE-FINAL-V5-OPAQUE-OUTCOME-MEMBER-V1\x00"
)

// OpaqueOutcomeMember is deliberately not a CanonicalFact. Outcome Merkle
// control cells exercise ordinary member-set mechanics; they must not be
// presented as legal semantic V5 atom/composite payloads.
type OpaqueOutcomeMember struct {
	SHA256 string `json:"sha256"`
}

type OpaqueOutcomeSetRequest struct {
	RootCardinality      int64
	CandidateCardinality int64
	OverlapCardinality   int64
	SampleIndex          int
	Seed                 int64
	SetOptions           StreamSetOptions
}

type OpaqueOutcomeSetReport struct {
	GeneratorVersion string           `json:"generator_version"`
	SampleIndex      int              `json:"sample_index"`
	Seed             int64            `json:"seed"`
	Existing         StreamSetSummary `json:"existing"`
	Candidate        StreamSetSummary `json:"candidate"`
	Overlap          StreamSetSummary `json:"overlap"`
	Novel            StreamSetSummary `json:"novel"`
	Union            StreamSetSummary `json:"union"`
}

// GenerateOpaqueOutcomeSets constructs one physical-control sample. Overlap
// members are a deterministic, duplicate-free subset of the root. Novel
// members occupy a disjoint tagged hash domain.
func GenerateOpaqueOutcomeSets(request OpaqueOutcomeSetRequest) (OpaqueOutcomeSetReport, error) {
	if request.RootCardinality < 0 || request.CandidateCardinality < 0 || request.OverlapCardinality < 0 ||
		request.OverlapCardinality > request.RootCardinality ||
		request.OverlapCardinality > request.CandidateCardinality || request.SampleIndex < 1 {
		return OpaqueOutcomeSetReport{}, errors.New("opaque outcome set cardinalities or sample index are invalid")
	}
	rootStream := func(yield func(string) error) error {
		for index := int64(0); index < request.RootCardinality; index++ {
			if err := yield(OpaqueOutcomeRootMember(index).SHA256); err != nil {
				return err
			}
		}
		return nil
	}
	overlapStream := func(yield func(string) error) error {
		return streamOpaqueOverlapMembers(request, yield)
	}
	novelStream := func(yield func(string) error) error {
		for index := int64(0); index < request.CandidateCardinality-request.OverlapCardinality; index++ {
			if err := yield(OpaqueOutcomeNovelMember(request.Seed, request.SampleIndex, index).SHA256); err != nil {
				return err
			}
		}
		return nil
	}
	candidateStream := func(yield func(string) error) error {
		if err := overlapStream(yield); err != nil {
			return err
		}
		return novelStream(yield)
	}
	unionStream := func(yield func(string) error) error {
		if err := rootStream(yield); err != nil {
			return err
		}
		return novelStream(yield)
	}

	report := OpaqueOutcomeSetReport{GeneratorVersion: OpaqueOutcomeGeneratorVersion,
		SampleIndex: request.SampleIndex, Seed: request.Seed}
	var err error
	if request.OverlapCardinality == request.CandidateCardinality {
		candidate, buildErr := SummarizeSemanticSetRoles([]string{"candidate", "overlap"}, candidateStream, request.SetOptions)
		if buildErr != nil {
			return OpaqueOutcomeSetReport{}, buildErr
		}
		report.Candidate, report.Overlap = candidate["candidate"], candidate["overlap"]
	} else {
		if report.Candidate, err = SummarizeSemanticSet("candidate", candidateStream, request.SetOptions); err != nil {
			return OpaqueOutcomeSetReport{}, err
		}
		if report.Overlap, err = SummarizeSemanticSet("overlap", overlapStream, request.SetOptions); err != nil {
			return OpaqueOutcomeSetReport{}, err
		}
	}
	if request.OverlapCardinality == request.CandidateCardinality {
		root, buildErr := SummarizeSemanticSetRoles([]string{"existing", "union"}, rootStream, request.SetOptions)
		if buildErr != nil {
			return OpaqueOutcomeSetReport{}, buildErr
		}
		report.Existing, report.Union = root["existing"], root["union"]
	} else {
		if report.Existing, err = SummarizeSemanticSet("existing", rootStream, request.SetOptions); err != nil {
			return OpaqueOutcomeSetReport{}, err
		}
		if report.Union, err = SummarizeSemanticSet("union", unionStream, request.SetOptions); err != nil {
			return OpaqueOutcomeSetReport{}, err
		}
	}
	if report.Novel, err = SummarizeSemanticSet("novel", novelStream, request.SetOptions); err != nil {
		return OpaqueOutcomeSetReport{}, err
	}
	if report.Existing.Cardinality != request.RootCardinality ||
		report.Candidate.Cardinality != request.CandidateCardinality ||
		report.Overlap.Cardinality != request.OverlapCardinality ||
		report.Novel.Cardinality != request.CandidateCardinality-request.OverlapCardinality ||
		report.Union.Cardinality != request.RootCardinality+request.CandidateCardinality-request.OverlapCardinality {
		return OpaqueOutcomeSetReport{}, errors.New("opaque outcome generator violated exact set algebra")
	}
	return report, nil
}

func OpaqueOutcomeRootMember(index int64) OpaqueOutcomeMember {
	return opaqueOutcomeMember("root", 0, 0, index)
}

func OpaqueOutcomeNovelMember(seed int64, sampleIndex int, index int64) OpaqueOutcomeMember {
	return opaqueOutcomeMember("novel", seed, int64(sampleIndex), index)
}

func opaqueOutcomeMember(kind string, seed, sample, index int64) OpaqueOutcomeMember {
	h := sha256.New()
	_, _ = h.Write([]byte(opaqueOutcomeMemberDomain))
	opaqueWriteString(h, kind)
	opaqueWriteUint64(h, uint64(seed))
	opaqueWriteUint64(h, uint64(sample))
	opaqueWriteUint64(h, uint64(index))
	return OpaqueOutcomeMember{SHA256: hex.EncodeToString(h.Sum(nil))}
}

func streamOpaqueOverlapMembers(request OpaqueOutcomeSetRequest, yield func(string) error) error {
	if request.OverlapCardinality == 0 {
		return nil
	}
	start, step := opaqueRootPermutation(request.RootCardinality, request.Seed, request.SampleIndex)
	position := start
	for count := int64(0); count < request.OverlapCardinality; count++ {
		if err := yield(OpaqueOutcomeRootMember(position).SHA256); err != nil {
			return err
		}
		position = (position + step) % request.RootCardinality
	}
	return nil
}

func opaqueRootPermutation(root int64, seed int64, sample int) (int64, int64) {
	var payload bytes.Buffer
	_, _ = payload.WriteString("TASKGATE-FINAL-V5-OPAQUE-ROOT-PERMUTATION-V1\x00")
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(seed))
	_, _ = payload.Write(encoded[:])
	binary.BigEndian.PutUint64(encoded[:], uint64(sample))
	_, _ = payload.Write(encoded[:])
	digest := sha256.Sum256(payload.Bytes())
	start := int64(binary.BigEndian.Uint64(digest[:8]) % uint64(root))
	step := int64(binary.BigEndian.Uint64(digest[8:16]) % uint64(root))
	if step == 0 {
		step = 1
	}
	for dependencyGCD(step, root) != 1 {
		step++
		if step == root {
			step = 1
		}
	}
	return start, step
}

type V5OutcomeVectorInput struct {
	Atoms                   []V5PredicateAtomInput
	QueryNormalFormVersion  string
	QueryNormalFormSHA256   string
	ResultObservationSHA256 string
	VisibleRows             int64
	PredicateContextSHA256  string
}

// V5OutcomeVector contains only legal, independently encoded semantic V5
// payloads. It is intentionally separate from OpaqueOutcomeSetReport.
type V5OutcomeVector struct {
	Atoms              []CanonicalFact `json:"atoms"`
	Composite          CanonicalFact   `json:"composite"`
	PredicateSetSHA256 string          `json:"predicate_set_sha256"`
	Members            []string        `json:"members"`
	OutcomeSetSHA256   string          `json:"outcome_set_sha256"`
}

// BuildV5OutcomeVector deduplicates exact atoms, binds the composite to the
// resulting atom set, and always returns a composite—even for zero visible
// rows and an empty atom footprint.
func BuildV5OutcomeVector(input V5OutcomeVectorInput) (V5OutcomeVector, error) {
	if !validSHA256(input.PredicateContextSHA256) {
		return V5OutcomeVector{}, errors.New("V5 outcome vector predicate context is not SHA-256")
	}
	byHash := make(map[string]CanonicalFact, len(input.Atoms))
	for index, atomInput := range input.Atoms {
		if atomInput.PredicateContextSHA256 == "" {
			atomInput.PredicateContextSHA256 = input.PredicateContextSHA256
		}
		if atomInput.PredicateContextSHA256 != input.PredicateContextSHA256 {
			return V5OutcomeVector{}, fmt.Errorf("V5 atom %d has a different predicate context", index+1)
		}
		atom, err := BuildV5PredicateAtomFact(atomInput)
		if err != nil {
			return V5OutcomeVector{}, fmt.Errorf("build V5 atom %d: %w", index+1, err)
		}
		if previous, exists := byHash[atom.SHA256]; exists && !bytes.Equal(previous.Payload, atom.Payload) {
			return V5OutcomeVector{}, errors.New("V5 atom SHA-256 collision")
		}
		byHash[atom.SHA256] = atom
	}
	atoms := make([]CanonicalFact, 0, len(byHash))
	for _, atom := range byHash {
		atoms = append(atoms, atom)
	}
	sort.Slice(atoms, func(i, j int) bool { return atoms[i].SHA256 < atoms[j].SHA256 })
	predicateSet, err := HashV5PredicateSet(atoms)
	if err != nil {
		return V5OutcomeVector{}, err
	}
	composite, err := BuildV5CompositeOutcomeFact(V5CompositeOutcomeInput{
		QueryNormalFormVersion: input.QueryNormalFormVersion, QueryNormalFormSHA256: input.QueryNormalFormSHA256,
		ResultObservationSHA256: input.ResultObservationSHA256, VisibleRows: input.VisibleRows,
		PredicateContextSHA256: input.PredicateContextSHA256, PredicateSetSHA256: predicateSet,
		PredicateAtomCount: int64(len(atoms)),
	})
	if err != nil {
		return V5OutcomeVector{}, err
	}
	members := make([]string, 0, len(atoms)+1)
	for _, atom := range atoms {
		members = append(members, atom.SHA256)
	}
	members = append(members, composite.SHA256)
	sort.Strings(members)
	summary, err := SummarizeSemanticSet("candidate", func(yield func(string) error) error {
		for _, member := range members {
			if err := yield(member); err != nil {
				return err
			}
		}
		return nil
	}, StreamSetOptions{MaxInMemoryMembers: maxOutcomeVectorBuffer(len(members)), CaptureMembers: len(members)})
	if err != nil {
		return V5OutcomeVector{}, err
	}
	return V5OutcomeVector{Atoms: atoms, Composite: composite, PredicateSetSHA256: predicateSet,
		Members: members, OutcomeSetSHA256: summary.SetSHA256}, nil
}

func maxOutcomeVectorBuffer(members int) int {
	if members < 2 {
		return 2
	}
	return members
}

func opaqueWriteString(target interface{ Write([]byte) (int, error) }, value string) {
	opaqueWriteUint64(target, uint64(len(value)))
	_, _ = target.Write([]byte(value))
}

func opaqueWriteUint64(target interface{ Write([]byte) (int, error) }, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}
