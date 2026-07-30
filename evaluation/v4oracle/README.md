# V4 million-Fact independent oracle

This package verifies the committed maximum-point V4 observation without
calling the Gateway bitmap-derivation path. It reconstructs expected base
FactIDs and witness multiplicities row by row from the frozen Business
snapshot, independently parses every immutable COLD dictionary record, and
external-merge-compares full FactHashes plus canonical payloads.

The independence claim is deliberately scoped: the derivation algorithm and
COLD parser are evaluation-owned, while the oracle shares TaskGate's versioned
canonical FactID specification/encoder, algebra normalizer, portable bitmap
decoder, and observation digest specification. The report binds all of those
first-party source dependencies, the executable, the original full-campaign
results, dictionary manifests, and complete COLD artifact bytes by SHA-256.

The command reads the Business and Control PostgreSQL DSNs from
`V4_EVAL_BUSINESS_DSN` and `V4_EVAL_CONTROL_DSN` by default. Both accounts
must be read-only for this audit. It writes the output report exclusively and
refuses to overwrite an existing file:

```sh
go run ./evaluation/cmd/v4-million-oracle \
  -results evaluation/v4-acceptance/evidence/results.json \
  -artifact-dir /verified/snapshot-index \
  -spool-parent /private/oracle-spool \
  -repository-root . \
  -output /private/v4-million-oracle.json
```

A passing `taskgate-v4-million-fact-oracle-v1` report establishes:

- exactly 12 Release, 1,035,000 Influence, and one Outcome FactID;
- 1,035,013 exact full-hash and canonical-payload matches, with zero mismatch,
  missing, or extra records;
- all 12 derived witness commitments and their exact multiplicities;
- equality of the independently recomputed Release/Influence/Outcome set
  digests and observation digest with Control PostgreSQL and the original
  full-campaign result;
- zero calls to the V4 bitmap derivation hot path and bounded external-sort
  resource measurements (runs, spool bytes, resident records, and peak RSS).
