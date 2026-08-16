# EndlessFS

EndlessFS is an open-source, provider-neutral, security-first private cloud drive. Its Go control plane authorizes file operations while browser file bytes travel directly to and from the configured object-storage provider through short-lived provider-native capabilities.

> [!IMPORTANT]
> This repository is at **Milestone 1**. The typed domain, strict paths, state CAS/codec contracts, provider abstraction, deterministic in-memory provider, and capability-aware local data plane are implemented. Authentication, application workflows, themes, and the complete browser UI remain under construction. Do not deploy this implementation or describe it as v1 complete or production-ready.

The normative implementation contract is [docs/v1-specification.md](./docs/v1-specification.md). A feature-complete mock-backed v1 will prove the full product locally, but it will not prove Google Cloud Storage interoperability or provide a production storage adapter.

## Why EndlessFS

- Passkey-only, usernameless identity with no password, email identity, or OAuth path.
- Strict logical user isolation and deny-by-default authorization.
- Direct browser-to-provider uploads and provider-to-browser downloads; the control plane does not proxy file bodies.
- Provider-neutral domain and state contracts backed by deterministic local implementations in v1.
- One Go binary with embedded HTML, application CSS, vanilla JavaScript, and validated theme media.
- Data-only themes: typed tokens and allowlisted static assets, never theme CSS, HTML, JavaScript, or remote code.
- No required SQL database, cache, queue, external identity service, analytics service, or persistent application filesystem.
- Nix as the sole development, build, test, package, and CI/CD interface.

## Architecture

```mermaid
flowchart LR
    B["Browser"] -->|"Authentication, metadata, authorization"| C["EndlessFS control plane"]
    C -->|"Provider control and state contracts"| P["Configured storage provider"]
    C -->|"Short-lived capability"| B
    B ==>|"File bytes"| P
```

Required dependency direction:

```text
HTTP and embedded UI -> application use cases -> domain + provider/state interfaces
provider implementations ---------------------> provider/state interfaces
```

Provider object keys never cross the public API. Private operations derive the owner scope from the authenticated session, not from client input.

## Start developing

