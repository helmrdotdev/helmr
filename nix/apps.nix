{
  system,
  nixpkgs,
  nixpkgs-unstable,
  helmrPackages,
}:

let
  pkgs = import nixpkgs { inherit system; };
  pkgsUnstable = import nixpkgs-unstable { inherit system; };
  toolsets = import ./build-support/toolsets.nix {
    inherit
      pkgs
      pkgsUnstable
      helmrPackages
      ;
  };
  helmrApp = {
    type = "app";
    program = "${helmrPackages.helmr}/bin/helmr";
    meta.description = "run the Helmr CLI";
  };

  app =
    name: description: runtimeInputs: text:
    let
      program = pkgs.writeShellApplication {
        inherit name runtimeInputs text;
      };
    in
    {
      type = "app";
      program = "${program}/bin/${name}";
      meta.description = description;
    };

  ciApps = {
    ci-policy =
      app "ci-policy" "run repository policy and release script checks for CI" toolsets.ciChecks
        ''
          bun install --frozen-lockfile --ignore-scripts
          bun audit
          actionlint
          scripts/security-checks.sh
          bash -n scripts/dev-console-stack.sh
          bash tests/ci_workflow_test.sh
          bash tests/install_test.sh
          bash tests/release_manifest_test.sh
          bash tests/release_artifact_contracts_test.sh
          bash tests/release_workflow_test.sh
          bash tests/release_manifest_verify_test.sh
          bash tests/release_worker_ami_cleanup_test.sh
          bash tests/release_worker_image_identity_test.sh
          bash tests/pre_aws_release_gate_test.sh
          bash tests/aws_bootstrap_helmr_secrets_test.sh
          bash tests/aws_release_artifacts_test.sh
          bash tests/platform_release_materialize_test.sh
          bash tests/platform_release_publish_test.sh
          bash tests/publish_materialized_platform_release_test.sh
          bash tests/release_smoke_selector_test.sh
          bash tests/runtime_naming_contract_test.sh
          bash tests/worker_host_bundle_test.sh
          bash tests/worker_runtime_bundle_test.sh
          bash tests/linux_worker_host_bundle_materialize_test.sh
          bash tests/netboot_inputs_test.sh
          bash tests/boot_artifacts_make_test.sh
          bash tests/guest_init_cgroup_test.sh
        '';
    ci-generated =
      app "ci-generated" "check generated artifacts and formatting for CI" toolsets.ciChecks
        ''
          bun install --frozen-lockfile --ignore-scripts
          scripts/build-compiler-entry.sh --check
          scripts/build-runtime-entry.sh --check
          make generate
          make fmt
          make console-build
          git diff --exit-code
        '';
    ci-typescript =
      app "ci-typescript" "run TypeScript type checks and tests for CI" toolsets.ciChecks
        ''
          ${pkgs.lib.optionalString pkgs.stdenv.isLinux ''
            export LD_LIBRARY_PATH=${pkgs.lib.makeLibraryPath [ pkgs.stdenv.cc.cc.lib ]}
          ''}
          bun install --frozen-lockfile --ignore-scripts
          scripts/check-dev-samples.sh
          bun run typecheck
          bun run test:ts
          bun run build:web
        '';
    ci-go-lint =
      app "ci-go-lint" "run Go lint checks with embedded console assets for CI" toolsets.ciChecks
        ''
          bun install --frozen-lockfile --ignore-scripts
          make lint
        '';
    ci-go-build =
      app "ci-go-build" "build Go commands with embedded console assets for CI" toolsets.ciChecks
        ''
          bun install --frozen-lockfile --ignore-scripts
          make build
        '';
    ci-go-race =
      app "ci-go-race" "run Go race tests with embedded console assets for CI" toolsets.ciChecks
        ''
          export HELMR_SKIP_POSTGRES_TESTS=1
          bun install --frozen-lockfile --ignore-scripts
          make test-race
        '';
    ci-linux-compile =
      app "ci-linux-compile" "cross-compile Linux Go test binaries for CI" toolsets.ciChecks
        ''
          make test-linux-compile
        '';
    ci-firecracker-probe =
      app "ci-firecracker-probe" "validate the pinned Firecracker probe output on Linux"
        toolsets.runtimeProbe
        ''
          if [ "$(uname -s)" != "Linux" ] || [ "$(uname -m)" != "x86_64" ]; then
            echo "ci-firecracker-probe requires an x86_64 Linux host." >&2
            exit 1
          fi
          FIRECRACKER_PATH="$(command -v firecracker)"
          export FIRECRACKER_PATH
          go test ./internal/firecracker -run '^TestPackagedFirecrackerProbeOutputIsAccepted$' -count=1
        '';
    ci-linux-lint =
      app "ci-linux-lint" "run Linux-targeted Go static analysis for CI" toolsets.ciChecks
        ''
          bun install --frozen-lockfile --ignore-scripts
          make console-build
          CGO_ENABLED=0 GOOS=linux GOARCH=amd64 staticcheck -tags embed_console ./...
        '';
    ci-infra-test =
      app "ci-infra-test" "run AWS module tests with pinned OpenTofu" toolsets.infraTest
        ''
          for module in bootstrap controlplane network release-artifacts worker worker-image; do
            (
              cd "infra/aws/modules/$module"
              if [ "$module" = worker-image ]; then
                bash tests/prepare_root_test.sh
              fi
              tofu init -backend=false -input=false
              tofu fmt -check -recursive
              tofu test
            )
          done
          for stack in quickstart stacks/release-build standard; do
            (
              cd "infra/aws/$stack"
              tofu init -backend=false -input=false
              tofu fmt -check -recursive
              tofu test
            )
          done
        '';
    ci-postgres = app "ci-postgres" "run Postgres-backed CI tests" toolsets.appRuntime ''
      exec ./scripts/ci-postgres.sh "$@"
    '';
  };
