# TaskGate TKDE Revision Implementation Plan

## Target Paper Identity

New title:

> TaskGate: Accounting and Controlling Cumulative Data Exposure in Agentic Database Systems

The repository should no longer be positioned as a generic Agent Gateway.
The primary research contribution is:

> Controlling cumulative data exposure caused by autonomous database agents through task-scoped exposure accounting.

## Phase 0 — Repository Positioning

Align documentation, naming, and explanations with the TKDE positioning.

### README.md

Replace the opening description with:

> TaskGate is a research prototype for cumulative data exposure accounting and control in agentic database systems.

The first paragraph must emphasize autonomous AI agents, database access,
cumulative exposure, task-scoped accounting, and deterministic enforcement.
MCP transport, tool routing, and API-gateway concerns are implementation
details rather than the research contribution.

## Phase 1 — Rename Concepts

Use the following documentation and API-facing terminology without renaming Go
packages, compatibility identifiers, environment variables, executable paths,
or wire fields:

| Old | New |
|---|---|
| Gateway | TaskGate Enforcement Layer |
| Query Accounting | Exposure Accounting |
| Query Count | Exposure Event |
| Result Tracking | Exposure Ledger |
| Agent Request | Agent Task Execution |

This mapping concerns conceptual documentation. Literal compatibility fields,
environment variables, executable paths, and measured baseline labels retain
their exact names when changing them would be inaccurate or incompatible. In
particular, the ordinary query-count resource guard remains separate from
exposure events, result-artifact lifecycle tracking is not the Exposure
Ledger, and a transport-level Agent request is not automatically a successful
Agent Task Execution.

## Phase 2 — Introduce the Exposure Model

Create `docs/exposure-model.md` and define the exposure space. A database fact
has identity

```text
F = (snapshot, entity, attribute, version)
```

A task maintains three ledger dimensions:

```text
Ledger(T):
  L_release
  L_dependency
  L_outcome
```

A query contributes only its positive set difference from the root-family
ledger:

```text
Delta(T, q) = Exposure(q) - Ledger(T)
```

Only novel facts consume the corresponding exposure budget.

## Phase 3 — Versioned Enterprise Publication

Create `docs/versioned-publication.md`. Explain that business data continues to
change; TaskGate does not freeze the operational business database. Each
governed reporting cut creates a new immutable publication and fact-identity
namespace:

```text
Business Database
        |
Publication Compiler
        |
Fact Identity Layer
        |
Exposure Bitmap Version
```

For example, a Day 1 customer publication may contain one million rows and a
Day 2 publication 1.05 million rows. Snapshot changes create new publication
versions, historical tasks remain bound to their original version, and new
tasks bind the newest governed publication.

## Phase 4 — Baseline Evaluation Documentation

Create `docs/evaluation-baseline.md` with a reproducible comparison framework:

- B1 PostgreSQL audit logging measures query events and accessed relations but
  does not compute cumulative delivered facts.
- B2 PostgreSQL row-level security enforces access permission but does not
  maintain a cumulative task ledger.
- B3 Apache Ranger provides authorization and auditing controls; evaluate its
  documented control objective separately from semantic result exposure.
- B4 Snowflake dynamic data masking and secure views control visible values and
  access paths but do not provide TaskGate's root-task three-set ledger.
- TaskGate measures release, positive-output dependency, and outcome exposure.

Do not present unlike systems as performance substitutes, and do not fabricate
measurements.

## Phase 5 — Adversarial Agent Evaluation

Create `docs/adversarial-agent-evaluation.md`. Evaluate an adaptive agent that
tries to reconstruct a sensitive dataset such as `customer_salary` through a
sequence of individually legal aggregate queries:

```sql
SELECT avg(salary) FROM customer_salary WHERE region = 'A';
SELECT avg(salary) FROM customer_salary WHERE region = 'B';
-- ... adaptive Query N
```

Compare an unmetered control with TaskGate's withhold-before-settlement path.
Report queries until block, facts exposed, and attack success rate. A budget
failure must deny release; it must not be described as eliminating every
inference channel.

## Phase 6 — Multi-Agent Evaluation

Create `docs/multi-agent-evaluation.md` for delegated agents sharing one root
task and budget:

```text
              Root Task
              /       \
         Agent A     Agent B
```

Verify the shared ledger, absence of double spending, monotonic permission
attenuation, and CAS-conflict recovery.

## Phase 7 — Performance Evaluation

Document campaigns at 10K, 100K, 1M, 10M, and 100M facts. Measure:

- bitmap `ANDNOT`, union, and cardinality;
- ledger-update latency and throughput;
- query-path overhead for PostgreSQL alone versus PostgreSQL plus TaskGate.

Keep kernel, control-ledger, and end-to-end measurements separate. Mark scales
without completed artifacts as planned rather than measured.

## Phase 8 — SQL Capability Positioning

Do not make the unqualified claim that TaskGate "supports SQL." Use:

> TaskGate defines a controlled analytical SQL profile.

Full SQL is intentionally unsupported to prevent semantic ambiguity, close
exposure-accounting bypasses, and preserve deterministic compilation. Document
the exact accepted profile and fail-closed boundary.

## Phase 9 — Security Threat Model

Update `docs/threat-model.md` with explicit treatments of:

- query-accumulation attacks: many legal queries attempt to reconstruct
  sensitive information; the cumulative exposure ledger bounds modeled facts;
- retry amplification: `(task_id, request_id)` idempotency prevents an exact
  retry from becoming a second execution or spend;
- multi-agent budget sharing: delegated descendants settle against the same
  root-family ledger.

Retain residual risks, including denial-bit leakage, cross-root accumulation,
background knowledge, and unsupported inference channels.

## Phase 10 — Paper-Oriented README Structure

Order the README around:

1. Problem — autonomous agents create cumulative database exposure risk.
2. Key Insight — access permission is insufficient; exposure accounting is
   required.
3. TaskGate Model.
4. Architecture.
5. Exposure Ledger.
6. Enforcement.
7. Evaluation.
8. Limitations.
9. Production Gap.

Operational setup may follow the paper-oriented overview.

## Code Change Restrictions

Do not rewrite the core accounting algorithm, remove the bitmap
implementation, change the database schema without a migration, or break API
compatibility. Focus on documentation, the experiment framework, terminology,
and reproducibility.

## Commit Plan

1. `docs: Reposition TaskGate as exposure accounting system`
2. `docs: Add cumulative exposure model`
3. `docs: Add evaluation methodology`
4. `docs: Strengthen threat model`

## Final Acceptance Criteria

The README title is:

> TaskGate: Accounting and Controlling Cumulative Data Exposure in Agentic Database Systems

A reviewer should immediately understand that existing access-control
mechanisms do not solve cumulative exposure, autonomous database agents require
task-scoped accounting, TaskGate provides deterministic enforcement for its
declared profile, and the evaluation tests adaptive-query resistance without
overclaiming general inference control.
