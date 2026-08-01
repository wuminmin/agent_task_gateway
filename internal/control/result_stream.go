package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const resultStreamChunkSize = 1 << 20

const chunkedResultManifestVersion = "taskgate-chunked-result-v1"

type chunkedResultManifest struct {
	Version       string `json:"version"`
	PlaintextSize int64  `json:"plaintext_size"`
	ChunkSize     int    `json:"chunk_size"`
	ChunkCount    int64  `json:"chunk_count"`
	SHA256        string `json:"sha256"`
}

// FinalizeOrdinalQueryStreamMeasuredWithReceipt is the bounded-memory V4
// finalize path. plaintext is consumed once, in fixed-size pieces. Every
// piece is independently authenticated and persisted inside the same atomic
// result/ledger/audit/receipt transaction used by the in-memory path.
func (s *Store) FinalizeOrdinalQueryStreamMeasuredWithReceipt(ctx context.Context, settlement BudgetSettlement,
	plaintext io.Reader, publish *OrdinalMaterializationPublish,
	builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, FinalizeQueryMetrics, error) {
	if (settlement.OrdinalExposure == nil) == (settlement.OrdinalObservationRef == nil) || settlement.Exposure != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, FinalizeQueryMetrics{}, opErr("finalize ordinal query stream", ErrInvalid,
			fmt.Errorf("exactly one ordinal observation or committed observation reference is required"))
	}
	settlement.OrdinalMaterialization = publish
	return s.FinalizeQueryStreamMeasuredWithReceipt(ctx, settlement, plaintext, builder)
}

// FinalizeQueryStreamMeasuredWithReceipt is equivalent to
// FinalizeQueryMeasuredWithReceipt but never materializes the complete
// plaintext or ciphertext in Go memory.
func (s *Store) FinalizeQueryStreamMeasuredWithReceipt(ctx context.Context, settlement BudgetSettlement,
	plaintext io.Reader, builder TerminalReceiptBuilder) (QueryRecord, PersistedQueryReceipt, FinalizeQueryMetrics, error) {
	const op = "finalize query stream"
	var metrics FinalizeQueryMetrics
	if err := s.checkOpen(op); err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, err
	}
	if s.cipher == nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrCipherUnavailable, nil)
	}
	if plaintext == nil || settlement.QueryID == "" || settlement.Rows < 0 || settlement.DBMS < 0 || settlement.ObservedDBMS < 0 {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrInvalid, fmt.Errorf("invalid settlement or plaintext stream"))
	}
	if settlement.OrdinalMaterialization != nil && settlement.OrdinalExposure == nil && settlement.OrdinalObservationRef == nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrInvalid,
			fmt.Errorf("ordinal materialization requires V4 exposure evidence"))
	}
	current, err := s.GetQuery(ctx, settlement.QueryID)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, err
	}
	if current.Status == QueryReleased || current.Status == QueryInterrupted {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrReservationNotFound, fmt.Errorf("query is %s", current.Status))
	}
	keyID, err := resultCipherKeyID(s.cipher)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrInvalid, err)
	}

	encryptionStarted := time.Now()
	prepared, cleanup, err := s.prepareChunkedResult(plaintext, current, keyID)
	metrics.Encryption = time.Since(encryptionStarted)
	if err != nil {
		return QueryRecord{}, PersistedQueryReceipt{}, metrics, opErr(op, ErrCiphertextInvalid, err)
	}
	defer cleanup()
	return s.finalizePreparedQuery(ctx, settlement, current, prepared, builder, metrics)
}

