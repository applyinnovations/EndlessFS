# EndlessFS v0.7.4 folder pressure test — 2026-10-11

Production: f9ec2d48fb0da9b04b4f00a67a1b3e059e34412d, OCI digest 8b77bfe93c7820c9b8412fea124900bb3dfc16d4e35938f73f8449baacfcad71.

## Result

The user dropped a folder through Chrome into an isolated test destination. The agent clicked Start upload with Smart merge selected. All 119 discovered files completed: 56 existing identical files were reused and 63 files transferred through direct GCS upload sessions. The resulting folder reports 335.6 MiB and 119 files. This is a mixed camera folder, not proof that 119 RAW images were decoded. Root inventory includes seven ARW files and metadata files, with 110 items under a nested folder.

No new failures appeared: completed history increased from 18 to 137, total history from 14,300 to 14,419, while the historical failed count remained 14,229. The entire upload plan and publication sequence completed in about 57 seconds, measured from server size-plan start to completion response. Admission ended 05:17:56.628 UTC and publication ended 05:18:43.561 UTC.

## Request evidence

Loki recorded the complete control-plane sequence:

| Route | Status | Server duration |
|---|---:|---:|
| POST /api/v1/uploads/plan/sizes | 200 | 1.157 s |
| POST /api/v1/uploads/plan/fingerprints | 200 | 0.318 s |
| POST /api/v1/uploads/batch | 201 | 5.465 s |
| POST /api/v1/uploads/batch/complete | 200 | 1.804 s |

Captured direct GCS responses were successful 200s with expected intermediate 308s. No CORS denial was captured. Response-body cancellation produced ERR_ABORTED events after successful provider responses, as in the previous smoke test. An unassociated ERR_CACHE_MISS was also present. Browser capture missed requests already in flight before its refreshed Network enable; native DevTools recorded the full interval, and Loki supplied the missing control-plane evidence. No raw signed URLs, request bodies, image contents or personal filenames are included in the sanitized network report.

No HTTP 5xx logs matched during 05:17:20–05:20:35 UTC (149 log lines scanned). Neither replica restarted. Sampled container working-set maxima were 82,026,496 bytes (78.2 MiB) and 40,333,312 bytes (38.5 MiB), below their 512 MiB limits. Two-minute average CPU samples were low; these averages cannot exclude short CPU bursts.

## Browser performance

Native Chrome trace: 249.608 s including setup, idle time and post-upload UI interaction; screenshots, resource contents and source maps excluded. DevTools reports INP 62 ms, CLS 0.00, heap upper bound 21.3 MB, scripting 1.188 s, rendering 0.793 s, painting 0.511 s. A Go trace scan found 29,388 renderer main-thread tasks, one exceeding 50 ms, maximum 51.332 ms, and 3.770 s total task time. This is observational production evidence, not a controlled benchmark or an asymptotic proof.

## Findings

1. The global transfer overview combines old failed uploads with the latest successful batch: after success it still showed 1%, Needs attention, and a 66 GiB denominator. The current batch itself completed successfully. Keep historical failures visible and clearable while showing current work independently.
2. Backend allocation profiling over 05:16:45–05:20:00 UTC reports about 1.57 GiB of sampled allocation churn. Callgraph samples attribute substantial cumulative allocation to metadata page rewriting, canonical encoding and pack decoding. These overlapping percentages must not be summed; allocation churn is not resident heap. `leafGroups` and `branchGroups` serialize each growing prefix to find the page limit, giving avoidable quadratic encoding work within each bounded page. Compare exact-boundary replacements and preserve canonical bytes, request economics, fail-closed validation, publication and recovery.
3. No preview child executions occurred during this transfer interval. Upload success therefore does not qualify concurrent thumbnail rendering. Prior smoke testing separately verified preview generation, but this test does not establish its high-volume behavior or the cause of the earlier OOM incident.
4. CSP audit events came from the extension's data-URL favicon badge; no application image was opened in this test. Do not broaden application CSP to accommodate that extension behavior.
5. The user's screenshot also exposed clipped size units, cropped failed-group
   errors, and transfer actions covered by the scrollbar. File columns now use
   intrinsic content sizing, including at narrow widths through their existing
   horizontal table scroller. Errors determine row height; a reserved scrollbar
   gutter and padding keep action targets clear. Virtual-window shifts preserve
   an overlapping item's measured viewport position instead of assuming 60 px
   for every row.

The local native trace can contain capabilities in URLs despite excluded resource contents and must not be attached to a public PR without redaction. Files remain in the user's isolated destination. No originals were downloaded, deleted or modified.

## Implementation qualification

The production test succeeded before these changes; the changes below address
observed overhead, misleading feedback, and a separate recovery defect exposed
by deterministic browser qualification.

