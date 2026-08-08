# Final-V5 outside-Product route matrix derivation and rerun

## Scope and non-goals

This document fixes the derivation rule and the rerun boundary for these two
records:

- `taskgate-final-v5-outside-product-route-matrix-v1`;
- `taskgate-final-v5-semantic-cache-isolation-evidence-v1`.

The route matrix is an aggregator over the outside-Product probes already
emitted by `final-v5-profile-activate`; it is not a second probe
implementation. The isolation record is a second aggregator over the formal
Product-intersection record, the route matrix, and the production lookup test
manifest.

The two aggregation commands do not themselves amend a contract or mutate
profile readiness. A separately authorized live follow-up did consume their
validated records to generate `activation-support-v1.json` and regenerate the
registry. The seven live-route profiles now have
`status.activation_supported=true`, `status.activation_smoke_passed=true`, and
`targeted_run_eligible=true`; all eleven profiles still have
`targeted_validation_passed=false` and `routable=false`. The evidence
run-status flags `publication_eligible`, `capability_changing`,
`activation_support_changing`, and `formal_campaign` remain false. The
repository remains on contract release v1.4; the v1.3 material below is
test-only historical input.

## Executable derivation rule

Let `R` be the decoded profile registry. A profile participates exactly when
all three pre-activation deployment predicates already used by
`buildIntersection` hold:

```text
participates(p) :=
    p.status.closure_complete
 && p.status.catalog_materializable
 && p.status.live_route_available
```

`activation_supported`, `activation_smoke_passed`,
`targeted_validation_passed`, `targeted_run_eligible`, and `routable` are not
inputs to this predicate. In particular, a new contract release correctly
resets activation support without changing which closures are capable of a
live outside-Product check.

The command must derive this set from registry fields and reject a
Product-intersection input whose profile set, pair count, aliases, closures, or
contract/registry version does not describe the same set. It must never carry
a source-coded allowlist of profile aliases.

For the current registry this predicate derives seven participants:

```text
attack-expense-detail
concurrency-expense-detail
expense-detail
provsql-nonce-join
result-heavy
rls-bounded
rls-unlimited
```

It excludes four profiles as a consequence of their fields, not their names:
`depth4-semantic-view` and `exposure-scale` are not Catalog-materializable and
have no live route; `analytics-orders-lineitem` and `analytics-orders` have no
live route.

Let `C(p)` be the set of Products in participating profile `p`'s closure, and
let:

```text
P := { p in R.profiles | participates(p) }
U := union(C(p) for p in P)
Q := { (p, product) | p in P && product in U && product not in C(p) }
```

`Q` is the complete probe plan. Each participating profile is asked for every
Product in the participating universe that is outside its own closure. A
Product is a set member, so duplicate Product declarations are rejected rather
than counted twice.

Participant derivation, the intersection cross-check, and live activation order
use profile ID in ascending byte order, matching the existing
`buildIntersection` rule. Outside Products within each activation are sorted in
ascending byte order. The final matrix probes are independently sorted by
profile alias and then requested Product in ascending byte order. Registry
validation makes IDs and aliases unique; an ambiguous identity fails rather
than acquiring an incidental input order.

For the frozen Final-V5 set, six profiles each own one Product and the ProvSQL
profile owns three. Therefore:

```text
|P| = 7
|U| = 9
6 * (9 - 1) + 1 * (9 - 3) = 48 + 6 = 54 probes
```

The expected per-profile counts are eight for each one-Product profile and six
for `provsql-nonce-join`. These counts, the seven profiles, and the nine unique
Products are derived assertions, not constants used to construct `Q`.

The Product-intersection record contains all `7 * 6 / 2 = 21` unordered pairs.
For the frozen set every intersection is empty, every
`intersection_count` is zero, and every `same_query_live_test_applicable` is
false. Those facts explain why the route-refusal proof is composed with the
production lookup tests instead of pretending that a same-query live
cross-profile request exists.

## The ten negative properties

