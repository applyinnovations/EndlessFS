# EndlessFS v1 implementation evidence

This is the release evidence index for the acceptance criteria and feature-completion checklist in [the v1 specification](./v1-specification.md). It records the provider-portable, multi-replica implementation and credential-free local qualification of the GCS adapter. It does not claim live GCS interoperability, production durability, deployment readiness, or production operations validation.

## v1.1 media browsing and generated image previews — complete

The [v1.1 extension specification](./v1.1-media-preview-specification.md) is implemented with the portable engine as its authoritative source and an independently faultable preview store as its artifact boundary. The deterministic memory store and durable shared store implement the same contract; the durable semantics run over the existing thin object-store interface and are exercised through the credential-free GCS protocol fake. The virtualized grid, metadata filters, full-screen viewer, and file-type icons are always available. Image thumbnail artifacts are optional static WebP only. Original files remain authoritative and ordinary file operations remain available when previews are disabled or unavailable. This proves the durable GCS integration path locally, not live-cloud interoperability or a production deployment. Video and PDF generation remain deferred to the separate v1.2 and v1.3 specifications.

| Requirement | Automated evidence |
|---|---|
| Independent, default-off browser/store configuration; unpackaged generators and inaccessible configured stores fail startup | `TestParsePreviewConfiguration`, `TestParseRejectsInvalidPreviewConfiguration`, `TestNewServiceFailsFastForGeneratorAndStoreMisconfiguration`, and `TestRunValidatesConfiguredPreviewDependenciesBeforeServing` |
| Non-public move-stable identity is canonical across replicas and raw-copy cutovers, is preserved by rename/move/trash/restore, and cannot alias copy/replacement artifacts | shared provider contract `preview content identity lifecycle` over portable memory and local GCS; `TestPortabilityRawCopyPreservesCompleteStateAndContinuesInBothDirections`; `TestResolveGeneratesOnceAndRenameReusesArtifact`; `TestResolveCopyAndReplacementRequireDistinctArtifacts` |
| Automatic recency and source-size policies are independent, exact at their boundaries, and read zero excluded source bytes; explicit generation bypasses those policies | `TestResolveAutomaticPolicyExclusionsReadNoOriginalBytes` and `TestResolveAutomaticPolicyIncludesExactAgeAndSizeBoundaries` |
| Closed PNG/JPEG/GIF/WebP plus DNG/CR2/CR3/RAF/NEF/ORF/RW2/PEF/ARW input set produces bounded, metadata-free, source-aspect-preserving static WebP variants through portable memory and locally qualified GCS source capabilities | image-generator positive/negative matrix beginning with `TestGeneratorProducesStaticWebPAtSourceAspectRatio` and `TestWorkerGeneratorDecodesClosedCameraRAWFormatsAndRejectsMismatchedBytes`; LibRaw startup self-test; strict PPM boundary matrix; `TestIntegrationGeneratedPreviewReadsPortableGCSSource`; every-orientation coverage; and `FuzzGeneratorMalformed` |
| Artifact identity, validation, immutable generations, exact capabilities, expiry, corruption denial, runtime removal, and revalidation | reusable preview-store contract via `TestContractMemoryPreviewStore`, `TestContractDurablePreviewStoreOverObjectBackend`, and `TestContractDurablePreviewStoreOverGCSProtocolFake`; `TestVerifyUsesProviderIntegrityMetadataWithoutReadingObjectBytes`; browser SHA-256 denial workflow; durable boundary, corruption, lifecycle, and startup-validation matrices |
| Multi-replica preview claim fencing, one-winner takeover, stale-worker denial, ambiguous provider-success recovery, and opaque provider-neutral layout | `TestDurablePreviewClaimsFenceAcrossReplicas`, `TestDurableStoreLostSuccessAndImmutableRecovery`, `TestDurableConditionalWriteFailureMatrix`, and `TestDurableStoreCorruptionLifecycleAndOpaqueLayout` |
| Authenticated exact-version resolve, explicit generate/regenerate, CSRF/origin enforcement, owner isolation, runtime health failure, safe logging, and unchanged file availability | `TestIntegrationGeneratedPreviewResolveRegenerateAndAuthorization`, `TestPreviewHTTPRejectsUnexpectedFieldsAndStaleVersions`, and `TestIntegrationPreviewRuntimeLossFailsReadinessButNotFileListing` |
| Lazy virtual grid, square frames with uncropped source-ratio WebP, full-screen viewer, keyboard navigation, 320-pixel dark-theme operation, and bounded 10,002-entry DOM/request behavior | `TestE2EBrowserBootstrapLoginDriveShareAndTrash`, `TestE2EInviteSettingsAdminRecoveryAndShareRevocation`, and embedded-browser source assertions |
| Profile-selectable release contract and identical configuration schema | `.#container-images` and `.#release-images`; release `CAPABILITIES.json`, dependency/license inventories, recipe/dependency digests, OCI sizes, and explicit memory/durable-GCS local-qualification inventory |

