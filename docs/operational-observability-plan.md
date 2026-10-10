# Operational observability implementation plan

## Decision and present implementation

The operator's October 8, 2026 decision replaces the blanket prohibition on
telemetry with an observability-first engineering policy. Specification section
16 is normative. Telemetry is encouraged; privacy, provider portability,
reproducibility, and storage correctness remain guarantees.

The current application emits structured request result/duration and migration
events. Kubernetes metrics identify replica CPU, working set, readiness, and
OOM termination. The GCS module closure already contains pinned OpenTelemetry
v1.45.0 packages, but dependency presence does not prove application trace export.
The backend baseline now implements restricted metrics/aggregate profiles,
manual application traces and bounded asynchronous lifecycle events. Its
signal/failure/privacy qualification is recorded in
`docs/operational-observability-evidence.md`. Live collector wiring, child
profile streams and production load/profile overhead remain qualification gaps.

The xlab deployment already provides Alloy OTLP receivers on 4317/4318, forwarding
traces to Tempo, metrics to Mimir, and logs to Loki. Alloy also scrapes annotated
pods. Pyroscope is deployed, but its service/profile discovery returned no data
for the October 8 upload incident. Reuse this infrastructure; a new monitoring
stack is unnecessary.

## Implementation and remaining diagnosis work

The first baseline is implemented as recorded in the evidence. The following
program remains the target for extending signal detail and deployment proof;
source-size/pixel bands, individual codec sub-phases, continuous child profiles
and live dashboard/collector correlation are not claimed complete.

1. Add an operator-restricted diagnostics listener separate from public HTTP,
   shares, and direct file data. Expose bounded application metrics and only
   aggregate CPU/allocation/live-heap profiles. Do not expose command-line,
   environment, raw-heap, or arbitrary debug endpoints. Keep the browser
   self-contained and introduce no third-party browser code.
2. Collect request latency/count/result and in-flight work; Go heap/allocation/GC
   pressure; upload phase activity/queue depth and retries; metadata decode
   input/expanded bytes and duration; preview queue waits, active workers,
   source-size/pixel bands, worker duration, coarse failures, and worker resource
   measurements. Give parent process and preview child accounting distinct
   signals because both contribute to one container's memory limit.
3. Emit safe preview and upload lifecycle/failure events with correlation IDs,
   phase enums, result classes, and durations. Record worker termination before
   returning the existing safe error. Never log raw codec/provider exception
   text, capability material, paths, identities, or file bodies.
4. Wire Mimir/Loki/Pyroscope through the existing Alloy deployment, add an
   EndlessFS resource/phase dashboard, and alert on OOM/restart, queue growth,
   error/retry rate, export loss, and sustained memory pressure. Thresholds come
   from measured workload baselines and configured resource limits.

Application collection should be part of normal engineering and production
qualification. Export destinations and restricted listener exposure are
operator configuration; there is no mandatory hosted collector or vendor
account. A collector outage cannot change mutation behavior or readiness.

## Distributed tracing

Use the pinned official OpenTelemetry Go SDK and exporter with explicit
dependency/closure review. Add manual spans for HTTP route templates, upload
phases, provider requests by role/kind, metadata decoding, conditional publication,
conflict/recovery, migration, and preview queue/worker execution. Correlate safe
request logs with trace IDs without putting IDs in metric labels.

Export a closed attribute allowlist. Do not simply enable unrestricted SDK/HTTP
instrumentation: provider spans may carry object paths or capability URLs.
Validate/redact attributes before export, omit raw exceptions and baggage, and
prove secret/path denial with representative SDK and application spans. Export
uses a bounded queue, sampling, timeouts, and bounded shutdown. Measure dropped
and export-failed samples without retrying indefinitely or backpressuring uploads.

Official references: [OpenTelemetry collector security guidance](https://opentelemetry.io/docs/security/config-best-practices/)
and [Pyroscope profile types](https://grafana.com/docs/pyroscope/latest/configure-client/profile-types/).

## Acceptance and rollout

| Guarantee | Required proof |
| --- | --- |
| Useful diagnosis | Deterministic positive/error/timeout/conflict schedules emit the correct phase, count, duration, and worker outcome. A controlled upload can distinguish metadata allocation, provider retries, preview processing, and container termination. |
| Privacy | Sentinel paths, names, IDs, bodies, cookies, CSRF, signed URLs, keys, and raw exceptions never reach exported attributes, logs, or profile labels. |
| Bounded overhead | Compare enabled/disabled collection on the same representative workload; measure CPU, allocations, retained bytes, queue capacity, cardinality, and export loss before selecting ratchets. |
| Collector independence | Offline, slow, rejecting, and unavailable collectors leave commits, upload completion, readiness, shutdown bounds, and replica convergence unchanged. |
| Portability | No diagnostic state enters canonical authority, schema ledgers, or checkpoints; disabled/unconfigured exporters require no cloud resource or network service. |
| Restricted diagnostics | Public and share routes cannot access profiles/metrics; deployment network/service boundaries admit only configured collectors. |

Run the focused Nix gates and the complete local PR gate before push. Deployment
changes, if needed, go through xlab-deployments PRs and Flux. Verify actual samples
arrive and correlate with a controlled upload after rollout. Checklist 22.11.1
stays unchecked until each signal and guarantee is implemented and qualified.

## Remaining incident uncertainty

Both replicas OOM-killed four times under a 512 MiB container limit. Configured
preview concurrency is one per replica, with a semaphore before source download
and codec execution. Loki returned no completed preview request events during
10:48–11:06 UTC; this does not exclude a worker killed before logging completion.
The control metadata decode workload independently showed substantial allocation
amplification. Neither observation alone proves the production allocation cause.
Collect the new resource/phase evidence before claiming a memory-limit change or
decoder optimization makes the original 14,169-file upload reliable.
