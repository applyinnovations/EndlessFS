# EndlessFS implementation guide

This file applies to the entire repository. `docs/v1-specification.md` is normative for the feature-complete v1.0 baseline. Approved versioned extension specifications inherit it and are normative only for their declared release scope. When this guide and an applicable specification differ, follow the specification and correct this guide in the same change.

## Mission and current state

Maintain and extend the provider-portable v1 implementation without weakening its security or reproducibility requirements. Milestones 0–7 provide the reproducible foundation, application-facing provider/state contracts, passkey identity, file/trash/preview/share control plane, closed data-only theme system, complete accessible browser workflows, adversarial hardening, and release proof.
The v1.1 media-preview extension is implemented for provider-portable source storage and the deterministic independent preview-store boundary recorded in its evidence. Its virtualized grid and accessible viewer are unconditional; generated image thumbnails remain optional. The v1.2 video and v1.3 PDF drafts are deferred for revision after v1.1. Never present an unimplemented placeholder, graceful fallback, skipped path, or empty test selection as evidence that an optional extension itself is complete.

The clarified provider-agnostic v1 contract requires the canonical provider-independent single-/split-bucket storage-set format, portable logical versions, one portable storage engine over thin object-store backends, safe multi-replica write admission/fencing/recovery, and verified quiescent raw-copy portability in spec sections 5, 8, 9, 18, 21, 22.3, and 22.4. Implemented claims still require evidence for every relevant checklist item; the repository MUST NOT be described as v1 feature complete while any required item remains unchecked. These are clarifications of v1, not a new specification or post-v1 milestone.

The mock backend remains ephemeral and is not a production storage provider. Local GCS protocol qualification, live GCS validation, cloud resources, credentials, deployment, and production-provider claims remain distinct stages. No live GCS resource or credential is required by the deterministic v1 gate. Never present an unimplemented placeholder, skipped path, empty test selection, local protocol mock, or unchecked portability item as evidence of completion or live interoperability.

## Required workflow

1. Read the relevant spec sections, acceptance criteria, and feature-completion checklist items before editing.
2. Express the next observable behavior with a failing test and confirm the expected failure.
3. Implement the smallest behavior that makes the test pass.
4. Refactor without changing behavior.
5. Run focused checks, then `nix flake check` before declaring the change complete.
6. Record acceptance evidence in the release record/checklist as those artifacts are introduced.

Bug fixes start with a regression test. Security fixes need both an exploit/denial test and a valid-path test. Provider behavior starts in the shared contract suite. Race-sensitive state changes need explicit concurrent tests.

Do not delete, skip, relax, or rename a gate simply to turn CI green. A reserved Nix command may fail closed while its milestone is unimplemented, but it must be implemented before its corresponding checklist item can be claimed.

## Storage schema and migration law

Safe upgrade is a permanent storage-format invariant, not an incident response or operator repair task. The production source of truth is the append-only schema and release ledgers in `internal/portable/migration_ledger.go`. The schema ledger records immutable epochs with each new entry carrying the one adjacent transformation from its predecessor. The separate release ledger records the first release written with an epoch; the next appended boundary closes the preceding half-open validity interval without editing it. Startup resolves the persisted epoch, constructs the complete remaining suffix of the schema ledger, and executes each edge in order. Bespoke release conditionals, direct jumps, branching histories, and ordinary-runtime decode fallbacks are forbidden.

The migration law is:

1. Treat a schema epoch as the complete canonical state/file-backend data model, not one changed record or one release. Epoch IDs, order, feature signatures, checkpoint IDs, completed transformations, and release boundaries are immutable. A new durable schema appends one epoch carrying its `N -> N+1` transformation. Its first released version appends one release boundary. It never edits or inserts an old step or boundary.
2. Every durable key, body, invariant, or required-feature change MUST create the next epoch unless the bytes and semantics remain exactly unchanged. The adjacent transformation owns all record changes needed for that edge. Multiple record changes in one release still form one coherent epoch transition.
3. Preserve immutable raw state/file-backend fixtures for every ledger epoch under `internal/portable/testdata/migrations`, including untagged intermediate epochs that existed in deployed code. The fixture matrix MUST cover every durable application feature profile that a release can write, not only a feature-minimal portable-engine configuration. Produce each fixture with code that actually wrote that epoch and profile; never regenerate, normalize, or repair it with current code. Bind it to its source tag or commit and a hard-coded SHA-256 digest. Fixtures and their ledger association are append-only.
4. Release validity belongs in the production ledger, separate from fixtures. Consecutive releases may share an epoch when their durable bytes and semantics are identical. Release CI MUST resolve the candidate tag through a declared validity range and refuse candidates that map to no epoch or to an epoch without a fixture.
5. The migration gate MUST start independently from every recorded epoch/profile fixture in fresh deterministic single- and split-backend stores, traverse the entire remaining ordered edge sequence, verify the complete authoritative result, and complete a new mutation. Application-level qualification MUST construct the writer configuration through the real startup path for every supported durable profile and open the corresponding predecessor fixture. It MUST also exercise a large split-backend inventory through delayed/interrupted reads, prove completed checkpoint work is not downloaded again after restart, and prove startup/liveness remains available while readiness stays false for the entire migration window. Merely decoding one record, testing only the immediately previous epoch, substituting a feature-minimal writer, using tiny file bodies as performance evidence, or reaching readiness is insufficient proof.
6. Every adjacent edge MUST prove idempotent restart after each durable boundary and safe two-to-eight-replica convergence for every predecessor profile that can reach it. Preserve additional immutable fixtures for predecessor-binary intermediate states observed after an interrupted or failed upgrade, and start the current binary from those exact bytes. Current-code fault injection supplements this evidence but cannot substitute for predecessor-produced residue. Resumable checkpoint work MUST be provider-independent, authenticated, bound to the exact closed gate and backend role, revalidated through the object-store integrity contract, excluded from the authoritative checkpoint, and denied when forged, stale, misplaced, missing, or corrupt. The composed oldest-to-current path MUST prove exact edge order. The matrix also covers valid zero/empty values and fail-closed malformed, truncated, mixed, newer, and semantically corrupt state. No test may depend on an already-mutated fixture, wall-clock sleep, provider metadata, or a network service.
7. A schema change starts with the prior epoch fixture and a failing test from that entry point. The same pull request appends the ledger definition (including its incoming adjacent transformer), appends its release boundary when applicable, adds exact fixtures and full-suffix/crash/concurrency/denial coverage, and updates the specification, operations guide, and evidence. A one-off startup branch is not a migration implementation.
8. Transformations are forward-only, deterministic, idempotent, crash-resumable, and safe under mixed-version replicas. They quiesce through the canonical write gate, use portable records plus native CAS only for the immediate conditional operation, fail closed on ambiguity or corruption, and do not publish partial logical state.
9. `nix run .#test-migration` and the `migration` flake check are mandatory pull-request gates. They MUST execute the epoch/profile matrix, predecessor-interruption fixtures, real application writer-profile startup tests, candidate release mapping locally, and enforce at least 98% aggregate statement coverage across `internal/portable/migration.go` and `internal/portable/migration_ledger.go`. The percentage is a backstop, not permission to omit explicit boundary, denial, restart, concurrency, or fault-injection assertions. Release CI runs the same `nix run .#test-migration -- "$release_tag"` command before building or publishing artifacts; CI is confirmation, never the first place a migration profile is exercised.

Removing an epoch, fixture, transformation, or supported release range requires an explicit major-version compatibility policy and a reviewed specification change. It is never incidental cleanup.

## Toolchain contract

Nix is the only public task interface. Do not add a Makefile, Taskfile, Justfile, bespoke host setup, or duplicated shell test pipeline.

Use these commands:

```text
nix develop
nix build
nix build .#container
nix build .#release
nix flake check
nix run .#dev
nix run .#dev-fixture
nix run .#fmt
nix run .#fmt-check
nix run .#lint
nix run .#test
nix run .#test-unit
nix run .#test-integration
nix run .#test-contract
nix run .#test-migration
nix run .#test-preview
nix run .#test-replica
nix run .#test-portability
nix run .#test-e2e
nix run .#test-ui-benchmark
nix run .#test-coverage
nix run .#test-race
nix run .#test-fuzz
nix run .#test-theme
nix run .#theme-check -- PATH
nix run .#theme-preview -- PATH
nix run .#security
nix run .#dependency-check
nix run .#container
nix run .#provider-verify -- check CONFIG
nix build .#container-images
nix build .#release-images
```

Application, server, test-driver, helper, and generator code must be Go. Browser code is embedded semantic HTML, application-owned CSS, and minimal vanilla JavaScript. Do not introduce Node.js or a frontend/CSS framework. Do not add Python, Ruby, Java, .NET, PHP, Rust, SQL, Redis, queues, Docker Compose, or a required container runtime.

Pin every Go module, Nix input, and Tekton task runtime image. GitHub Actions
workflows are retired after the xlab PaC cutover and must not be reintroduced.
Go modules are locked by `go.mod`, `go.sum`, and Nix's fixed-output module hash;
`vendor/` is generated inside Nix builds and MUST NOT be tracked. Justify a
direct dependency in review: maintenance health, license, security history, and
why the standard library is insufficient. Cryptography and WebAuthn must use
established libraries, never custom protocols.

