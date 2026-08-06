package queryreceipt

import "fmt"

// # Why a matrix and not a comparison
//
// Every feature this file describes used to be decided by comparing the receipt
// version against a literal: an equality against V8 for artifact intent, a range
// against V4 for exposure evidence, a range against V7 for the outcome shape.
// Both spellings have failed here already. The equalities stopped matching when
// V9 arrived, so a V9 receipt skipped the artifact inclusion proofs and the
// registration projection it in fact carried; the ranges silently captured V9
// into rules written for V8, which is only correct while every later version is
// strictly additive.
//
// V10 is the version where "strictly additive" stops being true. It requires an
// execution binding V9 forbids, forbids the one V9 requires, and makes the
// artifact intent conditional on how the result was delivered rather than
// mandatory. There is no ordering of V9 and V10 under which a range check gives
// the right answer for both, so ordering stops being the thing features are read
// from.
//
// VersionAtLeast survives for what it actually means -- which of two versions is
// older -- and nothing else. Every question of the form "does this receipt carry
// X" is answered here, once, by a table that a new version has to be added to
// before it can validate at all.

// ResultDeliveryMode is how a governed operation's rows reached the caller.
//
// It is signed rather than inferred from the artifact intent's presence, because
// inferring it makes the two indistinguishable: a receipt that should have
// registered an artifact and did not would read as a well-formed inline
// delivery, which is precisely the omission the mode exists to catch.
type ResultDeliveryMode string

const (
	// DeliveryInline is a result returned in the response. The operation
	// registers no result object, so the receipt carries no artifact intent.
	DeliveryInline ResultDeliveryMode = "inline"
	// DeliveryArtifact is a result written to a PENDING result object. The
	// receipt carries the artifact intent that describes it.
	DeliveryArtifact ResultDeliveryMode = "artifact"
)

// ResultDeliveryModes returns the closed set.
func ResultDeliveryModes() []ResultDeliveryMode {
	return []ResultDeliveryMode{DeliveryInline, DeliveryArtifact}
}

func (mode ResultDeliveryMode) valid() bool {
	for _, known := range ResultDeliveryModes() {
		if mode == known {
			return true
		}
	}
	return false
}

// artifactIntentRule says how a version decides whether the artifact intent is
// present.
type artifactIntentRule int

const (
	// artifactIntentForbidden: the version's signature does not cover the intent,
	// so carrying one would present unsigned material as signed.
	artifactIntentForbidden artifactIntentRule = iota
	// artifactIntentAlways: every receipt of this version registers a result
	// object. V8 and V9.
	artifactIntentAlways
	// artifactIntentByDeliveryMode: the signed delivery mode decides. V10.
	artifactIntentByDeliveryMode
)

// outcomeShape names the exposure rules one version's evidence must satisfy.
//
// The shapes are named after the exposure profile rather than after the receipt
// version that introduced them, because that is what they are about: two receipt
// versions sharing a profile share a shape, and a receipt version that changes
// nothing about exposure does not get a shape of its own.
type outcomeShape string

const (
	// outcomeShapeNone: the version predates exposure evidence and must carry
	// none.
	outcomeShapeNone outcomeShape = "none"
	// outcomeShapeWithoutOutcomes: exposure evidence with no outcome dimension.
	outcomeShapeWithoutOutcomes outcomeShape = "without_outcomes"
	outcomeShapeExposureV3      outcomeShape = "taskgate-exposure-v3"
	outcomeShapeExposureV4      outcomeShape = "taskgate-exposure-v4"
	outcomeShapeExposureV5      outcomeShape = "taskgate-exposure-v5"
)

// Capabilities is everything one receipt version's signature covers and
// requires.
//
// It is a value rather than a set of methods so that the whole answer for a
// version can be read in one place, and so a test can assert the table is total
// over the known versions.
type Capabilities struct {
	Version string

	// The identity and timing evidence.
	RequiresDatasourceID bool
	RequiresSchemaDigest bool
	RequiresSignedAt     bool

	// The exposure accounting.
	RequiresExposureEvidence bool
	OutcomeShape             outcomeShape

	// The result object.
	artifactIntent artifactIntentRule

	// The execution evidence. 0 means the version describes no execution;
	// otherwise it is the QueryExecutionBinding version the receipt must carry,
	// and every other version is forbidden.
	ExecutionBindingVersion int
	// RequiresExposureLedgerBefore is true exactly when an execution binding is
	// required: the binding's row limits are only checkable against the pre-state
	// they were derived from.
	RequiresExposureLedgerBefore bool

	// RequiresResultDeliveryMode is true when the signature covers the mode and
	// the receipt must name it.
	RequiresResultDeliveryMode bool

	signatureDomain string
}

// SupportsArtifactIntent reports whether a receipt of this version may carry
// artifact intent at all.
func (capabilities Capabilities) SupportsArtifactIntent() bool {
	return capabilities.artifactIntent != artifactIntentForbidden
}

// RequiresArtifactIntent reports whether a receipt of this version, delivering
// its result the given way, must carry artifact intent.
//
// The mode is a parameter rather than read off the version because that is the
// whole change V10 makes: whether a result object exists is a property of the
// operation, not of the receipt format. Passing the empty mode is correct for
// every version that does not sign one.
func (capabilities Capabilities) RequiresArtifactIntent(mode ResultDeliveryMode) bool {
	switch capabilities.artifactIntent {
	case artifactIntentAlways:
		return true
	case artifactIntentByDeliveryMode:
		return mode == DeliveryArtifact
	default:
		return false
	}
}

