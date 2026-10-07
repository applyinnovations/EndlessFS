# Pending terminal-upload cleanup recovery

## Failure and selected repair

On 2026-10-07 the xlab deployment still had two ready v0.6.0 replicas and one
v0.7.1 replica with `ProgressDeadlineExceeded` since 2026-09-16T08:29:12Z. The
new replica repeatedly logged `schema-010-to-011` at `started`, then stopped
with `unavailable: consistency domain is frozen`. Readiness on the old replicas
did not prevent observed authentication and directory-creation HTTP 503s.

`finishClosingWrites` freezes the catalog and domains before upload drain.
The released drain calls ordinary terminal cleanup, which performs transient
provider effects and then conditionally clears `CleanupPending` in the owner
domain. That last commit correctly denies a frozen domain. Closure rolls back
the freeze on the unavailable error, so every restart repeats the attempt.

The repair separates transient provider effects from the ordinary authoritative
cleanup commit. Frozen drain authenticates the exact owner/upload/blob binding
and runs only the idempotent provider effects. Terminal record bytes, logical
versions, and the pending flag remain unchanged throughout checkpoint closure.
Normal completion/cancellation replay can clear the flag after reopening.
No epoch, feature signature, release boundary, key, or record format changes.

Rejected alternatives: allowing ordinary commits through frozen heads would
break the checkpoint boundary; manually editing freeze flags would bypass its
epoch/proof binding; a pre-freeze cleanup pass alone would add another complete
traversal while still allowing concurrent writers to leave new pending cleanup.
The selected split retains the successful ordinary-upload request shape and
avoids a redundant upload/head lookup during frozen drain.

## Guarantee evidence

| Guarantee | Proof |
|---|---|
| Frozen authority remains unchanged | Completed/aborted terminal records retain exact bytes and logical versions through closure and repeated drain; ordinary cleanup remains denied under freeze. |
| Provider failure remains retryable | Injected lease deletion failure preserves pending state, reopens the gate and unfreezes domains; a recovered provider permits retry. Existing malformed-record, misbinding, corruption, and live-capability denial matrices remain required. |
| Migration conserves authority | Both immutable predecessor fixtures enter the full epoch/profile matrix, preserve namespace aggregates, complete a newly signed passkey assertion, authenticate its session, and complete a new mutation. |
| Crash recovery | Both fixtures restart after all seven declared `010 -> 011` boundaries. The pending-cleanup fixture also enters the exhaustive state/file transport-interruption matrix. |
| Concurrent migration | Two through eight independently constructed engines start at an explicit detection barrier, converge, preserve file-object bytes, and continue identity/namespace operations. |
| File data stays outside migration | Instrumented file-role events reject body reads, puts, copies, and deletes. Existing immutable objects are referenced in place. |
| Raw-copy portability | A second frozen checkpoint with pending cleanup copies only its authority and checkpoint metadata to fresh independent state/file backends, excludes leases, reopens, authenticates, and mutates. |
| Successful ordinary cleanup | Existing valid-path tests still clear pending state through ordinary conditional publication; provider and transfer denial suites remain required. |

Principal tests: `TestCheckpointTerminalUploadCleanupPreservesFrozenAuthority`,
`TestCheckpointTerminalCleanupProviderFailureDoesNotPublishOrStrandFreeze`,
`TestMigrationPendingCleanupReplicasConverge`,
`TestCheckpointPendingCleanupRawCopyPreservesAuthority`, and
`TestProviderBudgetMigrationPendingCleanup`, plus the owning migration,
provider-budget, portability, replica, race, and complete PR gates.

## Immutable fixture provenance

The supplemental fixtures preserve the complete schema-010 application corpus
and its independent cryptographic semantic oracle. They add one completed
four-byte file and one aborted upload whose terminal commits succeeded while
transient lease deletion was deliberately interrupted. Fixture bodies were
written by the recorded predecessor production code, never synthesized or
normalized by the repair.

