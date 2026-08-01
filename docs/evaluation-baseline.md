# Baseline evaluation methodology

This document defines a comparison protocol for cumulative data-exposure
control. It is a methodology, not a report of completed experiments. No number
from the existing four-path performance runner should be transferred into the
tables below: its `native_view` path is not PostgreSQL row-level security (RLS),
and its `resource_taskgate` path does not execute the full exposure ledger.

The baseline identifiers in this document are local to this protocol.

## Comparison boundary

The systems have different control objectives. The first comparison is
therefore qualitative and configuration-specific:

- a **control objective** states what the configured mechanism decides or
  records;
- an **empirical baseline** is a real deployment that executes the frozen
  workload and emits raw evidence; and
- the **independent exposure oracle** measures the same Release, Dependency,
  and Outcome FactID sets for every successfully released result. It is a
  measurement tool, not another enforcement system.

Do not turn the absence of a TaskGate-format ledger in an experiment into a
product-wide claim. The defensible statement is that the tested baseline
configuration does not maintain or enforce the experiment's task-scoped FactID
budget. Likewise, TaskGate is not a replacement for audit logging, row
authorization, masking, or secure views; those controls can be composed with
exposure accounting.

## Common workload and fairness controls

Use a versioned, immutable logical dataset and a fixed non-owner analyst
identity. The workload must contain projection/filter, join, grouped aggregate,
nested-view, and adaptive-query traces. Every statement in the primary trace
must be legal under the configured row and column policy. Keep a policy-denied
query as a separate negative control.

For each empirical arm:

1. Pin the database/service version, policy bytes, role grants, dataset import
   digest, query trace, parameters, and adaptive-policy seed.
2. Use the same logical rows, columns, role scope, query semantics, and complete
   result-drain boundary. Cross-engine SQL may differ, but canonical result
   multisets must match before a security comparison is accepted.
3. Preserve every concrete query, decision, result digest, error, and audit
   record. Derive FactID exposure with the same independent oracle; a baseline
   without a ledger has unknown exposure until the oracle measures it, not zero
   exposure.
4. Use a fresh session and, for TaskGate, a fresh root task per paired
   replicate. Randomize arm order and run at least three fresh-deployment
   replicates.
5. Give every arm sufficient ordinary query, row, and time limits. The bounded
   TaskGate arm alone receives a predeclared cumulative exposure budget below
   the oracle's full-trace effect.

Cross-engine latency is not a fair overhead comparison. Report security
outcomes for all arms, but restrict latency-overhead claims to the paired
PostgreSQL and PostgreSQL-plus-TaskGate experiment in
[performance-evaluation.md](performance-evaluation.md).

## B1 -- PostgreSQL audit logging

### Control objective

Configure PostgreSQL 16 server logging to retain executed statements with
session identity and timing. A reproducible configuration may use
`logging_collector=on`, `log_destination=jsonlog`, `log_statement=all`, and a
prefix/application name that identifies the experimental arm. Record the
effective settings from `pg_settings`; do not rely only on the configuration
file. PostgreSQL warns that statement logs can contain sensitive values, so the
evidence store must be access controlled and sanitized before publication.

The empirical measurements are:

- executed query count and completion/error status;
- syntactically referenced relations, derived by a version-pinned SQL parser
  from the logged statement; and
- statement duration when enabled and correctly correlated with the statement.

Call the second metric **referenced relations**, not rows or facts accessed.
Statement text does not by itself identify the rows inspected, the facts
released, or the positive-output dependency set. If sampled logging is used,
the arm is invalid for exact query counting.

