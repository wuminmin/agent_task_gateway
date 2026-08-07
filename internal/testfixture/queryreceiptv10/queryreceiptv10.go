// Package queryreceiptv10 builds signed Query Receipt documents for tests.
//
// It exists because the receipt fixtures that already existed lived in package
// queryreceipt's own _test.go files as an eight-deep chain of unexported
// helpers, reachable only from inside that package. The alternative to this
// package was exporting that chain into production API so another package's
// tests could call it, which would have put test scaffolding on the production
// surface permanently in order to serve a temporary need.
//
// Everything here is built from public production constructors and types. It
// therefore also serves as a standing check that a valid receipt CAN be built
// from the public surface: if a future change makes that impossible without an
// unexported helper, this package stops compiling.
//
// Two rules hold and are enforced by TestFixtureIsNotImportedByProduction in
// this directory:
//
//   - no raw SQL appears here, in any field, in any form. Every statement is a
//     digest, exactly as it is in a real receipt;
//   - no non-test production file imports this package.
//
// The signer is seeded, so receipt bytes and signatures are byte-for-byte
// reproducible across runs. Gate 22 compares stored receipt bytes, and a
// fixture that re-randomized its key would make that comparison vacuous.
package queryreceiptv10

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"taskbound.local/agent-data-gateway/internal/preparedbinding"
	"taskbound.local/agent-data-gateway/internal/querybinding"
	"taskbound.local/agent-data-gateway/internal/queryreceipt"
)

// KeyID is the fixture signing key's identity.
const KeyID = "taskgate-test-fixture-ed25519-v1"

// seed is fixed so the key, and therefore every signature, is deterministic.
const seed = "taskgate-final-v5-queryreceipt-v10-fixture-seed-001"

// digest is a deterministic stand-in digest derived from a label. Labels rather
// than repeated characters make a failing assertion say which value moved.
func digest(label string) string {
	sum := sha256.Sum256([]byte("taskgate-fixture/" + label))
	return fmt.Sprintf("%x", sum)
}

// Signer returns the deterministic fixture signer.
func Signer() (*queryreceipt.Signer, error) {
	key := ed25519.NewKeyFromSeed([]byte(seed)[:ed25519.SeedSize])
	return queryreceipt.NewSigner(KeyID, key)
}

// Verifier returns a verifier that accepts exactly what Signer produces.
func Verifier() (*queryreceipt.Verifier, error) {
	signer, err := Signer()
	if err != nil {
		return nil, err
	}
	keyring, err := queryreceipt.NewKeyring(signer, nil)
	if err != nil {
		return nil, err
	}
	return keyring.Verifier(), nil
}

// The exposure pre-state every fixture is sealed against. The row limits below
// are DERIVED from these, not chosen: QueryReceiptV1 reproduces the limit
// arithmetic from the signed pre-state, so a fixture that picked its own limits
// would simply fail to validate.
const (
	ledgerRemainingRows  = 10
	ledgerInfluenceFacts = 4
)

// The derived row limits, exported because a test binding carried evidence to a
// fixture has to state the same numbers the signature does.
const (
	// VisibleRowLimit is the visible limit on an expanded-evidence path, bounded
	// by the pre-state's remaining rows.
	VisibleRowLimit = int64(ledgerRemainingRows)
	// CompanionRowLimit is the companion's policy limit: one more than its
	// evidence rows under expanded evidence, so a truncated companion result is
	// distinguishable from a complete one.
	CompanionRowLimit = int64(ledgerInfluenceFacts + 1)
	// SingleQueryVisibleRowLimit is the visible limit with no companion. Without
	// expanded evidence the pre-state bounds it by the influence facts instead.
	SingleQueryVisibleRowLimit = int64(ledgerInfluenceFacts)
)

// Target describes one signed target statement. The digests are supplied by the
// caller so a test can bind the fixture to statements its finalizer will
// independently reproduce.
//
// The row limit is deliberately not a member: it is derived from the signed
// pre-state, and letting a caller choose it would only produce receipts that
// fail to validate. A test that needs a different limit mutates one through
// Mutate, which is what gates 18 and 19 do.
type Target struct {
	ExactSQLSHA256              string
	StrictASTSHA256             string
	PolicyFingerprint           string
	PreparedTargetBindingSHA256 string
}

