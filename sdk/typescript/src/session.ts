import type {
  JsonValue,
  SessionOperationOptions,
  SessionRef,
  TurnRef,
} from "./contract"
import { resourceID } from "./internal/id"
import { currentRuntimeOperations } from "./internal/runtime"

const rejectedBrand = Symbol.for("helmr.sdk.MessageRejected")

/** A known rejection before uncertain application effects. */
export class MessageRejected extends Error {
  readonly details?: JsonValue
  constructor(message: string, details?: JsonValue) {
    super(message)
    this.name = "MessageRejected"
    if (details !== undefined) this.details = details
    Object.defineProperty(this, rejectedBrand, { value: true })
  }
  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === MessageRejected &&
      typeof value === "object" &&
      value !== null &&
      rejectedBrand in value
    )
  }
}

export function sessionOperationOptions(
  request: SessionOperationOptions = {},
): SessionOperationOptions & { readonly idempotencyKey: string } {
  return { idempotencyKey: request.idempotencyKey ?? crypto.randomUUID() }
}

export function createRuntimeSessionRef(id: string): SessionRef {
  const sessionId = resourceID(id, "Session ID")
  const turn = (id: string): TurnRef => {
    const turnId = resourceID(id, "Turn ID")
    return Object.freeze({
      id: turnId,
      sessionId,
      async send(data, request, options) {
        const receipt = await currentRuntimeOperations().sessionTurnSend(
          sessionId,
          turnId,
          data,
          sessionOperationOptions(request),
          options?.signal,
        )
        return Object.freeze({ id: receipt.messageId, status: receipt.status })
      },
      retrieve(options) {
        return currentRuntimeOperations().sessionTurnRetrieve(
          sessionId,
          turnId,
          options?.signal,
        )
      },
      interrupt(request, options) {
        return currentRuntimeOperations().sessionTurnInterrupt(
          sessionId,
          turnId,
          sessionOperationOptions(request),
          options?.signal,
        )
      },
    } satisfies TurnRef)
  }
  return Object.freeze({
    id: sessionId,
    turn,
    async send(data, request, options) {
      const receipt = await currentRuntimeOperations().sessionSend(
        sessionId,
        data,
        sessionOperationOptions(request),
        options?.signal,
      )
      return receipt.kind === "enqueued"
        ? Object.freeze({ kind: receipt.kind, turn: turn(receipt.turnId) })
        : Object.freeze({
            kind: receipt.kind,
            turn: turn(receipt.turnId),
            message: Object.freeze({
              id: receipt.messageId,
              status: "accepted" as const,
            }),
          })
    },
    async enqueue(data, request, options) {
      const receipt = await currentRuntimeOperations().sessionEnqueue(
        sessionId,
        data,
        sessionOperationOptions(request),
        options?.signal,
      )
      return turn(receipt.turnId)
    },
    events: Object.freeze({
      list(query, options) {
        return currentRuntimeOperations().sessionEvents(
          sessionId,
          query,
          options?.signal,
        )
      },
    }),
    retrieve(options) {
      return currentRuntimeOperations().sessionRetrieve(
        sessionId,
        options?.signal,
      )
    },
    close(request, options) {
      return currentRuntimeOperations().sessionClose(
        sessionId,
        sessionOperationOptions(request),
        options?.signal,
      )
    },
    cancel(request, options) {
      return currentRuntimeOperations().sessionCancel(
        sessionId,
        sessionOperationOptions(request),
        options?.signal,
      )
    },
    resume(request, options) {
      return currentRuntimeOperations().sessionResume(
        sessionId,
        { ...request, ...sessionOperationOptions(request) },
        options?.signal,
      )
    },
  } satisfies SessionRef)
}

export const sessions = Object.freeze({ ref: createRuntimeSessionRef })
