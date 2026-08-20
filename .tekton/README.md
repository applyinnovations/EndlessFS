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

All active runs select `storage.xlab.now/fast-local=true`, use a 10 GiB per-run
`fast-local` source volume, and reuse the shared Git mirror and 96 GiB v2 Nix
store in `tekton-buildkit` on local NVMe. They run in xlab's isolated
`tekton-buildkit` privileged/userns namespace because the ordinary
`tekton-pipelines` namespace correctly enforces baseline Pod Security. Cache
placement is also fail-closed at the cluster boundary: admission denies a Pod
in either Tekton execution namespace when it mounts `nix-store`,
`git-repo-cache`, or `oci-layer-cache` without selecting
`storage.xlab.now/fast-local=true`. Sirius is currently the only node carrying
that label. The full gate requests 6 CPU/12 GiB and may burst to 12 CPU/24 GiB;
coverage requests
4 CPU/8 GiB and may burst to
8 CPU/16 GiB. These values leave enough capacity for xlab system workloads
while allowing the parallel gate to use most of the otherwise-idle node.
One small `prepare-cache` Task seeds an empty shared Nix PVC before verification
begins, avoiding first-run seed races. Fast checks and the full Nix gate run in
parallel; coverage follows them so Chromium gets predictable resources.
Releases reuse the same caches and never create release-specific persistent
storage.

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
key and exposes it only for the publishing step as `GH_TOKEN`. The installation
token is used for cloning, release creation, and release asset uploads.

GitHub Container Registry rejected that general App installation token even
with `packages: write`. Container and release publishing therefore bind the
shared, SOPS-encrypted `github-packages-credentials` Secret through the Task's
optional `github-packages-auth` workspace. The workspace is read-only and
isolated to the trusted publishing step; pull-request and merge-queue CI never
binds it. Its classic PAT has only `write:packages`. `GHCR_USER` and
`GHCR_TOKEN` use that credential, while `GH_TOKEN` remains the short-lived App
token for GitHub API operations.

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

## Cutover evidence

The xlab cutover was proven on 2026-08-19 before GitHub Actions retirement:

- xlab-deployments PR 62 installed the optional, read-only package-auth
  workspace on `nix-run-github-v2` and the SOPS-managed
  `github-packages-credentials` Secret;
- EndlessFS PR 17 passed pull-request PipelineRun `endlessfs-ci-xcwpj` and
  merge-queue PipelineRun `endlessfs-ci-v7wwj`, including real Chromium coverage;
- every cache-consuming task and affinity assistant used Sirius with
  `storage.xlab.now/fast-local=true`, while the shared Git and Nix PVCs and each
  per-run source PVC were Bound;
- main commit `113d739334fd6b095210b6cb91fb779439566226` completed
  `endlessfs-container-7wzsv`; its immutable `sha-113d739334fd6b095210b6cb91fb779439566226`
  and moving `edge` tags both resolved to manifest
  `sha256:ebd23aba9664c8f71c4c647bde4af673c68fe34b1bb25decb10c7b5fec7e1862`;
- the package workspace was mounted only in the trusted publishing step, not in
  Nix-store seeding, pull-request CI, or merge-queue CI.

The checked-in ruleset requires the App-owned
`tekton-xlab / endlessfs-ci-` context from GitHub App integration `949094`.
`nix run .#pipeline-policy` rejects reintroduced GitHub Actions workflows or an
Actions-only Dependabot configuration. The Darwin job remains retired and must
not be restored.
