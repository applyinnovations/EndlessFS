# Upload recovery corrective acceptance

Candidate base: deployed production source
`12908dd820ec0afa404b1ed2c87d2e4886902aee` (PR 61).
This is a fresh checkout and engineering session on `xlabs`, branch
`codex/upload-recovery-corrective`. The revoked session was not reused. All
fixtures are synthetic. Investigation used read-only operational metadata;
no customer file body or customer queue was opened or mutated. Deployment
health and endpoint-only tests are not upload acceptance.

## Incident evidence and precise uncertainty

Read-only Loki queries on 2026-10-17 confirmed:

| UTC completion | Request reference | Route | Status | Duration |
| --- | --- | --- | --- | --- |
| 2026-10-17 13:35:03.975 | evkrwXFtnjgltCIY | POST /api/v1/uploads/plan/sizes | 400 | 43.308601085 s |
| 2026-10-17 13:52:10.674 | mCiGkyMaenSBt5QY | POST /api/v1/uploads/plan/sizes | 400 | 43.425942462 s |

The first correlated trace in Loki is
`f639a73ef118d02f3de71db54f2e9165`; the handoff contained an extra `d`.
The second is `8cc9a03e019955cdb450f6878f1f73f3`. Tempo returned empty
traces for these IDs during investigation. Historical request logs retain only
`client_error`, not the underlying stage/reason. The reported 329,109-byte JSON
request is below the unchanged 1 MiB limit. No body was inspected. These logs
cannot retrospectively establish the exact production cause. The synthetic
failures below demonstrate concrete defects in the deployed implementation;
they do not prove which defect affected that customer's metadata.

## Regression-first diagnosis and selected design

| Real portable implementation workload | Before correction | Corrected proof |
| --- | --- | --- |
| 7,966 stored short-name files; 2,638 size queries | PASS, 2.601 s, five state calls | Cardinality alone is insufficient to reproduce the incident. |
| 7,966 stored files with valid 231-byte names; 2,638 size queries | FAIL, 4.008 s: expanded page pack exceeds limit | Successful real cold planning with unchanged size/integrity limits. |
| Build a 256-file projection; add a lexically later file; remove only its head; rebuild retaining immutable packs | FAIL, 0.12 s: physical pack identity collision | Complete intent binding prevents aliasing; independent replica can read the published projection. |
| Valid deep paths with a final projection closure exceeding 32 MiB expanded | One-pack publication cannot represent the closure | Bounded multiple compressed packs; authenticated reopen and warm lookups. |
| Real browser recovery after 7,966 completed files | Transferred bytes but stopped at 8,378 complete; finalization invalid | Authoritative materialization capacity recovery preserves prepared children and the original owner-head CAS. Full journey acceptance below. |
| Durable pack PUT with lost response, then retry after a 1 ms clock advance | Public admission fails with an immutable pack identity collision; no capability is returned | Bind exact preparation time and replay deadline before staging; retry and independent-replica replay return one native session and complete actual bytes. |
| Durable compaction pack PUT with lost response, then retry crossing an outcome expiry | Changed expiry pruning aliases the earlier physical pack | Pin the pruning cutoff to preparation, bind it into physical identity, and verify the deadline before publication. |

The builder previously retained sorted runs and merge intermediates alongside
final pages. It now prunes only unreachable staged projection pages, authenticates
full references including bounds/counts/filter, and binds physical identity to
the complete current/prior source and projection roots. Existing immutable pack
references remain unchanged. If the valid final closure still exceeds one pack,
only the specific capacity sentinel permits repacking into multiple compressed
packs. Every actual encoding checks the unchanged 4 MiB wire and 32 MiB expanded
bounds. All packs become durable before the original conditional head update.
Corruption, collision, cancellation and provider failure cannot select a weaker
encoding or bypass validation.

The real browser acceptance exposed a second capacity boundary during completion:
the materialized authoritative tree and its pre-staged opaque child references
could no longer fit one pack. The correction publishes the prepared dependency
set under its original descriptor, then packs the materialized Base, Outcomes
and OutcomeExpiry trees separately. Opaque leaf values remain byte-identical.
The original head CAS, expected revision, freeze validation and outcome receipt
remain the only authority/visibility point. An oversized prepared dependency set
itself still fails closed; no dangling root is published. This deliberately
supersedes the old mechanism-level test expecting all prepared-child capacity
recovery to fail, with stronger child/oracle/replay/fault assertions.

