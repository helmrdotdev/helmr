import { agent, computer, triggers, builder as configBuilder, defineConfig, image, source,
  type HelmrClient, type ImageBuilder, type ComputerDefinitionInfo, type SessionEventPage,
  type SecretCreateRequest, type SecretRotateRequest, type SecretRevokeRequest,
  type SecretBinding, type SourceFile, type SourceDirectory, type ComputerRef } from "."
import * as sdk from "."

export function assertGreenfieldTypes(): void {
  // @ts-expect-error ImageBuilder values must be created by image().
  const unbrandedImage: ImageBuilder = {
    key: "unbranded",
    from: () => null as never,
    run: () => null as never,
    copy: () => null as never,
    copyFrom: () => null as never,
    workdir: () => null as never,
    env: () => null as never,
    user: () => null as never,
  }
  void unbrandedImage

  const placement: SecretBinding = { secretId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38", env: {name: "TOKEN", mode: "raw"} }
  const secretCreate: SecretCreateRequest = {
    name: "TOKEN",
    value: "secret",
  }
  const secretRotate: SecretRotateRequest = {
    value: "replacement",
    idempotencyKey: "rotate-token",
  }
  const secretRevoke: SecretRevokeRequest = {
    idempotencyKey: "revoke-token",
  }
  void placement
  void secretCreate
  void secretRotate
  void secretRevoke

  // @ts-expect-error registry authentication belongs to the local BuildKit session.
  image("private").from("ghcr.io/acme/base:1", { auth: {} })
  const sourceFile: SourceFile = source.file("./package.json")
  const sourceDirectory: SourceDirectory = source.directory("./src")
  image("source-copy")
    .copy(sourceFile, "/app/package.json")
    .copy(sourceDirectory, "/app/src")
  // @ts-expect-error The source comes first and the destination second.
  image("source-copy").copy("/app/package.json", sourceFile)
  // @ts-expect-error A copy needs a destination.
  image("source-copy").copy(sourceFile)
  const prepared = configBuilder().copy("build/setup.sh", "/opt/setup.sh").run(["/bin/sh", "/opt/setup.sh"])
  defineConfig({ dirs: ["src"], build: { builder: prepared, installCommand: "./prepare.sh", secrets: ["NPM_TOKEN"] } })
  // @ts-expect-error A Computer image is not a build environment.
  defineConfig({ build: { builder: image("computer").from("debian") } })
  // @ts-expect-error The build environment is not a Computer image.
  computer({ id: "wrong-role", image: prepared, resources: { cpu: 1, memory: "1GiB" } })
  // @ts-expect-error Builder sources are captured project paths, not installed-tree selectors.
  configBuilder().copy(sourceFile, "/opt/setup.sh")
  // @ts-expect-error The builder always starts from the Helmr builder image.
  configBuilder().from("debian")
  // @ts-expect-error builder() takes no base image or options.
  configBuilder("debian:bookworm")
  // @ts-expect-error Source files must be created by source.file().
  const unbrandedSourceFile: SourceFile = { path: "./package.json" }
  // @ts-expect-error Source directories must be created by source.directory().
  const unbrandedSourceDirectory: SourceDirectory = { path: "./src" }
  void unbrandedSourceFile
  void unbrandedSourceDirectory
  // @ts-expect-error Directory copy has no implementation-defined ignore language.
  source.directory("./src", { ignore: ["**/*.test.ts"] })
  const client = null as unknown as HelmrClient
  client.agents.retrieve("operator").then((info) => {
    info.id satisfies string
    info.deploymentId satisfies string
  })
  client.computerDefinitions.retrieve("machine").then((info: ComputerDefinitionInfo) => {
    info.id satisfies string
    info.deploymentId satisfies string
  })
  const clientComputer = client.computers.ref(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
  )
  clientComputer satisfies ComputerRef
  client.computerDefinitions.createComputer("machine").then((computer) => {
    computer satisfies ComputerRef
  })
  client.sessions.get(
    "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33",
  ).events.list({ after: 0, limit: 10 }) satisfies Promise<SessionEventPage>
  client.agents.start("operator", { input: [{ type: "text", text: "ok" }], computer: clientComputer }).then(({ session, turn, created }) => {
    created satisfies boolean
    session.id satisfies string
    turn.sessionId satisfies string
    turn.wait().then((outcome) => { outcome.status satisfies string })
  })
  // @ts-expect-error Agent admission requires explicit text-part input.
  client.agents.start("operator", { computer: clientComputer })

  const workspace = computer({ id: "workspace", image: image("workspace").from("debian"), resources: { cpu: 1, memory: "1GiB" } })
  agent({ id: "operator", computer: workspace, setup: () => ({ count: 0 }),
    async turn(turn, ctx) {
      ctx.setupResult.count satisfies number
      // @ts-expect-error Runtime delegation is available through managed MCP.
      void turn.agents
      if (ctx.session.parent) {
        ctx.session.parent.id satisfies string
        // @ts-expect-error Parent metadata grants no control handle.
        void ctx.session.parent.enqueue
      }
      await turn.output.write("Working")
      await turn.respond("Done")
      // @ts-expect-error The runtime owns settlement after the handler returns.
      void turn.complete
      // @ts-expect-error Session inbox loops are not Agent authoring.
      void ctx.session.receive
      return { count: ++ctx.setupResult.count }
    },
  })
  void triggers
  // @ts-expect-error Runtime Session handles belong to injected Turn authority.
  void sdk.sessions
  // @ts-expect-error Computer handles belong to the authenticated client.
  void sdk.computers
}

export function assertProcessDiagnosticsAreInternal(client: HelmrClient): void {
  // @ts-expect-error Process-wide diagnostics are not a public Session API.
  void client.sessions.logs
  // @ts-expect-error Internal producer identities are not publicly discoverable.
  void client.sessions.logStreams
  // @ts-expect-error Preparation process diagnostics remain internal.
  void client.computers.preparationLogs
  // @ts-expect-error Preparation producer identities remain internal.
  void client.computers.preparationLogStreams
}

function assertSessionOwnsInterruption(client: HelmrClient): void {
 const turn = client.sessions.get("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33").turn("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34")
 // @ts-expect-error Interruption belongs to the Session subtree.
 void turn.interrupt()
}
void assertSessionOwnsInterruption
