# EndlessFS comprehensive storage-architecture economics

**Date:** 2026-08-24
**Status:** executable architecture-selection evidence; no production schema,
migration, writer, or release change is authorized by this report

## Conclusion

The evidence supports continuing with the proposed consistency-domain,
persistent-page, bounded-delta architecture. The result is not just a faster
Trash implementation: the same representational change removes amplification
from identity, sessions, control records, shares, duplicate projections,
administrative views, batches, startup, and checkpoint critical paths.

The strongest directly measured comparisons are:

| Provider-boundary use case | Current | Prototype | Request reduction | Current cost | Prototype cost | Current modeled p95 | Prototype modeled critical p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Bootstrap verification | 211 | 12 | 94.3% | $0.0003556 | $0.0000416 | 14.556 s | 450 ms |
| Admin user page, 128 users | 1,296 | 4 | 99.7% | $0.0005184 | $0.0000016 | 84.253 s | 260 ms |
| Trash, one root | 100 | 5 | 95.0% | $0.0001952 | $0.0000158 | 6.997 s | 370 ms |
| Trash, 100 selected roots | 12,891 | 6 | 99.95% | $0.0210932 | $0.0000208 | 890.044 s | 450 ms |
| Duplicate overlap query | 281 | 2 | 99.3% | $0.0001860 | $0.0000008 | 18.828 s | 130 ms |
| Apply one-file duplicate reconciliation | 606 | 7 | 98.8% | $0.0010140 | $0.0000212 | 42.678 s | 515 ms |
| Complete upload control plane | 68 | 7 | 89.7% | $0.0001448 | $0.0000162 | 5.136 s | 490 ms |
| Warm startup | 9 | 1 | 88.9% | $0.0000174 | $0.0000004 | 630 ms | 65 ms |
| Checkpoint, 128 domains/records fixture | 825 | 391 | 52.6% | $0.0009460 | $0.0007544 | 54.822 s | 565 ms |

These costs are marginal request charges under the checked-in GCS regional
Standard flat-namespace fixture. They are fractions of a US dollar, not dollar
amounts. Storage-at-rest, retrieval, egress, replication, free tiers, taxes,
and negotiated discounts are excluded.

No weighted “overall score” is reported. Assigning every pathway the same
frequency would be arbitrary, while assigning production frequencies requires
production telemetry that EndlessFS deliberately does not collect. The full
vector is presented so a workload mix can be applied explicitly later.

## What was benchmarked

The executable catalog classifies all 57 application/provider workloads and all
61 HTTP routes that can reach one of them. Every current runtime workload
family is executed before and after except schema migration: its “after” path
does not exist until an architecture and canonical format are approved.
Reflection tests cover every method of `state.Store`, `provider.Storage`,
`provider.DuplicateStorage`, and `preview.Store`. A newly added interface method
or provider-backed HTTP route must be classified before this evidence remains
complete.

The local-only routes are health/readiness, embedded configuration and app
shells, the public-share shell, built-in theme metadata, and embedded theme
assets. They issue no provider request and are excluded from the numeric
tables. Direct capability data transfer is included separately.

Measurements use a deterministic in-memory object backend wrapped by the same
request ledger and the checked-in GCS economics model used by the production
ratchets. Thus:

- request kind, role, target, failures, request bytes, and response bytes are
  observed from executable code;
- cost is calculated from the reviewed GCS request-price fixture;
- “serial p95” is the sum of the fixture's per-request engineering estimates;
- “critical p95” collapses only explicitly declared independent page/domain
  phases; and
- neither latency value is a Google SLA or a live-network benchmark.

“Current” means either the real portable engine/use case under instrumentation
or its exact checked-in ratchet. “Prototype” means executable code under
`internal/architecturelab`. “Composed HTTP” adds independently measured session
authentication to the measured service path. “Extrapolated” is used only where
the current API limit prevents running the requested cardinality.

## Architecture exercised by the prototype

The selected family has five distinct shapes rather than forcing every record
through one generic transaction:

1. Each real consistency domain has one small conditional head. Owner
   namespace, owner control, administration, capability, and share invariants
   are separate domains.
2. Ordinary namespace changes publish a bounded delta with one head CAS.
   Compacted state is a structurally shared, content-addressed, high-fan-out
   tree. A directory copy shares immutable pages and forks only a changed path.
3. Large explicit selections are immutable batch-delta trees. Preparation
   reads/writes pages by tree level and one head CAS publishes the whole
   selection atomically.
4. Truly independent records, especially sessions and ephemeral ceremonies,
   use direct conditional objects. Cross-domain invariants use a durable
   prepare/decision/finalize protocol only when they actually cross domains.
5. Duplicates, admin-user pages, secondary sort/accounting views, search, and
   similar indexes are immutable checkpoint-bound projections. Foreground
   namespace commits do not synchronously rewrite them.

The head CAS or multi-domain decision object is the visibility point. Prepared
immutable pages and claims are unreachable or recoverable garbage. File blobs
are referenced in place: logical copy, move, Trash, restore, duplicate cleanup,
checkpoint, and metadata migration issue zero file-provider copy requests and
never read file bodies through the Go service.

## State and bounded control records

| Operation | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Create | 12 | 4 | $0.0000274 | $0.0000154 | 850 ms | 305 ms |
| Read | 4 | 1 | $0.0000016 | $0.0000004 | 260 ms | 65 ms |
| List one bounded record | 5 | 1 | $0.0000020 | $0.0000004 | 325 ms | 65 ms |
| Update/CAS | 18 | 4 | $0.0000298 | $0.0000154 | 1.240 s | 305 ms |
| Delete | 14 | 4 | $0.0000190 | $0.0000154 | 950 ms | 305 ms |

The four-request mutation is head GET, durable fingerprint-bound claim PUT,
head CAS, and claim finalization PUT. A naturally growing collection is paged
behind the head instead of allowing the head to grow without bound.

The executable multi-domain protocol costs `2 + 3D` requests for `D` touched
domains: immutable operation plan, parallel domain reads, parallel prepares,
one decision CAS, and parallel finalization.

| Touched domains | Requests | Cost | Serial p95 | Critical p95 |
| ---: | ---: | ---: | ---: | ---: |
| 2 | 8 | $0.0000308 | 610 ms | 385 ms |
| 3 | 11 | $0.0000412 | 835 ms | 385 ms |
| 8 | 26 | $0.0000932 | 1.960 s | 385 ms |

Crash tests prove old visibility after a prepared-only crash, new visibility
after the decision, and idempotent recovery/finalization in both cases.

## Namespace and transfer control plane

The following are service/provider boundary values; browser authentication is
shown in the next table.

| Operation | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Create directory | 76 | 4 | $0.0001542 | $0.0000154 | 5.831 s | 305 ms |
| Copy one root | 47 | 5 | $0.0000782 | $0.0000158 | 3.246 s | 370 ms |
| Move one root | 72 | 5 | $0.0001388 | $0.0000158 | 5.037 s | 370 ms |
| Trash one root | 100 | 5 | $0.0001952 | $0.0000158 | 6.997 s | 370 ms |
| Restore one root | 103 | 5 | $0.0001688 | $0.0000158 | 7.102 s | 370 ms |
| Permanently delete one root | 40 | 5 | $0.0000570 | $0.0000158 | 2.731 s | 370 ms |
| Stat/list/batched child lookup, one-level page | 4–5 | 2 | $0.0000016–$0.0000020 | $0.0000008 | 260–325 ms | 130 ms |
| Latest operation result | 1 | 1 | $0.0000004 | $0.0000004 | 65 ms | 65 ms |
| Create upload | 12 | 5 | $0.0000320 | $0.0000204 | 905 ms | 425 ms |
| Upload status | 3 | 2 | $0.0000008 | $0.0000004 | 190 ms | 125 ms |
| Complete upload | 68 | 7 | $0.0001448 | $0.0000162 | 5.136 s | 490 ms |
| Abort upload | 10 | 6 | $0.0000166 | $0.0000158 | 680 ms | 430 ms |
| Create download | 6 | 4 | $0.0000020 | $0.0000012 | 320 ms | 190 ms |

