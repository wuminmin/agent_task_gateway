# V5 Outcome deterministic evidence

`evidence.json` binds the V5 exact-set regressions to an existing implementation
base commit, a bounded manifest of production and test source files, and a raw
`go test -json` execution receipt. The paper evidence generator verifies the
Git object, re-hashes every manifest entry and its canonical source-set digest,
parses every raw JSON event, and rejects a missing, skipped, failed, extra, or
stale required test. The base commit alone is not represented as the tested
tree: the source manifest binds all listed working-tree changes.

Before final submission, run `scripts/record-compose-e2e.sh` from a clean,
frozen submission commit. It refuses measured-path drift, runs the complete
Compose acceptance suite, retains `raw/compose-e2e.log`, and writes
`compose-receipt.json` with the submission SHA, immutable Compose image IDs,
Catalog digest, exit status, four required E2E assertions, and raw-log digest.
On success the wrapper automatically promotes `evidence.json` to schema version
3, adds `submission_commit` and the receipt-hash binding, and runs the paper
generator as an independent final check. The receipt and evidence may live in a
later evidence-only commit: the generator requires the frozen SHA to be an
ancestor and every measured path in the checked-out tree to be identical to
that SHA, avoiding a circular "receipt must already be in the measured commit"
dependency. Schema version 2 remains readable only for the current pre-freeze
evidence; it does not make the stronger final-submission claim.

The PostgreSQL regression commits the 100,000-member immutable object graph in
one transaction and performs novelty/replay in a separate transaction. It does
not claim a production latency measurement or a formally settled root-head CAS.
The same-prefix regression compares the incremental implementation with the
shared canonical builder's full-rebuild reference implementation. It is an
adversarial exactness boundary, not an implementation-independent oracle or a
hash preimage attack campaign.
