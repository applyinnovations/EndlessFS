# Direct dependency review

EndlessFS keeps direct dependencies deliberately small. Every addition must document why the Go standard library is insufficient, maintenance health, licensing, and security implications.

## Inter `4.0`

- **Purpose:** The sole interface typeface for the rebuilt EndlessFS UI, embedded at Regular 400, Medium 500, and Semibold 600.
- **Source pin:** Official `rsms/inter` release `v4.0`, commit `2ce9119398be143fa289c3e180824db1b7ed803e`. The release archive SHA-256 is `ff970a5d4561a04f102a7cb781adbd6ac4e9b6c460914c7a101f15acb7f7d1a4`.
- **Asset digests:** `inter-regular.woff2` is `b6f9db9e45be20f3c1312c97fbee7ec36b7d8280f8caa4d53c9ba0408cc9997a`; `inter-medium.woff2` is `8458f8afa67b5691c1fcbe51607a2dafb53a9839e48131c608a186b65415d96d`; `inter-semibold.woff2` is `8e52a861dc26ff4608c50bd7ff89b65d0d6216a2afe7b47ce5d84544811ca400`.
- **Maintenance:** Inter is an established open-source type family with tagged releases. EndlessFS intentionally remains on the approved 4.0 pin until a reviewed brand-system update changes it.
- **License:** SIL Open Font License 1.1; the upstream notice and complete license are retained in [`licenses/Inter-OFL-1.1.txt`](./licenses/Inter-OFL-1.1.txt).
- **Security posture:** The three validated WOFF2 files are compile-time embedded in the Go binary and served only from immutable same-origin asset routes. The UI makes no runtime request to a font CDN or other third party.

## Tabler Icons `3.46.0`

- **Purpose:** A coherent application-owned outline vocabulary for compact file, navigation, identity, security, and administrative actions.
- **Why a maintained icon set is necessary:** Unicode glyphs vary by platform, provide inconsistent geometry, and make related actions difficult to distinguish. Recreating a broad filesystem and administration icon vocabulary locally would increase ambiguity and ongoing design maintenance.
- **Source pin:** Official `tabler/tabler-icons` release `v3.46.0`, commit `8ac7d81b72ece11072ef25ea9fd92e80c6f3c9fc`.
- **Maintenance:** Tabler Icons is actively maintained, has a large contributor community, and provides more than 6,000 icons in one consistent 24 px outline system. Upgrades remain explicit review changes.
- **License:** MIT; the upstream notice and complete license are retained in [`licenses/Tabler-Icons-MIT.txt`](./licenses/Tabler-Icons-MIT.txt).
- **Security posture:** EndlessFS embeds only the reviewed path data for icons it uses. The application imports no JavaScript package, icon font, build plugin, CDN, remote asset, or runtime parser. SVG elements are constructed through fixed DOM operations and inherit semantic application colors through `currentColor`.

## `golang.org/x/text`

- **Purpose:** Unicode NFC normalization for canonical virtual paths, display names, and passkey labels.
- **Why the standard library is insufficient:** Go's standard library provides UTF-8 validation and Unicode character properties but no Unicode normalization implementation. Implementing normalization locally would be error-prone and security-sensitive.
- **Maintenance:** This is an official Go subrepository maintained through the Go project and pinned in `go.mod`, `go.sum`, and Nix's fixed-output module closure.
- **License:** BSD-3-Clause; its license and patent notice are retained in the locked module closure.
- **Security posture:** The dependency is used only for deterministic in-process normalization. It performs no I/O, networking, dynamic loading, or code generation at runtime.

## `github.com/go-webauthn/webauthn`

- **Purpose:** Standards-based WebAuthn registration and discoverable/passkey authentication parsing, origin and RP verification, COSE public-key handling, signature verification, authenticator flags, and signature-counter guidance.
- **Why the standard library is insufficient:** WebAuthn spans evolving W3C/FIDO data formats, CBOR/COSE parsing, attestation formats, browser client-data validation, and subtle verification ordering. Reimplementing it would be custom cryptographic protocol code and is explicitly prohibited by the v1 specification.
- **Maintenance:** The upstream project is actively maintained, documents support for current Go releases, publishes tagged releases, and reports FIDO2 conformance testing. EndlessFS pins `v0.17.4`; because upstream remains pre-v1, upgrades require an explicit migration and threat review.
- **License:** BSD-3-Clause. The license and required transitive license files are retained in the locked module closure.
- **Security posture:** The library is isolated behind `internal/auth.WebAuthnEngine`. EndlessFS configures exactly one RP ID and origin, cross-origin use remains disabled, attestation conveyance is `none`, resident keys and user verification are required, and generated challenges are replaced with injected 256-bit values. The detailed review is [webauthn-threat-review.md](./webauthn-threat-review.md).

