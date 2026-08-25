# EndlessFS v1 operations

This guide covers the provider-portable, multi-replica v1 runtime and its locally qualified GCS adapter. Local protocol qualification is not live-service or production-readiness validation.

## Runtime model

EndlessFS runs one Go control-plane binary. Application use cases always use one portable storage engine; only the thin atomic-object backends change. The `mock` backend holds canonical records in memory and starts empty after a restart. The `gcs` backend stores the same canonical keys and bodies in a private state/file storage set. By default both authoritative roles use one bucket. `ENDLESSFS_GCS_STATE_BUCKET` can select a distinct bucket for state, filesystem metadata, operations, leases, and checkpoints; immutable blobs, unpublished direct-final uploads, and decode-only legacy upload staging remain in `ENDLESSFS_GCS_FILE_BUCKET`. Optional generated previews use provider-neutral preview-store semantics over the same thin object-store interface, with a separate disposable GCS bucket rather than duplicating the transport adapter.

Several replicas may share one storage set. They must use the same state/file
bucket pairing, base URL/RP identity, registration policy,
session-secret-derived keyring identity, stable writer-set ID, writer protocol,
and canonical features. Startup rejects an incompatible writer before it serves
bucket-backed requests. There is no leader, process-local lock, admission
transaction, or lease around an ordinary same-domain mutation. Schema 009
partitions identity, administration, preview jobs, and the owner namespace by
their real invariants and binds each state payload to a canonical record type. A
same-domain mutation writes changed immutable pages and conditionally replaces
one domain head as its sole visibility point. A genuine cross-domain invariant
uses a helpable immutable plan, participant locks, and one create-only decision;
readers and checkpoint closure finish the durable decision before exposing the
participant state.

If a replica disappears before the head replacement, it has made no visible
change. If the provider accepted the replacement but its response was lost, the
same or another replica rereads the head and returns the retained
fingerprint-bound outcome. A returning stale replica cannot publish through the
old native head condition. Interrupted preparation leaves only unreachable
immutable pages and unrelated domains continue immediately.

## Automatic storage-schema migration chain

Startup detects one exact epoch in the append-only storage-schema ledger and
executes every remaining adjacent transformation in order. The current ledger
contains schemas 001 through 009. Schema 009 is the typed transactional-state
and owner-namespace-graph format and is the only ordinary runtime. A schema-001
bucket therefore runs `001 -> 002 -> 003 -> 004 -> 005 -> 006 -> 007 -> 008 -> 009`;
schema 003 runs the suffix beginning `003 -> 004`; schema 008 runs only
`008 -> 009`; current schema-009 state performs no migration. No operator
migration command or bucket edit is required. Unknown or contradictory epoch
markers fail closed.

Each edge owns a stable checkpoint and CAS-closes the durable write gate before
changing state. Schemas 001 through 007 and their operation/preparation records
are migration input only; their edge-specific recovery rules remain implemented
and tested so every released predecessor can advance safely. The `007 -> 008`
edge deterministically imports mutable state into owner-control,
administration, capability, share, and owner-namespace domains; installs
content-addressed pages; freezes the installed domains; creates the migration
checkpoint; advances the feature binding; reopens the gate; and unfreezes the
domains. File bodies are never read. Blob integrity uses provider-attested
`(size, MD5, CRC32C)` metadata. The edge is restartable after every durable
boundary and converges under two through eight concurrent migrators.

The `008 -> 009` edge authenticates every source domain and state key, wraps
unchanged application payloads in typed records, and deterministically
repartitions them into namespace, owner-identity, owner-jobs, administration,
and capability domains. It installs and freezes the complete target catalog,
checkpoints it, advances the `transactional-state-domains-v1` feature binding,
reopens, and unfreezes. It neither reads nor copies file bodies and is
restartable/convergent at the same durable migration boundaries.

The `003 -> 004` graph walk persists authenticated transform and verification marks for each completed directory. Every mark is tied to the exact migration checkpoint, parent/root/manifest logical versions, parent entry, aggregates, and content summary. The process holds only the active ancestor stack; after restart, a valid completed mark skips that entire subtree. Several replicas may advance the same deterministic walk through CAS. Stale, forged, misplaced, corrupt, or contradictory marks fail closed and marks are removed only after the independent verification phase succeeds. Provider-ordered scope discovery retains one owner/area at a time. Transforming a historical page manifest retains at most that one legacy directory while deriving its differently ordered persistent indexes; current-index verification and CAS-winner reconciliation are page-bounded.

