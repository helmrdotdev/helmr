import { image, schedules, workspace, workspaces } from "@helmr/sdk"

const base = image("helmr-schedule-smoke")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")

export const scheduleSmokeWorkspace = workspace("helmr-schedule-smoke")
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

export const scheduleSmoke = schedules.task({
  id: "schedule-smoke",
  cron: {
    pattern: "* * * * *",
    timezone: "UTC",
  },
  workspace: workspaces.ref({ key: "release-gate" }),
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
