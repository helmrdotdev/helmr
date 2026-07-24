import { expect, test } from "bun:test"

import { inspectDefinition } from "./internal"
import { schedules } from "./schedules"

test("scheduled payload accepts Control RFC3339Nano timestamps", async () => {
  const definition = schedules.task({
    id: "daily-report",
    cron: { pattern: "0 9 * * *", timezone: "UTC" },
    workspace: { key: "scheduler" },
    run: () => null,
  })
  const internal = inspectDefinition(definition)
  if (internal?.kind !== "task" || internal.payloadSchema === undefined) {
    throw new Error("scheduled Task payload schema is unavailable")
  }
  const result = await internal.payloadSchema["~standard"].validate({
    scheduledAt: "2026-07-24T03:00:00Z",
    lastScheduledAt: "2026-07-23T03:00:00.123456789Z",
    timezone: "UTC",
    scheduleId: "sch_aaaaaaaaaaaaaaaaaaaaaaaaaa",
    upcoming: ["2026-07-25T03:00:00Z"],
  })
  expect(result.issues).toBeUndefined()
  if ("value" in result) {
    expect(result.value.scheduledAt).toEqual(
      new Date("2026-07-24T03:00:00Z"),
    )
  }
})
