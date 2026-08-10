# Publication review candidates

Files below this directory are credential-free, pre-run review candidates.
They are not runtime activation material, publication bindings, author
approvals, or evidence from a measurement campaign. Large HOT, COLD, and
sidecar artifacts are regenerated into a private temporary directory and are
represented here only by compiler-verified manifests and digests.

The bundle manifest is the single canonical small review surface for the
dictionary and sidecar: it embeds the complete dictionary manifest and binds
the HOT, COLD, and sidecar transports by name, SHA-256, and byte length.
`review.json` binds that bundle to the compiler input, dedicated Catalog
candidate, complete semantic-to-ordinal publication-universe check, and the
full 105-file ProvSQL and 24-file Scale oracle closed sets. Separate derived
dictionary or sidecar summaries are intentionally not duplicated.

The two oracle-set aggregates retain their already-reviewed ledger conventions
and state those conventions in `review.json`. ProvSQL uses
`<oracle-root-relative path><TAB><manifest SHA-256><LF>`; Scale uses the earlier
sha256sum-style `<manifest SHA-256><SPACE><SPACE><repository-relative path><LF>`.
Their explicit verifier, path root, and record format prevent the two path
domains from being compared as though they were the same aggregate algorithm.

The complete 2,070,000-Fact publication is linked with semantic role `union`,
not `candidate`: its expected cardinality and role-bound digest come from both
fully verified C1 `1035000-overlap-0` manifests. The linker independently
streams every canonical Fact and resolves every reviewed ordinal.
`actual_ordinal_source=reviewed_publication_universe` means that comparison
operand is the complete candidate dictionary, not a production query result.
The separate live Gateway regression labels its operand `production_factset`
and is the evidence for production member equality.
