# Backend observability implementation and qualification

The observability baseline adds operator-only Prometheus metrics and aggregate
Go CPU/allocation/live-heap profiles, manual OpenTelemetry spans exported using
the official OTLP/HTTP exporter, and bounded asynchronous lifecycle events.
Signals are non-authoritative: they add no schema epoch, writer identity,
provider object, checkpoint dependency, browser analytics, or external service
required for correctness or readiness. Deployment and live sample delivery are
separate qualification steps.

## Signal coverage

| Layer | Implemented signals |
| --- | --- |
| HTTP | Closed route templates, coarse results, duration histograms and active requests; standard incoming trace context without baggage; trace IDs in safe request logs. Authentication, administration, namespace, trash, shares and theme workflows enter through this common boundary. |
| Provider | All atomic object and direct-transfer operations by state/file/preview/shared role. Streaming reads finish on close. GCS wire attempts use the reviewed economics classifier, distinguishing logical calls from actual SDK retries. No key, native version, lease, capability URL, header or body is an attribute. |
| Uploads | Size/fingerprint planning, admission, status, completion and cancellation span/activity/result/latency measurements, including batches. |
| Metadata | Pack and cumulative upload-lease segment decoding, input/expanded byte totals, duration and errors. The canonical encoding and allocation ratchets remain unchanged. |
| Publication | Conditional domain commits, lost-response head recovery, and genuine conflicting-head retries. Provider signals also cover freeze, checkpoint and maintenance work. |
| Migration | Startup/open duration, provider spans, and closed stage/role object, byte and resumed-progress gauges before application readiness. A zero total can mean inventory is still incomplete. |
| Previews | Queue wait/active work, source fetch/read, complete generation, artifact persistence, worker duration/error class, and completed child CPU time/max resident memory/exit class. Startup self-tests and bounded cleanup retain observation context. |
| Process | Build identity, Go heap allocated/in-use/system bytes, cumulative allocations, GC count/pause and goroutine count. Aggregate CPU, heap and allocation pprof samples contain code locations, not raw heap dumps. |

The Go profiles describe the parent process. Child usage is measured separately
from `ProcessState`/`rusage` on Linux and Darwin. Max RSS is a completed-child
high-water measurement, not live combined parent-plus-child memory. One-shot
child CPU/heap profile streams are not exposed. A container killed before
reporting can lose its final event; Kubernetes OOM/restart and container metrics
remain necessary. Browser-to-GCS file traffic bypasses Go and still needs browser
network/performance monitoring. These boundaries must not be presented as a
complete production OOM diagnosis.

## Security and failure boundaries

- Metrics use fixed operation/role/result arrays and a closed route inventory.
  Unknown dimensions are ignored; unknown wire request kinds use a bounded
  `provider.http.unknown` signal. No per-user or per-file series are created.
- The SDK provider is local rather than installed globally. Opaque provider-SDK
  instrumentation is disabled. Before queueing, spans must have the exact
  operation/name/role/result/optional-route vocabulary and no arbitrary
  attributes, events, links, raw status descriptions or vendor trace state.
  Instrumentation scope is fixed. Resources include only
  the constant service name and validated build version; no environment/host
  detectors are used.
- Export retains at most 256 sampled spans plus a 64-span batch. Default sampling
  is 10%. Export is asynchronous, with one-second timeout, no retry, no proxy or
  redirects, no environment headers, and a 1 MiB response-read limit. Failed,
  dropped and exported sample counts remain visible. Shutdown cancels exports
  and is bounded to two seconds; pending samples can be discarded.
- Lifecycle logging has a separate 64-event nonblocking queue. A blocked sink
  drops events and increments a counter rather than blocking storage work.
  Events contain only operation, role, result, duration and trace ID.
- Diagnostics requires a distinct 256-bit bearer secret in the Authorization
  header. Tokens in URLs are rejected. The public Drive handler denies metrics
  and debug-profile paths. The private handler exposes only `/metrics`, binary
  `/debug/pprof/heap`, `/debug/pprof/allocs`, and `/debug/pprof/profile`.
  Command line, goroutine stacks, raw heap dumps, runtime execution traces and
  debug text are absent. CPU captures are one to ten seconds; profiles have an
  8 MiB output limit and a single capture slot. A busy capture returns 429.

