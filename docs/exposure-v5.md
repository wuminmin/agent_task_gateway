# TaskGate exposure V5

V5 accounts the caller-controlled predicate footprint separately from the
complete query result:

```text
Outcome(q) = PredicateAtoms(q) union { CompositeOutcome(q) }
```

An atom means only that a normalized condition was tested. It does not claim
that the condition evaluated to true, false, or unknown. The single composite
fact binds query normal form V4, the committed result observation, visible row
count, predicate context, predicate-set digest, and atom count.

## Identity and normalization

- Profile: `taskgate-exposure-v5`
- Fact domain: `TASKGATE-FACT-V5\0`
- Query normal form: `taskgate-query-normal-form-v4`
- Atomizer: `taskgate-predicate-footprint-v1`
- `IN` members become `EQ` atoms; `NOT IN` members become `NE` atoms.
- Duplicate atoms are removed. `NULL` remains the canonical literal `null`.
- Mandatory scopes, fixed View filters, joins, projections, grouping, ordering,
  and pagination do not become atoms.
- Context commits Catalog, publications, stable relation graph, View binding,
  and effective mandatory scope. It excludes caller literals and projections.

Every successful query contains exactly one composite and zero or more atoms,
so `actual_outcome_facts = actual_predicate_atom_count + 1`.

## Signed limits

A V5 budget profile must carry:

```yaml
exposure_profile_version: taskgate-exposure-v5
predicate_footprint:
  version: taskgate-predicate-footprint-v1
  max_raw_literals_per_query: 20000
  max_unique_atoms_per_query: 10000
  max_atom_payload_bytes: 4096
  max_total_atom_payload_bytes: 8388608
```

The unique-atom implementation ceiling is 65,536. These limits are signed in
the authorization manifest and grant, persisted in `task_grants`, checked
before reservation/business SQL, and inherited without expansion by delegated
tasks.

## Storage and settlement

Release and influence retain the V4 publication-ordinal Roaring representation.
Outcome uses an immutable exact SHA-256 Merkle radix set:

- full hashes grouped by 16-bit prefix;
- deterministic 4,096-member leaf chunks;
- content-addressed leaves, first-byte blocks, and root manifests;
- candidate hashes grouped by prefix16 and only their prefix8 blocks and leaf
  chunks loaded;
- exact full-hash difference/union inside touched leaf families, with collision
  and manifest verification;
- untouched leaf/block digests reused directly, with only new leaves, changed
  blocks, and the new small root manifest persisted;
- no membership row per fact and no full-root rebuild on a small candidate.

The merge reports `root_cardinality`, `candidate_cardinality`,
`blocks_loaded`, `leaves_loaded`, `hashes_loaded`, `blocks_reused`, and
`leaves_changed`. Therefore the novelty path depends on candidate-touched radix
branches rather than total root membership (apart from occupancy inside those
touched prefix16 families).

Atoms and the composite settle in the same R/D/O transaction and root-head
CAS. A CAS loser reloads root state and recomputes all novelty. V4 roots and
facts are never converted or replayed as V5.

## Replay and receipts

The V5 semantic replay key explicitly binds predicate profile, context, set
digest, and atom count in addition to the existing authority, publication,
normal-form, compiler, result-encoding, and artifact bindings. V5 cache rows
and observations are physically separate from V4. A valid replay references
the committed complete observation and charges zero R/D/O novelty while still
performing authorization, resource accounting, audit, receipt, and result
publication.

Receipt V7 reports predicate and composite actual/charged counts and enforces:

```text
actual_outcome = actual_atoms + 1
charged_outcome = charged_atoms + charged_composite
```

Receipts and public audit events contain only counts and digests, never raw
predicate literals. In `QUERY_V5_EXPOSURE_SETTLED`, `outcome_set_sha256` is the
query candidate set digest used by the observation, charge, and V7 receipt;
`root_outcome_set_sha256` is the merged cumulative root digest.
