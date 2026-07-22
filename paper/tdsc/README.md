# TaskGate TDSC draft

Build the authoritative paper from the repository root with:

```sh
make paper
```

This delegates to the paper-local container wrapper, which supplies
`IEEEtran`:

```sh
paper/tdsc/build-container.sh
```

The wrapper mounts the repository at `/workspace` and runs the container as the
calling user, so the generated tables and PDF remain ordinary workspace files.
For a host-TeX syntax build, use `paper/tdsc/build.sh`.

Both public paper entrypoints first run
`evaluation/generate-artifacts.sh --allow-empty`, reconstructing
`evaluation/generated/paper-results.json` from raw evidence rather than trusting
an existing generated summary.  Each path performs that reconstruction exactly
once: `make paper` delegates directly to `build-container.sh`, and the container
then executes the internal compile-only `compile.sh`.  The host `build.sh`
regenerates once and invokes the same compile-only step locally.  Consequently,
even the host-TeX entrypoint requires Docker for evidence reconstruction;
`compile.sh` alone is not an authoritative evidence build.

Before LaTeX, `compile.sh` also runs `verify_manifest_vector.py`.  That
independent Python check must reproduce the digest in the shared
`testdata/authorization-manifest-v1-vector.json` fixture; the OA protocol test
reads the same fixture, and the Go approval package has a matching fixed-vector
canonicalization test.  A mismatch stops the paper build.

`generate_tables.py` never supplies fallback measurements. It reads the
source-backed summary `evaluation/generated/paper-results.json`, schema version
1. The summary must list every raw input as a repository-relative path plus a
lowercase SHA-256 under `provenance.raw_inputs`. The generator rehashes every
listed file; an absent file, malformed path, digest mismatch, or incomplete
record makes the affected generated result say `not measured`. Performance rows
come from `performance.summary[]` and require an experiment, baseline,
concurrency, at least 30 measurements, positive throughput, and ordered
p50/p95/p99 latency.
Rows enter the manuscript table only for the four declared baselines, SF1/SF10,
concurrency 1/8/32, and at least 30 measurements; smoke-run output therefore
remains a pipeline check rather than a paper result.

The evaluation pipeline, not the paper directory, owns that summary and its raw
inputs.  Public paper builds always reconstruct the summary before rendering
these tables.

Formal evidence is independent of benchmark availability. If
`formal/results/tlc.json` exists, the paper generator verifies the recorded
SHA-256 values for the TLA+ model, configuration, and complete TLC log before
reporting the run status.  A claimed pass additionally requires exactly one
recognizable TLC no-error completion marker, final zero-queue state counts, and
search depth in the verified log.  Parsed values must agree with `tlc.json`;
missing, ambiguous, or inconsistent statistics cannot fall back to JSON values
and remain `not measured`.

Security evidence is fail-closed in the same way. A reported partial or full
campaign must come from `evaluation/security/results.json`; that result and
every path/digest pair in its evidence manifest must also occur in the
top-level, rehashed provenance. The paper generator checks the three declared
fuzz targets, per-target and aggregate CPU time, the 24-CPU-hour publication
threshold, and acceptance-count consistency. A verified partial campaign can
populate measurements it actually made, but nullable connector-crossing or
budget-fault counts remain `not measured` and the campaign is not presented as
a pass.

The preferred build uses `IEEEtran.cls`. A two-column `article` fallback exists
only so a minimal environment can syntax-check the manuscript. A submission PDF
must be built with IEEEtran; the paper container installs it from Debian's
`texlive-publishers` package.