Authoritative mutation preparation now pins its exact UTC preparation time,
expiry-pruning cutoff and replay deadline before staging any page or opaque child
descriptor. The existing 30-day retention duration begins at that binding point.
An explicitly supplied deadline must be bound before preparation and cannot change
later. An expired preparation is unavailable with bounded
`snapshot_check/snapshot_stale` diagnosis before work and again before the head
CAS. Pure derived-projection identity remains deterministic. Strict byte equality
for an existing immutable pack is unchanged; the correction includes every
byte-affecting preparation input in its identity rather than accepting a mismatch.
Tests cover default and explicit deadlines, a reused prepared session and fresh
replicas, expiry-boundary compaction, stale preparation with opaque children,
lost successful pack/head responses and actual bytes through completion.

Preparation-bound authoritative branches omit an optional negative membership
hint only when their child is in the same already-authenticated pack, where the
hint cannot save a provider read. Cross-pack and unpacked hints remain; path copy
can restore an omitted hint from a fully verified cached child without another
GET. Corrupt cached descriptor bounds/counts/digests still fail closed. The two
independent admission phase receipts omit an unused duplicate result; the
retained fingerprint, revision and expiry and all upload/idempotency/lease records
remain the replay authority. Their compact internal phase IDs retain all 256 digest
bits and phase separation. The legacy exact-cohort initializer retains its
original receipt ID and result representation. Independent terminal actions also
retain their full generic mutation identity, fingerprint, revision and deadline,
but replay the exact completed/aborted per-file records instead of retaining an
unused generic result copy. Private action transaction IDs use a separate full
SHA-256 domain; a public legacy caller choosing the same idempotency text cannot
consume the private receipt representation. The error-only namespace publication
path reuses root/record validation and the same materialized head CAS. Delta
publication cannot omit its recognized result. Public upload IDs, batch IDs and
idempotency keys are unchanged. Literal predecessor receipts, completed replay, changed intent,
forged completion CRC and checkpoint round trips prove compatibility.

No authoritative field, canonical record encoding, feature requirement, page/pack
format, checkpoint or schema epoch changes. Existing readers already follow
these authenticated pack references. Unreachable successful writes remain
ordinary checkpoint/GC residue; they never make partial namespace state visible.

The former large-admission raw-page assertion is superseded by a stronger
independent oracle: a 183,169-byte compact request admits 4,001 items using ten
state calls, two bounded packs (53 pages, 2,074,115 wire bytes, 37,473,881 total
expanded bytes) and exactly one visibility-head write. Every pack is durable
before that head. A fresh engine authenticates all 4,001 persisted identities,
ordered memberships and intents from one shared view; replay creates no new
provider sessions and changed intent conflicts. Additional fault cases prove
no authority or immutable writes after invalid scope, cancellation, ID exhaustion
or global/nested metadata read failure; a later healthy retry succeeds.

### Comparative economics and guarantee matrix

Independent review compared raw canonical per-page output with rolling compressed
packs for the same deep synthetic namespace. Counts include the cold planning
route or warm planning route, as labelled, over instrumented memory backends and
the retained regional Standard model; they are not live GCS latency measurements.

| Deep-path route | Raw per-page fallback | Selected compressed packs |
| --- | ---: | ---: |
| Cold state calls | 136 (27 GET, 109 PUT) | 8 (5 GET, 3 PUT) |
| Cold request / response bytes | 89,264,059 / 24,916,307 | 2,012,289 / 2,393,511 |
| Cold modeled marginal cost, pico USD | 555,800,000 | 17,000,000 |
| Warm state calls | 26 GET | 5 GET |
| Warm response bytes | 23,871,262 | 2,394,165 |
| Warm modeled marginal cost, pico USD | 10,400,000 | 2,000,000 |

The selected cold/warm modeled critical p50 is 227,022 / 107,486 microseconds.
The output-only prototype's two pack PUTs are not claimed as full-route numbers.
Raw per-page fallback was rejected for its object/read/write/byte amplification.
The implementation still retains construction maps and can re-encode candidate
packs repeatedly; this slice establishes pack/page and foreground request bounds,
not a global heap ceiling or production throughput target.

