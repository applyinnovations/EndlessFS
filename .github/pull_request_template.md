## Related issue

<!-- Use "Closes #123" when this pull request fully resolves an issue. -->

## Observable change

<!-- Describe what a user or operator can observe. -->

## Scope

<!-- Explain why this is one coherent change and identify anything deliberately left for a separate pull request. -->

## Evidence

<!-- List the exact Nix commands run and their results. Add sanitized screenshots or recordings for visible UI changes. -->

## Security and privacy

<!-- Note affected trust boundaries, or write "No boundary change." -->

## Review checklist

- [ ] A failing test or reproducible failure came first where practicable.
- [ ] Focused checks pass.
- [ ] `nix flake check` passes, or the reason it was not run is stated above.
- [ ] Security-boundary changes have explicit success and denial coverage.
- [ ] UI changes are keyboard operable, responsive at 320 CSS pixels, and do not introduce layout shift.
- [ ] No secret, personal data, private file content, provider identifier, or capability appears in code, fixtures, logs, screenshots, or issue links.
- [ ] No forbidden tool, runtime service, identity field, frontend framework, or provider-specific durable behavior was added.
- [ ] Documentation and applicable acceptance evidence are current.
