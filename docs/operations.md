# EndlessFS v1 operations

This guide covers the provider-portable, multi-replica v1 runtime and its locally qualified GCS adapter. Local protocol qualification is not live-service or production-readiness validation.

## Runtime model

EndlessFS runs one Go control-plane binary. Application use cases always use one portable storage engine; only the thin atomic-object backends change. The `mock` backend holds canonical records in memory and starts empty after a restart. The `gcs` backend stores the same canonical keys and bodies in a private state/file storage set. By default both authoritative roles use one bucket. `ENDLESSFS_GCS_STATE_BUCKET` can select a distinct bucket for state, filesystem metadata, operations, leases, and checkpoints; immutable blobs and upload staging remain in `ENDLESSFS_GCS_FILE_BUCKET`. Optional generated previews use provider-neutral preview-store semantics over the same thin object-store interface, with a separate disposable GCS bucket rather than duplicating the transport adapter.

Several replicas may share one storage set. They must use the same state/file bucket pairing, base URL/RP identity, registration policy, session-secret-derived keyring identity, stable writer-set ID, writer protocol, and canonical features. Startup rejects an incompatible writer before it serves bucket-backed requests. There is no leader or process-local lock: every mutation uses a durable state-bucket candidate/admitted ticket, canonical operation intent, conditional object updates, and a monotonically increasing fence.

If a replica disappears while it owns a mutation, the durable operation remains. The affected resource can be temporarily unavailable until the lease expires. One competing replica wins the takeover CAS, increments the fence, reconciles any ambiguous provider result, and resumes the same intent. A returning stale replica cannot commit, unlock, or replace the recovered result; its old fence and object preconditions fail.

## Local start and stop

Generate independent bootstrap and session secrets, export them only in the process environment, and start through Nix as shown in the README. Remove `ENDLESSFS_BOOTSTRAP_TOKEN` after the first administrator exists. Use HTTPS with a matching base URL and RP ID for any non-loopback listener.

`SIGTERM` and `SIGINT` stop admission and give the HTTP listeners up to ten seconds to shut down. The in-memory backend is intentionally ephemeral, so shutdown does not make it durable.

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

1. Close the canonical write gate. Every replica stops admitting new mutations.
2. Allow fenced recovery to finish admitted operations and drain or abort live data-plane capabilities and native leases. A crashed operation may delay closure; do not delete its lock or force the gate closed.
3. Create the closed-gate checkpoint and copy exactly its sorted authoritative key/body inventory plus the state-bucket checkpoint object. Do not copy admissions, staging garbage, or backend leases.
4. Copy each key and body unchanged to its destination role: blob keys to the file bucket and all other authoritative keys to the state bucket. In single-bucket mode both roles name the same destination. Destination-native versions and metadata may differ and are not preserved.
5. Run the read-only destination verifier against both configured roles. Missing, extra-authoritative, misplaced, corrupt, mixed-version, or unsupported objects fail closed.
6. Reconfigure compatible replicas to the destination while retaining the same provider-independent application secrets and writer-set identity. Verify the checkpoint, increment/open the destination gate epoch, and continue mutations.

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
  "requiredFeatures": ["directory-manifests", "fenced-operations", "portable-checkpoints"]
}
```

Then run:

```console
nix run .#provider-verify -- check ./verify.json
```

The command performs no `Put`, `Copy`, or `Delete`. Its local `memory` mode accepts a fixture path containing a JSON map from canonical object key to standard-base64 body; this exists for deterministic migration rehearsal.

## Health and observation

- `GET /healthz` reports process liveness.
- `GET /readyz` reports successful assembly, including writer-set compatibility checked during startup.
- Logs are structured JSON. `ENDLESSFS_LOG_LEVEL` accepts exactly `debug`, `info`, `warn`, or `error`.
- Central redaction remains active at every level. Logs are not a file/account audit trail.

Capability responses and public configuration use `no-store`. Diagnostics omit token-bearing queries, request bodies, authorization values, provider keys, full user paths, native continuation values, and credential material.

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
cluster. Applying branch/tag rules is a separate explicit administrator operation through
`nix run .#repository-policy -- apply`; the ordinary CI token cannot administer
repository policy.

## Failure handling

Use stable problem kinds and durable operation states rather than provider error text. Retriable provider faults, ambiguous successes, and resumable offsets are reconciled by rereading canonical/provider state. An idempotency key is safe only for the same authenticated owner, operation kind, and request fingerprint.

Never repair a portable bucket by editing canonical objects directly. If checkpoint verification fails, keep writes disabled and repair or repeat the byte-for-byte copy from the closed source. Promote a GCS deployment only after the opt-in live interoperability, real CORS/direct-transfer, IAM, durability, backup/restore, monitoring, incident-response, and security review required by specification section 23.