| Preserved guarantee | Replacement evidence |
| --- | --- |
| Canonical integrity and complete references | Corrupt page, descriptor bounds/filter/count, prior pack, unpacked and unreachable-page tests. |
| Atomic visibility and opaque child references | Independent Base/child/Outcomes/OutcomeExpiry rereads, exact child descriptor, interrupted publication and replay. |
| Idempotent lost success | Lost prepared/root pack response and lost head response; replay does not duplicate authority. |
| Multi-replica concurrency | Newer projection-head winner remains exact; concurrent authoritative writer wins without stale publication. |
| Freeze, cancellation and denial | Freeze/cancel before commit, corrupt prepared pages, collision and oversized prepared-set no-write denial. |
| Provider portability | Shared memory/portable/GCS-protocol contracts and complete migration/checkpoint gates. Live GCS qualification remains separate. |

## Bounded directory contract and lifecycle audit

`POST /api/v1/directories/prepare` uses authenticated mutation controls and strict
JSON. It accepts 1–100 unique live paths and returns index/path-bound results.
An existing directory is ready; a file is never replaced. Missing unselected
parents or file ancestors fail only selected descendants. Independent sibling
subtrees share immutable reads and run concurrently; eligible creations publish
with one owner-head CAS. A retry revalidates current state and is naturally
idempotent, including a lost success response. Metadata corruption, provider
read failure or uncertain head publication fails the envelope; per-item success
is never fabricated for an uncertain transaction. `retryable` is reserved in the
closed response shape; current semantic path errors are terminal for that page.

The browser queues all needed ancestors before waiting, packs exact UTF-8 bodies
into at most 100-item pages, and allows four independent pages in flight. Only a
page with an ancestor in another page waits for that dependency. Response size
is conservatively checked before mutation with non-HTML-escaping JSON encoding;
a valid HTML-rich path fixture proves it cannot mutate then fail response size.
A failed folder marks only dependent unfinished transfers. Completed, cancelled
and independent successful members retain their outcomes.

Transient envelope failures and a rolling route gap retry the identical page
within the existing eight-total-attempt limit, honoring server backoff. HTTP
slots are released before backoff. Controllers register before namespace enqueue;
logout/cancellation abort queued groups, active fetches, dependency/slot waits
and timers. Consumer cancellation rejects only its own wait; a shared page
aborts when no interested consumer remains. Native Chromium acceptance covers
a lost successful directory response, a 405 route gap, retry exhaustion,
logout during a 120-second backoff or hung fetch, folder cancellation, and an
empty-folder drop queued behind suspended work. Fresh namespace work must
complete promptly, with no stale replay or scalar fallback.

A separate real-browser pagination journey completes 450 actual four-byte files
under 901 selected directories in ten bounded pages. A server barrier holds
four independent pages until all four arrive, proving sibling overlap and the
four-request ceiling. An earlier barrier holds the root page and rejects any
premature child request. Cross-page parent dependencies must resolve for every
file to publish; an independent replica verifies all 450 paths and aggregates.
Every body arrives once and no scalar stat/create fallback occurs.

For 44 synthetic paths (folder plus 43 children), scalar stats require **88 GETs**;
one existing-directory page requires **2 GETs and no writes**. Creating the same
set takes **7 state calls**. Shared contracts cover unordered parent selection,
missing parents, file ancestors, owner/area denial, bounds, exact replay and real
file upload. Fault tests cover concurrent creation, lost response and interruption
before head publication.

| Lifecycle phase | Request/provider behavior and evidence |
| --- | --- |
| Discovery/source | Existing bounded discovery, owner ledger and two hash workers remain. Reconnect does not reopen completed files. |
| Directories | Coalesced 100-item exact-body pages, four concurrent independent pages; no per-path stat/create loop. |
| Size/fingerprint planning | Up to 10,000 items partitioned by actual UTF-8 body size; shared snapshot; all fingerprint reads precede reuse mutation. |
| Logical reuse | Existing bounded batch copy with version/snapshot checks and no file-provider copy. |
| Admission | Independent 100-item pages; stable intent committed before request; accepted siblings remain independent. HTTP mutations share the existing browser namespace turn with planning and completion. |
| Resume/status | Existing 100-ID shared pages; no per-ledger-item startup status requests. Real resumed-offset acceptance below. |
| Completion | Fresh independent admission uses 100-item terminal-action pages. Persisted legacy exact cohorts keep their original key/order and replay semantics. |
| Cancellation | Existing 100-item actions or compact exact legacy cohort; uncertain outcomes retain intent. |
| Rendering/history | Virtualized rows and bounded error history; new attempts clear obsolete current errors and invalidate only relevant directory readiness. |

