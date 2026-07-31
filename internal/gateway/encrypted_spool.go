package gateway

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	querySpoolThreshold = int64(128 << 20)
	querySpoolChunkSize = 1 << 20
	querySpoolMagic     = "TGSPOOL1"
)

// encryptedQuerySpool is an adaptive, query-private result buffer. Payloads
// at or below threshold never touch disk. Once threshold is crossed, all
// buffered plaintext is moved into independently authenticated AES-GCM chunks
// in a mode-0700 private directory and the in-memory plaintext buffer is
// cleared. Its ephemeral key is never persisted.
type encryptedQuerySpool struct {
	baseDir   string
	threshold int64
	aad       []byte

	memory  bytes.Buffer
	pending []byte
	size    int64
	chunks  uint64

	dir  string
	path string
	file *os.File
	key  []byte
	aead cipher.AEAD

	sealed   bool
	closed   bool
	opened   bool
	unlinked bool
	err      error
}

func newEncryptedQuerySpool(baseDir, taskID, queryID string, threshold int64) (*encryptedQuerySpool, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(queryID) == "" || threshold < 1 {
		return nil, errors.New("encrypted query spool requires task, query, and a positive threshold")
	}
	if baseDir == "" {
		baseDir = os.TempDir()
	}
	return &encryptedQuerySpool{
		baseDir: baseDir, threshold: threshold,
		aad: []byte("taskgate-query-spool-v1\x00" + taskID + "\x00" + queryID),
	}, nil
}

func (s *encryptedQuerySpool) Write(value []byte) (int, error) {
	if s == nil || s.closed {
		return 0, os.ErrClosed
	}
	if s.sealed {
		return 0, errors.New("encrypted query spool is sealed")
	}
	if s.err != nil {
		return 0, s.err
	}
	if int64(len(value)) > int64(^uint64(0)>>1)-s.size {
		s.err = errors.New("encrypted query spool size overflow")
		return 0, s.err
	}
	if s.file == nil && s.size+int64(len(value)) <= s.threshold {
		written, err := s.memory.Write(value)
		s.size += int64(written)
		if err != nil {
			s.err = err
		}
		return written, err
	}
	if s.file == nil {
		if err := s.startDiskSpool(); err != nil {
			s.err = err
			return 0, err
		}
	}
	written, err := s.writeEncrypted(value)
	s.size += int64(written)
	if err != nil {
		s.err = err
	}
	return written, err
}

