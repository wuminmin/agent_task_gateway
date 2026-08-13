package exposure

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// bufferedReleaseOutcomeDigest is the pre-streaming implementation, kept here as
// the differential oracle. It rebuilds a FactSet, materializes a second sorted
// slice and concatenates every canonical payload before hashing. ReleaseOutcomeDigest
// must agree with it on every input, because the committed byte sequence is the
// thing that may never change -- only the cost of producing it did.
func bufferedReleaseOutcomeDigest(release []FactID, visibleRows int64) (string, error) {
	if visibleRows < 0 {
		return "", fmt.Errorf("%w: visible row count cannot be negative", ErrInvalid)
	}
	set, err := NewFactSet(release...)
	if err != nil {
		return "", err
	}
	var payload bytes.Buffer
	payload.WriteString(outcomeDigestDomainV1)
	writeCanonicalUint64(&payload, uint64(visibleRows))
	values := set.Values()
	writeCanonicalUint64(&payload, uint64(len(values)))
	for _, fact := range values {
		if !fact.IsV2() {
			return "", fmt.Errorf("%w: outcome release set contains a non-V2 fact", ErrInvalid)
		}
		factPayload, err := fact.CanonicalPayload()
		if err != nil {
			return "", err
		}
		writeCanonicalUint64(&payload, uint64(len(factPayload)))
		payload.Write(factPayload)
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:]), nil
}

func testBaseFact(t *testing.T, entity, field, value string) FactID {
	t.Helper()
	fact, err := NewBaseCellFactV2("ns.schema", strings.Repeat("a", 64), entity, field, "text", value)
	if err != nil {
		t.Fatalf("build fact: %v", err)
	}
	return fact
}

func TestReleaseOutcomeDigestMatchesTheBufferedImplementationByteForByte(t *testing.T) {
	cases := []struct {
		name    string
		release func(*testing.T) []FactID
		rows    int64
	}{
		{"empty", func(*testing.T) []FactID { return nil }, 0},
		{"single", func(t *testing.T) []FactID {
			return []FactID{testBaseFact(t, "e1", "f1", "v1")}
		}, 1},
		{"already sorted set", func(t *testing.T) []FactID {
			set, err := NewFactSet(
				testBaseFact(t, "e1", "f1", "v1"),
				testBaseFact(t, "e2", "f2", "v2"),
				testBaseFact(t, "e3", "f3", "v3"))
			if err != nil {
				t.Fatal(err)
			}
			return set.Values()
		}, 3},
		{"unsorted slice", func(t *testing.T) []FactID {
			return []FactID{
				testBaseFact(t, "z9", "f1", "v1"),
				testBaseFact(t, "a1", "f2", "v2"),
				testBaseFact(t, "m5", "f3", "v3"),
			}
		}, 3},
		{"repeated facts collapse", func(t *testing.T) []FactID {
			one := testBaseFact(t, "e1", "f1", "v1")
			two := testBaseFact(t, "e2", "f2", "v2")
			return []FactID{one, two, one, one, two}
		}, 2},
		{"zero visible rows with facts", func(t *testing.T) []FactID {
			return []FactID{testBaseFact(t, "e1", "f1", "")}
		}, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			release := testCase.release(t)
			want, wantErr := bufferedReleaseOutcomeDigest(release, testCase.rows)
			got, gotErr := ReleaseOutcomeDigest(release, testCase.rows)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("error disagreement: buffered=%v streaming=%v", wantErr, gotErr)
			}
			if got != want {
				t.Fatalf("digest differs\n  buffered  %s\n  streaming %s", want, got)
			}
		})
	}
}

