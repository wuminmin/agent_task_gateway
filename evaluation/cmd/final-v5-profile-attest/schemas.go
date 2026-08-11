package main

import (
	"taskbound.local/agent-data-gateway/internal/catalog"
	"taskbound.local/agent-data-gateway/internal/catalogschema"
	"taskbound.local/agent-data-gateway/internal/dataconnector"
)

// expectedSchemas delegates to the one evaluation-only reporting-surface
// derivation shared by profile attestation, schema-digest, and P4.0-E1.
func expectedSchemas(logical *catalog.Catalog) (catalog.Source, []dataconnector.ViewSchema, error) {
	built, err := catalogschema.Build(logical)
	return built.Source, built.Entries, err
}
