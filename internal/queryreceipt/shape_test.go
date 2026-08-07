package queryreceipt

import (
	"sort"
	"strings"
	"testing"
)

// There is one version and one signature domain, and both are stated once.
//
// This is the whole of what used to be a ten-row capability table. A second
// version constant appearing here is not a test failure to be patched -- it
// means the ladder has grown back.
func TestThereIsExactlyOneReceiptVersion(t *testing.T) {
	if Version != "10" {
		t.Fatalf("the current receipt version is %q; fixtures and stored rows name %q", Version, "10")
	}
	if !strings.HasPrefix(signatureDomain, "TASKGATE-QUERY-RECEIPT-") || !strings.HasSuffix(signatureDomain, "\x00") {
		t.Fatalf("the signature domain %q is not a NUL-terminated query receipt domain", signatureDomain)
	}
	if err := RequireCurrentVersion(Version); err != nil {
		t.Fatalf("the current version is not accepted by its own predicate: %v", err)
	}
}

// The exposure profile is an experiment variable, and every profile the system
// runs must have a stated evidence shape.
//
// The set is closed on purpose: the profile string is signed, and admitting an
// unlisted one would mean accepting exposure evidence against no rules at all.
func TestEveryExposureProfileStatesAShape(t *testing.T) {
	want := []string{
		"taskgate-exposure-v1", "taskgate-exposure-v2", "taskgate-exposure-v3",
		"taskgate-exposure-v4", "taskgate-exposure-v5",
	}
	got := ExposureProfiles()
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("the receipt states shapes for %v; the experiment runs %v", got, want)
	}
	for index, profile := range want {
		if got[index] != profile {
			t.Fatalf("the receipt states shapes for %v; the experiment runs %v", got, want)
		}
		if _, err := outcomeShapeForProfile(profile); err != nil {
			t.Errorf("profile %q has no shape: %v", profile, err)
		}
	}
	for _, unknown := range []string{"", "taskgate-exposure-v6", "exposure-v5", "TASKGATE-EXPOSURE-V5"} {
		if _, err := outcomeShapeForProfile(unknown); err == nil {
			t.Errorf("profile %q was given a shape", unknown)
		}
	}
}

// Every delivery mode is in the closed set, and nothing outside it validates.
func TestTheDeliveryModeSetIsClosed(t *testing.T) {
	for _, mode := range ResultDeliveryModes() {
		if !mode.valid() {
			t.Errorf("mode %q is in the set but does not validate", mode)
		}
	}
	for _, mode := range []ResultDeliveryMode{"", "parquet", "ARTIFACT", "inline ", "NONE"} {
		if mode.valid() {
			t.Errorf("mode %q validated", mode)
		}
	}
}

// The contract, stated once, in the form a reader can check against the paper:
// what the receipt requires is decided by what the operation did, not by which
// version it is.
func TestTheDeclaredShapeContract(t *testing.T) {
	for name, test := range map[string]struct {
		receipt                func(*testing.T) QueryReceiptV1
		carriesArtifactIntent  bool
		requiresArtifactProofs bool
		describesAnExecution   bool
		accountsExposure       bool
	}{
		"inline, no exposure": {
			receipt:              func(t *testing.T) QueryReceiptV1 { return validReceipt(t) },
			describesAnExecution: true,
		},
		"inline under exposure v3": {
			receipt:              func(t *testing.T) QueryReceiptV1 { return exposureV3Receipt(t) },
			describesAnExecution: true,
			accountsExposure:     true,
		},
		"inline under exposure v5": {
			receipt:              func(t *testing.T) QueryReceiptV1 { return exposureV5Receipt(t) },
			describesAnExecution: true,
			accountsExposure:     true,
		},
		"artifact delivery": {
			receipt:                validArtifactReceipt,
			carriesArtifactIntent:  true,
			requiresArtifactProofs: true,
			describesAnExecution:   true,
			accountsExposure:       true,
		},
		"paired novel under expanded evidence": {
			receipt:                validExecutionReceipt,
			carriesArtifactIntent:  true,
			requiresArtifactProofs: true,
			describesAnExecution:   true,
			accountsExposure:       true,
		},
		"released, nothing executed": {
			receipt: func(t *testing.T) QueryReceiptV1 {
				return validReleasedReceipt(t, StatusReleased, "BUDGET_EXHAUSTED")
			},
		},
		"failed": {
			receipt: func(t *testing.T) QueryReceiptV1 { return validFailedReceipt(t) },
		},
		"indeterminate": {
			receipt: func(t *testing.T) QueryReceiptV1 { return validIndeterminateReceipt(t) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			receipt := test.receipt(t)
			receipt.GatewayKeyID = "gateway-demo-ed25519-v1"
			if err := receipt.ValidateUnsigned(); err != nil {
				t.Fatalf("the declared shape does not validate: %v", err)
			}
			for label, pair := range map[string][2]bool{
				"carries artifact intent":  {receipt.CarriesArtifactIntent(), test.carriesArtifactIntent},
				"requires artifact proofs": {receipt.RequiresArtifactInclusionProofs(), test.requiresArtifactProofs},
				"describes an execution":   {receipt.DescribesAnExecution(), test.describesAnExecution},
				"accounts exposure":        {receipt.Exposure != nil, test.accountsExposure},
			} {
				if pair[0] != pair[1] {
					t.Errorf("%s: got %t, want %t", label, pair[0], pair[1])
				}
			}
			// Describing an execution is the same fact as having completed. The
			// table states the two independently so that a row where they came
			// apart would have to be written down to pass.
			if receipt.DescribesAnExecution() != (receipt.Status == StatusCompleted) {
				t.Errorf("a %s receipt describes an execution: %t", receipt.Status, receipt.DescribesAnExecution())
			}
			// The ledger pre-state belongs to the exposure charge, not to the
			// binding: a completed non-exposure query describes an execution and
			// carries no ledger at all.
			if (receipt.ExposureLedgerBefore != nil) != (receipt.Exposure != nil) {
				t.Error("the ledger pre-state and the exposure charge disagree about whether exposure was accounted")
			}
		})
	}
}