Each route entry has 17 fields. Seven identify and bind the probe
(`requested_product`, its digest, the target profile ID/alias/Catalog digest,
the response digest, and the stable refusal classification). The other ten are
negative properties:

1. `catalog_list_absent`;
2. `live_request_refused`;
3. `no_active_task`;
4. `no_artifact`;
5. `no_available`;
6. `no_business_sql`;
7. `no_observation`;
8. `no_receipt`;
9. `no_root_ledger_change`;
10. `no_semantic_cache_hit`.

The first property is observed from the running Gateway's activation
diagnostic. The second is observed by the activator through the ordinary public
MCP `request_data_task` path. The aggregator must consume those activator
observations; it must not reimplement the request or infer refusal merely from
the registry.

The remaining eight fields are causal consequences of refusal at the
task-issuance boundary: no task is issued, so no task-scoped Business SQL,
semantic-cache lookup, observation, receipt, artifact lifecycle, availability
transition, or root-ledger transition can occur. They are derivation fields,
not claims that the route-matrix command independently queried eight stores.
This distinction must remain explicit in reviews and reports.

A passing aggregated probe requires the complete activation evidence to be a
non-publication PASS with activation smoke passed and no failures. The probe
must report the Product absent from the active Catalog, the live request
refused, `refused=true`, the expected pre-task refusal observation, a lowercase
SHA-256 for both requested Product and response, and profile/Product identities
matching the derived plan. For this v1 matrix, `tool_error` is the accepted
task-boundary refusal class; HTTP- and JSON-RPC-layer errors are rejected so an
authentication or protocol failure cannot masquerade as a closure refusal.
The historical v1.3 record classified all 54 refusals as `tool_error`; the
v1.3 live-equivalence golden below pins that classification.

## Deterministic record and digest rules

Both commands must write deterministic two-space-indented JSON with a single
final LF. Input digest fields are recomputed over the exact input file bytes;
they are never copied from another evidence record.

For the route matrix, `matrix_sha256` is computed by:

1. assembling the complete output object;
2. deleting only its top-level `matrix_sha256` member;
3. applying the equivalent of `json.MarshalIndent(value, "", "  ")`;
4. hashing those bytes without a trailing LF.

For the v1.3 matrix this produces
`f3163dc62d99eaaf55e400d646f299ab079a91981fb9d4ef84c09863bb8cfcae`.
The file-level digest is taken after restoring `matrix_sha256` and appending the
one final LF; it is therefore a different digest.

The isolation command recomputes and binds all four file-level links:

```text
profile_registry_sha256
product_intersection_matrix_sha256
outside_product_route_matrix_sha256
production_lookup_manifest_sha256
```

The activation-support consumer's `requireEvidenceDigest` checks are the
reverse oracle for these links. A golden test must run that consumer logic in a
temporary v1.3 root. Pointing the historical records at the current v1.4
registry must fail by design because execution evidence does not carry across a
contract release.

## How to rebuild the two records

The exact flag spelling below is the intended interface shape; `-h` for the
implemented commands is authoritative if it changes during implementation.

### Route matrix: offline plan and captured replay

The host-Go-only phase loads registry plus Product-intersection input, evaluates
`participates`, constructs and sorts `Q`, and validates the 7/9/54 and 21-pair
relationships. It performs no network or Docker operation:

```sh
go run ./evaluation/cmd/final-v5-route-matrix \
  -registry <registry.json> \
  -product-intersection <product-intersection-v1.json> \
  -derive-only
```

For a byte-exact historical replay, feed the derived plan and the frozen seven
activator output records to the aggregation phase and write to a temporary
path:

```sh
go run ./evaluation/cmd/final-v5-route-matrix \
  -mode aggregate \
  -registry <v1.3-registry.json> \
  -product-intersection <v1.3-product-intersection-v1.json> \
  -activation-evidence-dir <v1.3-routes> \
  -out <tmp>/outside-product-route-matrix-v1.json
```

This mode proves derivation, parsing, aggregation, field layout, serialization,
and digest construction. Each captured record must pass the complete activation
validator, and all seven must form one deployment's unique, contiguous
activation-sequence and predecessor chain. This mode does not claim to have
repeated those seven activations.

