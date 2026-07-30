package main

import (
	"fmt"
	"testing"
)

func contractDigest(seed int) string {
	return fmt.Sprintf("%064x", seed)
}

func validOnlineEvidenceForTest() onlineEvidence {
	const rows = int64(2000)
	result := onlineEvidence{
		SchemaVersion: onlineEvidenceSchema, RoutingModel: routingModel,
		RowsPerPublication: rows, MeasurementBoundary: measurementBoundary,
		Fixture: fixtureEvidence{
			FixtureClass: "correctness_fixture", RowsPerPublication: rows,
			GeneratorSHA256: contractDigest(1), ConfigSHA256: contractDigest(2),
			DatasetManifestSHA256: contractDigest(3),
		},
	}
	for index, day := range days {
		base := 100 + index*20
		result.Fixture.Publications = append(result.Fixture.Publications, publicationFixtureEvidence{
			Day: day, PublicationName: fmt.Sprintf("daily-lineitem-%s-r%d", day, rows), RowCount: uint64(rows),
			ApprovedInputSHA256: contractDigest(base), CatalogSHA256: contractDigest(base + 1),
			BundleManifestSHA256: contractDigest(base + 2), PublicationManifestDigest: contractDigest(base + 3),
			DictionaryDigest: contractDigest(base + 4), SidecarDigest: contractDigest(base + 5),
			SchemaDigest: contractDigest(base + 6), HotArtifactSHA256: contractDigest(base + 7),
			ColdArtifactSHA256: contractDigest(base + 8), SidecarArtifactSHA256: contractDigest(base + 9),
			DirectResultSHA256: contractDigest(base + 10),
		})
	}
	for index := 0; index < len(days)-1; index++ {
		oldPublication := result.Fixture.Publications[index]
		newPublication := result.Fixture.Publications[index+1]
		result.Transitions = append(result.Transitions, transitionEvidence{
			From: days[index], To: days[index+1], SwitchWallMS: 0.001,
			FirstQueryWallMS: 10, ReplayWallMS: 5,
			OldTask: oldTaskEvidence{
				PublicationDigestBefore:   oldPublication.PublicationManifestDigest,
				PublicationDigestAfter:    oldPublication.PublicationManifestDigest,
				ExpectedPublicationDigest: oldPublication.PublicationManifestDigest,
				ResultSHA256Before:        oldPublication.DirectResultSHA256,
				ResultSHA256After:         oldPublication.DirectResultSHA256,
				ExpectedResultSHA256:      oldPublication.DirectResultSHA256,
			},
			NewTask: newTaskEvidence{
				PublicationDigest:         newPublication.PublicationManifestDigest,
				ExpectedPublicationDigest: newPublication.PublicationManifestDigest,
				ResultSHA256:              newPublication.DirectResultSHA256,
				ExpectedResultSHA256:      newPublication.DirectResultSHA256,
			},
			OldLedger: oldLedgerEvidence{
				BeforeSwitchSHA256: contractDigest(500 + index),
				AfterSwitchSHA256:  contractDigest(500 + index),
			},
			Cache: cacheEvidence{
				OldCacheKeySHA256:       contractDigest(600 + index),
				FirstNewCacheKeySHA256:  contractDigest(610 + index),
				ReplayNewCacheKeySHA256: contractDigest(610 + index), ReplayNewSemanticReplay: true,
			},
			Delegation: delegationEvidence{
				RootTaskID: fmt.Sprintf("root-%d", index), ChildRootTaskID: fmt.Sprintf("root-%d", index),
				ChildParentTaskID:      fmt.Sprintf("root-%d", index),
				RootPublicationDigest:  newPublication.PublicationManifestDigest,
				ChildPublicationDigest: newPublication.PublicationManifestDigest,
			},
		})
	}
	return result
}

func TestOnlineEvidenceContractAcceptsBoundEvidence(t *testing.T) {
	if err := validOnlineEvidenceForTest().validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestOnlineEvidenceContractRejectsStableWrongOldOracle(t *testing.T) {
	evidence := validOnlineEvidenceForTest()
	evidence.Transitions[0].OldTask.ExpectedPublicationDigest = contractDigest(900)
	if err := evidence.validate(); err == nil {
		t.Fatal("stable old task with a wrong expected publication was accepted")
	}
}

func TestOnlineEvidenceContractRejectsMatchingArbitraryDelegationDigest(t *testing.T) {
	evidence := validOnlineEvidenceForTest()
	arbitrary := contractDigest(901)
	evidence.Transitions[0].Delegation.RootPublicationDigest = arbitrary
	evidence.Transitions[0].Delegation.ChildPublicationDigest = arbitrary
	if err := evidence.validate(); err == nil {
		t.Fatal("matching root/child delegation digests outside the target fixture were accepted")
	}
}

func TestOnlineEvidenceContractRejectsTransitionOutsideFixture(t *testing.T) {
	evidence := validOnlineEvidenceForTest()
	arbitraryPublication := contractDigest(902)
	evidence.Transitions[0].NewTask.PublicationDigest = arbitraryPublication
	evidence.Transitions[0].NewTask.ExpectedPublicationDigest = arbitraryPublication
	evidence.Transitions[0].Delegation.RootPublicationDigest = arbitraryPublication
	evidence.Transitions[0].Delegation.ChildPublicationDigest = arbitraryPublication
	if err := evidence.validate(); err == nil {
		t.Fatal("self-consistent transition outside the four-publication fixture was accepted")
	}
}
