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
      '';
  ci-generated =
    app "ci-generated" "check generated artifacts and formatting for CI" toolsets.ciChecks
      ''
        bun install --frozen-lockfile --ignore-scripts
        scripts/build-embedded-adapter.sh
        git diff --exit-code -- internal/adapter/js
        test -z "$(git status --porcelain -- internal/adapter/js)"
        make generate
        make fmt
        make console-build
        git diff --exit-code
      '';
  ci-typescript =
    app "ci-typescript" "run TypeScript type checks and tests for CI" toolsets.ciChecks
      ''
        bun install --frozen-lockfile --ignore-scripts
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
        CGO_ENABLED=0 GOOS=linux GOARCH=amd64 staticcheck -tags embed_console ./...
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

        export ARCH=''${ARCH:-x86_64}
        export HELMR_WORKER_IMAGES_DIR=''${HELMR_WORKER_IMAGES_DIR:-$PWD/images}
        export HELMR_WORKER_FIRECRACKER_PATH=''${HELMR_WORKER_FIRECRACKER_PATH:-$(command -v firecracker)}
        export HELMR_WORKER_FIRECRACKER_JAILER_PATH=''${HELMR_WORKER_FIRECRACKER_JAILER_PATH:-$(command -v jailer)}
        export HELMR_WORKER_FIRECRACKER_JAILER_UID=''${HELMR_WORKER_FIRECRACKER_JAILER_UID:-$(id -u)}
        export HELMR_WORKER_FIRECRACKER_JAILER_GID=''${HELMR_WORKER_FIRECRACKER_JAILER_GID:-$(id -g)}
        export HELMR_WORKER_FIRECRACKER_CGROUP_VERSION=''${HELMR_WORKER_FIRECRACKER_CGROUP_VERSION:-2}
        export HELMR_WORKER_CNI_NETWORK=''${HELMR_WORKER_CNI_NETWORK:-helmr}
        export HELMR_WORKER_CNI_PROFILE=''${HELMR_WORKER_CNI_PROFILE:-$HELMR_WORKER_CNI_NETWORK/v1}
        export HELMR_WORKER_CNI_CONF_DIR=''${HELMR_WORKER_CNI_CONF_DIR:-$PWD/.helmr-smoke/cni/conf.d}
        export HELMR_WORKER_CNI_BIN_DIR=''${HELMR_WORKER_CNI_BIN_DIR:-$PWD/.helmr-smoke/cni/bin}
        export HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS=''${HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS:-0.0.0.0/8,10.0.0.0/8,100.64.0.0/10,127.0.0.0/8,169.254.0.0/16,172.16.0.0/12,192.168.0.0/16,224.0.0.0/4,240.0.0.0/4}
        export HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS=''${HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS:-::/128,::1/128,fc00::/7,fe80::/10,ff00::/8}
        export HELMR_WORKER_BUILDKIT_ADDR=''${HELMR_WORKER_BUILDKIT_ADDR:-unix:///run/helmr/buildkit/buildkitd.sock}
        export HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE=''${HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE:-helmr-smoke}
        export HELMR_VM_E2E=''${HELMR_VM_E2E:-1}
        export XDG_DATA_HOME=''${XDG_DATA_HOME:-$PWD/.helmr-smoke/data}
        export XDG_RUNTIME_DIR=''${XDG_RUNTIME_DIR:-$PWD/.helmr-smoke/runtime}
        mkdir -p "$XDG_DATA_HOME" "$XDG_RUNTIME_DIR" "$HELMR_WORKER_CNI_CONF_DIR" "$HELMR_WORKER_CNI_BIN_DIR"
        guest_resolv_conf="$HELMR_WORKER_CNI_CONF_DIR/guest-resolv.conf"
        printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > "$guest_resolv_conf"
        for plugin in ptp host-local firewall tc-redirect-tap; do
          ln -sf "$(command -v "$plugin")" "$HELMR_WORKER_CNI_BIN_DIR/$plugin"
        done
        printf '%s\n' \
          '{' \
          '  "name": "'"$HELMR_WORKER_CNI_NETWORK"'",' \
          '  "cniVersion": "1.0.0",' \
          '  "plugins": [' \
          '    {' \
          '      "type": "ptp",' \
          '      "ipMasq": true,' \
          '      "ipam": {' \
          '        "type": "host-local",' \
          '        "subnet": "192.168.127.0/24",' \
          '        "resolvConf": "'"$guest_resolv_conf"'"' \
          '      }' \
          '    },' \
          '    { "type": "firewall" },' \
          '    { "type": "tc-redirect-tap" }' \
          '  ]' \
          '}' > "$HELMR_WORKER_CNI_CONF_DIR/helmr.conflist"

        ./scripts/doctor.sh linux
        make images
      '';
}
