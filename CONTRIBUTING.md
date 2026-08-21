# Contributing to EndlessFS

EndlessFS is security-sensitive, provider-portable infrastructure. Read [`AGENTS.md`](./AGENTS.md), the applicable specification, and the relevant acceptance criteria before proposing or implementing a change. UI work must also follow the [brand guidelines](./docs/brand/README.md) and [UI rebuild plan](./docs/ui-rebuild-plan.md).

## Raise an issue

Search existing issues before opening a new one, then use the repository issue chooser:

- **Bug report** for reproducible behavior that is incorrect or unreliable.
- **Feature request** for a user problem or workflow the project does not yet address.
- **Security vulnerability** must follow [`SECURITY.md`](./SECURITY.md) and must not be filed publicly.

Write the smallest deterministic reproduction possible. Include the affected revision, expected and actual behavior, relevant scale, and a sanitized environment description. UI reports benefit from a screenshot or short recording and the browser viewport size. Never include credentials, tokens, provider identifiers, personal data, or private file content.

Feature requests should lead with the problem and desired outcome. Describe how the workflow should remain efficient and predictable with large collections or queues, and identify any security, offline, or provider-portability constraints.

## Prepare a change

Keep each pull request to one coherent behavior whose security boundaries can be reviewed independently. If a discovery requires unrelated backend, migration, infrastructure, or policy work, describe it in a separate issue rather than expanding the pull request silently.

Use red → green → refactor:

1. Add a failing behavior test. A bug starts with a regression test.
2. Confirm the expected failure.
3. Implement the smallest complete behavior.
4. Refactor without changing the behavior.
5. Run focused checks, then the full gate before merge.

Security changes require both exploit or denial coverage and a valid-path test. Race-sensitive changes require explicit concurrent tests. Provider behavior begins in the shared contract suite.

## Use the supported toolchain

Nix is the public task interface:

```console
nix develop
nix run .#fmt
nix run .#test
nix flake check
```

Use the narrower commands documented in [`AGENTS.md`](./AGENTS.md) while iterating. Do not add a second task runner, host setup path, frontend framework, or runtime service.

## Submit a pull request

Complete the pull-request template and include:

- the related issue;
- the observable behavior and explicit scope;
- the exact checks run and their results;
- sanitized visual evidence for UI changes;
- affected trust boundaries, or `No boundary change`; and
- follow-up work that intentionally remains out of scope.

UI evidence should cover the relevant desktop layout, an intermediate aspect ratio, and 320 CSS pixels. Confirm keyboard operation, visible focus, deterministic loading geometry, no layout shift, and comfortable behavior with large fixture collections.

Pull requests must preserve provider neutrality and the direct data-plane boundary, contain no secret or production data, update documentation and acceptance evidence with the behavior, and pass `nix flake check` from a clean checkout before merge.

New direct dependencies require a written rationale covering standard-library alternatives, maintenance health, license, and security history.

By contributing, you agree that your contribution is licensed under Apache-2.0.
