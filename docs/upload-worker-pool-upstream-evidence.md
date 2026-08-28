# Upload worker-pool upstream evidence

This record fixes the upstream source and design evidence used by the browser
upload worker pool. It exists to prevent an unexplained concurrency constant
from becoming an application contract again.

## Sources inspected

| Project | Pinned source | License | Relevant behavior |
|---|---|---|---|
| Google `upload-cloud-storage` action | [`d1308790579b5dcf26c7354c32ffbd2b9aa89377`](https://github.com/google-github-actions/upload-cloud-storage/tree/d1308790579b5dcf26c7354c32ffbd2b9aa89377) | Apache-2.0 | The public action defaults `concurrency` to 100. Its upload client creates one task per file and drains their shared iterator with the configured number of asynchronous workers. Every worker takes the next file only after its current upload settles, so the queue is bounded without serializing the workload. |
| Google `gsutil` | [`5b0b4a018ecdf4167a93a606b97796ad1e91d364`](https://github.com/GoogleCloudPlatform/gsutil/tree/5b0b4a018ecdf4167a93a606b97796ad1e91d364) | Apache-2.0 | Parallel mode uses a configurable process/thread worker pool. Its thread-only default is 24; Linux can use up to 32 processes with five threads each. The documentation explicitly recommends workload-specific tuning for large file sets instead of treating a small constant as a provider limit. |
| Firebase JavaScript SDK storage client | [`50213a1cdba8e3b0f029197ec20f47c1110060a6`](https://github.com/firebase/firebase-js-sdk/tree/50213a1cdba8e3b0f029197ec20f47c1110060a6/packages/storage) | Apache-2.0 | Browser GCS uploads are resumable. An uncertain upload request is not blindly replayed: the client queries the committed resumable offset before continuing and applies bounded retry/backoff. |
| Uppy | [`93689cd27d1b379b338946f57c97a825353ed379`](https://github.com/transloadit/uppy/tree/93689cd27d1b379b338946f57c97a825353ed379) | MIT | Upload plugins use a cancellable bounded task queue. The rate-limited queue pauses on a remote rate-limit response, drops concurrency, and progressively restores it. Its implementations do not infer upload capacity from browser CPU count or estimated download bandwidth. |
| rclone | [`1583cce1e28340e5d064ed955179f5f2b31e7757`](https://github.com/rclone/rclone/tree/1583cce1e28340e5d064ed955179f5f2b31e7757) | MIT | File-transfer workers are explicitly configurable and GCS requests run through a provider pacer with retry classification. The default of four is an operator default, not a GCS maximum. |

Google Cloud Storage documents initial bucket capacity of approximately 1,000
object writes per second and requires gradual ramp-up only when workloads grow
beyond that rate. The provider guidance therefore does not support the old
eight-upload maximum. See [Cloud Storage request-rate and access-distribution
guidelines](https://cloud.google.com/storage/docs/request-rate).

## EndlessFS selection

EndlessFS ports the Google GCS action's 100-worker shared-queue model into the
existing dependency-free browser scheduler:

- a selected set of up to 100 discovered files is admitted in one existing
  100-item batch;
- the scheduler starts up to 100 file tasks, bounded by queue depth;
- every task sends bytes directly from `File.slice(...)` to its provider
  capability; object bytes never enter the Go service;
- a settling task releases exactly one worker slot and the next queued task
  takes it;
- explicit browser Data Saver intent reduces the active worker count to one;
- cancellation, confirmed-offset recovery, retry classification, exponential
  backoff, jitter, and the retry circuit remain the pre-existing EndlessFS
  equivalents of the Firebase/Uppy safety behavior; and
- successful completions accumulate their affected directory and issue one
  listing refresh when the active burst drains, rather than one refresh per
  file or a second competing group refresh.

No source file from an upstream project is vendored and no runtime dependency
is introduced. The shared-worker algorithm is reimplemented in the existing
EndlessFS queue to preserve its transfer ledger, capability boundary, and
provider-portable protocol.

Parallel composite uploads are deliberately not copied from `gsutil` or GCS
server libraries. They create temporary component objects and a composite
result, add provider operations and cleanup obligations, and change available
checksum metadata. EndlessFS continues to use one resumable destination object
per file.

## Deterministic evidence

`TestE2EUpstreamGCSWorkerPoolUploadsOneHundredFilesAndCoalescesRefresh` selects
100 real browser `File` objects and proves:

- the scheduler has 100 live upload tasks simultaneously;
- the HTTP/1.1 fixture data plane has all six available wire connections busy;
- one 100-item batch admission and no single-item admission are issued;
- exactly 100 direct data-plane body requests and 100 completion requests are
  issued;
- all 100 transfers reach the complete UI state; and
- one coalesced directory listing makes the uploaded files visible.

The deterministic fixture proves orchestration, bounds, request counts, and UI
publication. It is not presented as a live-GCS throughput benchmark.