func (s *Store) prepareChunkedResult(plaintext io.Reader, current QueryRecord, keyID string) (preparedEncryptedResult, func(), error) {
	file, err := os.CreateTemp("", ".taskgate-result-ciphertext-")
	if err != nil {
		return preparedEncryptedResult{}, func() {}, fmt.Errorf("create encrypted result staging file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return preparedEncryptedResult{}, func() {}, fmt.Errorf("restrict encrypted result staging file: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	buffer := make([]byte, resultStreamChunkSize)
	defer zeroResultBytes(buffer)
	hash := sha256.New()
	var plaintextSize int64
	var chunkCount int64
	for {
		read, readErr := io.ReadFull(plaintext, buffer)
		if read != 0 {
			if plaintextSize > int64(^uint64(0)>>1)-int64(read) {
				cleanup()
				return preparedEncryptedResult{}, func() {}, errors.New("result plaintext size overflow")
			}
			piece := buffer[:read]
			_, _ = hash.Write(piece)
			nonce, ciphertext, encryptErr := s.cipher.Encrypt(piece, resultChunkAAD(current.TaskID, current.ID, chunkCount))
			zeroResultBytes(piece)
			if encryptErr != nil {
				cleanup()
				return preparedEncryptedResult{}, func() {}, fmt.Errorf("encrypt result chunk: %w", encryptErr)
			}
			if len(nonce) == 0 || uint64(len(nonce)) > uint64(^uint32(0)) ||
				len(ciphertext) == 0 || uint64(len(ciphertext)) > uint64(^uint32(0)) {
				zeroResultBytes(ciphertext)
				cleanup()
				return preparedEncryptedResult{}, func() {}, errors.New("encrypted result chunk exceeds framing limits")
			}
			var header [8]byte
			binary.BigEndian.PutUint32(header[0:4], uint32(len(nonce)))
			binary.BigEndian.PutUint32(header[4:8], uint32(len(ciphertext)))
			if err := writeResultFrame(file, header[:], nonce, ciphertext); err != nil {
				zeroResultBytes(ciphertext)
				cleanup()
				return preparedEncryptedResult{}, func() {}, err
			}
			zeroResultBytes(ciphertext)
			plaintextSize += int64(read)
			chunkCount++
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			cleanup()
			return preparedEncryptedResult{}, func() {}, fmt.Errorf("read result plaintext stream: %w", readErr)
		}
	}
	if chunkCount == 0 {
		cleanup()
		return preparedEncryptedResult{}, func() {}, errors.New("result plaintext stream is empty")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	manifest := chunkedResultManifest{
		Version: chunkedResultManifestVersion, PlaintextSize: plaintextSize,
		ChunkSize: resultStreamChunkSize, ChunkCount: chunkCount, SHA256: digest,
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		cleanup()
		return preparedEncryptedResult{}, func() {}, fmt.Errorf("encode chunked result manifest: %w", err)
	}
	nonce, ciphertext, err := s.cipher.Encrypt(manifestJSON, resultAAD(current.TaskID, current.ID))
	zeroResultBytes(manifestJSON)
	if err != nil {
		cleanup()
		return preparedEncryptedResult{}, func() {}, fmt.Errorf("encrypt chunked result manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		zeroResultBytes(ciphertext)
		cleanup()
		return preparedEncryptedResult{}, func() {}, fmt.Errorf("sync encrypted result staging file: %w", err)
	}
	// Keep only the open descriptor while the transaction consumes the final
	// ciphertext. A crash or cancellation closes it without leaving a named
	// staging artifact; no plaintext was ever written to this file.
	if err := os.Remove(file.Name()); err != nil {
		zeroResultBytes(ciphertext)
		cleanup()
		return preparedEncryptedResult{}, func() {}, fmt.Errorf("unlink encrypted result staging file: %w", err)
	}
	prepared := preparedEncryptedResult{metadata: EncryptedResult{
		QueryID: current.ID, TaskID: current.TaskID, KeyID: keyID,
		Nonce: nonce, Ciphertext: ciphertext, SHA256: digest,
		StorageFormat: resultStorageChunked, PlaintextSize: &plaintextSize, ChunkCount: chunkCount,
	}}
	prepared.chunks = func(ctx context.Context, tx *sql.Tx) error {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return opErr("store chunked result", ErrConflict, err)
		}
		for ordinal := int64(0); ordinal < chunkCount; ordinal++ {
			nonce, ciphertext, err := readResultFrame(file)
			if err != nil {
				return opErr("store chunked result", ErrCiphertextInvalid, err)
			}
			_, err = tx.ExecContext(ctx, `
INSERT INTO encrypted_query_result_chunks(query_id,chunk_ordinal,nonce,ciphertext)
VALUES ($1,$2,$3,$4)`, current.ID, ordinal, nonce, ciphertext)
			zeroResultBytes(ciphertext)
			if err != nil {
				return opErr("store chunked result", ErrConflict, err)
			}
		}
		var trailing [1]byte
		if read, err := file.Read(trailing[:]); read != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return opErr("store chunked result", ErrCiphertextInvalid, errors.New("encrypted result staging file has trailing data"))
		}
		return nil
	}
	return prepared, cleanup, nil
}

func resultChunkAAD(taskID, queryID string, ordinal int64) []byte {
	prefix := []byte("taskbound-result-chunk-v1\x00" + taskID + "\x00" + queryID + "\x00")
	result := make([]byte, len(prefix)+8)
	copy(result, prefix)
	binary.BigEndian.PutUint64(result[len(prefix):], uint64(ordinal))
	return result
}

func writeResultFrame(writer io.Writer, values ...[]byte) error {
	for _, value := range values {
		for len(value) != 0 {
			written, err := writer.Write(value)
			if err != nil {
				return fmt.Errorf("write encrypted result staging file: %w", err)
			}
			if written <= 0 {
				return io.ErrShortWrite
			}
			value = value[written:]
		}
	}
	return nil
}

func readResultFrame(reader io.Reader) ([]byte, []byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, nil, err
	}
	nonceLength := uint64(binary.BigEndian.Uint32(header[0:4]))
	ciphertextLength := uint64(binary.BigEndian.Uint32(header[4:8]))
	if nonceLength == 0 || nonceLength > 1<<16 || ciphertextLength == 0 || ciphertextLength > resultStreamChunkSize+(1<<16) {
		return nil, nil, errors.New("encrypted result staging frame length is invalid")
	}
	nonce := make([]byte, int(nonceLength))
	ciphertext := make([]byte, int(ciphertextLength))
	if _, err := io.ReadFull(reader, nonce); err != nil {
		return nil, nil, err
	}
	if _, err := io.ReadFull(reader, ciphertext); err != nil {
		return nil, nil, err
	}
	return nonce, ciphertext, nil
}

func zeroResultBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (s *Store) decryptChunkedResult(ctx context.Context, result EncryptedResult) ([]byte, error) {
	manifestJSON, err := s.cipher.Decrypt(result.Nonce, result.Ciphertext, resultAAD(result.TaskID, result.QueryID))
	if err != nil {
		return nil, opErr("get encrypted result", ErrCiphertextInvalid, err)
	}
	defer zeroResultBytes(manifestJSON)
	var manifest chunkedResultManifest
	decoder := json.NewDecoder(bytes.NewReader(manifestJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, opErr("get encrypted result", ErrCiphertextInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, opErr("get encrypted result", ErrCiphertextInvalid, errors.New("chunked result manifest has trailing data"))
	}
	if manifest.Version != chunkedResultManifestVersion || manifest.PlaintextSize < 0 ||
		manifest.ChunkSize != resultStreamChunkSize || manifest.ChunkCount <= 0 ||
		manifest.ChunkCount != result.ChunkCount || result.PlaintextSize == nil || manifest.PlaintextSize != *result.PlaintextSize ||
		subtle.ConstantTimeCompare([]byte(manifest.SHA256), []byte(result.SHA256)) != 1 {
		return nil, opErr("get encrypted result", ErrCiphertextInvalid, errors.New("chunked result manifest mismatch"))
	}
	if manifest.PlaintextSize > int64(int(^uint(0)>>1)) {
		return nil, opErr("get encrypted result", ErrCiphertextInvalid, errors.New("chunked result is too large for this process"))
	}
	plaintext := make([]byte, 0, int(manifest.PlaintextSize))
	hash := sha256.New()
	rows, err := s.db.QueryContext(ctx, `
SELECT chunk_ordinal,nonce,ciphertext
FROM encrypted_query_result_chunks
WHERE query_id=$1
ORDER BY chunk_ordinal`, result.QueryID)
	if err != nil {
		return nil, opErr("get encrypted result", ErrConflict, err)
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var ordinal int64
		var nonce, ciphertext []byte
		if err := rows.Scan(&ordinal, &nonce, &ciphertext); err != nil {
			zeroResultBytes(plaintext)
			return nil, opErr("get encrypted result", ErrConflict, err)
		}
		if ordinal != count {
			zeroResultBytes(plaintext)
			return nil, opErr("get encrypted result", ErrCiphertextInvalid, errors.New("chunked result ordinals are not contiguous"))
		}
		piece, err := s.cipher.Decrypt(nonce, ciphertext, resultChunkAAD(result.TaskID, result.QueryID, ordinal))
		if err != nil || len(piece) == 0 || len(piece) > resultStreamChunkSize || int64(len(piece)) > manifest.PlaintextSize-int64(len(plaintext)) {
			zeroResultBytes(piece)
			zeroResultBytes(plaintext)
			return nil, opErr("get encrypted result", ErrCiphertextInvalid, errors.New("chunked result authentication or length failed"))
		}
		_, _ = hash.Write(piece)
		plaintext = append(plaintext, piece...)
		zeroResultBytes(piece)
		count++
	}
	if err := rows.Err(); err != nil {
		zeroResultBytes(plaintext)
		return nil, opErr("get encrypted result", ErrConflict, err)
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if count != manifest.ChunkCount || int64(len(plaintext)) != manifest.PlaintextSize ||
		subtle.ConstantTimeCompare([]byte(actualHash), []byte(result.SHA256)) != 1 {
		zeroResultBytes(plaintext)
		return nil, opErr("get encrypted result", ErrCiphertextInvalid, errors.New("chunked result digest or count mismatch"))
	}
	return plaintext, nil
}
