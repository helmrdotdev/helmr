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
