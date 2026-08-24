# EndlessFS storage architecture v2 replacement proposal

**Status:** proposal; not yet normative  
**Scope:** complete replacement of the authoritative state and namespace
representation while preserving existing product, security, portability,
concurrency, and recovery guarantees

## Decision summary

EndlessFS should replace the current object-per-concern storage model with a
packed, root-committed architecture. The current format turns one logical
mutation into synchronous writes and rereads of directory manifests, name and
sort indexes, duplicate indexes, operation records, idempotency records,
admission tickets, and pending/final roots. It is safe but economically and
operationally inefficient.

The replacement has four defining properties:

1. Each independently consistent shard has one bounded canonical commit root.
   The root contains its write epoch and open/frozen state, so one conditional
   root update is both the logical linearization point and the stale-writer
   fence.
2. Common mutations are encoded as bounded deltas and published by one root
   compare-and-swap. Large mutations add bounded immutable transaction pages
   before the same single root commit. Subtree work is proportional to path
   depth or bounded transaction payload, never descendant count.
3. Secondary sort, duplicate, search, audit, and checkpoint indexes are derived
   from committed transactions. They are rebuildable and do not participate in
   the foreground commit.
4. Crash recovery determines success from the commit root. Unreferenced
   immutable preparation is garbage. Small mutations do not need separate
   `running`, `committed`, and `succeeded` provider writes.

The target for authoritative publication is two state-provider requests for a
normal warm mutation—read the current root and conditionally replace it—and at
most three when an immutable transaction object is required. A cold request may
also read bounded immutable pages while resolving paths; those reads are
measured separately and scale with path depth, never subtree descendants.
Normal UI mutations should carry authenticated snapshot proofs from the
listing/stat response they already consumed so unchanged-root mutations avoid
repeating that resolution. Direct file bytes remain outside the Go service and
are never moved for namespace operations.

This proposal deliberately changes internal contracts and mechanisms. It does
not relax the externally observable or security properties they were intended
to provide.

## Why replacement is necessary

The provider-economics gate measured these successful single-item paths:

| Path | State requests | Modeled request cost | Modeled p95 serial latency |
| --- | ---: | ---: | ---: |
| Move | 72 | $0.0001388 | 5.037 s |
| Trash | 100 | $0.0001952 | 6.997 s |
| Restore | 103 | $0.0001688 | 7.102 s |
| Complete upload | 66 | $0.0001448 | 5.016 s |
| Create directory | 76 | $0.0001542 | 5.831 s |

A one-file move currently performs 47 state reads, 24 state writes, and one
state delete. Fifty-seven of those requests maintain directory and duplicate
indexes; only fifteen belong to the write gate, admission, operation, and
idempotency machinery. A trash/restore loop performs 203 state requests: 135
reads, 62 writes, and six deletes.

The problem is structural:

- the object store is being used as a very fine-grained transactional database;
- each derived view is synchronously materialized as independently versioned
  provider objects;
- safe composition invokes the complete admission protocol multiple times for
  one user action;
- operation recovery persists intermediate mechanism state rather than one
  compact intent and one commit decision;
- rare checkpoint and migration requirements impose a provider-call tax on
  every normal mutation; and
- immutable history and derived objects amplify storage, checkpoint, listing,
  and garbage-collection work.

Tuning, parallelism, or larger pages cannot remove this amplification. The
authoritative model and its commit protocol must change.

## Goals

The replacement MUST:

- preserve every user-facing file, directory, trash, restore, upload,
  download, share, duplicate-management, identity, and administration feature
  unless a removal is separately proposed and approved;
- keep ordinary namespace request count independent of subtree descendants;
- reduce successful move, trash, restore, delete, copy, and directory creation
  publication to at most three foreground state-provider requests in the
  common unchanged-root case, with cold path-resolution reads explicitly
  bounded and budgeted;
- reduce upload completion to at most four state requests plus bounded
  provider metadata/status verification and no file copy;