The focused gate is `nix run .#test-preview`. The final acceptance run used `nix flake check --print-build-logs`; it passed the full build, format, lint, unit, contract, integration, preview, theme, race, fuzz, offline, dependency, security, OCI, and release set without cloud credentials or external services. The real Chromium workflows passed through `nix run .#test-e2e`.

## Reproducible foundation — implemented baseline

- The one-binary Go module, embedded browser shell, pinned Nix environment,
  minimal OCI image, release artifacts, bootstrap workflows, and repository
  rulesets were introduced in commit `8c04cbb`. The bootstrap workflows were
  retired after the xlab pull-request, merge-queue, Chromium, cache-placement,
  and GHCR proof recorded in `.tekton/README.md`. The active workflow contract is
  expressed as xlab Tekton PaC definitions validated by
  `nix run .#pipeline-policy`; the Darwin smoke path is explicitly retired.
- `tools/check-source` rejects forbidden languages, dependency managers, task runners, and external browser resources.
- `internal/config` validates the implemented environment contract and provides `FuzzParse`.
- `internal/httpapi` tests health, readiness, public configuration, route boundaries, payload limits, and security headers.
- `nix flake check`, multi-system flake evaluation, OCI inspection, release checksums, and a local server smoke test form the bootstrap evidence.

## Domain, provider, and state — implemented baseline

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

## Portable format, replicas, and GCS adapter

The v1 portability clarification is implemented without a second application provider implementation:

| Requirement | Automated evidence |
|---|---|
| Canonical keys, envelopes, logical versions, manifests/pages, writer/gate/admission/operation/idempotency/checkpoint schemas | `TestCanonicalEnvelopeAndLogicalVersion`, `TestCanonicalKeyLayoutAndBounds`, `TestStrictEnvelopeRejectsCorruption`, portable filesystem/operation suites |
| One portable state/provider engine over atomic object backends | `TestContractPortableStateStore`, `TestContractPortableProviderOverMemoryBackend`, `TestContractPortableProviderOverGCSProtocolFake` |
| Optional file backend isolates immutable blobs/staging while preserving one state gate, cross-replica reads, direct transfers, combined checkpoints, and continued mutation after raw-copy cutover | `TestPortableSeparateFileBackendIsolatesBytesAndSharesOneCheckpoint`, `TestPortabilityRawCopyPreservesSplitStateAndFileBackends`, `TestParseGCSAllowsSeparateOrSharedStateBucket` |
| Stable encrypted state pagination across replicas and concurrent CAS | `TestPortableStateCursorMovesAcrossReplicasAndKeepsImmutableSnapshot`, `TestPortableStateCASAcrossReplicas`, `TestEightReplicaConcurrentCASHasOneWinner` |
| Candidate admission barrier, node-loss recovery, and writer compatibility | `TestCandidateCannotAdmitAfterGateStartsClosing`, `TestReplicaDropAfterAdmissionIsFencedRecoveredAndClosed`, `TestReplicaCompatibilityRejectsWriterConfigurationDrift` |
| Immutable multi-root preparation, one commit point, takeover fencing, and crash recovery | `TestReplicaDropAfterRootPrepareRecoversAtOneCommitPoint`, `TestSupersededReplicaCannotCommitWithTakeoverFence`, `TestReplicaDropAfterCommitOrFinalizationRecoversPostCommitView` |
| Persisted recursive-byte aggregates at every directory and area root, with constant-metadata-read lookup, upload/replacement/move/copy/trash/restore/delete maintenance, pre/post-commit visibility, overflow/corruption denial, and raw-copy continuation | `TestPortableRecursiveByteAggregatesTrackEveryFileMutation`, `TestPortableRecursiveAggregateStatDoesNotReadManifestPages`, `TestEightReplicaConcurrentMultiFileCompletionConvergesRecursiveAggregates`, `TestEightReplicaSameUploadCompletionIsIdempotentAndAggregatedOnce`, `TestEightReplicaSameTargetUploadRacesHaveOneAggregateWinner`, `TestFailedPartialAbortedAndReplayedUploadsDoNotSkewAggregates`, `TestConcurrentReplicaUploadCompletionAndAbortNeverSkewAggregate`, `TestConcurrentReplicaFolderMutationsKeepRecursiveAggregatesAtomic`, `TestFolderMutationsRecoverAtEveryAggregateCommitBoundary`, aggregate assertions in `TestReplicaDropAfterRootPrepareRecoversAtOneCommitPoint` and `TestPortabilityRawCopyPreservesCompleteStateAndContinuesInBothDirections`, `TestPortableDirectoryManifestCorruptionMatrixFailsClosed`, `TestPortableDirectoryEntryValidationMatrix` |
| Automatic pre-aggregate bucket migration, old-writer fencing, split-backend support, crash resumption, concurrent starters, upload drain, and corrupt-graph denial | `TestStartupAutomaticallyMigratesLegacyRecursiveByteAggregates`, `TestStartupAutomaticallyMigratesLegacySplitBackend`, `TestStartupRecursiveByteMigrationResumesAfterEveryDurableBoundary`, `TestEightReplicasConcurrentlyMigrateOneLegacyAggregateTree`, `TestStartupRecursiveByteMigrationWaitsForAndDrainsActiveUpload`, `TestStartupRecursiveByteMigrationRejectsCorruptLegacyTrees` |
| Cross-replica upload idempotency, ancestor-root contention retry, lost-success reconciliation, capability drain, and resumability | `TestPortableUploadInitiationIsIdempotentAcrossReplicas`, `TestConcurrentReplicaUploadInitiationHasOneIdempotentOutcome`, `TestEightReplicaConcurrentMultiFileCompletionConvergesRecursiveAggregates`, `TestEightReplicaSameUploadCompletionIsIdempotentAndAggregatedOnce`, `TestUploadCompletionLostSuccessIsIdempotentlyReconciled`, `TestCheckpointWaitsForActiveCapabilityThenDrainsItAfterExpiry` |
| Authoritative-only raw copy, native-version replacement, read-only verification, reopen, and continued mutation both ways | `TestPortabilityRawCopyPreservesCompleteStateAndContinuesInBothDirections`, `TestCheckpointVerifierIsStrictlyReadOnly`, `TestCheckpointVerifierRejectsMissingExtraAndUnsupportedState`, `TestCheckpointPrunesExpiredStateSnapshotsButKeepsCurrentVersions` |
| GCS generations, conditional mutation, checksums, pagination, errors, disconnect/lost-success, and full backend contract | `TestContractGCSProtocol`, `TestGenerationConditionsFenceEveryMutation`, `TestChecksumsSizesListingsAndCursorsFailClosed`, `TestLostUploadSuccessIsUnavailableAndNotRetried`, `TestProtocolErrorsMapToStableSafeKinds` |
| GCS direct V4/resumable capabilities, replica handoff, finalized-versus-incomplete cleanup, generation binding, exact-origin CORS, and keyless construction | `TestGCSResumableCapabilityCanMoveBetweenReplicas`, `TestGCSUploadCleanupDistinguishesFinalizedAndIncompleteSessions`, `TestGCSSignedSingleUploadAndDownloadAreGenerationBound`, `TestGCSCORSRequiresExactApplicationOriginAndTransferHeaders`, `TestWorkloadIdentityTransferConstructionRequiresNoPrivateKeyOrNetwork` |
| Read-only operator verification through Nix, including split state/file fixtures | `TestRunVerifiesLocalRawCopyFixtureWithoutWritingIt`, `TestRunVerifiesSeparateStateAndFileFixtures`, `TestRunRejectsMalformedFixtureAndUnknownConfiguration`; `nix run .#provider-verify -- check CONFIG` |

