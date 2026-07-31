{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.helmr.firecrackerHost;
  buildKitServiceBlockedIPv6CIDRs = [
    "::/128"
    "::1/128"
    "fc00::/7"
    "fe80::/10"
    "ff00::/8"
  ];
  firecrackerReleaseVersion = "1.13.2";
  firecrackerPackage =
    assert lib.assertMsg pkgs.stdenv.hostPlatform.isx86_64
      "Helmr Firecracker hosts support only x86_64-linux";
    pkgs.stdenvNoCC.mkDerivation {
      pname = "firecracker-runtime";
      version = firecrackerReleaseVersion;

      src = pkgs.fetchurl {
        url = "https://github.com/firecracker-microvm/firecracker/releases/download/v${firecrackerReleaseVersion}/firecracker-v${firecrackerReleaseVersion}-x86_64.tgz";
        hash = "sha256-pts7RR9QDf2CmJRH/r9Utci7iSnk7nx/hKlpXxMNpUc=";
      };

      installPhase = ''
        runHook preInstall

        release_dir=.
        install -d "$out/bin" "$out/share/firecracker"
        install -m 0755 "$release_dir/firecracker-v${firecrackerReleaseVersion}-x86_64" "$out/bin/firecracker"
        install -m 0755 "$release_dir/jailer-v${firecrackerReleaseVersion}-x86_64" "$out/bin/jailer"
        install -m 0644 "$release_dir/LICENSE" "$release_dir/NOTICE" "$release_dir/THIRD-PARTY" "$out/share/firecracker/"

        runHook postInstall
      '';
    };
  direnvPackage = pkgs.direnv.overrideAttrs (_: {
    doCheck = false;
  });
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
        inherit address prefix;
        inherit start;
        end = start + size - 1;
      };
  validIPv4CIDR = cidr: parseIPv4CIDR cidr != null;
  canonicalIPv4CIDR =
    cidr:
    let
      parsed = parseIPv4CIDR cidr;
    in
    parsed != null && parsed.address == parsed.start;
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

    guestNameservers = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "1.1.1.1"
        "8.8.8.8"
      ];
      description = "DNS resolver addresses advertised to Helmr Firecracker guests.";
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

    buildKitSlirpCIDR = lib.mkOption {
      type = lib.types.str;
      default = "198.18.0.0/24";
      description = "IPv4 CIDR used by rootlesskit/slirp4netns inside the Helmr BuildKit service namespace.";
    };

    buildKitBlockedIPv4CIDRs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      description = "Deployment-supplied canonical IPv4 CIDRs denied from the Helmr BuildKit service. Set an explicit empty list to disable additional IPv4 destination denies.";
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
          HELMR_VM_E2E = "1";
        }
        // lib.optionalAttrs cfg.enableBuildKit {
          HELMR_WORKER_BUILDKIT_ADDR = "unix://${cfg.buildKitSocket}";
        };

        environment.etc."helmr/guest-resolv.conf".text =
          lib.concatMapStringsSep "\n" (nameserver: "nameserver ${nameserver}") cfg.guestNameservers + "\n";

        boot.kernelModules = [ "kvm" ];
        networking.firewall.checkReversePath = lib.mkDefault false;
        assertions = [
          {
            assertion = pkgs.stdenv.hostPlatform.isx86_64;
            message = "services.helmr.firecrackerHost supports only x86_64-linux.";
          }
        ]
        ++ lib.optionals cfg.enableBuildKit [
          {
            assertion = validIPv4CIDR cfg.buildKitSlirpCIDR;
            message = "services.helmr.firecrackerHost.buildKitSlirpCIDR must be a valid IPv4 CIDR prefix.";
          }
          {
            assertion =
              lib.all canonicalIPv4CIDR cfg.buildKitBlockedIPv4CIDRs
              && lib.length (lib.unique cfg.buildKitBlockedIPv4CIDRs) == lib.length cfg.buildKitBlockedIPv4CIDRs;
            message = "services.helmr.firecrackerHost.buildKitBlockedIPv4CIDRs must contain unique canonical IPv4 CIDRs.";
          }
          {
            assertion = !lib.any (ipv4CIDROverlaps cfg.buildKitSlirpCIDR) cfg.buildKitBlockedIPv4CIDRs;
            message = "services.helmr.firecrackerHost.buildKitSlirpCIDR must not overlap the deployment-supplied BuildKit service deny set because rootless BuildKit DNS and NAT must remain reachable inside the service namespace.";
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
            CPUQuota = "100%";
            MemoryMax = "2G";
            MemorySwapMax = 0;
            TasksMax = 1024;
            MemoryOOMGroup = true;
            KillMode = "mixed";
            BindReadOnlyPaths = [ "/etc/helmr/guest-resolv.conf:/etc/resolv.conf" ];
            IPAddressDeny = cfg.buildKitBlockedIPv4CIDRs ++ buildKitServiceBlockedIPv6CIDRs;
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
