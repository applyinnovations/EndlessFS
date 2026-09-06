# Storage schema 008 implementation record

**Status:** implemented as the only ordinary runtime backend; untagged and not
yet released or deployed

**Date:** 2026-08-25

**Evidence base:** [`storage-architecture-v2-proposal.md`](./storage-architecture-v2-proposal.md),
[`storage-architecture-experiment-2026-08-24.md`](./storage-architecture-experiment-2026-08-24.md),
and [`storage-architecture-comprehensive-economics-2026-08-24.md`](./storage-architecture-comprehensive-economics-2026-08-24.md)

## 1. Cutover result

Schema 008 replaces the schema-007 runtime. `FileStore` and `StateStore`
ordinary operations now resolve only through consistency domains. The old
filesystem, operation, transfer, state-index, and synchronous duplicate-index
implementations are removed from the runtime surface. Frozen schema-007
decoders remain private to the adjacent `007 -> 008` transformer because the
append-only migration contract requires every released predecessor to remain
upgradeable. They are compatibility readers, not an alternate backend or a
fallback path.

The runtime boundary test executes create-directory, move, copy, Trash,
restore, and permanent delete through the public store and rejects every:

- provider `LIST`, `Open`, `Copy`, or `Delete` call;
- stored-file body access; and
- schema-007 state, index, admission, operation, idempotency, duplicate, or
  filesystem key.

Schema 008 is intentionally untagged in this pull request. Its release ledger
boundary is appended only by the first release that actually writes it.

## 2. Authority model

Each invariant is stored in the smallest consistency domain that must commit
atomically:

| Domain | Partition | Authority |
| --- | --- | --- |
| Namespace | owner | live/Trash graph, stable directory nodes, file references, recursive byte/file aggregates, upload publication, namespace outcomes |
| Owner control | owner | profiles, accounts, preferences, owner-scoped operations and idempotency |
| Administration | global | bootstrap and administrator-set invariants |
| Capability | bounded digest shard | credentials, sessions, ceremonies, invitations, recoveries, and independent operation records |
| Share | bounded digest shard | private/public share lifecycle |

The generic application `state.Store` API is routed deterministically into
these domains. A state key without an exact route is rejected. Prefix listing
uses authenticated immutable snapshots and can merge bounded pages across
domains; provider listings never define application state.

A domain head contains its full identity and kind, one logical revision, a
content-addressed base-tree root, compacted idempotency-outcome and expiry-tree
roots, and at most 32 ordered deltas. Each delta binds the complete normalized
intent fingerprint, mutation ID, contiguous revision, expiry, changes, and
result. A domain head CAS is the sole visibility point.

The immutable trees are canonical, content addressed, and high fan-out. Every
page binds its domain, level, key range, children or values, logical versions,
entry count, byte count, and exact body digest. Reads reject noncanonical,
misbound, missing, altered, reordered, revision-gapped, or digest-mismatched
authority.

## 3. Mutation and recovery protocol

An ordinary mutation:

1. reads the exact domain head and only the immutable page paths needed to
   validate its preconditions;
2. prepares changed immutable pages, which are not authoritative yet;
3. conditionally publishes one new head containing the change and retained
   outcome; and
4. on an ambiguous provider response, rereads the head and returns the exact
   committed outcome when its mutation fingerprint won.

There is no candidate ticket, global admission transaction, mutable operation
record, fence lease, or synchronous projection update on this successful path.
A conflicting writer cannot publish through the stale native head condition.
A retry with the same mutation ID and fingerprint returns the retained outcome;
reuse with a different intent fails closed.

Compaction folds the bounded delta window into structurally shared base and
outcome trees. It writes only changed immutable pages and conditionally swaps
the same head. An interrupted compaction leaves unreachable pages but cannot
change visibility. Expired outcomes are removed through the authenticated
outcome-expiry tree; elapsed time alone never unlocks or authorizes a mutation.

Crash injection proves every namespace mutation exposes no change before its
head commit. Lost-success injection proves the originating call and another
replica can recover the committed result. Eight independently opened replicas
racing one idempotent mutation converge on one operation outcome.

## 4. Namespace, Trash, and large batches

