# Storage schema 008 implementation record

**Status:** implementation in progress; not yet a writable storage epoch

**Date:** 2026-08-24

**Depends on:** [`storage-architecture-v2-proposal.md`](./storage-architecture-v2-proposal.md) and [`storage-architecture-comprehensive-economics-2026-08-24.md`](./storage-architecture-comprehensive-economics-2026-08-24.md)

**Will inherit:** [`v1-specification.md`](./v1-specification.md)

## 1. Purpose and activation rule

This record translates the selected consistency-domain and persistent-page
architecture into production canonical types and executable portable-engine
behavior. It is deliberately not a ledger entry yet. The current writer remains
schema 007 until all of the following land together:

- every authoritative schema-008 key and body is specified and implemented;
- application state and namespace operations use the new authority rather than
  shadowing or dual-writing it;
- the adjacent `007 -> 008` transformation is deterministic, resumable,
  metadata-only for file blobs, and proven from every predecessor profile;
- immutable schema-008 fixtures are produced by the exact writer commit and
  pinned to their digests;
- checkpoint, raw-copy portability, multi-replica, corruption, interrupted
  migration, and provider-economics gates pass; and
- rollback and release validity are documented.

Adding unused schema-008 readers and writers before that cutover is safe.
Advertising schema 008 in the superblock, changing the current ledger epoch, or
serving mixed schema-007/schema-008 authority before those requirements are met
is forbidden.

## 2. Selected authority model

Schema 008 replaces generic per-record and per-directory publication with the
smallest conditional head that contains each actual invariant:

| Authority | Partition | Foreground responsibility |
| --- | --- | --- |
| Namespace | owner | live/Trash graph edges, stable directory identities, exact file references, and recursive aggregates |
| Owner control | owner | profile, account security generation, credentials, preferences, and owner-scoped mutation state |
| Administration | global | bootstrap and administrator-set invariants |
| Capability | exact bounded capability collection | invites, recoveries, and other bearer-authority state |
| Share | owner plus exact public capability | private share lifecycle and public capability resolution |

Sessions and ceremonies that have no collection-wide invariant use independent
conditional records. Duplicates, administration pages, accounting, and search
are rebuildable projections and never authorize a mutation. A cross-domain
invariant uses prepare/decision/finalize only for the domains it actually
touches; unrelated mutations do not pay a global admission cost.

The initial production slice implements the shared conditional-head,
content-addressed-page, durable-claim, freeze, and compaction substrate. It does
not yet activate any of the partitions in the table.

## 3. Canonical key grammar

All keys remain below the provider-independent 240-byte bound and use the
existing `endlessfs/v1` root while the schema epoch is carried by the
superblock/ledger:

```text
endlessfs/v1/domains/catalog/head.json
endlessfs/v1/domains/catalog/pages/{digest-of-page-id}.json
endlessfs/v1/domains/{kind}/{digest-of-domain-id}/head.json
endlessfs/v1/domains/{kind}/{digest-of-domain-id}/pages/{digest-of-page-digest}.json
endlessfs/v1/domains/{kind}/{digest-of-domain-id}/claims/{digest-of-mutation-id}.json
endlessfs/v1/projections/{digest-of-owner-id}/{kind}/head.json
endlessfs/v1/projections/{digest-of-owner-id}/{kind}/pages/{digest-of-page-digest}.json
```

The full domain ID, mutation ID, owner ID, kind, and page context are bound in
the canonical body or the trusted lookup context. Hashing a key component is
only a bounded key encoding; a digest collision or misplaced object fails the
body/key binding check.

Provider generations, ETags, version IDs, bucket/container identifiers,
provider timestamps, upload IDs, and provider checksum encodings are absent
from every canonical record. A native object version is retained only in the
request-local snapshot that immediately supplies a conditional `PutMatch`.

## 4. Canonical domain records

### 4.1 Conditional head

A domain head contains:

- schema version, full domain ID, and closed domain kind;
- the logical domain revision;
- a separately authenticated compacted-base revision;
- an optional frozen epoch whose presence exactly matches the frozen flag;
- a bounded descriptor for the immutable base-tree root; and
- an ordered contiguous window of committed deltas.

Each delta binds one mutation ID, a SHA-256 fingerprint derived internally from
the complete normalized change set and result, its exact revision, ordered
changes, and its committed result. Revisions are contiguous from
`baseRevision + 1` through the head revision. Omitting, reordering, duplicating,
or splicing a delta therefore fails validation.

The head is the only visibility point for its domain. Its portable envelope
revision and logical version are canonical; its native version is not persisted.

### 4.2 Immutable pages

