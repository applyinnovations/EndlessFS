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

## Rejected alternatives and remaining scope

Increasing retry limits cannot repair a deterministic identity collision.
Deleting or rebinding old upload records would weaken idempotency, cancellation
isolation and immutable-blob guarantees. Changing ID derivation without a
schema boundary would allow incompatible writer rules in one storage set.

The external-drive stall is a separate client source-read problem. Controlled
tests completed with 64 external photos and 100 SSD copies, while 100 external
photos in a fresh destination stopped transmitting with the drive still busy.
This identity change does not claim to repair that stall or qualify a new
source-read scheduler.
