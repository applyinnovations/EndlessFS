# Contributing to EndlessFS

EndlessFS is implementing the normative contract in `docs/v1-specification.md`. Start with `AGENTS.md`, identify the next incomplete acceptance behavior in the current milestone, and keep the pull request small enough that its security boundaries are reviewable.

All development and checks run through Nix:

```console
nix develop
nix run .#fmt
nix flake check
```

Use red → green → refactor. A feature begins with a failing behavior test, a bug begins with a failing regression test, and a security change includes success and denial cases. Explain that evidence in the pull request.

Pull requests should:

- implement one coherent slice of a specification milestone;
- preserve provider neutrality and the direct data-plane boundary;
- contain no secret, credential, personal data, or production file metadata;
- add no forbidden language, task runner, frontend framework, or runtime service;
- update documentation and acceptance evidence with the behavior; and
- pass `nix flake check` from a clean checkout.

New direct dependencies require a written rationale covering standard-library alternatives, maintenance, license, and security history.

By contributing, you agree that your contribution is licensed under Apache-2.0.
