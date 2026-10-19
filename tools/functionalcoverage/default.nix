# This function has no eager reference to the producer from its consumer config.
{
  pkgs,
  go,
  fullSource,
  coverageSource,
  vendor,
  seed,
  browser,
}:
let
  lib = pkgs.lib;
  system = pkgs.stdenv.hostPlatform.system;
  helper = builtins.derivation {
    inherit system;
    name = "endlessfs-functional-coverage-helper";
    builder = "${pkgs.bash}/bin/bash";
    args = [
      "-euc"
      ''
        export HOME="$TMPDIR" GOCACHE="$TMPDIR/go-cache" GOMODCACHE="$TMPDIR/go-mod-cache"
        export XDG_CONFIG_HOME="$HOME/.config"
        export GOENV=off GOWORK=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOFLAGS= CGO_ENABLED=0
        ${pkgs.coreutils}/bin/mkdir -p "$out/bin"
        cd ${fullSource}
        ${go}/bin/go telemetry off
        exec ${go}/bin/go build -trimpath -buildvcs=false -o "$out/bin/functionalcoverage" ./tools/functionalcoverage
      ''
    ];
    preferLocalBuild = true;
    allowSubstitutes = false;
  };
  base = {
    Schema = "endlessfs-canonical-functional-coverage-v1";
    Profile = "linux-amd64-canonical-functional-v1";
    FullSource = toString fullSource;
    CoverageSource = toString coverageSource;
    Vendor = toString vendor;
    Go = "${go}/bin/go";
    GoRoot = "${go}/share/go";
    GoVersion = go.version;
    Native = "${pkgs.stdenv.cc}/bin/cc";
    Browser = "${browser}/bin/chrome-headless-shell";
    Decoder = "${pkgs.libraw}/bin/dcraw_emu";
    Gawk = "${pkgs.gawk}/bin/gawk";
    Bash = "${pkgs.bash}/bin/bash";
    Seed = toString seed;
    Policy = "${coverageSource}/tools/coverage.awk";
    Path = lib.makeBinPath [
      pkgs.bash
      go
      pkgs.stdenv.cc
      browser
      pkgs.libraw
      pkgs.gawk
      pkgs.nix
      pkgs.git
    ];
    Nix = "${pkgs.nix}/bin/nix";
    Git = "${pkgs.git}/bin/git";
    NamespaceLauncher = "${pkgs.util-linuxMinimal}/bin/unshare";
    Helper = "${helper}/bin/functionalcoverage";
    ProducerInstallable = "path:${fullSource}#packages.x86_64-linux.local-functional-coverage";
    Legacy = "";
  };
  config = pkgs.writeText "functional-coverage-producer.json" (builtins.toJSON base);
  producer = builtins.derivation {
    inherit system;
    name = "endlessfs-canonical-functional-coverage";
    builder = "${helper}/bin/functionalcoverage";
    args = [
      "produce"
      config
    ];
    __structuredAttrs = true;
    exportReferencesGraph.closure = [
      helper
      config
      fullSource
      coverageSource
      vendor
      go
      pkgs.stdenv.cc
      browser
      pkgs.libraw
      pkgs.gawk
      pkgs.bash
      seed
      pkgs.nix
      pkgs.git
      pkgs.util-linuxMinimal
    ];
    preferLocalBuild = true;
    allowSubstitutes = false;
    # Existing ordinary Nix sandbox. No __noChroot/network/resource exception.
  };
in
{
  inherit helper producer;
  consumer =
    legacy:
    let
      consumerConfig = pkgs.writeText "functional-coverage-consumer.json" (
        builtins.toJSON (base // { Legacy = legacy; })
      );
    in
    pkgs.writeShellApplication {
      name = "endlessfs-local-functional-coverage-consumer";
      runtimeInputs = [ ];
      text = ''exec ${helper}/bin/functionalcoverage consume ${consumerConfig} "$@"'';
    };
}
