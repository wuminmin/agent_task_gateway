# TKDE Revision Status

## Included in this branch

- Adopt the paper identity *TaskGate: Accounting and Controlling Cumulative
  Data Exposure in Agentic Database Systems* in the README, main manuscript,
  supplement, and revision plan.
- Reposition the article around task-scoped cumulative data-exposure
  accounting for autonomous database agents.
- Introduce the approved task tuple `T=(P,S,B,C)`, the three-dimensional
  exposure function `E(T,Q)`, and scoped safety/consistency properties.
- Distinguish TaskGate from database provenance, database access control, and
  agent/tool authorization without presenting those systems as substitutes.
- Add compact exposure-model and versioned-publication documents that
  distinguish a changing business source from an immutable task-bound fact
  namespace.
- Document reproducible protocols and blank result tables for database-control
  baselines, an adaptive salary-reconstruction attack, shared-root multi-agent
  settlement, and the 10K--100M bitmap/ledger/end-to-end matrix, in addition to
  the existing six experiment families.
- Link the new model, publication, baseline, attack, multi-agent, performance,
  formal, provenance, and experiment documents from the repository README.

## Deliberately not changed

- No accounting algorithm or runtime behavior is changed by this revision.
- No existing benchmark evidence is relabeled or modified.
- No new measurement is claimed. Every new result cell remains blank until an
  author executes the corresponding protocol locally and validates its raw
  artifacts.

## Pending author-side experiment execution

1. PostgreSQL direct-execution performance baseline.
2. PostgreSQL RLS semantic comparison over 100 adaptive legal queries.
3. Adaptive attack benchmark: pagination, equivalent SQL, retry, UNION
   splitting, and aggregation probing.
4. Non-competitive ProvSQL provenance-generation comparison.
5. Nested View DAG depth and join-graph scalability campaign.
6. Shared-root concurrency isolation at 10, 50, 100, and 500 queries.
7. PostgreSQL logging, Apache Ranger, and Snowflake masking/secure-view
   baseline deployments under the pinned comparison protocol.
8. The complete 10K, 100K, 1M, 10M, and 100M bitmap, durable-ledger, and paired
   PostgreSQL/TaskGate performance matrix.

## Acceptance status

The repository-positioning, terminology, model, publication, SQL-profile,
threat-model, and evaluation-methodology changes are complete. Existing
deterministic fixtures demonstrate accounting conservation for scripted
split/merge, pagination, retry, outcome-probing, delegation, and concurrency
cases. The stronger criterion that a completed end-to-end adaptive-agent
campaign empirically demonstrates resistance remains pending author-side
execution; it is not inferred from blank tables or the small deterministic
fixtures.
