{
  lib,
  stdenv,
  stdenvNoCC,
  buildEnv,
  closureInfo,
  fetchurl,
  bash,
  coreutils,
  findutils,
  gcc,
  gnumake,
  jq,
  pkg-config,
  proot,
  python3,
  managedRuntime,
  releaseTool,
  squashfsTools,
  unzip,
}:

let
  target = "x86_64";
  node = managedRuntime.nodejs_24;
  libc = stdenv.cc.libc;
  libcLib = lib.getLib libc;
  bunVersion = "1.3.10";
  bun = {
    asset = "bun-linux-x64-baseline.zip";
    hash = "sha256-QSAajF7nSp3Lsc4loRBPH5KYOLV6hFqnjZg3mwznzeI=";
    root = "bun-linux-x64-baseline";
  };
  bunArchive = fetchurl {
    url = "https://github.com/oven-sh/bun/releases/download/bun-v${bunVersion}/${bun.asset}";
    inherit (bun) hash;
  };
  loader = "ld-linux-x86-64.so.2";
  interpreter = "/lib64/${loader}";
  managerLibraries = [
    loader
    "libc.so.6"
    "libpthread.so.0"
    "libdl.so.2"
    "libm.so.6"
  ];
  roots = [
    bash
    coreutils
    gcc
    gnumake
    node
    pkg-config
    python3
  ];
  environment = buildEnv {
    name = "helmr-standard-toolchain-env";
    paths = roots;
    pathsToLink = [ "/bin" ];
    ignoreCollisions = false;
  };
  closure = closureInfo {
    rootPaths = [
      environment
      libcLib
    ];
  };
