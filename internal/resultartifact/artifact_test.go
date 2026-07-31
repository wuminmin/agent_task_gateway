package resultartifact

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestParquetRoundTripPreservesNullUnicodeAndExactNumbers(t *testing.T) {
	columns, rows, expected := parquetValueFixture()
	var encoded bytes.Buffer
	schema, err := WriteParquet(&encoded, "res_parquet_round_trip", columns, rows)
	if err != nil {
		t.Fatalf("WriteParquet: %v", err)
	}
	value := encoded.Bytes()
	if !bytes.HasPrefix(value, []byte("PAR1")) || !bytes.HasSuffix(value, []byte("PAR1")) {
		t.Fatalf("encoded value is not a Parquet file: prefix=%q suffix=%q", value[:4], value[len(value)-4:])
	}
	if len(schema) != len(columns) {
		t.Fatalf("schema width=%d, want %d", len(schema), len(columns))
	}
	for index := range schema {
		if schema[index].Name != columns[index].Name || schema[index].DataTypeOID != columns[index].DataTypeOID ||
			schema[index].PhysicalName == "" || schema[index].ParquetType == "" {
			t.Fatalf("schema[%d]=%+v does not preserve column metadata %+v", index, schema[index], columns[index])
		}
	}

	decoded, err := ReadParquet(value, "res_parquet_round_trip", schema, 0, int64(len(rows)))
	if err != nil {
		t.Fatalf("ReadParquet: %v", err)
	}
	assertArtifactRowsEqual(t, decoded, expected)
	if _, err := ReadParquet(value, "res_other", schema, 0, 1); err == nil ||
		!strings.Contains(err.Error(), "identity metadata") {
		t.Fatalf("ReadParquet accepted the wrong result identity: %v", err)
	}

	page, err := ReadParquet(value, "res_parquet_round_trip", schema, 1, 1)
	if err != nil {
		t.Fatalf("ReadParquet offset page: %v", err)
	}
	assertArtifactRowsEqual(t, page, expected[1:2])

	beyond, err := ReadParquet(value, "res_parquet_round_trip", schema, int64(len(rows)), 1)
	if err != nil {
		t.Fatalf("ReadParquet beyond end: %v", err)
	}
	if len(beyond) != 0 {
		t.Fatalf("ReadParquet beyond end returned %d rows", len(beyond))
	}
}