### Route matrix: fresh live execution

The live phase iterates the same canonical plan. For each participating profile
it activates that profile and passes the complete sorted `U - C(p)` list to the
existing `final-v5-profile-activate -outside-products` implementation. It then
aggregates the emitted activation record. The driver must use the deployment,
artifact, token, DSN, and evidence-output arguments required by the activator;
it must not add another HTTP probe path.

A minimal invocation shape is:

```sh
go run ./evaluation/cmd/final-v5-route-matrix \
  -mode live \
  -activation-evidence-dir <new-empty-dir> \
  -out <tmp>/outside-product-route-matrix-v1.json \
  -compose-project <project> \
  -deployment-id <deployment-id> \
  -profile-artifact-root <profile-artifact-root> \
  -profile-artifact-manifest <profile-artifact-manifest>
```

Deployment-specific Compose files, dataset binding, tokens, DSN environment
names, and schema attestations are forwarded with the corresponding flags shown
by `-h`. The driver preflights every evidence target before the first
activation and refuses to overwrite an existing per-profile JSON record.

If the complete deployment, required tokens, Business DSN, or per-profile
artifacts are absent, this phase exits with the explicit
`skipped_environment` result and does not write a matrix. An activation failure
also exits nonzero and does not write a matrix. Neither outcome is reported as
pass merely because the offline plan was derivable.

### Semantic-cache isolation: offline composition

Given existing inputs, the second command validates the 21 formal pairs, all 54
route probes and their ten properties, and the six miss/hit booleans carried
into the isolation record. It then recomputes the four digest links and emits
the composed record:

```sh
go run ./evaluation/cmd/final-v5-cache-isolation \
  -registry <registry.json> \
  -product-intersection <product-intersection-v1.json> \
  -route-matrix <outside-product-route-matrix-v1.json> \
  -production-lookup-manifest <production-lookup-manifest-v1.json> \
  -out <tmp>/semantic-cache-isolation-evidence-v1.json>
```

The six booleans written to the isolation record are the same-binding hit, four
one-member changed-binding misses (Catalog, grant, publication/dictionary set,
and task), and incomplete-binding rejection. The production manifest also
records the same-binding hit after the negative probes; the aggregator should
cross-check it even though the v1 isolation schema does not duplicate that
seventh field.

### Semantic-cache isolation: production lookup tests

The production-lookup half is backed by the two existing `internal/control`
tests:

```sh
./scripts/db-test-env.sh test ./internal/control \
  -run '^(TestSemanticCacheMissesUnderAChangedProfileBinding|TestSemanticCacheLookupRequiresACompleteBinding)$' \
  -count=1
```

With `-run-production-tests`, the isolation command first verifies the db-test
environment, invokes this harness with `-json`, and requires an explicit PASS
action for each exact test before reading the existing manifest. An unavailable
environment or skipped test returns exit status 3; a missing or failed test
returns exit status 1. In every such case the output is left untouched. The
command cross-checks the existing manifest; it does not regenerate it, because
the v1 manifest has no execution-status or skip fields.

Use the composition command above with the additional
`-run-production-tests` flag when the db-test environment is available. Omit
that flag only for a declared offline replay that consumes the frozen manifest.

## S5 golden criteria fixed before rerun

There are two different acceptance lanes and they must not be conflated.

### Offline byte-exact golden

The v1.3 historical boundary is commit
`f5a9766d5455b51d7a6d9a08bedf04bebbb91cf0`, immediately before the v1.4
freeze. The exact inputs and expected outputs are:

| Role | SHA-256 |
| --- | --- |
| v1.3 `config/profiles/registry.json` | `155a116c9aa37850de11cc7846aeb5d4e9cb202c8255b1e73a4c7b2fe393a9ed` |
| v1.3 `profile-coverage-v1.2.json` derivation audit | `adfd8a779ec34bb6ceab6980d5e66d56ef1e44b931ade59da528eb95fb81ebc9` |
| v1.3 `product-intersection-v1.json` | `ea8f083fdb986684b8136b55f09b5bb8bb15ea4a04d2e4e8b6faccb8521ad71a` |
| `production-lookup-manifest-v1.json` | `c959d611e01ff8d8c089477c932ff7e7adcc8f9555140396e5b1ef55512af6a4` |
| expected route `matrix_sha256` | `f3163dc62d99eaaf55e400d646f299ab079a91981fb9d4ef84c09863bb8cfcae` |
| expected route file | `342d257718a58bf3e084c7d776f5dc169b6a9d67f5063f72caff5cc1d1694bad` |
| expected isolation file | `6d6f281a43512ebe07bd53329c0e275e8c48d6a82587c731b7bde05a574e1712` |

The v1.3 expected bytes are frozen under the two commands' `testdata/v1.3`
directories. The canonical files under `evaluation/final-v5-wsl2/profiles/`
now carry fresh v1.4 evidence, so they must not be used as the historical
golden. The current registry and Product-intersection file are likewise v1.4
and must not be substituted for the historical inputs.

Seven matching v1.3 raw route activation records do exist on the present host
at:

```text
/home/wmm/taskgate-final-v13/activation-20260803T232701Z/routes/
```

They total 67,572 bytes, cover the seven participating aliases, and their 54
outside-Product entries (including response digests) match the committed v1.3
route matrix. The machine-local absolute path is provenance, not a reproducible
dependency. The seven complete records plus the v1.3 registry and intersection
bytes are frozen as compressed, source-controlled route-command testdata. The
isolation command separately freezes its five complete inputs/expected output
under `evaluation/cmd/final-v5-cache-isolation/testdata/v1.3/`. Those tracked
fixtures—not the machine-local archive—are the portable golden sources.

With those captured observations, the route command must reproduce the expected
route file byte for byte and return the `342d...` file digest. Feeding that
result and the frozen production lookup manifest to the isolation command must
reproduce the expected isolation record field for field and byte for byte. Any
difference is a defect; the test must stop rather than rewrite the golden.

### Fresh-live semantic golden

Fresh HTTP response bodies are not byte-stable. This is already demonstrated
by the two historical live records: comparing commit `7108986` (v1.2) with
`f5a9766` (v1.3), all 54 of 54 `response_sha256` values changed, while all of
the following remained equal:

- the ordered `(target profile, requested Product)` set;
- the per-profile counts of 8/8/8/6/8/8/8 and the total of 54;
- all ten negative properties on every probe;
- `stable_refusal_classification=tool_error` on every probe.

After removing release-specific digest links and `response_sha256`, the two
route objects are semantically equal. The corresponding isolation objects are
also semantically equal after removing their release-specific digest links.
This is the predeclared reason for the live acceptance boundary, not a
post-failure exception.

A fresh live run therefore passes the additional S5 comparison only when it has
exactly the derived 54 identities, the expected per-profile counts and
nine-Product universe, all 540 negative booleans true, all 54 stable
classifications equal to `tool_error`, and a valid lowercase response SHA-256
for every probe. This S5 check is a comparison against the stable historical
semantics; it does not compare response digests to v1.3.

Because `matrix_sha256` includes the response digests, a fresh live matrix
digest, route file digest, and the isolation record's route-matrix digest are
also expected to change. They must still be recomputed and internally
consistent. No other identity, count, assertion, classification, or digest
link is waived. A difference in one of those stable fields is recorded as a
golden defect and the rerun stops.

## Environment used for the authorized live rerun

The initial read-only check on 2026-08-08 found Docker 29.1.3, Docker Compose
2.40.3, and the existing `taskgate-dbtest` Control and Business PostgreSQL 16.14
containers healthy. After separate operator authorization, the rerun created
the unique Compose project `taskgate-v14-live-20260808t133622z` with new project
volumes and the following Compose files:

```text
compose.yaml
compose.debug.yaml
evaluation/final-v5-wsl2/compose.real-pilot.yaml
evaluation/final-v5-wsl2/compose.observer-v3.yaml
```

