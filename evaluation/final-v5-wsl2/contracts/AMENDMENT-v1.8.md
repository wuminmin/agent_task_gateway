# Final-V5 contract amendment v1.8

Previous release:
final-v5-contracts-v1.7

New release:
final-v5-contracts-v1.8

## Why this release exists

This is the first Final-V5 freeze that contains runtime code changes. It
incorporates three digest-preserving changes: streaming outcome-digest
construction from `593ce83`, the `P5-mem.1` binary `[32]byte` FactSet key, and
the `P5-mem.2` adaptive encrypted FactSet spill. It also carries the
exposure-scale review candidate re-sealed under Decision 23 and
`APPROVE-C2-v1.8` after the v1.5 normalization-spec correction.

The three runtime changes claim to preserve every digest and output byte. That
claim is not accepted from implementation reasoning alone. The fresh-live
activation and Attestation-footprint qualification required for this release
must independently reproduce the historical portable identity before this
freeze is complete. This pre-activation amendment does not claim that result
has already been measured.

## What did not change

The Gateway cgroup ceiling remains `12g`, and
`GATEWAY_CONNECTOR_STATEMENT_TIMEOUT` remains `10m`. The accounting mechanism,
the 1,000,000 estimated-base-fact lane threshold and lane capacity, and every
wire, Sample, ledger and Receipt byte remain unchanged. FactSet spill changes
only the resident representation of Fact values; its public JSON, binary and
hex identities are unchanged.

The hash-locked protocol and workload matrix are unchanged. All 28 indexed
contract artifact digests are byte-identical to v1.7; `contracts/index-v1.json`
moves only its release, supersedes and amendment fields.

## What is still NOT claimed

The campaign's cumulative peak is still **not measured**. `12g` remains an
engineering margin over a single-experiment measurement, not demonstrated
sufficiency for the nine-experiment campaign.

The FactSet spill reduced measured Go-heap residency by **4.66x**, below the
planned 5--7x range. That is a Go heap reading, not a cgroup or Gateway peak;
the Gateway peak remains for the later `P5.2-pre` standalone Scale measurement.

The RQ4 spatial threshold of at most 512 MiB has **not** been restored. Spill
trades latency for memory headroom; it is not a performance improvement. The
measured AEAD A/B read-back overhead of +8.7% is an internal engineering tradeoff
input and is not a paper conclusion.

## Activation support does not carry across this release

The recorded smokes ran under v1.7. `config/profiles/activation-support-v1.json`
is removed rather than relabelled, and the registry is regenerated with all 11
profiles initially ineligible. No old evidence byte receives a v1.8 label. The
seven live-route profiles must be activated against a fresh v1.8 deployment and
regenerated to a byte-stable fixed point before this freeze is complete.

## SQL executability record

`contracts/sql-executability-v1.json` embeds the Contract Index digest, which
the release bump invalidates. It is re-derived by executing the indexed SQL
against a disposable PostgreSQL 16.14 database, never by editing the digest.
The measured v1.8 result is recorded after that live gate runs.

## Publication evidence affected

The exposure-scale review candidate has been re-sealed under Decision 23 and
`APPROVE-C2-v1.8`. The publication-binding provenance is known to be stale
because Decision 23 was appended after its signed input binding. Regenerating
and author-signing that provenance is a later task; this freeze neither rolls
back Decision 23 nor rewrites the signed provenance.

## Execution status

v1 through v1.7 remain preserved for audit. v1.8 is not complete until fresh
activation, SQL executability, Attestation portable-identity requalification,
the combined validation gate, and the freeze ledger entry all pass.