func (target Target) record(role querybinding.TargetRole, executed bool, rowLimit int64) querybinding.TargetRecordV1 {
	record := querybinding.TargetRecordV1{
		Role: role, Authorized: true, Executed: executed,
		ExactSQLSHA256: target.ExactSQLSHA256, StrictASTSHA256: target.StrictASTSHA256,
		RowLimit: rowLimit, PolicyFingerprint: target.PolicyFingerprint,
		PolicyRendererVersion:       policyRendererVersion,
		PolicyRendererDigest:        digest("policy-renderer"),
		PreparedTargetBindingSHA256: target.PreparedTargetBindingSHA256,
	}
	if record.PolicyFingerprint == "" {
		record.PolicyFingerprint = "fixture-" + string(role) + "-fingerprint"
	}
	if record.PreparedTargetBindingSHA256 == "" {
		record.PreparedTargetBindingSHA256 = digest("prepared-target-" + string(role))
	}
	return record
}

// Options selects which receipt to build.
type Options struct {
	// Visible is required. Companion is required for a paired-novel receipt and
	// optional for a semantic replay, which may authorize a companion in order
	// to derive its semantic key.
	Visible   Target
	Companion *Target
	// TaskID, QueryID and RequestID identify the settled request. Empty values
	// take fixture defaults.
	TaskID, QueryID, RequestID string
}

// unsignedBase is the receipt with every member the execution evidence does not
// supply, so that adding the binding yields a receipt that validates for the
// reason under test rather than for a missing member elsewhere.
func unsignedBase(options Options) (queryreceipt.QueryReceiptV1, error) {
	created := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	completed := created.Add(time.Millisecond)
	signedAt := completed.Add(time.Millisecond)
	common := digest("common")

	taskID, queryID, requestID := options.TaskID, options.QueryID, options.RequestID
	if taskID == "" {
		taskID = "task-fixture-1"
	}
	if queryID == "" {
		queryID = "query-fixture-1"
	}
	if requestID == "" {
		requestID = "request-fixture-1"
	}

	receipt := queryreceipt.QueryReceiptV1{
		Version: queryreceipt.Version, ReceiptID: queryID, TaskID: taskID,
		ResultDeliveryMode: queryreceipt.DeliveryArtifact,
		QueryID:            queryID, RequestID: requestID,
		ManifestDigest: common, GrantDigest: common, CatalogDigest: common,
		CatalogVersion: "catalog-v1", DatasourceID: "taskgate-fixture-expenses",
		SchemaDigest: common, RequestDigest: digest("request/" + requestID),
		// A fingerprint is an opaque policy audit token, not SQL.
		SQLFingerprint: "fixture-visible-fingerprint", PolicyDecision: "ALLOW",
		BudgetBefore: queryreceipt.BudgetStateV1{
			Limits: queryreceipt.BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100},
		},
		BudgetReserved: queryreceipt.BudgetVectorV1{Queries: 1, Rows: 5, DBMS: 50},
		BudgetCharged:  queryreceipt.BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		BudgetAfter: queryreceipt.BudgetStateV1{
			Limits: queryreceipt.BudgetVectorV1{Queries: 2, Rows: 10, DBMS: 100},
			Used:   queryreceipt.BudgetVectorV1{Queries: 1, Rows: 1, DBMS: 2},
		},
		RowCount: 1, DatabaseMS: 2, ResultHash: common, Status: "COMPLETED",
		CreatedAt: created, CompletedAt: completed, SignedAt: &signedAt,
		AuditSequence: 7, PreviousAuditHash: common, AuditHash: common,
		GatewayKeyID: KeyID,
		Exposure: &queryreceipt.ExposureEvidenceV1{
			RootTaskID: "task-root", RootEpoch: 7,
			ProfileVersion:            "taskgate-exposure-v5",
			ActualReleaseFacts:        3,
			ActualInfluenceFacts:      7,
			ChargedReleaseFacts:       2,
			ChargedInfluenceFacts:     5,
			ActualOutcomeFacts:        2,
			ChargedOutcomeFacts:       2,
			ObservationSHA256:         digest("observation"),
			DictionarySetSHA256:       digest("dictionary-set"),
			ReleaseSetSHA256:          digest("release-set"),
			InfluenceSetSHA256:        digest("influence-set"),
			OutcomeSetSHA256:          digest("outcome-set"),
			PredicateProfileVersion:   "taskgate-predicate-footprint-v1",
			PredicateContextSHA256:    digest("predicate-context"),
			PredicateSetSHA256:        digest("predicate-set"),
			ActualPredicateAtomCount:  1,
			ChargedPredicateAtomCount: 1,
			CompositeOutcomeSHA256:    digest("composite-outcome"),
			ActualCompositeCount:      1,
			ChargedCompositeCount:     1,
		},
	}

	expires := completed.Add(time.Hour)
	intent, err := queryreceipt.BuildArtifactIntent(queryreceipt.ArtifactIntentEvidenceV1{
		Version: queryreceipt.ArtifactIntentVersionV1, ResultID: "result-" + queryID,
		Format: "parquet", Encryption: "chunked-aes-gcm-v1", KeyID: "result-key-1",
		ParquetSHA256: receipt.ResultHash, ObjectSHA256: digest("object"),
		ParquetSize: 128, ObjectSize: 160, RowCount: receipt.RowCount, ColumnCount: 2,
		SchemaSHA256: digest("artifact-schema"), ResultMetadataSHA256: digest("result-metadata"),
		ACLSHA256: digest("acl"), ObjectKeySHA256: digest("object-key"),
		StagingKeySHA256: digest("staging-key"),
		ExpiresAt:        &expires, Status: queryreceipt.ArtifactStatusPending,
		RegistrationAuditSequence:     receipt.AuditSequence + 1,
		RegistrationPreviousAuditHash: receipt.AuditHash,
		RegistrationAuditHash:         digest("registration-audit"),
	})
	if err != nil {
		return queryreceipt.QueryReceiptV1{}, fmt.Errorf("build artifact intent: %w", err)
	}
	receipt.ArtifactIntent = &intent
	return receipt, nil
}

