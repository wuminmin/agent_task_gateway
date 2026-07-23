# TaskGate TLA+ model

`TaskGate.tla` is a finite abstraction of one task. It models fresh and
replayed approval receipts, approval-time grant narrowing, scalar budget
reservation and settlement, one in-flight request, request-ID replay, three
crash outcomes, revocation, expiry, completion, rejection, and catalog drift.
`VectorBudget.tla` is a smaller split model for the reserve/settle/release
discipline over the query, row, and DBMS budget vector.
`SQLAuthorization.tla` separately models product-to-column provenance,
qualified and unqualified name binding, constant-only safe lists, and
multi-product function/operator intersections. `MultiTaskAudit.tla` models
multi-task request interleavings, per-task one-in-flight admission, revocation
or expiry races with in-flight requests, and the global audit sequence.
`ReceiptAudit.tla` models persisted terminal receipt semantics and the binding
between receipt audit fields and the terminal audit event. `RecoveryLiveness.tla`
models conservative recovery under weak fairness.
`ExposureLedger.tla` models the TKDE data-semantics path: two distinct fact
ledgers shared by a root task and its delegated child, execution into a
withheld buffer, provenance derivation, exact novel-fact settlement,
over-budget rejection before delivery, terminal replay, and root revocation.
`REFINEMENT.md` maps each modeled action to the current Go method,
PostgreSQL transaction or invariant, and test/fault-injection evidence. That
mapping is an audit artifact, not a mechanized refinement proof.

Run the pinned stable TLC 1.7.1 toolchain in Docker:

```sh
make formal
```

`make formal` writes the original TaskGate result to `formal/results/tlc.json`
and writes one JSON/log pair for each split model under `formal/results/`,
including `exposure_ledger.json` and `exposure_ledger.log`.

The core task lifecycle model keeps a scalar budget to control state growth.
The split vector-budget model checks the same `used + reserved <= limit`
discipline independently for query count, result rows, and database
milliseconds. The exposure model uses finite sets of release and source-
influence facts and checks the root-family set-union ledger directly.
Successful TLC runs establish the listed invariants and temporal properties for
the submitted finite configurations; they are not a proof that the Go
implementation refines these models.

| Property | Model element / invariant |
|---|---|
| Valid approval before execution | `ApproveFresh`, `NoExecutionWithoutApproval` |
| Relations, columns, scopes remain in the grant | `ReserveQuery`, `ExecutedWithinGrant` |
| Grant only shrinks | `NarrowGrant`, `GrantMonotonic` |
| Reserved and used budget stay bounded | `BudgetSafety` |
| Per-dimension vector budget stays bounded | `VectorBudgetSafety` in `VectorBudget.tla` |
| Settlement never charges more than reservation | `TerminalChargeBounded`, `ReleaseChargesNothing`, `IndeterminateChargesFullReservation` in `VectorBudget.tla` |
| Product-column provenance and product-specific SQL controls | `AcceptedQueriesAuthorized`, `AcceptedSourcesMatchColumns`, `UnqualifiedColumnsAreUnique`, `MultiProductUsesIntersection` in `SQLAuthorization.tla` |
| Multi-task interleaving and global audit order | `SingleInFlightPerTask`, `AuditIsLinear`, `TerminalRequestsHaveTerminalAudit` in `MultiTaskAudit.tla` |
| Revocation/expiry races block new requests but preserve in-flight settlement | `NoNewRequestAfterTaskTerminal` in `MultiTaskAudit.tla` |
| Terminal receipt semantics and audit binding | `ReceiptExistsForTerminal`, `ReceiptBindsTerminalAudit`, `BudgetTransitionValid`, `StatusFieldsValid` in `ReceiptAudit.tla` |
| Approval and request replay are idempotent | `ReplayApproval`, `DuplicateRequest`, `ApprovalReplaySafe`, `AtMostOnceExecution` |
| Revoked, expired, and terminal tasks start no new query | terminal transitions, `NoNewQueryAfterTerminal` |
| Catalog drift fails closed | `CatalogDrift`, `CatalogFailClosed` |
| Crash recovery is conservative | `DefinitePreConnectorFailure`, `CrashFromDurableReservation`, `CrashUnknownOutcome`, `CrashWithKnownResult`, `SettleKnown` |
| Recovery eventually converges when the recovery step remains enabled | `RecoveryConverges` in `RecoveryLiveness.tla` under `WF_vars(RecoverStep)` |
| Release and source-influence exposure stay independently bounded | `DualBudgetSafety` in `ExposureLedger.tla` |
| Only facts novel to the root task are charged | `ExactNovelCharge`, `NovelChargesDoNotOverlap`, `TaskFamilyNonAmplification` |
| Query results remain withheld until successful settlement | `NoDeliveryBeforeSettle`, `RejectedResultsStayBuffered` |
| Provenance evidence corresponds to the buffered execution | `DerivedEvidenceMatchesBuffer` |
| Root and delegated tasks cannot multiply exposure by repeating facts | shared `knownRelease`/`knownInfluence`, `TaskFamilyNonAmplification` |
| Terminal retry does not execute or charge again | `DeliveredQueriesExecutedOnce`, `NoChargeWithoutSettlement`, `Replay` |

`DefinitePreConnectorFailure` is only a known in-process failure before the
connector is called, so releasing its reservation is sound. A process crash
observed later from durable `RESERVED` state cannot establish that boundary;
`CrashFromDurableReservation` therefore charges the reservation, records
`INDETERMINATE`, and never resets the request for automatic retry. This matches
startup recovery without assuming a durable connector-boundary marker.

To explore a larger state space, copy the config and increase the finite sets
or counters. Keep the checked config and complete TLC log with any published
result.

Before using the models as paper evidence, review `REFINEMENT.md` and keep the
paper claim within the mapped abstraction. The current model suite still does
not provide a mechanized Go refinement proof, does not model cryptographic key
custody or external WORM/timestamp guarantees, and keeps receipt/audit bytes
abstract rather than modeling signature formats. `ExposureLedger.tla` also
abstracts the SQL-to-provenance compiler and FactID hashing as exact finite-set
inputs; their correctness is covered by executable algebra tests and the
evaluation corpus, not by TLC.
