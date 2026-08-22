# Part 1: scalable state and duplicate-reconciliation foundation

**Status:** implementation in progress for the first independently releasable pull request
**Input:** production migration review dated 2026-08-22  
**Primary objective:** make authoritative state management and maintenance materially faster, cheaper, and bounded before adding the duplicate-management UI.

Part 1 contains no duplicate-management browser workflow. It establishes the provider-neutral records, indexes, use cases, safety rules, fixtures, and HTTP contracts that Part 2 can exercise with local fixtures.

## Non-negotiable constraints

- The Go service never reads stored file bodies for hashing, inventory, duplicate detection, migration, or reconciliation.
- Stored-file identity is the normalized provider-attested tuple `(size, MD5, CRC32C)`. Provider generations, ETags, multipart IDs, encodings, and timestamps are not durable values.
- A file without both checksums is explicitly `unfingerprinted`; it is never silently grouped as a confirmed duplicate. Upload publication requires a complete tuple. Historical externally-created objects may remain visible but ineligible until the provider can attest the tuple.
- Image preview generation is the only initial optional feature allowed to read a source body. Any further exception requires a named, reviewed source exemption and tests.
- All indexes are derived, authenticated canonical state. Roots are constant-size, pages are independently bounded, writes use copy-on-write plus root CAS, and interrupted publication is recoverable.
- A single file addition, removal, move, or rename updates only the affected bounded pages and ancestor summaries. It must not scan the bucket, rebuild a complete directory, or rewrite a complete duplicate catalog.
- Ignore decisions are durable, owner-scoped policy records keyed by stable duplicate identities, not paths alone. Moving a file does not accidentally unignore or broaden a decision.
- Reconciliation is previewed first and then executed against pinned logical versions. It moves selected occurrences to trash; it does not directly destroy blobs.

## Delivery sequence

### 1. Provider metadata boundary and enforcement

- Extend `Head`, `List`, direct-upload completion, server-side copy, and local contract backends with normalized MD5 and CRC32C metadata.
- Remove the client SHA-256 completion contract and all stored-file SHA-256 generation from the service.
- Add the source lint with an actionable explanation and an exact optional-feature exemption registry.
- Add runtime interface separation so ordinary control-plane components cannot obtain file-body readers by accident.
- Make concurrent server-side copy recovery compare the complete tuple, not size alone.

Acceptance budget: upload completion uses one provider metadata result and zero service body bytes; a missing or partial tuple fails closed.

### 2. Metadata-only checkpoints and bounded migration

- Append schema epoch 004 and its one adjacent, resumable transformation.
- Replace body hashing, per-object work records, work-record rereads, and repeated totals scans with one ordered metadata traversal per backend role that emits bounded inventory pages directly.
- Verify checkpoint v3 with one ordered metadata traversal per role and bounded page state.
- Retire obsolete v1/v2 checkpoint work safely while the canonical gate remains closed; never reopen from an unverifiable legacy SHA-only checkpoint.
- Emit listing, role, object, byte, page, retry, and completion progress without keys or paths.

Acceptance budget: zero file-body reads; zero per-authoritative-object journal objects; at most one source listing pass and one verification listing pass per role; O(page-size) process memory.

### 3. Incremental content and directory indexes

- Store file occurrences in a persistent copy-on-write index keyed by `(size, MD5, CRC32C, owner, area, path digest)`.
- Store an occurrence-by-path reverse index so deletes, moves, restores, and replacements mutate a bounded number of pages.
- Store directory content summaries as order-independent multiset accumulators with byte/file counts. Updating one file changes its leaf and ancestor summaries only.
- Store exact directory-tree identities separately from similarity statistics. Exact equality requires equal recursive counts, bytes, and strong structural digest derived from complete file tuples and relative paths.
- Derive overlap candidates from content postings. Compute exact intersection/difference lazily for the selected pair; do not persist an O(number-of-directory-pairs) matrix.

Acceptance budget: mutation cost is O(log N + path depth); adding one object does not rescan unrelated files or folders; no pairwise directory explosion.

### 4. Bounded runtime state structures

- Replace full-directory materialization, complete-page rewrites, and growing manifest page-ID arrays with a bounded persistent search tree. Name lookup and name-sorted pagination read O(log D + page size); one entry mutation rewrites O(log D) nodes.
- Replace StateStore's whole-namespace snapshot cursor with a constant-size authenticated continuation over a persistent logical-key index.
- Replace embedded prerequisite bodies and whole-subtree operation arrays with bounded operation-step pages and resumable cursors.
- Process subtree copy, gate drains, state-version pruning, and migration graph walks in bounded batches.

Acceptance budgets are enforced with deterministic provider-call counts and maximum encoded-record sizes, not only functional tests.

### 5. Retention and garbage collection

- Give admissions, terminal operations, idempotency bindings, superseded immutable tree nodes, leases, and checkpoint artifacts explicit retention classes.
- Add a fenced, resumable mark-and-sweep maintenance operation whose marks are bound to a closed gate or an immutable root set.
- Never collect a prerequisite, blob, tree node, version snapshot, or reconciliation target reachable from an authoritative or recoverable operation root.

