export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "succeeded"
  | "failed"
  | "cancelled"

declare const runOutputBrand: unique symbol

export interface RunHandle<TOutput = unknown> {
  readonly id: string
  readonly taskId: string
  readonly [runOutputBrand]?: TOutput
}

export type RunOutput<T> = T extends { readonly [runOutputBrand]?: infer TOutput } ? TOutput : unknown

export interface RunStateBooleans {
  readonly isQueued: boolean
  readonly isRunning: boolean
  readonly isWaiting: boolean
  readonly isTerminal: boolean
  readonly isSuccess: boolean
  readonly isFailed: boolean
  readonly isCancelled: boolean
}

export interface RunSnapshot<TOutput = unknown> extends RunStateBooleans {
  readonly id: string
  readonly taskId: string
  readonly status: RunStatus
  readonly exitCode: number | null
  readonly createdAt: string | null
  readonly updatedAt: string | null
  readonly pendingWaitpoint: PendingWaitpoint | null
  readonly output?: TOutput
}

export type RunSummary<TOutput = unknown> = RunSnapshot<TOutput>

export type PendingWaitpoint = PendingApprovalWaitpoint | PendingMessageWaitpoint | PendingTokenWaitpoint | PendingDelayWaitpoint

interface PendingWaitpointBase {
  readonly runId: string
  readonly waitpointId: string
  readonly timeout: number | null
  readonly requestedAt: string
  readonly request: unknown
  readonly displayText: string
}

export interface PendingApprovalWaitpoint extends PendingWaitpointBase {
  readonly kind: "approval"
  readonly message: string
}

export interface PendingMessageWaitpoint extends PendingWaitpointBase {
  readonly kind: "message"
  readonly prompt: string | null
}

export interface PendingTokenWaitpoint extends PendingWaitpointBase {
  readonly kind: "token"
}

export interface PendingDelayWaitpoint extends PendingWaitpointBase {
  readonly kind: "delay"
}

export interface WaitpointRef {
  readonly runId: string
  readonly waitpointId: string
}

export interface WaitpointApprovalOptions {
  readonly reason?: string
}

export interface WaitpointReplyOptions {
  readonly text: string
}

export interface RunWaitOptions {
  readonly timeoutMs?: number
  readonly intervalMs?: number
  readonly signal?: AbortSignal
}

export interface RetrieveRunOptions {
  readonly signal?: AbortSignal
}

export interface ListRunsOptions {
  readonly status?: RunStatus | "live" | "all"
  readonly limit?: number
  readonly projectId?: string
  readonly environmentId?: string
  readonly signal?: AbortSignal
}

export interface ListRunEventsOptions {
  readonly cursor?: number
  readonly pageSize?: number
  readonly signal?: AbortSignal
}

export interface SubscribeRunEventsOptions {
  readonly cursor?: number
  readonly signal?: AbortSignal
}

export type RunEvent =
  | {
      readonly type: "log"
      readonly run_id: string
      readonly stream: "stdout" | "stderr"
      readonly bytes: number
      readonly observed_seq: number
      readonly at: string
    }
  | {
      readonly type: "approval_request"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly message: string
      readonly timeout?: number
      readonly at: string
    }
  | {
      readonly type: "approval_decided"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly decision: "approved" | "denied"
      readonly reason?: string
      readonly at: string
    }
  | {
      readonly type: "message_request"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly prompt?: string
      readonly timeout?: number
      readonly at: string
    }
  | {
      readonly type: "message_received"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly text: string
      readonly at: string
    }
  | {
      readonly type: "emit"
      readonly run_id: string
      readonly event_type: string
      readonly content: unknown
      readonly at: string
    }
  | { readonly type: "task_complete"; readonly run_id: string; readonly exit_code: number; readonly at: string }
  | {
      readonly type: "run_failed"
      readonly run_id: string
      readonly failure_kind: string
      readonly detail?: unknown
      readonly at: string
    }
  | {
      readonly type: "run_timeout"
      readonly run_id: string
      readonly elapsed_secs: number
      readonly limit_secs: number
      readonly at: string
    }
  | { readonly type: "run_cancelled"; readonly run_id: string; readonly reason?: string; readonly at: string }

export interface RunEventRecord {
  readonly id: string
  readonly run_id?: string | null
  readonly kind: string
  readonly message: string
  readonly at: string
  readonly attributes: unknown
}

