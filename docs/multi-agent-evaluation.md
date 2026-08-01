# Multi-agent evaluation

This document defines correctness and concurrency experiments for delegated
agents in one approved TaskGate root family. It is an evaluation protocol, not
a report of measured results.

The claim boundary is one root family and one immutable publication. A newly
approved root has an independent ledger; this experiment does not claim a
principal-wide or tenant-wide global exposure limit.

## Model and invariants

```text
                         root task T0
                  approved budget B = (BR, BD, BO)
                               |
             +-----------------+-----------------+
             |                                   |
       delegated task TA                  delegated task TB
             |                                   |
       delegated task TA1                    agent B
          agent A1

             all descendants resolve to the T0 root head
```

Let `E_i = (R_i,D_i,O_i)` be the exact effect of a successfully settled query
and `L_0` the initial root ledger. For every execution order and interleaving,
the experiment requires, dimension by dimension:

```text
L_final = L_0 union E_1 union ... union E_n
sum(charged_i) = cardinality(L_final ANDNOT L_0)
cardinality(L_final) <= B
```

The required properties are:

1. **Shared root-family ledger:** every descendant reads and updates the same
   three-dimensional root head.
2. **No double spend:** overlapping effects are charged once across agents,
   and concurrent settlement cannot commit a vector beyond any root limit.
3. **Delegation attenuation:** a child may only narrow the parent's products,
   fields, scope, sensitivity, expiry, resource limits, and R/D/O limits while
   preserving the root, Catalog, datasource, schema, publication bindings, and
   exposure profile.
4. **CAS conflict recovery:** a losing optimistic root-epoch compare-and-swap
   retries from the new head and recomputes novelty in all three dimensions;
   it must not reuse a stale charge or publish only part of the vector.

`Dependency` is the prose name for the positive-output dependency footprint;
some machine-readable fields retain `influence` for compatibility.

## Fixture and task topology

Use one immutable publication and create tasks through the public request,
delegation, submission, and approval flow. Do not insert ready-made task rows
for a publication experiment. The minimum topology is `T0 -> {TA, TB}` plus
`TA -> TA1`; use distinct enabled principals where the deployment supports
them.

Record the signed authorization manifest and effective grant for every node.
All valid descendants must bind the same `root_task_id`, Catalog digest,
dictionary set, datasource, schema, and publication. Revalidate the complete
ancestor chain before each query.

Prepare exact oracle-backed effects with these overlap classes:

- **identical:** two agents submit the same `(R,D,O)` effect;
- **partial:** effects overlap by a fixed 50% or 90% in the target dimension;
- **disjoint:** effects have zero intersection; and
- **boundary:** the root begins at `B-1`, contenders offer the same one-unit
  novel effect, and a later distinct `B+1` effect must fail closed.

Use fresh root families for every overlap class, width, ordering, and
replicate. A root used to provision or calibrate effects cannot become a
measured root.

## E1 -- shared ledger and no double spend

Run the identical, partial, and disjoint effects first serially in both agent
orders, then concurrently behind a client start barrier. For every query save
actual and charged R/D/O sets, set digests, observation digest, root epoch, and
receipt.

The independent oracle computes the expected union. Acceptance requires:

- identical effects charge the first successful settlement only;
- partial overlap charges exactly the set difference against the committed
  root, regardless of which agent wins;
- disjoint effects charge their exact union;
- the final ledger digest is order-independent for equivalent effect sets;
- the sum of per-query charged cardinalities equals final ledger growth; and
- no success response becomes visible before its atomic settlement commits.

Include independent-root tasks as a scope control. The same FactID may be
charged once in each independently approved root; that expected behavior must
not be mislabeled a root-family double spend.

## E2 -- delegation attenuation

Create one valid child that narrows at least product fields, a categorical or
date-range scope, TTL, ordinary resource limits, and every R/D/O limit. Show
that an in-scope query succeeds and settles against the root head.

Then attempt one expansion at a time:

- add a product or projected field;
- widen an enum scope or date range;
- raise a sensitivity ceiling;
- extend expiry or an ordinary query/row/time budget;
- raise one of the Release, Dependency, or Outcome limits;
- change the exposure profile, Catalog, datasource, schema, publication, or
  root lineage; and
- delegate from an inactive ancestor or to a disabled principal.

Every expansion must fail before Business PostgreSQL execution and before any
exposure reservation or head mutation. Capture a stable rejection
classification and prove that the child was not made ACTIVE.

Also test the child-specific absolute ceiling. If the shared root already
exceeds a child's narrower ceiling, the child may replay an observation with
zero novelty when otherwise authorized, but it must not add a new fact beyond
its signed ceiling. Report this behavior separately from the root's overall
remaining budget.

## E3 -- concurrent boundary and no overspend

For the target dimension, settle a prefix that places the shared root at
`B-1`. Release all agents through a barrier with the same one-unit novel
effect. Exactly one settlement should add the shared novelty; equivalent
followers should settle with zero novelty after recomputing against the new
head. Then issue a distinct effect that would reach `B+1`.

The overflow request must return `EXPOSURE_BUDGET_EXHAUSTED`, release no
result, and leave no canonical/`AVAILABLE`/deliverable artifact, result
capability, committed materialization, observation reference, root observation,
success audit, success receipt, bitmap content, or partial head update. The
terminal failure audit and FAILED receipt are expected evidence and must be
preserved. Private staging cleanup is best effort: any deletion failure or
orphan staging object must be classified and shown to be unreachable through
normal result APIs. Repeat with Release, Dependency, and Outcome as the
boundary dimension while validating all three dimensions every time.

