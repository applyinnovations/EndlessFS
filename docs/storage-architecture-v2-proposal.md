# EndlessFS storage architecture replacement research proposal

**Status:** architectural investigation proposal; not normative
**Scope:** discover and prove the best replacement for the authoritative state
and namespace architecture without weakening product or reliability guarantees

## Purpose

EndlessFS needs a new storage architecture. The current architecture converts a
single user mutation into dozens or hundreds of object-store operations and
synchronously maintains state that does not belong on the mutation's critical
path. That is a systemic design problem, not a tuning problem.

This document proposes how to discover, validate, and specify the replacement.
It deliberately does **not** prescribe a request-count ceiling or declare a
particular storage layout to be the answer before competing designs have been
implemented and measured.

The objective is not to make a design pass a guessed threshold. The objective
is to find the smallest, cheapest, fastest, and simplest operating model that
can deliver the complete EndlessFS feature set while preserving security,
atomicity, concurrency safety, crash recovery, distributed operation,
portability, migration safety, and rollback guarantees.

Request budgets remain valuable after that work. Once the selected architecture
has demonstrated its optimized behavior, its measured request counts, cost,
latency, bytes, and state amplification become regression ratchets. They protect
a proven result; they do not manufacture the result in advance.

## Decision being proposed

Approve an evidence-driven replacement program with four outcomes:

1. A complete fact base describing current behavior, provider primitives,
   workloads, failure cases, and any provable lower bounds.
2. Multiple executable candidate architectures evaluated through the same
   deterministic functional, economic, scale, and fault-injection harness.
3. A documented selection from the non-dominated candidates, including its
   tradeoffs and why no known alternative better satisfies the project goals.
4. A new normative storage specification, schema epoch, migration, and removal
   plan written from the selected evidence rather than from the current v1
   mechanisms.

This proposal does not approve a final v2 schema. It approves the process and
proof required to choose one.

## Correction to the earlier proposal

The earlier draft proposed fixed numerical ceilings such as a particular number
of state requests for move, trash, restore, and upload completion. Those numbers
were not derived from a lower-bound proof or a comparative prototype. They were
therefore arbitrary.

That approach is rejected because it can fail in both directions:

- it can accept an architecture as soon as it falls under an invented limit
  even when a materially better architecture is possible; and
- it can reject the best balanced architecture because an additional operation
  is necessary to preserve a real guarantee or dramatically improves another
  more important property.

This rewrite removes those ceilings. All numeric limits in the final normative
specification must be derived from measured selected behavior, a demonstrated
provider constraint, an explicit product requirement, or a proven safety or
resource bound.

## Evidence that replacement is necessary

The provider-economics work measured the current successful single-item paths:

| Path | State requests | Modeled request cost | Modeled p95 serial latency |
| --- | ---: | ---: | ---: |
| Move | 72 | $0.0001388 | 5.037 s |
| Trash | 100 | $0.0001952 | 6.997 s |
| Restore | 103 | $0.0001688 | 7.102 s |
| Complete upload | 66 | $0.0001448 | 5.016 s |
| Create directory | 76 | $0.0001542 | 5.831 s |

A one-file move currently performs 47 state reads, 24 state writes, and one
state delete. Fifty-seven of those calls maintain directory and duplicate
projections. Fifteen implement gate, admission, operation, and idempotency
mechanisms. A trash/restore loop performs 203 state requests: 135 reads, 62
writes, and six deletes.

The production migration report supplies a second data point: a metadata
migration over 12,095 authoritative objects and 39.2 GB required approximately
72 minutes. It exposed serial provider calls, unbounded work discovery, one
journal object per work item, and body streaming where metadata should have
been sufficient.

These figures are baselines and diagnostic evidence. They are not budgets for
the replacement and do not imply what the optimum must be.

## Non-negotiable product and safety guarantees

Candidate architectures may replace every internal schema, key layout,
transaction protocol, index, interface, or package boundary. They must preserve
the following outcomes unless a separate product proposal deliberately removes
a feature.

