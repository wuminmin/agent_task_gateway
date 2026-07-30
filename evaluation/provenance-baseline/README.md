# ProvSQL external provenance baseline

This track measures ProvSQL as an **external provenance-generation baseline**.
It does not present ProvSQL as a complete TaskGate competitor and does not turn
missing TaskGate semantics into a ProvSQL performance penalty.

The executable comparison boundary is:

```text
ordered SQL result production
    + complete client drain
    + provenance representation generation
```

The byte-identical SQL returns one canonical visible-result column followed by
three aggregate carrier columns. Vanilla PostgreSQL drains ordinary scalar
values in those carriers. ProvSQL rewrites them to `agg_token` values whose text
form is the aggregate-circuit root UUID, and appends one hidden row-level UUID.
The runner hashes only the first column for ordered visible-result equivalence
but drains every column inside the timed interval. Query rewriting,
aggregate-token construction, and persistent circuit construction are therefore
included. Root-type inspection is deliberately outside that interval. Semiring
evaluation, probability calculation, and Shapley values are excluded.

TaskGate's task-root history lookup, set difference, budget check, atomic
ledger commit, receipt, and result release are also outside this external
track. Those mechanisms remain measured by `evaluation/v4-acceptance` and are
`N/A`, not zero, for ProvSQL.

## Reproducibility and fairness controls

- Vanilla PostgreSQL uses a pinned PostgreSQL 16.14 image digest; the ProvSQL
  image is built from that exact base digest and then installs the extension.
- ProvSQL is pinned to release 1.11.0's peeled commit
  `6388fd06b79b7d247b4ff4dad4959374d0e92358`; the image build fails if Git
  resolves a different commit.
- Both services load the same deterministic 50,000-order/250,000-lineitem
  business fixture plus 1,000 provenance-nonce rows. The runner drains and
  hashes the identical ordered 301,000-row canonical business stream from both
  systems before timing anything. ProvSQL's hidden physical token columns are
  disabled only for this fingerprint query.
- Both systems receive byte-identical workload SQL and the same scale/nonce
  parameters. A derived grouped subquery computes each aggregate once;
  `coalesce(aggregate, NULL::type)` preserves the scalar business value for the
  canonical JSON column, while three raw aggregate columns carry the same
  outputs. On ProvSQL those raw columns have type `agg_token`; the hidden row
  token and persistent gate/file growth are recorded as well.
- Every visible result is checked with a multiplicity- and order-sensitive,
  length-framed SHA-256. Provenance representation hashes are order-independent
  because they identify a multiplicity-preserving multiset of opaque carriers.
  Each ProvSQL execution must return exactly one hidden row token and every
  configured aggregate provenance carrier per visible row. The runner verifies
  carrier OIDs as `provsql.agg_token`, UUID syntax, hidden-column name/OID, and
  the expected `agg` and `delta` gate types.
- Workload order within each iteration and baseline order within each pair are
  randomized from a recorded seed, and every measured position is retained.
  This balances ProvSQL's persistently growing circuit across configured scales.
  Warmups and measured runs are explicit. The accepted protocol has a warm
  business-data cache but a novel circuit for every execution: each query joins
  one distinct, genuinely provenance-tracked nonce row without changing its
  visible aggregates. Nonce ranges cannot overlap, every gate count must grow,
  and no representation hash may repeat. A paired TaskGate campaign must include
  this same dependency; the nonce is not hidden setup work.
- Parallel gather is disabled on both sessions so circuit structure is not
  affected by worker merge order. Warning output is suppressed symmetrically;
  both settings and ProvSQL's aggregate-token UUID rendering mode are read back
  and recorded.
- `statement_timeout_ms` is installed and read back on both database sessions;
  it is a real per-statement PostgreSQL timeout, not merely a campaign label.
