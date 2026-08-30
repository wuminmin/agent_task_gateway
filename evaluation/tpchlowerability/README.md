# TPC-H lowerability of the closed reporting fragment

`queries/q01.sql`--`q22.sql` are the 22 TPC-H query templates (TPC-H
specification, validation substitution parameters, transcribed by hand; the
text is TPC-derived and is not an official TPC-H run). `go run
./evaluation/cmd/tpch-lowerability` lowers every query through the production
`internal/sqllowering` package against a synthetic eight-table Catalog whose
products expose every TPC-H column with `count`, `sum`, `min`, and `max` (the lowerer admits no other aggregate)
approved, and writes `results.json`: for each query whether it lowers into the
closed fragment as written and, if not, the first rejection code, reason, and
clause the lowerer reports. The lowerer stops at the first rejection, so the
reason is the first construct encountered, not the only unsupported one.

`queries-explicit-join/` holds the same 22 templates after one purely
syntactic normalization: the 13 comma-join queries are rewritten to explicit
`JOIN ... ON` with their equality predicates (no other change); the other nine
are byte-identical copies. The program lowers both directories and reports
each pass separately, so the reader can separate the FROM-shape rejection
(a syntactic form the lowerer marks retryable-after-rewrite) from the first
semantic construct outside the fragment.