Direct upload still requires one provider upload request, and direct download
still requires one provider data request. File bytes go browser-to-provider and
provider-to-browser. Upload completion uses provider-attested metadata.

### Composed authenticated HTTP examples

Current authentication is 8 provider requests, $0.0000032, and 520 ms modeled
p95. The prototype performs the session and owner-auth-generation reads in
parallel: 2 requests, $0.0000008, 130 ms serial / 65 ms critical p95.

| HTTP workflow | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype critical p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Create directory | 84 | 6 | $0.0001574 | $0.0000162 | 6.351 s | 370 ms |
| Copy one root | 55 | 7 | $0.0000814 | $0.0000166 | 3.766 s | 435 ms |
| Move one root | 80 | 7 | $0.0001420 | $0.0000166 | 5.557 s | 435 ms |
| Trash one root | 108 | 7 | $0.0001984 | $0.0000166 | 7.517 s | 435 ms |
| Restore one root | 111 | 7 | $0.0001720 | $0.0000166 | 7.622 s | 435 ms |
| Permanent delete one root | 48 | 7 | $0.0000602 | $0.0000166 | 3.251 s | 435 ms |
| Complete upload | 76 | 9 | $0.0001480 | $0.0000170 | 5.656 s | 555 ms |
| List one directory page | 13 | 4 | $0.0000052 | $0.0000016 | 845 ms | 195 ms |
| Stat one-level path | 12 | 4 | $0.0000048 | $0.0000016 | 780 ms | 195 ms |
| Storage map with one expanded directory | 21 | 5 | $0.0000084 | $0.0000020 | 1.365 s | 260 ms |
| Trash page with one item | 17 | 4 | $0.0000068 | $0.0000016 | 1.105 s | 195 ms |

A directory containing 10,000 descendants remains one root mutation. Neither
side performs one request per descendant. The large-selection results below are
for 10,000 independently selected roots, which is a different workload.

## Batch scaling

The current HTTP API accepts at most 100 selected roots and runs one complete
storage transaction per item. The prototype emits one or two compact changes
per selected root into immutable pages, then publishes one root.

### Directly measured 100-item service paths

| Batch | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype critical p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Copy 100 roots | 6,234 | 6 | $0.0103514 | $0.0000208 | 431.020 s | 450 ms |
| Move 100 roots | 8,614 | 6 | $0.0149098 | $0.0000208 | 597.589 s | 450 ms |
| Trash 100 roots | 12,891 | 6 | $0.0210932 | $0.0000208 | 890.044 s | 450 ms |
| Permanently delete 100 Trash roots | 12,291 | 6 | $0.0191802 | $0.0000208 | 844.918 s | 450 ms |
| Create 100 upload capabilities | 1,200 | 104 | $0.0032000 | $0.0005154 | 90.508 s | 425 ms |

Upload initiation retains one unavoidable provider upload-session request per
object. Those 100 requests are one parallel phase, while their application
records are committed in one bounded control-domain mutation.

### Executed 10,000-item prototype and current extrapolation

The current column is `100 ×` the directly measured 100-item service path,
because the current public contract cannot accept 10,000 items. It is not
presented as a direct execution. Prototype values are directly executed.

| Batch | Current extrapolated requests | Prototype requests | Current extrapolated cost | Prototype cost | Current serial p95 | Prototype critical p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Copy 10,000 roots | 623,400 | 86 | $1.03514 | $0.0002368 | 11.97 h | 596 ms |
| Move 10,000 roots | 861,400 | 125 | $1.49098 | $0.0004318 | 16.60 h | 596 ms |
| Trash 10,000 roots | 1,289,100 | 125 | $2.10932 | $0.0004318 | 24.72 h | 596 ms |
| Restore 10,000 roots | no current batch route | 125 | n/a | $0.0004318 | n/a | 596 ms |
| Delete 10,000 roots | 1,229,100 | 86 | $1.91802 | $0.0002368 | 23.47 h | 596 ms |