export interface RunEventRecordPage {
  readonly events: readonly RunEventRecord[]
  readonly cursor: number
  readonly next_cursor?: number | null
}

export interface RunEventPage {
  readonly events: readonly RunEvent[]
  readonly cursor: number
  readonly nextCursor: number | null
}

export interface LogSnapshot {
  readonly stdout: string
  readonly stderr: string
  readonly cursor: string
  readonly truncated: boolean
}

export type PendingWaitpointResponse =
  | {
      readonly kind: "approval"
      readonly waitpoint_id: string
      readonly message?: string | null
      readonly timeout?: number | null
      readonly request?: unknown
      readonly display_text?: string | null
      readonly requested_at: string
    }
  | {
      readonly kind: "message"
      readonly waitpoint_id: string
      readonly prompt?: string | null
      readonly timeout?: number | null
      readonly request?: unknown
      readonly display_text?: string | null
      readonly requested_at: string
    }
  | {
      readonly kind: "token" | "delay"
      readonly waitpoint_id: string
      readonly timeout?: number | null
      readonly request?: unknown
      readonly display_text?: string | null
      readonly requested_at: string
    }

export function runHandle<TOutput = unknown>(id: string, taskId: string): RunHandle<TOutput> {
  return { id, taskId }
}

export function runSnapshot<TOutput = unknown>(snapshot: {
  readonly id: string
  readonly taskId: string
  readonly status: string
  readonly exitCode?: number | null
  readonly createdAt?: string | null
  readonly updatedAt?: string | null
  readonly pendingWaitpoint?: PendingWaitpoint | null
  readonly output?: TOutput
}): RunSnapshot<TOutput> {
  const status = runStatus(snapshot.status)
  return {
    id: snapshot.id,
    taskId: snapshot.taskId,
    status,
    exitCode: snapshot.exitCode ?? null,
    createdAt: snapshot.createdAt ?? null,
    updatedAt: snapshot.updatedAt ?? null,
    pendingWaitpoint: snapshot.pendingWaitpoint ?? null,
    ...runStateBooleans(status),
    ...(snapshot.output === undefined ? {} : { output: snapshot.output }),
  }
}

export function pendingWaitpointFromResponse(
  runId: string,
  wait: PendingWaitpointResponse | null | undefined,
): PendingWaitpoint | null {
  if (wait === undefined || wait === null) return null
  if (wait.kind === "approval") {
    return {
      kind: "approval",
      runId,
      waitpointId: wait.waitpoint_id,
      message: wait.message ?? "",
      timeout: wait.timeout ?? null,
      request: wait.request ?? {},
      displayText: wait.display_text ?? wait.message ?? "",
      requestedAt: wait.requested_at,
    }
  }
  if (wait.kind === "message") {
    return {
    kind: "message",
    runId,
    waitpointId: wait.waitpoint_id,
    prompt: wait.prompt ?? null,
    timeout: wait.timeout ?? null,
    request: wait.request ?? {},
    displayText: wait.display_text ?? wait.prompt ?? "",
    requestedAt: wait.requested_at,
    }
  }
  return {
    kind: wait.kind,
    runId,
    waitpointId: wait.waitpoint_id,
    timeout: wait.timeout ?? null,
    request: wait.request ?? {},
    displayText: wait.display_text ?? "",
    requestedAt: wait.requested_at,
  }
}

export function isTerminalRunStatus(status: RunStatus): boolean {
  return status === "succeeded" || status === "failed" || status === "cancelled"
}

export function runId(value: string | RunHandle<unknown>): string {
  return typeof value === "string" ? value : value.id
}

export function runStateBooleans(status: RunStatus): RunStateBooleans {
  return {
    isQueued: status === "queued",
    isRunning: status === "running",
    isWaiting: status === "waiting",
    isTerminal: isTerminalRunStatus(status),
    isSuccess: status === "succeeded",
    isFailed: status === "failed",
    isCancelled: status === "cancelled",
  }
}

function runStatus(status: string): RunStatus {
  switch (status) {
    case "queued":
    case "running":
    case "waiting":
    case "succeeded":
    case "failed":
    case "cancelled":
      return status
    default:
      throw new Error(`unsupported run status ${JSON.stringify(status)}`)
  }
}
