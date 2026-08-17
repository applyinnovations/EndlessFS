# EndlessFS v1 release notes

EndlessFS v1 provides the single-binary passkey identity system, private Drive control plane, direct capability data plane, trash, read-only public sharing, administration and recovery, accessible embedded browser application, and closed data-only theme system described in [the v1 specification](./v1-specification.md).

The clarified v1 storage contract is now implemented by one provider-independent engine. Its canonical keys, record bodies, logical versions, directory manifests, operation/idempotency state, writer gate, and checkpoints do not depend on provider-native identifiers or metadata. Deterministic raw-copy tests move only checkpoint-authorized key/body pairs between independent backends, regenerate every native version, reopen at a new gate epoch, preserve all logical state, and continue mutations in both directions without a state migration.

Multi-replica mutations use a durable candidate/admitted barrier, immutable preparation, conditional visibility points, expiring ownership, and monotonically increasing fences. When a replica disappears, exactly one eligible takeover resumes the recorded intent; a stale returning worker cannot publish or unlock it. Gate closure waits for recovery and capability/lease drain rather than sacrificing consistency for availability.

The GCS adapter is locally qualified without credentials using an in-process protocol server that exercises documented JSON/XML requests, generation preconditions, strong visibility, checksums, ranges, resumable offsets and cancellation, generation-bound V4 capabilities, exact-origin CORS, stable error mapping, disconnects, and ambiguous lost-success recovery. The same application provider/state contracts run through the portable engine over the GCS adapter.

The release is built and verified exclusively through the pinned Nix interface. Its release archive includes the application binary, OCI archive, source/input inventory, binary and OCI hashes, dependency and license inventory, installed-theme inventory, and the acceptance evidence index.

Important limitation: no live GCS bucket interoperability, cloud resource creation, deployment, production operations, backup/restore, regional durability, or incident-response procedure was tested. They are not deterministic v1 acceptance requirements. “Locally qualified GCS adapter” is evidence of integration-layer behavior against the documented protocol, not evidence that a particular live deployment is production-ready.

No GCP credentials, cloud services, database, external identity provider, container runtime, deployment target, or non-loopback runtime service is needed to build or accept v1. The local `mock` backend is intentionally in-memory; the `gcs` runtime uses Application Default Credentials and keyless workload identity/IAM signing, but still requires the separate live qualification and operations review before production use.