The 10,000-item move/trash fixture transfers 2.99 MB of request bodies and
1.67 MB of response bodies. Copy transfers 2.68/1.67 MB; delete transfers
1.09/1.67 MB. The count slope is tree pages, not selected items.

The critical latency assumes all independent calls at one tree level can share
a phase. With a conservative concurrency cap of 32, the 40-page base read and
79-page write need two and three waves, adding about 225 ms to the modeled
10,000-item move/trash result. A cap of 16 adds about 450 ms. This sensitivity
still puts the provider portion near one second rather than hours.

## Duplicate reconciliation

| Operation | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| List groups | 13 | 2 | $0.0000144 | $0.0000008 | 915 ms | 130 ms |
| List occurrences | 3 | 2 | $0.0000058 | $0.0000008 | 230 ms | 130 ms |
| Ignore/unignore group | 7 | 4 | $0.0000162 | $0.0000154 | 495 ms | 305 ms |
| Compare directories | 13 | 2 | $0.0000052 | $0.0000008 | 845 ms | 130 ms |
| List directory overlaps | 281 | 2 | $0.0001860 | $0.0000008 | 18.828 s | 130 ms |
| Ignore/unignore directory pair | 20 | 4 | $0.0000214 | $0.0000154 | 1.340 s | 305 ms |
| Create reconciliation preview | 15 | 3 | $0.0000060 | $0.0000058 | 975 ms | 210 ms |
| Validate plan | 26 | 2 | $0.0000104 | $0.0000008 | 1.690 s | 130 ms |
| Apply one-file plan | 606 | 7 | $0.0010140 | $0.0000212 | 42.678 s | 515 ms |

The projection head is bound to an authoritative namespace revision. A plan is
immutable, and apply revalidates its namespace revision before the batch head
CAS. Ignore preferences remain authoritative owner-control state. A missing or
corrupt projection can be rebuilt without blocking unrelated mutations.

## Identity, sessions, and administration

| Operation | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype critical p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Bootstrap options | 16 | 2 | $0.0000290 | $0.0000054 | 1.110 s | 145 ms |
| Bootstrap verification | 211 | 12 | $0.0003556 | $0.0000416 | 14.556 s | 450 ms |
| Authentication options | 18 | 1 | $0.0000298 | $0.0000050 | 1.240 s | 80 ms |
| Authentication verification + issue session | 80 | 10 | $0.0000998 | $0.0000362 | 5.410 s | 530 ms |
| Issue session | 12 | 1 | $0.0000274 | $0.0000050 | 850 ms | 80 ms |
| Authenticate session | 8 | 2 | $0.0000032 | $0.0000008 | 520 ms | 65 ms critical |
| Rotate session | 26 | 2 | $0.0000464 | $0.0000050 | 1.800 s | 140 ms |
| Logout | 14 | 1 | $0.0000190 | $0 | 950 ms | 60 ms |
| Revoke one user's sessions, one live session | 19 | 4 | $0.0000210 | $0.0000154 | 1.275 s | 305 ms |
| Admin user page, one user | 21 | 4 | $0.0000084 | $0.0000016 | 1.365 s | 260 ms |
| Admin user page, 128 users | 1,296 | 4 | $0.0005184 | $0.0000016 | 84.253 s | 260 ms |

The current admin page repeatedly fetches account and role state per returned
profile. The prototype reads owner/admin authority and one immutable projection
page. Disable/grant/revoke operations still use the multi-domain decision
protocol and an owner auth-generation increment; they do not scan sessions.

Invited registration and recovery use the same measured two-domain protocol as
capability consumption plus owner creation (8 protocol requests, before any
route-specific pre-read). Bootstrap is the measured three-domain case because
it also establishes administration state. No unimplemented identity route is
being assigned an invented fixed total; the route-to-primitive mapping is in
the coverage appendix.