## Architectural boundaries

Required dependency direction:

```text
HTTP/UI -> application use cases -> domain + provider/state interfaces
portable storage engine ---------> provider/state + object-store interfaces
object-store adapters -----------> object-store interface
```

Keep process setup in `cmd/endlessfs`. Put behavior in narrow `internal` packages. Domain and application packages must not import HTTP transport, mock backends, object-store adapters, GCS SDKs, or construct raw object keys. Backend adapters must not receive virtual paths, implement filesystem/state semantics, invent provider-specific durable layouts, or decode/re-encode canonical records.

The canonical format package alone constructs bounded provider-independent object keys and encodes authoritative records. The portable engine alone implements `StorageProvider` and `StateStore` behavior, canonical write-gate admission, immutable directory manifests, durable operation state, fencing, takeover, staging publication, and checkpoint quiescence. GCS, memory, local HTTP, and every future S3/Azure adapter implement only atomic conditional object operations, strong read/list visibility, server-side copy, direct capabilities, resumability, and provider error/authentication translation.

Expected v1 package responsibilities are described in spec section 5.3. Add them as their milestone begins; avoid speculative abstractions with no tested behavior.

The base production artifact is one Go application binary with all frontend and validated theme assets embedded. Helper commands used only for development/policy may exist under `tools`, but they must not enter the base production image. An approved specialized media profile may add only the pinned, inventoried codec/renderer runtime expressly allowed by its specification; those dependencies must remain absent from profiles that do not declare the capability.

## Security invariants

Treat spec section 7 as a mandatory review checklist. In particular:

- Derive every private owner scope from the authenticated session; never accept a user ID as storage scope.
- Accept only canonical validated virtual paths. Never expose or accept provider keys in public APIs.
- Keep reserved application metadata outside list, file, trash, preview, and share namespaces.
- Keep all authoritative state in the canonical `endlessfs/v1` key/body format. Provider-native generations, ETags, version IDs, metadata, endpoints, capabilities, upload/multipart/block IDs, and rewrite/copy tokens cannot enter durable canonical records.
- Use portable logical versions for application concurrency. Native versions are immediate backend preconditions only and must be discarded after the request.
- Treat provider custom metadata, tags, ACLs, storage class, object versioning, native timestamps/checksums, folder resources, listing order, and page sizes as non-authoritative.
- Create portability checkpoints only while mutations are quiesced and provider-native leases are drained or aborted; corrupt, incomplete, mixed, unsupported, or unverified destination state fails closed.
- Admit every mutation through candidate-ticket creation, a second canonical gate read, and candidate-to-admitted CAS. Do not substitute process-local maintenance state, a leader, load-balancer draining, sticky routing, or a grace period for this durable admission protocol.
- Publish directory/file changes only through CAS-controlled roots or a committed durable operation. Direct browser uploads and intermediate provider results target immutable operation staging, never visible file state.
- Treat lease expiry only as eligibility for a one-winner CAS takeover that increments a portable fence. Never delete or unlock solely because time elapsed; stale workers must fail the same-object conditional visibility commit.
- Require simultaneous replicas to match the canonical writer-set identity, protocol, security-critical configuration fingerprint, feature set, and provider-independent keyring identifiers before readiness.
- Deny by default when there is no explicit policy decision.
- Never log or persist raw session, CSRF, ceremony, bootstrap, invite, recovery, share, or provider-capability secrets.
- Keep the identity profile to exactly `userID` and `displayName`. Do not model email, username, OAuth subject, or social identity.
- Use injected clocks, randomness, IDs, and fault schedules in tests; use cryptographically secure system sources in normal operation.
- Implement one-time and final-admin changes with state-store conditional operations and crash-safe, idempotent state machines.
- Reject unexpected control-plane bodies. File bytes must use the distinct capability-bearing data plane.
- Keep themes data-only and closed-schema. Theme input can never add code, markup, arbitrary CSS, network origins, application wording, accessibility semantics, or behavior.

Any change touching authentication, authorization, paths, canonical keys/records/versions, state CAS, backend conditions/consistency, write admission, directory manifests, operation ownership/fencing/takeover, staging, tokens, capabilities, shares, trash, checkpoints, replica compatibility, portability, themes, logging, or provider scoping needs explicit positive and negative tests.

## Test organization

Name cross-layer tests with the runner prefixes already used by the flake:

- `TestIntegration...` for real router/middleware/use-case tests.
- `TestContract...` for reusable provider/state contract behavior.
- `TestE2E...` for Go-controlled Chromium workflows.

Keep unit tests beside their packages. Put reusable application-facing provider/state, object-store backend, canonical-format, multi-replica, and raw-copy portability contract suites in importable test packages. Application semantics run once through the portable engine over every backend. Add fuzz seeds for every known traversal/encoding, key, envelope, logical-version, superblock, writer-set/gate, admission, manifest, operation/fence, and checkpoint case. Tests must not depend on order, wall-clock sleeps, cloud credentials, network services, or persistent host state.

Multi-replica tests use two through eight separately constructed engines and a deterministic scheduler that can pause, crash, partition, restart, and resume them at every admission, lease, staging, provider-response, root-prepare, operation-commit, finalization, gate, and checkpoint boundary. They prove one-winner takeover, stale-fence denial, lost-success recovery, complete directory visibility, compatibility rejection, and no permanent lock without wall-clock sleeps.

Portability tests close the canonical state-backend gate, recover all admissions/operations, and copy only authoritative object keys and bodies into independently configured destination roles. They cover both one-backend and split state/file-backend layouts, deliberately change every native version and provider metadata value, reopen the destination at a new gate epoch, and continue multi-replica mutations. They cover complete identity/file/share/trash/theme state and fail-closed corruption or misplaced-object cases. GCS integration tests use an in-process protocol-level HTTP fake and must not contact GCP metadata, token, or storage endpoints.

The completed v1 gate requires at least 85% repository statement coverage, at least 95% in the security-sensitive packages enumerated in spec section 18.4, and at least 98% across the production migration ledger and implementation. Coverage does not replace invariant tests.

## HTTP and browser rules

Public API routes live under `/api/v1` except documented health, asset, and public-share routes. Strictly decode JSON: bounded body, valid UTF-8, no unknown or duplicate fields, no trailing content. Return stable `application/problem+json` responses without secrets or cross-boundary existence leaks.

Authenticated mutations require CSRF plus exact-origin validation. Authentication ceremonies require exact-origin and ceremony binding before a session exists. GET and HEAD are side-effect free.

Keep the browser self-contained. Do not add runtime-fetched scripts, fonts, images, analytics, telemetry, service workers, or sensitive local storage. Render untrusted values as text. Maintain the CSP, `nosniff`, no-referrer, permissions, and opener policies. Core workflows target WCAG 2.2 AA, keyboard operation, visible focus, reduced motion, live status, and 320 CSS-pixel layouts.

## Themes

Read all of spec section 14 before theme work. Built-in light and dark themes are immutable, complete bundles processed by the same compiler as custom bundles. A custom bundle directly extends one built-in parent and can only override registered typed tokens, fonts, and semantic media.

Never interpret raw theme strings as CSS or HTML. Validate archives, normalized paths, sizes/ratios, signatures, decoded dimensions, reference closure, SVG static subsets, WOFF2 declarations, contrast, inheritance, IDs, API compatibility, and canonical digests before embedding. Runtime selection never parses an archive or reads a mutable theme directory.

## GitHub and releases

Tekton PaC workflows run on xlab Linux compute, bootstrap Nix, invoke flake
commands, cache Nix outputs on local NVMe, and publish their results. Do not
duplicate project test logic in YAML or install Go/Node tools directly. Do not
add GitHub Actions workflows. The retired Darwin workflow must remain
triggerless and must never reference or start Namespace Mac runners.

`.github/rulesets/*.json` is the source of truth for branch/tag policy. Validate
it with `nix run .#repository-policy -- check`. Applying it is an explicit
administrator action from a trusted checkout through
`nix run .#repository-policy -- apply`; never place an administration token in
source or ordinary CI.

Release tags are `vMAJOR.MINOR.PATCH`. A v1 release needs the evidence in spec section 19.3, including source/input hashes, check and coverage summaries, canonical-format/writer-protocol/checkpoint/portability fixture digests, multi-replica schedule results, binary/OCI hashes, dependency and theme inventories, limitations, and confirmation that no credentials or external services were used.

## Definition of done

Before handing off a change, verify every item in spec section 24. At minimum: tests prove the behavior, relevant Nix checks pass, boundaries have success and denial coverage, logs/errors remain safe, no forbidden dependency or service was introduced, canonical format/logical versions/write admission/fencing/stale-worker denial/raw-copy portability remain intact, no native provider value entered authoritative state, no lock relies on one process or timeout-only release, UI/theme contracts stay complete, and user/implementation documentation matches reality.
