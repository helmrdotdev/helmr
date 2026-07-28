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
          pkgs.go_1_26
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
    && require (buildkitService.Delegate == true) "helmr-buildkit service delegation changed"
    && require (buildkitService.CPUQuota == "100%") "helmr-buildkit CPU limit changed"
    && require (buildkitService.MemoryMax == "2G") "helmr-buildkit memory limit changed"
    && require (buildkitService.MemorySwapMax == 0) "helmr-buildkit swap limit changed"
    && require (buildkitService.TasksMax == 1024) "helmr-buildkit task limit changed"
    && require (buildkitService.MemoryOOMGroup == true) "helmr-buildkit OOM group policy changed"
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
  squashfs-tools = helmrPackages.squashfsTools;
}
// lib.optionalAttrs (system == "x86_64-linux") (
  let
    platformAcquisitionCgroupTestBinary =
      pkgs.runCommand "platform-acquisition-cgroup-test-binary"
        {
          nativeBuildInputs = [ pkgs.go_1_26 ];
          src = ../.;
        }
        ''
          cp -R "$src" source
          chmod -R u+w source
          cd source
          export HOME="$TMPDIR/home"
          mkdir -p "$HOME" "$out/bin"
          cp -R ${helmrPackages.helmr.goModules} vendor
          export GOFLAGS=-mod=vendor
          export GOPROXY=off
          export GOSUMDB=off
          export GOTOOLCHAIN=local
          export CGO_ENABLED=0
          go test -c -o "$out/bin/worker-cgroup.test" ./internal/worker
        '';
  in
  {
    firecracker-host-module = firecrackerHostModuleCheck;
    platform-acquisition-cgroup = pkgs.testers.runNixOSTest {
      name = "platform-acquisition-cgroup";
      nodes.machine =
        { ... }:
        {
          virtualisation.memorySize = 2048;
          systemd.services.platform-acquisition-cgroup-test = {
            environment.HELMR_PLATFORM_ACQUISITION_CGROUP_INTEGRATION = "1";
            serviceConfig = {
              Type = "oneshot";
              Delegate = true;
              DelegateSubgroup = "supervisor";
              TasksMax = "infinity";
              ExecStart = "${platformAcquisitionCgroupTestBinary}/bin/worker-cgroup.test -test.run=^TestPlatformAcquisitionCgroupIntegration$ -test.v";
            };
          };
        };
      testScript = ''
        machine.start()
        machine.wait_for_unit("multi-user.target")
        machine.succeed("systemctl start platform-acquisition-cgroup-test.service")
      '';
    };
    platform-release = helmrPackages.platformRelease;
    program-archive-contract =
      pkgs.runCommand "program-archive-contract-check"
        {
          nativeBuildInputs = [
            pkgs.go_1_26
            helmrPackages.squashfsTools
          ];
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
          HELMR_SQUASHFS_ENCODER=${helmrPackages.squashfsTools}/bin/mksquashfs \
            go test ./internal/deployment -run '^TestPinnedProgramEncoder$'
          touch "$out"
        '';
  }
)
