# EndlessFS v1 release notes

EndlessFS v1 provides the single-binary passkey identity system, private Drive control plane, direct capability data plane, trash, read-only public sharing, administration and recovery, accessible embedded browser application, and closed data-only theme system described in [the v1 specification](./v1-specification.md).

The v0.7.0 provider-efficiency release appends schema 011. Bounded
content-addressed domain packs replace per-page request amplification while one
conditional domain-head CAS remains the visibility point. At 10,000 logical
items, composed provider totals are 3 for browse; 5 for copy, move, Trash,
restore, or permanent deletion; 3 for lost-response replay; 5 for size-only
upload planning; and 9 when every size needs a fingerprint lookup. These
namespace operations make zero file-provider requests and never relocate file
bytes.

One browser transaction now admits, completes, or cancels up to 10,000 real
uploads. The unavoidable object/session operations run through 100 workers;
authenticated progress every 1,000 items bounds crash rework, and whole-batch
cancellation publishes one compact bitmap instead of rewriting every admission
record. Each composed lifecycle phase performs 10,014 provider requests rather
than 20,700 admission, 100,000 completion, or 90,000 cancellation requests.
Ready-preview resolution and checkpoint garbage collection are likewise
bounded by the visible window and one authenticated 128-entry plan page.

The `010 -> 011` feature-only migration verifies complete frozen authority,
does not rewrite file blobs, and leaves schema-010 pages readable until their
first eligible packed mutation. Immutable fixtures cover portable-minimal, both
real application writer profiles, and the complete signed-passkey corpus.
Request count, modeled cost/latency, crash, lost-success, corruption,
multi-replica, race, migration, and portability evidence is recorded in
`docs/storage-schema-011-implementation.md`.

The browser upload scheduler now ports the 100-worker shared-queue model from
Google's open-source `upload-cloud-storage` action instead of embedding an
unexplained eight-transfer maximum or guessing from CPU count and estimated
download speed. One 100-file selection uses one batch admission, starts 100
browser tasks, saturates all six HTTP/1.1 fixture connections, sends exactly
100 direct data bodies and completion requests, and refreshes the directory
once after the burst drains. Data Saver reduces the worker count to one;
confirmed-offset recovery, cancellation, bounded retry/backoff, and the rule
that object bytes never enter the Go service remain unchanged. Pinned upstream
sources and licenses are recorded in
`docs/upload-worker-pool-upstream-evidence.md`.

The v0.5.1 upload-concurrency patch coalesces browser multi-file initialization
into the existing bounded 100-item batch route before running direct provider
transfers concurrently. Stable per-item idempotency lets a lost batch response
or reload resume the same provider upload through either initialization route.
The consistency-domain engine now rereads and retries a lost head CAS only
after canonical revalidation proves the winning mutation touched unrelated
keys; genuine same-key conflicts retain exactly one winner. Deterministic
two- and eight-replica tests cover both outcomes, and the Chromium workflow
proves a two-file folder uses one batch admission and no single-item admission.
This patch writes the unchanged schema-010 format.

The clarified v1 storage contract is implemented by one provider-independent
schema-010 authority-conserving epoch over the invariant-aligned state-domain engine. Its canonical typed records,
immutable pages, logical versions, retained outcomes, owner namespace graph,
writer gate, and checkpoints do not depend on provider-native identifiers or
metadata. Deterministic raw-copy tests move only checkpoint-authorized key/body
pairs between independent backends, regenerate every native version, reopen at
a new gate epoch, preserve all logical state, and continue mutations in both
directions without a state migration.

The v0.5.0 recovery release appends schema 010. The released `007 -> 008`
migration enumerated an obsolete `state/` namespace while real schema-007
profile, passkey, session, administrator, invite, recovery, share, preference,
operation, and ceremony authority lived behind persistent `state-indexes` and
`state-versions`. Schema 010 recovers that retained authority under the closed
gate, writes exact source-to-target conservation receipts, fails closed on any
missing source or unequal target, and independently verifies the complete
relation before the shared activation path permits the new writer feature.
Recovery reads state metadata only and never reads or copies file bodies.

