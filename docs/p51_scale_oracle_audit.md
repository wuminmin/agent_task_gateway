# P51 Scale dependency-e2e oracle audit

Date: 2026-08-18

Audited tree: `6138e12bd73beef41f1191e1557c7f7e46c60ed8`

Retained run: `evaluation/final-v5-wsl2/raw/p50-mech-full-10`

## Decision

`p50-mech-full-10` exposed a **harness observation/specification defect**. The
P45 binding commits role-bound semantic ordinary-set digests. A TaskGate sample
and root snapshot retain the production Control ledger's native hybrid ordinal
set digest. The old Scale adapter compared those two different digest domains
as byte-identical values. That comparison is not a membership comparison and
cannot establish either an oracle defect or a real TaskGate behavior difference.

The P45 binding and its Dataset/Catalog inputs are internally consistent. This
repair therefore does not regenerate or alter any signed material and does not
require author signing session #2'. It adds an evaluation-side
semantic-to-ordinal link and a new evidence-v4 wire while retaining evidence
v1/v2/v3 unchanged.

No deployment, campaign, or diagnostic database was started for this audit.
The source and retained bytes were sufficient to classify the failure.

## P50 retained evidence and its limit

The exposure-scale deployment reached `failure_stage=cells_scale` on commit
`6138e12bd73beef41f1191e1557c7f7e46c60ed8`. Its baseline workload passed all
15/15 cells. Its dependency-e2e workload retained 24 records:

| Mode | Count | Retained status/error | Acceptance decision |
| --- | ---: | --- | --- |
| novel | 12 | `fail` / `dependency_e2e_measurement_failed` | none |
| semantic_replay | 12 | `invalid` / `semantic_replay_lacks_novel_anchor` | none |

The twelve stderr lines are identical except for the Scale key:
`verified TaskGate result differs from its bound rows/columns/result/Dependency oracle`.
The failure occurred in history prefill, before the measured candidate query.
At the audited tree, `executeDependencyE2E` returned an empty `Sample` when
`prefillDependencyHistory` returned this error. The outer failure constructor
then serialized zero/empty defaults. Consequently, the retained zeros below are
**not actual query observations** and must not be read as `0 rows`, `0 columns`,
or an empty production dependency set.

| Scale key | Novel retained rows/cols/result/facts/set | Candidate reached? | Replay retained record |
| --- | --- | --- | --- |
| `10k-overlap-0` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `10k-overlap-50` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `10k-overlap-90` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `10k-overlap-100` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `100k-overlap-0` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `100k-overlap-50` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `100k-overlap-90` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `100k-overlap-100` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `1035000-overlap-0` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `1035000-overlap-50` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `1035000-overlap-90` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |
| `1035000-overlap-100` | `0 / 0 / "" / 0 / ""` (discarded actual) | no | invalid: no novel anchor |

This is a uniform failure-stage pattern, not a retained per-field mismatch
pattern. P51 changes a future history-prefill failure to retain the real
rows/columns/result/dependency observation under the dedicated
`dependency_history_prefill_failed` error code. It remains sample-v1 failure
evidence and cannot masquerade as sample-v3 acceptance.

## P45 expectation lineage

The approved private binding has SHA-256
`3bb2771fa07b3cd7b0e0d806cf84af41d05628b958f425310368b854b77b7526`.
It binds:

- Catalog `ac2dc5cf30ef500a96c15bbbe2d6e067a4ed9eedb18c93970c40cea652eb88b6`;
- Dataset `f90239bb32ef9542089ca8f1bd7c30c7870cbe627e835698364bdb9b4dc15978`;
- live Dataset probe `0eb905408442997de37ac810683f18c758b614a716c50758312015aeb753d314`;
- adapter binding section `a32e78ba5c19ece0b391cbdd0456d3446459e1df1ea41d70933268da09d18290`.

The generation chain is:

1. `evaluation/internal/finalv5publication/current_binding.go:82-134` loads the
   source-controlled Catalog, generates fixed Scale Outcome expectations before
   observation, attests the Catalog, verifies the complete typed PostgreSQL
   Dataset, observes the publication closure, and passes all inputs to
   `finalv5binding.BuildCompleteBinding`.
2. `evaluation/internal/finalv5binding/generate.go:333-404` pairs the 24 verified
   novel/replay manifests into 12 keys. Candidate result/cardinality/semantic
   digest come from the fixed manifest; history result comes from
   `ExposureScaleHistoryResultSummary`; history semantic digest is the
   manifest's `Existing`; union cardinality/digest is the manifest's `Union`.
