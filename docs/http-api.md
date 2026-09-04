# EndlessFS HTTP API

This document fixes the v1 JSON field casing and control-plane routes implemented through Milestone 3. The normative security and behavior contract remains [v1-specification.md](./v1-specification.md).

## Conventions

- JSON request bodies use `application/json`, reject unknown or duplicate fields and trailing content, and are limited to 1 MiB. File bytes are never accepted by these routes.
- Authenticated mutations require the session cookie, exact `Origin`, and `X-EndlessFS-CSRF`. Mutations named by specification section 12.1 additionally require a printable 16–128 byte `Idempotency-Key`.
- Errors use `application/problem+json` with `type`, `title`, `status`, `code`, and `requestID`. Capability, authentication, and public-share responses are `Cache-Control: no-store`.
- Paths are canonical absolute virtual paths. Provider keys, buckets, owner prefixes, and caller-selected capability URLs are not request fields.
- Capability responses contain `url`, `method`, `headers`, and `expiresAt`. The browser sends file bytes directly to that URL on the separate data-plane origin.

The public configuration advertises 100 direct upload workers, matching the
existing maximum upload-initialization batch. The browser drains one shared
file queue with those workers, bounded by queue depth, and reduces the worker
count to one only for explicit Data Saver intent. Browser CPU count and
estimated download connection type/speed do not constrain the direct upload
data plane. Retryable failures retain bounded backoff and confirmed-offset
reconciliation. The implementation and pinned upstream evidence are recorded
in [upload-worker-pool-upstream-evidence.md](./upload-worker-pool-upstream-evidence.md).

## Identity and administration

| Method | Route | Request summary |
|---|---|---|
| `POST` | `/api/v1/bootstrap/options` | `bootstrapToken`, `displayName` |
| `POST` | `/api/v1/bootstrap/verify` | `ceremonyID`, WebAuthn `credential` |
| `POST` | `/api/v1/registration/options` | `displayName`, optional `inviteToken` |
| `POST` | `/api/v1/registration/verify` | `ceremonyID`, WebAuthn `credential` |
| `POST` | `/api/v1/authentication/options` | Empty JSON object |
| `POST` | `/api/v1/authentication/verify` | `ceremonyID`, WebAuthn `credential` |
| `POST` | `/api/v1/logout` | Empty JSON object |
| `GET`, `PATCH` | `/api/v1/me` | Read identity/roles/CSRF; patch `displayName` only |
| `GET` | `/api/v1/me/passkeys` | Safe passkey metadata |
| `POST` | `/api/v1/me/passkeys/options`, `/verify` | Add-passkey ceremony |
| `DELETE` | `/api/v1/me/passkeys/{credentialID}` | Remove a verified non-final passkey |
| `GET`, `POST` | `/api/v1/admin/invites` | List metadata; create an expiring one-use link |
| `DELETE` | `/api/v1/admin/invites/{inviteID}` | Revoke an invite |
| `GET` | `/api/v1/admin/users` | `limit`, opaque `cursor` |
| `POST` | `/api/v1/admin/users/{userID}/disable`, `/enable`, `/admin` | Account/role mutation |
| `DELETE` | `/api/v1/admin/users/{userID}/admin` | Revoke admin subject to final-admin guard |
| `POST` | `/api/v1/admin/users/{userID}/recoveries` | Optional `ttlSeconds`; returns the raw link once |
| `POST` | `/api/v1/recovery/options`, `/verify` | Token-bound credential recovery |

## Files and direct transfers

