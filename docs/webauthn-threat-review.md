# WebAuthn adapter threat review

**Reviewed dependency:** `github.com/go-webauthn/webauthn v0.17.4`  
**Boundary:** `internal/auth.GoWebAuthn`  
**Review status:** required Milestone 2 controls implemented and covered by local virtual-authenticator tests

EndlessFS delegates WebAuthn parsing and cryptographic verification to the established [go-webauthn implementation](https://github.com/go-webauthn/webauthn). The project describes itself as FIDO2-conformance tested and documents discoverable/passkey flows, complete credential persistence requirements, and current counter/flag behavior. EndlessFS does not implement WebAuthn cryptography.

## Enforced relying-party policy

- Startup configuration supplies one validated base origin and an RP ID exactly equal to its hostname. Wildcards, origin paths, HTTP on public listeners, and cross-origin ceremonies are rejected.
- Registration requests require a discoverable credential (`residentKey: required`) and user verification (`userVerification: required`). Authentication is discoverable/usernameless and also requires user verification.
- `user.id` is the decoded opaque random EndlessFS user ID; `user.name` is its opaque base64url form; `user.displayName` is presentation only. No email or externally meaningful identity is passed to WebAuthn.
- Attestation conveyance is `none`. EndlessFS makes no authenticator provenance claim and configures no remote metadata service.
- Every challenge is 32 bytes from the injected cryptographic ID generator. The library's registration challenge is replaced consistently in both options and its opaque session record.

## Ceremony and storage boundary

- Library session data is stored atomically as opaque server-side JSON. It is never trusted from a browser.
- A separate random ceremony ID and 256-bit browser-binding cookie select the record. Only hashes of binding cookies are stored.
- Records expire after five minutes. Successful verification is followed by one CAS consumption; concurrent verification produces at most one application mutation or session.
- Registration state machines persist verified public credential material before claiming and materializing account records. Accounts remain disabled until profile, credential index, role guard where applicable, and account records are consistent.
- Credential IDs are indexed by SHA-256-derived state keys and must belong to exactly one opaque user ID. Discoverable login resolves both the authenticator user handle and credential ID and requires exact ownership.
- Stored credential data restores the public key, transport hints, backup flags, and signature count needed for login. Successful login conditionally writes updated flags/count and exposes clone warnings as a security result without inventing counter cryptography.

## Tested failures

The real adapter is exercised with `github.com/descope/virtualwebauthn` for successful registration and usernameless authentication and rejection of:

- wrong origin;
- wrong RP ID and RP hash;
- wrong challenge;
- absent user verification;
- invalid registration and assertion signatures;
- wrong authenticator user handle;
- credential/user ownership mismatch; and
- malformed response data.

Application tests separately cover browser-binding mismatch, expiry, atomic replay handling, concurrent bootstrap/invite use, disabled accounts, session rotation/revocation, and final-passkey protection.

## Residual risk and upgrade rule

The upstream module is still pre-v1 and may make breaking security-driven changes. A version upgrade must read its release notes and storage guidance, rerun the real-adapter negative matrix, inspect changes to credential flags/counters and origin validation, update this review, regenerate `go.sum` and `vendor/`, and pass the offline Nix security gate. A mock-backed v1 does not claim certification of EndlessFS itself or interoperability with every authenticator.