## `github.com/descope/virtualwebauthn`

- **Purpose:** Test-only virtual authenticators for full registration and assertion flows against the real WebAuthn adapter.
- **Why the standard library is insufficient:** Producing correct authenticator data, COSE keys, attestation objects, and signatures in tests is itself protocol implementation work. A maintained test helper makes negative verification tests auditable and independent from browser hardware.
- **Maintenance:** The project publishes tagged v1 releases and is pinned at `v1.0.5`.
- **License:** MIT; the license is retained in the locked module closure.
- **Security posture:** This module is imported only by `_test.go` files and is absent from the application binary's runtime call graph. It generates in-memory test credentials and performs no external I/O.

The WebAuthn dependency closure includes audited format/parsing support for CBOR, COSE, TPM, JWT metadata, UUIDs, and generated serialization. It is locked by `go.sum` and Nix's fixed-output hash, materialized only inside offline Nix builds, statically linked, and receives no network or filesystem authority from EndlessFS application code.

## `cloud.google.com/go/storage`

- **Purpose:** Official Google Cloud Storage transport, conditional-generation operations, JSON reads, server-side rewrite/copy, checksum verification, signed capabilities, and Application Default Credentials including workload identity federation.
- **Why the standard library is insufficient:** Correct GCS authentication, request signing, retry classification, resumable protocol details, checksums, and generation preconditions are security- and data-integrity-sensitive evolving protocols. A bespoke client would duplicate the official implementation and materially increase interoperability risk.
- **Maintenance:** The module is maintained and released by Google Cloud's Go client-library team. EndlessFS pins `v1.64.0`; upgrades require the credential-free protocol suite, dependency/security gates, and any opt-in live qualification to pass again.
- **License:** Apache-2.0. The module and every transitive license notice are retained in the locked module closure and inventoried by the Nix dependency gate.
- **Security posture:** Only `internal/objectstore/gcs` imports the client. Production construction uses JSON reads and ADC without endpoint overrides, static service-account keys, or HMAC credentials. Tests inject an unauthenticated loopback client. Mutations disable automatic transport retries so an ambiguous lost-success response remains recoverable by EndlessFS's canonical admission/fencing protocol; provider errors are translated to safe domain kinds.

## Pinned security inputs