func TestParquetRoundTripSupportedPGXCatalogTypes(t *testing.T) {
	columns := []Column{
		{Name: "numeric", DataTypeOID: 1700},
		{Name: "date", DataTypeOID: 1082},
		{Name: "time", DataTypeOID: 1083},
		{Name: "timestamp", DataTypeOID: 1114},
		{Name: "timestamptz", DataTypeOID: 1184},
		{Name: "timetz", DataTypeOID: 1266},
		{Name: "uuid", DataTypeOID: 2950},
		{Name: "text", DataTypeOID: 25},
		{Name: "varchar", DataTypeOID: 1043},
		{Name: "bpchar", DataTypeOID: 1042},
		{Name: "qchar", DataTypeOID: 18},
	}
	numericCoefficient, ok := new(big.Int).SetString("123456789012345678901234567890", 10)
	if !ok {
		t.Fatal("invalid numeric test coefficient")
	}
	uuid := [16]byte{0x55, 0x0e, 0x84, 0x00, 0xe2, 0x9b, 0x41, 0xd4, 0xa7, 0x16, 0x44, 0x66, 0x55, 0x44, 0x00, 0x00}
	timestamp := time.Date(2026, 7, 31, 23, 59, 58, 123456000, time.FixedZone("CST", 8*60*60))
	timestamptz := time.Date(2026, 7, 31, 8, 9, 10, 654321000, time.FixedZone("CST", 8*60*60))
	rows := [][]any{
		{
			pgtype.Numeric{Int: numericCoefficient, Exp: -10, Valid: true},
			time.Date(2026, 7, 31, 12, 34, 56, 0, time.UTC),
			pgtype.Time{Microseconds: int64(24 * time.Hour / time.Microsecond), Valid: true},
			timestamp,
			timestamptz,
			"04:05:06.789123-08:30:15",
			uuid,
			"plain text",
			"variable text",
			"fixed text ",
			rune('Z'),
		},
		{
			pgtype.Numeric{Int: big.NewInt(1234500), Exp: -4, Valid: true},
			pgtype.Date{InfinityModifier: pgtype.Infinity, Valid: true},
			pgtype.Time{Microseconds: int64(12*time.Hour/time.Microsecond) + 345678, Valid: true},
			pgtype.Timestamp{InfinityModifier: pgtype.Infinity, Valid: true},
			pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true},
			[]byte("24:00:00+00"),
			pgtype.UUID{Bytes: uuid, Valid: true},
			[]byte("bytes as text"),
			"",
			"x",
			"Q",
		},
	}
	expected := [][]any{
		{
			json.Number("12345678901234567890.1234567890"),
			"2026-07-31",
			"24:00:00",
			"2026-07-31T23:59:58.123456",
			"2026-07-31T00:09:10.654321Z",
			"04:05:06.789123-08:30:15",
			"550e8400-e29b-41d4-a716-446655440000",
			"plain text",
			"variable text",
			"fixed text ",
			"Z",
		},
		{
			json.Number("123.4500"),
			"infinity",
			"12:00:00.345678",
			"infinity",
			"-infinity",
			"24:00:00+00:00",
			"550e8400-e29b-41d4-a716-446655440000",
			"bytes as text",
			"",
			"x",
			"Q",
		},
	}

	var encoded bytes.Buffer
	schema, err := WriteParquet(&encoded, "res_pgx_catalog_types", columns, rows)
	if err != nil {
		t.Fatalf("WriteParquet: %v", err)
	}
	wantTypes := []string{
		"DECIMAL_STRING", "DATE_STRING", "TIME_MICROS", "TIMESTAMP_STRING", "TIMESTAMPTZ_STRING",
		"TIMETZ_STRING", "UUID", "UTF8", "UTF8", "UTF8", "UTF8",
	}
	for index := range schema {
		if schema[index].ParquetType != wantTypes[index] {
			t.Fatalf("schema[%d].ParquetType=%q, want %q", index, schema[index].ParquetType, wantTypes[index])
		}
	}
	decoded, err := ReadParquet(encoded.Bytes(), "res_pgx_catalog_types", schema, 0, int64(len(rows)))
	if err != nil {
		t.Fatalf("ReadParquet: %v", err)
	}
	assertArtifactRowsEqual(t, decoded, expected)
}

func TestParquetTimeTZRejectsAmbiguousOrOutOfRangeValues(t *testing.T) {
	boundary, err := timetzValue("23:59:59.999999+15:59:59")
	if err != nil || string(boundary.ByteArray()) != "23:59:59.999999+15:59:59" {
		t.Fatalf("maximum valid timetz = %q, %v", boundary.ByteArray(), err)
	}
	for _, value := range []string{
		"04:05:06",
		"25:00:00+00",
		"24:00:00.000001+00",
		"04:05:06.1234567+00",
		"04:05:06+16:00",
		"04:05:06+08::15",
		"04:05:06+0815",
		"04:05:06+08:3015",
		"04:05:06 UTC",
	} {
		t.Run(value, func(t *testing.T) {
			var encoded bytes.Buffer
			_, err := WriteParquet(&encoded, "res_invalid_timetz", []Column{{Name: "t", DataTypeOID: 1266}}, [][]any{{value}})
			if err == nil {
				t.Fatalf("WriteParquet accepted invalid timetz %q", value)
			}
		})
	}
}