Acceptance budget: physical state approaches live state plus a documented recovery/retention window; repeated checkpoints do not accumulate object-per-file journals.

### 6. Backend reconciliation use cases for Part 2

- Paginate duplicate content groups, occurrences, exact duplicate directory groups, overlap candidates, and ignored groups.
- Return reclaimable bytes without double-counting shared occurrences.
- Preview `A minus B`, `B minus A`, and selected-intersection-to-trash plans with unique-file safeguards.
- Apply a plan only when every pinned logical version, directory root, ignore revision, and gate epoch still matches.
- Record ignore/unignore policy and reconciliation audit results without provider keys or secrets.

Part 2 consumes these APIs to implement the review-first UI, bulk selection, folder comparison, ignore controls, and move-to-trash confirmation.

## Additional smells added after implementation inspection

- The removed client SHA-256 field was not a dependable production integrity contract: the browser did not send it and the GCS transfer completion path did not produce it, while the memory provider did. Tests therefore overstated production behavior.
- Server-side copy recovery accepted any same-size destination after a race. It must also match the provider fingerprint when the operation was written by epoch 004.
- A lint tied to one known checkpoint call would miss aliases and future helpers. Enforcement must be structural at the storage boundary and backed by an exact exemption registry.
- Checkpoint v3 initially required complete provider fingerprints for state objects as well as blobs. This is useful for portable checkpoint integrity, but missing metadata must produce a clear provider-capability error rather than trigger a body-read fallback.
- Synthetic large-object tests must model provider-attested metadata explicitly; materializing or hashing multi-gigabyte fixtures in the service is not acceptable test behavior.
- The first refactor left the complete checkpoint-v2 builder, authenticated SHA journal codec, and its secret key in production even though checkpoint-v3 no longer called them. Keeping a forbidden mechanism as dead production code invites regression and needlessly expands the maintenance/security surface. Part 1 removes the builder, journal codec, and key; only legacy artifact keys and checkpoint envelopes remain recognizable for closed-gate retirement.
- Directory mutation originally converted the persistent index back into a complete entry slice before changing one child. Part 1 now applies an exact before/after entry delta to O(log D) copy-on-write nodes and then propagates bounded aggregate/content deltas through the ancestor trail.
- State listing originally embedded a complete namespace snapshot in every cursor. Part 1 now uses a constant-size authenticated continuation over an immutable persistent state-index root.
- Aggregate migration originally retained complete discovered/root maps and revisited every completed leaf after restart. Part 1 now uses bounded provider listing plus exact persisted transform/verify directory marks. Its active graph traversal is O(depth), but the current adjacent transformer still materializes one legacy directory and its derived descendant-content run while bulk-building indexes; eliminating that final O(largest selected subtree) component remains in scope before this PR is complete.
- Operation and admission records originally embedded prerequisite bodies and whole copy/root arrays. Part 1 moves bodies once to immutable operation staging and publishes bounded hash-chained step pages; terminal artifacts and idempotency bindings now have an explicit 30-day recovery window.
- Closed-gate maintenance ran a complete legacy state-version lookup pass immediately before reachability collection. Part 1 removes that duplicate traversal for schema 004 and binds resumable mark/sweep state to the exact closed-gate logical version.
- Directory comparison arithmetic could wrap reclaimable/intersection bytes and its zero-byte size consistency check was incomplete. Part 1 adds overflow-safe accumulation and rejects any same-group size contradiction, including `0` versus nonzero.
- Non-name directory sorting originally materialized and re-sorted the selected directory. Part 1 now maintains fixed persistent `kind`, `modified`, and `size` indexes beside the name index, and all four orders use authenticated keyset cursors with O(log D + page-size) reads.
- Selected partial-folder comparison originally materialized both complete file multisets, and exact reconciliation recomputed them for each preview page. Part 1 now pins an immutable per-directory content-occurrence B+ tree in every manifest. Comparison and preview merge two group/path-ordered trees with O(page-size) memory; continuation cursors bind both roots and carry the authenticated comparison totals so later pages resume rather than rebuild either inventory.
- Recursive copy/move catalog preparation still accumulates the complete subtree's prerequisites, copy list, and occurrences before it emits operation pages. This is now a Part 1 scalability item: make subtree preparation a durable paged operation phase with a restart cursor and O(depth + page-size) working memory before this PR is complete.
- Automatic overlap-candidate discovery is not supplied by exact directory digests alone. This remains in Part 1 backend scope: maintain a bounded mutation-friendly similarity sketch/posting index and expose paginated candidates; the Part 2 UI must not discover candidates by scanning the bucket or materializing an O(directory-pairs) matrix.

## Pull-request gates

- Focused failing tests precede each behavior change.
- Provider contract, migration fixture/profile, interrupted predecessor residue, multi-replica convergence, denial, call-count, and no-body-read tests pass.
- The epoch-004 fixtures are produced by the actual epoch-004 writer and pinned to their producer commit plus hard-coded SHA-256 fixture-file digest.
- The specification, operations guide, HTTP API, release evidence, and limitation text match the implemented behavior.
- `nix run .#lint`, `nix run .#test-migration`, focused race/contract checks, and finally `nix flake check` pass.
