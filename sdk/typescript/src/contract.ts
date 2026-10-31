import type { Duration, Retention } from "./duration"
import type { NormalizedQuestion, AnswerSubmission } from "./question"
import type { Content, InputContent } from "./content"
import type { RequestOptions } from "./request"

export type JsonValue =
  | null
  | boolean
  | number
  | string
  | readonly JsonValue[]
  | { readonly [key: string]: JsonValue }

export interface CursorPage<T> {
  readonly items: readonly T[]
  readonly nextCursor?: string
}

export type { Duration, Retention } from "./duration"

export interface HelmrError extends Error {
  readonly code: string
  readonly requestId?: string
}

export interface APIError extends HelmrError {
  readonly details?: Readonly<Record<string, JsonValue>>
}

export interface SessionOperationOptions {
  readonly idempotencyKey?: string
}

export type SessionStatus = "open" | "closing" | "closed" | "cancelled"
/** A Computer rollback that lost acknowledged file changes. `holdId` is null for a notice to a Session that keeps running. */
export interface RecoveryGap {
 readonly holdId:string|null; readonly computerId:string; readonly leaseEpoch:number; readonly recoverySaveId:string|null; readonly restoredAt:string|null; readonly affectedTurnIds:readonly string[]
}
/** Why a Session is held. Only an interrupt also holds the Session's descendants. */
export type SessionHoldKind = "interrupt" | "process_failed" | "recovery_gap" | "storage_pressure"
/**
 * A hold in effect for a Session. `id` and `sessionId` are null for a hold on
 * an ancestor the caller cannot read. `canRelease` reports whether the caller
 * may resume it; resume it on `sessionId`.
 */
export interface SessionHold {
  readonly recoveryGap?: RecoveryGap
  readonly kind: SessionHoldKind
  readonly id: string | null
  readonly sessionId: string | null
  readonly createdAt: string
  readonly canRelease: boolean
}
export interface ProcessStop {
  readonly processEpoch: number
  readonly turnId: string | null
  readonly state: "requested" | "stopping" | "stopped"
  readonly deadline: string
}
export interface SessionActivity {
  readonly process: Readonly<{epoch: number; status: "starting" | "ready" | "stopping" | "lost" | "stopped"; applied: boolean; reconstructionRequired: boolean; stop: ProcessStop | null}> | null
}
export interface Session {
  readonly agent: string
  readonly template: string
  readonly activity: SessionActivity
  readonly suspension: "manual" | "after_turn"
  readonly closeAfterIdle: Retention
  readonly idleSince: string | null
  readonly id: string
  readonly deploymentId: string
  readonly computerId: string | null
  /** Null when the caller cannot read the root Session. */
  readonly rootSessionId: string | null
  readonly slackChannelId?: string
  readonly parentSessionId: string | null
  readonly requesterSessionId: string | null
  readonly initialTurn: Readonly<{ id: string; status: TurnStatus }> | null
  readonly key?: string
  readonly status: SessionStatus
  readonly createdAt: string
  readonly holds: readonly SessionHold[]
}
export type TurnStatus = "queued" | "running" | "completed" | "failed" | "interrupted" | "cancelled"
type TurnIdentity = Readonly<{
  id: string; sessionId: string; sequence: number; stop?: ProcessStop; startedAt?: string
}>
type RetainedTurn = Readonly<{ input: JsonValue; payloadExpiredAt?: never }>
type ExpiredTurn = Readonly<{ input?: never; payloadExpiredAt: string; result?: never; response?: never }>
export type TurnState<O = JsonValue> = TurnIdentity & (
  | (RetainedTurn & Readonly<{ status: "queued" | "running"; terminalAt?: never; result?: never; response?: never; error?: never }>)
  | (Readonly<{ terminalAt: string }> & (
    | (Readonly<{ status: "completed"; error?: never }> & ((RetainedTurn & Readonly<{ result: O; response?: Content }>) | ExpiredTurn))
    | (Readonly<{ status: "failed"; result?: never; response?: never }> & ((RetainedTurn & Readonly<{ error?: Readonly<{ code: string; message: string }> }>) | (ExpiredTurn & Readonly<{ error?: Readonly<{ code: string; message?: never }> }>)))
    | (Readonly<{ status: "interrupted" | "cancelled"; result?: never; response?: never; error?: never }> & (RetainedTurn | ExpiredTurn))
  ))
)
export interface TurnListQuery {
  readonly cursor?: string
  readonly limit?: number
}

