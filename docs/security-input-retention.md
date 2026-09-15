# Retaining vulnerability data for reproducible verification

## Problem and guarantees

The v0.7.0 release verification on main commit `ba366451` failed on
2026-09-07 because its generation-pinned Go vulnerability database returned
HTTP 404. `nix run --refresh .#security` reproduced the same failure from the
unchanged commit on 2026-09-15. Previously, a moving database URL had caused a
hash mismatch (PR #13). Pinning the generation fixed byte identity but did not
provide retention.

The required guarantees remain: exact reviewed inputs, an offline required
scan, a complete upstream database, fail-closed corruption/missing-input
handling, and release evidence identifying the data used. A later upstream
deletion must not prevent verification from a checkout containing those inputs.

## Research and selected approach

- The [official Go database API](https://go.dev/doc/security/vuln/database)
  documents a complete bulk ZIP and `govulncheck -db=file://...`. Its database
  modification timestamp should not be treated as wall-clock cache expiry.
- [GCS Object Versioning](https://docs.cloud.google.com/storage/docs/object-versioning)
  allows explicit or lifecycle deletion of noncurrent generations. A generation
  identifies fixed bytes; it does not promise that those bytes remain available.
- [Nix input archiving](https://nix.dev/manual/nix/2.28/command-ref/new-cli/nix3-flake-archive)
  supports preserving a flake and its inputs. Retention must be deliberate;
  a workstation or CI cache is not an archival policy.
- [Go security guidance](https://go.dev/doc/security/best-practices) recommends
  regular vulnerability scanning. Replaying a pinned scan does not discover
  advisories added after that snapshot.

For this repository, retain the unchanged official bulk archive in
`security/vulndb`, accompanied by its checksum, provenance, and upstream license.
The selected September 2026 snapshot is approximately 3.3 MB compressed and
6.6 MB expanded. This avoids adding an external storage service or credentials
to the contributor and CI paths. The archive is a verification input and is
absent from the application binary and OCI image.

| Alternative | Reproducibility and availability | Decision |
| --- | --- | --- |
| Refresh the GCS generation pin | Exact bytes while retained upstream; deletion repeats the failure | Reject as the permanent fix |
| Fetch the latest database in the required check | Findings and required inputs can change without a source change; needs live network access | Does not satisfy the required gate |
| Commit the unchanged snapshot and checksum | Every source checkout retains its exact data; increases Git history size | Selected for this small archive |
| Retain a checksum-addressed artifact in a controlled archive | Suitable with durable retention, contributor-readable access, and verified recovery | Reasonable future option if snapshot history becomes burdensome |
| Generate a database from a pinned upstream source commit | Requires pinning and maintaining the generator and its build closure | More machinery than retaining the published format; upstream warns that its internal tooling has no compatibility promise |

The upstream tooling warning is in the
[golang/vulndb README](https://github.com/golang/vulndb).

## Implementation and update policy

`nix run .#vulndb -- update security/vulndb` is the only snapshot command that
contacts the official HTTPS endpoint. It preserves the downloaded ZIP bytes,
validates the index/report relationships, and records SHA-256, source URL,
retrieval time, and the database's own modification time. These fields describe
provenance and integrity; the locally computed checksum is not an upstream
signature.

Updates are explicit reviewed source changes. Review them regularly and before
a release, with the security check and complete local commit/push gate. An
optional future scheduled freshness scan would run through Tekton and remain
distinct from the pinned acceptance gate. This change does not install a
scheduled freshness job.

The normal Nix derivation validates and extracts the retained archive inside
the sandbox, without an upstream fetch or a pre-existing database cache.
`govulncheck` continues to use the same local-file database API. The dependency
policy verifies the checked-out archive and manifest. Release artifacts include
`VULNERABILITY-DATABASE.json`; the inventory records the archive SHA-256 and
database modification timestamp in place of the former remote-input NAR hash.
The Go toolchain, scanner, modules, and Nixpkgs remain pinned independently.

## Acceptance evidence

- Expected failure on unchanged main: `nix run --refresh .#security` returned
  HTTP 404 for generation `1787929740610734`.
- `TestRetainedSnapshotExtractsWithoutUpstreamOrCache` uses only retained bytes,
  an empty output directory, and no network client; it compares every extracted
  byte to the fixture.
- Negative tests reject changed or missing inputs, inconsistent provenance,
  malformed/truncated archives, unsafe/duplicate paths, empty indices, absent
  reports, unindexed reports, and broken module references, including archives
  whose checksum has been updated to match the invalid bytes.
- Updater tests prove that an upstream 404 preserves the previous snapshot and
  that a valid update retains the official response bytes unchanged.
- The `vulndb` flake check builds the real retained corpus offline and checks the
  scanner's interpretation of CVE-2023-29403: it must report `GO-2023-1840` for
  Go 1.20.0 and exclude it for the fixed Go 1.20.5. The mandatory `security` and
  `dependencies` checks scan the application using that corpus. Focused and
  complete gate results are recorded in the PR.
- The `release-input-evidence` flake check first failed when the manifest was
  present only inside the release archive. It now verifies the standalone
  manifest byte-for-byte against the retained input and requires its inclusion
  in the complete passing release checksum inventory.

The fix applies to new commits. It does not rewrite v0.7.0 or repair the old
tag's dependency URL. Re-running the original tag still needs its exact old
input to be recovered, or a new release must be made from a corrected commit.
