# Final-V5 contract amendment v1.7

Previous release:
final-v5-contracts-v1.6

New release:
final-v5-contracts-v1.7

## Why this release exists

No contract artifact changes, exactly as in v1.6. All 28 indexed digests are
byte-identical, and `contracts/index-v1.json` moves only its release,
supersedes and amendment fields.

One deployment parameter changes:

```text
mem_limit  8g -> 12g
```

`GATEWAY_CONNECTOR_STATEMENT_TIMEOUT` stays at 10m. The accounting mechanism,
the lane threshold and every executed byte are untouched.

v1.6 sized `8g` for a **single derivation**, and said so explicitly. That was
the right ceiling for the question v1.6 could answer and the wrong one for the
campaign, for a reason that is structural rather than marginal.

## The ceiling is a campaign property, not a cell property

`gateway_memory_peak_bytes` is a cumulative cgroup high-water mark that never
resets, and the formal campaign runs all nine experiments
(`baseline scale artifact rls attack provsql compiler concurrency rq5`) against
**one** Gateway. `run-deployment.sh` requires `container_restarts == 0` and
`oom_events == 0`, and campaign finalization rejects any deployment with
`SwapInDelta != 0` or `SwapOutDelta != 0`. There is no point at which the
cgroup is allowed to start clean, and a single OOM or a single page of swap
invalidates the deployment outright.

Measured against that: the Artifact experiment **alone**, from a fresh Gateway,
reached **7,886,876,672 B = 7.35 GiB**, which is 91.8% of the former `8g`
ceiling. In the campaign it runs third, after `baseline` and `scale`, whose
largest derivation carries 1,035,000 Influence facts
(`evaluation/finalv5oracle/dependency.go`). The campaign therefore begins the
Artifact experiment from an already-elevated arena and continues with six more
experiments afterwards.

`12g` keeps roughly 63% headroom over the only hard measurement. `16g` was
rejected deliberately: the host has 25 GiB, and a Gateway permitted to reach
16 GiB alongside two PostgreSQL working sets moves the risk from OOM to swap,
which is equally fatal to the campaign.

## Concurrency is now measured, which v1.6 declined to claim

v1.6 recorded that no harness drove concurrent wide-result Artifact queries and
that its ceiling was not a claim about N concurrent derivations. That gap is
closed by measurement, with the system under test unchanged: N concurrent
copies of the unmodified evidence runner against one Gateway, reading the same
`/sys/fs/cgroup/memory.peak` the observer reads, with the Gateway container
recreated before each trial so the mark starts fresh.

**Unprotected cells add.** `100k-x4` estimates 500,000 base facts, below the
1,000,000 lane threshold, so it takes no lane:

```text
N=1  1,877,254,144 B  1.75 GiB
N=2  3,424,505,856 B  3.19 GiB
N=4  6,737,903,616 B  6.28 GiB

least squares: peak ~= 210 MiB + N x 1,550 MiB
```

Every concurrent instance retrieved the full 100,000 x 4 result, so this is not
an artifact of partial work.

**Protected cells do not add.** `100k-x16` estimates 1,700,000 base facts and
takes the capacity-1 lane:

```text
N=1  6,623,100,928 B  6.17 GiB  drain 18,580 ms
N=2  7,966,765,056 B  7.42 GiB  drain 37,233 / 18,201 ms
```

Unserialized, N=2 would need about 12.3 GiB and would have been OOM-killed at
the 8g ceiling. It was not, and memory rose only 1.20x. The drain times show
the queue directly: one instance ran immediately at 18.2 s, the other waited
and finished at 37.2 s. The single lane is measured, not derived.

So the counter-intuitive shape of the system is confirmed: the **largest** cell
is the protected one, and the real concurrent exposure is the largest cell that
falls below the threshold.

## What is still NOT claimed

The campaign's cumulative peak is **not measured**. P5.2, P5.3 and P5.4 remain
blocked on P4.0 publication-binding closure, so only the Artifact experiment
has been measured in campaign topology. `12g` is an engineering margin over
that one measurement, not a demonstrated sufficiency.

If the campaign's accumulation turns out to exceed what the host can back
without swapping, the remedy is not a larger ceiling: it is bounding the
resident accounting set itself. A FactSet spill was demoted to an optimization
on the strength of the P5.1 six cells; that judgement holds for P5.1 and does
not automatically extend to the full campaign.

## What did not change

The accounting mechanism. `FactSet` is unchanged: no bound, no spill, no
key-representation change. The high-cardinality threshold stays at 1,000,000
estimated base facts and the lane keeps capacity 1. `FactID`, Receipt, Exposure
settlement and PENDING/AVAILABLE are untouched, as are Dataset logical content,
Products, Publications, Profile closures, Query Contracts, Oracles, workload
cells, statistics, the 160 MiB hot-artifact ceiling and the closed-world
observer accounting.

A cgroup limit has no digest surface: it enters no contract, binding or
receipt.

## Activation support does not carry across this release

The recorded smokes ran under v1.6, so `config/profiles/activation-support-v1.json`
is removed rather than retagged and the registry is regenerated with the
manifest absent. No Artifact run can execute under v1.7 until an operator
records a live activation smoke against this release.

The P5.1 six-cell result that passed under v1.6 keeps naming v1.6 and does not
transfer. It was measured under an 8g ceiling, and this release changes what
the deployment permits, so it is re-run rather than relabelled. That is the
same rule v1.6 stated, applied to v1.6's own evidence.

## SQL executability record

`contracts/sql-executability-v1.json` embeds `contract_index_sha256`, which the
index bump invalidates. It is re-derived by re-running the gate against a real
PostgreSQL 16, never by editing the recorded digest.

## Publication evidence affected

None. No publication-eligible sample exists under any release.

## Execution status

v1 through v1.6 are preserved for audit but superseded for Final-V5 execution
by v1.7.
