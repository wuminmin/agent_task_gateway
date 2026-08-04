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
