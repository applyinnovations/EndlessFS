# Upload recovery corrective acceptance

The fixed current release blockers, pass/fail conditions and bounded next
validation proposal are in [the release checklist](upload-recovery-release-checklist.md).
This document preserves chronological evidence; earlier passes do not qualify
the current follow-through tree or override its failed concurrent acceptance.

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

## Provider-efficiency continuation: bounded immutable wire reuse

The first qualified corrective commit is
`df80c15d7b6dd0e11ea9504cd1b4e084284446fd` (tree
`ad23c449d0570459d3d9c401631c349f389295c5`). Its successful real browser journey
still read 5,400,980,443 metadata bytes and wrote 348,964,171 metadata bytes in
foreground recovery of 2,638 four-byte files. Bounded HTTP pages did not resolve
that amplification. This continuation targets repeated immutable downloads;
it does not claim to eliminate namespace/history-dependent work, decoding,
publication bytes or per-file lease operations.

### Comparisons and selected scope

A shared, outside-release exploration measured all independent admission and
completion maxima 100/250/1,000 with idle wire capacities 0/4/16/64 MiB. Planning
remained at 100. All 72 namespace-only cells completed real four-byte transfers,
with exact authority membership and sampled stored bytes checked: existing256 /
remaining1,001 and existing7,966 / remaining300. Separate comparisons constructed
256 and 7,966 **actual completed upload histories**, rather than merely seeding
namespace entries. Every measured cell began with a cold engine coordinator after
fixture construction. The same deterministic IDs, source paths, admission and
completion policy, provider model and observation boundary were used within each
comparison. No customer content was used.

These are engine-level, eager full-page observations: they exclude browser
arrival timing, its publication age timer, login and directory-refresh requests.
Their in-process object store retains synthetic immutable history and therefore
contributes to sampled heap. Allocation, peak heap and wall time are single-run
observations, not provider latency, production RSS or acceptance ceilings.

At admission100/completion100, the retained7,966 / remaining300 comparison was:

| Idle wire capacity | Provider calls, including data | Metadata read bytes | Metadata write bytes | Strict pack decodes | Sampled peak Go heap bytes | Measured recovery microseconds |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Disabled | 1,459 | 253,306,094 | 16,662,415 | 221 | 753,177,464 | 18,631,295 |
| 4 MiB | 1,447 | 234,371,493 | 16,662,415 | 221 | 784,910,544 | 20,562,507 |
| 16 MiB | 1,438 | 221,211,287 | 16,662,415 | 221 | 785,802,480 | 19,009,296 |
| 64 MiB | 1,372 | 147,100,642 | 16,662,415 | 221 | 874,098,088 | 18,677,853 |

Each row transferred exactly300 payloads /1,200 bytes and independently verified
8,266 authoritative files. The single failed provider GET per row is the same
expected cold-projection miss. Four-MiB results from namespace-only fixtures
would have overstated the benefit for retained history: those reads fell about
77.6%, whereas actual retained7,966 history improved only7.5%. At64 MiB the latter
read reduction was41.9%, with87 hits,122 misses and71 evictions. Requests fell87;
modeled total cost fell3,334,000,000 to3,299,200,000 picoUSD. Unchanged per-file
session/verification/lease calls dominate marginal request cost. No wall-time or
strict-decoding improvement is claimed.

The initial wire-only candidate retained at most64 MiB of copied verified
compressed bodies and256 entries **per engine**, allocated on demand. The
deployed settings observed in deployment PR131 are2 GiB memory limit and
`GOMEMLIMIT=450MiB` per replica;64 MiB is about14% of that soft target. This is an
explicit resource/read tradeoff, not a universal optimal cache size or a
process-memory bound. The larger128/256-MiB history comparison both reached1,329 total calls and
99,804,462 read bytes, with identical writes/221 decodes. Both retained exactly
99,785,600 bytes in79 packs,130 hits and no evictions; all79 fetched immutable
packs were fetched once. Thus128 reduced another32.15% of reads versus64, while
256 provided no further wire benefit in this fixture. The128 configured bound
is28.44% of the deployed450-MiB per-replica soft target; its observed retained
wire was21.15%. This is a material memory/read frontier, not proof that64 or128
is universally optimal. The selected implementation uses128 MiB /256 entries
per engine: it reaches the measured no-repeat frontier in the retained-history
comparison;256 provides no further read benefit there. The actual standard-runtime
browser journey below passed before final qualification. A cold or evicting replica remains
correct and may download the same pack again.

Publication frequency was compared using total calls **and bytes**, independently
of admission size. For namespace-only existing7,966/remaining300 at cache4 MiB,
completion100→250 reduced state calls363→351 and writes2,709,528→2,273,771, but
without the cache it increased reads17,710,557→26,294,136. Admission1,000 /
completion1,000 at cache4 MiB read4,450,248 and wrote3,182,412 versus3,355,849 /
2,087,713 for completion250 at the same admission maximum. A directory edit set
of at least256 can select a full-tree rebuild; smaller mutations path-copy
changed branches, so a blanket claim that every publication rewrites all N
entries would be inaccurate. These measurements reject a blanket larger-page
change in this slice. Production remains at100 items; increasing it also needs
pre-mutation request/response envelope proofs for long/escaped resolved paths,
partial results and the browser age guarantee. Existing post-mutation envelope
checks are not such a proof.

### Guarantee preservation