### Product behavior

- Files, directories, uploads, downloads, move, copy, rename, trash, restore,
  logical deletion, sharing, duplicate reconciliation, preview, identity, and
  administration continue to work.
- Namespace operations over a directory do not copy or relocate its file
  bodies.
- Moving a subtree is not proportional to its descendant count.
- Batch operations have explicit atomicity and partial-failure semantics and do
  not accidentally become one complete transaction protocol per selected item.
- UI-visible completion means the authoritative outcome is known. Lost
  responses can be reconciled without repeating unsafe side effects.

### Security and authority

- Owner scope derives only from the authenticated session.
- Authorization and path validation use canonical application state, never
  provider listings, metadata, ACLs, tags, or caller-supplied provider keys.
- Provider-native generations and versions remain request-local conditional
  tokens and never enter portable authoritative records.
- One-time tokens, final-administrator protection, share revocation, and other
  security invariants remain atomic and fail closed.
- Corrupt, incomplete, mixed-version, misplaced, or forged reachable state is
  rejected.

### Concurrency and distributed operation

- Two through eight interchangeable replicas can concurrently read, mutate,
  crash, restart, partition, and recover without relying on sticky routing or a
  process-local leader.
- Conflicting mutations have an unambiguous conditional winner.
- Stale workers cannot publish after another writer, takeover, freeze, or
  cutover has invalidated their authority.
- Idempotent retries return the original outcome and do not repeat provider
  side effects.
- No lock is released merely because time elapsed. Where ownership is needed,
  takeover is conditional and fenced.

### Crash recovery and durability

- A crash cannot expose half of an atomic user operation.
- Recovery is deterministic from canonical durable state and does not require
  an operator to edit provider objects by hand.
- A lost-success response is distinguishable from a failed commit.
- Prepared but unpublished data is either safely reusable or unambiguously
  unreachable garbage.
- Long-running work is bounded, restartable, and cannot be duplicated by stale
  workers.

### File-body isolation

- Stored object bodies never flow through the Go control plane unless an
  explicitly approved optional feature, such as image preview generation,
  requires it.
- Upload and download bodies use direct capability-bearing data paths.
- Move, rename, trash, restore, copy-by-reference, duplicate reconciliation,
  checkpoint, migration, and metadata repair do not read file bodies.
- Existing immutable blobs are referenced in place during schema migration.

### Portability and lifecycle

- Authoritative records, logical versions, and integrity closure are provider
  independent.
- Quiescent raw key/body copies can be verified and reopened on an independently
  configured supported backend whose native versions and metadata differ.
- Checkpoint and migration safely race with writers and new shard/user creation.
- Every released storage epoch retains immutable fixtures and a deterministic,
  restartable migration path.
- The new architecture has a bounded cutover and an explicit rollback or
  downgrade policy.

### Scale and resource safety

- Foreground work has stated asymptotic behavior for path depth, directory
  width, subtree descendants, batch size, history length, and replica count.
- Memory, canonical record size, transaction size, retained history,
  unreachable garbage, and maintenance work have enforceable bounds.
- State remains operable at hundreds of thousands and millions of entries.
- Rare checkpoint, recovery, migration, and index work does not impose an
  unexplained tax on every ordinary mutation.

These are pass/fail constraints. Provider count, cost, and latency are then
optimized as far as possible within this valid design space.

## Optimization objective

There is no single magic metric. A provider call can be free or billable, on or
off the critical path, conditional or unconditional, small or large, and
parallel or serial. A design that saves one call but introduces an unbounded
record, unsafe recovery, expensive body transfer, or global contention is not
an improvement.

Every valid candidate is evaluated across this complete economic vector:

- request count by provider role, operation kind, and logical subsystem;
- modeled marginal provider cost using versioned provider pricing fixtures;
- measured local protocol latency and modeled provider p50, p95, and p99
  critical-path latency;
