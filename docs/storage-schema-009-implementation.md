# Storage schema 009 implementation record

**Historical qualification correction:** the original migration claim in this
record was incomplete. The `007 -> 008` edge listed obsolete `state/` records
instead of the authoritative schema-007 `state-indexes`/`state-versions` graph,
and the then-current fixtures did not contain complete identity/passkey state.
Schema 010 repairs retained production-shaped authority and makes conservation
a pre-activation invariant. See
[`storage-schema-010-implementation.md`](./storage-schema-010-implementation.md).
The schema-009 runtime/economics architecture below remains current, but this
document is not evidence that the released predecessor migration conserved all
application state.

**Status:** phases 1–7 are implemented and locally qualified on the schema-009
branch. Strict coverage, complete race shards, and the composed
`nix flake check` pass. Phase 8 commits and pushes the implementation for CI.

Schema 009 keeps the schema-008 owner namespace graph and replaces the generic
application-state placement contract with typed, invariant-aligned consistency
domains. It also adds atomic state mutations, a helpable cross-domain decision
protocol, batched upload intent publication, checkpoint-bound garbage
collection, and executable request/cost/latency coverage for every
provider-backed production route.

## 1. State authority and partitioning

Every application value is stored in a canonical `StateRecord009` envelope that
binds the opaque application payload to its expected record type; application
record codecs continue to enforce strict JSON. Decoding a
profile as a session, accepting non-canonical bytes, or routing a key to an
unrecognized invariant fails closed.

| Domain | Canonical authority |
|---|---|
| `namespace/<owner>` | Live/Trash graph, upload intents, shares, drive idempotency, and batch operations for one owner |
| `owner-identity/<owner>` | Profile, account, credentials and indexes, sessions, recoveries, owner-bound ceremonies, identity operations, and identity idempotency |
| `owner-jobs/<owner>` | Preview operations, preview indexes, and preview idempotency |
| `administration/administration` | Bootstrap state, administrator roles, invites, and admin operations |
| `capability/<shard>` | Public/unowned ceremony capabilities that have no authenticated owner yet |

This placement makes the common multi-record invariants single-domain. Profile,
account, credential, session, and identity-operation changes for one owner can
publish through one owner-identity head. Upload intent, namespace placement,
Trash metadata, shares, and drive idempotency publish through the same owner
namespace head. No bucket-wide mutable account, file, directory, or job record
is introduced.

`state.AtomicStore.Mutate` validates and normalizes every change before touching
the provider, rejects a cross-domain set, writes only changed immutable pages,
and publishes the complete result with one domain-head CAS. The retained
mutation outcome binds the normalized fingerprint, result, logical versions,
and expiry. A lost successful provider response is resolved by rereading that
outcome; reuse of the mutation ID with another intent is rejected.

## 2. Helpable cross-domain transitions

`state.TransactionalStore.Transact` is reserved for invariants that genuinely
span domains, such as administration plus owner identity. It does not expose a
partially applied sequence:

1. A create-only canonical plan binds the transition ID, complete normalized
   intent fingerprint, participants, preconditions, result, and retention.
2. Each participant conditionally installs the same transition lock in its
   domain head after validating its local preconditions.
3. One create-only canonical decision is the global commit/abort linearization
   point.
4. Any replica can finalize every locked participant from that decision. A
   reader encountering a lock helps the transition before returning state.
5. Expired undecided plans are durably aborted; committed/aborted outcomes are
   retained for idempotent retry and later collected only when no lock remains.

A crash can therefore leave an immutable plan, prepared locks, a decision, or
some finalized participants, but never an authoritative ambiguous outcome.
Checkpoint closure resolves all plans before freezing domains. Conditional
domain-head writes fence stale workers, and no correctness step depends on a
process-local leader, graceful shutdown, elapsed-time unlock, or provider-native
identifier persisted in canonical state.

## 3. External provider effects

Object-provider effects are treated as idempotent consequences of canonical
intent, never as the visibility point for application state.

- An upload first publishes an `initializing` intent and idempotency binding in
  the owner namespace. Only then does it create the unavoidable provider upload
  session and a bounded encrypted lease. Activation is another owner-head
  mutation. Completion atomically publishes the verified immutable blob
  reference and terminal upload state in one namespace revision.
