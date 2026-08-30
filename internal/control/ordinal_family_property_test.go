package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"taskbound.local/agent-data-gateway/internal/exposure"
	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/testpostgres"
)

// Randomized root-family settlement sequences.
//
// The scripted cases above fix one tree shape and one order each. This case
// draws many random delegation trees, random observation sets, and random
// interleavings (sequential steps and concurrent bursts) and checks, after every
// step, the three properties the paper claims of the root-family ledger:
//
//  1. the committed root head equals the plain set union of the committed
//     observations in every dimension (the model below is an ordinary Go map,
//     independent of the bitmap and radix code under test);
//  2. the head never exceeds the root limits in any dimension;
//  3. every refusal was necessary: the refused observation, unioned with the
//     final committed set, exceeds a limit (the committed set only grows, so an
//     observation that fits at the end fit when it was refused).
//
// For sequential steps the per-query charge is also checked to be exactly the
// novelty of the observation against the union at that moment, including zero
// for replays from any task in the family.
func TestOrdinalRootFamilyRandomSequencesConserveExactNovelty(t *testing.T) {
	store := openTestStore(t, testpostgres.SchemaDSN(t), testCipher(t, 41))
	const ordinalsPerSegment = 24
	publishOrdinalTestDictionaryWithCount(t, store, ordinalsPerSegment)
	expires := time.Now().UTC().Add(time.Hour)

	const seeds = 24
	for seed := int64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			random := rand.New(rand.NewSource(seed))
			limits := ExposureLimits{
				ReleaseFacts:   int64(6 + random.Intn(36)),
				InfluenceFacts: int64(8 + random.Intn(56)),
				OutcomeFacts:   int64(3 + random.Intn(6)),
			}
			rootID := fmt.Sprintf("task_family_prop_%d_root", seed)
			createAwaitingApprovalTask(t, store, rootID, expires)
			approveOrdinalTask(t, store, rootID, expires, limits)

			// Random delegation tree: every descendant shares the root ledger and
			// signs its own ceiling drawn uniformly at or below its parent's in
			// each dimension (the Gateway's Catalog intersection can only narrow).
			// Settlement admits a query only while the family total stays within
			// both the root's and the settling task's ceiling (exceedsOrdinalLimit).
			tasks := []string{rootID}
			taskLimits := map[string]ExposureLimits{rootID: limits}
			narrowed := 0
			for child := 0; child < 1+random.Intn(6); child++ {
				parent := tasks[random.Intn(len(tasks))]
				parentLimits := taskLimits[parent]
				childLimits := ExposureLimits{
					ReleaseFacts:   1 + random.Int63n(parentLimits.ReleaseFacts),
					InfluenceFacts: 1 + random.Int63n(parentLimits.InfluenceFacts),
					OutcomeFacts:   1 + random.Int63n(parentLimits.OutcomeFacts),
				}
				if childLimits != parentLimits {
					narrowed++
				}
				childID := fmt.Sprintf("task_family_prop_%d_child_%d", seed, child)
				createOrdinalChildTask(t, store, childID, parent, expires, childLimits)
				tasks = append(tasks, childID)
				taskLimits[childID] = childLimits
			}
			// approveOrdinalTask grants ten queries per task; reservations, refused
			// or not, consume that budget.
			remainingQueries := make(map[string]int, len(tasks))
			for _, task := range tasks {
				remainingQueries[task] = 10
			}

			model := newFamilyUnionModel()
			var refused []familyObservation
			queryCounter := 0
			for round := 0; round < 20; round++ {
				var eligible []string
				for _, task := range tasks {
					if remainingQueries[task] > 0 {
						eligible = append(eligible, task)
					}
				}
				if len(eligible) == 0 {
					break
				}
				random.Shuffle(len(eligible), func(i, j int) { eligible[i], eligible[j] = eligible[j], eligible[i] })
				concurrent := random.Intn(3) == 0 && len(eligible) > 1
				width := 1
				if concurrent {
					width = 2 + random.Intn(min(3, len(eligible)-1))
				}
				batch := make([]familyObservation, 0, width)
				for _, task := range eligible[:width] {
					queryCounter++
					remainingQueries[task]--
					queryID := fmt.Sprintf("query_family_prop_%d_%03d", seed, queryCounter)
					reservation := reserveOrdinalQuery(t, store, task, queryID, "request-"+queryID)
					batch = append(batch, randomFamilyObservation(t, random, ordinalsPerSegment, task, reservation.QueryID))
				}

				if !concurrent {
					step := batch[0]
					expected := model.novelty(step)
					headBefore, err := store.GetOrdinalRootHead(context.Background(), rootID)
					if err != nil {
						t.Fatal(err)
					}
					_, err = store.FinalizeQuery(context.Background(), BudgetSettlement{
						QueryID: step.queryID, Rows: 1, DBMS: 1, OrdinalExposure: &step.observation,
					}, []byte(`{"ok":true}`))
					fits := model.fits(step, taskLimits[step.taskID])
					switch {
					case err == nil && fits:
						model.commit(step)
						headAfter, err := store.GetOrdinalRootHead(context.Background(), rootID)
						if err != nil {
							t.Fatal(err)
						}
						charged := ExposureLimits{
							ReleaseFacts:   headAfter.Used.ReleaseFacts - headBefore.Used.ReleaseFacts,
							InfluenceFacts: headAfter.Used.InfluenceFacts - headBefore.Used.InfluenceFacts,
							OutcomeFacts:   headAfter.Used.OutcomeFacts - headBefore.Used.OutcomeFacts,
						}
						if charged != expected {
							t.Fatalf("seed %d step %s: charged %+v, novelty %+v", seed, step.queryID, charged, expected)
						}
					case errors.Is(err, ErrExposureBudgetExhausted) && !fits:
						releaseRefusedReservation(t, store, step.queryID)
						refused = append(refused, step)
					default:
						t.Fatalf("seed %d step %s: err=%v fits=%v novelty=%+v used=%+v limits=%+v",
							seed, step.queryID, err, fits, expected, model.used(), limits)
					}
				} else {
					results := make([]error, len(batch))
					var wait sync.WaitGroup
					for index := range batch {
						wait.Add(1)
						go func(index int) {
							defer wait.Done()
							_, results[index] = store.FinalizeQuery(context.Background(), BudgetSettlement{
								QueryID: batch[index].queryID, Rows: 1, DBMS: 1, OrdinalExposure: &batch[index].observation,
							}, []byte(`{"ok":true}`))
						}(index)
					}
					wait.Wait()
					for index, err := range results {
						switch {
						case err == nil:
							model.commit(batch[index])
						case errors.Is(err, ErrExposureBudgetExhausted):
							releaseRefusedReservation(t, store, batch[index].queryID)
							refused = append(refused, batch[index])
						default:
							t.Fatalf("seed %d concurrent %s: %v", seed, batch[index].queryID, err)
						}
					}
				}

				head, err := store.GetOrdinalRootHead(context.Background(), rootID)
				if err != nil {
					t.Fatal(err)
				}
				if head.Used != model.used() {
					t.Fatalf("seed %d round %d: head %+v, model union %+v", seed, round, head.Used, model.used())
				}
				if head.Used.ReleaseFacts > limits.ReleaseFacts || head.Used.InfluenceFacts > limits.InfluenceFacts ||
					head.Used.OutcomeFacts > limits.OutcomeFacts {
					t.Fatalf("seed %d round %d: head %+v exceeds limits %+v", seed, round, head.Used, limits)
				}
			}
			for _, step := range refused {
				if model.fits(step, taskLimits[step.taskID]) {
					t.Fatalf("seed %d: refused %s would fit the final union %+v under %+v",
						seed, step.queryID, model.used(), limits)
				}
			}
			t.Logf("seed %d: tasks=%d queries=%d committed=%d refused=%d final=%+v limits=%+v narrowed=%d",
				seed, len(tasks), queryCounter, queryCounter-len(refused), len(refused), model.used(), limits, narrowed)
		})
	}
}

