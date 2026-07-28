# PostgreSQL 16 multi-scale Join--Group evaluation

This campaign executes the public `execute_plan` path, not an internal
microbenchmark.  It joins deterministic Orders and Lineitem relations, groups
by a three-valued order status, and computes `sum(extendedprice)`,
`sum(linenumber)`, and `count(*)`.  The default three scales contain 1,000,
10,000, and 45,000 orders
with exactly five line items each.  Their positive-output dependency sets have
23 facts per order, so the largest query settles 1,035,000 dependency facts.

The schema is **TPC-H-derived only**: it uses the Orders--Lineitem relationship
shape but neither the official dbgen data nor a TPC benchmark query.  Results
must not be described as official or comparable TPC-H numbers.

Run from the repository root:

```sh
./evaluation/run-exposure-scale.sh
```

This default writes an ignored engineering result under `raw/<run-id>/`.  The
source-controlled paper campaign was promoted with:

```sh
EXPOSURE_SCALE_RUN_ID=paper-20260728 EXPOSURE_SCALE_PROMOTE=1 \
  ./evaluation/run-exposure-scale.sh
```

Allow at least 20 minutes and roughly 12 GiB of available container memory for
the publication configuration; observed gateway peak memory was about 8 GiB.

Every scale/trial uses a fresh root task.  The runner measures direct
PostgreSQL, a novel full-path query, and a different-request-ID replay.  It
asserts the expected dependency cardinality, identical observation digests,
and zero replay charge/growth before writing the report. `finalize.py` rejects a
publication result unless it contains at least three scales, three trials, and
a largest point of at least one million dependency facts, then binds the raw
report and relevant implementation sources by SHA-256.
