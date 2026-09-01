# Provider request surface and scale audit — 2026-09-01

## Outcome

This audit closes the browser request-amplification incident and records the
complete production provider boundary. It does **not** accept the surviving
schema-010 request counts as efficient. The measured values below are failure
baselines: their append-only ratchets prevent regression while the replacement
architecture is built, but passing a current-baseline ratchet is not evidence
that a pathway meets its efficiency target.

Provider-backed work is acceptable only when its cardinality follows the data
that must actually change:

- dormant device-local UI state performs zero provider work;
- metadata-only operations over a bounded selection persist one compact intent
  and one atomic namespace publication, without rewriting a page tree or
  retaining one success record per item;
- a 10,000-row directory or Trash projection is read from at most two packed
  4 MiB segments and progressively decoded through one browser request;
- previews are bounded by the virtualized visible/overscan window rather than
  logical directory size; and
- linear transfer work is permitted only for distinct active provider upload,
  download, or abort sessions that necessarily correspond to distinct object
  operations. Its concurrency remains bounded by the server-advertised worker
  pool.

The numerical targets are derived from those request shapes, measured encoded
payload envelopes, and the reviewed GCS price/latency fixtures. They are not
percentage reductions selected from the current implementation. The current
ratchets and the lower target catalog are intentionally separate so an
inefficient measurement can never be mistaken for an acceptable ceiling.

## Incident and corrections

The production transfer ledger contained 4,446 nonterminal records. Startup
treated every record as an active provider upload and called upload status for
each one before revealing the workspace. The transfer-status component alone
therefore produced `4,446 × 3 = 13,338` provider requests.

The corrected workflow:

1. reveals the authenticated workspace before device-local transfer history is
   restored;
2. restores a record without a reacquired `File` as local `needs-source` state;
3. performs no provider-backed status lookup merely because an IndexedDB record
   exists; and
4. uses status only after a real idempotent upload-admission conflict needs
   exceptional disambiguation.

The latest deterministic 10,000-record browser proof completed in 32 ms on the test host,
with zero upload-status and zero provider requests. The former loading path was
blocked after its first four concurrent requests and never became interactive.

Other amplification removed in the same audit:

- upload folder preparation remembers each successfully prepared virtual
  directory across planning, admission, and transfer phases; the browser proof
  for two unique directories now requires exactly two stat checks;
- live, Trash, and public-share browsers use 1,000-entry pages, reducing a
  10,000-entry browse from about 400 composed provider requests to 80;
- selected restore, selected permanent deletion, and Trash Undo use one atomic
  batch route instead of one mutation and one poll per row;
- terminal mutation results are consumed directly instead of paying an
  immediate operation-status lookup; and
- group cancellation uses the configured transfer control pool instead of
  starting one simultaneous abort request per active upload.

## Exhaustiveness boundary

The audit is enforced at four layers:

| Layer | Completeness proof |
|---|---|
| Object-store wire operations | The GCS protocol economics tests classify every head, verify, get, open, list, put, delete, copy, upload-session, progress, abort, signed-download, and data-plane request. Unclassified requests fail. |
| Application contracts | Reflection checks map every method of `state.Store`, `state.AtomicStore`, `state.TransactionalStore`, `provider.Storage`, `provider.TrashStorage`, `provider.BatchStorage`, `provider.UploadBatchStorage`, `provider.DuplicateStorage`, and `preview.Store` to a production workload. |
| HTTP surface | The registered-route AST check requires every route to be classified as provider-backed or deliberately local-only. A newly registered unclassified route fails the gate. |
| Browser composition | E2E and source-policy tests cover restored transfer history, directory pagination, preview-window bounds, unique directory preparation, terminal operation polling, mutation batching, and transfer-control concurrency. |

The production workload catalog covers state, namespace, transfers, upload
planning, duplicate reconciliation, preview state/source/artifacts, direct data
planes, sessions, identity and administration, shares, themes, startup,
checkpointing, compaction, recovery, derived-view rebuild, and migration. Every
catalogued GCS budget must both exist in the append-only ledger and be referenced
by an executable test.

## Measured current baselines — not acceptance

Costs use the reviewed GCS Regional Standard Storage flat-namespace marginal
pricing fixture, excluding free tier and negotiated discounts. Latency is a
conservative engineering model, not a Google Cloud SLA. “Critical p95” accounts
for the explicitly proven immutable-page or worker-pool parallel groups;
“aggregate p95” remains available in the emitted `provider-scale-v1` evidence.

