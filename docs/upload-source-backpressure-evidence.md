# Slow-source upload repair qualification

Release scope: v0.8.0. This record supplements the schema-012 identity and
conservation evidence. It does not claim live deployment or GCS throughput.

## Incident and observable guarantees

On v0.7.5, 64 external-drive RAW photos completed, but a fresh 100-photo attempt
stopped acknowledging bytes while the drive continued reading approximately
128 MB/s. The same 100 photos copied to internal SSD completed in about 194
seconds. Cancellation stopped the drive activity. Production replicas stayed
healthy and idle; no image thumbnail jobs ran. An independent cancelled-batch
identity collision was reproduced in the portable engine and is repaired by
the adjacent schema-012 migration.

The client must continue at the source's available rate, remain cancellable,
bound foreground buffers, and avoid starting a network request before a slow
chunk is readable. Only provider acknowledgements count as uploaded bytes.
Completed, verified files must not wait indefinitely for another source.
Expiry, ownership, checksum verification, confirmed-offset resume, atomic
publication and provider portability remain mandatory.

## Candidate comparison

The opt-in Chromium hardware test used the same 100 external-drive photos,
2,102,194,176 bytes, with one serial source reader and candidate byte windows.
Each file was streamed into a local sink and independently verified by SHA-256;
the sink retained no photo bodies. No original was changed and no production
test copies were created by this qualification.

| Window | Time | Observed peak reserved source bytes | Result |
| --- | ---: | ---: | --- |
| One 8 MiB chunk | 70.530 s | 8 MiB | All 100 verified |
| Two chunks | 61.538 s | 16 MiB | All 100 verified |
| Four chunks | 57.472 s | 16 MiB | All 100 verified |

The selected four-chunk window has a hard 32 MiB source-buffer limit. It was
the fastest measured candidate and permits read/network overlap without
reintroducing independent native disk streams for 100 large files. The observed
16 MiB peak is not the worst-case bound: a slower destination can fill 32 MiB.
The existing two fingerprint workers have their separate bounded chunk memory.
CPU count and estimated download bandwidth do not control the upload pool.

These sequential trials may benefit from filesystem caching. The local sink
uses HTTP/1.1 and excludes control-plane admission, duplicate planning,
fingerprinting and live GCS publication. They prove that the selected reader
can finish this source cohort; they do not establish a cold-disk speedup,
identify every Chromium internal cause, or replace a live pressure test.

Run the explicit hardware qualification through the public Nix interface:

```sh
ENDLESSFS_SOURCE_QUALIFICATION_DIRECTORY=/path/to/authorized/photos \
  nix run .#test-e2e -- -run '^TestE2EUploadSourceHardwareQualification$' -v
```

## Deterministic replacement proofs

- `TestE2EUploadSourceBufferBudgetAndCancellation`: one-, two- and four-chunk
  reservations stay bounded, one reader runs, queued readers cancel, and every
  buffer/slot releases after cancellation.
- `TestE2EUploadSourceBackpressureRemainsResponsiveAndCompletes`: held source
  reads do not fail or block animation frames; all files complete after release
  with one admission and one completion request.
- `TestE2EUploadSourceReadFailureReleasesBudgetAndRequestsReconnection`: an
  unreadable source releases resources and asks for reconnection while an
  independent file completes.
- `TestE2EUploadStalledSourceDoesNotBlockVerifiedFiles`: the original wait-for-all
  rule failed. After the acknowledgement window ages, one verified partial
  batch publishes and refreshes, speed ages to zero, and remaining work can
  finish. This recovery case uses a partial and a final publication; it does
  not charge that extra transaction to every healthy upload.
- The existing 100-task test still fills six HTTP/1.1 connections, sends one
  admission and one completion, and coalesces one healthy-burst refresh. Lost
  data-plane response and original-session retry regressions remain mandatory.
- Memory and GCS run the same initiation/session lifetime contract. A six-hour
  resume preserves the destination and absolute expiry; tampered and expired
  leases fail. A second replica with a longer admission default cannot extend
  an existing transfer's canonical deadline.

New resumable sessions default to a configurable 24-hour absolute window rather
than inheriting the five-minute initiation deadline. GCS initiation signatures
remain at most five minutes by default. Existing records keep their original
deadlines. GCS session URIs are provider bearers that can remain usable for up
to one week; they are redacted and revoked by cancellation, and expired
canonical authority cannot publish. See the provider's
[resumable upload contract](https://docs.cloud.google.com/storage/docs/resumable-uploads).

## Economics and migration limits

The complete provider gate retains request counts and modeled marginal cost:
100-file admission is 105 calls, and each 2,001-item admission/completion/abort
workflow is 2,007 calls. Explicit current catalog entries and an append-only
schema-012 delta replace changed measurements; no supersession-name shim or
catalog exemption is used. The adjacent conservation edge is measured
separately, including its two metadata-only file listings. The composed
schema-002-to-012 inventory proof uses 26 interrupted/restarted listings and 15
ordinary listings, versus 24 and 13 for the prior suffix; both retain zero
file-body reads and checkpoint-page reuse.

The production console's data-SVG warnings were not reproduced by application
source: icons are inline SVG or same-origin assets, and the repository contains
no application data-image generator. Do not weaken CSP to suppress a possible
browser-extension badge warning. Its live initiator attribution remains an
inspection item for the post-deployment Chrome pressure test.

The exact final source tree must pass pr-check, online security freshness,
flake checks and browser-enabled coverage before push. Post-merge release and
deployment digest verification, followed by the authorized live photo upload,
remain separate stages.
