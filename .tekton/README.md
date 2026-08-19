# EndlessFS xlab pipelines

These Pipelines-as-Code definitions run CI and release work on the xlab
bare-metal Talos Linux cluster. They do not deploy EndlessFS and do not use the
GKE cluster that hosts `drive.endlessfs.com`.

| File | Trigger | Purpose |
| --- | --- | --- |
| `endlessfs-ci.yaml` | pull request to `main` or `gh-readonly-queue/main/*` push | Fast policy, full Nix gate, and Chromium coverage |
| `endlessfs-container.yaml` | `main` push | Publish immutable commit and `edge` OCI tags |
| `endlessfs-release.yaml` | `vMAJOR.MINOR.PATCH` tag push | Re-verify, publish release OCI tags, create the GitHub release, and upload every Nix release artifact |
| `endlessfs-darwin-smoke.disabled.yaml` | none | Deprecated, inert record for the retired Darwin smoke job |

All active runs select `storage.xlab.now/fast-local=true`, use a per-run
`fast-local` source volume, and reuse the shared Git mirror and 256 GiB v2 Nix
store in `tekton-buildkit` on local NVMe. They run in xlab's isolated
`tekton-buildkit` privileged/userns namespace because the ordinary
`tekton-pipelines` namespace correctly enforces baseline Pod Security. The full
gate requests 6 CPU/12 GiB and may burst to 12 CPU/24 GiB; coverage requests
4 CPU/8 GiB and may burst to
8 CPU/16 GiB. These values leave enough capacity for xlab system workloads
while allowing the parallel gate to use most of the otherwise-idle node.
One small `prepare-cache` Task seeds an empty shared Nix PVC before the three
parallel verification tasks begin, avoiding first-run seed races without
serializing the warm-cache gate. Releases reuse the same caches and never create
release-specific persistent storage.

Like Odysseus's BuildKit release path, each privileged Nix task is privileged
only inside a Kubernetes-managed pod user namespace selected by its
`taskRunSpecs[].podTemplate.hostUsers: false` override. The common pod template
sets `fsGroup: 1000` for the persistent cache volumes. Cluster admission rejects
either sandboxed Nix task when the user-namespace override is missing, and the
task itself verifies that container UID 0 is not host UID 0 before executing.

## Authentication boundary

Pipelines-as-Code creates a per-run Secret containing a short-lived xlab.now
GitHub App installation token. The clone task mounts its generated Git
configuration. `nix-run-github-v2` reads the same Secret's `git-provider-token`
key and exposes it only for the publishing step as `GH_TOKEN` and `GHCR_TOKEN`.
The installation token is supported for cloning, release creation, and release
asset uploads. It is also the first GHCR credential attempted now that the App
has `packages: write`, but GitHub's published Container registry authentication
matrix does not document general GitHub App tokens. A successful xlab push must
prove this path before Actions are removed; if GHCR rejects it, registry
credentials need a separate reviewed design. No fallback token is added by this
change.

The App needs `contents: write` and `packages: write`; those permissions do not
grant repository administration. Applying `.github/rulesets/*.json` remains an
explicit administrator action with a separate, short-lived Administration token
through `nix run .#repository-policy -- apply`.

## Darwin retirement

The Darwin file is deliberately a `Pipeline`, not a `PipelineRun`. It has no
`pipelinesascode.tekton.dev/on-*` annotation and no active definition references
it. Its only possible manual behavior is a Linux deprecation message. It does
not reference xlab's `namespace-macos-fastlane` task or any Namespace CLI,
credential, cache, or runner, so ordinary PaC events cannot allocate a Mac.

## Safe cutover

1. Merge the companion xlab-deployments change that adds the EndlessFS
   `Repository`, repository-keyed clone caching, sandboxed Nix task, and
   GitHub-token publishing task.
2. Land these `.tekton` definitions on `main` while the legacy Linux GitHub
   checks still protect the bootstrap merge. The xlab `Repository` resolves
   PipelineRuns from the default branch, so pull requests cannot replace trusted
   pipeline code.
3. Confirm a pull-request run, a merge-queue run, and an isolated GHCR tag push
   from the xlab App token. Do not treat `packages: write` alone as proof that
   registry authentication works. Delete the disposable tag after verification
   if the available package permissions permit deletion.
4. Apply the checked-in rulesets only after the successful PaC check is visible
   as `tekton-xlab / endlessfs-ci-` from GitHub App integration `949094`.
5. Remove the remaining bootstrap GitHub workflows in the cutover PR. The Darwin
   job is already retired and must not be restored during bootstrap.
