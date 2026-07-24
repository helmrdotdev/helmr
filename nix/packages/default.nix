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
  runtimeTrustedRoot = pkgs.fetchurl {
    url = "https://raw.githubusercontent.com/sigstore/root-signing/83a0a6eed690d1fa1443e62d3fd3c9a2f85d6147/targets/trusted_root.json";
    hash = "sha256-ZJTiHqc/p+52n4X1fVo+aghyXq4eOMdV/DUXyea8C2Y=";
  };
  managedNode =
    let
      source = pkgs.nodejs-slim_24.override { enableNpm = false; };
    in
    assert lib.assertMsg (
      source.version == "24.16.0"
    ) "managed runtime requires pinned nodejs_24 24.16.0";
    source.overrideAttrs (old: {
      configureFlags = builtins.filter (flag: flag != "--openssl-use-def-ca-store") old.configureFlags;
      postInstall = (old.postInstall or "") + ''
        test "$("$out/bin/node" -p \
          'String(Boolean(process.config.variables.node_use_openssl_ca))')" = false
      '';
    });
  managedRuntime = pkgs.callPackage ./runtime.nix {
    nodejs_24 = managedNode;
    inherit squashfsTools;
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
  firecrackerReleaseVersion = "1.13.2";
  firecrackerRelease =
    {
      x86_64-linux = {
        arch = "x86_64";
        hash = "sha256-pts7RR9QDf2CmJRH/r9Utci7iSnk7nx/hKlpXxMNpUc=";
      };
      aarch64-linux = {
        arch = "aarch64";
        hash = "sha256-pkwLkTspuOpLWZDsuUqSy3y064UAFbaaoWgNJdnLQb8=";
      };
    }
    .${system} or null;
in
{
  inherit helmr;
  inherit staticcheck;
  inherit unparam;
  inherit squashfsTools;
  inherit runtimeTrustedRoot;
  default = helmr;
  bun = pkgsBun.bun;
  apko = if pkgsUnstable ? apko then pkgsUnstable.apko else pkgs.apko;
}
//
  lib.optionalAttrs
    (
      pkgs.stdenv.isLinux
      && builtins.elem system [
        "x86_64-linux"
        "aarch64-linux"
      ]
    )
    {
      inherit managedRuntime;
      standardToolchain =
        let
          releaseTool = buildGo126Module {
            pname = "helmr-tool-candidate";
            version = "0";
            src = lib.fileset.toSource {
              root = ../..;
              fileset = lib.fileset.unions [
                ../../go.mod
                ../../go.sum
                ../../internal
              ];
            };
            vendorHash = "sha256-nm9r7z+b+TRvWgMDXo0eUwVKNkEuQIsF3sFGCDiJQ5g=";
            subPackages = [ "internal/cmd/tool-release" ];
          };
        in
        pkgs.callPackage ./toolchain.nix {
          inherit managedRuntime;
          inherit releaseTool squashfsTools;
        };
    }
// lib.optionalAttrs (system == "x86_64-linux") {
  managerRelease = pkgs.callPackage ./managers.nix {
    inherit squashfsTools;
  };
}
// lib.optionalAttrs (firecrackerRelease != null) {
  firecrackerRuntime = pkgs.stdenvNoCC.mkDerivation {
    pname = "firecracker-runtime";
    version = firecrackerReleaseVersion;

    src = pkgs.fetchurl {
      url = "https://github.com/firecracker-microvm/firecracker/releases/download/v${firecrackerReleaseVersion}/firecracker-v${firecrackerReleaseVersion}-${firecrackerRelease.arch}.tgz";
      hash = firecrackerRelease.hash;
    };

    installPhase = ''
      runHook preInstall

      release_dir=.
      install -d "$out/bin" "$out/share/firecracker"
      install -m 0755 "$release_dir/firecracker-v${firecrackerReleaseVersion}-${firecrackerRelease.arch}" "$out/bin/firecracker"
      install -m 0755 "$release_dir/jailer-v${firecrackerReleaseVersion}-${firecrackerRelease.arch}" "$out/bin/jailer"
      install -m 0644 "$release_dir/LICENSE" "$release_dir/NOTICE" "$release_dir/THIRD-PARTY" "$out/share/firecracker/"

      runHook postInstall
    '';
  };
}