| Guarantee | Implementation and independent evidence |
| --- | --- |
| Fresh authority and atomic visibility | Heads, gates, leases, native CAS versions and create-only exact-wire conflict checks still use the provider; the cache never proves publication presence. |
| Immutable integrity and binding | Only successful strict provider decodes admit copied wire bodies and an opaque validation memo. Every hit verifies the full wire SHA-256, length and domain/kind/pack binding before expansion, then performs bounded gzip validation and fresh JSON decoding. The memo reuses deterministic canonical/member validation for the exact previously verified bytes. Per-use page digest and descriptor checks remain. In-flight keys bind all expected identities and distinguish maintenance reads. |
| Corruption and cold recovery | Invalid cached wire fails closed and is evicted; provider faults are not admitted. Subsequent independent requests can retry. Ordinary verified immutable hits can survive external deletion/corruption; startup, checkpoint closure and reachability explicitly fetch provider authority and detect it. |
| Freeze, checkpoint and GC | Startup enables idle caching only after initialization. Checkpoint verification bypasses idle cache and ordinary in-flight reads. GC retains fresh gate/LIST/version-conditional deletion. Warm corrupted and missing pack tests deny checkpoint; restored valid authority succeeds. |
| Isolation and cancellation | Cache belongs to one engine/backend. Different expected identities cannot share an in-flight result. Cancelled leaders do not cancel healthy waiters; cancellation does not evict valid wire. |
| Resource ownership | Independent byte and256-entry LRU bounds, copied provider buffers, exact eviction and oversize/disabled tests. Proof issuance takes its bounded wire copy inside the existing single decode-admission slot. Active requests may pin evicted wire and own decoded maps; this is not global RSS admission. |
| Canonical bytes and replay | Whole-pack decoding and page residency semantics are retained; no staged/PUT bodies are admitted. A page-only cache was rejected as a drop-in because current child-hint reconstruction depends on full-pack residency and could alter canonical branch bytes. |

The independent reviewer found no remaining cache correctness blocker. Targeted
cache/concurrency tests passed before final qualification; exact-tree results
belong to the final PR gate record. Sensitive coverage now includes the cache
coordinator in the existing95% CAS/publication groups, without lowering a
threshold. Cheaper provider vectors require a new append-only economics epoch;
all historical ceilings remain immutable. Full numeric baseline/candidate vectors
include request/response bytes even though the existing economics ratchet schema
does not itself encode byte ceilings.

Economics epoch020 (`020-bounded-immutable-pack-cache`) appends32 strictly
cheaper rows after019. Its JSON SHA-256 is
`4260a3bdc6f9c240e14f756140b6f7be4ddbd7549d42353fff71d4cd93e35dcd`.
Independent review paired134 exported application vectors and reconciled6,498 aggregate,
role, kind and subsystem numeric fields: zero increases, exact failure counts,
32 strict /102 unchanged calibrated rows. All65 prior economics/migration
fixtures remain byte-identical.
This model measures provider operations, not codec CPU or production latency.

The initial public-budget selection exported70 rows and missed four exact-budget
owners whose names lacked `ProviderBudget`. Focused and complete unfiltered
portable checks caught four cheaper calibrations: empty-trash1,024 (4→3 calls),
incremental planning (7→5), sizes1,024 (4→2), and fingerprints1,024 (3→1).
The unfiltered run completed2,242 passing tests/subtests with only those cheaper
calibration failures. The expanded Go-AST inventory resolves26 application
owners plus one synthetic ratchet-contract owner; the134 application rows are
complete for these exact assertions, not a claim that all255 active/historical
budget rows were remeasured. Eleven identical numeric-export test overlays are
applied to the immutable baseline and candidate; fixtures/assertions stay intact.
The32-row020 retains the original28 reviewed entries and adds only those four.
Both the public and flake budget commands now share the package roots and test
selection, including all four legacy owners. A policy test audits exact callers,
helper reachability and selected package roots to prevent another silent omission.
Its focused suite passed with26 application owners /six roots. Outside-tree
negative controls removing the four legacy selections or the identity package
both failed with the precise omitted owners. Unsupported method/function-value,
cross-package and orphan helper forms fail closed; only the named synthetic
ratchet contract and shared numeric-export helper are exempt.


Comparative JSON provenance (outside-Git evidence retained with this session):

- 72-cell namespace matrix SHA-256
  `11633642a699e7987c852092352a54229b406c45390efece8725c2dea9ee2378`.
- Retained256-history matrix SHA-256
  `ded10c6d0adc6cd2261268062b4ccd73fc310838b8f3e404a827d825d098badf`.
- Retained7,966-history0/4/16/64 matrix SHA-256
  `ec6067c80caf919eb0678c990657d6594ce376fcde3d3e3e82d725712e4d8182`.

Whole-lifecycle provider efficiency remains under qualification with open
structural costs: repeated expansion/fresh decoding, immutable publication writes,
retained-history/namespace-dependent reads, and one lease per admitted file.
The four-per-file provider operation floor applies only under the unchanged
lease/session/verification contract; it is not a universal lower bound for a
future storage design. New layout, digest-aware diffing or replayable lease
segments need separate canonical/recovery proofs and matched full-vector
comparisons before claiming a further improvement.

The first cache browser preflights stopped before foreground recovery on a test
assumption, not an application400: `warm` on replica1 with a cold idle cache
made5 provider calls, while its later warm query made2. The old assertion
incorrectly demanded equal physical call counts across different cache heat.
The replacement preserves both5-call ceilings and all cold/rebuilt ceilings,
compares every ordered public decision field and ID across replicas/stages,
forbids provider writes in warm stages, and adds an identical same-replica
repeat that MUST read exactly the namespace and planning-projection heads once
each. These failed probes are not successful upload acceptance evidence.

Corrected wire-only64/128 browser stress probes then used the unchanged20-minute
browser and12-minute recovery deadlines under `GOMEMLIMIT=450MiB`. Both failed
after1,300 actual remaining files transferred; the64-MiB probe had published1,200
with zero failed items. The128-MiB probe exhausted the browser context. Neither
produced the success artifact. These probes run two engines and an in-memory
provider retaining synthetic history in one Go process, whereas production has
one engine per replica. Observed process high-water RSS was997,257,216 /
1,125,208,064 bytes respectively; these include fixture/provider/runtime memory
and do not measure isolated cache memory or a production replica. The failures
show that reduced provider bytes alone did not establish successful recovery.

