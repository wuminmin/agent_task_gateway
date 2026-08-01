# V5 Outcome deterministic evidence

`evidence.json` binds the source-controlled V5 exact-set regressions to their
test-source SHA-256 and records only properties asserted by those tests. The
paper evidence generator rejects a stale source digest or changed counter.

The PostgreSQL regression commits the 100,000-member immutable object graph in
one transaction and performs novelty/replay in a separate transaction. It does
not claim a production latency measurement or a formally settled root-head CAS.
The same-prefix regression is an adversarial exactness boundary, not a hash
preimage attack campaign.