export interface SessionCloseReceipt {
  readonly id: string
  readonly sessionId: string
  readonly status: "accepted"
}
export interface SessionCancelReceipt {
  readonly id: string
  readonly sessionId: string
  readonly status: "accepted"
}
export interface SessionInterruptReceipt {
  readonly id: string
  readonly sessionId: string
  readonly holdId: string
  readonly status: "accepted"
}
export interface SessionResumeReceipt {
  readonly id: string
  readonly sessionId: string
  readonly holdId: string
  readonly status: "accepted"
}
export interface SessionResumeRequest extends SessionOperationOptions {
  readonly holdId: string
}
export interface SessionMessageReceipt {
  readonly id: string
  readonly turnId: string
  readonly messageId: string
  readonly status: "accepted"
}
export interface MessageReceipt {
  readonly id: string
  readonly status: "accepted"
}

type EmptyEventData = Readonly<Record<string, never>>
export interface ComputerLossInterruption {
  readonly reason: "computer_execution_lost"
  readonly computerId: string
  readonly leaseEpoch: number
  readonly recoveryGap: boolean
}
/** The Turn reached its maximum duration. Its process stops and queued Turns continue. */
export interface TurnDeadlineInterruption {
  readonly reason: "turn_deadline_elapsed"
}
/** Counted harness failures reported by `session.process_failed`. */
export type ProcessFailureReason = "process_failed" | "stop_deadline_elapsed" | "execution_lost"
type ControlEventData = Readonly<{ controlId: string; holdId?: string }>
type MessageEventData<S extends string, D extends string = S> = Readonly<{ message_id: string; status: S; delivery: D; reason?: string }>
interface SessionEventData {
  "message.admitted": MessageEventData<"admitted">
  "message.started": MessageEventData<"started">
  "message.delivered": MessageEventData<"delivered">
  "message.rejected": MessageEventData<"rejected", "not_delivered" | "uncertain">
  "slack.delivery_unavailable": Readonly<{ reason: "route_unavailable"; source_seq: number }>
  "ask.created": Readonly<{ ask_id: string; question: NormalizedQuestion }>
  "ask.answered": Readonly<{ ask_id: string; answer: JsonValue }> & (
    | Readonly<{ answered_by_user_id: string; answered_by_api_key_id?: never }>
    | Readonly<{ answered_by_user_id?: never; answered_by_api_key_id: string }>
  )
  "ask.cancelled": Readonly<{ ask_id: string }>
  "turn.output": Content
  "turn.queued": EmptyEventData
  "turn.running": EmptyEventData
  "turn.completed": EmptyEventData
  "turn.failed": Readonly<{ code?: string }>
  "turn.interrupt": ControlEventData
  "turn.interrupted": (EmptyEventData & Readonly<{ reason?: never }>) | ComputerLossInterruption | TurnDeadlineInterruption
  "turn.cancelled": EmptyEventData
  "session.cancelled": EmptyEventData
  "session.closed": EmptyEventData
  "session.process_stopped": EmptyEventData
  "session.storage_pressure": Readonly<{computerId:string;leaseEpoch:number;holdId:string;state:"paused"|"resolved"|"stopped"}>
  "session.recovery_gap": RecoveryGap
  /** `holdId` is set once consecutive failures hold the Session. */
  "session.process_failed": Readonly<{ processEpoch: number; reason: ProcessFailureReason; holdId: string | null }>
  "session.interrupt": ControlEventData
  "session.resume": ControlEventData
  "session.close": ControlEventData | Readonly<{ reason: "close_after_idle" }>
  "session.cancel": ControlEventData
}
export type SessionEventKind = keyof SessionEventData
export type SessionEvent = Readonly<{
  sessionId: string
  turnId: string | null
  sequence: number
  createdAt: string
}> & { [K in SessionEventKind]: Readonly<{ kind: K; data: SessionEventData[K] }> }[SessionEventKind]
export interface SessionEventQuery {
  readonly after?: number
  readonly limit?: number
}
export interface SessionEventPage {
  readonly records: readonly SessionEvent[]
  readonly nextAfter: number
  readonly hasMore: boolean
  readonly retainedAfter: number
}