This stress boundary includes two independent engine caches but one shared fake
provider. `memory.Backend.Put` retains its own copy under each immutable key until
explicit deletion; this browser lifecycle runs no GC/checkpoint. The successful
earlier browser artifact records145 distinct large immutable writes totaling
348,579,670 bytes (332.43 MiB) during foreground alone. Separate root-folder
H=7,966 profiles attribute about146.42 MiB of sampled post-seed in-use heap to
fake-provider Put storage, rising to161.15 MiB after300 remaining files. Those
profiles have different folder geometry and do not establish the current
browser's exact live inventory. Both caches may additionally retain256 MiB in
aggregate; provider Get/Open and decoding allocate transient copies. These
facts make the aggregate450-MiB stress materially different from one production
engine using an external provider. They do not prove the live slowdown's cause
or qualify production memory. Standard-runtime acceptance and this additional
stress result must be reported separately, with unchanged deadlines in both.

Profiling motivated an exact-wire validation memo rather than caching decoded
mutable maps or weakening cold verification. The memo has private fields, an
invalid zero value and no durable representation. Strict validation owns its
wire copy before validation/hash, and only successful provider reads admit it.
Mismatched wire, scope, kind or pack fails closed without a same-call fallback.
Every new decode retains all pack members and owns fresh maps; page-only reuse
would alter existing child-hint reconstruction semantics. Initial migration and
checkpoint verification bypass both idle reuse and ordinary in-flight results.

On the same valid36,903-byte synthetic metadata pack, the matched strict codec
measured28.024ms /23,880,798 allocated bytes per operation; exact-wire reuse
measured17.014ms /9,915,401 bytes. Three ordinary repeats measured9,915,407–
9,915,438 bytes; three race repeats measured17,138,865–17,139,313 bytes. Separate
warm-path guards round those observed upper values to the next whole MiB (10 /
17 MiB); the older strict-decoder guards remain unchanged. Codec timings are
microbenchmarks, not full recovery or production throughput evidence.

Independent tests cover exact strict/proven equivalence, fresh-map isolation,
zero/altered/wrong-binding proof denial, noncanonical/invalid-member/wire and
expanded bounds, cancellation before/during/after admission, and concurrent
callers. A queued4-MiB input allocated952 bytes while waiting for the single slot;
an isolated pre-wait-copy control failed at4,195,896 bytes. Copy placement thus
does not allocate another whole wire buffer per queued decoder. The focused
proof and codec/cache race tests passed before final-tree qualification.

The proof-based128-MiB aggregate450-MiB stress probe also FAILED the unchanged
12-minute recovery deadline (1,071.43 seconds including setup). It transferred
1,600 remaining files and published1,500; UI counts were9,466 complete /1,138
current /zero failed, with Preparing90%. It produced no success artifact.
Compared with the prior wire-only probes this is additional completed work, not
successful end-to-end acceptance. The exact cause of aggregate memory pressure
and the production one-engine/external-provider memory envelope remain open.
Its raw log SHA-256 is
`b759a915fe315fdf2870700c8f6c235922b3fabf34a9c562e6855271d352ecf0`.

An independently profiled actual-byte H=7,966/R=300 comparison starts CPU and
allocation observation after fixture construction and stops before final
authority/download oracles. All three capacities transfer300 four-byte payloads
and verify8,266 members. Complete provider vectors exactly match their earlier
wire-only rows; the memo changes codec work, not provider semantics:

| Capacity | Foreground allocated bytes | Sampled peak Go heap bytes | Decoder share of sampled CPU | Observed foreground seconds |
| --- | ---: | ---: | ---: | ---: |
| Disabled | 13,438,574,576 | 725,872,048 | 72.96% | 21.494 |
| 64 MiB | 11,852,297,976 | 872,187,712 | 69.60% | 19.037 |
| 128 MiB | 10,708,034,584 | 911,328,528 | 67.79% | 17.813 |

At128 MiB, strict cold verification and proven decoding account for32.58% and
30.01% of sampled CPU respectively, both subsets of the decoder share. Canonical
pack re-encoding falls6.01→2.67 CPU seconds, while JSON unmarshalling remains
6.66→6.65 seconds. Cold initial size planning still allocates roughly4.2–4.35 GB
in every row. This identifies expansion/fresh JSON and projection construction
as remaining costs, even when every fetched immutable pack is fetched once.
There are221 pack-decode observer events in every row; that event count no
longer means221 full strict canonical validations when memo reuse is enabled.
Peak heap includes fixture history and the in-memory provider. Timings under
host contention are observations, not production latency or browser acceptance.
Evidence JSON SHA-256:
`84e599c259f86657e08a3ea1a6dc529fcf9090a61c99dca3023733a053b466f4`.
Compiled source manifest SHA-256:
`b427c76c53aa0a154e851bcbfbd4a1ad90e0014a1590e26f7178558ecfe0ccf8`.
The temporary experiment source was archived and removed before final freeze.

### Complete standard-runtime recovery preflight

The unchanged real-browser test PASSED with repository-standard runtime settings
(no additional `GOMEMLIMIT` override), including cold/rebuilt/warm projection
checks, reload/logout/passkey login/restore, reconnect, planning, admission,
actual data transfer and terminal publication. All2,638 remaining four-byte
files transferred exactly once. None of7,966 completed files were readmitted or
retransferred. Independent authority membership, sampled stored bytes and
durable history passed for all10,604 files. Preparing advanced75→100% with
intermediate progress and at most71 rendered rows. The initial size body was
248,587 bytes. Terminal pages were26×100 +1×38; maximum observed page3,878ms,
zero pages at least15 seconds. Total observed test time535.75 seconds is a
synthetic, uninstrumented observation, not production throughput.

| Phase | State calls | File-control calls | Actual payloads | Metadata written bytes | Metadata read bytes | Actual control/PATCH/OPTIONS HTTP |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Restore/auth | 51 | 0 | 0 | 61,207 | 320,127 | 12/0/0 |
| Foreground recovery | 4,374 | 5,276 | 2,638 | 342,215,399 | 2,697,963,746 | 138/2,638/2,638 |
| Whole journey | 4,425 | 5,276 | 2,638 | 342,276,606 | 2,698,283,873 | 150/2,638/2,638 |

