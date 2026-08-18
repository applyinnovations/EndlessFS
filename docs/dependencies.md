# Direct dependency review

EndlessFS keeps direct dependencies deliberately small. Every addition must document why the Go standard library is insufficient, maintenance health, licensing, and security implications.

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

The flake pins the Go toolchain source and the [official Go vulnerability database](https://go.dev/doc/security/vuln/database) as immutable Nix inputs. The required `govulncheck` gate reads that local database with `-db=file://...`; it never depends on mutable network results. `flake.lock` is therefore both the dependency-resolution record and the vulnerability-data freshness record for a release. Updating either input is an explicit, reviewed change followed by the complete Nix gate.

Every module is pinned by `go.mod`, `go.sum`, and the fixed Nix module hash. Nix materializes that closure in its sandbox for offline builds without tracking `vendor/` in Git. `nix run .#dependency-check` inventories the locked module versions and fails when a module root does not retain a license or copying notice. The release derivation emits the module inventory and a deterministic hash inventory of all retained dependency license files.

## `github.com/deepteams/webp`

- **Purpose:** Decode the closed PNG/JPEG/GIF/WebP source allowlist where applicable, validate static WebP containers, and encode every generated v1.1 preview as a static WebP artifact.
- **Why the standard library is insufficient:** Go's standard library decodes PNG, JPEG, and GIF but neither decodes nor encodes WebP. A single audited codec is required to accept WebP originals and produce the one artifact format selected for low transfer and storage cost.
- **Maintenance:** The project publishes signed, verified release tags and maintains a pure-Go codec with active tests and fuzzing. EndlessFS pins `v1.2.6`, materializes it only in Nix's fixed-output module closure, and runs its own malformed-input, resource-bound, static-frame, metadata-removal, and startup-integrity tests before accepting an upgrade.
- **License:** MIT; the license is retained and inventoried in the Nix-generated module closure.
- **Security posture:** The codec is statically linked with `CGO_ENABLED=0`; it starts no process, performs no network or filesystem access, and loads no runtime plugin. EndlessFS bounds compressed bytes, decoded dimensions, decoded pixels, execution time, and output variants before or around codec use. Artifacts are decoded and structurally revalidated before immutable commit. Source metadata is not copied into generated output.

## `github.com/chromedp/chromedp` and `github.com/chromedp/cdproto`

- **Purpose:** Test-only control of pinned Chromium and its WebAuthn virtual-authenticator, network, download, viewport, and DOM accessibility surfaces.
- **Why the standard library is insufficient:** The standard library can test HTTP handlers but does not implement the Chrome DevTools Protocol or execute the embedded browser application. The v1 contract explicitly requires a headless Chromium driver written in Go and forbids a Node-based browser toolchain.
- **Maintenance:** The chromedp organization actively maintains both modules and tracks the evolving DevTools protocol. EndlessFS pins `chromedp` at `v0.15.1` and `cdproto` at an immutable pseudo-version; upgrades require the real-browser suite to remain green against Nix Chromium.
- **License:** MIT. License notices are retained in the locked module closure.
- **Security posture:** Both modules are imported only by `_test.go` files. Browser execution is loopback-only, uses a temporary profile, disables background networking, records every HTTP(S) request origin, and fails if the UI contacts anything except its control and returned capability origins. Neither module enters the production application binary.