Official behavior reference: [PostgreSQL 16 error reporting and
logging](https://www.postgresql.org/docs/16/runtime-config-logging.html).

### Empirical setup

Run the trace as the restricted analyst role. Save the exact log interval,
PostgreSQL log sequence/session identifiers, parser version, and the derived
query-to-relation mapping. Check that the number of workload completions equals
the number of correlated statement records. Apply the independent oracle to
the returned results; do not attempt to infer FactIDs from the log alone.

The tested configuration observes activity. It does not reject a legal query
because earlier legal queries cumulatively crossed the experiment's FactID
budget.

## B2 -- PostgreSQL row-level security

### Control objective

Install a real RLS policy with `ALTER TABLE ... ENABLE ROW LEVEL SECURITY` and
`CREATE POLICY`; execute as a non-owner role without `BYPASSRLS`. Capture
`pg_policies`, `pg_class.relrowsecurity`, effective user, role membership, and
all relevant grants. A reporting view, predicate in application SQL, or
TaskGate scope injection must not be labeled PostgreSQL RLS.

RLS is evaluated per statement to restrict which rows a role may operate on.
The empirical baseline asks whether every query in the legal trace is allowed
and what cumulative effect those allowed results have. The independent oracle,
not RLS metadata, supplies the latter measurement.

Official behavior reference: [PostgreSQL 16 row security
policies](https://www.postgresql.org/docs/16/ddl-rowsecurity.html).

### Empirical setup

Use exactly 100 individually legal adaptive queries, as specified in
[experiment-guide.md](experiment-guide.md#2-postgresql-rls-comparison-with-100-adaptive-legal-queries).
Record the prefix exposure vector after each result. Include an unlimited
TaskGate validity control whose results must match the RLS arm, and a bounded
TaskGate arm that uses the same scope and trace.

The primary distinction is not that RLS is weak. RLS answers row authorization;
the tested TaskGate configuration additionally asks whether the next query's
novel cumulative Release, Dependency, and Outcome effect fits one approved
root-family budget.

## B3 -- Apache Ranger

### Control objective

Deploy a pinned Apache Ranger release with one explicitly named,
Ranger-supported execution service and plugin. Record both Ranger and engine
versions. Configure table/column permissions and, if used, row-filter or data-
masking policies that implement the common analyst scope. Retain the native
access-audit fields, including resource, action, allow/deny decision, actor,
time, and policy identifier.

Ranger's policy model supports resource authorization and access audit, with
service-dependent row-filter and masking capabilities. The experiment must
state which of these capabilities is actually enabled; it must not generalize
from one plugin to every Ranger integration.

Official behavior reference: [Apache Ranger policy
model](https://ranger.apache.org/blogs/policy_model.html).

### Empirical setup

Load the canonical dataset into the selected engine, validate canonical result
hashes, then execute the same logical trace. Preserve the policy export and raw
Ranger audit records. Measure allowed/denied requests and audited resources.
Compute R/D/O FactID prefixes independently from results.

The baseline is considered non-cumulative only if its tested policy and any
adjacent enforcement component have no stateful, task-level FactID budget. Do
not describe Ranger generally as unable to audit or mask data, and do not call
its resource audit a semantic result-exposure ledger.

## B4 -- Snowflake masking and secure views

### Control objective

Use a pinned Snowflake account configuration and a least-privilege analyst
role. Exercise two sub-arms when the account supports them:

- **B4a, Dynamic Data Masking:** attach a masking policy to the sensitive
  column and verify the value visible under the analyst's execution context.
- **B4b, secure view:** grant the analyst the secure view and no direct access
  to its base tables; record `IS_SECURE`, grants, and the view definition's
  owner-visible digest without publishing protected SQL.

Dynamic Data Masking is documented as applying masking policy expressions to
protected table/view columns at query time. Secure views protect underlying
data and restrict exposure of view definitions. These are per-query data-
presentation and object-access controls, not labels for the TaskGate ledger.

Official behavior references: [Snowflake Dynamic Data
Masking](https://docs.snowflake.com/en/user-guide/security-column-ddm-intro) and
[Snowflake secure views](https://docs.snowflake.com/en/user-guide/views-secure).

### Empirical setup

Record account edition/features, region, warehouse size, role hierarchy,
grants, masking policy, secure-view metadata, query IDs, and query-history
records. Run canonical-result checks on the values the analyst is intended to
see. For intentionally masked fields, compare against a predeclared masked
result oracle rather than the unmasked PostgreSQL output.

Use the independent FactID oracle to measure facts represented by successful
results. In the comparison, state only that the tested B4 policy has no added
component that accumulates those FactIDs under the experiment's root-task
budget.

## Proposed system -- TaskGate

The TaskGate arm must use the full exposure-enabled execution path:
authorization, visible/provenance execution, deterministic fact derivation,
root-history set difference, budget check, atomic three-dimensional settlement,
receipt, and result release. The measured dimensions are:

- **Accounted result exposure** (wire label `release`): result facts selected
  for publication and charged at settlement, whether or not promotion later
  reaches `AVAILABLE`;
- **Dependency exposure:** the contract-defined positive-output dependency
  footprint (`influence` in compatibility fields); and
- **Outcome exposure:** canonical query-outcome facts.

Repeated facts are charged only on first exposure within the approved root
family. A request that would exceed any dimension must release no result and
must leave no committed ledger/observation mutation, canonical/`AVAILABLE`/
deliverable artifact, or success receipt. A failure audit and FAILED receipt
are expected. Record transient private staging and classify any best-effort
cleanup failure; an orphan staging object must remain unreachable through
normal result APIs.

`resource_taskgate` in `evaluation/cmd/runner` is not this arm. The advanced
`execute_plan` evaluation path and exposure-enabled `query_sql` share the full
accounting chain, but the former is retained for deterministic harnesses and is
not advertised as an ordinary agent tool.

## Metrics and blank comparison table

Report exact counts for security outcomes and keep latency separate unless the
arms share an engine and measurement boundary.

For an arm without a native cumulative budget, compute "results released after
budget crossing" against the bounded TaskGate arm's predeclared vector using
the independent-oracle prefix curve. This is a counterfactual comparison
threshold, not a rejection that the baseline claims to implement.

| Arm | Legal queries released | Policy denials | Referenced/audited resources | Unique release facts | Unique dependency facts | Unique outcome facts | First cumulative-budget rejection | Results released after budget crossing |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| B1 PostgreSQL logging | | | | | | | N/A | |
| B2 PostgreSQL RLS | | | | | | | N/A | |
| B3 Apache Ranger | | | | | | | N/A unless configured | |
| B4a Snowflake masking | | | | | | | N/A | |
| B4b Snowflake secure view | | | | | | | N/A | |
| TaskGate, unlimited validity control | | | | | | | N/A | |
| TaskGate, bounded exposure budget | | | | | | | | |

`N/A` means that the arm has no cumulative-budget rejection event in the
declared experimental configuration. It does not mean that its measured
exposure is zero.

## Repository support and commands

The current repository supplies semantic preconditions and a direct
PostgreSQL/full-path foundation:

```sh
make eval-validate
make eval-exposure
```

The first validates the existing four-path workload configuration without
contacting a backend. The second runs deterministic exposure semantics and a
real PostgreSQL oracle. Neither command executes B1--B4 above.

`make eval-smoke` and `make eval-full` currently use `native_view` and
`resource_taskgate`; their values must not fill this document's RLS or full-
TaskGate rows. No checked-in runner currently provisions PostgreSQL statement
logging, real RLS, Apache Ranger, or Snowflake as this paired baseline matrix.
Until such a runner exists, retain blank result cells and publish the
methodology as planned work.

## Evidence acceptance

A baseline cell is publishable only when it includes:

- immutable policy/configuration bytes and effective-policy introspection;
- environment, engine, plugin, account-edition, and dataset manifests as
  applicable;
- exact query trace and adaptive-policy digest;
- per-request decision, complete-result hash, and raw native audit record;
- independent-oracle R/D/O sets and prefix digests;
- fresh-root TaskGate receipts, ledger snapshots, and rejection evidence; and
- a limitations statement describing the measured configuration rather than
  making unsupported product-wide claims.
