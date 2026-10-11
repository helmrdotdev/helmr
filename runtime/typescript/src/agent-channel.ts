import { randomUUID } from "node:crypto"
import { create, toBinary } from "@bufbuild/protobuf"
import { agentProto } from "@helmr/proto"
import { requireJson, type Json } from "../../../sdk/typescript/src/agent"

export const MAX_AGENT_FRAME_BYTES = 16 * 1024 * 1024

interface Pending {
  resolve(value: Json): void
  reject(error: unknown): void
  removeAbort(): void
}

export class OperationError extends Error {
  readonly code: string
  constructor(code: string, message: string) { super(message); this.name = "OperationError"; this.code = code }
}
// Only the local encoder creates this error; no guest response can claim that a
// mutation was never sent. Settlement may safely choose a failure instead.
export class OperationNotSent extends OperationError {}

// The local pipe and this object survive coherent RAM continuation. The guest
// retains original operation frames across upstream reattachment; a broken local
// pipe is process loss, not permission to regenerate uncertain mutations.
export class AgentChannel {
  private readonly identity: agentProto.SessionIdentity
  private readonly pending = new Map<string, Pending>()
  private writes: Promise<void> = Promise.resolve()
  private failure: Error | undefined
  private readonly write: (frame: Uint8Array) => Promise<void>
  private readonly onFailure: (error: Error) => void
  constructor(identity: agentProto.SessionIdentity, write: (frame: Uint8Array) => Promise<void>, onFailure: (error: Error) => void = () => {}) {
    if (!identity.sessionId || identity.processEpoch <= 0n) throw new Error("Invalid Session process identity")
    this.identity = create(agentProto.SessionIdentitySchema, identity)
    this.write = write
    this.onFailure = onFailure
  }

  matches(identity: agentProto.SessionIdentity | undefined): boolean {
    return identity?.sessionId === this.identity.sessionId && identity.processEpoch === this.identity.processEpoch
  }

  operation(method: agentProto.Operation_Method, payload: Json, options: { turnId?: string; signal?: AbortSignal } = {}): Promise<Json> {
    if (this.failure) return Promise.reject(this.failure)
    try { options.signal?.throwIfAborted(); requireJson(payload) } catch (error) { return Promise.reject(error) }
    if (method === agentProto.Operation_Method.UNSPECIFIED) return Promise.reject(new Error("Operation method is required"))
    const requestId = randomUUID()
    // Encode before yielding: mutations of a caller-owned object cannot change
    // the payload that the guest retains for receipt reconciliation.
    let payloadJson: Uint8Array
    try { payloadJson = Buffer.from(JSON.stringify(payload)) }
    catch (error) {
      // Escaping can exceed the engine's string/buffer capacity before a frame
      // exists. No request identity has been registered or written at this point.
      return Promise.reject(error instanceof RangeError
        ? new OperationNotSent("request_too_large", "Agent payload exceeds the encoder limit")
        : error)
    }
    const operation = create(agentProto.OperationSchema, { requestId, method, payloadJson, ...(options.turnId === undefined ? {} : { turnId: options.turnId }) })
    const result = new Promise<Json>((resolve, reject) => {
      const abort = () => {
        const pending = this.pending.get(requestId)
        if (!pending) return
        this.pending.delete(requestId)
        pending.removeAbort()
        reject(options.signal!.reason)
        // Cancellation removes only the observation. The guest still reconciles
        // an admitted mutation under its original request identity.
        void this.event({ case: "cancelOperation", value: create(agentProto.OperationCancelSchema, { requestId }) }).catch(() => {})
      }
      this.pending.set(requestId, { resolve, reject, removeAbort: () => options.signal?.removeEventListener("abort", abort) })
      options.signal?.addEventListener("abort", abort, { once: true })
    })
    void this.event({ case: "operation", value: operation }).catch(error => {
      const pending = this.pending.get(requestId)
      if (!pending) return
      this.pending.delete(requestId)
      pending.removeAbort()
      pending.reject(error)
    })
    return result
  }

  receive(identity: agentProto.SessionIdentity | undefined, result: agentProto.OperationResult): void {
    if (!this.matches(identity)) throw new Error("Operation result belongs to a different Session process")
    const pending = this.pending.get(result.requestId)
    if (!pending) return
    this.pending.delete(result.requestId)
    pending.removeAbort()
    switch (result.outcome.case) {
      case "valueJson":
        try { const value: unknown = JSON.parse(Buffer.from(result.outcome.value).toString("utf8")); requireJson(value); pending.resolve(value) }
        catch { pending.reject(new OperationError("invalid_response", "Guest operation returned invalid JSON")) }
        return
      case "error": pending.reject(new OperationError(result.outcome.value.code, result.outcome.value.message)); return
      default: pending.reject(new OperationError("invalid_response", "Guest operation has no outcome"))
    }
  }

  event(event: agentProto.ProgramEvent["event"]): Promise<void> {
    if (this.failure) return Promise.reject(this.failure)
    let body: Uint8Array
    try { body = toBinary(agentProto.ProgramEventSchema, create(agentProto.ProgramEventSchema, { identity: this.identity, event })) }
    catch (error) { this.close(error); return Promise.reject(this.failure) }
    if (body.length > MAX_AGENT_FRAME_BYTES) return Promise.reject(new OperationNotSent("request_too_large", "Agent frame exceeds the transport limit"))
    const frame = new Uint8Array(body.length + 4)
    new DataView(frame.buffer).setUint32(0, body.length)
    frame.set(body, 4)
    const next = this.writes.then(() => {
      if (this.failure) throw this.failure
      return this.write(frame)
    })
    this.writes = next.catch(error => { this.close(error) })
    return next
  }

  close(reason: unknown): void {
    if (this.failure) return
    this.failure = reason instanceof Error ? reason : new Error(String(reason))
    for (const pending of this.pending.values()) { pending.removeAbort(); pending.reject(this.failure) }
    this.pending.clear()
    this.onFailure(this.failure)
  }
}
