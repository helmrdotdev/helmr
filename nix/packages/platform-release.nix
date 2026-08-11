{
  stdenvNoCC,
  coreutils,
  jq,
  runtimeHarness,
  toolchainBase,
  nodeReleaseKeys,
  policyTool,
}:

stdenvNoCC.mkDerivation {
  pname = "helmr-platform-release";
  version = "0";
  dontUnpack = true;
  strictDeps = true;

  nativeBuildInputs = [
    coreutils
    jq
    policyTool
  ];

  buildCommand = ''
    set -euo pipefail

    install -d "$out/objects/sha256"
    platform-policy \
      --runtime ${runtimeHarness}/harness.descriptor.json \
      --toolchain ${toolchainBase}/base.descriptor.json \
      --node-keyring ${nodeReleaseKeys}/node-release-keyring.gpg \
      --node-fingerprints ${nodeReleaseKeys}/fingerprints \
      --output "$TMPDIR/build-policy.json"

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
      ${runtimeHarness}/harness.tar \
      ${runtimeHarness}/harness.descriptor.json)"
    jq -cS '.base' \
      ${toolchainBase}/base.descriptor.json \
      >"$TMPDIR/toolchain-base-object.json"
    toolchain_object="$(install_object \
      ${toolchainBase}/base.tar \
      "$TMPDIR/toolchain-base-object.json")"

    policy_digest="sha256:$(sha256sum "$TMPDIR/build-policy.json" | cut -d' ' -f1)"
    policy_size="$(stat -c %s "$TMPDIR/build-policy.json")"
    install -m0444 \
      "$TMPDIR/build-policy.json" \
      "$out/objects/sha256/''${policy_digest#sha256:}"
    policy_object="$(jq -cn \
      --arg digest "$policy_digest" \
      --arg mediaType 'application/vnd.helmr.build-policy.v0+json' \
      --argjson sizeBytes "$policy_size" \
      '{digest:$digest,mediaType:$mediaType,sizeBytes:$sizeBytes}')"

    jq -cSj -n \
      --argjson formatVersion 0 \
      --argjson policy "$policy_object" \
      --argjson runtimeHarness "$runtime_object" \
      --argjson toolchainBase "$toolchain_object" \
      '{
        formatVersion:$formatVersion,
        policy:$policy,
        runtimeHarness:$runtimeHarness,
        toolchainBase:$toolchainBase
      }' >"$out/platform-release.json"
    printf '%s' "$policy_digest" >"$out/build-policy.digest"
  '';
}
