import { cache, image, logger, sandbox, source, streams, task, type PayloadSchema } from "@helmr/sdk"

interface ReleaseApprovalPayload {
  readonly release: string
  readonly summary: string
  readonly approvalCorrelationId: string
  readonly risk?: string
  readonly stagingUrl?: string
  readonly productionUrl?: string
}

interface ApprovalDecision {
  readonly approved: boolean
  readonly actor?: string
  readonly channelId?: string
  readonly actionTs?: string
}

type SchemaIssue = { readonly message: string; readonly path?: readonly string[] }

const releasePayload: PayloadSchema<ReleaseApprovalPayload> = {
  "~standard": {
    version: 1,
    vendor: "slack-approval",
    validate(value) {
      if (value === null || typeof value !== "object") return { issues: [{ message: "expected object" }] }
      const record = value as Record<string, unknown>
      const issues = [
        stringIssue(record.release, "release"),
        stringIssue(record.summary, "summary"),
        stringIssue(record.approvalCorrelationId, "approvalCorrelationId"),
        optionalStringIssue(record.risk, "risk"),
        optionalStringIssue(record.stagingUrl, "stagingUrl"),
        optionalStringIssue(record.productionUrl, "productionUrl"),
      ].filter((issue): issue is SchemaIssue => issue !== null)

      if (issues.length > 0) return { issues }
      return {
        value: {
          release: record.release,
          summary: record.summary,
          approvalCorrelationId: record.approvalCorrelationId,
          ...(record.risk === undefined ? {} : { risk: record.risk }),
          ...(record.stagingUrl === undefined ? {} : { stagingUrl: record.stagingUrl }),
          ...(record.productionUrl === undefined ? {} : { productionUrl: record.productionUrl }),
        } as ReleaseApprovalPayload,
      }
    },
  },
}

const approvalDecision: PayloadSchema<ApprovalDecision> = {
  "~standard": {
    version: 1,
    vendor: "slack-approval.decision",
    validate(value) {
      if (value === null || typeof value !== "object") return { issues: [{ message: "expected object" }] }
      const record = value as Record<string, unknown>
      const issues = [
        booleanIssue(record.approved, "approved"),
        optionalStringIssue(record.actor, "actor"),
        optionalStringIssue(record.channelId, "channelId"),
        optionalStringIssue(record.actionTs, "actionTs"),
      ].filter((issue): issue is SchemaIssue => issue !== null)

      if (issues.length > 0) return { issues }
      return {
        value: {
          approved: record.approved,
          ...(record.actor === undefined ? {} : { actor: record.actor }),
          ...(record.channelId === undefined ? {} : { channelId: record.channelId }),
          ...(record.actionTs === undefined ? {} : { actionTs: record.actionTs }),
        } as ApprovalDecision,
      }
    },
  },
}

const approvalInput = streams.input("approval", { schema: approvalDecision })

const base = image("slack-approval")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/opt/helmr-task/package.json", source.file("package.json"))
  .workdir("/opt/helmr-task")
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("slack-approval-bun") }],
  })
  .workdir("/workspace")

const sbx = sandbox("slack-approval")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const releaseApproval = task({
  id: "slack-approval",
  sandbox: sbx,
  maxDuration: 15 * 60,
  payload: releasePayload,
  run: async (payload) => {
    logger.info("waiting for Slack approval input", { release: payload.release })
    const decision = await approvalInput.wait({
      correlationId: payload.approvalCorrelationId,
      timeout: "7d",
      tags: ["approval", "bridge:slack-approval", "medium:slack"],
      metadata: {
        release: payload.release,
        summary: payload.summary,
      },
    }).unwrap()

    return {
      release: payload.release,
      approved: decision.approved,
      actor: decision.actor ?? null,
      channelId: decision.channelId ?? null,
      actionTs: decision.actionTs ?? null,
      completedAt: new Date().toISOString(),
    }
  },
})

function stringIssue(value: unknown, key: string): SchemaIssue | null {
  return typeof value === "string" && value.length > 0 ? null : { message: "expected non-empty string", path: [key] }
}

function optionalStringIssue(value: unknown, key: string): SchemaIssue | null {
  return value === undefined || typeof value === "string" ? null : { message: "expected string", path: [key] }
}

function booleanIssue(value: unknown, key: string): SchemaIssue | null {
  return typeof value === "boolean" ? null : { message: "expected boolean", path: [key] }
}
