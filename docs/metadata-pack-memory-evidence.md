# Metadata pack memory amplification, 15 October 2026

## Finding and limits of attribution

Base/deployed build: `61e8e5574eab2a2534140a1c41a16b318c615bb6`, including
PR 59. That change handles encoding overflow during large admission; valid
stored packs still require bounded expansion and authentication on reads.

The supplied production observations show about 50 simultaneous `upload.status`
operations per pod, 42/19 active `metadata.pack.decode` operations, heaps rising
from 5–10 MB after startup to about 433–434 MB, and repeated OOMKilled restarts
under 512 MiB container limits. Nodes have ample memory and no MemoryPressure.
The allocating path is confirmed in source and reproduced with synthetic
metadata:

`UploadStatus -> uploadStatus008 -> portableUpload -> newNamespaceStore/loadView
-> newConsistencyDomainTreeSession -> loadPack -> DecodeDomainPagePack`.

Each request creates a separate store/session. The existing cache and
singleflight only coalesce workers inside that session. Fifty independent
requests therefore read, inflate, parse, authenticate every page and recompress
the same metadata pack fifty times. Wire size substantially understates the
expanded working set. The strict JSON decoder also scans duplicate tokens and
retains decoder buffers before the existing exact canonical-wire comparison.

This proves a backend amplification mechanism consistent with the incident;
there is no production heap dump establishing the precise contribution of each
allocation to the killed process. No customer file bodies were opened. The
308.7 GiB data total is not metadata heap size.

## Reproducer and comparison

`TestMetadataPackConcurrentRequestsShareReadAndDecode` starts fifty independent
namespace request sessions with a deterministic provider barrier. Its valid
synthetic pack has eight pages and 30,946 wire bytes of repetitive control
metadata. No sleeps, external service, file content or credentials are used.
The test first failed before production edits (50 reads/decodes). The finalized
larger fixture was also copied into a clean archive of deployed main and failed
with the same count. The selected implementation returns the same authenticated
metadata through one read/decode. It retains no idle decoded-pack cache.

The same isolated compiled test binaries, with default Go runtime settings,
measured 662,516 KiB maximum resident set on main (646.99 MiB) and 55,288 KiB
on the fix (53.99 MiB). A temporary standard-library Go launcher ran each
binary independently and read Linux child `Rusage.Maxrss`; build-tool memory
is excluded. Main exceeds a 512 MiB container limit in this synthetic case.
The failing count assertion remains the regression oracle; these RSS samples
are supporting evidence, not a portable or timing-sensitive test threshold.

The independent `BenchmarkDomainPagePackDecodeMetadataAmplification` has eight
pages, 128 entries and 36,903 wire bytes. Pinned Go 1.26.9 on x86_64-linux,
three measured iterations:

| Decoder | Bytes/op | Allocations/op | Time/op |
| --- | ---: | ---: | ---: |
| Deployed main | 37,531,645 | 6,826 | 43.4 ms |
| Direct unmarshal with exact canonical verification | 22,960,720 | 690 | 23.9 ms |

These are total allocated bytes per decode, not retained heap or peak RSS.
Race instrumentation measures 50.9 MB/op on the selected implementation and
is not a production memory estimate. The measured normal/race allocation
ratchets are 26/56 MiB. The finalized normal ratchet fails on main at 37,530,881
bytes/op and passes on the selected implementation.

Reproduction commands from the candidate Nix shell:

```text
nix develop --command go test ./internal/portable -run '^TestMetadataPackConcurrentRequestsShareReadAndDecode$' -count=1 -v
nix develop --command go test ./internal/storageformat -run '^TestDomainPagePackDecodeRetainsMeasuredAllocationImprovement$' -count=1 -v
nix develop --command go test ./internal/storageformat -run '^$' -bench '^BenchmarkDomainPagePackDecodeMetadataAmplification$' -benchmem -benchtime=3x
```

## Selected mechanism and preserved guarantees

One coordinator per engine shares only concurrent reads of the complete
canonical immutable pack key. The namespace and routed-state stores created
by that engine use this coordinator. Finished entries are removed immediately;
heads, successful idle packs and failures are never retained. Independent
engines/backends have separate coordinators. Read-only pages can be shared:
tree rewrites copy entries/children and lookup results copy value bytes.

One process-wide cancellable slot covers expansion, parsing, member validation
and canonical verification, including reads of unrelated packs and the writer's
post-write verification. Waiting does not allocate expanded metadata. This
bounds simultaneous decoder working sets to one existing 32 MiB expanded
envelope; it does not claim a 32 MiB total process heap ceiling. Multiple
unrelated cold packs may now queue. No new background goroutine or detached
provider request is introduced. A canceled leader releases its provider work;
healthy waiters retry with their own contexts. Canceled waiters leave without
canceling a healthy leader. Decode checks cancellation before admission, during
expansion, after parsing and before returning validated authority.

Direct `json.Unmarshal` replaces the full strict token scan and decoder-owned
copies only here. Existing canonical gzip re-encoding must still match the
original bytes exactly, after every page digest and binding is verified.
Consequently duplicate/unknown fields, alternate case, invalid UTF-8, whitespace,
trailing content and non-canonical compression still fail closed. New denial
tests exercise both outer and nested ambiguous fields.

| Guarantee | Proof |
| --- | --- |
| One concurrent read/decode of the same pack | Fifty-session barrier regression |
| Scope and isolation | Owner, domain-kind and pack-ID separation; per-engine coordinator |
| Cancellation and retry | Canceled leader, canceled waiter, pre-canceled read, queued decode and every decode cancellation boundary |
| No cached error or idle history | Corrupt shared read fails all callers; fresh retries fetch again; finished registry is empty |
| Canonical authority | Existing format/corruption suite plus ambiguous JSON denial matrix |
| Atomic publication, lost-success, stale-writer and freeze denial | Existing domain/fallback, replica, migration and portability gates remain required |
| Provider economics | One GET per overlapping identical cohort; no new writes, file calls, temporary objects or retained objects; existing deterministic provider-budget gate |

Canonical bytes, schemas, ID derivation, head CAS, idempotency, migration ledgers,
format limits and recovery rules do not change. No storage epoch is required.

## Alternatives and remaining risks

Per-decode allocation reduction alone retains the request-count multiplier.
Coalescing alone permits unrelated packs to expand simultaneously. A durable
or unbounded process cache would retain historical metadata and complicate
corruption visibility, eviction and memory accounting. Increasing format or
application memory limits leaves the amplification intact. The selected
combination shares active immutable work and bounds expansion without changing
storage semantics or adding a cache service.

The slot bounds decoders, not every request allocation, compressed provider
response, materialized mutation, page-map lifetime or other subsystem. Read
coalescing ends when the leader finishes; a later wave reads again. Independent
replicas still perform independent reads. Production OOM resolution requires
post-deployment telemetry and cannot be claimed from these tests. The separate
deployment memory-limit mitigation is outside this application change.

## Separate browser observation

`internal/web/ui/js/transfers.js` resumes a task with an existing upload ID by
issuing a status GET before requesting its resume capability. The ordinary
queue allows up to the configured 100 tasks, so browser recovery can produce
the observed status cohort. That GET also omits the task controller's signal,
although the subsequent resume POST includes it. This is a separate frontend
request-budget/cancellation finding; frontend behavior is unchanged here.

Safari's excessive-browser-memory reload does not establish a Go heap leak.
The source reader already has a separate 32 MiB shared chunk budget, which is
not a bound on browser/native file, ledger, DOM or other browser memory. A
Safari reproduction/profile is still needed to attribute the browser failure.
No customer retry, production restart or deployment was performed for this work.
