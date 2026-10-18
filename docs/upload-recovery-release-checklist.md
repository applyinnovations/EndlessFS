# Upload recovery release acceptance

This is the fixed acceptance checklist for the corrective slice. It supersedes
any inference of readiness from the earlier chronological evidence record.
Every required row must pass on the final frozen tree. An earlier-tree pass is
supporting evidence, not a pass for the current candidate. Status recorded
2026-10-18 UTC: **closure candidate selected; final release qualification pending**.
The results below preserve their original source and workload boundaries. Final
frozen-tree acceptance, gate and CI results are recorded against the immutable
commit and tree in PR 62 and its task-local delivery manifest; this checklist
does not predeclare those results.

The published baseline is commit
`6db3cd38d1e8932970fefc7d1b6bfd154028abe9`, tree
`718a07ebdb06c7586c132ac185cf2c2225a48352`, draft PR 62.
The current follow-through branch starts from that baseline. No merge,
deployment, customer-file-body access or customer-queue mutation is authorized.
The latest instruction explicitly authorizes selecting from the sealed
diagnostic evidence, completing R1–R8, reviewing, committing and pushing the
qualified continuation to existing draft PR 62, and verifying its actual CI.
The earlier diagnostic-only stop has been lifted. No overlapping heavy runs,
duplicate harnesses, blind repeats or weakened limits are authorized.

## Required release blockers

