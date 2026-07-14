import { cache, image, sandbox, source, task, tokens, type PayloadSchema } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

interface ApprovalDecision {
  readonly approved: boolean
}

interface HandoffMessage {
  readonly text: string
}

const approvalDecision: PayloadSchema<ApprovalDecision> = {
  "~standard": {
    version: 1,
    vendor: "human-in-the-loop.approval",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected object" }] }
      }
      const record = value as Record<string, unknown>
      if (typeof record.approved !== "boolean") {
        return { issues: [{ message: "expected boolean", path: ["approved"] }] }
      }
      return { value: { approved: record.approved } }
    },
  },
}

const handoffMessage: PayloadSchema<HandoffMessage> = {
  "~standard": {
    version: 1,
    vendor: "human-in-the-loop.message",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected object" }] }
      }
      const record = value as Record<string, unknown>
      if (typeof record.text !== "string" || record.text.length === 0) {
        return { issues: [{ message: "expected non-empty string", path: ["text"] }] }
      }
      return { value: { text: record.text } }
    },
  },
}

const base = image("human-in-the-loop")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/helmr-task/package.json", source.file("package.json"))
  .workdir("/opt/helmr-task")
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("human-in-the-loop-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("human-in-the-loop")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const handoff = task({
  id: "human-in-the-loop",
  sandbox: sbx,
  maxDuration: 900,
  run: async (ctx) => {
    const decisionToken = await tokens.create({ timeout: "15m" })
    const decision = await tokens.wait(decisionToken, {
      schema: approvalDecision,
      metadata: {
        prompt: "Continue and ask for a handoff note?",
        form: { version: 1, type: "approval" },
      },
    }).unwrap()
    if (!decision.approved) {
      return { approved: false }
    }

    const replyToken = await tokens.create({ timeout: "15m" })
    const reply = await tokens.wait(replyToken, {
      schema: handoffMessage,
      metadata: {
        prompt: "What should this run write to handoff.txt?",
        form: { version: 1, type: "message" },
      },
    }).unwrap()
    await writeFile("handoff.txt", `${reply.text}\n`)
    return {
      approved: true,
      path: "handoff.txt",
    }
  },
})
