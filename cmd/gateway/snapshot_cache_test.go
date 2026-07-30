package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheDroppingReaderPreservesExactStream(t *testing.T) {
	payload := bytes.Repeat([]byte("taskgate-cache-hint\x00"), 257)
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	reader := newCacheDroppingReader(file)
	actual, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	reader.dropConsumed()
	if !bytes.Equal(actual, payload) {
		t.Fatal("cache hint wrapper changed the artifact stream")
	}
	if reader.offset != int64(len(payload)) || reader.dropOffset != reader.offset {
		t.Fatalf("reader offsets = consumed %d, dropped %d, want %d", reader.offset, reader.dropOffset, len(payload))
	}
}
