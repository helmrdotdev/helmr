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
  ciChecksPath = pkgs.lib.makeBinPath toolsets.ciChecks;
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
in
{
  default = helmrApp;
  helmr = helmrApp;
  ci-checks = app "ci-checks" "run repository checks for CI" toolsets.ciChecks ''
    export PATH=${ciChecksPath}
    exec ${pkgs.bash}/bin/bash ./scripts/ci-checks.sh "$@"
  '';
  ci-policy =
    app "ci-policy" "run repository policy and release script checks for CI" toolsets.ciChecks
      ''
        bun install --frozen-lockfile --ignore-scripts
        bun audit
        actionlint
        scripts/security-checks.sh
        bash tests/install_test.sh
        bash tests/release_manifest_test.sh
        bash tests/release_workflow_test.sh
        bash tests/release_worker_ami_cleanup_test.sh
        bash tests/release_worker_image_identity_test.sh
        bash tests/pre_aws_release_gate_test.sh
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
        bun install --frozen-lockfile --ignore-scripts
        scripts/check-dev-samples.sh
        bun run typecheck
        bun run test:ts
      '';
  ci-go-test =
    app "ci-go-test" "run Go tests with embedded console assets for CI" toolsets.ciChecks
      ''
        bun install --frozen-lockfile --ignore-scripts
        make test
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
        bun install --frozen-lockfile --ignore-scripts
        make test-race
      '';
  ci-linux-compile =
    app "ci-linux-compile" "cross-compile Linux Go test binaries for CI" toolsets.ciChecks
      ''
        make test-linux-compile
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
        for module in bootstrap control worker worker-image; do
          (
            cd "infra/aws/modules/$module"
            tofu init -backend=false -input=false
            tofu fmt -check -recursive
            tofu test
          )
        done
      '';
  test = app "test" "run the full Helmr test recipe" toolsets.appRuntime "make test";
  lint = app "lint" "run Go vet with repository lint settings" toolsets.appRuntime "make lint";
  modernize = app "modernize" "apply Go modernizer fixes" toolsets.appRuntime "make modernize";
  modernize-check =
    app "modernize-check" "check Go modernizer fixes" toolsets.appRuntime
      "make modernize-check";
  dev = app "dev" "run the local Helmr control plane and console dashboard" toolsets.appRuntime ''
    exec ./scripts/dev-console-stack.sh "$@"
  '';
  ci-postgres = app "ci-postgres" "run Postgres-backed CI tests" toolsets.appRuntime ''
    exec ./scripts/ci-postgres.sh "$@"
  '';
  ci-buildkit = app "ci-buildkit" "run the BuildKit CI smoke test" toolsets.appRuntime ''
    exec ./scripts/ci-buildkit.sh "$@"
  '';
  ci-boot-artifacts =
    app "ci-boot-artifacts" "build and stage guest boot artifacts for CI" toolsets.appRuntime
      ''
        exec ./scripts/ci-boot-artifacts.sh "$@"
      '';
  fmt-check = app "fmt-check" "check Go formatting" toolsets.appRuntime ''
    unformatted="$(find . -name '*.go' -not -path './.git/*' -exec gofmt -l {} +)"
    if [ -n "$unformatted" ]; then
      printf '%s\n' "$unformatted" >&2
      exit 1
    fi
  '';
  images = app "images" "build Helmr boot artifacts" toolsets.appRuntime "make images";
  doctor = app "doctor" "check Helmr host prerequisites" toolsets.appRuntime ''
    exec ./scripts/doctor.sh "$@"
  '';
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
        export HELMR_WORKER_IMAGES_DIR=''${HELMR_WORKER_IMAGES_DIR:-$PWD/images}
        export HELMR_WORKER_FIRECRACKER_PATH=''${HELMR_WORKER_FIRECRACKER_PATH:-$(command -v firecracker)}
        export HELMR_WORKER_FIRECRACKER_JAILER_PATH=''${HELMR_WORKER_FIRECRACKER_JAILER_PATH:-$(command -v jailer)}
        export HELMR_WORKER_FIRECRACKER_JAILER_UID=''${HELMR_WORKER_FIRECRACKER_JAILER_UID:-$(id -u)}
        export HELMR_WORKER_FIRECRACKER_JAILER_GID=''${HELMR_WORKER_FIRECRACKER_JAILER_GID:-$(id -g)}
        export HELMR_WORKER_FIRECRACKER_CGROUP_VERSION=''${HELMR_WORKER_FIRECRACKER_CGROUP_VERSION:-2}
        export HELMR_WORKER_BUILDKIT_ADDR=''${HELMR_WORKER_BUILDKIT_ADDR:-unix:///run/helmr/buildkit/buildkitd.sock}
        export HELMR_VM_E2E=''${HELMR_VM_E2E:-1}
        export XDG_DATA_HOME=''${XDG_DATA_HOME:-$PWD/.helmr-smoke/data}
        export XDG_RUNTIME_DIR=''${XDG_RUNTIME_DIR:-$PWD/.helmr-smoke/runtime}
        mkdir -p "$XDG_DATA_HOME" "$XDG_RUNTIME_DIR"

        ./scripts/doctor.sh linux
        make images
      '';
}
