# EndlessFS storage architecture experiment

**Status:** executable architecture-selection evidence; production cutover is
not enabled by this document

## Result

The experiments reject the current per-record transaction architecture and
select a composed family for production hardening:

1. a small conditional head per real consistency domain;
2. bounded inline namespace deltas for foreground publication;
3. immutable, content-addressed, high-fan-out pages as the compacted base;
4. durable idempotency claims bound to the mutation fingerprint;
5. a separately frozen domain catalog plus per-domain head freezing for
   checkpoints; and
6. derived sort, duplicate, similarity, accounting, search, and audit views
   outside the authoritative foreground transaction.

This is a family selection, not permission to copy the prototype constants
into a released schema. Page fan-out, delta bytes, compaction watermarks,
idempotency retention, and garbage grace periods still require sensitivity
results and must be derived from provider limits, workload measurements, or an
explicit product policy before the storage format becomes normative.

## Reproducible implementation

The experimental implementation is in `internal/architecturelab`. It contains:

- one shared namespace semantic model;
- eight executable candidates;
- a content-addressed immutable B-tree with bounded pages and batched edits;
- exact provider request, byte, cost, latency, subsystem, and critical-path
  instrumentation;
- deterministic crash-before-commit, lost-success, corruption, freeze, and
  two-replica CAS schedules;
- wide-directory and descendant-invariance scale fixtures;
- a domain-catalog checkpoint experiment; and
- Pareto comparison without a weighted score or guessed passing threshold.

Run the focused evidence with:

```text
nix develop -c go test -v ./internal/architecturelab
```

## Current fact base

The current successful single-item paths, measured by the provider-economics
gate, are:

| Current path | State requests | Modeled GCS request cost | Modeled serial p95 |
| --- | ---: | ---: | ---: |
| Move | 72 | $0.0001388 | 5.037 s |
| Trash | 100 | $0.0001952 | 6.997 s |
| Restore | 103 | $0.0001688 | 7.102 s |
| Complete upload | 66 | $0.0001448 | 5.016 s |
| Create directory | 76 | $0.0001542 | 5.831 s |

The target classifier added with the experiment attributes every state request
to its physical subsystem. It distinguishes the write gate, admission,
operation state, idempotency, directory root, manifest, name index, secondary
sort index, content index, duplicate occurrence, duplicate summary, duplicate
similarity, state index, state value, upload state, checkpoint, migration, and
file-blob namespaces. This makes it possible to remove a subsystem and observe
the actual economic change instead of inferring it from code.

The dominant defects are representational:

- every small mutation pays a bucket-global admission protocol created for the
  rare checkpoint path;
- atomicity across many mutable roots requires pending/final/rollback operation
  machinery;
- duplicate and secondary directory projections are updated synchronously;
- idempotency and operation polling are separate transactions around the real
  namespace mutation; and
- the authoritative shape makes recovery manipulate many provider objects
  instead of selecting one already-prepared immutable result.

## Candidate measurements

The table below is the measured foreground vector for renaming a directory.
Costs use the checked-in GCS regional Standard flat-namespace fixture. Stored
objects and bytes include the seeded candidate history, so they expose garbage
and compaction amplification as well as the foreground event.

### Directory containing one file

| Candidate | Requests | Cost (USD) | Modeled p95 | Request bytes | Response bytes | Stored objects | Stored bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Packed snapshot | 2 | $0.0000054 | 145 ms | 1,098 | 924 | 1 | 1,098 |
| Immutable journal | 6 | $0.0000116 | 420 ms | 671 | 1,291 | 5 | 1,719 |
| Flat bounded delta | 3 | $0.0000058 | 210 ms | 1,008 | 1,002 | 2 | 1,279 |
| Whole-directory graph | 4 | $0.0000108 | 290 ms | 1,157 | 1,003 | 8 | 2,361 |
| Separate-directory paged graph | 11 | $0.0000228 | 775 ms | 1,506 | 2,092 | 13 | 3,535 |
| Embedded paged namespace | 4 | $0.0000108 | 290 ms | 1,445 | 1,306 | 6 | 2,408 |
| Claimed paged namespace | 6 | $0.0000208 | 450 ms | 1,672 | 1,160 | 9 | 3,013 |
| Paged-delta hybrid | 5 | $0.0000158 | 370 ms | 4,217 | 2,896 | 5 | 4,626 |