// releaseRefusedReservation records the refusal the Gateway would record, which
// frees the task for its next query; the reservation still counts against the
// task's query budget.
func releaseRefusedReservation(t *testing.T, store *Store, queryID string) {
	t.Helper()
	if _, err := store.FailBudget(context.Background(), BudgetSettlement{
		QueryID: queryID, Rows: 0, DBMS: 0, ErrorCode: "EXPOSURE_BUDGET_EXHAUSTED",
	}); err != nil {
		t.Fatalf("fail refused reservation %s: %v", queryID, err)
	}
}

type familyObservation struct {
	taskID, queryID string
	release         []string
	influence       []string
	outcome         []string
	observation     OrdinalExposureObservation
}

func randomFamilyObservation(t *testing.T, random *rand.Rand, ordinals int, taskID, queryID string) familyObservation {
	t.Helper()
	pick := func(segment string, count int) ([]ordinal.FactRef, []string) {
		chosen := make(map[uint32]struct{}, count)
		for len(chosen) < count {
			chosen[uint32(random.Intn(ordinals))] = struct{}{}
		}
		refs := make([]ordinal.FactRef, 0, count)
		keys := make([]string, 0, count)
		for value := range chosen {
			refs = append(refs, ordinal.FactRef{DictionaryDigest: testOrdinalDictionary, SegmentID: segment, Ordinal: value})
			keys = append(keys, fmt.Sprintf("%s#%d", segment, value))
		}
		return refs, keys
	}
	releaseRefs, releaseKeys := pick(testCellSegment, random.Intn(4))
	rowRefs, rowKeys := pick(testRowSegment, random.Intn(4))
	cellRefs, cellKeys := pick(testCellSegment, random.Intn(4))
	influenceRefs := append(rowRefs, cellRefs...)
	influenceKeys := append(rowKeys, cellKeys...)
	release, err := ordinal.NewBitmapSet(releaseRefs...)
	if err != nil {
		t.Fatal(err)
	}
	influence, err := ordinal.NewBitmapSet(influenceRefs...)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := ordinal.NewBitmapSet()
	if err != nil {
		t.Fatal(err)
	}
	// A V4 observation carries exactly one dynamic Outcome fact (the settled
	// composite); a small payload pool makes repeats, and so zero-novelty
	// Outcome charges, frequent.
	payload := fmt.Sprintf("outcome-%c", 'a'+random.Intn(6))
	dynamic := []OrdinalDynamicFact{{
		SHA256: digestDynamic(OrdinalDynamicOutcome, []byte(payload)), Kind: OrdinalDynamicOutcome,
		CanonicalPayload: []byte(payload),
	}}
	outcomeKeys := []string{payload}
	return familyObservation{
		taskID: taskID, queryID: queryID, release: releaseKeys, influence: influenceKeys, outcome: outcomeKeys,
		observation: OrdinalExposureObservation{ProfileVersion: exposure.ProfileV4, DictionarySetDigest: testOrdinalSet,
			Release: OrdinalHybridSet{Static: release}, Influence: OrdinalHybridSet{Static: influence},
			Outcome: OrdinalHybridSet{Static: empty, DynamicFacts: dynamic}},
	}
}

