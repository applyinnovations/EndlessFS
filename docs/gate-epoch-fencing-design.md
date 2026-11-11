# Gate-epoch fencing for consistency-domain freezes

Status: proposal, not implemented. Needs a reviewed specification change and
a new storage schema epoch before any code lands.

## Problem

Checkpoint closure freezes the domain catalog and then every registered
domain head at the closing write gate's epoch. Reopening thaws every domain
and then the catalog. When several replicas start at once, each one may run
the same migration edge. A replica that falls behind (descheduled, slow
disk, long GC) can still be inside `finishClosingWrites` for epoch E after
another replica has finished that checkpoint, thawed everything and moved the
gate on. Its next write then freezes the catalog or a domain at E again.

Nothing rejects that write. A domain head only checks its own state: an
unfrozen head accepts a freeze at any epoch. The result is a freeze no gate
authorizes:

- a domain frozen at E while the catalog is thawed or frozen at a later
  epoch; every mutation of that domain then fails, and the next closer fails
  with "frozen at another epoch";
- a catalog frozen at E whose domains are frozen at other epochs;
  `domainCatalog.unfreeze(E)` stops at the first domain that is not frozen at
  E, so `reconcileGateDomainFreeze` retries until it gives up.

PR #80 thaws a stale domain freeze where it is found (closure and the
mutation paths). That repairs the common case but cannot stop a lagging
closer from writing the stale freeze again immediately after it was thawed,
and it does not unblock a catalog thaw. Under eight replicas on two CPUs,
about 1% of startups still fail this way.

## Proposal: thawed-epoch watermark

Make a stale freeze impossible instead of repairing it afterwards.

Add a monotonic `thawedEpoch` to every domain head and to the catalog head.

- `freeze(E)` succeeds only if `E > thawedEpoch` (and the existing rules: not
  frozen, or already frozen at E). A freeze at or below the watermark returns
  a conflict that names the watermark.
- `unfreeze(E)` sets `frozen = false`, `freezeEpoch = 0`,
  `thawedEpoch = max(thawedEpoch, E)` in the same conditional write.
- `freezeEpoch` and `thawedEpoch` change only through these two writes, each a
  compare-and-swap on the head, so the watermark is ordered with every freeze.

Gate epochs strictly increase across closures, so after the checkpoint at E is
thawed, a lagging closer's freeze at E (or earlier) is rejected by every head
it touches, and the next real closure at E+1 still passes. The fence is a
per-object comparison inside a write that already happens; it needs no extra
provider call, no leader and no clock.

### Behaviour of a fenced closer

`finishClosingWrites` treats "freeze rejected by watermark" as "the
checkpoint I was closing has already been completed": it stops, rereads the
gate and returns a conflict, which `closeStorageMigrationGate` /
`resolveMigrationRunError` already turn into a reread of the current state.
It never thaws anything it did not freeze.

### What it replaces

- The help-thaw added in #80 (`thawStaleFreeze`, the stale branch in
  `finishClosingWrites`) becomes unreachable for heads written by the new
  schema. Keep it for heads migrated from older epochs until no supported
  release can produce them, then remove it in a reviewed change.
- `reconcileGateDomainFreeze` keeps only the catalog suffix it has today; the
  per-domain blocking case cannot arise.

## Format change

| Object | Field | Rule |
| --- | --- | --- |
| Domain head | `thawedEpoch` (uint64, omit when 0) | `frozen` implies `freezeEpoch > thawedEpoch` |
| Catalog head | `thawedEpoch` (uint64, omit when 0) | same |

Both decoders are strict, so the field is a durable format change: older
binaries reject heads that carry it. It ships as a new required writer
feature (`freeze-watermarks-v1`) and a new schema epoch.

## Migration

New ledger edge from the current schema to the next:

1. Close the gate through the existing migration path (catalog-first freeze
   at the closing epoch C).
2. For every registered domain and the catalog, set `thawedEpoch` to the
   epoch of the last checkpoint that thawed it. That value is not stored
   today, so use `C - 1`: every freeze from a completed closure is at an
   epoch below C, and no closure at or above C has happened yet. Heads
   currently frozen at C keep `freezeEpoch = C`.
3. Activate the writer feature and superblock, bind the gate, write the
   checkpoint and reopen; reopening thaws at C and sets `thawedEpoch = C`.

Like every edge it needs the immutable predecessor fixture for each profile,
full-suffix tests, restart after every durable boundary, two-to-eight-replica
convergence, and fail-closed tests for a head whose `thawedEpoch` is above the
gate epoch or above its own `freezeEpoch`.

Mixed versions: until the writer feature is active, no head carries the field
and old replicas run unchanged. After activation, older binaries are refused
by the writer-set check before they can write a head.

## Alternatives rejected

- **Bound the reconcile loop by time (30 s) instead of 16 attempts.** Tried
  in stress runs: when a domain is frozen at another epoch, catalog thaw
  fails every time, so a longer budget only fails later. It hides the cause.
- **Thaw every stale per-domain freeze during catalog thaw.** Tried: it raced
  with live closures and raised failures from about 1.5% to about 5% of
  startups at two CPUs
  ("registered consistency domain is not checkpoint-frozen", "verify
  authoritative state before migration activation"). Without a watermark a
  thaw cannot tell a stale freeze from a newer closure's.
- **Recheck the gate after freezing and undo.** Narrows the window but cannot
  close it: the gate can move between the recheck and the next write.
- **Leave the write gate out of the checkpoint inventory.** Verification of a
  copied checkpoint needs the closed gate, so this is a checkpoint-format
  change. It also does not fix the freezes; the inventory conflicts it
  targeted are handled by reporting a conflict and resuming instead.
- **A single migration leader.** The specification forbids leaders and
  process-local coordination as correctness dependencies.
- **Fence with the gate object in the same write.** Object stores offer no
  multi-object transaction; the watermark keeps the fence inside one object.

## Evidence to require

- `TestEightReplicasConcurrentlyMigrateSchema001Fixture` and
  `TestMigrationPendingCleanupReplicasConverge`: 0 failures in `-count=100`
  at 2, 4 and 8 CPUs (`taskset` + `GOMAXPROCS`) and in `-count=20 -race`.
- A deterministic test, without sleeps, that pauses a closer between reading
  the gate and freezing, completes the checkpoint with another replica, then
  resumes the first: its freeze is rejected and leaves every head unchanged.
