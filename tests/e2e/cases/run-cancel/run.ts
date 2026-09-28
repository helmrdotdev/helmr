import { verify, assert, assertEqual, waitRun, deadline } from "../../support/context"
import type { timerSmoke } from "../timer/task"
await verify("run-cancel", async ({ client, marker, objects, cleanup }) => {
  const timerComputer = await client.sandboxes.createComputer(
    "helmr-timer-smoke",
    {
      key: `cancel-${marker}`,
      idempotencyKey: `timer-computer:create:${marker}`,
    },
    { signal: AbortSignal.timeout(10 * 60_000) },
  )
  objects.computer_ids.push(timerComputer.id)
  cleanup(() =>
    timerComputer.delete({ idempotencyKey: `delete:${marker}` }, { signal: deadline(30_000) }),
  )
  const cancellable = await client.tasks.start<typeof timerSmoke>(
    "timer-smoke",
    {
      payload: { marker: `cancel-${marker}`, waitFor: "2m" },
      computer: timerComputer,
      idempotencyKey: `timer:start:${marker}`,
    },
    { signal: AbortSignal.timeout(30_000) },
  )
  objects.run_ids.push(cancellable.id)
  await waitRun(client, cancellable.id, ["waiting", "running"])
  await client.runs.cancel(
    cancellable.id,
    {},
    {
      signal: AbortSignal.timeout(30_000),
    },
  )
  const cancelled = await waitRun(client, cancellable.id, ["cancelled"])
  assertEqual(cancelled.status, "cancelled", "Run cancel did not reach terminal state")
  const runs = await client.runs.list(
    { status: "cancelled", limit: 100 },
    { signal: AbortSignal.timeout(30_000) },
  )
  assert(
    runs.items.some((run) => run.id === cancellable.id),
    "Run list omitted the cancelled Run",
  )

  return { cancelled: true, listed: true }
})