- preserve linearizable conflicting mutations and deterministic idempotency;
- preserve safe operation across two through eight interchangeable replicas;
- preserve crash recovery, lost-success reconciliation, one-winner takeover
  for genuinely asynchronous work, and stale-worker denial;
- preserve provider-independent logical versions and raw key/body portability;
- preserve fail-closed corruption, mixed-format, incompatible-writer, and
  unsupported-provider behavior;
- keep provider-native generations request-local and absent from canonical
  bodies;
- keep file bodies on the browser/provider data plane;
- keep foreground memory and record sizes bounded independently of total file
  count and subtree size;
- bound retained history and make unreachable-data collection explicit; and
- make request count, modeled cost, latency, storage amplification, and
  migration traffic mandatory acceptance criteria.

## Non-goals

The replacement does not require:

- SQL, Redis, an external queue, a persistent application filesystem, a
  distinguished leader, or sticky routing;
- moving or downloading existing immutable file bytes;
- a provider-specific authoritative format;
- cross-user physical deduplication;
- weakening direct upload/download capability boundaries;
- online multi-provider replication; or
- retaining the current gate, admission-ticket, manifest, staging, or operation
  record shapes merely because they are already implemented.

## Design principles

### Guarantees are permanent; mechanisms are replaceable

Specifications should state properties such as atomic visibility, one-winner
concurrency, stale-writer denial, recoverability, bounded work, portability,
and provider economics. They should not make one particular ticket, lease,
page, or state-machine layout permanent when a simpler protocol proves the
same properties.

Every replaced mechanism requires a guarantee matrix showing how the new
protocol proves each old guarantee. Passing that matrix permits deletion of
mechanism-specific code and tests after migration coverage exists.

### One foreground commit point

A user action should have one authoritative conditional commit. All state that
must change atomically belongs to the same consistency domain and transaction.
Trash metadata, idempotency, aggregate deltas, and the operation outcome must
not be implemented as additional generic StateStore transactions surrounding a
namespace transaction.

### Primary state is minimal

Only data required to authorize, resolve, and validate current behavior is
authoritative. Secondary orderings, duplicate groups, overlap candidates,
storage maps, audit projections, and checkpoint inventories are derived views.
They may lag behind a committed revision, must publish their watermark, and
must revalidate authoritative versions before causing a destructive action.

### Optimize the common successful path

Crash recovery may do additional bounded work after an actual crash. Every
successful foreground mutation must not prepay every recovery branch. A
successful conditional commit is already a durable decision; separate terminal
state writes are unnecessary when the commit root contains the outcome.

### Rare maintenance must not tax every mutation

Checkpoint creation and migration are rare. Their safety protocol should use
shard freezing, reachability, and incremental canonical inventories rather
than a global candidate/admission lifecycle around every ordinary request.

## Proposed canonical model

The exact key grammar will be finalized by the normative format specification.
The conceptual v2 storage set is:

```text
[state] endlessfs/v2/
  superblock.json
  control/shard-registry-root.json
  shards/<shard-id>/root.json
  shards/<shard-id>/bases/<base-id>/...
  shards/<shard-id>/transactions/<transaction-id>.json
  shards/<shard-id>/transaction-pages/<transaction-id>/<page>.json
  derived/<index-kind>/<shard-id>/root.json
  derived/<index-kind>/<shard-id>/pages/<page-id>.json
  checkpoints/<checkpoint-id>/root.json
  checkpoints/<checkpoint-id>/pages/<page-id>.json
  maintenance/<bounded-job-id>/...
[file]
  existing immutable blob keys
  endlessfs/v2/blobs/<encoded-user-id>/<blob-id>
```

Existing immutable v1 blobs remain valid canonical blob locations. Migration
must reference them in place; it must not copy, rename, download, or re-upload
their bodies. New v2 blobs use the v2 namespace. A versioned portable blob
locator distinguishes the canonical key formats without exposing them through
public APIs.