| ID | Observable pass condition | Current evidence and status |
| --- | --- | --- |
| R1: valid smart planning | Real size and fingerprint planning succeeds for synthetic 10,604-file authority with 7,966 already complete and 2,638 remaining, including valid long names, deep paths, existing nested folders, and cold/rebuilt/warm projections. Malformed or corrupt authority still fails closed. Actual compressed and expanded packs retain their 4 MiB / 32 MiB limits. | Baseline regression and real planning tests pass; precise historical October 17 cause remains unknown. **Current tree pending.** The 256-existing-file / 1-versus-1,024-query component fixture is described at its actual scale and is not the 10,604-file proof. |
| R2: whole user journey | Initial upload and persisted partial recovery traverse real smart planning, directory preparation, admission, native transferred bytes and completion. Recovery includes reconnect, retry, refresh, logout/passkey login and source reselection. Independent authority, membership, logical-version, checksum and byte oracles prove all 2,638 remaining synthetic 4-byte files transfer once and none of the 7,966 completed files is readmitted or retransferred. | Baseline single-owner whole journey passes: 10,552 new bytes, 2,638 PATCH plus 2,638 preflight requests, 150 control requests. **Current tree pending.** Expired-session classification and keep-both fixtures cannot satisfy this row. |
| R3: selected concurrent resource profile | Two independent owners complete R2 concurrently through two independent engines/replicas, each at 1 CPU / 2 GiB hard memory / 450 MiB Go soft limit / unset GOMAXPROCS and the selected 50-second control deadline. Retain the existing deterministic routing, unchanged 12-minute recovery wait, owner isolation, lost-success replay and opposite-replica oracles. Zero OOM, duplicate transfer, authority loss or abandoned active work. | **Final release run pending.** Earlier baseline, pre-closure and unprofiled closure runs failed; all remain failures. The later matched diagnostic control fails at 720 seconds with 2,200 / 2,400 completion responses, while the closure candidate passes in 440.264 seconds with all byte/version/owner oracles. The earlier 943.41-second unprofiled closure failure remains unexplained. Release validation disables CPU/trace/allocation profiling while retaining bounded failure diagnostics and unchanged limits. Single-owner successes do not satisfy R3. |
| R4: bounded whole-lifecycle work | Directory preparation uses bounded pages, parent dependencies and independent siblings with per-item partial success, safe retries and cancellation. Planning, admission, status, resume, completion and cancellation avoid scalar per-file control explosions. Fresh independent lifecycle pages remain at most 100 items and directory concurrency at most four pages. Retain exact historical legacy-cohort key/order/membership and its separate protocol; do not repage a frozen legacy intent. Preserve 1 MiB request/reply bounds; capacity subdivision retains all correlated IDs, terminates finitely and never truncates capabilities or retries completed siblings. Preserve the full provider vector and failed/replayed-operation accounting. | Baseline batching and provider gates pass. New real-router long-path / compact-response and browser finite-subdivision cases pass focused normal/race checks on earlier follow-through snapshots; mixed unknown action and oversized capability regressions are reproduced and corrected. **Current tree pending.** |
| R5: preparation and errors | Whole-folder Preparing credits completed work immediately, advances remaining preparation and reaches 100% on initial upload/reconnect/retry/resume. For the representative recovery it never falls below 75% and shows intermediate progress. Render at most the existing 72-row window. Clear obsolete per-item and group current errors after reconnect; retain only useful bounded history (three entries, message 240 characters, code 80 characters). Independent partial successes retain their progress. | Baseline single-owner progress/rendering passes (observed 71 rows). **Current tree pending.** The failed diagnostic counted 538 / 338 items with nonempty message/code but did not capture categories. Those counts equal exact-pending counts; source deliberately uses the same field for `Checking for matching content.` Public reconnect clears old errors. The earlier inference of stale reconnect messages is withdrawn. Assert closed `source_required` and obsolete reconnect text directly, allowing current informational preparation messages. |
| R6: safe diagnosis | Failure logs expose bounded closed stage/reason, operation, result, duration, build and trace/request correlation. No raw private errors, paths, filenames, user IDs, storage keys, credentials, capabilities or bodies are logged. Request IDs remain outside metric labels. Test redaction/cardinality, queued metadata-decode admission, real socket/flush failures, cancellation and panic behavior without duplicate terminal observations. | Baseline diagnosis passes. Follow-through focused normal/race and actual HTTP boundary checks pass on earlier snapshots. **Current tree pending.** A successful buffered Write is not proof of final transport delivery; explicit final flush has a documented boundary. Historical logs cannot retrospectively reveal the October 17 reason. |
| R7: unchanged integrity and replay | Preserve every member's validation, canonical wire bytes, immutable identity/collision denial, owner binding, original head CAS/freeze, cancellation, restart, migration and mixed-replica retry guarantees. New encoder implementation must equal the previous encoder byte-for-byte, return owned output, reject partial/repeated writes, and preserve capacity error categories. No validation or actual compressed-size check may be skipped. | Earlier encoder, incremental-decode, closure preservation and mixed-writer proofs pass at their bound snapshots. The matched diagnostic supports selecting closure repacking with its measured memory/byte tradeoff; it does not qualify the final tree. Final normal/race/migration and release binding remain required. |
| R8: exact-tree gates and delivery | Freeze/stage all candidate sources; run focused Nix checks and diff check, then ordered `pr-check`, `security-freshness`, `nix flake check --print-build-logs`, `test-coverage`; also unfiltered `test-migration` and `test-provider-budget`. Preserve 85% repository / 95% sensitive / 98% migration thresholds. Commit only that exact passed tree, push, update PR and own the actual CI verdict under the unchanged 3,600-second deadline. | Baseline exact-tree local gates and CI pass (CI 3,283 seconds). **Current tree pending; push blocked.** Full final gates follow successful R1–R7 in the authorized sequential validation. PR 62 still points to the baseline commit. |

R2's existing single-owner request ratchets remain unchanged: foreground state
calls at most 5,962, writes 376 MiB, reads 5,748 MiB; restore/auth state calls at
most 53, writes 64 KiB, reads 8 MiB; foreground control requests at most 140 and
restore control at most 12. Exactly 2,638 native begins and verifications are
required, with replay progress/resume counted separately. The demonstrated
foreground route composition is 27 admissions, 27 fingerprints, 28 sizes, one
directory preparation and 27 action pages. Cold/rebuilt/warm/other-replica/repeat
planning state-call ceilings remain 8 / 10 / 5 / 5 / 2. These are measured selected
design regression ratchets, not claims of optimal provider economics. The
concurrent run must report its complete vector separately, without silently
doubling or changing the single-owner assertions.