Foreground requests were28 sizes,27 fingerprints,27 admissions,27 terminal
pages,one directory batch and27 coalesced listings, with no scalar stat or
legacy lifecycle routes. Foreground had zero failed provider operations;
restore/auth retained its three expected failed lookups. Large immutable reads
were984 requests /196 unique targets /2,694,948,388 bytes; writes were144 unique
targets /341,831,299 bytes, with maximum read/write4,192,053 bytes. Repeated reads
and substantial publication bytes therefore remain visible at full scale.

The earlier qualified full browser observed5,327 foreground state calls,
5,400,980,443 metadata read bytes and348,964,171 written bytes. Current observed
read traffic is approximately half and state calls fall953. These browser
fixtures use fresh identities/time and are not byte-identical causal comparisons;
the fixed-input engine/full134-vector experiments isolate reuse effects. Existing
full-browser cold/recovery ceilings remain preserved, while exact fixed-input
provider rows tighten through020. Success-only preflight JSON SHA-256:
`e9827e21714c8f9886c9b05c6ecf1518250040c4487df7a44112d89300c2fa4e`.
This preflight is separate from both the failed aggregate450-MiB stress and
the final exact-tree mandatory gates recorded in the PR. It does not qualify
production one-engine memory, live-provider latency or308.7-GiB throughput.

Economics comparison provenance includes134 paired application numeric rows,32
with strictly cheaper calibrated vectors and102 with identical calibrated vectors. The existing
schema encodes calls, costs and modeled latencies; full exported observations
also retain bytes and failed outcomes. One unchanged migration row has
pre-existing byte variability: old-source repeats wrote195,891–195,894 bytes
and read1,084,038–1,084,044, with identical639 calls and14 expected failed GETs.
The two optimization packs have identical decoded pages/values/digests; only
physical pack IDs differ. `stageSchema008Domains009` constructs a domain catalog
whose nested store defaults to system time, so the fixture's fixed clock does
not determine preparation-time-bound pack IDs. An outside-tree diagnostic
passing the fixed clock produced identical canonical key/body maps, ordered PUTs
and complete vectors in eight repeats (195,894 written/1,084,044 read). This
establishes the source of variation; it is not evidence of cache-induced writes.
The stricter epoch covers only changed rows with paired nonworse full vectors.
No claim that every unchanged row has identical byte counts is made, and no
historical or byte ceiling is increased. Diagnostic proof SHA-256:
`c69eaa92aa0ac7d7c48149aafba72d296e73f286fedec6796d87d55bdde93513`.

The additional128/256-MiB frontier JSON SHA-256 is
`4a5730687dda2f0b23be99adbbef9c2b829f7e24a354f8bd8597b8ed0e4417de`.

## Streaming tree preparation runtime continuation

Required CI for `55e82f11e09e7d2aa79152693c20fa61fc9eaf9d` failed at
01:14:14 UTC on October 18: the unchanged one-hour pipeline expired while browser
coverage was still running. Its full Nix checks passed in 2,556 seconds,
including the ordinary portable race suite in 1,574.342 seconds. Coverage was
cancelled after 877 seconds; no assertion failure was reported before cancellation.
That failed run is preserved and is not qualification. No unchanged retry,
timeout increase, resource increase, scheduler change or test omission is used
as a correction.

The fixed H=7,966/R=300 foreground profile identifies another avoidable cost:
the streaming tree builder copied and canonically encoded every growing leaf
prefix for every admitted member. It accounted for 11.72% of sampled CPU and
27.69% of sampled allocation bytes in the paired baseline. The selected change
encodes the first member with the real page header, then accounts for each
additional canonical member and one comma. The existing 256-item and exact
canonical-byte limits, maximal leaf boundaries, final canonical validation,
immutable writes, prepared pack identities, native conditions and publication
CAS are unchanged. Accepted value bytes become privately owned, preventing a
caller's reused buffer from invalidating the cached size. Cancellation is
checked before sizing or copying. No new idle cache is introduced.

| Fixed-engine observation | Prior builder | Linear builder |
| --- | ---: | ---: |
| Foreground wall time, microseconds |15,208,501|13,237,098|
| Foreground allocated bytes |10,653,323,560|7,823,179,936|
| Sampled peak Go heap bytes |915,947,792|872,014,456|
| Sampled CPU seconds |16.55|14.11|
| Provider operations |1,329|1,329|
| Provider response bytes |99,804,462|99,804,462|
| Provider request bytes including payload |16,663,615|16,663,615|

The entire modeled provider vector and every phase vector match exactly,
including cost, failed requests and modeled latency. Both runs transfer 300
four-byte bodies, verify 8,266 authoritative files and sample independent bytes.
The observed foreground time reduction is 12.96% and allocation reduction 26.57%.
This one fixed-engine pair does not predict CI, browser or GCS time. Complete
final-tree gates and required CI remain necessary after this change.

An independent test retains the previous complete builder as an oracle and
compares 28 packed/unpacked, item-bound, byte-bound and lost-response cases.
Canonical pages, roots, packed provider wire, full provider events and modeled
vectors match. The matrix includes escaping, non-ASCII, nil/empty values,
base64 padding, exact canonical limits and one-byte overflow, oversized input,
malformed ordering, failed prefix flush and immutable retry. Ownership and
cancelled-admission tests fail against the old source and pass after the fix.
The quantitative cases are selected by the public provider-budget gate.

Cold complete-tree construction with 64/128/256/1,024 one-KiB values measured
0.69/1.35/2.65/8.21 MB allocated, compared with the former
3.89/14.53/55.44/211.41 MB. Race measurements were
1.35/2.73/5.29/24.74 MB compared with 13.02/46.26/171.96/684.25 MB.
A separate linear allocation guard includes final canonical validation and
provider writes, with 12 KiB per item ordinarily, 28 KiB under race instrumentation
and 64 KiB fixed work. Five repeats of both builds pass. Existing grouping,
codec, provider and coverage ratchets are unchanged. The more complex private
parsed-snapshot idea remains unintegrated research because it adds persistent
memory and another ownership/budget contract; these results do not claim that
the remaining decode cost is optimal.

### Deployed resource and response-deadline reconciliation