Migration qualification now includes predecessor-produced complete application
corpora, not only writer-profile fixtures. The full suffix must preserve every
required logical key and complete a real signed passkey assertion and session
lookup. The migration gate runs both owning packages in full, and the 98%
coverage group includes every numbered migration implementation plus the
ledger. Exact provenance, denial cases, and rollout behavior are recorded in
the schema-010 implementation record.

An ordinary same-domain mutation writes changed immutable pages and
conditionally replaces one authenticated domain head. If a replica disappears
before that replacement it publishes nothing; if the successful response is
lost, any replica reconciles the retained fingerprint-bound outcome. Genuine
cross-domain invariants prepare every participant before one create-only
decision becomes the commit or abort point, after which any replica can finish
the transition. Gate closure resolves transitions, freezes the complete domain
catalog and every registered head, and drains capabilities/leases rather than
sacrificing consistency for availability. Schema-008 generic domains and every
older admission, operation, fence, index, and per-directory authority are
migration-only input, not alternate runtimes.

The v0.4.0 state-architecture release replaces the historical generic mutation
protocol with schema 009. Authority is partitioned by actual invariants into
namespace, owner-identity, owner-jobs, administration, and capability domains.
State values are bound to exact canonical record types; owner-scoped opaque
identifiers remove global scans; sessions use scoped authentication epochs;
registration, authentication, credential, administrator, upload, preview-job,
and duplicate-reconciliation changes publish atomically at their correct
boundary. External upload-provider effects are durable, replayable consequences
of canonical intent. Checkpoint closure drives a resumable, conditional,
checkpoint-bound garbage collector and never reads file bodies.

Exact executable GCS economics ratchets cover every provider-backed production
route. Representative state-provider request counts fall from 72 to 4 for a
one-file move, 100 to 5 for Trash, 103 to 4 for restore, 68 to 6 for upload
completion, and 18 to 2 for StateStore CAS. The 10,000-root Trash fixture uses
125 state requests, one authoritative head publication, no file-provider
request, and no stored-body read. These are deterministic modeled count, price,
aggregate-latency, and critical-path budgets rather than live-cloud latency
measurements; the complete table and model are in the schema-009 implementation
and provider-economics records.

The v0.3.2 upload-recovery patch repairs namespace roots left at the durable committed transition after a replica disappears before finalization. Reads continue to interpret the committed post-state without mutation. Once the recorded owner lease expires, the next upload, copy, move, Trash, delete, or directory-creation mutation performs bounded targeted recovery of that operation and replans from a fresh namespace trail. Recovery never scans the bucket or reads stored-file bodies, preserves active-owner fencing, tolerates one-winner finalization races, and fails closed on unexpected state errors. This patch writes the unchanged schema-006 format.

The v0.3.1 recovery hotfix makes schema-gate quiescence safely fail expired, unsealed predecessor operations instead of recomputing their preparation pages with newer namespace semantics. No visibility root has been published in the preparing state, so the original namespace remains unchanged, the stale worker loses its next CAS, the admission can be removed, and migration continues. Sealed running and committed operations still recover from their immutable step pages. This patch writes the unchanged schema-006 format.

The v0.3.0 namespace-efficiency release makes folder copy, move, Trash, restore, and permanent deletion independent of descendant count. Schema 006 represents a directory entry as a pointer to an immutable snapshot plus its physical metadata area; a namespace mutation attaches or detaches that one snapshot and rewrites only affected ancestor paths. Exact bytes, file counts, content identity, and lazy descendant-content views are reused. Deterministic 1-versus-128-descendant tests bound state-provider calls to the same constant envelope in both Trash directions and prove zero file-provider copies.

Same-owner file copy is now a metadata reflink to the existing immutable blob, reuses any compatible preview artifact after independent path/version authorization, and is not reported as reclaimable physical duplication. New upload capabilities target the final random blob key directly. Completion verifies provider-attested size/MD5/CRC32C and publishes the canonical reference without GCS rewrite/copy; aborted or expired unpublished blobs remain unreachable for closed-gate collection. The only copy-based upload publication path is decode-only compatibility for historical schema-001 records. The `005 -> 006` migration is feature-only and does not walk or rewrite the existing directory graph.