Every edge is safe to retry after process loss and safe for several new replicas
to attempt concurrently. A replica that loses a CAS follows the durable winner.
An interrupted chain resumes its checkpointed edge and accepts only the
explicitly reviewed mixture of source, target, and later already-published
records without downgrading them. Until an edge commits, predecessor records
remain authoritative and the gate stays closed once migration has begun.
Immutable schema-004 through schema-009 fixtures cover portable-minimal,
application-disabled, and application-GCS writer profiles and are pinned to
their producer revision and fixture digest.

A live upload capability can temporarily prevent checkpoint closure; allow it
to finish or expire and retry. Missing roots, cycles, multiple parents,
unreachable roots, malformed canonical records, overflow, an unrelated
closed-gate maintenance operation, an unknown epoch, or unrelated
feature/configuration drift fails closed. The chain supports both single- and
split-bucket storage sets. It does not discover or import arbitrary provider
objects outside the canonical `endlessfs/v1` graph.

## Local start and stop

Generate independent bootstrap and session secrets, export them only in the process environment, and start through Nix as shown in the README. Remove `ENDLESSFS_BOOTSTRAP_TOKEN` after the first administrator exists. Use HTTPS with a matching base URL and RP ID for any non-loopback listener.

`SIGTERM` and `SIGINT` stop accepting requests and give the HTTP listeners up
to ten seconds to shut down. The in-memory backend is intentionally ephemeral,
so shutdown does not make it durable.

## GCS identity and bucket policy

Select GCS with:

```console
export ENDLESSFS_STORAGE_PROVIDER=gcs
export ENDLESSFS_GCS_FILE_BUCKET=endlessfs-files
# Optional; omit for single-bucket mode or set equal to ENDLESSFS_GCS_FILE_BUCKET.
export ENDLESSFS_GCS_STATE_BUCKET=endlessfs-state
export ENDLESSFS_WRITER_SET_ID="$(nix run .#generate-secret)"
export ENDLESSFS_BASE_URL=https://drive.example
```

The runtime uses [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials). Prefer a dedicated application service account attached through the platform workload identity mechanism. For workloads outside Google Cloud, follow Google's [Workload Identity Federation best practices](https://cloud.google.com/iam/docs/best-practices-for-using-workload-identity-federation) with a narrowly matched external principal and service-account impersonation; do not deploy service-account JSON keys or HMAC keys.

