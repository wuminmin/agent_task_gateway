# RQ5 compact offline evidence

`scale-20260730-final3/` is the source-controlled compact record of the
345,000-row offline publication campaign. It retains the original 79 campaign
JSON files, all 16 bundle manifests, an exact archive of the 33 source files
bound by the original result, and explicit environment and omission records.
HOT, COLD, and sidecar payload bytes are intentionally omitted.

The authoritative validation command is:

```sh
PYTHONDONTWRITEBYTECODE=1 \
  python3 evaluation/daily-publication/evidence/validate.py
```

Use `--json` to print the independently recomputed canonical summary. Python
consumers, including paper generators, should import and call:

```python
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path("evaluation/daily-publication/evidence").resolve()))
import validate

summary = validate.validate_pack(path)       # sealed pack + recomputation
summary = validate.recompute_canonical(path) # primary evidence only
```

Because the repository path contains `daily-publication`, it is not a regular
Python package name. `validate_pack()` is the authoritative API; paper code
must not read `results.json` or `canonical-offline.json` directly.

The stable paper-facing fields are:

- `workload.rows_per_publication`
- `workload.facts_per_publication`
- `metrics.maximum_cycle_ms`
- `metrics.maximum_build_ms`
- `metrics.maximum_strict_verify_ms`
- `metrics.maximum_activation_ms`
- `metrics.maximum_builder_peak_rss_bytes`
- `metrics.artifact_bytes_by_day`
- `metrics.hot_artifact_bytes_by_day`
- `days[*].summary`
- `days[*].bundle_manifest_sha256`
- `days[*].determinism.bundle_manifest_exact_bytes`
- `rq5_overall_status`

The validator checks every pack hash, exact source-archive member, candidate and
approved input, calibration binding, phase executable and argv, reconstructable
stdout, receipt body and file digest, bundle measurement, transport descriptor,
artifact limit, sample identity, Type-7 summary, and offline gate. It requires
byte-identical bundle manifests and identical transport descriptors across the
calibration plus three measured builds for each day.

## Audit boundary

The compact pack lists all 48 omitted binary occurrences with path, byte count,
role, and SHA-256 in `transport-omissions.json`. A fresh clone can validate the
retained receipt/bundle/descriptor chain but cannot independently re-hash
payload bytes. The pack manifest is anchored by the Git or release object that
contains it; it is not an external signature.

The original run-bound source map omitted `db/init/00-schema.sql` and the two
`*_test.go` files executed by the Dockerfile. Their exact bytes are retained in
`post-run-supplemental-source.tar`, explicitly as post-run supplemental
evidence—not as files covered by the historical run-bound hash.

The campaign did not retain cgroup `memory.max`, `memory.peak`, swap, or OOM
events, nor a cryptographic dataset-to-phase binding. The recorded 6 GiB limit
is the archived Compose declaration. RSS is direct-child `v4-offline` VmHWM,
and cycle time is the sum of three child intervals; neither is a full-system or
container-startup-inclusive measurement. Caches were not reset. These
limitations are preserved in `canonical-offline.json` and cannot be converted
into passing evidence.

Offline completion does not establish version-routed task binding, replay/cache
separation, ledger isolation, delegation behavior, or online transition
latency. Therefore this pack's `rq5_overall_status` remains
`incomplete_without_online_transition_evidence`.

Run the adversarial unit tests with:

```sh
PYTHONDONTWRITEBYTECODE=1 \
  python3 -m unittest evaluation/daily-publication/evidence/test_validate.py
```

`seal.py` is a one-time, fail-on-overwrite materializer for this historical
pack. It contains post-run observations and must not be used to describe a new
campaign or machine.