- serial depth and parallel width, not only aggregate call count;
- bytes uploaded, downloaded, copied, listed, and retained;
- number and size of temporary and durable provider objects;
- write, read, checkpoint, migration, and garbage-collection amplification;
- foreground CPU and peak memory;
- background work and convergence time;
- contention and retry amplification as replica and mutation concurrency rise;
- recovery work after each durable failure boundary; and
- implementation, operational, and proof complexity.

Selection uses Pareto dominance rather than an arbitrary weighted score. A
candidate is dominated when another candidate preserves the same guarantees
and is no worse on all material dimensions while being materially better on at
least one. Dominated candidates are rejected.

When non-dominated candidates have real tradeoffs, the decision must expose
them. EndlessFS priorities are, in order:

1. preserve correctness, security, durability, and recoverability;
2. minimize provider cost and user-visible critical-path latency;
3. minimize scaling slopes, body traffic, state amplification, and contention;
4. minimize operational and implementation complexity; and
5. preserve provider portability and self-contained deployment.

The comparison must use representative workloads and sensitivity analysis.
There is no point at which a candidate becomes “good enough” merely by crossing
a request-count line.

## Questions the research must answer

The investigation must answer these questions before a final schema is chosen:

1. What is the smallest consistency domain that contains each real invariant
   without introducing frequent cross-domain transactions?
2. Which data is truly authoritative, and which data can be derived,
   asynchronously materialized, cached, or rebuilt?
3. What is the least provider work needed to authorize and durably publish
   each mutation in cold, warm, conflicted, retried, and recovery cases?
4. Can a listing or stat response safely carry a portable authenticated proof
   that removes repeated path-resolution work without weakening authorization?
5. Which state should be packed together, and where would packing create
   unacceptable contention or record growth?
6. Is the best mutation model a packed mutable root, an immutable journal with
   a conditional head, a persistent tree, a bounded delta/base hybrid, or
   another design discovered during prototyping?
7. How should idempotency and outcomes be retained without a separate generic
   state machine around every small mutation?
8. How should large batches publish atomically without per-item transaction
   amplification or unbounded records?
9. How should sorted listing, duplicate detection, directory similarity,
   search, storage accounting, and audit history be maintained and rebuilt?
10. What checkpoint/freeze protocol totally orders with mutation publication
    while remaining rare-path work?
11. What is the cheapest safe upload-completion protocol available from direct
    provider capabilities and provider-attested metadata?
12. How should garbage eligibility, retention, and physical deletion work
    without entering ordinary namespace latency?
13. What migration strategy gives the best balance of ongoing write overhead,
    final freeze duration, rollback safety, temporary storage, and provider
    cost at actual production scale?
14. Which current application-facing interfaces encode v1 implementation
    assumptions and must be replaced rather than emulated?

## Fact-base phase

### Operation inventory

Create an exhaustive inventory of every pathway that can contact state, file,
preview, or direct data-plane storage. At minimum it includes:

- state get, list, create, compare-and-swap, delete, and batch forms;
- file stat, upload allocation, upload completion, abort, download allocation,
  copy-by-reference, and physical collection;
- create directory, move, rename, copy, trash, restore, logical delete, purge,
  and metadata updates;
- share create, resolve, revoke, and access accounting;
- duplicate index consumption, ignore rules, and reconciliation;
- preview lookup, generation, publication, invalidation, and collection;
- authentication, session, recovery, invite, bootstrap, theme, and
  administration mutations;
- checkpoint, raw-copy verification, migration, compaction, repair, recovery,
  and garbage collection; and
- success, denial, conflict, retry, lost response, crash, takeover, and
  multi-replica variants of each relevant path.

For each pathway, record exact current calls, bytes, objects, serial depth,
cost, latency, durable boundaries, and the reason each call exists. Mark each
call as logically necessary, necessary only because of the v1 representation,
derived-view maintenance, recovery preparation, compatibility, or unknown.

### Workload model

Measurements must cover more than one-file happy paths. Define versioned,
reviewable workload fixtures including:

- cold and warm reads;
- shallow and maximum-depth paths;
- narrow and very wide directories;
- subtrees with zero, one, thousands, and millions of descendants;
- single-item and large batch mutations;
- single-replica and concurrent multi-replica writers;
- hot-user contention and unrelated-user concurrency;
- read-heavy, upload-heavy, organization-heavy, duplicate-heavy, and mixed
  workloads;
- fresh stores, long-lived stores, maximum retained history, and maintenance
  backlog; and
- realistic production inventory distributions, not only uniform synthetic
  trees.

Workload fixtures are inputs to comparison, not hand-tuned demonstrations for
one candidate.

### Provider primitive model

Document the exact portable object-store primitive set and the stronger
optional capabilities of each provider:

- conditional create, replace, delete, and server-side copy semantics;
- read, stat, and list consistency;
- request and object size limits;
- batch or compose primitives and their portability consequences;
- direct upload/download capability lifecycle;
- provider-attested size and checksum availability;
- pricing by request class, retrieval, transfer, and storage;
- typical latency distributions and concurrency limits; and
- lost-response and retry semantics.

Do not assume that all GETs, writes, metadata operations, or providers have the
same economics. Provider-native accelerations may be used only behind a
portable semantic contract with a correct baseline implementation.

### Lower-bound analysis

For each operation, derive only what can actually be proven.

Examples of legitimate reasoning include:

- if the authoritative state is remote and no current conditional token is
  safely available, a writer must obtain enough current authority to make a
  conditional decision;
- an atomic durable mutation needs some durable linearization event;
- direct upload completion needs trustworthy evidence that the immutable blob
  exists with the expected provider-backed identity; and
- a checkpoint must establish a total order between included state and every
  mutation capable of publication.

These statements do not automatically imply a fixed number of calls. A safe
snapshot proof, batched primitive, already-held conditional token, different
consistency domain, or different state representation may change the count.

Every claimed lower bound must list its assumptions and must be revisited when
a candidate changes those assumptions. Unknown values remain unknown; they are
questions for a prototype, not invitations to invent a ceiling.

## Candidate architecture phase

The team must implement more than one credible candidate far enough to measure
the complete mutation and recovery protocols. Candidate selection cannot be a
paper comparison in which one favored design receives all implementation
detail and alternatives remain strawmen.

Initial families worth exploring include, but are not limited to:

### Packed consistency-domain state

Atomically related metadata is packed into bounded per-domain state objects and
mutated by conditional replacement. The experiment must determine how far
packing can reduce calls before contention, object size, read amplification, or
write amplification becomes worse than the alternatives.

### Immutable transaction journal with conditional heads

Mutations create immutable transaction records or segments and conditionally
advance a small authoritative head. The experiment must measure publication,
lost-success recovery, read reconstruction, compaction, history retention, and
garbage amplification.

### Persistent high-fan-out trees

Mutations publish structurally shared immutable pages through a conditional
root. The experiment must measure cold path depth, pages rewritten per
mutation, sorted views, batch behavior, root contention, and checkpoint
reachability.

### Bounded delta plus compacted-base hybrids

Foreground commits publish bounded deltas while background work folds them into
immutable packed bases. The experiment must measure steady state, delta-window
read amplification, compactor capacity, saturation behavior, crash races, and
the real amortized provider cost rather than moving existing amplification into
background work.

### Alternative partitioning and transaction boundaries

Each viable representation must be tested with multiple shard and consistency
domain strategies. A single global root, per-user root, directory-level root,
and adaptive partitioning have different contention and cross-domain costs.
No boundary is selected until invariant mapping and workload measurements
support it.

New candidate families discovered during implementation must be added when they
plausibly dominate the current set. The investigation is not limited to the
ideas listed here.

## Prototype rules

Candidate prototypes should live behind an experimental boundary and share:

- the same in-memory semantic reference model;
- the same canonical operation and workload fixtures;
- the same instrumented object-store fake and GCS protocol fake;
- the same injected clocks, IDs, randomness, provider latency, and fault
  scheduler;
- the same security, corruption, portability, and multi-replica tests; and
- the same economics report format.