The two-phase bring-up started the complete activation substrate without first
starting a master-Catalog Gateway. All four long-running substrate services
became healthy and all five snapshot/object-store initialization jobs exited
zero. The resulting snapshot volume was copied read-only and materialized into
nine per-profile artifact directories; the live driver selected the seven
participating profiles. Credentials were resolved from the in-memory Compose
configuration and the Business DSN was derived from the isolated deployment's
loopback port. No interpolated Compose configuration or secret was persisted.

This two-phase pilot path proves new project volumes and an isolated live
deployment, but it does not emit the formal `start-fresh-deployment.sh`
freshness-proof document. The pre-existing db-test project was not stopped or
modified; it was used only by the two production semantic-cache lookup tests.

## Rerun result

The 2026-08-08 rerun completed the offline and authorized live lanes with these
outcomes:

- the current v1.4 offline plan derived 7 profiles, 9 unique Products, and 54
  probes without an allowlist;
- captured v1.3 aggregation reproduced the committed route file byte for byte,
  with file SHA-256
  `342d257718a58bf3e084c7d776f5dc169b6a9d67f5063f72caff5cc1d1694bad`;
- composition of that route with the frozen v1.3 registry, intersection, and
  production manifest reproduced the committed isolation file byte for byte,
  with file SHA-256
  `6d6f281a43512ebe07bd53329c0e275e8c48d6a82587c731b7bde05a574e1712`;
- seven v1.4 activations passed and emitted exactly 54 fresh HTTP probes with
  per-profile counts `8/8/8/6/8/8/8`; all 540 negative assertions were true,
  all classifications were `tool_error`, and every response carried a valid
  lowercase SHA-256;
- after removing only the predeclared release/digest links and per-response
  SHA-256 values, the fresh route and isolation objects were semantically equal
  to the v1.3 records;
- the first live route file, bound to the unsupported v1.4 registry, had file
  SHA-256 `ca70175bedb2fdddc669f66758be7d24881c873bb598cd1100bf750d8f9db965`;
  the corresponding staged isolation file was
  `b754d66552377e4a56257c73a104ee6421392fc6762580cb5692a29283f60877`;
- both production lookup tests executed through the db-test harness and passed;
- activation support changed the registry digest, so the same seven raw live
  records were aggregated a second time to reach a digest fixed point. The
  canonical v1.4 route file is
  `6e8883fcd41c2cbf0dd1efe11d1ee4725e8c33feab7607154929796c50c42bb1`
  with internal `matrix_sha256`
  `af2450e96a92e0b67797856bd705f4ffcda7315928d782c2b361723c90136d88`;
  the canonical isolation file is
  `d8442d569212acae9ee025a40b6907ca2a319633e0529b1edf9feb276e0e01c8`;
- the final registry digest is
  `ca84cba9810d378ea513bdadbf8cb9516a8bc3feaeb26b61af0156d857aee537`;
  regenerating it a second time was byte-identical;
- `config/profiles/activation-support-v1.json` has file SHA-256
  `c7f10b07efb443e739daaa4768aae0af52a94e5d98aea92874a9a1886bcac7a2`
  and activation-smoke-manifest SHA-256
  `eb239f271dfdf314559445541283468ad012ccbf77f30791a49840c68cdd152d`;
  it supports exactly the seven live-route profiles;
- the regenerated Product intersection remained
  `a6b5eb7cf9e6da53172348572722491c3cf0a3e3854f333db251a017437e0688`,
  while the regenerated coverage file is
  `a151412c7d2013414dfce9f4b2b9a9c827d0997f49f5a75ab89c10430cd6a603`.

Those seven profiles now have `activation_supported=true`,
`activation_smoke_passed=true`, `targeted_run_eligible=true`, and
`routable=false`. The remaining four profiles are unsupported. No targeted
Artifact pilot result is claimed here: `targeted_validation_passed` remains
false for every profile, so the fresh activation evidence does not promote a
profile to routable.