// build seals the pre-state and the execution binding onto a base receipt and
// signs it.
func build(options Options, pathKind querybinding.PathKind, executed bool) (queryreceipt.QueryReceiptV1, error) {
	var empty queryreceipt.QueryReceiptV1
	receipt, err := unsignedBase(options)
	if err != nil {
		return empty, err
	}

	// Expanded evidence settles against the companion, so it is exactly the
	// paths that bind one. The pre-state and the binding must agree, and the
	// receipt validator checks that they do.
	expanded := options.Companion != nil

	ledger, err := querybinding.ExposureLedgerBeforeV1{
		ProfileVersion: receipt.Exposure.ProfileVersion,
		RootTaskID:     receipt.Exposure.RootTaskID,
		RootEpoch:      receipt.Exposure.RootEpoch,
		Limits: querybinding.FactVector{ReleaseFacts: 500, InfluenceFacts: ledgerInfluenceFacts,
			OutcomeFacts: 10, PredicateAtoms: 25, CompositeOutcomes: 5},
		Used: querybinding.FactVector{ReleaseFacts: 100, InfluenceFacts: 0, OutcomeFacts: 2,
			PredicateAtoms: 5, CompositeOutcomes: 1},
		Remaining: querybinding.FactVector{ReleaseFacts: 400, InfluenceFacts: ledgerInfluenceFacts,
			OutcomeFacts: 8, PredicateAtoms: 20, CompositeOutcomes: 4},
		RemainingRows:        ledgerRemainingRows,
		UsesExpandedEvidence: expanded,
		HasExposureContext:   true,
	}.Seal()
	if err != nil {
		return empty, fmt.Errorf("seal exposure pre-state: %w", err)
	}
	receipt.ExposureLedgerBefore = &ledger

	budgetDigest, err := queryreceipt.BudgetStateSHA256(receipt.BudgetBefore)
	if err != nil {
		return empty, fmt.Errorf("budget digest: %w", err)
	}

	visibleLimit := VisibleRowLimit
	if !expanded {
		visibleLimit = SingleQueryVisibleRowLimit
	}
	visible := options.Visible.record(querybinding.RoleVisible, executed, visibleLimit)
	var companionRecord *querybinding.TargetRecordV1
	if options.Companion != nil {
		record := options.Companion.record(querybinding.RoleCompanion, executed, CompanionRowLimit)
		companionRecord = &record
	}
	// The preparation is sealed against the same prepared-target digests the
	// target records carry, because that is the cross-check the binding exists to
	// make: a fixture that sealed its own digests would produce a binding whose
	// statements were rendered from a preparation it does not describe.
	prepared, err := preparedOperation(visible, companionRecord, expanded)
	if err != nil {
		return empty, err
	}
	binding := querybinding.QueryExecutionBindingV2{
		PathKind:                   pathKind,
		PreparedOperation:          prepared,
		Compiler:                   compiler,
		ExposureProfileVersion:     ledger.ProfileVersion,
		VisibleRowLimit:            visible.RowLimit,
		BudgetBeforeSHA256:         budgetDigest,
		ExposureLedgerBeforeSHA256: ledger.SHA256,
		Visible:                    visible,
	}
	if companionRecord != nil {
		binding.Companion = companionRecord
		// Under expanded evidence the evidence rows are the pre-state's influence
		// facts and the policy limit is one more, so a truncated companion result
		// is distinguishable from a complete one.
		binding.CompanionEvidenceRows = ledgerInfluenceFacts
		binding.CompanionPolicyRows = companionRecord.RowLimit
	}
	sealed, err := binding.Seal()
	if err != nil {
		return empty, fmt.Errorf("seal execution binding: %w", err)
	}
	receipt.ExecutionBindingV2 = &sealed

	signer, err := Signer()
	if err != nil {
		return empty, err
	}
	signed, err := signer.Sign(receipt)
	if err != nil {
		return empty, fmt.Errorf("sign receipt: %w", err)
	}
	return signed, nil
}