An owner namespace is a persistent tree of stable nodes. Live and Trash are
root entries in the same owner domain. Directory entries contain exact child
tree roots and recursive byte/file aggregates. Move, rename, Trash, restore,
copy-by-reference, and logical delete rewrite only the affected edges and
ancestor paths. They do not enumerate descendants and never relocate a blob.

A folder move is therefore proportional to changed path depth and immutable
tree height, not subtree descendants. A 256-descendant regression fixture
observes the same provider-call shape as a small folder. Copy shares unchanged
immutable content and forks only rewritten paths.

Explicit batches are different: each selected root must be validated and
represented. Schema 008 prepares their changed pages with a 32-worker bound
and publishes the entire batch with one head CAS. The executable 10,000-file
Trash fixture performs exactly 125 state-provider requests, one authoritative
head `PUT`, zero file-provider requests, and zero body reads. Its measured GCS
model is:

| Metric | Aggregate work | Modeled critical path |
| --- | ---: | ---: |
| Requests | 125 | bounded page waves plus one head CAS |
| Marginal request cost | $0.0004318 | same requests |
| p50 | 3.175648 s | 1.089983 s |
| p95 | 9.546648 s | 3.115983 s |
| p99 | 23.496648 s | 7.585983 s |

Aggregate latency is the sum of all modeled requests. Critical latency
collapses only page writes explicitly marked as independent and is the better
estimate of user-visible provider time. These are deterministic engineering
estimates from the checked-in GCS fixture, not Google SLAs or live benchmarks.

## 5. Upload and file-body boundary

Browser uploads target their final immutable blob key directly. The Go service
stores only portable upload authority and an excluded encrypted provider lease;
it never proxies the bytes. Completion verifies provider-attested size, MD5,
and CRC32C, then publishes the blob reference and terminal upload state in the
owner namespace head. It then conditionally deletes the now-inert runtime lease;
abort terminates the provider session before conditionally deleting the same
lease. Both cleanup paths are idempotent after lost responses, and completion
never invokes provider abort after the final blob exists. It performs no
provider copy or rewrite. Downloads are direct provider capabilities over
committed blob references.

Move, copy, Trash, restore, permanent logical delete, duplicate
reconciliation, schema migration, and checkpoint construction issue zero file
body reads and zero file-provider copy/delete requests. Checkpoint closure uses
provider `Head` metadata to validate each reachable blob.

## 6. Duplicate projections

Duplicate groups and directory-overlap views are derived projections over the
owner namespace revision. A file identity is the provider-backed tuple
`(size, MD5, CRC32C)`. Projection pages are immutable and rebuildable; they do
not participate in namespace commits. Ignore decisions are authoritative
owner-control values. A reconciliation token binds the selected projection and
namespace revision, and apply revalidates both before one namespace mutation.

This preserves exact duplicate groups, full-directory equality, subset and
intersection workflows, and intentional-ignore behavior without synchronously
rewriting a bucket-wide duplicate index for every file mutation.

## 7. Checkpoint freeze and portability

The canonical domain catalog contains every registered head. First use creates
an inert head, registers it in the catalog, and only then activates it, so a
concurrent freeze either excludes a non-authoritative head or names all
authority that can publish.

Checkpoint closure:

1. closes the storage-set gate;
2. freezes the catalog at that epoch;
3. freezes every catalogued domain by CAS;
4. drains or aborts expired upload capabilities and refuses closure while a
   live capability remains;
5. authenticates every base, delta, outcome, namespace, and reachable blob
   reference; and
6. constructs the metadata-only checkpoint inventory.

A writer paused before publication loses its head CAS after freeze. Startup
reconciles a lost-success reopen by idempotently unfreezing every domain before
serving mutations. Reachability uses a disk-backed exact visited set and
bounded merge chunks, so checkpoint memory does not grow with the complete
object graph. Provider bytes are not downloaded to discover or verify file
content.

## 8. Migration and high-availability findings

The adjacent `007 -> 008` edge stages deterministic domain values, installs
content-addressed pages, freezes the installed domains, creates the migration
checkpoint, advances the writer/superblock/gate feature binding, reopens, and
unfreezes. It is forward-only, metadata-only for blobs, restartable at every
durable boundary, and safe for two through eight concurrent migrators.

