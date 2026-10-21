# Upload recovery acceptance

This record defines the corrective slice's acceptance conditions and the scope
of its evidence. The [v1 specification](v1-specification.md),
[HTTP contract](http-api.md), [storage implementation](storage-schema-011-implementation.md)
and [decoder memo contract](metadata-decoder-memo-v1.md) remain authoritative.
Healthy replicas or passing individual endpoints do not prove upload recovery.

## Incident boundary

On October 17, 2026, `POST /api/v1/uploads/plan/sizes` returned 400 after
43.309 and 43.426 seconds for request references `evkrwXFtnjgltCIY` and
`mCiGkyMaenSBt5QY`. The reported 329,109-byte JSON request was below the
unchanged 1 MiB limit. Historical logs discarded the underlying stage/reason;
the available trace lookup was empty. The exact production cause remains
unknown. No customer body or upload queue was opened or mutated.

Synthetic regressions establish expanded-pack overflow with 7,966 valid
long-name files, projection-rebuild identity collisions, authoritative
completion capacity exhaustion and lost-success retry collisions. Fixing
these defects does not establish which affected the customer.

## Required acceptance

### R1: valid smart planning

Real size and fingerprint planning must handle the 10,604-file authority,
7,966 completed / 2,638 remaining split, long names, deep existing folders and
cold/rebuilt/warm projections. Malformed or corrupt authority still fails
closed. Encoded and expanded packs retain their 4 MiB / 32 MiB limits.
See [planning regressions](../internal/portable/upload_planning_review_internal_test.go)
and [10,000-file provider coverage](../internal/portable/upload_corrective_provider_budget_test.go).

### R2: the complete user journey

Initial upload and persisted recovery must traverse smart planning, directory
preparation, admission, actual native bytes and completion. Include reconnect,
retry, refresh, logout/passkey login, source reselection and byte-offset resume.
Independent authority, membership, version, checksum and byte oracles must
prove the 2,638 remaining four-byte files transfer once, with zero completed-file
readmission or retransfer. Smaller complete journeys cover cold/rebuilt/warm
projections; the full 10,604-file transfer journey is warm, with separate scale
planning across projection states. Classification-only expired uploads and
keep-both fixtures cannot satisfy this condition.
See [smart recovery](../internal/e2e/upload_smart_recovery_acceptance_test.go)
and [stored-source restoration](../internal/e2e/upload_stored_source_restore_test.go).

### R3: concurrent owners and the selected resource profile

Two independent owners must complete R2 through two physical application
processes/engines, each at 1 CPU / 2 GiB hard memory / no swap / 450 MiB Go soft
target / unset GOMAXPROCS / 50-second control deadline. Retain deterministic
routing, the 12-minute recovery wait, owner denial, overlapping work and
opposite-process lost-success replay. Require zero OOM, duplicate transfer,
authority loss or abandoned active work. Single-owner and in-process tests
cannot substitute for this qualification.

### R4: bounded lifecycle work and independent partial success

Directory preparation uses at most 100 items per page and four independent
pages concurrently, with parent dependencies, per-item outcomes, repeatable
publication and cancellation. Planning, admission, resume, status, completion
and cancellation remain batched. Fresh independent lifecycle pages contain at
most 100 items, with no scalar per-file fallback. Capacity subdivision must
terminate, retain every correlated ID and complete capability, and preserve
successful siblings.
The 1 MiB reply guarantee covers independent admit/status/resume/actions;
frozen legacy cohorts retain their original ordered membership/key and full
response protocol, including its memory/deadline exposure.
See [directory acceptance](../internal/e2e/upload_directory_preparation_test.go)
and [response-capacity acceptance](../internal/e2e/upload_response_capacity_test.go).

The scale journey requires exactly 2,638 PATCH and 2,638 OPTIONS requests, at
most 152 whole control / 5,428 total HTTP requests. Foreground state ceilings
remain 5,962 calls / 376 MiB written / 5,748 MiB read; restore/auth ceilings
remain 53 calls / 64 KiB / 8 MiB. Report restore, foreground and whole cuts
separately, including failed/replayed provider work. Body bytes exclude HTTP,
TCP and TLS overhead; modeled cost/latency is not observed live GCS behavior.

### R5: preparation, errors and rendering

Completed members retain their preparation share; remaining work advances the
remainder to 100% on initial upload and reconnect/retry/resume. Representative
recovery must show intermediate progress without falling below 75%, with at
most 72 rendered rows.
Clear satisfied previous-attempt errors while retaining useful history bounded
to three entries, 240-character messages and 80-character codes. Preserve
completed/cancelled members and independent siblings. Test explicit obsolete
source-required messages/codes, rather than treating every informational
preparation message as an error.

### R6: useful, safe diagnosis