| Method | Route | Request or query |
|---|---|---|
| `GET` | `/api/v1/files` | `path`, `limit`, opaque `cursor`, `sort=name|modified|size|kind`, `order=asc|desc` |
| `GET` | `/api/v1/files/stat` | `path` |
| `POST` | `/api/v1/directories` | `path`, optional `conflict`, `expectedVersion` |
| `POST` | `/api/v1/uploads` | `path` or directory `path` plus `name`, `size`, `mediaType`, `resumable`, optional conflict/version |
| `POST` | `/api/v1/uploads/batch` | `uploads` with 1–10,000 upload initialization objects; each may carry a stable `idempotencyKey` for item-level replay independent of the batch request key. Capabilities carry safe `batchID`, `batchIndex`, and `batchCount` recovery bindings. |
| `POST` | `/api/v1/uploads/batch/complete` | `uploads` with 1–10,000 ordered `uploadID`/provider-attested `crc32c` items; one idempotency key binds the complete atomic publication. |
| `DELETE` | `/api/v1/uploads/batch` | `uploadIDs` with 1–10,000 ordered IDs and optional `batchID`. The batch ID is accepted only for the complete admitted batch and enables one compact cancellation bitmap; partial/legacy selections remain individually bound. |
| `POST` | `/api/v1/uploads/plan/sizes` | `items` with 1–10,000 client transfer `id`, virtual destination `path`, and local `size`; returns per-item target metadata, `fingerprintRequired`, and an opaque exact-phase token. |
| `POST` | `/api/v1/uploads/plan/fingerprints` | Opaque size-phase `token` and 1–10,000 ambiguous items with `id`, `path`, `size`, canonical `md5`, and `crc32c`; returns `upload`, `skip`, or same-owner metadata-only `reuse` decisions. |
| `GET` | `/api/v1/uploads/{uploadID}` | Owner-scoped `active`, `completed`, `aborted`, or `expired` state with the safe provider-confirmed offset; no capability or provider-native material |
| `POST` | `/api/v1/uploads/{uploadID}/complete` | `path`, `size`, `mediaType`; stored-object fingerprints come from provider metadata |
| `DELETE` | `/api/v1/uploads/{uploadID}` | Abort and invalidate the upload capability |
| `POST` | `/api/v1/downloads` | `path`, exact `version`, optional `preview` |
| `POST` | `/api/v1/files/copy`, `/move` | Singular `source`/`destination`, or `items` with 1–10,000 source/destination objects; optional conflict/version fields. |
| `POST` | `/api/v1/files/trash` | `paths` with 1–10,000 virtual paths. |
| `GET` | `/api/v1/operations/{operationID}` | Poll a session-owner-scoped provider or aggregate operation |
| `GET` | `/api/v1/trash` | `limit`, owner-scoped opaque `cursor` |
| `POST` | `/api/v1/trash/restore` | `trashIDs` with 1–10,000 records and optional `conflict`; one atomic owner-namespace publication. |
| `POST` | `/api/v1/trash/delete` | `trashIDs` with 1–10,000 records; one atomic permanent metadata removal. |
| `POST` | `/api/v1/trash/{trashID}/restore` | Optional `conflict` (`fail` default or `rename`) |
| `DELETE` | `/api/v1/trash/{trashID}` | Permanently delete exactly one trash record |
| `POST` | `/api/v1/trash/empty` | `confirm: true`; bounded to 10,000 records per call. |

`preview: true` is accepted only for provider-validated PNG, JPEG, GIF, WebP, PDF, and UTF-8 `text/plain` within `ENDLESSFS_TEXT_PREVIEW_MAX_BYTES`. HTML, JavaScript, SVG, XML, office, unknown, oversized, and media-spoofed files remain attachment-only.

Every entry returns `fileCount`. A file has `fileCount: 1`. A directory has the persisted recursive number of descendant logical files; directories themselves are not counted. File-entry `size` is the file's byte length. Directory-entry `size`, including the entry returned for `/`, is the persisted recursive sum of all descendant file bytes in that live or trash tree. Both directory aggregates are retrieved by verifying constant-size root and manifest metadata without scanning the subtree. Empty directories return `size: 0` and `fileCount: 0`; a zero-byte file returns `size: 0` and `fileCount: 1`.

In schema 009, upload capabilities target their final immutable blob key and completion publishes metadata only after provider checksum verification. Same-owner file copies reuse that blob. Folder copy, move, Trash, restore, and deletion attach or detach one immutable snapshot and update only affected namespace ancestors; their synchronous control-plane cost does not grow with the number of descendants and they do not issue provider byte copy/move/delete operations.

