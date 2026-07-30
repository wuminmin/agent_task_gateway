package v4oracle

import (
	"crypto/sha256"
	"errors"
	"io"
	"testing"
)

func TestExternalSorterMergesRunsAndExactMultiplicities(t *testing.T) {
	tracker := &sortResourceTracker{}
	sorter, err := newExternalSorter(t.TempDir(), "facts", 1<<20, tracker)
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 128<<10)
	wantMultiplicity := make(map[[sha256.Size]byte]uint64)
	for index := 19; index >= 0; index-- {
		payload[0] = byte(index % 5)
		hash := sha256.Sum256([]byte{byte(index % 5)})
		wantMultiplicity[hash] += uint64(index + 1)
		if err := sorter.Add(factRecord{Hash: hash, Multiplicity: uint64(index + 1), Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	iterator, err := sorter.Finish()
	if err != nil {
		t.Fatal(err)
	}
	defer iterator.Close()
	var previous [sha256.Size]byte
	for index := 0; index < 5; index++ {
		record, err := iterator.NextCombined()
		if err != nil {
			t.Fatal(err)
		}
		if index > 0 && compareFactRecord(factRecord{Hash: previous}, record) >= 0 {
			t.Fatal("merged hashes are not strictly ordered")
		}
		if record.Multiplicity != wantMultiplicity[record.Hash] {
			t.Fatalf("hash %x multiplicity=%d, want %d", record.Hash, record.Multiplicity, wantMultiplicity[record.Hash])
		}
		delete(wantMultiplicity, record.Hash)
		previous = record.Hash
	}
	if len(wantMultiplicity) != 0 {
		t.Fatalf("merge omitted %d expected hashes", len(wantMultiplicity))
	}
	if _, err := iterator.NextCombined(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of merge = %v, want EOF", err)
	}
	if len(sorter.runs) < 2 || sorter.spool == 0 || tracker.maximumRecords == 0 {
		t.Fatalf("external-sort resources runs=%d spool=%d records=%d", len(sorter.runs), sorter.spool, tracker.maximumRecords)
	}
}

func TestExternalSorterRejectsHashCollisionPayloads(t *testing.T) {
	sorter, err := newExternalSorter(t.TempDir(), "collision", 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("one-hash"))
	for _, payload := range [][]byte{[]byte("left"), []byte("right")} {
		if err := sorter.Add(factRecord{Hash: hash, Multiplicity: 1, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	iterator, err := sorter.Finish()
	if err != nil {
		t.Fatal(err)
	}
	defer iterator.Close()
	if _, err := iterator.NextCombined(); err == nil {
		t.Fatal("distinct payloads under one FactHash were accepted")
	}
}