R5 distinguishes explicit source reselection from automatic permission-granted
stored-handle restoration. A separate native-OPFS regression reproduced one
stale `source_required` code while the first real size response was held; it
recorded zero obsolete-message and unbounded-history counts. Code-only persisted
state reproduced the same defect. The fix preserves bounded history and clears
only satisfied source-required state on nonterminal transfers with a restored
File. Both cases now pass through real fingerprint planning, admission, four-byte
transfer, completion and authority checks (2.294 seconds combined). This separate
reproduction does not identify the earlier aggregate 538 / 338 categories.

The one sequential encoder-only comparison passes integrity: all whole/per-phase
modeled provider aggregates and encoded lengths match across 32 phases. It
records 2,576 provider requests, 47,420,952 written and 170,135,839 read bytes;
600 actual new four-byte transfers and 8,566 final authoritative files. Allocated
bytes fall 15,749,489,744 to 14,796,279,744 (6.05%) and GC cycles 202 to 190
(5.94%). Foreground time changes 34.727290 to 34.442200 seconds (0.82%), sampled
CPU 34.54 to 34.13 seconds (1.19%). This is a modest allocation improvement, not
evidence of a material latency or concurrent-recovery fix. Raw ordered events
are not exported/compared by this model. The comparison artifact SHA-256 is
`9cbe7b06b26554712a95698df2200b29f3cd9a5ce6761e1ce4c9d5f897a60544`.

Current follow-through acceptance before final freeze: the native stored-source
regression passes both message-plus-code and code-only cases; 14 focused real
recovery scenarios pass in 38.532 seconds. The standard 10,604-file browser
journey then passes in 536.854 seconds, including 410.758 seconds of foreground
recovery after real refresh/logout/passkey login. All 2,638 new four-byte files
transfer once, with zero completed-file retransfer. Actual HTTP remains 150
control + 2,638 PATCH + 2,638 OPTIONS = 5,426 requests. Foreground provider counts
are 4,402 state, 5,276 file-control and 2,638 data requests; foreground state
writes are 347,959,131 bytes and reads 2,876,272,288 bytes. Restore/auth adds 51
state calls, 61,221 written and 320,167 read bytes. The final rendered window is
71 rows. Cold/rebuilt/warm/other-replica/repeat planning uses 8 / 7 / 5 / 2 / 2
state calls with no file-provider work. These source-snapshot results support
R1/R2/R4/R5; they do not satisfy the separate physical two-replica R3 or final
exact-tree R8 gates.

The current follow-through R3 attempt subsequently **failed** the unchanged
12-minute recovery wait (945.09 seconds including setup and cleanup). Both
owners reconnected their 2,638 sources, but final authority, all-blob/version and
strict foreign-owner oracles were not reached. Exact partial completion counts
and a current CPU profile were not captured; earlier-attempt counts must not be
substituted. Provider guard totals were 18,333 calls, 505,139,210 written and
5,403,566,817 read bytes, without a guard stop. Application kernel peaks were
795,504,640 and 863,559,680 bytes, with zero OOM events. Automatic owned-resource
cleanup completed. Source manifest SHA-256
`e9b6114b0d43cfc3bcc9e1aa608e650faebd749567cfef7807e603e17548aded`
and binary SHA-256
`f5c49c0da37b848f0afe327b3181aedc817147b70aaef18ee1f08a54eff27d0c`
bind that failed attempt. Validation stopped to address repeated-prefix
repacking; final R8 gates and delivery remain blocked.

## Conditional qualification and explicit exclusions

The selected deployed preview profile resolves its control deadline to 50
seconds. The default profile uses 30 seconds. Observed size requests reached
35.36 seconds in the constrained single-owner success and 42.49 seconds in the
concurrent diagnostic. The 30-second profile is therefore **unqualified for this
workload** and blocks any deployment/readiness claim for that profile. It is not
permission to increase its timeout. This release checklist targets the selected
50-second profile and must state that scope plainly.

Optional future qualification includes workloads beyond the selected two
concurrent owners, different node contention, live cloud-provider behavior,
full 308.7 GiB file-body throughput and other resource profiles. Synthetic
4-byte payloads prove lifecycle bytes and no retransfer, not large-file network
throughput. These exclusions cannot excuse an R1–R8 failure. Actual customer
retry and production confirmation require separately approved deployment and
customer action. Healthy replicas never establish successful uploads.