Upload planning is a metadata-only control-plane optimization over schema-011
namespace state. The size route may build or incrementally refresh a separate
rebuildable projection using state-backend GET/PUT operations; the warm size
and fingerprint routes issue only state-backend GETs. Neither route calls the
file backend, reads an object body, accepts a provider key, or returns a raw
checksum. The token binds the exact owner, projection root, namespace root, and
expiry. A namespace change between phases returns conflict rather than using a
stale source. Reuse is executed by the existing batch-copy API with the
returned virtual source path and logical version, so its authorization,
idempotency, atomic publication, and conflict rules remain unchanged. Detailed
request economics and browser recovery evidence are in
[smart-upload-planning-evidence.md](./smart-upload-planning-evidence.md).

The browser persists upload plan decisions, batch membership, stable item and
transaction idempotency keys, progress, and terminal state in IndexedDB. A
refresh or connection loss resumes the same batch rather than allocating new
provider sessions. Admission, completion, and whole-batch cancellation each
use one browser control-plane request and bounded server-side provider
concurrency; object bytes continue to flow only between the browser and the
direct provider capability.

`GET /api/v1/files` returns `{ "current": Entry, "entries": [Entry...], "nextCursor": "..." }`. `current` is the directory represented by `path`; its `size`, `fileCount`, and every child row come from the same immutable manifest snapshot. Every subsequent page selected by `nextCursor` repeats that exact `current` entry even if the live directory changes between requests.

Each successful `GET /api/v1/trash` row preserves the prior trash-record fields and adds exact `size`, `fileCount`, and file `mediaType`. Directory media type is absent. The service joins each state page to one bounded snapshot lookup in the persisted trash root, rather than issuing one `Stat` per row. Canonical trash records remain schema v1: buckets upgraded from either supported predecessor use the automatically migrated trash directory tree without rewriting application records.

`GET /api/v1/public/shares/{token}` returns safe `root`, `current`, and child entries with the same `size` and `fileCount` contract. A nested directory's `current` aggregates describe that nested target, while `root` continues to describe the original shared root. Neither response exposes an owner path or provider identity.

`GET /api/v1/public/shares/{token}/stat?path=...` returns the same safe share-relative entry metadata for one exact item inside the shared root. It exists so a copied public-preview URL can restore one file without walking paginated directory listings. Invalid, escaped, stale, revoked, expired, disabled-owner, or changed-root requests all return the same not-found boundary.

## Duplicate reconciliation foundation

These authenticated owner routes expose the duplicate catalog introduced by schema 004 and served from schema 009's authoritative state domains plus rebuildable projections for the separate Part 2 browser workflow. They return only virtual paths, portable logical versions, counts, and opaque group/cursor/plan values. Provider keys, provider-native versions, and raw checksums are never public fields. A logical reflink is not a second physical file duplicate and therefore does not inflate reclaimable bytes.

| Method | Route | Request or query |
|---|---|---|
| `GET` | `/api/v1/duplicates/groups` | Optional `kind=file|directory`, `includeIgnored=true|false`, `limit`, and owner-scoped opaque `cursor` |
| `GET` | `/api/v1/duplicates/groups/{groupID}/occurrences` | `limit` and owner/group-scoped opaque `cursor` |
| `PUT` | `/api/v1/duplicates/groups/{groupID}/ignore` | `{ "ignored": true|false, "expectedRevision": n }`; CSRF and exact origin required |
| `POST` | `/api/v1/duplicates/directories/compare` | `{ "left": {"area":"live|trash","path":"/..."}, "right": {...} }`; CSRF and exact origin required |
| `POST` | `/api/v1/duplicates/directories/overlaps` | One `directory`, optional `includeIgnored`, `limit` 1–100, and owner/manifest/gate-scoped opaque `cursor`; CSRF and exact origin required |
| `PUT` | `/api/v1/duplicates/directories/ignore` | Two directory locations plus `ignored` and optional `expectedRevision`; the stable directory pair, not either path, is the preference identity |
| `POST` | `/api/v1/duplicates/directories/reconciliation-preview` | Two disjoint live locations plus `removeFrom=left|right`, optional `limit` 1–100, and optional opaque `cursor`; CSRF and exact origin required |
| `POST` | `/api/v1/duplicates/directories/reconcile` | `{ "planToken": "..." }`; CSRF, exact origin, and `Idempotency-Key` required |

