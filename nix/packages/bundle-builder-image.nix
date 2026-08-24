{
  lib,
  runCommand,
  writeShellApplication,
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
  timezoneData,
}:

let
  bunForVersion = writeShellApplication {
    name = "bun-for-version";
    runtimeInputs = [ nodejs_24 ];
    text = ''
      if [ "$#" -lt 2 ]; then
        printf 'usage: bun-for-version VERSION ARGS...\n' >&2
        exit 2
      fi
      version="$1"
      shift
      case "$version" in
        *[!0-9A-Za-z.-]* | "")
          printf 'bun-for-version: invalid version\n' >&2
          exit 2
          ;;
      esac
      root="''${XDG_CACHE_HOME:?}/helmr-bun/$version"
      binary="$root/node_modules/@oven/bun-linux-x64-baseline/bin/bun"
      if [ ! -x "$binary" ]; then
        mkdir -p "$root"
        npm install \
          --prefix "$root" \
          --ignore-scripts \
          --no-audit \
          --no-fund \
          --no-save \
          "@oven/bun-linux-x64-baseline@$version"
      fi
      exec /opt/helmr/runtime/lib/ld-linux-x86-64.so.2 \
        --library-path /opt/helmr/runtime/lib \
        "$binary" "$@"
    '';
  };
  root = runCommand "helmr-bundle-builder-root" { } ''
    mkdir -p \
      "$out/bin" \
      "$out/nix/helmr" \
      "$out/opt/helmr/release" \
      "$out/usr/bin" \
      "$out/usr/local/bin" \
      "$out/usr/share"

    cp -a ${runtimeRelease}/tree "$out/opt/helmr/runtime"
    cp -a ${compiler}/tree/helmr/. "$out/nix/helmr/"
    cp -a ${compiler}/tree/node_modules "$out/nix/node_modules"
    chmod u+w "$out/nix/helmr"
    cp ${compiler}/compiler.descriptor.json "$out/nix/helmr/compiler.descriptor.json"
    chmod u-w "$out/nix/helmr"
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
    ln -s ${bunForVersion}/bin/bun-for-version "$out/usr/local/bin/bun-for-version"
    ln -s ${gitMinimal}/bin/git "$out/usr/local/bin/git"
    ln -s ${timezoneData}/zoneinfo "$out/usr/share/zoneinfo"
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
    bunForVersion
    bundleBuilder
    squashfsTools
    timezoneData
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
