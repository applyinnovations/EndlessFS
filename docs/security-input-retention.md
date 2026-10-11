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
live freshness scan runs through Tekton for PRs, merge-queue candidates and
publishing, separately from the pinned acceptance gate. This change does not
install a scheduled freshness job.

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

## October 2026 pre-release review

The pre-v0.7.2 review fetched the unchanged official ZIP modified on
2026-10-07T14:10:51Z: SHA-256
`ed8a9636dd6aaa55458d2a97f58b439b523a23f14c499b91088d45babbb01e1a`,
4,598 reports. The first scan with the merged repair and its existing module
pins failed with reachable reports for
[GO-2026-6505](https://pkg.go.dev/vuln/GO-2026-6505) (OpenTelemetry exporter
configuration logging) and
[GO-2026-6348](https://pkg.go.dev/vuln/GO-2026-6348) (gRPC HTTP/2 fragmentation
memory exhaustion). Reachability is the release-gate result; it is not evidence
that a production exploit occurred. The Go OpenTelemetry report is marked
unreviewed, and its maintainer advisory remains the source for impact details.

The existing OpenTelemetry core/SDK family advances together from v1.44.0 to
v1.45.0; gRPC advances from v1.82.1 to v1.83.1, the reported fixed versions.
Their minimum-version-selected dependency updates remain pinned in `go.mod`,
`go.sum`, and Nix's module hash. No new direct dependency, application mechanism,
storage format, or release-ledger boundary is introduced. The updated snapshot
remains a retained offline verification input. The provider contracts, exact
economics, application workflows, migration matrix, and complete local push gate
must pass with these dependencies before the refresh is published for review.

## October 8 advisory review before v0.7.4

The pre-release review of merged commit
`4e602c96aa76411c398f8a826886ddf27b5f8b26` fetched the unchanged official ZIP
modified on 2026-10-08T22:31:09Z, with 4,613 reports and SHA-256
`931855e697c9adf12b79d205c7f7f1f8e1e080f936647757c4ea246546438073`.
The standalone scan and the retained-data `nix run .#security` regression both
exited 3 with twelve reachable advisories affecting the pinned Go 1.26.6
standard library; several also affect the pinned `golang.org/x/net` v0.57.0.
The interrupted release qualification was not counted as a passing gate, and
the local candidate tag was removed before publication.

The repair pins [Go 1.26.9](https://go.dev/doc/devel/release#go1.26.9), preserving
the existing Go major/minor line. Its source archive is bound to the official
download SHA-256
`9735d7dcdb65b35d3fa577f04064737c03b89cf1a2b71e6e69fe2f3c6f9fd4ca`.
It advances x/net to v0.60.0, the reported fixed version for
[GO-2026-6617](https://pkg.go.dev/vuln/GO-2026-6617) and
[GO-2026-6603](https://pkg.go.dev/vuln/GO-2026-6603), and retains the official
snapshot so the normal offline gate evaluates these reports reproducibly.
The required minimum-version dependency changes are x/crypto v0.57.0,
x/sync v0.23.0, x/sys v0.48.0 and x/text v0.42.0. All remain existing
dependencies, locked by go.mod, go.sum and the recomputed Nix module hash.
The release inventory derives its compiler version from the same explicit pin.

The binary-metadata regression also first failed with an actual Go 1.26.5
application binary despite the declared Go pin. Passing `go` as a package
attribute did not override buildGoModule's captured compiler. The application
builder now overrides that function's compiler argument explicitly; the new
compiler-identity flake check reads the built binary's actual Go metadata and
requires the declared pin. This covers the base production builder used by
release CI and prevents an inventory-only toolchain update from passing.

No application path, storage epoch, writer identity, provider budget, telemetry
boundary or deployment configuration changes. Qualification must include the
passing retained-data security gate, provider economics, valid HTTP/provider
paths and denial tests, browser workflows, complete migration/race coverage,
and the complete ordered local commit/push gate on the exact candidate tree.
The security scanner's reachability analysis does not establish a production
exploit. Application repair rollout remains pending a qualified release image.

The Go 1.26.9 candidate's first complete migration gate reported 97.995%
statement coverage; a diagnostic rerun of the same source reported 98.067%.
The aggregate was relying on incidental execution of some guard branches.
Deterministic schema-011 authority-verifier qualification now covers each
incorrect adjacent-edge field, open/closing/wrong-checkpoint gates, a closed
marker without frozen authority, an unavailable gate read, and valid frozen
typed state. Verification must issue no writes and conserve the state's bytes
and logical version on both valid and denial paths. The 98% migration threshold,
immutable predecessor fixtures and exhaustive interruption portfolio remain
unchanged.

`nix run .#security-freshness` now performs the separate live review using the
existing bounded updater and validated extraction logic, a temporary
database and the pinned compiler/scanner. It reports the corpus checksum and
modification time and fails on retrieval, integrity or reachable-advisory
failure. It does not edit the retained snapshot or the candidate tree. PR and
merge-queue CI run it after pr-check; main-image and release publishing run it
before distribution. Offline pipeline-policy checks require the invocation in
all active PaC workflows. Local qualification includes the same live command
before push. Normal flake/security tests still require no live service.