func (s *encryptedQuerySpool) startDiskSpool() (err error) {
	directory, err := os.MkdirTemp(s.baseDir, ".taskgate-query-spool-")
	if err != nil {
		return fmt.Errorf("create encrypted query spool directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(directory)
		}
	}()
	if err = os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("restrict encrypted query spool directory: %w", err)
	}
	path := filepath.Join(directory, "payload.spool")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create encrypted query spool: %w", err)
	}
	defer func() {
		if err != nil {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	key := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, key); err != nil {
		zeroBytes(key)
		return fmt.Errorf("generate encrypted query spool key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		zeroBytes(key)
		return fmt.Errorf("initialize encrypted query spool: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		zeroBytes(key)
		return fmt.Errorf("initialize encrypted query spool AEAD: %w", err)
	}
	if _, err = file.Write([]byte(querySpoolMagic)); err != nil {
		zeroBytes(key)
		return fmt.Errorf("write encrypted query spool header: %w", err)
	}

	s.dir, s.path, s.file, s.key, s.aead = directory, path, file, key, aead
	if s.memory.Len() != 0 {
		plaintext := s.memory.Bytes()
		if _, err = s.writeEncrypted(plaintext); err != nil {
			return err
		}
		zeroBytes(plaintext)
		s.memory.Reset()
	}
	return nil
}

func (s *encryptedQuerySpool) writeEncrypted(value []byte) (int, error) {
	written := 0
	for len(value) != 0 {
		space := querySpoolChunkSize - len(s.pending)
		amount := len(value)
		if amount > space {
			amount = space
		}
		s.pending = append(s.pending, value[:amount]...)
		value = value[amount:]
		written += amount
		if len(s.pending) == querySpoolChunkSize {
			if err := s.flushChunk(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (s *encryptedQuerySpool) flushChunk() error {
	if len(s.pending) == 0 {
		return nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate encrypted query spool nonce: %w", err)
	}
	ciphertext := s.aead.Seal(nil, nonce, s.pending, spoolChunkAAD(s.aad, s.chunks))
	var header [8]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(len(nonce)))
	binary.BigEndian.PutUint32(header[4:8], uint32(len(ciphertext)))
	if err := writeAll(s.file, header[:]); err != nil {
		return fmt.Errorf("write encrypted query spool frame: %w", err)
	}
	if err := writeAll(s.file, nonce); err != nil {
		return fmt.Errorf("write encrypted query spool nonce: %w", err)
	}
	if err := writeAll(s.file, ciphertext); err != nil {
		return fmt.Errorf("write encrypted query spool ciphertext: %w", err)
	}
	zeroBytes(s.pending)
	s.pending = s.pending[:0]
	s.chunks++
	return nil
}

func (s *encryptedQuerySpool) Seal() error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if s.sealed {
		return s.err
	}
	if s.err != nil {
		return s.err
	}
	if s.file != nil {
		if err := s.flushChunk(); err != nil {
			s.err = err
			return err
		}
		if err := s.file.Sync(); err != nil {
			s.err = fmt.Errorf("sync encrypted query spool: %w", err)
			return s.err
		}
		if err := s.file.Close(); err != nil {
			s.err = fmt.Errorf("close encrypted query spool writer: %w", err)
			return s.err
		}
		s.file = nil
	}
	s.sealed = true
	return nil
}

func (s *encryptedQuerySpool) Spilled() bool { return s != nil && s.path != "" }

func (s *encryptedQuerySpool) Bytes() ([]byte, error) {
	if s == nil || s.closed {
		return nil, os.ErrClosed
	}
	if err := s.Seal(); err != nil {
		return nil, err
	}
	if s.Spilled() {
		return nil, errors.New("spilled query result has no in-memory plaintext")
	}
	return s.memory.Bytes(), nil
}

func (s *encryptedQuerySpool) Open() (io.ReadCloser, error) {
	if s == nil || s.closed {
		return nil, os.ErrClosed
	}
	if err := s.Seal(); err != nil {
		return nil, err
	}
	if !s.Spilled() {
		return io.NopCloser(bytes.NewReader(s.memory.Bytes())), nil
	}
	if s.opened {
		return nil, errors.New("encrypted query spool is single-read")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open encrypted query spool: %w", err)
	}
	header := make([]byte, len(querySpoolMagic))
	if _, err := io.ReadFull(file, header); err != nil || string(header) != querySpoolMagic {
		_ = file.Close()
		return nil, errors.New("encrypted query spool header is invalid")
	}
	// Unlink before plaintext streaming begins. On Unix the already-open file
	// descriptor remains readable, while failures, cancellation, or process
	// death cannot leave a named ciphertext artifact behind.
	if err := os.Remove(s.path); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("unlink encrypted query spool: %w", err)
	}
	if err := os.Remove(s.dir); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("remove encrypted query spool directory: %w", err)
	}
	s.opened = true
	s.unlinked = true
	return &encryptedQuerySpoolReader{
		file: file, aead: s.aead, aad: s.aad,
		expectedChunks: s.chunks, expectedSize: s.size,
	}, nil
}

func (s *encryptedQuerySpool) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	var cleanup error
	if s.file != nil {
		cleanup = errors.Join(cleanup, s.file.Close())
		s.file = nil
	}
	zeroBytes(s.memory.Bytes())
	s.memory.Reset()
	zeroBytes(s.pending)
	s.pending = nil
	zeroBytes(s.key)
	s.key = nil
	s.aead = nil
	if s.path != "" && !s.unlinked {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup = errors.Join(cleanup, err)
		}
	}
	if s.dir != "" && !s.unlinked {
		if err := os.Remove(s.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanup = errors.Join(cleanup, err)
		}
	}
	return cleanup
}

type encryptedQuerySpoolReader struct {
	file           *os.File
	aead           cipher.AEAD
	aad            []byte
	expectedChunks uint64
	expectedSize   int64
	next           uint64
	produced       int64
	current        []byte
	offset         int
	finished       bool
}

func (r *encryptedQuerySpoolReader) Read(target []byte) (int, error) {
	if r == nil || r.file == nil {
		return 0, os.ErrClosed
	}
	if len(target) == 0 {
		return 0, nil
	}
	for r.offset == len(r.current) {
		zeroBytes(r.current)
		r.current = nil
		r.offset = 0
		if r.finished {
			return 0, io.EOF
		}
		if err := r.readChunk(); err != nil {
			return 0, err
		}
	}
	n := copy(target, r.current[r.offset:])
	r.offset += n
	return n, nil
}