// familyUnionModel is the independent oracle: three plain string sets.
type familyUnionModel struct {
	mu        sync.Mutex
	release   map[string]struct{}
	influence map[string]struct{}
	outcome   map[string]struct{}
}

func newFamilyUnionModel() *familyUnionModel {
	return &familyUnionModel{release: map[string]struct{}{}, influence: map[string]struct{}{}, outcome: map[string]struct{}{}}
}

func novelCount(set map[string]struct{}, keys []string) int64 {
	var novel int64
	for _, key := range keys {
		if _, present := set[key]; !present {
			novel++
		}
	}
	return novel
}

func (model *familyUnionModel) novelty(step familyObservation) ExposureLimits {
	model.mu.Lock()
	defer model.mu.Unlock()
	return ExposureLimits{
		ReleaseFacts:   novelCount(model.release, step.release),
		InfluenceFacts: novelCount(model.influence, step.influence),
		OutcomeFacts:   novelCount(model.outcome, step.outcome),
	}
}

// fits mirrors exceedsOrdinalLimit: a dimension is checked only when the
// observation adds novel facts to it, so a zero-novelty replay settles even
// when the family total already exceeds the settling task's narrower ceiling
// (Section 5.1's rule); a novel dimension must fit both the root's and the
// settling task's ceiling, and the latter never exceeds the former here.
func (model *familyUnionModel) fits(step familyObservation, limits ExposureLimits) bool {
	novelty := model.novelty(step)
	used := model.used()
	return (novelty.ReleaseFacts == 0 || used.ReleaseFacts+novelty.ReleaseFacts <= limits.ReleaseFacts) &&
		(novelty.InfluenceFacts == 0 || used.InfluenceFacts+novelty.InfluenceFacts <= limits.InfluenceFacts) &&
		(novelty.OutcomeFacts == 0 || used.OutcomeFacts+novelty.OutcomeFacts <= limits.OutcomeFacts)
}