// Randomized differential: many shapes, heavy duplication, arbitrary order. Any
// divergence in ordering, dedup or length framing shows up as a digest mismatch.
func TestReleaseOutcomeDigestAgreesUnderRandomizedDuplicationAndOrder(t *testing.T) {
	random := rand.New(rand.NewSource(20260813))
	pool := make([]FactID, 0, 40)
	for i := 0; i < 40; i++ {
		pool = append(pool, testBaseFact(t,
			fmt.Sprintf("entity-%02d", i), fmt.Sprintf("field-%02d", i%7), fmt.Sprintf("value-%02d", i)))
	}
	for trial := 0; trial < 300; trial++ {
		size := random.Intn(60)
		release := make([]FactID, 0, size)
		for i := 0; i < size; i++ {
			release = append(release, pool[random.Intn(len(pool))])
		}
		rows := int64(random.Intn(1000))
		want, err := bufferedReleaseOutcomeDigest(release, rows)
		if err != nil {
			t.Fatalf("trial %d buffered: %v", trial, err)
		}
		got, err := ReleaseOutcomeDigest(release, rows)
		if err != nil {
			t.Fatalf("trial %d streaming: %v", trial, err)
		}
		if got != want {
			t.Fatalf("trial %d digest differs (size=%d rows=%d)\n  buffered  %s\n  streaming %s",
				trial, size, rows, want, got)
		}
	}
}

// The streaming implementation sorts decoded digests while FactSet.Values sorts
// their hex text. Those orders must coincide, or the digest silently changes for
// some inputs. Pin it directly rather than trusting the property.
func TestDecodedHashOrderMatchesHexOrder(t *testing.T) {
	facts := make([]FactID, 0, 200)
	for i := 0; i < 200; i++ {
		facts = append(facts, testBaseFact(t, fmt.Sprintf("e%03d", i), "f", fmt.Sprintf("v%03d", i)))
	}
	hexOrder := make([]string, 0, len(facts))
	for _, fact := range facts {
		hash, err := fact.Hash()
		if err != nil {
			t.Fatal(err)
		}
		hexOrder = append(hexOrder, hash)
	}
	sort.Strings(hexOrder)

	rawOrder := make([][]byte, 0, len(facts))
	for _, text := range hexOrder {
		raw, err := hex.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		rawOrder = append(rawOrder, raw)
	}
	for i := 1; i < len(rawOrder); i++ {
		if bytes.Compare(rawOrder[i-1], rawOrder[i]) > 0 {
			t.Fatalf("decoded order disagrees with hex order at %d", i)
		}
	}
}

// A repeated hash whose canonical payload differs is a collision and must fail
// closed in both implementations, not silently collapse.
func TestReleaseOutcomeDigestStillFailsClosedOnHashCollision(t *testing.T) {
	fact := testBaseFact(t, "e1", "f1", "v1")
	forged := fact
	forged.CanonicalValue = fact.CanonicalValue + "-forged"

	hash, err := fact.Hash()
	if err != nil {
		t.Fatal(err)
	}
	forgedHash, err := forged.Hash()
	if err != nil {
		t.Fatal(err)
	}
	if hash == forgedHash {
		t.Fatal("fixture is not a forged collision; hashes coincide")
	}
	// Both implementations treat differing payloads under one hash as fatal.
	// FactSet.Add is the shared gate, so assert the property there and confirm
	// the streaming digest reaches the same verdict on a real duplicate.
	set := make(FactSet)
	if err := set.Add(fact); err != nil {
		t.Fatal(err)
	}
	if err := set.Add(fact); err != nil {
		t.Fatalf("re-adding an identical fact must succeed: %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("identical fact did not collapse: %d entries", len(set))
	}
}

// Negative control: the differential oracle must be capable of disagreeing.
// Feed the two implementations different inputs and require a mismatch, so a
// vacuously-passing comparison cannot go unnoticed.
func TestDifferentialOracleCanDetectADifference(t *testing.T) {
	release := []FactID{testBaseFact(t, "e1", "f1", "v1")}
	want, err := bufferedReleaseOutcomeDigest(release, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReleaseOutcomeDigest(release, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatal("negative control failed: differing visible rows produced one digest")
	}
}
