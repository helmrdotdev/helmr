{
  stdenvNoCC,
  coreutils,
  jq,
  runtimeRelease,
}:

stdenvNoCC.mkDerivation {
  pname = "helmr-platform-release";
  version = "0";
  dontUnpack = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    jq
  ];

  buildCommand = ''
    set -euo pipefail

    install -d "$out/objects/sha256"
    install_object() {
      source="$1"
      descriptor="$2"
      digest="$(jq -er '.digest' "$descriptor")"
      size="$(jq -er '.sizeBytes' "$descriptor")"
      media_type="$(jq -er '.mediaType' "$descriptor")"
      actual_digest="sha256:$(sha256sum "$source" | cut -d' ' -f1)"
      actual_size="$(stat -c %s "$source")"
      [ "$digest" = "$actual_digest" ]
      [ "$size" = "$actual_size" ]
      install -m0444 "$source" "$out/objects/sha256/''${digest#sha256:}"
      jq -cn \
        --arg digest "$digest" \
        --arg mediaType "$media_type" \
        --argjson sizeBytes "$size" \
        '{digest:$digest,mediaType:$mediaType,sizeBytes:$sizeBytes}'
    }

    runtime_object="$(install_object \
      ${runtimeRelease}/runtime.squashfs \
      ${runtimeRelease}/runtime.descriptor.json)"
    jq -cSj -n \
      --argjson formatVersion 0 \
      --argjson runtime "$runtime_object" \
      '{
        formatVersion:$formatVersion,
        runtime:$runtime
      }' >"$out/platform-release.json"
  '';
}
