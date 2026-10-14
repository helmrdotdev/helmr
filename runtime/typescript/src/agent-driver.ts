import { parseTurnOutcome as outcome } from "../../../sdk/typescript/src/internal/turn-outcome"
import { abortableDelay } from "../../../sdk/typescript/src/internal/abort"
import { normalizeAnswer, type NormalizedQuestion as Question, type AskAnswer } from "../../../sdk/typescript/src/question"
import { agentProto } from "@helmr/proto"
import type { Content } from "../../../sdk/typescript/src/content"
import type { TurnOutcome } from "../../../sdk/typescript/src/agent"
import type { JsonValue } from "../../../sdk/typescript/src/contract"
import type { SessionDriver } from "./agent-session"
import { AgentChannel, OperationNotSent } from "./agent-channel"

const method = agentProto.Operation_Method
function object(value: JsonValue): Record<string, JsonValue> {
  if (value === null || Array.isArray(value) || typeof value !== "object") throw new Error("Invalid guest response object")
  return value as Record<string, JsonValue>
}
function string(value: JsonValue | undefined): string {
  if (typeof value !== "string" || !value) throw new Error("Invalid guest response identifier")
  return value
}
// This driver carries logical identity only. The guest must authorize every
// operation against its currently attached Computer and Session authority.
export class GuestSessionDriver implements SessionDriver {
  private readonly channel: AgentChannel

  constructor(channel: AgentChannel) { this.channel = channel }
  private call(operation: agentProto.Operation_Method, turnId: string, payload: JsonValue = null, signal?: AbortSignal): Promise<JsonValue> {
    return this.channel.operation(operation, payload, { turnId, ...(signal === undefined ? {} : { signal }) })
  }
  async registerMessages(turnId: string): Promise<void> { await this.call(method.REGISTER_MESSAGES, turnId) }
  async closeProcessing(turnId: string): Promise<void> { await this.call(method.CLOSE_PROCESSING, turnId) }
  async ask(turnId: string, creationId: string, question: Question, _signal: AbortSignal) {
    // Registration must reconcile even after local interruption so its durable
    // question can be withdrawn. An abort is never evidence of non-admission.
    const registration = object(await this.call(method.ASK, turnId, { creationId, question: question as unknown as JsonValue }))
    const askId = string(registration['id'])
    const wait = new AbortController()
    const answer = (async (): Promise<AskAnswer> => {
      for (;;) {
        const value = await this.call(method.WAIT_ASK, turnId, { askId }, wait.signal)
        if (value === null) { await abortableDelay(250, wait.signal); continue }
        const response = object(value), responder = object(response['answeredBy']!)
        if (!Object.hasOwn(response, 'answer') || (responder['kind'] !== 'user' && responder['kind'] !== 'api_key')) throw new Error("Invalid ask response")
        return { answer: normalizeAnswer(question, response['answer']), answeredBy: { kind: responder['kind'], id: string(responder['id']) } }
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
    const receipt = object(await this.call(method.OUTPUT, turnId, { outputId, value: value as unknown as JsonValue }))
    const sequence = receipt['sequence']
    if (typeof sequence !== "number" || !Number.isSafeInteger(sequence) || sequence <= 0) throw new Error("Invalid output receipt sequence")
    return { sequence }
  }
  async respond(turnId: string, responseId: string, value: Content): Promise<void> { await this.call(method.RESPOND, turnId, { responseId, value: value as unknown as JsonValue }) }
  async confirmDrain(turnId: string): Promise<void> {
    await this.call(method.CONFIRM_DRAIN, turnId)
  }
  async finalize(turnId: string, result: JsonValue): Promise<TurnOutcome> {
    try { return outcome(await this.call(method.FINALIZE, turnId, { result })) }
    catch (error) {
      if (!(error instanceof OperationNotSent) || error.code !== "request_too_large") throw error
      // Authored work already drained. This deterministic, never-admitted
      // result is a contract failure, not uncertain success publication.
      return this.fail(turnId, { code: "result_too_large", message: "Turn result exceeds the transport limit" })
    }
  }
  async fail(turnId: string, error: { code: string; message: string }): Promise<TurnOutcome> { return outcome(await this.call(method.FAIL, turnId, { error })) }
  async hold(reason: string): Promise<void> { await this.channel.operation(method.HOLD, { reason }) }

}
