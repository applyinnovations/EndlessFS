# Provider request surface and scale audit — 2026-09-01

## Outcome

This audit closes the browser request-amplification incident and records the
complete production provider boundary. Provider-backed work is acceptable only
when its cardinality follows the data that must actually change:

- dormant device-local UI state performs zero provider work;
- metadata-only operations over many selected roots use one HTTP mutation and
  one atomic namespace publication, with immutable page work proportional to
  the touched tree pages rather than one transaction per item;
- directory reads paginate at the provider's maximum application page size;
- previews are bounded by the virtualized visible/overscan window rather than
  logical directory size; and
- linear transfer work is permitted only for distinct active provider upload,
  download, or abort sessions that necessarily correspond to distinct object
  operations. Its concurrency remains bounded by the server-advertised worker
  pool.

These are structural rules, not guessed numerical targets. The exact observed
request, price, and latency values are then frozen by append-only ratchets so a
future implementation can tighten them but cannot silently spend more.

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

## Massive-workload evidence

Costs use the reviewed GCS Regional Standard Storage flat-namespace marginal
pricing fixture, excluding free tier and negotiated discounts. Latency is a
conservative engineering model, not a Google Cloud SLA. “Critical p95” accounts
for the explicitly proven immutable-page or worker-pool parallel groups;
“aggregate p95” remains available in the emitted `provider-scale-v1` evidence.

| Production state / intent | Logical items | Browser requests | Provider requests | Modeled marginal cost (USD) | Critical p95 | Scaling basis |
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

## Atomic restore design

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
- Latency sources and methodology are embedded in
  `internal/objectstore/gcs/economics/latency-regional-standard-flat-2026-08.json`.
- The price model excludes storage-at-rest, retrieval, replication, and egress;
  those are data-volume or infrastructure choices rather than request
  amplification.
- Aggregate modeled latency sums provider work. Critical-path estimates use
  only concurrency explicitly represented in request events or the production
  scale catalog.