## Shares

These service values exclude private-route session authentication; public
share paths have no session.

| Operation | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Create share | 19 | 6 | $0.0000302 | $0.0000162 | 1.305 s | 435 ms |
| List shares | 5 | 1 | $0.0000020 | $0.0000004 | 325 ms | 65 ms |
| Public directory list | 22 | 4 | $0.0000088 | $0.0000016 | 1.430 s | 260 ms |
| Public stat | 21 | 4 | $0.0000084 | $0.0000016 | 1.365 s | 260 ms |
| Public download capability | 25 | 6 | $0.0000096 | $0.0000020 | 1.555 s | 320 ms |
| Revoke share | 27 | 4 | $0.0000334 | $0.0000154 | 1.825 s | 305 ms |

Public list/stat use one request-local namespace revision and page cache, so
the root authority is not fetched again after the same-revision proof.

## Preview and data plane

The independent preview artifact store is intentionally unchanged by the
state redesign. These are therefore both before and after values.

| Preview store operation | Requests | Cost | Modeled p95 |
| --- | ---: | ---: | ---: |
| Full startup validation, including one capability download | 25 | $0.0000314 | 1.650 s |
| Check | 1 | $0.0000050 | 100 ms |
| Claim | 2 | $0.0000054 | 145 ms |
| Release | 2 | $0.0000054 | 145 ms |
| Commit | 5 | $0.0000158 | 370 ms |
| Latest | 3 | $0.0000012 | 190 ms |
| Read artifact | 3 | $0.0000012 | 195 ms |
| Create artifact download | 4 | $0.0000012 | 190 ms |

Preview resolution/generation routes compose session, namespace, control, and
the store rows above. They retain the explicit optional-feature exemption that
allows source image bytes to flow through the preview generator. No ordinary
state/file path receives that exemption.

| Direct data operation | Requests | Cost | Modeled p95 |
| --- | ---: | ---: | ---: |
| Four-byte upload | 1 | $0 | 100 ms |
| Four-byte download | 1 | $0.0000004 | 100 ms |

Payload-size latency, retrieval, and egress depend on bytes and location and
are not attributed to metadata architecture.

## Startup, recovery, checkpoint, compaction, and garbage

| Maintenance operation | Current requests | Prototype requests | Current cost | Prototype cost | Current p95 | Prototype critical p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Warm startup/domain validation | 9 | 1 | $0.0000174 | $0.0000004 | 630 ms | 65 ms |
| Idempotent replay/lost-success recovery | 7 | 4 | $0.0000028 | $0.0000108 | 455 ms | 290 ms |
| Checkpoint, one record/domain | 49 | 7 | $0.0001656 | $0.0000166 | 4.030 s | 500 ms |
| Checkpoint, 128 records/domains | 825 | 391 | $0.0009460 | $0.0007544 | 54.822 s | 565 ms |
| Compact 32 namespace deltas | folded into current mutations | 4 | folded into current rows | $0.0000108 | n/a | 290 ms |
| Rebuild one derived-view page | synchronous current indexes | 2 | folded into current mutations | $0.0000100 | n/a | 160 ms |
| Sweep 128 unreachable pages | folded into current checkpoint | 131 | folded into current checkpoint | $0.0000100 | n/a | 160 ms critical |

The recovery cost row compares a normal current idempotent replay with the
stronger prototype lost-success path; request count and latency improve, while
the prototype pays Class A writes to durably reconcile the claim. This is a
real cost tradeoff, not hidden by a score.

The checkpoint now verifies the complete recursive closure: catalog pages,
domain roots, B-tree children, and directory-introduced page roots. The 128
domain run has 27.367 seconds of serial provider work, but independent domain
read/freeze/closure phases produce a 565 ms unconstrained critical estimate.
A concurrency cap of 32 raises that estimate to roughly 1.2 seconds.

