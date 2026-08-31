# Smart-upload planning evidence

## Outcome

Duplicate-aware upload planning is implemented without routing stored or local
file bodies through the Go service. The browser first asks a cheap size question,
starts unambiguous uploads immediately, and computes MD5 plus CRC32C together in
one local pass only for ambiguous files. An exact match in the owner's live
namespace is either skipped at the same destination or reused by the existing
logical-copy transaction. The immutable blob is referenced; GCS receives no
file copy, rewrite, download, upload, or delete request for reuse.

The UI offers four explicit strategies after file/folder selection or drop:

- **Smart merge** reuses identical content, uploads new names, and renames a
  changed same-name destination.
- **Replace changed files** reuses identical content and replaces a changed
  same-name file using its logical-version precondition.
- **Only add new names** leaves existing destination names unchanged while
  still reusing identical content for absent names.
- **Keep both** bypasses planning and retains the existing unique-name upload
  behavior.

## Data flow and persistence

1. One control request carries up to 1000 transfer IDs, virtual destinations,
   and sizes. The service queries a derived owner projection plus the pinned
   namespace snapshot.
2. Unique-size items enter the ordinary direct-upload queue immediately.
3. At most two dedicated browser workers process ambiguous `File` objects in
   4 MiB chunks. Each chunk updates both MD5 and CRC32C before it is released.
4. One exact request carries up to 1000 completed fingerprints. Its opaque token
   pins the owner, projection, live namespace root, and expiry.
5. The response exposes only an action and, for reuse, a virtual source path
   plus portable logical version. It never exposes provider keys, native
   versions, or stored checksums.
6. Reuse is one existing batch-copy mutation. Uploads that remain necessary use
   the existing browser-to-provider capability and completion path.

The IndexedDB transfer ledger stores strategy, phase, completed fingerprints,
target logical preconditions, and outcome. It does not store bytes, capability
URLs/headers, provider identifiers, session material, or absolute local paths.
Connection loss with the same in-memory `File` reruns the cheap size phase and
reuses a completed fingerprint. After refresh, any reacquired source reruns the
size phase and recomputes an ambiguous fingerprint: matching name, size, and
modification time is not proof of byte identity. The browser asks for the source
only when no safe file handle is available. Cancellation terminates the active
worker so no unresolved hashing promise or busy slot survives.

## Projection update model

Upload planning has a dedicated rebuildable projection ID under the existing
projection format. It is not authoritative and is excluded from portability
checkpoints, so adding it does not change schema-010. A cold build streams live
namespace metadata into size and exact-source postings with bounded page memory.
Normal refresh compares the prior and current immutable live roots. Equal roots
and equal directory subtrees are skipped; only removed or added file postings on
changed branches are applied to the persistent tree. Concurrent builders use
CAS, reload after contention, reject inconsistent equal revisions, and cannot
publish a source revision older than an already-published head.

The size response is issued only when its namespace view equals the projection's
source root. The exact phase reopens current state and rejects a changed root.
This makes a plan an optimistic snapshot: ordinary concurrent mutations cause a
cheap replan, never stale reuse or cross-owner disclosure. Checkpoint garbage
collection may deliberately remove the pinned rebuildable page; that expected
disappearance also returns a replan conflict rather than a terminal missing-file
response.

## Provider budget evidence

The GCS regional-standard-flat pricing/latency fixture produced these exact
ratchets on the deterministic schema-010 store:

| Workload | Requests | Modeled cost (USD) | p50 | p95 | p99 | State request types | File requests |
|---|---:|---:|---:|---:|---:|---|---:|
| Cold index, 256 files | 15 | $0.0000336 | 360.430 ms | 1.077430 s | 2.652430 s | GET, PUT | 0 |
| Incremental index, one added file | 14 | $0.0000240 | 325.627 ms | 975.627 ms | 2.405627 s | GET, PUT | 0 |
| Warm size plan, 1000 items | 6 | $0.0000024 | 134.998 ms | 392.998 ms | 962.998 ms | GET | 0 |
| Warm exact plan, 1000 items | 4 | $0.0000016 | 90.930 ms | 262.930 ms | 642.930 ms | GET | 0 |

The tests compare one-item and 1000-item warm calls and require the same provider
request count. CPU work still scales with the submitted items, but provider
round trips, modeled request price, and aggregate request latency do not. The
append-only `005-smart-upload-planning` fixture ratchets exact totals and fails
on drift. Logical reuse composes with the already-ratcheted same-owner batch-copy
path, which has zero file-provider requests.

## Automated evidence

- `TestUploadPlanningUsesSizeBeforeExactProviderFingerprint`
- `TestUploadPlanningDetectsExactTargetAndRejectsCrossOwnerToken`
- `TestUploadPlanningRejectsStaleSnapshotsAndIncrementallyRemovesTrashedFiles`
- `TestUploadPlanningWarmRequestBudgetIsIndependentOfBatchCardinality`
- `TestIntegrationUploadPlanningRoutesAreStrictOwnerScopedMetadataQueries`
- `TestE2EUploadPlannerHashesMD5AndCRC32CInOneWorkerPass`
- `TestBrowserSourceKeepsSecretsEphemeralAndUntrustedTextOutOfHTML`

The deterministic evidence uses no cloud credential, network service, stored
object body read, or provider file operation.
