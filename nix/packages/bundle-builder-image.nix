{
  lib,
  runCommand,
  dockerTools,
  bash,
  cacert,
  coreutils,
  findutils,
  gitMinimal,
  nodejs_24,
  pnpm,
  yarn,
  bun,
  bundleBuilder,
  compiler,
  runtimeRelease,
  squashfsTools,
}:

let
  root = runCommand "helmr-bundle-builder-root" { } ''
    mkdir -p \
      "$out/bin" \
      "$out/nix" \
      "$out/opt/helmr/release" \
      "$out/usr/bin" \
      "$out/usr/local/bin"

    cp -a ${runtimeRelease}/tree "$out/opt/helmr/runtime"
    cp -a ${compiler}/tree/helmr "$out/nix/helmr"
    cp -a ${compiler}/tree/node_modules "$out/nix/node_modules"
    cp ${compiler}/compiler.descriptor.json "$out/nix/helmr/compiler.descriptor.json"
    cp ${runtimeRelease}/runtime.descriptor.json "$out/opt/helmr/release/runtime.descriptor.json"

    ln -s ${bash}/bin/bash "$out/bin/bash"
    ln -s bash "$out/bin/sh"
    ln -s ${coreutils}/bin/env "$out/usr/bin/env"
    ln -s ${bundleBuilder}/bin/bundle-builder "$out/usr/local/bin/bundle-builder"
    ln -s ${squashfsTools}/bin/mksquashfs "$out/usr/local/bin/mksquashfs"
    ln -s ${nodejs_24}/bin/npm "$out/usr/local/bin/npm"
    ln -s ${nodejs_24}/bin/npx "$out/usr/local/bin/npx"
    ln -s ${nodejs_24}/bin/corepack "$out/usr/local/bin/corepack"
    ln -s ${pnpm}/bin/pnpm "$out/usr/local/bin/pnpm"
    ln -s ${yarn}/bin/yarn "$out/usr/local/bin/yarn"
    ln -s ${bun}/bin/bun "$out/usr/local/bin/bun"
    ln -s ${gitMinimal}/bin/git "$out/usr/local/bin/git"
  '';
in
dockerTools.buildLayeredImage {
  name = "helmr/bundle-builder";
  tag = "0";
  created = "1970-01-01T00:00:01Z";
  maxLayers = 120;

  contents = [
    root
    bash
    cacert
    coreutils
    findutils
    gitMinimal
    nodejs_24
    pnpm
    yarn
    bun
    bundleBuilder
    squashfsTools
  ];

  config = {
    Cmd = [ "/usr/local/bin/bundle-builder" ];
    Env = [
      "HOME=/workspace/home"
      "LANG=C.UTF-8"
      "LC_ALL=C.UTF-8"
      "PATH=/usr/local/bin:/usr/bin:/bin"
      "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt"
      "TMPDIR=/workspace/tmp"
      "TZ=UTC"
      "XDG_CACHE_HOME=/workspace/home/cache"
    ];
    WorkingDir = "/workspace/project";
  };

  meta = {
    description = "Canonical linux/amd64 Helmr deployment bundle builder";
    license = lib.licenses.asl20;
    platforms = [ "x86_64-linux" ];
  };
}
