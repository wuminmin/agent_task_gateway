// Package encryptedspool provides the one shared adaptive encrypted-spool
// implementation used by query results and exposure FactSets.
package encryptedspool

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Config fixes the storage format and lifecycle for one private spool.
type Config struct {
	BaseDir           string
	DirectoryPrefix   string
	FileName          string
	Magic             string
	AAD               []byte
	Threshold         int64
	ChunkSize         int
	UnlinkOnOpen      bool
	SingleRead        bool
	UnlinkImmediately bool
}

// Spool is an adaptive private byte buffer. Once Threshold is crossed, all
// plaintext is moved into independently authenticated AES-GCM chunks and the
// in-memory plaintext is cleared. Its ephemeral key is never persisted.
type Spool struct {
	config Config

	memory  bytes.Buffer
	pending []byte
	size    int64
	chunks  uint64

	dir  string
	path string
	file *os.File
	key  []byte
	aead cipher.AEAD

	spilled  bool
	sealed   bool
	closed   bool
	opened   bool
	unlinked bool
	err      error
}

// New constructs an adaptive encrypted spool without touching disk.
func New(config Config) (*Spool, error) {
	if config.Threshold < 1 || config.ChunkSize < 1 || config.Magic == "" ||
		config.DirectoryPrefix == "" || config.FileName == "" || len(config.AAD) == 0 {
		return nil, errors.New("encrypted spool requires format, AAD, and positive size limits")
	}
	if config.BaseDir == "" {
		config.BaseDir = os.TempDir()
	}
	config.AAD = append([]byte(nil), config.AAD...)
	return &Spool{config: config}, nil
}

