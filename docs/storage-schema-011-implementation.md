# Storage schema 011: bounded provider mutation architecture

**Status:** implemented and locally qualified when the complete pull-request
gate is green. The first declared release boundary is v0.7.0.

## Outcome

Schema 011 replaces request amplification that followed logical item count with
bounded, content-addressed state packs and compact transaction overlays. The
provider is still the durable source of truth, but a 10,000-route mutation no
longer rewrites and rereads a multi-page state tree. Stored file objects remain
immutable at their original provider keys; namespace mutation never reads,
copies, rewrites, relocates, or deletes their bytes.

The table below composes the exact executable provider budgets with the one
session-authentication read made by the HTTP route. Costs use the checked-in GCS
Regional Standard flat-namespace price fixture. Latencies are conservative
modeled values, not a Google SLA.

| Production intent | Schema-010 requests | Schema-011 requests | Schema-011 marginal cost | Schema-011 critical p95 |
|---|---:|---:|---:|---:|
| Browse 10,000 live or Trash rows | 80 | 3 | $0.0000012 | 0.210 s |
| Copy 10,000 selected roots | 128 | 5 | $0.0000112 | 0.409 s |
| Move 10,000 selected roots | 129 | 5 | $0.0000112 | 0.433 s |
| Trash 10,000 selected roots | 127 | 5 | $0.0000112 | 0.395 s |
| Restore 10,000 selected roots | 167 | 5 | $0.0000112 | 0.401 s |
| Permanently delete 10,000 selected Trash roots | 86 | 5 | $0.0000112 | 0.386 s |
| Replay a lost Trash/restore/delete response | 44 | 3 | $0.0000012 | 0.200–0.221 s |
| Deny a stale 10,000-item Trash intent | 44 | 3 | $0.0000012 | 0.210 s |
| Plan 10,000 upload sizes | 80 | 5 | $0.0000020 | 0.261 s |
| Plan 10,000 sizes and fingerprints | 140 | 9 | $0.0000036 | 0.392 s |
| Admit 10,000 real provider uploads | 20,700 | 10,014 | $0.0500608 | 1.259 s |
| Complete 10,000 uploaded objects | 100,000 | 10,014 | $0.0040562 | 1.223 s |
| Cancel 10,000 active provider sessions | 90,000 | 10,014 | $0.0000516 | 1.178 s |
| Resolve a 32-thumbnail visible window in 10,000 rows | 352 | 36 | $0.0000144 | 0.360 s |
| Sweep 128 unreachable checkpoint objects | 163 | 131 | $0.0000058 | 0.270 s |
| Compact a 300-item state domain | 15 | 5 | $0.0000112 | 0.356 s |

The 10,000 upload workflows retain exactly one unavoidable provider operation
for each actual object or upload session. The remaining fourteen requests are
one authenticated control-plane operation and thirteen state operations. A
dormant 10,000-record browser transfer ledger performs zero provider requests.

## Packed consistency-domain publication

A schema-011 domain head can reference one immutable `DomainPagePack`. The pack
contains canonical page records sorted by digest, uses deterministic gzip, and
is bounded to 4 MiB compressed and 32 MiB expanded. Readers authenticate the
head, the pack key binding, every contained page digest, the canonical encoding,
and the requested logical root. A mutation loads the head and its pack once,
changes the materialized pages, writes one new content-addressed pack, and
publishes one conditional head replacement. The head CAS is the sole visibility
point.

Pack identity includes the complete mutation binding before the first page is
written. Two mutations that happen to share an early page therefore cannot
alias a pack whose later pages differ. Corrupt, oversized, non-canonical,
misbound, missing, or digest-inconsistent packs fail closed. Large domains that
do not fit the bounded pack continue to use authenticated copy-on-write pages;
the optimization is an encoding choice, not a weakening of the domain contract.

For a 10,000-root namespace mutation the raw storage operation is two reads and
two writes. The composed HTTP route adds one authentication read. A directory
move retains the immutable child root, so moving a directory with one descendant
or one million descendants performs the same metadata operation. Replay reads
the retained compact outcome and reconstructs uniform item success instead of
loading 10,000 stored result rows.

## Bounded upload transactions

