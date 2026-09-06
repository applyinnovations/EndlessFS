# Storage schema 010 implementation record

**Status:** implemented and locally qualified when the complete repository gate recorded by the pull request is green. The first declared release boundary is v0.5.0.

## Incident and root cause

The released `007 -> 008` migration enumerated `endlessfs/v1/state/`, an obsolete mutable-record namespace. Real schema-004 through schema-007 application state was authoritative through `state-indexes/<namespace>/root.json`; each leaf referenced an immutable `state-versions` body. The migration could therefore publish schema 008 and later schema 009 while omitting profiles, accounts, passkey credential indexes and credentials, sessions, roles, invites, recoveries, shares, preferences, operations, and ceremonies from the new consistency domains. Authentication then returned 401 because the profile/credential lookup was absent, not because the passkey signature was invalid.

The old test matrix did not detect this. Fixtures named `application-disabled` and `application-gcs` represented application writer configurations but contained only filesystem samples and one preference. Tests proved that startup opened, file aggregates survived, and a later file mutation worked; they did not assert the complete application authority or perform authentication. The migration coverage matcher also counted only `migration.go`, `migration008.go`, and `migration_ledger.go`, excluding later migration implementations while still reporting a passing percentage.

## Recovery edge

Schema 010 appends one adjacent `009 -> 010` edge and the `state-conservation-v1` feature. It does not edit schema 008 or 009 history. Under the canonical closed write gate, every replica:

1. reads the actual legacy state-index roots and walks their authenticated immutable pages in bounded batches;
2. reads every referenced state-version body, validates the exact logical-key/version binding, and deterministically routes and encodes the corresponding typed current record;
3. writes a create-only conservation receipt binding source root key and digest, logical key and version, source-version key and digest, target domain/key/type/value digest, and recovery disposition;
4. merges only absent values into frozen current consistency domains; an unequal current value is a conflict and fails closed;
5. publishes any required domain-catalog additions and independently re-walks every source, receipt, catalog entry, frozen domain head, and target value;
6. crosses the shared writer-set activation barrier only after that durable proof succeeds, then advances the superblock/gate, checkpoints, reopens, and unfreezes.

The source indexes and version bodies remain unchanged during recovery. The migration reads state metadata bodies only; it never reads file-backend object bodies and performs no provider copy, rewrite, upload, or file delete. Missing or changed sources, malformed pages, duplicate keys, digest mismatch, target conflict, forged/missing/corrupt receipts, an unfrozen target, or a changed catalog prevents activation and leaves readiness false.

## Activation law

Schema 010 starts the conservation era. The central migration writer-set activation function rejects schema 010 and every later epoch unless that ledger edge registers a pre-activation authority verifier. Schema 010's verifier reloads the durable proof and independently checks the complete source-to-receipt-to-target relation. A future migration cannot become the current writer merely by updating feature markers or by returning success from its edge implementation; it must supply and pass its authority verifier while the gate and target domains are frozen.

This barrier complements, rather than replaces, immutable fixtures. A verifier proves the particular storage set being upgraded. Predecessor-produced complete fixtures and semantic oracles prove that the verifier and transformation enumerate the application authority the software actually wrote.

## Immutable evidence

The migration registry binds these additional raw fixtures to their producer commit and SHA-256 digest:

| Entry point | Producer | Purpose |
| --- | --- | --- |
| `schema-006-v0.3.2-application-complete.json` | exact v0.3.2 binary | complete indexed application authority before the defective edge |
| `schema-007-application-complete.json` | exact schema-007 binary | complete untagged predecessor corpus |
| `schema-008-application-complete-residue.json` | exact schema-008 binary | predecessor-produced interrupted history after authority omission |
| `schema-009-v0.4.0-application-complete-residue.json` | exact v0.4.0 binary | production-shaped recovery entry point |
| `schema-010-application-complete.json` | exact schema-010 writer | current complete corpus after recovery |

Each complete fixture carries the persisted virtual authenticator needed to produce a new valid assertion against the migrated passkey credential. The test first verifies all required logical keys exist in the predecessor's indexed authority, opens the full remaining migration suffix in both single- and split-backend layouts, asserts every typed namespace value, completes a real signed usernameless WebAuthn authentication and session lookup, and performs a new mutation.

Denial and recovery tests cover unequal current authority, missing state-version objects, corrupt receipts across restart, exact edge-boundary interruption, provider-operation interruption, and eight simultaneous migrators. The migration gate now runs the entire `internal/portable` and `cmd/endlessfs` owning packages rather than selecting tests through a permissive name regex. Its 98% denominator includes every numbered production migration implementation and the ledger.

## Rollout behavior

During an upgrade from v0.4.x, liveness remains available but readiness remains false while the canonical gate is closed and recovery is in progress. No operator should rewrite bucket objects or recreate passkeys. If the retained indexed source is complete and non-conflicting, one or more replicas converge on the same receipts and targets and normal startup resumes. If any source is missing, corrupt, or conflicts with current typed authority, startup fails closed with a migration error; preserve the bucket and investigate rather than deleting proof or source objects.

The schema 010 edge is a recovery and prevention change. Ordinary post-startup state and namespace mutation economics remain those of the schema-009 invariant-aligned domain engine; no extra provider request is added to normal authentication or file operations. The append-only GCS economics ledger records the measured `008 -> 010` minimal-fixture migration independently, so migration work cannot silently grow while unchanged runtime-operation ratchets remain exact.
