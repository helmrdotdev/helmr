import { image, sandbox, task } from "@helmr/sdk"

const base = image("human-in-the-loop").from("debian:trixie-slim")

const sbx = sandbox("human-in-the-loop")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const handoff = task({
  id: "human-in-the-loop",
  sandbox: sbx,
  maxDuration: 900,
  run: async (_payload: unknown, ctx) => {
    const decision = await ctx.wait.approval("Continue and ask for a handoff note?")
    if (!decision.approved) {
      return { approved: false, approvedBy: decision.approvedBy }
    }

    const reply = await ctx.wait.message("What should this run write to handoff.txt?")
    await Bun.write("handoff.txt", `${reply.text}\n`)
    return {
      approved: true,
      approvedBy: decision.approvedBy,
      messageFrom: reply.sentBy,
      path: "handoff.txt",
    }
  },
})
