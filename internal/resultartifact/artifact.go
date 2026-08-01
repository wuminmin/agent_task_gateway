package resultartifact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	FormatParquet             = "parquet"
	EncryptionChunkedAESGCMV1 = "chunked-aes-gcm-v1"
	artifactMagic             = "TGPARQ1\n"
	artifactChunkSize         = 1 << 20
	artifactMaxFrameOverhead  = 1 << 16
	objectContentType         = "application/vnd.taskgate.parquet+encrypted"
	localStagingPrefix        = ".taskgate-parquet-"
	localStagingSuffix        = ".enc"
)

type Cipher interface {
	Encrypt(plaintext, aad []byte) (nonce, ciphertext []byte, err error)
	Decrypt(nonce, ciphertext, aad []byte) ([]byte, error)
	KeyID() string
}

type Manager struct {
	backend Backend
	cipher  Cipher
	tempDir string
}

type StageRequest struct {
	ResultID   string
	TaskID     string
	StagingKey string
	ObjectKey  string
	Columns    []Column
	Rows       [][]any
}

type StagedArtifact struct {
	ResultID      string
	TaskID        string
	StagingKey    string
	ObjectKey     string
	KeyID         string
	Format        string
	Encryption    string
	ParquetSHA256 string
	ObjectSHA256  string
	ParquetSize   int64
	ObjectSize    int64
	RowCount      int64
	ColumnCount   int
	Schema        []ColumnSchema
	ETag          string
}

// StageMetrics reports non-overlapping intervals from the existing Stage
// path. EncodeEncrypt intentionally combines Parquet encoding and inline
// encryption because the streaming implementation cannot separate them
// without changing the execution path.
type StageMetrics struct {
	EncodeEncrypt time.Duration
	Sync          time.Duration
	Put           time.Duration
	Total         time.Duration
}

// PromoteMetrics reports intervals from the existing idempotent promotion
// path. Stat covers canonical-object metadata lookup, Verify covers metadata
// checks, HashVerify covers the authenticated ciphertext read (overlapping
// Copy on a new canonical object), and DeleteStaging covers best-effort
// removal of the private staging object.
type PromoteMetrics struct {
	Stat                    time.Duration
	Copy                    time.Duration
	Verify                  time.Duration
	HashVerify              time.Duration
	DeleteStaging           time.Duration
	Total                   time.Duration
	ReusedExistingCanonical bool
}

type ArtifactRef struct {
	ResultID      string
	TaskID        string
	ObjectKey     string
	KeyID         string
	ParquetSHA256 string
	ObjectSHA256  string
	ParquetSize   int64
	ObjectSize    int64
}

