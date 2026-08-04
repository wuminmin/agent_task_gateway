# observer-accounting-v3: measurement window and target derivability

Two findings established by source inspection before Stage C4 integration. Both
were confirmed against the tree at `da390a4`, not assumed.

## 1. The periodic healthcheck contaminates the measurement window

Confirmed exactly as reported.

`compose.yaml:364` healthchecks the Gateway with

```text
["CMD", "curl", "--fail", "--silent", "http://127.0.0.1:8082/health/ready"]
interval: 3s
```

`cmd/gateway/main.go:317` routes `/health/ready` to `readiness(...)`, which calls
`connector.Ping`. `internal/dataconnector/postgres.go:331` shows `Ping` doing a
pool ping **and then `c.Attestation(ctx)`** — the full Business PostgreSQL
attestation.

So every three seconds the deployment issues, as `gateway_reader`:

| class | per probe |
| --- | --- |
| `datasource_identity` | 1 |
| `view_column_attestation` | E |
| `view_definition_attestation` | E |
| `nested_viewdef_rewrite_lookup` | E |

For the Result-heavy profile (`E = 1`) that is four statements every three
seconds. Any sample outstanding for longer than one interval would absorb a
wall-clock-dependent number of them, and every per-class multiplicity would be
wrong by an amount no plan can predict.

### Resolution

`evaluation/final-v5-wsl2/compose.observer-v3.yaml` overrides the Gateway
healthcheck to `/health/live`, which returns 204 without touching the control
store or the data source. Verified: the override resolves cleanly over the
frozen 12-service formal topology and yields exactly that command.

This is deliberately **not** modelled into `GatewayControlPlanV3`. Adding a
background term would make the plan depend on sample duration, which is the
opposite of a derived expectation. Health monitoring is not disabled either —
liveness still runs on the same interval, and readiness is proven explicitly by
the harness, outside the accounting window.

Still outstanding, and landing with Stage C4: the observer must reject a formal
v1.5 measurement whose running Gateway carries any other periodic healthcheck
command, and must bind that command into its runtime identity so the override
cannot be silently omitted.

## 2. The companion identity IS independently derivable

The Stage C3.6 stop condition does **not** apply. The derivation exists as a
production path; it simply needs more shared surface than the query compiler
alone.

Production builds the companion in two stages:

1. **Shape** — `internal/gateway/exposure.go:158` and
   `internal/queryplan/ordinal_program.go:308` produce `provenanceSQL` through
   `queryplan.Compile(provenancePlan, executionProduct)`. This is a pure
   function of the QueryPlan and the execution Product, and is directly
   reusable.
2. **Authorization** — `internal/gateway/query.go:894` passes that SQL through
   `engine.Authorize(sqlpolicy.Request{SQL: provenanceSQL, Grant: policyGrant,
   RowLimit: provenancePolicyRows})`, and `Decision.SQL` is what actually
   executes.

### The consequence for the two target digests

`internal/sqlpolicy/policy.go:112` renders the row limit **into** the executable
SQL. And `provenancePolicyRows` is runtime state: it derives from
`decision.RowLimit`, `exposureLedger.Limits.InfluenceFacts` and
`usesExpandedEvidence()`.

That splits the two digests Stage C3.5 requires:

| digest | depends on the runtime row limit? | derivable from frozen inputs alone? |
| --- | --- | --- |
| `strict_ast_sha256` | **no** — the limit is a constant, normalized to a placeholder | yes |
| `exact_rendered_sql_sha256` | **yes** | only by also reproducing the exposure-budget computation |

This is a useful asymmetry rather than a problem. Classification — the thing the
observer actually does — keys on `strict_ast_sha256`, which is stable across row
limits, so a budget difference cannot cause a misclassification. Only the
constant-pinning digest needs the budget.

### What Stage C3.6 must therefore share

For a fresh novel task the budget is deterministic from the Catalog budget
profile and the approved grant, so `exact_rendered_sql_sha256` is derivable —
but the shared package has to cover the exposure-ledger limit computation, not
just `queryplan.Compile`. A shared helper that stops at the compiler would
produce a shape-correct companion whose rendered digest never matches a live
run.

Concretely, `internal/physicalquery` must expose, for one operation:

- visible and companion physical SQL from `queryplan.Compile`;
- the authorized `Decision.SQL` for each, via `sqlpolicy.Authorize` with the
  row limits the exposure ledger derives;
- exact SHA-256 and strict AST SHA-256 of each;
- the plan/compiler/sidecar binding identity.

and the Gateway must delegate to it so a second evaluation implementation cannot
drift.

## Status

The compose override is delivered. The observer-side enforcement, the shared
physical-query package, and the C3.5 manifest hardening are not yet implemented.

## 3. Stage N1: the Attestation internal footprint is stable, and it scales with E

Measured against an isolated full 12-service topology with the observer-v3
override last, PostgreSQL 16.14, `track=all`, `track_utility=on`,
`track_planning=off`. Diagnosis only: `publication_eligible=false`,
`capability_changing=false`, `activation_support_changing=false`,
`formal_campaign=false`.

Fourteen trials covering both scopes (pool/preflight and transactional), both
relation kinds, cold and warm repetitions, and a two-entry ExpectedSchema.

### Stability — no stop condition

**`ATTESTATION INTERNAL FOOTPRINT NOT STABLE` does not apply.** Every trial at a
given ExpectedSchema produced an identical internal footprint: one structural
key `e5738df16502…`, with no variation across

- scope (preflight vs transactional),
- relation kind (plain view vs materialized),
- cold first invocation vs warm repetitions.

The comparison is over the exact multiset of structural keys and multiplicities,
never a union across repetitions.

### The earlier relation-kind hypothesis was wrong

The previous note speculated that the Result-heavy relation being materialized
explained why the Stage M-B readiness probe and the Stage B query window looked
different. **It does not.** Plain and materialized views produce the same
internal key and the same multiplicity. The `relkind` read observed in the
Stage M-B breakdown is a *top-level* statement inside the view-definition path,
not the internal statement at all.

All three observations reconcile under one rule, once each is normalized by the
number of Attestations it actually performed:

| observation | Attestations | internal statements |
| --- | --- | --- |
| Stage B query window (preflight + transaction) | 2 | 2 |
| Stage M-B explicit readiness (1 attestation, 3 top-level + 1 internal = 4) | 1 | 1 |
| Stage N1, every trial | 2 | 2 |

`dataconnector.New` itself attests once at construction, which is why each trial
contains two Attestations; the probe normalizes by that count rather than
reporting the raw delta.

### It scales with E — so the footprint must stay schema-qualified

At `E = 1` "one per Attestation" and "one per ExpectedSchema entry per
Attestation" are indistinguishable. The two-entry trial separates them:

| ExpectedSchema entries | top-level per trial | internal per Attestation |
| --- | --- | --- |
| 1 | 6 | 1 |
| 2 | 10 | 2 |

The internal statement is emitted once per `pg_get_viewdef` call, so the
multiplicity **is** proportional to E.

This means the arithmetic of the superseded rule `(P + S + Q) * E` was
empirically correct. What was unjustified was everything around it: naming the
class after one particular internal statement shape, asserting that identity as
a universal constant, and assuming the preflight and transactional scopes share
a footprint without measuring them separately. Those assumptions happened to
hold here; they were not evidence.

`AttestationFootprintV1` therefore remains the right shape, and its
`expected_schema_digest` field is load-bearing rather than decorative: a
footprint qualified for one ExpectedSchema is invalid for another, and must be
re-qualified whenever E changes.
