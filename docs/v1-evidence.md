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

## Identity and registration — complete

The Milestone 2 checkpoint implements specification sections 10, 12.3, and 12.4 plus checklist sections 22.3 and 22.4:

| Requirement | Automated evidence |
|---|---|
| Pinned established WebAuthn library, discoverable credentials, user verification, opaque user fields, and virtual-authenticator success | `TestIntegrationGoWebAuthnVirtualAuthenticatorUsernamelessFlow`; [WebAuthn threat review](./webauthn-threat-review.md) |
| Wrong origin, RP ID/hash, challenge, user handle, signature, owner, or missing verification fails | `TestIntegrationGoWebAuthnRejectsWrongOriginRPChallengeAndVerification`, `TestIntegrationGoWebAuthnAuthenticationNegativeMatrix` |
| Browser/type-bound, expiring, atomically consumed ceremonies and usernameless session issuance | `TestIntegrationCeremonyBindingExpiryReplayAndUsernamelessAuthentication` |
| Secure/loopback configuration coherence and secret-free public configuration | `TestParseDefaults`, `TestParseSecureConfiguration`, `TestParseRejectsUnsafeOrMalformedValues`, `TestIntegrationPublicConfigExposesNoSecrets` |
| One concurrent bootstrap winner and durable enabled first-admin materialization | `TestIntegrationConcurrentBootstrapCreatesExactlyOneAdmin` |
| Independent four-way public/invite policy at start and verification | `TestIntegrationRegistrationPolicyMatrixAndVerificationRecheck` |
| Process-local public-registration rate limit | `TestPublicRegistrationRateLimitIsLocalDeterministicAndBounded` |
| Single-use hashed invite; concurrent use, expiry, revocation, and safe listing | `TestIntegrationInviteIsHashedSingleUseAndConcurrent` |
| Stateful hashed sessions, secure host cookie, rotation, expiry, revocation, exact-origin CSRF, and disabled-account denial | `TestIntegrationSessionsCSRFRotationExpiryAndDisabledAccount`, `TestKeyedHashBindsProtectionKeyAndValue` |
| Recovery adds a passkey to the same identity, retains old credentials, consumes the hashed token, and revokes sessions | `TestIntegrationRecoveryAddsPasskeyPreservesIdentityAndRevokesSessions` |
| Multiple passkeys, labels, authentication with either credential, and concurrent final-passkey protection | `TestIntegrationRecoveryAddsPasskeyPreservesIdentityAndRevokesSessions`, `TestDisplayNameAndPasskeyLabelChangesDoNotAlterIdentityOrRoles` |
| Display name remains presentation-only and roles remain separate | `TestDisplayNameAndPasskeyLabelChangesDoNotAlterIdentityOrRoles`, `TestProfileContainsExactlyIdentityFields` |
| Public registration never grants admin; final enabled admin survives concurrent changes; admin cursors reject tampering | `TestIntegrationConcurrentAdminChangesPreserveEnabledAdministrator` |
| Strict identity HTTP JSON/problems, origin, session/CSRF cookies, protected mutations, and one-time admin link responses | `TestIntegrationIdentityHTTPBootstrapLoginCSRFAndAdmin`, `TestIdentityHTTPRejectsOriginAndMalformedJSONWithStableProblem` |
| No password-adjacent external identity concepts enter request, persistence, or embedded UI surfaces | `tools/check-source` identity-surface scan |

The selected WebAuthn and virtual-authenticator modules are pinned in `go.mod`, `go.sum`, `vendor/`, and the Nix source closure. The application binary uses only the real verifier; the virtual authenticator is test-only. Invite and recovery creation use durable idempotency claims: repeating a key cannot create another resource and cannot recover the deliberately one-time raw link.

## Remaining milestones

## File, transfer, trash, preview, and share control plane — complete

The Milestone 3 checkpoint implements specification sections 11, 12.5, and 12.6 plus checklist sections 22.5–22.7:

| Requirement | Automated evidence |
|---|---|
| Root/nested browse, deterministic pagination/sorting, scoped cursors, stat, and empty directories | reusable `providercontract.Run`; `TestIntegrationDirectTransfersAndIsolation`; `TestIntegrationFileHTTPDirectDataPathTrashAndShare` |
| Exact session-derived scope and cross-user denial for paths, uploads, cursors, operations, trash IDs, and shares | `TestIntegrationDirectTransfersAndIsolation`, `TestIntegrationCopyMoveTrashRestoreAndDelete`, `TestFileHTTPRejectsProviderFieldsBodiesAndTraversalBeforeProvider` |
| Direct single/resumable upload, provider-confirmed offsets, retry, checksum, cancellation, expiry, faults, and >1 TiB simulation | reusable `providercontract.Run`; `TestIntegrationDirectTransfersAndIsolation` |
| Control-plane byte exclusion, separate loopback data listener, exact-origin CORS, range reads, and safe disposition | `TestIntegrationFileHTTPDirectDataPathTrashAndShare`; provider contract `single upload direct download and range` |
| File/tree copy and move, deterministic conflict modes, bounded batch results, durable aggregate polling, and idempotency | `TestIntegrationCopyMoveTrashRestoreAndDelete`; provider contract `recursive operations idempotency and faults` |
| Isolated trash, list, restore with generated-name conflict, permanent delete, empty-trash bounds, and replay-safe mutations | `TestIntegrationCopyMoveTrashRestoreAndDelete`; `TestIntegrationFileHTTPDirectDataPathTrashAndShare` |
| Hashed 256-bit file/folder shares, relative subtree confinement, expiry, revocation, disabled owners, and moved/trashed root invalidation | `TestIntegrationSharesPreviewAndRevocation`; `TestIntegrationFileHTTPDirectDataPathTrashAndShare` |
| Provider-validated PNG/JPEG/GIF/WebP, bounded UTF-8 text, and PDF inline policy; spoofed or active formats remain attachments | `TestSafePreviewAllowlistUsesProviderValidatedMedia`; `TestIntegrationSharesPreviewAndRevocation` |
| Strict 1 MiB control documents, provider-field rejection before access, no-store capability responses, no-referrer public responses, and exact data-origin CSP | `TestFileHTTPRejectsProviderFieldsBodiesAndTraversalBeforeProvider`; `TestIntegrationFileHTTPDirectDataPathTrashAndShare` |

The public API accepts virtual paths only. Storage scopes are constructed from authenticated session owners, trash resides in a separate provider area, and share listings translate all paths relative to a version-bound root. The mock provider validates safe preview signatures after direct upload completion so a client-supplied media type alone cannot enable inline rendering.

## Remaining milestones

- Closed theme compiler and immutable fallback bundles.
- Complete accessible browser workflows and browser-level tests.
- Adversarial matrices, coverage thresholds, threat-model review, and final release evidence.

Until every remaining section is implemented and every section 21 criterion and section 22 item has evidence, the repository MUST continue to identify itself as under construction rather than v1 complete.
