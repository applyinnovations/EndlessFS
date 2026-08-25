# Provider economics and mutation budgets

EndlessFS treats provider traffic as a tested architectural boundary. The GCS
adapter owns versioned fixtures for request pricing, conservative latency
estimates, and an append-only per-operation ratchet under
`internal/objectstore/gcs/economics`. Tests fail when a storage pathway adds an
unclassified request, exceeds its count/cost/latency ceiling, uses a provider
role that was expected to remain untouched, or becomes cheaper without adding
a new tighter ratchet epoch.

Run the deterministic gate with:

```text
nix run .#test-provider-budget
```

## What is measured

The wire-level GCS suite instruments the pinned client transport. It verifies
the actual HTTP emitted for object metadata, reads, lists, writes, deletes,
rewrites, resumable-upload control, direct browser upload, and direct browser
download. Unknown protocol shapes fail closed.

The application-level suites instrument each thin object-store role and measure
complete state, file, data-plane, and durable-preview pathways. The schema-009
delta records 90 exact workloads covering single- and multi-record state,
identity/administration/session mutations, themes, shares, uploads, namespace
reads and mutations, duplicates, preview storage, startup, transition recovery,
compaction, checkpoint creation/verification/reopen, checkpoint garbage
collection, and the 008→009 migration. It includes 10,000-root namespace
batches and a 100-capability upload batch. The production catalog separately
proves that every provider-facing interface method and every registered HTTP
route maps to these executable workloads, and that no current ratchet is an
orphan.
A zero ceiling on the file role for namespace operations proves that logical
moves, copies, Trash, and restore never copy or relocate stored file bytes.

Each event carries its provider role, request kind, byte counts, status, and a
test-only target for diagnostics. Costs use integer pico-US dollars so the gate
has no floating-point or network dependency. Aggregate latency totals are the
sum of reviewed per-request estimates plus a byte term. The budget also records
p50, p95, and p99 critical-path ceilings. Serial operations have identical
aggregate and critical ceilings; explicitly independent bounded page-write
waves collapse only in the critical model. This makes parallelism visible
without pretending that work disappeared.

## GCS model provenance and limits

Pricing is the published regional Standard Storage flat-namespace list price
from [Cloud Storage pricing](https://cloud.google.com/storage/pricing), without
free-tier allowances, negotiated discounts, storage-at-rest, retrieval, or
egress charges. The fixture assigns a JSON API resumable upload's one Class A
charge to session initiation and assigns no extra operation price to its data
continuations. Deletes and local capability signing are zero-cost; zero-cost
requests still count and carry latency where they contact GCS. The cost model
is deliberately an upper bound: it charges other observed wire requests even
where Google's general failed-response waiver or single-operation billing for
a multi-request rewrite may make the invoice lower. Request counts always
remain wire-level.

Google Cloud does not publish per-method p50/p95/p99 guarantees. The committed
latency values are therefore conservative engineering estimates, not measured
Google percentiles or an SLA. Their assumptions and limitations are embedded
in the fixture, informed by Google's documentation for
[request rates](https://docs.cloud.google.com/storage/docs/request-rate),
[best practices](https://docs.cloud.google.com/storage/docs/best-practices),
[latency troubleshooting](https://docs.cloud.google.com/storage/docs/troubleshooting),
and [Colossus data placement](https://cloud.google.com/blog/products/storage-data-transfer/how-colossus-optimizes-data-placement-for-performance).
Production SLOs still require measurements from the deployed region and
workload.

## Ratchet law

The economics ratchet is append-only. Every materialized epoch retains every
existing pathway and may only retain or lower every overall and per-role count,
cost, p50, p95, p99, critical-p50, critical-p95, and critical-p99 ceiling. An
on-disk sparse delta inherits omitted budgets unchanged; changed existing
budgets must keep the exact role set and tighten every metric. It cannot remove
a pathway or loosen one metric.

Operation tests require exact calibration, not merely `observed <= maximum`.
When an optimization reduces any metric, the test fails with an instruction to
append a tighter epoch. This prevents old headroom from silently becoming the
budget for a future regression. Pricing or latency research changes use a new
versioned economics profile or fixture review; they are not disguised as an
application efficiency improvement.

## Schema-009 mutation architecture

The first economics epoch records the schema-007 baseline. The second epoch
records the schema-008 owner-namespace replacement. The sparse third epoch
records schema 009 without duplicating unchanged prior budgets. Schema 009
retains the persistent high-fan-out namespace graph, repartitions application
state by real invariant, binds every value to a canonical record type, adds
single-domain atomic mutation and a helpable cross-domain decision protocol,
and integrates resumable checkpoint garbage collection. An ordinary
single-domain mutation still writes changed immutable pages and conditionally
replaces one domain head with its retained outcome. There is no
candidate/admission transaction, mutable per-file operation record, fence
lease, or synchronous duplicate-projection update on the successful path.

Examples from the exact ratchet are: one-file move falls from 72 to 4 provider
requests, Trash from 100 to 5, restore from 103 to 4, upload completion from 68
to 6, and StateStore CAS from 18 to 2. Their modeled p95 aggregate latencies
fall from 5.037 s to 290 ms, 6.997 s to 370 ms, 7.102 s to 275 ms, 5.136 s to
410.065 ms, and 1.240 s to 145 ms. The historical comparison remains in
[`storage-schema-008-implementation.md`](./storage-schema-008-implementation.md).
The current architecture, migration, checkpoint collector, and measured table
are in
[`storage-schema-009-implementation.md`](./storage-schema-009-implementation.md).

The executable 10,000-file Trash fixture performs exactly 125 state-provider
requests, one authoritative head write, no file-provider request, and no stored
body read. Its modeled GCS request cost is $0.0004318; aggregate p95 work is
9.546648 s while its bounded 32-worker critical p95 is 3.115983 s. These are
checked-in engineering estimates, not a live GCS benchmark or SLA.

The 100-upload schema-009 fixture performs 205 total requests: two batched
authoritative namespace mutations plus the unavoidable independent provider
session/lease work for each object. Its modeled aggregate p95 is 20.359904 s,
while its explicitly parallel critical p95 is 479.804 ms. A 128-object
checkpoint garbage sweep performs 163 requests, costs $0.0001014, reads no file
body, and has a modeled 2.860128 s critical p95. The full exact table is the
versioned GCS schema-009 fixture under `internal/objectstore/gcs/economics`.
