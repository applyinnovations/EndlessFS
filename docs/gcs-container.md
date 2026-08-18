# Run the container image with GCS

The image uses [Application Default Credentials (ADC)](https://cloud.google.com/docs/authentication/application-default-credentials). Give the workload a dedicated service account through your platform's workload identity; do not put a service-account key in the image.

## GCS prerequisites

- Use one private bucket for both roles, or private file and state buckets, with public access prevention and uniform bucket-level access.
- Grant the workload service account `roles/storage.objectUser` on every configured bucket.
- Enable the IAM Service Account Credentials API. Grant the workload identity `iam.serviceAccounts.signBlob` on the signing service account, usually with `roles/iam.serviceAccountTokenCreator`.
- Configure file-bucket CORS for the exact `ENDLESSFS_BASE_URL` origin. Allow `GET`, `HEAD`, and `PUT`; allow `Content-Type`, `Content-Range`, and `Range`; expose `Content-Length`, `Content-Range`, `Range`, and `X-Goog-Generation`. Never use `*` for the origin. A distinct state bucket needs no browser CORS.

## Required configuration

```console
export ENDLESSFS_SESSION_SECRET="$(nix run .#generate-secret)"
export ENDLESSFS_BOOTSTRAP_TOKEN="$(nix run .#generate-secret)" # first admin only
export ENDLESSFS_WRITER_SET_ID="$(nix run .#generate-secret)"

docker run --rm -p 8080:8080 \
  -e ENDLESSFS_LISTEN_ADDR=0.0.0.0:8080 \
  -e ENDLESSFS_BASE_URL=https://drive.example.com \
  -e ENDLESSFS_STORAGE_PROVIDER=gcs \
  -e ENDLESSFS_GCS_FILE_BUCKET=endlessfs-files \
  -e ENDLESSFS_GCS_STATE_BUCKET=endlessfs-state \
  -e ENDLESSFS_GCS_SIGNING_SERVICE_ACCOUNT=endlessfs@example-project.iam.gserviceaccount.com \
  -e ENDLESSFS_SESSION_SECRET \
  -e ENDLESSFS_BOOTSTRAP_TOKEN \
  -e ENDLESSFS_WRITER_SET_ID \
  ghcr.io/applyinnovations/endlessfs:VERSION
```

The example assumes the container runtime supplies ADC. Terminate TLS at the ingress or reverse proxy; `ENDLESSFS_BASE_URL` must be the public HTTPS origin.

Omit `ENDLESSFS_GCS_STATE_BUCKET`, or set it equal to `ENDLESSFS_GCS_FILE_BUCKET`, for single-bucket mode. Keep the state/file bucket pairing, `ENDLESSFS_SESSION_SECRET`, and `ENDLESSFS_WRITER_SET_ID` stable across restarts and identical on every replica using the storage set. Remove `ENDLESSFS_BOOTSTRAP_TOKEN` after creating the first administrator. Check `GET /healthz` for liveness and `GET /readyz` for readiness.

The GCS adapter is locally protocol-qualified, but a live GCS deployment still requires IAM, CORS, backup/restore, monitoring, and security validation. See [operations](./operations.md#gcs-identity-and-bucket-policy) for details.