Stress testing found a predecessor-GC race: a winning replica removed terminal
marks while a lagging replica still held a sweeping snapshot, allowing the
lagging replica to mistake live objects for garbage. Schema 008 retains the
terminal migration GC session and marks as excluded compatibility residue.
They are not checkpoint authority and cannot be collected until no supported
predecessor can still sweep. A deterministic regression reproduces the exact
interleaving, and repeated eight-replica schema-001-to-008 runs converge without
data loss.

Immutable raw fixtures cover portable-minimal, application-preview-disabled,
and application-preview-GCS profiles for schema 008. They were written by
commit `359ec9fbc9e8020257659c0d91e64372baece1b9`; their digests are pinned in
the migration registry and fixture README. The application profiles open
through the real startup writer builder, not a feature-minimal substitute.

## 9. Measured provider-budget comparison

Costs are marginal GCS request charges from the checked-in regional Standard
flat-namespace fixture. They are fractions of a US dollar and exclude storage,
retrieval, egress, free tiers, taxes, and negotiated discounts.

| Operation | Before requests | Schema 008 requests | Before cost | Schema 008 cost | Before p95 | Schema 008 p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| State list empty | 2 | 1 | $0.0000008 | $0.0000004 | 130 ms | 65 ms |
| State create | 12 | 6 | $0.0000274 | $0.0000208 | 850 ms | 450 ms |
| State read | 4 | 1 | $0.0000016 | $0.0000004 | 260 ms | 65 ms |
| State compare-and-swap | 18 | 2 | $0.0000298 | $0.0000054 | 1.240 s | 145 ms |
| State delete | 14 | 2 | $0.0000190 | $0.0000054 | 950 ms | 145 ms |
| Create upload | 12 | 8 | $0.0000320 | $0.0000308 | 905 ms | 650 ms |
| Upload status | 3 | 3 | $0.0000008 | $0.0000008 | 190.011 ms | 190.019 ms |
| Complete upload | 68 | 6 | $0.0001448 | $0.0000108 | 5.136 s | 410.065 ms |
| Create download | 6 | 4 | $0.0000020 | $0.0000012 | 320 ms | 190 ms |
| Abort upload | 10 | 5 | $0.0000166 | $0.0000058 | 680 ms | 330.232 ms |
| Create directory | 76 | 4 | $0.0001542 | $0.0000108 | 5.831 s | 290 ms |
| Copy one file | 47 | 4 | $0.0000782 | $0.0000108 | 3.246 s | 290 ms |
| Move one file | 72 | 4 | $0.0001388 | $0.0000108 | 5.037 s | 290 ms |
| Trash one file | 100 | 5 | $0.0001952 | $0.0000158 | 6.997 s | 370 ms |
| Restore one file | 103 | 4 | $0.0001688 | $0.0000062 | 7.102 s | 275 ms |
| Permanently delete one file | 40 | 4 | $0.0000570 | $0.0000062 | 2.731 s | 275 ms |

These exact observed values are the second append-only GCS ratchet epoch. The
budget schema now also records modeled p50/p95/p99 critical-path ceilings, so a
future change cannot hide a slower serial or parallel phase behind an unchanged
request count.

## 10. Proof surface

The replacement tests cover:

- canonical head/page/key validation and fuzzed corruption denial;
- state routing, point operations, pagination, immutable snapshots, and
  multi-domain list merge;
- namespace create/list/stat/copy/move/Trash/restore/delete and aggregates;
- no publication before head commit for every namespace mutation;
- idempotent replay, changed-intent denial, lost success, stale CAS, and
  eight-replica convergence;
- delta compaction, outcome retention/expiry, structural sharing, bounded
  pages, and bounded concurrent writes;
- 10,000-item atomic batch behavior and exact provider economics;
- upload create/status/complete/abort, direct-final blob publication, terminal
  lease removal, replay, and every completion/abort provider-failure boundary;
- duplicate grouping, overlap, ignore, preview, validation, and apply;
- catalog registration/freeze races, checkpoint reachability, raw-copy
  verification, corruption, and restart reconciliation;
- every historical epoch/profile fixture, every remaining migration suffix,
  split and single backends, interruption boundaries, and multi-replica
  migration; and
- runtime isolation from retired schema-007 authority and all forbidden file
  body/provider operations.

Coverage percentage remains a backstop rather than a substitute for these
explicit guarantees. The release boundary must not be appended until the full
Nix gate, including migration, race, portability, provider-budget, source lint,
and coverage checks, passes on the final tree.
