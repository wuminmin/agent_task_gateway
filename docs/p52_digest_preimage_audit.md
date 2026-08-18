# P52 Scale history digest preimage audit

Date: 2026-08-18
Start HEAD: `b97a226824295c29805fd0f44b27c8f42ec315da`
Scope: zero campaign; one disposable PostgreSQL 16.14 diagnostic; no `internal/` change.

## Retained P51 observation

`raw/p51-mech-full-11` reached six profiles before Scale: `analytics-orders`
18/18, `analytics-orders-lineitem` 10/10, `attack-expense-detail` 13/13,
`depth4-semantic-view` 3/3, `expense-detail` Attack 2/2 plus RQ5 2/2, and
`exposure-scale` Baseline 15/15. Scale then retained 12 novel failures with
`dependency_history_prefill_failed` and 12 invalid replays with
`semantic_replay_lacks_novel_anchor`; no Scale row contains v3 acceptance or v1
rejection. The failure stage is `cells_scale`, and no retry was performed.

For the first history prefill (`10k-overlap-0`) the exact field comparison is:

| Field | Frozen binding | Retained live observation | Interpretation |
| --- | --- | --- | --- |
| rows | 1 | 1 | equal |
| columns | 1 | 1 | equal |
| SQL numeric value | `782130.00` | `782130.00` | equal |
| result digest | `00e4f5977565ab625bd37beb541c855edd4f70e9f0f950dfbaf9d24f8b5d54ea` | `d9a06a3ef0d26534e90191534cf3223530bc982e724d50e6ada4ea4fe77395b0` | different encodings of the equal value |
| dependency facts | 10000 | 10000 | equal |
| dependency identity | semantic `98a92106ae1b9ac9379cad0e3977cf9985b19ece54b49e117651d4a0efcfeada` | production hybrid `437dc81c61ea97c4ab358bf166d4d2bc57158d39b41a2c6b81ec6d4ec6d7f4a7` | intentionally different domains, linked below |

The raw sample is
`evaluation/final-v5-wsl2/raw/p51-mech-full-11/deployments/exposure-scale/001/raw/scale.jsonl`.
Its adapter stderr has 12 lines, all the same bound rows/columns/result/cardinality
error. The credential scan reports URL-userinfo / PEM / secret-assignment /
JSON-scalar-exact / exact-value-substring hits = `0/0/0/0/0`.

## Expected result preimage

The binding chain calls `ExposureScaleHistoryResultSummary` in
`evaluation/internal/finalv5binding/generate.go:363-386`. The oracle computes
the exact rational sum from the fixed Product formula, without SQL or production
output, in `evaluation/finalv5oracle/scale_manifest.go:108-137`; that formula's
sole row implementation is `evaluation/finalv5oracle/dataset.go:322-333`.
Numeric normalization removes insignificant decimal zeroes in
`evaluation/finalv5oracle/canonical.go:256-365`. Schema, row and final result
framing are defined in `evaluation/finalv5oracle/result.go:57-69,128-185`.

For `10k-overlap-0`, `M=2000`, `K=0`, so history is member ranks `(2000,4000]`.
The SQL value is `782130.00` and its typed canonical numeric bytes are `782130`.
All integers below are unsigned 64-bit big-endian values.

```text
schema preimage hex:
5441534b474154452d46494e414c2d56352d524553554c542d534348454d412d5631000000000000000001000000000000000d686973746f72795f746f74616c00000000000000076e756d65726963
schema sha256: 5c7937e4047c62aad250944a89dce6108873d47d88a9bd65e8ad8e993753c059

typed row payload hex:
000000000000000100000000000000076e756d65726963010000000000000006373832313330
row-stream sha256: 75b1933cc472ae52626f7578a6bbee892d09f9f7faccffd6861fdf9eff58c908

final result preimage hex:
5441534b474154452d46494e414c2d56352d43414e4f4e4943414c2d524553554c542d56310000000000000000205c7937e4047c62aad250944a89dce6108873d47d88a9bd65e8ad8e993753c05900000000000000010000000000000001000000000000002075b1933cc472ae52626f7578a6bbee892d09f9f7faccffd6861fdf9eff58c908
sha256: 00e4f5977565ab625bd37beb541c855edd4f70e9f0f950dfbaf9d24f8b5d54ea
```

This reproduces the signed binding exactly.

## Live result preimage

Before P52, `completeTaskgateSampleWithParquet` decoded the verified artifact
and called `experiment.CanonicalResultHash`
(`evaluation/cmd/final-v5-adapter/baseline.go:646-658`). That legacy helper JSON
marshals and lexically sorts rows, then hashes ASCII decimal length, `:`, and
JSON bytes (`evaluation/internal/experiment/types.go:1884-1899`). Parquet
NUMERIC decoding yields `json.Number("782130.00")`, so the one-row JSON is the
unquoted number `[782130.00]`.

