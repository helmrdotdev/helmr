{
  lib,
  stdenv,
  stdenvNoCC,
  closureInfo,
  bun,
  bash,
  coreutils,
  findutils,
  gcc,
  gnumake,
  jq,
  pkg-config,
  python3,
  managedRuntime,
  releaseTool,
  squashfsTools,
}:

let
  target =
    {
      x86_64-linux = "x86_64";
      aarch64-linux = "aarch64";
    }
    .${stdenv.hostPlatform.system}
      or (throw "dependency tools are unsupported on ${stdenv.hostPlatform.system}");
  node = managedRuntime.nodejs_24;
  managerClosure = closureInfo {
    rootPaths = [ bun ];
  };
  toolchainRoots = [
    bash
    coreutils
    gcc
    gnumake
    node
    pkg-config
    python3
  ];
  toolchainClosure = closureInfo {
    rootPaths = toolchainRoots;
  };
  toolsetClosure = closureInfo {
    rootPaths = [ bun ] ++ toolchainRoots;
  };
  path = lib.concatStringsSep ":" (
    [ "/opt/helmr/dependency-tools/bin" ]
    ++ map (package: "${package}/bin") ([
      bun
      bash
      coreutils
      gcc
      python3
      gnumake
      pkg-config
      node
    ])
  );