The deterministic GCS server is loopback-only and uses an injected unauthenticated official client plus deterministic signer. No test contacts a metadata, token, IAM, or storage endpoint. `nix run .#test-replica` and `nix run .#test-portability` make the two clarified gates independently visible.

## Identity and registration — implemented baseline

The Milestone 2 checkpoint implements specification sections 10, 12.3, and 12.4 plus the identity checklist sections:

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

## File, transfer, trash, preview, and share control plane — implemented baseline

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

## Data-only theme system — implemented baseline

The Milestone 4 checkpoint implements specification section 14, acceptance criteria AC-056–AC-059 at the compiler/control-plane layer, and checklist 22.8 except the browser workflows that intentionally land in Milestone 5:

| Requirement | Automated evidence |
|---|---|
| Closed versioned registry with typed serializers, bounds, defaults, CSS mapping, contrast pairs, fonts, and complete semantic media slots | `TestThemeTokensAreClosedTypedBoundedAndContrastChecked`; generated `tools/theme api`; [Theme API 1.1](./theme-api.md) |
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

## Browser Drive and accessibility — implemented baseline

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
| Linux CI uses Nix-pinned Chromium; macOS contributors use the same Go driver with installed Chrome | `nix run .#test-e2e`; CI's host-side Nix browser/coverage gate |

The browser source creates every untrusted filename and display name through text nodes, keeps bearer material in short-lived closures, removes capability-bearing preview DOM on close, extracts invite/recovery tokens before the first request, and records no sensitive client-side storage.

## Sharing and administration browser workflows — implemented baseline

The Milestone 6 checkpoint completes the browser workflow inventory in specification section 18.2 and the remaining user-facing checklist portions:

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

## Adversarial hardening and release proof — implemented baseline