func NewManager(backend Backend, cipher Cipher, tempDir string) (*Manager, error) {
	if backend == nil || cipher == nil || strings.TrimSpace(cipher.KeyID()) == "" {
		return nil, errors.New("result artifact backend and keyed cipher are required")
	}
	tempDir = strings.TrimSpace(tempDir)
	if tempDir == "" {
		tempDir = filepath.Join(os.TempDir(), "taskgate-result-artifacts")
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, fmt.Errorf("create result artifact temporary directory: %w", err)
	}
	info, err := os.Lstat(tempDir)
	if err != nil {
		return nil, fmt.Errorf("inspect result artifact temporary directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("result artifact temporary directory must be a private non-symlink directory")
	}
	return &Manager{backend: backend, cipher: cipher, tempDir: tempDir}, nil
}

func (manager *Manager) Ready(ctx context.Context) error {
	if manager == nil || manager.backend == nil {
		return errors.New("result artifact manager is unavailable")
	}
	return manager.backend.Ready(ctx)
}

// Stage produces Parquet directly into an encrypted private temporary file,
// then uploads that ciphertext under a non-canonical staging key. No plaintext
// Parquet file is ever written to local storage.
func (manager *Manager) Stage(ctx context.Context, request StageRequest) (StagedArtifact, error) {
	staged, _, err := manager.StageMeasured(ctx, request)
	return staged, err
}

// StageMeasured is Stage with observational timings. Stage delegates here so
// measured and unmeasured callers always execute identical artifact logic.
func (manager *Manager) StageMeasured(ctx context.Context, request StageRequest) (staged StagedArtifact, metrics StageMetrics, err error) {
	totalStarted := time.Now()
	defer func() { metrics.Total = time.Since(totalStarted) }()
	if manager == nil || manager.backend == nil || manager.cipher == nil {
		return staged, metrics, errors.New("result artifact manager is unavailable")
	}
	if strings.TrimSpace(request.ResultID) == "" || strings.TrimSpace(request.TaskID) == "" ||
		strings.TrimSpace(request.StagingKey) == "" || strings.TrimSpace(request.ObjectKey) == "" ||
		request.StagingKey == request.ObjectKey {
		return staged, metrics, errors.New("result, task, staging key, and canonical object key are required")
	}
	file, err := os.CreateTemp(manager.tempDir, localStagingPrefix+"*"+localStagingSuffix)
	if err != nil {
		return staged, metrics, fmt.Errorf("create encrypted Parquet staging file: %w", err)
	}
	path := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(path)
	}()
	if err := file.Chmod(0o600); err != nil {
		return staged, metrics, fmt.Errorf("restrict encrypted Parquet staging file: %w", err)
	}
	objectDigest := sha256.New()
	counted := &countingWriter{writer: io.MultiWriter(file, objectDigest)}
	encrypted, err := newArtifactEncryptWriter(counted, manager.cipher, request.TaskID, request.ResultID)
	if err != nil {
		return staged, metrics, err
	}
	encodeStarted := time.Now()
	schema, err := WriteParquet(encrypted, request.ResultID, request.Columns, request.Rows)
	if err == nil {
		err = encrypted.Close()
	}
	if err != nil {
		metrics.EncodeEncrypt = time.Since(encodeStarted)
		return staged, metrics, fmt.Errorf("encode encrypted Parquet artifact: %w", err)
	}
	metrics.EncodeEncrypt = time.Since(encodeStarted)
	syncStarted := time.Now()
	if err := file.Sync(); err != nil {
		metrics.Sync = time.Since(syncStarted)
		return staged, metrics, fmt.Errorf("sync encrypted Parquet staging file: %w", err)
	}
	metrics.Sync = time.Since(syncStarted)
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return staged, metrics, fmt.Errorf("rewind encrypted Parquet staging file: %w", err)
	}
	parquetHash := encrypted.PlaintextSHA256()
	objectHash := hex.EncodeToString(objectDigest.Sum(nil))
	metadata := map[string]string{
		"taskgate-result-id":      request.ResultID,
		"taskgate-key-id":         manager.cipher.KeyID(),
		"taskgate-format":         FormatParquet,
		"taskgate-encryption":     EncryptionChunkedAESGCMV1,
		"taskgate-parquet-sha256": parquetHash,
		"taskgate-object-sha256":  objectHash,
	}
	putStarted := time.Now()
	info, err := manager.backend.Put(ctx, request.StagingKey, file, counted.count, PutOptions{
		ContentType: objectContentType, Metadata: metadata,
	})
	metrics.Put = time.Since(putStarted)
	if err != nil {
		return staged, metrics, err
	}
	if info.Size != 0 && info.Size != counted.count {
		_ = manager.backend.Delete(context.WithoutCancel(ctx), request.StagingKey)
		return staged, metrics, errors.New("object store reported an unexpected artifact size")
	}
	staged = StagedArtifact{
		ResultID: request.ResultID, TaskID: request.TaskID, StagingKey: request.StagingKey,
		ObjectKey: request.ObjectKey, KeyID: manager.cipher.KeyID(), Format: FormatParquet,
		Encryption: EncryptionChunkedAESGCMV1, ParquetSHA256: parquetHash, ObjectSHA256: objectHash,
		ParquetSize: encrypted.PlaintextSize(), ObjectSize: counted.count,
		RowCount: int64(len(request.Rows)), ColumnCount: len(request.Columns), Schema: schema, ETag: info.ETag,
	}
	return staged, metrics, nil
}

