# Run the container image with GCS

The image uses [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials). Give the workload a dedicated service account through your platform's workload identity; do not put a service-account key in the image.

## GCS prerequisites

- Use one private bucket for the authoritative file/state roles, or distinct private file and state buckets. Generated previews use a third, distinct private bucket. Enable public access prevention and uniform bucket-level access on each bucket.
- Grant the workload service account `roles/storage.objectUser` on every configured bucket.
- Enable the IAM Service Account Credentials API. Grant the workload identity `iam.serviceAccounts.signBlob` on the signing service account, usually with `roles/iam.serviceAccountTokenCreator`.
- Configure file-bucket CORS for the exact `ENDLESSFS_BASE_URL` origin. Allow `GET`, `HEAD`, and `PUT`; allow `Content-Type`, `Content-Range`, and `Range`; expose `Content-Length`, `Content-Range`, `Range`, and `X-Goog-Generation`.
- When GCS previews are enabled, configure preview-bucket CORS for the same exact origin with browser `GET` and `HEAD` only. Preview writes are exclusively server-side. Never use `*` for either origin. A distinct state bucket needs no browser CORS.

## Required configuration

```console
export ENDLESSFS_SESSION_SECRET="$(nix run .#generate-secret)"
export ENDLESSFS_BOOTSTRAP_TOKEN="$(nix run .#generate-secret)" # first admin only
export ENDLESSFS_WRITER_SET_ID="$(nix run .#generate-secret)"
export ENDLESSFS_PREVIEW_KEY_SECRET="$(nix run .#generate-secret)"

docker run --rm -p 8080:8080 \
  -e ENDLESSFS_LISTEN_ADDR=0.0.0.0:8080 \
  -e ENDLESSFS_BASE_URL=https://drive.example.com \
  -e ENDLESSFS_STORAGE_PROVIDER=gcs \
  -e ENDLESSFS_GCS_FILE_BUCKET=endlessfs-files \
  -e ENDLESSFS_GCS_STATE_BUCKET=endlessfs-state \
  -e ENDLESSFS_GCS_PREVIEW_BUCKET=endlessfs-previews \
  -e ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT=endlessfs@example-project.iam.gserviceaccount.com \
  -e ENDLESSFS_PREVIEW_PROVIDER=gcs \
  -e ENDLESSFS_PREVIEW_FORMATS=image \
  -e ENDLESSFS_PREVIEW_KEY_SECRET \
  -e ENDLESSFS_SESSION_SECRET \
  -e ENDLESSFS_BOOTSTRAP_TOKEN \
  -e ENDLESSFS_WRITER_SET_ID \
  ghcr.io/applyinnovations/endlessfs:VERSION
```

The example assumes the container runtime supplies ADC. Terminate TLS at the ingress or reverse proxy; `ENDLESSFS_BASE_URL` must be the public HTTPS origin.

Set `ENDLESSFS_PREVIEW_PROVIDER=gcs` to enable durable shared image previews, or leave it unset/`disabled` to run the media browser without generated thumbnails. The preview bucket must differ from both authoritative buckets. `ENDLESSFS_PREVIEW_KEY_SECRET` derives opaque binding and generation identities and must remain stable and identical across replicas. The application writes immutable WebP artifacts and manifests server-side, publishes visibility with conditional head updates, and gives the browser only short-lived signed `GET` capabilities. Provider lifecycle deletion is safe because previews are disposable and regenerated on demand.

Every server-written GCS object carries `Cache-Control: no-store` metadata. Cloud Storage can return that policy with case differences, multiple field lines, or additional directives. Startup and runtime validation parse the complete policy and require a syntactically valid bare `no-store` directive; an absent, malformed, or parameterized lookalike fails closed. GCS does not document a signed `response-cache-control` query override, so preview capabilities rely on the server-written object metadata and exercise the resulting response before readiness succeeds.

Omit `ENDLESSFS_GCS_STATE_BUCKET`, or set it equal to `ENDLESSFS_GCS_FILE_BUCKET`, for authoritative single-bucket mode. Keep the bucket selection, `ENDLESSFS_SESSION_SECRET`, `ENDLESSFS_WRITER_SET_ID`, and—when enabled—`ENDLESSFS_PREVIEW_KEY_SECRET` stable across restarts and identical on every replica. Remove `ENDLESSFS_BOOTSTRAP_TOKEN` after creating the first administrator. Check `GET /healthz` for liveness and `GET /readyz` for readiness.

The authoritative and preview GCS paths are locally protocol-qualified, but a live GCS deployment still requires IAM, CORS, lifecycle, backup/restore, monitoring, and security validation. See [operations](./operations.md#gcs-identity-and-bucket-policy) for details.
