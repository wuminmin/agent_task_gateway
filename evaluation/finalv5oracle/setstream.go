package finalv5oracle

import (
	"bufio"
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const StreamingSemanticSetVersion = "TASKGATE-FINAL-V5-SET-ALGEBRA-V1"

const defaultStreamSetBufferMembers = 64 * 1024

// SemanticMemberStream emits canonical lowercase SHA-256 member identities.
// The callback must not be retained after the stream returns.
type SemanticMemberStream func(yield func(string) error) error

type StreamSetOptions struct {
	// MaxInMemoryMembers bounds the unsorted chunk. Zero selects 65,536.
	MaxInMemoryMembers int
	// CaptureMembers retains this many lexically first members as a proof
	// sample. Members contains the whole set only when it fits this bound.
	CaptureMembers int
	// TempDir optionally selects the parent of the private spill directory.
	TempDir string
}

type StreamSetStats struct {
	InputMembers        int64 `json:"input_members"`
	DuplicateMembers    int64 `json:"duplicate_members"`
	PeakBufferedMembers int   `json:"peak_buffered_members"`
	SpillRuns           int   `json:"spill_runs"`
	PeakMergeHeads      int   `json:"peak_merge_heads"`
}

// StreamSetSummary is a bounded-memory ordinary-set commitment. SetSHA256 is
// role-bound using the same public evaluation format as EvaluateSetAlgebra.
type StreamSetSummary struct {
	Role            string         `json:"role"`
	Cardinality     int64          `json:"cardinality"`
	SetSHA256       string         `json:"set_sha256"`
	Members         []string       `json:"members,omitempty"`
	MembersComplete bool           `json:"members_complete"`
	SampleMembers   []string       `json:"sample_members,omitempty"`
	Stats           StreamSetStats `json:"stats"`
}

// SummarizeSemanticSet is the single-role convenience wrapper around the
// multi-role external sorter.
func SummarizeSemanticSet(role string, stream SemanticMemberStream, options StreamSetOptions) (StreamSetSummary, error) {
	summaries, err := SummarizeSemanticSetRoles([]string{role}, stream, options)
	if err != nil {
		return StreamSetSummary{}, err
	}
	return summaries[role], nil
}

// SummarizeSemanticSetRoles sorts and deduplicates one stream once, then emits
// commitments for multiple manifest roles. Memory is O(chunk size + run
// count), never O(set cardinality). Runtime spill files are removed on return.
func SummarizeSemanticSetRoles(roles []string, stream SemanticMemberStream, options StreamSetOptions) (map[string]StreamSetSummary, error) {
	return summarizeSemanticSetRoles(roles, stream, options, nil)
}

// SummarizeUnitWitnessSemanticSetRoles sorts one duplicate-free semantic
// member stream once and returns both its role-bound set summaries and the V2
// witness-multiset commitment in which every member has multiplicity one.
// Scale count(*) release facts use this because a role label is not a witness.
func SummarizeUnitWitnessSemanticSetRoles(roles []string, stream SemanticMemberStream, options StreamSetOptions) (map[string]StreamSetSummary, string, error) {
	var witness string
	summaries, err := summarizeSemanticSetRoles(roles, stream, options,
		func(cardinality int64, members func(func([sha256.Size]byte) error) error) error {
			if cardinality > int64(^uint(0)>>1)/2 {
				return errors.New("unit witness member count exceeds the platform integer range")
			}
			target := sha256.New()
			oracleWriteHashString(target, "witness-multiset")
			if cardinality == 0 {
				writeUint64(target, 1)
				oracleWriteHashString(target, "empty")
				witness = hex.EncodeToString(target.Sum(nil))
				return nil
			}
			writeUint64(target, uint64(cardinality)*2)
			err := members(func(member [sha256.Size]byte) error {
				oracleWriteHashString(target, hex.EncodeToString(member[:]))
				oracleWriteHashString(target, "00000000000000000001")
				return nil
			})
			if err != nil {
				return err
			}
			witness = hex.EncodeToString(target.Sum(nil))
			return nil
		})
	return summaries, witness, err
}

type orderedSemanticMemberSink func(int64, func(func([sha256.Size]byte) error) error) error

func summarizeSemanticSetRoles(roles []string, stream SemanticMemberStream, options StreamSetOptions,
	orderedSink orderedSemanticMemberSink) (map[string]StreamSetSummary, error) {
	if len(roles) == 0 || stream == nil {
		return nil, errors.New("streaming semantic set requires roles and a member stream")
	}
	seenRoles := make(map[string]bool, len(roles))
	for _, role := range roles {
		if !streamSetRole(role) || seenRoles[role] {
			return nil, fmt.Errorf("invalid or duplicate streaming semantic-set role %q", role)
		}
		seenRoles[role] = true
	}
	limit := options.MaxInMemoryMembers
	if limit == 0 {
		limit = defaultStreamSetBufferMembers
	}
	if limit < 2 {
		return nil, errors.New("streaming semantic-set memory bound must be at least two members")
	}
	if options.CaptureMembers < 0 {
		return nil, errors.New("streaming semantic-set capture bound is negative")
	}

	buffer := make([]string, 0, limit)
	stats := StreamSetStats{}
	var spillDirectory string
	var runPaths []string
	cleanup := func() {
		if spillDirectory != "" {
			_ = os.RemoveAll(spillDirectory)
		}
	}
	defer cleanup()

	flush := func() error {
		if len(buffer) == 0 {
			return nil
		}
		if spillDirectory == "" {
			var err error
			spillDirectory, err = os.MkdirTemp(options.TempDir, "taskgate-final-v5-set-")
			if err != nil {
				return fmt.Errorf("create semantic-set spill directory: %w", err)
			}
		}
		sort.Strings(buffer)
		path := filepath.Join(spillDirectory, fmt.Sprintf("run-%06d.bin", len(runPaths)))
		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create semantic-set spill run: %w", err)
		}
		writer := bufio.NewWriterSize(file, 128*1024)
		var previous string
		for index, member := range buffer {
			if index > 0 && member == previous {
				continue
			}
			decoded, _ := hex.DecodeString(member)
			if _, err := writer.Write(decoded); err != nil {
				_ = file.Close()
				return fmt.Errorf("write semantic-set spill run: %w", err)
			}
			previous = member
		}
		if err := writer.Flush(); err != nil {
			_ = file.Close()
			return fmt.Errorf("flush semantic-set spill run: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close semantic-set spill run: %w", err)
		}
		runPaths = append(runPaths, path)
		buffer = buffer[:0]
		return nil
	}

	err := stream(func(member string) error {
		if !validSHA256(member) {
			return fmt.Errorf("semantic-set input member %d is not canonical SHA-256", stats.InputMembers+1)
		}
		if len(buffer) == limit {
			if err := flush(); err != nil {
				return err
			}
		}
		buffer = append(buffer, member)
		stats.InputMembers++
		if len(buffer) > stats.PeakBufferedMembers {
			stats.PeakBufferedMembers = len(buffer)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(runPaths) == 0 {
		sort.Strings(buffer)
		unique := buffer[:0]
		for _, member := range buffer {
			if len(unique) == 0 || member != unique[len(unique)-1] {
				unique = append(unique, member)
			}
		}
		stats.DuplicateMembers = stats.InputMembers - int64(len(unique))
		if orderedSink != nil {
			if stats.DuplicateMembers != 0 {
				return nil, errors.New("unit witness semantic stream contains duplicate members")
			}
			if err := orderedSink(int64(len(unique)), func(yield func([sha256.Size]byte) error) error {
				for _, member := range unique {
					decoded, _ := hex.DecodeString(member)
					var value [sha256.Size]byte
					copy(value[:], decoded)
					if err := yield(value); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return nil, err
			}
		}
		return streamSummariesFromHex(roles, unique, options.CaptureMembers, stats), nil
	}
	if err := flush(); err != nil {
		return nil, err
	}
	stats.SpillRuns = len(runPaths)
	return mergeStreamSetRuns(roles, runPaths, spillDirectory, options.CaptureMembers, stats, orderedSink)
}

func mergeStreamSetRuns(roles, runPaths []string, spillDirectory string, captureLimit int, stats StreamSetStats,
	orderedSink orderedSemanticMemberSink) (map[string]StreamSetSummary, error) {
	runs := make([]streamSetRun, len(runPaths))
	for index, path := range runPaths {
		file, err := os.Open(path)
		if err != nil {
			closeStreamSetRuns(runs)
			return nil, fmt.Errorf("open semantic-set spill run: %w", err)
		}
		runs[index] = streamSetRun{file: file, reader: bufio.NewReaderSize(file, 128*1024)}
	}
	defer closeStreamSetRuns(runs)

	heads := &streamSetHeap{}
	heap.Init(heads)
	for index := range runs {
		value, found, err := readStreamSetMember(runs[index].reader)
		if err != nil {
			return nil, err
		}
		if found {
			heap.Push(heads, streamSetHead{member: value, run: index})
		}
	}
	stats.PeakMergeHeads = heads.Len()
	uniquePath := filepath.Join(spillDirectory, "unique-members.bin")
	uniqueFile, err := os.Create(uniquePath)
	if err != nil {
		return nil, fmt.Errorf("create merged semantic-set stream: %w", err)
	}
	writer := bufio.NewWriterSize(uniqueFile, 128*1024)
	var cardinality int64
	var previous [sha256.Size]byte
	havePrevious := false
	sample := make([]string, 0, captureLimit)
	for heads.Len() > 0 {
		head := heap.Pop(heads).(streamSetHead)
		if !havePrevious || head.member != previous {
			if _, err := writer.Write(head.member[:]); err != nil {
				_ = uniqueFile.Close()
				return nil, fmt.Errorf("write merged semantic-set stream: %w", err)
			}
			cardinality++
			if len(sample) < captureLimit {
				sample = append(sample, hex.EncodeToString(head.member[:]))
			}
			previous = head.member
			havePrevious = true
		}
		next, found, err := readStreamSetMember(runs[head.run].reader)
		if err != nil {
			_ = uniqueFile.Close()
			return nil, err
		}
		if found {
			heap.Push(heads, streamSetHead{member: next, run: head.run})
			if heads.Len() > stats.PeakMergeHeads {
				stats.PeakMergeHeads = heads.Len()
			}
		}
	}
	if err := writer.Flush(); err != nil {
		_ = uniqueFile.Close()
		return nil, fmt.Errorf("flush merged semantic-set stream: %w", err)
	}
	if err := uniqueFile.Close(); err != nil {
		return nil, fmt.Errorf("close merged semantic-set stream: %w", err)
	}
	stats.DuplicateMembers = stats.InputMembers - cardinality
	if orderedSink != nil {
		if stats.DuplicateMembers != 0 {
			return nil, errors.New("unit witness semantic stream contains duplicate members")
		}
		if err := orderedSink(cardinality, func(yield func([sha256.Size]byte) error) error {
			file, err := os.Open(uniquePath)
			if err != nil {
				return fmt.Errorf("open merged semantic-set stream for witness: %w", err)
			}
			defer file.Close()
			reader := bufio.NewReaderSize(file, 128*1024)
			for {
				member, found, readErr := readStreamSetMember(reader)
				if readErr != nil {
					return readErr
				}
				if !found {
					return nil
				}
				if err := yield(member); err != nil {
					return err
				}
			}
		}); err != nil {
			return nil, err
		}
	}
	digests, err := streamSetRoleDigestsFromRaw(roles, uniquePath, cardinality)
	if err != nil {
		return nil, err
	}
	return assembleStreamSummaries(roles, digests, cardinality, sample, captureLimit, stats), nil
}

func oracleWriteHashString(target hash.Hash, value string) {
	writeUint64(target, uint64(len(value)))
	_, _ = target.Write([]byte(value))
}

func streamSummariesFromHex(roles, members []string, captureLimit int, stats StreamSetStats) map[string]StreamSetSummary {
	hashers := streamSetRoleHashers(roles, int64(len(members)))
	for _, member := range members {
		decoded, _ := hex.DecodeString(member)
		for _, target := range hashers {
			streamWriteFramed(target, decoded)
		}
	}
	digests := make(map[string]string, len(roles))
	for role, target := range hashers {
		digests[role] = hex.EncodeToString(target.Sum(nil))
	}
	sampleCount := len(members)
	if sampleCount > captureLimit {
		sampleCount = captureLimit
	}
	sample := append([]string(nil), members[:sampleCount]...)
	return assembleStreamSummaries(roles, digests, int64(len(members)), sample, captureLimit, stats)
}

func streamSetRoleDigestsFromRaw(roles []string, path string, cardinality int64) (map[string]string, error) {
	hashers := streamSetRoleHashers(roles, cardinality)
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open merged semantic-set stream for hashing: %w", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 128*1024)
	for {
		member, found, err := readStreamSetMember(reader)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		for _, target := range hashers {
			streamWriteFramed(target, member[:])
		}
	}
	digests := make(map[string]string, len(roles))
	for role, target := range hashers {
		digests[role] = hex.EncodeToString(target.Sum(nil))
	}
	return digests, nil
}

func assembleStreamSummaries(roles []string, digests map[string]string, cardinality int64, sample []string, captureLimit int, stats StreamSetStats) map[string]StreamSetSummary {
	complete := cardinality <= int64(captureLimit)
	result := make(map[string]StreamSetSummary, len(roles))
	for _, role := range roles {
		summary := StreamSetSummary{Role: role, Cardinality: cardinality, SetSHA256: digests[role],
			MembersComplete: complete, SampleMembers: append([]string(nil), sample...), Stats: stats}
		if complete {
			summary.Members = append([]string(nil), sample...)
		}
		result[role] = summary
	}
	return result
}

func streamSetRoleHashers(roles []string, cardinality int64) map[string]hash.Hash {
	result := make(map[string]hash.Hash, len(roles))
	for _, role := range roles {
		target := sha256.New()
		streamWriteFramed(target, []byte(StreamingSemanticSetVersion+"/"+role))
		streamWriteUint64(target, uint64(cardinality))
		result[role] = target
	}
	return result
}

func streamWriteFramed(target hash.Hash, value []byte) {
	streamWriteUint64(target, uint64(len(value)))
	_, _ = target.Write(value)
}

func streamWriteUint64(target hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = target.Write(encoded[:])
}

func streamSetRole(role string) bool {
	switch role {
	case "candidate", "existing", "overlap", "novel", "union":
		return true
	default:
		return false
	}
}

type streamSetRun struct {
	file   *os.File
	reader *bufio.Reader
}

func closeStreamSetRuns(runs []streamSetRun) {
	for _, run := range runs {
		if run.file != nil {
			_ = run.file.Close()
		}
	}
}

func readStreamSetMember(reader *bufio.Reader) ([sha256.Size]byte, bool, error) {
	var member [sha256.Size]byte
	_, err := io.ReadFull(reader, member[:])
	if errors.Is(err, io.EOF) {
		return member, false, nil
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return member, false, errors.New("semantic-set spill run is truncated")
	}
	if err != nil {
		return member, false, fmt.Errorf("read semantic-set spill run: %w", err)
	}
	return member, true, nil
}

type streamSetHead struct {
	member [sha256.Size]byte
	run    int
}

type streamSetHeap []streamSetHead

func (values streamSetHeap) Len() int { return len(values) }

func (values streamSetHeap) Less(i, j int) bool {
	comparison := bytes.Compare(values[i].member[:], values[j].member[:])
	if comparison != 0 {
		return comparison < 0
	}
	return values[i].run < values[j].run
}

func (values streamSetHeap) Swap(i, j int) { values[i], values[j] = values[j], values[i] }

func (values *streamSetHeap) Push(value any) { *values = append(*values, value.(streamSetHead)) }

func (values *streamSetHeap) Pop() any {
	previous := *values
	last := previous[len(previous)-1]
	*values = previous[:len(previous)-1]
	return last
}
