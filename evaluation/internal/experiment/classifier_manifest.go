package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"taskbound.local/agent-data-gateway/internal/approval"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

// ClassifierManifestVersion identifies the manifest schema.
//
// Version 2 makes the closed world path-aware. Version 1 carried no path kind
// and required every manifest to define a fixed universal class set, which
// conflated "every class the Gateway can ever produce" with "every class THIS
// execution path may produce". That was not a safety rule: it made an exact
// idempotent replay -- which reaches Business PostgreSQL not at all -- unable to
// have a manifest, because the required set included
// postgresql_internal_attestation whose only source is a qualified footprint the
// non-attesting operation is forbidden to name.
//
// Under v2 a manifest declares exactly the structures its path can legitimately
// produce with an expected multiplicity above zero, and nothing else. For an
// idempotent replay that set is empty, and an empty entry list is the strongest
// contract available: every Business statement in the window is unclassified,
// becomes V3Unexpected, and fails the all-zero plan.
const ClassifierManifestVersion = "taskgate-final-v5-observer-classifier-manifest-v2"

// classifierManifestVersionV1 is the superseded schema. It is named so a v1
// document is rejected with the reason rather than as an unrecognised string:
// v1 manifests remain valid historical development evidence and must not be
// silently reinterpreted under the v2 rules, because the class set they carry
// means something else.
const classifierManifestVersionV1 = "taskgate-final-v5-observer-classifier-manifest-v1"

// classifierManifestDomain domain-separates the manifest digest.
const classifierManifestDomain = "TASKGATE-FINAL-V5-OBSERVER-CLASSIFIER-MANIFEST-V2"

// ClassifierSourceKind records where an entry's identity came from, so a
// reviewer can tell a statement pinned to Connector source bytes from one bound
// only to observed server behaviour.
type ClassifierSourceKind string

const (
	// SourceConnectorConstant is a statement whose exact bytes are a
	// source-controlled Connector constant. Its SourceSHA256 is the digest of
	// those bytes, which is what pins the constants the strict AST digest
	// deliberately ignores.
	SourceConnectorConstant ClassifierSourceKind = "connector_constant"
	// SourceRuntimeTemplate is a statement neither the Connector nor the
	// evaluation writes: the driver's transaction control, or a lookup
	// PostgreSQL performs internally. It has no source bytes to pin, so it is
	// bound to a PostgreSQL version instead and must be confirmed live.
	SourceRuntimeTemplate ClassifierSourceKind = "runtime_template"
	// SourceQueryContract is a target statement whose identity comes from the
	// frozen rendered query contract for one operation.
	SourceQueryContract ClassifierSourceKind = "query_contract"
	// SourceQualifiedFootprint is a PostgreSQL-internal statement whose identity
	// was measured, not written. There is no TaskGate source to pin and no
	// portable template to assert: the shape is an implementation detail of one
	// server build, so it is bound to the qualification run that measured it.
	//
	// The previous design hard-coded one pg_rewrite lookup as a runtime
	// template. That asserted a particular internal statement as a universal
	// constant on the strength of a single deployment, which is the same
	// unjustified generalization the footprint contract exists to retire.
	SourceQualifiedFootprint ClassifierSourceKind = "qualified_footprint"
)

// ClassifierEntry maps one structural key to exactly one class.
type ClassifierEntry struct {
	Class GatewayStatementClassV3 `json:"class"`
	// StrictASTSHA256 is the observable identity: what the classifier compares
	// against text pg_stat_statements returned.
	StrictASTSHA256 string `json:"strict_ast_sha256"`
	// RequiredTopLevel is part of the key. The nested pg_rewrite lookup is only
	// itself when PostgreSQL recorded it as nested; the same shape at top level
	// is a different statement and must not match.
	RequiredTopLevel bool                 `json:"required_toplevel"`
	SourceKind       ClassifierSourceKind `json:"source_kind"`
	// SourceSHA256 is the digest of the exact production source bytes, empty
	// for runtime templates. It carries the constants the AST digest drops.
	SourceSHA256 string `json:"source_sha256,omitempty"`
	// ContractIdentity names the frozen contract a target statement came from,
	// empty otherwise.
	ContractIdentity string `json:"contract_identity,omitempty"`
	// PostgreSQLVersionNum binds a runtime template to the server that emits
	// it, empty for source-pinned statements.
	PostgreSQLVersionNum int64 `json:"postgresql_version_num,omitempty"`
	// FootprintSHA256 names the qualification that measured a PostgreSQL-internal
	// statement, empty otherwise.
	FootprintSHA256 string `json:"footprint_sha256,omitempty"`
}

