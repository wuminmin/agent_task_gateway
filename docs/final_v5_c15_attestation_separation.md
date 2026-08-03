# C15 — separating the two schema attestations

Design record for the author decision to separate the attestation domains in the
loader, rather than compiling per-profile Publication bundles.

Both values are called a "schema digest" and both live near a Publication, which
is why the loader came to require them to be equal. They are traced here from
the code that produces them, not renamed by assumption.

## Profile Reporting-Surface Attestation

`catalog.Source.SchemaDigest`, as re-derived at runtime.

Generation path:

1. `cmd/gateway/main.go` builds a `dataconnector.Config` whose `ExpectedSchema`
   is derived from the **active Catalog's Products**: one `ViewSchema` per
   Product `reporting_view`, carrying that Product's declared fields, types,
   collations and collation versions.
2. `internal/dataconnector.attestSchemaDigest` queries `pg_attribute` for each
   of those views, rejects any column drift, reads each view definition, and
   folds the result through `dataconnector.SchemaDigest`.
3. `internal/gateway/datasource.go` compares the live attestation with
   `catalog.Source.SchemaDigest` and fails closed on a mismatch
   (`DATA_CONNECTOR_SCHEMA_DRIFT`).

Input set: **exactly the reporting views the active Catalog declares.** Under
amendment v1.2 a profile Catalog declares only its own Product closure, so this
digest is per profile. That is what C14 established, and
`config/profiles/schema-attestations-v1.json` records the reviewed value for
each profile.

It answers: *does the live Business PostgreSQL still present the reporting
surface this profile declares?*

## Publication-build Schema Attestation

`snapshotbundle.BundleManifest.DictionaryManifest.SchemaDigest`, and the
identical value inside the HOT dictionary manifest.

Generation path:

1. The compiler input `config/snapshots/<publication>.json` carries
   `snapshot.schema_digest`.
2. `internal/snapshotbundle.bundle.go` validates it is a SHA-256 and passes it
   through as `ordinal.SnapshotSpec.SchemaDigest`
   (`bundle.go:897`, guarded at `bundle.go:831`).
3. `internal/ordinal/dictionary.go:397` copies it into the emitted
   `DictionaryManifest`, and `dictionary.go:136` folds it into
   `DictionaryManifest.Digest()`.
4. That manifest digest **is** the Publication `manifest_digest` the Catalog
   pins in `snapshot_publications[].manifest_digest`.

Input set: **a build-time constant recorded into the immutable bundle.** In this
repository the value authored into every descriptor was the full-Catalog source
attestation current when the descriptor was written. It is therefore *not* a
relation-local digest, and this record deliberately does not call it one. Its
accurate name is the **Publication-build Schema Attestation**: the schema
attestation that was in force when this Publication was compiled.

It answers: *which schema attestation was this immutable Publication built
under?*

## Why the equality was wrong

A Publication is deliberately shared. `expense-detail-v1` backs five profiles.
Its bundle embeds one build attestation, fixed at compile time. After C14 those
five profiles each declare their own surface attestation, so no single bundle
value can equal all five. Requiring

    bundle.DictionaryManifest.SchemaDigest == source.SchemaDigest

conflated a build-time property of one artifact with a runtime property of one
profile, and made a shared Publication unloadable.

## What replaces it

Nothing is merely deleted. The build attestation stays fully verified, by a
stronger binding than the equality it replaces:

- `DictionaryManifest.Digest()` folds `SchemaDigest` in, so editing it changes
  the manifest digest.
- The loader requires `bundle.ManifestDigest == publication.ManifestDigest`,
  `hot.ManifestDigest() == publication.ManifestDigest`, and recomputes
  `manifest.Digest()` itself rather than trusting the recorded value.
- `matchHotIndexToCatalog` additionally requires the HOT manifest to be
  `reflect.DeepEqual` to the bundle's manifest.

So a tampered build attestation still fails closed — it simply fails as a
Publication identity violation, which is what it is, instead of as a false
mismatch against whichever profile happens to be active.

The datasource identity check is kept in both functions:
`manifest.SourceID == source.DatasourceID`. A Publication compiled against a
different datasource is still rejected.

## Product–Publication compatibility

Separating the domains removes the only thing that previously, if accidentally,
tied a Product's declared fields to the bundle it reads through. That link is
restored explicitly rather than left implicit; see
`cmd/gateway/publication_compatibility.go` and its tests.

`ordinal.DictionaryManifest` carries no semantic field set and no entity-key
list — only versions, source identity, snapshot, the four artifact digests and
the segment manifests. Field-level name, type, collation and collation-version
compatibility is therefore **not** re-derived from it. The equivalent check
already exists one layer out and is stronger: `dataconnector.attestSchemaDigest`
compares every Product's declared fields against the live view's columns, types,
collations and collation versions before it will produce a digest at all, and
fails closed with `DATA_CONNECTOR_SCHEMA_DRIFT`. That runs against the database
the queries actually read, not against a build-time constant. This record exists
so the boundary is documented rather than guessed at, and
`TestProductPublicationCompatibilityDoesNotGuessFieldTypes` pins it.

## How the separation is evidenced

The activation evidence records the two chains separately, and never derives one
from the other:

- `profile_surface_attestation` — the Catalog-pinned digest, the digest re-derived
  against the live database during this activation, the reporting-view set, and
  `verified`.
- `publication_bundle_attestations` — one entry per activated Publication: its
  build attestation, its identity digests, the SHA-256 of the bundle manifest
  file, `recomputed_identity_matches_bundle` (the activator re-digests the
  dictionary manifest itself), and `activated_by_loader` (the identity the
  running Gateway reported activating).
- `attestation_domains_separated` — true only when **both** chains completed
  independently. It is deliberately not derived from the two digests being equal
  or unequal; requiring either would re-couple the domains.

`equals_profile_surface_attestation` is recorded per Publication as an
observation only. On the Result-heavy profile it is `false`: the surface
attestation is `ae4d7458…` while `final-v5-result-heavy-v1` was built under
`ebedbecf…`. Both are correct, and before the separation that deployment could
not have activated at all.