- Both clients use PostgreSQL's simple text query protocol. ProvSQL appends its
  hidden token column in the planner hook after parse/describe, so using the
  extended protocol would create a target-list/result-format mismatch; using
  simple protocol on both sides keeps transport behavior symmetric.
- ProvSQL gate count and the five circuit mmap file sizes are sampled before
  and after every query, outside the timed interval.
- The outer driver binds both database container image IDs and reads exact
  cgroup-v2 lifetime `memory.peak`. This peak includes database initialization
  and warmup, so it is not query-interval memory and cannot support a query-peak
  comparison by itself. The exact runner executable is bound by SHA-256.
- A run writes a failure report when a runtime gate fails and never overwrites
  an existing result.

The data has a TPC-H Orders--Lineitem relationship shape, but it is generated
from repository formulas and is **not** an official TPC-H benchmark.

## Commands

Validate the strict smoke configuration without contacting a database:

```sh
docker run --rm -v "$PWD:/src" -w /src \
  golang@sha256:ea341baa9bd5ba6784f6d7161ace70544349a6242d54d34a0fbfd2c4d51c9d58 \
  go run ./evaluation/cmd/provenance-baseline \
  -config evaluation/provenance-baseline/config.smoke.json -validate-only
```

Run the real smoke campaign (one warmup and three measured executions at 1,000
orders):

```sh
make eval-provenance-baseline
```

The run creates an ignored, private directory under `raw/` containing:

- `config.json`: the exact, byte-preserved configuration used by the runner;
- `report.json`: runner output and all per-query samples;
- `results.json`: finalized evidence with image IDs and cgroup memory peaks.

For the three-scale protocol, copy the template, replace the campaign ID, and
run it from a fresh Compose project:

```sh
cp evaluation/provenance-baseline/config.full.example.json /tmp/provsql-full.json
# Edit campaign_id in /tmp/provsql-full.json.
PROVENANCE_BASELINE_CONFIG=/tmp/provsql-full.json \
PROVENANCE_BASELINE_RUN_ID="paper-$(date -u +%Y%m%dt%H%M%Sz)" \
  ./evaluation/provenance-baseline/run.sh
```

Publication evidence should additionally record a run-specific host/software
environment manifest and repeat the campaign on at least three fresh database
volumes. Every table must label this protocol as **warm business-data cache,
novel nonce-bound circuit per execution**. Do not promote the smoke output or
compare it numerically with an older TaskGate campaign merely because both ran
on the same named host.

## Interpreting the result

This track alone supports only a ProvSQL-versus-direct-PostgreSQL overhead,
defined explicitly as a paired difference or ratio for the same workload and
summary statistic. A TaskGate comparison becomes admissible only after a
current TaskGate provenance-generation campaign uses the same fixture, tracked
nonce dependency, query semantics, host, cache policy, run count, and
result-drain boundary; replay must be disabled or reported separately.

Conditionally allowed claim after that paired TaskGate evidence exists:

> On the common result-plus-provenance-generation boundary, TaskGate's scoped
> FactSet production has overhead of the same order as / higher than / lower
> than ProvSQL under the recorded workload and environment.

Disallowed claim:

> TaskGate is globally faster or better than ProvSQL.

ProvSQL stores a general circuit with tuple-level input tokens. TaskGate V4
constructs a budget-oriented, typed row/cell-oriented exact FactSet from
immutable snapshot ordinals. The artifact sizes and cardinalities therefore
describe different objects; report each side's semantics beside every timing
or size table and do not interpret absolute latency as equal-work ranking.

The complementary baselines remain separate experiments:

- PostgreSQL BatchHash: exact bulk set/history operations in the database;
- SortedHash Merge: a non-bitmap exact-set implementation;
- TaskGate V3: the legacy materialized per-Fact ledger;
- TaskGate V4: the public snapshot-ordinal/Roaring path.

BatchHash and SortedHash answer the representation-design question. ProvSQL
answers the external-provenance question. Neither should be substituted for
the other.
