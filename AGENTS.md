# EndlessFS implementation guide

This file applies to the entire repository. `docs/v1-specification.md` is normative; when this guide and the spec appear to differ, follow the spec and correct this guide in the same change.

## Mission and current state

Implement the complete mock-backed v1 specification in milestone order without weakening its security or reproducibility requirements. Milestones 0–5 currently provide the reproducible foundation, provider/state contracts, passkey identity, file/trash/preview/share control plane, closed data-only theme system, and accessible browser Drive with real Chromium evidence. Administration/recovery browser hardening and final adversarial, coverage, operations, and release proof remain unfinished, and the mock-backed runtime is not a production storage provider. Never present an unimplemented placeholder or empty test selection as v1 evidence.

Real GCS integration, cloud resources, credentials, deployment, and production-provider claims are outside the v1 completion boundary. Preserve that distinction in code, tests, docs, and release notes.

## Required workflow

1. Read the relevant spec sections, acceptance criteria, and feature-completion checklist items before editing.
2. Express the next observable behavior with a failing test and confirm the expected failure.
3. Implement the smallest behavior that makes the test pass.
4. Refactor without changing behavior.
5. Run focused checks, then `nix flake check` before declaring the change complete.
6. Record acceptance evidence in the release record/checklist as those artifacts are introduced.

Bug fixes start with a regression test. Security fixes need both an exploit/denial test and a valid-path test. Provider behavior starts in the shared contract suite. Race-sensitive state changes need explicit concurrent tests.

Do not delete, skip, relax, or rename a gate simply to turn CI green. A reserved Nix command may fail closed while its milestone is unimplemented, but it must be implemented before its corresponding checklist item can be claimed.

## Toolchain contract

Nix is the only public task interface. Do not add a Makefile, Taskfile, Justfile, bespoke host setup, or duplicated shell test pipeline.

Use these commands:

```text
nix develop
nix build
nix build .#container
nix flake check
nix run .#dev
nix run .#fmt
nix run .#fmt-check
nix run .#lint
nix run .#test
nix run .#test-unit
nix run .#test-integration
nix run .#test-contract
nix run .#test-e2e
nix run .#test-race
nix run .#test-fuzz
nix run .#test-theme
nix run .#theme-check -- PATH
nix run .#theme-preview -- PATH
nix run .#security
nix run .#container
```

Application, server, test-driver, helper, and generator code must be Go. Browser code is embedded semantic HTML, application-owned CSS, and minimal vanilla JavaScript. Do not introduce Node.js or a frontend/CSS framework. Do not add Python, Ruby, Java, .NET, PHP, Rust, SQL, Redis, queues, Docker Compose, or a required container runtime.

Pin every Go module, Nix input, and GitHub Action. Justify a direct dependency in review: maintenance health, license, security history, and why the standard library is insufficient. Cryptography and WebAuthn must use established libraries, never custom protocols.

## Architectural boundaries

Required dependency direction:

```text
HTTP/UI -> application use cases -> domain + provider/state interfaces
provider implementations --------> provider/state interfaces
```

Keep process setup in `cmd/endlessfs`. Put behavior in narrow `internal` packages. Domain and application packages must not import HTTP transport, mock implementations, GCS SDKs, or construct raw provider object keys.

Expected v1 package responsibilities are described in spec section 5.3. Add them as their milestone begins; avoid speculative abstractions with no tested behavior.

The production artifact is one Go application binary with all frontend and validated theme assets embedded. Helper commands used only for development/policy may exist under `tools`, but they must not enter the production image.

## Security invariants

Treat spec section 7 as a mandatory review checklist. In particular:

