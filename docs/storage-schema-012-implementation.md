# Schema 012: independent upload admission identities

Release scope: v0.8.0. This specification supersedes schema 011 only for new
upload admission identity and its writer-compatibility boundary. All other
schema-011 storage, publication, recovery, conservation and economics
guarantees remain applicable.

## Problem and selected behavior

A cancelled upload batch followed by a new attempt at the same destinations
can exhaust every admission retry. Schema 011 derived batch and mutation IDs
from normalized file properties without the item idempotency keys. Independent
attempts therefore requested the same immutable upload-record keys and retained
mutation identity. HTTP 201 could carry `unavailable` for every item, while
the browser eventually displayed a generic retry-exhausted error.

Schema 012 binds a new batch ID to the authenticated owner and ordered digests
of the item idempotency keys. Its admission mutation ID binds that batch ID;
the retained intent fingerprint still binds the complete normalized content.
Different keys identify independent attempts even when content is identical.
The same keys with changed content continue to conflict.

On replay, the complete canonical membership supplies the batch and upload
IDs. All members must agree on batch ID, ordinal and count. This preserves
predecessor-created IDs without keeping an alternative creation algorithm or
rewriting historical records. A subset, reorder or mixed membership cannot
become another admission. Expired initializing/active intents fail with a
conflict before any provider capability is created.

## Adjacent migration and compatibility

The schema and release ledgers append schema 012, edge 011-to-012, and the
v0.8.0 release boundary. The incoming transformation uses the shared feature
migration protocol and adds `upload-intent-identities-v1`. It never rewrites
existing upload membership, idempotency records, outcomes or blobs. The new
required feature prevents predecessor binaries from writing with the old
identity derivation after cutover.

The pre-activation verifier requires the exact closed migration gate and its
checkpoint ID, then authenticates the complete frozen consistency-domain
catalog and typed authority. An active predecessor capability blocks closure
until the existing capability-drain protocol permits quiescence. The verifier
cannot turn a successful runner or a feature marker into proof of conservation.
Expired predecessor intents retain their original IDs and cannot be revived;
a new explicitly keyed attempt obtains new authority.

## Guarantee matrix and evidence

| Guarantee | Replacement proof |
| --- | --- |
| Independent attempts do not share upload/blob authority | Cancel and re-admit identical destinations with new keys; IDs differ |
| Replay preserves exact committed intent | Same keys return the same IDs; changed content, order and cardinality are denied |
| Cancellation affects only its admitted members | Deterministically interleave two, four and eight engines before a head CAS, then cancel one batch |
| Historical authority remains intact | Import immutable v0.7.5 after-intents raw bytes, wait for capability drain with an injected clock, migrate, verify original IDs and expired status, then admit fresh keys |
| No incompatible writer observes partial activation | Ordered adjacent migration, closed-gate authority verification, required-feature fencing |
| Provider portability and economics remain required | Complete ordinary migration/profile, provider-budget, raw-copy and replica gates; no file-body copy or new provider operation is introduced |

The initial cancel/re-admit regression failed with `upload batch intent
remained contended` before the identity repair. The required-feature regression
failed before the adjacent migration was appended. The supplemental predecessor
fixture and producer provenance are recorded in
`internal/portable/testdata/upload-identity/README.md`.

Full local gates, schema-012 epoch/profile fixtures, aggregate migration
coverage and release qualification are required before this change is ready
for review or publication. Focused regressions alone are not release proof.

The immutable schema-012 application fixtures were written by commit
`2397a5c21f967cc62db9f4c9d9f34687884ca784`. Their configuration digest and
records remain intact. Admission lifetime defaults do not change writer
identity: every replica resolves an existing transfer's absolute deadline
from canonical authority, not from its own defaults. A different local
default cannot renew a persisted session or change capability drain. The
application startup matrix uses the real configured lifetimes and preserves
the producer's records.

## Rejected alternatives and remaining scope

Increasing retry limits cannot repair a deterministic identity collision.
Deleting or rebinding old upload records would weaken idempotency, cancellation
isolation and immutable-blob guarantees. Changing ID derivation without a
schema boundary would allow incompatible writer rules in one storage set.

The external-drive stall is a separate client source-read problem. Controlled
tests completed with 64 external photos and 100 SSD copies, while 100 external
photos in a fresh destination stopped transmitting with the drive still busy.
The accompanying source scheduler retains the 100 logical tasks but separates
them from a cancellable, serial disk reader and a bounded chunk window. The
client qualification and hardware comparison are recorded separately; they do
not substitute for migration or provider-economics proof.