3. `evaluation/finalv5oracle/scale_manifest.go:53-65,108-137` freezes the twelve
   Scale cells and evaluates the history aggregate from the fixed row formula,
   without parsing or executing SQL.
4. `evaluation/finalv5oracle/dependency.go:78-205,207-226` generates five facts
   per row and summarizes candidate `(0,N]`, existing `(N-K,2N-K]`, and union
   `(0,2N-K]`. `SummarizeSemanticSetRoles` includes the role in each digest
   domain, so candidate/existing/union commitments are intentionally distinct.

### Exact twelve-key P45 table

`H` is history/existing, `C` is candidate, and `U` is union. Result digests are
only defined for H and C. Every query expectation is exactly 1 row and 1 column.

| Scale | H result SHA-256 | H facts | H semantic-set SHA-256 | C result SHA-256 | C facts | C semantic-set SHA-256 | U facts | U semantic-set SHA-256 |
| --- | --- | ---: | --- | --- | ---: | --- | ---: | --- |
| `10k-overlap-0` | `00e4f5977565ab625bd37beb541c855edd4f70e9f0f950dfbaf9d24f8b5d54ea` | 10000 | `98a92106ae1b9ac9379cad0e3977cf9985b19ece54b49e117651d4a0efcfeada` | `9528745fee7030eb82173d2ba6fa7ad5750da1dee0396a372eac08049ea2ee46` | 10000 | `d0f6452e4e475a91aa850a7a63a1e6108c72dafcde717805aedc566beb0ecf54` | 20000 | `876a733591dc6003fae14f10ff0bfd267a2896fb7cc43939cf3147f49a579126` |
| `10k-overlap-50` | `eb42f37d81be0369c5a5ec6a2a3f4711f98460edd1fba07a2ec6a302a4680a31` | 10000 | `0fd0311444b77c60a75be1c0e5ddfa39813c015f21c6842016ac65080586efea` | `9528745fee7030eb82173d2ba6fa7ad5750da1dee0396a372eac08049ea2ee46` | 10000 | `d0f6452e4e475a91aa850a7a63a1e6108c72dafcde717805aedc566beb0ecf54` | 15000 | `060b8a69030209778c7b4dc6f1329a09aceb9a16d97b5f14975e50b479cbe885` |
| `10k-overlap-90` | `230fe1b7d47ec31eb781da03117b5ac6497e1abb41aa7d33bfa8113714fae91b` | 10000 | `a8c39ad67d85a97ca9382cbcb8d3f9220ff12cb71e15318fc39938f863744854` | `9528745fee7030eb82173d2ba6fa7ad5750da1dee0396a372eac08049ea2ee46` | 10000 | `d0f6452e4e475a91aa850a7a63a1e6108c72dafcde717805aedc566beb0ecf54` | 11000 | `3ff443a6088aa0f4f2f1a95cc4e093e9564739febd68c9422f9223267a7f1389` |
| `10k-overlap-100` | `4c4ae71ddee2417d5399ab6f2e908e8fe4bfa481055237543983d6d7449dee4a` | 10000 | `f2e5fac2b2725749d4bdaad716649efa4ba55608877e07253d36c0e48cf83285` | `9528745fee7030eb82173d2ba6fa7ad5750da1dee0396a372eac08049ea2ee46` | 10000 | `d0f6452e4e475a91aa850a7a63a1e6108c72dafcde717805aedc566beb0ecf54` | 10000 | `3e9910ada11b3b54ed76420e204c8757419e9332b531fa241adcdb25468f35a4` |
| `100k-overlap-0` | `f27f85672524532993eabd37f0402ad6204e2a7e027965090ed2e00469dc7c84` | 100000 | `1bc476db7c8b23b41ecda5d8abd8cf3cc4a879252e29ac45b0f90c980e6e4e17` | `0ec0a2a22e7e185f12257f7f45060f67c0b3754d4d11f7acff860f47ea63fa0c` | 100000 | `e2e4dfc1f7763f8351d8d0bfa73f70b1474fc3f0f8abf9edb9c6ea987cdecf99` | 200000 | `444f28a97aafaea70d6e7b1054c0782f9a4775836c09316f7b05923eed734165` |
| `100k-overlap-50` | `a9e7670a49c0a47c6c220a81d96d5f0b5d577d7c6af15769a0dea37373523011` | 100000 | `69dc3df2f3b277011d5841aa02df70112efb1d018fe249fb4e4861e395cef20e` | `0ec0a2a22e7e185f12257f7f45060f67c0b3754d4d11f7acff860f47ea63fa0c` | 100000 | `e2e4dfc1f7763f8351d8d0bfa73f70b1474fc3f0f8abf9edb9c6ea987cdecf99` | 150000 | `beb3e228f3379e810347bb4ae7fc409e6d802c62419387455b14796beda20835` |
| `100k-overlap-90` | `27df896648aa4d54d65354fb85ce53527de8a83cdd8cd20415f53a23da0358c0` | 100000 | `65ac92edcb2ee4e07f4affae9e646603964ead789d1a9c5941a994d4ba4b7df9` | `0ec0a2a22e7e185f12257f7f45060f67c0b3754d4d11f7acff860f47ea63fa0c` | 100000 | `e2e4dfc1f7763f8351d8d0bfa73f70b1474fc3f0f8abf9edb9c6ea987cdecf99` | 110000 | `7bbfd350d8b2339af4c8674002b96c714554157514ee1de926b6600b2aefad3e` |
| `100k-overlap-100` | `1b4b8ef053880a0e4bf62c3871a7c0495d8bef173212f95c37e9d9c1fe9fb3bf` | 100000 | `c9080fbf54f50b35f8eab4f85e1f22fda56438b8bcb17b31bc21559a0d3ba183` | `0ec0a2a22e7e185f12257f7f45060f67c0b3754d4d11f7acff860f47ea63fa0c` | 100000 | `e2e4dfc1f7763f8351d8d0bfa73f70b1474fc3f0f8abf9edb9c6ea987cdecf99` | 100000 | `3ff1046cdb4ba2ef313e7641c589297875df654b58e95d63517d28a08cf12b21` |
| `1035000-overlap-0` | `0d4dd549af915541e94874d58eb6570ac86aaa18c33fccf91a8a706c6c5fe1f1` | 1035000 | `fb6bdf78d4b2f4c1047c7e707b517e93edde6b62e8c6a26ea25582fd28896232` | `48f7de9160702299adc2cb00311d9d23c378dcd30c6100e34a143346abecdfe1` | 1035000 | `5d63478f9799ccd4635efded18e17bbb391dd753cdc9e9c429cb668d3c36c09b` | 2070000 | `4354d04853af871bc62d3eba9d51b2da2b1abe9e8e365bcf307e3d7ccd50f02f` |
| `1035000-overlap-50` | `7594374b3e6b96f3569066a884fb6290e5a59868b30894b8e449719658631348` | 1035000 | `b788db727d20df5ddf023265de8b5b6744e78652cfd5bdc3d41e2078bd453b9f` | `48f7de9160702299adc2cb00311d9d23c378dcd30c6100e34a143346abecdfe1` | 1035000 | `5d63478f9799ccd4635efded18e17bbb391dd753cdc9e9c429cb668d3c36c09b` | 1552500 | `5be68913c1cce5f89ce623203c46af90322f0c09348aae4ef213d3ff025435ab` |
| `1035000-overlap-90` | `c7f1f3ea6e0fcd905ab5f151f2c7c42175df0ca6d8dcdb9e14248422bd9bf791` | 1035000 | `a9956838a1d7cfc92d58929b4913ad59bd0d4c64140508f7bb37da7b4c1ffecd` | `48f7de9160702299adc2cb00311d9d23c378dcd30c6100e34a143346abecdfe1` | 1035000 | `5d63478f9799ccd4635efded18e17bbb391dd753cdc9e9c429cb668d3c36c09b` | 1138500 | `aeb72976d4b2f7b53b9cbb2561786a72f07d0b4663757b2b0bb789a12251b1eb` |
| `1035000-overlap-100` | `886d2dbf0a1ac68e65e6796d576be248fb5ec7ca71533d22fac32604a26ae237` | 1035000 | `2f4bae4579763ed7a686041b14151a3daa8d43684795211df42991e7f4953fb1` | `48f7de9160702299adc2cb00311d9d23c378dcd30c6100e34a143346abecdfe1` | 1035000 | `5d63478f9799ccd4635efded18e17bbb391dd753cdc9e9c429cb668d3c36c09b` | 1035000 | `2d634783c54eb8d5fe679eb14c2821a38d2e8878917819823fc71fe02c883cff` |

