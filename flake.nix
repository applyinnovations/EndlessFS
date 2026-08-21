{
  description = "EndlessFS — a private, provider-neutral cloud drive";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.vulndb = {
    # The canonical vuln.go.dev hostname rejects GitHub-hosted runner IPs;
    # pin the official bulk object by its immutable GCS generation as well as
    # the Nix content hash recorded in flake.lock.
    url = "https://storage.googleapis.com/download/storage/v1/b/go-vulndb/o/vulndb.zip?alt=media&generation=1787088262759230";
    flake = false;
  };

  outputs =
    {
      self,
      nixpkgs,
      vulndb,
    }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      goFor =
        pkgs:
        pkgs.go_1_26.overrideAttrs (_old: {
          version = "1.26.6";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go1.26.6.src.tar.gz";
            hash = "sha256-oHIcVMaIkBRI13rZs+x+p8R0cwdV/4kTgukuy5P/LLE=";
          };
        });
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
      containerTransportPolicyFor =
        pkgs:
        pkgs.writeText "endlessfs-container-transport-policy.json" ''
          {
            "default": [
              {
                "type": "reject"
              }
            ],
            "transports": {
              "docker-archive": {
                "": [
                  {
                    "type": "insecureAcceptAnything"
                  }
                ]
              }
            }
          }
        '';
      dependencyPolicyCommand = moduleClosure: ''
        vulndb_locked_url="$(jq -er '.nodes.vulndb.locked.url' flake.lock)"
        vulndb_original_url="$(jq -er '.nodes.vulndb.original.url' flake.lock)"
        if [ "$vulndb_locked_url" != "$vulndb_original_url" ]; then
          echo "vulnerability database lock URL differs from the declared input" >&2
          exit 1
        fi
        vulndb_prefix='https://storage.googleapis.com/download/storage/v1/b/go-vulndb/o/vulndb.zip?alt=media&generation='
        case "$vulndb_locked_url" in
          "$vulndb_prefix"*) vulndb_generation="''${vulndb_locked_url#"$vulndb_prefix"}" ;;
          *)
            echo "vulnerability database must use a generation-pinned official GCS media URL" >&2
            exit 1
            ;;
        esac
        if ! printf '%s\n' "$vulndb_generation" | grep -Eq '^[0-9]+$'; then
          echo "vulnerability database generation must be numeric" >&2
          exit 1
        fi

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
          yq -e '.spec.taskRunTemplate.podTemplate.securityContext.fsGroup == 1000' "$pipeline" >/dev/null
          yq -e '.spec.workspaces[] | select(.name == "nix-store") | .persistentVolumeClaim.claimName == "nix-store"' "$pipeline" >/dev/null
          yq -e '.spec.workspaces[] | select(.name == "git-cache") | .persistentVolumeClaim.claimName == "git-repo-cache"' "$pipeline" >/dev/null
          yq -e '.spec.workspaces[] | select(.name == "source") | .volumeClaimTemplate.spec.resources.requests.storage == "10Gi"' "$pipeline" >/dev/null
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
        release_verify_script="$(yq -r '.spec.pipelineSpec.tasks[] | select(.name == "verify") | .params[] | select(.name == "SCRIPT") | .value' .tekton/endlessfs-release.yaml)"
        printf '%s\n' "$release_verify_script" | rg -F 'nix run .#test-migration -- "$release_tag"' >/dev/null
        release_script="$(yq -r '.spec.pipelineSpec.tasks[] | select(.name == "release") | .params[] | select(.name == "SCRIPT") | .value' .tekton/endlessfs-release.yaml)"
        for command in view upload create; do
          printf '%s\n' "$release_script" | rg -F "gh release $command \"\$release_tag\" --repo \"applyinnovations/EndlessFS\"" >/dev/null
        done

        for task in prepare-cache fast-checks nix-checks coverage; do
          yq -e ".spec.taskRunSpecs[] | select(.pipelineTaskName == \"$task\") | .podTemplate.hostUsers == false" .tekton/endlessfs-ci.yaml >/dev/null
        done
        yq -e '.spec.taskRunSpecs[] | select(.pipelineTaskName == "coverage" and .podTemplate.automountServiceAccountToken == false)' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "coverage") | .taskRef.params[] | select(.name == "name" and .value == "nix-run-v2")' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "coverage") | .runAfter[] | select(. == "fast-checks")' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "coverage") | .runAfter[] | select(. == "nix-checks")' .tekton/endlessfs-ci.yaml >/dev/null
        yq -e '.spec.taskRunSpecs[] | select(.pipelineTaskName == "publish") | .podTemplate.hostUsers == false' .tekton/endlessfs-container.yaml >/dev/null
        for task in verify release; do
          yq -e ".spec.taskRunSpecs[] | select(.pipelineTaskName == \"$task\") | .podTemplate.hostUsers == false" .tekton/endlessfs-release.yaml >/dev/null
        done
        yq -e '.spec.pipelineSpec.tasks[] | select(.name == "release") | .retries >= 2' .tekton/endlessfs-release.yaml >/dev/null

        check_packages_binding() {
          pipeline="$1"
          task="$2"
          yq -e ".spec.pipelineSpec.tasks[] | select(.name == \"$task\") | .workspaces[] | select(.name == \"github-packages-auth\" and .workspace == \"github-packages-auth\")" "$pipeline" >/dev/null
          yq -e '.spec.workspaces[] | select(.name == "github-packages-auth") | .secret.secretName == "github-packages-credentials"' "$pipeline" >/dev/null
        }
        check_packages_binding .tekton/endlessfs-container.yaml publish
        check_packages_binding .tekton/endlessfs-release.yaml release
        if rg -n 'github-packages-(auth|credentials)' .tekton/endlessfs-ci.yaml; then
          echo "pull-request and merge-queue CI must not receive the GitHub Packages credential" >&2
          exit 1
        fi

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
            pkgs.buildGoModule {
              pname = "endlessfs";
              inherit version;
              src = goSource;
              subPackages = [ "cmd/endlessfs" ];
              vendorHash = "sha256-Rgxyk0aXuICRsQ8rJEhEbx2RbNZ81xcV9khACnVPcEY=";
              # Keep the fixed-output dependency closure address stable when the
              # source revision changes without changing go.mod/go.sum.
              overrideModAttrs = _final: _previous: {
                name = "endlessfs-go-modules";
              };
              inherit go;
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
                vulndb_hash="$(jq -r '.nodes.vulndb.locked.narHash' ${projectSource}/flake.lock)"
                {
                  printf 'source-revision=%s\n' '${version}'
                  printf 'flake-lock-sha256=%s\n' "$lock_hash"
                  printf 'vulnerability-database-nar-hash=%s\n' "$vulndb_hash"
                  printf 'target-system=%s\n' '${system}'
                  printf 'go-toolchain=%s\n' '1.26.6'
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
                (
                  cd "$out"
                  sha256sum \
                    "endlessfs-${version}-${system}.tar.gz" \
                    "endlessfs-container-${version}.tar.gz" \
                    endlessfs-raw-decoder \
                    CAPABILITIES.json DEPENDENCIES.txt DEPENDENCY-LICENSES.sha256 RELEASE-INVENTORY.txt THEMES.json \
                    > SHA256SUMS
                )
              '';
        in
        {
          default = endlessfs;
          inherit container release;
          container-images = container;
          release-images = release;
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
          containerTransportPolicy = containerTransportPolicyFor pkgs;
          relativePath = path: lib.removePrefix (toString ./. + "/") (toString path);
          coverageSource = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: _type:
              let
                relative = relativePath path;
              in
              relative == "go.mod"
              || relative == "go.sum"
              || relative == "cmd"
              || lib.hasPrefix "cmd/" relative
              || relative == "internal"
              || lib.hasPrefix "internal/" relative
              || relative == "tools"
              || lib.hasPrefix "tools/" relative;
          };
          goTools = [
            go
            pkgs.libraw
          ];
          qualityTools = goTools ++ [
            pkgs.go-tools
            pkgs.gosec
            pkgs.nixfmt
            pkgs.ripgrep
            pkgs.yq-go
          ];
          mkTask = name: runtimeInputs: text: {
            type = "app";
            meta.description = name;
            program = "${
              pkgs.writeShellApplication {
                inherit name runtimeInputs text;
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
          '';

          test = goTask "endlessfs-test" ''
            go test ./...
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
          test-migration = goTask "endlessfs-test-migration" ''
            candidate="''${1:-}"
            if [ -n "$candidate" ]; then
              export ENDLESSFS_MIGRATION_CANDIDATE_RELEASE="$candidate"
            fi
            exec go test ./internal/portable -run '(Migrat|ReleasedStorage)' -count=1
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
              ''
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
                go test ./... -count=1 -covermode=atomic -coverpkg=./... -coverprofile="$profile"
                gawk -f tools/coverage.awk "$profile"
              '';

          test-race = mkTask "endlessfs-test-race" (goTools ++ [ pkgs.stdenv.cc ]) ''
            export CGO_ENABLED=1
            export ENDLESSFS_INTERNAL_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            export ENDLESSFS_TEST_RAW_DECODER=${pkgs.libraw}/bin/dcraw_emu
            go test -race ./...
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
                govulncheck -db=file://${vulndb} ./...
                go test ./internal/config -count=1
                ${dependencyPolicyCommand packages.default.goModules}
                go run ./tools/check-source .
                nix build '.#checks.${system}.container-policy' --no-link --print-build-logs
              '';

          dependency-check = mkTask "endlessfs-dependency-check" [
            pkgs.findutils
            pkgs.gawk
            pkgs.gnugrep
            pkgs.jq
          ] (dependencyPolicyCommand packages.default.goModules);

          pr-check = mkTask "endlessfs-pr-check" qualityTools ''
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
            pkgs.ripgrep
            pkgs.yq-go
          ] pipelinePolicyCommand;

          provider-verify = goTask "endlessfs-provider-verify" ''
            exec go run ./tools/provider-verify "$@"
          '';

          container = mkTask "endlessfs-container" [ pkgs.nix ] ''
            exec nix build .#container "$@"
          '';

          publish-container =
            mkTask "endlessfs-publish-container"
              [
                pkgs.coreutils
                pkgs.skopeo
              ]
              ''
                if [ "$#" -lt 1 ]; then
                  echo "usage: nix run .#publish-container -- ghcr.io/owner/image:tag [...]" >&2
                  exit 2
                fi
                if [ -z "''${GHCR_TOKEN:-}" ] || [ -z "''${GHCR_USER:-}" ]; then
                  echo "GHCR_TOKEN and GHCR_USER are required" >&2
                  exit 1
                fi
                printf '%s' "$GHCR_TOKEN" | skopeo login --username "$GHCR_USER" --password-stdin ghcr.io >/dev/null
                for destination in "$@"; do
                  skopeo --policy ${containerTransportPolicy} copy --all "docker-archive:${packages.container}" "docker://$destination"
                done
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
                ${command}
                touch "$out"
              '';
          goCheck =
            name: command: tools:
            goCheckWithSource name testSource command tools;
          testSuite = goCheck "tests" "go test ./..." [ ];
          migrationCheck =
            goCheck "migration" "go test ./internal/portable -run '(Migrat|ReleasedStorage)' -count=1"
              [ ];
          e2eCompile = goCheck "e2e-compile" "go test ./internal/e2e -run '^TestE2E'" [ ];
          coverageCompile = goCheck "coverage-compile" "go test ./... -run '^$' -coverpkg=./..." [ ];
          publishContainerPolicy =
            pkgs.runCommand "endlessfs-publish-container-policy"
              {
                nativeBuildInputs = [
                  pkgs.jq
                  pkgs.ripgrep
                ];
              }
              ''
                rg --fixed-strings --quiet -- 'skopeo --policy ' ${self.apps.${system}.publish-container.program}
                jq -e '
                  .default == [{"type": "reject"}]
                  and .transports."docker-archive"."" == [{"type": "insecureAcceptAnything"}]
                ' ${containerTransportPolicyFor pkgs} >/dev/null
                touch "$out"
              '';
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
              ''
              [
                pkgs.go-tools
              ];
          pipelinePolicyCheck =
            pkgs.runCommand "endlessfs-pipeline-policy"
              {
                nativeBuildInputs = [
                  pkgs.ripgrep
                  pkgs.yq-go
                ];
              }
              ''
                cp -R ${pipelineSource} source
                chmod -R u+w source
                cd source
                ${pipelinePolicyCommand}
                touch "$out"
              '';
          raceCheck = goCheck "race" "CGO_ENABLED=1 go test -race ./..." [ pkgs.stdenv.cc ];
          fuzzCheck = goCheck "fuzz" ''
            fuzztime=1000x
            ${fuzzSmokeCommand}
          '' [ ];
          securityCheck =
            goCheckWithSource "security" fullSource
              ''
                gosec -quiet -nosec-require-justification -nosec-require-rules ./...
                govulncheck -db=file://${vulndb} ./...
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
          integration = testSuite;
          contract = testSuite;
          migration = migrationCheck;
          replica = testSuite;
          portability = testSuite;
          provider-verify = testSuite;
          preview = testSuite;
          theme = testSuite;
          race = raceCheck;
          coverage = coverageCompile;
          publish-container-policy = publishContainerPolicy;
          fuzz = fuzzCheck;
          offline = testSuite;
          security = securityCheck;
          dependencies = securityCheck;

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
