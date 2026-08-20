# EndlessFS HTTP API

This document fixes the v1 JSON field casing and control-plane routes implemented through Milestone 3. The normative security and behavior contract remains [v1-specification.md](./v1-specification.md).

## Conventions

- JSON request bodies use `application/json`, reject unknown or duplicate fields and trailing content, and are limited to 1 MiB. File bytes are never accepted by these routes.
- Authenticated mutations require the session cookie, exact `Origin`, and `X-EndlessFS-CSRF`. Mutations named by specification section 12.1 additionally require a printable 16–128 byte `Idempotency-Key`.
- Errors use `application/problem+json` with `type`, `title`, `status`, `code`, and `requestID`. Capability, authentication, and public-share responses are `Cache-Control: no-store`.
- Paths are canonical absolute virtual paths. Provider keys, buckets, owner prefixes, and caller-selected capability URLs are not request fields.
- Capability responses contain `url`, `method`, `headers`, and `expiresAt`. The browser sends file bytes directly to that URL on the separate data-plane origin.

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
| `POST` | `/api/v1/uploads/batch` | `uploads` with 1–100 upload initialization objects |
| `GET` | `/api/v1/uploads/{uploadID}` | Owner-scoped `active`, `completed`, `aborted`, or `expired` state with the safe provider-confirmed offset; no capability or provider-native material |
| `POST` | `/api/v1/uploads/{uploadID}/complete` | `path`, `size`, `mediaType`, optional `checksumSHA256` |
| `DELETE` | `/api/v1/uploads/{uploadID}` | Abort and invalidate the upload capability |
| `POST` | `/api/v1/downloads` | `path`, exact `version`, optional `preview` |
| `POST` | `/api/v1/files/copy`, `/move` | Singular `source`/`destination`, or `items` with 1–100 source/destination objects; optional conflict/version fields |
| `POST` | `/api/v1/files/trash` | `paths` with 1–100 virtual paths |
| `GET` | `/api/v1/operations/{operationID}` | Poll a session-owner-scoped provider or aggregate operation |
| `GET` | `/api/v1/trash` | `limit`, owner-scoped opaque `cursor` |
| `POST` | `/api/v1/trash/{trashID}/restore` | Optional `conflict` (`fail` default or `rename`) |
| `DELETE` | `/api/v1/trash/{trashID}` | Permanently delete exactly one trash record |
| `POST` | `/api/v1/trash/empty` | `confirm: true`; bounded to 100 records per call |

`preview: true` is accepted only for provider-validated PNG, JPEG, GIF, WebP, PDF, and UTF-8 `text/plain` within `ENDLESSFS_TEXT_PREVIEW_MAX_BYTES`. HTML, JavaScript, SVG, XML, office, unknown, oversized, and media-spoofed files remain attachment-only.

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

Public errors deliberately do not distinguish absent, expired, revoked, disabled-owner, moved, replaced, or trashed roots. Folder output paths are relative to the recorded root, and public routes cannot upload, mutate, re-share, or escape that subtree.