The release also refreshes the pinned official Go vulnerability database generation and its Nix content hash after the preceding upstream GCS generation was retired. Security qualification remains offline and reports zero reachable vulnerabilities.

The v0.2.0 scalability release adds the backend foundation for the separately released duplicate-management UI. Its migration chain contains the untagged schema-004 indexed-metadata intermediate and the released schema-005 resumable-operation format. Stored-file identity is now the normalized provider-attested `(size, MD5, CRC32C)` tuple. Upload completion, copy recovery, migration, duplicate indexing, and checkpointing use metadata requests and server-side operations only; they never download file bodies through the Go control plane. A structural source lint rejects production object-body readers with actionable metadata/server-copy/direct-plane guidance. Its exact initial optional-feature exemption is image preview generation. Historical SHA fields remain decode-only migration input, while new file, copy, and checkpoint records contain no file-content SHA-256.

Schema epoch 005 adds durable resumable operation preparation. Recursive copy/move and non-empty directory replacement admit a bounded preparing header before subtree traversal, persist authenticated bounded runs and cursors, reduce sorted prerequisites/copies/catalog/content deltas through fixed-fan-in provider-backed merges, and seal directly into the existing bounded operation-step chain. Takeover reuses completed runs; root changes fail before visibility preparation. Runtime preparation uses O(path depth + bounded buffers), neither materializes the subtree nor reads stored-file bodies, and leaves no local scratch state. The `004 -> 005` migration is feature-only: it closes/drains the canonical gate, advances the exact writer/superblock/gate feature signature, creates its own checkpoint, and does not repeat schema-004 graph conversion.

Schema 004 replaces mutable whole-namespace state scans and growing directory page arrays with persistent copy-on-write search trees. State and directory roots remain constant size; point lookup and mutation are logarithmic, cursors are constant-size authenticated capabilities, and one file change rewrites only bounded index nodes plus ancestor deltas. Name, modified-time, size, and kind listing orders each use immutable keyset indexes, so later pages do not reload and sort the directory. Directory roots also maintain incremental order-independent content accumulators and exact structure-sensitive digests. Large operation prerequisites and copy instructions live in immutable staging plus bounded hash-chained step pages instead of growing operation/admission envelopes. Runtime code no longer carries whole-directory snapshot/update helpers; clone, catalog rebuild, scope discovery, directory replacement, and current-schema migration reconciliation use bounded iterators.

The owner-scoped duplicate catalog maintains file groups, exact directory groups, bounded occurrence/count shards, and stable revisioned ignore policy in the same fenced commit as each filesystem mutation. Every manifest pins a copy-on-write descendant-content index ordered by file group and relative path plus a fixed 16-position deletion-safe MinHash summary. The content index omits logical versions and resolves them only for the bounded reconciliation output page, so republishing identical content at the same path shares the existing content nodes. Narrow inverted postings change only when a slot minimum or directory path changes; another occurrence of an existing content group rewrites no posting. Candidate lookup probes those 16 ranges, deduplicates repeated hits, and lazily computes exact overlap without an O(directory-pairs) matrix. Selected comparison and reconciliation merge two indexes with page-sized memory, and continuation cursors resume both immutable roots while reusing authenticated totals. Exact-group and stable directory-pair ignore preferences are separate, so an intentional exact or partial mirror can be hidden without suppressing unrelated copies. Part 1 APIs page groups/occurrences/candidates, preview one bounded pinned reconciliation page, and move chosen duplicate occurrences to Trash after revalidation. No duplicate route permanently deletes a blob. Per-group reclaimable bytes are overflow checked; directory groups and descendant file groups intentionally are not presented as a summable global total.

Checkpoint v3 replaces the v0.1.14 body-hashing path with a single ordered provider-metadata traversal per role and bounded inventory pages containing role, key, size, MD5, and CRC32C. Destination verification performs the same metadata traversal and fails closed on missing integrity metadata. Legacy v1/v2 checkpoint roots and per-object journals cannot reopen writes and are retired under the closed gate. Schema-004 migration persists exact transform/verification directory marks so restart skips completed subtrees; its fixed-fanout content-index builder merges bounded pages from already-migrated children and publishes deterministic immutable nodes as buffers fill, avoiding a complete descendant-content slice. Closed-gate maintenance retains terminal operations/idempotency bindings for 30 days, prunes transient artifacts and finalized duplicate tombstones, rejects unresolved catalog transitions, and performs a gate-version-bound resumable reachability collection over persistent tree nodes, state versions, and blobs.