// Write appends plaintext to the single adaptive buffer.
func (s *Spool) Write(value []byte) (int, error) {
	if s == nil || s.closed {
		return 0, os.ErrClosed
	}
	if s.sealed {
		return 0, errors.New("encrypted spool is sealed")
	}
	if s.err != nil {
		return 0, s.err
	}
	if int64(len(value)) > int64(^uint64(0)>>1)-s.size {
		s.err = errors.New("encrypted spool size overflow")
		return 0, s.err
	}
	if !s.spilled && s.size+int64(len(value)) <= s.config.Threshold {
		written, err := s.memory.Write(value)
		s.size += int64(written)
		if err != nil {
			s.err = err
		}
		return written, err
	}
	if !s.spilled {
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

func (s *Spool) startDiskSpool() (err error) {
	directory, err := os.MkdirTemp(s.config.BaseDir, s.config.DirectoryPrefix)
	if err != nil {
		return fmt.Errorf("create encrypted spool directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(directory)
		}
	}()
	if err = os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("restrict encrypted spool directory: %w", err)
	}
	path := filepath.Join(directory, s.config.FileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create encrypted spool: %w", err)
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
		return fmt.Errorf("generate encrypted spool key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		zeroBytes(key)
		return fmt.Errorf("initialize encrypted spool: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		zeroBytes(key)
		return fmt.Errorf("initialize encrypted spool AEAD: %w", err)
	}
	if _, err = file.Write([]byte(s.config.Magic)); err != nil {
		zeroBytes(key)
		return fmt.Errorf("write encrypted spool header: %w", err)
	}

	s.dir, s.path, s.file, s.key, s.aead = directory, path, file, key, aead
	s.spilled = true
	if s.memory.Len() != 0 {
		plaintext := s.memory.Bytes()
		if _, err = s.writeEncrypted(plaintext); err != nil {
			return err
		}
		zeroBytes(plaintext)
		s.memory.Reset()
	}
	if s.config.UnlinkImmediately {
		if err = os.Remove(s.path); err != nil {
			return fmt.Errorf("unlink encrypted spool: %w", err)
		}
		if err = os.Remove(s.dir); err != nil {
			return fmt.Errorf("remove encrypted spool directory: %w", err)
		}
		s.path = ""
		s.dir = ""
		s.unlinked = true
	}
	return nil
}

func (s *Spool) writeEncrypted(value []byte) (int, error) {
	written := 0
	for len(value) != 0 {
		space := s.config.ChunkSize - len(s.pending)
		amount := len(value)
		if amount > space {
			amount = space
		}
		s.pending = append(s.pending, value[:amount]...)
		value = value[amount:]
		written += amount
		if len(s.pending) == s.config.ChunkSize {
			if err := s.flushChunk(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (s *Spool) flushChunk() error {
	if len(s.pending) == 0 {
		return nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate encrypted spool nonce: %w", err)
	}
	ciphertext := s.aead.Seal(nil, nonce, s.pending, chunkAAD(s.config.AAD, s.chunks))
	var header [8]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(len(nonce)))
	binary.BigEndian.PutUint32(header[4:8], uint32(len(ciphertext)))
	if err := writeAll(s.file, header[:]); err != nil {
		return fmt.Errorf("write encrypted spool frame: %w", err)
	}
	if err := writeAll(s.file, nonce); err != nil {
		return fmt.Errorf("write encrypted spool nonce: %w", err)
	}
	if err := writeAll(s.file, ciphertext); err != nil {
		return fmt.Errorf("write encrypted spool ciphertext: %w", err)
	}
	zeroBytes(s.pending)
	s.pending = s.pending[:0]
	s.chunks++
	return nil
}

// Seal makes the spool immutable and syncs its ciphertext.
func (s *Spool) Seal() error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if s.sealed {
		return s.err
	}
	if s.err != nil {
		return s.err
	}
	if s.spilled {
		if err := s.flushChunk(); err != nil {
			s.err = err
			return err
		}
		if err := s.file.Sync(); err != nil {
			s.err = fmt.Errorf("sync encrypted spool: %w", err)
			return s.err
		}
		if err := s.file.Close(); err != nil {
			s.err = fmt.Errorf("close encrypted spool writer: %w", err)
			return s.err
		}
		s.file = nil
	}
	s.sealed = true
	return nil
}

// Spilled reports whether this buffer crossed its configured threshold.
func (s *Spool) Spilled() bool { return s != nil && s.spilled }

// Bytes returns the in-memory payload and rejects spilled buffers.
func (s *Spool) Bytes() ([]byte, error) {
	if s == nil || s.closed {
		return nil, os.ErrClosed
	}
	if err := s.Seal(); err != nil {
		return nil, err
	}
	if s.Spilled() {
		return nil, errors.New("spilled result has no in-memory plaintext")
	}
	return s.memory.Bytes(), nil
}

// Open seals and opens the complete payload. It optionally applies the query
// spool's unlink-before-plaintext and single-read lifecycle.
func (s *Spool) Open() (io.ReadCloser, error) {
	if s == nil || s.closed {
		return nil, os.ErrClosed
	}
	if err := s.Seal(); err != nil {
		return nil, err
	}
	if !s.Spilled() {
		return io.NopCloser(bytes.NewReader(s.memory.Bytes())), nil
	}
	if s.config.SingleRead && s.opened {
		return nil, errors.New("encrypted spool is single-read")
	}
	if s.path == "" {
		return nil, errors.New("sealed anonymous encrypted spool cannot be reopened")
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open encrypted spool: %w", err)
	}
	if s.config.UnlinkOnOpen {
		if err := os.Remove(s.path); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("unlink encrypted spool: %w", err)
		}
		if err := os.Remove(s.dir); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("remove encrypted spool directory: %w", err)
		}
		s.unlinked = true
	}
	s.opened = true
	return s.newReader(file)
}

// Snapshot opens a complete authenticated view without sealing the writer.
// Flushing the current partial chunk creates a stable boundary; later writes
// start a new authenticated chunk and do not change this reader's extent.
func (s *Spool) Snapshot() (io.ReadCloser, error) {
	if s == nil || s.closed {
		return nil, os.ErrClosed
	}
	if s.sealed {
		return nil, errors.New("encrypted spool is sealed")
	}
	if s.err != nil {
		return nil, s.err
	}
	if !s.Spilled() {
		return io.NopCloser(bytes.NewReader(s.memory.Bytes())), nil
	}
	if err := s.flushChunk(); err != nil {
		s.err = err
		return nil, err
	}
	if err := s.file.Sync(); err != nil {
		s.err = fmt.Errorf("sync encrypted spool snapshot: %w", err)
		return nil, s.err
	}
	var source io.ReadCloser
	if s.path != "" {
		file, err := os.Open(s.path)
		if err != nil {
			return nil, fmt.Errorf("open encrypted spool snapshot: %w", err)
		}
		source = file
	} else {
		position, err := s.file.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, fmt.Errorf("size encrypted spool snapshot: %w", err)
		}
		source = io.NopCloser(io.NewSectionReader(s.file, 0, position))
	}
	return s.newReader(source)
}

func (s *Spool) newReader(source io.ReadCloser) (io.ReadCloser, error) {
	header := make([]byte, len(s.config.Magic))
	if _, err := io.ReadFull(source, header); err != nil || string(header) != s.config.Magic {
		_ = source.Close()
		return nil, errors.New("encrypted spool header is invalid")
	}
	return &reader{
		source: source, aead: s.aead, aad: s.config.AAD, chunkSize: s.config.ChunkSize,
		expectedChunks: s.chunks, expectedSize: s.size,
	}, nil
}

// Close clears plaintext/key material and removes any still-named ciphertext.
func (s *Spool) Close() error {
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

// ChunkCount and filesystem accessors expose format state for package tests.
func (s *Spool) ChunkCount() uint64     { return s.chunks }
func (s *Spool) CiphertextPath() string { return s.path }
func (s *Spool) Directory() string      { return s.dir }

type reader struct {
	source         io.ReadCloser
	aead           cipher.AEAD
	aad            []byte
	chunkSize      int
	expectedChunks uint64
	expectedSize   int64
	next           uint64
	produced       int64
	current        []byte
	offset         int
	finished       bool
}

func (r *reader) Read(target []byte) (int, error) {
	if r == nil || r.source == nil {
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

func (r *reader) readChunk() error {
	if r.next == r.expectedChunks {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n != 0 || (err != nil && !errors.Is(err, io.EOF)) || r.produced != r.expectedSize {
			return errors.New("encrypted spool trailer is invalid")
		}
		r.finished = true
		return io.EOF
	}
	var header [8]byte
	if _, err := io.ReadFull(r.source, header[:]); err != nil {
		return fmt.Errorf("read encrypted spool frame: %w", err)
	}
	nonceLength := int(binary.BigEndian.Uint32(header[0:4]))
	ciphertextLength := int(binary.BigEndian.Uint32(header[4:8]))
	if nonceLength != r.aead.NonceSize() || ciphertextLength <= r.aead.Overhead() ||
		ciphertextLength > r.chunkSize+r.aead.Overhead() {
		return errors.New("encrypted spool frame length is invalid")
	}
	nonce := make([]byte, nonceLength)
	ciphertext := make([]byte, ciphertextLength)
	if _, err := io.ReadFull(r.source, nonce); err != nil {
		return fmt.Errorf("read encrypted spool nonce: %w", err)
	}
	if _, err := io.ReadFull(r.source, ciphertext); err != nil {
		return fmt.Errorf("read encrypted spool ciphertext: %w", err)
	}
	plaintext, err := r.aead.Open(nil, nonce, ciphertext, chunkAAD(r.aad, r.next))
	zeroBytes(ciphertext)
	if err != nil {
		return errors.New("encrypted spool authentication failed")
	}
	if int64(len(plaintext)) > r.expectedSize-r.produced {
		zeroBytes(plaintext)
		return errors.New("encrypted spool plaintext length is invalid")
	}
	r.current = plaintext
	r.produced += int64(len(plaintext))
	r.next++
	return nil
}

func (r *reader) Close() error {
	if r == nil || r.source == nil {
		return nil
	}
	zeroBytes(r.current)
	r.current = nil
	err := r.source.Close()
	r.source = nil
	r.aead = nil
	return err
}

func chunkAAD(prefix []byte, ordinal uint64) []byte {
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
