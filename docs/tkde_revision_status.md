# TKDE Revision Status

## Included in this branch

- Reposition the article around task-scoped cumulative data-exposure
  accounting for autonomous database agents.
- Introduce the approved task tuple `T=(P,S,B,C)`, the three-dimensional
  exposure function `E(T,Q)`, and scoped safety/consistency properties.
- Distinguish TaskGate from database provenance, database access control, and
  agent/tool authorization without presenting those systems as substitutes.
- Document reproducible protocols and blank result tables for all six requested
  experiment families.
- Link the formal model, provenance boundary, and experiment guide from the
  repository README.

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
