# TKDE Revision Plan

Title:

TaskGate: A Task-Scoped Data Exposure Accounting Framework for Autonomous Database Agents

## Positioning

TaskGate is positioned as a database execution semantics framework for autonomous agents, not an API gateway or authorization proxy.

Core contribution:

- Task-scoped exposure accounting
- Provenance-aware fact identity
- Exposure-preserving relational compilation
- Adaptive multi-query agent accounting

## Formal model updates

The paper should introduce:

- Task model T=(P,S,B,C)
- Exposure vector:
  - release exposure
  - positive-output dependency footprint
  - query outcome exposure
- Budget safety property
- Canonical replay consistency
- View expansion preservation

## Terminology

Replace "influence" with "positive-output dependency footprint" where possible to avoid confusion with causal influence.

## Related work

Add discussion of:

- Database provenance systems including ProvSQL
- RLS/VPD/ABAC access control
- Data governance systems
- MCP and agent authorization systems

Key distinction:

Existing provenance systems explain query result origin. TaskGate maintains cumulative task-level exposure accounting across adaptive agent interactions.

## Evaluation plan (data to be filled by authors)

The following experiments should be completed locally:

1. PostgreSQL direct execution baseline
2. PostgreSQL RLS comparison
3. Agent adaptive attack benchmark
4. Provenance comparison with ProvSQL
5. Nested View DAG scalability

Benchmark result tables are intentionally left empty in this revision branch.
