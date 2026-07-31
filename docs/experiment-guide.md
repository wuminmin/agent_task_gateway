# TKDE Experiment Guide

Benchmark result tables are intentionally left empty. Authors should run experiments locally and fill results.

## 1. PostgreSQL Baseline

Compare direct PostgreSQL execution against TaskGate.

Measure:

- query latency
- throughput
- accounting overhead

## 2. PostgreSQL RLS Comparison

Demonstrate the difference between row authorization and task-level cumulative exposure accounting.

Workload:

- repeated adaptive agent queries
- shared task budget

## 3. Agent Attack Benchmark

Recommended attacks:

- pagination replay
- equivalent SQL rewriting
- retry duplication
- UNION splitting
- aggregation inference

Metrics:

- blocked attacks
- false acceptance
- exposure accounting correctness

## 4. Provenance Comparison

Compare with provenance systems such as ProvSQL.

Goal:

Show that provenance tracking and task-level exposure accounting solve different problems.

## 5. Nested View DAG Scalability

Vary:

- view depth
- join graph size
- dependency edges

Measure:

- compilation latency
- canonicalization overhead
- correctness