| Production state / intent | Logical items | Browser requests | Provider requests | Modeled marginal cost (USD) | Critical p95 | Current scaling basis |
|---|---:|---:|---:|---:|---:|---|
| Restored transfer history without sources | 10,000 | 0 | 0 | $0 | 0 s | Device-local only |
| Browse live directory | 10,000 | 10 | 80 | $0.0000320 | 5.265 s | Ten 1,000-entry pages |
| Browse Trash | 10,000 | 10 | 80 | $0.0000320 | 5.265 s | Ten 1,000-entry pages |
| Copy selected roots | 10,000 | 1 | 128 | $0.0004376 | 3.246 s | One atomic publication |
| Move selected roots | 10,000 | 1 | 129 | $0.0004380 | 3.391 s | One atomic publication |
| Move selected roots to Trash | 10,000 | 1 | 127 | $0.0004326 | 3.246 s | One atomic publication |
| Restore selected Trash roots | 10,000 | 1 | 167 | $0.0004440 | 3.249 s | One atomic publication |
| Permanently delete selected Trash roots | 10,000 | 1 | 86 | $0.0002276 | 3.182 s | One atomic publication |
| Retry Trash after lost response | 10,000 | 1 | 44 | $0.0000176 | 2.892 s | Read durable outcome; zero writes |
| Retry restore after lost response | 10,000 | 1 | 44 | $0.0000176 | 2.893 s | Read durable outcome; zero writes |
| Retry permanent delete after lost response | 10,000 | 1 | 44 | $0.0000176 | 2.889 s | Read durable outcome; zero writes |
| Denied Trash with stale final item | 10,000 | 1 | 44 | $0.0000176 | 2.924 s | Validate closed snapshot; zero writes |
| Smart-upload size phase | 10,000 | 10 | 80 | $0.0000320 | 5.231 s | Metadata-only batches of 1,000 |
| Smart upload when every size is a candidate | 10,000 | 20 | 140 | $0.0000560 | 9.162 s | Hash phase only for candidates |
| Admit actual uploads | 10,000 | 100 | 20,700 | $0.1012000 | 60.995 s background | 10,000 necessary provider sessions; 100 batches |
| Complete actual uploaded objects | 10,000 | 10,000 | 100,000 | $0.1700000 | 68.534 s background | Only completed data transfers; 100-worker bound |
| Cancel active provider sessions | 10,000 | 10,000 | 90,000 | $0.1200000 | 60.575 s background | Only active sessions; control-pool bound |
| Resolve previews in a 10,000-item grid | 10,000 | 32 | 352 | $0.0001280 | 10.244 s background | Visible/overscan window only; two-worker bound |
| Checkpoint garbage collection | 128 | 0 | 163 | $0.0001014 | 2.860 s | One quiescent maintenance run |
| Domain compaction | 300 | 0 | 15 | $0.0000382 | 1.081 s | One bounded maintenance run |

The active-transfer totals deliberately remain linear: they represent 10,000
real provider sessions or completed objects, not 10,000 rendered/history rows.
The audit rejects moving that work onto page load or any other dormant state.
Their per-operation exact budgets remain visible separately by state/file/data
role, so future batching can tighten state overhead without hiding the distinct
provider object work.

## Required production-efficiency targets

The following are maximums for the replacement architecture. Schema 010 does
not meet them. A replacement is not complete until the real executable workload
events—not a fabricated target trace—meet or beat every count, modeled cost, and
critical-p95 value and append a tighter observed ratchet.

| Production state / intent | Current → target provider requests | Target browser requests | Target marginal cost (USD) | Target critical p95 |
|---|---:|---:|---:|---:|
| Restored transfer history without sources | 0 → 0 | 0 | $0 | 0 s |
| Browse live directory | 80 → 5 | 1 | $0.0000020 | 0.302 s |
| Browse Trash | 80 → 5 | 1 | $0.0000020 | 0.302 s |
| Copy selected roots | 128 → 5 | 1 | $0.0000112 | 0.398 s |
| Move selected roots | 129 → 5 | 1 | $0.0000112 | 0.398 s |
| Move selected roots to Trash | 127 → 5 | 1 | $0.0000112 | 0.398 s |
| Restore selected Trash roots | 167 → 5 | 1 | $0.0000112 | 0.398 s |
| Permanently delete selected Trash roots | 86 → 5 | 1 | $0.0000112 | 0.398 s |
| Retry Trash after lost response | 44 → 4 | 1 | $0.0000016 | 0.263 s |
| Retry restore after lost response | 44 → 4 | 1 | $0.0000016 | 0.263 s |
| Retry permanent delete after lost response | 44 → 4 | 1 | $0.0000016 | 0.263 s |
| Denied Trash with stale final item | 44 → 3 | 1 | $0.0000012 | 0.197 s |
| Smart-upload size phase | 80 → 5 | 1 | $0.0000020 | 0.302 s |
| Smart upload when every size is a candidate | 140 → 10 | 2 | $0.0000040 | 0.604 s |
| Admit actual uploads | 20,700 → 10,014 | 1 | $0.0500562 | 13.478 s background |
| Complete actual uploaded objects | 100,000 → 10,014 | 1 | $0.0040562 | 7.478 s background |
| Cancel active provider sessions | 90,000 → 10,014 | 1 | $0.0000562 | 7.478 s background |
| Resolve previews in a 10,000-item grid | 352 → 36 | 1 | $0.0000144 | 0.712 s background |
| Checkpoint garbage collection | 163 → 131 | 0 | $0.0000058 | 0.332 s |
| Domain compaction | 15 → 5 | 0 | $0.0000112 | 0.398 s |
| Move one directory with 1,000,000 descendants | unmeasured → 4 | 1 | $0.0000062 | 0.278 s |

