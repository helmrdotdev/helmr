{
  self,
  system,
  nixpkgs,
  nixpkgs-unstable,
  nixpkgs-bun,
  nixpkgs-go,
}:

let
  pkgs = import nixpkgs { inherit system; };
  inherit (pkgs) lib;
  pkgsUnstable = import nixpkgs-unstable { inherit system; };
  pkgsBun = import nixpkgs-bun { inherit system; };
  goPackage = pkgs.callPackage "${nixpkgs-go}/pkgs/development/compilers/go/1.27.nix" {
    inherit buildGo127Module;
  };
  squashfsTools = pkgs.callPackage ./squashfs-tools.nix { };
  timezoneData = pkgs.callPackage ./timezone-data.nix { };
  runtimeReleaseUnchecked = pkgs.callPackage ./runtime-release.nix { inherit squashfsTools; };
  compiler = pkgs.callPackage ./compiler.nix { };
  bundleBuilder = pkgs.callPackage ./bundle-builder.nix {
    buildGoModule = buildGo127Module;
  };
  bundleBuilderImage = pkgs.callPackage ./bundle-builder-image.nix {
    inherit
      bundleBuilder
      compiler
      runtimeRelease
      squashfsTools
      timezoneData
      ;
    bun = pkgsBun.bun;
  };
  buildGo127Module = pkgs.callPackage "${nixpkgs}/pkgs/build-support/go/module.nix" {
    go = goPackage;
  };
  staticcheck = buildGo127Module {
    pname = "staticcheck";
    version = "2026.2.1";

    src = pkgs.fetchFromGitHub {
      owner = "dominikh";
      repo = "go-tools";
      rev = "2026.2.1";
      hash = "sha256-wellofnfLW4lQy68UQyFJfvrKCfrZ/EllLODX1g9taY=";
    };

    vendorHash = "sha256-3no4wPqFG0RfSsWB0z8EYxeoZ30t+Zf7ZayzFCLEm2A=";
    subPackages = [ "cmd/staticcheck" ];
  };
  unparam = buildGo127Module {
    pname = "unparam";
    version = "2026-08-23";

    src = pkgs.fetchFromGitHub {
      owner = "mvdan";
      repo = "unparam";
      rev = "2fa3d841b0c8";
      hash = "sha256-NXsiP+rjxGPrVDk9Xl62NRGQBVlD/4nSjznhAxrsrU4=";
    };

    vendorHash = "sha256-ClZv8xMyxFuGNnLy135R/MIgWp3MWpZU3bq4FJAfK8U=";
  };
  revision = self.shortRev or self.dirtyShortRev or "dirty";
  releaseVersion = builtins.getEnv "HELMR_PLATFORM_VERSION";
  platformVersion = if releaseVersion == "" then "0.0.0-dev+${revision}" else releaseVersion;
  sourceCommit =
    self.rev or (
      if self ? dirtyRev then
        builtins.substring 0 40 self.dirtyRev
      else
        "0000000000000000000000000000000000000000"
    );
  helmr = pkgs.callPackage ./helmr.nix {
    buildGoModule = buildGo127Module;
    version = platformVersion;
    inherit sourceCommit;
    bun = pkgsBun.bun;
  };
  runtimeRelease =
    pkgs.runCommand "helmr-runtime-release-verified"
      {
        nativeBuildInputs = [ goPackage ];
        src = self;
      }
      ''
        cp -R "$src" source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME"
        cp -R ${helmr.goModules} vendor
        export GOFLAGS=-mod=vendor
        export GOPROXY=off
        export GOSUMDB=off
        export GOTOOLCHAIN=local
        export CGO_ENABLED=0
        HELMR_RUNTIME_RELEASE_DIR=${runtimeReleaseUnchecked} \
          go test ./internal/deployment -run '^TestVerifyPinnedRuntimeRelease$'
        cd ..
        cp -a ${runtimeReleaseUnchecked} "$out"
      '';
  firecrackerReleaseVersion = "1.16.1";
  worker = pkgs.callPackage ./worker.nix {
    buildGoModule = buildGo127Module;
    version = platformVersion;
    inherit sourceCommit;
  };
  firecrackerRuntime = pkgs.stdenvNoCC.mkDerivation {
    pname = "firecracker-runtime";
    version = firecrackerReleaseVersion;

    src = pkgs.fetchurl {
      url = "https://github.com/firecracker-microvm/firecracker/releases/download/v${firecrackerReleaseVersion}/firecracker-v${firecrackerReleaseVersion}-x86_64.tgz";
      hash = "sha256-OCoCqGnk1tXLFMQFd/lUXoRYAh6osLLT/BDsFNnCQuY=";
    };

    installPhase = ''
      runHook preInstall

      release_dir=.
      install -d "$out/bin" "$out/share/firecracker"
      install -m 0755 "$release_dir/cpu-template-helper-v${firecrackerReleaseVersion}-x86_64" "$out/bin/cpu-template-helper"
      install -m 0755 "$release_dir/firecracker-v${firecrackerReleaseVersion}-x86_64" "$out/bin/firecracker"
      install -m 0755 "$release_dir/jailer-v${firecrackerReleaseVersion}-x86_64" "$out/bin/jailer"
      install -m 0644 "$release_dir/LICENSE" "$release_dir/NOTICE" "$release_dir/THIRD-PARTY" "$out/share/firecracker/"

      runHook postInstall
    '';
  };
  substrateGenerator = pkgs.pkgsStatic.e2fsprogs;
in
{
  inherit
    firecrackerRuntime
    goPackage
    helmr
    worker
    ;
  inherit staticcheck;
  inherit unparam;
  inherit squashfsTools;
  inherit timezoneData;
  default = helmr;
  bun = pkgsBun.bun;
  apko = if pkgsUnstable ? apko then pkgsUnstable.apko else pkgs.apko;
}
// lib.optionalAttrs (system == "x86_64-linux") (rec {
  inherit
    compiler
    bundleBuilder
    bundleBuilderImage
    runtimeRelease
    ;
  workerHost = pkgs.runCommand "helmr-worker-host" { } ''
    install -d "$out/bin" "$out/share/helmr"
    install -m 0755 "${firecrackerRuntime}/bin/cpu-template-helper" "$out/bin/cpu-template-helper"
    install -m 0755 "${worker}/bin/helmr-worker" "$out/bin/helmr-worker"
    install -m 0755 "${firecrackerRuntime}/bin/firecracker" "$out/bin/firecracker"
    install -m 0755 "${firecrackerRuntime}/bin/jailer" "$out/bin/jailer"
    install -m 0755 "${lib.getBin substrateGenerator}/bin/mke2fs" "$out/bin/mkfs.ext4"
    install -m 0444 ${./mke2fs.conf} "$out/share/helmr/mke2fs.conf"
  '';
  platformRelease = pkgs.callPackage ./platform-release.nix {
    inherit runtimeRelease;
  };
})