Read-only selected configuration confirms two deployed replicas, each with
1 CPU and 2 GiB hard memory, and `GOMEMLIMIT=450MiB`. Selected secret-key presence
checks confirm these resource values are not overridden; no secret value is
emitted. The 128 MiB wire-body cache budget per engine is 28.44% of the Go soft
target and 6.25% of the hard memory limit. Cache bounds do not bound request
scratch, pinned evicted entries, construction memory or total RSS.

The earlier 450 MiB stress failure combined two engines, two caches and a state
provider retaining all metadata bodies in one Go process. A source-pinned
isolated probe of `55e82f11e09e7d2aa79152693c20fa61fc9eaf9d` instead places
one physical engine inside a 1 CPU/2 GiB/no-swap cgroup with the 450 MiB Go target;
the synthetic state provider and Chromium run outside it. The test driver and
tiny file backend remain inside. The proxy preserves all eight backend methods
and passes the shared backend and cancellation/inventory contracts. Two logical
HTTP handlers share the one engine; this is not two-replica performance proof.

Independent review caught the harness's unlimited HTTP write deadline. Selected
live settings are GCS previews with a 45-second preview operation limit; the
unchanged deployed and candidate server code therefore select a 50-second
control response deadline. A second full probe enforces that actual 50-second
server deadline, retaining the existing 20-minute browser and 12-minute recovery
deadlines. It passes: all 10,604 entries verified, all 2,638 remaining bodies
transferred once, zero completed-file readmission/retransfer, 75→100% Preparing,
71 rendered rows and 5,426 actual whole-journey HTTP requests. Observed pre-oracle
kernel memory high-water was 872,517,632 bytes (about 832 MiB), with no observed
OOM events or kill. The artifact's test duration is 678.605 seconds; foreground
recovery is 549.076 seconds. The external provider alone retains 665,181,921
metadata body bytes after the run, exceeding the older aggregate 450 MiB target.
That establishes the resource-boundary distinction, not the earlier timeout's
precise cause.

This successful predecessor probe does not substitute for exact-tree evidence
after the streaming change. Its 35.35-second maximum size-planning request
(four of 28 at least 15 seconds) is a material latency limitation. The ordinary
preview-disabled server selects 30 seconds; this probe qualifies the selected
deployed 50-second boundary only. Live GCS latency, separately capped replicas,
concurrent owners, node contention, telemetry overhead and worst-case metadata
distributions remain unqualified. A fresh independent verification engine is
opened only after the foreground snapshot; its constructor can perform normal
startup conditional writes. Its work is excluded from foreground counters.
The Go soft target, observed cgroup high-water and hard limit remain distinct.

## Whole-lifecycle followthrough

