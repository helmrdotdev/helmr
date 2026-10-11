import { agent, computer, image, triggers } from "@helmr/sdk"

export const scheduleSmokeComputer = computer({
  id: "helmr-schedule-smoke",
  image: image("helmr-schedule-smoke").from("node:24-bookworm-slim").workdir("/sandbox"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const scheduleSmoke = agent({
  id: "schedule-smoke",
  computer: scheduleSmokeComputer,
  triggers: [triggers.cron("each-minute", "* * * * *", { timezone: "UTC", input: [{ type: "text", text: JSON.stringify({ scheduled: true }) }] })],
  maxTurnDuration: "5m",
  turn: async (turn, ctx) => ({
    input: turn.input,
    source: turn.source.kind,
    sessionId: ctx.session.id,
    turnId: turn.id,
  }),
})
