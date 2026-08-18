{
  description = "EndlessFS — a private, provider-neutral cloud drive";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  inputs.vulndb = {
    # The canonical vuln.go.dev hostname rejects GitHub-hosted runner IPs;
    # this is the same immutable bulk object at the Go database's backing bucket.
    url = "https://storage.googleapis.com/go-vulndb/vulndb.zip";
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
          component = pkgs.playwright-driver.components."chromium-headless-shell";
        in
        pkgs.runCommand "endlessfs-headless-browser"
          {
            nativeBuildInputs = [
              pkgs.findutils
              pkgs.makeWrapper
            ];
          }
          ''
            browser="$(find ${component} -type f -name chrome-headless-shell -perm -0100 -print -quit)"
            test -n "$browser"
            mkdir -p "$out/bin"
            makeWrapper "$browser" "$out/bin/chrome-headless-shell"
          '';
      dependencyPolicyCommand = moduleClosure: ''
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
            };

          endlessfs = lib.makeOverridable mkEndlessFS { };

          linuxArchitecture = if pkgs.stdenv.hostPlatform.isAarch64 then "arm64" else "amd64";
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
                runHook postInstall
              '';
              passthru = { inherit themeBundles; };
            };

          linuxBinary = lib.makeOverridable mkLinuxBinary { };

          containerRoot = pkgs.runCommandLocal "endlessfs-container-root" { } ''
            mkdir -p "$out/bin" "$out/etc/ssl/certs" "$out/share"
            cp ${linuxBinary}/bin/endlessfs "$out/bin/endlessfs"
            cp ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt "$out/etc/ssl/certs/ca-bundle.crt"
            cp -R ${pkgs.tzdata}/share/zoneinfo "$out/share/zoneinfo"
            chmod 0555 "$out/bin/endlessfs"
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
                "org.opencontainers.image.licenses" = "Apache-2.0";
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
                    acceptedImageMediaTypes: ["image/gif", "image/jpeg", "image/png", "image/webp"],
                    artifactMediaTypes: ["image/webp"],
                    imageRecipeID: "image-webp-q80-v1",
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
                binary_hash="$(sha256sum staging/endlessfs | cut -d ' ' -f 1)"
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
                  printf 'binary-sha256=%s\n' "$binary_hash"
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
                  printf 'preview-store-providers=%s\n' 'disabled,deterministic-memory'
                  printf 'implementation-status=%s\n' 'v1.1-image-preview-complete-local-qualification'
                  printf 'live-gcs-validation=%s\n' 'not-performed'
                  printf 'deployment-validation=%s\n' 'not-performed'
                  printf 'build-and-test-credentials-used=%s\n' 'none'
                  printf 'build-and-test-external-services-used=%s\n' 'none'
                } > staging/RELEASE-INVENTORY.txt
                tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
                  -C staging -czf "$out/endlessfs-${version}-${system}.tar.gz" .
                cp "staging/endlessfs-container-${version}.tar.gz" "$out/"
                cp staging/RELEASE-INVENTORY.txt "$out/"
                cp staging/DEPENDENCIES.txt "$out/"
                cp staging/DEPENDENCY-LICENSES.sha256 "$out/"
                cp staging/THEMES.json "$out/"
                cp staging/CAPABILITIES.json "$out/"
                (
                  cd "$out"
                  sha256sum \
                    "endlessfs-${version}-${system}.tar.gz" \
                    "endlessfs-container-${version}.tar.gz" \
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
          goTools = [ go ];
          qualityTools = goTools ++ [
            pkgs.actionlint
            pkgs.go-tools
            pkgs.gosec
            pkgs.nixfmt
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
            actionlint .github/workflows/*.yml
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

          test-coverage =
            mkTask "endlessfs-test-coverage"
              (goTools ++ [ pkgs.gawk ] ++ lib.optionals pkgs.stdenv.hostPlatform.isLinux [ headlessBrowser ])
              ''
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
                  pkgs.nix
                ]
              )
              ''
                actionlint .github/workflows/*.yml
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
          ] (dependencyPolicyCommand packages.default.goModules);

          pr-check = mkTask "endlessfs-pr-check" qualityTools ''
            unformatted="$(gofmt -l .)"
            if [ -n "$unformatted" ]; then
              echo "Go files need formatting:" >&2
              echo "$unformatted" >&2
              exit 1
            fi
            nixfmt --check flake.nix
            actionlint .github/workflows/*.yml
            go vet ./...
            staticcheck ./...
            go run ./tools/check-source .
          '';

          repository-policy = goTask "endlessfs-repository-policy" ''
            exec go run ./tools/repository-policy "$@"
          '';

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
                  skopeo copy --all "docker-archive:${packages.container}" "docker://$destination"
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
          isWorkflowSource =
            relative:
            relative == ".github"
            || relative == ".github/workflows"
            || lib.hasPrefix ".github/workflows/" relative;
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
          lintSource = lib.cleanSourceWith {
            src = ./.;
            filter =
              path: _type:
              let
                relative = relativePath path;
              in
              isGoTestSource relative || isWorkflowSource relative;
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
          e2eCompile = goCheck "e2e-compile" "go test ./internal/e2e -run '^TestE2E'" [ ];
          coverageCompile = goCheck "coverage-compile" "go test ./... -run '^$' -coverpkg=./..." [ ];
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
            goCheckWithSource "lint" lintSource
              ''
                actionlint .github/workflows/*.yml
                go vet ./...
                ${sandboxedStaticcheck}
              ''
              [
                pkgs.actionlint
                pkgs.go-tools
              ];
          raceCheck = goCheck "race" "CGO_ENABLED=1 go test -race ./..." [ pkgs.stdenv.cc ];
          fuzzCheck = goCheck "fuzz" ''
            fuzztime=1000x
            ${fuzzSmokeCommand}
          '' [ ];
          securityCheck =
            goCheckWithSource "security" fullSource
              ''
                actionlint .github/workflows/*.yml
                gosec -quiet -nosec-require-justification -nosec-require-rules ./...
                govulncheck -db=file://${vulndb} ./...
                ${dependencyPolicyCommand self.packages.${system}.default.goModules}
                go run ./tools/check-source .
              ''
              [
                pkgs.actionlint
                pkgs.findutils
                pkgs.gawk
                pkgs.gnugrep
                pkgs.gosec
                pkgs.govulncheck
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

          tests = testSuite;
          integration = testSuite;
          contract = testSuite;
          replica = testSuite;
          portability = testSuite;
          provider-verify = testSuite;
          preview = testSuite;
          theme = testSuite;
          race = raceCheck;
          coverage = coverageCompile;
          fuzz = fuzzCheck;
          offline = testSuite;
          security = securityCheck;
          dependencies = securityCheck;

          repository-policy = repositoryPolicyCheck;
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
              pkgs.actionlint
              pkgs.nixfmt
              pkgs.ripgrep
              pkgs.skopeo
            ];
            shellHook = ''
              export GOFLAGS=-mod=readonly
              echo "EndlessFS development shell — run: nix flake check"
            '';
          };
        }
      );

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
