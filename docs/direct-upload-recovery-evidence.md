# Direct upload recovery repair

Production v0.7.3 smoke testing accepted three small synthetic files, but an
empty file and a 16 MiB file retried. Chrome sent the exact application Origin;
preflights succeeded, while final GCS HTTP 200 responses omitted the CORS header.
The multi-chunk transfer reached 8 MiB. Retries caused an admission 429. Both
application replicas remained healthy with no restarts; this is not evidence of
the earlier high-volume crash's cause or a successful high-volume qualification.

Read-only XML endpoint preflights on 10 October 2026 returned an allowed-method
set of GET, HEAD, PUT for the file bucket. POST returned no CORS allow header.
The infrastructure Terraform file declares the same method set and omits
X-Goog-Resumable from permitted headers. Full authenticated bucket metadata
inspection was denied by storage.buckets.get; no live configuration was changed.
The reviewed correction must include POST and X-Goog-Resumable while retaining
the exact drive.endlessfs.com origin. A post-apply live browser test is required
to establish that the missing final headers have been resolved.

Independent application defects amplified lost responses: batched initializing
records reported zero progress without inspecting their durable provider lease;
empty transfers had no explicit transferred-data signal; automatic recovery
could initialize another session after exhausting its inner retries.

## Guarantees and replacement evidence

| Guarantee | Repair evidence |
|---|---|
| Empty and nonempty transferred data remain unpublished until verified completion | Shared provider contracts check dataComplete before/after transfer and Stat denial before publication, for individual and batched admissions |
| Lost responses do not resend accepted bytes or allocate another session | Chromium test uploads zero, small, and two-chunk files with unreadable data responses and requires exactly four data requests |
| Automatic retry retains the original batch and offset | Chromium test accepts one 8 MiB chunk, rejects four attempts, resumes the same session, then sends only the final byte |
| Session recovery preserves owner, expiry, and terminal denial | Shared provider contracts and authenticated HTTP tests check exact session identity, cross-owner denial, aborted denial, CSRF/origin/body constraints and no-store |
| Bad CORS cannot expose an unreadable fresh session | GCS contract removes initiation CORS, requires rejection and revocation; the valid-origin contract covers readable intermediate and final responses |
| Status never finalizes an untransferred zero-byte object | GCS contract proves metadata-only status before explicit bytes */0 finalization, matching the Google upload implementation |
| Provider economics and authority remain intact | Resume instrumentation permits only state reads and the existing provider resume primitive; no new session, file-body read, state publication, or normal-path provider call is added |

No authoritative bytes, feature set, writer identity, or schema ledger are
changed. dataComplete is a transient status projection. Resume resolves the
existing canonical upload and encrypted transient provider lease, including
batched initializing records; it does not rewrite batched admission authority.
An expired or aborted session requires an explicit new upload attempt, not an
implicit expiry extension. All existing provider ratchets remain mandatory.

The new resume workload has its own append-only measured ratchet: two state
reads and one local file-backend resume primitive, modeled at 800,000 picoUSD
and 44,028/130,028/320,028 microseconds p50/p95/p99. The GCS resume primitive
performs no network request. Zero state mutations and zero file-body transfers
are separately asserted; no historical budget is renamed, aliased, or relaxed.

Local protocol and Chromium tests are deterministic qualification, not proof
that Google configuration has been applied. Release and deployment must retain
the exact-tree gates and immutable digest verification, followed by live empty,
multi-chunk, Smart merge, download integrity, and resource smoke checks.