### Consistency domains and shards

A shard is the smallest independently committed consistency domain. Initial
shards are:

- one namespace shard per user containing live and trash trees, shares,
  namespace-scoped idempotency, and bounded operation outcomes;
- one security shard per user containing credentials, sessions, recovery
  state, and account status whose invariants must be atomic together;
- one global administration shard containing the enabled-administrator
  invariant, registration policy, invites, and bootstrap state; and
- small fixed system shards for themes and storage-set configuration.

Shard boundaries are chosen around invariants, not record types. State that
must change atomically must share a root. This avoids introducing a general
cross-shard transaction protocol into ordinary paths.

Public lookup objects such as hashed share or session locators may be immutable
and written before the shard commit. A resolver treats them only as hints and
validates active authority in the referenced shard root. Before the root
commit they grant nothing; after revocation they grant nothing; abandoned hints
are garbage.

### Bounded shard root

Each `root.json` is a bounded canonical envelope containing at least:

```text
schema version
storage-set and shard identity
logical revision and logical version
writer protocol and required feature set
write epoch and open | frozen mode
base snapshot reference and digest
bounded committed delta window
derived-index watermarks
aggregate byte/file counts where applicable
retention and compaction watermarks
last committed transaction identity and digest
```

The native provider version returned with a root read is used only as the
immediate conditional replacement precondition and is discarded afterward.
The canonical logical revision and transaction digest survive raw-copy
portability.

### Namespace base and bounded delta window

The compacted namespace is a persistent high-fan-out search tree. Pages are
immutable, content-digested, independently bounded, and structurally share
unchanged data. Directory IDs remain stable. A base snapshot contains the
live/trash roots and exact recursive aggregates.

The shard root also contains a bounded window of recent committed deltas. A
delta can:

- add, remove, replace, or rename an immediate entry;
- attach or detach an immutable subtree reference;
- adjust the exact aggregates of affected ancestors;
- add or remove trash metadata;
- add, consume, or revoke a share or other namespace capability;
- bind an idempotency key to its committed outcome; and
- reference a bounded immutable transaction page set for a large batch.

A file or folder move encodes source-parent removal, destination-parent
insertion, ancestor aggregate changes, and trash metadata where applicable in
one delta. It never enumerates descendants. Payload work is O(path depth),
publication writes are O(1), and a cold resolver reads at most the bounded
immutable search pages required by the source and destination paths.

Directory listing and stat responses may include an opaque authenticated
snapshot proof containing the shard logical revision, parent directory IDs,
selected immutable base/delta boundary, entry identity/version, and required
aggregate digest. It contains no provider-native value and is safe to carry
through the browser. A mutation can authorize and commit from that proof when
the shard root revision still matches. A changed root invalidates the proof and
forces bounded re-resolution rather than accepting stale authority.

Readers merge the immutable base with the bounded delta window. The window has
a hard read-cost limit. Compaction starts at a low watermark and creates a new
immutable base without blocking mutations. It publishes only by conditionally
replacing the shard root if the expected base and delta boundary remain valid.
Concurrent mutation can continue against the existing root; a losing compactor
leaves only unreachable immutable pages and retries.

The design must reserve sufficient headroom between the low and hard
watermarks. If maintenance cannot keep up, the shard reports an explicit
degraded/rate-limited state rather than allowing unbounded read amplification
or performing an unbudgeted synchronous full compaction inside an ordinary
mutation.

### Single-root mutation protocol

For a bounded ordinary mutation:

1. Read and strictly validate the shard root unless the caller already carries
   an acceptable current logical precondition and request-local native version.
2. Authorize against that exact snapshot and construct one deterministic
   canonical delta containing the idempotency binding and final outcome.
3. Conditionally replace the root against the native version read in step 1.
4. Discard the native version. The new root is the only commit decision.