func (r *encryptedQuerySpoolReader) readChunk() error {
	if r.next == r.expectedChunks {
		var extra [1]byte
		n, err := r.file.Read(extra[:])
		if n != 0 || (err != nil && !errors.Is(err, io.EOF)) || r.produced != r.expectedSize {
			return errors.New("encrypted query spool trailer is invalid")
		}
		r.finished = true
		return io.EOF
	}
	var header [8]byte
	if _, err := io.ReadFull(r.file, header[:]); err != nil {
		return fmt.Errorf("read encrypted query spool frame: %w", err)
	}
	nonceLength := int(binary.BigEndian.Uint32(header[0:4]))
	ciphertextLength := int(binary.BigEndian.Uint32(header[4:8]))
	if nonceLength != r.aead.NonceSize() || ciphertextLength <= r.aead.Overhead() ||
		ciphertextLength > querySpoolChunkSize+r.aead.Overhead() {
		return errors.New("encrypted query spool frame length is invalid")
	}
	nonce := make([]byte, nonceLength)
	ciphertext := make([]byte, ciphertextLength)
	if _, err := io.ReadFull(r.file, nonce); err != nil {
		return fmt.Errorf("read encrypted query spool nonce: %w", err)
	}
	if _, err := io.ReadFull(r.file, ciphertext); err != nil {
		return fmt.Errorf("read encrypted query spool ciphertext: %w", err)
	}
	plaintext, err := r.aead.Open(nil, nonce, ciphertext, spoolChunkAAD(r.aad, r.next))
	zeroBytes(ciphertext)
	if err != nil {
		return errors.New("encrypted query spool authentication failed")
	}
	if int64(len(plaintext)) > r.expectedSize-r.produced {
		zeroBytes(plaintext)
		return errors.New("encrypted query spool plaintext length is invalid")
	}
	r.current = plaintext
	r.produced += int64(len(plaintext))
	r.next++
	return nil
}

func (r *encryptedQuerySpoolReader) Close() error {
	if r == nil || r.file == nil {
		return nil
	}
	zeroBytes(r.current)
	r.current = nil
	err := r.file.Close()
	r.file = nil
	r.aead = nil
	return err
}

func spoolChunkAAD(prefix []byte, ordinal uint64) []byte {
	result := make([]byte, len(prefix)+1+8)
	copy(result, prefix)
	result[len(prefix)] = 0
	binary.BigEndian.PutUint64(result[len(prefix)+1:], ordinal)
	return result
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// writeStoredQueryResult preserves storedQueryResult's JSON field order while
// writing one row at a time. This avoids json.Marshal allocating a second copy
// of a large complete result before the adaptive spool can enforce its limit.
func writeStoredQueryResult(writer io.Writer, result storedQueryResult) error {
	if err := writeAll(writer, []byte(`{"columns":`)); err != nil {
		return err
	}
	columns, err := json.Marshal(result.Columns)
	if err != nil {
		return err
	}
	if err := writeAll(writer, columns); err != nil {
		return err
	}
	if err := writeAll(writer, []byte(`,"rows":`)); err != nil {
		return err
	}
	if result.Rows == nil {
		if err := writeAll(writer, []byte("null")); err != nil {
			return err
		}
	} else {
		if err := writeAll(writer, []byte("[")); err != nil {
			return err
		}
		for index, row := range result.Rows {
			if index != 0 {
				if err := writeAll(writer, []byte(",")); err != nil {
					return err
				}
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				return err
			}
			if err := writeAll(writer, encoded); err != nil {
				return err
			}
		}
		if err := writeAll(writer, []byte("]")); err != nil {
			return err
		}
	}
	if err := writeAll(writer, []byte(`,"row_count":`+strconv.FormatInt(result.RowCount, 10)+`,"database_ms":`+
		strconv.FormatInt(result.DatabaseMS, 10))); err != nil {
		return err
	}
	if len(result.ComponentMS) != 0 {
		component, err := json.Marshal(result.ComponentMS)
		if err != nil {
			return err
		}
		if err := writeAll(writer, []byte(`,"component_ms":`)); err != nil {
			return err
		}
		if err := writeAll(writer, component); err != nil {
			return err
		}
	}
	if err := writeAll(writer, []byte(`,"limited":`+strconv.FormatBool(result.Limited))); err != nil {
		return err
	}
	optional := []struct {
		name    string
		value   any
		present bool
	}{
		{name: "query_plan", value: result.QueryPlan, present: result.QueryPlan != nil},
		{name: "sql_profile", value: result.SQLProfile, present: result.SQLProfile != ""},
		{name: "plan_digest", value: result.PlanDigest, present: result.PlanDigest != ""},
		{name: "output_format", value: result.OutputFormat, present: result.OutputFormat != ""},
		{name: "display_columns", value: result.DisplayColumns, present: len(result.DisplayColumns) != 0},
		{name: "result_order", value: result.ResultOrder, present: len(result.ResultOrder) != 0},
		{name: "semantic_columns", value: result.SemanticColumns, present: len(result.SemanticColumns) != 0},
	}
	for _, field := range optional {
		if !field.present {
			continue
		}
		encoded, err := json.Marshal(field.value)
		if err != nil {
			return err
		}
		if err := writeAll(writer, []byte(`,"`+field.name+`":`)); err != nil {
			return err
		}
		if err := writeAll(writer, encoded); err != nil {
			return err
		}
	}
	return writeAll(writer, []byte(`}`))
}