A page binds its schema version, complete domain ID, closed domain kind, level,
and either ordered leaf entries or ordered child descriptors. Leaf entries
embed their value and logical version so a point lookup does not need a second
value-object fetch. Child descriptors bind the exact key range, digest, level,
entry count, and byte aggregate of the child.

The SHA-256 digest of the exact canonical page body is its content identity.
Every read recomputes that digest, validates the body/key/domain binding, and
validates ordering, level, count, and canonical-size invariants before exposing
data.

The architecture sensitivity run selected a maximum fan-out of 256. At 4,096
entries it had the lowest request count, provider cost, and modeled critical
latency among the measured non-dominated choices; fan-out 1,024 had the same
request shape with materially more bytes and was dominated. Exact encoded size
is an independent bound: a page is split earlier when its canonical body would
exceed the existing one-mebibyte canonical-record limit. Thus a maximum-sized
application value cannot cause an oversized page and ordinary small values
still obtain high fan-out.

### 4.3 Durable claims

Before publishing a mutation, the writer creates a claim at the mutation's
stable claim key. The claim binds the full domain ID, mutation ID, internally
derived intent fingerprint, and prepared state. After the head CAS succeeds,
the same claim is conditionally finalized with the committed revision and
result.

A retry with the same mutation ID:

- returns the committed result when the claim is finalized;
- reconciles a prepared claim when a matching delta is still present;
- may retry an unpublished prepared intent against the current head; and
- rejects a different intent fingerprint or any corrupt/misplaced claim.

Compaction MUST verify or finalize every claim represented by the delta window
before retiring that window. A response lost before or after claim
finalization therefore cannot make a committed mutation ambiguous.

The retention/expiry representation for finalized claims remains part of the
pending operation-lifecycle slice. No claim may be collected until its replay
contract has ended through canonical policy; wall-clock expiry alone is not an
unlock or proof of non-commit.

## 5. Mutation protocol and measured foreground work

An ordinary cold mutation currently executes:

1. one head `GET`, retaining its native conditional token only in memory;
2. one create-only durable claim `PUT`;
3. validation against the head's inline deltas and only the required immutable
   base-page path;
4. one create-only or match-conditional head `PUT`; and
5. one match-conditional claim-finalization `PUT`.

For a new or delta-resident value, the executable production test observes four
state-provider requests for mutation and one head `GET` for read. These are
measurements, not an arbitrary acceptance ceiling. Further work may remove a
call only if it preserves durable idempotency and lost-success recovery.

Two through eight replicas use the same head CAS. Exactly one stale-version
writer can publish; another receives a portable conflict/precondition result.
A freeze CAS totally orders with publication: a writer paused before head
commit cannot publish through a newly frozen head.

## 6. Compaction and structural sharing

Compaction snapshots one head, reconciles its claims, coalesces its delta
window, and applies only the changed keys through a request-local page cache.
Unchanged subtrees retain their content identities. Changed leaves and their
ancestor path are written as immutable create-only pages, followed by one head
CAS that advances `baseRevision` to the existing logical revision and clears
the folded deltas.

There is no provider `LIST`, object deletion, file-provider request, or file
body read in this path. A head race leaves only unreachable immutable metadata;
the old head remains authoritative. A later mark-and-sweep pass may collect
that metadata only after proving it is unreachable from all heads,
checkpoints, migrations, retained claims/outcomes, and projections.

Page construction is bounded by both exact canonical bytes and the
sensitivity-selected fan-out. Point reads and rewrites follow one tree path,
so their request slope is a function of tree height, not total domain width.

## 7. File-body and provider-cost invariant

This substrate accepts only the state `objectstore.Backend`. It has no file
backend, `Open`, transfer, or object-body streaming dependency. Namespace
cutover will continue referencing existing immutable blob keys in place.
Schema migration, move, rename, Trash, restore, directory copy-by-reference,
duplicate reconciliation, checkpoint, and metadata compaction MUST issue zero
file-data requests and MUST NOT relocate file bytes.

## 8. Remaining implementation sequence

Before schema 008 can become current, this PR still needs to:

1. finish catalog, multi-domain decision, batch-fragment, projection watermark,
   authenticated read-proof/cursor, outcome retention, and garbage roots;
2. replace the generic state repositories with direct records or atomic owner,
   admin, capability, and share domains according to their real invariants;
3. replace per-directory namespace roots and synchronous duplicate projections
   with one owner namespace graph and derived views;
4. integrate full-closure checkpoint freeze and raw-copy verification;
5. add the adjacent resumable `007 -> 008` transformer without reading or
   copying file bodies;
6. generate immutable fixtures from the exact schema-008 writer commit; and
7. ratchet every production route's measured count, cost, bytes, serial depth,
   and modeled latency to the observed implementation.

Until those items are complete, this document records an implemented
foundation and its invariants, not a releasable or deployable schema claim.