Directory metadata now carries persisted recursive-byte and recursive-file-count aggregates. File upload or replacement, move, copy, trash, restore, and permanent deletion update every affected ancestor and the live or trash area root through the same durable commit as the visible tree mutation. Directory `size` and `fileCount` are therefore cheap prefix-total lookups; an area root reports that area's total logical bytes and files. A file contributes one even when its size is zero, while directories are not counted. Overflow or inconsistent canonical aggregates fail closed. Completion attempts that lose an unrelated ancestor-root race advance through the durable upload record and retry from authoritative state; true same-target races retain one version-precondition winner. Deterministic tests force eight replicas through multi-file, same-upload, and same-target completion races, completion/abort races, contested folder rename/trash/restore/delete, and every folder-operation commit boundary. Both aggregates are covered by checkpoint/raw-copy portability and advertised as required `recursive-byte-aggregates-v1` and `recursive-file-count-aggregates-v1` storage features.

Durable upgrades run through an append-only storage-schema ledger rather than
release-specific startup branches. It records schemas 001 through 011 with only
adjacent transforms. Startup resolves one exact epoch and executes the complete
remaining suffix. The `007 -> 008` edge installs the owner namespace graph and
consistency-domain foundation. The `008 -> 009` edge authenticates every source
domain and state key, binds unchanged application payloads to typed records,
and deterministically repartitions authority by invariant. The `009 -> 010`
edge proves and restores retained indexed application authority before the
central activation barrier permits the new epoch. The `010 -> 011` edge
activates bounded packs and upload transactions after verifying the frozen
typed-domain closure. All edges reuse
immutable file blobs in place and never read or copy their bodies. Each edge
quiesces the durable write gate, upgrades and verifies its owned records,
advances writer/superblock/gate markers, checkpoints, and reopens before the
next edge. The chain is crash-resumable, supports simultaneous migrators and
split state/file buckets, and fails closed on corruption, overflow, unknown or
mixed epochs, unrelated configuration drift, or undrained work. Immutable
producer fixtures exist for every epoch/profile; the mandatory matrix starts
from each fixture, traverses every remaining edge, verifies authoritative state,
and performs a post-upgrade mutation. Arbitrary non-EndlessFS bucket objects are
not imported.

The v0.1.10 patch introduced resumable body-inventory journals and startup migration observation. Its liveness/readiness split, structured safe progress, delayed split-bucket coverage, and upload-abort race fix remain. Schema 004 supersedes its checkpoint journal and body-read mechanism with checkpoint v3 metadata traversal and directory migration marks.

The v0.1.14 patch removed the whole-storage-set checkpoint record ceiling exposed by a live 12,080-object split-backend migration by introducing bounded hash-chained inventory pages. Schema 004 retains that bounded page shape but replaces checkpoint-v2, its per-object journal, and its version-pinned source-body reader with metadata-only checkpoint v3. Legacy checkpoint roots remain decodable only so closed-gate startup can retire and replace them; they are no longer accepted as verification evidence.

Directory-list responses include the current directory's `size` and `fileCount` from the same immutable snapshot as every page, including later cursor pages. Trash pages batch-join unchanged schema-v1 trash records to the persisted trash-root manifest, returning exact file/directory sizes, recursive file counts, and validated file media types with one storage lookup per page instead of one `Stat` per row. Public-share responses distinguish the original share root from the currently viewed relative directory and expose both aggregates without revealing owner paths or provider metadata.

The GCS adapter is locally qualified without credentials using an in-process protocol server that exercises documented JSON/XML requests, generation preconditions, strong visibility, checksums, ranges, resumable offsets and cancellation, generation-bound V4 capabilities, exact-origin CORS, stable error mapping, disconnects, and ambiguous lost-success recovery. The same application provider/state contracts run through the portable engine over the GCS adapter.

