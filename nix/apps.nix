{
  system,
  nixpkgs,
  nixpkgs-unstable,
  nixpkgs-clickhouse,
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
    ci-fast-policy =
      app "ci-fast-policy" "check workflow and repository security policy" toolsets.ciPolicy
        ''
          actionlint
          scripts/security-checks.sh
        '';
    ci-fast-go = app "ci-fast-go" "compile commands and run Go unit tests" toolsets.ciGoConsole ''
      export HELMR_SKIP_POSTGRES_TESTS=1
      bun install --frozen-lockfile --ignore-scripts
      make console-build
      go build -tags embed_console ./cmd/...
      go test -tags embed_console ./...
      bash tests/build/go-test-selection.test.sh
    '';
    ci-fast-typescript =
      app "ci-fast-typescript" "check TypeScript types and unit tests" toolsets.ciTypescript
        ''
          ${pkgs.lib.optionalString pkgs.stdenv.isLinux ''
            export LD_LIBRARY_PATH=${pkgs.lib.makeLibraryPath [ pkgs.stdenv.cc.cc.lib ]}
          ''}
          bun install --frozen-lockfile --ignore-scripts
          bun run typecheck
          bun run test:ts
        '';

    ci-policy =
      app "ci-policy" "run repository policy and release script checks for CI" toolsets.ciPolicy
        ''
          bun install --frozen-lockfile --ignore-scripts
          bun audit
          actionlint
          scripts/security-checks.sh
          bash -n dev/local/start.sh
          bash tests/build/buildkit-steps.test.sh
          bash tests/build/ci-workflow.test.sh
          bash tests/release/install.test.sh
          bash tests/release/manifest.test.sh
          bash tests/release/artifact-contracts.test.sh
          bash tests/release/workflow.test.sh
          bash tests/release/manifest-verify.test.sh
          bash tests/release/worker-ami-cleanup.test.sh
          bash tests/release/worker-image-identity.test.sh
          bash tests/release/aws-bootstrap-helmr-secrets.test.sh
          bash tests/release/aws-release-artifacts.test.sh
          bash tests/release/platform-release-materialize.test.sh
          bash tests/release/publish-materialized-platform-release.test.sh
          bash tests/build/runtime-naming-contract.test.sh
          bash tests/release/worker-host-bundle.test.sh
          bash tests/release/worker-runtime-bundle.test.sh
          bash tests/release/linux-worker-host-bundle-materialize.test.sh
          bash tests/build/netboot-inputs.test.sh
          bash tests/build/boot-artifacts-make.test.sh
          bash tests/build/guest-init-cgroup.test.sh
          bash tests/build/guest-init-network.test.sh
        '';
    ci-generated =
      app "ci-generated" "check generated artifacts and formatting for CI" toolsets.ciGenerated
        ''
          bun install --frozen-lockfile --ignore-scripts
          scripts/build-compiler-entry.sh --check
          scripts/build-hostconfig-entry.sh --check
          scripts/build-runtime-entry.sh --check
          make generate
          make fmt
          make console-build
          git diff --exit-code
        '';
    ci-typescript =
      app "ci-typescript" "run TypeScript type checks and tests for CI" toolsets.ciTypescript
        ''
          ${pkgs.lib.optionalString pkgs.stdenv.isLinux ''
            export LD_LIBRARY_PATH=${pkgs.lib.makeLibraryPath [ pkgs.stdenv.cc.cc.lib ]}
          ''}
          bun install --frozen-lockfile --ignore-scripts
          scripts/build-npm-packages.sh
          sdk_packages=$(mktemp -d)
          trap 'rm -rf "$sdk_packages"' EXIT
          scripts/pack-npm-packages.sh "$sdk_packages"
          scripts/check-e2e.sh --skip-sdk-build
          (cd examples/issue-fixer && bun install --frozen-lockfile --ignore-scripts && bun run typecheck && bun test tests)
          scripts/check-packed-sdk-consumer.sh --sdk-packages "$sdk_packages"
          scripts/build-compiler-entry.sh
          scripts/build-hostconfig-entry.sh
          node tests/build/check-fixture-analysis.mjs --sdk-packages "$sdk_packages"
          bun run typecheck
          bun run test:ts:prepared
          bun run build:web
        '';
    ci-go-lint =
      app "ci-go-lint" "run Go lint checks with embedded console assets for CI" toolsets.ciGoLint
        ''
          bun install --frozen-lockfile --ignore-scripts
          make lint
        '';
    ci-go-build =
      app "ci-go-build" "build Go commands with embedded console assets for CI" toolsets.ciGoConsole
        ''
          bun install --frozen-lockfile --ignore-scripts
          make build
        '';
    ci-go-race =
      app "ci-go-race" "run Go race tests with embedded console assets for CI" toolsets.ciGoConsole
        ''
          export HELMR_SKIP_POSTGRES_TESTS=1
          bun install --frozen-lockfile --ignore-scripts
          make test-race
        '';
    ci-linux-compile =
      app "ci-linux-compile" "cross-compile Linux Go test binaries for CI" toolsets.ciGo
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
          bash scripts/test-go-selection.sh '^TestPackagedFirecrackerProbeOutputIsAccepted$' ./internal/firecracker
        '';
    ci-linux-lint =
      app "ci-linux-lint" "run Linux-targeted Go static analysis for CI"
        (toolsets.ciGoConsole ++ [ helmrPackages.staticcheck ])
        ''
          bun install --frozen-lockfile --ignore-scripts
          make console-build
          CGO_ENABLED=0 GOOS=linux GOARCH=amd64 staticcheck -tags embed_console ./...
        '';
    ci-infra-test =
      app "ci-infra-test" "run AWS module tests with pinned OpenTofu" toolsets.infraTest
        ''
          for module in release-storage release-publisher controlplane network release-artifacts worker worker-image; do
            (
              cd "infra/aws/modules/$module"
              if [ "$module" = worker-image ]; then
                bash tests/prepare-root.test.sh
              fi
              tofu init -backend=false -input=false
              tofu fmt -check -recursive
              tofu test
            )
          done
          root_plans="$(mktemp -d)"
          trap 'rm -rf "$root_plans"' EXIT
          for stack in quickstart stacks/release-build stacks/worker-image standard; do
            (
              cd "infra/aws/$stack"
              tofu init -backend=false -input=false
              tofu fmt -check -recursive
              case "$stack" in
                quickstart|standard|stacks/worker-image)
                  if ! tofu test -json -verbose >"$root_plans/$(basename "$stack").jsonl"; then
                    cat "$root_plans/$(basename "$stack").jsonl"
                    exit 1
                  fi
                  ;;
                *) tofu test ;;
              esac
            )
          done
          python3 infra/aws/tests/check_root_composition.py "$root_plans/quickstart.jsonl" "$root_plans/standard.jsonl" "$root_plans/worker-image.jsonl"
        '';
    ci-clickhouse =
      let
        pkgsClickHouse = import nixpkgs-clickhouse { inherit system; };
      in
      app "ci-clickhouse" "run real ClickHouse projection and access tests"
        (toolsets.ciGo ++ [ pkgsClickHouse.clickhouse ])
        ''
          unset HELMR_TEST_CLICKHOUSE_URL HELMR_TEST_CLICKHOUSE_IDLE_ONLY HELMR_TEST_CLICKHOUSE_BATCH_KIND
          export HELMR_TEST_CLICKHOUSE_BOOTSTRAP=1
          exec go test -race -count=1 ./internal/clickhouse/... ./internal/telemetry
        '';
    ci-postgres = app "ci-postgres" "run Postgres-backed CI tests" toolsets.ciPostgres ''
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
    ${ciApps.ci-clickhouse.program}
  '';
  ci-bundle-builder =
    app "ci-bundle-builder" "run the canonical bundle builder end-to-end tests" toolsets.ciBundleBuilder
      ''
        exec bash ./tests/build/bundle-builder.test.sh
      '';
  ci-version-cohort =
    app "ci-version-cohort" "verify cohort stamps and Worker artifact identity guidance"
      [
        pkgs.bash
        pkgs.coreutils
        pkgs.git
        helmrPackages.goPackage
        pkgs.nix
        pkgs.stdenv.cc
      ]
      ''
        exec bash ./tests/release/version-cohort.test.sh "$@"
      '';
  dev =
    let
      pkgsClickHouse = import nixpkgs-clickhouse { inherit system; };
    in
    app "dev" "run the local Helmr control plane and console dashboard"
      (toolsets.appRuntime ++ [ pkgsClickHouse.clickhouse ])
      ''
        exec ./dev/local/start.sh "$@"
      '';
  measure-dispatch = app "measure-dispatch" "measure PostgreSQL dispatch discovery" toolsets.base ''
    exec ./scripts/measure-dispatch.sh "$@"
  '';
  ci-boot-artifacts =
    app "ci-boot-artifacts" "build and stage guest boot artifacts for CI"
      [
        helmrPackages.apko
        pkgs.bash
        pkgs.coreutils
        pkgs.curl
        pkgs.docker_29
        pkgs.findutils
        pkgs.gawk
        pkgs.git
        pkgs.gnugrep
        pkgs.gnumake
        pkgs.gnutar
        helmrPackages.goPackage
        pkgs.jq
        pkgs.ruby
        helmrPackages.squashfsTools
      ]
      ''
        exec ./scripts/ci-boot-artifacts.sh "$@"
      '';
  ci-boot-artifacts-repro =
    app "ci-boot-artifacts-repro" "prove guest boot artifact reproducibility for CI"
      [
        pkgs.bash
        pkgs.coreutils
        pkgs.curl
        pkgs.diffutils
        pkgs.findutils
        pkgs.gawk
        pkgs.git
        pkgs.gnugrep
        pkgs.gnumake
        pkgs.gnused
        pkgs.gnutar
        helmrPackages.goPackage
        pkgs.jq
        pkgs.nix
      ]
      ''
        bash ./tests/build/netboot-inputs.test.sh
        bash ./tests/build/boot-artifacts-make.test.sh
        bash ./tests/build/guest-init-cgroup.test.sh
        bash ./tests/build/guest-init-network.test.sh
        exec ./tests/build/boot-artifacts-reproducibility.test.sh "$@"
      '';
  doctor = app "doctor" "check Helmr host prerequisites" toolsets.appRuntime ''
    exec ./scripts/doctor.sh "$@"
  '';
}
// pkgs.lib.optionalAttrs (system == "x86_64-linux") {
  ci-browser =
    let
      pkgsClickHouse = import nixpkgs-clickhouse { inherit system; };
      playwrightVersion =
        (builtins.fromJSON (builtins.readFile ../package.json)).devDependencies."@playwright/test";
    in
    assert pkgs.lib.assertMsg (
      playwrightVersion == pkgsUnstable.playwright-driver.version
    ) "@playwright/test must match the pinned nixpkgs playwright-driver";
    app "ci-browser" "run the console browser acceptance test"
      (
        toolsets.ciGoConsole
        ++ [
          pkgs.postgresql_18
          pkgs.redis
          pkgsClickHouse.clickhouse
          pkgs.curl
          pkgsUnstable.playwright-driver.browsers
        ]
      )
      ''
        export PLAYWRIGHT_BROWSERS_PATH=${pkgsUnstable.playwright-driver.browsers}
        export PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true
        bun install --frozen-lockfile --ignore-scripts
        bash dev/local/start.test.sh
        bun run test:browser
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
