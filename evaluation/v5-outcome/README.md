# V5 Outcome deterministic evidence

`evidence.json` binds the V5 exact-set regressions to an existing implementation
base commit, a bounded manifest of production and test source files, and a raw
`go test -json` execution receipt. For schema version 3, the paper evidence
generator re-hashes every source-manifest entry from the historical
`submission_commit` Git blobs, checks the canonical source-set digest, and
verifies both the implementation-base-to-submission lineage and the recorded
submission's ancestry to `HEAD`. It also parses every raw JSON event
and rejects a missing, skipped, failed, extra, or stale required test. This
historical validation remains meaningful as descendant draft development
changes the current `HEAD`; it does not reinterpret current files as measured
evidence.

This is not a one-command source/raw/Compose resealer. Once the manuscript and
protocol are actually final, use this order:

1. freeze and commit the measured source tree;
2. rerun the exact V5 raw test/counter procedure and regenerate the schema-2
   source manifest and `raw_execution` receipt for that tree (there is currently
   no separate one-command recorder for this step);
3. run `scripts/record-compose-e2e.sh` with that clean frozen commit as
   `HEAD` (or `TASKGATE_SUBMISSION_COMMIT`);
4. review and commit the resulting evidence-only files and generated macros,
   then run `make paper-final-check` from the clean evidence commit.

The Compose wrapper preflights the source manifest and raw execution receipt
before starting the costly acceptance run. It refuses stale evidence or
measured-path drift, runs the complete Compose acceptance suite, retains
`raw/compose-e2e.log`, and writes
`compose-receipt.json` with the submission SHA, all immutable Compose image IDs
(including `test-runner` and `mcp-probe`), distinct Catalog file/runtime
digests, evidence-tooling file/digest bindings, exit status, five required E2E
assertions, and raw-log digest.
The complete `go test -json -race -count=1 -tags=taskgate_integration ./...`
gate rejects every named test skip and every unexplained package skip. Go's
package-level `[no test files]` lifecycle event is the sole explicit exception,
so packages without tests remain in the complete `./...` traversal without
being misreported as a skipped test. Costly scale experiments use the separate
`taskgate_scale` build tag and are not part of this acceptance run. Two further
classes sit outside it for reasons of their own. Cases that prepare an
ordinal-program plan also carry `taskgate_scale`: preparation resolves every
snapshot publication the Catalog declares, and five of the seven are scanned out
of the Business database, which measured 25.84 GB peak against a 30 GB host.
Cases that need host resources the product stack has no reason to carry -- a
Docker socket, the retained qualification artifacts, a live benchmark Dataset --
carry `taskgate_hostonly`; they exercise the evaluation harness rather than the
product, and the formal campaign exercises the same material at runtime. So the
gate is the complete traversal of what the product's own Compose deployment can
execute, not of the module's every file.
On success the wrapper promotes `evidence.json` to schema version
3, adds `submission_commit` and the receipt-hash binding, and invokes the paper
generator with `--evidence-mode final`. In its default draft mode, the generator
validates the source manifest, Catalog, and evidence-tooling hashes against that
historical commit while continuing to validate receipt bindings, raw-log hashes,
and internal canonical digests; current measured paths may evolve. In explicit
`strict` or `final` mode, it additionally requires every measured path in the
checked-out tree to be clean and identical to that SHA. This avoids a circular
"receipt must already be in the measured commit" dependency while preserving a
fail-closed final gate.
Schema version 2 remains readable in draft mode with its implementation base
required to be an ancestor of `HEAD`; strict/final validation rejects schema 2
because it has no `submission_commit` freeze claim.

The PostgreSQL regression commits the 100,000-member immutable object graph in
one transaction and performs novelty/replay in a separate transaction. It does
not claim a production latency measurement or a formally settled root-head CAS.
The same-prefix regression compares the incremental implementation with the
shared canonical builder's full-rebuild reference implementation. It is an
adversarial exactness boundary, not an implementation-independent oracle or a
hash preimage attack campaign.