```text
legacy preimage ASCII: 11:[782130.00]
legacy preimage hex:   31313a5b3738323133302e30305d
sha256:                d9a06a3ef0d26534e90191534cf3223530bc982e724d50e6ada4ea4fe77395b0
```

This exactly reproduces the retained live digest. The second implementation is
the legacy JSON-shaped sample reducer; the authoritative form is the frozen
typed logical-result normalizer used by the binding and Direct/BDG/Parquet
agreement.

## Dependency preimages

The independent oracle emits five canonical Fact hashes per retained Product
row (`evaluation/finalv5oracle/dependency.go:207-225`) and selects the existing
range `[N,2N)` for zero overlap (`dependency.go:139-158`). It sorts and
deduplicates Fact hashes, then hashes:

```text
frame("TASKGATE-FINAL-V5-SET-ALGEBRA-V1/existing")
u64(10000)
for each of 10000 lexically sorted members: frame(raw_sha256_32_bytes)
```

The exact preimage length is 400057 bytes. The first member is
`00083453b7c23c046c5a2bd5361fb73cea16210bb463f5e0c9f59b10e4237615`,
the last is
`ffe5eee3799111e54ff863e65a8331712d28f8da2c39a68c07693c010ae3d004`,
and the digest is `98a92106ae1b9ac9379cad0e3977cf9985b19ece54b49e117651d4a0efcfeada`.
The framing implementation is
`evaluation/finalv5oracle/setstream.go:350-425`.

Production does not publish that semantic digest as its native set identity.
It normalizes the dictionary-backed ordinal bitmap and hashes the dictionary-set
digest, normalized bitmap digest, and sorted dynamic facts
(`internal/control/ordinal_exposure.go:381-405,1098-1113`). The P51 linker
replayed the fixed oracle Fact stream through the retained, verified HOT/COLD
publication and obtained:

```text
semantic digest: 98a92106ae1b9ac9379cad0e3977cf9985b19ece54b49e117651d4a0efcfeada
dictionary set:  c23fb787f54ce149ea645b051ecd47907a9226cbb0e2120dd93dac48a65b92ad
static bitmap:   3a00c2c241989cceaca0f8efb7eec11c22120c2614690fa2a3901a49b2f41012
cardinality:     10000
dynamic facts:   0
```

The 178-byte production preimage is:

```text
5441534b474154452d56342d4859425249442d5345542d5631000000000000000040633233666237383766353463653134396561363435623035316563643437393037613932323663626230653231323064643933646163343861363562393261640000000000000040336130306332633234313938396363656163613066386566623765656331316332323132306332363134363930666132613339303161343962326634313031320000000000000000
```

Its SHA-256 is
`437dc81c61ea97c4ab358bf166d4d2bc57158d39b41a2c6b81ec6d4ec6d7f4a7`,
exactly the retained production digest. The P51 linker is the authorized bridge:
it keeps semantic and native identities separate and compares exact ordinal
membership (`evaluation/internal/experiment/deployment_scale_dependency_link_v1.go:36-80`).
The two original digests therefore must not be compared byte-for-byte.

## Disposable PostgreSQL check and decision

A disposable container used the contract-pinned PostgreSQL digest
`postgres@sha256:92620d...d55`, mounted `db/init` and the exact
`benchmark-v1-generate.sql`, and reported:

```text
server_version_num=160014
rows/min/max=414000/1/414000
SUM(metric) over (2000,4000]=782130.00
pg_typeof(sum(metric))=numeric
```

The container was removed after the query; residual containers and volumes were
both zero. The deployment SQL formula at
`evaluation/final-v5-wsl2/sql/datasets/benchmark-v1-generate.sql:30-44` and the
oracle formula at `evaluation/finalv5oracle/dataset.go:322-333` agree byte-level
in arithmetic and range. H2 is rejected. H1 is established: the value and Fact
membership agree, while the harness used a non-authoritative result encoding;
the dependency pair was already an intentional semantic/native domain split.

P52 changes only the evaluation harness. Scale candidate and history results now
re-reduce the verified released Parquet through the shared typed normalizer,
using result schemas exported by the sole oracle definition. The approved P45
binding (`file_sha256=3bb2771fa07b3cd7b0e0d806cf84af41d05628b958f425310368b854b77b7526`),
oracle manifests, capability flags, frozen evidence and tags are unchanged; no
author re-sign is required.
