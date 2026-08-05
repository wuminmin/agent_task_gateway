# Committed harness evidence

Records about the repository's own verification, retained so a reviewer holding
the commit can check what a run actually proved rather than what its exit code
said.

This is **not** campaign evidence. Frozen publication campaigns are sealed into
`evaluation/final-v5-wsl2/evidence/`, which stays empty until an author completes
and validates one.

| File | What it records |
| --- | --- |
| `dbtest-suite-<commit>.json` | One complete DSN-enabled `go test -json` run, summarized and accepted by `evaluation/cmd/final-v5-dbtest-report`. Carries the package and test counts, every skip with its reason, the declared justification for each, the run's PostgreSQL image and Go toolchain, and the SHA-256 of the raw report it was derived from. `"accepted": true` means zero failed packages, zero failed tests and zero undeclared skips. |

A skip is a failure unless `evaluation/cmd/final-v5-dbtest-report` declares it,
with a reason that must match what the test actually printed. That rule exists
because three DB-backed tests — the pin domain-separation proof and both halves
of the strict-AST C3 gate the classifier rests on — skipped for months against a
harness that could run them, and every one of those runs still exited zero.
