package finalv5oracle

import (
	"fmt"
	"slices"
	"testing"
)

func TestStreamingSemanticSetExternalSortIsExactAndBounded(t *testing.T) {
	input := []string{testDigest(3), testDigest(1), testDigest(2), testDigest(2)}
	summary, err := SummarizeSemanticSet("candidate", digestSliceStream(input), StreamSetOptions{
		MaxInMemoryMembers: 2, CaptureMembers: 10, TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Cardinality != 3 || summary.Stats.InputMembers != 4 || summary.Stats.DuplicateMembers != 1 ||
		summary.Stats.PeakBufferedMembers > 2 || summary.Stats.SpillRuns != 2 || summary.Stats.PeakMergeHeads > 2 {
		t.Fatalf("unexpected external-sort summary: %+v", summary)
	}
	wantMembers := []string{testDigest(1), testDigest(2), testDigest(3)}
	if !summary.MembersComplete || !slices.Equal(summary.Members, wantMembers) || !slices.Equal(summary.SampleMembers, wantMembers) {
		t.Fatalf("auditable members = %#v / %#v", summary.Members, summary.SampleMembers)
	}
	if summary.SetSHA256 != "324bc5705a4c272bc7123b9c97e331a37fcdd2935099fc105cdc395c76408c8d" {
		t.Fatalf("candidate fixed set digest = %s", summary.SetSHA256)
	}

	inMemory, err := SummarizeSemanticSet("candidate", digestSliceStream([]string{testDigest(2), testDigest(3), testDigest(1)}),
		StreamSetOptions{MaxInMemoryMembers: 16, CaptureMembers: 10})
	if err != nil {
		t.Fatal(err)
	}
	if inMemory.SetSHA256 != summary.SetSHA256 || inMemory.Cardinality != summary.Cardinality || inMemory.Stats.SpillRuns != 0 {
		t.Fatalf("chunking or input order changed the set: memory=%+v spill=%+v", inMemory, summary)
	}
	ordinary, err := EvaluateSetAlgebra(wantMembers, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.CandidateSetSHA256 != summary.SetSHA256 {
		t.Fatalf("stream digest %s differs from ordinary-set digest %s", summary.SetSHA256, ordinary.CandidateSetSHA256)
	}
}

func TestStreamingSemanticSetMutationAndRoleBinding(t *testing.T) {
	original := []string{testDigest(10), testDigest(20), testDigest(30)}
	mutated := []string{testDigest(10), testDigest(20), testDigest(31)}
	left, err := SummarizeSemanticSetRoles([]string{"candidate", "union"}, digestSliceStream(original),
		StreamSetOptions{MaxInMemoryMembers: 2, CaptureMembers: 2, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	right, err := SummarizeSemanticSet("candidate", digestSliceStream(mutated),
		StreamSetOptions{MaxInMemoryMembers: 2, CaptureMembers: 2, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if left["candidate"].SetSHA256 == right.SetSHA256 {
		t.Fatal("member mutation did not change the canonical set digest")
	}
	if left["candidate"].SetSHA256 == left["union"].SetSHA256 {
		t.Fatal("candidate and union role domains collapsed")
	}
	if left["candidate"].MembersComplete || left["candidate"].Members != nil || len(left["candidate"].SampleMembers) != 2 {
		t.Fatalf("large-set capture contract = %+v", left["candidate"])
	}
}

func TestStreamingSemanticSetRejectsMalformedInputAndInvalidBounds(t *testing.T) {
	if _, err := SummarizeSemanticSet("candidate", digestSliceStream([]string{"not-a-hash"}), StreamSetOptions{}); err == nil {
		t.Fatal("malformed member was accepted")
	}
	if _, err := SummarizeSemanticSet("wrong-role", digestSliceStream(nil), StreamSetOptions{}); err == nil {
		t.Fatal("invalid set role was accepted")
	}
	if _, err := SummarizeSemanticSet("candidate", digestSliceStream(nil), StreamSetOptions{MaxInMemoryMembers: 1}); err == nil {
		t.Fatal("unusable memory bound was accepted")
	}
}

func digestSliceStream(values []string) SemanticMemberStream {
	return func(yield func(string) error) error {
		for _, value := range values {
			if err := yield(value); err != nil {
				return err
			}
		}
		return nil
	}
}

func testDigest(value int) string { return fmt.Sprintf("%064x", value) }
