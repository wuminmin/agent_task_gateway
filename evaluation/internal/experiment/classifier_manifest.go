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
const ClassifierManifestVersion = "taskgate-final-v5-observer-classifier-manifest-v1"

// classifierManifestDomain domain-separates the manifest digest.
const classifierManifestDomain = "TASKGATE-FINAL-V5-OBSERVER-CLASSIFIER-MANIFEST-V1"

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
	Version string            `json:"version"`
	Entries []ClassifierEntry `json:"entries"`
}

// requiredManifestClasses are the classes every operation-scoped manifest must
// define. Target classes are deliberately absent: a semantic or idempotent
// replay legitimately has none, so they are added per operation.
func requiredManifestClasses() []GatewayStatementClassV3 {
	return []GatewayStatementClassV3{
		V3TransactionBegin, V3TransactionCommit,
		V3SafetySessionPin, V3RepresentationPin, V3StatementTimeoutPin,
		V3DatasourceIdentity, V3ViewColumnAttestation, V3ViewDefinitionAttest,
		V3NestedViewdefRewrite,
	}
}

// Validate rejects a manifest that cannot classify deterministically.
func (manifest ClassifierManifest) Validate() error {
	if manifest.Version != ClassifierManifestVersion {
		return fmt.Errorf("classifier manifest version %q is unsupported", manifest.Version)
	}
	if len(manifest.Entries) == 0 {
		return errors.New("classifier manifest is empty")
	}
	known := map[GatewayStatementClassV3]bool{}
	for _, class := range GatewayStatementClassesV3() {
		known[class] = true
	}
	seen := map[classifierKey]GatewayStatementClassV3{}
	defined := map[GatewayStatementClassV3]bool{}
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
		defined[entry.Class] = true

		switch entry.SourceKind {
		case SourceConnectorConstant:
			if !validSHA256(entry.SourceSHA256) {
				return fmt.Errorf("class %s is pinned to Connector source but carries no source digest", entry.Class)
			}
			if entry.PostgreSQLVersionNum != 0 || entry.ContractIdentity != "" {
				return fmt.Errorf("class %s carries an identity that does not belong to a Connector constant", entry.Class)
			}
		case SourceRuntimeTemplate:
			if entry.PostgreSQLVersionNum == 0 {
				return fmt.Errorf("class %s is a runtime template but is bound to no PostgreSQL version", entry.Class)
			}
			if entry.SourceSHA256 != "" || entry.ContractIdentity != "" {
				return fmt.Errorf("class %s carries an identity that does not belong to a runtime template", entry.Class)
			}
		case SourceQueryContract:
			if entry.ContractIdentity == "" {
				return fmt.Errorf("class %s is a target statement but names no frozen contract", entry.Class)
			}
			if entry.SourceSHA256 != "" || entry.PostgreSQLVersionNum != 0 {
				return fmt.Errorf("class %s carries an identity that does not belong to a query contract", entry.Class)
			}
		default:
			return fmt.Errorf("class %s has unknown source kind %q", entry.Class, entry.SourceKind)
		}
	}
	for _, class := range requiredManifestClasses() {
		if !defined[class] {
			return fmt.Errorf("classifier manifest defines no entry for required class %s", class)
		}
	}
	if !sort.SliceIsSorted(manifest.Entries, func(left, right int) bool {
		return manifestLess(manifest.Entries[left], manifest.Entries[right])
	}) {
		return errors.New("classifier manifest entries are not in canonical order")
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
// Connector constants and the confirmed PostgreSQL 16.14 runtime templates.
//
// Nothing here retypes SQL: every connector_constant entry hashes the same
// exported constant the Connector executes, so a production edit changes the
// manifest digest and invalidates prior evidence.
func controlManifestEntries() ([]ClassifierEntry, error) {
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
	entries := make([]ClassifierEntry, 0, len(constants)+3)
	for _, constant := range constants {
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
		// PostgreSQL performs this inside pg_get_viewdef. It is only counted
		// under track=all, and only ever as a nested statement.
		{V3NestedViewdefRewrite, runtimeNestedRewriteTemplate, false},
	}
	for _, template := range runtime {
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
const (
	runtimeBeginTemplate         = `begin isolation level repeatable read read only`
	runtimeCommitTemplate        = `commit`
	runtimeNestedRewriteTemplate = `SELECT * FROM pg_catalog.pg_rewrite WHERE ev_class = $1 AND rulename = $2`
)

// BuildClassifierManifest assembles the manifest for one operation: the shared
// controls plus exactly the target identities that operation's frozen contract
// renders. Targets are per operation on purpose -- a global manifest listing
// every workload's allowed targets would let a missing legitimate target be
// replaced by another workload's, with no change to any class count.
func BuildClassifierManifest(targets []ClassifierEntry) (ClassifierManifest, error) {
	entries, err := controlManifestEntries()
	if err != nil {
		return ClassifierManifest{}, err
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
	manifest := ClassifierManifest{Version: ClassifierManifestVersion, Entries: entries}
	if err := manifest.Validate(); err != nil {
		return ClassifierManifest{}, err
	}
	return manifest, nil
}

// TargetEntry builds one target identity from the exact rendered SQL the frozen
// contract produced for this operation.
func TargetEntry(class GatewayStatementClassV3, renderedSQL, contractIdentity string) (ClassifierEntry, error) {
	if class != V3TargetedVisible && class != V3TargetedCompanion {
		return ClassifierEntry{}, fmt.Errorf("class %s is not a target statement", class)
	}
	if contractIdentity == "" {
		return ClassifierEntry{}, errors.New("a target identity must name its frozen contract")
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