### Directory containing 32 files

| Candidate | Requests | Cost (USD) | Modeled p95 | Request bytes | Response bytes | Stored objects | Stored bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Packed snapshot | 2 | $0.0000054 | 145 ms | 12,754 | 12,579 | 1 | 12,754 |
| Immutable journal | 37 | $0.0000240 | 2.435 s | 676 | 16,417 | 36 | 16,847 |
| Flat bounded delta | 3 | $0.0000058 | 210 ms | 747 | 12,672 | 4 | 19,408 |
| Whole-directory graph | 4 | $0.0000108 | 290 ms | 6,275 | 6,120 | 70 | 121,885 |
| Separate-directory paged graph | 11 | $0.0000228 | 775 ms | 2,544 | 3,135 | 161 | 192,227 |
| Embedded paged namespace | 4 | $0.0000108 | 290 ms | 2,482 | 2,342 | 71 | 133,314 |
| Claimed paged namespace | 6 | $0.0000208 | 450 ms | 1,683 | 1,170 | 102 | 133,545 |
| Paged-delta hybrid | 5 | $0.0000158 | 370 ms | 3,430 | 2,418 | 38 | 20,091 |

These numbers are evidence, not proposed limits. The candidate code retains
the raw request events, including failed conditional creates, exact targets,
bytes, and logical subsystem labels.

## Why the superficially cheapest candidates lose

The packed snapshot is optimal only while the complete namespace is a small
object. Its read and write bytes, memory, contention, and object-size risk grow
with the entire consistency domain.

The immutable journal has a small write but its cold read and recovery cost
grow with uncompacted history. Compaction rewrites a complete snapshot.

The original bounded-delta candidate hides the same complete-snapshot read and
compaction cost behind a bounded head. It is not a scalable base.

The whole-directory graph makes a subtree move cheap, but reading and rewriting
one directory grows with immediate directory width. It also allowed outcomes
to grow without a bound in the conditional head.

The separate-directory paged graph bounds width but adds an avoidable object
read and write for every directory whose embedded index root could instead be
carried by its parent entry or the domain head.

The embedded paged namespace has the right asymptotic base, but every foreground
mutation rewrites the affected page paths. The claimed variant proves strong
idempotency but makes those page writes plus claim lifecycle visible on the
critical path.

The paged-delta hybrid keeps the embedded persistent pages and publishes a
bounded low-level edge/aggregate delta in the conditional head. Foreground
mutation does not rewrite base pages. Compaction merges only pages named by the
bounded dirty-directory set. A 4,096-entry wide-directory fixture proves that a
foreground rename publishes no page PUT; the cold path performs seven provider
requests in the current prototype, including a three-page base lookup and two
claim writes. A subtree move reads no descendant page and issues no file-store
request.

## Selected consistency domains

The ordinary namespace consistency domain is one owner namespace, containing
its live tree, trash tree, authoritative trash metadata, upload-publication
outcomes, and any other metadata that must commit atomically with namespace
visibility. A move between live and trash is therefore one head update. It does
not create a second transaction for a trash repository record.

Security state is partitioned by actual invariant rather than forced through a
generic key transaction:

- account, preferences, and owner-scoped policy use an owner control domain;
- the administrator set and final-admin invariant use one administration
  domain;
- a one-time invite/recovery capability and its consumption outcome share one
  capability domain;
- sessions are independently conditional bounded records and do not contend on
  the namespace head; and
- share state uses a share domain whose revocation authority is read before
  access and whose namespace target is a portable logical version.

Cross-domain protocols are allowed only for real cross-domain invariants. They
are not the default implementation of a one-record update.

## Authoritative namespace record

The selected head contains only bounded authority:

- schema and writer protocol identity;
- portable logical revision and freeze epoch;
- live and trash compacted base roots plus recursive aggregates;
- a bounded ordered delta window;
- the most recent committed outcome needed for lost-success recovery; and
- integrity closure over every referenced delta and page.

A delta contains low-level changes keyed by stable directory identity:

- child-name insertion, replacement, or tombstone;
- the resulting directory byte/file aggregates;
- parent identity and edge name needed for bottom-up compaction; and
- the mutation fingerprint and committed outcome.

Moving a directory changes its source and destination edges and ancestor
aggregates. The directory's immutable child pages and every file blob remain
unchanged. The work is a function of path depth, not descendant count.