## Dataset and P47 audit

The deployed Product and the oracle use the same frozen row domain and formula:

- `evaluation/final-v5-wsl2/sql/datasets/benchmark-v1-generate.sql:30-44`
  creates 414,000 rows and computes
  `((((member_rank * 13) % 100000) + 100)::numeric / 100)`, with
  `family_id=1` and `partition_key=1`.
- `evaluation/finalv5oracle/dataset.go:303-333` uses the same 414,000-row
  domain and exact cents formula `(rank*13)%100000 + 100`, then formats it at
  scale 2 with the same two fixed integer fields.
- `db/init/20-final-v5-benchmark-dataset.sh:4-15` executes the contract-indexed
  SQL file itself; there is no second Compose-side copy.

`git show 3ce30f7` verifies that P47 changed only the exposure profile's query
budget/routing metadata, registry/coverage, profile generator/tests, and ledger.
There is no diff in the Dataset SQL, `finalv5oracle/dataset.go`,
`finalv5oracle/dependency.go`, or the P45 binding. Thus “P47 only changed the
budget, not the data” is verified.

## Root cause and repair boundary

The semantic oracle hashes canonical Fact SHA-256 members under a role-bound
ordinary-set domain. The production Control ledger computes
`Sample.DependencySetSHA256` with `ordinalHybridSetDigest`, which frames the
dictionary-set digest, portable static bitmap digest, and dynamic Fact list
(`internal/control/ordinal_exposure.go:1098-1113`). Equal semantic membership
does not imply equal bytes across these domains.

