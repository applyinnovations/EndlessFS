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

All active runs run in xlab's `tekton-buildkit` namespace on the standard v1
Tasks only: `git-clone-v1` (exact revision, no mirror), `nix-v1` (every Nix
job), `oci-publish-v1` (the only registry credential) and
`nix-cache-publish-v1` (the only Nix cache pusher). Each `nix-v1` pod has its
own Nix store and scratch, builds every derivation with `sandbox = true` and no
fallback, is privileged only inside a pod user namespace
(`taskRunSpecs[].podTemplate.hostUsers: false`, enforced by admission) and
holds no credentials. Stores come from xlab's shared Nix cache; push runs
(merge queue, `main`, tags) export the paths it lacks to a per-run
`nix-cache-export` volume, and `publish-cache` in `finally`, running as
`nix-cache-publisher`, pushes them. Runs bind only per-run `fast-local`
volumes, so they select `storage.xlab.now/fast-local=true` (Sirius).

Pull-request CI runs the fast policy gate, then the full Nix gate, then
coverage, so Chromium gets predictable resources. The full gate requests
6 CPU/12 GiB and may burst to 12 CPU/24 GiB; coverage requests 8 CPU/8 GiB and
may burst to 16 CPU/16 GiB.

## Authentication boundary

No pod that runs repository code holds a credential. Building and publishing
are separate Tasks that share only a per-run `artifacts` volume:

- `endlessfs-container`: `build` (`nix-v1`, `IMAGE=.#container`) runs the
  advisory freshness check and exports `image.oci.tar`; `publish`
  (`oci-publish-v1`) checks its `ARCHIVE_SHA256` and pushes the commit SHA,
  `sha-<commit>` and `edge` tags.
- `endlessfs-release`: `verify` and `build` (`nix-v1`) run side by side;
  `build` copies the `.#release` assets to `artifacts/release` and exports the
  image. After both pass, `publish-image` (`oci-publish-v1`) pushes the commit
  SHA, `vX.Y.Z` and `latest` tags, then `github-release` creates the release
  with generated notes (or, on a rerun, replaces its assets). That Task is
  defined inline: a standard-library Go program compiled by a pinned Go image,
  holding the PaC App token, reading only `artifacts/release`.

`oci-publish-v1` reads the `github-packages-docker-config` dockerconfigjson
Secret (GHCR, classic PAT with only `write:packages`; GHCR rejects App tokens).
The App token in the per-run PaC Secret is used for cloning and, in
`github-release`, for release creation and asset upload; the App needs
`contents: write`. Neither credential is bound in pull-request or merge-queue
CI. `nix run .#pipeline-policy` enforces these boundaries.

Applying `.github/rulesets/*.json` remains an
explicit administrator action with a separate, short-lived Administration token
through `nix run .#repository-policy -- apply`.

## Darwin retirement

The Darwin file is deliberately a `Pipeline`, not a `PipelineRun`. It has no
`pipelinesascode.tekton.dev/on-*` annotation and no active definition references
it. Its only possible manual behavior is a Linux deprecation message. It does
not reference xlab's `namespace-macos-fastlane` task or any Namespace CLI,
credential, cache, or runner, so ordinary PaC events cannot allocate a Mac.

## Cutover evidence

The xlab cutover (on the since-retired shared-store Tasks) was proven on 2026-08-19 before GitHub Actions retirement:

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