// PurgeLocalStagingBefore removes only TaskGate's encrypted local scratch
// files. They are not canonical artifacts and exist solely while a completed
// Parquet stream is uploaded to the private staging prefix.
func (manager *Manager) PurgeLocalStagingBefore(cutoff time.Time) (int, error) {
	if manager == nil || manager.tempDir == "" || cutoff.IsZero() {
		return 0, errors.New("result artifact manager and local staging cutoff are required")
	}
	entries, err := os.ReadDir(manager.tempDir)
	if err != nil {
		return 0, fmt.Errorf("list local result staging: %w", err)
	}
	removed := 0
	var failures []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, localStagingPrefix) || !strings.HasSuffix(name, localStagingSuffix) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			failures = append(failures, fmt.Errorf("inspect local result staging %s: %w", name, infoErr))
			continue
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if removeErr := os.Remove(filepath.Join(manager.tempDir, name)); removeErr != nil {
			failures = append(failures, fmt.Errorf("delete local result staging %s: %w", name, removeErr))
			continue
		}
		removed++
	}
	return removed, errors.Join(failures...)
}

// Promote creates or verifies the deterministic canonical object. This is the
// physical publication step, not the logical availability/consumption
// boundary: Control PG may still durably report PENDING after this returns.
// Copy is deterministic and safe to retry after a crash.
func (manager *Manager) Promote(ctx context.Context, staged StagedArtifact) (ObjectInfo, error) {
	info, _, err := manager.PromoteMeasured(ctx, staged)
	return info, err
}