| Requirement | Automated or review evidence |
|---|---|
| Every private file/share/trash/operation family rejects another owner's paths, versions, cursor, upload/trash/share/operation IDs, and mutation intent | `TestIntegrationCrossUserPrivateEndpointMatrix`, `TestIntegrationDirectTransfersAndIsolation`, `TestIntegrationCopyMoveTrashRestoreAndDelete` |
| Raw, encoded, double-encoded, Unicode-normalized, slash/backslash, dot, NUL/control, reserved, and overlong paths fail before provider access | `TestReservedNamespaceAndEncodingCorpusFailsBeforeProviderAccess`, `TestFileHTTPRejectsProviderFieldsBodiesAndTraversalBeforeProvider`, `FuzzParseUserPath`, `FuzzParseUserPathEncodingBoundary` |
| Structured logs remain secret-safe at every configured level and request logs use route templates | `TestJSONLoggerRedactsSensitiveFieldsAtEveryLevel`, `TestJSONLoggerHonorsConfiguredLevelAndKeepsSafeFields`, `TestRequestLoggingUsesRouteTemplatesAndOmitsSensitiveMaterial`, `FuzzStructuredLogRedaction` |
| Every required parser/security boundary has a bounded fuzz target | `nix run .#test-fuzz`: fixed 1,000-input CI campaigns for configuration/origin, path, percent encoding, JSON records, cursors, WebAuthn responses, share subtrees, ranges/dispositions, logging, the complete theme boundary, and malformed image-preview inputs; `TestCheckRejectsWallClockFuzzSmokeBudgets` prevents timing-dependent defaults |
| Race-sensitive identity, state, provider, transfer, trash, idempotency, and final-admin behavior is clean | `nix run .#test-race`; explicit concurrent tests and both reusable contracts |
| Threats, runtime behavior, shutdown, ephemeral state, and absence of a v1 backup claim are reviewed | [Implemented threat model](./threat-model.md), [operations guide](./operations.md), and `TestRunStartsAndGracefullyStopsCompleteApplication` |
| Static, vulnerability, dependency-license, source, configuration, and OCI policies pass with pinned inputs | `nix run .#security`, `nix run .#dependency-check`, `checks.security`, `checks.dependencies`, `checks.container-policy` |
| Release output is self-describing and verifiable | `packages.release` emits the release and OCI archives, `SHA256SUMS`, `RELEASE-INVENTORY.txt`, locked module/license inventories, `THEMES.json`, release notes, and this evidence record |

### Coverage result

The release coverage command is `nix run .#test-coverage`. CI first runs the cacheable `nix flake check` umbrella, whose overlapping named test-layer checks resolve to one derivation, then executes the real Chromium E2E and all-package coverage gate through a Nix app outside the build sandbox. The app consumes the fixed-output vendored module closure, performs no module downloads, and fails below any threshold.

| Boundary | Statements | Result | Required |
|---|---:|---:|---:|
| Repository | 9,958 / 11,101 | 89.704% | 85% |
| Authentication | 154 / 161 | 95.652% | 95% |
| Authorization | 419 / 438 | 95.662% | 95% |
| Canonical path | 207 / 212 | 97.642% | 95% |
| Bearer token | 20 / 21 | 95.238% | 95% |
| Provider capability | 930 / 975 | 95.385% | 95% |
| State CAS | 420 / 438 | 95.890% | 95% |
| Scope mapping | 2,260 / 2,374 | 95.198% | 95% |
| Canonical format/key/version/checkpoint | 309 / 319 | 96.865% | 95% |
| Write gate/admission | 421 / 443 | 95.034% | 95% |
| Operation fencing/recovery | 787 / 828 | 95.048% | 95% |
| Directory manifest | 438 / 457 | 95.842% | 95% |
| GCS transport | 384 / 404 | 95.050% | 95% |
| Theme validation/sanitization | 650 / 684 | 95.029% | 95% |
| Configuration | 290 / 294 | 98.639% | 95% |
| Preview core | 573 / 602 | 95.183% | 95% |
| Preview image generator | 467 / 490 | 95.306% | 95% |
| Preview store | 616 / 643 | 95.801% | 95% |

### Acceptance-criterion index

