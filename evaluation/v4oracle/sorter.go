package v4oracle

import (
	"bufio"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultSortMemory = int64(16 << 20)
	maxRecordPayload  = uint64(1 << 20)
)

type factRecord struct {
	Hash         [sha256.Size]byte
	Multiplicity uint64
	Payload      []byte
}

func compareFactRecord(left, right factRecord) int {
	for index := range left.Hash {
		if left.Hash[index] < right.Hash[index] {
			return -1
		}
		if left.Hash[index] > right.Hash[index] {
			return 1
		}
	}
	return 0
}

// externalSorter creates sorted immutable runs and later k-way merges them.
// Its heap is O(run count); no phase retains all million records.
type externalSorter struct {
	directory  string
	name       string
	limit      int64
	buffer     []factRecord
	bytes      int64
	runs       []string
	spool      int64
	runDigests []string
	maxRecords int
	peakRSS    uint64
	adds       uint64
	tracker    *sortResourceTracker
	closed     bool
}

type sortResourceTracker struct {
	currentRecords int
	maximumRecords int
	peakRSS        uint64
}

func newExternalSorter(directory, name string, limit int64, tracker *sortResourceTracker) (*externalSorter, error) {
	if limit <= 0 {
		limit = defaultSortMemory
	}
	if directory == "" || name == "" || limit < 1<<20 {
		return nil, errors.New("external sorter requires a directory, name, and at least 1 MiB")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	return &externalSorter{directory: directory, name: name, limit: limit, tracker: tracker}, nil
}

func (s *externalSorter) Add(record factRecord) error {
	if s == nil || s.closed || record.Multiplicity == 0 || uint64(len(record.Payload)) > maxRecordPayload {
		return errors.New("invalid external-sort record")
	}
	record.Payload = append([]byte(nil), record.Payload...)
	s.buffer = append(s.buffer, record)
	s.adds++
	if s.tracker != nil {
		s.tracker.currentRecords++
		if s.tracker.currentRecords > s.tracker.maximumRecords {
			s.tracker.maximumRecords = s.tracker.currentRecords
		}
	}
	if len(s.buffer) > s.maxRecords {
		s.maxRecords = len(s.buffer)
	}
	if s.adds&4095 == 0 {
		if rss := currentRSSBytes(); rss > s.peakRSS {
			s.peakRSS = rss
			if s.tracker != nil && rss > s.tracker.peakRSS {
				s.tracker.peakRSS = rss
			}
		}
	}
	s.bytes += int64(sha256.Size + 8 + 8 + len(record.Payload) + 32)
	if s.bytes >= s.limit {
		return s.flush()
	}
	return nil
}

func (s *externalSorter) flush() error {
	if len(s.buffer) == 0 {
		return nil
	}
	sort.Slice(s.buffer, func(i, j int) bool { return compareFactRecord(s.buffer[i], s.buffer[j]) < 0 })
	path := filepath.Join(s.directory, fmt.Sprintf("%s-%06d.run", s.name, len(s.runs)))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	runHash := sha256.New()
	writer := bufio.NewWriterSize(io.MultiWriter(file, runHash), 64<<10)
	for _, record := range s.buffer {
		if err = writeFactRecord(writer, record); err != nil {
			break
		}
	}
	if flushErr := writer.Flush(); err == nil {
		err = flushErr
	}
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	s.spool += info.Size()
	s.runDigests = append(s.runDigests, fmt.Sprintf("%x", runHash.Sum(nil)))
	s.runs = append(s.runs, path)
	if s.tracker != nil {
		s.tracker.currentRecords -= len(s.buffer)
	}
	s.buffer = nil
	s.bytes = 0
	return nil
}

func (s *externalSorter) Finish() (*mergeIterator, error) {
	if s == nil || s.closed {
		return nil, errors.New("external sorter already finished")
	}
	if err := s.flush(); err != nil {
		return nil, err
	}
	s.closed = true
	if rss := currentRSSBytes(); rss > s.peakRSS {
		s.peakRSS = rss
		if s.tracker != nil && rss > s.tracker.peakRSS {
			s.tracker.peakRSS = rss
		}
	}
	return s.Iterator()
}

func currentRSSBytes() uint64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "VmRSS:" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return value * 1024
			}
		}
	}
	return 0
}