Install [Nix with flakes enabled](https://nixos.org/download/), then run:

```console
nix develop
nix flake check
nix run .#dev
```

The development server listens on `http://127.0.0.1:8080` by default. The current implementation intentionally rejects non-loopback listeners until the complete deployment security contract is enforced.

Build the binary or an OCI archive without Docker:

```console
nix build
nix build .#container
```

All required builds and checks are Nix sandbox derivations, so project code cannot quietly depend on tools installed on the host. The required test gates are designed to run without cloud credentials, GCP, databases, persistent services, a container daemon, or non-loopback network access.

## Nix task interface

The v1 spec reserves the following interface. Implemented commands are usable now; incomplete commands deliberately return an error so an empty placeholder cannot be mistaken for validation.

| Command | Current purpose |
|---|---|
| `nix develop` | Enter the pinned Go/Nix development environment. |
| `nix build` | Build the static `endlessfs` binary with embedded browser assets. |
| `nix build .#container` | Build a minimal, shell-free OCI archive. |
| `nix flake check` | Run the authoritative current build, format, lint, test, fuzz, race, security, policy, offline-sandbox, and OCI hardening gates. |
| `nix run .#dev` | Run the loopback-only development control plane. |
| `nix run .#fmt` / `.#fmt-check` | Apply or verify Go and Nix formatting. |
| `nix run .#lint` | Run `actionlint`, `go vet`, and `staticcheck`. |
| `nix run .#test` / `.#test-unit` | Run the current Go suite. |
| `nix run .#test-integration` | Run tests named as integration tests. |
| `nix run .#test-contract` | Run reusable provider and state-store contract suites. |
| `nix run .#test-e2e` | Reserved for Milestone 5; currently fails closed. |
| `nix run .#test-race` | Run the suite with Go's race detector. |
| `nix run .#test-fuzz` | Run bounded configuration and canonical-path fuzz smoke targets. |
| `nix run .#security` | Run deterministic static and forbidden-source checks. |
| `nix run .#container` | Build the local OCI archive through Nix. |
| `nix run .#theme-check -- PATH` | Reserved for Milestone 4; currently fails closed. |
| `nix run .#theme-preview -- PATH` | Reserved for Milestone 4; currently fails closed. |
| `nix run .#test-theme` | Reserved for Milestone 4; currently fails closed. |

Longer fuzz campaigns can set `ENDLESSFS_FUZZTIME`, for example:

```console
ENDLESSFS_FUZZTIME=2m nix run .#test-fuzz
```

## Current configuration

Only settings that have validation and tests are parsed by the current binary:

| Variable | Default | Behavior now |
|---|---:|---|
| `ENDLESSFS_LISTEN_ADDR` | `127.0.0.1:8080` | Must be a loopback `host:port`. |
| `ALLOW_REGISTRATION` | `false` | Exact `true` or `false`; exposed as non-secret public policy. |
| `INVITE_REGISTRATION` | `true` | Exact `true` or `false`; exposed as non-secret public policy. |

The remaining environment contract in specification section 15 will be added with the behavior it protects. Secrets will never be accepted as command-line arguments or exposed through the public configuration endpoint.

## Repository map

```text
cmd/endlessfs/           process entry point
internal/config/         environment parsing and validation
internal/domain/         strict paths, names, IDs, entries, operations, and capabilities
internal/model/          strict versioned persistence records
internal/provider/       provider contract and deterministic capability-aware memory provider
internal/state/          state contract, strict codec, and concurrency-safe memory CAS store
internal/secret/         redacted bearer-token hashing and validation
internal/httpapi/        router and transport security headers
internal/web/            embedded HTML, CSS, and vanilla JavaScript
tools/check-source/      forbidden dependency/source policy check
tools/repository-policy/ checked-in GitHub ruleset validator/applier
.github/rulesets/        declarative default-branch and release-tag policy
.github/workflows/       PR, CI, GHCR, release, and policy workflows
docs/                    normative specification and project documentation
AGENTS.md                repository instructions for implementation agents
```

The package structure will expand in the order defined by specification section 20: identity, file control plane, themes, browser drive, sharing/admin UI, then adversarial hardening and release proof. Direct dependency rationale is recorded in [docs/dependencies.md](./docs/dependencies.md).

## CI, containers, releases, and branch protection

GitHub Actions contains no project test selection or tool installation logic beyond bootstrapping Nix. Actions are pinned to immutable revisions, Nix build outputs are cached, full checks use a standard Ubuntu runner, and a bounded macOS job proves contributor-platform compatibility.

- `CI` runs the authoritative Nix gate on pull requests, merge groups, and `main`, plus a Darwin build/unit smoke test.
- `PR` provides the fast format, lint, and source-policy status used by the default-branch ruleset.
- `Container` publishes `sha-<commit>` and `edge` images to `ghcr.io/applyinnovations/endlessfs` after changes reach `main`.
- `Release` re-verifies `v*.*.*` tags, publishes version and `latest` images, and creates a GitHub release containing the Nix-built archive, checksum, and initial release inventory.
- `Repository Policy` explicitly applies the checked-in branch and tag rulesets.

Repository rules are external GitHub state, so checking in JSON does not activate them by itself. A repository administrator must:

1. Create a fine-grained token limited to this repository with **Administration: write**.
2. Add it as the `REPOSITORY_RULESET_TOKEN` secret in the protected `repository-policy` environment.
3. Run the `Repository Policy` workflow once, and again after intentionally changing `.github/rulesets/*.json`.

The policy requires one approval, resolved review threads, linear history, and the `Nix checks` and `Fast checks` job contexts; it blocks deletion and force-push of the default branch and protects `v*` release tags from mutation. Review these defaults before the first collaborative merge.

## Delivery roadmap

- **Milestone 0 — complete:** reproducible skeleton, binary, embedded shell, Nix checks, OCI, CI and repository policy.
- **Milestone 1 — complete:** typed domain, strict paths, state CAS, provider contracts, capability-aware local data plane, and deterministic faults.
- **Milestone 2 — next:** WebAuthn, sessions, CSRF/origin policy, bootstrap, registration matrix, invites, roles, and recovery.
- **Milestone 3:** browse and file operations, direct resumable transfers, idempotency, trash, previews, and sharing control plane.
- **Milestone 4:** closed Theme API, safe media validation, complete light/dark bundles, inheritance and fallback.
- **Milestones 5–6:** accessible browser drive, transfers, themes, shares, settings, and administration UI.
- **Milestone 7:** cross-user/adversarial matrices, full fuzz/race/coverage gates, browser accessibility, OCI inspection, and release evidence.

v1 is done only when every acceptance criterion in section 21 is evidenced, the section 22 checklist is complete, and a clean, network-denied `nix flake check` passes.

## Contributing and security

Read [AGENTS.md](./AGENTS.md) and [CONTRIBUTING.md](./CONTRIBUTING.md) before making a change. Security reports should follow [SECURITY.md](./SECURITY.md), not a public issue.

EndlessFS is licensed under [Apache-2.0](./LICENSE).