// classifierKey is the classification key: structural identity plus toplevel.
type classifierKey struct {
	digest   string
	topLevel bool
}

// ClassifierManifest is the closed set of statements one operation may produce.
//
// It carries no SQL. The strict AST digest is the observable identity and the
// source digest pins the bytes behind it; neither reveals the statement.
type ClassifierManifest struct {
	Version string `json:"version"`
	// PathKind is the execution path whose closed world this manifest describes.
	//
	// It is a member rather than an argument because the class set is now
	// path-specific: the same entry list means a different contract under a
	// different path. Carrying it makes the manifest digest move with the path,
	// and lets compilation require that the operation, the independently derived
	// plan and the manifest all name the same one.
	PathKind GatewayPathKind   `json:"path_kind"`
	Entries  []ClassifierEntry `json:"entries"`
}

// Validate rejects a manifest that cannot classify deterministically.
//
// It is deliberately structural. Which classes a manifest MUST declare is not
// its own to say -- that would be the Adapter declaring the standard it is held
// to -- so it is settled by the independently derived GatewayControlPlanV3 in
// requireClassSet, which compilation applies. What is checked here is only what
// a manifest can be wrong about on its own terms: an unknown class, an
// ambiguous key, an identity that does not belong to its source kind, an order
// that would make the digest depend on assembly.
func (manifest ClassifierManifest) Validate() error {
	if manifest.Version == classifierManifestVersionV1 {
		return fmt.Errorf("classifier manifest is %s, which required a universal class set and carried no path kind; "+
			"v1.5 acceptance requires %s and a v1 document must be rebuilt rather than reinterpreted",
			classifierManifestVersionV1, ClassifierManifestVersion)
	}
	if manifest.Version != ClassifierManifestVersion {
		return fmt.Errorf("classifier manifest version %q is unsupported", manifest.Version)
	}
	dimensions, knownPath := dimensionsFor(manifest.PathKind)
	if !knownPath {
		return fmt.Errorf("classifier manifest path_kind %q is not a derivable execution path", manifest.PathKind)
	}
	// An empty entry list is a legitimate contract, but only where the path
	// reaches Business PostgreSQL not at all. Anywhere else it would mean every
	// statement the path certainly executes is unclassified, which no plan can
	// accept and which would otherwise fail far from its cause.
	if len(manifest.Entries) == 0 && !manifest.pathIsSilent(dimensions) {
		return fmt.Errorf("classifier manifest for path_kind %s declares no statement, "+
			"but that path reaches Business PostgreSQL", manifest.PathKind)
	}
	known := map[GatewayStatementClassV3]bool{}
	for _, class := range GatewayStatementClassesV3() {
		known[class] = true
	}
	seen := map[classifierKey]GatewayStatementClassV3{}
	for index, entry := range manifest.Entries {
		if !known[entry.Class] {
			return fmt.Errorf("classifier manifest entry %d names unknown class %q", index, entry.Class)
		}
		if entry.Class == V3Unexpected {
			return errors.New("classifier manifest must not define the unexpected class; it is the absence of a match")
		}
		if !validSHA256(entry.StrictASTSHA256) {
			return fmt.Errorf("classifier manifest entry for %s has no strict AST SHA-256", entry.Class)
		}
		key := classifierKey{digest: entry.StrictASTSHA256, topLevel: entry.RequiredTopLevel}
		if other, duplicate := seen[key]; duplicate {
			if other == entry.Class {
				return fmt.Errorf("classifier manifest defines class %s twice for one key", entry.Class)
			}
			// One key resolving to two classes makes classification ambiguous,
			// so the manifest itself is invalid rather than the run merely
			// failing on the ambiguous statement.
			return fmt.Errorf("classifier manifest maps one key to both %s and %s", other, entry.Class)
		}
		seen[key] = entry.Class

		switch entry.SourceKind {
		case SourceConnectorConstant:
			if !validSHA256(entry.SourceSHA256) {
				return fmt.Errorf("class %s is pinned to Connector source but carries no source digest", entry.Class)
			}
			if entry.PostgreSQLVersionNum != 0 || entry.ContractIdentity != "" || entry.FootprintSHA256 != "" {
				return fmt.Errorf("class %s carries an identity that does not belong to a Connector constant", entry.Class)
			}
		case SourceRuntimeTemplate:
			if entry.PostgreSQLVersionNum == 0 {
				return fmt.Errorf("class %s is a runtime template but is bound to no PostgreSQL version", entry.Class)
			}
			if entry.SourceSHA256 != "" || entry.ContractIdentity != "" || entry.FootprintSHA256 != "" {
				return fmt.Errorf("class %s carries an identity that does not belong to a runtime template", entry.Class)
			}
		case SourceQueryContract:
			if entry.ContractIdentity == "" {
				return fmt.Errorf("class %s is a target statement but names no frozen contract", entry.Class)
			}
			if entry.SourceSHA256 != "" || entry.PostgreSQLVersionNum != 0 || entry.FootprintSHA256 != "" {
				return fmt.Errorf("class %s carries an identity that does not belong to a query contract", entry.Class)
			}
		case SourceQualifiedFootprint:
			if entry.Class != V3PostgreSQLInternalAttestation {
				return fmt.Errorf("class %s is not a PostgreSQL-internal statement and must not be bound to a footprint", entry.Class)
			}
			if !validSHA256(entry.FootprintSHA256) {
				return fmt.Errorf("class %s is measured but names no qualification footprint", entry.Class)
			}
			// PostgreSQL-internal Attestation statements are nested by
			// definition; only track=all records them at all.
			if entry.RequiredTopLevel {
				return fmt.Errorf("class %s requires toplevel, but PostgreSQL-internal statements are nested", entry.Class)
			}
			if entry.SourceSHA256 != "" || entry.ContractIdentity != "" || entry.PostgreSQLVersionNum != 0 {
				return fmt.Errorf("class %s carries an identity that does not belong to a measured statement", entry.Class)
			}
		default:
			return fmt.Errorf("class %s has unknown source kind %q", entry.Class, entry.SourceKind)
		}
	}
	if !sort.SliceIsSorted(manifest.Entries, func(left, right int) bool {
		return manifestLess(manifest.Entries[left], manifest.Entries[right])
	}) {
		return errors.New("classifier manifest entries are not in canonical order")
	}
	return nil
}