A prototype may omit production polish, but it may not omit the hard part of
its own design. If its claimed efficiency depends on compaction, recovery,
checkpoint freeze, batch publication, or garbage collection, that mechanism
must exist far enough to measure and fault-test. Deferred work must appear in
the comparison as unknown cost, not zero cost.

Each candidate produces an architecture dossier containing:

1. canonical data model and consistency domains;
2. state diagrams for success, conflict, retry, lost success, crash, and
   takeover;
3. request derivations for each operation and scenario;
4. measured economics and scaling curves;
5. proof matrix for every non-negotiable guarantee;
6. checkpoint, migration, compaction, and garbage behavior;
7. operational recovery procedure;
8. known limitations, unresolved questions, and sensitivity to provider or
   workload assumptions; and
9. code and deterministic commands that reproduce every result.

## Comparative evaluation

### Functional and adversarial gate

A candidate cannot enter the efficiency comparison until it passes the common
semantic and guarantee suite. Faster incorrect designs are invalid, not
tradeoffs.

The suite includes deterministic schedules that pause, crash, partition,
restart, and resume two through eight replicas at every durable boundary. It
proves atomic visibility, one-winner conflict behavior, stale-writer denial,
lost-success recovery, idempotency, bounded takeover, checkpoint races,
corruption denial, raw-copy portability, and migration/cutover safety.

### Economics report

For every operation/workload/scenario, report:

| Dimension | Required result |
| --- | --- |
| Requests | exact count by provider role, request kind, and subsystem |
| Cost | modeled marginal cost under every supported provider fixture |
| Latency | measured and modeled critical path, including serial depth |
| Transfer | metadata and body bytes read, written, copied, and listed |
| Storage | authoritative, derived, historical, and temporary amplification |
| Scale | slope against entries, descendants, width, depth, batch, history, and replicas |
| Contention | successes, retries, wasted preparation, and tail latency |
| Recovery | requests, bytes, and time after each injected failure boundary |
| Maintenance | amortized compaction, indexing, checkpoint, migration, repair, and GC work |
| Complexity | durable states, invariants, operational interventions, and proof surface |

Raw tables and machine-readable fixtures are committed. Summaries may not hide
an expensive cold, recovery, maintenance, or high-contention path behind an
average.

### Pareto and sensitivity analysis

Compare candidates for each meaningful workload and across the aggregate
portfolio. Identify:

- candidates that are strictly dominated;
- dimensions where a candidate is uniquely best;
- workload or provider assumptions that change the result;
- the cost of every additional guarantee or feature;
- gaps between measured behavior and any proven lower bound; and
- remaining improvement ideas for each non-dominated candidate.

If no candidate is clearly best, continue the research, combine compatible
ideas, or make the real product tradeoff explicit. Do not resolve uncertainty
by declaring a guessed threshold and accepting the first design beneath it.

## High-value hypotheses to test

The current evidence suggests several promising directions. They are
hypotheses, not decisions:

- a namespace move should be expressible as metadata reference changes and
  aggregate adjustments without any file-provider operation;
- atomically related outcome, idempotency, and namespace state may be cheaper
  when committed together instead of through generic nested StateStore
  transactions;
- rebuildable sort, duplicate, search, audit, and checkpoint projections may
  not belong in the foreground mutation;
- immutable preparation plus one conditional authority update may provide
  crash safety without `running`, `committed`, and `succeeded` writes for small
  operations;
- authenticated listing/stat proofs may avoid repeated resolution on unchanged
  state;
- batch transactions may share authorization, publication, and recovery work;
- existing immutable file blobs can survive a complete metadata-schema
  replacement without provider copies; and
- rare freeze and migration mechanisms may be separated from ordinary write
  admission without weakening their total ordering.

Each hypothesis must be falsifiable and compared against alternatives. A
promising idea that loses under representative measurements is not retained
for aesthetic reasons.

## Derived data investigation

Sorting, duplicate grouping, directory equality and overlap, search, storage
accounting, audit projections, and checkpoint inventories require explicit
classification.