// PairedNovel returns a signed paired-novel receipt: both targets authorized and
// executed.
func PairedNovel(options Options) (queryreceipt.QueryReceiptV1, error) {
	if options.Companion == nil {
		return queryreceipt.QueryReceiptV1{}, fmt.Errorf("paired_novel requires a companion target")
	}
	return build(options, querybinding.PathPairedNovel, true)
}

// SemanticReplay returns a signed semantic-replay receipt: targets authorized so
// the semantic key could be derived, and executed by nothing.
func SemanticReplay(options Options) (queryreceipt.QueryReceiptV1, error) {
	return build(options, querybinding.PathSemanticReplay, false)
}

// SingleQuery returns a signed single-query receipt: one executed visible
// statement and no companion.
func SingleQuery(options Options) (queryreceipt.QueryReceiptV1, error) {
	options.Companion = nil
	return build(options, querybinding.PathSingleQuery, true)
}

// PersistedJSON is the receipt document as the Control Store holds it.
//
// Gate 22 compares stored bytes rather than two decoded structs, so a fixture
// has to be able to produce the bytes. Marshaling here, once, is what makes
// "the persisted document" a single definite thing in a test.
func PersistedJSON(receipt queryreceipt.QueryReceiptV1) ([]byte, error) {
	return json.Marshal(receipt)
}

// Mutate returns a copy of the receipt with fn applied to its execution binding,
// re-sealed and re-signed.
//
// Re-sealing matters. A mutation that left the binding digest stale would be
// caught by QueryExecutionBindingV2.Validate for the wrong reason -- the test
// would pass while proving only that a digest check exists. Re-sealing produces
// a binding that is internally consistent and genuinely describes a different
// execution, which is what acceptance has to reject.
func Mutate(receipt queryreceipt.QueryReceiptV1,
	fn func(*querybinding.QueryExecutionBindingV2)) (queryreceipt.QueryReceiptV1, error) {
	var empty queryreceipt.QueryReceiptV1
	if receipt.ExecutionBindingV2 == nil {
		return empty, fmt.Errorf("receipt carries no execution binding to mutate")
	}
	binding := *receipt.ExecutionBindingV2
	if binding.Companion != nil {
		companion := *binding.Companion
		binding.Companion = &companion
	}
	fn(&binding)
	binding.SHA256 = ""
	sealed, err := binding.Seal()
	if err != nil {
		return empty, fmt.Errorf("re-seal mutated binding: %w", err)
	}
	mutated := receipt
	mutated.ExecutionBindingV2 = &sealed
	mutated.Signature = ""
	signer, err := Signer()
	if err != nil {
		return empty, err
	}
	signed, err := signer.Sign(mutated)
	if err != nil {
		return empty, fmt.Errorf("re-sign mutated receipt: %w", err)
	}
	return signed, nil
}