## Smallest next change and bounded validation proposal

The completed exact-baseline diagnostic captured one 45-second late window.
App0 consumed 45.20 CPU seconds, allocated 18.21 GB and completed 658 GC cycles;
app1 and the external provider were nearly idle. Projection repacking accounted
for 22.44 of 44.98 sampled CPU seconds. Nested deterministic gzip accounted for
15.41 seconds; these cumulative frames overlap and must not be added. Source
inspection confirms that each added page encodes and compresses the entire
growing candidate pack. Deterministic routing can concentrate both owners'
planning on the same replica. This supports an application encoding bottleneck
in this window; it does not prove whole-journey causality or a matched slowdown
against the differently configured single-owner run.

The selected small patch removes the extra expanded JSON buffer copy before
unchanged synchronous gzip. Go 1.26.9's encoder supplies one borrowed full-value
write plus newline; the writer retains only owned compressed output and fails
closed if that contract changes. Full validation, gzip bytes, capacity checks
and pack cuts remain unchanged. Repeated compression remains a known cost.
Its matched observation above establishes the narrow allocation benefit.

The user authorized only this sequential validation at 05:30 UTC:

1. Run the focused encoder oracle/ownership/capacity and relevant integrity
   tests, including race checks. Stop on any failure.
2. Run one sequential matched control/candidate comparison using the existing
   43-directory / 7,966-completed-history / six 100-file actual-byte fixture.
   Freeze both source manifests; the control must be the current tree with
   only this encoder-copy change reverted, so unrelated follow-through fixes
   cannot be credited to this optimization. Preserve the
   unchanged 1 CPU / 2 GiB / 450 MiB / 600-second cap and exact provider-phase
   vectors. Compare CPU, allocation, foreground time and kernel peak. Do not
   launch unrelated ready-planning, filter or cache experiments. Select this
   patch only on preserved guarantees and measured benefit.
3. Run the authored R5 per-item reconnect-error assertions and fixed R1–R7
   acceptance, including the unchanged R3 concurrent run. Report each result
   and the complete request/provider vectors. A failed R3 remains a blocker;
   reconsider the measured repeated-prefix encoding mechanism instead of
   starting broad exploratory runs or relaxing acceptance.
4. Only after R1–R7 pass, freeze the tree and perform R8 once. Any subsequent
   source change invalidates that exact-tree gate evidence.

Record actual results separately; authorization is not a passing result. Stop
the sequence on failure and diagnose it before proceeding.

The 06:22 UTC pause was explicitly lifted at 06:44 UTC after the separate AWS
work completed. The user resumed the fixed checklist and evidence-backed fixes,
with sequential heavy runs, unchanged deadlines/gates and no merge/deployment.
After the current R3 failure, the next prototype replaces per-added-page
prefix encoding with owned, validated postorder closure groups. Complete
immutable source/mutation/preparation/root input, fixed algorithm policy and
stable original ranges determine retry identities. Only typed actual capacity
failures refine groups; every final group is rechecked before any new-layout
PUT. Existing opaque dependency flushing and original head CAS remain intact.
Focused actual wire/expanded-overflow, corruption/ownership/identity, later-PUT
retry and collision proofs pass, including 32 metadata/publication race tests
in 571.447 seconds. The targeted comparison and deterministic cross-build
same-intent matrix pass, with complete evidence in the corrective acceptance
record. Encoding and CPU work decrease; sampled heap and kernel peak increase.
The genuine 4,001-intent canonical multi-pack checkpoint/raw-copy proof passes
normally and with the race detector in 69.070 / 1,051.327 seconds. The subsequent
reviewed restoration-order refinement passes normally in 56.052 seconds; it
requires a successful restored-child read before any reopening gate PUT and
retains every other assertion. This mechanism remains unselected until unchanged
real lifecycle/resource acceptance supports it. Final exact-tree R8 gates remain
mandatory, including the refined test.
No universal logarithmic encoding-work bound or October 17 attribution is
claimed by this prototype.

