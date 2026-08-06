package queryreceipt

import "testing"

// The table has to be total over the known versions and no wider. A version that
// orders but has no capabilities cannot validate; a capability row for a version
// nothing orders is a rule nothing can reach.
func TestCapabilityTableIsTotalOverTheKnownVersions(t *testing.T) {
	for _, version := range receiptVersions {
		found, err := CapabilitiesFor(version)
		if err != nil {
			t.Errorf("V%s orders but has no capabilities: %v", version, err)
			continue
		}
		if found.Version != version {
			t.Errorf("the row for V%s names V%s", version, found.Version)
		}
		if found.signatureDomain == "" {
			t.Errorf("V%s has no signature domain, so its payload would be signed in V1's", version)
		}
	}
	if len(capabilities) != len(receiptVersions) {
		t.Errorf("the table has %d rows for %d ordered versions", len(capabilities), len(receiptVersions))
	}
	for version := range capabilities {
		if receiptVersionIndex(version) < 0 {
			t.Errorf("the table has a row for V%s, which nothing orders", version)
		}
	}
}

// No two versions may share a signature domain. A shared domain would let a
// payload signed under one version verify under another, which is precisely the
// substitution the domains exist to prevent.
func TestEverySignatureDomainIsDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, version := range receiptVersions {
		found, err := CapabilitiesFor(version)
		if err != nil {
			t.Fatal(err)
		}
		if owner, taken := seen[found.signatureDomain]; taken {
			t.Errorf("V%s and V%s sign under the same domain %q", owner, version, found.signatureDomain)
		}
		seen[found.signatureDomain] = version
	}
}

// The contract, stated once, in the form a reader can check against the paper.
// This is the table the migration is specified by; if it drifts, this fails
// rather than some downstream behaviour changing quietly.
func TestTheDeclaredCapabilityContract(t *testing.T) {
	type expectation struct {
		artifactIntentRequiredArtifactMode bool
		artifactIntentRequiredInlineMode   bool
		supportsArtifactIntent             bool
		executionBindingVersion            int
		requiresDeliveryMode               bool
		requiresExposureEvidence           bool
		requiresSignedAt                   bool
		requiresSchemaDigest               bool
	}
	for version, want := range map[string]expectation{
		VersionV1: {},
		VersionV2: {requiresSchemaDigest: true},
		VersionV3: {requiresSchemaDigest: true, requiresSignedAt: true},
		VersionV4: {requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true},
		VersionV5: {requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true},
		VersionV6: {requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true},
		VersionV7: {requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true},
		VersionV8: {
			requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true,
			supportsArtifactIntent: true,
			// V8 and V9 require the intent unconditionally, so the mode makes no
			// difference to them. That is exactly the constraint V10 lifts.
			artifactIntentRequiredArtifactMode: true, artifactIntentRequiredInlineMode: true,
		},
		VersionV9: {
			requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true,
			supportsArtifactIntent:             true,
			artifactIntentRequiredArtifactMode: true, artifactIntentRequiredInlineMode: true,
			executionBindingVersion: 1,
		},
		VersionV10: {
			requiresSchemaDigest: true, requiresSignedAt: true, requiresExposureEvidence: true,
			supportsArtifactIntent:             true,
			artifactIntentRequiredArtifactMode: true, artifactIntentRequiredInlineMode: false,
			executionBindingVersion: 2, requiresDeliveryMode: true,
		},
	} {
		t.Run("V"+version, func(t *testing.T) {
			found, err := CapabilitiesFor(version)
			if err != nil {
				t.Fatal(err)
			}
			for name, pair := range map[string][2]bool{
				"schema digest":     {found.RequiresSchemaDigest, want.requiresSchemaDigest},
				"datasource id":     {found.RequiresDatasourceID, want.requiresSchemaDigest},
				"signed_at":         {found.RequiresSignedAt, want.requiresSignedAt},
				"exposure evidence": {found.RequiresExposureEvidence, want.requiresExposureEvidence},
				"delivery mode":     {found.RequiresResultDeliveryMode, want.requiresDeliveryMode},
				"artifact support":  {found.SupportsArtifactIntent(), want.supportsArtifactIntent},
				"artifact required under artifact delivery": {
					found.RequiresArtifactIntent(DeliveryArtifact), want.artifactIntentRequiredArtifactMode,
				},
				"artifact required under inline delivery": {
					found.RequiresArtifactIntent(DeliveryInline), want.artifactIntentRequiredInlineMode,
				},
				"ledger pre-state": {
					found.RequiresExposureLedgerBefore, want.executionBindingVersion != 0,
				},
			} {
				if pair[0] != pair[1] {
					t.Errorf("V%s %s: got %t, want %t", version, name, pair[0], pair[1])
				}
			}
			if found.ExecutionBindingVersion != want.executionBindingVersion {
				t.Errorf("V%s execution binding version: got %d, want %d",
					version, found.ExecutionBindingVersion, want.executionBindingVersion)
			}
			// The two execution-binding predicates must be mutually exclusive. A
			// version requiring both would forbid both, since each forbids the other.
			if found.RequiresExecutionBindingV1() && found.RequiresExecutionBindingV2() {
				t.Errorf("V%s requires both execution binding versions", version)
			}
			if SupportsArtifactIntent(version) != found.SupportsArtifactIntent() ||
				RequiresExposureEvidence(version) != found.RequiresExposureEvidence ||
				RequiresExecutionBindingV1(version) != found.RequiresExecutionBindingV1() ||
				RequiresExecutionBindingV2(version) != found.RequiresExecutionBindingV2() {
				t.Errorf("V%s: the package-level predicates disagree with the row", version)
			}
		})
	}
}

// An unknown version answers nothing rather than defaulting to something.
func TestUnknownVersionsSupportNothing(t *testing.T) {
	for _, version := range []string{"", "0", "11", "9.1", "V9", " 9"} {
		if _, err := CapabilitiesFor(version); err == nil {
			t.Errorf("version %q was given capabilities", version)
		}
		if SupportsArtifactIntent(version) || RequiresExposureEvidence(version) ||
			RequiresExecutionBindingV1(version) || RequiresExecutionBindingV2(version) {
			t.Errorf("version %q was reported as supporting something", version)
		}
	}
}
