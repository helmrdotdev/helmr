export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired"

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
  readonly version?: string | null
  readonly deploymentVersion?: string | null
  readonly apiVersion?: string | null
  readonly sdkVersion?: string | null
  readonly cliVersion?: string | null
  readonly attemptNumber: number | null
  readonly status: RunStatus
  readonly metadata: Record<string, unknown>
  readonly exitCode: number | null
  readonly createdAt: string | null
  readonly updatedAt: string | null
  readonly pendingWaitpoint: PendingWaitpoint | null
  readonly output?: TOutput
}

export type RunSummary<TOutput = unknown> = RunSnapshot<TOutput>

export interface PendingWaitpoint {
  readonly runId: string
  readonly id: string
  readonly kind: string
  readonly status: "pending" | "completed" | "timed_out" | "cancelled" | "failed" | string
  readonly timeout: number | null
  readonly createdAt: string
  readonly params: unknown
  readonly metadata: Record<string, unknown>
  readonly tags: readonly string[]
}

export interface WaitpointRef {
  readonly runId: string
  readonly waitpointId: string
  readonly kind?: never
}

export interface RunWaitpointOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly timeoutMs?: number
  readonly signal?: AbortSignal
}

export interface CancelRunOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly reason?: string
  readonly force?: boolean
  readonly idempotencyKey?: string
  readonly signal?: AbortSignal
}

export interface RetrieveRunOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly signal?: AbortSignal
}

export interface ListRunsOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly status?: RunStatus | "live" | "all"
  readonly limit?: number
  readonly signal?: AbortSignal
}

export interface ListRunEventsOptions {
  readonly projectId?: string
  readonly environmentId?: string
  readonly cursor?: number
  readonly pageSize?: number
  readonly signal?: AbortSignal
}

export interface SubscribeRunEventsOptions {
  readonly projectId?: string
  readonly environmentId?: string
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
      readonly type: "waitpoint"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly kind: string
      readonly params: unknown
      readonly metadata: Record<string, unknown>
      readonly tags: readonly string[]
      readonly timeout?: number
      readonly at: string
    }
  | {
      readonly type: "waitpoint_completed"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly kind: string
      readonly payload: unknown
      readonly at: string
    }
  | {
      readonly type: "waitpoint_timed_out"
      readonly run_id: string
      readonly waitpoint_id: string
      readonly kind: string
      readonly at: string
    }
  | { readonly type: "task_result"; readonly run_id: string; readonly exit_code: number; readonly at: string }
  | {
      readonly type: "run_failed"
      readonly run_id: string
      readonly failure_kind: string
      readonly detail?: unknown
      readonly at: string
    }
  | { readonly type: "run_cancelled"; readonly run_id: string; readonly reason?: string; readonly at: string }
  | { readonly type: "run_expired"; readonly run_id: string; readonly ttl?: string; readonly message?: string; readonly at: string }

export interface RunEventRecord {
  readonly id: string
  readonly run_id?: string | null
  readonly run_lease_id?: string | null
  readonly attempt_number?: number | null
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

export interface PendingWaitpointResponse {
  readonly id: string
  readonly kind?: string
  readonly status?: string | null
  readonly timeout?: number | null
  readonly params?: unknown
  readonly metadata?: Record<string, unknown> | null
  readonly tags?: readonly string[] | null
  readonly created_at: string
}

export function runHandle<TOutput = unknown>(id: string, taskId: string): RunHandle<TOutput> {
  return { id, taskId }
}

export function runSnapshot<TOutput = unknown>(snapshot: {
  readonly id: string
  readonly taskId: string
  readonly version?: string | null
  readonly deploymentVersion?: string | null
  readonly apiVersion?: string | null
  readonly sdkVersion?: string | null
  readonly cliVersion?: string | null
  readonly attemptNumber?: number | null
  readonly status: string
  readonly metadata?: Record<string, unknown> | null
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
    metadata: snapshot.metadata ?? {},
    exitCode: snapshot.exitCode ?? null,
    createdAt: snapshot.createdAt ?? null,
    updatedAt: snapshot.updatedAt ?? null,
    pendingWaitpoint: snapshot.pendingWaitpoint ?? null,
    ...(snapshot.version === undefined && snapshot.deploymentVersion === undefined
      ? {}
      : { version: snapshot.version ?? snapshot.deploymentVersion ?? null }),
    ...(snapshot.deploymentVersion === undefined && snapshot.version === undefined
      ? {}
      : { deploymentVersion: snapshot.deploymentVersion ?? snapshot.version ?? null }),
    ...(snapshot.apiVersion === undefined ? {} : { apiVersion: snapshot.apiVersion }),
    ...(snapshot.sdkVersion === undefined ? {} : { sdkVersion: snapshot.sdkVersion }),
    ...(snapshot.cliVersion === undefined ? {} : { cliVersion: snapshot.cliVersion }),
    attemptNumber: snapshot.attemptNumber ?? null,
    ...runStateBooleans(status),
    ...(snapshot.output === undefined ? {} : { output: snapshot.output }),
  }
}

export function pendingWaitpointFromResponse(
  runId: string,
  wait: PendingWaitpointResponse | null | undefined,
): PendingWaitpoint | null {
  if (wait === undefined || wait === null) return null
  return {
    runId,
    id: wait.id,
    kind: wait.kind ?? "token",
    status: wait.status ?? "pending",
    timeout: wait.timeout ?? null,
    params: wait.params === undefined ? {} : wait.params,
    metadata: wait.metadata ?? {},
    tags: wait.tags ?? [],
    createdAt: wait.created_at,
  }
}

export function isTerminalRunStatus(status: RunStatus): boolean {
  return status === "succeeded" || status === "failed" || status === "cancelled" || status === "expired"
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
    case "expired":
      return status
    default:
      throw new Error(`unsupported run status ${JSON.stringify(status)}`)
  }
}
