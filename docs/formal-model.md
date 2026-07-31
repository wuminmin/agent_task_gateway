# TaskGate Formal Model (TKDE Revision Draft)

## Task

A task represents a human-approved execution boundary for an autonomous database agent.

T = (P, S, B, C)

where:

- P: approved data products
- S: bound reporting snapshot
- B: exposure budget vector
- C: semantic constraints

## Exposure Vector

TaskGate models exposure as a three-dimensional vector:

- release exposure: facts delivered to the agent
- positive-output dependency footprint: facts contributing to visible outputs
- query outcome exposure: query-derived information exposure

## Budget Safety

For every accepted execution sequence:

Exposure(T, Q1...Qn) <= B(T)

The system rejects executions that exceed any approved exposure dimension.

## Canonical Query Semantics

Equivalent relational queries are normalized into canonical QueryPlans. Exposure accounting is performed on canonical plans rather than raw SQL strings.

## View Closure

Supported semantic views are expanded into an exposure-preserving relational representation. Expansion must preserve:

- query result semantics
- fact identity semantics
- exposure accounting semantics
