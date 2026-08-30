package exposure

import (
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// The observation must commit exactly the bytes ReleaseOutcomeDigest commits
// over FactSet.Values, whatever the residency threshold, run count, insertion
// order, or duplication pattern.
func TestReleaseObservationDigestMatchesReleaseOutcomeDigest(t *testing.T) {
	for _, threshold := range []int64{1 << 8, 1 << 10, 1 << 14, factSetSpoolThresholdBytes} {
		for seed := int64(1); seed <= 4; seed++ {
			random := rand.New(rand.NewSource(seed*7919 + threshold))
			pool := make([]FactID, 0, 300)
			for index := 0; index < cap(pool); index++ {
				pool = append(pool, testBaseFact(t, fmt.Sprintf("entity-%d", index%97),
					fmt.Sprintf("field-%d", index%5), strings.Repeat("v", 1+random.Intn(40))+fmt.Sprint(index)))
			}
			for index := 0; index < 6; index++ {
				pool = append(pool, testDerivedFact(t, fmt.Sprintf("row-%d", index), int64(index)))
			}
			facts := make([]FactID, 0, 900)
			for len(facts) < cap(facts) {
				facts = append(facts, pool[random.Intn(len(pool))])
			}
			rows := int64(random.Intn(1000))
			expected, err := ReleaseOutcomeDigest(facts, rows)
			if err != nil {
				t.Fatal(err)
			}
			observation := newReleaseObservation(threshold, t.TempDir(), 1<<12)
			for index, fact := range facts {
				if index%2 == 0 {
					if err := observation.Add(fact); err != nil {
						t.Fatal(err)
					}
					continue
				}
				payload, hash, err := fact.CanonicalPayloadHash()
				if err != nil {
					t.Fatal(err)
				}
				if err := observation.AddCanonical(fact, payload, hash); err != nil {
					t.Fatal(err)
				}
			}
			if threshold < 1<<14 && !observation.Spilled() {
				t.Fatalf("threshold %d did not spill", threshold)
			}
			if observation.Len() != len(facts) {
				t.Fatalf("Len %d, want %d", observation.Len(), len(facts))
			}
			actual, err := observation.Digest(rows)
			if err != nil {
				t.Fatal(err)
			}
			if actual != expected {
				t.Fatalf("threshold %d seed %d: digest %s, want %s", threshold, seed, actual, expected)
			}
			// Digest is repeatable and Close leaves nothing behind.
			again, err := observation.Digest(rows)
			if err != nil || again != expected {
				t.Fatalf("second digest %s (%v), want %s", again, err, expected)
			}
			if err := observation.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := observation.Digest(rows); err == nil {
				t.Fatal("digest after Close must fail")
			}
		}
	}
}

func testDerivedFact(t *testing.T, rowKey string, value int64) FactID {
	t.Helper()
	bundle := []SnapshotBinding{{SourceNamespace: "ns.schema", Snapshot: strings.Repeat("a", 64)}}
	fact, err := NewDerivedFactV2(bundle, rowKey, "sum(amount)", "bigint", value, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	return fact
}

func TestReleaseObservationEmptyMatchesEmptyDigest(t *testing.T) {
	expected, err := ReleaseOutcomeDigest(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	observation := NewReleaseObservation()
	defer observation.Close()
	actual, err := observation.Digest(0)
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("empty digest %s, want %s", actual, expected)
	}
	if _, err := observation.Digest(-1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative rows: %v", err)
	}
}

func TestReleaseObservationRejectsNonV2Facts(t *testing.T) {
	observation := NewReleaseObservation()
	defer observation.Close()
	if err := observation.Add(FactID{}); err == nil {
		t.Fatal("zero fact must be rejected")
	}
	fact := testBaseFact(t, "e", "f", "v")
	payload, hash, err := fact.CanonicalPayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	notV2 := fact
	notV2.Profile = ""
	if err := observation.AddCanonical(notV2, payload, hash); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-V2 fact: %v", err)
	}
}

// A hash shared by two different payloads must fail closed whether the pair
// meets inside one run or across runs.
func TestReleaseObservationFailsClosedOnHashCollision(t *testing.T) {
	left := testBaseFact(t, "e", "f", "left")
	right := testBaseFact(t, "e", "f", "right")
	leftPayload, hash, err := left.CanonicalPayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	rightPayload, _, err := right.CanonicalPayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("within a run", func(t *testing.T) {
		observation := newReleaseObservation(1<<20, t.TempDir(), 1<<12)
		defer observation.Close()
		observation.resident = append(observation.resident, releaseEntry{hash: hash, payload: leftPayload},
			releaseEntry{hash: hash, payload: rightPayload})
		if _, err := observation.Digest(1); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("collision within a run: %v", err)
		}
	})
	t.Run("across runs", func(t *testing.T) {
		observation := newReleaseObservation(1<<20, t.TempDir(), 1<<12)
		defer observation.Close()
		observation.resident = append(observation.resident, releaseEntry{hash: hash, payload: leftPayload})
		if err := observation.spillRun(); err != nil {
			t.Fatal(err)
		}
		observation.resident = append(observation.resident, releaseEntry{hash: hash, payload: append([]byte(nil), rightPayload...)})
		if _, err := observation.Digest(1); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("collision across runs: %v", err)
		}
	})
	t.Run("duplicate across runs is not a collision", func(t *testing.T) {
		observation := newReleaseObservation(1<<20, t.TempDir(), 1<<12)
		defer observation.Close()
		if err := observation.Add(left); err != nil {
			t.Fatal(err)
		}
		if err := observation.spillRun(); err != nil {
			t.Fatal(err)
		}
		if err := observation.Add(left); err != nil {
			t.Fatal(err)
		}
		expected, err := ReleaseOutcomeDigest([]FactID{left, left}, 1)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := observation.Digest(1)
		if err != nil || actual != expected {
			t.Fatalf("duplicate across runs: %s (%v), want %s", actual, err, expected)
		}
	})
}
