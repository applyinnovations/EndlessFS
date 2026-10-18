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
