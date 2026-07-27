# Control PostgreSQL ledger storage scaling

This campaign measures immutable fact growth through the production Control
PostgreSQL reservation and atomic settlement path. It creates three isolated,
fresh migrated schemas and, in each, grows one root ledger to 10, 100, 1,000,
and 10,000 release facts plus the same number of positive-output dependency
facts. Every cumulative novel settlement is followed by a new-query replay of
the identical observation, which must charge zero facts and leave physical
storage unchanged.

Run:

```sh
./evaluation/run-exposure-storage.sh
```

The runner uses an isolated `postgres:16-bookworm` container, drops only the
fresh schemas it created, and writes `results.json`. The report contains raw
trial points, medians and min--max trial ranges for settlement/fact-store time,
canonical payload bytes, and PostgreSQL table/index allocation. It also
attempts one fact beyond the 10,000/10,000 signed budget in every trial and
requires rejection with unchanged fact-row count.

This is a real production-ledger storage/settlement scaling experiment, not an
end-to-end Business PostgreSQL query benchmark or a live-agent utility study.