Admission previously bypassed the browser namespace turn and could race a
planning snapshot or a completion publication against the same owner head. A
real Chromium regression first failed with one admission overlapping a held
smart-planning request. The HTTP-only queue fixes that overlap without holding
local hashing or data transfers. Its controlled barriers also prove admission
waits behind completion, already admitted bytes continue independently, the
existing 15-second partial-completion durability window remains effective, and
all three synthetic bodies publish exactly once and reopen on replica two.
Legacy admissions and cancellation/cleanup mutations use the same turn while
retaining their exact persisted cohort and idempotency keys. Directory sibling
pages retain their separate four-request concurrency bound.

The original provider-budget test really seeds 256 existing files and compares
1 with 1,024 queries; it remains useful but is not 10,000-file evidence. The new
test seeds **10,000 existing files** and compares **1 with 10,000** size and exact
queries: cold **5 state calls**, warm sizes **4**, warm fingerprints **3**, with
zero file-provider calls. Append-only economics ratchet
`016-upload-recovery-corrective` retains the measured directory and actual-scale
vectors, including tighter bytes for the existing smaller fixtures. Earlier
ratchet epochs and thresholds are unchanged. Economics epoch
`017-prepared-mutation-metadata` records 16 strictly tighter rows from 60
exported observations, with 44 rows exact. Full provider qualification also
exposes two cheaper duplicate-projection measurements whose legacy tests do not
export calibration JSON; `018-derived-view-corrective-metadata` tightens those
two rows by one modeled microsecond. Complete unit qualification additionally
measures seven cheaper compaction/trash/restore rows outside the focused provider
suite; `019-maintenance-corrective-metadata` retains those strictly tighter
measurements. No existing ceiling increases, and all earlier economics epochs
remain unchanged.
The private record-only action path reduces the mixed 100-item page from
188,069 to 186,536 response bytes and from 97,717 to 96,184 request bytes compared
with the time-bound implementation retaining duplicate results. Public legacy
resume remains exactly at its original 203-operation / 49,144-byte measurement.
Guard tests use otherwise-valid bound mutations and a publishing positive
control: removing the guard makes delta, empty-digest and malformed-digest cases
publish 1 / 2 / 2 state writes and fail. The restored guards deny all three
without provider work. Actual public key orderings, predecessor recognized
markers, lost-head one-winner recovery, terminal CRC/area binding, reordered
expiry replay and quiescent checkpoint/raw-copy with an exact byte oracle pass.
A complete focused race selection passed in 20.369 seconds. These are focused
proofs; the final ordered gate still qualifies the exact candidate tree.

## Safe diagnosis

Closed diagnostic stages identify request validation, namespace read,
projection build/publication, target lookup, snapshot check and namespace
publication. Closed reasons distinguish validation, corrupt metadata, pack
capacity, pack collision, stale snapshot, provider failure, cancellation and
internal invariant failure. The HTTP problem boundary preserves this internal
diagnosis through nested operations while keeping the existing safe public error.

Lifecycle events contain operation, result, duration, sanitized build and
trace/span correlation, plus a validated request reference of at most 64
characters. Request references are log fields only. Metric dimensions are
unchanged; trace attributes use the closed allowlist. Queued events never hold
the raw error or request body. Independent tests inject paths, filenames, owner
identifiers, keys, credentials and capability material; enumerate all 256 stage
and reason input values; verify malformed request IDs, concurrent request
isolation, cancelled contexts, build/trace correlation, blocked collectors and
real HTTP validation/stale-token failures.

## Whole-journey acceptance

