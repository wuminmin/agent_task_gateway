# Final-V5 contract amendment v1.6

Previous release:
final-v5-contracts-v1.5

New release:
final-v5-contracts-v1.6

## Why this release exists

No contract artifact changes in this release. Not one of the 28 indexed
digests moves, no query, no normalizer, no plan, no oracle, no gate code.

What changes is the deployment's runtime parameter model, and it changes what
the frozen contract can actually execute. Under v1.5 three of the six frozen
Artifact `result-heavy` cells could not complete at all:

- the Gateway's 512 MiB cgroup ceiling was not survivable by any wide cell.
  `100k-x16` was confirmed OOM-killed there (`OOMKilled=true ExitCode=137`);
- the connector's 5 s statement timeout was exceeded by the database read
  alone, so wide cells failed with `DATA_CONNECTOR_QUERY_TIMEOUT` before they
  ever reached exposure accounting.

A contract whose cells cannot execute is not a contract that measurement can
be attributed to. Two deployment parameters therefore change, both on the
Gateway service in `compose.yaml`:

```text
mem_limit                            512m -> 8g
GATEWAY_CONNECTOR_STATEMENT_TIMEOUT  (code default 5s) -> 10m
```

A release boundary is the only honest way to record this. Evidence names the
release it ran under, so samples measured under the old parameter model must
not be readable as evidence about the new one, in either direction.

## These are declared governance parameters, not measurement conveniences

Both values are properties of what the deployment permits, and the paper must
state them as such rather than presenting them as tuning.

**The statement timeout is an exposure window.** A longer permitted statement
is a longer interval in which a single authorized request may draw data from
the Business database. Ten minutes is not a new number chosen to make a
measurement pass: it is what the exposure-scale attestation path already sets
(`evaluation/cmd/final-v5-publication-review/review.go`), and the publication
live path already uses thirty minutes. The code default stays 5 s; the
deployment declares its own value, so the relaxation is visible in the
deployment manifest rather than buried in a binary.

**The memory ceiling is a property of the accounting mechanism, not of the
implementation's efficiency.** Cumulative exposure accounting holds one Fact
resident per exposed (row x visible field) for the life of a derivation, so
peak RSS tracks the exposed-cell count rather than the serialized result size.
Measured across three orders of magnitude, peak RSS is approximately
`443 MiB + ~3.6 KiB per exposed cell`. The widest frozen cell
(100,000 x 16 = 1,600,000 cells) measured 6,607,790,080 B and, on an
independent later run, 6,630,240,256 B: 6.15 and 6.17 GiB, against a Parquet
object of 18,743,852 B.

That memory is live, not garbage. `GOMEMLIMIT` was measured at 384 MiB and
again at 224 MiB and still produced `OOMKilled=true ExitCode=137`, because the
garbage collector cannot reclaim an accounting set that is still growing. The
ceiling is raised because the mechanism requires the memory, not because the
implementation wastes it.

`8g` leaves roughly 22% headroom over the highest measured peak.

## Why these parameters cannot perturb any identity

Neither value has a digest surface, and this was verified rather than assumed.

`StatementTimeoutPinSQL` binds the timeout as `$1`
(`internal/dataconnector/pins.go`), so the value never enters statement text
and is invisible to the StrictAST digest, which is schema v2 and normalizes
ParamRef. The Attestation footprint counts `statement_timeout_pin` calls, not
their argument. `mem_limit` is a cgroup property of the container and enters
no contract, binding or receipt.

Confirmed empirically: the Attestation footprint qualification re-run on the
tree carrying both parameter changes reproduced `portable=58d58d30326e`, which
is byte-identical to the qualification taken before them.

## What did not change

**The accounting mechanism.** `FactSet` is unchanged: no bound, no spill, no
key-representation change. The high-cardinality lane threshold stays at
1,000,000 estimated base facts and the lane keeps capacity 1. `FactID`,
Receipt, Exposure settlement and PENDING/AVAILABLE are untouched.

**Every indexed contract artifact.** All 28 digests in `contracts/index-v1.json`
are byte-identical to v1.5. Only the index's own release, supersedes and
amendment fields move, which is what obliges the SQL-executability record
below to be re-derived.

Also unchanged: Dataset logical content; probe output schema and meaning;
Products; Publications; Profile closures and IDs; Query Contracts; Oracles;
workload cells; warmups; statistics; the 160 MiB hot-artifact ceiling; the
observer runtime schema, source-build manifest and digest binding; and the
closed-world observer accounting introduced by v1.4.

## What is deliberately NOT claimed by this release

The concurrent-memory behaviour of the wide cells is not measured. Lane
assignment is confirmed from retained signed receipts: all three `x16` cells
estimate 1,700,000 base facts and take the capacity-1 lane, and all three `x4`
cells estimate 500,000 and take no lane. The unprotected `100k-x4` cell
measured 1,833.9 MiB, so concurrent instances of it add rather than queue.
No harness exists that drives concurrent wide-result Artifact queries against
one Gateway, and none was built for this release.

The `8g` ceiling is therefore sized for the measured single-derivation peak.
It is not a claim about N concurrent wide derivations.

## Activation support does not carry across this release

`ActivationSupport.ContractRelease` pins the contract a smoke ran under, and
activation support deliberately does not survive a release change. The recorded
smokes ran under v1.5, so `config/profiles/activation-support-v1.json` is
removed rather than retagged, and the registry is regenerated with the manifest
absent. Retagging would claim the probes executed under a contract they never
saw.

This is self-enforcing: `resolveArtifactProfileBinding` refuses to produce a
binding for a profile that is not targeted-run eligible, so no Artifact run can
execute under v1.6 until an operator records a live activation smoke against
this release.

**This is the designed cost of the bump.** The feasibility runs that established
the two parameter values above executed under v1.5. That evidence keeps naming
v1.5, is not retagged, and does not transfer. In particular the `100k-x16` run
that passed v3 acceptance with these parameters is a v1.5 feasibility probe,
not v1.6 acceptance evidence; P5.1 is re-run under this release.

## SQL executability record

`contracts/sql-executability-v1.json` embeds `contract_index_sha256`, which the
index bump invalidates. It is re-derived by re-running the gate against a real
PostgreSQL 16, never by editing the recorded digest.

## Publication evidence affected

None. The publication binding approved by author decision 22 is a pre-run
review candidate: it binds independent-oracle expectations and direct Business
PostgreSQL result observations, and contains no activation-dependent sample and
no publication-eligible sample. No publication-eligible sample exists under any
release.

## Execution status

v1, v1.1, v1.2, v1.3, v1.4 and v1.5 are preserved for audit but superseded for
Final-V5 execution by v1.6.