The browser and API accept one batch containing up to 10,000 uploads. Admission
publishes all portable upload intents atomically, starts independent provider
sessions through a bounded 100-worker pool, and persists sealed provider leases
at each 1,000-item boundary. Provider leases are encrypted transient recovery
state, are size bounded, never enter authoritative checkpoints, and never expose
provider URLs through canonical application state.

Completion verifies provider size and CRC32C metadata in the same bounded
worker pool. It persists authenticated completion progress every 1,000 items,
then publishes file entries and terminal upload records in one packed namespace
head transition. Cancellation aborts the provider sessions with the same
progress discipline and publishes one compact batch-ID/count/bitmap overlay;
it does not rewrite 10,000 immutable admission records. A partial or legacy
cancellation deliberately falls back to individually bound records because a
whole-batch overlay would be semantically incorrect.

If a pod stops between progress boundaries, the next replica validates the
stored prefix and repeats at most the current 1,000-item segment. A stale worker
cannot publish after another replica wins the final head CAS. Completion and
cancellation racing across replicas have exactly one visible winner. Replaying
the same idempotency key returns the committed result; changing the ordered
intent conflicts.

The browser persists batch ID, index, count, idempotency keys, planner state,
and transfer status in IndexedDB. Refresh or connection loss therefore resumes
the same server-side transaction. It sends the compact batch ID only when the
selected active records are proven to be the complete admitted batch.

## Preview and maintenance bounds

Ready preview metadata is indexed by cache scope in one bounded, sorted catalog.
A virtualized visible window resolves up to 64 selections in one control-plane
request and issues direct artifact capabilities only for the visible 32-item
window. Immutable artifact capabilities require no preliminary provider lookup;
the Go service still never proxies image bytes except inside the expressly
optional preview-generation feature.

Checkpoint garbage collection authenticates a frozen reachability inventory,
writes a deterministic paged garbage plan while the gate is closed, validates
the complete plan before deleting anything, and then performs conditional
deletes with bounded concurrency. The cursor publication makes restart
idempotent. Unknown keys, changed native versions, stale gate/checkpoint
bindings, corrupt pages, cycles, missing entries, or misplaced roles fail
closed before destructive work.

## Migration and compatibility

`010 -> 011` is one append-only, feature-only edge. It closes and drains the
canonical write gate, verifies the complete current authority under the frozen
checkpoint, advances writer/superblock/gate features, creates the migration
checkpoint, and reopens at the next gate epoch. It does not rewrite file blobs
or eagerly repack every schema-010 state page. Existing authenticated pages
remain readable and the first relevant schema-011 mutation publishes their
packed successor.

Four immutable epoch-011 fixtures are bound to producer commit
`4c5694008e30489e76ad1b7e3c959229d25fa7c1`: portable-minimal, real application
startup with previews disabled, real application startup with GCS previews,
and the complete cryptographic-passkey corpus migrated from schema 010 and then
mutated by the schema-011 writer. The migration matrix opens every epoch/profile
in single- and split-backend layouts, traverses the complete remaining suffix,
crashes after every durable edge boundary, resumes, converges concurrent
replicas, denies corruption, completes a new mutation, and never reads a stored
file body.

## Executable evidence

The principal proofs are:

- `TestNamespaceBatchTrashPublishesTenThousandEdgesThroughOneHead`,
  `TestNamespaceBatchRestorePublishesTenThousandEdgesThroughOneHead`, and
  `TestProviderBudgetNamespaceCopyAndMoveTenThousandRoots`;
- `TestProviderBudgetUploadBatchTenThousandLifecycle`,
  `TestPortableUploadBatchAbortIsAtomicAndReplayable`,
  `TestConcurrentCompletionAndCompactBatchAbortHaveOneAtomicWinner`, and
  `TestUploadBatchAbortProgressRestartBoundsRepeatedProviderWork`;
- `TestProviderBudgetVisiblePreviewWindowUsesOneReadyCatalogRead` and
  `TestProviderBudgetCheckpointGarbageCollection128Objects`;
- `TestProviderBudgetProductionScaleScenariosConformToTargets` and the exact
  schema-011 GCS economics delta; and
- the complete migration, replica, portability, race, coverage, and Nix gates
  required by `AGENTS.md`.
