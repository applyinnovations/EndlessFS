# EndlessFS v1 implementation evidence

This file is the living evidence index for the acceptance criteria and feature-completion checklist in [the v1 specification](./v1-specification.md). A checked milestone means its scoped implementation and automated evidence exist; it does not mean that v1 as a whole is complete.

## Reproducible foundation — complete

- The one-binary Go module, embedded browser shell, pinned Nix environment, minimal OCI image, release artifacts, GitHub workflows, and repository rulesets were introduced in commit `8c04cbb`.
- `tools/check-source` rejects forbidden languages, dependency managers, task runners, and external browser resources.
- `internal/config` validates the implemented environment contract and provides `FuzzParse`.
- `internal/httpapi` tests health, readiness, public configuration, route boundaries, payload limits, and security headers.
- `nix flake check`, multi-system flake evaluation, OCI inspection, release checksums, and a local server smoke test form the bootstrap evidence.

## Domain, provider, and state — complete

The Milestone 1 checkpoint implements specification sections 7–9 and checklist 22.2:

| Requirement | Automated evidence |
|---|---|
| Opaque IDs and injected entropy/time | `TestIDGeneratorUsesRequiredEntropyAndUnpaddedBase64URL`, `TestIDGeneratorRejectsShortRandomReads`, `TestFixedClockIsDeterministic` |
| Every canonical `UserPath` rule and trusted scopes | `TestParseUserPathCanonicalizesAndPreservesCase`, `TestParseUserPathRejectsEveryBoundaryViolation`, `TestUserPathNavigationCannotEscapeRoot`, `TestUserPathJSONAlwaysRevalidates`, `TestScopeRequiresTrustedValidValues`, `FuzzParseUserPath` |
| Strict display names and passkey labels | `TestDisplayNameNormalizesAndTrims`, `TestDisplayNameAndCredentialLabelLimits`, `TestCredentialLabelUsesShorterLimit` |
| Strict, bounded persistence records | `TestStrictJSONRoundTrip`, `TestStrictJSONRejectsMalformedRecords`, `TestMemoryStoreRejectsInvalidRecordSizes` |
| Profile has exactly two identity fields; theme preference is separate | `TestProfileContainsExactlyIdentityFields`, `TestThemePreferenceIsSeparateAndStrict`, `TestRecordDecoderRejectsFieldsOutsideSchema` |
| State CRUD, CAS, concurrency, pagination, cursor scope, and copy safety | reusable `statecontract.Run`, executed by `TestContractMemoryStore` |
| Provider listing, conflicts, versions, capabilities, range, resumability, faults, idempotency, isolation, metadata exclusion, and large logical objects | reusable `providercontract.Run`, executed by `TestContractMemoryProvider` |
| Raw bearer values cannot be recovered from storage or logs | `TestTokenHashUsesConstantShapeAndMatches`, `TestSecretValueCannotLeakThroughStringOrStructuredLog` |

The shared suites are independently runnable with `nix run .#test-contract`. The race gate executes every implementation and contract under Go's race detector. The in-memory provider's HTTP loopback data plane instruments transfer bytes so contract tests prove that file bodies do not pass through application use cases.

## Remaining milestones

- Identity, sessions, bootstrap, registration, invites, roles, and recovery.
- File/trash/share control-plane use cases and HTTP API.
- Closed theme compiler and immutable fallback bundles.
- Complete accessible browser workflows and browser-level tests.
- Adversarial matrices, coverage thresholds, threat-model review, and final release evidence.

Until every remaining section is implemented and every section 21 criterion and section 22 item has evidence, the repository MUST continue to identify itself as under construction rather than v1 complete.