| Criterion | Evidence |
|---|---|
| AC-001 | `checks.build`, `checks.container`, `checks.release`, and the clean umbrella gate build without credentials/services. |
| AC-002 | Nix-sandboxed `checks.offline` and all test derivations use the fixed-output module closure and explicit loopback listeners only. |
| AC-003 | One `cmd/endlessfs` entry point, embedded `internal/web`, `tools/check-source`, and OCI inspection; no Node/runtime frontend toolchain. |
| AC-004 | `tools/check-source`, dependency inventory, runtime assembly tests, and the implemented threat review prove the prohibited services/identity/telemetry are absent. |
| AC-005 | Provider-neutral domain interfaces and source-policy scans; only `internal/objectstore/gcs` imports the GCS SDK and adapter tests run the same portable contracts. |
| AC-006 | `checks.container-policy` inspects user, entry point, ports, volumes, and every layer path for shells, package managers, source, or credential-shaped material. |
| AC-007 | Theme schema/archive/media/SVG negative matrices plus source policy reject executable/raw/remote theme inputs. |
| AC-008 | Built-in/custom compiler tests, overridden custom-build smoke proof, and `THEMES.json` inventory all embedded bundles. |
| AC-009 | `internal/portable` is the sole application-facing `StorageProvider`/`StateStore` implementation over `internal/objectstore`; memory and GCS execute the shared contract suites through it. |
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
| AC-023 | Canonical format golden/key/bounds tests plus strict writer, gate, admission, state-version, directory, operation, idempotency, and checkpoint codecs. |
| AC-024 | Canonical path boundary tests, bounded directory-ID key construction, and stored name-digest collision/corruption denial in state/filesystem reads. |
| AC-025 | Canonical schema/source inspection and authoritative-only checkpoint tests; native versions remain transport values and encrypted GCS session URLs occur only in excluded leases. |
| AC-026 | `TestPortabilityRawCopyPreservesCompleteStateAndContinuesInBothDirections` copies checkpoint-authorized bodies into independently versioned backends. |
| AC-027 | The raw-copy test preserves logical versions/state CAS and continues state plus filesystem mutation after destination and reverse reopen. |
| AC-028 | `TestCheckpointVerifierRejectsMissingExtraAndUnsupportedState`, `TestCheckpointDetectsAuthoritativeCorruption`, and strict envelope/collision tests. |
| AC-029 | Gate/admission race, crashed-operation recovery, active-capability drain, pending-root recovery, authoritative inventory, and read-only verifier tests. |
| AC-030 | `TestIntegrationCrossUserPrivateEndpointMatrix` plus service/provider isolation matrices. |
| AC-031 | Reserved/encoding no-provider-call corpus and both path fuzz targets. |
| AC-032 | Provider contract listing/pagination plus Drive HTTP/E2E browse workflows. |
| AC-033 | Provider recursive-operation contract, batch/idempotency integrations, and copy/move E2E. |
| AC-034 | Trash service/HTTP/browser suites cover restore conflict, generated rename, permanent delete, and bounded empty. |
| AC-035 | Share move/trash invalidation integrations. |
| AC-036 | Recursive-byte lifecycle, operation pre/post visibility, corruption/overflow denial, and raw-copy continuation assertions in the portable filesystem, operation, and checkpoint suites. |
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
| AC-070 | The clean `nix flake check` umbrella contains build, format, lint, all non-browser test layers, E2E/coverage compilation, race, fuzz, dependency, security, policy, offline, OCI, and release checks; CI then runs real Chromium E2E and enforced coverage through `nix run .#test-coverage`. |
| AC-071 | Enforced statement results are recorded in the coverage table above. |
| AC-072 | Confirmed fixes for same-folder generated-name copy, duplicate-trash prevalidation, CR/LF-safe disposition fallback, reverse-domain theme IDs, request status recording, and timing-dependent fuzz smoke budgets each landed with regression coverage. |
| AC-073 | The release output records source/input/artifact hashes, locked dependencies/licenses, check/coverage results, themes, and limitations. |
| AC-074 | README, release notes, operations, threat model, inventory, and this record distinguish provider-portable local qualification from live GCS interoperability and production readiness. |
| AC-080 | Independent two-replica state/filesystem/operation/upload tests plus `TestEightReplicaConcurrentCASHasOneWinner`; no engine coordination is process-local. |
| AC-081 | `TestCandidateCannotAdmitAfterGateStartsClosing` and `TestReplicaDropAfterAdmissionIsFencedRecoveredAndClosed`. |
| AC-082 | Prepared-operation and admitted-state crash tests advance a fixed clock and recover through one CAS takeover. |
| AC-083 | `TestSupersededReplicaCannotCommitWithTakeoverFence` deterministically resumes the stale owner while takeover owns the higher fence. |
| AC-084 | Recursive operation, pending-root pre/post visibility, commit/finalization recovery, and shared-replica directory tests. |
| AC-085 | Portable direct upload, concurrent initiation, lost-success completion, expiry/abort, and checkpoint-drain tests. |
| AC-086 | `TestReplicaCompatibilityRejectsWriterConfigurationDrift` covers writer set, security configuration, keyring, and feature mismatch. |
| AC-087 | Crashed admitted state/operation and active-capability tests require safe recovery/drain before checkpoint closure. |
| AC-088 | The shared atomic backend contract runs against memory and the protocol-level GCS adapter; generation and lost-success tests enforce the linearization boundary. |

