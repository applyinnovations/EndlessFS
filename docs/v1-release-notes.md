# EndlessFS v1 release notes

EndlessFS v1 is feature complete for the specification's deterministic, mock-backed acceptance boundary. It provides the single-binary passkey identity system, private Drive control plane, direct capability data plane, trash, read-only public sharing, administration and recovery, accessible embedded browser application, and closed data-only theme system described in [the v1 specification](./v1-specification.md).

The release gate covers the exhaustive cross-user endpoint and traversal corpora, structured-log redaction, direct-data-plane byte exclusion, state/provider contracts, concurrency and race invariants, bounded fuzzing across every required parser boundary, real Chromium workflows under both built-in appearances, 85% repository coverage, and 95% coverage for each security-sensitive boundary named by the specification.

The release is built and verified exclusively through the pinned Nix interface. Its release archive includes the application binary, OCI archive, source/input inventory, binary and OCI hashes, dependency and license inventory, installed-theme inventory, and the acceptance evidence index.

Important limitation: v1 implements only the deterministic local mock storage provider. Real Google Cloud Storage integration, live bucket interoperability, cloud resource creation, deployment, production operations, and production durability were not implemented or tested. They are not v1 acceptance requirements. A green v1 release is therefore not evidence of GCS interoperability or production-storage readiness.

No GCP credentials, cloud services, database, external identity provider, container runtime, deployment target, or non-loopback runtime service is needed to build or accept v1. The local provider and state store are intentionally in-memory; restarting the process starts an empty demo instance, so there is no v1 production backup/restore claim.
