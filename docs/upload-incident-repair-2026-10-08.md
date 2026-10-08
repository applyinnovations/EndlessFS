# October 8 upload incident repair

## Observed behavior

The 14,169-file Smart merge attempt stopped at a 10,000-item size request:
1,196,597 encoded bytes exceeded the server's existing 1 MiB control-body limit.
The smaller 119-file attempt encountered GCS response CORS failures, control
502/503s, and four OOM terminations on each 512 MiB replica. It was cancelled by
the operator's explicit instruction. Eight transfer outcomes were complete,
60 failed, and the remaining 51 cancelled; this is not proof of eight newly
published physical files, because Smart merge can reuse or skip content.

The original failed queue remained visible and polluted subsequent aggregate
volume/progress. The repair adds clearing without deleting completed files.

## Preserved guarantees and proofs

| Area | Repair and qualification |
| --- | --- |
| Folder intake | Capture native handles/entries/files synchronously during the drop event, before awaiting options. `TestE2EProductionFolderDropRetainsSourcesAcrossUploadOptions` uses Chromium's protected drag data store and the portable engine. The old synthetic fixture did not exercise this lifetime. |
| Control-body limits | Count exact JSON UTF-8 bytes, including each envelope, token, escaped path, and logical version. Partition sizes, fingerprints, and reuse requests while retaining the 10,000-item limit. `TestE2ESmartMergePartitionsUTF8RequestsWithinControlBodyLimit` fails on a 1,695,501-byte request before repair and completes 900 long-path reuse decisions through the portable engine after repair. |
| Snapshot safety | Complete fingerprint reads against one pinned token before its reuse mutations change the namespace root. Every reuse partition retains stable transfer-derived idempotency keys and source/target logical-version conditions. |
| GCS browser transfers | Bind resumable-session initiation to the configured exact application Origin. The protocol fake now preserves initiation-origin behavior; its strengthened contract rejects missing-origin responses before repair and verifies two browser PUT chunks, 308/200, and exposed Range after repair. Invalid origins fail configuration. |
| Failed clearing | Item, folder-failed-members, and bulk actions clean up unfinished owner-scoped sessions before deleting local item/source/empty-group records. Complete admitted batches use compact batch cancellation; partial batches remain safe. Permission/provider failures retain history. Ambiguous terminal conflicts require safe status verification. |
| History and isolation | Tombstone delayed persistence references; preserve newly queued sources/workers during asynchronous restoration. Browser tests verify reload, owner isolation, completed files, and pending/complete mixed-group siblings over the portable engine. Persisted terminal failures require explicit retry or clearing after reload; source reconnection and finalization-only retry remain available. |
| Canonical decoding | Return owned encoder buffers directly and avoid validating pack pages twice during exact canonical-wire comparison. All structural/digest/binding/canonical checks remain, and authoritative bytes/semantics are unchanged. Existing corruption and migration fixtures remain required gates. |

## Decoder comparison

One valid 8-page, 128-record repetitive metadata fixture produces 36,903 wire
bytes. Values represent control records, not file bodies. On Apple M2 with the
pinned Nix Go toolchain:

| Candidate | Allocated bytes/op | Measured duration/op |
| --- | ---: | ---: |
| Existing decoder | 49,609,160 | 50.35 ms |
| Owned canonical buffers | 41,557,058 | 40.92 ms |
| Owned buffers plus one page-validation pass | 37,867,421 | 37.90 ms |

These are local characterization measurements, not production latency claims.
The selected candidate improves allocation by about 24% and preserves the
provider request/role/subsystem vector, transferred bytes, critical-path shape,
created/retained objects, conflict/recovery behavior, and durable encoding. The
40 MiB allocation ratchet is derived from the selected measurement. Its
regression fails on the original decoder at 48,544,823 bytes/op after warmup.

Race instrumentation changes the measured allocation program, including Go's
instrumented encoder-pool behavior. The same 10-iteration comparison measures
82,418,575 bytes/op before repair and 63,614,468 after repair under `-race`.
The separate 70 MiB instrumented ratchet rejects the original decoder at
81,416,252 bytes/op and accepts the selected implementation. Both modes execute
the same workload and denial/format tests; neither check is skipped. The tighter
40 MiB ordinary ratchet remains unchanged. Instrumented allocations are not a
production memory estimate.

This reduces avoidable copying; it does not prove that 100 concurrent metadata
requests plus a preview worker fit the deployment limit. Cross-request decode
sharing, decode admission, streaming validation, and measured deployment sizing
remain candidates for further qualification. Do not call the memory incident
resolved from this benchmark alone.

## Release limits

No schema epoch is appended: canonical records, application authority, and
portable semantics remain schema 011. The repair must pass all local PR gates
on its exact tree before push. Repeat controlled browser/GCS multi-chunk and
volume qualification after the user merges and GitOps rollout is complete.
Operational telemetry is now encouraged by specification section 16; its
implementation gaps and deployment plan are explicit in
`docs/operational-observability-plan.md`.
