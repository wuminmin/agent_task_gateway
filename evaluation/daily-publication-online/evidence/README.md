# Compact RQ5 online evidence

`scale-20260730-final/` is the compact, source-controlled record of the
345,000-row retained-publication transition campaign. It contains exactly:

- `online-evidence.json`, `dataset-manifest.json`, and `preparation.json`;
- four approved inputs and four Catalog YAML files; and
- one bundle manifest for each of Day0 through Day3.

`pack-manifest.json` records the byte count, role, and SHA-256 of every retained
evidence file plus a deterministic content-tree hash. It also inventories the
12 omitted HOT, COLD, and sidecar payloads from the bundle descriptors. Those
large payload bytes are deliberately not in the compact pack. A fresh clone can
re-hash every retained file and validate the complete descriptor/digest chain,
but it cannot independently re-hash an omitted payload.

## Reproduce the seal

The sealer first validates the raw run through the combined RQ5 validator,
copies only the fixed allowlist, builds a deterministic manifest, validates the
staging pack, and atomically renames it. It refuses to overwrite its output.
To reproduce into a new directory:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 \
  evaluation/daily-publication-online/evidence/seal.py \
  --source evaluation/daily-publication-online/raw/scale-20260730-final \
  --output /tmp/taskgate-rq5-online-pack
```

No timestamp, inode, owner, or source absolute path enters the manifest, so
identical source bytes produce an identical `pack-manifest.json`.

## Validate

```sh
PYTHONDONTWRITEBYTECODE=1 python3 \
  evaluation/daily-publication-online/evidence/validate.py
```

The validator rejects symlinks, non-regular members, duplicate/non-finite JSON,
missing or extra files/directories, role/size/hash drift, an incomplete omission
inventory, and any online semantic or cross-fixture failure. For the final
check it calls the independent `paper/tkde/rq5_evidence.py:validate_rq5` API;
the pack validator does not reimplement or weaken that contract.

Run the adversarial tests with:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest \
  evaluation/daily-publication-online/evidence/test_validate.py
```