After required paths or state keys are resolved, the successful publication
path is one root GET plus one conditional root PUT. When an authenticated
snapshot proof supplies the expected logical revision, the same root GET both
validates the proof and supplies the request-local native precondition. A lost
success is resolved by rereading the root and finding the
transaction/idempotency digest. A concurrent writer has one CAS winner; a loser
reloads, reauthorizes, and rebases or returns a real precondition conflict.

If the bounded root cannot carry the transaction, immutable transaction pages
are written first, preferably concurrently. The root delta references their
canonical digests. They are invisible before the root commit and garbage if
the commit loses or the worker crashes. Publication remains one root CAS.

### Logical versions

Public entry versions derive from canonical entry identity and content plus the
committed shard revision needed to detect replacement. They never derive from
provider-native versions. Moving the same node may preserve its stable object
identity while changing its placement version, so existing shares and
preconditions cannot silently retarget to a new object at the old path.

### Idempotency and operation status

Small synchronous actions need no separate operation record. Their transaction
ID and outcome live in the same committed delta as the namespace or state
change. Retried requests resolve the idempotency digest from the root window or
compacted idempotency tree.

Truly asynchronous or payload-paged work uses one bounded immutable intent and
bounded progress pages. Ownership and fencing are CAS transitions on one small
job-control root. Workers may stage unreachable immutable results, but only the
target shard-root CAS publishes the final outcome. Expiry permits a one-winner
fenced takeover; it does not itself change ownership or unlock anything.

Terminal operation and idempotency retention is explicit and bounded. A
compacted summary may retain the replayable outcome without retaining every
intermediate page. Expiry and garbage collection are tested parts of the
format, not optional housekeeping.

## Concurrency and reliability argument

### Conflicting replicas

Two replicas reading the same root receive the same canonical revision but
request-local native versions. Only one conditional root replacement can win.
The loser cannot publish any visible change and must re-read and revalidate.
There is no process-local lock, preferred replica, or routing assumption.

### Stale workers

A stale worker can create only immutable unreachable transaction or compaction
pages. It cannot replace a root after another commit because its native
condition is stale. For a long-running job, a takeover also advances the
canonical job fence; the final shard-root transaction binds that fence and
expected job digest.

### Crash before commit

The old root remains authoritative. Prepared immutable objects are unreachable
and collectible. No rollback is needed.

### Crash after commit with lost response

The new root is authoritative. A retry finds the transaction digest and
idempotency outcome and returns the original success. Re-executing provider
side effects is unnecessary.

### Compactor crash or race

The old base and deltas remain authoritative. A new base is invisible until a
matching root CAS. A compactor that loses the CAS leaves only immutable garbage.
Any replica can resume from canonical root state.

### Corruption

Every root, transaction, base page, and derived page has strict canonical
encoding, type, identity, size bounds, and digest closure. Missing, malformed,
misplaced, cyclic, mixed-version, or digest-inconsistent reachable objects fail
closed. Unreachable garbage does not become authoritative merely because it is
present.

## Checkpoint and portability protocol

The current global candidate-ticket protocol exists primarily to close a gate
without racing a new mutation in another object. V2 moves the write epoch and
open/frozen mode onto the same shard root that mutations replace.

Checkpoint creation proceeds as follows:

1. CAS-freeze the shard registry so no new authoritative shard can appear.
2. Enumerate its bounded/paged canonical shard set.
3. CAS each shard root from `open(epoch N)` to `frozen(epoch N)`. A mutation and
   freeze racing on one root have one conditional winner. Once frozen, a stale
   mutation cannot commit to that shard.
4. Resolve or fence only genuinely asynchronous jobs that could still target a
   frozen root. Unpublished uploads and preparations are non-authoritative and
   do not delay the checkpoint; they resume or restart after reopen.
5. Record the frozen roots and their incremental reachability/Merkle inventory
   in bounded checkpoint pages. Do not create one work object per authoritative
   object and do not read file bodies.
6. Copy canonical keys and bodies unchanged to the destination. Existing and v2
   blob namespaces are both included according to reachable canonical locators.