- Abort and completion mark provider cleanup as pending before deleting or
  aborting the transient lease. Any replica can finish the cleanup. A cleanup
  outage cannot roll back or contradict the authoritative terminal state.
- A batch of at most 100 uploads validates every member, publishes every intent
  in one owner-head mutation, creates independent provider sessions in parallel,
  and activates every member in one further owner-head mutation. It never runs
  one full state transaction per item. The shared provider contract proves
  replay and all-or-nothing intent creation; memory-provider rollback retains
  pre-existing concurrent state.
- Move, copy, Trash, restore, and logical delete only update the namespace graph.
  They issue no file-provider copy/delete and do not enumerate descendants.

## 4. Checkpoint-bound garbage collection

After a schema-009 checkpoint root is durably published while the gate is
closed, its authenticated inventory becomes the immutable mark set. A portable
garbage-collection session binds the checkpoint ID, gate epoch, and inventory
digest and stores only a sweep index and exclusive canonical-key cursor.

The collector sweeps only explicitly recognized domain pages/inert heads,
transition records absent from the checkpoint, rebuildable projections,
transient leases, query snapshots, and unreachable immutable file blobs.
Unknown reserved keys are never deletion-eligible. It never opens file bodies.
Each deletion is conditional on the native version returned by the immediately
preceding listing, so a stale worker cannot delete a recreated incarnation.
Independent deletes in one provider page run concurrently.

Progress is persisted only when another provider page remains. Final pages and
empty prefixes are safe to replay and collapse into the terminal session CAS,
avoiding billed state writes that add no recovery guarantee. `OpenWrites`
requires the exact terminal session, verifies the checkpoint again, then opens
the gate. Crash tests cover session creation, a persisted multi-page cursor,
terminal publication, concurrent replicas, corrupt/misbound sessions, unknown
key denial, and a 128-object scale fixture.

## 5. Adjacent 008 → 009 migration

The append-only ledger adds exactly one `008 -> 009` edge and the
`transactional-state-domains-v1` feature. Under the closed gate the transformer:

1. authenticates every schema-008 source domain and state key;
2. wraps each unchanged application payload in its typed schema-009 envelope;
3. deterministically stages values into namespace, owner-identity, owner-jobs,
   administration, and capability domains;
4. installs content-addressed pages and the complete target catalog;
5. freezes the target closure, creates/verifies the edge checkpoint, advances
   writer/superblock/gate feature binding, reopens, and unfreezes; and
6. retires predecessor domain authority only after the target is complete.

The edge is deterministic, forward-only, idempotent, resumable at durable
boundaries, and convergent under concurrent starters. It performs no file-body
read, blob copy, path rewrite, user-ID rewrite, or provider-native metadata
persistence. Immutable schema-009 fixtures exist for portable-minimal,
application-disabled, and application-GCS profiles and are bound to their
producer commit and SHA-256 digest in
`internal/portable/testdata/migrations/README.md`.

## 6. Executable provider economics

The GCS schema-009 delta contains 90 exact budgets. Sparse append-only deltas
inherit every unchanged prior budget; an existing pathway can only retain or
tighten all count, cost, aggregate-latency, critical-path-latency, and per-role
limits. A role-set change, removed pathway, or loosened value fails closed.

The production catalog classifies every object-store-facing state/provider/
preview contract and every registered HTTP route. Tests also prove that every
current ratchet is present in that catalog and referenced from executable test
code. The complete machine-readable table is
[`budgets-schema-009-regional-standard-flat-2026-08.json`](../internal/objectstore/gcs/economics/budgets-schema-009-regional-standard-flat-2026-08.json).

Selected exact calibrations follow. Costs are modeled marginal GCS request
costs; latencies are conservative fixture estimates, not production percentiles
or an SLA.