in
ciApps
// {
  default = helmrApp;
  helmr = helmrApp;
  ci-checks = app "ci-checks" "run repository checks for CI" [ ] ''
    ${ciApps.ci-policy.program}
    ${ciApps.ci-generated.program}
    ${ciApps.ci-typescript.program}
    ${ciApps.ci-go-lint.program}
    ${ciApps.ci-go-build.program}
    ${ciApps.ci-go-race.program}
    ${ciApps.ci-linux-compile.program}
    ${pkgs.lib.optionalString (system == "x86_64-linux") ciApps.ci-firecracker-probe.program}
    ${ciApps.ci-linux-lint.program}
    ${ciApps.ci-infra-test.program}
    ${ciApps.ci-postgres.program}
  '';
  dev = app "dev" "run the local Helmr control plane and console dashboard" toolsets.appRuntime ''
    exec ./scripts/dev-console-stack.sh "$@"
  '';
  measure-dispatch = app "measure-dispatch" "measure PostgreSQL dispatch discovery" toolsets.base ''
    exec ./scripts/measure-dispatch.sh "$@"
  '';
  ci-boot-artifacts =
    app "ci-boot-artifacts" "build and stage guest boot artifacts for CI" toolsets.appRuntime
      ''
        exec ./scripts/ci-boot-artifacts.sh "$@"
      '';
  ci-boot-artifacts-repro =
    app "ci-boot-artifacts-repro" "prove guest boot artifact reproducibility for CI"
      (
        toolsets.appRuntime
        ++ [
          pkgs.gnutar
          pkgs.nix
        ]
      )
      ''
        bash ./tests/netboot_inputs_test.sh
        bash ./tests/boot_artifacts_make_test.sh
        bash ./tests/guest_init_cgroup_test.sh
        exec ./tests/boot_artifacts_reproducibility_test.sh "$@"
      '';
  doctor = app "doctor" "check Helmr host prerequisites" toolsets.appRuntime ''
    exec ./scripts/doctor.sh "$@"
  '';
}
// pkgs.lib.optionalAttrs (system == "x86_64-linux") {
  smoke-linux =
    app "smoke-linux" "build artifacts and check Linux Firecracker prerequisites" toolsets.appRuntime
      ''
        if [ "$(uname -s)" != "Linux" ]; then
          echo "smoke-linux requires a Linux host with KVM/Firecracker." >&2
          echo "Use nix run .#doctor on macOS, and run this app on a Linux host." >&2
          exit 1
        fi
        if [ "$(uname -m)" != "x86_64" ]; then
          echo "smoke-linux requires an x86_64 Linux host." >&2
          exit 1
        fi

        export ARCH=''${ARCH:-x86_64}
        export WORKER_IMAGES_DIR=''${WORKER_IMAGES_DIR:-$PWD/images}
        export FIRECRACKER_PATH=''${FIRECRACKER_PATH:-$(command -v firecracker)}
        export JAILER_PATH=''${JAILER_PATH:-$(command -v jailer)}
        export MKFS_EXT4_PATH=''${MKFS_EXT4_PATH:-${helmrPackages.workerHost}/bin/mkfs.ext4}
        export MKE2FS_CONFIG_PATH=''${MKE2FS_CONFIG_PATH:-${helmrPackages.workerHost}/share/helmr/mke2fs.conf}
        export JAILER_UID=''${JAILER_UID:-$(id -u)}
        export JAILER_GID=''${JAILER_GID:-$(id -g)}
        export JAILER_CGROUP_VERSION=''${JAILER_CGROUP_VERSION:-2}
        export XDG_DATA_HOME=''${XDG_DATA_HOME:-$PWD/.helmr-smoke/data}
        export XDG_RUNTIME_DIR=''${XDG_RUNTIME_DIR:-$PWD/.helmr-smoke/runtime}
        mkdir -p "$XDG_DATA_HOME" "$XDG_RUNTIME_DIR"

        ./scripts/doctor.sh linux
        make images
      '';
}
