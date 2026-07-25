import { createHash } from "node:crypto"
import {
  HelmrClient,
  type JsonValue,
  type RunEventRecord,
  type RunLogRecord,
  type WorkspaceExecResult,
  type WorkspaceFileEntry,
} from "@helmr/sdk"
import type {
  runtimeSmoke,
  runtimeSmokeWorkspace,
} from "../../workflows/tasks/smoke/runtime"
import { assert, assertEqual } from "./assert"
import { readConfig } from "./config"

type Evidence = {
  readonly marker: string
  readonly workspaceId: string
  readonly workspaceKey: string
  readonly initialExec: ExecEvidence
  readonly replayExec: ExecEvidence
  readonly committedFile: {
    readonly path: string
    readonly sizeBytes: number
    readonly digest: string
  }
  readonly taskRunId: string
  readonly taskOutput: JsonValue
  readonly taskLogs: readonly RunLogRecord[]
  readonly taskEvents: readonly RunEventRecord[]
  readonly tokenId: string
  readonly secretDelivery: "covered" | "skipped"
  readonly deletedWorkspaceId: string
}

type ExecEvidence = {
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
}

const config = readConfig()
const client = new HelmrClient({ url: config.apiUrl, apiKey: config.apiKey })
const evidence = await runSmoke()
console.log(JSON.stringify(evidence, null, 2))

