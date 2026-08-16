# Direct dependency review

EndlessFS keeps direct dependencies deliberately small. Every addition must document why the Go standard library is insufficient, maintenance health, licensing, and security implications.

## `golang.org/x/text`

- **Purpose:** Unicode NFC normalization for canonical virtual paths, display names, and passkey labels.
- **Why the standard library is insufficient:** Go's standard library provides UTF-8 validation and Unicode character properties but no Unicode normalization implementation. Implementing normalization locally would be error-prone and security-sensitive.
- **Maintenance:** This is an official Go subrepository maintained through the Go project and pinned in `go.mod`, `go.sum`, `vendor/`, and the Nix source closure.
- **License:** BSD-3-Clause; the vendored license and patent notice are retained.
- **Security posture:** The dependency is used only for deterministic in-process normalization. It performs no I/O, networking, dynamic loading, or code generation at runtime.
