# SSB lowerability

Lowers the 13 Star Schema Benchmark (SSB) queries -- a star-schema reporting/BI
workload (Q1.1-Q4.3) -- through the production `internal/sqllowering` lowerer
against a synthetic five-table Catalog (`lineorder`, `customer`, `supplier`,
`part`, `dwdate`) whose products expose every SSB column with `count`, `sum`,
`min`, and `max` approved (the lowerer admits no other aggregate). `queries/`
holds the queries as published (comma joins); `queries-explicit-join/` rewrites
the same joins as explicit inner joins and changes nothing else. `results.json`
records, per query, whether it lowers and otherwise the first rejection the
lowerer reports. Regenerate with `make eval-ssb-lowerability`.
