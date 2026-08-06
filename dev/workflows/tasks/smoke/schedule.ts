import { image, schedules, sandbox } from "@helmr/sdk"

const base = image("helmr-schedule-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/sandbox")

export const scheduleSmokeWorkspace = sandbox({ id: "helmr-schedule-smoke" })
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const scheduleSmoke = schedules.task({
  id: "schedule-smoke",
  cron: {
    pattern: "* * * * *",
    timezone: "UTC",
  },
  workspace: { sandbox: scheduleSmokeWorkspace },
  maxDuration: "5m",
  retry: { enabled: false },
  run: async (input, ctx) => {
    return {
      scheduleId: input.scheduleId,
      scheduledAt: input.scheduledAt.toISOString(),
      lastScheduledAt: input.lastScheduledAt?.toISOString() ?? null,
      timezone: input.timezone,
      upcoming: input.upcoming.map((value) => value.toISOString()),
      runId: ctx.run.id,
      causeType: ctx.run.cause.type,
    }
  },
})