The compacted base is a structurally shared immutable ordered tree. A page is
addressed by a digest of its canonical body. Corrupt content, bad ordering,
invalid child ranges, bad counts, oversized pages, unknown fields, and trailing
data fail closed. The prototype proves split, update, removal, sorted walk,
batch edit, content-address corruption denial, and a 4,096-entry multi-level
tree.

### Page fan-out sensitivity

A 4,096-entry point update produces the following current prototype tradeoff:

| Fan-out | Root level | Requests | Cost (USD) | Modeled p95 | Request bytes | Response bytes |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 16 | 3 | 8 | $0.0000216 | 580 ms | 7,621 | 7,624 |
| 64 | 2 | 6 | $0.0000162 | 435 ms | 16,868 | 16,871 |
| 256 | 1 | 4 | $0.0000108 | 291 ms | 41,593 | 41,596 |
| 1,024 | 1 | 4 | $0.0000108 | 293 ms | 163,209 | 163,212 |

Fan-out 1,024 is dominated by 256 in this workload. Fan-outs 16, 64, and 256
remain non-dominated because request cost trades against bytes. This is why the
released page bound cannot be guessed from call count alone.

## Mutation protocol

For a cold ordinary mutation, the prototype performs the following logical
work:

1. read the owner head and retain its request-local native conditional token;
2. create or validate a durable idempotency claim bound to the complete intent;
3. resolve only the required base-page paths not already answered by the delta
   window or an authenticated listing/stat proof;
4. validate authorization, expected logical versions, conflicts, and
   aggregates;
5. encode one bounded delta and conditionally replace the head; and
6. finalize the claim, or reconcile it from the committed head after a lost
   response.

The head CAS is the only namespace visibility point. Immutable preparation and
an unfinalized claim are unreachable or recoverable garbage, never partial
namespace state. Two-replica schedules prove one winner; the loser reloads and
rebases. A lost successful head response is recovered through the prepared
claim and latest head outcome.

The implementation must investigate moving claim finalization off the UI
critical path while retaining a bounded recent-outcome window. The total
provider cost must still be reported; moving a PUT to background work is a
latency optimization, not a cost reduction.

## Checkpoint protocol without ordinary admission tax

The experiment replaces the bucket-global candidate/admitted/delete sequence:

1. New consistency domains are registered through one conditional domain
   catalog.
2. A checkpoint conditionally freezes that catalog, totally ordering itself
   against domain creation.
3. It enumerates the frozen catalog with strong listing semantics.
4. It conditionally freezes each domain head. A racing mutation and freeze use
   the same head CAS, so exactly one wins; freeze retries a mutation winner, and
   a mutation loser observes the frozen head.
5. The checkpoint walks the now-immutable reachable closure with bounded,
   restartable work.

The 128-domain fixture proves an ordinary mutation performs zero catalog
requests. Checkpoint work scales with domains because checkpoint is the operation
that needs global quiescence. No global ticket, second gate read, admitted-state
write, or foreground ticket deletion is required.

## Batches and authenticated read proofs

A directory subtree move is already one edge mutation regardless of the number
of descendants. A selection of independent files necessarily carries metadata
proportional to the selection, but it does not require one complete transaction
protocol per file.

The production API should accept an authenticated listing/stat proof containing
the domain logical revision, directory identity, entry versions, and aggregate
inputs. The server reads the current head once, validates the proof against that
head, encodes all edge changes into one bounded batch delta or a bounded set of
immutable delta fragments, and performs one head CAS. If the head changed, the
proof is stale and the batch is re-resolved; it never publishes against mixed
snapshots.

Without a proof, preparation reads distinct base pages, not one provider object
per selected file. Page reads and immutable fragment writes can be performed in
bounded parallel groups. Publication remains one conditional head update.

Batch byte and fragment bounds must be determined from provider object limits,
tail-latency sensitivity, and the 10,000-item workload fixture. They are not set
in this experiment.

## Derived views

Name lookup and recursive aggregates are authoritative because authorization,
path resolution, and destructive changes require them. The following are
derived projections:

- modified, size, and kind ordering;
- duplicate hash groups and directory overlap/similarity;
- storage accounting summaries;
- search;
- presentation audit timelines; and
- checkpoint work inventories.