in
assert lib.assertMsg (
  bun.version == "1.3.10"
) "dependency tools require Bun 1.3.10, got ${bun.version}";
assert lib.assertMsg (
  node.version == "24.16.0"
) "dependency tools require Managed Node 24.16.0, got ${node.version}";
stdenvNoCC.mkDerivation {
  pname = "helmr-dependency-tools-${target}";
  version = "0";
  dontUnpack = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    findutils
    jq
    releaseTool
    squashfsTools
  ];

  buildCommand = ''
    set -euo pipefail

    encode_image() {
      source="$1"
      destination="$2"
      LC_ALL=C TZ=UTC env -u SOURCE_DATE_EPOCH mksquashfs "$source" "$destination" \
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
    }

    build_closure() {
      paths="$1"
      destination="$2"
      tree="$TMPDIR/$3"
      install -d -m0755 "$tree/store"
      while IFS= read -r source; do
        cp -a "$source" "$tree/store/$(basename "$source")"
      done <"$paths"
      encode_image "$tree" "$destination"
    }

    verify_closure() {
      archive="$1"
      paths="$2"
      name="$3"
      root="$TMPDIR/verify-$name"
      unsquashfs -no-progress -d "$root" "$archive" >/dev/null
      test "$(find "$root" -mindepth 1 -maxdepth 1 -printf '%f\n')" = store
      actual="$TMPDIR/$name.actual"
      expected="$TMPDIR/$name.expected"
      for path in "$root"/store/*; do
        basename "$path"
      done | LC_ALL=C sort >"$actual"
      while IFS= read -r path; do
        basename "$path"
      done <"$paths" | LC_ALL=C sort >"$expected"
      cmp "$actual" "$expected"
    }

    work="$TMPDIR/release"
    install -d -m0755 "$work"
    manager="$work/manager.squashfs"
    toolchain="$work/toolchain.squashfs"
    toolset="$work/toolset.squashfs"
    build_closure ${managerClosure}/store-paths "$manager" manager
    build_closure ${toolchainClosure}/store-paths "$toolchain" toolchain
    verify_closure "$manager" ${managerClosure}/store-paths manager
    verify_closure "$toolchain" ${toolchainClosure}/store-paths toolchain

    descriptor() {
      file="$1"
      media_type="$2"
      jq -cn \
        --arg digest "sha256:$(sha256sum "$file" | cut -d' ' -f1)" \
        --arg mediaType "$media_type" \
        --argjson sizeBytes "$(stat -c %s "$file")" \
        '{digest:$digest,mediaType:$mediaType,sizeBytes:$sizeBytes}'
    }

    manager_descriptor="$(descriptor \
      "$manager" \
      application/vnd.helmr.package-manager-component.v0+squashfs)"
    toolchain_descriptor="$(descriptor \
      "$toolchain" \
      application/vnd.helmr.standard-toolchain.v0+squashfs)"
    runtime_digest="$(jq -er '.digest' ${managedRuntime}/runtime.descriptor.json)"
    raw_components="$work/components.raw.json"
    components="$work/components.json"
    jq -cn \
      --arg architecture ${lib.escapeShellArg target} \
      --arg bun ${lib.escapeShellArg "${bun}/bin/bun"} \
      --arg bash ${lib.escapeShellArg "${bash}/bin/sh"} \
      --arg env ${lib.escapeShellArg "${coreutils}/bin/env"} \
      --arg path ${lib.escapeShellArg path} \
      --arg runtimeDigest "$runtime_digest" \
      --argjson managerClosure "$manager_descriptor" \
      --argjson toolchainClosure "$toolchain_descriptor" \
      '{
        architecture:$architecture,
        environment:[
          {name:"HOME",value:"/work/home"},
          {name:"PATH",value:$path}
        ],
        formatVersion:0,
        launchers:[{path:"bin/bun",target:$bun}],
        managedRuntimeDigest:$runtimeDigest,
        manager:{
          architecture:$architecture,
          executable:"/opt/helmr/dependency-tools/bin/bun",
          formatVersion:0,
          lifecycle:{argv:["/opt/helmr/dependency-tools/bin/bun"]},
          lockfileAdapter:"bun-lock-v0",
          managerClosure:$managerClosure,
          offlineStore:{
            readOnlyMountPath:"/opt/helmr/offline-store",
            workPath:"/work/offline-store"
          },
          packageManager:{name:"bun",version:"1.3.10"},
          proxy:{registryOrigin:"http://127.0.0.1:4873"},
          resolution:{argv:["/opt/helmr/dependency-tools/bin/bun"]},
          versionProbe:{
            argv:["/opt/helmr/dependency-tools/bin/bun","--version"],
            stdoutBase64:"MS4zLjEwCg=="
          }
        },
        materializerVersion:"helmr.dependencies.v0",
        packageManager:{name:"bun",version:"1.3.10"},
        systemAliases:[
          {path:"/bin/sh",target:$bash},
          {path:"/usr/bin/env",target:$env}
        ],
        toolchain:{
          architecture:$architecture,
          formatVersion:0,
          managedRuntimeDigest:$runtimeDigest,
          toolchainClosure:$toolchainClosure
        }
      }' >"$raw_components"
    tool-release components --input "$raw_components" --output "$components"

    tree="$TMPDIR/toolset"
    install -d -m0755 "$tree/bin" "$tree/helmr" "$tree/store"
    while IFS= read -r source; do
      cp -a "$source" "$tree/store/$(basename "$source")"
    done <${toolsetClosure}/store-paths
    ln -s ${bun}/bin/bun "$tree/bin/bun"
    install -m0444 "$components" "$tree/helmr/components.json"
    encode_image "$tree" "$toolset"
    repeated="$work/toolset.repeated.squashfs"
    encode_image "$tree" "$repeated"
    cmp "$toolset" "$repeated"

    verified="$TMPDIR/verify-toolset"
    unsquashfs -no-progress -d "$verified" "$toolset" >/dev/null
    printf '%s\n' bin helmr store >"$TMPDIR/toolset.expected"
    find "$verified" -mindepth 1 -maxdepth 1 -printf '%f\n' |
      LC_ALL=C sort >"$TMPDIR/toolset.actual"
    cmp "$TMPDIR/toolset.actual" "$TMPDIR/toolset.expected"
    cmp "$verified/helmr/components.json" "$components"

    jq -r '.launchers[].target,.systemAliases[].target' "$components" |
      while IFS= read -r target; do
        case "$target" in
          /nix/store/*) ;;
          *) exit 1 ;;
        esac
        test -x "$verified/store/''${target#/nix/store/}"
      done

    registered_path="$(jq -er '.environment[] | select(.name == "PATH") | .value' "$components")"
    IFS=: read -r -a registered_paths <<<"$registered_path"
    for entry in "''${registered_paths[@]}"; do
      if [ "$entry" = /opt/helmr/dependency-tools/bin ]; then
        test -d "$verified/bin"
      else
        test -d "$verified/store/''${entry#/nix/store/}"
      fi
    done

    probe="$work/version-probe"
    "$verified/bin/bun" --version >"$probe"
    test "$(base64 -w0 "$probe")" = \
      "$(jq -er '.manager.versionProbe.stdoutBase64' "$components")"

    fixture="$work/toolchain-fixture"
    install -d -m0755 "$fixture"
    printf '%s\n' '#include <stdio.h>' \
      'int main(void) { puts("helmr"); return 0; }' >"$fixture/main.c"
    env -i HOME="$fixture" PATH="$registered_path" \
      bash -euo pipefail -c '
        node --version >/dev/null
        test "$(python3 -c "print(\"helmr\")")" = helmr
        cc "$HOME/main.c" -o "$HOME/main"
        test "$("$HOME/main")" = helmr
        printf "all:\\n\\t@true\\n" >"$HOME/Makefile"
        make -C "$HOME"
      '

    tool-release registry \
      --components "$components" \
      --manager "$manager" \
      --toolchain "$toolchain" \
      --toolset "$toolset" \
      --output "$out"
  '';

  passthru = {
    inherit managedRuntime;
    architecture = target;
  };

  meta = {
    description = "Helmr dependency tool release candidate";
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
  };
}