The preserved baseline is commit
`6db3cd38d1e8932970fefc7d1b6bfd154028abe9`, tree
`718a07ebdb06c7586c132ac185cf2c2225a48352`. Its actual required CI
[`endlessfs-ci-g4m5v`](https://tekton.xlab.now/#/namespaces/tekton-buildkit/pipelineruns/endlessfs-ci-g4m5v)
passed in 3,283 seconds under the unchanged 3,600-second deadline. Repository
coverage was 87.268%, every sensitive group met 95%, and migration coverage was
98.181%. The 19 printed coverage results are one repository aggregate plus 18
overlapping groups. They do not constitute 19 independent upload-acceptance
proofs. In particular, theme, preview, migration and local GCS protocol coverage
do not establish concurrent-owner capacity or live-provider throughput.

The exact-baseline isolated single-engine probe also passed with the actual
50-second control write deadline, one CPU, 2 GiB hard memory, no application
swap and the 450 MiB Go soft target. It transferred the remaining 2,638 bodies
once, preserved all 7,966 completed files and verified 10,604 entries. Foreground
recovery took 622.316 seconds; observed pre-oracle kernel peak was 816,037,888
bytes. The maximum size handler was 35.362482 seconds. This is distinct from
the earlier predecessor result above and remains single-process evidence.

### Bounded acknowledgements after durable work

Regression-first actual-router tests found another correctness gap independent
of the deadline: successful admission or publication could precede a response
503 caused solely by repeated valid path details expanding beyond 1 MiB.

| Synthetic real-router workload | Before | Corrected result |
| --- | --- | --- |
| 24 files with valid 4,096-byte paths; 96 transferred bytes; 1,835-byte completion request | First completion and replay both 503 after all files were already published | Correlated 200 acknowledgements, 7,947 bytes first and 8,139 bytes reordered replay; identical published versions |
| One valid completion plus one unknown action; 1,048,575-byte request | Invalid action text expands the reply and hides the published sibling behind 503 | 200 partial result; valid four-byte file published once; unknown action independently rejected with bounded `invalid` action marker |
| 48 files with valid 4,096-byte paths; 202,286-byte admission request | Admission commits before 503; compact-ID status and resume also return 503 | Every admission/status/resume outcome remains representable and usable; 48 actual files complete once, 192 payload bytes |

Ordinary admission, lifecycle and supported-action JSON remains byte-identical. Exceptional
`detailsOmitted` replies omit display paths and action entry details, retaining
correlation, outcomes and operational status. Capabilities are never truncated.
If complete capabilities do not fit together, successful siblings retain
theirs. An accepted admission retains its exact upload ID and directs actual
resume, whose capability can differ from the initial one. Resume deferrals keep
the existing promise and halve only unresolved cohorts. Only an actual resume
capability that cannot fit even alone requires terminal attention. Singleton
sizing normalizes index zero and considers both ordinary and compact forms;
short paths can make the ordinary representation smaller.

Real-browser smart uploads prove eight actual four-byte files complete after
one admission, zero transient retries and no repeated body. The 600,000-byte
opaque capability fixture recovers pages of 7, 3, 3, 1, 1, 1, 1. A near-limit
fixture contains three all-deferral pages and recovers cohorts
8, 4, 4, 2, 2, 2, 2, 1, 1, 1, 1. A truly unrepresentable resume retains its
admitted ID, requires attention without retries or file bytes, and clear-failed
cancels that exact attempt. Maximum 100-item escaped correlations preserve an
independent missing item and completed sibling alongside usable/deferred ones.

The encoder validates every ordinary item even after detecting overflow, so
compaction cannot hide a later malformed path. It retains a bounded serialized
page plus one transient full-item encoding, rather than all oversized capability
encodings or another aggregate page. This is not a literal 1 MiB Go heap bound.
Boundary tests cover nil/empty/ordinary byte equality, exact 1 MiB, one-byte
overflow, escaped fields, short paths, index renumbering and actual
admission-to-resume representation changes.

Independent browser binding tests also exposed missing nested upload-ID checks.
Untouched baseline controls consumed a misbound completed status, and a misbound
capability sent one byte and completed the wrong nested identity. The correction
checks outer, status and capability IDs before any progress or data. Both
negative cases now stop with zero bytes/completions and cancel the original ID;
matching controls still complete. Existing cancellation and 2,638-item bounded
classification tests remain unchanged and passing.

### Transport and decode-admission observation

Response write errors now retain only closed stage/reason/result codes. A real
HTTP/1 regression demonstrated that an expired socket deadline can still allow
a small JSON Write to return its full length with no error: Go buffers the
body until after middleware returns. One shared final control-response flush
now runs before request logging and HTTP activity completion, so buffered-body
timeout becomes `response_write/deadline/timeout` instead of success. Intended
HTTP status and payload remain unchanged. Tests cover success, canceled and
failed writes, short writes, wrapped writers, unsupported flush, HEAD,
explicit lengths, 204/304, empty responses, trailers, panic and hijack.

This changes valid small-response HTTP/1 framing: a measured 12-byte JSON reply
with identical fixed headers grew from 657 to 675 wire bytes, an 18-byte header
and chunk-framing overhead. Explicit Content-Length is preserved; implicit
HEAD Content-Length can be absent. Later terminal chunks/trailers and actual
client receipt remain outside this observation promise. No listener, timeout,
provider or data-plane behavior changes.

`metadata.pack.admission` now measures waiting for the existing single decoder
slot separately from `metadata.pack.decode`. Cancellation and deadlines before
acquisition have safe distinct diagnoses. The slot count and integrity checks
are unchanged. Privacy/cardinality tests include errors whose Error method
panics, 100 distinct private-error/request-reference values, all 256 enum inputs,
real response problems, trace allowlists and metric-label denial. Request IDs
remain correlated log/per-item fields, never metric labels.

The selected deployed preview configuration uses a 45-second preview operation
limit and therefore the existing 50-second control deadline. Preview-disabled
configuration selects 30 seconds. The preserved 35.362482-second observation
exceeds that default; it is not evidence of a deployed 50-second expiry or a
permanent recovery failure. Real socket tests configure 30 seconds and inject
an already-expired write deadline without sleeping, demonstrating that the
handler context remains live and the buffered response fails at flush. This
does not qualify the full planning workload at 30 seconds or explain the
historical 400. Production configuration and all timeout limits remain unchanged.

### Separately capped replicas and concurrent owners: failed qualification

An independently source-bound research driver ran the exact preserved `6db`
application in two physical processes, one engine per process. Each retained
one CPU, 2 GiB hard memory, no application swap, the 450 MiB Go soft target,
unset GOMAXPROCS and the actual 50-second control write deadline. Shared state
and native file-session providers, two real passkey browser profiles and the
driver ran outside the application cgroups. Both owners deliberately used the
same paths and local item IDs with owner-salted four-byte bodies. The shared
file-provider contract and 10/7/3-per-owner smoke passed, including cross-process
completion replay and owner denial.

The full workload used 10,604 files per owner, 7,966 already complete and 2,638
remaining. Both real reload/logout/login restorations completed, both selected
all source files, and their initial smart-size handlers genuinely overlapped on
one physical replica. Both processes subsequently served both owners with
handler overlap. The manifest binds 617 baseline files; production source
remained byte-identical and only explicit research tests/adapters differed.

The full run **failed the unchanged 12-minute recovery deadline**, ending after
941.92 seconds including setup. Its last retained browser observation showed
2,000 new completions per owner, Preparing 94%, and pending work without current
displayed errors. Final authority, exact-once, completed-version, transport and
controlled-replay oracles were not reached: the failure path exited before
those final counters were saved. They must not be inferred from the partial
ledger observations or the successful smoke.

Observed kernel peaks were 884,649,984 and 938,803,200 bytes, with no OOM/max
events. A retained approximately 712-second foreground cgroup bracket used
449.379/450.474 CPU seconds and recorded throttling; this does not attribute CPU
to GC, decoding, publication or provider waits. The provider recorded 19,880
calls, 548,923,891 request-wire bytes and 6,083,472,680 response-wire bytes,
including research framing but excluding HTTP/TCP headers. No work/memory
safety guard fired. The last retained
size-handler maxima were 34.544105/36.184200 seconds; admission maxima were
21.875699/22.227929 seconds. Completed handlers at least 50 seconds were zero
at that snapshot. Size handlers exceeded 30 seconds on both replicas, with
exact counts unavailable from the original aggregate metric. These observations
do not establish actual terminal response-delivery counts.

All owned containers, browser groups and runtime tokens were cleaned up. The
failed source, binary, safe snapshots and hash allowlist remain preserved. This
result blocks comprehensive concurrent-owner qualification. It requires measured
attribution and a successful corrected full run under the same limits; a passing
component gate or healthy deployment cannot replace it.

### Incremental projection decoding: selected narrow improvement

Incremental planning traversed both the prior and current namespace and fully
decoded both entries even when their immutable bytes and logical versions were
identical. It now performs one full semantic decode for that identical pair.
Both iterators, all page/pack/descriptor validation and changed-entry decoding
remain. Identical malformed entries still fail before staging projection pages;
the optimization never treats byte equality as semantic validation.

A separately frozen comparison used 7,966 genuinely completed four-byte files
across 43 existing nested directories, followed by six 100-file rounds of actual
admission, data, completion and incremental planning. Each application run had
one CPU, 2 GiB hard memory, no application swap and the 450 MiB Go soft target.
The external state provider was outside that cap. The candidate used a reviewed
Go launcher in place of the baseline's task-local Python launcher; source,
application arguments, provider protocol and workload were bound independently.
This is a direct-engine comparison, without browser or HTTP-deadline coverage.

| Observed measure | Prior decoding | One decode for identical entries |
| --- | ---: | ---: |
| Six incremental-size calls | 3.910577 s | 2.836232 s |
| Allocation in those calls | 1,866,749,400 B | 1,401,698,504 B |
| Complete foreground | 33.801031 s | 32.838586 s |
| Complete foreground allocation | 16,451,581,224 B | 15,911,721,992 B |
| Whole-run kernel peak | 703,660,032 B | 706,732,032 B |

Every one of the 32 phase provider vectors and the whole vector matched exactly:
2,576 requests, 170,135,839 read bytes and 47,420,952 request-write bytes. Both
runs verified all 8,566 final files, stored checksums and the 7,966 prior versions;
600 new four-byte bodies were transferred once. Old-implementation oracles also
match projection entries, roots and compressed wire for append, prepend, middle,
remove and nested rename. Corrupt equal entries, invalid current entries and
different versions retain denial behavior. Unsafe skip-all-decode controls fail.

The measured incremental latency reduction is 27.47%; whole foreground improves
2.85% in this pair. Heap and kernel observations are mixed. This is neither a
peak-memory improvement nor an explanation or fix for the 35-second planning
tail or failed concurrent-owner recovery. Those require separate qualification.

## Bounded closure repacking candidate

The unchanged current two-owner run also failed its 12-minute recovery wait.
The earlier exact-baseline profile and source identify repeated encoding of a
growing pack prefix as a supported corrective target; no current profile or
October 17 root cause is inferred. The replacement owns and authenticates the
reachable original staged closure, assigns deterministic stable-range IDs,
rewrites postorder, and encodes whole groups. Only typed actual capacity failures
split a group. Every final group validates and fits before any new-layout PUT;
original opaque dependencies and the original head CAS/freeze remain intact.
The former incremental-prefix packer is removed. No universal logarithmic
encoding-work bound is claimed for adversarial compressed-size behavior.

An old-helper regression normalized same-length corrupt cached bytes into a
new digest and published one pack. It fails before the replacement and passes
after original-digest validation. Runtime-entry tests cover both actual pack
bounds, memoized descriptor forgeries, ownership, deterministic complete-input
identity, prior references, cancellation, lost later PUTs and exact-byte
collision denial. The focused normal preservation suite passes in 26.465 seconds
and 32 metadata-integrity/publication race tests pass in 571.447 seconds.

One sequential matched comparison uses identical 641-file research sources
except `domain_repack.go`, the same five numeric-only instrumentation sites,
pinned Go, 1 CPU, 2 GiB hard memory, zero swap, Go 450 MiB soft target and unset
GOMAXPROCS. It measures real cold/fresh-replica-warm planning over 2,048 deep-path
metadata files and an actual eight-by-600,000-byte materialized metadata commit
with an opaque prepared child. These are metadata fixtures without retained
upload history or transferred file bytes. Independent strict readers verify all
logical entries, receipts, expiry, opaque references and replay.

| Measured phase | Control wall | Candidate wall | Control allocation | Candidate allocation | Control sampled heap peak | Candidate sampled heap peak |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Cold planning | 5.746013 s | 3.859268 s | 4,716,939,312 B | 3,714,094,424 B | 349,351,904 B | 421,469,024 B |
| Fresh-replica warm | 2.423190 s | 2.414723 s | 2,421,021,576 B | 2,418,926,344 B | 221,511,296 B | 244,914,624 B |
| Materialized commit | 0.403778 s | 0.310447 s | 371,688,480 B | 289,983,688 B | 93,867,496 B | 168,331,368 B |

All provider vectors reconcile: 17 requests (10 GET, seven PUT), one failed cold
GET, unchanged modeled 39,000,000 picoUSD; request bodies increase 116 bytes and
response bodies 220 bytes. Modeled serial/critical percentiles increase three
microseconds. Object/pack counts remain unchanged, with no newly unreachable
packs or file-provider work. Encoding calls fall 64 to 12, gzip input
863,553,747 to 260,053,138 bytes, and measured GC cycles 99 to 58. Whole-process
kernel peak increases 488,366,080 to 496,549,888 bytes, including seeding and
independent oracles; no memory-limit or OOM event occurs. Higher sampled heap
is a real tradeoff. The materialized oracle, excluded from operation timing,
increases 1.199 to 1.315 seconds. This pair proves neither byte neutrality nor
physical concurrent-recovery readiness. Its complete validated comparison
SHA-256 is `d1affe3528be039e063e82caf404fa7eb02413e414c703242e4ec7af53f1cfe9`.

A separate 14-worker deterministic cross-build export/import matrix passes
both winner orders for the same logical intent, actual stale native CAS denial,
lost pack/head response recovery, opposite-implementation restart and strict
reader, freeze, exact zero-PUT replay and changed-intent denial. Its safe summary
SHA-256 is `2f84b3eb553a00fe95bb2f91eb3dbd417a877db077919af8f5a00a55dfc36499`.
It does not substitute for two physical processes sharing a provider.

The existing 4,001-intent large-admission test now also proves a real canonical
checkpoint/raw copy with three reachable authority packs, complete inventory,
shifted native versions, all terminal records/logical versions and the retained
receipt. An ordinary warm-cache read survives deletion of a non-root pack, but
both checkpoint verifiers deny it; the engine must perform the failed provider
GET. Exact wire restoration permits verification/reopen and replay. The initial
normal/race tests pass in 69.070 / 1,051.327 seconds under the unchanged 30-minute
race bound. A reviewed refinement removes only the duplicate standalone restored
verification: the closed-gate `OpenWrites` path must read the restored child
successfully before any gate PUT, then open at epoch + 1. All other copy,
missing-pack, record/version, receipt and replay assertions remain. This refined
normal test passes in 56.052 seconds; the final exact-tree gates remain pending.
Fixture terminalization deliberately uses materialized legacy cancellation,
not the compact batch overlay. The latter's raw-initializing-record interaction
with checkpoint capability draining is a separate unqualified concern.

The fixed R1–R8 checklist remains authoritative. Selection and delivery require
the remaining proof, unchanged real-byte journeys and physical R3, then all
exact-tree gates. Healthy replicas and these component results do not establish
successful customer uploads.

The closure product snapshot subsequently passes all 14 affected real-browser
scenarios in 38.421 seconds, including the 8 MiB-confirmed-prefix recovery that
sends only 17 remaining bytes. The standard 10,604-file journey passes in
357.810 seconds including fixture setup; foreground recovery is 236.609 seconds.
All 2,638 remaining four-byte files transfer exactly once and no completed file
is readmitted or retransferred. Progress remains 75–100% with intermediate
advancement and at most 71 rendered rows. Projection-state call counts remain
8 / 7 / 5 / 2 / 2, without file-provider work. Actual whole-journey HTTP remains
150 control + 2,638 PATCH + 2,638 OPTIONS = 5,426 requests.

Foreground work is 4,410 state, 5,276 file-control and 2,638 data calls; state
writes are 343,455,346 bytes and reads 2,606,915,920 bytes. Restore/auth adds
51 state calls, 61,203 written and 320,134 read bytes, including three expected
not-found reads. Whole-journey totals are 12,375 provider calls, 343,516,549 state
bytes written, 2,607,236,054 state bytes read and 10,552 transferred file bytes.
The full role/kind/subsystem/model vectors and unchanged regression ceilings are
retained in evidence SHA-256
`d247ccd9617eb4ed55bf686490f03742594712f4f2be491699d8e78378899635`.
This is a new snapshot acceptance result, not a matched timing comparison or
the independent physical two-replica R3 result; final-tree R8 remains pending.

The subsequent physical closure run fails the unchanged recovery deadline:
943.41 seconds including setup/cleanup, with owner A incomplete. Final authority,
all-blob/version and owner-isolation oracles are unreached. Observed provider
totals are 24,720 calls, 676,662,325 request bytes and 7,447,617,716 response bytes,
without a guard stop. Kernel peaks are 978,059,264 / 833,200,128 bytes; no OOM is
recorded and automatic owned-container cleanup completes. Exact partial item
counts were not captured and cannot be inferred from provider totals. The
uninstrumented harness disabled application logging and fetched route summaries
only after success, so retained logs do not establish the current CPU bottleneck
or final request-duration distribution. This failure blocks selection and all
final gates/push. It is bound to input manifest
`86dd6074fc93221107d84b4b0d9b7102659aa865e9fa03f2815938462e6dfdc0`,
binary `49a01194eeff1a002db72cd63b83e1919e8c7473995454d430866a5bc0d669c0`
and failed log SHA-256
`59fdd589c851e2fd1482eac22b8e49048957ba59f7b9f3c8c6cab813aea30232`.

## Reviewed diagnostic selection and release continuation

The later sealed diagnostic report is bound by SHA-256
`0318db73b7a4b39a560774a4bd58fab1e25497bbb0604a9265f43ee302ead3f2`.
A real short negative control first verifies bounded failure capture and safe
HTTP request/trace correlation. One sequential matched pair then compares
identical sources except the old versus closure repacker. The control fails
at the unchanged 720-second foreground deadline with 2,200 / 2,400 completion
responses. The closure candidate passes in 440.264 foreground seconds, with
all 5,276 new four-byte transfers exactly once, zero old-file retransfer, all
21,208 final namespace/blob bindings and 15,932 prior portable entry versions
verified, plus strict owner denial and lost-success replay.

The candidate records 277 foreground control requests, two directory batches
and zero scalar stats; the independent provider cut is 25,406 calls,
705,714,931 request-body and 7,386,110,692 response-body bytes. The fixed ceilings
remain 282 / 27,852 / 805,306,368 / 12,188,647,424. Post-foreground oracles,
OPTIONS and HTTP headers are excluded from those provider counters. Kernel
application peaks are 769,982,464 / 815,423,488 bytes, without OOM. Parsed CPU
profiles show remaining pack validation on eligible cache misses/provider GETs
dominates; they do not establish the proportions of identity churn, first
replica access and eviction. No speculative cache change is selected.

The earlier unprofiled closure failure remains an unexplained discrepancy;
one instrumented success does not prove repeatability or identify the October
17 root cause. The latest user instruction lifts the diagnostic-only stop and
authorizes selecting closure repacking, final fixed R1–R8 qualification,
commit/push to draft PR 62 and actual CI verification. Release R3 disables all
CPU/trace/allocation profiling while retaining bounded failure diagnostics,
the original 12-minute foreground wait, routing, provider budgets and resource
limits. The complete final-tree results are recorded against the immutable
commit/tree in PR 62 and the task-local delivery manifest. This source record
states the frozen acceptance contract; it does not predeclare a passing gate.
No merge, deployment or customer retry is authorized.

Independent source review also identified a missing combination of two covered
behaviors: a late admission reply after cancellation/retry and omitted oversized
capabilities. The new actual-router browser regression holds an unchanged
successful three-item response containing two real capacity deferrals and one
usable capability. It proves every local attempt identity is replaced before
release. Restoring the old capability-only cleanup makes it fail in 0.775
seconds because an original outer upload ID remains active. Cleanup by the
retained outer ID passes in 0.772 seconds: all three originals are aborted,
old-attempt data requests are zero and the three replacements each transfer
four bytes exactly once. The independent engine verifies terminal states,
namespace, signed downloads and persisted ledger. Before/after log hashes are
`aca622067262b841e950039b06a5eecd6b2bbe7ce755a6b2a7989acb00e6e275`
and `2900f2e7a47aa8dd461b317e8a85ed6b4fae727413b2fa3ff3f1083724aaa4fc`.
This test is included in the final frozen focused browser and complete coverage
selections; no customer source or queue is used.
