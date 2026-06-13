{
  system,
  nixpkgs,
  helmrPackages,
}:

let
  pkgs = import nixpkgs { inherit system; };
  inherit (pkgs) lib;

  commandCheck =
    name: command:
    pkgs.runCommand name
      {
        nativeBuildInputs = [
          pkgs.go
          pkgs.git
        ];
        src = ../.;
      }
      ''
        cp -R "$src" source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home"
        mkdir -p "$HOME"
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
      buildkitService = cfg.systemd.services.helmr-buildkit.serviceConfig;
      buildkitExecStart = buildkitService.ExecStart;
      workerGroups = cfg.users.users.helmr-ci.extraGroups;
    in
    require (buildkitService.User == "helmr-buildkit") "helmr-buildkit service user changed"
    && require (buildkitService.Group == "helmr-buildkit") "helmr-buildkit service group changed"
    && require (cfg.boot.kernel.sysctl."net.ipv4.ip_forward" == 1) "IPv4 forwarding is not enabled"
    && require (
      cfg.boot.kernel.sysctl."user.max_user_namespaces" == 16384
    ) "user namespace limit changed"
    && require (lib.elem "kvm" cfg.boot.kernelModules) "kvm kernel module is not requested"
    && require (lib.elem "kvm" workerGroups) "firecracker users are not added to kvm"
    && require (lib.elem "helmr-buildkit" workerGroups) "firecracker users are not added to helmr-buildkit"
    && require (lib.hasInfix ''KERNEL=="kvm", GROUP="helmr-vmm", MODE="0660"'' cfg.services.udev.extraRules) "kvm udev rule changed"
    && require (lib.hasInfix "rootlesskit" buildkitExecStart) "BuildKit service no longer starts through rootlesskit"
    && require (lib.hasInfix "--net=slirp4netns" buildkitExecStart) "BuildKit service no longer uses slirp4netns"
    && require (lib.hasInfix "buildkitd" buildkitExecStart) "BuildKit service no longer starts buildkitd"
    && require (lib.hasInfix "unix:///run/helmr/buildkit/buildkitd.sock" buildkitExecStart) "BuildKit socket path changed";

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
  fmt = commandCheck "fmt-check" ''
    unformatted="$(find . -name '*.go' -not -path './.git/*' -print | xargs gofmt -l)"
    if [ -n "$unformatted" ]; then
      printf '%s\n' "$unformatted" >&2
      exit 1
    fi
  '';
}
// lib.optionalAttrs pkgs.stdenv.isLinux {
  firecracker-host-module = firecrackerHostModuleCheck;
}