Grant only the bucket object permissions the adapter needs. `roles/storage.objectUser` scoped to each configured bucket is the standard predefined starting role. Keep public access prevention and uniform bucket-level access enabled where policy permits. The service account used for [signed URLs](https://cloud.google.com/storage/docs/access-control/signed-urls) must also have `iam.serviceAccounts.signBlob` on itself (normally `roles/iam.serviceAccountTokenCreator`) and the IAM Service Account Credentials API must be enabled. Set `ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT` to that service-account email when automatic ADC identity discovery is unavailable; this is an identifier, not a credential.

The browser needs [exact-origin bucket CORS](https://cloud.google.com/storage/docs/configuring-cors) on the file bucket for `GET`, `HEAD`, and `PUT`, request headers `Content-Type`, `Content-Range`, and `Range`, and exposed response headers `Content-Length`, `Content-Range`, `Range`, and `X-Goog-Generation`. A GCS preview bucket needs exact-origin `GET` and `HEAD`; preview writes are exclusively server-side. A distinct state bucket needs no browser CORS. Do not use a wildcard origin. Signed URLs and [resumable session URLs](https://cloud.google.com/storage/docs/performing-resumable-uploads) are short-lived bearer capabilities and must never be logged.

State and file buckets may use different storage classes, billing boundaries, encryption settings, retention policies, and backup schedules. Those policies must preserve live canonical objects and the required strong read/list/conditional-operation behavior. Do not configure lifecycle deletion of state records, committed blobs, or other live objects outside EndlessFS. Retrieval-delayed file storage classes can make interactive downloads unavailable and require separate qualification.

Choose the layout before first initialization when possible. On an existing single-bucket deployment, setting `ENDLESSFS_GCS_STATE_BUCKET` alone does not move blobs and will make the new pairing incomplete. Change layouts only through the closed-gate checkpoint procedure below: copy blob keys to the file-bucket role, retain every other authoritative key in the state-bucket role, verify the combined destination, and only then reopen writes.

Bucket creation, IAM, CORS, lifecycle, retention, monitoring, regional design, and deployment remain explicit operator responsibilities. The deterministic gate does not mutate cloud policy and no live GCS deployment has been qualified by this repository.

## Quiescent provider cutover

Canonical state deliberately contains no bucket/account identifier, GCS generation, S3 version ID, Azure ETag, provider metadata, signed URL, or resumable session URL. A supported cutover copies bytes; it never converts state:

1. Close the canonical write gate. Every replica stops accepting new mutations,
   freezes the complete domain catalog, and conditionally freezes every
   registered domain head.
2. Drain or abort expired data-plane capabilities and native leases. Closure
   refuses to proceed while a live capability remains. A writer holding a
   pre-freeze head condition cannot publish after freeze; there is no operation
   lock to delete or force.
3. Create the closed-gate metadata checkpoint and copy exactly its paged
   authoritative key/body inventory plus the state-bucket checkpoint root and
   every inventory page. The root and each page remain independently bounded;
   their count may grow with the reachable graph. Inventory entries contain
   role, key, size, MD5, and CRC32C taken from provider metadata. Checkpoint
   construction and verification do not fetch object bodies. Do not copy
   historical admissions/operations, unreachable pages, provider leases,
   derived projections, or maintenance records.
4. Copy each key and body unchanged to its destination role: blob keys to the file bucket and all other authoritative keys to the state bucket. In single-bucket mode both roles name the same destination. Destination-native versions and metadata may differ and are not preserved.
5. Run the read-only destination verifier against both configured roles. It performs ordered metadata traversals and compares exact `(size, MD5, CRC32C)` tuples. Missing, extra-authoritative, misplaced, fingerprint-mismatched, mixed-version, or unsupported objects fail closed without a body-download fallback.
6. Reconfigure compatible replicas to the destination while retaining the same
   provider-independent application secrets and writer-set identity. Verify the
   checkpoint, increment/open the destination gate epoch, idempotently unfreeze
   every domain, and continue mutations.

The source remains closed. Online dual writes and reconciliation of mutations made outside EndlessFS are not supported. A pre-copy may reduce downtime, but the final checkpoint-authorized copy must be taken after quiescence.

The verifier configuration is strict JSON. For GCS:

```json
{
  "provider": "gcs",
  "fileBucket": "endlessfs-files-destination",
  "stateBucket": "endlessfs-state-destination",
  "checkpointID": "cutover-2026-08-17",
  "writerSetID": "BASE64URL_WRITER_SET_ID",
  "configurationDigest": "EXPECTED_CONFIGURATION_DIGEST",
  "keyringIdentifiers": ["EXPECTED_KEYRING_ID"],
  "requiredFeatures": ["consistency-domains-v1", "directory-content-digests-v1", "directory-manifests", "duplicate-catalog-v1", "fenced-operations", "metadata-only-checkpoints-v1", "owner-namespace-graph-v1", "paged-operation-steps-v1", "persistent-directory-indexes-v1", "persistent-namespace-snapshots-v1", "persistent-state-indexes-v1", "portable-checkpoints", "provider-content-fingerprints-v1", "rebuildable-derived-projections-v1", "recursive-byte-aggregates-v1", "recursive-file-count-aggregates-v1", "resumable-operation-preparation-v1", "transactional-state-domains-v1", "user-addressable-duplicate-directories-v1"]
}
```

Then run:

```console
nix run .#provider-verify -- check ./verify.json
```

The command performs no `Put`, `Copy`, or `Delete`. Its local `memory` mode accepts a fixture path containing a JSON map from canonical object key to standard-base64 body; this exists for deterministic migration rehearsal.

## Health and observation

- `GET /healthz` reports process liveness. The listener binds before storage migration begins, so Kubernetes startup and liveness probes MUST use this route and allow the migrator to continue for as long as the closed-gate work requires.
- `GET /readyz` reports successful assembly, including completed storage migration and writer-set compatibility. Readiness probes use this route; startup probes MUST NOT use it because an intentionally unready migrator may run longer than a fixed probe window.
- Storage migrations emit `storage_migration_progress` JSON records with only the ledger edge, stage, backend role, provider-independent object/byte totals, and resumed count. They never include object keys, virtual paths, bucket names, provider versions, or secrets.
- Logs are structured JSON. `ENDLESSFS_LOG_LEVEL` accepts exactly `debug`, `info`, `warn`, or `error`.
- Central redaction remains active at every level. Logs are not a file/account audit trail.

Capability responses and public configuration use `no-store`. Diagnostics omit token-bearing queries, request bodies, authorization values, provider keys, full user paths, native continuation values, and credential material.

## Duplicate maintenance foundation

Schema 009 preserves each owner's live and Trash trees in one persistent
namespace graph. A folder move, copy-by-reference, Trash, restore, or logical delete
rewrites only affected edges and ancestor paths; it never enumerates descendants
or relocates a blob. Same-owner copies share immutable content. Browser uploads
target a newly allocated final blob directly and completion publishes its
reference through the owner namespace head without GCS rewrite/copy.

Duplicate groups and directory-overlap views are rebuildable projections over a
specific owner namespace revision. File identity is the provider-attested
`(size, MD5, CRC32C)` tuple. Exact directory identity also binds recursive
counts, relative names, kinds, and nested structural digests. Ignore decisions
remain authoritative owner-namespace values. Reconciliation binds its projection
and namespace revisions, revalidates them at apply time, and publishes removals
through one owner namespace mutation. A stale plan fails closed and no route
permanently deletes duplicate data.

Projection pages are immutable and do not participate in ordinary namespace
commits. This prevents a bucket-wide duplicate index from adding synchronous
provider traffic to every upload or move. Exact and partial comparisons merge
bounded persistent trees lazily; there is no directory-pair matrix and no file
body read.

Provider economics are an executable architecture gate. `nix run .#test-provider-budget` classifies actual GCS wire requests and checks exact request-count, price, and modeled-latency ratchets for state, file, data-plane, and preview pathways. See `docs/provider-economics-budgets.md` for fixture provenance, limitations, and the append-only tightening law.

The Part 1 control API can page groups, occurrences, and selected-directory overlap candidates; include or hide ignored results; compare any selected live/trash directory pair; preview the exact file-content intersection of two disjoint live directories; and apply one bounded preview page to Trash. Candidate lookup probes 16 posting ranges and exact comparison remains lazy—there is no directory-pair table. Partial comparison and preview merge immutable per-directory content indexes with bounded memory; subsequent preview cursors resume both trees and reuse authenticated totals. Exact-group ignores and stable unordered directory-pair ignores have separate revisions. Apply revalidates the selected manifests, gate epoch/version, each keep/remove file group and logical version, and every ignore revision. A stale plan fails closed; a successful plan is recoverable through Trash and leaves a durable redacted batch audit result. No route permanently deletes duplicate data.

Per-group reclaimable bytes count all but one occurrence of that exact group. Do not sum directory-group savings with descendant file-group savings because they overlap. The Part 2 browser will select non-overlapping actions and present ignored groups in a collapsed section.

## Closed-gate retention and collection

Fingerprint-bound mutation outcomes and idempotency bindings are retained in
bounded domain trees and indexed by expiry. Trash is not subject to that
window. During gate closure EndlessFS freezes the catalog and all domains,
resolves cross-domain plans, drains upload leases, authenticates the exact
reachable domain/namespace/blob closure, and excludes unreachable immutable
pages and rebuildable projections from the checkpoint. The completed
schema-009 checkpoint is then the immutable mark set for a resumable conditional
sweep of recognized domain pages, transition residue, projections, leases, and
blobs. The sweep stores only a portable ordered-key cursor, never reads file
bodies, and must reach its terminal checkpoint-bound session before writes can
reopen. Reachability uses a disk-backed exact visited set and bounded merge
chunks, so service memory does not grow with the full graph.

The schema-009 runtime retains terminal predecessor-GC session/mark objects as
excluded compatibility residue. A lagging supported predecessor may still hold
a sweeping snapshot, so deleting those marks could let it mistake live schema-
009 authority for garbage. They are neither runtime authority nor checkpoint
contents. Do not configure an independent bucket lifecycle rule to approximate
application reachability collection.

## Optional v1.1 previews

The media browser is always available. The list/grid choice, metadata filters, full-screen viewer, and file-type icons require no preview storage and never retrieve originals automatically. `ENDLESSFS_PREVIEW_PROVIDER=disabled` keeps generated thumbnails off; `mock` enables the separate ephemeral loopback store; and `gcs` enables the durable shared store for GCS deployments. GCS previews require a distinct `ENDLESSFS_GCS_PREVIEW_BUCKET` and a stable canonical `ENDLESSFS_PREVIEW_KEY_SECRET` shared by every replica. The removed `ENDLESSFS_MEDIA_BROWSER_ENABLED` variable is a startup error so an obsolete deployment cannot silently hide this behavior. Original-file transfer remains on its existing data origin.

```console
export ENDLESSFS_PREVIEW_PROVIDER=gcs
export ENDLESSFS_GCS_PREVIEW_BUCKET=endlessfs-previews
export ENDLESSFS_PREVIEW_KEY_SECRET="$(nix run .#generate-secret)"
export ENDLESSFS_PREVIEW_FORMATS=image
```

Configured generators and preview-store access are startup requirements. The process self-tests the packaged image codec, performs a complete deterministic DNG decode through the pinned LibRaw worker, and creates, reads, fully decodes, commits, retrieves, capability-issues, and capability-serves a fixed one-pixel WebP probe before becoming ready. A missing, non-executable, or incompatible RAW decoder prevents startup with a sanitized error. Configuring `video` or `pdf` in the v1.1 image build is an intentional startup error. Preview-store access loss after startup returns `unavailable`, logs `preview_unavailable` without file/store identity, and makes `/readyz` fail while original listing and file operations continue.

Normal durable preview capability issuance does not download the artifact through the control plane. EndlessFS verifies the provider-independent manifest size and CRC32C through the object-store integrity contract, binds the capability to the exact verified object incarnation, and has the browser verify manifest SHA-256 before display. The GCS adapter performs its verification with an object-metadata request; native GCS checksum and generation values are not persisted.

Preview data is disposable. The durable store keeps an HMAC-derived opaque binding head with a bounded committed-generation history plus immutable manifests and WebP objects. Conditional head updates publish visibility, and one-winner claim takeover fences stale generators across replicas. Browser reads use short-lived exact-generation signed `GET` capabilities; the browser verifies the exact WebP type and RIFF/WebP signature before constructing an image blob. Removing or expiring preview objects never removes an original; the next eligible viewport request regenerates them. Rename, move, trash, and restore preserve the opaque content binding so no regeneration is required. Copy and content replacement receive distinct render identities. Provider lifecycle, storage class, billing, retention, and deletion policy remain independent of the authoritative store.

## Build and release verification

Run the acceptance gates from a clean checkout:

```console
nix run .#test-replica
nix run .#test-portability
nix flake check --print-build-logs
nix build
nix build .#container
nix build .#container-images
nix build .#release
nix build .#release-images
```

No required gate needs GCP credentials or a cloud service. The release inventory distinguishes the ephemeral memory preview store, locally qualified durable GCS preview store, absent live-GCS validation, and absent deployment validation.

The release output includes `SHA256SUMS`, `RELEASE-INVENTORY.txt`, the binary/archive, OCI archive, `CAPABILITIES.json`, dependency and license inventories, installed-theme inventory, release notes, and the acceptance record. Verify `SHA256SUMS` before distribution. The inventory records the source revision, `flake.lock` hash, pinned vulnerability database hash, Go toolchain, artifact hashes, thresholds, provider kind, and explicit no-cloud/no-deployment status.

Tekton publishing on the xlab bare-metal Talos Linux cluster is tag-driven.
Protected `vMAJOR.MINOR.PATCH` tags cause the PaC release workflow to repeat the
full gate, push version and `latest` tags to GHCR, and attach the Nix-built
evidence. The same short-lived xlab.now GitHub App installation token used to
clone the tag performs release creation and asset upload. GHCR publishing uses
a separate SOPS-encrypted classic PAT limited to `write:packages`, mounted only
into the trusted publishing step after the general App installation token was
rejected by the registry. The workflow never targets the production GKE
cluster. GitHub Actions workflows are retired; build, test, publish, and release
automation is owned by these PaC definitions. Applying branch/tag rules is a
separate explicit administrator operation through
`nix run .#repository-policy -- apply`; the ordinary CI token cannot administer
repository policy.

## Failure handling

Use stable problem kinds and durable operation states rather than provider error text. Retriable provider faults, ambiguous successes, and resumable offsets are reconciled by rereading canonical/provider state. An idempotency key is safe only for the same authenticated owner, operation kind, and request fingerprint.

Never repair a portable bucket by editing canonical objects directly. If checkpoint verification fails, keep writes disabled and repair or repeat the byte-for-byte copy from the closed source. Promote a GCS deployment only after the opt-in live interoperability, real CORS/direct-transfer, IAM, durability, backup/restore, monitoring, incident-response, and security review required by specification section 23.
