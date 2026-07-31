// Package viewcompiler expands a closed PostgreSQL view fragment into the
// existing trusted QueryPlan grammar. It is deliberately pure: callers must
// discover and attest an immutable RegistrySnapshot before compilation.
package viewcompiler

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"taskbound.local/agent-data-gateway/internal/queryplan"
)

const (
	MaxViewDepth       = 16
	MaxViewNodes       = 64
	MaxDependencyEdges = 128
	MaxPredicates      = 128
	MaxDefinitionBytes = 1 << 20
)

type RelationKind string

const (
	RelationBase RelationKind = "base"
	RelationView RelationKind = "view"
)

// RelationName is a resolved PostgreSQL relation identity. Schema may be
// empty only for synthetic/unit-test registries; discovered relations should
// always be schema-qualified.
type RelationName struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
}

func (name RelationName) String() string {
	if name.Schema == "" {
		return name.Name
	}
	return name.Schema + "." + name.Name
}

// Column is the ordered, attested public interface of a relation.
type Column struct {
	Name             string `json:"name"`
	SQLType          string `json:"sql_type"`
	Collation        string `json:"collation,omitempty"`
	CollationVersion string `json:"collation_version,omitempty"`
}

// Relation is one immutable registry record. View definitions are the exact
// pg_get_viewdef SELECT bytes and must carry their exact SHA-256. Base records
// are opaque governed leaves identified by ProductName.
type Relation struct {
	Name             RelationName   `json:"name"`
	Kind             RelationKind   `json:"kind"`
	ProductName      string         `json:"product_name,omitempty"`
	DefinitionSQL    string         `json:"definition_sql,omitempty"`
	DefinitionDigest string         `json:"definition_digest,omitempty"`
	Columns          []Column       `json:"columns"`
	Dependencies     []RelationName `json:"dependencies,omitempty"`
}

// RegistrySnapshot is collected atomically by the PostgreSQL discovery
// boundary. New clones it, so subsequent caller mutation cannot affect a
// Compiler.
type RegistrySnapshot struct {
	PostgreSQLMajorVersion int                       `json:"postgresql_major_version"`
	RevisionDigest         string                    `json:"revision_digest,omitempty"`
	Relations              map[RelationName]Relation `json:"relations"`
}

type OutputKind string

const (
	OutputField     OutputKind = "field"
	OutputAggregate OutputKind = "aggregate"
)

// Output records both the public view interface and its expanded semantic
// binding. FieldID and aggregate details are diagnostics; InterfaceDigest is
// computed only from the ordered public name/type/collation contract.
type Output struct {
	Name             string     `json:"name"`
	SQLType          string     `json:"sql_type"`
	Collation        string     `json:"collation,omitempty"`
	CollationVersion string     `json:"collation_version,omitempty"`
	Kind             OutputKind `json:"kind"`
	FieldID          string     `json:"field_id,omitempty"`
	Function         string     `json:"function,omitempty"`
	Argument         string     `json:"argument,omitempty"`
}

// DependencyRef is one deterministic member of the reachable registry
// closure. The root is included and marked explicitly.
type DependencyRef struct {
	Name             RelationName   `json:"name"`
	Kind             RelationKind   `json:"kind"`
	ProductName      string         `json:"product_name,omitempty"`
	DefinitionDigest string         `json:"definition_digest,omitempty"`
	Dependencies     []RelationName `json:"dependencies,omitempty"`
	Root             bool           `json:"root,omitempty"`
}

type Artifact struct {
	Root                RelationName        `json:"root"`
	Plan                queryplan.QueryPlan `json:"plan"`
	Outputs             []Output            `json:"outputs"`
	BaseProducts        []string            `json:"base_products"`
	DependencyClosure   []DependencyRef     `json:"dependency_closure"`
	DefinitionDigest    string              `json:"definition_digest"`
	DependencyDigest    string              `json:"dependency_digest"`
	InterfaceDigest     string              `json:"interface_digest"`
	CanonicalPlanDigest string              `json:"canonical_plan_digest"`
	BindingDigest       string              `json:"binding_digest"`
}

type ErrorCode string

const (
	CodeInvalidRegistry          ErrorCode = "VIEW_REGISTRY_INVALID"
	CodeRelationNotFound         ErrorCode = "VIEW_RELATION_NOT_FOUND"
	CodeDefinitionDigestMismatch ErrorCode = "VIEW_DEFINITION_DIGEST_MISMATCH"
	CodeDefinitionUnsupported    ErrorCode = "VIEW_DEFINITION_UNSUPPORTED"
	CodeDependencyMismatch       ErrorCode = "VIEW_DEPENDENCY_MISMATCH"
	CodeSchemaMismatch           ErrorCode = "VIEW_OUTPUT_SCHEMA_MISMATCH"
	CodeCycle                    ErrorCode = "VIEW_CYCLE"
	CodeDepthLimit               ErrorCode = "VIEW_DEPTH_LIMIT_EXCEEDED"
	CodeNodeLimit                ErrorCode = "VIEW_NODE_LIMIT_EXCEEDED"
	CodeEdgeLimit                ErrorCode = "VIEW_EDGE_LIMIT_EXCEEDED"
	CodeDefinitionBytesLimit     ErrorCode = "VIEW_DEFINITION_BYTES_EXCEEDED"
	CodeSourceLimit              ErrorCode = "VIEW_SOURCE_LIMIT_EXCEEDED"
	CodeStableRoleCollision      ErrorCode = "VIEW_STABLE_ROLE_COLLISION"
	CodeAggregationBarrier       ErrorCode = "VIEW_AGGREGATION_BARRIER"
	CodePlanInvalid              ErrorCode = "VIEW_PLAN_INVALID"
)

type Error struct {
	Code     ErrorCode    `json:"code"`
	Relation RelationName `json:"relation,omitempty"`
	Message  string       `json:"message"`
}

func (err *Error) Error() string {
	if err == nil {
		return "<nil>"
	}
	if err.Relation.Name == "" {
		return fmt.Sprintf("%s: %s", err.Code, err.Message)
	}
	return fmt.Sprintf("%s (%s): %s", err.Code, err.Relation, err.Message)
}

func reject(code ErrorCode, relation RelationName, format string, args ...any) *Error {
	return &Error{Code: code, Relation: relation, Message: fmt.Sprintf(format, args...)}
}

// ExactDefinitionDigest hashes the exact bytes returned by pg_get_viewdef.
// It intentionally performs no whitespace or SQL-literal normalization.
func ExactDefinitionDigest(definition string) string {
	digest := sha256.Sum256([]byte(definition))
	return hex.EncodeToString(digest[:])
}