The flake pins the Go toolchain source and the [official Go vulnerability database](https://go.dev/doc/security/vuln/database) as immutable Nix inputs. The database URL selects an exact Google Cloud Storage object generation, which uniquely identifies immutable object bytes, and `flake.lock` independently records the Nix content hash. The dependency policy rejects a moving latest-object URL or a nonnumeric generation. The required `govulncheck` gate reads that local database with `-db=file://...`; it never depends on mutable network results. `flake.lock` is therefore both the dependency-resolution record and the vulnerability-data freshness record for a release. Updating either input is an explicit, reviewed change followed by the complete Nix gate.

Every module is pinned by `go.mod`, `go.sum`, and the fixed Nix module hash. Nix materializes that closure in its sandbox for offline builds without tracking `vendor/` in Git. `nix run .#dependency-check` inventories the locked module versions and fails when a module root does not retain a license or copying notice. The release derivation emits the module inventory and a deterministic hash inventory of all retained dependency license files.

## `golang.org/x/sys`

- **Purpose:** Create and seal the Linux anonymous memory file used to pass camera-RAW bytes to the isolated decoder without a persistent filesystem name.
- **Why the standard library is insufficient:** Go exposes inherited file descriptors but does not expose Linux `memfd_create` or file-seal operations through `os` or `syscall`. Re-declaring architecture-specific syscall numbers locally would be less portable and less auditable.
- **Maintenance:** This is an official Go subrepository maintained through the Go project and pinned at `v0.46.0`; it was already present in the locked transitive module closure and is now a direct dependency because EndlessFS imports its Linux API.
- **License:** BSD-3-Clause; its license and patent notice are retained in the locked module closure.
- **Security posture:** The dependency is used only for `memfd_create` and write/grow/shrink/seal controls. The descriptor is bounded by the preview source limit, inherited only by the one decoder child, and closed after the one-shot operation. It performs no networking, authentication, parsing, or dynamic loading.

## `github.com/deepteams/webp`

- **Purpose:** Decode the closed PNG/JPEG/GIF/WebP source allowlist where applicable, validate static WebP containers, and encode every generated v1.1 preview as a static WebP artifact.
- **Why the standard library is insufficient:** Go's standard library decodes PNG, JPEG, and GIF but neither decodes nor encodes WebP. A single audited codec is required to accept WebP originals and produce the one artifact format selected for low transfer and storage cost.
- **Maintenance:** The project publishes signed, verified release tags and maintains a pure-Go codec with active tests and fuzzing. EndlessFS pins `v1.2.6`, materializes it only in Nix's fixed-output module closure, and runs its own malformed-input, resource-bound, static-frame, metadata-removal, and startup-integrity tests before accepting an upgrade.
- **License:** MIT; the license is retained and inventoried in the Nix-generated module closure.
- **Security posture:** The codec is statically linked with `CGO_ENABLED=0`; it starts no process, performs no network or filesystem access, and loads no runtime plugin. EndlessFS bounds compressed bytes, decoded dimensions, decoded pixels, execution time, and output variants before or around codec use. Artifacts are decoded and structurally revalidated before immutable commit. Source metadata is not copied into generated output.

## LibRaw `0.22.1`

- **Purpose:** Decode the closed DNG, CR2, CR3, RAF, NEF, ORF, RW2, PEF, and ARW camera-RAW input set into a bounded raster that EndlessFS re-encodes as metadata-free WebP.
- **Why the standard library is insufficient:** Go's standard library has no camera-RAW decoder, and the formats contain camera-specific sensor layouts, compression, color calibration, and model quirks that would be unsafe and impractical to reimplement.
- **Maintenance:** LibRaw publishes maintained releases and a current supported-camera matrix. EndlessFS pins Nixpkgs' LibRaw `0.22.1`; upgrades require the deterministic DNG test, malformed/mismatched-input denial tests, worker-isolation checks, full Nix gate, and live representative-camera validation.
- **License:** LibRaw and `dcraw_emu` are dual-licensed under CDDL-1.0 or LGPL-2.1-or-later. Both license texts are hashed in the release license inventory and the OCI label records the combined application/runtime expression.
- **Security posture:** Nix installs only the fixed `dcraw_emu` helper as `endlessfs-raw-decoder` plus its immutable runtime closure. It is invoked only inside the existing one-shot preview worker with fixed options, an empty environment, discarded diagnostics, bounded stdout, and a hard operation deadline. Linux source bytes use a sealed anonymous memory file and the decoder receives parent-death termination. No provider path, filename, capability, credential, configurable executable, or configurable argument reaches the helper. The Go worker strictly parses 8-bit PPM, enforces dimensions/pixels/output size, and emits the same static metadata-free WebP contract.

## `github.com/chromedp/chromedp` and `github.com/chromedp/cdproto`

- **Purpose:** Test-only control of pinned Chromium and its WebAuthn virtual-authenticator, network, download, viewport, and DOM accessibility surfaces.
- **Why the standard library is insufficient:** The standard library can test HTTP handlers but does not implement the Chrome DevTools Protocol or execute the embedded browser application. The v1 contract explicitly requires a headless Chromium driver written in Go and forbids a Node-based browser toolchain.
- **Maintenance:** The chromedp organization actively maintains both modules and tracks the evolving DevTools protocol. EndlessFS pins `chromedp` at `v0.15.1` and `cdproto` at an immutable pseudo-version; upgrades require the real-browser suite to remain green against Nix Chromium.
- **License:** MIT. License notices are retained in the locked module closure.
- **Security posture:** Both modules are imported only by `_test.go` files. Browser execution is loopback-only, uses a temporary profile, disables background networking, records every HTTP(S) request origin, and fails if the UI contacts anything except its control and returned capability origins. Neither module enters the production application binary.
