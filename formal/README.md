# TaskGate TLA+ model

`TaskGate.tla` is a finite abstraction of one task. It models fresh and
replayed approval receipts, approval-time grant narrowing, scalar budget
reservation and settlement, one in-flight request, request-ID replay, three
crash outcomes, revocation, expiry, completion, rejection, and catalog drift.

Run the pinned stable TLC 1.7.1 toolchain in Docker:

```sh
make formal
```

The scalar budget transition is intentionally reusable: query count, result
rows, and database milliseconds use the same `used + reserved <= limit`
discipline in the implementation. A successful TLC run establishes the listed
invariants for the finite configuration in `TaskGate.cfg`; it is not a proof
that the Go implementation refines this model.

| TDSC requirement | Model element / invariant |
|---|---|
| Valid approval before execution | `ApproveFresh`, `NoExecutionWithoutApproval` |
| Relations, columns, scopes remain in the grant | `ReserveQuery`, `ExecutedWithinGrant` |
| Grant only shrinks | `NarrowGrant`, `GrantMonotonic` |
| Reserved and used budget stay bounded | `BudgetSafety` |
| Approval and request replay are idempotent | `ReplayApproval`, `DuplicateRequest`, `ApprovalReplaySafe`, `AtMostOnceExecution` |
| Revoked, expired, and terminal tasks start no new query | terminal transitions, `NoNewQueryAfterTerminal` |
| Catalog drift fails closed | `CatalogDrift`, `CatalogFailClosed` |
| Crash recovery is conservative | `DefinitePreConnectorFailure`, `CrashFromDurableReservation`, `CrashUnknownOutcome`, `CrashWithKnownResult`, `SettleKnown` |

`DefinitePreConnectorFailure` is only a known in-process failure before the
connector is called, so releasing its reservation is sound. A process crash
observed later from durable `RESERVED` state cannot establish that boundary;
`CrashFromDurableReservation` therefore charges the reservation, records
`INDETERMINATE`, and never resets the request for automatic retry. This matches
startup recovery without assuming a durable connector-boundary marker.

To explore a larger state space, copy the config and increase the finite sets
or counters. Keep the checked config and complete TLC log with any published
result.