- Derive every private owner scope from the authenticated session; never accept a user ID as storage scope.
- Accept only canonical validated virtual paths. Never expose or accept provider keys in public APIs.
- Keep reserved application metadata outside list, file, trash, preview, and share namespaces.
- Deny by default when there is no explicit policy decision.
- Never log or persist raw session, CSRF, ceremony, bootstrap, invite, recovery, share, or provider-capability secrets.
- Keep the identity profile to exactly `userID` and `displayName`. Do not model email, username, OAuth subject, or social identity.
- Use injected clocks, randomness, IDs, and fault schedules in tests; use cryptographically secure system sources in normal operation.
- Implement one-time and final-admin changes with state-store conditional operations and crash-safe, idempotent state machines.
- Reject unexpected control-plane bodies. File bytes must use the distinct capability-bearing data plane.
- Keep themes data-only and closed-schema. Theme input can never add code, markup, arbitrary CSS, network origins, application wording, accessibility semantics, or behavior.

Any change touching authentication, authorization, paths, state CAS, tokens, capabilities, shares, trash, themes, logging, or provider scoping needs explicit positive and negative tests.

## Test organization

Name cross-layer tests with the runner prefixes already used by the flake:

- `TestIntegration...` for real router/middleware/use-case tests.
- `TestContract...` for reusable provider/state contract behavior.
- `TestE2E...` for Go-controlled Chromium workflows.

Keep unit tests beside their packages. Put reusable provider contract suites in an importable test package so every implementation runs identical semantics. Add fuzz seeds for every known traversal/encoding case. Tests must not depend on order, wall-clock sleeps, cloud credentials, network services, or persistent host state.

The completed v1 gate requires at least 85% repository statement coverage and at least 95% in the security-sensitive packages enumerated in spec section 18.4. Coverage does not replace invariant tests.

## HTTP and browser rules

Public API routes live under `/api/v1` except documented health, asset, and public-share routes. Strictly decode JSON: bounded body, valid UTF-8, no unknown or duplicate fields, no trailing content. Return stable `application/problem+json` responses without secrets or cross-boundary existence leaks.

Authenticated mutations require CSRF plus exact-origin validation. Authentication ceremonies require exact-origin and ceremony binding before a session exists. GET and HEAD are side-effect free.

Keep the browser self-contained. Do not add runtime-fetched scripts, fonts, images, analytics, telemetry, service workers, or sensitive local storage. Render untrusted values as text. Maintain the CSP, `nosniff`, no-referrer, permissions, and opener policies. Core workflows target WCAG 2.2 AA, keyboard operation, visible focus, reduced motion, live status, and 320 CSS-pixel layouts.

## Themes

Read all of spec section 14 before theme work. Built-in light and dark themes are immutable, complete bundles processed by the same compiler as custom bundles. A custom bundle directly extends one built-in parent and can only override registered typed tokens, fonts, and semantic media.

Never interpret raw theme strings as CSS or HTML. Validate archives, normalized paths, sizes/ratios, signatures, decoded dimensions, reference closure, SVG static subsets, WOFF2 declarations, contrast, inheritance, IDs, API compatibility, and canonical digests before embedding. Runtime selection never parses an archive or reads a mutable theme directory.

## GitHub and releases

Workflows should bootstrap Nix, invoke flake commands, cache Nix outputs, and publish their results. Do not duplicate project test logic in YAML or install Go/Node tools directly. Pin actions by full commit SHA and let Dependabot propose reviewed updates.

`.github/rulesets/*.json` is the source of truth for branch/tag policy. Validate it with `nix run .#repository-policy -- check`. Applying it is an explicit administrator action through the protected `Repository Policy` workflow; never place an administration token in source or ordinary CI.

Release tags are `vMAJOR.MINOR.PATCH`. A v1 release needs the evidence in spec section 19.3, including source/input hashes, check and coverage summaries, binary/OCI hashes, dependency and theme inventories, limitations, and confirmation that no credentials or external services were used.

## Definition of done

Before handing off a change, verify every item in spec section 24. At minimum: tests prove the behavior, relevant Nix checks pass, boundaries have success and denial coverage, logs/errors remain safe, no forbidden dependency or service was introduced, UI/theme contracts stay complete, and user/implementation documentation matches reality.
