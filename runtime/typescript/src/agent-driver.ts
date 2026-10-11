import { abortableDelay } from "../../../sdk/typescript/src/internal/abort"
import { normalizeAnswer, type Question, type AskResponse } from "../../../sdk/typescript/src/question"
import { agentProto } from "@helmr/proto"
import { normalizeContent, type Content } from "../../../sdk/typescript/src/content"
import type { Json, TurnOutcome } from "../../../sdk/typescript/src/agent"
import type { SessionDriver } from "./agent-session"
import { NativeContinuationLost, type SessionNativeRegistry } from "./agent-native"
import { AgentChannel, OperationNotSent } from "./agent-channel"

const method = agentProto.Operation_Method
function object(value: Json): Record<string, Json> {
  if (value === null || Array.isArray(value) || typeof value !== "object") throw new Error("Invalid guest response object")
  return value as Record<string, Json>
}
function string(value: Json | undefined): string {
  if (typeof value !== "string" || !value) throw new Error("Invalid guest response identifier")
  return value
}
function outcome(value: Json): TurnOutcome {
  const result = object(value)
  const status = result['status']
  if (status !== "completed" && status !== "failed" && status !== "interrupted" && status !== "cancelled") throw new Error("Invalid guest Turn outcome")
  if (status !== "completed" && result['response'] !== undefined) throw new Error("Unsuccessful Turn cannot publish a response")
  const error = result['error'] === undefined ? undefined : object(result['error'])
  return { status, ...(result['payloadExpiredAt'] === undefined ? {} : { payloadExpiredAt: string(result['payloadExpiredAt']) }), ...(result['result'] === undefined ? {} : { result: result['result'] }), ...(result['response'] === undefined ? {} : { response: normalizeContent(result['response'], false) }), ...(error === undefined ? {} : { error: { code: string(error['code']), ...(error['message'] === undefined && result['payloadExpiredAt'] !== undefined ? {} : { message: typeof error['message'] === "string" ? error['message'] : string(undefined) }) } }) }
}

// This driver carries logical identity only. The guest must authorize every
// operation against its currently attached Computer and Session authority.
export class GuestSessionDriver implements SessionDriver {
  private readonly channel: AgentChannel

  private readonly native: SessionNativeRegistry | undefined
  constructor(channel: AgentChannel, native?: SessionNativeRegistry) { this.channel = channel; this.native = native }
  private call(operation: agentProto.Operation_Method, turnId: string, payload: Json = null, signal?: AbortSignal): Promise<Json> {
    return this.channel.operation(operation, payload, { turnId, ...(signal === undefined ? {} : { signal }) })
  }
  async registerMessages(turnId: string): Promise<void> { await this.call(method.REGISTER_MESSAGES, turnId) }
  async closeProcessing(turnId: string): Promise<void> { await this.call(method.CLOSE_PROCESSING, turnId) }
  async ask(turnId: string, creationId: string, question: Question, _signal: AbortSignal) {
    // Registration must reconcile even after local interruption so its durable
    // question can be withdrawn. An abort is never evidence of non-admission.
    const registration = object(await this.call(method.ASK, turnId, { creationId, question: question as unknown as Json }))
    const askId = string(registration['id'])
    const wait = new AbortController()
    const answer = (async (): Promise<AskResponse> => {
      for (;;) {
        const value = await this.call(method.WAIT_ASK, turnId, { askId }, wait.signal)
        if (value === null) { await abortableDelay(250, wait.signal); continue }
        const response = object(value), responder = object(response['respondedBy']!)
        if (!Object.hasOwn(response, 'answer') || (responder['kind'] !== 'user' && responder['kind'] !== 'api_key')) throw new Error("Invalid ask response")
        return { answer: normalizeAnswer(question, response['answer']), respondedBy: { kind: responder['kind'], id: string(responder['id']) } }
      }
    })()
    void answer.catch(() => {})
    let withdrawal: Promise<void> | undefined
    return { answer, withdraw: () => withdrawal ??= (async () => {
      await this.call(method.WITHDRAW_ASK, turnId, { askId })
      wait.abort(new Error("Question withdrawn"))
    })() }
  }
  async output(turnId: string, outputId: string, value: Content) {
    const receipt = object(await this.call(method.OUTPUT, turnId, { outputId, value: value as unknown as Json }))
    const sequence = receipt['sequence']
    if (typeof sequence !== "number" || !Number.isSafeInteger(sequence) || sequence <= 0) throw new Error("Invalid output receipt sequence")
    return { sequence }
  }
  async respond(turnId: string, responseId: string, value: Content): Promise<void> { await this.call(method.RESPOND, turnId, { responseId, value: value as unknown as Json }) }
  async convergeNative(turnId: string, disposition: "returned" | "failed", _signal: AbortSignal): Promise<void> {
    // Failure convergence must execute even when the Turn signal is aborted.
    await this.native?.join(turnId, disposition, _signal.reason)
    const evidence = await this.native?.evidence() ?? { scopes: [], reusable: true }
    await this.call(method.CONVERGE_NATIVE, turnId, { disposition, scopes: evidence.scopes.map(scope => ({ ...scope })) })
    if (!evidence.reusable) throw new NativeContinuationLost()
  }
  async finalize(turnId: string, result: Json): Promise<TurnOutcome> {
    try { return outcome(await this.call(method.FINALIZE, turnId, { result })) }
    catch (error) {
      if (!(error instanceof OperationNotSent) || error.code !== "request_too_large") throw error
      // Native work already converged. This deterministic, never-admitted
      // result is a contract failure, not uncertain success publication.
      return this.fail(turnId, { code: "result_too_large", message: "Turn result exceeds the transport limit" })
    }
  }
  async fail(turnId: string, error: { code: string; message: string }): Promise<TurnOutcome> { return outcome(await this.call(method.FAIL, turnId, { error })) }
  async hold(reason: string): Promise<void> { await this.channel.operation(method.HOLD, { reason }) }

}