New resumable intent deadlines use the configured session lifetime, default
24 hours and capped at the provider-supported seven days. Signed initiation
remains short lived. Existing canonical record bytes, semantics and expiries
remain unchanged; this is configuration of the existing absolute deadline,
not a renewal protocol or migration rewrite. Operators must cancel unfinished sessions before
an urgent checkpoint/upgrade rather than expecting a five-minute drain of a
newly admitted day-long session.

## Current provider vectors

### Oversized admission metadata fallback

`TestUploadBatchLargeMetadataAdmission` reproduces a valid compact request
that failed before provider initialization on `e856157`: 4,001 synthetic
zero-byte uploads share a 3,012-byte destination directory. The compact
control request is 183,169 bytes, but the canonical upload/idempotency records
expand past the 32 MiB pack envelope. The red result was
`invalid: expanded consistency-domain page pack exceeds size limit`.
An 8,377-item short-path synthetic control passed. File count and total file
bytes alone therefore do not identify this metadata bound.

Materialized mutation publication now distinguishes a locally encoded pack
overflow from malformed authority. For admission, which has no pre-staged
child-page references, only overflow retries preparation with
ordinary immutable pages from the same authenticated snapshot. It retains
the exact intent fingerprint, mutation ID, expected revision, retained
outcome, and conditional head version. The regression observes 53 page
writes, 61 state requests, 4,001 independent provider starts and exactly one
head publication. It records the state/file economic vectors using
the existing offline model. Small packed workloads keep their existing
provider budgets. This fallback pays an additional metadata preparation
pass and page requests for a workload that previously failed; it does not
raise either pack bound or alter provider limits.

The boundary suite covers compressed overflow, a lost successful head
response, exact replay, concurrent-writer and freeze denial, cancellation,
and malformed-pack denial. Sessions with pre-staged child pages still fail
closed on overflow: retrying their opaque value references in a new session
would lose the staged pages. Extending fallback to those other namespace
workflows is outside this admission fix. The upload regression verifies stable upload and
batch IDs without new provider starts on replay, and rejects changed content
under existing item keys. Preparation remains invisible until the one head
CAS; unsuccessful fallback leaves only unreachable immutable metadata.

This implements the existing section 5 pack-overflow rule. Authoritative
record shapes, value semantics, key construction, IDs, fingerprints, format
limits and required features are unchanged. Existing readers already accept
unpacked pages; no storage epoch, migration, fixture rewrite or file-body
operation is needed. Raising format limits would weaken the existing bounds;
splitting logical admission would change batch membership and recovery
semantics. Both are unnecessary for the required fallback.

The 15 October incident's trace reported `domain.commit=invalid` and the HTTP
batch handler copied that error to every valid item under HTTP 201. It did not
record the validation reason. This reproduction establishes a compatible
failure mechanism, not proof that the incident hit this exact bound. The
observed directory 429 and long provider starts remain separate limitations.
GCS initiation uses the incoming context for its HTTP POST, but its default
transfer client has no explicit timeout; capability signing calls the pinned
storage SDK's `BucketHandle.SignedURL`, whose default IAM signer uses a
background context and transient-error retries. A server write timeout does
not establish a handler/signing deadline. The observed approximately
360-second failures cannot be attributed to GCS quota exhaustion from the
collapsed telemetry alone. This patch does not change those timeouts,
provider concurrency, retry policy, HTTP per-item response behavior, or
directory admission. It performs no production retry or repair.

### Existing qualified workflows

The current executable catalog references explicit schema-012 workloads.
Prior schema-011 measurements remain immutable in historical epochs; their
replaced names are retired with this reviewed qualification record. There is
no runtime budget-name translation or exemption for unreferenced workloads.
The append-only delta is
`budgets-schema-012-regional-standard-flat-2026-10.json`.

100-file admission remains 105 provider calls (100 file, five state).
2,001-file admission, completion and cancellation each remain 2,007 calls
(2,001 file, six state), with unchanged modeled request cost. The changed ID
and required-feature metadata alter modeled byte-transfer latency by single
microseconds; completion improves by 11 microseconds. The full composed
migration from schema 008 adds the separately qualified 011-to-012 freeze,
checkpoint and conservation edge: 639 calls versus the predecessor suffix's
513. Pending/interrupted cleanup predecessor fixtures add the same 126 calls
(271/270 versus 145/144). These are one-time migration vectors, never charged
to ordinary uploads. The complete role-separated costs and critical paths are
recorded in the exact delta. All new names still fail closed until their
measured vectors exist and match; ceilings have not been loosened in place.
