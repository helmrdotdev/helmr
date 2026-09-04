{
  system,
  nixpkgs,
  nixpkgs-unstable,
  nixpkgs-clickhouse,
  helmrPackages,
}:

let
  pkgs = import nixpkgs { inherit system; };
  pkgsClickHouse = import nixpkgs-clickhouse { inherit system; };
  pkgsUnstable = import nixpkgs-unstable {
    inherit system;
    config.allowUnfreePredicate =
      pkg:
      builtins.elem (nixpkgs.lib.getName pkg) [
        "1password-cli"
      ];
  };
  toolsets = import ./build-support/toolsets.nix {
    inherit
      pkgs
      pkgsUnstable
      helmrPackages
      ;
  };

  shellHook = ''
    go_version="$(go version | awk '{print $3}' | sed 's/^go//')"
    if [ "$go_version" != "1.27.0" ]; then
      echo "warning: expected go 1.27.0 from go.mod, got $go_version" >&2
    fi
    bun_version="$(bun --version)"
    if [ "$bun_version" != "1.3.10" ]; then
      echo "warning: expected bun 1.3.10 from package.json, got $bun_version" >&2
    fi
    postgres_version="$(postgres --version | awk '{print $3}')"
    case "$postgres_version" in
      18.*) ;;
      *) echo "warning: expected PostgreSQL 18.x for local dev, got $postgres_version" >&2 ;;
    esac
  '';
in
{
  default = pkgs.mkShell {
    packages =
      toolsets.base
      ++ [ pkgs.redis ]
      ++ pkgs.lib.optionals (system != "x86_64-darwin") [ pkgsClickHouse.clickhouse ];
    inherit shellHook;
  };

  images = pkgs.mkShell {
    packages = toolsets.base ++ toolsets.image;
    inherit shellHook;
  };

  infra = pkgs.mkShell {
    packages = toolsets.infra;
    inherit shellHook;
  };
}
// pkgs.lib.optionalAttrs (system == "x86_64-linux") {
  smoke-linux = pkgs.mkShell {
    packages = toolsets.base ++ toolsets.image ++ toolsets.smokeLinux;
    shellHook = shellHook + ''
      export ARCH=''${ARCH:-x86_64}
      export WORKER_IMAGES_DIR=''${WORKER_IMAGES_DIR:-$PWD/images}
      export FIRECRACKER_PATH=''${FIRECRACKER_PATH:-$(command -v firecracker || true)}
      export JAILER_PATH=''${JAILER_PATH:-$(command -v jailer || true)}
      export MKFS_EXT4_PATH=''${MKFS_EXT4_PATH:-${helmrPackages.workerHost}/bin/mkfs.ext4}
      export MKE2FS_CONFIG_PATH=''${MKE2FS_CONFIG_PATH:-${helmrPackages.workerHost}/share/helmr/mke2fs.conf}
      export JAILER_UID=''${JAILER_UID:-$(id -u)}
      export JAILER_GID=''${JAILER_GID:-$(id -g)}
      export JAILER_CGROUP_VERSION=''${JAILER_CGROUP_VERSION:-2}
      export XDG_DATA_HOME=''${XDG_DATA_HOME:-$PWD/.helmr-smoke/data}
      export XDG_RUNTIME_DIR=''${XDG_RUNTIME_DIR:-$PWD/.helmr-smoke/runtime}
      mkdir -p "$XDG_DATA_HOME" "$XDG_RUNTIME_DIR"
    '';
  };
}
