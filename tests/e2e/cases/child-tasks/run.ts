import type { childTaskSmoke } from "./task"
import { cleanupChildren } from "./cleanup"
import { verify, assert, assertEqual, deadline, waitRun } from "../../support/context"
await verify("child-tasks", async ({ client, marker, objects, computer, cleanup }) => {
  const target = await computer("helmr-child-task-target-smoke", "target")
  for (const mode of [
    "call-success",
    "same-sandbox-call",
    "call-failure",
    "start-detached",
  ] as const) {
    const caller = await computer("helmr-child-task-caller-smoke", mode)
    const run = await client.tasks.start<typeof childTaskSmoke>(
      "child-task-smoke",
      {
        computer: caller,
        payload: { mode, marker, childComputerId: target.id },
        idempotencyKey: `child:${mode}:${marker}`,
      },
      { signal: deadline(30_000) },
    )
    objects.run_ids.push(run.id)
    cleanup(() => cleanupChildren(client, run.id, [caller.id, target.id], objects.run_ids))
    const output = await client.runs.wait(run, { signal: deadline(20 * 60_000) }).unwrap()
    assert(output !== null && typeof output === "object" && !Array.isArray(output))
    assertEqual(output.mode, mode, "Child mode changed")
    assertEqual(output.marker, marker, "Child marker changed")
    assert(typeof output.childRunId === "string", "Missing child Run")
    objects.run_ids.push(output.childRunId)
    if (mode === "same-sandbox-call")
      assertEqual(output.sameComputerMarkerObserved, true, "Parent lost child writes")
    if (mode === "call-failure") assert(output.childFailure !== null, "Child failure was lost")
    if (mode === "start-detached") await waitRun(client, output.childRunId, ["succeeded"])
  }
  return { sameComputer: true, differentComputer: true, childFailure: true, detached: true }
})
