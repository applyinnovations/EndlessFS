# Application batch-read audit

Base: PR #63, `b46d526c39febf02fb6e1e0084ffad25b8d3913e`.
This optimization depends on that change and leaves its branch unchanged.
While #63 is unmerged, review this PR against `codex/durable-thumbnail-queue`.
After #63 merges, retarget to main and compare the actual merge tree; rebase
and rerun the complete gate if the source or merge context changes.

## Scope and selection

The audit searched the 212 production Go files under `internal`, `cmd` and
`tools` at the base revision, inventoried logical/physical Get calls and
repository wrappers, and traced every application `state.Store` consumer:
identity, drive, preview and theme, plus session authentication. It inspected
the portable GetMany implementation/domain router and read-session use in
namespace, upload, transaction, checkpoint and migration paths. Browser
resolve/upload batching and HTTP handlers were checked for a separate state
reader. This is a structural call-site audit, not a claim that every source
line was manually reviewed or a production workload profile.

Only known keys in one consistency domain use the existing GetMany capability.
Every read is fresh for that operation; no authority is cached across requests,
jobs, replicas or authentication commit. Mutations retain their original
expected versions, lease/epoch checks and atomic visibility points. No schema,
storage API, external service, frontend scheduling or idle-poll policy changes.

| Opportunity | Decision and constraint |
| --- | --- |
| Thumbnail completion job + lane | Batch two known administration-domain keys; retain both versions and lease fencing. A missing member uses the existing exact-read path so late completion stays harmless; other batch failures fail closed. |
| Indexed passkey records | Read the index first, then batch its known owner keys in index order. Bound each batch to the existing 1000-key GetMany limit (at most 64 MiB of raw state values); never return a partial list after an error/cancellation. Existing total result size is unchanged. |
| Discoverable authentication | Read the selected credential, account, profile and index together in the parsed owner's identity domain. Decode/bind every record to that owner, check enabled status, then load the indexed credentials. |
| Authentication commit inputs | After WebAuthn validation, freshly batch account + credential and retain both versions in the existing atomic commit. Discovery is not commit-time authority. |
| Session + account; thumbnail admission | Already batched; retained. Admission's cold/mixed missing-member fallback stays intact because existing GetMany is all-or-nothing. |
| Credential removal; bootstrap probes | Keep index/final-passkey checks and expected-absence probes before dependent work. Speculative batching would complicate early-denial/partial-missing semantics for little common-path benefit. |
| Admin users, roles, shares and themes | Owner identities, administration and namespace records occupy different domains. GetMany rejects cross-domain keys. Repeated admin-role reuse is a separate request-snapshot change, not a reason to broaden this API. Theme/share lookups are single records. |
| Preview operation replay/retention | IDs and bindings must be decoded before dependent keys are known. A later operation/index pair could be combined, but its retention-only path and missing-binding error ordering do not justify additional orchestration here. |
| Queue selection and maintenance | Lane counters select recent/oldest indexes; index contents select job keys. Lists cannot share an existing GetMany snapshot. Do not add a read-view framework, claim jobs ahead, cache old heads or alter cleanup/idle timing. |
| Namespace/upload trees and transactions | Existing bounded read sessions share authenticated heads/pages; preserve specialized snapshot and publication protocols rather than replacing them with application-state reads. |
| Raw providers, preview artifacts, migrations, checkpoints and GC | These read physical objects, separate backends or predecessor formats. Application GetMany does not apply; preserve integrity/presence checks and ordered recovery boundaries. |

## Measurements and proof

Identical deterministic fixtures use the portable engine with instrumented
in-memory object backends. Counts are backend operations; modeled GCS pricing
and latency are not production HTTP, billing or wall-time observations. Source
and preview artifact traffic is excluded from the thumbnail state fixture.
The eight-passkey scalar control hides only the optional batch capability.

| Workload | Operations before → after | Response bytes before → after | Modeled serial p50, microseconds |
| --- | --- | --- | --- |
| Drain 64 thumbnails | 656 → 592 (520 → 456 GET; 136 PUT unchanged) | 50,330,821 → 44,283,479 | 15,437,498 → 13,971,795 |
| Existing authentication verification | 20 → 16 | 78,546 → 45,970 | 459,041 → 370,729 |
| List eight passkeys, warm | 9 → 2 GET | 66,438 → 14,764 | 198,639 → 44,142 |

The 64-job drain's modeled request cost falls from $0.000888 to $0.0008624;
removing GETs saves less money than request count because PUTs cost more in the
retained model. Writes and request bytes are unchanged in all three vectors.
Cold admission (71), priority refresh (3), ready resolution (0) and empty worker
polling (three GETs per single-lane step, backing off to 30 seconds) are unchanged.
The new economics epoch tightens existing paths and adds the scalar/batch
passkey controls without editing historical fixtures.

Regression-first tests expose the original scalar reads and ignored batch
errors before implementation. Added cases cover bounded 1001-key fan-out,
ordering, cancellation at batch return and between chunks, missing/malformed/foreign records,
disabled accounts and no partial authority fallback. Interleaved account and
credential changes after the fresh commit read must reject authentication
without consuming its ceremony. Queue fault adapters exercise batch reads while
retaining existing stale-lane, priority-refresh, restart, overlapping-request,
retry, corruption and two-worker assertions. Existing portable GetMany tests
qualify one authenticated snapshot and deny cross-domain reads.

The tighter candidate budgets, complete affected packages, race checks and full
publication gates must pass before publication. CI and fresh full-gate results
belong in the PR evidence; focused measurements alone do not establish readiness.