The new tests use real embedded browser smart merge, HTTP router, application,
portable engine and two replicas sharing instrumented backends. A synthetic
HTTP data plane receives actual bytes and provider-attests their integrity.
Real WebAuthn bootstrap, logout and login exercise persisted owner intent.
Final version-bound downloads are independent byte oracles, excluded from
foreground provider budgets. No customer content or queue is used.
Refresh qualification requires a new complete document and the outer
authenticated workspace to finish loading. Login requires successful real
logout/options/verification/session requests and a newly rendered restored
source control; stale inner-view nodes cannot stand in for authentication or
ledger restoration. These checks retain the original bounded stage deadlines.

- Small cold/rebuilt/warm journeys first complete seven of ten real uploads,
  deny delivery of the other three accepted capabilities, refresh/logout/login,
  reconnect sources and complete all ten. Every body is transferred exactly
  once. Nested existing folders, real smart size planning, preserved history,
  independent partial outcomes and both replicas are asserted.
- A lost first-chunk response during refresh preserves the provider-confirmed
  8 MiB offset of an 8 MiB + 17 byte file. After login/reconnect, the browser
  sends only the final 17 bytes: two data requests total, one admission,
  one size-plan and one batch resume. Downloaded bytes match exactly.
- Representative-scale acceptance uses 10,604 files, 7,966 real completed
  authoritative uploads and 2,638 remaining synthetic small files. It checks
  persisted partial completion, refresh/login, source reacquisition, actual
  transferred bytes, final namespace publication, cold/rebuilt/warm projection
  requests, zero completed-file readmission/retransfer, bounded rendered rows
  and the whole-folder Preparing floor/advancement/final 100%.

The atomic-coverage preflight on tree
`b3d295faf4da79f5b9cf39432f4bf3490fa5bd94` passed in 661.02 seconds,
including 475.730 seconds for foreground recovery. These are instrumented test
durations, not production performance. Independent authority verification found
all 10,604 files; all 2,638 remaining bodies arrived exactly once and no completed
body was resent or readmitted. Requests are distributed across two independently
opened engines; the complete fixture counters were 84 / 83. Preparing retained
its 75% floor, advanced and reached 100%; expanded rendering remained at 71 rows.
The final exact-tree gate and exported numeric evidence are recorded in the PR.

Accounting uses one continuous ledger from actual refresh, logout and login
through workspace/history restoration, reconnect and completion. A bounded
network-idle check cuts restore/auth from foreground work; the whole vector is
asserted to equal their exact sum. Synthetic fixture construction, projection
exploration and final independent authority/download oracles are excluded.
Actual HTTP counts include data PATCH and CORS OPTIONS; modeled provider events
separately count state work, file-control operations and data upload work.
Browser resource events may include static cache hits and are not substituted
for actual server request counters.

| Measured phase | Restore/auth | Foreground recovery | Whole journey |
| --- | ---: | ---: | ---: |
| State provider requests | 53 | 5,332 | 5,385 |
| File-control / data-provider requests | 0 / 0 | 5,276 / 2,638 | 5,276 / 2,638 |
| Modeled provider requests | 53 | 13,246 | 13,299 |
| Modeled request bytes, including data | 61,220 | 350,824,565 | 350,885,785 |
| Response bytes | 7,870,749 | 5,397,823,214 | 5,405,693,963 |
| Modeled marginal cost, pico USD | 81,000,000 | 29,934,200,000 | 30,015,200,000 |
| Actual control HTTP requests | 12 | 138 | 150 |
| Actual data PATCH / CORS OPTIONS | 0 / 0 | 2,638 / 2,638 | 2,638 / 2,638 |
| Actual total HTTP requests | 12 | 5,414 | 5,426 |

| Foreground composition | Observed |
| --- | ---: |
| Size / exact fingerprint / admission / completion-action pages | 28 / 27 / 27 / 27 |
| Directory pages / scalar lifecycle calls / legacy completion calls | 1 / 0 / 0 |
| Initial size-plan JSON body | 248,587 bytes |
| Metadata request / response bytes | 350,814,013 / 5,397,823,214 |
| Cold / rebuilt / warm / second-replica projection state requests | 8 / 10 / 5 / 5 |
| Completion page sizes | 26 pages of 100; one page of 38 |
| Completion pages aged at least the existing 15-second flush bound | 0 |

