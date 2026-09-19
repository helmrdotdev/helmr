{
  system,
  nixpkgs,
  helmrPackages,
}:

let
  pkgs = import nixpkgs { inherit system; };
  inherit (pkgs) lib;

  vendoredGoCheck =
    name: nativeBuildInputs: command:
    pkgs.runCommand name
      {
        nativeBuildInputs = [ helmrPackages.goPackage ] ++ nativeBuildInputs;
        src = ../.;
      }
      ''
        cp -R "$src" source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME"
        cp -R ${helmrPackages.helmr.goModules} vendor
        export GOFLAGS=-mod=vendor
        export GOPROXY=off
        export GOSUMDB=off
        export GOTOOLCHAIN=local
        export CGO_ENABLED=0
        ${command}
        touch "$out"
      '';

  firecrackerHostEval = import (nixpkgs + "/nixos/lib/eval-config.nix") {
    inherit system;
    modules = [
      ./modules/nixos/firecracker-host.nix
      (
        { ... }:
        {
          system.stateVersion = "25.11";
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "none";
            fsType = "tmpfs";
          };
          users.users.helmr-ci = {
            isNormalUser = true;
          };
          services.helmr.firecrackerHost = {
            enable = true;
            users = [ "helmr-ci" ];
          };
        }
      )
    ];
  };

  require = condition: message: if condition then true else throw message;

  checkedFirecrackerHostModule =
    let
      cfg = firecrackerHostEval.config;
      workerGroups = cfg.users.users.helmr-ci.extraGroups;
    in
    require (cfg.boot.kernel.sysctl."net.ipv4.ip_forward" == 1) "IPv4 forwarding is not enabled"
    && require (lib.elem "kvm" cfg.boot.kernelModules) "kvm kernel module is not requested"
    && require (lib.elem "nft_tproxy" cfg.boot.kernelModules) "nft TPROXY kernel module is not requested"
    && require (lib.elem "kvm" workerGroups) "firecracker users are not added to kvm"
    && require (
      cfg.environment.sessionVariables.MKFS_EXT4_PATH
      == "${lib.getBin pkgs.pkgsStatic.e2fsprogs}/bin/mkfs.ext4"
    ) "the pinned substrate generator is not exported"
    && require (
      cfg.environment.sessionVariables.MKE2FS_CONFIG_PATH == toString ./packages/mke2fs.conf
    ) "the canonical mke2fs config is not exported"
    && require (lib.hasInfix ''KERNEL=="kvm", GROUP="helmr-vmm", MODE="0660"'' cfg.services.udev.extraRules) "kvm udev rule changed";

  firecrackerHostModuleCheck =
    assert checkedFirecrackerHostModule;
    pkgs.runCommand "firecracker-host-module-check" { } ''
      touch "$out"
    '';
in
{
  helmr-package = helmrPackages.helmr;
  helmr-smoke = pkgs.runCommand "helmr-smoke" { } ''
    export HOME="$TMPDIR/home"
    export XDG_CACHE_HOME="$TMPDIR/cache"
    mkdir -p "$HOME" "$XDG_CACHE_HOME"

    ${helmrPackages.helmr}/bin/helmr --version
    ${helmrPackages.helmr}/bin/helmr init --dir "$TMPDIR/project"
    test -f "$TMPDIR/project/helmr.config.ts"
    test -f "$TMPDIR/project/package.json"

    touch "$out"
  '';
  fmt =
    pkgs.runCommand "fmt-check"
      {
        nativeBuildInputs = [ helmrPackages.goPackage ];
        src = ../.;
      }
      ''
        unformatted="$(find "$src" -name '*.go' -print | xargs gofmt -l)"
        if [ -n "$unformatted" ]; then
          printf '%s\n' "$unformatted" >&2
          exit 1
        fi
        touch "$out"
      '';
  squashfs-tools = helmrPackages.squashfsTools;
  protected-egress = vendoredGoCheck "protected-egress-check" [ ] ''
    go test ./internal/secretproxy
  '';
  deployment-bundle-finalizer =
    vendoredGoCheck "deployment-bundle-finalizer-check" [ helmrPackages.squashfsTools ]
      ''
        HELMR_SQUASHFS_ENCODER=${helmrPackages.squashfsTools}/bin/mksquashfs \
          go test ./internal/builder \
            -run '^(TestFinalizeBundleWritesExactAtomicDirectory|TestFinalizeBundlePublishesExactlyOneConcurrentWriter)$'
      '';
  timezone-manifest =
    pkgs.runCommand "timezone-manifest-check" { src = ../internal/schedule/tzdb_names.txt; }
      ''
        LC_ALL=C sort -u "$src" > normalized
        cmp normalized "$src"
        diff -u ${helmrPackages.timezoneData}/tzdb_names.txt "$src"
        touch "$out"
      '';
  worker-host-target =
    assert lib.assertMsg (
      (system == "x86_64-linux") == (helmrPackages ? workerHost)
    ) "workerHost must be exported only for x86_64-linux";
    pkgs.runCommand "worker-host-target-check" { } "touch $out";
}
// lib.optionalAttrs (system == "x86_64-linux") {
  firecracker-host-module = firecrackerHostModuleCheck;
  worker-host = helmrPackages.workerHost;
  substrate-projection = vendoredGoCheck "substrate-projection-check" [ pkgs.e2fsprogs ] ''
    export HELMR_SUBSTRATE_MKFS_EXT4=${helmrPackages.workerHost}/bin/mkfs.ext4
    export HELMR_SUBSTRATE_MKE2FS_CONFIG=${helmrPackages.workerHost}/share/helmr/mke2fs.conf
    export HELMR_SUBSTRATE_E2FSCK=${lib.getBin pkgs.e2fsprogs}/bin/e2fsck
    export HELMR_SUBSTRATE_DEBUGFS=${lib.getBin pkgs.e2fsprogs}/bin/debugfs
    go test ./internal/substrate -run '^TestDeterministicExt4Projection$' -count=1 -v
  '';
  platform-release = helmrPackages.platformRelease;
  platform-release-publish-contract = vendoredGoCheck "platform-release-publish-contract-check" [ ] ''
    HELMR_PLATFORM_RELEASE_DIR=${helmrPackages.platformRelease} \
    HELMR_RUNTIME_RELEASE_DIR=${helmrPackages.runtimeRelease} \
      go test ./internal/deployment \
        -run '^(TestPublishPinnedPlatformRelease|TestVerifyPinnedRuntimeRelease)$'
  '';
  program-archive-contract =
    vendoredGoCheck "program-archive-contract-check" [ helmrPackages.squashfsTools ]
      ''
        HELMR_SQUASHFS_ENCODER=${helmrPackages.squashfsTools}/bin/mksquashfs \
          go test ./internal/deployment -run '^TestPinnedProgramEncoder$'
      '';
}