Garbage collection starts from a retained full-closure checkpoint and sweeps
only immutable page namespaces. The 131 calls are two billed inventory LISTs
plus 129 free conditional DELETEs (128 synthetic pages and one superseded
catalog page). File-blob collection remains subject to Trash retention,
checkpoint roots, derived-view watermarks, and migration/cutover roots.

## Route coverage appendix

The 61 provider-backed routes reduce to the measured primitives as follows.
The executable catalog retains the exact per-route list; this grouping is for
readability.

| Routes | Provider plan |
| --- | --- |
| Bootstrap, registration, recovery options/verify | Ceremony/control read plus direct ceremony creation; verification uses the two- or three-domain decision protocol |
| Authentication options/verify | Direct ceremony creation; ceremony/owner decision plus direct session issue |
| Logout, `/me`, display-name, passkeys | Session authentication plus session or owner-control operations; passkey verify also rotates the session |
| Admin invites/recoveries | Session/admin authority plus bounded capability-domain read/list/mutation |
| Admin users and enable/disable/admin role mutations | Session/admin authority; derived admin projection for reads; multi-domain admin/owner decision and auth-generation invalidation for cross-domain mutations |
| Files list/stat/storage map/Trash list | Session authentication plus shared-head paged namespace reads |
| Directory, copy, move, Trash, restore, permanent delete, empty Trash | Session authentication plus one ordinary or paged-batch namespace publication |
| Upload create/batch/status/complete/abort | Session authentication plus upload-domain state and the unavoidable direct-transfer provider operation; completion also publishes one namespace delta |
| Download | Session or public-share authority, namespace proof, file metadata HEAD, and local capability signing |
| Duplicate groups/occurrences/compare/overlaps | Session authentication plus immutable derived-view head/page |
| Duplicate ignore and reconciliation | Session plus owner-control mutation, immutable plan, namespace revision validation, and atomic batch publication |
| Shares private list/create/revoke | Session authentication plus owner share-domain operations and, for create, one namespace proof |
| Public share list/stat/download | Share authority plus a shared-revision namespace page/stat; download adds file metadata and signing |
| Preview resolve/generate/operation | Session, namespace proof, bounded operation state, and the independent preview-store primitives |
| Theme preference read/write | Session plus owner-control read/mutation |

The seven local-only routes are `/healthz`, `/readyz`, `/api/v1/config`, the
embedded app shell, the public-share shell, built-in theme metadata, and
embedded theme assets.

## What the evidence does not authorize

This branch contains no production schema epoch, migration ledger entry,
startup cutover, rollback implementation, production adapter change, release,
or deployment. The prototype constants are not proposed specification values.

Before a production implementation PR is mergeable, the approved architecture
still needs:

- a normative canonical format derived from provider/object-size and
  sensitivity evidence, not copied from prototype constants;
- complete deterministic multi-replica schedules across head CAS,
  multi-domain decision, compaction, batch publication, checkpoint, and sweep;
- authenticated cursor/read-proof grammar and key-rotation behavior;
- bounded derived-view lag, fallback, rebuild, and watermark policy;
- file-blob reachability/retention collection integrated with checkpoints and
  Trash policy;
- an adjacent, resumable, metadata-only schema transformation from every
  supported predecessor fixture, plus predecessor-produced interruption
  residues; and
- cutover/rollback evidence under the repository's migration law.

Migration has deliberately not been assigned a fabricated “after” budget. Its
page/range shape depends on the approved canonical format and the real fixture
inventory. The migration benchmark belongs in the schema PR, starting from
every immutable predecessor profile, and must report metadata requests and
bytes without reading file bodies.

## Reproduce

```text
nix develop -c go test -v ./internal/architecturelab
```

The coverage catalogs are `production_workloads.go` and
`production_routes.go`; the before/after tests are the `*_economics_test.go`,
batch, duplicate, catalog, recovery, and scale tests in the same package. The
GCS price and latency sources are the checked-in fixtures under
`internal/objectstore/gcs/economics`.