For each projection, determine:

- whether it is required for authorization or only for discovery/presentation;
- whether it must be current, can expose a watermark, or can be rebuilt on
  demand;
- the cheapest incremental, batch, or rebuild strategy;
- how lag and corruption appear in the UI and operations;
- how destructive actions revalidate authoritative entry versions; and
- its foreground and amortized economics at production scale.

Moving work to a background worker is not itself an optimization. A design
that performs the same number of provider operations later may improve UI
latency while leaving cost and state amplification unchanged. Both outcomes
must be reported.

## Upload and file-byte investigation

The selected design must retain direct browser-to-provider upload and
download. The research should compare provider-portable completion protocols
using body-free status and metadata queries, including `(size, MD5, CRC32C)`
where available from GCS.

For each protocol, measure allocation, completion, lost-response retry,
abandonment, root/metadata conflict, and garbage collection. Determine whether
the upload intent, final namespace entry, idempotency outcome, and provider
attestation can be committed together or whether a separate durable boundary
provides a demonstrated benefit.

Move, trash, restore, rename, logical deletion, same-owner copy, duplicate
reconciliation, checkpoint, and migration must remain metadata-only. Physical
blob deletion is reachability and retention work, not a foreground namespace
side effect.

## Checkpoint and portability investigation

The current candidate-ticket gate is one way to stop mutations from racing a
checkpoint. It is not a permanent requirement.

Every candidate must specify and prove how it:

1. prevents new authoritative domains from escaping enumeration;
2. totally orders each mutation against freeze;
3. handles unpublished uploads and long-running jobs;
4. inventories reachable canonical state with bounded restartable work;
5. excludes provider-native metadata from authority;
6. copies canonical keys and bodies without reading file bodies through Go;
7. verifies destination provider-attested blob identity and canonical state
   integrity; and
8. reopens independently with changed native versions and continues
   multi-replica mutations.

Compare global gates, per-domain freeze state, immutable revision checkpoints,
and any other credible protocol on both ordinary-path tax and checkpoint cost.
The chosen protocol must be the best complete tradeoff, not merely the one with
the cheapest happy-path mutation.

## Migration and cutover investigation

The replacement is a new storage epoch, not an in-place reinterpretation.
Existing ledgers and fixtures remain immutable inputs.

Every migration candidate must preserve these properties:

- file bodies are not downloaded, uploaded, copied, renamed, or rewritten;
- immutable v1 blobs are referenced in place;
- v2 metadata is built in a disjoint unreachable namespace;
- work is bounded, authenticated, restartable, and safe under multiple
  migrators;
- completed work is reused after restart;
- partial, forged, stale, mixed, or corrupt shadow state fails closed;
- stale predecessor writers cannot publish after cutover;
- preflight reports request count, cost, time, transfer, temporary state, and
  final freeze projection before production authority changes; and
- the final decision and rollback/downgrade policy are unambiguous.

Compare at least:

- a measured offline freeze/build/cutover;
- an online shadow build fed by a temporary bounded canonical change stream;
  and
- any snapshot or revision-based approach enabled by a candidate architecture.

Select based on measured production inventory and write rate. Do not add a
permanent bridge tax to every future mutation when a bounded offline cutover is
safer and cheaper, and do not assume downtime is acceptable when evidence shows
otherwise.

## From selected architecture to normative specification

Only after comparative evidence selects an architecture should the project
write the normative replacement specification. That specification must include:

- the complete canonical key/body grammar and numeric record/page bounds;
- consistency domains and every atomic invariant;
- success, conflict, retry, crash, recovery, compaction, checkpoint, and
  garbage protocols;
- portable logical version and integrity rules;
- provider capability requirements and fallback behavior;
- exact asymptotic bounds;
- the complete guarantee-to-test matrix;
- migration, cutover, rollback, and removal procedures; and
- measured economics for every operation and scenario.

At that point, deterministic request counts and cost/latency envelopes become
append-only regression ratchets. The initial ratchet is set from the optimized
selected implementation, not from this proposal. It records exact behavior and
prevents accidental regression.

