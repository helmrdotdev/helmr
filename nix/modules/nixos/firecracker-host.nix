{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.helmr.firecrackerHost;
  firecrackerReleaseVersion = "1.16.1";
  firecrackerPackage =
    assert lib.assertMsg pkgs.stdenv.hostPlatform.isx86_64
      "Helmr Firecracker hosts support only x86_64-linux";
    pkgs.stdenvNoCC.mkDerivation {
      pname = "firecracker-runtime";
      version = firecrackerReleaseVersion;

      src = pkgs.fetchurl {
        url = "https://github.com/firecracker-microvm/firecracker/releases/download/v${firecrackerReleaseVersion}/firecracker-v${firecrackerReleaseVersion}-x86_64.tgz";
        hash = "sha256-OCoCqGnk1tXLFMQFd/lUXoRYAh6osLLT/BDsFNnCQuY=";
      };

      installPhase = ''
        runHook preInstall

        release_dir=.
        install -d "$out/bin" "$out/share/firecracker"
        install -m 0755 "$release_dir/cpu-template-helper-v${firecrackerReleaseVersion}-x86_64" "$out/bin/cpu-template-helper"
        install -m 0755 "$release_dir/firecracker-v${firecrackerReleaseVersion}-x86_64" "$out/bin/firecracker"
        install -m 0755 "$release_dir/jailer-v${firecrackerReleaseVersion}-x86_64" "$out/bin/jailer"
        install -m 0644 "$release_dir/LICENSE" "$release_dir/NOTICE" "$release_dir/THIRD-PARTY" "$out/share/firecracker/"

        runHook postInstall
      '';
    };
  direnvPackage = pkgs.direnv.overrideAttrs (_: {
    doCheck = false;
  });
  substrateGenerator = pkgs.pkgsStatic.e2fsprogs;
in
{
  options.services.helmr.firecrackerHost = {
    enable = lib.mkEnableOption "host prerequisites for Helmr Firecracker smoke tests";

    users = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "alice" ];
      description = "Users that should be allowed to access KVM.";
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
        ++ cfg.extraPackages;

        environment.sessionVariables = {
          CPU_TEMPLATE_HELPER_PATH = "${firecrackerPackage}/bin/cpu-template-helper";
          FIRECRACKER_PATH = "${firecrackerPackage}/bin/firecracker";
          JAILER_PATH = "${firecrackerPackage}/bin/jailer";
          MKFS_EXT4_PATH = "${lib.getBin substrateGenerator}/bin/mkfs.ext4";
          MKE2FS_CONFIG_PATH = toString ../../packages/mke2fs.conf;
          JAILER_UID = toString cfg.jailerUID;
          JAILER_GID = toString cfg.jailerGID;
          JAILER_CGROUP_VERSION = "2";
        };

        boot.kernelModules = [ "kvm" ];
        networking.firewall.checkReversePath = lib.mkDefault false;
        assertions = [
          {
            assertion = pkgs.stdenv.hostPlatform.isx86_64;
            message = "services.helmr.firecrackerHost supports only x86_64-linux.";
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
          extraGroups = [ "kvm" ];
        });

        services.udev.extraRules = ''
          KERNEL=="kvm", GROUP="helmr-vmm", MODE="0660"
        '';
      }

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
