{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.helmr.firecrackerHost;
  firecrackerReleaseVersion = "1.13.2";
  firecrackerRelease =
    {
      x86_64-linux = {
        arch = "x86_64";
        hash = "sha256-pts7RR9QDf2CmJRH/r9Utci7iSnk7nx/hKlpXxMNpUc=";
      };
      aarch64-linux = {
        arch = "aarch64";
        hash = "sha256-pkwLkTspuOpLWZDsuUqSy3y064UAFbaaoWgNJdnLQb8=";
      };
    }
    .${pkgs.stdenv.hostPlatform.system} or null;
  firecrackerPackage =
    if firecrackerRelease == null then
      pkgs.firecracker
    else
      pkgs.stdenvNoCC.mkDerivation {
        pname = "firecracker-runtime";
        version = firecrackerReleaseVersion;

        src = pkgs.fetchurl {
          url = "https://github.com/firecracker-microvm/firecracker/releases/download/v${firecrackerReleaseVersion}/firecracker-v${firecrackerReleaseVersion}-${firecrackerRelease.arch}.tgz";
          hash = firecrackerRelease.hash;
        };

        installPhase = ''
          runHook preInstall

          release_dir=.
          install -d "$out/bin" "$out/share/firecracker"
          install -m 0755 "$release_dir/firecracker-v${firecrackerReleaseVersion}-${firecrackerRelease.arch}" "$out/bin/firecracker"
          install -m 0755 "$release_dir/jailer-v${firecrackerReleaseVersion}-${firecrackerRelease.arch}" "$out/bin/jailer"
          install -m 0644 "$release_dir/LICENSE" "$release_dir/NOTICE" "$release_dir/THIRD-PARTY" "$out/share/firecracker/"

          runHook postInstall
        '';
      };
  direnvPackage = pkgs.direnv.overrideAttrs (_: {
    doCheck = false;
  });
  tcRedirectTap = pkgs.buildGoModule rec {
    pname = "tc-redirect-tap";
    version = "34bf829";

    src = pkgs.fetchFromGitHub {
      owner = "awslabs";
      repo = "tc-redirect-tap";
      rev = "34bf829e9a5c99df47318c7feeb637576df239fc";
      hash = "sha256-yeokm0aTwlMXmnMcNVRER9cZVuuNqk/RW0HY9vjiPPA=";
    };

    vendorHash = "sha256-gKkWzy+PVlLSOSljFG/T5RmROmfaK/nfXDId4kTeZKM=";
    subPackages = [ "cmd/tc-redirect-tap" ];
  };
  cniPlugins = pkgs.symlinkJoin {
    name = "helmr-cni-plugins";
    paths = [
      pkgs.cni-plugins
      tcRedirectTap
    ];
  };
  pow2 = n: if n == 0 then 1 else 2 * pow2 (n - 1);
  parseIPv4CIDR =
    cidr:
    let
      octetPattern = "(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])";
      prefixPattern = "(3[0-2]|[12]?[0-9])";
      match = builtins.match "${octetPattern}\\.${octetPattern}\\.${octetPattern}\\.${octetPattern}/${prefixPattern}" cidr;
      octets = lib.optionals (match != null) (
        map (index: lib.toInt (builtins.elemAt match index)) [
          0
          1
          2
          3
        ]
      );
      prefix = if match == null then null else lib.toInt (builtins.elemAt match 4);
      valid =
        match != null && lib.all (octet: octet >= 0 && octet <= 255) octets && prefix >= 0 && prefix <= 32;
    in
    if !valid then
      null
    else
      let
        octet = index: builtins.elemAt octets index;
        address = (octet 0) * 16777216 + (octet 1) * 65536 + (octet 2) * 256 + (octet 3);
        size = pow2 (32 - prefix);
        start = address - (lib.mod address size);
      in
      {
        inherit start;
        end = start + size - 1;
      };
  validIPv4CIDR = cidr: parseIPv4CIDR cidr != null;
  ipv4CIDROverlaps =
    left: right:
    let
      a = parseIPv4CIDR left;
      b = parseIPv4CIDR right;
    in
    a != null && b != null && a.start <= b.end && b.start <= a.end;
in
{
  options.services.helmr.firecrackerHost = {
    enable = lib.mkEnableOption "host prerequisites for Helmr Firecracker smoke tests";

    users = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "alice" ];
      description = "Users that should be allowed to access KVM and the Helmr BuildKit socket.";
    };

    cniNetworkName = lib.mkOption {
      type = lib.types.str;
      default = "helmr";
      description = "CNI network name used by Helmr Firecracker workers.";
    };

    guestSubnet = lib.mkOption {
      type = lib.types.str;
      default = "192.168.127.0/24";
      description = "IPv4 subnet allocated to Helmr Firecracker guests.";
    };

    guestNameservers = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "1.1.1.1"
        "8.8.8.8"
      ];
      description = "DNS resolver addresses advertised to Helmr Firecracker guests.";
    };

    networkBlockedIPv4CIDRs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "0.0.0.0/8"
        "10.0.0.0/8"
        "100.64.0.0/10"
        "127.0.0.0/8"
        "169.254.0.0/16"
        "172.16.0.0/12"
        "192.168.0.0/16"
        "224.0.0.0/4"
        "240.0.0.0/4"
      ];
      description = "IPv4 CIDRs blocked from Helmr Firecracker task egress.";
    };

    networkBlockedIPv6CIDRs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "::/128"
        "::1/128"
        "fc00::/7"
        "fe80::/10"
        "ff00::/8"
      ];
      description = "IPv6 CIDRs blocked from Helmr Firecracker task egress.";
    };

    jailerUID = lib.mkOption {
      type = lib.types.int;
      default = 977;
      description = "UID used by the Firecracker jailer when it drops privileges.";
    };

    jailerGID = lib.mkOption {
      type = lib.types.int;
      default = 977;
      description = "GID used by the Firecracker jailer when it drops privileges.";
    };

    enableBuildKit = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Enable a dedicated rootless BuildKit daemon for Helmr workers.";
    };

    buildKitSocket = lib.mkOption {
      type = lib.types.str;
      default = "/run/helmr/buildkit/buildkitd.sock";
      description = "Unix socket path where the Helmr worker reaches buildkitd.";
    };

    buildKitStateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/helmr/buildkit";
      description = "Persistent state directory for the Helmr BuildKit daemon.";
    };

    buildKitCacheNamespace = lib.mkOption {
      type = lib.types.str;
      default = "helmr";
      description = "Default BuildKit persistent cache namespace for Helmr workers.";
    };

    buildKitSlirpCIDR = lib.mkOption {
      type = lib.types.str;
      default = "198.18.0.0/24";
      description = "IPv4 CIDR used by rootlesskit/slirp4netns inside the Helmr BuildKit service namespace.";
    };

    buildKitSubuidStart = lib.mkOption {
      type = lib.types.int;
      default = 231072;
      description = "First subordinate UID reserved for the rootless BuildKit service user.";
    };

    buildKitSubgidStart = lib.mkOption {
      type = lib.types.int;
      default = 231072;
      description = "First subordinate GID reserved for the rootless BuildKit service user.";
    };

    enableIpv4Forwarding = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Enable net.ipv4.ip_forward for Firecracker guest networking.";
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = "Additional packages to install on Helmr Firecracker hosts.";
    };

    enableDirenv = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Enable direnv and nix-direnv for automatic Helmr dev shell activation.";
    };
  };

  config = lib.mkIf cfg.enable (
    lib.mkMerge [
      {
        environment.systemPackages = [
          firecrackerPackage
          cniPlugins
          pkgs.iproute2
          pkgs.iptables
          pkgs.jq
          pkgs.zstd
        ]
        ++ lib.optionals cfg.enableBuildKit [
          pkgs.buildkit
          pkgs.rootlesskit
          pkgs.slirp4netns
          pkgs.fuse-overlayfs
          pkgs.runc
        ]
        ++ cfg.extraPackages;

        environment.sessionVariables = {
          HELMR_WORKER_FIRECRACKER_PATH = "${firecrackerPackage}/bin/firecracker";
          HELMR_WORKER_FIRECRACKER_JAILER_PATH = "${firecrackerPackage}/bin/jailer";
          HELMR_WORKER_FIRECRACKER_JAILER_UID = toString cfg.jailerUID;
          HELMR_WORKER_FIRECRACKER_JAILER_GID = toString cfg.jailerGID;
          HELMR_WORKER_FIRECRACKER_CGROUP_VERSION = "2";
          HELMR_WORKER_CNI_NETWORK = cfg.cniNetworkName;
          HELMR_WORKER_CNI_PROFILE = "${cfg.cniNetworkName}/v1";
          HELMR_WORKER_CNI_CONF_DIR = "/etc/cni/conf.d";
          HELMR_WORKER_CNI_BIN_DIR = "${cniPlugins}/bin";
          HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS =
            if cfg.networkBlockedIPv4CIDRs == [ ] then
              "none"
            else
              lib.concatStringsSep "," cfg.networkBlockedIPv4CIDRs;
          HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS =
            if cfg.networkBlockedIPv6CIDRs == [ ] then
              "none"
            else
              lib.concatStringsSep "," cfg.networkBlockedIPv6CIDRs;
          HELMR_VM_E2E = "1";
        }
        // lib.optionalAttrs cfg.enableBuildKit {
          HELMR_WORKER_BUILDKIT_ADDR = "unix://${cfg.buildKitSocket}";
          HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE = cfg.buildKitCacheNamespace;
        };

        environment.etc."helmr/guest-resolv.conf".text =
          lib.concatMapStringsSep "\n" (nameserver: "nameserver ${nameserver}") cfg.guestNameservers + "\n";

        environment.etc."cni/conf.d/helmr.conflist".text = builtins.toJSON {
          name = cfg.cniNetworkName;
          cniVersion = "1.0.0";
          plugins = [
            {
              type = "ptp";
              ipMasq = true;
              ipam = {
                type = "host-local";
                subnet = cfg.guestSubnet;
                resolvConf = "/etc/helmr/guest-resolv.conf";
              };
            }
            { type = "firewall"; }
            { type = "tc-redirect-tap"; }
          ];
        };

        boot.kernelModules = [ "kvm" ];
        networking.firewall.checkReversePath = lib.mkDefault false;
        assertions = [
          {
            assertion = lib.all validIPv4CIDR cfg.networkBlockedIPv4CIDRs;
            message = "services.helmr.firecrackerHost.networkBlockedIPv4CIDRs must contain valid IPv4 CIDR prefixes.";
          }
        ]
        ++ lib.optionals cfg.enableBuildKit [
          {
            assertion = validIPv4CIDR cfg.buildKitSlirpCIDR;
            message = "services.helmr.firecrackerHost.buildKitSlirpCIDR must be a valid IPv4 CIDR prefix.";
          }
          {
            assertion = !lib.any (ipv4CIDROverlaps cfg.buildKitSlirpCIDR) cfg.networkBlockedIPv4CIDRs;
            message = "services.helmr.firecrackerHost.buildKitSlirpCIDR must not overlap networkBlockedIPv4CIDRs because rootless BuildKit DNS and NAT must remain reachable inside the service namespace.";
          }
        ];

        users.groups.helmr-vmm.gid = cfg.jailerGID;
        users.users = {
          helmr-vmm = {
            isSystemUser = true;
            group = "helmr-vmm";
            uid = cfg.jailerUID;
            extraGroups = [ "kvm" ];
          };
        }
        // lib.genAttrs cfg.users (_: {
          extraGroups = [ "kvm" ] ++ lib.optionals cfg.enableBuildKit [ "helmr-buildkit" ];
        });

        services.udev.extraRules = ''
          KERNEL=="kvm", GROUP="helmr-vmm", MODE="0660"
        '';
      }

      (lib.mkIf cfg.enableBuildKit {
        users.groups.helmr-buildkit = { };
        users.users.helmr-buildkit = {
          isSystemUser = true;
          group = "helmr-buildkit";
          home = "/var/lib/helmr/buildkit-home";
          createHome = true;
          subUidRanges = [
            {
              startUid = cfg.buildKitSubuidStart;
              count = 65536;
            }
          ];
          subGidRanges = [
            {
              startGid = cfg.buildKitSubgidStart;
              count = 65536;
            }
          ];
        };

        systemd.tmpfiles.rules = [
          "d /run/helmr 0755 root root -"
          "d /run/helmr/buildkit 0770 helmr-buildkit helmr-buildkit -"
          "d /run/helmr/buildkit-runtime 0700 helmr-buildkit helmr-buildkit -"
          "d /var/lib/helmr 0755 root root -"
          "d ${cfg.buildKitStateDir} 0700 helmr-buildkit helmr-buildkit -"
          "d /var/lib/helmr/buildkit-home 0700 helmr-buildkit helmr-buildkit -"
        ];

        boot.kernel.sysctl."user.max_user_namespaces" = lib.mkDefault 16384;

        systemd.services.helmr-buildkit = {
          description = "Helmr BuildKit daemon";
          wantedBy = [ "multi-user.target" ];
          after = [ "network-online.target" ];
          wants = [ "network-online.target" ];
          path = [
            pkgs.buildkit
            pkgs.rootlesskit
            pkgs.slirp4netns
            pkgs.fuse-overlayfs
            pkgs.runc
          ];
          environment = {
            HOME = "/var/lib/helmr/buildkit-home";
            XDG_RUNTIME_DIR = "/run/helmr/buildkit-runtime";
          };
          serviceConfig = {
            User = "helmr-buildkit";
            Group = "helmr-buildkit";
            UMask = "0007";
            ExecStart = lib.concatStringsSep " " [
              "${pkgs.rootlesskit}/bin/rootlesskit"
              "--net=slirp4netns"
              "--cidr=${cfg.buildKitSlirpCIDR}"
              "--copy-up=/etc"
              "--disable-host-loopback"
              "${pkgs.buildkit}/bin/buildkitd"
              "--addr"
              "unix://${cfg.buildKitSocket}"
              "--root"
              cfg.buildKitStateDir
              "--oci-worker=true"
              "--oci-worker-snapshotter=fuse-overlayfs"
            ];
            Restart = "on-failure";
            RestartSec = "3s";
            Delegate = true;
            KillMode = "mixed";
            BindReadOnlyPaths = [ "/etc/helmr/guest-resolv.conf:/etc/resolv.conf" ];
            IPAddressDeny = cfg.networkBlockedIPv4CIDRs ++ cfg.networkBlockedIPv6CIDRs;
          };
        };
      })

      (lib.mkIf cfg.enableIpv4Forwarding {
        boot.kernel.sysctl."net.ipv4.ip_forward" = lib.mkDefault 1;
      })

      (lib.mkIf cfg.enableDirenv {
        programs.direnv.enable = lib.mkDefault true;
        programs.direnv.package = lib.mkDefault direnvPackage;
        programs.direnv.nix-direnv.enable = lib.mkDefault true;
      })
    ]
  );
}