Use widths `1,4,8,16` for compatibility with the current V4 acceptance runner.
The broader concurrency protocol in
[experiment-guide.md](experiment-guide.md#6-shared-root-concurrency-isolation)
defines future widths `10,50,100,500`; those values require a dynamic Gateway
pool and explicit CAS telemetry before they are publishable.

## E4 -- CAS conflict recovery

CAS behavior needs two separate evidence layers:

1. **Deterministic integration evidence:** force two transactions to read the
   same root epoch and queue at the conditional head update. Verify one CAS
   wins, the loser reports a retry, rereads the head, recomputes all R/D/O
   novelty, and commits an equivalent zero- or reduced-novelty effect. The
   existing test
   `TestOrdinalRootSettlementRetriesOptimisticCASAndRecomputesAllDimensions`
   exercises this mechanism against PostgreSQL.
2. **Live natural-contention evidence:** run public-path agents without an
   acceptance-owned root lock and collect explicit production CAS-attempt,
   conflict, retry, and exhaustion telemetry attributable to the cell.

PostgreSQL lock waiters, long latency, zero-novelty followers, or client retries
are not CAS-conflict counters. The current forced-queue V4 concurrency harness
proves a boundary safety condition but explicitly cannot reveal which root
epoch a contender read. If production CAS telemetry is absent, report live CAS
attempts/conflicts as `unmeasured`, not zero.

For every conflict, acceptance requires one transaction-wide retry. Release,
Dependency, and Outcome heads and the single epoch must move together. Retry
exhaustion must fail closed with the same no-partial-release conditions as a
budget failure.

## Metrics

- offered and observed concurrency, active and queued clients;
- successful/failed requests and stable error classes;
- actual and charged R/D/O cardinalities and exact set digests per request;
- initial/final root epochs, vectors, head digests, and content-row counts;
- duplicate charge and missing charge, both computed as exact set differences;
- charged winners, zero-novelty settlements, and rejected overflow attempts;
- CAS attempts, conflicts, retries, retry exhaustion, and conflict rate when
  explicit telemetry exists;
- budget violations: any committed dimension above its limit or any result
  released for a transaction that should not settle; and
- descriptive client latency and throughput, secondary to correctness.

## Current repository commands and evidence boundary

Run the deterministic exposure and PostgreSQL-backed integration preconditions:

```sh
make eval-exposure
make verify
```

`make eval-exposure` binds named task-family delegation and concurrent
settlement test events into the exposure evidence. `make verify` runs the wider
race/integration suite, including V4 delegation, atomic boundaries, optimistic
CAS retry, and recovery tests. Neither command is a large live multi-agent
campaign.

Validate the current V4 concurrency configuration without contacting services:

```sh
go run ./evaluation/cmd/v4-concurrency \
  -config evaluation/v4-concurrency/template.json -validate-only
```

The runnable `1/4/8/16` deployment and provisioning commands are maintained in
[evaluation/v4-concurrency/README.md](../evaluation/v4-concurrency/README.md).
Its forced root-lock queue is valid safety evidence but not live CAS-conflict
measurement. Do not copy it into the future `10/50/100/500` table or infer CAS
counts from `root_lock_waiters_observed`.

## Repetitions and acceptance

Run at least 30 fresh root families for each width and contention mode in a
publication campaign. Randomize width and effect order across at least three
fresh deployments. Preserve failed and timed-out trials. A trial is invalid if
the offered concurrency did not reach the service; it is a safety failure if a
budget vector is exceeded or a rejected transaction releases a result.

Counts and exact-set checks are primary. If zero budget violations are
observed, report the number of trials and requests. A binomial confidence bound
may describe the sampled run but is not proof that a violation is impossible.

| Case | Width | Valid trials | Final union exact | Duplicate charge | Missing charge | CAS attempts | CAS conflicts | Retry exhaustion | Rejected overflow / attempts | Budget violations |
|---|---:|---:|---|---:|---:|---:|---:|---:|---:|---:|
| Identical effects | | | | | | | | | N/A | |
| Partial overlap | | | | | | | | | N/A | |
| Disjoint effects | | | | | | | | | N/A | |
| `B-1 / B / B+1` | | | | | | | | | | |

| Delegation case | Expected decision | Observed decision | Business SQL executions | Reservations created | Root mutations |
|---|---|---|---:|---:|---:|
| Valid narrowed child | allow | | | | |
| Product/field expansion | deny | | | | |
| Scope/sensitivity expansion | deny | | | | |
| TTL/resource-budget expansion | deny | | | | |
| R/D/O budget expansion | deny | | | | |
| Binding/lineage change | deny | | | | |
| Inactive ancestor/disabled principal | deny | | | | |

## Artifacts to preserve

- task DAG, hashed task/principal identities, signed manifests, grants, and
  ancestor-state snapshots;
- immutable Catalog/publication/dictionary manifests and digests;
- exact workload effects and independent-oracle union/difference reports;
- per-request barrier/service timestamps, Enforcement Layer replica, response, result and
  observation digests, actual/charged sets, receipt, and root epoch;
- before/after root vectors, head/set digests, bitmap-content counts, artifacts,
  and audit classifications;
- explicit CAS event/counter stream and attribution method, or an explicit
  `unmeasured` marker;
- forced-queue lock evidence kept separate from natural-contention evidence;
  and
- source, configuration, image, environment, and raw-evidence SHA-256 bindings.
