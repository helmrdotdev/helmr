import type { PayloadSchema } from "@helmr/sdk"

export interface ApprovalDecision {
  readonly approved: boolean
  readonly workspaceText?: string
}

export interface MessageReply {
  readonly text: string
}

type SchemaIssue = { readonly message: string; readonly path?: readonly string[] }

export const approvalDecision: PayloadSchema<ApprovalDecision> = {
  "~standard": {
    version: 1,
    vendor: "full-rootfs-runtime.approval",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected object" }] }
      }
      const record = value as Record<string, unknown>
      const issues: SchemaIssue[] = []
      if (typeof record.approved !== "boolean") {
        issues.push({ message: "expected boolean", path: ["approved"] })
      }
      if (record.workspaceText !== undefined && typeof record.workspaceText !== "string") {
        issues.push({ message: "expected string", path: ["workspaceText"] })
      }
      if (issues.length > 0) return { issues }
      return {
        value: {
          approved: record.approved as boolean,
          ...(record.workspaceText === undefined ? {} : { workspaceText: record.workspaceText as string }),
        },
      }
    },
  },
}

export const messageReply: PayloadSchema<MessageReply> = {
  "~standard": {
    version: 1,
    vendor: "full-rootfs-runtime.message",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected object" }] }
      }
      const record = value as Record<string, unknown>
      if (typeof record.text !== "string") {
        return { issues: [{ message: "expected string", path: ["text"] }] }
      }
      return { value: { text: record.text } }
    },
  },
}
