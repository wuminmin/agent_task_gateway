package queryreceipt

import "fmt"

// # Why the shape is read from the content and not from a version
//
// There used to be ten receipt versions, and what a receipt carried was decided
// by which one it was: an equality against one version for artifact intent, a
// range for exposure evidence, another range for the outcome shape, then a
// capability table when those stopped agreeing with each other.
//
// The versions were never really a lineage. They were a matrix -- exposure
// profile times result delivery times whether an execution was described --
// projected onto one increasing integer, and the projection lost information
// every time a new combination appeared. The last version added required an
// execution binding every earlier one forbade and made the artifact intent
// conditional on the delivery, which no ordering answers correctly.
//
// So the ladder is gone. There is one receipt version and one signature domain,
// and every question of the form "does this receipt carry X" is answered from
// members the signature already covers:
//
//   - which exposure profile accounted the operation is read from
//     Exposure.ProfileVersion, and decides the shape its evidence must satisfy;
//   - whether a result object was registered is read from ResultDeliveryMode;
//   - whether an execution is described is not read at all. Every completed
//     query describes one and nothing else may, so the binding's presence is an
//     equivalence with the status rather than a variable.
//
// Whether the described execution ACCOUNTED exposure is read from the binding's
// own ExposureProfileVersion, which is empty exactly when the task held no
// exposure grant. That member decides the presence of both the ledger pre-state
// and the exposure charge, so a completed non-exposure query is expressible
// without being handed a fabricated empty ledger to claim it read.
//
// The last point is what makes reading the shape from the content safe. Every
// optional member is signed whether or not it is there -- an inline operation
// signs a null artifact intent, a released query signs a null binding -- so an
// absence is a signed statement rather than something a holder produced by
// deleting a member.

// ResultDeliveryMode is how a governed operation's rows reached the caller.
//
// It is signed rather than inferred from the artifact intent's presence, because
// inferring it makes the two indistinguishable: a receipt that should have
// registered an artifact and did not would read as a well-formed inline
// delivery, which is precisely the omission the mode exists to catch.
type ResultDeliveryMode string

const (
	// DeliveryNone is an operation that produced no result to deliver. A released
	// query never invoked the Connector; a failed or indeterminate one has no
	// result to describe.
	DeliveryNone ResultDeliveryMode = "none"
	// DeliveryInline is a result returned in the response. The operation
	// registers no result object, so the receipt carries no artifact intent.
	DeliveryInline ResultDeliveryMode = "inline"
	// DeliveryArtifact is a result written to a PENDING result object. The
	// receipt carries the artifact intent that describes it.
	DeliveryArtifact ResultDeliveryMode = "artifact"
)

// ResultDeliveryModes returns the closed set.
func ResultDeliveryModes() []ResultDeliveryMode {
	return []ResultDeliveryMode{DeliveryNone, DeliveryInline, DeliveryArtifact}
}

func (mode ResultDeliveryMode) valid() bool {
	for _, known := range ResultDeliveryModes() {
		if mode == known {
			return true
		}
	}
	return false
}

// outcomeShape names the exposure rules one operation's evidence must satisfy.
//
// The shapes are named after the exposure profile because that is what they are
// about. An exposure profile is an experiment variable -- which accounting
// mechanism was in force -- not a protocol version, and the receipt describes
// every one of them under the same signature rather than growing a version per
// profile.
type outcomeShape string

const (
	// outcomeShapeWithoutOutcomes: exposure evidence with no outcome dimension.
	outcomeShapeWithoutOutcomes outcomeShape = "without_outcomes"
	outcomeShapeExposureV3      outcomeShape = "taskgate-exposure-v3"
	outcomeShapeExposureV4      outcomeShape = "taskgate-exposure-v4"
	outcomeShapeExposureV5      outcomeShape = "taskgate-exposure-v5"
)

// exposureProfileShapes is the closed world of exposure profiles a receipt may
// account under.
//
// It is closed rather than permissive because the profile decides which rules
// the evidence is held to. A profile with no entry here has no stated rules, so
// admitting one would mean accepting exposure evidence against no contract at
// all -- the profile string is signed, and a forger's easiest move would be to
// name a profile nothing checks.
var exposureProfileShapes = map[string]outcomeShape{
	"taskgate-exposure-v1": outcomeShapeWithoutOutcomes,
	"taskgate-exposure-v2": outcomeShapeWithoutOutcomes,
	"taskgate-exposure-v3": outcomeShapeExposureV3,
	"taskgate-exposure-v4": outcomeShapeExposureV4,
	"taskgate-exposure-v5": outcomeShapeExposureV5,
}

func outcomeShapeForProfile(profile string) (outcomeShape, error) {
	shape, ok := exposureProfileShapes[profile]
	if !ok {
		return "", fmt.Errorf("%w: exposure profile %q has no stated evidence shape", ErrInvalidReceipt, profile)
	}
	return shape, nil
}

// ExposureProfiles returns the closed set of profiles a receipt may account
// under, for callers that need to state it rather than rediscover it.
func ExposureProfiles() []string {
	profiles := make([]string, 0, len(exposureProfileShapes))
	for profile := range exposureProfileShapes {
		profiles = append(profiles, profile)
	}
	return profiles
}

// IsCurrentVersion reports whether a version string is the one this build reads
// and writes.
//
// There is exactly one, and no reader for any other. A receipt naming a
// different version is not an older receipt to be handled leniently; it is a
// document this build has no contract for.
func IsCurrentVersion(version string) bool { return version == Version }

// RequireCurrentVersion is IsCurrentVersion as an error, for callers that reject
// a receipt before reading anything out of it.
func RequireCurrentVersion(version string) error {
	if !IsCurrentVersion(version) {
		return fmt.Errorf("%w: receipt version %q is not the current version %q",
			ErrInvalidReceipt, version, Version)
	}
	return nil
}

// CarriesArtifactIntent reports whether this receipt describes a registered
// result object.
//
// Audit code must ask this rather than infer it from the version: an inline
// operation registers no result object, and code that assumed every receipt has
// an artifact intent would fail on exactly those.
func (r QueryReceiptV1) CarriesArtifactIntent() bool { return r.ArtifactIntent != nil }

// RequiresArtifactInclusionProofs reports whether this receipt must be
// accompanied by the artifact registration and availability inclusion proofs.
//
// Asked of the signed delivery mode rather than of the intent's presence, so a
// receipt that should have registered an artifact and did not is caught here
// rather than quietly excused from its own proofs.
func (r QueryReceiptV1) RequiresArtifactInclusionProofs() bool {
	return r.ResultDeliveryMode == DeliveryArtifact
}

// DescribesAnExecution reports whether this receipt says which physical
// statements ran.
func (r QueryReceiptV1) DescribesAnExecution() bool { return r.ExecutionBindingV2 != nil }