The repaired path:

1. preserves result rows, columns, result digest, and dependency cardinality as
   direct binding checks;
2. resolves candidate/existing/union semantic expectations only inside the
   finalizer;
3. reads the named production bitmap set in a PostgreSQL read-only transaction;
4. verifies the activated profile's exact Catalog and HOT/COLD publication
   closure;
5. uses the existing evaluation-only `finalv5linker` to map fixed oracle facts
   through the reviewed HOT dictionary and compare the resulting ordinals with
   the production set;
6. retains both semantic and production identities in evidence-v4 and validates
   them independently. Historical evidence-v1/v2/v3 validation is unchanged.

The implementation is in
`evaluation/internal/experiment/deployment_scale_dependency_link_v1.go:36-259`,
`evaluation/internal/experiment/scale_dependency_link_v1.go:9-117`, and
`evaluation/internal/experiment/finalize_scale_artifact.go:79-93,399-449`.
The adapter cut-over and prefill preservation are in
`evaluation/cmd/final-v5-adapter/scale.go:236-247,333-365,480-565`.

Classification:

| Branch | Finding |
| --- | --- |
| expectation derivation defect | no evidence; fixed Dataset/manifest/binding chain and identities agree |
| harness observation/specification defect | **yes**; invalid cross-domain digest equality, plus loss of the prefill observation on error |
| real system behavior difference | not established by P50; the actual history sample was discarded and the candidate was never reached |

P50 remains a failed cell run and is not retried or reclassified. P51 only
repairs the harness contract for a future clean run.

## Reproducible audit commands

```sh
sha256sum /home/wmm/.taskgate-signing-backup-p45/current-dataset-binding/publication-binding.json
jq -r '.final_v5_adapter_v2.scale.dependency_e2e | to_entries[] | [...] | @tsv' \
  /home/wmm/.taskgate-signing-backup-p45/current-dataset-binding/publication-binding.json
jq -r '.sample | [.scale,.mode,.status,.error_code,.row_count,.column_count,
  .result_sha256,.actual_dependency_facts,.dependency_set_sha256] | @tsv' \
  evaluation/final-v5-wsl2/raw/p50-mech-full-10/deployments/exposure-scale/001/raw/scale.jsonl
nl -ba evaluation/final-v5-wsl2/raw/p50-mech-full-10/deployments/exposure-scale/001/adapter-stderr/scale.log
git show --stat --oneline 3ce30f7
git diff --quiet 3ce30f7^ 3ce30f7 -- \
  evaluation/final-v5-wsl2/sql/datasets/benchmark-v1-generate.sql \
  evaluation/finalv5oracle/dataset.go evaluation/finalv5oracle/dependency.go
```