7. Verify destination provider-attested size, MD5, and CRC32C against the
   canonical inventory and verify every canonical digest closure.
8. Open destination shard roots in a new epoch by conditional update after all
   roots and the registry verify.

Freezing is O(shard count), not part of normal mutation cost. Per-shard freezing
permits bounded progress and can support a short final maintenance window after
an online shadow build. The normative design must prove that registry freeze,
shard freeze, new-user creation, and cross-shard administrative operations have
total conditional outcomes.

Raw-copy portability remains a canonical key/body property. Provider-native
generations, ETags, metadata, page tokens, upload sessions, and rewrite tokens
remain transient. Destination-native versions are observed only after reopen.

## Upload and file-byte protocol

Browser uploads continue to target newly allocated immutable final blob keys.
The Go service never handles the body. Upload completion:

1. reads the namespace root;
2. asks the provider for body-free completion status and the required
   `(size, MD5, CRC32C)` tuple;
3. creates one namespace delta referencing that immutable blob and completing
   the upload intent; and
4. commits the root by conditional PUT.

A root-CAS loss retries with the same immutable blob. No state-object staging,
state-object server copy, or second admitted transaction is needed. An
abandoned blob is unreachable garbage subject to bounded lifecycle cleanup.

Move, trash, restore, rename, copy, and logical deletion never call the file
backend. Same-owner copy is a metadata reflink. Physical blob deletion is
asynchronous reachability collection after retention and checkpoint safety,
not a foreground namespace side effect.

## Derived indexes

Derived indexes consume committed shard revisions in order and publish a
canonical watermark. They batch many committed deltas into packed bounded
segments/pages so total provider cost is amortized rather than merely moved out
of the foreground latency path. They use bounded pages and CAS roots but cannot
make an authoritative namespace mutation visible or invisible.

### Listing order

The compacted base contains high-fan-out order views for name, modified time,
size, and kind. Recent root deltas are merged with those views at read time, so
one mutation does not synchronously rewrite four trees. Cursors bind the base,
delta boundary, sort order, and last composite key. Reads remain bounded by
tree depth, requested page size, and the fixed delta-window limit.

### Duplicate detection

The exact `(size, MD5, CRC32C)` identity remains on authoritative file entries.
Duplicate groups, directory equality, MinHash candidates, and ignore
projections are derived and owner scoped. The UI exposes the indexed-through
revision when it matters. A reconciliation action pins and revalidates every
selected authoritative entry/version in one namespace transaction before
removal, so stale derived results cannot delete current data.

### Rebuild and corruption

A missing, stale, or corrupt derived index can be discarded and rebuilt from
committed roots without changing logical state. It may make the related query
temporarily unavailable or explicitly stale, but it cannot corrupt or block
unrelated mutations.

## StateStore replacement

The application-facing `Get`, `List`, `Create`, `CompareAndSwap`, and `Delete`
semantics remain. Their physical implementation changes from separately
versioned value objects plus synchronously rewritten index nodes and a global
admission ticket to shard-root transactions.

Small values may be encoded in a bounded delta. Larger values are immutable
objects referenced by the delta and written before commit. Create, CAS, and
delete linearize at one shard-root replacement. One-time tokens and final-admin
invariants are grouped into the shard whose root contains the complete guard,
so they retain one-winner behavior without a general multi-object transaction.

State-list cursors bind one immutable base and bounded delta boundary. They do
not contain or materialize the complete namespace.

## Provider-economics acceptance budgets

The following are provisional design ceilings to prove in phase 0, not
aspirational averages. Exact cold ceilings depend on the approved tree fan-out,
maximum height, and snapshot-proof contract and must become numeric before the
format is normative. Request, cost, and latency fixtures remain provider-owned
and ratcheted.

