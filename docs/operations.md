# EndlessFS v1 operations

This guide covers the feature-complete mock-backed v1. It is suitable for local acceptance and demonstrations, not production storage.

## Runtime model

EndlessFS starts one Go control-plane binary and an in-process deterministic storage provider with a separate loopback HTTP data-plane listener. Account metadata, sessions, files, uploads, trash, shares, and provider operations are held in memory. Restarting the process intentionally starts an empty instance.

There is no v1 database, persistent filesystem requirement, background worker, migration job, backup format, or restore command. Do not place valuable data in this provider. Production backup, restore, retention, durability, and disaster-recovery procedures must be designed and validated with a future durable provider and are explicitly outside v1 acceptance.

## Start and stop

Generate independent bootstrap and session secrets, export them only in the process environment, and start through Nix as shown in the README. Remove `ENDLESSFS_BOOTSTRAP_TOKEN` from the environment after the first administrator exists. Use HTTPS with a matching base URL and RP ID for any non-loopback listener.

`SIGTERM` and `SIGINT` stop admission and give the control and data listeners up to ten seconds to shut down. The HTTP servers enforce header/read/write/idle limits, and control documents are independently bounded. Because v1 state is memory-only, shutdown integrity means no false success or in-process invariant corruption; it does not make state survive process termination.

## Health and observation

- `GET /healthz` reports process liveness.
- `GET /readyz` reports that the assembled application is ready to serve.
- Logs are structured JSON. `ENDLESSFS_LOG_LEVEL` accepts exactly `debug`, `info`, `warn`, or `error`.
- Central redaction remains active at every level. Logs must not be treated as a file/account audit trail.

Capability responses and public configuration use `no-store`. Browser and server diagnostics deliberately omit token-bearing query strings, request bodies, authorization values, provider keys, full user paths, and raw credential material.

## Build and release verification

Run the acceptance gate from a clean checkout:

```console
nix flake check --print-build-logs
nix build
nix build .#container
nix build .#release
```

The release output includes `SHA256SUMS`, `RELEASE-INVENTORY.txt`, the binary/archive, OCI archive, dependency and license inventories, installed-theme inventory, release notes, and the acceptance record. Verify `SHA256SUMS` before distribution. The inventory records the source revision, `flake.lock` hash, pinned vulnerability database hash, Go toolchain, artifact hashes, thresholds, provider kind, and explicit no-cloud/no-deployment status.

GitHub publishing is tag-driven. Protected `vMAJOR.MINOR.PATCH` tags cause the release workflow to repeat the full gate, push version and `latest` tags to GHCR, and attach the Nix-built evidence. Applying branch/tag rules is an explicit administrator operation through the protected Repository Policy workflow.

## Failure handling

Use stable problem kinds and operation states rather than provider internals when diagnosing a request. Retriable provider faults and resumable offsets are surfaced explicitly. Idempotency keys make mutation retries safe only for the same authenticated owner, operation kind, and request fingerprint; never reuse a key for different intent.

If the process exits unexpectedly, discard the ephemeral instance and restart cleanly. There is no supported recovery of its in-memory state. A release must not be promoted as a durable service until a real provider has passed specification section 23 and acquired provider-specific operations, backup, restore, and incident-response procedures.
