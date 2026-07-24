{
  lib,
  stdenv,
  stdenvNoCC,
  coreutils,
  fetchurl,
  jq,
  squashfsTools,
  gnutar,
  unzip,
}:

let
  bunVersion = "1.3.10";
  bunArchive = fetchurl {
    url = "https://github.com/oven-sh/bun/releases/download/bun-v${bunVersion}/bun-linux-x64-baseline.zip";
    hash = "sha256-QSAajF7nSp3Lsc4loRBPH5KYOLV6hFqnjZg3mwznzeI=";
  };
  npmVersion = "11.4.2";
  npmArchive = fetchurl {
    url = "https://registry.npmjs.org/npm/-/npm-${npmVersion}.tgz";
    hash = "sha256-i0aaVthaYavYRueGkGI86Va01JrlbxWsdt6g3OO9Sys=";
  };
in
assert lib.assertMsg stdenv.hostPlatform.isx86_64
  "Helmr v0 Manager release supports only x86_64";
stdenvNoCC.mkDerivation {
  pname = "helmr-manager-release";
  version = "0";
  dontUnpack = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    jq
    squashfsTools
    gnutar
    unzip
  ];

  buildCommand = ''
    set -euo pipefail

    build_tree() {
      local source="$1"
      local image="$2"
      LC_ALL=C TZ=UTC env -u SOURCE_DATE_EPOCH mksquashfs "$source" "$image" \
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

    bun_source="$TMPDIR/bun-source"
    unzip -q ${bunArchive} -d "$bun_source"
    bun_tree="$TMPDIR/bun-tree"
    install -d -m0755 "$bun_tree/bin"
    install -m0555 \
      "$bun_source/bun-linux-x64-baseline/bun" \
      "$bun_tree/bin/bun"
    bun_image="$TMPDIR/bun.squashfs"
    build_tree "$bun_tree" "$bun_image"

    npm_source="$TMPDIR/npm-source"
    install -d -m0755 "$npm_source"
    tar -xzf ${npmArchive} -C "$npm_source"
    npm_tree="$TMPDIR/npm-tree"
    install -d -m0755 "$npm_tree/lib"
    mv "$npm_source/package" "$npm_tree/lib/npm"
    npm_image="$TMPDIR/npm.squashfs"
    build_tree "$npm_tree" "$npm_image"

    bun_digest="sha256:$(sha256sum "$bun_image" | cut -d' ' -f1)"
    npm_digest="sha256:$(sha256sum "$npm_image" | cut -d' ' -f1)"
    bun_source_digest="sha256:$(sha256sum ${bunArchive} | cut -d' ' -f1)"
    npm_source_digest="sha256:$(sha256sum ${npmArchive} | cut -d' ' -f1)"

    install -d -m0755 "$out/objects/sha256"
    install -m0444 "$bun_image" \
      "$out/objects/sha256/''${bun_digest#sha256:}"
    install -m0444 "$npm_image" \
      "$out/objects/sha256/''${npm_digest#sha256:}"

    jq -jScn \
      --arg bunDigest "$bun_digest" \
      --arg bunSourceDigest "$bun_source_digest" \
      --arg bunVersion ${lib.escapeShellArg bunVersion} \
      --arg npmDigest "$npm_digest" \
      --arg npmSourceDigest "$npm_source_digest" \
      --arg npmVersion ${lib.escapeShellArg npmVersion} \
      --argjson bunSize "$(stat -c %s "$bun_image")" \
      --argjson bunSourceSize "$(stat -c %s ${bunArchive})" \
      --argjson npmSize "$(stat -c %s "$npm_image")" \
      --argjson npmSourceSize "$(stat -c %s ${npmArchive})" \
      '{
        formatVersion:0,
        managers:[
          {
            adapterVersion:"helmr.manager.v0",
            architecture:"x86_64",
            entrypoint:{kind:"native",path:"/opt/helmr/manager/bin/bun"},
            packageManager:{name:"bun",version:$bunVersion},
            source:{
              digest:$bunSourceDigest,
              origin:("https://github.com/oven-sh/bun/releases/download/bun-v"+$bunVersion+"/bun-linux-x64-baseline.zip"),
              sizeBytes:$bunSourceSize
            },
            tree:{
              digest:$bunDigest,
              mediaType:"application/vnd.helmr.package-manager.v0+squashfs",
              sizeBytes:$bunSize
            }
          },
          {
            adapterVersion:"helmr.manager.v0",
            architecture:"x86_64",
            entrypoint:{kind:"node",path:"/opt/helmr/manager/lib/npm/bin/npm-cli.js"},
            packageManager:{name:"npm",version:$npmVersion},
            source:{
              digest:$npmSourceDigest,
              origin:("https://registry.npmjs.org/npm/-/npm-"+$npmVersion+".tgz"),
              sizeBytes:$npmSourceSize
            },
            tree:{
              digest:$npmDigest,
              mediaType:"application/vnd.helmr.package-manager.v0+squashfs",
              sizeBytes:$npmSize
            }
          }
        ]
      }' >"$out/catalog.json"

    catalog_hex="$(sha256sum "$out/catalog.json" | cut -d' ' -f1)"
    jq -jScn \
      --arg catalogDigest "sha256:$catalog_hex" \
      --arg catalogHex "$catalog_hex" \
      --slurpfile catalog "$out/catalog.json" \
      '{
        _type:"https://in-toto.io/Statement/v1",
        predicateType:"https://helmr.dev/attestations/manager-release/v0",
        subject:[{
          name:"manager-release/catalog.json",
          digest:{sha256:$catalogHex}
        }],
        predicate:{
          catalogDigest:$catalogDigest,
          catalogMediaType:"application/vnd.helmr.manager-catalog.v0+json",
          formatVersion:0,
          managers:$catalog[0].managers,
          predecessor:null
        }
      }' >"$out/attestation.json"

    chmod 0444 "$out/catalog.json" "$out/attestation.json"
  '';

  passthru = {
    inherit bunArchive bunVersion npmArchive npmVersion;
  };

  meta = {
    description = "Helmr certified package-manager release";
    platforms = [ "x86_64-linux" ];
  };
}