| Successful foreground pathway | Warm/proven-snapshot state ceiling | Additional cold work | File/control requests | Scaling rule |
| --- | ---: | --- | ---: | --- |
| State Get | 2 | bounded base-page reads; numeric maximum required | 0 | O(log fan-out), fixed maximum height |
| State Create/CAS/Delete | 2 | the same bounded key-resolution reads | 0 | one root read + one root CAS |
| Create directory | 3 | unresolved parent-path pages | 0 | independent of directory/subtree size |
| Move or rename | 3 | unresolved source/destination path pages | 0 | O(path depth) cold reads; O(1) publication |
| Trash | 3 | unresolved source-path pages | 0 | independent of subtree descendants |
| Restore | 3 | unresolved destination-path pages | 0 | independent of subtree descendants |
| Logical delete | 3 | unresolved source-path pages | 0 | physical GC excluded from foreground |
| Same-owner copy | 3 | unresolved source/destination path pages | 0 | reflink; no blob copy |
| Complete upload | 4 | at most one destination-parent page | at most 2 metadata/status | no state copy; no body read |
| Abort upload | 3 | none when upload intent is proven | at most 1 cancellation | no body read |
| 100-item metadata batch | 4 | bounded transaction pages written concurrently | 0 | never one provider transaction per item |

An operation exceeding its request, modeled marginal cost, or modeled serial
latency ceiling fails the provider-economics gate. Tests cover cold and warm
paths, success, conflict, lost success, retry, crash recovery, compaction
interaction, and multiple replicas. Improvements append tighter epochs;
budgets cannot loosen without an explicit reviewed specification change.

Maintenance has separate budgets:

- compaction work is proportional to compacted delta bytes/pages, not total
  storage-set size;
- sort and duplicate-index work batches many committed deltas into each
  provider write, and its amortized request/cost budget is ratcheted per 1,000
  foreground commits;
- checkpoint creation creates bounded inventory pages, not one journal object
  per authoritative object;
- checkpoint and migration never stream file bodies through Go; and
- restart reuses completed page/range work without validating every unchanged
  object serially.

## Migration and cutover

This is a new storage epoch and format, not an in-place reinterpretation. The
existing append-only fixtures and ledger remain immutable migration inputs.
The final epoch number and release boundary are assigned only when the
normative v2 format is approved and implemented.

### Migration properties

- V1 remains readable until one conditional v2 cutover decision commits.
- Migration never writes, downloads, copies, renames, or re-uploads file bodies.
- Immutable v1 blobs are referenced in place by v2 entries.
- V2 metadata is built in a disjoint, unreachable namespace.
- Work is partitioned into bounded authenticated pages/ranges, not one work
  object per source object.
- Completed ranges are digest-closed and restartable without rereading every
  completed source object.
- Multiple migrators claim bounded ranges by CAS and converge on identical
  canonical output.
- A preflight phase estimates object count, request count, cost, time, and
  temporary state bytes before changing readiness or closing writes.
- The final freeze/catch-up/cutover window is bounded and measured.
- After cutover, ordinary runtime uses only v2. V1 is retained read-only for a
  reviewed rollback/backup window and later removed by explicit reachability
  policy; no indefinite dual-write or dual-read compatibility layer remains.

### Online build strategy

Because the current format lacks an efficient mutation journal, migration may
use one temporary bridge epoch that adds a bounded canonical change feed while
v1 remains authoritative. The bridge must itself have request budgets and must
not reproduce the current index amplification.

1. Deploy the bridge writer protocol and begin a canonical per-shard change
   feed.
2. Build v2 bases from a pinned v1 checkpoint while normal writes continue.
3. Replay bounded change-feed pages into the v2 shadow roots.
4. Preflight the remaining lag and cutover cost.
5. Briefly freeze v1, replay the final delta, verify v2 roots and reachable
   blobs, and conditionally publish the v2 superblock/cutover marker.
6. Start only v2 writers. A stale v1 writer is fenced by the bridge/cutover
   protocol and cannot publish after the decision.