func (model *familyUnionModel) commit(step familyObservation) {
	model.mu.Lock()
	defer model.mu.Unlock()
	for _, key := range step.release {
		model.release[key] = struct{}{}
	}
	for _, key := range step.influence {
		model.influence[key] = struct{}{}
	}
	for _, key := range step.outcome {
		model.outcome[key] = struct{}{}
	}
}

func (model *familyUnionModel) used() ExposureLimits {
	model.mu.Lock()
	defer model.mu.Unlock()
	return ExposureLimits{
		ReleaseFacts:   int64(len(model.release)),
		InfluenceFacts: int64(len(model.influence)),
		OutcomeFacts:   int64(len(model.outcome)),
	}
}

// publishOrdinalTestDictionaryWithCount publishes the test dictionary with a
// chosen number of ordinals per segment; the scripted cases keep the four-fact
// dictionary of publishOrdinalTestDictionary.
func publishOrdinalTestDictionaryWithCount(t *testing.T, store *Store, ordinals uint64) {
	t.Helper()
	rowPayload := []byte("test-row-dictionary-chunk")
	cellPayload := []byte("test-cell-dictionary-chunk")
	rowHash := sha256.Sum256(rowPayload)
	cellHash := sha256.Sum256(cellPayload)
	manifest := OrdinalDictionaryManifest{
		Digest: testOrdinalDictionary, ManifestDigest: strings.Repeat("1", 64), PublicationDigest: strings.Repeat("1", 64),
		DatasourceID: "taskgate-test-expenses", SourceNamespace: "expense_detail", SnapshotID: "snapshot-v4",
		FactCount: 2 * ordinals, ManifestJSON: []byte(`{"version":"test-v4"}`),
		Segments: []OrdinalDictionarySegment{
			{ID: testRowSegment, FactKind: "BASE_ROW", OrdinalCount: ordinals, Digest: strings.Repeat("d", 64),
				Chunks: []OrdinalDictionaryChunk{{Index: 0, SHA256: hex.EncodeToString(rowHash[:]),
					Compression: "NONE", Payload: rowPayload, UncompressedBytes: int64(len(rowPayload)), FactCount: ordinals}}},
			{ID: testCellSegment, FactKind: "BASE_CELL", FieldName: "amount", OrdinalCount: ordinals,
				Digest: strings.Repeat("f", 64), Chunks: []OrdinalDictionaryChunk{{Index: 0,
					SHA256: hex.EncodeToString(cellHash[:]), Compression: "NONE", Payload: cellPayload,
					UncompressedBytes: int64(len(cellPayload)), FactCount: ordinals}}},
		},
	}
	if err := store.putOrdinalDictionary(context.Background(), manifest); err != nil {
		t.Fatalf("putOrdinalDictionary: %v", err)
	}
	if err := store.PutOrdinalDictionarySet(context.Background(), testOrdinalSetManifest); err != nil {
		t.Fatalf("PutOrdinalDictionarySet: %v", err)
	}
}