The closure snapshot now passes the 14 focused browser scenarios (38.421 s) and
the standard 10,604-file journey (357.810 s including setup, 236.609 s foreground).
All 2,638 remaining four-byte files transfer once; zero completed files are
readmitted/retransferred. Preparing remains 75–100%, with at most 71 rows.
Whole-journey HTTP is unchanged at 150 control + 2,638 PATCH + 2,638 OPTIONS.
Foreground state/file/data calls are 4,410 / 5,276 / 2,638; state writes/reads are
343,455,346 / 2,606,915,920 bytes. Restore/auth adds 51 state calls and
61,203 / 320,134 bytes. Full vectors and model are retained under evidence
SHA-256 `d247ccd9617eb4ed55bf686490f03742594712f4f2be491699d8e78378899635`.
Physical R3 and final exact-tree R8 are still required; earlier row statuses
must not be read as final-tree passes.

The closure physical R3 attempt then **fails** the unchanged 12-minute recovery
wait (943.41 s including setup/cleanup), owner A incomplete. Final authority,
all-blob/version and foreign-owner oracles are not reached. Both owners had
reconnected all 2,638 sources after refresh/logout/passkey login. Provider totals
are 24,720 calls, 676,662,325 written and 7,447,617,716 read bytes; no guard stops.
Application kernel peaks are 978,059,264 / 833,200,128 bytes with zero OOM; owned
containers are removed by automatic cleanup. Exact partial file counts and a
current CPU profile were not captured. Disabled test-harness logging and
success-only route snapshot collection prevent retrospectively reconstructing
the route distribution. Qualification stops; the closure mechanism remains
unselected at that point, and no final gates, new commit, push or CI were started.

## Resumed selection from sealed diagnostics

The subsequent diagnostic-only session is complete and independently reviewed.
Its sealed report SHA-256 is
`0318db73b7a4b39a560774a4bd58fab1e25497bbb0604a9265f43ee302ead3f2`.
The genuine short negative control proved actual failure capture, HTTP 400
request/trace correlation, complete bounded operation metrics, redaction and
owned cleanup before one sequential matched full pair. The two full sources
differ only in the repacker implementation and share identical diagnostic
hooks, routing, fixtures, deadlines and resource limits.

The old-prefix control fails at 720.000944 seconds of foreground recovery,
with 2,200 / 2,400 completion responses. The closure candidate passes in
440.264 seconds of foreground recovery (680.23 seconds including setup and
verification). Each owner transfers exactly 2,638 new four-byte files once;
none of the 15,932 previously complete entries is readmitted or retransferred.
Independent oracles check all 21,208 final authority entries and stored blob
checksums/bindings, all 15,932 prior portable entry versions, six signed download
samples, strict owner denial and opposite-process lost-success replay.

The candidate uses 277 of the unchanged 282 foreground control-call budget,
25,406 of 27,852 provider calls, 705,714,931 of 805,306,368 request-body bytes and
7,386,110,692 of 12,188,647,424 response-body bytes. It makes two directory
batches and zero scalar stat calls. These provider counters exclude OPTIONS,
HTTP headers and post-foreground independent oracles. Application kernel peaks
are 769,982,464 / 815,423,488 bytes, with no OOM or hard-limit event. The 450 MiB
Go target remains a soft limit. No live-provider cost or large-body throughput
claim follows from these numbers.

The candidate's sampled remaining cost is eligible cache-miss/provider-GET pack
validation: decoding accounts for 71–73% of cumulative sampled CPU and
wire-proof validation for 58–59%; these overlap. New pack identities, first
replica access and eviction are not apportioned. No cache optimization is part
of this selection. This supports closure repacking as a bounded corrective
mechanism with measured allocation/encoding benefits and explicit memory and
metadata-byte tradeoffs. It neither explains the October 17 invalid response
nor erases the earlier unprofiled same-product failure.

The latest user instruction authorizes completing the release. The selected
candidate now proceeds through final R1–R8. R3 must run without CPU, trace or
allocation profiling, retaining the reviewed bounded failure collector and
native HTTP observer. Its shorter negative control must prove profiles absent.
Any material diagnostic/release discrepancy must be reported and diagnosed
without relaxing the fixed profile. Final results belong to the exact frozen
tree, then the immutable commit and its actual CI; no merge or deployment is
authorized.
