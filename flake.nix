{
  description = "EndlessFS — a private, provider-neutral cloud drive";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs =
    {
      self,
      nixpkgs,
    }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      # These older guarantee tests also contain exact economics assertions.
      # The inventory policy checks both this selection and its package roots.
      providerBudgetTestPattern = "ProviderBudget|^TestConsistencyDomainCompactionPersistsValuesAndOutcomesWithoutClaimReads$|^TestNamespaceBatchTrashPublishesScaleEdgesThroughOneHead$|^TestNamespaceBatchRestorePublishesScaleEdgesThroughOneHead$|^TestUploadPlanningWarmRequestBudgetIsIndependentOfBatchCardinality$";
      providerBudgetPackages = "./internal/portable ./internal/drive ./internal/identity ./internal/theme ./internal/httpapi ./internal/preview/...";
      goFor =
        pkgs:
        pkgs.go_1_26.overrideAttrs (_old: {
          version = "1.26.10";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go1.26.10.src.tar.gz";
            hash = "sha256-/WPog7ovK/ypPBZGsx6+9j4rHk0rMXyXidl2po2B828=";
          };
        });
      # Disposable compiler artifacts only. No application source or test run
      # enters this seed; import preflight remains an independent fail-fast gate.
      dependencyCompileCacheFor =
        pkgs: moduleClosure:
        let
          go = goFor pkgs;
          goMod = builtins.path {
            path = ./go.mod;
            name = "endlessfs-go.mod";
          };
          goSum = builtins.path {
            path = ./go.sum;
            name = "endlessfs-go.sum";
          };
          helper = builtins.path {
            path = ./tools/cache-seed/main.go;
            name = "endlessfs-cache-seed.go";
          };
        in
        pkgs.runCommand "endlessfs-dependency-compile-cache-v3"
          {
            nativeBuildInputs = [
              go
              pkgs.stdenv.cc
            ];
          }
          ''
            export GOCACHE="$TMPDIR/go-cache"
            export GOMODCACHE="$TMPDIR/go-mod-cache"
            export GOENV=off GOWORK=off GOTOOLCHAIN=local
            export GOPROXY=off GOSUMDB=off GOFLAGS=-mod=vendor
            mkdir -p source
            # Go's module-index action keys include source file mtimes. Keep
            # the immutable inputs' timestamps instead of build-time copies.
            # Timestamps-only avoids preserving foreign ownership or modes.
            ${pkgs.coreutils}/bin/cp --preserve=timestamps ${goMod} source/go.mod
            ${pkgs.coreutils}/bin/cp --preserve=timestamps ${goSum} source/go.sum
            ${pkgs.coreutils}/bin/cp --preserve=timestamps -R ${moduleClosure} source/vendor
            chmod -R u+w source
            cd source
            CGO_ENABLED=0 go build -p "''${NIX_BUILD_CORES:-1}" std
            ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
              CGO_ENABLED=0 go build -p "''${NIX_BUILD_CORES:-1}" ./vendor/...
            ''}
            CGO_ENABLED=1 go build -p "''${NIX_BUILD_CORES:-1}" -race std
            ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
              CGO_ENABLED=1 go build -p "''${NIX_BUILD_CORES:-1}" -race ./vendor/...
            ''}

            # All Go commands finish before the pinned metadata normalizer.
            # This helper has no application imports and uses the same compiler.
            mkdir -p "$out/bin"
            CGO_ENABLED=0 go build -o "$out/bin/cache-seed" ${helper}
            "$out/bin/cache-seed" normalize "$GOCACHE"
            cp -R "$GOCACHE" "$out/cache"
            printf '%s\n' ${
              pkgs.lib.escapeShellArg (
                builtins.toJSON {
                  schema = 1;
                  compiler = toString go;
                  nativeCompiler = toString pkgs.stdenv.cc;
                  vendor = toString moduleClosure;
                  vendorCompiled = pkgs.stdenv.hostPlatform.isLinux;
                  goMod = toString goMod;
                  goSum = toString goSum;
                  helper = toString helper;
                  normal = {
                    cgo = 0;
                    flags = [ "-mod=vendor" ];
                  };
                  race = {
                    cgo = 1;
                    flags = [
                      "-mod=vendor"
                      "-race"
                    ];
                  };
                  actionMetadata = "go1.26-v1-175bytes-time-zero";
                  sourceCopy = "preserve-retained-input-timestamps";
                  installation = "private-copy-close-atomic-link-noreplace-v2";
                }
              )
            } > "$out/seed-inputs.json"
          '';
      installDependencyCompileCache = seed: scratch: ''
        # Retain the caller's existing GOCACHE and its warm application
        # entries. Atomically publish complete private copies without replace;
        # never hardlink the source, chmod warm files, or write through the store.
        if ! endlessfs_seed_cache="$(go env GOCACHE 2>/dev/null)"; then
          endlessfs_seed_cache="lookup_failed"
        fi
        if [[ "$endlessfs_seed_cache" == /* ]]; then
          # No stamp: Go may trim individual entries between invocations.
          # The Go installer preserves existing files and executable bits.
          if ! ${seed}/bin/cache-seed install ${seed}/cache "$endlessfs_seed_cache"; then
            # Seeding is optional. Do not chmod, repair or reuse a rejected
            # destination; run every original check in private fresh scratch.
            endlessfs_fresh_cache="$(mktemp -d "${scratch}/endlessfs-fresh-compile.XXXXXX")"
            export GOCACHE="$endlessfs_fresh_cache"
            echo "dependency compilation seed: unavailable_fresh_compilation"
          fi
        elif [[ "$endlessfs_seed_cache" == "lookup_failed" ]]; then
          endlessfs_fresh_cache="$(mktemp -d "${scratch}/endlessfs-fresh-compile.XXXXXX")"
          export GOCACHE="$endlessfs_fresh_cache"
          echo "dependency compilation seed: lookup_failed_fresh_compilation"
        else
          # In particular, preserve an explicitly disabled GOCACHE=off.
          echo "dependency compilation seed: skipped_disabled_or_nonabsolute_cache"
        fi
      '';
      vulndbFor =
        pkgs:
        pkgs.runCommand "endlessfs-go-vulnerability-database" { nativeBuildInputs = [ (goFor pkgs) ]; } ''
          export GOCACHE="$TMPDIR/go-cache"
          export GO111MODULE=off GOPROXY=off GOSUMDB=off CGO_ENABLED=0
          go run ${./tools/vulndb/main.go} extract ${./security/vulndb} "$out"
          cp ${./security/vulndb/LICENSE} "$out/LICENSE"
          cp ${./security/vulndb/README.md} "$out/README.md"
          cp ${./security/vulndb/snapshot.json} "$out/snapshot.json"
        '';
      headlessBrowserFor =
        pkgs:
        let
          fontConfig = pkgs.makeFontsConf {
            fontDirectories = [ pkgs.dejavu_fonts ];
          };
        in
        pkgs.runCommand "endlessfs-headless-browser"
          {
            nativeBuildInputs = [
              pkgs.makeWrapper
            ];
          }
          ''
            mkdir -p "$out/bin"
            makeWrapper ${pkgs.chromium}/bin/chromium "$out/bin/chrome-headless-shell" \
              --set FONTCONFIG_FILE ${fontConfig}
          '';
      coverageSourceFor =
        pkgs:
        let
          lib = pkgs.lib;
          relativePath = path: lib.removePrefix (toString ./. + "/") (toString path);
        in
        lib.cleanSourceWith {
          src = ./.;
          filter =
            path: _type:
            let
              relative = relativePath path;
            in
            relative == "go.mod"
            || relative == "go.sum"
            # Exact-budget inventory verifies the shared Nix gate selection.
            || relative == "flake.nix"
            || relative == "cmd"
            || lib.hasPrefix "cmd/" relative
            || relative == "internal"
            || lib.hasPrefix "internal/" relative
            || relative == "tools"
            || lib.hasPrefix "tools/" relative;
        };
      dependencyPolicyCommand = moduleClosure: ''
        go run ./tools/vulndb check security/vulndb

        dependency_inventory="$(mktemp -t endlessfs-dependencies.XXXXXX)"
        trap 'rm -f "$dependency_inventory"' EXIT
        module_closure=${nixpkgs.lib.escapeShellArg (toString moduleClosure)}
        awk '/^# / && $2 != "=>" { print $2, $3 }' "$module_closure/modules.txt" | LC_ALL=C sort -u > "$dependency_inventory"
        test -s "$dependency_inventory"
        while read -r module _version; do
          module_root="$module_closure/$module"
          if ! find "$module_root" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) -print -quit | grep -q .; then
            echo "locked module lacks a root license file: $module" >&2
            exit 1
          fi
        done < "$dependency_inventory"
        echo "dependency policy: $(wc -l < "$dependency_inventory" | tr -d ' ') locked modules with licenses"
      '';
      pipelinePolicyCommand = ''
        active_pipelines="
          .tekton/endlessfs-ci.yaml
          .tekton/endlessfs-container.yaml
          .tekton/endlessfs-release.yaml
        "

        if [ -d .github/workflows ] && [ -n "$(rg --files .github/workflows)" ]; then
          echo "GitHub Actions workflows must remain retired after the Tekton cutover" >&2
          exit 1
        fi
        if [ -e .github/dependabot.yml ]; then
          echo "the GitHub-Actions-only Dependabot configuration must remain retired" >&2
          exit 1
        fi

        for pipeline in $active_pipelines; do
          test -f "$pipeline" || {
            echo "missing active Tekton PipelineRun: $pipeline" >&2
            exit 1
          }
          yq -e '.apiVersion == "tekton.dev/v1" and .kind == "PipelineRun"' "$pipeline" >/dev/null
          yq -e '.metadata.annotations."pipelinesascode.tekton.dev/target-namespace" == "tekton-buildkit"' "$pipeline" >/dev/null
          yq -e '.spec.taskRunTemplate.podTemplate.nodeSelector."storage.xlab.now/fast-local" == "true"' "$pipeline" >/dev/null
          yq -e '.spec.taskRunTemplate.podTemplate.automountServiceAccountToken == false' "$pipeline" >/dev/null
          yq -e '.spec.workspaces[] | select(.name == "source") | .volumeClaimTemplate.spec.resources.requests.storage == "10Gi"' "$pipeline" >/dev/null
          # Only the standard xlab v1 Tasks: no shared store, mirror or layer
          # cache volumes, and no legacy Task that mixes credentials and source.
          unexpected_tasks="$(yq -r '(.spec.pipelineSpec.tasks + .spec.pipelineSpec.finally)[] | select(has("taskRef")) | .taskRef.params[] | select(.name == "name") | .value' "$pipeline" \
            | rg -vx 'git-clone-v1|nix-v1|oci-publish-v1|nix-cache-publish-v1' || true)"
          if [ -n "$unexpected_tasks" ]; then
            echo "$pipeline must use only the standard v1 Tasks, not: $unexpected_tasks" >&2
            exit 1
          fi
          if yq -e '.spec.workspaces[] | select(has("persistentVolumeClaim"))' "$pipeline" >/dev/null 2>&1; then
            echo "$pipeline must not bind a shared PersistentVolumeClaim" >&2
            exit 1
          fi
          # Every sandboxed Nix Task runs in a pod user namespace and holds no
          # credential workspace.
          for task in $(yq -r '.spec.pipelineSpec.tasks[] | select(.taskRef.params[] | select(.name == "name" and .value == "nix-v1")) | .name' "$pipeline"); do
            yq -e ".spec.taskRunSpecs[] | select(.pipelineTaskName == \"$task\") | .podTemplate.hostUsers == false" "$pipeline" >/dev/null
            yq -e ".spec.pipelineSpec.tasks[] | select(.name == \"$task\") | [.workspaces[].name] - [\"source\", \"artifacts\", \"nix-cache-export\"] | length == 0" "$pipeline" >/dev/null
          done
          # Only publish-cache, under the nix-cache-publisher account, writes
          # the shared Nix cache.
          yq -e '.spec.pipelineSpec.finally[] | select(.name == "publish-cache") | .taskRef.params[] | select(.name == "name") | .value == "nix-cache-publish-v1"' "$pipeline" >/dev/null
          yq -e '.spec.taskRunSpecs[] | select(.pipelineTaskName == "publish-cache") | .serviceAccountName == "nix-cache-publisher"' "$pipeline" >/dev/null
          # The registry credential reaches only oci-publish-v1.
          yq -e '[.spec.pipelineSpec.tasks[] | select(.workspaces[]? | .workspace == "ghcr-docker-config") | .taskRef.params[] | select(.name == "name") | .value] | all_c(. == "oci-publish-v1")' "$pipeline" >/dev/null
          if rg -ni 'gke|drive\.endlessfs\.com|namespace-macos-fastlane|runs-on:[[:space:]]*macos' "$pipeline"; then
            echo "active CI must stay on xlab Linux compute: $pipeline" >&2
            exit 1
          fi
        done

        duplicate_generate_names="$({
          for pipeline in $active_pipelines; do
            yq -r '.metadata.generateName' "$pipeline"
          done
        } | LC_ALL=C sort | uniq -d)"
        if [ -n "$duplicate_generate_names" ]; then
          echo "active PaC PipelineRuns must have unique metadata.generateName values:" >&2
          printf '%s\n' "$duplicate_generate_names" >&2
          exit 1
        fi

        yq -e '.metadata.generateName == "endlessfs-ci-"' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.metadata.annotations."pipelinesascode.tekton.dev/on-cel-expression" | contains("event == \"pull_request\" && target_branch == \"main\"")' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '(.metadata.annotations."pipelinesascode.tekton.dev/on-cel-expression" | contains("event == \"push\"")) and (.metadata.annotations."pipelinesascode.tekton.dev/on-cel-expression" | contains("target_branch.startsWith(\"refs/heads/gh-readonly-queue/main/\")"))' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.metadata.annotations | has("pipelinesascode.tekton.dev/on-event") == false and has("pipelinesascode.tekton.dev/on-target-branch") == false' .tekton/endlessfs-ci.yaml >/dev/null

        yq -e '.metadata.annotations."pipelinesascode.tekton.dev/on-event" == "[push]"' .tekton/endlessfs-container.yaml >/dev/null
        yq -e '.metadata.annotations."pipelinesascode.tekton.dev/on-target-branch" == "[main]"' .tekton/endlessfs-container.yaml >/dev/null
        yq -e '.metadata.annotations."pipelinesascode.tekton.dev/on-event" == "[push]"' .tekton/endlessfs-release.yaml >/dev/null
        yq -e '.metadata.annotations."pipelinesascode.tekton.dev/on-target-branch" == "[refs/tags/v*.*.*]"' .tekton/endlessfs-release.yaml >/dev/null
        yq -e '.spec.params[] | select(.name == "release_tag") | .value == "{{ git_tag }}"' .tekton/endlessfs-release.yaml >/dev/null
        for pipeline in $active_pipelines; do
          advisory_script="$(yq -r '.spec.pipelineSpec.tasks[].params[]? | select(.name == "SCRIPT") | .value' "$pipeline")"
          if ! printf '%s\n' "$advisory_script" | rg -Fx 'nix run .#security-freshness' >/dev/null; then
            echo "$pipeline must run the upstream-advisory freshness check" >&2
            exit 1
          fi
        done
        release_verify_script="$(yq -r '.spec.pipelineSpec.tasks[] | select(.name == "verify") | .params[] | select(.name == "SCRIPT") | .value' .tekton/endlessfs-release.yaml)"
        printf '%s\n' "$release_verify_script" | rg -F 'nix run .#test-migration -- "$release_tag"' >/dev/null

        # Images are built without credentials and pushed by oci-publish-v1
        # only after the build (and, for releases, verification) succeeded.
        check_image_publish() {
          pipeline="$1"
          task="$2"
          tags="$3"
          shift 3
          yq -e '.spec.pipelineSpec.tasks[] | select(.name == "build") | .params[] | select(.name == "IMAGE") | .value == ".#container"' "$pipeline" >/dev/null
          yq -e ".spec.pipelineSpec.tasks[] | select(.name == \"$task\") | (.params[] | select(.name == \"IMAGE\") | .value == \"ghcr.io/applyinnovations/endlessfs\")" "$pipeline" >/dev/null
          yq -e ".spec.pipelineSpec.tasks[] | select(.name == \"$task\") | (.params[] | select(.name == \"ARCHIVE_SHA256\") | .value == \"\$(tasks.build.results.ARCHIVE_SHA256)\")" "$pipeline" >/dev/null
          actual_tags="$(yq -r ".spec.pipelineSpec.tasks[] | select(.name == \"$task\") | .params[] | select(.name == \"EXTRA_TAGS\") | .value" "$pipeline" | sed '/^$/d')"
          if [ "$actual_tags" != "$tags" ]; then
            echo "$pipeline $task must publish exactly: $tags" >&2
            exit 1
          fi
          for dependency in "$@"; do
            yq -e ".spec.pipelineSpec.tasks[] | select(.name == \"$task\") | .runAfter[] | select(. == \"$dependency\")" "$pipeline" >/dev/null
          done
        }
        # Tekton, not the shell, expands these tag expressions.
        # shellcheck disable=SC2016
        check_image_publish .tekton/endlessfs-container.yaml publish "$(printf '%s\n' 'sha-$(tasks.clone.results.COMMIT_SHA)' edge)" build
        # shellcheck disable=SC2016
        check_image_publish .tekton/endlessfs-release.yaml publish-image "$(printf '%s\n' '$(params.release_tag)' latest)" build verify
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "publish-image") | .retries >= 2' .tekton/endlessfs-release.yaml >/dev/null
        for pipeline in .tekton/endlessfs-container.yaml .tekton/endlessfs-release.yaml; do
          yq -e '.spec.workspaces[] | select(.name == "ghcr-docker-config") | .secret.secretName == "ghcr-push-credentials"' "$pipeline" >/dev/null
        done

        # The GitHub release Task holds the App token, so it reads only the
        # built assets and runs after the image is published.
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "github-release") | .retries >= 2' .tekton/endlessfs-release.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "github-release") | .runAfter | (length == 1 and .[0] == "publish-image")' .tekton/endlessfs-release.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "github-release") | .workspaces | (length == 1 and .[0].name == "assets" and .[0].workspace == "artifacts" and .[0].subPath == "release")' .tekton/endlessfs-release.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "github-release") | .taskSpec.steps[] | .image | test("@sha256:[0-9a-f]{64}$")' .tekton/endlessfs-release.yaml >/dev/null
        if [ "$(rg -c 'secretKeyRef' .tekton/endlessfs-release.yaml)" != 1 ]; then
          echo "only the GitHub release Task may read the App token" >&2
          exit 1
        fi
        if rg -n 'ghcr-docker-config|github-packages|secretKeyRef' .tekton/endlessfs-ci.yaml; then
          echo "pull-request and merge-queue CI must not receive publishing credentials" >&2
          exit 1
        fi
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "nix-checks") | .runAfter | (length == 1 and .[0] == "fast-checks")' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "coverage") | .runAfter[] | select(. == "fast-checks")' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "coverage") | .runAfter[] | select(. == "nix-checks")' .tekton/endlessfs-ci.yaml >/dev/null

        darwin_pipeline=.tekton/endlessfs-darwin-smoke.disabled.yaml
        test -f "$darwin_pipeline" || {
          echo "missing retired Darwin workflow definition: $darwin_pipeline" >&2
          exit 1
        }
        yq -e '.apiVersion == "tekton.dev/v1" and .kind == "Pipeline"' "$darwin_pipeline" >/dev/null
        yq -e '.metadata.labels."endlessfs.dev/workflow-state" == "deprecated-disabled"' "$darwin_pipeline" >/dev/null
        if yq -e '.metadata.annotations."pipelinesascode.tekton.dev/on-event" // .metadata.annotations."pipelinesascode.tekton.dev/on-target-branch" // .metadata.annotations."pipelinesascode.tekton.dev/on-cel-expression"' "$darwin_pipeline" >/dev/null 2>&1; then
          echo "retired Darwin workflow must not have a Pipelines-as-Code trigger" >&2
          exit 1
        fi
        if rg -ni 'namespace-macos-fastlane|nsc[[:space:]]|macos/[a-z0-9]|runs-on:[[:space:]]*macos' "$darwin_pipeline"; then
          echo "retired Darwin workflow must not run or allocate macOS compute" >&2
          exit 1
        fi

        echo "Tekton policy: xlab Linux CI active; Darwin smoke deprecated and disabled"
      '';
      fuzzSmokeCommand = ''
        go test ./internal/config -run '^$' -fuzz '^FuzzParse$' -fuzztime "$fuzztime"
        go test ./internal/domain -run '^$' -fuzz '^FuzzParseUserPath$' -fuzztime "$fuzztime"
        go test ./internal/domain -run '^$' -fuzz '^FuzzParseUserPathEncodingBoundary$' -fuzztime "$fuzztime"
        go test ./internal/state -run '^$' -fuzz '^FuzzStrictJSONDecoder$' -fuzztime "$fuzztime"
        go test ./internal/state -run '^$' -fuzz '^FuzzPaginationCursorBoundary$' -fuzztime "$fuzztime"
        go test ./internal/model -run '^$' -fuzz '^FuzzPersistenceRecordDecoders$' -fuzztime "$fuzztime"
        go test ./internal/auth -run '^$' -fuzz '^FuzzWebAuthnResponseBoundary$' -fuzztime "$fuzztime"
        go test ./internal/drive -run '^$' -fuzz '^FuzzShareSubtreeResolution$' -fuzztime "$fuzztime"
        go test ./internal/provider/memory -run '^$' -fuzz '^FuzzRangeAndContentDisposition$' -fuzztime "$fuzztime"
        go test ./internal/logging -run '^$' -fuzz '^FuzzStructuredLogRedaction$' -fuzztime "$fuzztime"
        go test ./internal/theme -run '^$' -fuzz '^FuzzThemeBoundaries$' -fuzztime "$fuzztime"
        go test ./internal/preview/imagegen -run '^$' -fuzz '^FuzzGeneratorMalformed$' -fuzztime "$fuzztime"
      '';
      raceTestCommand = ''
        # The migration transport matrix deterministically restarts the entire
        # schema chain at every provider boundary. The portable package also
        # contains the remaining migration matrix and every ordinary state
        # transition test. Give the exhaustive transport matrix exclusive
        # resources, then run the two disjoint portable remainder sets alongside
        # all other packages. This avoids both a single serial package schedule
        # and resource contention between the three expensive portable sets.
        # Every repository test still runs exactly once and every shard must
        # succeed.
        exhaustive_migration_test='^TestMigrationRecoversFromEveryObjectTransportInterruption$'
        portable_migration_tests='^TestMigration'
        other_packages=()
        while IFS= read -r package; do
          if [[ "$package" != */internal/portable ]]; then
            other_packages+=("$package")
          fi
        done < <(go list ./...)

        # This matrix saturates a local builder by itself. Running it beside the
        # other portable shards makes every process slower without increasing
        # test coverage, so complete it before admitting the parallel phase.
        go test -race -timeout=30m -count=1 ./internal/portable -run "$exhaustive_migration_test"

        race_pids=()
        go test -race -timeout=30m -count=1 ./internal/portable -run "$portable_migration_tests" -skip "$exhaustive_migration_test" &
        race_pids+=("$!")
        go test -race -timeout=30m -count=1 ./internal/portable -skip "$portable_migration_tests" &
        race_pids+=("$!")
        go test -race -timeout=30m -count=1 "''${other_packages[@]}" &
        race_pids+=("$!")

        race_status=0
        for race_pid in "''${race_pids[@]}"; do
          if ! wait "$race_pid"; then
            race_status=1
          fi
        done
        test "$race_status" -eq 0
      '';
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          lib = pkgs.lib;
          go = goFor pkgs;
          version = self.shortRev or self.dirtyShortRev or "dev";
          projectSource = lib.cleanSource ./.;
          goSource = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: _type:
              let
                relative = lib.removePrefix (toString ./. + "/") (toString path);
              in
              relative == "go.mod"
              || relative == "go.sum"
              || relative == "cmd"
              || lib.hasPrefix "cmd/" relative
              || relative == "internal"
              || lib.hasPrefix "internal/" relative
              || relative == "tools"
              || relative == "tools/theme"
              || lib.hasPrefix "tools/theme/" relative;
          };

          themePreBuild =
            themeBundles:
            lib.optionalString (themeBundles != [ ]) ''
              go run ./tools/theme check ${
                lib.escapeShellArgs (map (bundle: "${bundle}") themeBundles)
              } >/dev/null
              go run ./tools/theme compile-go ${
                lib.escapeShellArgs (map (bundle: "${bundle}") themeBundles)
              } > "$TMPDIR/custom_build_inputs.go"
              mv "$TMPDIR/custom_build_inputs.go" internal/theme/custom_build_inputs.go
              gofmt -w internal/theme/custom_build_inputs.go
            '';

          dependencyDigestBuildHook = ''
            if [ -f vendor/modules.txt ]; then
              dependency_inventory="$TMPDIR/endlessfs-dependencies.txt"
              awk '/^# / && $2 != "=>" { print $2, $3 }' vendor/modules.txt \
                | LC_ALL=C sort -u > "$dependency_inventory"
              printf '%s\n' 'libraw 0.22.1' >> "$dependency_inventory"
              LC_ALL=C sort -u -o "$dependency_inventory" "$dependency_inventory"
              dependency_digest="$(sha256sum "$dependency_inventory" | cut -d ' ' -f 1)"
              ldflags+=("-X=github.com/applyinnovations/endlessfs/internal/preview.DependencyInventoryDigest=$dependency_digest")
            fi
          '';

          mkEndlessFS =
            {
              themeBundles ? [ ],
            }:
            (pkgs.buildGoModule.override { inherit go; }) {
              pname = "endlessfs";
              inherit version;
              src = goSource;
              subPackages = [ "cmd/endlessfs" ];
              vendorHash = "sha256-rbtt30G5YMAV0PfLcOayu5hZfzPVjs4J3Bj/wKJkZH4=";
              # Keep the fixed-output name stable while the actual imported
              # dependency closure, including test packages, remains unchanged.
              overrideModAttrs = _final: _previous: {
                name = "endlessfs-go-modules";
              };
              doCheck = false;
              env.CGO_ENABLED = 0;
              preBuild = (themePreBuild themeBundles) + dependencyDigestBuildHook;
              ldflags = [
                "-s"
                "-w"
                "-X=main.version=${version}"
              ];
              passthru = { inherit themeBundles; };
              postInstall = ''
                install -D -m 0555 ${pkgs.libraw}/bin/dcraw_emu "$out/bin/endlessfs-raw-decoder"
              '';
            };

          endlessfs = lib.makeOverridable mkEndlessFS { };
          dependencyCompileCache = dependencyCompileCacheFor pkgs endlessfs.goModules;
          functionalCoverage = import ./tools/functionalcoverage/default.nix {
            inherit pkgs go;
            fullSource = self.outPath;
            coverageSource = coverageSourceFor pkgs;
            vendor = endlessfs.goModules;
            seed = dependencyCompileCache;
            browser = headlessBrowserFor pkgs;
          };

          linuxArchitecture = if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "amd64";
          linuxSystem = if pkgs.stdenv.hostPlatform.isAarch64 then "aarch64-linux" else "x86_64-linux";
          linuxPkgs = nixpkgs.legacyPackages.${linuxSystem};
          releaseRawDecoder =
            if pkgs.stdenv.hostPlatform.isLinux then
              pkgs.pkgsStatic.libraw.overrideAttrs (_: {
                pname = "endlessfs-raw-decoder";
                outputs = [ "out" ];
                buildPhase = ''
                  runHook preBuild
                  make -j"$NIX_BUILD_CORES" bin/dcraw_emu
                  runHook postBuild
                '';
                installPhase = ''
                  runHook preInstall
                  install -D -m 0555 bin/dcraw_emu "$out/bin/dcraw_emu"
                  runHook postInstall
                '';
              })
            else
              pkgs.libraw;
          mkLinuxBinary =
            {
              themeBundles ? [ ],
            }:
            pkgs.stdenvNoCC.mkDerivation {
              pname = "endlessfs-linux-${linuxArchitecture}";
              inherit version;
              src = goSource;
              nativeBuildInputs = [ go ];
              dontConfigure = true;
              dontFixup = true;
              buildPhase = ''
                runHook preBuild
                export GOCACHE="$TMPDIR/go-cache"
                cp -R ${endlessfs.goModules} vendor
                export GOFLAGS=-mod=vendor
                ${themePreBuild themeBundles}
                dependency_inventory="$TMPDIR/endlessfs-dependencies.txt"
                awk '/^# / && $2 != "=>" { print $2, $3 }' vendor/modules.txt \
                  | LC_ALL=C sort -u > "$dependency_inventory"
                printf '%s\n' 'libraw 0.22.1' >> "$dependency_inventory"
                LC_ALL=C sort -u -o "$dependency_inventory" "$dependency_inventory"
                dependency_digest="$(sha256sum "$dependency_inventory" | cut -d ' ' -f 1)"
                export GOOS=linux
                export GOARCH=${linuxArchitecture}
                export CGO_ENABLED=0
                go build -trimpath -buildvcs=false \
                  -ldflags "-s -w -buildid= -X=main.version=${version} -X=github.com/applyinnovations/endlessfs/internal/preview.DependencyInventoryDigest=$dependency_digest" \
                  -o endlessfs ./cmd/endlessfs
                runHook postBuild
              '';
              installPhase = ''
                runHook preInstall
                install -D -m 0555 endlessfs "$out/bin/endlessfs"
                install -D -m 0555 ${linuxPkgs.libraw}/bin/dcraw_emu "$out/bin/endlessfs-raw-decoder"
                runHook postInstall
              '';
              passthru = { inherit themeBundles; };
            };

          linuxBinary = lib.makeOverridable mkLinuxBinary { };

          containerRoot = pkgs.runCommandLocal "endlessfs-container-root" { } ''
            mkdir -p "$out/bin" "$out/etc/ssl/certs" "$out/share"
            cp ${linuxBinary}/bin/endlessfs "$out/bin/endlessfs"
            cp ${linuxBinary}/bin/endlessfs-raw-decoder "$out/bin/endlessfs-raw-decoder"
            cp ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt "$out/etc/ssl/certs/ca-bundle.crt"
            cp -R ${pkgs.tzdata}/share/zoneinfo "$out/share/zoneinfo"
            chmod 0555 "$out/bin/endlessfs" "$out/bin/endlessfs-raw-decoder"
          '';

          container = pkgs.dockerTools.buildLayeredImage {
            name = "endlessfs";
            tag = version;
            architecture = linuxArchitecture;
            created = "1970-01-01T00:00:01Z";
            contents = [ containerRoot ];
            config = {
              Entrypoint = [ "/bin/endlessfs" ];
              User = "65532:65532";
              Env = [
                "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
                "ZONEINFO=/share/zoneinfo"
              ];
              ExposedPorts."8080/tcp" = { };
              Labels = {
                "org.opencontainers.image.title" = "EndlessFS";
                "org.opencontainers.image.description" = "EndlessFS control plane";
                "org.opencontainers.image.revision" = version;
                "org.opencontainers.image.source" = "https://github.com/applyinnovations/EndlessFS";
                "org.opencontainers.image.licenses" = "Apache-2.0 AND (CDDL-1.0 OR LGPL-2.1-or-later)";
              };
            };
          };

          themeInventory =
            pkgs.runCommand "endlessfs-theme-inventory-${version}" { nativeBuildInputs = [ go ]; }
              ''
                cp -R ${goSource} source
                chmod -R u+w source
                cd source
                export GOCACHE="$TMPDIR/go-cache"
                export CGO_ENABLED=0
                cp -R ${endlessfs.goModules} vendor
                export GOFLAGS=-mod=vendor
                go run ./tools/theme inventory > "$out"
              '';

          dependencyInventory =
            pkgs.runCommand "endlessfs-dependency-inventory-${version}" { nativeBuildInputs = [ pkgs.gawk ]; }
              ''
                awk '/^# / && $2 != "=>" { print $2, $3 }' \
                  ${endlessfs.goModules}/modules.txt | LC_ALL=C sort -u > "$out"
                printf '%s\n' 'libraw 0.22.1' >> "$out"
                LC_ALL=C sort -u -o "$out" "$out"
              '';

          capabilityInventory =
            pkgs.runCommand "endlessfs-capabilities-${version}.json" { nativeBuildInputs = [ pkgs.jq ]; }
              ''
                dependency_digest="$(sha256sum ${dependencyInventory} | cut -d ' ' -f 1)"
                jq -n \
                  --arg applicationVersion '${version}' \
                  --arg dependencyInventorySHA256 "$dependency_digest" \
                  '{
                    applicationVersion: $applicationVersion,
                    previewSpecification: "v1.1",
                    profile: "images",
                    packagedCapabilities: ["image"],
                    acceptedImageMediaTypes: ["image/gif", "image/jpeg", "image/png", "image/webp", "image/x-adobe-dng", "image/x-canon-cr2", "image/x-canon-cr3", "image/x-fuji-raf", "image/x-nikon-nef", "image/x-olympus-orf", "image/x-panasonic-rw2", "image/x-pentax-pef", "image/x-sony-arw"],
                    artifactMediaTypes: ["image/webp"],
                    imageRecipeID: "image-webp-q80-v1",
                    packagedImageDecoders: ["go-standard-library", "deepteams-webp-1.2.6", "libraw-0.22.1"],
                    dependencyInventorySHA256: $dependencyInventorySHA256
                  }' > "$out"
              '';

          release =
            pkgs.runCommand "endlessfs-release-${version}-${system}"
              {
                nativeBuildInputs = [
                  pkgs.coreutils
                  pkgs.findutils
                  pkgs.gnutar
                  pkgs.gzip
                  pkgs.gawk
                  pkgs.jq
                ];
              }
              ''
                mkdir -p "$out" staging
                cp ${endlessfs}/bin/endlessfs staging/endlessfs
                cp ${releaseRawDecoder}/bin/dcraw_emu staging/endlessfs-raw-decoder
                cp ${container} "staging/endlessfs-container-${version}.tar.gz"
                cp ${projectSource}/LICENSE staging/LICENSE
                cp ${projectSource}/README.md staging/README.md
                cp ${projectSource}/docs/v1-evidence.md staging/V1-EVIDENCE.md
                cp ${projectSource}/docs/v1-release-notes.md staging/RELEASE-NOTES.md
                cp ${themeInventory} staging/THEMES.json
                cp ${dependencyInventory} staging/DEPENDENCIES.txt
                cp ${capabilityInventory} staging/CAPABILITIES.json
                (
                  cd ${endlessfs.goModules}
                  find . -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) -print0 \
                    | LC_ALL=C sort -z \
                    | xargs -0 sha256sum
                ) > staging/DEPENDENCY-LICENSES.sha256
                for license in LICENSE.CDDL LICENSE.LGPL; do
                  digest="$(sha256sum ${pkgs.libraw.src}/$license | cut -d ' ' -f 1)"
                  printf '%s  libraw/%s\n' "$digest" "$license" >> staging/DEPENDENCY-LICENSES.sha256
                done
                binary_hash="$(sha256sum staging/endlessfs | cut -d ' ' -f 1)"
                raw_decoder_hash="$(sha256sum staging/endlessfs-raw-decoder | cut -d ' ' -f 1)"
                container_hash="$(sha256sum "staging/endlessfs-container-${version}.tar.gz" | cut -d ' ' -f 1)"
                theme_hash="$(sha256sum staging/THEMES.json | cut -d ' ' -f 1)"
                capability_hash="$(sha256sum staging/CAPABILITIES.json | cut -d ' ' -f 1)"
                dependency_hash="$(sha256sum staging/DEPENDENCIES.txt | cut -d ' ' -f 1)"
                license_hash="$(sha256sum staging/DEPENDENCY-LICENSES.sha256 | cut -d ' ' -f 1)"
                container_archive_bytes="$(wc -c < "staging/endlessfs-container-${version}.tar.gz" | tr -d ' ')"
                mkdir -p image-archive image-root
                tar -xzf "staging/endlessfs-container-${version}.tar.gz" -C image-archive
                jq -r '.[0].Layers[]' image-archive/manifest.json | while IFS= read -r layer; do
                  tar -xf "image-archive/$layer" -C image-root
                done
                container_unpacked_bytes="$(du -sb image-root | cut -f 1)"
                lock_hash="$(sha256sum ${projectSource}/flake.lock | cut -d ' ' -f 1)"
                cp ${vulndbFor pkgs}/snapshot.json staging/VULNERABILITY-DATABASE.json
                vulndb_hash="$(jq -er '.sha256' staging/VULNERABILITY-DATABASE.json)"
                vulndb_modified="$(jq -er '.databaseModified' staging/VULNERABILITY-DATABASE.json)"
                {
                  printf 'source-revision=%s\n' '${version}'
                  printf 'flake-lock-sha256=%s\n' "$lock_hash"
                  printf 'vulnerability-database-archive-sha256=%s\n' "$vulndb_hash"
                  printf 'vulnerability-database-modified=%s\n' "$vulndb_modified"
                  printf 'target-system=%s\n' '${system}'
                  printf 'go-toolchain=%s\n' '${go.version}'
                  printf 'capability-profile=%s\n' 'images'
                  printf 'packaged-preview-capabilities=%s\n' 'image'
                  printf 'packaged-image-decoders=%s\n' 'go-standard-library,deepteams-webp-1.2.6,libraw-0.22.1'
                  printf 'binary-sha256=%s\n' "$binary_hash"
                  printf 'raw-decoder-sha256=%s\n' "$raw_decoder_hash"
                  printf 'oci-sha256=%s\n' "$container_hash"
                  printf 'oci-archive-bytes=%s\n' "$container_archive_bytes"
                  printf 'oci-unpacked-root-bytes=%s\n' "$container_unpacked_bytes"
                  printf 'theme-inventory-sha256=%s\n' "$theme_hash"
                  printf 'capability-inventory-sha256=%s\n' "$capability_hash"
                  printf 'dependency-inventory-sha256=%s\n' "$dependency_hash"
                  printf 'dependency-license-inventory-sha256=%s\n' "$license_hash"
                  printf 'verification-command=%s\n' 'nix flake check --print-build-logs'
                  printf 'coverage-command=%s\n' 'nix run .#test-coverage'
                  printf 'repository-coverage-minimum=%s\n' '85%'
                  printf 'security-boundary-coverage-minimum=%s\n' '95%'
                  printf 'migration-coverage-minimum=%s\n' '98%'
                  printf 'storage-providers=%s\n' 'deterministic-memory,locally-qualified-gcs-adapter'
                  printf 'canonical-format=%s\n' 'endlessfs-portable-bucket-v1'
                  printf 'writer-protocol-version=%s\n' '1'
                  printf 'preview-store-providers=%s\n' 'disabled,deterministic-memory,locally-qualified-gcs'
                  printf 'implementation-status=%s\n' 'v1.1-image-preview-complete-durable-gcs-local-qualification'
                  printf 'live-gcs-validation=%s\n' 'not-performed'
                  printf 'deployment-validation=%s\n' 'not-performed'
                  printf 'build-and-test-credentials-used=%s\n' 'none'
                  printf 'build-and-test-external-services-used=%s\n' 'none'
                } > staging/RELEASE-INVENTORY.txt
                tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
                  -C staging -czf "$out/endlessfs-${version}-${system}.tar.gz" .
                cp "staging/endlessfs-container-${version}.tar.gz" "$out/"
                cp staging/RELEASE-INVENTORY.txt "$out/"
                cp staging/endlessfs-raw-decoder "$out/"
                cp staging/DEPENDENCIES.txt "$out/"
                cp staging/DEPENDENCY-LICENSES.sha256 "$out/"
                cp staging/THEMES.json "$out/"
                cp staging/CAPABILITIES.json "$out/"
                cp staging/VULNERABILITY-DATABASE.json "$out/"
                (
                  cd "$out"
                  sha256sum \
                    "endlessfs-${version}-${system}.tar.gz" \
                    "endlessfs-container-${version}.tar.gz" \
                    endlessfs-raw-decoder \
                    CAPABILITIES.json DEPENDENCIES.txt DEPENDENCY-LICENSES.sha256 RELEASE-INVENTORY.txt THEMES.json VULNERABILITY-DATABASE.json \
                    > SHA256SUMS
                )
              '';
        in
        {
          default = endlessfs;
          dependency-compile-cache = dependencyCompileCache;
          inherit container release;
          container-images = container;
          release-images = release;
        }
        // lib.optionalAttrs (system == "x86_64-linux") {
          local-functional-coverage = functionalCoverage.producer;
        }
      );

      apps = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          lib = pkgs.lib;
          go = goFor pkgs;
          packages = self.packages.${system};
          headlessBrowser = headlessBrowserFor pkgs;
          coverageSource = coverageSourceFor pkgs;
          coverageText = ''
            export CGO_ENABLED=0
            export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            export ENDLESSFS_RUN_E2E=1
            ${lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
              export ENDLESSFS_CHROMIUM=${headlessBrowser}/bin/chrome-headless-shell
              export ENDLESSFS_CHROMIUM_NO_SANDBOX=1
            ''}
            coverage_root="$(mktemp -d "''${TMPDIR:-/tmp}/endlessfs-coverage.XXXXXX")"
            cleanup() {
              chmod -R u+w "$coverage_root"
              rm -rf "$coverage_root"
            }
            trap cleanup EXIT
            cp -R ${coverageSource} "$coverage_root/source"
            chmod -R u+w "$coverage_root/source"
            cp -R ${packages.default.goModules} "$coverage_root/source/vendor"
            cd "$coverage_root/source"
            export GOFLAGS=-mod=vendor
            profile="$coverage_root/endlessfs-coverage.out"
            ${installDependencyCompileCache packages.dependency-compile-cache "$coverage_root"}
            go test -timeout=30m ./... -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$profile"
            if [ -n "''${ENDLESSFS_COVERAGE_PROFILE:-}" ]; then
              install -m 0644 "$profile" "$ENDLESSFS_COVERAGE_PROFILE"
            fi
            gawk -f tools/coverage.awk "$profile"
          '';
          legacyCoverage = pkgs.writeShellApplication {
            name = "endlessfs-test-coverage-fresh";
            runtimeInputs =
              goTools ++ [ pkgs.gawk ] ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ headlessBrowser ];
            text = coverageText;
          };
          functionalCoverage = import ./tools/functionalcoverage/default.nix {
            inherit pkgs go;
            fullSource = self.outPath;
            inherit coverageSource;
            vendor = packages.default.goModules;
            seed = packages.dependency-compile-cache;
            browser = headlessBrowser;
          };
          goTools = [
            go
            pkgs.libraw
          ];
          qualityTools = goTools ++ [
            pkgs.bash
            pkgs.go-tools
            pkgs.gosec
            pkgs.nixfmt
            pkgs.ripgrep
            pkgs.yq-go
          ];
          # writeShellApplication only prepends runtimeInputs to the caller's
          # PATH, so every app declares the basic tools its scripts use rather
          # than relying on whatever the host (or CI image) happens to provide.
          mkTask = name: runtimeInputs: text: {
            type = "app";
            meta.description = name;
            program = "${
              pkgs.writeShellApplication {
                inherit name text;
                runtimeInputs = [
                  pkgs.coreutils
                  pkgs.gnused
                ]
                ++ runtimeInputs;
              }
            }/bin/${name}";
          };
          goTask =
            name: text:
            mkTask name goTools ''
              export CGO_ENABLED=0
              export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
              export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
              ${text}
            '';
          unavailable =
            name: milestone:
            mkTask name [ ] ''
              echo "${name} is reserved by the v1 specification and will be implemented in ${milestone}." >&2
              exit 1
            '';
        in
        {
          default = {
            type = "app";
            meta.description = "Run EndlessFS";
            program = "${packages.default}/bin/endlessfs";
          };

          dev = goTask "endlessfs-dev" ''
            exec go run ./cmd/endlessfs "$@"
          '';

          dev-fixture = goTask "endlessfs-dev-fixture" ''
            export ENDLESSFS_LOCAL_FIXTURE=true
            export ENDLESSFS_PREVIEW_PROVIDER=mock
            export ENDLESSFS_PREVIEW_AUTOMATIC=true
            ENDLESSFS_SESSION_SECRET="$(go run ./tools/generate-secret)"
            export ENDLESSFS_SESSION_SECRET
            exec go run ./cmd/endlessfs "$@"
          '';

          generate-secret = goTask "endlessfs-generate-secret" ''
            exec go run ./tools/generate-secret
          '';

          generate-migration-fixture = goTask "endlessfs-generate-migration-fixture" ''
            if [ "$#" -ne 1 ] || ! [[ "$1" =~ ^[0-9a-f]{40}$ ]]; then
              echo "usage: nix run .#generate-migration-fixture -- PRODUCER_COMMIT" >&2
              exit 2
            fi
            export ENDLESSFS_MIGRATION_FIXTURE_PRODUCER_COMMIT="$1"
            exec go test ./cmd/endlessfs -run '^TestGenerateCurrentMigrationFixtures$' -count=1
          '';

          fmt =
            mkTask "endlessfs-fmt"
              [
                pkgs.fd
                pkgs.go
                pkgs.nixfmt
              ]
              ''
                fd --type f --extension go --exec gofmt -w
                nixfmt flake.nix
              '';

          fmt-check =
            mkTask "endlessfs-fmt-check"
              [
                pkgs.go
                pkgs.nixfmt
              ]
              ''
                unformatted="$(gofmt -l .)"
                if [ -n "$unformatted" ]; then
                  echo "Go files need formatting:" >&2
                  echo "$unformatted" >&2
                  exit 1
                fi
                nixfmt --check flake.nix
              '';

          lint = mkTask "endlessfs-lint" qualityTools ''
            ${pipelinePolicyCommand}
            go vet ./...
            staticcheck ./...
            go run ./tools/check-source .
          '';

          test = goTask "endlessfs-test" ''
            go test -timeout=30m ./...
          '';

          test-unit = goTask "endlessfs-test-unit" ''
            go test -short ./...
          '';

          test-integration = goTask "endlessfs-test-integration" ''
            go test ./... -run '^TestIntegration'
          '';

          test-contract = goTask "endlessfs-test-contract" ''
            go test ./... -run '^TestContract'
          '';

          test-provider-budget = goTask "endlessfs-test-provider-budget" ''
            go test ./internal/providerbudget ./internal/objectstore/budgettest ./internal/objectstore/gcs -count=1
            go test ${providerBudgetPackages} -run '${providerBudgetTestPattern}' -count=1
          '';
          test-migration = mkTask "endlessfs-test-migration" (goTools ++ [ pkgs.nix ]) ''
            export CGO_ENABLED=0
            export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            if [ "$#" -gt 1 ]; then
              echo "test-migration accepts one optional candidate release" >&2
              exit 1
            fi
            export ENDLESSFS_MIGRATION_CANDIDATE_RELEASE="''${1:-''${ENDLESSFS_MIGRATION_CANDIDATE_RELEASE:-}}"
            ${self.apps.${system}.test-imports.program}
            # Consume the same complete hermetic execution as the flake check.
            # The explicit candidate string is a derivation input; arbitrary
            # inherited build/test flags cannot satisfy this canonical gate.
            # A successful immutable result can be reused, not relabeled fresh.
            exec nix build --impure --no-link --print-build-logs --expr '
              let f = builtins.getFlake "${self.outPath}";
              in f.checks.${system}.migration.overrideAttrs (_: {
                ENDLESSFS_MIGRATION_CANDIDATE_RELEASE = builtins.getEnv "ENDLESSFS_MIGRATION_CANDIDATE_RELEASE";
              })
            '
          '';
          test-replica = goTask "endlessfs-test-replica" ''
            exec go test ./internal/portable ./internal/objectstore/gcs \
              -run '(Replica|CandidateCannot|Superseded|GenerationConditionsFence|LostMutation)' -count=1 "$@"
          '';

          test-portability = goTask "endlessfs-test-portability" ''
            exec go test ./internal/storageformat ./internal/objectstore/... ./internal/portable \
              -run '(Portab|Checkpoint|ContractGCSProtocol|GCSResumableCapability)' -count=1 "$@"
          '';

          test-preview = goTask "endlessfs-test-preview" ''
            go test ./internal/preview/... -count=1
            go test ./internal/httpapi -run 'Preview' -count=1
          '';
          test-e2e =
            mkTask "endlessfs-test-e2e"
              (goTools ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ headlessBrowser ])
              ''
                export ENDLESSFS_RUN_E2E=1
                ${lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
                  export ENDLESSFS_CHROMIUM=${headlessBrowser}/bin/chrome-headless-shell
                  export ENDLESSFS_CHROMIUM_NO_SANDBOX=1
                ''}
                exec go test ./internal/e2e -run '^TestE2E' -count=1 "$@"
              '';

          test-ui-benchmark =
            mkTask "endlessfs-test-ui-benchmark"
              (goTools ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ headlessBrowser ])
              ''
                export ENDLESSFS_RUN_E2E=1
                ${lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
                  export ENDLESSFS_CHROMIUM=${headlessBrowser}/bin/chrome-headless-shell
                  export ENDLESSFS_CHROMIUM_NO_SANDBOX=1
                ''}
                benchmark_output="''${ENDLESSFS_UI_BENCHMARK_OUTPUT:-ui-benchmark-v1.json}"
                go test ./internal/e2e -run '^TestE2EBrowserBootstrapLoginDriveShareAndTrash$' -count=1 -json "$@" | tee "$benchmark_output"
                echo "UI benchmark evidence: $benchmark_output"
              '';

          test-coverage =
            mkTask "endlessfs-test-coverage"
              (goTools ++ [ pkgs.gawk ] ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ headlessBrowser ])
              (
                (lib.optionalString (system == "x86_64-linux") ''
                  if [ "''${ENDLESSFS_LOCAL_FUNCTIONAL_CACHE:-}" = 1 ]; then
                    exec ${pkgs.nix}/bin/nix run --offline --no-write-lock-file \
                      "path:${self.outPath}#apps.${system}.local-functional-coverage-consumer" -- "$@"
                  fi
                '')
                + coverageText
              );

          local-functional-coverage-consumer =
            if system == "x86_64-linux" then
              {
                type = "app";
                program = "${functionalCoverage.consumer "${legacyCoverage}/bin/endlessfs-test-coverage-fresh"}/bin/endlessfs-local-functional-coverage-consumer";
              }
            else
              {
                type = "app";
                program = "${legacyCoverage}/bin/endlessfs-test-coverage-fresh";
              };

          test-race = mkTask "endlessfs-test-race" (goTools ++ [ pkgs.stdenv.cc ]) ''
            export CGO_ENABLED=1
            export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            ${raceTestCommand}
          '';

          test-fuzz = goTask "endlessfs-test-fuzz" ''
            fuzztime="''${ENDLESSFS_FUZZTIME:-1000x}"
            ${fuzzSmokeCommand}
          '';

          test-theme = goTask "endlessfs-test-theme" ''
            exec go test ./internal/theme "$@"
          '';
          theme-check = goTask "endlessfs-theme-check" ''
            exec go run ./tools/theme check "$@"
          '';
          theme-preview = goTask "endlessfs-theme-preview" ''
            exec go run ./tools/theme preview "$@"
          '';

          vulndb = goTask "endlessfs-vulndb" ''
            exec go run ./tools/vulndb "$@"
          '';

          test-vulndb = goTask "endlessfs-test-vulndb" ''
            exec go test ./tools/vulndb -count=1 "$@"
          '';

          forbidden-check = goTask "endlessfs-forbidden-check" ''
            exec go run ./tools/check-source "$@"
          '';

          security =
            mkTask "endlessfs-security"
              (
                qualityTools
                ++ [
                  pkgs.findutils
                  pkgs.gawk
                  pkgs.gnugrep
                  pkgs.govulncheck
                  pkgs.jq
                  pkgs.nix
                ]
              )
              ''
                ${pipelinePolicyCommand}
                go vet ./...
                staticcheck ./...
                gosec -quiet -nosec-require-justification -nosec-require-rules ./...
                govulncheck -db=file://${vulndbFor pkgs} ./...
                go test ./internal/config -count=1
                ${dependencyPolicyCommand packages.default.goModules}
                go run ./tools/check-source .
                nix build '.#checks.${system}.container-policy' --no-link --print-build-logs
              '';

          security-freshness =
            mkTask "endlessfs-security-freshness"
              (
                goTools
                ++ [
                  pkgs.coreutils
                  pkgs.govulncheck
                ]
              )
              ''
                export CGO_ENABLED=0
                export GOFLAGS=-mod=readonly GOTOOLCHAIN=local
                review_directory="$(mktemp -d "''${TMPDIR:-/tmp}/endlessfs-security-freshness.XXXXXX")"
                trap 'rm -rf "$review_directory"' EXIT
                go run ./tools/vulndb update "$review_directory"
                go run ./tools/vulndb extract "$review_directory" "$review_directory/database"
                govulncheck -db="file://$review_directory/database" ./...
              '';

          dependency-check = mkTask "endlessfs-dependency-check" [
            go
            pkgs.findutils
            pkgs.gawk
            pkgs.gnugrep
            pkgs.jq
          ] ("export CGO_ENABLED=0\n" + dependencyPolicyCommand packages.default.goModules);

          test-imports = mkTask "endlessfs-test-imports" [ pkgs.nix pkgs.git ] ''
            if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
              untracked_go="$(git ls-files --others --exclude-standard -- '*.go')"
              if [ -n "$untracked_go" ]; then
                echo "Stage newly added Go source before the Git-flake import preflight." >&2
                exit 1
              fi
            fi
            exec nix build '.#checks.${system}.test-imports' --no-link --print-build-logs "$@"
          '';

          pr-check = mkTask "endlessfs-pr-check" (qualityTools ++ [ pkgs.nix ]) ''
            ${self.apps.${system}.test-imports.program}
            export CGO_ENABLED=0
            unformatted="$(gofmt -l .)"
            if [ -n "$unformatted" ]; then
              echo "Go files need formatting:" >&2
              echo "$unformatted" >&2
              exit 1
            fi
            nixfmt --check flake.nix
            ${pipelinePolicyCommand}
            go vet ./...
            staticcheck ./...
            go run ./tools/check-source .
          '';

          repository-policy = goTask "endlessfs-repository-policy" ''
            exec go run ./tools/repository-policy "$@"
          '';

          pipeline-policy = mkTask "endlessfs-pipeline-policy" [
            go
            pkgs.bash
            pkgs.ripgrep
            pkgs.yq-go
          ] pipelinePolicyCommand;

          provider-verify = goTask "endlessfs-provider-verify" ''
            exec go run ./tools/provider-verify "$@"
          '';

          container = mkTask "endlessfs-container" [ pkgs.nix ] ''
            exec nix build .#container "$@"
          '';

        }
      );

      checks = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          lib = pkgs.lib;
          go = goFor pkgs;
          headlessBrowser = headlessBrowserFor pkgs;
          relativePath = path: lib.removePrefix (toString ./. + "/") (toString path);
          isGoTestSource =
            relative:
            relative == "go.mod"
            || relative == "go.sum"
            # Exact-budget inventory verifies the shared Nix gate selection.
            || relative == "flake.nix"
            || relative == "cmd"
            || lib.hasPrefix "cmd/" relative
            || relative == "internal"
            || lib.hasPrefix "internal/" relative
            || relative == "tools"
            || lib.hasPrefix "tools/" relative;
          testSource = lib.cleanSourceWith {
            src = ./.;
            filter = path: _type: isGoTestSource (relativePath path);
          };
          formatSource = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: _type:
              let
                relative = relativePath path;
              in
              isGoTestSource relative || relative == "flake.nix";
          };
          pipelineSource = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: _type:
              let
                relative = relativePath path;
              in
              relative == ".tekton"
              || lib.hasPrefix ".tekton/" relative
              || relative == ".github"
              || relative == ".github/workflows"
              || lib.hasPrefix ".github/workflows/" relative
              || relative == ".github/dependabot.yml";
          };
          policySource = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: _type:
              let
                relative = relativePath path;
              in
              relative == "go.mod"
              || relative == "go.sum"
              || relative == "tools"
              || relative == "tools/repository-policy"
              || lib.hasPrefix "tools/repository-policy/" relative
              || relative == ".github"
              || relative == ".github/rulesets"
              || lib.hasPrefix ".github/rulesets/" relative;
          };
          fullSource = lib.cleanSource ./.;
          sandboxedStaticcheck = lib.optionalString pkgs.stdenv.hostPlatform.isLinux "staticcheck ./...";
          containerPolicy =
            pkgs.runCommand "endlessfs-container-policy"
              {
                nativeBuildInputs = [
                  pkgs.gnutar
                  pkgs.gzip
                  pkgs.jq
                  pkgs.ripgrep
                ];
              }
              ''
                mkdir image
                tar -xzf ${self.packages.${system}.container} -C image
                config_file="$(jq -r '.[0].Config' image/manifest.json)"
                jq -e '
                  .os == "linux" and
                  .created == "1970-01-01T00:00:01+00:00" and
                  .config.Entrypoint == ["/bin/endlessfs"] and
                  .config.User == "65532:65532" and
                  ((.config.Volumes == null) or (.config.Volumes == {})) and
                  .config.ExposedPorts == {"8080/tcp": {}}
                ' "image/$config_file" >/dev/null
                jq -r '.[0].Layers[]' image/manifest.json | while IFS= read -r layer; do
                  tar -tf "image/$layer"
                done > image-paths.txt
                if rg '(^|/)(sh|bash|ash|dash|zsh|apk|apt|dpkg|rpm|yum|dnf)(/|$)' image-paths.txt; then
                  echo "container includes a shell or package manager" >&2
                  exit 1
                fi
                if rg '(^|/)(go\.mod|go\.sum|flake\.lock|\.git)(/|$)|\.go$|credential|secret|token' image-paths.txt; then
                  echo "container includes source or credential-shaped material" >&2
                  exit 1
                fi
                rg --quiet '(^|/)bin/endlessfs$' image-paths.txt
                rg --quiet '(^|/)bin/endlessfs-raw-decoder$' image-paths.txt
                touch "$out"
              '';
          goCheckWithSource =
            name: checkSource: command: tools:
            pkgs.runCommand "endlessfs-${name}"
              {
                nativeBuildInputs = [ go ] ++ tools;
              }
              ''
                # Nix builds otherwise use /homeless-shelter. Chromium needs a
                # writable home and XDG runtime directory for its per-user
                # state, including crash reporter initialization.
                export HOME="$TMPDIR/home"
                export GOCACHE="$TMPDIR/go-cache"
                export GOMODCACHE="$TMPDIR/go-mod-cache"
                export XDG_CACHE_HOME="$TMPDIR/tool-cache"
                export XDG_CONFIG_HOME="$HOME/.config"
                export XDG_RUNTIME_DIR="$TMPDIR/runtime"
                mkdir -p "$HOME" "$XDG_CACHE_HOME" "$XDG_CONFIG_HOME" "$XDG_RUNTIME_DIR"
                chmod 700 "$XDG_RUNTIME_DIR"
                export CGO_ENABLED=0
                export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
                export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
                cp -R ${checkSource} source
                chmod -R u+w source
                cp -R ${self.packages.${system}.default.goModules} source/vendor
                cd source
                export GOFLAGS=-mod=vendor
                ${lib.optionalString (name != "test-imports") (
                  installDependencyCompileCache self.packages.${system}.dependency-compile-cache "$TMPDIR"
                )}
                ${command}
                touch "$out"
              '';
          goCheck =
            name: command: tools:
            goCheckWithSource name testSource command tools;
          testSuite = goCheck "tests" "go test -timeout=30m ./..." [ ];
          testImportCheck = goCheck "test-imports" ''
            # Resolve actual retained test imports before any expensive test.
            # A host module cache must not hide a missing vendored subpackage.
            export GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOWORK=off
            CGO_ENABLED=0 go list -deps -test ./... >/dev/null
            CGO_ENABLED=1 go list -race -deps -test ./... >/dev/null
            echo "retained normal/race test imports: ok (no tests executed)"
          '' [ pkgs.stdenv.cc ];
          migrationCheck =
            (goCheck "migration" ''
              profile="$TMPDIR/migration-coverage.out"
              go test -timeout=30m ./internal/portable ./cmd/endlessfs -count=1 \
                -covermode=atomic -coverpkg=./internal/portable -coverprofile="$profile"
              gawk -v only_group=migration -f tools/coverage.awk "$profile"
            '' [ pkgs.gawk ]).overrideAttrs
              (_: {
                ENDLESSFS_MIGRATION_CANDIDATE_RELEASE = "";
              });
          providerEconomicsCheck = goCheck "provider-economics" ''
            go test ./internal/providerbudget ./internal/objectstore/budgettest ./internal/objectstore/gcs -count=1
            go test ${providerBudgetPackages} -run '${providerBudgetTestPattern}' -count=1
          '' [ ];
          e2eCompile = goCheck "e2e-compile" "go test ./internal/e2e -run '^TestE2E'" [ ];
          coverageCompile = goCheck "coverage-compile" "go test ./... -run '^$' -coverpkg=./..." [ ];
          linuxCiAppPolicy =
            pkgs.runCommand "endlessfs-linux-ci-app-policy"
              {
                nativeBuildInputs = [ pkgs.ripgrep ];
              }
              ''
                for program in \
                  ${self.apps.${system}.pr-check.program} \
                  ${self.apps.${system}.test-coverage.program}; do
                  rg --quiet '^export CGO_ENABLED=0$' "$program"
                done
                for program in \
                  ${self.apps.${system}.test.program} \
                  ${self.apps.${system}.test-coverage.program}; do
                  rg --fixed-strings --quiet 'go test -timeout=30m' "$program" || {
                    echo "long-running Go test gate must use the explicit 30-minute process deadline: $program" >&2
                    exit 1
                  }
                done
                # Migration's public command now consumes the exact check, so
                # validate both that binding and the check's actual build code.
                migration_program=${self.apps.${system}.test-migration.program}
                rg --fixed-strings --quiet 'f.checks.${system}.migration.overrideAttrs' "$migration_program"
                rg --fixed-strings --quiet 'ENDLESSFS_MIGRATION_CANDIDATE_RELEASE = builtins.getEnv "ENDLESSFS_MIGRATION_CANDIDATE_RELEASE";' "$migration_program"
                printf '%s\n' ${lib.escapeShellArg migrationCheck.buildCommand} > migration-build.sh
                rg --fixed-strings --quiet 'go test -timeout=30m ./internal/portable ./cmd/endlessfs -count=1' migration-build.sh
                rg --fixed-strings --quiet -- '-covermode=atomic -coverpkg=./internal/portable -coverprofile="$profile"' migration-build.sh
                rg --fixed-strings --quiet 'gawk -v only_group=migration -f tools/coverage.awk "$profile"' migration-build.sh
                race_program=${self.apps.${system}.test-race.program}
                for required in \
                  "portable_migration_tests='^TestMigration'" \
                  'if [[ "$package" != */internal/portable ]]' \
                  'go test -race -timeout=30m -count=1 ./internal/portable -run "$exhaustive_migration_test"' \
                  'go test -race -timeout=30m -count=1 ./internal/portable -run "$portable_migration_tests" -skip "$exhaustive_migration_test"' \
                  'go test -race -timeout=30m -count=1 ./internal/portable -skip "$portable_migration_tests"' \
                  'go test -race -timeout=30m -count=1 "''${other_packages[@]}"' \
                  'for race_pid in "''${race_pids[@]}"' \
                  'test "$race_status" -eq 0'; do
                  rg --fixed-strings --quiet "$required" "$race_program" || {
                    echo "race gate must retain the fail-closed portable-package sharding contract: $required" >&2
                    exit 1
                  }
                done
                test "$(rg --fixed-strings --count 'race_pids+=("$!")' "$race_program")" -eq 3 || {
                  echo "race gate must wait for exactly three parallel remainder shards" >&2
                  exit 1
                }
                if rg --fixed-strings --quiet \
                  'go test -race -timeout=30m -count=1 ./internal/portable -run "$exhaustive_migration_test" &' \
                  "$race_program"; then
                  echo "race gate must give the exhaustive transport matrix exclusive resources" >&2
                  exit 1
                fi
                rg --fixed-strings --quiet 'FONTCONFIG_FILE' ${headlessBrowser}/bin/chrome-headless-shell
                touch "$out"
              '';
          formatCheck =
            goCheckWithSource "format" formatSource
              ''
                # goCheck installs the fixed-output module closure at ./vendor for
                # offline builds. Formatting policy applies only to repository-owned
                # source; generated third-party modules are immutable build inputs.
                unformatted="$(find . -path ./vendor -prune -o -type f -name '*.go' -exec gofmt -l {} +)"
                test -z "$unformatted"
                nixfmt --check flake.nix
              ''
              [
                pkgs.findutils
                pkgs.nixfmt
              ];
          lintCheck =
            goCheckWithSource "lint" testSource
              ''
                go vet ./...
                ${sandboxedStaticcheck}
                go run ./tools/check-source .
              ''
              [
                pkgs.go-tools
              ];
          pipelinePolicyCheck =
            pkgs.runCommand "endlessfs-pipeline-policy"
              {
                nativeBuildInputs = [
                  go
                  pkgs.bash
                  pkgs.ripgrep
                  pkgs.yq-go
                ];
              }
              ''
                cp -R ${pipelineSource} source
                chmod -R u+w source
                cd source
                export GOCACHE="$TMPDIR/go-cache" GOPROXY=off GOSUMDB=off GOFLAGS=-mod=readonly
                ${pipelinePolicyCommand}
                touch "$out"
              '';
          raceCheck = goCheck "race" ''
            export CGO_ENABLED=1
            ${raceTestCommand}
          '' [ pkgs.stdenv.cc ];
          fuzzCheck = goCheck "fuzz" ''
            fuzztime=1000x
            ${fuzzSmokeCommand}
          '' [ ];
          securityCheck =
            goCheckWithSource "security" fullSource
              ''
                gosec -quiet -nosec-require-justification -nosec-require-rules ./...
                govulncheck -db=file://${vulndbFor pkgs} ./...
                ${dependencyPolicyCommand self.packages.${system}.default.goModules}
                go run ./tools/check-source .
              ''
              [
                pkgs.findutils
                pkgs.gawk
                pkgs.gnugrep
                pkgs.gosec
                pkgs.govulncheck
                pkgs.jq
              ];
          vulndbCheck =
            pkgs.runCommand "endlessfs-vulnerability-snapshot-check"
              {
                nativeBuildInputs = [
                  pkgs.govulncheck
                  pkgs.jq
                ];
              }
              ''
                # Exercise the scanner's interpretation of the real snapshot:
                # CVE-2023-29403 affects Go 1.20.0 and was fixed in Go 1.20.5.
                govulncheck -db=file://${vulndbFor pkgs} -mode=query -json stdlib@v1.20.0 > vulnerable.json
                jq -se 'any(.[]; .osv.id == "GO-2023-1840")' vulnerable.json >/dev/null
                govulncheck -db=file://${vulndbFor pkgs} -mode=query -json stdlib@v1.20.5 > fixed.json
                jq -se 'all(.[]; .osv.id != "GO-2023-1840")' fixed.json >/dev/null
                touch "$out"
              '';
          releaseInputEvidenceCheck =
            pkgs.runCommand "endlessfs-release-input-evidence" { nativeBuildInputs = [ pkgs.ripgrep ]; }
              ''
                release=${self.packages.${system}.release}
                cmp "$release/VULNERABILITY-DATABASE.json" ${./security/vulndb/snapshot.json}
                rg --fixed-strings '  VULNERABILITY-DATABASE.json' "$release/SHA256SUMS" >/dev/null
                cd "$release"
                sha256sum --check SHA256SUMS
                touch "$out"
              '';
          repositoryPolicyCheck =
            goCheckWithSource "repository-policy" policySource "go run ./tools/repository-policy check"
              [ ];
        in
        {
          build = self.packages.${system}.default;
          container = self.packages.${system}.container;
          container-images = self.packages.${system}.container-images;
          container-policy = containerPolicy;
          release = self.packages.${system}.release;
          release-images = self.packages.${system}.release-images;

          e2e = e2eCompile;

          format = formatCheck;

          lint = lintCheck;
          pipeline-policy = pipelinePolicyCheck;

          tests = testSuite;
          test-imports = testImportCheck;
          integration = testSuite;
          contract = testSuite;
          migration = migrationCheck;
          replica = testSuite;
          portability = testSuite;
          provider-verify = testSuite;
          provider-economics = providerEconomicsCheck;
          preview = testSuite;
          theme = testSuite;
          race = raceCheck;
          coverage = coverageCompile;
          fuzz = fuzzCheck;
          offline = testSuite;
          security = securityCheck;
          dependencies = securityCheck;

          vulndb = vulndbCheck;
          release-input-evidence = releaseInputEvidenceCheck;

          compiler-identity = pkgs.runCommand "endlessfs-compiler-identity" { nativeBuildInputs = [ go ]; } ''
            actual="$(go version ${self.packages.${system}.default}/bin/endlessfs)"
            expected="${self.packages.${system}.default}/bin/endlessfs: go${go.version}"
            if [ "$actual" != "$expected" ]; then
              echo "application compiler identity differs from the pinned toolchain: $actual" >&2
              exit 1
            fi
            touch "$out"
          '';

          repository-policy = repositoryPolicyCheck;
        }
        // lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
          linux-ci-app-policy = linuxCiAppPolicy;
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          go = goFor pkgs;
        in
        {
          default = pkgs.mkShellNoCC {
            packages = [
              pkgs.fd
              pkgs.gh
              go
              pkgs.go-tools
              pkgs.gosec
              pkgs.nixfmt
              pkgs.ripgrep
              pkgs.skopeo
              pkgs.yq-go
              pkgs.libraw
            ];
            shellHook = ''
              export GOFLAGS=-mod=readonly
              export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
              export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
              echo "EndlessFS development shell — run: nix flake check"
            '';
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