These ceilings come from executable request-wave plans in
`providerbudget.ProductionScaleTargets`, evaluated by the same GCS model as the
observed ratchets. `TestProviderBudgetProductionScaleTargetsAreStrictlyBetterAndFeasible`
requires a target for every measured massive workload, requires every nonzero
target to improve count, cost, and critical latency, and emits
`provider-target-v1`. `TestProviderBudgetTargetPayloadEnvelopesAreMeasured`
records the byte evidence:

- a maximum accepted 1 MiB control intent plus transaction binding encodes to
  1,048,656 bytes, below one 4 MiB immutable transaction segment;
- 10,000 deliberately verbose file projection rows, including 255-byte names,
  encode to 6,050,001 bytes, below two 4 MiB projection segments; and
- 1,000 transfer progress rows with a conservative 2 KiB sealed lease each
  encode to 2,094,001 bytes, below one 4 MiB progress segment.

The target request formula is therefore based on encoded bytes, not item count:

```text
packed read     = 3 fixed authority/head reads
                  + ceil(encoded projection bytes / 4 MiB) parallel reads

metadata write  = 3 fixed authority/proof reads
                  + ceil(encoded compact intent bytes / 4 MiB) immutable writes
                  + 1 conditional visibility-head write

active transfer = one unavoidable provider object operation per active object
                  + 3 fixed reads
                  + max(ceil(progress bytes / 4 MiB), ceil(items / 1,000)) progress writes
                  + 1 terminal visibility-head write
```

For transfer workflows, the 1,000-item progress boundary limits the amount of
provider work that can become an unexposed orphan after a pod crash. A progress
segment is durable before its capabilities/results are streamed to the browser;
retry reads completed segments and continues from the first absent segment.
The ten progress writes are modeled serially, so the latency target does not
pretend they all complete in one wave. Sealed provider leases remain transient,
encrypted, bounded, and excluded from authoritative portability checkpoints.
The replacement adapter must reject or split a transient sealed lease that
exceeds the measured 2 KiB-per-item envelope; an oversized provider response
cannot silently invalidate the segment bound.

The remaining 10,000 active-transfer calls are an actual provider lower bound:
GCS requires a resumable-session initiation for each object, and completion or
revocation must verify or abort each corresponding object/session. GCS JSON API
batching can reduce HTTP connection overhead but each subrequest is still
counted and billed, and uploads/downloads are not supported by that batch API.
No target hides those calls as one synthetic request.

Metadata mutations use a compact immutable intent bound to the authenticated
base revision. Uniform success is reconstructed from the intent; only a compact
exception representation is retained. One conditional head replacement is the
visibility point. This removes the current immutable B-tree rewrite and paged
outcome amplification while preserving atomicity, idempotent replay,
lost-success recovery, and replica CAS convergence. Moving a directory retains
its child-root reference, so descendants contribute neither records nor
provider calls. Larger selections scale by encoded segment count and affected
namespace roots, never by one state transaction per logical item.

The target performs one write to a namespace visibility head for the complete
bulk mutation. It does not assume that many writes to the same GCS object can
run concurrently: GCS documents an approximately one-write-per-second limit for
replacing one object. Independent prepared intents can be coalesced behind one
head publication, while genuine conflicts still converge through the native
generation-match CAS. No provider batch request is counted as an atomic
multi-object transaction.

## Current schema-010 atomic restore design

Batch restore derives every original destination from trash metadata inside the
same closed owner-namespace snapshot used for publication. It validates all
10,000 sources, expected metadata, destination conflicts, and overlapping paths
before writing anything. It then removes all selected Trash edges, attaches all
live edges, clears Trash metadata, writes immutable outcome pages, and performs
one conditional namespace-head publication. A crash after the head succeeds is
recovered by the same idempotency key and returns the paged durable outcome.

Tests cover success, changed-intent replay denial, destination conflict with no
writes, lost-success replay, cross-owner denial, malformed and duplicate IDs,
and exact success/replay provider budgets. Permanent-delete already uses the
same single-publication batch discipline.

## Economics provenance and limitations

- Pricing source: <https://cloud.google.com/storage/pricing>, effective
  2026-08-24 in the fixture.
- GCS batch behavior and billing: <https://cloud.google.com/storage/docs/batch>.
- GCS request-rate guidance, including the same-object replacement limit:
  <https://cloud.google.com/storage/quotas>.
- Generation-match atomic preconditions:
  <https://cloud.google.com/storage/docs/request-preconditions>.
- Per-object resumable-session initiation:
  <https://cloud.google.com/storage/docs/resumable-uploads>.
- Latency sources and methodology are embedded in
  `internal/objectstore/gcs/economics/latency-regional-standard-flat-2026-08.json`.
- The price model excludes storage-at-rest, retrieval, replication, and egress;
  those are data-volume or infrastructure choices rather than request
  amplification.
- Aggregate modeled latency sums provider work. Critical-path estimates use
  only concurrency explicitly represented in request events or the production
  scale catalog.
