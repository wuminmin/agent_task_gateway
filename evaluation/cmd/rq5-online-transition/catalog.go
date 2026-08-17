package main

import (
	"taskbound.local/agent-data-gateway/evaluation/internal/experiment"
	"taskbound.local/agent-data-gateway/internal/catalog"
)

func catalogArtifact(publication loadedPublication) (*catalog.Catalog, []byte, error) {
	return experiment.BuildRQ5DailyCatalog(experiment.RQ5DailyCatalogInput{Day: publication.Day,
		PublicationName:           publication.Input.PublicationName,
		CatalogSource:             publication.Input.CatalogSource,
		SourceID:                  publication.Input.Snapshot.SourceID,
		SourceNamespace:           publication.Input.Snapshot.SourceNamespace,
		SourceRelation:            publication.Input.SourceRelation,
		Snapshot:                  publication.Input.Snapshot.Snapshot,
		OrdinalSidecar:            publication.Input.OrdinalSidecar,
		PublicationManifestSHA256: publication.Bundle.ManifestDigest,
		DictionarySHA256:          publication.Bundle.DictionaryManifest.DictionaryDigest,
		SidecarSHA256:             publication.Bundle.DictionaryManifest.SidecarDigest,
		SchemaSHA256:              publication.Input.Snapshot.SchemaDigest})
}
