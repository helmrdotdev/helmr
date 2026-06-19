import { cache, image, sandbox, source, task, wait, type PayloadSchema } from "@helmr/sdk"

interface ReleaseApprovalPayload {
  readonly release: string
  readonly summary: string
  readonly risk?: string
  readonly stagingUrl?: string
  readonly productionUrl?: string
}

interface ApprovalDecision {
  readonly approved: boolean
  readonly actor?: string
}

type SchemaIssue = { readonly message: string; readonly path?: readonly string[] }

const releasePayload: PayloadSchema<ReleaseApprovalPayload> = {
  "~standard": {
    version: 1,
    vendor: "resend-email-approval",
    validate(value) {
      if (value === null || typeof value !== "object") return { issues: [{ message: "expected object" }] }
      const record = value as Record<string, unknown>
      const issues = [
        stringIssue(record.release, "release"),
        stringIssue(record.summary, "summary"),
        optionalStringIssue(record.risk, "risk"),
        optionalStringIssue(record.stagingUrl, "stagingUrl"),
        optionalStringIssue(record.productionUrl, "productionUrl"),
      ].filter((issue): issue is SchemaIssue => issue !== null)

      if (issues.length > 0) return { issues }
      return {
        value: {
          release: record.release,
          summary: record.summary,
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
    vendor: "resend-email-approval.decision",
    validate(value) {
      if (value === null || typeof value !== "object") return { issues: [{ message: "expected object" }] }
      const record = value as Record<string, unknown>
      const issues = [
        booleanIssue(record.approved, "approved"),
        optionalStringIssue(record.actor, "actor"),
      ].filter((issue): issue is SchemaIssue => issue !== null)

      if (issues.length > 0) return { issues }
      return {
        value: {
          approved: record.approved,
          ...(record.actor === undefined ? {} : { actor: record.actor }),
        } as ApprovalDecision,
      }
    },
  },
}

const base = image("resend-email-approval")
  .from("node:24-bookworm-slim")
  .workdir("/workspace")
  .run(["npm", "install", "-g", "bun@1.3.10"])
  .copy("/workspace/package.json", source.file("package.json"))
  .run(["bun", "install"], {
    cache: [{ mountPath: "/root/.bun/install/cache", cache: cache("resend-email-approval-bun") }],
  })

const sbx = sandbox("resend-email-approval")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const releaseApproval = task({
  id: "resend-email-approval",
  sandbox: sbx,
  maxDuration: 86400,
  payload: releasePayload,
  run: async (payload, ctx) => {
    const token = await wait.createToken({ timeout: 86400 })
    const decision = await wait.forToken(token, {
      schema: approvalDecision,
      timeout: 86400,
      tags: ["approval", "channel:email"],
      metadata: {
        release: payload.release,
        summary: payload.summary,
        risk: payload.risk ?? "not specified",
        stagingUrl: payload.stagingUrl ?? null,
        productionUrl: payload.productionUrl ?? null,
      },
    }).unwrap()

    return {
      release: payload.release,
      approved: decision.approved,
      actor: decision.actor ?? null,
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