async function runSmoke(): Promise<Evidence> {
  const workspaceKey = `runtime-smoke-${config.marker}`
  const workspaceCreateOptions = {
    key: workspaceKey,
    ...(config.secretName === undefined
      ? {}
      : {
          secrets: [{
            name: config.secretName,
            env: "HELMR_SMOKE_SECRET_VALUE",
          }],
        }),
    idempotencyKey: `workspace:create:${config.marker}`,
  } as const
  const created = await client.workspaces.create<typeof runtimeSmokeWorkspace>(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  const replayedCreate = await client.workspaces.create<typeof runtimeSmokeWorkspace>(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  assertEqual(replayedCreate.id, created.id, "workspace create replay changed the ID")

  const byKey = client.workspaces.ref<typeof runtimeSmokeWorkspace>(
    "helmr-runtime-smoke",
    { key: workspaceKey },
  )
  const snapshot = await byKey.retrieve()
  assertEqual(snapshot.id, created.id, "workspace key resolved to a different Workspace")
  assertEqual(snapshot.declaredId, "helmr-runtime-smoke", "workspace declared ID mismatch")

  const markerPath = "workspace-smoke/nested/marker.txt"
  const stdin = `stdin:${config.marker}\n`
  const execOptions = {
    command: [
      "sh",
      "-ceu",
      [
        "mkdir -p workspace-smoke/nested",
        "IFS= read -r line",
        "printf 'marker=%s\\nstdin=%s\\n' \"$SMOKE_MARKER\" \"$line\" > workspace-smoke/nested/marker.txt",
        "printf 'stdout:%s:%s\\n' \"$SMOKE_MARKER\" \"$line\"",
        "printf 'stderr:%s\\n' \"$SMOKE_MARKER\" >&2",
        config.secretName === undefined
          ? "true"
          : "test -n \"$HELMR_SMOKE_SECRET_VALUE\" && printf 'secret:present\\n'",
        "exit 7",
      ].join("; "),
    ],
    cwd: "/workspace",
    env: { SMOKE_MARKER: config.marker },
    stdin: new TextEncoder().encode(stdin),
    timeout: "2m",
    idempotencyKey: `workspace:exec:${config.marker}`,
  } as const
  const initialExec = await byKey.exec(execOptions)
  assertExec(initialExec, config.marker)

  const replayExec = await byKey.exec(execOptions)
  assertExec(replayExec, config.marker)
  assertEqual(
    encodeExec(replayExec),
    encodeExec(initialExec),
    "BasicExec idempotency replay changed the result",
  )

  const fileBytes = await byKey.files.read(markerPath)
  const fileText = new TextDecoder().decode(fileBytes)
  assert(fileText.includes(`marker=${config.marker}`), "committed file missed the exec marker")
  assert(fileText.includes(`stdin=stdin:${config.marker}`), "committed file missed stdin")
  const fileStat = await byKey.files.stat(markerPath)
  assertFile(fileStat, markerPath, fileBytes.byteLength)
  const listing = await byKey.files.list("workspace-smoke/nested", { limit: 100 })
  assert(
    listing.items.some((entry) => entry.path === markerPath),
    "committed file was absent from the directory listing",
  )

  const taskMarker = `${config.marker}-task`
  const started = await client.tasks.start<typeof runtimeSmoke>(
    "runtime-smoke",
    {
      payload: {
        scenario: "workspace-basic-exec-client-smoke",
        marker: taskMarker,
        expectedWorkspaceMarker: config.marker,
        expectedEnvironment: "unknown",
        exerciseToken: false,
        tokenTimeout: 120,
        largeFileKiB: 256,
      },
      workspace: byKey,
      idempotencyKey: `task:start:${config.marker}`,
      tags: ["smoke", "workspace-basic-exec"],
      metadata: { marker: config.marker },
    },
  )
  const taskOutput = await client.runs.wait<typeof runtimeSmoke>(started.id, {
    signal: AbortSignal.timeout(20 * 60 * 1_000),
  }).unwrap()
  const taskLogs = await readTelemetry(() => client.runs.logs(started.id, { limit: 100 }))
  const taskEvents = await readTelemetry(() => client.runs.events(started.id, { limit: 100 }))
  for (const level of ["debug", "info", "warn", "error"] as const) {
    assert(
      taskLogs.items.some((record) =>
        record.kind === "structured" &&
        record.level === level &&
        record.attributes["marker"] === taskMarker
      ),
      `Task logs did not include the ${level} structured logger probe`,
    )
  }
  assert(
    taskEvents.items.some((event) => event.runId === started.id),
    "Task events did not include the completed Run",
  )
  const taskFile = new TextDecoder().decode(await byKey.files.read(markerPath))
  assert(taskFile.includes(`marker=${taskMarker}`), "Task did not advance the Workspace head")

  const token = await client.tokens.create({
    timeout: "10m",
    tags: ["smoke", "workspace-basic-exec"],
    metadata: { marker: config.marker },
    idempotencyKey: `token:create:${config.marker}`,
  })
  const tokenSnapshot = await client.tokens.retrieve(token.id)
  assertEqual(tokenSnapshot.id, token.id, "external Token retrieve changed the ID")
  const canceled = await client.tokens.cancel(token.id, {
    idempotencyKey: `token:cancel:${config.marker}`,
  })
  assertEqual(canceled.status, "canceled", "external Token was not canceled")

  const deleted = await byKey.delete({
    idempotencyKey: `workspace:delete:${config.marker}`,
  })
  assertEqual(deleted.workspaceId, created.id, "Workspace delete receipt changed the ID")
  const deleteReplay = await created.delete({
    idempotencyKey: `workspace:delete:${config.marker}`,
  })
  assertEqual(deleteReplay.workspaceId, created.id, "Workspace delete replay changed the ID")

  return {
    marker: config.marker,
    workspaceId: created.id,
    workspaceKey,
    initialExec: execEvidence(initialExec),
    replayExec: execEvidence(replayExec),
    committedFile: {
      path: markerPath,
      sizeBytes: fileBytes.byteLength,
      digest: createHash("sha256").update(fileBytes).digest("hex"),
    },
    taskRunId: started.id,
    taskOutput,
    taskLogs: taskLogs.items,
    taskEvents: taskEvents.items,
    tokenId: token.id,
    secretDelivery: config.secretName === undefined ? "skipped" : "covered",
    deletedWorkspaceId: deleted.workspaceId,
  }
}

async function readTelemetry<T>(
  read: () => Promise<Readonly<{ items: readonly T[] }>>,
): Promise<Readonly<{ items: readonly T[] }>> {
  const deadline = Date.now() + 60_000
  for (;;) {
    try {
      return await read()
    } catch (error) {
      const code = errorCode(error)
      if (code !== "telemetry_lagging" || Date.now() >= deadline) throw error
      await new Promise((resolve) => setTimeout(resolve, 500))
    }
  }
}

function errorCode(error: unknown): string | undefined {
  return error !== null && typeof error === "object" && "code" in error &&
      typeof error.code === "string"
    ? error.code
    : undefined
}

function assertExec(result: WorkspaceExecResult, marker: string): void {
  const stdout = new TextDecoder().decode(result.stdout)
  const stderr = new TextDecoder().decode(result.stderr)
  assertEqual(result.exitCode, 7, "BasicExec did not preserve its nonzero exit code")
  assert(stdout.includes(`stdout:${marker}:stdin:${marker}`), "BasicExec stdout mismatch")
  assert(stderr.includes(`stderr:${marker}`), "BasicExec stderr mismatch")
  if (config.secretName !== undefined) {
    assert(stdout.includes("secret:present"), "BasicExec did not receive its Workspace Secret")
  }
}

function assertFile(entry: WorkspaceFileEntry, path: string, sizeBytes: number): void {
  assertEqual(entry.path, path, "Workspace file stat path mismatch")
  assertEqual(entry.kind, "file", "Workspace file stat kind mismatch")
  if (entry.kind === "file") {
    assertEqual(entry.sizeBytes, sizeBytes, "Workspace file stat size mismatch")
  }
}

function execEvidence(result: WorkspaceExecResult): ExecEvidence {
  return {
    exitCode: result.exitCode,
    stdout: new TextDecoder().decode(result.stdout),
    stderr: new TextDecoder().decode(result.stderr),
  }
}

function encodeExec(result: WorkspaceExecResult): string {
  return JSON.stringify({
    exitCode: result.exitCode,
    stdout: Buffer.from(result.stdout).toString("base64"),
    stderr: Buffer.from(result.stderr).toString("base64"),
  })
}