func (s *externalSorter) Iterator() (*mergeIterator, error) {
	if s == nil || !s.closed {
		return nil, errors.New("external sorter is not finished")
	}
	iterator := &mergeIterator{}
	for index, path := range s.runs {
		file, err := os.Open(path)
		if err != nil {
			iterator.Close()
			return nil, err
		}
		cursor := &runCursor{index: index, file: file, reader: bufio.NewReaderSize(file, 64<<10)}
		cursor.record, cursor.err = readFactRecord(cursor.reader)
		if cursor.err == nil {
			heap.Push(&iterator.queue, cursor)
		} else if errors.Is(cursor.err, io.EOF) {
			_ = file.Close()
		} else {
			iterator.Close()
			_ = file.Close()
			return nil, cursor.err
		}
	}
	return iterator, nil
}

func writeFactRecord(writer io.Writer, record factRecord) error {
	if _, err := writer.Write(record.Hash[:]); err != nil {
		return err
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], record.Multiplicity)
	if _, err := writer.Write(encoded[:]); err != nil {
		return err
	}
	binary.BigEndian.PutUint64(encoded[:], uint64(len(record.Payload)))
	if _, err := writer.Write(encoded[:]); err != nil {
		return err
	}
	_, err := writer.Write(record.Payload)
	return err
}

func readFactRecord(reader io.Reader) (factRecord, error) {
	var result factRecord
	if _, err := io.ReadFull(reader, result.Hash[:]); err != nil {
		return factRecord{}, err
	}
	var encoded [8]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return factRecord{}, err
	}
	result.Multiplicity = binary.BigEndian.Uint64(encoded[:])
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return factRecord{}, err
	}
	length := binary.BigEndian.Uint64(encoded[:])
	if result.Multiplicity == 0 || length > maxRecordPayload {
		return factRecord{}, errors.New("corrupt external-sort record")
	}
	result.Payload = make([]byte, int(length))
	if _, err := io.ReadFull(reader, result.Payload); err != nil {
		return factRecord{}, err
	}
	return result, nil
}

type runCursor struct {
	index  int
	file   *os.File
	reader *bufio.Reader
	record factRecord
	err    error
}

type cursorHeap []*runCursor

func (h cursorHeap) Len() int { return len(h) }
func (h cursorHeap) Less(i, j int) bool {
	comparison := compareFactRecord(h[i].record, h[j].record)
	return comparison < 0 || comparison == 0 && h[i].index < h[j].index
}
func (h cursorHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *cursorHeap) Push(value any) { *h = append(*h, value.(*runCursor)) }
func (h *cursorHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type mergeIterator struct {
	queue  cursorHeap
	closed bool
}

func (m *mergeIterator) Next() (factRecord, error) {
	if m == nil || m.closed || m.queue.Len() == 0 {
		return factRecord{}, io.EOF
	}
	cursor := heap.Pop(&m.queue).(*runCursor)
	result := cursor.record
	cursor.record, cursor.err = readFactRecord(cursor.reader)
	switch {
	case cursor.err == nil:
		heap.Push(&m.queue, cursor)
	case errors.Is(cursor.err, io.EOF):
		_ = cursor.file.Close()
	default:
		_ = cursor.file.Close()
		return factRecord{}, cursor.err
	}
	return result, nil
}

// NextCombined consolidates equal hashes and sums multiplicity. Payloads must
// be byte-identical, making a hash collision a hard oracle failure.
func (m *mergeIterator) NextCombined() (factRecord, error) {
	first, err := m.Next()
	if err != nil {
		return factRecord{}, err
	}
	for m.queue.Len() != 0 && compareFactRecord(first, m.queue[0].record) == 0 {
		next, nextErr := m.Next()
		if nextErr != nil {
			return factRecord{}, nextErr
		}
		if string(first.Payload) != string(next.Payload) {
			return factRecord{}, errors.New("FactHash collision has distinct canonical payloads")
		}
		if ^uint64(0)-first.Multiplicity < next.Multiplicity {
			return factRecord{}, errors.New("witness multiplicity overflow")
		}
		first.Multiplicity += next.Multiplicity
	}
	return first, nil
}

func (m *mergeIterator) Close() error {
	if m == nil || m.closed {
		return nil
	}
	m.closed = true
	var result error
	for m.queue.Len() != 0 {
		cursor := heap.Pop(&m.queue).(*runCursor)
		if err := cursor.file.Close(); err != nil && result == nil {
			result = err
		}
	}
	return result
}