func TestParquetNumericAndFloatingSpecialValuesAreJSONSafe(t *testing.T) {
	columns := []Column{
		{Name: "numeric", DataTypeOID: 1700},
		{Name: "real", DataTypeOID: 700},
		{Name: "double", DataTypeOID: 701},
	}
	rows := [][]any{
		{pgtype.Numeric{NaN: true, Valid: true}, float32(math.NaN()), math.Inf(1)},
		{pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}, float32(math.Inf(-1)), math.NaN()},
		{pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, float32(1.25), math.Inf(-1)},
		{pgtype.Numeric{Int: big.NewInt(1234500), Exp: -4, Valid: true}, float32(-2.5), float64(3.75)},
		{"NaN", float32(math.Inf(1)), math.Inf(1)},
	}
	expected := [][]any{
		{"nan", "nan", "+infinity"},
		{"+infinity", "-infinity", "nan"},
		{"-infinity", float32(1.25), "-infinity"},
		{json.Number("123.4500"), float32(-2.5), float64(3.75)},
		{"nan", "+infinity", "+infinity"},
	}
	var encoded bytes.Buffer
	schema, err := WriteParquet(&encoded, "res_special_numbers", columns, rows)
	if err != nil {
		t.Fatalf("WriteParquet: %v", err)
	}
	decoded, err := ReadParquet(encoded.Bytes(), "res_special_numbers", schema, 0, int64(len(rows)))
	if err != nil {
		t.Fatalf("ReadParquet: %v", err)
	}
	assertArtifactRowsEqual(t, decoded, expected)
	if _, err := json.Marshal(decoded); err != nil {
		t.Fatalf("decoded special values are not JSON-safe: %v", err)
	}
}

func TestParquetExactNumericRejectsInvalidAndInexactValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "invalid pgx", value: pgtype.Numeric{}},
		{name: "nil coefficient", value: pgtype.Numeric{Valid: true}},
		{name: "invalid modifier", value: pgtype.Numeric{Int: big.NewInt(1), InfinityModifier: 2, Valid: true}},
		{name: "conflicting NaN", value: pgtype.Numeric{Int: big.NewInt(1), NaN: true, Valid: true}},
		{name: "conflicting infinity", value: pgtype.Numeric{Int: big.NewInt(1), InfinityModifier: pgtype.Infinity, Valid: true}},
		{name: "float32", value: float32(1.25)},
		{name: "float64", value: float64(1.25)},
		{name: "malformed text", value: "not-a-number"},
		{name: "non-JSON decimal", value: "+1.25"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var encoded bytes.Buffer
			_, err := WriteParquet(&encoded, "res_invalid_numeric", []Column{{Name: "n", DataTypeOID: 1700}}, [][]any{{test.value}})
			if err == nil {
				t.Fatalf("WriteParquet accepted %T(%v)", test.value, test.value)
			}
		})
	}
}

func TestParquetRejectsUnknownOIDBeforeWriting(t *testing.T) {
	var encoded bytes.Buffer
	_, err := WriteParquet(&encoded, "res_unknown_oid", []Column{{Name: "extension_value", DataTypeOID: 999999}}, [][]any{{"value"}})
	if err == nil || !strings.Contains(err.Error(), "unsupported PostgreSQL type OID 999999") {
		t.Fatalf("WriteParquet err=%v, want unsupported OID failure", err)
	}
	if encoded.Len() != 0 {
		t.Fatalf("WriteParquet wrote %d bytes before rejecting the schema", encoded.Len())
	}
}

