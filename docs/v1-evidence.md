# EndlessFS v1 implementation evidence

This is the release evidence index for the acceptance criteria and feature-completion checklist in [the v1 specification](./v1-specification.md). The deterministic, mock-backed v1 boundary is feature complete. This record does not claim production durability, deployment readiness, or Google Cloud Storage interoperability.

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

The selected WebAuthn and virtual-authenticator modules are pinned in `go.mod`, `go.sum`, and Nix's fixed-output module closure. The application binary uses only the real verifier; the virtual authenticator is test-only. Invite and recovery creation use durable idempotency claims: repeating a key cannot create another resource and cannot recover the deliberately one-time raw link.

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

## Data-only theme system — complete

The Milestone 4 checkpoint implements specification section 14, acceptance criteria AC-056–AC-059 at the compiler/control-plane layer, and checklist 22.8 except the browser workflows that intentionally land in Milestone 5:

| Requirement | Automated evidence |
|---|---|
| Closed versioned registry with typed serializers, bounds, defaults, CSS mapping, contrast pairs, fonts, and complete semantic media slots | `TestThemeTokensAreClosedTypedBoundedAndContrastChecked`; generated `tools/theme api`; [Theme API 1.0](./theme-api.md) |
| Immutable complete light/dark bundles through the ordinary compiler | `TestBuiltinsAndMinimalCustomUseOrdinaryCompletePipeline`; `nix run .#test-theme` |
| Direct built-in inheritance, old-bundle compatible additions, unavailable selection, media fallback, and emergency safe theme | `TestBuiltinsAndMinimalCustomUseOrdinaryCompletePipeline`, `TestOlderCompatibleCustomInheritsSimulatedNewMediaSlot`, `TestThemeReferenceClosureAndAssetFallback`, `TestThemePreferenceIsSeparatePersistentAndSafelyResolved` |
| Strict manifest metadata/API/license/ID validation and collision prevention | `TestThemeManifestStrictMetadataAndCompatibility`, `TestThemeDigestIsCanonicalAndIDsCannotCollide` |
| ZIP/directory traversal, normalized duplicate, symlink, hard-link, raw-code, compression-ratio, count/size, and reference-closure defenses | `TestThemeZIPAndDirectoryDefenses`, `TestThemeReferenceClosureAndAssetFallback`, `FuzzThemeBoundaries` |
| Signature/dimension/pixel validation for PNG/WebP/AVIF, WOFF2 declarations, and bounded raster sprites | `TestMediaSignaturesDimensionsSpritesAndFontDeclarations` |
| Static SVG subset rejects declarations, scripts, handlers, external/data references, embedded content, raw style, and text | `TestSVGSanitizerRejectsActiveContentAndExternalReferences`, `FuzzThemeBoundaries` |
| Separate per-user `system`/installed preference, light/dark resolution, safe override, and allowlisted non-identity device cookie | `TestThemePreferenceIsSeparatePersistentAndSafelyResolved`, `TestIntegrationThemeHTTPMetadataPreferenceAssetsAndSafeFallback` |
| Exact content types, immutable digest URLs, `nosniff`, restrictive asset CSP, and no arbitrary filesystem lookup | `TestIntegrationThemeHTTPMetadataPreferenceAssetsAndSafeFallback` |
| Nix validation, preview, tests, and reproducible custom build-input embedding | `nix run .#theme-check`, `.#theme-preview`, `.#test-theme`; overridden `themeBundles` build and runtime inventory smoke proof |
| Application-owned responsive/focus/reduced-motion conformance fixture | `TestConformanceFixtureOwnsAccessibilityResponsiveAndReducedMotionRules`; loopback preview smoke proof at desktop/320 CSS rules |

The Nix-built production binary can be overridden with `themeBundles = [ ... ]`; each input is validated and compiled into generated Go data before the binary build. Normal runtime selection performs no archive parsing, mutable theme-directory lookup, or network installation. Release archives include `THEMES.json` with IDs, API versions, licenses, and content digests.

## Browser Drive and accessibility — complete

The Milestone 5 checkpoint implements specification section 13’s core Drive workflows and checklist section 22.9 at the Drive/browser boundary:

| Requirement | Automated evidence |
|---|---|
| Embedded bootstrap, registration, sign-in, Drive, trash, settings, admin, and public-share workspaces with explicit loading/empty/error states | `TestApplicationShellExposesCompleteAccessibleWorkspaces`, `TestIntegrationPublicEndpoints` |
| Real passkey bootstrap and usernameless login through a browser virtual authenticator | `TestE2EBrowserBootstrapLoginDriveShareAndTrash` |
| Keyboard bootstrap/sign-in, visible focus, labeled controls, and focus-restoring native dialogs | `TestE2EBrowserBootstrapLoginDriveShareAndTrash`; embedded source assertions |
| Direct resumable upload with bounded 1–8 concurrency, provider-confirmed offsets, retry, cancellation, multi-file, folder fallback, and conflict policy | `TestE2EBrowserBootstrapLoginDriveShareAndTrash`; `TestBrowserSourceKeepsSecretsEphemeralAndUntrustedTextOutOfHTML`; provider contract fault suite |
| Download initiation, safe preview modes, public share creation, trash and restore | `TestE2EBrowserBootstrapLoginDriveShareAndTrash`; file/share integration suite |
| Responsive 320 CSS-pixel layout and reduced-motion handling | Chromium viewport assertion in `TestE2EBrowserBootstrapLoginDriveShareAndTrash`; `TestApplicationShellExposesCompleteAccessibleWorkspaces` |
| No external runtime origin and no browser persistence of tokens or capabilities | Chromium request-origin assertion; `TestBrowserSourceKeepsSecretsEphemeralAndUntrustedTextOutOfHTML` |
| Selected validated theme stylesheet in the initial HTML response, with safe fallback | `TestIntegrationThemeHTTPMetadataPreferenceAssetsAndSafeFallback`, `TestThemeResolverCanOnlyInjectValidatedSameOriginStylesheet` |
| Linux CI uses Nix-pinned Chromium; macOS contributors use the same Go driver with installed Chrome | `nix run .#test-e2e`; Linux `checks.e2e` derivation |

The browser source creates every untrusted filename and display name through text nodes, keeps bearer material in short-lived closures, removes capability-bearing preview DOM on close, extracts invite/recovery tokens before the first request, and records no sensitive client-side storage.

## Sharing and administration browser workflows — complete

The Milestone 6 checkpoint completes the browser workflow inventory in specification section 18.2 and the remaining user-facing portions of checklist sections 22.3, 22.4, 22.7, and 22.9:

| Requirement | Automated evidence |
|---|---|
| Admin creates a one-use invite and a second virtual authenticator registers without a separate identity field | `TestE2EInviteSettingsAdminRecoveryAndShareRevocation` |
| Profile rename, light/dark preference, add-passkey, passkey listing, and non-final removal work in the embedded settings view | `TestE2EInviteSettingsAdminRecoveryAndShareRevocation`; `TestApplicationShellExposesCompleteAccessibleWorkspaces` |
| Keyboard-driven create, same-folder copy with generated-name conflict resolution, move, and nested browsing work | browser workflow plus provider-contract `same-path renamed Copy` regression |
| Owner share listing exposes safe metadata and supports confirmed revocation | `TestE2EInviteSettingsAdminRecoveryAndShareRevocation`; settings shell assertions |
| Administrator user listing and disable/enable actions immediately gate the owner's public share | `TestE2EInviteSettingsAdminRecoveryAndShareRevocation` |
| Administrator recovery adds a passkey to the same account, revokes the prior session, and supports a fresh usernameless sign-in | `TestE2EInviteSettingsAdminRecoveryAndShareRevocation` |
| Public access remains generically unavailable after share revocation | `TestE2EInviteSettingsAdminRecoveryAndShareRevocation` |
| Both built-in appearance paths execute complete functional browser workflows without external requests | two Chromium workflows, one exercising the light/system path and one selecting dark; request-origin assertions; `nix run .#test-e2e -- -count=2` |

Browser network diagnostics retain only request methods and paths: credential ceremony bodies, bearer values, provider capability query strings, and authorization fields are deliberately excluded.

## Adversarial hardening and release proof — complete

| Requirement | Automated or review evidence |
|---|---|
| Every private file/share/trash/operation family rejects another owner's paths, versions, cursor, upload/trash/share/operation IDs, and mutation intent | `TestIntegrationCrossUserPrivateEndpointMatrix`, `TestIntegrationDirectTransfersAndIsolation`, `TestIntegrationCopyMoveTrashRestoreAndDelete` |
| Raw, encoded, double-encoded, Unicode-normalized, slash/backslash, dot, NUL/control, reserved, and overlong paths fail before provider access | `TestReservedNamespaceAndEncodingCorpusFailsBeforeProviderAccess`, `TestFileHTTPRejectsProviderFieldsBodiesAndTraversalBeforeProvider`, `FuzzParseUserPath`, `FuzzParseUserPathEncodingBoundary` |
| Structured logs remain secret-safe at every configured level and request logs use route templates | `TestJSONLoggerRedactsSensitiveFieldsAtEveryLevel`, `TestJSONLoggerHonorsConfiguredLevelAndKeepsSafeFields`, `TestRequestLoggingUsesRouteTemplatesAndOmitsSensitiveMaterial`, `FuzzStructuredLogRedaction` |
| Every required parser/security boundary has a bounded fuzz target | `nix run .#test-fuzz`: configuration/origin, path, percent encoding, JSON records, cursors, WebAuthn responses, share subtrees, ranges/dispositions, logging, and the complete theme boundary |
| Race-sensitive identity, state, provider, transfer, trash, idempotency, and final-admin behavior is clean | `nix run .#test-race`; explicit concurrent tests and both reusable contracts |
| Threats, runtime behavior, shutdown, ephemeral state, and absence of a v1 backup claim are reviewed | [Implemented threat model](./threat-model.md), [operations guide](./operations.md), and `TestRunStartsAndGracefullyStopsCompleteApplication` |
| Static, vulnerability, dependency-license, source, configuration, and OCI policies pass with pinned inputs | `nix run .#security`, `nix run .#dependency-check`, `checks.security`, `checks.dependencies`, `checks.container-policy` |
| Release output is self-describing and verifiable | `packages.release` emits the release and OCI archives, `SHA256SUMS`, `RELEASE-INVENTORY.txt`, locked module/license inventories, `THEMES.json`, release notes, and this evidence record |