// PromoteMeasured is Promote with observational timings. Promote delegates
// here so compatibility callers retain the exact idempotent behavior.
func (manager *Manager) PromoteMeasured(ctx context.Context, staged StagedArtifact) (info ObjectInfo, metrics PromoteMetrics, err error) {
	totalStarted := time.Now()
	defer func() { metrics.Total = time.Since(totalStarted) }()
	if err := validateStaged(staged); err != nil {
		return info, metrics, err
	}
	statStarted := time.Now()
	existing, statErr := manager.backend.Stat(ctx, staged.ObjectKey)
	metrics.Stat += time.Since(statStarted)
	if statErr == nil {
		if objectMatches(existing, staged) {
			verifyStarted := time.Now()
			if verifyErr := manager.verifyObjectDigest(ctx, existing, staged.ObjectSHA256); verifyErr != nil {
				metrics.HashVerify += time.Since(verifyStarted)
				return info, metrics, verifyErr
			}
			metrics.HashVerify += time.Since(verifyStarted)
			deleteStarted := time.Now()
			_ = manager.backend.Delete(context.WithoutCancel(ctx), staged.StagingKey)
			metrics.DeleteStaging += time.Since(deleteStarted)
			metrics.ReusedExistingCanonical = true
			return existing, metrics, nil
		}
		return info, metrics, errors.New("canonical result object already exists with different evidence")
	} else if !errors.Is(statErr, ErrObjectNotFound) {
		return info, metrics, fmt.Errorf("check canonical result object: %w", statErr)
	}
	stagingInfo, err := manager.backend.Stat(ctx, staged.StagingKey)
	if err != nil {
		return info, metrics, fmt.Errorf("check staging result object: %w", err)
	}
	verifyStarted := time.Now()
	if stagingInfo.ETag != staged.ETag || !objectMatches(stagingInfo, staged) {
		metrics.Verify += time.Since(verifyStarted)
		return info, metrics, errors.New("staging result object differs from committed evidence")
	}
	metrics.Verify += time.Since(verifyStarted)
	copyStarted := time.Now()
	info, err = manager.backend.Copy(ctx, staged.StagingKey, staged.ObjectKey, staged.ObjectSHA256)
	metrics.Copy += time.Since(copyStarted)
	// Copy authenticates the complete ciphertext stream before the conditional
	// canonical commit, so hash verification is intentionally an overlapping
	// diagnostic for this path.
	metrics.HashVerify += metrics.Copy
	if err != nil {
		if errors.Is(err, ErrObjectAlreadyExists) {
			statStarted = time.Now()
			existing, statErr := manager.backend.Stat(ctx, staged.ObjectKey)
			metrics.Stat += time.Since(statStarted)
			if statErr == nil && objectMatches(existing, staged) {
				verifyStarted = time.Now()
				if verifyErr := manager.verifyObjectDigest(ctx, existing, staged.ObjectSHA256); verifyErr != nil {
					metrics.HashVerify += time.Since(verifyStarted)
					return info, metrics, verifyErr
				}
				metrics.HashVerify += time.Since(verifyStarted)
				deleteStarted := time.Now()
				_ = manager.backend.Delete(context.WithoutCancel(ctx), staged.StagingKey)
				metrics.DeleteStaging += time.Since(deleteStarted)
				metrics.ReusedExistingCanonical = true
				return existing, metrics, nil
			}
			if statErr != nil {
				return info, metrics, fmt.Errorf("verify concurrently created canonical result object: %w", statErr)
			}
			return info, metrics, errors.New("canonical result object already exists with different evidence")
		}
		return info, metrics, err
	}
	verifyStarted = time.Now()
	if !objectMatches(info, staged) {
		metrics.Verify += time.Since(verifyStarted)
		return info, metrics, errors.New("promoted result object failed integrity metadata checks")
	}
	metrics.Verify += time.Since(verifyStarted)
	deleteStarted := time.Now()
	if err := manager.backend.Delete(context.WithoutCancel(ctx), staged.StagingKey); err != nil {
		metrics.DeleteStaging += time.Since(deleteStarted)
		// The canonical object is authoritative. A stale private staging object is
		// harmless and is eligible for the deployment's orphan cleanup policy.
		return info, metrics, nil
	}
	metrics.DeleteStaging += time.Since(deleteStarted)
	return info, metrics, nil
}

func (manager *Manager) verifyObjectDigest(ctx context.Context, info ObjectInfo, expectedSHA256 string) error {
	expected, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expected) != sha256.Size {
		return fmt.Errorf("%w: invalid committed object digest", ErrArtifactIntegrity)
	}
	reader, err := manager.backend.Get(ctx, info.Key)
	if err != nil {
		return err
	}
	defer reader.Close()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(reader, info.Size+1))
	if err != nil {
		return err
	}
	if written != info.Size || subtle.ConstantTimeCompare(digest.Sum(nil), expected) != 1 {
		return fmt.Errorf("%w: canonical object bytes differ from committed evidence", ErrArtifactIntegrity)
	}
	return nil
}

func (manager *Manager) DeleteStaging(ctx context.Context, key string) error {
	return manager.backend.Delete(ctx, key)
}

func (manager *Manager) ListStaging(ctx context.Context, startAfter string, limit int) ([]ObjectInfo, error) {
	if manager == nil || manager.backend == nil {
		return nil, errors.New("result artifact manager is unavailable")
	}
	return manager.backend.List(ctx, "staging/", startAfter, limit)
}

func (manager *Manager) PurgeIncompleteUploadsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	if manager == nil || manager.backend == nil || cutoff.IsZero() {
		return 0, errors.New("result artifact manager and incomplete-upload cutoff are required")
	}
	cleaner, ok := manager.backend.(IncompleteUploadCleaner)
	if !ok {
		return 0, nil
	}
	return cleaner.PurgeIncompleteUploadsBefore(ctx, cutoff)
}

func (manager *Manager) Delete(ctx context.Context, key string) error {
	return manager.backend.Delete(ctx, key)
}