Closed stage/reason codes must survive HTTP mapping and nested telemetry with
operation, result, duration, build and trace/request correlation. Request
references belong in correlated logs, never metric labels. Test redaction,
cardinality, concurrent isolation, decoder-admission cancellation, panic and
real response socket/flush failures without duplicate terminal observations.
Exclude raw private errors, paths, names, identities, keys, credentials,
capabilities and bodies. Final flush observes the server buffered-body boundary,
not client receipt or later framing.
See [diagnostic tests](../internal/telemetry/diagnostic_test.go)
and [HTTP correlation tests](../internal/httpapi/problem_diagnostic_test.go).

### R7: integrity, replay and portability

Repacking validates every original digest/descriptor, owns mutable pages and
preserves opaque values and prior references. Complete immutable intent,
preparation/expiry inputs, layout policy and stable ranges determine physical
identities; only typed actual capacity failure may refine groups. Validate all
new groups before PUT, then retain the original head CAS/freeze visibility
point, strict collision denial, cancellation and stale-writer/replay rules.
Schema epoch and canonical representations are unchanged.
See [closure preservation](../internal/portable/domain_repack_closure_preservation_review_internal_test.go)
and [lost-response retries](../internal/portable/upload_planning_repack_review_internal_test.go).

Immutable wire reuse remains engine/backend scoped. The separate 64 MiB charged
decoder-result memo retains only owned, fully validated pure results; it cannot
prove provider presence, authorization or publication. Required independent
checkpoint/reachability GETs remain fresh. Its bound excludes active caller
copies, allocator overhead and total process memory; the normative
[memo contract](metadata-decoder-memo-v1.md) defines the complete conditions.

### R8: validation and delivery

Use the ordered [local commit/push gate](../AGENTS.md#local-commit-and-push-gate)
and [iteration workflow](iteration-workflow.md), preserving workloads,
`-count=1`, 30-minute Go timeout and 85% repository / 95% sensitive / 98%
migration floors. Reproduction entry points include:

```text
nix run .#test-contract
nix run .#test-provider-budget
nix run .#test-coverage
nix run .#test-migration
```

Commit the verified source and inspect actual CI. Carry evidence forward only
with explicit relevant-input equivalence; functional reuse is not a new
browser, physical-resource or production qualification. No merge or deployment
follows from this record.

## Qualified evidence and remaining limits

The application/test inputs at commit
`8acafd720738f0d81ed97394e9c8f3d27f22b04b` passed the complete local gates and two
fresh default-CI runs. The independently audited functional artifact accounts
for all 55 E2E roots: 54 passed and one optional hardware test skipped. Its
request/rendering numbers are passed assertions; normalized events did not
retain actual provider/control/row measurements. Do not substitute historical
150-request / 71-row observations for those missing measurements.

The physical two-owner result remains bound to tree
`50f9650b619cbef875cca47905be5243f969b61f` and binary SHA-256
`30e1101eab1eb30e76e0b84a4ee2858061d88ecc9f456f321af05a9772b458d8`.
All 5,276 new bodies transferred once; all 21,208 authority/blob/checksum/content
bindings and 15,932 prior portable entry versions were checked. Six signed
downloads were samples, not exhaustive HTTP downloads. Measured totals were
277 control requests, 25,462 provider calls, 703,749,039 request body bytes and
7,479,612,237 response body bytes, with zero OOM. Later tooling/prose changes
carry this only through runtime-input equivalence, not as a new physical run.

Closure repacking reduced repeated encoding/allocation in the matched fixture
but increased sampled heap and some metadata bytes. It makes no universal
complexity or optimal-efficiency claim. The earlier unprofiled 943.41-second
concurrent failure remains unexplained; later successful runs do not erase it.
The 4,001-intent checkpoint proof uses materialized legacy cancellation: the
compact-overlay/raw-initializing-record interaction with checkpoint capability
draining remains separately unqualified.

Ordinary 30-second operation, live paid GCS, full 308.7 GiB throughput and native
HTTP/2 socket-error behavior remain unqualified. Native connectivity fixtures
qualify offline/online signaling, not a transport outage. Exact October 17
causality and actual customer recovery remain unknown. Local identical-source
cache timing includes ordinary Nix reuse; it proves no general cold,
edited-source, CI, resource or release speedup.

## Historical evidence

Detailed attempts, failed results, comparisons and source/resource bindings are
preserved in the immutable [original acceptance record](https://github.com/applyinnovations/EndlessFS/blob/8acafd720738f0d81ed97394e9c8f3d27f22b04b/docs/upload-recovery-corrective-acceptance.md)
and [original checklist](https://github.com/applyinnovations/EndlessFS/blob/8acafd720738f0d81ed97394e9c8f3d27f22b04b/docs/upload-recovery-release-checklist.md),
with SHA-256 hashes `0495ebd54078e34410cbb29d445292a1ea9a90012747b9652025e554c54bee14`
and `88ecd015a7df1fb4f09449d90110638c00cc63b0c4e808ae9059ad079cbdc679`.
The hash-verified external archive and final gate/artifact manifests are indexed
in [PR62](https://github.com/applyinnovations/EndlessFS/pull/62). Historical
session status and authorization prose are not current acceptance results.