// RequiresExecutionBindingV1 and RequiresExecutionBindingV2 are the two
// execution-evidence predicates. A version requires at most one of them, and
// forbids the other.
func (capabilities Capabilities) RequiresExecutionBindingV1() bool {
	return capabilities.ExecutionBindingVersion == 1
}

func (capabilities Capabilities) RequiresExecutionBindingV2() bool {
	return capabilities.ExecutionBindingVersion == 2
}

// capabilities is the whole table. A version absent from it cannot validate,
// which is the intended failure: a receipt version whose capabilities nobody
// wrote down has no defined evidence contract.
var capabilities = map[string]Capabilities{
	VersionV1: {
		Version: VersionV1, OutcomeShape: outcomeShapeNone,
		artifactIntent: artifactIntentForbidden, signatureDomain: signatureDomainV1,
	},
	VersionV2: {
		Version: VersionV2, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		OutcomeShape: outcomeShapeNone, artifactIntent: artifactIntentForbidden,
		signatureDomain: signatureDomainV2,
	},
	VersionV3: {
		Version: VersionV3, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, OutcomeShape: outcomeShapeNone,
		artifactIntent: artifactIntentForbidden, signatureDomain: signatureDomainV3,
	},
	VersionV4: {
		Version: VersionV4, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape:   outcomeShapeWithoutOutcomes,
		artifactIntent: artifactIntentForbidden, signatureDomain: signatureDomainV4,
	},
	VersionV5: {
		Version: VersionV5, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape:   outcomeShapeExposureV3,
		artifactIntent: artifactIntentForbidden, signatureDomain: signatureDomainV5,
	},
	VersionV6: {
		Version: VersionV6, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape:   outcomeShapeExposureV4,
		artifactIntent: artifactIntentForbidden, signatureDomain: signatureDomainV6,
	},
	VersionV7: {
		Version: VersionV7, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape:   outcomeShapeExposureV5,
		artifactIntent: artifactIntentForbidden, signatureDomain: signatureDomainV7,
	},
	VersionV8: {
		Version: VersionV8, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape:   outcomeShapeExposureV5,
		artifactIntent: artifactIntentAlways, signatureDomain: signatureDomainV8,
	},
	VersionV9: {
		Version: VersionV9, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape:            outcomeShapeExposureV5,
		artifactIntent:          artifactIntentAlways,
		ExecutionBindingVersion: 1, RequiresExposureLedgerBefore: true,
		signatureDomain: signatureDomainV9,
	},
	VersionV10: {
		Version: VersionV10, RequiresDatasourceID: true, RequiresSchemaDigest: true,
		RequiresSignedAt: true, RequiresExposureEvidence: true,
		OutcomeShape: outcomeShapeExposureV5,
		// The change that makes V10 the general execution-evidence receipt rather
		// than V9 plus a field: a governed operation that returns its rows inline
		// registers no result object, and must not be forced to invent one in
		// order to describe what it executed.
		artifactIntent:          artifactIntentByDeliveryMode,
		ExecutionBindingVersion: 2, RequiresExposureLedgerBefore: true,
		RequiresResultDeliveryMode: true,
		signatureDomain:            signatureDomainV10,
	},
}

// CapabilitiesFor is the only way to ask what a version carries.
func CapabilitiesFor(version string) (Capabilities, error) {
	found, ok := capabilities[version]
	if !ok {
		return Capabilities{}, fmt.Errorf("%w: unsupported version %q", ErrInvalidReceipt, version)
	}
	return found, nil
}

// The package-level predicates, for callers holding a version string rather than
// a receipt. Each returns false for an unknown version: a version nobody has
// written capabilities for supports nothing.

func SupportsArtifactIntent(version string) bool {
	found, err := CapabilitiesFor(version)
	return err == nil && found.SupportsArtifactIntent()
}

func RequiresExposureEvidence(version string) bool {
	found, err := CapabilitiesFor(version)
	return err == nil && found.RequiresExposureEvidence
}

func RequiresExecutionBindingV1(version string) bool {
	found, err := CapabilitiesFor(version)
	return err == nil && found.RequiresExecutionBindingV1()
}

func RequiresExecutionBindingV2(version string) bool {
	found, err := CapabilitiesFor(version)
	return err == nil && found.RequiresExecutionBindingV2()
}

// Capabilities is this receipt's row of the table.
func (r QueryReceiptV1) Capabilities() (Capabilities, error) {
	return CapabilitiesFor(r.Version)
}

// CarriesArtifactIntent reports whether this receipt describes a registered
// result object.
//
// Audit code must ask this rather than infer it from the version. Under V8 and
// V9 the two agreed; under V10 they do not, and code that assumed every V10
// receipt has an artifact intent would fail on exactly the inline deliveries V10
// exists to admit.
func (r QueryReceiptV1) CarriesArtifactIntent() bool {
	return r.ArtifactIntent != nil
}

// RequiresArtifactInclusionProofs reports whether this receipt must be
// accompanied by the artifact registration and availability inclusion proofs.
func (r QueryReceiptV1) RequiresArtifactInclusionProofs() bool {
	found, err := r.Capabilities()
	if err != nil {
		return false
	}
	return found.RequiresArtifactIntent(r.ResultDeliveryMode)
}