func (manager *Manager) OpenParquet(ctx context.Context, reference ArtifactRef) (io.ReadCloser, error) {
	if manager == nil || manager.backend == nil || manager.cipher == nil ||
		strings.TrimSpace(reference.ResultID) == "" || strings.TrimSpace(reference.TaskID) == "" ||
		strings.TrimSpace(reference.ObjectKey) == "" || reference.KeyID != manager.cipher.KeyID() ||
		!validDigest(reference.ParquetSHA256) || !validDigest(reference.ObjectSHA256) ||
		reference.ParquetSize < 0 || reference.ObjectSize <= 0 {
		return nil, errors.New("invalid result artifact reference")
	}
	object, err := manager.backend.Get(ctx, reference.ObjectKey)
	if err != nil {
		return nil, err
	}
	reader, err := newArtifactDecryptReader(object, manager.cipher, reference)
	if err != nil {
		_ = object.Close()
		return nil, err
	}
	return reader, nil
}

func (manager *Manager) ReadParquet(ctx context.Context, reference ArtifactRef, maximum int64) ([]byte, error) {
	if maximum <= 0 || (reference.ParquetSize > 0 && reference.ParquetSize > maximum) {
		return nil, errors.New("Parquet artifact exceeds the preview read limit")
	}
	reader, err := manager.OpenParquet(ctx, reference)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	limited := io.LimitReader(reader, maximum+1)
	value, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > maximum {
		return nil, errors.New("Parquet artifact exceeds the preview read limit")
	}
	return value, nil
}

func validateStaged(staged StagedArtifact) error {
	if staged.ResultID == "" || staged.TaskID == "" || staged.StagingKey == "" || staged.ObjectKey == "" ||
		staged.StagingKey == staged.ObjectKey || staged.Format != FormatParquet ||
		staged.Encryption != EncryptionChunkedAESGCMV1 || staged.KeyID == "" || staged.ETag == "" ||
		!validDigest(staged.ParquetSHA256) || !validDigest(staged.ObjectSHA256) ||
		staged.ParquetSize < 0 || staged.ObjectSize <= 0 || staged.RowCount < 0 || staged.ColumnCount < 0 {
		return errors.New("invalid staged result artifact")
	}
	return nil
}

func objectMatches(info ObjectInfo, staged StagedArtifact) bool {
	if info.Size != staged.ObjectSize {
		return false
	}
	return metadataValue(info.Metadata, "taskgate-object-sha256") == staged.ObjectSHA256 &&
		metadataValue(info.Metadata, "taskgate-parquet-sha256") == staged.ParquetSHA256 &&
		metadataValue(info.Metadata, "taskgate-result-id") == staged.ResultID
}

