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
    pkgsUnstable.buf
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
    pkgs.awscli2
    pkgs.cosign
    pkgs.docker_29
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

  # Shared release transport and verification tools; no source build toolchain.
  releaseCommon = [
    pkgs.bash
    pkgs.coreutils
    pkgs.python3
    pkgs.curl
    pkgs.cosign
    pkgs.skopeo
    helmrPackages.nodejs # Includes the pinned npm used for trusted publishing.
  ];

  # Admission/completion use cosign too: already-complete cohorts are verified.
  release = releaseCommon ++ [
    pkgs.gitMinimal # Exact source admission and discovery ancestry.
    pkgs.awscli2 # Preview OIDC role assumption and conditional object writes.
  ];

  # Candidate execution belongs only in the fresh, unprivileged verifier.
  # The installer uses tar/gzip/awk/sed; the downloaded CLI uses Node and Buildx.
  # Compilation and SquashFS encoding run inside the canonical builder image.
  releaseConsumer = releaseCommon ++ [
    (pkgs.docker_29.override { clientOnly = true; })
    pkgs.gnutar
    pkgs.gzip
    pkgs.gawk
    pkgs.gnused
  ];

  ciPolicy = ciShell ++ [
    pkgs.python3
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
    helmrPackages.nodejs
  ];

  ciGo = ciShell ++ [
    helmrPackages.goPackage
    helmrPackages.nodejs # Host config evaluation in internal/hostconfig and cmd/helmr tests.
    pkgs.stdenv.cc
    pkgs.gnumake
  ];

  ciGoConsole = ciGo ++ [
    pkgs.python3 # Command-boundary Docker fixture in cmd/helmr tests.
    helmrPackages.bun
    helmrPackages.nodejs
  ];

  ciGoLint = ciGoConsole ++ [
    helmrPackages.staticcheck
    helmrPackages.unparam
  ];

  ciGenerated = ciGoConsole ++ [
    bpfClang
    pkgsUnstable.buf
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
    helmrPackages.bun # Packs the current SDK that fixture configs import on the host.
    pkgs.python3
    pkgs.nix
    pkgs.curl
    pkgs.docker_29
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

  appRuntime =
    base
    ++ image
    ++ smokeLinux
    ++ [
      pkgs.redis
      pkgs.curl
    ];

  infraTest = [
    pkgs.python3
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
