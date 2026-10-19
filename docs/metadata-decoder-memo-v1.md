# Immutable metadata decoder memo v1

This internal mechanism amends only the fresh-parsing requirement of v1 section
8's packed-page read protocol. It changes no durable schema, canonical bytes,
pack identity, writer protocol, release ledger or public API. The implementation
and retention policy are under qualification in this candidate; source review is
not runtime, memory or performance qualification.

A codec may retain the complete successful result of the pure function of exact
immutable wire and expected domain, kind and pack identity. Before lookup it must
enter the existing cancellable single process-wide decode admission, enforce the
unchanged wire bound, copy provider wire inside admission before hashing, and
bind the full SHA-256 and length plus every expected identity. Caller-owned
proven wire retains its existing immutability contract. Zero or mismatched opaque
proofs must fail before lookup, without fallback or a provider retry.

Unknown inputs use the unchanged strict decoder, including gzip integrity,
canonical spelling and validation of every member, including unused pages.
Existing proven wire may use its unchanged validating decoder on a memo miss.
Only fully successful validation may prepare a privately owned snapshot. A hit
returns independent pages, entry and child slices, and value bytes, preserving
nil versus nonnil empty values. All retained strings and backing arrays are
owned and charged. No snapshot handle or mutable alias may escape. Per-use page
digest and descriptor checks remain mandatory.

Hashing, preparation and cloning share the original admission slot and remain
cancellable. A returned failure exposes no result, owned wire or usable proof,
and cannot publish a new snapshot. Explicit charged-byte and entry limits cover
the fixed table and retained backing storage. Oversized snapshots may be declined
without failing valid reads. Eviction clears retained references. This accounting
does not bound active caller copies, compressed bodies, allocator/GC overhead or
total process memory; those require separate measurement.

This memo contains no heads, authorization or provider-presence decisions,
failures, leases, provider callbacks or publication state. The compressed-body
cache remains engine/backend scoped. Startup, migration, maintenance and
checkpoint verification still bypass idle bodies and ordinary in-flight reads.
Each existing fresh provider GET must succeed before pure result reuse. A later
independent reachability read must still GET and reject missing or corrupt
authority. Bypassed reads admit no idle compressed body or wire proof. Conditional
publication retains its independent provider-presence and freeze/CAS proofs.

| Guarantee | Required replacement evidence |
| --- | --- |
| Canonical integrity | Strict cold reference, unknown/duplicate fields, unused-member corruption, bounds and identical re-encoding |
| Complete binding | Warm zero/wrong proof, full expected tuple and changed-wire denials |
| Ownership | Recursive field inventory, provider/caller mutation, nil/empty, concurrent independent outputs and unsafe-clone negative control |
| Admission | Maximum-wire queued-allocation denial, queued and per-clone cancellation, late failed issuance |
| Bounded retention | Charged working set, decline, eviction, entry bound and no escaped snapshot handles |
| Fresh authority | Cross-backend missing/corrupt/unavailable denials, independent fresh GETs and real mid-operation checkpoint faults |
| Compatibility | Existing replay, checkpoint/raw-copy, migration and multi-replica gates; unchanged bytes and publication point |
| Diagnosis | Closed materialization operation/stage, redaction and cardinality; request IDs only in correlated logs |

`metadata.pack.materialize` encloses a strict decode on misses. Its input-byte
counter measures wire inspected, not additional provider traffic or expanded
allocation. Parent/child timers overlap and must not be summed. Compare disabled,
32 MiB and explicitly source-bound larger retention candidates against actual
working sets before selecting policy. Keep expansion limits, resources, tests,
assertions, deadlines and coverage floors unchanged. Record cold overhead,
allocation, GC, peak memory, contention, full provider vectors and complete-check
cold/warm wall time. No tenfold or memory-neutral claim follows from source review.