If production scale permits a simpler offline migration within an explicitly
approved downtime budget, the bridge may be omitted. The choice must be based
on measured preflight evidence, not assumed bucket size.

### Migration evidence

The implementation must retain exact predecessor fixtures and add:

- full feature-profile v2 producer fixtures;
- predecessor-binary interruption fixtures at bridge and cutover boundaries;
- crash/restart tests after every durable range and root publication;
- two-to-eight-migrator convergence schedules;
- stale v1 writer denial before and after cutover;
- raw-copy portability before, during the safe frozen boundary, and after v2;
- corrupted, partial, mixed, newer, and forged shadow-state denial;
- a large inventory test with injected provider latency and enforced request,
  cost, wall-time, and temporary-object ceilings; and
- proof that no file body was opened by migration or checkpoint code.

## Verification matrix

The normative v2 specification must map every existing guarantee to new proof:

| Guarantee | V2 proof shape |
| --- | --- |
| Atomic file/directory visibility | one conditional shard-root commit |
| Concurrent one-winner mutation | same-root CAS race |
| No stale-worker publication | stale native condition plus canonical job fence for long work |
| Crash before commit | old root remains authoritative; preparation unreachable |
| Lost success | transaction digest and idempotency outcome found in committed root |
| Crash recovery | root/transaction replay; no rollback of visible partial state |
| Multi-replica operation | independent replicas race/recover against canonical roots |
| Checkpoint quiescence | registry freeze followed by per-shard root freeze |
| Provider portability | canonical key/body copy and logical versions; native values discarded |
| File-body isolation | source lint plus zero-byte-flow tests |
| Trash/restore subtree safety | one delta transfers immutable subtree reference and aggregates |
| Duplicate reconciliation safety | derived candidate plus authoritative version revalidation |
| One-time token/final admin | guard and outcome in one security/admin root CAS |
| Bounded scale | root/page/delta limits and asymptotic/provider-budget tests |

Mechanism-specific v1 assertions may be deleted only after their corresponding
guarantee has passing v2 tests and migration evidence. Tests are preserved for
behavior and adversarial outcomes, not for obsolete object layouts.

## Implementation sequence

### Phase 0 — Approve guarantees and budgets

- Review this proposal and make the v2 guarantee matrix normative.
- Decide shard boundaries and exact provider-call ceilings.
- Add failing target-budget and asymptotic tests for the new engine.
- Prototype root size, delta-window read cost, compaction throughput, and
  same-user contention using deterministic latency injection.

### Phase 1 — Canonical v2 format and reference model

- Specify canonical roots, transactions, pages, logical versions, blob
  locators, reachability, and garbage eligibility.
- Implement strict encoders/decoders and fuzz tests.
- Build an in-memory reference engine and model-based state-machine tests.

### Phase 2 — Namespace and StateStore engine

- Implement single-root mutation, idempotency, live/trash moves, uploads,
  shares, security shards, and bounded state listing.
- Run every application contract unchanged where it describes behavior.
- Replace mechanism-specific tests with guarantee-level v2 schedules.

### Phase 3 — Compaction and derived views

- Implement non-blocking compaction and reachability collection.
- Implement sort and duplicate derived indexes with revision watermarks.
- Prove lag, rebuild, corruption, restart, and authoritative revalidation.

### Phase 4 — Checkpoint, portability, and migration

- Implement registry/shard freezing and incremental inventories.
- Implement bridge/shadow migration if preflight evidence requires it.
- Complete the full fixture, crash, concurrency, denial, and raw-copy matrix.

### Phase 5 — Cutover and removal

- Run provider-economics, latency-injected, storage-amplification, and scale
  gates against the candidate release.
- Cut production only after preflight evidence meets the approved window.
- Remove v1 runtime writers, readers, staging, ticket, and synchronous derived
  index code after the compatibility window; retain only reviewed migration and
  recovery tooling required by policy.

## Alternatives considered

### Tune or parallelize the v1 protocol