A future change may add a provider operation when evidence proves that the
operation is required for a new feature, fixes a correctness issue, or produces
a better overall economic tradeoff. Such a change must update the comparative
evidence and specification. Ratchets are guards against unexplained regression,
not a prohibition on reasoned architecture evolution.

Passing the ratchet never means optimization is finished. Reports retain the
gap to known lower bounds, Pareto alternatives, and remaining improvement ideas
so the architecture can continue to improve.

## Delivery sequence

### Stage 1 — Fact base and common harness

- Complete the operation, workload, provider-primitive, and current-call
  inventories.
- Add subsystem attribution for every provider request and byte.
- Build the shared semantic reference model, economics reporter, scale
  generator, and deterministic replica/fault scheduler.
- Record assumptions and prove only defensible lower bounds.

### Stage 2 — Candidate prototypes

- Implement multiple credible candidate families behind an experimental
  boundary.
- Exercise the complete operation and failure matrix.
- Publish reproducible architecture dossiers and raw results.
- Iterate on candidates while material improvements remain unexplored.

### Stage 3 — Selection and specification

- Remove invalid and dominated candidates.
- Perform provider/workload sensitivity analysis on the non-dominated set.
- Select the best-supported architecture and document every tradeoff.
- Write the normative replacement specification and evidence-derived
  regression ratchets.

### Stage 4 — Production engine and migration

- Implement the selected canonical format and portable engine with
  test-first schema fixtures.
- Implement compaction, derived views, checkpoint, repair, and garbage paths;
  none may remain an unmeasured placeholder.
- Implement and fault-test the selected migration/cutover protocol.
- Run scale, economics, coverage, race, fuzz, portability, and predecessor
  migration gates before release.

### Stage 5 — Cutover and simplification

- Run production preflight and review its cost, time, transfer, storage, and
  freeze projections.
- Cut over only when the complete safety and operational evidence passes.
- Observe the selected economics and recovery behavior in production.
- Remove v1 runtime writers, readers, tickets, staging, operation machinery,
  and synchronous derived indexes after the approved compatibility window.
- Retain immutable fixtures and only the explicit migration/recovery tooling
  required by policy; do not keep an indefinite dual architecture.

## Research deliverables

The architecture-selection review must receive:

1. a machine-readable current-state provider-call and byte inventory;
2. versioned workload and provider economics fixtures;
3. lower-bound arguments with explicit assumptions;
4. executable prototypes for multiple credible candidates;
5. guarantee matrices and deterministic failure schedules for each candidate;
6. complete economics and scaling curves, including maintenance and recovery;
7. Pareto and sensitivity analysis with dominated alternatives identified;
8. a recommended architecture with unresolved tradeoffs stated plainly;
9. a draft normative schema and migration plan based on that recommendation;
   and
10. reproducible commands and immutable evidence artifacts.

## Selection criteria

The final architecture is ready for normative review only when:

1. it preserves every non-negotiable product and safety guarantee;
2. no known candidate with equivalent guarantees dominates it;
3. its request, cost, latency, transfer, storage, contention, recovery, and
   maintenance behavior are fully measured rather than inferred from a happy
   path;
4. its asymptotic and resource bounds are explicit and proven by scale tests;
5. its chosen tradeoffs remain favorable across realistic provider pricing,
   latency, inventory, and workload sensitivity analysis;
6. its checkpoint and migration designs are complete and body-free;
7. its operational recovery requires no manual provider-state surgery;
8. its implementation and proof complexity are justified by benefits that
   simpler candidates cannot deliver;
9. its remaining distance from any known lower bound is documented; and
10. the implementation plan removes the superseded v1 architecture instead of
    wrapping it indefinitely.

The intended outcome is not an architecture that satisfies a number chosen in
advance. It is the best architecture the project can substantiate: every
provider operation has a demonstrated purpose, every retained tradeoff is
explicit, no known valid alternative is better overall, and future regressions
are measured against that proven result.
