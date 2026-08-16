# Security policy

EndlessFS is security-sensitive and is still pre-v1. The current scaffold is not suitable for production data.

Do not report suspected vulnerabilities in a public issue. Use GitHub's private vulnerability reporting for this repository when available. If that channel is unavailable, contact the repository owners privately through their GitHub organization profile and share only enough information to establish a secure reporting channel.

Include:

- the affected revision;
- the violated trust boundary or invariant;
- minimal reproduction steps;
- realistic impact; and
- any known workaround.

Do not include live credentials, bearer tokens, passkey material, personal data, or another person's file content. Use deterministic local fixtures.

The threat model and explicit non-claims are in sections 17 and 17.5 of `docs/v1-specification.md`. In particular, EndlessFS does not claim end-to-end encryption, protection from a malicious operator/provider, cryptographic user isolation, complete denial-of-service protection, or production readiness based on local mocks.

Security fixes require a regression/exploit test, a valid-path test, and review of adjacent authorization boundaries before disclosure.
