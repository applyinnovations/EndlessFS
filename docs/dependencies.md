# Direct dependency review

EndlessFS keeps direct dependencies deliberately small. Every addition must document why the Go standard library is insufficient, maintenance health, licensing, and security implications.

## `golang.org/x/text`

- **Purpose:** Unicode NFC normalization for canonical virtual paths, display names, and passkey labels.
- **Why the standard library is insufficient:** Go's standard library provides UTF-8 validation and Unicode character properties but no Unicode normalization implementation. Implementing normalization locally would be error-prone and security-sensitive.
- **Maintenance:** This is an official Go subrepository maintained through the Go project and pinned in `go.mod`, `go.sum`, `vendor/`, and the Nix source closure.
- **License:** BSD-3-Clause; the vendored license and patent notice are retained.
- **Security posture:** The dependency is used only for deterministic in-process normalization. It performs no I/O, networking, dynamic loading, or code generation at runtime.

## `github.com/go-webauthn/webauthn`

- **Purpose:** Standards-based WebAuthn registration and discoverable/passkey authentication parsing, origin and RP verification, COSE public-key handling, signature verification, authenticator flags, and signature-counter guidance.
- **Why the standard library is insufficient:** WebAuthn spans evolving W3C/FIDO data formats, CBOR/COSE parsing, attestation formats, browser client-data validation, and subtle verification ordering. Reimplementing it would be custom cryptographic protocol code and is explicitly prohibited by the v1 specification.
- **Maintenance:** The upstream project is actively maintained, documents support for current Go releases, publishes tagged releases, and reports FIDO2 conformance testing. EndlessFS pins `v0.17.4`; because upstream remains pre-v1, upgrades require an explicit migration and threat review.
- **License:** BSD-3-Clause. The license and required transitive license files are retained in `vendor/`.
- **Security posture:** The library is isolated behind `internal/auth.WebAuthnEngine`. EndlessFS configures exactly one RP ID and origin, cross-origin use remains disabled, attestation conveyance is `none`, resident keys and user verification are required, and generated challenges are replaced with injected 256-bit values. The detailed review is [webauthn-threat-review.md](./webauthn-threat-review.md).

## `github.com/descope/virtualwebauthn`

- **Purpose:** Test-only virtual authenticators for full registration and assertion flows against the real WebAuthn adapter.
- **Why the standard library is insufficient:** Producing correct authenticator data, COSE keys, attestation objects, and signatures in tests is itself protocol implementation work. A maintained test helper makes negative verification tests auditable and independent from browser hardware.
- **Maintenance:** The project publishes tagged v1 releases and is pinned at `v1.0.5`.
- **License:** MIT; the vendored license is retained.
- **Security posture:** This module is imported only by `_test.go` files and is absent from the application binary's runtime call graph. It generates in-memory test credentials and performs no external I/O.

The WebAuthn dependency closure includes audited format/parsing support for CBOR, COSE, TPM, JWT metadata, UUIDs, and generated serialization. It is locked by `go.sum`, vendored for offline Nix builds, statically linked, and receives no network or filesystem authority from EndlessFS application code.

## `github.com/chromedp/chromedp` and `github.com/chromedp/cdproto`

- **Purpose:** Test-only control of pinned Chromium and its WebAuthn virtual-authenticator, network, download, viewport, and DOM accessibility surfaces.
- **Why the standard library is insufficient:** The standard library can test HTTP handlers but does not implement the Chrome DevTools Protocol or execute the embedded browser application. The v1 contract explicitly requires a headless Chromium driver written in Go and forbids a Node-based browser toolchain.
- **Maintenance:** The chromedp organization actively maintains both modules and tracks the evolving DevTools protocol. EndlessFS pins `chromedp` at `v0.15.1` and `cdproto` at an immutable pseudo-version; upgrades require the real-browser suite to remain green against Nix Chromium.
- **License:** MIT. Vendored license notices are retained.
- **Security posture:** Both modules are imported only by `_test.go` files. Browser execution is loopback-only, uses a temporary profile, disables background networking, records every HTTP(S) request origin, and fails if the UI contacts anything except its control and returned capability origins. Neither module enters the production application binary.
