import type { AnswerControl } from "./question"
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

export type Duration = string

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
export interface SessionHold {
  readonly id: string
  readonly sessionId: string
  readonly scope: "local" | "subtree"
  readonly reason: string
  readonly createdAt: string
}
export interface Session {
  readonly id: string
  readonly agentId: string
  readonly deploymentId: string
  readonly computerId: string
  readonly rootSessionId: string
  readonly slackChannelId?: string
  readonly parentSessionId: string | null
  readonly requesterSessionId: string | null
  readonly initialTurn: Readonly<{ id: string; status: TurnStatus }> | null
  readonly key?: string
  readonly status: SessionStatus
  readonly createdAt: string
  readonly holds: readonly SessionHold[]
}
export type TurnStatus = "queued" | "running" | "finalizing" | "completed" | "failed" | "interrupted" | "cancelled"
export interface TurnState {
  readonly payloadExpiredAt?: string
  readonly response?: Content
  readonly error?: Readonly<{ code: string; message?: string }>
  readonly id: string
  readonly sessionId: string
  readonly sequence: number
  readonly input?: InputContent
  readonly status: TurnStatus
  readonly startedAt?: string
  readonly terminalAt?: string
  readonly completionSaveId?: string
  readonly result?: JsonValue
}
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
export type SessionAdmissionReceipt =
  | Readonly<{ id: string; kind: "enqueued"; turnId: string }>
  | Readonly<{
      id: string
      kind: "messaged"
      turnId: string
      messageId: string
    }>
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

export type SessionEventKind =
  | "message.admitted"
  | "message.started"
  | "message.delivered"
  | "message.rejected"
  | "slack.delivery_unavailable"
  | "ask.created"
  | "ask.responded"
  | "ask.cancelled"
  | "turn.output"
  | "turn.queued"
  | "turn.running"
  | "turn.finalizing"
  | "turn.completed"
  | "turn.failed"
  | "turn.interrupted"
  | "turn.cancelled"
  | "session.cancelled"
  | "session.closed"
  | "session.process_stopped"
  | "session.process_failed"
  | "session.deadline"
  | "session.interrupt"
  | "session.resume"
  | "session.close"
  | "session.cancel"
export interface SessionEvent {
  readonly sessionId: string
  readonly turnId: string | null
  readonly sequence: number
  readonly createdAt: string
  readonly kind: SessionEventKind
  readonly data: JsonValue
}
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

export interface AskState {
  readonly id: string
  readonly sessionId: string
  readonly turnId: string
  readonly status: "pending" | "responded" | "cancelled"
  readonly prompt?: Content
  readonly answerControl?: AnswerControl
  readonly answer?: JsonValue
  readonly payloadExpired?: true
  readonly createdAt: string
  readonly respondedAt?: string
  readonly cancelledAt?: string
  readonly respondedByUserId?: string
  readonly respondedByApiKeyId?: string
}
export type AskResponseReceipt = Omit<AskState, "prompt" | "answerControl">
export interface AskQuery { readonly cursor?: string; readonly limit?: number }
export interface AskPage { readonly asks: readonly AskState[]; readonly nextCursor?: string }
export interface TurnAsks {
  list(query?: AskQuery, options?: RequestOptions): Promise<AskPage>
  get(askId: string, options?: RequestOptions): Promise<AskState>
  respond(askId: string, request: { readonly answer: JsonValue; readonly responseId: string }, options?: RequestOptions): Promise<AskResponseReceipt>
}

export interface TurnOutcome<O extends JsonValue = JsonValue> {
  readonly status: "completed" | "failed" | "interrupted" | "cancelled"
  readonly result?: O
  readonly response?: Content
  readonly payloadExpiredAt?: string
  readonly error?: Readonly<{ code: string; message?: string }>
}
export interface TurnWaitOptions {
  readonly timeout?: string | number
  readonly signal?: AbortSignal
}
export type TimedTurnWaitResult<O extends JsonValue = JsonValue> =
  | Readonly<{ status: "timeout" }>
  | Readonly<{ status: "settled"; outcome: TurnOutcome<O> }>

export interface TurnRef {
  wait(options: TurnWaitOptions & { readonly timeout: string | number }): Promise<TimedTurnWaitResult>
  wait(options?: TurnWaitOptions & { readonly timeout?: undefined }): Promise<TurnOutcome>
  wait(options: TurnWaitOptions): Promise<TurnOutcome | TimedTurnWaitResult>
  readonly asks: TurnAsks
  readonly id: string
  readonly sessionId: string
  send(
    data: InputContent,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<MessageReceipt>
  retrieve(options?: RequestOptions): Promise<TurnState>
}
export type SessionSendResult =
  | Readonly<{ kind: "enqueued"; turn: TurnRef }>
  | Readonly<{
      kind: "messaged"
      turn: TurnRef
      message: MessageReceipt
    }>
export interface SessionRef {
  readonly id: string
  send(
    data: InputContent,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<SessionSendResult>
  enqueue(
    input: InputContent,
    request?: SessionOperationOptions,
    options?: RequestOptions,
  ): Promise<TurnRef>
  turn(id: string): TurnRef
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
  retrieve(options?: RequestOptions): Promise<Session>
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