in
assert lib.assertMsg stdenv.hostPlatform.isx86_64 "standard toolchain supports only x86_64-linux";
assert lib.assertMsg (
  node.version == "24.16.0"
) "standard toolchain requires Managed Node 24.16.0, got ${node.version}";
stdenvNoCC.mkDerivation {
  pname = "helmr-standard-toolchain-${target}";
  version = "0";
  dontUnpack = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    findutils
    jq
    proot
    releaseTool
    squashfsTools
    unzip
  ];

  buildCommand = ''
    set -euo pipefail

    tree="$TMPDIR/toolchain"
    install -d -m0755 "$tree/store"
    while IFS= read -r source; do
      cp -a "$source" "$tree/store/$(basename "$source")"
    done <${closure}/store-paths
    ln -s "store/$(basename ${environment})/bin" "$tree/bin"
    install -d -m0755 "$tree/helmr/manager/lib"
    for library in ${lib.escapeShellArgs managerLibraries}; do
      test -e "${libcLib}/lib/$library"
      ln -s "${libcLib}/lib/$library" "$tree/helmr/manager/lib/$library"
    done

    image="$TMPDIR/toolchain.squashfs"
    LC_ALL=C TZ=UTC env -u SOURCE_DATE_EPOCH mksquashfs "$tree" "$image" \
      -noappend \
      -all-root \
      -no-xattrs \
      -no-exports \
      -no-fragments \
      -no-tailends \
      -no-duplicates \
      -no-hardlinks \
      -no-progress \
      -exit-on-error \
      -processors 2 \
      -mem 1024M \
      -comp zstd \
      -b 131072 \
      -root-mode 0755 \
      -mkfs-time 0 \
      -all-time 0

    verified="$TMPDIR/verified"
    unsquashfs -no-progress -d "$verified" "$image" >/dev/null
    printf '%s\n' bin helmr store >"$TMPDIR/root.expected"
    find "$verified" -mindepth 1 -maxdepth 1 -printf '%f\n' |
      LC_ALL=C sort >"$TMPDIR/root.actual"
    cmp "$TMPDIR/root.actual" "$TMPDIR/root.expected"
    test -L "$verified/bin"
    test "$(readlink "$verified/bin")" = \
      "store/$(basename ${environment})/bin"
    actual="$TMPDIR/store.actual"
    expected="$TMPDIR/store.expected"
    for entry in "$verified"/store/*; do
      basename "$entry"
    done | LC_ALL=C sort >"$actual"
    while IFS= read -r entry; do
      basename "$entry"
    done <${closure}/store-paths | LC_ALL=C sort >"$expected"
    cmp "$actual" "$expected"
    for library in ${lib.escapeShellArgs managerLibraries}; do
      test -L "$verified/helmr/manager/lib/$library"
      test "$(readlink "$verified/helmr/manager/lib/$library")" = \
        "${libcLib}/lib/$library"
    done

    fixture="$TMPDIR/fixture"
    install -d -m0755 "$fixture" "$fixture/tmp"
    printf '%s\n' '#include <stdio.h>' \
      'int main(void) { puts("helmr"); return 0; }' >"$fixture/main.c"
    cat >"$fixture/addon.c" <<'EOF'
    #include <node_api.h>

    static napi_value init(napi_env env, napi_value exports) {
      napi_value value;
      if (napi_create_string_utf8(env, "helmr", NAPI_AUTO_LENGTH, &value) != napi_ok) {
        return NULL;
      }
      if (napi_set_named_property(env, exports, "value", value) != napi_ok) {
        return NULL;
      }
      return exports;
    }

    NAPI_MODULE(NODE_GYP_MODULE_NAME, init)
    EOF

    guest="$TMPDIR/guest"
    install -d -m0755 \
      "$guest/bin" \
      "$guest/dev" \
      "$guest/nix" \
      "$guest/proc" \
      "$guest/usr/bin" \
      "$guest/work"
    ln -s /nix/bin/sh "$guest/bin/sh"
    ln -s /nix/bin/env "$guest/usr/bin/env"
    build_guest=(
      -r "$guest"
      -b /dev
      -b /proc
      -b "$verified:/nix"
      -b "$fixture:/work"
      -w /work
      -i 65532:65532
    )
    env -i HOME=/work PATH=/nix/bin TMPDIR=/work/tmp \
      ${proot}/bin/proot "''${build_guest[@]}" /bin/sh -euo pipefail -c '
        test "$(/usr/bin/env sh -c "printf helmr")" = helmr
        node --version >/dev/null
        test "$(python3 -c "print(\"helmr\")")" = helmr
        pkg-config --version >/dev/null
        g++ --version >/dev/null
        cc "$HOME/main.c" -o "$HOME/main"
        test "$("$HOME/main")" = helmr
        cc -shared -fPIC \
          -I${node}/include/node \
          "$HOME/addon.c" \
          -o "$HOME/addon.node"
        printf "all:\\n\\t@true\\n" >"$HOME/Makefile"
        make -C "$HOME"
      '

    runtime="$TMPDIR/runtime"
    unsquashfs -no-progress -d "$runtime" \
      ${managedRuntime}/runtime.squashfs >/dev/null
    runtime_guest="$TMPDIR/runtime-guest"
    install -d -m0755 \
      "$runtime_guest/dev" \
      "$runtime_guest/opt/helmr/runtime" \
      "$runtime_guest/proc" \
      "$runtime_guest/work"
    env -i HOME=/work PATH=/opt/helmr/runtime/bin \
      ${proot}/bin/proot \
        -r "$runtime_guest" \
        -b /dev \
        -b /proc \
        -b "$runtime:/opt/helmr/runtime" \
        -b "$fixture:/work" \
        -w /work \
        -i 65532:65532 \
        /opt/helmr/runtime/bin/node \
        -e 'if (require("/work/addon.node").value !== "helmr") process.exit(1)'

    bun_source="$TMPDIR/bun-source"
    unzip -q ${bunArchive} -d "$bun_source"
    manager="$TMPDIR/manager"
    install -d -m0755 "$manager/bin"
    install -m0555 "$bun_source/${bun.root}/bun" "$manager/bin/bun"
    cmp "$bun_source/${bun.root}/bun" "$manager/bin/bun"

    bun_guest="$TMPDIR/bun-guest"
    install -d -m0755 \
      "$bun_guest/bin" \
      "$bun_guest${builtins.dirOf interpreter}" \
      "$bun_guest/dev" \
      "$bun_guest/nix" \
      "$bun_guest/opt/helmr/manager" \
      "$bun_guest/opt/helmr/runtime" \
      "$bun_guest/tmp" \
      "$bun_guest/usr/bin" \
      "$bun_guest/work"
    chmod 1777 "$bun_guest/tmp"
    for device in null zero random urandom; do
      touch "$bun_guest/dev/$device"
    done
    ln -s /nix/bin/sh "$bun_guest/bin/sh"
    ln -s /nix/helmr/manager/lib/${loader} \
      "$bun_guest${interpreter}"
    ln -s /nix/bin/env "$bun_guest/usr/bin/env"
    install -d -m0755 "$fixture/home"
    cat >"$fixture/child.ts" <<'EOF'
    import { spawnSync } from "node:child_process";
    import { existsSync } from "node:fs";

    if (existsSync("/proc/self")) process.exit(1);
    const child = spawnSync(process.execPath, ["--version"], { encoding: "utf8" });
    if (child.status !== 0 || child.stdout.trim() !== process.versions.bun) process.exit(1);
    EOF
    cat >"$fixture/shebang" <<'EOF'
    #!/usr/bin/env bun
    if (process.versions.bun.length === 0) process.exit(1);
    EOF
    chmod 0755 "$fixture/shebang"
    bun_environment=(
      HOME=/work/home
      LD_LIBRARY_PATH=/nix/helmr/manager/lib
      PATH=/opt/helmr/manager/bin:/opt/helmr/runtime/bin:/nix/bin
    )
    bun_root=(
      -r "$bun_guest"
      -b /dev/null:/dev/null
      -b /dev/zero:/dev/zero
      -b /dev/random:/dev/random
      -b /dev/urandom:/dev/urandom
      -b "$verified:/nix"
      -b "$manager:/opt/helmr/manager"
      -b "$runtime:/opt/helmr/runtime"
      -b "$fixture:/work"
      -w /work
      -i 65532:65532
    )
    env -i "''${bun_environment[@]}" \
      ${proot}/bin/proot "''${bun_root[@]}" \
      /opt/helmr/manager/bin/bun --version |
      grep -Fx ${lib.escapeShellArg bunVersion}
    env -i "''${bun_environment[@]}" \
      ${proot}/bin/proot "''${bun_root[@]}" \
      /opt/helmr/manager/bin/bun install --help >/dev/null
    env -i "''${bun_environment[@]}" \
      ${proot}/bin/proot "''${bun_root[@]}" \
      /opt/helmr/manager/bin/bun /work/child.ts
    env -i "''${bun_environment[@]}" \
      ${proot}/bin/proot "''${bun_root[@]}" \
      /work/shebang

    descriptor="$TMPDIR/toolchain.json"
    jq -cn \
      --arg architecture ${lib.escapeShellArg target} \
      --arg digest "sha256:$(sha256sum "$image" | cut -d' ' -f1)" \
      --arg runtimeDigest "$(jq -er '.digest' ${managedRuntime}/runtime.descriptor.json)" \
      --argjson sizeBytes "$(stat -c %s "$image")" \
      '{
        architecture:$architecture,
        formatVersion:0,
        managedRuntimeDigest:$runtimeDigest,
        toolchainClosure:{
          digest:$digest,
          mediaType:"application/vnd.helmr.standard-toolchain.v0+squashfs",
          sizeBytes:$sizeBytes
        }
      }' >"$descriptor"

    tool-release candidate \
      --input "$descriptor" \
      --closure "$image" \
      --output "$out"
  '';

  passthru = {
    inherit bunArchive managedRuntime;
    architecture = target;
  };

  meta = {
    description = "Helmr standard-toolchain release candidate";
    platforms = [ "x86_64-linux" ];
  };
}