// PreparedTargetBinding is the prepared-target binding digest the fixture signs
// for a role when the caller supplies none. Acceptance compares the carried
// value with the signed one, so a test building carried evidence has to be able
// to state the same digest.
func PreparedTargetBinding(role querybinding.TargetRole) string {
	return digest("prepared-target-" + string(role))
}

// policyRendererVersion is the renderer every target record names. It has to
// equal the renderer the preparation was compiled against, or the binding's
// renderer cross-check refuses the fixture.
const policyRendererVersion = "sqlpolicy-v3"

// compiler is the typed compiler identity the preparation is sealed against and
// the binding carries. It is a package-level value rather than a constructor
// call per fixture so that every receipt this package builds names one compiler,
// which is what makes two fixtures comparable.
var compiler = mustSealCompiler()

func mustSealCompiler() preparedbinding.CompilerIdentityV1 {
	sealed, err := preparedbinding.CompilerIdentityV1{
		QueryPlanVersion: "queryplan-v7", QueryPlanSHA256: digest("query-plan-compiler"),
		PolicyRendererVersion: policyRendererVersion, PolicyRendererSHA256: digest("policy-renderer"),
	}.Seal()
	if err != nil {
		panic("queryreceiptv10: seal compiler identity: " + err.Error())
	}
	return sealed
}

// preparedOperation seals the preparation the two target records were rendered
// from.
//
// The prepared target digests are taken from the records rather than chosen
// here. The binding requires them to agree, and a caller that supplied its own
// target digests is describing statements its finalizer will independently
// reproduce -- so the preparation has to be the one that prepared those, not a
// fixture-shaped stand-in that happens to validate on its own.
func preparedOperation(visible querybinding.TargetRecordV1, companion *querybinding.TargetRecordV1,
	expanded bool) (preparedbinding.PreparedOperationBindingV1, error) {
	binding := preparedbinding.PreparedOperationBindingV1{
		HasCompanion: companion != nil, Grouped: true, ExpandedEvidence: expanded,
		VisibleFieldCount: 4, FactFieldCount: 2, ProvenanceFieldCount: 3,
		VisibleFieldsSHA256:      digest("visible-fields"),
		FactFieldsSHA256:         digest("fact-fields"),
		ProvenanceFieldsSHA256:   digest("provenance-fields"),
		PreparationInputsSHA256:  digest("preparation-inputs"),
		GrantSHA256:              digest("grant"),
		CatalogSHA256:            digest("catalog"),
		SnapshotBindingSetSHA256: digest("snapshot-binding-set"),
		PlanSHA256:               digest("plan"),
		CompilerIdentitySHA256:   compiler.SHA256,
		PolicyGrantSHA256:        digest("policy-grant"),
		NormalFormSHA256:         digest("normal-form"),
		OrdinalProgramSHA256:     digest("ordinal-program"),
		DictionarySetSHA256:      digest("dictionary-set"),
		SourcePublicationsSHA256: digest("source-publications"),
		PredicateFootprintSHA256: digest("predicate-footprint"),
		EstimatedBaseFacts:       4096,
		VisibleTargetSHA256:      visible.PreparedTargetBindingSHA256,
	}
	if companion != nil {
		binding.CompanionTargetSHA256 = companion.PreparedTargetBindingSHA256
	}
	sealed, err := binding.Seal()
	if err != nil {
		return preparedbinding.PreparedOperationBindingV1{}, fmt.Errorf("seal prepared operation: %w", err)
	}
	return sealed, nil
}
