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
  default = helmr;
  bun = pkgsBun.bun;
  apko = if pkgsUnstable ? apko then pkgsUnstable.apko else pkgs.apko;
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
