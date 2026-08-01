# V5 Outcome deterministic evidence

`evidence.json` binds the V5 exact-set regressions to an existing implementation
base commit, a bounded manifest of production and test source files, and a raw
`go test -json` execution receipt. The paper evidence generator verifies the
Git object, re-hashes every manifest entry and its canonical source-set digest,
parses every raw JSON event, and rejects a missing, skipped, failed, extra, or
stale required test. The base commit alone is not represented as the tested
tree: the source manifest binds all listed working-tree changes.

The PostgreSQL regression commits the 100,000-member immutable object graph in
one transaction and performs novelty/replay in a separate transaction. It does
not claim a production latency measurement or a formally settled root-head CAS.
The same-prefix regression compares the incremental implementation with the
shared canonical builder's full-rebuild reference implementation. It is an
adversarial exactness boundary, not an implementation-independent oracle or a
hash preimage attack campaign.
