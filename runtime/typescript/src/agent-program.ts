import type { LoadedAgentDefinition } from "../../../sdk/typescript/src/internal/program"
import { normalizeInput, normalizeInvocation } from "../../../sdk/typescript/src/content"
import { create, fromBinary } from "@bufbuild/protobuf"
import { agentProto } from "@helmr/proto"
import type { InitContext, Turn } from "../../../sdk/typescript/src/agent"
import type { JsonValue } from "../../../sdk/typescript/src/contract"
import { requireJson } from "../../../sdk/typescript/src/agent"
import { AgentChannel, OperationError, MAX_AGENT_FRAME_BYTES } from "./agent-channel"
import { installSessionHttp } from "./agent-http"
import { createAgentMcp } from "./agent-mcp"
import { GuestSessionDriver } from "./agent-driver"
import { MessageBusy, SessionRuntime, type TurnTiming } from "./agent-session"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"

export interface AgentProgramIO {
  readonly input: AsyncIterable<Uint8Array>
  write(frame: Uint8Array): Promise<void>
  timing?(record: TurnTiming): void
}
function json(bytes: Uint8Array): JsonValue {
  const value: unknown = JSON.parse(Buffer.from(bytes).toString("utf8"))
  requireJson(value)
  return value
}
function safeSequence(value: bigint): number {
  if (value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error("Session sequence is outside the supported range")
  return Number(value)
}
function source(bytes: Uint8Array): Turn['source'] {
  const raw = json(bytes)
  if (raw === null || Array.isArray(raw) || typeof raw !== "object") throw new Error("Invalid Turn source")
  const value = raw as Record<string, JsonValue>
  if (typeof value['kind'] !== "string") throw new Error("Invalid Turn source")
  if (value['requesterSessionId'] !== undefined && typeof value['requesterSessionId'] !== "string") throw new Error("Invalid requester Session")
  if (value['originTurnId'] !== undefined && typeof value['originTurnId'] !== "string") throw new Error("Invalid origin Turn")
  let slack: Turn['source']['slack']
  if (value['slack'] !== undefined) {
    const rawReference = value['slack']
    if (rawReference === null || Array.isArray(rawReference) || typeof rawReference !== "object") throw new Error("Invalid Slack source")
    const reference = rawReference as Record<string, JsonValue>
    for (const key of ["teamId", "channelId", "threadTs", "messageTs", "userId"]) {
      if (typeof reference[key] !== "string" || !reference[key]) throw new Error("Invalid Slack source")
    }
    slack = { teamId: reference['teamId'] as string, channelId: reference['channelId'] as string, threadTs: reference['threadTs'] as string, messageTs: reference['messageTs'] as string, userId: reference['userId'] as string }
  }
  const kind = value['kind']
  if (kind === "session") {
    if (typeof value['requesterSessionId'] !== "string" || !value['requesterSessionId'] || slack !== undefined) throw new Error("Invalid Session source")
    if (value['originTurnId'] === "") throw new Error("Invalid origin Turn")
    return { kind, requesterSessionId: value['requesterSessionId'], ...(value['originTurnId'] === undefined ? {} : { originTurnId: value['originTurnId'] as string }) }
  }
  if (value['requesterSessionId'] !== undefined || value['originTurnId'] !== undefined) throw new Error("Invalid caller source")
  if (kind === "user") return { kind, ...(slack === undefined ? {} : { slack }) }
  if ((kind === "api_key" || kind === "schedule") && slack === undefined) return { kind }
  throw new Error("Invalid Turn source")
}
function failure(error: unknown): agentProto.OperationError {
  const code = error instanceof MessageBusy ? "message_busy" : error instanceof MessageRejected ? "message_rejected" : error instanceof OperationError ? error.code : "runtime_error"
  return create(agentProto.OperationErrorSchema, { code, message: error instanceof Error ? error.message : String(error) })
}

// A Session owns one process, one setup Promise and one permanent command reader.
// Dispatch is intentionally not awaited by the reader: its own operations need
// responses from that same pipe, as do incoming messages and interruption.
export type AgentDefinitionLoader = (agentId: string) => Promise<LoadedAgentDefinition>

export async function runAgentProgram(definitionSource: LoadedAgentDefinition | AgentDefinitionLoader, io: AgentProgramIO): Promise<void> {
  const stopped = new AbortController()
  const failureConvergenceMs = 10_000
  const uninstallHttp = installSessionHttp()
  let channel: AgentChannel | undefined
  let runtime: SessionRuntime | undefined
  let initialized = false
  let shutdown = false
  let controlSequence = 0n
  const tasks = new Set<Promise<void>>()
  const observe = (pending: Promise<void>) => {
    tasks.add(pending)
    void pending.finally(() => tasks.delete(pending)).catch(error => { stopped.abort(error); channel?.close(error) })
  }
  try {
    for await (const command of readCommands(io.input, stopped.signal)) {
      if (stopped.signal.aborted) break
      if (!channel) {
        if (command.command.case !== "start") throw new Error("First guest command must start a Session")
        const start = command.command.value
        if (command.controlSequence < 0n) throw new Error("Invalid initial control sequence")
        controlSequence = command.controlSequence
        if (!command.identity || !start.agentId || !start.computerId || !start.deploymentId) throw new Error("Invalid Session start identity")
        channel = new AgentChannel(command.identity, frame => io.write(frame), error => stopped.abort(error))
        let definition: LoadedAgentDefinition
        try {
          definition = typeof definitionSource === "function" ? await definitionSource(start.agentId) : definitionSource
          if (start.agentId !== definition.id) throw new Error("Agent bundle does not match Session start")
        } catch (error) {
          const { code, message } = failure(error)
          await channel.event({ case: "failed", value: create(agentProto.SessionFailedSchema, { code, message }) })
          return
        }
        const driver = new GuestSessionDriver(channel)
        const kind = start.recoveryKind === agentProto.SessionStart_RecoveryKind.INITIAL ? "initial" : start.recoveryKind === agentProto.SessionStart_RecoveryKind.RECONSTRUCTED ? "reconstructed" : undefined
        if (!kind) throw new Error("Invalid Session recovery kind")
        const context: InitContext = {
          mcp: createAgentMcp(channel),
          session: { id: command.identity.sessionId, ...(start.conversationKey === undefined ? {} : { key: start.conversationKey }), ...(start.parentSessionId === undefined ? {} : { parent: Object.freeze({ id: start.parentSessionId }) }) },
          computer: { id: start.computerId }, deployment: { id: start.deploymentId }, recovery: { kind, ...(start.recoveryReason ? { reason: start.recoveryReason } : {}) }, signal: stopped.signal,
        }
        runtime = new SessionRuntime(definition, context, driver, { failureConvergenceMs, terminalSequence: safeSequence(start.terminalSequence), ...(io.timing ? { onTiming: io.timing } : {}) })
        const activeChannel = channel
        observe(runtime.initialize().then(async () => {
          initialized = true
          await activeChannel.event({ case: "ready", value: create(agentProto.SessionReadySchema) })
        }, async error => {
          const { code, message } = failure(error)
          await activeChannel.event({ case: "failed", value: create(agentProto.SessionFailedSchema, { code, message }) })
        }))
        continue
      }
      if (!channel.matches(command.identity)) throw new Error("Guest command belongs to another Session process")
      if (command.command.case === "operationResult") { channel.receive(command.identity, command.command.value); continue }
      if (command.command.case === "start") throw new Error("Session already started")
      if (command.command.case === "checkpoint") {
        const checkpointId = command.command.value.checkpointId
        // The supervisor has sealed admission and drained earlier local writes.
        // A settled Helmr continuation grants protocol readiness. The physical
        // owner freezes the entire Linux process tree, including background work.
        const activeChannel = channel
        observe((async () => {
          let outcome: agentProto.SessionCheckpointReady["outcome"]
          try {
            if (!checkpointId || !initialized || !runtime!.canCheckpoint || shutdown) throw new Error("Session is not ready for coherent capture")
            outcome = { case: "ready", value: true }
          } catch (error) { outcome = { case: "error", value: failure(error) } }
          await activeChannel.event({ case: "checkpointReady", value: create(agentProto.SessionCheckpointReadySchema, { checkpointId, outcome }) })
        })())
        continue
      }
      if (!command.deliveryId) throw new Error("Guest command delivery identity is required")
      const current = runtime!
      const activeChannel = channel
      const run = async (): Promise<JsonValue> => {
        const control = command.command.case === "control"
        if (control) {
          if (command.controlSequence <= 0n) throw new Error("Control sequence is required")
          if (command.controlSequence <= controlSequence) return { superseded: true }
        }
        switch (command.command.case) {
          case "dispatch": {
            if (!initialized) throw new Error("Session setup has not completed")
            const turn = command.command.value
            return await current.dispatch({ id: turn.turnId, sequence: safeSequence(turn.sequence), createdAt: turn.createdAt, input: normalizeInvocation(json(turn.inputJson)), source: source(turn.sourceJson) }) as unknown as JsonValue
          }
          case "message": {
            const message = command.command.value
            const messageSource = source(message.sourceJson)
            if (messageSource.kind === "schedule") throw new Error("A schedule cannot send live messages")
            await current.deliverMessage(message.turnId, message.messageId, normalizeInput(json(message.inputJson)), messageSource); return null
          }
          case "control": {
            const control = command.command.value
            controlSequence = command.controlSequence
            if (control.desired === "shutdown") { shutdown = true; stopped.abort(new Error("Session process must stop")); return null }
            if (control.desired !== "hold" && control.desired !== "run") throw new Error("Invalid desired Session state")
            if (control.desired === "hold") current.suspend(new Error("Session is held"))
            if (control.stopDeadlineUnixNano !== 0n) {
              if (control.stopSequence <= 0n) throw new Error("Invalid stop identity")
              if (!control.stopTurnId) {
                shutdown = true
                stopped.abort(new Error("Session initialization interrupted; reconstruction required"))
                throw new Error("Session initialization requires reconstruction")
              }
              await current.interrupt(control.stopTurnId, new Error("Turn interrupted"))
            }
            if (controlSequence !== command.controlSequence) return { superseded: true }
            if (control.desired === "run") {
              await current.waitForResume()
              if (controlSequence !== command.controlSequence) return { superseded: true }
              current.resume()
            }
            return null
          }
          case "reconcile": return await current.reconcileFinalization(command.command.value.turnId) as unknown as JsonValue
          case "acknowledged": current.acknowledgeTerminal(command.command.value.turnId, safeSequence(command.command.value.sequence)); return null
          default: throw new Error("Unknown guest command")
        }
      }
      const delivery = run().then(
        value => activeChannel.event({ case: "deliveryResult", value: create(agentProto.DeliveryResultSchema, { deliveryId: command.deliveryId, outcome: { case: "valueJson", value: Buffer.from(JSON.stringify(value)) } }) }),
        error => activeChannel.event({ case: "deliveryResult", value: create(agentProto.DeliveryResultSchema, { deliveryId: command.deliveryId, outcome: { case: "error", value: failure(error) } }) }),
      )
      observe(delivery)
      if (command.command.case === "control" && shutdown) { await delivery; break }
    }
    if (!channel) throw new Error("Guest pipe closed before Session start")
    if (!shutdown) throw stopped.signal.reason ?? new Error("Guest pipe closed unexpectedly")
  } finally {
    const error = stopped.signal.reason ?? new Error("Guest pipe closed")
    stopped.abort(error)
    channel?.close(error)
    uninstallHttp()
    // Uncooperative authored tasks are fenced by the guest process owner. Never
    // report a clean shutdown merely because their JavaScript promises hung.
  }
}

async function* readCommands(input: AsyncIterable<Uint8Array>, signal: AbortSignal): AsyncGenerator<agentProto.GuestCommand> {
  const iterator = input[Symbol.asyncIterator]()
  const header = Buffer.alloc(4)
  let body: Buffer | undefined
  let filled = 0
  try {
    for (;;) {
      const next = await new Promise<IteratorResult<Uint8Array>>((resolve, reject) => {
        const abort = () => reject(signal.reason)
        signal.addEventListener("abort", abort, { once: true })
        Promise.resolve().then(() => { signal.throwIfAborted(); return iterator.next() }).then(resolve, reject).finally(() => signal.removeEventListener("abort", abort))
        if (signal.aborted) abort()
      })
      if (next.done) break
      const chunk = next.value
      let offset = 0
      while (offset < chunk.length) {
        const destination = body ?? header
        const count = Math.min(destination.length - filled, chunk.length - offset)
        destination.set(chunk.subarray(offset, offset + count), filled)
        filled += count; offset += count
        if (filled !== destination.length) continue
        filled = 0
        if (!body) {
          const length = header.readUInt32BE(0)
          if (!length || length > MAX_AGENT_FRAME_BYTES) throw new Error("Invalid guest frame length")
          body = Buffer.allocUnsafe(length)
        } else {
          yield fromBinary(agentProto.GuestCommandSchema, body)
          body = undefined
        }
      }
    }
    if (filled || body) throw new Error("Truncated guest frame")
  } finally {
    // A failed output pipe must not wait forever for a still-open input pipe.
    void Promise.resolve().then(() => iterator.return?.()).catch(() => {})
  }
}
