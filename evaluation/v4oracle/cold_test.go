package v4oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskbound.local/agent-data-gateway/internal/ordinal"
)

func TestScanColdDictionaryExpandsEveryCanonicalFactAndRejectsTampering(t *testing.T) {
	artifact, err := ordinal.CompileSnapshotArtifact(ordinal.SnapshotSpec{
		SourceID: "business", SourceNamespace: "evaluation.oracle", Snapshot: "snapshot-1",
		SchemaDigest: strings.Repeat("a", 64),
		Fields:       []ordinal.SnapshotField{{Name: "amount", CanonicalFieldID: "oracle.amount", SQLType: "numeric"}},
		Rows: []ordinal.SnapshotRow{{EntityKey: "row-a", Values: map[string]any{"amount": "10.0"}},
			{EntityKey: "row-b", Values: map[string]any{"amount": "20.0"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := artifact.Cold.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	fileDigest := sha256.Sum256(raw)
	path := filepath.Join(t.TempDir(), "dictionary.tgcold")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var yielded uint64
	scan, err := scanColdDictionary(path, artifact.Hot.ManifestDigest(), artifact.Hot.DictionaryDigest(),
		hex.EncodeToString(fileDigest[:]), func(fact coldFact) error {
			hash, err := fact.Fact.Hash()
			if err != nil || hash != hex.EncodeToString(fact.Hash[:]) {
				t.Fatalf("expanded FactHash mismatch: %s/%x (%v)", hash, fact.Hash, err)
			}
			payload, err := fact.Fact.CanonicalPayload()
			if err != nil || !bytes.Equal(payload, fact.Payload) {
				t.Fatalf("expanded canonical payload mismatch: %v", err)
			}
			yielded++
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if scan.Facts != 4 || yielded != 4 || scan.SHA256 != hex.EncodeToString(fileDigest[:]) {
		t.Fatalf("scan facts=%d yielded=%d digest=%s", scan.Facts, yielded, scan.SHA256)
	}

	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-1] ^= 0xff
	tamperedPath := filepath.Join(t.TempDir(), "tampered.tgcold")
	if err := os.WriteFile(tamperedPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	tamperedDigest := sha256.Sum256(tampered)
	if _, err := scanColdDictionary(tamperedPath, artifact.Hot.ManifestDigest(), artifact.Hot.DictionaryDigest(),
		hex.EncodeToString(tamperedDigest[:]), nil); err == nil {
		t.Fatal("tampered COLD transport seal was accepted")
	}
}