### v1.1 acceptance-criterion index

| Criterion | Evidence |
|---|---|
| MP-001 | Default configuration tests, disabled-service provider-call instrumentation, and the complete unchanged v1 regression suite. |
| MP-002 | Browser/config regression assertions prove grid/filter/viewer availability has no feature flag; `TestE2EMediaBrowserIsAvailableWithoutGeneratedPreviews` proves the icon-only keyboard path and zero preview requests with the provider disabled. |
| MP-003 | Preview-store contract, runtime loss/revalidation tests, readiness integration, and successful ordinary file listing during preview loss. |
| MP-004 | Automatic policy exclusion and exact-boundary tests, including source-byte instrumentation. |
| MP-005 | Explicit generation after age exclusion, regenerate HTTP/browser workflows, and hard-limit/authorization negatives. |
| MP-006 | Provider identity lifecycle contract, binding validation, cross-owner HTTP denial, and exact-version capability issuance. |
| MP-007 | Real generator format/aspect/orientation/metadata/animation/resource matrix and malformed-input fuzz target. |
| MP-008 | Chromium 10,002-entry proof: at most 64 rendered tiles and at most 32 new resolve requests, with square-frame and 96×48 intrinsic-image assertions. |
| MP-009 | Keyboard viewer workflows in the default desktop path and dark 320-pixel path; semantic controls and focus restoration in embedded-browser tests. |
| MP-010 | `.#container-images`, `.#release-images`, OCI policy, `CAPABILITIES.json`, dependency inventories, and the full offline flake gate. |
| MP-011 | Configuration, service assembly, store access-probe, and process-startup fail-fast tests with sanitized errors. `TestDurableStartupAcceptsEffectiveNoStoreCachePolicy`, `TestHasNoStoreAcceptsEffectivePolicies`, and the corresponding malformed-policy denial matrix cover provider-equivalent Cache-Control formatting without weakening the required `no-store` directive. |
| MP-012 | Provider lifecycle contract plus rename reuse and copy/replacement distinct-generation service tests. |
| MP-013 | Store/service/HTTP negative matrices, preview CSP and origin tests, safe runtime-loss logging, fuzz, race, and security gates. |
| MP-014 | Loaded-metadata browser filters, no search API/index, and documentation that preserves search as a future feature. |
| MP-015 | README, operations guide, threat model, release notes, capability inventory, and release inventory distinguish deterministic memory proof, local GCS source/preview protocol qualification, and absent live/production validation. |

### Release record contract

`nix build .#release` derives every record from the exact Git source revision. `RELEASE-INVENTORY.txt` contains the source revision, `flake.lock` SHA-256, pinned vulnerability-database NAR hash, target, Go version, binary/OCI/theme/dependency/license hashes, thresholds, canonical format/writer protocol, supported local provider modes, and explicit no-live-GCS/no-deployment/no-credentials/no-external-services fields. `SHA256SUMS` covers every separately published file. The archive also contains this evidence, release notes, README, license, binary, and all inventories.

The build/test boundary used no GCP credential, cloud service, database, external identity provider, container daemon, persistent service, deployment permission, or non-loopback application dependency. The local mock backend is deliberately ephemeral and the GCS adapter is locally protocol-qualified but not live-qualified; see [v1 release notes](./v1-release-notes.md).
