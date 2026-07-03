{
  system,
  nixpkgs,
  nixpkgs-unstable,
  helmrPackages,
}:

let
  pkgs = import nixpkgs { inherit system; };
  pkgsUnstable = import nixpkgs-unstable {
    inherit system;
    config.allowUnfreePredicate = pkg: builtins.elem (nixpkgs.lib.getName pkg) [
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
    if [ "$go_version" != "1.26.3" ]; then
      echo "warning: expected go 1.26.3 from go.mod, got $go_version" >&2
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
    packages = toolsets.base;
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

  smoke-linux = pkgs.mkShell {
    packages = toolsets.base ++ toolsets.image ++ toolsets.smokeLinux;
    shellHook = shellHook + ''
      export ARCH=''${ARCH:-x86_64}
      export HELMR_WORKER_IMAGES_DIR=''${HELMR_WORKER_IMAGES_DIR:-$PWD/images}
      export HELMR_WORKER_FIRECRACKER_PATH=''${HELMR_WORKER_FIRECRACKER_PATH:-$(command -v firecracker || true)}
      export HELMR_WORKER_FIRECRACKER_JAILER_PATH=''${HELMR_WORKER_FIRECRACKER_JAILER_PATH:-$(command -v jailer || true)}
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
        if command -v "$plugin" >/dev/null 2>&1; then
          ln -sf "$(command -v "$plugin")" "$HELMR_WORKER_CNI_BIN_DIR/$plugin"
        fi
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
    '';
  };

  unstable-tools = pkgs.mkShell {
    packages = toolsets.base ++ [
      pkgsUnstable.nix
    ];
    inherit shellHook;
  };
}