The release is built and verified exclusively through the pinned Nix interface. Its release archive includes the application binary, OCI archive, source/input inventory, binary and OCI hashes, dependency and license inventory, installed-theme inventory, and the acceptance evidence index.

Important limitation: no live GCS bucket interoperability, cloud resource creation, deployment, production operations, backup/restore, regional durability, or incident-response procedure was tested. They are not deterministic v1 acceptance requirements. “Locally qualified GCS adapter” is evidence of integration-layer behavior against the documented protocol, not evidence that a particular live deployment is production-ready.

No GCP credentials, cloud services, database, external identity provider, container runtime, deployment target, or non-loopback runtime service is needed to build or accept v1. The local `mock` backend is intentionally in-memory; the `gcs` runtime uses Application Default Credentials and keyless workload identity/IAM signing, but still requires the separate live qualification and operations review before production use.

## v1.1 media browsing and generated image previews

v1.1 adds an always-available row-virtualized thumbnail grid, loaded-metadata filtering, file-type icon fallback, and a full-screen previous/next viewer. Generated thumbnails are optional. When configured, PNG/JPEG/GIF/WebP and the closed DNG/CR2/CR3/RAF/NEF/ORF/RW2/PEF/ARW camera-RAW set produce static WebP only, preserve the source aspect ratio, remove source metadata, and are served in configured maximum-edge variants. The UI centers those uncropped artifacts inside square grid frames; otherwise it shows the appropriate built-in file-type icon.

Preview artifacts live behind an independent store contract and data origin. The local profile uses the deterministic memory store; GCS deployments can select a distinct durable shared preview bucket using the same thin conditional-object and signed-transfer adapter as authoritative storage. Provider-neutral preview semantics use HMAC-derived opaque keys, immutable WebP/manifest objects, bounded committed history, conditional visibility updates, and monotonically fenced claims across replicas. Opaque content bindings survive rename, move, same-owner reflink copy, trash, and restore, while content replacement receives a distinct identity. Automatic age and source-size policies are independently optional and are evaluated before any original read; explicit owner-authorized Generate and Regenerate bypass only those automatic policies.

Configuration mistakes fail fast: a configured unpackaged `video` or `pdf` generator, unknown capability, invalid policy, failed codec self-test, or inaccessible configured preview store prevents startup. Runtime preview-store loss fails readiness and logs a safe error while authoritative file operations continue. The base and `container-images` outputs remain the same image-only static binary profile. Release output now includes `CAPABILITIES.json` with recipe, format, profile, and dependency-inventory binding.

The v0.1.1 patch corrects durable GCS preview startup validation for live responses that express the required cache policy with additional directives or multiple header fields. GCS server-written objects still carry `no-store` metadata, and validation now parses Cache-Control syntax to require an effective bare `no-store` directive. Missing, malformed, and parameterized lookalikes remain startup failures; content type, safe inline disposition, exact CORS origin, artifact bytes, integrity, and capability binding checks are unchanged.

The v0.1.2 patch corrects live GCS file-upload completion after a resumable session has already materialized its staging object. Cleanup now uses a strong metadata read to distinguish a finalized object from an incomplete session: finalized staging is removed with its exact generation condition without attempting to cancel the terminal session, while incomplete sessions still require explicit cancellation. The shared portable-provider contract runs with the stricter completed-session behavior, and a focused positive/negative regression proves both branches. It also permits only validated in-memory preview images through the `img-src` policy and removes runtime grid style attributes, preserving `style-src 'self'` without `unsafe-inline`. Browser proof now requires generated WebP images to decode before the grid or viewer reports them loaded while retaining the 10,002-item virtualization bounds.

Video and PDF generated previews remain separate deferred v1.2 and v1.3 deliverables. Neither dependency class is present in the v1.1 image build. Authoritative source access and preview content identities use the portable engine over memory or the locally qualified GCS adapter; generated artifacts use either the deterministic memory store or the durable shared GCS store. Both GCS source and preview paths are credential-free protocol-qualified. Production incident observations do not replace the separate opt-in live qualification, deployment/security review, or operations validation, none of which is claimed by the deterministic release gate.