### Coverage result

The release coverage command is `nix run .#test-coverage`. It executes all Go packages plus real Chromium E2E coverage, de-duplicates the shared `-coverpkg=./...` profile, and fails below any threshold.

| Boundary | Statements | Result | Required |
|---|---:|---:|---:|
| Repository | 4,418 / 5,197 | 85.011% | 85% |
| Authentication | 154 / 161 | 95.652% | 95% |
| Authorization | 419 / 438 | 95.662% | 95% |
| Canonical path | 204 / 209 | 97.608% | 95% |
| Bearer token | 20 / 21 | 95.238% | 95% |
| Provider capability | 701 / 734 | 95.504% | 95% |
| State CAS | 204 / 209 | 97.608% | 95% |
| Scope mapping | 701 / 734 | 95.504% | 95% |
| Theme validation/sanitization | 650 / 684 | 95.029% | 95% |
| Configuration | 142 / 149 | 95.302% | 95% |

### Acceptance-criterion index

| Criterion | Evidence |
|---|---|
| AC-001 | `checks.build`, `checks.container`, `checks.release`, and the clean umbrella gate build without credentials/services. |
| AC-002 | Nix-sandboxed `checks.offline` and all test derivations use the fixed-output module closure and explicit loopback listeners only. |
| AC-003 | One `cmd/endlessfs` entry point, embedded `internal/web`, `tools/check-source`, and OCI inspection; no Node/runtime frontend toolchain. |
| AC-004 | `tools/check-source`, dependency inventory, runtime assembly tests, and the implemented threat review prove the prohibited services/identity/telemetry are absent. |
| AC-005 | Provider-neutral domain interfaces, import boundaries, and source policy; GCS appears only in documentation. |
| AC-006 | `checks.container-policy` inspects user, entry point, ports, volumes, and every layer path for shells, package managers, source, or credential-shaped material. |
| AC-007 | Theme schema/archive/media/SVG negative matrices plus source policy reject executable/raw/remote theme inputs. |
| AC-008 | Built-in/custom compiler tests, overridden custom-build smoke proof, and `THEMES.json` inventory all embedded bundles. |
| AC-010 | `TestIntegrationConcurrentBootstrapCreatesExactlyOneAdmin`. |
| AC-011 | Concurrent, invalid, absent, and replay bootstrap matrix in the identity integration suite. |
| AC-012 | `TestIntegrationRegistrationPolicyMatrixAndVerificationRecheck`. |
| AC-013 | `TestIntegrationInviteIsHashedSingleUseAndConcurrent` and admin HTTP/E2E workflows. |
| AC-014 | Identity API/browser registration models accept only display name and passkey ceremony material. |
| AC-015 | Identity-surface source scan plus strict request/profile codecs and browser assertions. |
| AC-016 | Opaque-ID tests, duplicate-name registration, and server-only user creation. |
| AC-017 | Real WebAuthn positive flow and complete origin/RP/challenge/handle/signature/owner/replay/expiry negative matrices. |
| AC-018 | Multi-passkey integration and browser settings workflow, including final-passkey denial. |
| AC-019 | `TestIntegrationSessionsCSRFRotationExpiryAndDisabledAccount` and owner-share disable/enable E2E. |
| AC-020 | `TestIntegrationConcurrentAdminChangesPreserveEnabledAdministrator`. |
| AC-021 | `TestIntegrationRecoveryAddsPasskeyPreservesIdentityAndRevokesSessions` and recovery E2E. |
| AC-022 | `TestDisplayNameAndPasskeyLabelChangesDoNotAlterIdentityOrRoles`. |
| AC-030 | `TestIntegrationCrossUserPrivateEndpointMatrix` plus service/provider isolation matrices. |
| AC-031 | Reserved/encoding no-provider-call corpus and both path fuzz targets. |
| AC-032 | Provider contract listing/pagination plus Drive HTTP/E2E browse workflows. |
| AC-033 | Provider recursive-operation contract, batch/idempotency integrations, and copy/move E2E. |
| AC-034 | Trash service/HTTP/browser suites cover restore conflict, generated rename, permanent delete, and bounded empty. |
| AC-035 | Share move/trash invalidation integrations. |
| AC-040 | Provider capability contract binds method, scope, path, headers, and expiry. |
| AC-041 | Separate mock listener and control-byte instrumentation in HTTP integration and Chromium E2E. |
| AC-042 | Provider resumable interruption/confirmed-offset/retry/completion contract and browser workflow. |
| AC-043 | Provider fault/checksum/size/cancel/expiry matrices plus explicit browser retry/error/cancel states. |
| AC-044 | Browser multi-file/folder queue and public max-init/concurrency policy; service batch bounds. |
| AC-045 | Provider contract simulates greater-than-1-TiB logical size/offset without equivalent allocation. |
| AC-046 | Exact-object download/preview capabilities and separate data-plane byte flow integration. |
| AC-047 | Provider expiry/range/disposition contract and preview/download browser workflows. |
| AC-048 | No-store HTTP assertions, request-log test, log fuzzing, and browser-storage source/runtime assertions. |
| AC-050 | Share service/HTTP/E2E create/list/expire/revoke coverage; raw token appears only at creation. |
| AC-051 | Share-subtree integration corpus and `FuzzShareSubtreeResolution`. |
| AC-052 | Public failure matrix for absent/invalid/expired/revoked/disabled/moved records. |
| AC-053 | Public router exposes only confined listing/stat/download; method and subtree denial tests. |
| AC-054 | Signature-validated preview allowlist tests for raster/text/PDF and active/unknown attachments. |
| AC-055 | Text-node-only browser source assertions, hostile-value tests, CSP, and Chromium execution diagnostics. |
| AC-056 | Both built-ins use the ordinary compiler/conformance pipeline and run complete Chromium workflows. |
| AC-057 | Minimal/older inheritance, selection, media, emergency, and reset fallback tests. |
| AC-058 | Archive/manifest/media/SVG hostile matrices, fuzz target, build-input rejection, and browser origin capture. |
| AC-059 | Preference persistence/resolution HTTP tests and both appearance-path E2E workflows. |
| AC-060 | `TestE2EBrowserBootstrapLoginDriveShareAndTrash` and `TestE2EInviteSettingsAdminRecoveryAndShareRevocation`. |
| AC-061 | Keyboard-driven Chromium paths at desktop/320 pixels plus focus, labels, live-region, reduced-motion, and responsive assertions. |
| AC-062 | Chromium network capture permits only the control origin and returned loopback data origin. |
| AC-063 | Header/cookie/origin/CSRF/strict-body/page/batch/rate/expiry tests cover valid and denied paths. |
| AC-064 | Structured redaction unit/fuzz tests and route-template request-log integration. |
| AC-065 | Nix race gate, explicit concurrency tests, deterministic provider faults, and leak-free E2E cleanup. |
| AC-070 | The clean `nix flake check` umbrella contains build, format, lint, all test layers, race, fuzz, coverage, dependency, security, policy, offline, OCI, and release checks. |
| AC-071 | Enforced statement results are recorded in the coverage table above. |
| AC-072 | Confirmed fixes for same-folder generated-name copy, duplicate-trash prevalidation, CR/LF-safe disposition fallback, reverse-domain theme IDs, and request status recording each landed with regression coverage. |
| AC-073 | The release output records source/input/artifact hashes, locked dependencies/licenses, check/coverage results, themes, and limitations. |
| AC-074 | README, release notes, operations, threat model, inventory, and this record distinguish feature-complete mock v1 from GCS/production readiness. |

### Release record contract

`nix build .#release` derives every record from the exact Git source revision. `RELEASE-INVENTORY.txt` contains the source revision, `flake.lock` SHA-256, pinned vulnerability-database NAR hash, target, Go version, binary/OCI/theme/dependency/license hashes, thresholds, and explicit mock/no-GCS/no-deployment/no-credentials/no-external-services fields. `SHA256SUMS` covers every separately published file. The archive also contains this evidence, release notes, README, license, binary, and all inventories.

The build/test boundary used no GCP credential, cloud service, database, external identity provider, container daemon, persistent service, deployment permission, or non-loopback application dependency. The current provider/state implementations are deliberately ephemeral; see [v1 release notes](./v1-release-notes.md).