| Fixture | Producer | SHA-256 |
|---|---|---|
| `schema-010-v0.6.0-pending-cleanup.json` | v0.6.0, `e7a9a46afede8e4b154e876700ed372e97105aed` | `f47611085c11c1176ff5f85d801972051626e31530573f201607305b28978bff` |
| `schema-010-v0.7.1-interrupted-cleanup.json` | v0.7.1, `9650c8a1c8e107f17e71b4272200c77f6ed42eac` | `1d369f375028e2471acbefb04f4fc48beb3b0ac353d4f2c06959f5b31410d4ea` |

The archived Go test drivers in `internal/portable/testdata/migrations/producers`
are copied into the indicated predecessor checkout's `internal/portable` package.
The v0.6.0 driver loads its already-bound complete application fixture and writes
the new raw fixture via `ENDLESSFS_RECOVERY_FIXTURE_OUTPUT`. The v0.7.1 driver
loads those exact bytes via `ENDLESSFS_RECOVERY_FIXTURE_INPUT`, requires the
released frozen-domain failure, and writes the resulting raw stores without
repair. Both were executed through the predecessor's `nix develop` toolchain.
They are provenance, not regeneration commands for existing immutable fixtures.

## Measured provider economics

The selected complete single-replica migration was measured with the existing
offline GCS Regional Standard flat-namespace model. These supplemental workloads
exercise cleanup recovery; their small file bodies are semantic evidence, not
large-inventory performance qualification. Expected not-found/create-only
responses are counted, including provider abort of an already absent session.

| Entry point | State calls | File calls | Request/response bytes | Marginal USD | Modeled critical p50 / p95 / p99 | Retained state objects before / after |
|---|---:|---:|---:|---:|---|---:|
| v0.6.0 pending cleanup | 138 | 7 | 31,252 / 225,540 | 0.0001898 | 3.304 / 9.983 / 24.683 s | 203 / 204 |
| v0.7.1 failed-upgrade residue | 137 | 7 | 31,254 / 225,534 | 0.0001898 | 3.284 / 9.923 / 24.533 s | 202 / 204 |

File-role response/request body bytes are zero in both workloads. The original
file objects remain unchanged. Exact call/cost/latency measurements append the
`008-frozen-upload-cleanup-migration` economics delta; previous ratchets remain
immutable. Test diagnostics expose the complete role, request-kind, subsystem,
failed-request, byte-transfer, and critical-path vectors. Concurrent migration
tests instrument the full traffic and reject file-body access; their contention
counts depend on the admitted schedule and are not assigned a guessed ceiling.

## Deployment sequence

1. Read the live superblock, writer set, gate, catalog, and every registered
   domain head. A v0.6.0 rollback is allowed only if the storage features remain
   schema 010 and the gate/catalog/domains are open/unfrozen. If schema 011 has
   activated or a closure remains in progress, use the verified forward repair.
2. Merge the validated xlab GitOps rollback to the existing v0.6.0 digest when
   that precondition is satisfied. Let Flux reconcile; do not mutate the live
   deployment or bucket objects manually. Verify two ready replicas and that
   the failing migrator disappears.
3. Merge the repair only after focused checks and the exact complete local
   `pr-check`, `flake check`, and `test-coverage` sequence pass. Require CI's
   confirmation on the reviewed merge candidate.
4. Build the repair release from the merged commit. Before pushing its tag,
   run `nix run .#test-migration -- v0.7.2` and
   `nix flake check --print-build-logs` on that exact tagged commit.
5. Pin the verified published OCI digest in a second xlab deployment PR, render
   and run its local checks, then merge. Verify migration reaches completion,
   both replicas run the new digest, the gate/domains reopen, and authentication
   and file mutations succeed. Preserve application secrets, identities, and
   bucket pairing throughout.

Live observations establish this incident's rollout failure; they do not replace
the deterministic provider-portability or production-qualification gates.
