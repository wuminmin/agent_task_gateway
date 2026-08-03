# Artifact targeted validation is blocked by the frozen dataset probe

Stage B.2b attempted the non-publication Artifact targeted validation of the six
frozen `artifact/result-heavy/*` cells. All six cells failed identically, before
any query reached the Gateway. The cause is a defect in a contract-frozen file,
not in the harness.

## What happens

`evaluation/final-v5-wsl2/sql/datasets/benchmark-v1-probe.sql` opens with

```sql
WITH
collation AS (
    SELECT collname, collprovider, collversion,
           pg_collation_actual_version(oid) AS actual_version
    FROM pg_collation
    ...
```

`COLLATION` is a **reserved** PostgreSQL keyword, so it cannot be used as a bare
CTE name. Confirmed on the deployment this stage ran (PostgreSQL 16.14):

```
=> SELECT word, catcode, catdesc FROM pg_get_keywords() WHERE word='collation';
 collation | T | reserved (can be function or type name)

=> WITH collation AS (SELECT 1 AS x) SELECT x FROM collation;
ERROR:  syntax error at or near "collation"
LINE 1: WITH collation AS (SELECT 1 AS x) SELECT x FROM collation
             ^

=> WITH "collation" AS (SELECT 1 AS x) SELECT x FROM "collation";
 1
```

The Artifact adapter runs this probe first, in
`artifactAdapter.verifyDatasetProbe`, so every cell fails at
`ERROR: syntax error at or near "collation" (SQLSTATE 42601)` and is retained as
`artifact_measurement_failed`.

## Why it was not caught earlier

Only the Artifact path executes the *contract-indexed* probe through
`Runtime.DatasetProbeSQL()`. Every other experiment uses the separate private
binding probe, `finalv5binding.DatasetProbeSQL()`. The contract-indexed probe
had never been executed against a live PostgreSQL: `artifactRealSystemValidated`
was `false`, and this stage was the first targeted run. Contract verification
checks the file's digest and its structure, never that it parses as SQL.

## Why it is not fixed here

The file is pinned by SHA-256 in the frozen contract index:

```
evaluation/final-v5-wsl2/contracts/index-v1.json
  {"kind": "dataset_probe", "path": "sql/datasets/benchmark-v1-probe.sql",
   "sha256": "d739dc754ad497848ebbf09c6b97331342f83d4956c15a69f0dc2fe52e7276f0"}
```

The blob is identical in `final-v5-contracts-v1`, `v1.1`, `v1.2` and at HEAD
(`ade4ae4e90773cc9aacc749c0e7ae4bcc129b7a9`). Quoting the CTE would change the
file digest, the contract index digest and therefore the contract release. This
stage is explicitly forbidden from modifying contract v1.2 or creating a v1.3,
so the fix is an author decision, not something to take unilaterally.

Working around it in the adapter — skipping the probe, substituting the private
binding probe, or tolerating the error — would remove the deployment
fingerprint from the Artifact evidence. That is a weakening, and this stage is
equally forbidden from weakening a check to finish the task.

## The fix, when it is authorized

Quote the CTE (`WITH "collation" AS (...)`, and `FROM "collation"` at its use
site) or rename it, then recompute the file digest, the contract index digest
and every transitive digest, and freeze the result as a new contract release
with an amendment record. Nothing else in the probe needs to change: the query
is otherwise valid, and quoting an identifier does not change its meaning.

A regression test should execute every contract-indexed SQL file against a live
PostgreSQL parser, so a contract can never again freeze SQL that cannot parse.