// pathIsSilent reports whether the path reaches Business PostgreSQL not at all.
// It is read off the shared dimension table rather than by naming
// idempotent_replay, so a later path with the same property gets the same
// treatment without an edit here.
func (manifest ClassifierManifest) pathIsSilent(dimensions pathDimensions) bool {
	return dimensions.preflight == 0 && dimensions.single == 0 && dimensions.paired == 0 &&
		dimensions.visible == 0 && dimensions.companion == 0
}

// requireClassSet is the v2 rule, and it is derived entirely from the plan.
//
//	plan.Expected()[class] > 0  -- the manifest must declare that class exactly
//	plan.Expected()[class] == 0 -- the manifest must not declare it at all
//
// The second half is what version 1 had backwards. A manifest listing a class
// the path cannot produce is not harmless surplus: it makes that statement
// classifiable, so a control statement appearing where none should would be
// counted as a known class rather than landing in V3Unexpected. Forbidding it
// is what makes an idempotent replay's empty manifest a contract -- every
// Business statement in the window is unclassified by construction.
//
// The internal class is compared key by key rather than by presence, because it
// is the one class whose keys are measured rather than derived; and targets are
// compared by cardinality, because a path's target count is what distinguishes
// the four paths from each other.
func (manifest ClassifierManifest) requireClassSet(plan GatewayControlPlanV3) error {
	expected := plan.Expected()
	byClass := map[GatewayStatementClassV3][]ClassifierEntry{}
	for _, entry := range manifest.Entries {
		byClass[entry.Class] = append(byClass[entry.Class], entry)
	}
	for _, class := range GatewayStatementClassesV3() {
		switch class {
		case V3Unexpected, V3PostgreSQLInternalAttestation:
			// The sink is never declared -- Validate rejects it outright -- and
			// the internal class is settled key by key below.
			continue
		case V3TargetedVisible, V3TargetedCompanion:
			// The plan's target expectation IS the cardinality: one target
			// statement executes once.
			if got, want := int64(len(byClass[class])), expected[class]; got != want {
				return fmt.Errorf("path_kind %s expects %d %s statement(s), the manifest declares %d",
					plan.PathKind, want, class, got)
			}
		default:
			// Every control class is realized by exactly one Connector constant
			// or one runtime template, so its key count is one when the plan
			// expects it and zero when it does not. The multiplicity itself is
			// the plan's to check, not the manifest's to declare.
			want := 0
			if expected[class] > 0 {
				want = 1
			}
			if got := len(byClass[class]); got != want {
				if want == 0 {
					return fmt.Errorf("path_kind %s executes no %s, but the manifest declares %d entr(ies) for it; "+
						"a class the path cannot produce must stay unclassifiable",
						plan.PathKind, class, got)
				}
				return fmt.Errorf("path_kind %s expects %s but the manifest declares %d entr(ies) for it",
					plan.PathKind, class, got)
			}
		}
	}

	// PostgreSQL-internal keys: exactly the plan's expectation, key for key,
	// each bound to the qualification the plan was derived under. A superset
	// would make an internal statement the plan does not expect classifiable; a
	// subset would leave a qualified one unclassified.
	planKeys := map[string]bool{}
	for _, entry := range plan.InternalExpectation {
		planKeys[entry.StrictASTSHA256] = true
	}
	manifestKeys := map[string]bool{}
	for _, entry := range byClass[V3PostgreSQLInternalAttestation] {
		if entry.FootprintSHA256 != plan.AttestationFootprintSHA256 {
			return fmt.Errorf("internal key %s is bound to qualification %s, the plan was derived under %s",
				shortDigest(entry.StrictASTSHA256), shortDigest(entry.FootprintSHA256),
				shortDigest(plan.AttestationFootprintSHA256))
		}
		manifestKeys[entry.StrictASTSHA256] = true
	}
	for key := range planKeys {
		if !manifestKeys[key] {
			return fmt.Errorf("the plan expects PostgreSQL-internal key %s, which the manifest does not declare",
				shortDigest(key))
		}
	}
	for key := range manifestKeys {
		if !planKeys[key] {
			return fmt.Errorf("the manifest declares PostgreSQL-internal key %s, which the plan does not expect",
				shortDigest(key))
		}
	}
	return nil
}

