# Iteration and release validation

Use the fast edit loop while implementation and review are still changing the
candidate. Freeze once it is ready for the complete release gate. Keep every
required final command, assertion, workload, count, timeout and coverage floor.

Include newly added source files in the Git flake with `git add` first. Nix's
Git source omits untracked files; the public preflight fails closed if it finds
untracked Go files. Then run `nix run .#test-imports`. This resolves normal and race test
imports in the sandbox using the actual fixed-output vendor closure, with
module networking disabled for the selected Nix system's normal and race build
constraints. It does not typecheck, link or execute tests and proves no runtime
behavior. It catches an added test import missing from retained vendor output
even when the SDK version is already pinned and the host module cache has it.
`pr-check` also runs this preflight before the existing checks.

For a pure functional edit, run the relevant package/test selection through the
pinned environment, for example:

```text
nix develop --command go test ./internal/storageformat -run '^TestDomainPack'
```

Keep the same persistent `GOCACHE` and `GOMODCACHE` across edits. Go can reuse
compiled artifacts and successful cacheable test results when the package,
dependency, compiler, flags and observed runtime/file inputs remain valid.
An unchanged successful Nix derivation is also reusable by its declared input
identity. A compilation cache hit is not evidence that a test executed. The
example deliberately leaves out `-count=1` only for this edit loop; do not
remove it from final race, provider, migration, browser or coverage commands.
Confirm that the chosen expression actually selects the intended tests.

The normal Nix test source currently includes the Go packages, embedded assets,
fixtures, tools and `flake.nix`. The flake is an observed input of the exact
budget-selection policy test. Its raw contents therefore invalidate those
derivations even for a comment-only edit. Do not exclude it or another observed
input merely to obtain a cache hit. Narrowing cache keys needs a separately
proved dependency boundary; this change does not introduce one.

Adding an import from a previously unused subpackage can require regenerating
the vendor hash even with unchanged `go.mod` and `go.sum`. The vendor hash binds
actual retained bytes. Never replace that check with a host module-cache pass.

Keep expensive runtime evidence under its actual source, test, dependency,
toolchain, fixture and resource bindings. Carry it forward only through explicit
review of relevant input equivalence. Cached functional evidence cannot replace
a required fresh browser, replica or performance qualification. Preserve failed
runs and distinguish the full stress/release suite from the fast edit loop.

The flake also builds a disposable dependency compilation seed for the pinned
compiler, native compiler, platform, retained vendor closure and module inputs.
It contains normal/race compiler artifacts, never application test results.
The helper validates the pinned cache layout and copies complete files into an
owned, nonshared destination through an opened `os.Root`, preserving existing
warm entries. It rejects links and special files, stages in private files and
publishes without replacement. Initial CLI root selection assumes trusted task
parents; it is not a defense against hostile same-user replacement of those
parents. A rejected cache selects fresh private scratch and still runs the
original check. An explicitly disabled cache stays disabled. Do not repair
shared cache permissions or link immutable seed inodes into a writable cache.

Cache identity and compiler flags still matter. Random copied vendor source
paths can miss compilation entries, including the public coverage workflow;
seeding is not a promise of reuse. Measure installation and full command time
as well as compiler hits. Source timestamp preservation is required for seed
reproducibility. Qualify the actual seed with a real Nix rebuild after helper or
input changes, rather than inferring reproducibility from a successful build.

Local Linux-amd64 users can explicitly select the closed canonical functional
coverage profile:

```text
ENDLESSFS_LOCAL_FUNCTIONAL_CACHE=1 nix run .#test-coverage
```

Stage the intended source first. Before starting a producer, the consumer
compares the complete working tree and index with the immutable selected Nix
source. Extra ignored/untracked inputs, unsafe ownership or paths, and
unsupported arguments, environment or evidence selectors take the original
fresh path. Once realization starts, build/validation failures or subsequent
source drift fail the command; they never trigger a second fresh run.

The eligible local consumer uses the locked util-linux launcher in a private
user, PID and proc namespace, preserving the caller's numeric UID and GID.
It validates that isolation before a bounded, message-framed READY/CONTINUE
handshake. Only a fully reaped rejection before any CONTINUE attempt can
select the fresh path. Cancellation, uncertain startup and every failure after
that boundary fail closed. The launcher forwards signals and kills remaining
namespace children on exit. Network, clocks, cgroups and resource limits are
unchanged. This prevents unrelated host processes with unreadable metadata
from making the consumer's cleanup observation incomplete. Every process
inside the private scope still needs the exact owned scratch marker; denied
reads, incomplete scans and forced cleanup cannot earn natural-zero success.
Orphans are reaped at serial command boundaries after the direct child wait.
The producer retains the ordinary Nix sandbox and its original ownership
observer. Diagnostics distinguish complete observations from default zero
counts and publish only closed scan stages and reason codes.

The helper emits bounded diagnostics with closed stage, command and result
codes before removing private scratch. Records include durations, capture and
cleanup status, plus bounded numeric helper source locations. A failed Go run
can report hashes of known source-owned packages and test roots; dynamic
subtest names and raw output remain private. Diagnostics are not receipts and
cannot turn a failed producer into a reusable result.

A missing artifact executes the complete original coverage workload, including
`-count=1` and real browser tests. Changes to declared source, tests, assets,
fixtures, dependencies, tools, flags or policy select a new complete artifact.
Go-tool telemetry mode is off only in fresh private homes; application
observability is unchanged.
A reused result prints `REUSED` with its original producer timestamps. Every
consumption freshly derives the canonical package/test inventory and rechecks
the artifact, coverage profile and current coverage policy. The original
aggregate Go profile can exceed 64 MiB because `-coverpkg=./...` repeats blocks
across test packages; its complete bytes are bounded at 128 MiB and are never
truncated or rewritten. Repository, sensitive-package and migration coverage
floors remain 85%, 95% and 98%. Supported
`ENDLESSFS_COVERAGE_PROFILE` export copies the validated normal profile; the
destination is output-only and must be an absolute owned path outside source.

Reused functional evidence is not a fresh browser, resource, physical-replica
or CI run. CI keeps the original fresh coverage path; the separate online
security review and all required gate commands remain unchanged. Nix owns
artifact publication, reuse and garbage collection; there is no extra cache
service or result-expiration framework. Unsupported modes use the fresh path.

`nix run .#test-migration` realizes the same complete, unfiltered migration
check as the flake; an optional candidate release remains a declared derivation
input. Its coverage minimum and fixture matrix are unchanged. Browser fixture
concurrency defaults to two, with bounded modes one and four available through
`ENDLESSFS_E2E_PARALLELISM` for controlled comparison. Fixture scheduling
does not reduce any test body, assertion or deadline. Report shared-host load
and actual peak memory when comparing modes.

Browser fixtures declare an online connectivity signal through native CDP
after every real navigation and reload, since Chromium's override belongs to
the renderer process and a new document can replace it. The signal uses a reported
1 ms RTT to keep the override active; zero RTT with unspecified throughput
clears it in the pinned browser. This metadata does not add HTTP latency.
An isolated network namespace can provide working
loopback HTTP while Chromium infers offline from absent external interfaces.
This declaration does not emulate successful requests or enable external
networking. A native offline/online signal regression verifies pending smart
uploads, real requests and bytes, completion, and restoration after reload.
It qualifies browser connectivity signaling, not a transport outage.