The domain head supplies the old compacted root/delta watermark and the new
watermark. A worker diffs structurally shared roots plus the bounded delta
window, writes a new immutable projection, and conditionally advances its
watermark. It never reads file bodies. A destructive action selected from a
derived view must revalidate the authoritative entry version in its namespace
transaction.

The UI and API must expose projection watermarks or lag where freshness matters.
Corrupt or missing projections are rebuilt from authoritative roots; they do
not block unrelated namespace publication.

## Upload and file objects

Direct upload continues to target the final immutable owner-scoped blob. Upload
completion obtains provider-attested size, MD5, and CRC32C through metadata and
commits the blob identity, namespace edge, upload outcome, and idempotency result
in the owner domain. It does not copy the object, stream the object through Go,
or synchronously update duplicate projections.

Move, rename, trash, restore, same-owner logical copy, duplicate reconciliation,
checkpoint, and metadata migration issue zero file-provider requests. Physical
blob deletion is delayed reachability collection after every head, checkpoint,
derived watermark, and retention root no longer references the blob.

## Compaction and garbage

Compaction snapshots one conditional head, merges its bounded deltas into only
the affected persistent-tree paths, and conditionally publishes a new head with
the same logical namespace and an empty or shorter delta prefix. A head race
discards the unpublished pages as garbage and retries. Crash-before-commit and
restart are tested; visibility remains on the old head until the compaction CAS.

For 256 sequential creates, changing only the prototype delta window produced:

| Window | Total requests | Total cost (USD) | Serial modeled p95 | Request bytes | Response bytes | Final head bytes |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 2,884 | $0.0079386 | 209.7 s | 3,208,417 | 5,565,239 | 1,415 |
| 8 | 1,640 | $0.0046074 | 119.5 s | 1,572,943 | 3,810,117 | 6,952 |
| 32 | 1,482 | $0.0042360 | 108.3 s | 3,745,988 | 5,592,473 | 25,970 |
| 128 | 1,414 | $0.0041168 | 103.8 s | 13,393,006 | 14,906,305 | 102,132 |

Every window is non-dominated in that vector: a larger window reduces request
and compaction cost while increasing head traffic and recovery/CPU work. The
prototype default is therefore an experimental point, not a proposed storage
contract.

Garbage collection marks from:

- current domain heads;
- frozen checkpoints;
- retained idempotency/outcome claims;
- active upload and long-running job roots;
- derived-view watermarks still needed for incremental rebuild; and
- migration/cutover roots.

Only unmarked metadata older than the derived safety horizon is deleted.
Unreferenced file blobs additionally obey trash and product retention. Collection
is restartable and fenced; it is never on an ordinary mutation path.

## Migration and rollback

The replacement is a new append-only epoch. Migration must:

1. close and drain the existing v1 gate using the released protocol;
2. build disjoint v2 domain heads and persistent pages from authoritative v1
   metadata without reading or copying any file body;
3. write resumable authenticated progress by bounded page/range;
4. verify every root, aggregate, logical version, blob reference, identity,
   share, trash record, upload, and derived-view rebuild seed;
5. freeze the v2 catalog and checkpoint its complete closure;
6. conditionally cut the superblock authority to v2; and
7. reopen v2 domains under a new epoch.

Rollback before the first v2 mutation restores the frozen v1 authority. After a
v2 mutation, rollback requires the explicit adjacent reverse/cutback policy or a
forward fix; silently reopening stale v1 roots is forbidden. Existing immutable
blobs are referenced in place throughout.

The current migration implementation cannot be reused unchanged: one journal
object per work item, serial provider calls, and body reads for metadata are
specifically rejected.

## Remaining evidence before a normative epoch

The following are deliberately unresolved rather than guessed:

- page fan-out and canonical page byte bound;
- delta count/byte window and compaction low/high watermarks;
- idempotency and operation-result retention;
- batch fragment size and maximum bounded parallelism;
- domain partition sensitivity under hot-user and unrelated-user load;
- exact read-proof grammar and key rotation;
- derived-view freshness policy;
- garbage safety horizon;
- online shadow versus offline production cutover; and
- complete economics for state, share, preview, migration, checkpoint, repair,
  and garbage paths under the selected family.

No release ledger entry or production writer should be added until those values
are supported by the remaining sensitivity and migration evidence. Once the
selected implementation is production-complete, its observed request, cost,
latency, byte, contention, and maintenance vectors become the first ratchet.
They remain optimization baselines, not a declaration that further improvement
is unnecessary.