func manifestLess(left, right ClassifierEntry) bool {
	if left.Class != right.Class {
		return left.Class < right.Class
	}
	if left.StrictASTSHA256 != right.StrictASTSHA256 {
		return left.StrictASTSHA256 < right.StrictASTSHA256
	}
	return !left.RequiredTopLevel && right.RequiredTopLevel
}

// SHA256 is the manifest's canonical domain-separated digest. It enters the
// Sample and the sealed evidence, and the finalizer recomputes it.
func (manifest ClassifierManifest) SHA256() (string, error) {
	if err := manifest.Validate(); err != nil {
		return "", err
	}
	canonical, err := approval.CanonicalJSON(manifest)
	if err != nil {
		return "", fmt.Errorf("canonicalize classifier manifest: %w", err)
	}
	hash := sha256.New()
	hash.Write([]byte(classifierManifestDomain + "\x00"))
	hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Classify resolves one observed statement to its class. A key the manifest does
// not define is V3Unexpected -- the closed world's sink, which no plan expects.
func (manifest ClassifierManifest) Classify(strictASTSHA256 string, topLevel bool) GatewayStatementClassV3 {
	for _, entry := range manifest.Entries {
		if entry.StrictASTSHA256 == strictASTSHA256 && entry.RequiredTopLevel == topLevel {
			return entry.Class
		}
	}
	return V3Unexpected
}

// sourceDigest is the digest of exact production source bytes.
func sourceDigest(sql string) string {
	sum := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(sum[:])
}

// controlManifestEntries builds the control half of a manifest from the exact
// Connector constants and the confirmed PostgreSQL 16.14 runtime templates,
// emitting only the classes the supplied expectation puts above zero.
//
// Nothing here retypes SQL: every connector_constant entry hashes the same
// exported constant the Connector executes, so a production edit changes the
// manifest digest and invalidates prior evidence.
//
// The filter is the whole of the v2 change on this side. A single-query
// operation issues no representation pin and a semantic replay opens no
// transaction, so neither may be able to classify one: not listing them is what
// sends such a statement to V3Unexpected instead of to a class that happens to
// be expected zero times.
func controlManifestEntries(expected map[GatewayStatementClassV3]int64) ([]ClassifierEntry, error) {
	required := RequiredMeasurementEnvironment()
	constants := []struct {
		class GatewayStatementClassV3
		sql   string
	}{
		{V3SafetySessionPin, dataconnector.SafetySessionPinSQL},
		{V3RepresentationPin, dataconnector.RepresentationPinSQL},
		{V3StatementTimeoutPin, dataconnector.StatementTimeoutPinSQL},
		{V3DatasourceIdentity, dataconnector.DatasourceIdentitySQL},
		{V3ViewColumnAttestation, dataconnector.ViewColumnAttestationSQL},
		{V3ViewDefinitionAttest, dataconnector.ViewDefinitionAttestationSQL},
	}
	entries := make([]ClassifierEntry, 0, len(constants)+2)
	for _, constant := range constants {
		if expected[constant.class] == 0 {
			continue
		}
		digest, err := StrictASTDigest(constant.sql)
		if err != nil {
			return nil, fmt.Errorf("strict AST digest for %s: %w", constant.class, err)
		}
		entries = append(entries, ClassifierEntry{
			Class: constant.class, StrictASTSHA256: digest, RequiredTopLevel: true,
			SourceKind: SourceConnectorConstant, SourceSHA256: sourceDigest(constant.sql),
		})
	}
	runtime := []struct {
		class    GatewayStatementClassV3
		sql      string
		topLevel bool
	}{
		{V3TransactionBegin, runtimeBeginTemplate, true},
		{V3TransactionCommit, runtimeCommitTemplate, true},
	}
	for _, template := range runtime {
		if expected[template.class] == 0 {
			continue
		}
		digest, err := StrictASTDigest(template.sql)
		if err != nil {
			return nil, fmt.Errorf("strict AST digest for %s: %w", template.class, err)
		}
		entries = append(entries, ClassifierEntry{
			Class: template.class, StrictASTSHA256: digest, RequiredTopLevel: template.topLevel,
			SourceKind: SourceRuntimeTemplate, PostgreSQLVersionNum: required.PostgreSQLVersionNum,
		})
	}
	return entries, nil
}

// Runtime templates, bound to PostgreSQL 16.14 and confirmed live by
// TestRuntimeTemplateDigestsAreStableOnLivePostgreSQL. These are what pgx and
// the server emit; there is no Connector constant to pin them to.
//
// The PostgreSQL-internal Attestation statements are deliberately NOT here.
// They are measured per qualification and enter the manifest from the footprint,
// because their shape is a property of one server build rather than a portable
// template.
const (
	runtimeBeginTemplate  = `begin isolation level repeatable read read only`
	runtimeCommitTemplate = `commit`
)

// BuildClassifierManifestV2 assembles the manifest one execution path is
// entitled to, from the independently derived plan.
//
// The plan settles the class set, so the manifest cannot declare what it is
// required to contain. Targets stay per operation on purpose -- a global
// manifest listing every workload's allowed targets would let a missing
// legitimate target be replaced by another workload's, with no change to any
// class count. The PostgreSQL-internal keys come from the qualified footprint,
// re-derived here rather than copied from the plan, so the manifest can classify
// exactly the internal statements that qualification measured for these scopes
// and nothing else.
//
// The footprint is a pointer because its absence is meaningful. A path that
// performs no Attestation must supply none: a zero-valued footprint would be a
// document claiming a qualification that never happened, and passing one by
// value made "no footprint" and "an empty footprint" the same argument.
func BuildClassifierManifestV2(plan GatewayControlPlanV3, footprint *AttestationFootprintV2,
	targets []ClassifierEntry) (ClassifierManifest, error) {
	if err := plan.Validate(); err != nil {
		return ClassifierManifest{}, fmt.Errorf("control plan: %w", err)
	}
	dimensions, _ := dimensionsFor(plan.PathKind)
	expected := plan.Expected()

	entries, err := controlManifestEntries(expected)
	if err != nil {
		return ClassifierManifest{}, err
	}

	switch {
	case dimensions.requiresSchema:
		if footprint == nil {
			return ClassifierManifest{}, fmt.Errorf(
				"path_kind %s attests, so its manifest cannot be built without the qualified footprint "+
					"its PostgreSQL-internal keys were measured by", plan.PathKind)
		}
		footprintDigest, err := footprint.SHA256()
		if err != nil {
			return ClassifierManifest{}, fmt.Errorf("qualified footprint: %w", err)
		}
		if footprintDigest != plan.AttestationFootprintSHA256 {
			return ClassifierManifest{}, fmt.Errorf(
				"the plan was derived under Attestation footprint %s, the supplied qualification is %s",
				shortDigest(plan.AttestationFootprintSHA256), shortDigest(footprintDigest))
		}
		// Re-derived from the measurement rather than read off the plan, then
		// compared: the plan carries the expectation as a claim, and a manifest
		// built from that claim would agree with it by construction.
		derived, err := footprint.InternalExpectation(plan.PreflightAttestationPasses,
			plan.SingleQueryTransactions, plan.PairedQueryTransactions)
		if err != nil {
			return ClassifierManifest{}, err
		}
		if err := requireSameInternalExpectation(plan.InternalExpectation, derived); err != nil {
			return ClassifierManifest{}, fmt.Errorf("PostgreSQL-internal expectation: %w", err)
		}
		for _, entry := range derived {
			entries = append(entries, ClassifierEntry{
				Class: V3PostgreSQLInternalAttestation, StrictASTSHA256: entry.StrictASTSHA256,
				// Always nested: the same shape at top level is a different
				// statement and must not satisfy this class.
				RequiredTopLevel: false, SourceKind: SourceQualifiedFootprint,
				FootprintSHA256: footprintDigest,
			})
		}
	case footprint != nil:
		return ClassifierManifest{}, fmt.Errorf(
			"path_kind %s performs no Attestation, so its manifest must carry no qualified footprint",
			plan.PathKind)
	}

	for _, target := range targets {
		if target.Class != V3TargetedVisible && target.Class != V3TargetedCompanion {
			return ClassifierManifest{}, fmt.Errorf("class %s is not a target statement", target.Class)
		}
		if target.SourceKind != SourceQueryContract {
			return ClassifierManifest{}, fmt.Errorf("target class %s must be bound to a frozen query contract", target.Class)
		}
		entries = append(entries, target)
	}
	sort.Slice(entries, func(left, right int) bool { return manifestLess(entries[left], entries[right]) })
	manifest := ClassifierManifest{
		Version: ClassifierManifestVersion, PathKind: plan.PathKind, Entries: entries,
	}
	if err := manifest.Validate(); err != nil {
		return ClassifierManifest{}, err
	}
	if err := manifest.requireClassSet(plan); err != nil {
		return ClassifierManifest{}, err
	}
	return manifest, nil
}

// TargetEntry builds one target identity from the exact rendered SQL the frozen
// contract produced for this operation.
// The contract identity is DERIVED from the operation's, not declared. A
// free-form per-target string let a manifest name any contract it liked, so a
// target belonging to another workload could be introduced without any class
// count changing. Deriving it means the only way to change what a target claims
// is to change the operation it is compiled for.
func TargetEntry(class GatewayStatementClassV3, renderedSQL, operationContractIdentity string) (ClassifierEntry, error) {
	contractIdentity, err := TargetContractIdentity(operationContractIdentity, class)
	if err != nil {
		return ClassifierEntry{}, err
	}
	digest, err := StrictASTDigest(renderedSQL)
	if err != nil {
		return ClassifierEntry{}, fmt.Errorf("strict AST digest for %s: %w", class, err)
	}
	return ClassifierEntry{
		Class: class, StrictASTSHA256: digest, RequiredTopLevel: true,
		SourceKind: SourceQueryContract, ContractIdentity: contractIdentity,
	}, nil
}
