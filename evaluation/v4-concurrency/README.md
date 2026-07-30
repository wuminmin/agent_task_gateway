# V4 same-root concurrency acceptance

This supplemental campaign uses the real MCP Gateway, Control PostgreSQL, OA
request/submit/approve flow, immutable expense-detail snapshot, V4 bitmap
derivation, encrypted result store, audit chain, and root-head CAS.

Two identical Gateway replicas share the same Catalog, snapshot artifacts,
Control PostgreSQL, and root-family ledger. Contenders are assigned
round-robin, so width 16 sends eight requests to each process. This stays
below the production `database/sql` limit of 10 Control connections per
Gateway while still making all 16 requests enter one transitive wait chain
rooted at the held root row. Prefix, public provisioning, and B+1 use the
primary Gateway only.

The measured queue value is the number of same-user PostgreSQL lock waiters
transitively downstream of the acceptance-owned backend that holds the target
root row. It establishes that every contender was queued behind that lock
before release. A transitive lock chain does not reveal which root epoch a
contender read, so this campaign does not infer a CAS conflict or retry count
from the queue.

The dedicated Catalog fixes the human-reviewed boundary at `2R/3I/2O`.
`prefix_plan` releases `receipt_no` (`1R/2I/1O`), the shared contender adds
`amount` and a new outcome (`+1R/+1I/+1O`), and `overflow_plan` adds `city` and
another outcome. Thus B-1 and B succeed and B+1 must fail closed. Release is
the repeated boundary label for width 16 because four required widths cover
three dimensions; every cell nevertheless checks all three dimensions.

Run with a fresh Compose project/volumes:

```sh
docker compose -p taskgate-v4-concurrency \
  -f compose.yaml -f evaluation/v4-concurrency/compose.yaml up -d --build

export V4_CONCURRENCY_CONTROL_DSN="postgres://gateway_control:${CONTROL_DB_PASSWORD}@127.0.0.1:${CONTROL_POSTGRES_PORT:-25433}/${CONTROL_POSTGRES_DB:-taskbound_gateway}?sslmode=disable"

go run ./evaluation/cmd/v4-concurrency \
  -config evaluation/v4-concurrency/template.json \
  -prepare -output /new-private-run/config.json

go run ./evaluation/cmd/v4-concurrency \
  -config /new-private-run/config.json \
  -output /new-private-run/results.json
```

The prepared configuration contains task IDs but no passwords or bearer
tokens. The result hashes every task identity and binds both config and source
digests. Client latency is descriptive; this axis introduces no new SLO.