Profiles and metrics can be pulled continuously by existing Alloy components.
Restrict the listener/service/network boundary to the collector and store its
bearer secret through the deployment's encrypted-secret workflow. Never expose
the diagnostic listener through the public ingress.

## Executable evidence

Missing signal and authenticated-profile valid paths were first reproduced with
failing tests. Qualification includes:

- concurrent accounting, double-end denial, closed dimensions, typed errors and
  coarse panic classification without changing panic behavior;
- parent/child trace hierarchy, global SDK isolation, raw attribute/error denial,
  official protobuf receipt, and a 503 receiver receiving exactly one attempt;
- a deterministically blocked exporter with overflow, and real portable Create,
  CompareAndSwap/Get and stale-writer denial while collection remains stalled;
- diagnostic bind failure and unreachable collector leaving readiness and the
  canonical writer configuration unchanged;
- authenticated aggregate heap/allocation profiles, CPU generation/cancellation,
  forbidden endpoint/query denial, and bounded profile output;
- a real GCS SDK retry: one logical Head, one failed wire attempt and one
  successful wire attempt, without exporting the sentinel key/error/URL;
- exact equality of delegated provider event vectors with observation enabled or
  disabled, including bytes, kinds, conditions and denial outcomes;
- existing 1,024-item planning and 2,001-item segmented upload lifecycle ratchets
  running with telemetry enabled, without changing their budgets or scale;
- real image worker completion producing separate lifecycle/resource signals.

Browser-enabled coverage also exposed a viewer race: an earlier image fetch
could finish while regeneration was polling and clear the newer progress
notification. The unchanged contention assertion reproduced the failure;
viewer image completion now preserves active-generation status, and successful
generation clears it explicitly. Three consecutive focused browser runs passed.

The focused Nix unit/contract/provider gates and the ordered `pr-check`,
`flake check --print-build-logs`, and browser-enabled `test-coverage` are required
before publishing the exact candidate. Gate results and the immutable source
identity are recorded in the PR; local tests do not claim live collector delivery.

## Measured overhead and choices

Pinned Go on Apple M2, three runs of the same small operation, ordinary build:

| Collection | Time/op | Allocated bytes/op |
| --- | ---: | ---: |
| Disabled context | 5.38–6.05 ns | 0 |
| Fixed metrics | 132.0–132.7 ns | 144 |
| Metrics plus 10% sampled traces | 659.9–668.8 ns | 766 |
| Concurrent metrics (eight Go workers) | 319.1–364.6 ns | 144 |
| Concurrent lifecycle collection | 309.8–318.3 ns | 149 |

Concurrent logging can drop samples when its queue fills; its throughput is not
proof of guaranteed log delivery. These are instrumentation microbenchmarks,
not production latency, memory sizing or profiling-overhead claims. Actual
deployment profile collection and representative high-volume uploads must be
measured before selecting application-wide overhead ratchets.

The fixed metric registry avoids unconstrained label maps and a new Prometheus
runtime dependency. A short mutex protects coherent histogram snapshots; it is
never held during export, logging or provider calls. The official OpenTelemetry
SDK/exporter is used because the standard library does not implement OTLP or its
interoperable trace model. The exporter is pinned to the existing v1.45.0 SDK;
its Apache-2.0 upstream license, module closure and retained vulnerability scan
are part of the normal dependency/release gates. Its required protobuf/gateway
and networking updates are pinned in go.mod/go.sum and Nix's vendor hash.

Rejected approaches: global automatic SDK/HTTP tracing (uncontrolled URL/key
attributes); synchronous export/logging (collector backpressure); unbounded
metric maps/queues (memory/cardinality growth); the default public pprof mux
(command line/debug surfaces); and a mandatory vendor agent or account.

## Remaining deployment evidence

Wire the private metrics/profile pulls and OTLP endpoint in xlab-deployments,
verify actual Mimir/Tempo/Loki/Pyroscope samples and correlation, and measure
profiling plus representative upload/preview concurrency under configured
resources. Preserve exact image/release qualification. The resource dashboard
preparation is in xlab-deployments PR #117. Container OOM root cause and the
original 14,169-file upload remain unqualified by this implementation alone.
