{
  lib,
  runCommand,
  dockerTools,
  unzip,
  crane,
  openssl,
  openssh,
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
  # Rebuild Debian's bundle and hashed CA directory with the distrusted
  # Mozilla roots disabled. Keep Debian's selection for later package updates.
  trustStore =
    runCommand "helmr-builder-trust-store"
      {
        nativeBuildInputs = [
          crane
          openssl
        ];
      }
      ''
        root="$PWD/rootfs"
        mkdir -p "$root"
        crane export - - < ${baseImage} | tar -x -C "$root" --no-same-owner --no-same-permissions \
          etc/ca-certificates.conf usr/sbin/update-ca-certificates \
          usr/share/ca-certificates usr/local/share/ca-certificates etc/ssl/certs
        sed -i -E '/^mozilla\/emSign_(ECC_Root_CA_-_(C3|G3)|Root_CA_-_C1)\.crt$/s/^/!/' "$root/etc/ca-certificates.conf"
        sh -e "$root/usr/sbin/update-ca-certificates" --sysroot "$root" --fresh --hooksdir "$TMPDIR/no-hooks"
        mkdir -p "$out/etc/ssl"
        cp "$root/etc/ca-certificates.conf" "$out/etc/"
        cp -a "$root/etc/ssl/certs" "$out/etc/ssl/"
        # The sysroot helper creates sysroot-prefixed absolute PEM symlinks.
        for link in "$out/etc/ssl/certs/"*.pem; do
          target=$(readlink "$link")
          ln -sfn "''${target#"$root"}" "$link"
        done
        # Hide old hash and PEM links from lower OCI layers.
        touch "$out/etc/ssl/certs/.wh..wh..opq"
      '';
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
    chmod u+w "$out/nix/helmr"
    cp ${compiler}/compiler.descriptor.json "$out/nix/helmr/compiler.descriptor.json"
    chmod u-w "$out/nix/helmr"

    ln -s ${lib.getBin substrateGenerator}/bin/mke2fs "$out/opt/helmr/bin/mke2fs"
    cp ${./mke2fs.conf} "$out/opt/helmr/release/mke2fs.conf"
    cp ${bundleBuilder}/bin/bundle-builder "$out/opt/helmr/bin/bundle-builder"
    mkdir -p "$out/opt/helmr/empty" "$TMPDIR/npm-node"
    tar -xJf ${nodeArchive} --strip-components=1 --directory "$TMPDIR/npm-node"
    cp -a "$TMPDIR/npm-node/lib/node_modules/npm" "$out/opt/helmr/npm"
    grep -q '"version": "11.19.1"' "$out/opt/helmr/npm/package.json"
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

  # Exported compiler/runtime paths must be real files so the scratch bundling
  # stage can copy only that closure. Native filesystem tools keep store links.
  extraCommands = ''
    cp -a ${trustStore}/etc .
    rm -rf opt/helmr nix/helmr
    mkdir -p opt/helmr nix/helmr
    cp -a ${platform}/opt/helmr/. opt/helmr/
    cp -a ${platform}/nix/helmr/. nix/helmr/
    mkdir -p usr/local
    cp -a ${userTools}/usr/local/. usr/local/
    chmod -R u+w usr/local
    mkdir -p usr/bin
    for command in ssh scp sftp ssh-add ssh-agent ssh-keygen ssh-keyscan; do
      ln -s ${openssh}/bin/"$command" "usr/bin/$command"
    done
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
