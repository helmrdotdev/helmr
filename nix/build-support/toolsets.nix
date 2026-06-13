{
  pkgs,
  pkgsUnstable ? pkgs,
  helmrPackages,
}:

let
  inherit (pkgs) lib stdenv;
  goPackage = if pkgs ? go_1_26 then pkgs.go_1_26 else pkgs.go;

  direnv = pkgs.direnv.overrideAttrs (_: {
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
in
rec {
  repoChecks = [
    pkgs.bash
    pkgs.coreutils
    pkgs.findutils
    pkgs.gawk
    pkgs.gnugrep
    pkgs.gnused
    pkgs.ripgrep
    pkgs.stdenv.cc
    goPackage
    pkgsUnstable.gopls
    pkgs.gotools
    helmrPackages.staticcheck
    helmrPackages.unparam
    helmrPackages.bun
    pkgs.nodejs
    pkgs.buf
    pkgsUnstable.protoc-gen-go
    pkgsUnstable.protoc-gen-es
    pkgsUnstable.sqlc
    pkgs.jq
    pkgs.zstd
    pkgs.protobuf
    pkgs.git
    pkgs.gnumake
    pkgs.curl
    pkgs.actionlint
    pkgs.zizmor
  ];

  base = repoChecks ++ [
    pkgs.postgresql_18
    direnv
    pkgs.nix-direnv
    pkgs.nixfmt
  ];

  image = [
    helmrPackages.apko
    pkgs.cosign
    pkgs.docker
    pkgs.e2fsprogs
    pkgs.squashfsTools
    pkgs.cpio
    pkgs.gzip
    pkgs.ruby
    pkgs.binutils
  ]
  ++ lib.optionals stdenv.isLinux [ pkgs.kmod ];

  smokeLinux = lib.optionals stdenv.isLinux [
    (helmrPackages.firecrackerRuntime or pkgs.firecracker)
    cniPlugins
    pkgs.buildkit
    pkgs.rootlesskit
    pkgs.slirp4netns
    pkgs.fuse-overlayfs
    pkgs.runc
    pkgs.iptables
    pkgs.iproute2
    pkgs.nftables
    pkgs.procps
  ];

  ciChecks =
    repoChecks
    ++ [
      pkgs.gnutar
    ]
    ++ image;

  appRuntime = base ++ image ++ smokeLinux ++ lib.optionals stdenv.isLinux [ pkgs.kmod ];

  infra = base ++ [
    pkgs.opentofu
    pkgs.awscli2
    pkgs.ssm-session-manager-plugin
  ];
}