Page grouping now encodes the first member with the actual page header, then
encodes each additional member independently. The canonical compact JSON array
adds precisely that member's bytes and one comma; surrounding fields remain
unchanged. The previous mechanism encoded every growing prefix. Both select
the same maximal prefix. Tests cover empty, singleton, 255/256/257 and 1,024-item inventories,
base64 expansion, JSON escaping, UTF-8, byte-limited leaves and branches, and
oversized-single-item denial. No alternate encoder, guessed byte estimate,
cache bypass, schema change or modified digest validation was introduced.

Measured allocations for identical one-KiB values, after warming encoding
caches:

| Items | Pages | Previous allocated bytes | Selected allocated bytes |
|---:|---:|---:|---:|
| 64 | 1 | 3,178,392 | 109,624 |
| 128 | 1 | 12,268,440 | 219,192 |
| 256 | 1 | 47,987,464 | 438,328 |
| 1,024 | 4 | 194,331,360 | 1,753,360 |

The selected measured behavior has a linear regression ratchet with encoder
headroom. This isolates grouping; it does not claim that all application
allocation was reduced by the same percentage. Pack decoding, JSON validation
and deterministic recompression remain visible optimization opportunities.
A full-prefix probe followed by bisection allocated only 1,540,816 bytes for
the 1,024-item common workload, but was rejected by the oversized-first-item
denial test: widening the input from one to 16 equally invalid records raised
allocation from 2,802,096 to 50,366,808 bytes before rejection. It could encode
the whole oversized candidate before noticing the first invalid member. The
selected design rejects at that member, independent of the remaining width,
and retains only a bounded member encoding while counting the page bytes.
The additional 14% common-case grouping allocation buys this resource bound;
the result still removes about 99.1% of the old allocation at 1,024 items.
A hand-counted escaping/base64 estimator was also rejected: actual member
encodings supply those lengths, while the compact-array separator is fixed by
the canonical-json-v1 contract. Work is linear in member bytes rather than in
the sum of all prefix encodings.

The pinned Go 1.26.9 race runtime deliberately discards one in four
`sync.Pool.Put` values (`src/sync/pool.go`), so a race-instrumented encoder does
not have the same warm-pool allocation model as the released binary. The first
full gate caught applying the normal allocation limit to that different model.
An isolated ten-run race measurement observed 310–435 kB at 64 items,
703–782 kB at 128, 1,490–1,717 kB at 256, and 6,114–6,586 kB at 1,024.
Both builds retain linear allocation guards: 3,072 bytes/member plus 16,384
bytes in normal builds, unchanged from the original measured ratchet, and
8,192 bytes/member plus 16,384 in race builds. No test or fault case is skipped.
Three separate 64-item reproductions observed up to 453 kB as well, inside the
same instrumented guard.
The former quadratic prefix algorithm exceeds either guard even at 64 items.

| Guarantee/economic property | Preservation evidence |
|---|---|
| Page boundaries and canonical bytes | Exact maximal-prefix and byte-comparison tests |
| Publication, fencing, integrity and recovery | Existing page validation, pack decoding and head CAS are unchanged; full gates still required |
| Provider requests, transferred metadata, cost and retained objects | Same pages/packs and request paths; all existing provider budgets retained |
| File-body separation and portability | No file-backend or authoritative format changes |
| Browser progress | Regression failed before repair with a successful new upload labeled Needs attention; current work now excludes unrelated terminal history |
| Historical failures and owner scoping | History counts and retry/reconnect/clear actions retained, including after reload |
| Completion replay | Deterministically reversed IndexedDB order failed before repair after the server committed and its response was lost; a persisted ordinal preserves the exact request order |

The completion test also covers an existing single-folder ledger without the
new ordinal, using its existing ordered folder-member list. An old mixed-folder
pending completion lacking an ordinal does not contain sufficient information
to reconstruct arbitrary original interleaving; this change does not invent
an order or weaken server idempotency checks for that ambiguity. New batches
persist the complete order regardless of folder membership.

The overview maintains a separate current-work aggregate and membership set at
structural updates. Progress ticks update it in constant time without rescanning
the complete historical ledger. The source of truth for files and server
transactions remains unchanged. Failed history is never automatically removed.

The layout regression failed before repair with overflowing size/date columns,
72 cropped error rows and 144 controls within the scrollbar area. It passes in
both built-in themes at 1440/1000/768/320 px for file columns and
1440/1000/320 px for transfer errors, with action clearance and forward/reverse
scroll anchoring checked. The rendered window remains at most 72 items. The
earlier fixed-height/optical-offset source assertions are deliberately replaced
by minimum-height density, measured-anchor and gutter assertions plus these
browser geometry proofs; none of the rendering or accessibility guarantees is
removed.