func metadataValue(metadata map[string]string, key string) string {
	for candidate, value := range metadata {
		candidate = strings.ToLower(strings.TrimPrefix(candidate, "x-amz-meta-"))
		if candidate == key {
			return value
		}
	}
	return ""
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

type artifactEncryptWriter struct {
	output    io.Writer
	cipher    Cipher
	aad       []byte
	pending   []byte
	plainHash hash.Hash
	plainSize int64
	ordinal   uint64
	closed    bool
}

func newArtifactEncryptWriter(output io.Writer, cipher Cipher, taskID, resultID string) (*artifactEncryptWriter, error) {
	if output == nil || cipher == nil || taskID == "" || resultID == "" {
		return nil, errors.New("encrypted artifact writer requires output, cipher, task, and result")
	}
	if err := writeAll(output, []byte(artifactMagic)); err != nil {
		return nil, err
	}
	return &artifactEncryptWriter{output: output, cipher: cipher,
		aad: []byte("taskgate-parquet-artifact-v1\x00" + taskID + "\x00" + resultID), plainHash: sha256.New()}, nil
}

func (writer *artifactEncryptWriter) Write(value []byte) (int, error) {
	if writer == nil || writer.closed {
		return 0, os.ErrClosed
	}
	original := len(value)
	_, _ = writer.plainHash.Write(value)
	writer.plainSize += int64(original)
	for len(value) != 0 {
		amount := artifactChunkSize - len(writer.pending)
		if amount > len(value) {
			amount = len(value)
		}
		writer.pending = append(writer.pending, value[:amount]...)
		value = value[amount:]
		if len(writer.pending) == artifactChunkSize {
			if err := writer.flush(); err != nil {
				return original - len(value), err
			}
		}
	}
	return original, nil
}

func (writer *artifactEncryptWriter) Close() error {
	if writer == nil || writer.closed {
		return nil
	}
	writer.closed = true
	return writer.flush()
}

func (writer *artifactEncryptWriter) flush() error {
	if len(writer.pending) == 0 {
		return nil
	}
	plaintext := writer.pending
	nonce, ciphertext, err := writer.cipher.Encrypt(plaintext, artifactChunkAAD(writer.aad, writer.ordinal))
	if err != nil {
		return err
	}
	if len(nonce) == 0 || len(nonce) > artifactMaxFrameOverhead || len(ciphertext) == 0 ||
		len(ciphertext) > artifactChunkSize+artifactMaxFrameOverhead {
		return errors.New("artifact encryption produced an invalid frame")
	}
	var header [12]byte
	binary.BigEndian.PutUint32(header[0:4], uint32(len(plaintext)))
	binary.BigEndian.PutUint32(header[4:8], uint32(len(nonce)))
	binary.BigEndian.PutUint32(header[8:12], uint32(len(ciphertext)))
	for _, value := range [][]byte{header[:], nonce, ciphertext} {
		if err := writeAll(writer.output, value); err != nil {
			return err
		}
	}
	zeroBytes(plaintext)
	writer.pending = writer.pending[:0]
	writer.ordinal++
	return nil
}

func (writer *artifactEncryptWriter) PlaintextSHA256() string {
	return hex.EncodeToString(writer.plainHash.Sum(nil))
}

func (writer *artifactEncryptWriter) PlaintextSize() int64 { return writer.plainSize }

type artifactDecryptReader struct {
	source       io.ReadCloser
	input        *bufio.Reader
	cipher       Cipher
	aad          []byte
	expected     ArtifactRef
	objectHash   hash.Hash
	objectReader *countingReader
	plainHash    hash.Hash
	plainCount   int64
	ordinal      uint64
	current      []byte
	offset       int
	terminal     error
}

func newArtifactDecryptReader(source io.ReadCloser, cipher Cipher, reference ArtifactRef) (*artifactDecryptReader, error) {
	objectHash := sha256.New()
	counter := &countingReader{reader: io.TeeReader(source, objectHash)}
	buffered := bufio.NewReader(counter)
	magic := make([]byte, len(artifactMagic))
	if _, err := io.ReadFull(buffered, magic); err != nil || string(magic) != artifactMagic {
		return nil, fmt.Errorf("%w: invalid encrypted Parquet artifact header", ErrArtifactIntegrity)
	}
	return &artifactDecryptReader{source: source, input: buffered, cipher: cipher,
		aad:      []byte("taskgate-parquet-artifact-v1\x00" + reference.TaskID + "\x00" + reference.ResultID),
		expected: reference, objectHash: objectHash, objectReader: counter, plainHash: sha256.New()}, nil
}

func (reader *artifactDecryptReader) Read(output []byte) (int, error) {
	if reader == nil || reader.terminal != nil {
		if reader != nil && errors.Is(reader.terminal, io.EOF) {
			return 0, io.EOF
		}
		if reader == nil {
			return 0, os.ErrClosed
		}
		return 0, reader.terminal
	}
	if len(output) == 0 {
		return 0, nil
	}
	for reader.offset == len(reader.current) {
		if err := reader.nextFrame(); err != nil {
			reader.terminal = err
			return 0, err
		}
	}
	written := copy(output, reader.current[reader.offset:])
	reader.offset += written
	if reader.offset == len(reader.current) {
		zeroBytes(reader.current)
		reader.current = nil
		reader.offset = 0
	}
	return written, nil
}

func (reader *artifactDecryptReader) nextFrame() error {
	var header [12]byte
	read, err := io.ReadFull(reader.input, header[:])
	if errors.Is(err, io.EOF) && read == 0 {
		return reader.verifyEOF()
	}
	if err != nil {
		return fmt.Errorf("%w: encrypted Parquet artifact is truncated", ErrArtifactIntegrity)
	}
	plainLength := int(binary.BigEndian.Uint32(header[0:4]))
	nonceLength := int(binary.BigEndian.Uint32(header[4:8]))
	cipherLength := int(binary.BigEndian.Uint32(header[8:12]))
	if plainLength <= 0 || plainLength > artifactChunkSize || nonceLength <= 0 || nonceLength > artifactMaxFrameOverhead ||
		cipherLength <= 0 || cipherLength > artifactChunkSize+artifactMaxFrameOverhead {
		return fmt.Errorf("%w: encrypted Parquet artifact frame is invalid", ErrArtifactIntegrity)
	}
	nonce := make([]byte, nonceLength)
	ciphertext := make([]byte, cipherLength)
	if _, err := io.ReadFull(reader.input, nonce); err != nil {
		return fmt.Errorf("%w: encrypted Parquet artifact nonce is truncated", ErrArtifactIntegrity)
	}
	if _, err := io.ReadFull(reader.input, ciphertext); err != nil {
		return fmt.Errorf("%w: encrypted Parquet artifact ciphertext is truncated", ErrArtifactIntegrity)
	}
	plaintext, err := reader.cipher.Decrypt(nonce, ciphertext, artifactChunkAAD(reader.aad, reader.ordinal))
	zeroBytes(ciphertext)
	if err != nil || len(plaintext) != plainLength {
		zeroBytes(plaintext)
		return fmt.Errorf("%w: encrypted Parquet artifact authentication failed", ErrArtifactIntegrity)
	}
	_, _ = reader.plainHash.Write(plaintext)
	reader.plainCount += int64(len(plaintext))
	reader.current = plaintext
	reader.offset = 0
	reader.ordinal++
	return nil
}

func (reader *artifactDecryptReader) verifyEOF() error {
	// The buffered reader has consumed the entire tee stream at this point.
	objectHash := hex.EncodeToString(reader.objectHash.Sum(nil))
	plainHash := hex.EncodeToString(reader.plainHash.Sum(nil))
	if reader.objectReader == nil || reader.objectReader.count != reader.expected.ObjectSize ||
		reader.plainCount != reader.expected.ParquetSize || plainHash != reader.expected.ParquetSHA256 ||
		objectHash != reader.expected.ObjectSHA256 {
		return fmt.Errorf("%w: encrypted Parquet artifact digest or size mismatch", ErrArtifactIntegrity)
	}
	return io.EOF
}

func (reader *artifactDecryptReader) Close() error {
	if reader == nil || reader.source == nil {
		return nil
	}
	zeroBytes(reader.current)
	reader.current = nil
	err := reader.source.Close()
	reader.source = nil
	return err
}

func artifactChunkAAD(prefix []byte, ordinal uint64) []byte {
	result := make([]byte, len(prefix)+8)
	copy(result, prefix)
	binary.BigEndian.PutUint64(result[len(prefix):], ordinal)
	return result
}

type countingWriter struct {
	writer io.Writer
	count  int64
}

func (writer *countingWriter) Write(value []byte) (int, error) {
	written, err := writer.writer.Write(value)
	writer.count += int64(written)
	return written, err
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (reader *countingReader) Read(value []byte) (int, error) {
	read, err := reader.reader.Read(value)
	reader.count += int64(read)
	return read, err
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
