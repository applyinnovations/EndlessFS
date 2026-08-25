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
complete state, file, data-plane, and durable-preview pathways. The current
ratchet covers StateStore create/read/list/CAS/delete; upload create/status/
completion/abort; download capability creation; directory creation; file copy,
move, delete, trash, and restore; direct data upload/download; and durable
preview check/claim/commit/read/download/release. A zero ceiling on the file
role for namespace operations proves that logical moves, copies, trash, and
restore never copy or relocate stored file bytes.

Each event carries its provider role, request kind, byte counts, status, and a
test-only target for diagnostics. Costs use integer pico-US dollars so the gate
has no floating-point or network dependency. Latency totals are the sum of
reviewed per-request estimates plus a byte term and represent a conservative
serial critical path.

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

The economics ratchet is append-only. A later epoch must carry every existing
pathway and may only retain or lower every overall and per-role count, cost,
p50, p95, and p99 ceiling. It cannot remove a pathway or loosen one metric.

Operation tests require exact calibration, not merely `observed <= maximum`.
When an optimization reduces any metric, the test fails with an instruction to
append a tighter epoch. This prevents old headroom from silently becoming the
budget for a future regression. Pricing or latency research changes use a new
versioned economics profile or fixture review; they are not disguised as an
application efficiency improvement.

## Namespace mutation architecture

Schema 007 makes two bounded changes to the ordinary mutation path:

- Small operation plans are reduced in memory and encoded once into the durable
  admitted operation. This avoids the prior external-sort run objects,
  preparation header/page objects, and their repeated provider reads. Plans are
  capped at 64 preparation items and the canonical record limit. Larger plans
  automatically use the existing crash-resumable paged preparation path.
- Live and trash area roots are navigation containers, not user-addressable
  duplicate-directory candidates. Schema 007 stops rewriting their 16 MinHash
  similarity postings on every namespace change. Schema-006 root postings are
  immutable compatibility residue and schema-007 readers ignore them.

Both paths still use the canonical distributed write gate, durable admission,
operation fencing, conditional visibility roots, and recovery validation.
They perform no file-provider copy or delete. Request count remains independent
of subtree descendants, and the existing namespace-cost test compares one and
128 descendant files.

The first ratchet is intentionally an honest baseline, not a target. For
example, direct move currently records 72 provider operations and a modeled
p95 serial critical path of about 5.04 seconds; the Drive trash wrapper records
100 operations. Those fixed costs are substantially smaller than the prior
preparation path but remain candidates for future schema work. The ratchet
makes that debt explicit and prevents it from growing while later epochs move
visibility and derived-index publication toward fewer packed transactions.
