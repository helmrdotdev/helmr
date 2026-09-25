{
  lib,
  runCommand,
  dockerTools,
  unzip,
  baseImage,
  nodeArchive,
  bunArchive,
  bundleBuilder,
  compiler,
  runtimeRelease,
  squashfsTools,
  substrateGenerator,
}:

let
  # Helmr's verified tools live only under /opt/helmr and /nix, the two trees
  # the build graph mounts read-only from this pinned image.
  platform = runCommand "helmr-bundle-builder-platform" { } ''
    mkdir -p \
      "$out/nix/helmr" \
      "$out/opt/helmr/bin" \
      "$out/opt/helmr/release"

    cp -a ${runtimeRelease}/tree "$out/opt/helmr/runtime"
    cp ${runtimeRelease}/runtime.descriptor.json "$out/opt/helmr/release/runtime.descriptor.json"
    cp -a ${compiler}/tree/helmr/. "$out/nix/helmr/"
    cp -a ${compiler}/tree/moduleexecution "$out/nix/moduleexecution"
    cp -a ${compiler}/tree/share "$out/nix/share"
    chmod u+w "$out/nix/helmr"
    cp ${compiler}/compiler.descriptor.json "$out/nix/helmr/compiler.descriptor.json"
    chmod u-w "$out/nix/helmr"

    ln -s ${lib.getBin substrateGenerator}/bin/mke2fs "$out/opt/helmr/bin/mke2fs"
    cp ${./mke2fs.conf} "$out/opt/helmr/release/mke2fs.conf"
    ln -s ${bundleBuilder}/bin/bundle-builder "$out/opt/helmr/bin/bundle-builder"
    ln -s ${squashfsTools}/bin/mksquashfs "$out/opt/helmr/bin/mksquashfs"
    ln -s /workspace/project "$out/opt/helmr/program"
  '';
  # The user-facing toolchain is the unmodified official release, running on
  # the Debian base like any other program there.
  userTools = runCommand "helmr-bundle-builder-user-tools" { nativeBuildInputs = [ unzip ]; } ''
    mkdir -p "$out/usr/local" "$TMPDIR/node" "$TMPDIR/bun"
    tar -xJf ${nodeArchive} --strip-components=1 --directory "$TMPDIR/node"
    for directory in bin include lib share; do
      cp -a "$TMPDIR/node/$directory" "$out/usr/local/$directory"
    done
    unzip -q ${bunArchive} -d "$TMPDIR/bun"
    install -m0755 "$TMPDIR"/bun/*/bun "$out/usr/local/bin/bun"
  '';
in
dockerTools.buildLayeredImage {
  name = "bundle-builder";
  tag = "0";
  created = "1970-01-01T00:00:01Z";
  fromImage = baseImage;
  maxLayers = 120;

  contents = [ platform ];

  # Real files, not store links: /usr/local stays an ordinary writable prefix.
  extraCommands = ''
    mkdir -p usr/local
    cp -a ${userTools}/usr/local/. usr/local/
    chmod -R u+w usr/local
  '';

  config = {
    Labels."org.opencontainers.image.source" = "https://github.com/helmrdotdev/helmr";
    Cmd = [ "/opt/helmr/bin/bundle-builder" ];
    Env = [
      "COREPACK_DEFAULT_TO_LATEST=0"
      "COREPACK_ENABLE_DOWNLOAD_PROMPT=0"
      "HOME=/workspace/home"
      "LANG=C.UTF-8"
      "LC_ALL=C.UTF-8"
      "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
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
