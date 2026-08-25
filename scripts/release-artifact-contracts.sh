validate_worker_host_bundle_receipt() {
  jq -e '
    (keys | sort) == ["bundle", "manifest", "schema", "sourceCommit"] and
    .schema == "helmr.worker-host-bundle.v0" and
    (.sourceCommit | test("^[0-9a-f]{40}$")) and
    .bundle.path == "worker-host-artifacts.tar" and
    (.bundle.digest | test("^sha256:[0-9a-f]{64}$")) and
    .manifest.path == "worker-host-artifacts.json" and
    (.manifest.digest | test("^sha256:[0-9a-f]{64}$"))
  ' "$1" >/dev/null
}

validate_worker_runtime_bundle_receipt() {
  jq -e '
    (keys | sort) == ["bundle", "runtimeArtifactsManifest", "schema", "sourceCommit"] and
    .schema == "helmr.worker-runtime-bundle.v0" and
    (.sourceCommit | test("^[0-9a-f]{40}$")) and
    .bundle.path == "runtime-artifacts.tar" and
    (.bundle.digest | test("^sha256:[0-9a-f]{64}$")) and
    .runtimeArtifactsManifest.path == "runtime-artifacts.json" and
    (.runtimeArtifactsManifest.digest | test("^sha256:[0-9a-f]{64}$"))
  ' "$1" >/dev/null
}

validate_worker_image_definition() {
  local definition_file=$1
  jq -e '
    (keys | sort) == [
      "componentARN",
      "componentDefinitionDigest",
      "distributionConfigurationARN",
      "distributionRegions",
      "hostArtifacts",
      "imageDefinitionDigest",
      "imagePipelineARN",
      "imageRecipeARN",
      "prepareRootDigest",
      "resolvedParentImageID",
      "rootBlockDeviceMapping",
      "runtimeArtifacts",
      "schema",
      "visibility"
    ] and
    .schema == "helmr.worker-image-definition-state.v0" and
    (.componentARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:component/.+/1[.]0[.]0/[0-9]+$")) and
    (.componentDefinitionDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.distributionConfigurationARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:distribution-configuration/.+$")) and
    (.distributionRegions | type == "array" and length > 0 and . == (sort | unique) and all(.[]; test("^[a-z]{2}-[a-z-]+-[0-9]+$"))) and
    (.imageDefinitionDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.imagePipelineARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:image-pipeline/.+$")) and
    (.imageRecipeARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:image-recipe/.+/1[.]0[.]0$")) and
    (.prepareRootDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.resolvedParentImageID | test("^ami-([0-9a-f]{8}|[0-9a-f]{17})$")) and
    .rootBlockDeviceMapping == {
      deviceName: "/dev/sda1",
      ebs: {
        deleteOnTermination: true,
        encrypted: .rootBlockDeviceMapping.ebs.encrypted,
        volumeSize: .rootBlockDeviceMapping.ebs.volumeSize,
        volumeType: "gp3"
      }
    } and
    (.rootBlockDeviceMapping.ebs.encrypted | type == "boolean") and
    (.rootBlockDeviceMapping.ebs.volumeSize | type == "number" and floor == . and . > 0) and
    (.visibility == "public" or .visibility == "private") and
    (.hostArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
    (.hostArtifacts.sourceCommit | test("^[0-9a-f]{40}$")) and
    all(.hostArtifacts.bundleDigest, .hostArtifacts.manifestDigest; test("^sha256:[0-9a-f]{64}$")) and
    (.runtimeArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
    (.runtimeArtifacts.sourceCommit | test("^[0-9a-f]{40}$")) and
    all(.runtimeArtifacts.bundleDigest, .runtimeArtifacts.manifestDigest; test("^sha256:[0-9a-f]{64}$"))
  ' "${definition_file}" >/dev/null
}

validate_worker_image_receipt() {
  local receipt_file=$1
  jq -e '
    (keys | sort) == [
      "amis",
      "componentDefinitionDigest",
      "hostArtifacts",
      "imageBuildVersionARN",
      "imageDefinitionDigest",
      "imageRecipeARN",
      "prepareRootDigest",
      "resolvedParentImageID",
      "runtimeArtifacts",
      "schema",
      "visibility"
    ] and
    .schema == "helmr.worker-image.v0" and
    (.amis | type == "object" and length > 0) and
    all(.amis | keys[]; test("^[a-z]{2}-[a-z-]+-[0-9]+$")) and
    all(.amis[]; test("^ami-([0-9a-f]{8}|[0-9a-f]{17})$")) and
    (.visibility == "public" or .visibility == "private") and
    (.imageBuildVersionARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:image/.+/1[.]0[.]0/[0-9]+$")) and
    (.imageRecipeARN | test("^arn:[^:]+:imagebuilder:[^:]+:[0-9]{12}:image-recipe/.+/1[.]0[.]0$")) and
    (.componentDefinitionDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.imageDefinitionDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.prepareRootDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.resolvedParentImageID | test("^ami-([0-9a-f]{8}|[0-9a-f]{17})$")) and
    (.hostArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
    (.hostArtifacts.sourceCommit | test("^[0-9a-f]{40}$")) and
    all(.hostArtifacts.bundleDigest, .hostArtifacts.manifestDigest; test("^sha256:[0-9a-f]{64}$")) and
    (.runtimeArtifacts | keys == ["bundleDigest", "manifestDigest", "sourceCommit"]) and
    (.runtimeArtifacts.sourceCommit | test("^[0-9a-f]{40}$")) and
    all(.runtimeArtifacts.bundleDigest, .runtimeArtifacts.manifestDigest; test("^sha256:[0-9a-f]{64}$"))
  ' "${receipt_file}" >/dev/null
}
