import { verify, assert, assertEqual, errorCode, readTelemetry } from "../../support/context"
import type { WorkspaceRef, WorkspaceExecResult } from "@helmr/sdk"
import type { runtimeSmoke } from "../runtime/task"
await verify("workspace-exec", async ({ client, marker, objects, cleanup }) => {
  const workspaceKey = `runtime-smoke-${marker}`
  const workspaceCreateOptions = {
    key: workspaceKey,
    idempotencyKey: `workspace:create:${marker}`,
  } as const
  const created = await client.sandboxes.createWorkspace(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  objects.workspace_ids.push(created.id)
  cleanup(() =>
    created.delete(
      { idempotencyKey: `workspace:delete:${marker}` },
      { signal: AbortSignal.timeout(30_000) },
    ),
  )
  const replayedCreate = await client.sandboxes.createWorkspace(
    "helmr-runtime-smoke",
    workspaceCreateOptions,
  )
  assertEqual(replayedCreate.id, created.id, "workspace create replay changed the ID")
  let unexpectedConflictWorkspace: WorkspaceRef | undefined
  try {
    unexpectedConflictWorkspace = await client.sandboxes.createWorkspace("helmr-runtime-smoke", {
      ...workspaceCreateOptions,
      key: `${workspaceKey}-conflict`,
    })
  } catch (error) {
    assertEqual(
      errorCode(error),
      "idempotency_conflict",
      "divergent Workspace create returned the wrong error",
    )
  }
  if (unexpectedConflictWorkspace !== undefined) {
    await unexpectedConflictWorkspace.delete({
      idempotencyKey: `workspace:conflict-cleanup:${marker}`,
    })
    throw new Error("divergent Workspace create reused an idempotency key")
  }

  const matches = await client.workspaces.list({ key: workspaceKey })
  assertEqual(matches.items.length, 1, "workspace key lookup was not exact")
  const workspace = matches.items[0]!
  const byKey = client.workspaces.ref(workspace.id)
  assertEqual(workspace.id, created.id, "workspace key resolved to a different Workspace")
  assertEqual(workspace.sandboxId, "helmr-runtime-smoke", "workspace Sandbox ID mismatch")

  const markerDirectory = "sandbox-smoke/nested"
  const markerPath = `${markerDirectory}/marker.txt`
  const largeFileKiB = 256
  const largePath = `${markerDirectory}/large-${largeFileKiB}k.txt`
  const stdin = `stdin:${marker}\n`
  const execOptions = {
    command: [
      "sh",
      "-ceu",
      [
        `mkdir -p ${markerDirectory}`,
        `node -e 'require("node:fs").writeFileSync("${largePath}", "x".repeat(${largeFileKiB} * 1024))'`,
        "IFS= read -r line",
        `printf 'marker=%s\\nstdin=%s\\n' "$SMOKE_MARKER" "$line" > ${markerPath}`,
        'printf \'stdout:%s:%s\\n\' "$SMOKE_MARKER" "$line"',
        "printf 'stderr:%s\\n' \"$SMOKE_MARKER\" >&2",
        "exit 7",
      ].join("; "),
    ],
    cwd: "/workspace",
    env: { SMOKE_MARKER: marker },
    stdin: new TextEncoder().encode(stdin),
    timeout: "2m",
    idempotencyKey: `workspace:exec:${marker}`,
  } as const
  const initialExec = await byKey.exec(execOptions, {
    signal: AbortSignal.timeout(15 * 60_000),
  })
  assertExec(initialExec, marker)

  const replayExec = await byKey.exec(execOptions, {
    signal: AbortSignal.timeout(15 * 60_000),
  })
  assertExec(replayExec, marker)
  assertEqual(
    encodeExec(replayExec),
    encodeExec(initialExec),
    "BasicExec idempotency replay changed the result",
  )

  const taskMarker = `${marker}-task`
  const started = await client.tasks.start<typeof runtimeSmoke>(
    "runtime-smoke",
    {
      payload: {
        scenario: "workspace-basic-exec-client-smoke",
        marker: taskMarker,
        expectedWorkspaceMarker: marker,
        expectedEnvironment: "unknown",
        exerciseToken: false,
        tokenTimeout: 120,
        largeFileKiB,
      },
      workspace: byKey,
      idempotencyKey: `task:start:${marker}`,
      tags: ["smoke", "workspace-basic-exec"],
      metadata: { marker: marker },
    },
    { signal: AbortSignal.timeout(30_000) },
  )
  objects.run_ids.push(started.id)
  const taskOutput = await client.runs
    .wait(started, {
      signal: AbortSignal.timeout(20 * 60 * 1_000),
    })
    .unwrap()
  const taskLogs = await readTelemetry(() => client.runs.logs(started.id, { limit: 100 }))
  const taskEvents = await readTelemetry(() => client.runs.events(started.id, { limit: 100 }))
  for (const level of ["debug", "info", "warn", "error"] as const) {
    assert(
      taskLogs.items.some(
        (record) =>
          record.kind === "structured" &&
          record.level === level &&
          record.attributes["marker"] === taskMarker,
      ),
      `Task logs did not include the ${level} structured logger probe`,
    )
  }
  assert(
    taskEvents.items.some((event) => event.runId === started.id),
    "Task events did not include the completed Run",
  )
  const postTaskExec = await byKey.exec(
    {
      command: ["cat", markerPath],
      cwd: "/workspace",
      idempotencyKey: `workspace:verify-task:${marker}`,
    },
    {
      signal: AbortSignal.timeout(15 * 60_000),
    },
  )
  assertEqual(postTaskExec.exitCode, 0, "Task Workspace verification command failed")
  const taskFile = new TextDecoder().decode(postTaskExec.stdout)
  assert(taskFile.includes(`marker=${taskMarker}`), "Task did not advance the Workspace head")

  const deleted = await byKey.delete({
    idempotencyKey: `workspace:delete:${marker}`,
  })
  assertEqual(deleted.workspaceId, created.id, "Workspace delete receipt changed the ID")
  const deleteReplay = await created.delete({
    idempotencyKey: `workspace:delete:${marker}`,
  })
  assertEqual(deleteReplay.workspaceId, created.id, "Workspace delete replay changed the ID")

  return {
    workspaceCreateReplay: true,
    workspaceDeleteReplay: true,
    executionReplay: true,
    taskOutput,
    telemetry: true,
  }
})
function assertExec(result: WorkspaceExecResult, marker: string): void {
  const stdout = new TextDecoder().decode(result.stdout)
  const stderr = new TextDecoder().decode(result.stderr)
  assertEqual(result.exitCode, 7, "BasicExec did not preserve its nonzero exit code")
  assert(stdout.includes(`stdout:${marker}:stdin:${marker}`), "BasicExec stdout mismatch")
  assert(stderr.includes(`stderr:${marker}`), "BasicExec stderr mismatch")
}

function encodeExec(result: WorkspaceExecResult): string {
  return JSON.stringify({
    exitCode: result.exitCode,
    stdout: Buffer.from(result.stdout).toString("base64"),
    stderr: Buffer.from(result.stderr).toString("base64"),
  })
}