func TestParquetPhysicalNamesRemainUniqueAcrossGeneratedSuffixes(t *testing.T) {
	columns := []Column{
		{Name: "value", DataTypeOID: 25},
		{Name: "value", DataTypeOID: 25},
		{Name: "value__2", DataTypeOID: 25},
		{Name: "", DataTypeOID: 25},
		{Name: "column_4", DataTypeOID: 25},
	}
	var encoded bytes.Buffer
	schema, err := WriteParquet(&encoded, "res_unique_names", columns, [][]any{{"a", "b", "c", "d", "e"}})
	if err != nil {
		t.Fatalf("WriteParquet: %v", err)
	}
	seen := make(map[string]struct{}, len(schema))
	for index, column := range schema {
		if _, exists := seen[column.PhysicalName]; exists {
			t.Fatalf("schema[%d] repeats physical name %q: %+v", index, column.PhysicalName, schema)
		}
		seen[column.PhysicalName] = struct{}{}
	}
}

func TestEncryptedArtifactRoundTripAndIntegrityFailures(t *testing.T) {
	ctx := context.Background()
	backend := newMemoryBackend()
	testCipher := newArtifactTestCipher(t)
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := NewManager(backend, testCipher, tempDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	columns, rows, expected := parquetValueFixture()
	staged, err := manager.Stage(ctx, StageRequest{
		ResultID: "res_encrypted_round_trip", TaskID: "task_integrity",
		StagingKey: "staging/task_integrity/res_encrypted_round_trip.enc",
		ObjectKey:  "results/task_integrity/res_encrypted_round_trip.parquet.enc",
		Columns:    columns, Rows: rows,
	})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if staged.RowCount != int64(len(rows)) || staged.ColumnCount != len(columns) ||
		staged.Format != FormatParquet || staged.Encryption != EncryptionChunkedAESGCMV1 ||
		staged.KeyID != testCipher.KeyID() {
		t.Fatalf("unexpected staged metadata: %+v", staged)
	}
	if entries, readErr := os.ReadDir(tempDir); readErr != nil || len(entries) != 0 {
		t.Fatalf("Stage retained local temporary files: entries=%v err=%v", entries, readErr)
	}

	stagingObject := backend.mustObject(t, staged.StagingKey)
	if bytes.HasPrefix(stagingObject.body, []byte("PAR1")) || bytes.HasSuffix(stagingObject.body, []byte("PAR1")) {
		t.Fatal("encrypted object is directly recognizable as plaintext Parquet")
	}
	for _, plaintext := range [][]byte{
		[]byte("PAR1"), []byte("上海🚀"), []byte("123456789012345678901234567890.12345678901234567890"),
	} {
		if bytes.Contains(stagingObject.body, plaintext) {
			t.Fatalf("encrypted object contains plaintext marker %q", plaintext)
		}
	}
	if got := sha256Hex(stagingObject.body); got != staged.ObjectSHA256 {
		t.Fatalf("object sha256=%s, want %s", got, staged.ObjectSHA256)
	}
	if metadataValue(stagingObject.info.Metadata, "taskgate-parquet-sha256") != staged.ParquetSHA256 ||
		metadataValue(stagingObject.info.Metadata, "taskgate-object-sha256") != staged.ObjectSHA256 {
		t.Fatalf("staging object metadata does not bind both hashes: %#v", stagingObject.info.Metadata)
	}

	promoted, err := manager.Promote(ctx, staged)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted.Key != staged.ObjectKey || !objectMatches(promoted, staged) {
		t.Fatalf("promoted object evidence mismatch: %+v", promoted)
	}
	if backend.has(staged.StagingKey) {
		t.Fatal("Promote retained the private staging object")
	}
	canonical := backend.mustObject(t, staged.ObjectKey)
	reference := artifactReference(staged)
	t.Run("existing canonical is reauthenticated", func(t *testing.T) {
		tampered := append([]byte(nil), canonical.body...)
		tampered[len(tampered)/2] ^= 0x40
		backend.replaceBody(staged.ObjectKey, tampered)
		defer backend.replaceBody(staged.ObjectKey, canonical.body)
		if _, promoteErr := manager.Promote(ctx, staged); promoteErr == nil ||
			!errors.Is(promoteErr, ErrArtifactIntegrity) {
			t.Fatalf("Promote accepted same-size canonical tamper: %v", promoteErr)
		}
	})

	parquetBytes, err := manager.ReadParquet(ctx, reference, staged.ParquetSize)
	if err != nil {
		t.Fatalf("ReadParquet artifact: %v", err)
	}
	if !bytes.HasPrefix(parquetBytes, []byte("PAR1")) || !bytes.HasSuffix(parquetBytes, []byte("PAR1")) {
		t.Fatal("decrypted artifact is not valid Parquet framing")
	}
	if got := sha256Hex(parquetBytes); got != staged.ParquetSHA256 {
		t.Fatalf("Parquet sha256=%s, want %s", got, staged.ParquetSHA256)
	}
	decoded, err := ReadParquet(parquetBytes, staged.ResultID, staged.Schema, 0, int64(len(rows)))
	if err != nil {
		t.Fatalf("decode decrypted Parquet: %v", err)
	}
	assertArtifactRowsEqual(t, decoded, expected)

	if _, err := manager.ReadParquet(ctx, reference, staged.ParquetSize-1); err == nil ||
		!strings.Contains(err.Error(), "preview read limit") {
		t.Fatalf("ReadParquet below declared size err=%v, want preview limit failure", err)
	}

	t.Run("object hash mismatch", func(t *testing.T) {
		wrong := reference
		wrong.ObjectSHA256 = differentDigest(reference.ObjectSHA256)
		assertArtifactReadFails(t, manager, wrong, staged.ParquetSize, "digest or size mismatch")
	})

	t.Run("Parquet hash mismatch", func(t *testing.T) {
		wrong := reference
		wrong.ParquetSHA256 = differentDigest(reference.ParquetSHA256)
		assertArtifactReadFails(t, manager, wrong, staged.ParquetSize, "digest or size mismatch")
	})

	t.Run("AAD task binding", func(t *testing.T) {
		wrong := reference
		wrong.TaskID = "task_other"
		assertArtifactReadFails(t, manager, wrong, staged.ParquetSize, "authentication failed")
	})

	t.Run("ciphertext tamper", func(t *testing.T) {
		tampered := append([]byte(nil), canonical.body...)
		tampered[len(tampered)-1] ^= 0x80
		backend.replaceBody(staged.ObjectKey, tampered)
		t.Cleanup(func() { backend.replaceBody(staged.ObjectKey, canonical.body) })
		assertArtifactReadFails(t, manager, reference, staged.ParquetSize, "authentication failed")
	})
}

func TestLocalEncryptedStagingCleanupIsPrefixAndAgeScoped(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.Chmod(tempDir, 0o700); err != nil {
		t.Fatalf("restrict temp directory: %v", err)
	}
	manager, err := NewManager(newMemoryBackend(), newArtifactTestCipher(t), tempDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	files := map[string]time.Time{
		localStagingPrefix + "old" + localStagingSuffix:    old,
		localStagingPrefix + "recent" + localStagingSuffix: now,
		"unrelated.enc": old,
	}
	for name, modified := range files {
		path := filepath.Join(tempDir, name)
		if err := os.WriteFile(path, []byte("encrypted scratch"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatalf("age %s: %v", name, err)
		}
	}
	removed, err := manager.PurgeLocalStagingBefore(now.Add(-time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("PurgeLocalStagingBefore = %d, %v; want 1", removed, err)
	}
	if _, err := os.Stat(filepath.Join(tempDir, localStagingPrefix+"old"+localStagingSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old TaskGate scratch remains: %v", err)
	}
	for _, name := range []string{localStagingPrefix + "recent" + localStagingSuffix, "unrelated.enc"} {
		if _, err := os.Stat(filepath.Join(tempDir, name)); err != nil {
			t.Fatalf("cleanup removed %s: %v", name, err)
		}
	}
}

func parquetValueFixture() ([]Column, [][]any, [][]any) {
	columns := []Column{
		{Name: "nullable_text", DataTypeOID: 25},
		{Name: "unicode_列🚀", DataTypeOID: 25},
		{Name: "exact_numeric", DataTypeOID: 1700},
		{Name: "large_integer", DataTypeOID: 20},
		{Name: "integer", DataTypeOID: 23},
		{Name: "approved", DataTypeOID: 16},
		{Name: "json_payload", DataTypeOID: 3802},
		{Name: "binary_payload", DataTypeOID: 17},
	}
	jsonFirst := json.RawMessage(`{"amount":9007199254740993,"label":"上海🚀","missing":null}`)
	jsonSecond := map[string]any{"nested": []any{json.Number("1.2300"), nil, "é"}}
	rows := [][]any{
		{nil, "上海🚀 café e\u0301", json.Number("123456789012345678901234567890.12345678901234567890"),
			int64(math.MaxInt64), int64(math.MinInt32), true, jsonFirst, []byte{0, 1, 2, 0xff}},
		{"", "مرحبا・こんにちは・🙂", json.Number("-0.0000000000000000000000000000000000001e+120"),
			int64(math.MinInt64), int64(math.MaxInt32), false, jsonSecond, []byte("binary-PAR1-value")},
		{nil, nil, nil, nil, nil, nil, nil, nil},
	}
	expected := [][]any{
		{nil, "上海🚀 café e\u0301", json.Number("123456789012345678901234567890.12345678901234567890"),
			int64(math.MaxInt64), int64(math.MinInt32), true,
			map[string]any{"amount": json.Number("9007199254740993"), "label": "上海🚀", "missing": nil},
			[]byte{0, 1, 2, 0xff}},
		{"", "مرحبا・こんにちは・🙂", json.Number("-0.0000000000000000000000000000000000001e+120"),
			int64(math.MinInt64), int64(math.MaxInt32), false, jsonSecond, []byte("binary-PAR1-value")},
		{nil, nil, nil, nil, nil, nil, nil, nil},
	}
	return columns, rows, expected
}

func artifactReference(staged StagedArtifact) ArtifactRef {
	return ArtifactRef{
		ResultID: staged.ResultID, TaskID: staged.TaskID, ObjectKey: staged.ObjectKey, KeyID: staged.KeyID,
		ParquetSHA256: staged.ParquetSHA256, ObjectSHA256: staged.ObjectSHA256,
		ParquetSize: staged.ParquetSize, ObjectSize: staged.ObjectSize,
	}
}

func assertArtifactRowsEqual(t *testing.T, actual, expected [][]any) {
	t.Helper()
	if !reflect.DeepEqual(actual, expected) {
		actualJSON, _ := json.Marshal(actual)
		expectedJSON, _ := json.Marshal(expected)
		t.Fatalf("rows mismatch\nactual:   %s\nexpected: %s", actualJSON, expectedJSON)
	}
}

func assertArtifactReadFails(t *testing.T, manager *Manager, reference ArtifactRef, maximum int64, message string) {
	t.Helper()
	_, err := manager.ReadParquet(context.Background(), reference, maximum)
	if err == nil || !strings.Contains(err.Error(), message) {
		t.Fatalf("ReadParquet err=%v, want error containing %q", err, message)
	}
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func differentDigest(value string) string {
	if value[0] == '0' {
		return "1" + value[1:]
	}
	return "0" + value[1:]
}

type artifactTestCipher struct {
	aead    cipher.AEAD
	ordinal uint64
}

func newArtifactTestCipher(t *testing.T) *artifactTestCipher {
	t.Helper()
	block, err := aes.NewCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	return &artifactTestCipher{aead: aead}
}

func (cipher *artifactTestCipher) KeyID() string { return "test-aes256-gcm-v1" }

func (cipher *artifactTestCipher) Encrypt(plaintext, aad []byte) ([]byte, []byte, error) {
	nonce := make([]byte, cipher.aead.NonceSize())
	binary.BigEndian.PutUint64(nonce[len(nonce)-8:], cipher.ordinal)
	cipher.ordinal++
	return nonce, cipher.aead.Seal(nil, nonce, plaintext, aad), nil
}

func (cipher *artifactTestCipher) Decrypt(nonce, ciphertext, aad []byte) ([]byte, error) {
	return cipher.aead.Open(nil, nonce, ciphertext, aad)
}

type memoryObject struct {
	body []byte
	info ObjectInfo
}

type memoryBackend struct {
	objects map[string]memoryObject
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{objects: make(map[string]memoryObject)}
}

func (backend *memoryBackend) Put(_ context.Context, key string, body io.Reader, size int64, options PutOptions) (ObjectInfo, error) {
	value, err := io.ReadAll(body)
	if err != nil {
		return ObjectInfo{}, err
	}
	if int64(len(value)) != size {
		return ObjectInfo{}, io.ErrUnexpectedEOF
	}
	info := ObjectInfo{Key: key, Size: size, ETag: sha256Hex(value)[:16], Metadata: cloneMetadata(options.Metadata), LastModified: time.Now().UTC()}
	backend.objects[key] = memoryObject{body: append([]byte(nil), value...), info: info}
	return info, nil
}

func (backend *memoryBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	object, exists := backend.objects[key]
	if !exists {
		return nil, ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), object.body...))), nil
}

func (backend *memoryBackend) Stat(_ context.Context, key string) (ObjectInfo, error) {
	object, exists := backend.objects[key]
	if !exists {
		return ObjectInfo{}, ErrObjectNotFound
	}
	info := object.info
	info.Metadata = cloneMetadata(info.Metadata)
	return info, nil
}

func (backend *memoryBackend) Copy(_ context.Context, source, destination, expectedSHA256 string) (ObjectInfo, error) {
	object, exists := backend.objects[source]
	if !exists {
		return ObjectInfo{}, ErrObjectNotFound
	}
	if _, exists := backend.objects[destination]; exists {
		return ObjectInfo{}, ErrObjectAlreadyExists
	}
	if sha256Hex(object.body) != expectedSHA256 {
		return ObjectInfo{}, errors.New("staging result object digest differs from committed evidence")
	}
	object.body = append([]byte(nil), object.body...)
	object.info.Key = destination
	object.info.Metadata = cloneMetadata(object.info.Metadata)
	backend.objects[destination] = object
	return object.info, nil
}

func (backend *memoryBackend) List(_ context.Context, prefix, startAfter string, limit int) ([]ObjectInfo, error) {
	keys := make([]string, 0, len(backend.objects))
	for key := range backend.objects {
		if strings.HasPrefix(key, prefix) && key > startAfter {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if limit > 0 && len(keys) > limit {
		keys = keys[:limit]
	}
	result := make([]ObjectInfo, len(keys))
	for index, key := range keys {
		result[index] = backend.objects[key].info
	}
	return result, nil
}

func (backend *memoryBackend) Delete(_ context.Context, key string) error {
	delete(backend.objects, key)
	return nil
}

func (backend *memoryBackend) Ready(context.Context) error { return nil }

func (backend *memoryBackend) has(key string) bool {
	_, exists := backend.objects[key]
	return exists
}

func (backend *memoryBackend) mustObject(t *testing.T, key string) memoryObject {
	t.Helper()
	object, exists := backend.objects[key]
	if !exists {
		t.Fatalf("object %q does not exist", key)
	}
	object.body = append([]byte(nil), object.body...)
	object.info.Metadata = cloneMetadata(object.info.Metadata)
	return object
}

func (backend *memoryBackend) replaceBody(key string, body []byte) {
	object := backend.objects[key]
	object.body = append([]byte(nil), body...)
	object.info.Size = int64(len(body))
	backend.objects[key] = object
}
