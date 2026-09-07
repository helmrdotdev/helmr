{
  pkgs,
  pkgsUnstable ? pkgs,
  helmrPackages,
}:

let
  inherit (pkgs) lib stdenv;

  direnv = pkgs.direnv.overrideAttrs (_: {
    doCheck = false;
  });

  bpfClang = pkgs.writeShellScriptBin "bpf-clang" ''
    exec ${pkgs.llvmPackages.clang-unwrapped}/bin/clang "$@"
  '';

in
rec {
  repoChecks = [
    pkgs.bash
    pkgs.coreutils
    pkgs.diffutils
    pkgs.findutils
    pkgs.file
    pkgs.gawk
    pkgs.gnugrep
    pkgs.gnused
    pkgs.ripgrep
    pkgs.rsync
    pkgs.stdenv.cc
    bpfClang
    helmrPackages.goPackage
    pkgsUnstable.gopls
    pkgs.gotools
    helmrPackages.staticcheck
    helmrPackages.unparam
    helmrPackages.bun
    helmrPackages.nodejs
    pkgs.python3
    pkgs.buf
    pkgsUnstable.protoc-gen-go
    helmrPackages.protocGenEs
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
    helmrPackages.squashfsTools
    pkgs.cpio
    pkgs.gzip
    pkgs.ruby
    pkgs.skopeo
    pkgs.binutils
  ]
  ++ lib.optionals stdenv.isLinux [ pkgs.kmod ];

  smokeLinux = lib.optionals (stdenv.isLinux && stdenv.isx86_64) [
    helmrPackages.firecrackerRuntime
    pkgs.iptables
    pkgs.iproute2
    pkgs.nftables
    pkgs.procps
    pkgs.gnupg
    pkgs.patchelf
    pkgs.xz
  ];

  # CI apps declare their own execution tools instead of pulling the developer
  # environment (including image tools, language servers and generators).
  ciShell = [
    pkgs.bash
    pkgs.coreutils
    pkgs.diffutils
    pkgs.findutils
    pkgs.gawk
    pkgs.gnugrep
    pkgs.gnused
    pkgs.git
  ];

  ciPolicy = ciShell ++ [
    helmrPackages.bun
    pkgs.actionlint
    pkgs.zizmor
    pkgs.ripgrep
    pkgs.file
    pkgs.curl
    pkgs.gnumake
    pkgs.gnutar
    pkgs.gzip
    pkgs.jq
  ];

  ciGo = ciShell ++ [
    helmrPackages.goPackage
    pkgs.stdenv.cc
    pkgs.gnumake
  ];

  ciGoConsole = ciGo ++ [
    helmrPackages.bun
    helmrPackages.nodejs
  ];

  ciGoLint = ciGoConsole ++ [
    helmrPackages.staticcheck
    helmrPackages.unparam
  ];

  ciGenerated = ciGoConsole ++ [
    bpfClang
    pkgs.buf
    pkgsUnstable.protoc-gen-go
    helmrPackages.protocGenEs
    pkgsUnstable.sqlc
    pkgs.protobuf
  ];

  ciTypescript = ciShell ++ [
    helmrPackages.bun
    helmrPackages.nodejs
    pkgs.rsync
    pkgs.gnutar
    pkgs.gzip
  ];

  ciPostgres = ciGo ++ [
    pkgs.postgresql_18
    pkgs.redis
  ];

  ciBundleBuilder = ciGo ++ [
    pkgs.nix
    pkgs.curl
    pkgs.docker
    pkgs.skopeo
    pkgs.jq
    pkgs.ripgrep
    helmrPackages.squashfsTools
  ];

  runtimeProbe =
    ciGo
    ++ lib.optionals (stdenv.isLinux && stdenv.isx86_64) [
      helmrPackages.firecrackerRuntime
    ];

  appRuntime = base ++ image ++ smokeLinux;

  infraTest = [
    pkgs.bash
    pkgs.coreutils
    pkgs.gnugrep
    pkgs.opentofu
  ];

  infra = base ++ [
    pkgs.opentofu
    pkgs.awscli2
    pkgs.clickhouse
    pkgsUnstable._1password-cli
    pkgs.ssm-session-manager-plugin
  ];
}
