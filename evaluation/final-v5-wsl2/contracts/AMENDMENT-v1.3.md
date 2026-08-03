# Final-V5 contract amendment v1.3

Previous release:
final-v5-contracts-v1.2

New release:
final-v5-contracts-v1.3

## Defect

`benchmark-v1-probe.sql` used the PostgreSQL reserved keyword `COLLATION` as an
unquoted CTE identifier. PostgreSQL 16.14 rejected the contract-indexed probe
before any Gateway query executed:

```
ERROR:  syntax error at or near "collation"   (SQLSTATE 42601)
LINE 3: collation AS (
        ^
```

`pg_get_keywords()` classifies `collation` as `T` — *reserved (can be function
or type name)* — so it is not usable as a bare CTE name. Only the Artifact path
executes this contract-indexed probe; every other experiment uses the separate
private binding probe. Contract verification checked the file's digest and
structure but never that it parses as SQL, so the defect survived three
releases.

## Correction

Rename only the internal CTE identifier from `collation` to `collation_info`,
at its single definition and its single use site. Two lines change:

```diff
-collation AS (
+collation_info AS (
```

```diff
-    'collation', (SELECT to_jsonb(collation) FROM collation),
+    'collation', (SELECT to_jsonb(collation_info) FROM collation_info),
```

The output JSON key remains `"collation"`. A CTE identifier is not observable in
the probe's result: it does not appear in the output, and renaming it cannot
change which rows are selected, how they are aggregated or ordered, or what the
fingerprint means.

## Discovery point

The defect was discovered during the first non-publication Artifact targeted
validation. Six requested cells were retained as failures. No formal Publication
Campaign had run.

## This is a syntax-only erratum

The change is a bare identifier rename inside one SQL file, made because
PostgreSQL cannot parse the statement at all. It is **not** an adjustment of a
workload, a statistical protocol, an acceptance rule or an expected value after
observing experimental results. No result had been observed, because no cell
ever reached the Gateway. Semantic equivalence is proven mechanically rather
than asserted: `TestBenchmarkProbeRenameIsSemanticsPreserving` executes the
quoted-original form and the released form against the same fresh benchmark
database and requires identical column names, identical JSON structure, an
identical canonical output digest, and an identical Dataset fingerprint.

## Unchanged

- Dataset logical content
- Probe output schema and meaning
- Products
- Publications
- Profile closures and IDs
- Query Contracts
- Oracles
- workload cells
- warmups
- measured samples
- statistics
- 160 MiB ceiling
- FactID
- Receipt
- Exposure settlement
- PENDING/AVAILABLE

The hash-locked protocol, workload manifest, acceptance rules and statistics
references are byte-identical to v1.2.

## Publication evidence affected

None. No publication-eligible sample was produced under v1.2.

## New gate introduced with this release

`evaluation/cmd/final-v5-contract-sql-check` executes every SQL and plan
artifact the Contract Index names — the dataset generator, the dataset probe,
every Direct SQL template rendered at every declared scale, and every BDG
template and JSON plan — against a real PostgreSQL 16 deployment and the
production compiler. It reads the Contract Index rather than a hand-written file
list, so a newly indexed artifact is covered automatically. It emits
`taskgate-final-v5-contract-sql-executability-v1`. A contract whose indexed SQL
cannot parse, execute or compile now fails validation, so this class of defect
cannot reach a release again.

## Execution status

v1, v1.1 and v1.2 are preserved for audit but superseded for Final-V5 execution
by v1.3.