| Workload | Requests | Cost (USD) | Aggregate p95 | Critical p95 |
|---|---:|---:|---:|---:|
| State `Get` | 1 | $0.0000004 | 65 ms | 65 ms |
| State single-record CAS | 2 | $0.0000054 | 145 ms | 145 ms |
| Atomic two-record same-domain mutation | 8 | $0.0000216 | 580 ms | 580 ms |
| Atomic two-domain transition | 18 | $0.0000486 | 1.305 s | 1.305 s |
| Trash one file/tree root | 5 | $0.0000158 | 370 ms | 370 ms |
| Restore one file/tree root | 4 | $0.0000062 | 275 ms | 275 ms |
| Trash 10,000 selected roots | 125 | $0.0004318 | 9.547 s | 3.116 s |
| Copy 10,000 selected roots | 126 | $0.0004368 | 9.612 s | 3.116 s |
| Move 10,000 selected roots | 127 | $0.0004372 | 9.673 s | 3.261 s |
| Empty 10,000 Trash roots | 84 | $0.0002268 | 6.200 s | 3.052 s |
| Create 100 upload capabilities | 205 | $0.0010112 | 20.360 s | 480 ms |
| Warm startup | 10 | $0.0000178 | 695 ms | 695 ms |
| Minimal checkpoint plus empty sweep | 34 | $0.0001010 | 2.735 s | 2.735 s |
| Checkpoint sweep of 128 unreachable pages | 163 | $0.0001014 | 10.480 s | 2.860 s |
| Compact a 300-mutation domain | 15 | $0.0000382 | 1.081 s | 1.081 s |
| Recover a prepared two-domain transition | 12 | $0.0000278 | 855 ms | 855 ms |
| Migrate minimal 008 fixture to 009 | 294 | $0.0005680 | 20.736 s | 19.356 s |

The 100-upload path contains 100 unavoidable provider session initiations and
100 transient lease writes, but only two batched authoritative namespace
mutations. Its 480 ms critical estimate records the parallel session wave; the
20.360 s aggregate is the sum of all modeled provider work and is retained for
cost/capacity analysis.

## 7. Qualification evidence

Focused schema-009 codec, routing, mutation, transition, migration, upload,
checkpoint-GC, provider-contract, and exact economics tests pass. The current
source tree also has the following local evidence:

| Gate | Result |
|---|---|
| Strict repository coverage | 85.647% (19,722/23,027; required ≥85%) |
| Security-sensitive coverage | Every named group ≥95%; token 100%, configuration 98.328% |
| Migration coverage | 98.220% (1,766/1,798; required ≥98%) |
| Exhaustive migration fault matrix under `-race` | Pass, 1,473.393 s under composed-gate load |
| Every remaining repository test under `-race` | Pass, portable package 552.603 s under composed-gate load |
| Provider economics | GCS budget catalog and all executable request/count/cost/latency ratchets pass |
| Browser coverage | Go-controlled Chromium E2E passes in the strict coverage run |
| Final composed `nix flake check` | Pass, all 24 checks on `aarch64-darwin` |

The race gate deliberately uses two package processes without changing its
30-minute timeout. The exhaustive migration test restarts the entire schema
chain at every provider boundary and passes once by itself; the complementary
invocation uses Go's `-skip` to run every other repository test once. Together
the shards retain complete race coverage while preventing two unrelated
portable fault matrices from competing for the same constrained Nix build
cores.

The schema-008 migration freeze fence was also tightened during qualification.
One post-CAS gate read per domain is the ordering barrier; an earlier pre-CAS
read closed no additional race and was removed. If a lagging worker observes
that another replica already reopened the gate, it retracts its own old-epoch
head before conditionally thawing the old catalog. An open gate helps every
non-future catalog freeze, and a closing gate helps every older freeze; future
epochs and closed-gate epoch mismatches still fail closed. Head cleanup remains
safe when the catalog already belongs to the next closing epoch, while catalog
cleanup cannot disturb that next migration. Tests cover the exact
reopen/next-close and multi-edge late-worker interleavings. The historical schema-001→009
eight-replica matrix passes 100 consecutive focused repetitions plus the
complete migration gate under concurrent strict-coverage load.

This implementation record qualified the schema-009 runtime and economics, not
the semantic completeness of the historical `007 -> 008` migration. Current
release qualification requires the schema-010 complete-corpus, conservation,
cryptographic-authentication, and activation-barrier evidence in addition to
the unchanged schema-009 runtime evidence. It does not by itself claim a
migration rollout or live deployment.
