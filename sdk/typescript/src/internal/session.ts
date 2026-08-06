import type {
  JsonValue,
  Session,
  SessionFailure,
  SessionInputRecord,
  SessionInputSource,
  SessionOutputRecord,
  SessionStatus,
} from "../contract"
import { validateTaskId } from "../schema/task"
import { canonicalizeJsonValue } from "./jsoncanon"
import { resourceID } from "./id"
import { timestampString } from "./timestamp"

export function parseSession(value: unknown): Session {
  const input = objectValue(value, "Session response")
  const status = sessionStatus(input["status"])
  const failure = input["failure"] === undefined
    ? undefined
    : parseSessionFailure(input["failure"])
  const terminalFailure = status === "cancelled" || status === "failed"
  if (terminalFailure !== (failure !== undefined)) {
    throw new Error("Session response has an inconsistent failure projection")
  }
  if (
    failure !== undefined &&
    ((status === "cancelled") !== (failure.code === "cancelled"))
  ) {
    throw new Error("Session response failure code is inconsistent with status")
  }
  const actorId = requiredString(input, "actor_id", "Session response")
  validateTaskId(actorId)
  return Object.freeze({
    id: resourceID(input["id"], "Session response.id"),
    actorId,
    deploymentId: resourceID(input["deployment_id"], "Session response.deployment_id"),
    ...(input["key"] === undefined
      ? {}
      : { key: requiredString(input, "key", "Session response") }),
    status,
    createdAt: timestampString(input["created_at"], "Session response.created_at"),
    updatedAt: timestampString(input["updated_at"], "Session response.updated_at"),
    ...(input["current_run_id"] === undefined
      ? {}
      : { currentRunId: resourceID(input["current_run_id"], "Session response.current_run_id") }),
    ...(failure === undefined ? {} : { failure }),
  })
}

export function parseSessionInputRecord(value: unknown): SessionInputRecord {
  const input = objectValue(value, "Session input response")
  if (!Object.hasOwn(input, "data")) {
    throw new Error("Session input response.data is required")
  }
  const data = input["data"] as JsonValue
  canonicalizeJsonValue(data)
  const source = objectValue(input["source"], "Session input response.source")
  const type = source["type"]
  if (type !== "external" && type !== "run") {
    throw new Error("Session input response.source.type is invalid")
  }
  const parsedSource: SessionInputSource = type === "external"
    ? Object.freeze({ type })
    : Object.freeze({
        type,
        runId: resourceID(source["run_id"], "Session input response.source.run_id"),
      })
  return Object.freeze({
    id: resourceID(input["id"], "Session input response.id"),
    sequence: safeSequence(input["sequence"], "Session input response.sequence"),
    data,
    source: parsedSource,
    createdAt: timestampString(input["created_at"], "Session input response.created_at"),
  })
}

export function parseSessionOutputRecord(value: unknown): SessionOutputRecord {
  const input = objectValue(value, "Session output record")
  if (!Object.hasOwn(input, "data")) {
    throw new Error("Session output record.data is required")
  }
  const provenance = objectValue(input["provenance"], "Session output provenance")
  return Object.freeze({
    id: resourceID(input["id"], "Session output record.id"),
    sequence: safeSequence(input["sequence"], "Session output record.sequence"),
    data: input["data"],
    contentType: requiredString(input, "content_type", "Session output record"),
    createdAt: timestampString(input["created_at"], "Session output record.created_at"),
    provenance: Object.freeze({
      runId: resourceID(provenance["run_id"], "Session output provenance.run_id"),
      attemptNumber: safeSequence(
        provenance["attempt_number"],
        "Session output provenance.attempt_number",
      ),
      deploymentId: resourceID(
        provenance["deployment_id"],
        "Session output provenance.deployment_id",
      ),
    }),
  })
}

function parseSessionFailure(value: unknown): SessionFailure {
  const input = objectValue(value, "Session failure")
  const code = requiredString(input, "code", "Session failure")
  if (
    code !== "cancelled" &&
    code !== "no_progress" &&
    code !== "run_failed" &&
    code !== "run_expired" &&
    code !== "platform_failure"
  ) {
    throw new Error("Session failure.code is invalid")
  }
  const details = objectValue(input["details"], "Session failure.details")
  const runId = details["run_id"] === undefined
    ? undefined
    : resourceID(details["run_id"], "Session failure.details.run_id")
  return Object.freeze({
    code,
    message: requiredString(input, "message", "Session failure"),
    details: Object.freeze(runId === undefined ? {} : { runId }),
  })
}

function sessionStatus(value: unknown): SessionStatus {
  if (value !== "open" && value !== "closed" && value !== "cancelled" && value !== "failed") {
    throw new Error("Session response.status is invalid")
  }
  return value
}

function requiredString(
  value: Record<string, unknown>,
  field: string,
  label: string,
): string {
  const result = value[field]
  if (typeof result !== "string" || result.length === 0) {
    throw new Error(`${label}.${field} must be a non-empty string`)
  }
  return result
}

function objectValue(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`)
  }
  return value as Record<string, unknown>
}

function safeSequence(value: unknown, label: string): number {
  if (!Number.isSafeInteger(value) || (value as number) < 0) {
    throw new Error(`${label} must be a non-negative safe integer`)
  }
  return value as number
}
