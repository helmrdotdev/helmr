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
      workerGroups = cfg.users.users.helmr-ci.extraGroups;
    in
    require (cfg.boot.kernel.sysctl."net.ipv4.ip_forward" == 1) "IPv4 forwarding is not enabled"
    && require (lib.elem "kvm" cfg.boot.kernelModules) "kvm kernel module is not requested"
    && require (lib.elem "kvm" workerGroups) "firecracker users are not added to kvm"
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
  fmt = commandCheck "fmt-check" ''
    unformatted="$(find . -name '*.go' -not -path './.git/*' -print | xargs gofmt -l)"
    if [ -n "$unformatted" ]; then
      printf '%s\n' "$unformatted" >&2
      exit 1
    fi
  '';
  squashfs-tools = helmrPackages.squashfsTools;
  deployment-bundle-finalizer =
    pkgs.runCommand "deployment-bundle-finalizer-check"
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
          go test ./internal/builder \
            -run '^(TestFinalizeBundleWritesExactAtomicDirectory|TestFinalizeBundlePublishesExactlyOneConcurrentWriter)$'
        touch "$out"
      '';
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
    bundle-builder-program =
      pkgs.runCommand "bundle-builder-program-check"
        {
          nativeBuildInputs = [
            helmrPackages.squashfsTools
          ];
        }
        ''
                    set -euo pipefail
                    export HOME="$TMPDIR/home"
                    mkdir -p "$HOME"

                    make_project() {
                      project="$1"
                      mkdir -p "$project/node_modules/@helmr/sdk" "$project/tasks"
                      cat >"$project/package.json" <<'JSON'
                    {"name":"builder-fixture","private":true,"type":"module"}
                    JSON
                      cat >"$project/node_modules/@helmr/sdk/package.json" <<'JSON'
                    {"name":"@helmr/sdk","type":"module","exports":"./index.js"}
                    JSON
                      cat >"$project/node_modules/@helmr/sdk/index.js" <<'JS'
                    const brand = Symbol.for("helmr.sdk.v0.definition")
                    export function defineConfig(config) { return config }
                    export function task(config) {
                      return Object.freeze({
                        [brand]: Object.freeze({
                          kind: "task",
                          id: config.id,
                          hasPayload: false,
                          handler: config.run,
                        }),
                      })
                    }
                    JS
                      cat >"$project/helmr.config.ts" <<'TS'
                    import { defineConfig } from "@helmr/sdk"
                    export default defineConfig({ dirs: ["tasks"], ignorePatterns: [] })
                    TS
                      cat >"$project/tasks/hello.ts" <<'TS'
                    import { task } from "@helmr/sdk"
                    export const hello = task({ id: "hello", run: () => "hello" })
                    TS
                    }

                    for build in first second; do
                      project="$TMPDIR/$build/project"
                      work="$TMPDIR/$build/work"
                      images="$TMPDIR/$build/images"
                      mkdir -p "$work" "$images"
                      printf '[]' >"$images/images.json"
                      make_project "$project"
                      ${helmrPackages.bundleBuilder}/bin/bundle-builder \
                        --project "$project" \
                        --work "$work" \
                        --analysis-output "$images/build-plan.json" \
                        --runtime-descriptor ${helmrPackages.runtimeRelease}/runtime.descriptor.json \
                        --runtime-metadata ${helmrPackages.runtimeRelease}/runtime.metadata.json \
                        --compiler-descriptor ${helmrPackages.compiler}/compiler.descriptor.json \
                        --node ${helmrPackages.runtimeRelease}/tree/bin/node \
                        --node-loader ${helmrPackages.runtimeRelease}/tree/lib/ld-linux-x86-64.so.2 \
                        --node-library-path ${helmrPackages.runtimeRelease}/tree/lib \
                        --config-evaluator ${helmrPackages.compiler}/tree/helmr/config-evaluator.mjs \
                        --program-compiler ${helmrPackages.compiler}/tree/helmr/program-compiler.mjs \
                        --encoder ${helmrPackages.squashfsTools}/bin/mksquashfs
                      ${helmrPackages.bundleBuilder}/bin/bundle-builder \
                        --project "$project" \
                        --work "$work" \
                        --prepare-output "$TMPDIR/$build/prepared" \
                        --runtime-descriptor ${helmrPackages.runtimeRelease}/runtime.descriptor.json \
                        --runtime-metadata ${helmrPackages.runtimeRelease}/runtime.metadata.json \
                        --compiler-descriptor ${helmrPackages.compiler}/compiler.descriptor.json \
                        --node ${helmrPackages.runtimeRelease}/tree/bin/node \
                        --node-loader ${helmrPackages.runtimeRelease}/tree/lib/ld-linux-x86-64.so.2 \
                        --node-library-path ${helmrPackages.runtimeRelease}/tree/lib \
                        --config-evaluator ${helmrPackages.compiler}/tree/helmr/config-evaluator.mjs \
                        --program-compiler ${helmrPackages.compiler}/tree/helmr/program-compiler.mjs \
                        --encoder ${helmrPackages.squashfsTools}/bin/mksquashfs
                      cp -a "$project" "$TMPDIR/$build/program-project"
                      ${helmrPackages.bundleBuilder}/bin/bundle-builder \
                        --prepared "$TMPDIR/$build/prepared" \
                        --program-project "$TMPDIR/$build/program-project" \
                        --work "$work" \
                        --bundle-output "$TMPDIR/$build/bundle" \
                        --workspace-images "$images/images.json" \
                        --expected-plan "$images/build-plan.json" \
                        --runtime-descriptor ${helmrPackages.runtimeRelease}/runtime.descriptor.json \
                        --runtime-metadata ${helmrPackages.runtimeRelease}/runtime.metadata.json \
                        --compiler-descriptor ${helmrPackages.compiler}/compiler.descriptor.json \
                        --encoder ${helmrPackages.squashfsTools}/bin/mksquashfs
                    done
          diff -r "$TMPDIR/first/bundle" "$TMPDIR/second/bundle"
                    touch "$out"
        '';
    worker-host = helmrPackages.workerHost;
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
