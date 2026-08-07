package gateway

import (
	"crypto/sha256"
	"encoding/hex"

	"taskbound.local/agent-data-gateway/internal/ordinal"
	"taskbound.local/agent-data-gateway/internal/queryplan"
	"taskbound.local/agent-data-gateway/internal/sqlpolicy"
)

// boundOrdinalExecution is the ordinal half of one prepared operation as the
// Gateway holds it.
//
// Everything here except Indexes is read off the sealed preparation: the
// program, the companion statement that joins the pinned sidecars, the
// Catalog-wide dictionary universe, the sidecar grants and the working-set
// estimate are all produced by internal/physicalquery, from values the finalizer
// can obtain for itself.
//
// Indexes is the exception, and the reason this type still exists. A
// SnapshotIndex is a live handle onto a compiled artifact in this process's
// verified registry; it cannot cross into preparation, and the finalizer never
// holds one. The preparation records which Publication each source alias
// resolved to, and the Gateway reattaches its own handles from that -- see
// Service.resolveOrdinalIndexes.
type boundOrdinalExecution struct {
	Program             queryplan.OrdinalProgram
	ProvenanceSQL       string
	ProvenanceFields    []string
	Indexes             map[string]ordinal.SnapshotIndex
	DictionarySet       ordinal.DictionarySetManifest
	DictionarySetDigest string
	SidecarGrants       []sqlpolicy.ProductGrant
	// EstimatedBaseFacts is a conservative pre-execution working-set estimate
	// derived from immutable publication row counts and the exact evidence-field
	// contract. It selects the million-fact lane; it is not a budget charge.
	EstimatedBaseFacts uint64
}

// ordinalSidecarLogicalName is the generated policy name one Publication's
// sidecar is granted under.
//
// It is the Gateway's copy of the construction preparation uses, kept so the
// sidecar-grant tests can name the relation a grant must bind without reaching
// into internal/physicalquery. Nothing in production derives a grant from it.
func ordinalSidecarLogicalName(publication string) string {
	sum := sha256.Sum256([]byte("taskgate-sidecar-logical\x00" + publication))
	return "ordinal_sidecar_" + hex.EncodeToString(sum[:8])
}