The original foreground ratchets remain unchanged: at most 5,962 state requests,
376 MiB metadata written and 5,748 MiB read. Measured restore/auth adds at most
53 state requests, 64 KiB written and 8 MiB read; it performs zero file-provider
or data requests. The byte allowances round the measured selected representation
and permit bounded identifier/descriptor encoding variation. Whole provider
ceilings are their exact addition, including 10,552 transferred payload bytes:
13,929 modeled operations, 376 MiB + 64 KiB + 10,552 request bytes and 5,756 MiB
response bytes. Actual HTTP caps are separately enforced: at most 12 restore
control requests, 140 foreground control requests and 152 whole control
requests, plus exactly 2,638 data PATCH and 2,638 OPTIONS. The two foreground
control requests of allowance cover coalesced final listings.

These measurements expose substantial remaining metadata amplification: about
335 MiB written and 5.03 GiB read in foreground recovery of 10,552 synthetic
payload bytes. They establish functional recovery and bounded request behavior,
not optimal metadata byte efficiency. The unchanged foreground regression
budget permits bounded coalesced listing work and rounds to 4 MiB pack units;
it is not an architectural efficiency target. The retained whole-journey model's
aggregate p50/p95/p99 service work is
419,674,962 / 1,187,969,962 / 2,881,504,962 microseconds, not observed latency or
a critical path prediction. Global memory, retained object inventory and live
provider throughput remain unqualified.

Earlier browser fault injections have been moved to the actual fresh terminal
action route and explicitly assert that the fault executed. Nine clear/recovery
tests prove denial, lost success, source-free finalization, selective retry and
no retransmission. Separate legacy ordering tests persist a genuine exact
cohort before its first request, actually publish and lose its response, then
replay after reload. Both explicit-ordinal and older group-order variants
assert one admission, two identical ordered legacy completion requests, zero
action fallback, two data writes and unchanged authoritative file versions.

## Release checklist and limits

- [x] Fresh authorized xLabs checkout; deployed-base inspection; read-only incident evidence and precise root-cause uncertainty.
- [x] Regression-first cold/rebuilt planning failures; valid closure size recovery and complete identity binding.
- [x] Integrity/CAS/freeze/replay/owner/body guarantees preserved, including authoritative prepared children.
- [x] Safe bounded correlated diagnostics and independent privacy/cardinality review.
- [x] Directory batch contract, provider comparison, dependency/partial-outcome tests and lifecycle audit.
- [x] Whole-folder preparation and bounded history/rendering assertions in real browser acceptance.
- [x] Actual 10,000-existing/10,000-query planning economics reconciled with the smaller test.
- [x] Independent telemetry, batch/publication and acceptance review.
- [x] Atomic-instrumented complete 10,604-file browser preflight, including separately measured restore/auth and foreground provider/HTTP vectors.
Final exact-tree qualification is recorded in the PR. An earlier tree does not satisfy the release gate.

The local release gate is, in order: focused Nix checks and
`git diff --check HEAD`; `nix run .#pr-check`;
`nix run .#security-freshness`; `nix flake check --print-build-logs`;
`nix run .#test-coverage`. Existing thresholds must pass unchanged. Critical
coverage matchers now also include the new directory and packing implementations
in the existing 95% namespace/CAS/publication groups; the 85% repository and 98%
migration requirements are unchanged. Independent large metadata fixtures share
a four-fixture test-only semaphore; each retains its own backend, clock, IDs,
fault hooks and ledger. Immutable seed snapshots are imported separately, and
the seed releases its slot before parallel fault cases begin. Every workload,
comparison and assertion remains present under the existing 30-minute race
package deadline. Instrumented test runtime is qualification evidence, not
production upload latency. The PR
records the exact immutable commit and gate outcomes; no merge or deployment is
authorized by this slice.

Remaining limits: exact historical production cause is unknowable from current
coarse retained logs; real production/GCS load and latency remain unqualified;
pack construction is not globally memory-bounded; an oversized opaque prepared
dependency set fails closed; infrastructure-level directory failure is an
envelope failure; existing per-engine native concurrency does not establish
cluster-wide fairness or a global concurrency ceiling. The 308.7 GiB customer
payload is represented by tiny synthetic bodies for cardinality/correctness,
not throughput equivalence.
