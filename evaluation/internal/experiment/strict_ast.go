package experiment

import (
	"taskbound.local/agent-data-gateway/internal/sqlidentity"
)

// The strict normalized-AST digest is defined by internal/sqlidentity.
//
// It used to be defined here, which made it unreachable from the production tree
// -- Go's internal rule keeps evaluation/internal/... inside evaluation/ -- so
// the Gateway passed a nil digester to physicalquery.Derive and signed statement
// identities whose strict digest was empty. There is now exactly one
// implementation, and these aliases exist only so the evaluation call sites read
// as they did before.
//
// Do not reintroduce a second implementation here. The observer, the classifier
// manifest, the Adapter, the finalizer, the Attestation-footprint qualification
// and the production Gateway must all key on the same digest space.
const (
	StrictASTDomain        = sqlidentity.StrictASTDomain
	StrictASTSchemaVersion = sqlidentity.StrictASTSchemaVersion
	StrictASTParserModule  = sqlidentity.StrictASTParserModule
)

// StrictASTDigest is the structural identity of one SQL statement. See
// sqlidentity.StrictASTDigest.
func StrictASTDigest(sql string) (string, error) { return sqlidentity.StrictASTDigest(sql) }
