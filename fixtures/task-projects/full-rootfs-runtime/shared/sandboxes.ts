import { cache, image, sandbox, source as sourceRef } from "@helmr/sdk"

const imageWorkspace = sourceRef.directory("image-workspace")

const debianRoot = image("full-rootfs-debian")
  .from("debian:trixie-slim")
  .run([
    "sh",
    "-ceu",
    [
      "apt-get update",
      "apt-get install -y --no-install-recommends curl passwd",
      "useradd -m -u 10001 -s /bin/sh agent",
      "mkdir -p /custom/bin /tmp/task /tmp/home-agent/.cache /home/agent /var/log",
      "chmod 1777 /tmp /tmp/task /tmp/home-agent /tmp/home-agent/.cache /var/log",
      "chown -R agent:agent /home/agent",
    ].join(" && "),
  ])
  .copy("/workspace", imageWorkspace)

const debianContract = debianRoot
  .workdir("/tmp/task")
  .env("FOO", "BAR")
  .env("HOME", "/tmp/home-agent")
  .env("PATH", "/custom/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")

const debianAgent = debianContract.user("agent")

const debianDefault = image("full-rootfs-debian-default").from("debian:trixie-slim")
const alpineRoot = image("full-rootfs-alpine").from("alpine:3.22")
const distrolessRoot = image("full-rootfs-distroless").from(
  "gcr.io/distroless/static-debian12:nonroot",
)
const sourceAwareImage = image("full-rootfs-source-aware")
  .from("debian:trixie-slim")
  .workdir("/workspace")
  .copy("/opt/helmr-deps/package.json", sourceRef.file("package.json"))
  .run(
    [
      "sh",
      "-ceu",
      [
        "mkdir -p /opt/helmr-deps",
        "sha256sum /opt/helmr-deps/package.json > /opt/helmr-deps/install-input.sha256",
        "printf 'install layer executed\\n' > /opt/helmr-deps/install.log",
      ].join(" && "),
    ],
    { cache: [{ mountPath: "/var/cache/helmr-deps", cache: cache("full-rootfs-runtime-deps") }] },
  )
export const contractSandbox = sandbox("full-rootfs-contract")
  .image(debianContract)
  .workspace("/workspace")

export const agentSandbox = sandbox("full-rootfs-agent")
  .image(debianAgent)
  .workspace("/workspace")

export const defaultRootSandbox = sandbox("full-rootfs-default-root")
  .image(debianDefault)
  .workspace("/workspace-default")

export const defaultPathSandbox = sandbox("full-rootfs-default-path")
  .image(debianDefault)
  .workspace("/workspace-default")

export const alpineSandbox = sandbox("full-rootfs-alpine")
  .image(alpineRoot)
  .workspace("/workspace")

export const distrolessSandbox = sandbox("full-rootfs-distroless")
  .image(distrolessRoot)
  .workspace("/workspace")

export const implSandbox = sandbox("full-rootfs-impl")
  .image(sourceAwareImage)
  .workspace("/workspace")