Parallel requests can reduce wall-clock latency but do not reduce billable
request count, state amplification, checkpoint inventory, or the number of
independent failure boundaries. Larger pages and caches can improve particular
reads but cannot remove the mandatory ticket, operation, root, and synchronous
index transactions. This is useful only as a short-lived production mitigation,
not as the target architecture.

### Put authoritative state in an external database

A transactional database could make many of these mutations cheap, but it
would add an operational dependency and a second authoritative persistence
system, weaken raw object-store portability, and conflict with the current
self-contained product contract. It should be reconsidered only as an explicit
product and deployment-model change with its own portability, availability,
backup, and cost analysis—not introduced incidentally to hide object-store
amplification.

### Use one storage-set-wide commit root

One global root gives a simple linearization point but serializes unrelated
users, creates a global hot object, and expands the blast radius of contention
and corruption. Consistency-domain roots retain the same CAS proof while
allowing unrelated users and system concerns to progress independently.

### Publish every change directly into copy-on-write search trees

A pure copy-on-write tree has a clean snapshot model, but a normal mutation
would write a new immutable page at every modified tree level and repeat that
work for each ordering. The bounded root-delta window keeps foreground
publication constant while asynchronous compaction amortizes tree maintenance.
The window is acceptable only with hard read-cost and saturation bounds.

### Make existing derived indexes asynchronous without packing them

This would remove foreground latency but preserve almost all provider request
cost and object-count growth. Derived work must consume ranges of revisions and
publish packed segments so its amortized requests and bytes per foreground
commit are independently budgeted and ratcheted.

### Keep v1 and v2 readers and writers indefinitely

Permanent dual operation doubles the correctness surface, constrains the new
format to old semantics, and makes every later change prove mixed-format
behavior forever. Migration uses a bounded bridge and compatibility window,
then removes superseded runtime paths. Immutable fixtures and explicit recovery
tooling preserve upgrade evidence without retaining two live architectures.

## Risks and required decisions

| Risk or decision | Required resolution before implementation |
| --- | --- |
| Per-user root contention | benchmark 1–8 concurrent uploads and disjoint directory mutations; shard further only if evidence requires it |
| Delta-window saturation | prove compactor capacity/headroom and explicit rate-limited behavior at the hard bound |
| Derived-index lag | define UI watermark/staleness behavior and destructive-action revalidation |
| Share/session hint garbage | define bounded retention and reachability collection |
| Large batches | set transaction page bounds and parallel-write limits without per-item provider calls |
| Checkpoint shard enumeration | define registry paging and freeze ordering with new-user race tests |
| Cross-shard invariants | redesign shard ownership so ordinary invariants have one root; specify a rare multi-root protocol only if unavoidable |
| Existing blob locations | approve versioned canonical blob locators and no-copy migration |
| V1 rollback window | define operator backup, downgrade prohibition after new writes, and removal timing |
| Bridge epoch | choose online bridge or measured offline cutover after production preflight |

## Approval criteria for the normative specification

The v2 specification should not be approved until:

1. every existing security, concurrency, recovery, portability, and feature
   guarantee appears in the guarantee matrix;
2. the common mutation protocols have exact provider-call derivations within
   the proposed ceilings;
3. deterministic model tests cover all commit/freeze/compaction race states;
4. root, delta, page, cursor, history, and garbage bounds are numeric;
5. checkpoint and migration have request/cost/time/storage projections at one
   million and one hundred million objects;
6. no ordinary path synchronously updates a derived index;
7. no migration, checkpoint, hash, or reconciliation path can open a file body;
8. the migration plan has a bounded final freeze and predecessor-writer denial;
9. operational recovery never requires editing provider objects by hand; and
10. the implementation plan deletes superseded runtime machinery instead of
    retaining an indefinite dual architecture.

The desired outcome is not merely fewer calls than v1. It is a storage model in
which the simplest successful action performs the simplest possible durable
commit, while reliability work is invoked only when reliability circumstances
actually require it.