type AskIdentity = Readonly<{ id: string; sessionId: string; turnId: string; createdAt: string }>
type AnsweredAttribution = Readonly<{ answeredAt: string }> & (
  | Readonly<{ answeredByUserId: string; answeredByApiKeyId?: never }>
  | Readonly<{ answeredByUserId?: never; answeredByApiKeyId: string }>
)
type RetainedQuestion = Readonly<{ question: NormalizedQuestion; payloadExpired?: never }>
type ExpiredQuestion = Readonly<{ question?: never; answer?: never; payloadExpired: true }>
export type AskState = AskIdentity & (
  | (Readonly<{ status: "pending"; answer?: never; answeredAt?: never; cancelledAt?: never; answeredByUserId?: never; answeredByApiKeyId?: never }> & RetainedQuestion)
  | (Readonly<{ status: "answered"; cancelledAt?: never }> & AnsweredAttribution & ((RetainedQuestion & Readonly<{ answer: JsonValue }>) | ExpiredQuestion))
  | (Readonly<{ status: "cancelled"; cancelledAt: string; answer?: never; answeredAt?: never; answeredByUserId?: never; answeredByApiKeyId?: never }> & (RetainedQuestion | ExpiredQuestion))
)
export type AskAnswerReceipt = AskIdentity & Readonly<{ status: "answered" }> & AnsweredAttribution & (
  | Readonly<{ answer: JsonValue; payloadExpired?: never }>
  | Readonly<{ answer?: never; payloadExpired: true }>
)
export interface AskQuery { readonly cursor?: string; readonly limit?: number }
export type AskPage = CursorPage<AskState>
export interface TurnAsks {
  list(query?: AskQuery, options?: RequestOptions): Promise<AskPage>
  get(askId: string, options?: RequestOptions): Promise<AskState>
  answer(askId: string, request: { readonly answer: AnswerSubmission; readonly answerId: string }, options?: RequestOptions): Promise<AskAnswerReceipt>
}

export type TurnOutcome<O = JsonValue> =
  | (Readonly<{ status: "completed"; error?: never }> & (
    | Readonly<{ result: O; response?: Content; payloadExpiredAt?: never }>
    | Readonly<{ result?: never; response?: never; payloadExpiredAt: string }>
  ))
  | (Readonly<{ status: "failed"; result?: never; response?: never }> & (
    | Readonly<{ error?: Readonly<{ code: string; message: string }>; payloadExpiredAt?: never }>
    | Readonly<{ error?: Readonly<{ code: string; message?: never }>; payloadExpiredAt: string }>
  ))
  | Readonly<{ status: "interrupted" | "cancelled"; result?: never; response?: never; error?: never; payloadExpiredAt?: string }>
export interface TurnWaitOptions {
  readonly timeout?: Duration
  readonly signal?: AbortSignal
}
export type TimedTurnWaitResult<O = JsonValue> =
  | Readonly<{ status: "timeout" }>
  | Readonly<{ status: "settled"; outcome: TurnOutcome<O> }>

export interface TurnInterruptReceipt {
  readonly id: string
  readonly sessionId: string
  readonly turnId: string
  readonly status: "accepted"
}
export interface TurnRef<O = JsonValue> {
  /**
   * Requests interruption of this Turn. The receipt and an `interrupted` outcome
   * record the stop decision; runtime cleanup can still be in progress.
   * Successor Turns stay gated by Session stop and hold state. Inspect
   * `session.get()` activity and holds to observe that progress.
   */
  interrupt(request?: SessionOperationOptions, options?: RequestOptions): Promise<TurnInterruptReceipt>
  /** Waits for a terminal Turn outcome, which does not join Session cleanup. */
  wait(options: TurnWaitOptions & { readonly timeout: import("./duration").Duration }): Promise<TimedTurnWaitResult<O>>
  wait(options?: TurnWaitOptions & { readonly timeout?: undefined }): Promise<TurnOutcome<O>>
  wait(options: TurnWaitOptions): Promise<TurnOutcome<O> | TimedTurnWaitResult<O>>
  readonly asks: TurnAsks
  readonly id: string
  readonly sessionId: string
  send(
    message: InputContent,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<MessageReceipt>
  get(options?: RequestOptions): Promise<TurnState<O>>
}
export interface SessionRef<I = JsonValue, O = JsonValue> {
  readonly id: string
  enqueue(
    input: I,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<TurnRef<O>>
  interrupt(request?: SessionOperationOptions, options?: RequestOptions): Promise<SessionInterruptReceipt>
  readonly turns: Readonly<{
    ref(id: string): TurnRef<O>
    list(query?: TurnListQuery, options?: RequestOptions): Promise<CursorPage<TurnState<O>>>
  }>
  readonly events: Readonly<{
    list(
      query?: SessionEventQuery,
      options?: RequestOptions,
    ): Promise<SessionEventPage>
    stream(
      query?: SessionEventQuery,
      options?: RequestOptions,
    ): AsyncIterableIterator<SessionEvent>
  }>
  get(options?: RequestOptions): Promise<Session>
  close(
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<SessionCloseReceipt>
  cancel(
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<SessionCancelReceipt>
  resume(
    request: SessionResumeRequest,
    options?: RequestOptions,
  ): Promise<SessionResumeReceipt>
}