A group row is `{ "id", "kind", "occurrenceCount", "size", "fileCount", "reclaimableBytes", "ignored", "ignoreRevision"? }`. A file occurrence has `fileCount: 1`; a directory occurrence uses its recursive count. `reclaimableBytes` is exact for that one group and counts all but one occurrence. Directory rows and their descendant file rows overlap, so clients must not add unrelated row values into a bucket-wide total.

Directory comparison returns `exact`, `commonFiles`, `commonBytes`, and both `leftOnly*` and `rightOnly*` totals. Intersection is a multiset of confirmed file identities, so repeated equal files are counted no more times than they appear on both sides. Exact directory equality additionally requires the complete name-sensitive nested structure to match.

Overlap candidates return `sharedSketch`, `sketchSize`, the exact comparison, pair `ignored`/`ignoreRevision`, and independent exact-group ignore state. The sketch fraction is candidate-discovery evidence, not the reported exact overlap percentage. Default pages omit either ignored relationship; `includeIgnored` supports the Part 2 collapsed ignored section.

Preview returns at most 100 removal/keep pairs, the comparison, page reclaimable bytes, an optional next cursor, and a short-lived authenticated `planToken`. Ignored groups are omitted. Reconcile revalidates the gate, selected manifests, occurrence group/version, and ignore revisions, then moves only the pinned removal paths to Trash. Stale items fail safely and no duplicate route permanently deletes data.

## Generated image previews (v1.1)

These authenticated-owner routes use the optional independent preview store. Resolve is a POST because it may lazily generate a missing artifact. All three routes derive owner scope from the session; content identities, store keys, and bucket configuration are never public fields.

| Method | Route | Request or query |
|---|---|---|
| `POST` | `/api/v1/previews/resolve` | `items` with 1–64 exact `path`, `version`, and configured `variant` values; CSRF and exact origin required. |
| `POST` | `/api/v1/previews/generations` | One exact `path`, `version`, `variant`, and `action=generate|regenerate`; also requires `Idempotency-Key`. |
| `GET` | `/api/v1/previews/operations/{operationID}` | Poll one authenticated-owner-scoped explicit generation result. |

Resolve items return `disabled`, `unsupported`, `ineligible`, `missing`, `generating`, `ready`, `failed`, or `unavailable`. `ready` includes static WebP metadata and a short-lived exact-artifact capability on the separate preview data origin. Automatic age/size exclusions never retrieve the original. Generate and Regenerate bypass those two automatic policies but not authorization, format, hard byte, pixel, dimension, concurrency, or timeout limits.

## Public shares

| Method | Route | Request or query |
|---|---|---|
| `GET`, `POST` | `/api/v1/shares` | List safe owned metadata; create from `path` and optional `expiresAt` |
| `DELETE` | `/api/v1/shares/{shareID}` | Revoke an owned share |
| `GET` | `/s/{token}` | Generic public shell; token is never embedded into asset URLs |
| `GET` | `/api/v1/public/shares/{token}` | Relative `path`, `limit`, and scoped `cursor` |
| `POST` | `/api/v1/public/shares/{token}/downloads` | Relative `path`, exact `version`, optional `preview` |

Successful public-share metadata is `{ "root": PublicEntry, "current": PublicEntry, "entries": [PublicEntry...], "nextCursor": "..." }`. `root` is always the originally shared file or directory. `current` is the relative target currently being viewed and, for a directory, carries recursive `size` and `fileCount` from the listing snapshot. A file share returns the root as `current` with `fileCount: 1`. Public errors deliberately do not distinguish absent, expired, revoked, disabled-owner, moved, replaced, or trashed roots. Folder output paths are relative to the recorded root, and public routes cannot upload, mutate, re-share, expose owner/provider identifiers, or escape that subtree.
