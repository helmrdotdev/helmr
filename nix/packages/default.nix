{
  self,
  system,
  nixpkgs,
  nixpkgs-unstable,
  nixpkgs-bun,
}:

let
  pkgs = import nixpkgs { inherit system; };
  inherit (pkgs) lib;
  pkgsUnstable = import nixpkgs-unstable { inherit system; };
  pkgsBun = import nixpkgs-bun { inherit system; };
  squashfsTools = pkgs.callPackage ./squashfs-tools.nix { };
  timezoneData = pkgs.callPackage ./timezone-data.nix { };
  runtimeReleaseUnchecked = pkgs.callPackage ./runtime-release.nix { inherit squashfsTools; };
  compiler = pkgs.callPackage ./compiler.nix { };
  bundleBuilder = pkgs.callPackage ./bundle-builder.nix {
    buildGoModule = buildGo126Module;
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
  buildGo126Module = pkgs.callPackage "${nixpkgs}/pkgs/build-support/go/module.nix" {
    go = pkgs.go_1_26;
  };
  staticcheck = buildGo126Module {
    pname = "staticcheck";
    version = "2026.1";

    src = pkgs.fetchFromGitHub {
      owner = "dominikh";
      repo = "go-tools";
      rev = "2026.1";
      hash = "sha256-cj/pHKwp7eGuOO1zhv5bFmuPHgsFytktLQmihhdYkfY=";
    };

    vendorHash = "sha256-Wu8+e0r0bkztLbxekbHktoKjg6c8q7ls5APSEdO8CKs=";
    subPackages = [ "cmd/staticcheck" ];
  };
  deadcode = buildGo126Module {
    pname = "deadcode";
    version = "0.34.0";

    src = pkgs.fetchFromGitHub {
      owner = "golang";
      repo = "tools";
      tag = "v0.34.0";
      hash = "sha256-C+P2JoD4NzSAkAQuA20bVrfLZrMHXekvXn8KPOM5Nj4=";
    };

    vendorHash = "sha256-UZNYHx5y+kRp3AJq6s4Wy+k789GDG7FBTSzCTorVjgg=";
    subPackages = [ "cmd/deadcode" ];
    doCheck = false;
  };
  unparam = buildGo126Module {
    pname = "unparam";
    version = "2025-10-27";

    src = pkgs.fetchFromGitHub {
      owner = "mvdan";
      repo = "unparam";
      rev = "5beb8c8f8f15";
      hash = "sha256-Xxl2ERHRqKbC0fqFSMqw5+yF/UiqEtz0xaFCBdYy85k=";
    };

    vendorHash = "sha256-TzyN1epeEmIuAorNO3X6xBQSANDnPeJ4mbWPNjB0mrk=";
  };
  revision = self.shortRev or self.dirtyShortRev or "dirty";
  helmrVersion = "0.0.0-dev+${revision}";
  helmr = pkgs.callPackage ./helmr.nix {
    buildGoModule = buildGo126Module;
    version = helmrVersion;
    bun = pkgsBun.bun;
  };
  runtimeRelease =
    pkgs.runCommand "helmr-runtime-release-verified"
      {
        nativeBuildInputs = [ pkgs.go_1_26 ];
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
  worker = pkgs.callPackage ./worker.nix { buildGoModule = buildGo126Module; };
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
    helmr
    worker
    ;
  inherit staticcheck;
  inherit deadcode;
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
